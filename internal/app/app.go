// Package app wires llm: the health probes, the chat routes behind the token check,
// and the HTTP middlewares.
//
// Two token issuers (#30): Verifier checks Zitadel user tokens, WidgetVerifier the
// widget tokens of synthwerk-identity. A widget token passes only on the chat routes
// the widget uses (create, read, send) and must carry synthwerk:origin, which the
// chat handler binds to the Origin header. Export and AI settings take user tokens
// only. It starts from the example service of the
// synthwerk-contracts Go kit (go/examples/service).
package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/jackc/pgx/v5/pgconn"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"

	"github.com/VelimirMueller/synthwerk-contracts/go/kit/auth"
	"github.com/VelimirMueller/synthwerk-contracts/go/kit/health"
	kitlog "github.com/VelimirMueller/synthwerk-contracts/go/kit/log"

	"github.com/VelimirMueller/synthwerk-llm/internal/aisettings"
	"github.com/VelimirMueller/synthwerk-llm/internal/chat"
	"github.com/VelimirMueller/synthwerk-llm/internal/cors"
	"github.com/VelimirMueller/synthwerk-llm/internal/problem"
)

// Service is the short service name: env prefix SYNTHWERK_LLM_, source /synthwerk/llm/<env>.
const Service = "llm"

// Config of the app.
type Config struct {
	Version string
	Commit  string
	// Env is SYNTHWERK_PLATFORM_ENV.
	Env string
}

// DB is the Postgres interface the app needs: the readiness ping and the migrations.
// *pgxpool.Pool implements it.
type DB interface {
	Ping(ctx context.Context) error
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// Deps are the parts the app wires. Chat and Verifier nil: probes only.
type Deps struct {
	DB       DB
	Log      *slog.Logger
	Chat     *chat.Handler
	Verifier *auth.Verifier
	// WidgetVerifier checks the widget tokens of synthwerk-identity (#30). Nil: llm
	// refuses widget tokens (401).
	WidgetVerifier *auth.Verifier
	// Settings serves /v1/ai-settings (#39). Nil: no settings routes.
	Settings *aisettings.Handler
	// NATS reports the bus connection for /readyz. Nil: no check.
	NATS func(ctx context.Context) error
	// Erasure reports the user.deleted consumer for /readyz (not critical). Nil: no check.
	Erasure func(ctx context.Context) error
	// CORS answers CORS for the widget chat routes (infra#21). Nil: every origin
	// is denied (Vary: Origin, preflight 403).
	CORS *cors.Policy
}

// App holds the dependencies of the service.
type App struct {
	DB      DB
	Log     *slog.Logger
	Health  *health.Handler
	handler http.Handler
}

// New wires the app. Call Migrate, then Health.MarkStarted, then serve Handler.
func New(d Deps, cfg Config) (*App, error) {
	checks := []health.Check{{Name: "postgres", Critical: true, Run: d.DB.Ping, Detail: "postgres unreachable"}}
	if d.NATS != nil {
		checks = append(checks, health.Check{Name: "nats", Critical: true, Run: d.NATS, Detail: "nats not connected"})
	}
	if d.Erasure != nil {
		// Not critical: identity creates EVT_IDENTITY; until then the events wait there.
		checks = append(checks, health.Check{Name: "erasure", Run: d.Erasure, Detail: "user.deleted consumer not attached"})
	}
	if d.Verifier != nil {
		// Not critical: a token check fails closed (401/503) while the keys are away.
		checks = append(checks, health.Check{Name: "jwks", Run: d.Verifier.Ready, Detail: "token keys unavailable"})
	}
	if d.WidgetVerifier != nil {
		checks = append(checks, health.Check{Name: "jwks_widget", Run: d.WidgetVerifier.Ready, Detail: "widget token keys unavailable"})
	}
	h, err := health.New(cfg.Version, cfg.Commit, checks...)
	if err != nil {
		return nil, err
	}
	a := &App{DB: d.DB, Log: d.Log, Health: h}

	mux := http.NewServeMux()
	h.Register(mux)
	if (d.Chat != nil || d.Settings != nil) && d.Verifier == nil {
		return nil, errors.New("app: the chat and settings routes need a token verifier")
	}
	if d.Verifier != nil {
		chatAuthn, userAuthn, err := authn(d.Verifier, d.WidgetVerifier)
		if err != nil {
			return nil, err
		}
		if d.Chat != nil {
			policy := d.CORS
			if policy == nil {
				policy = cors.New(cors.Config{})
			}
			d.Chat.Register(mux, chatAuthn, userAuthn, policy)
		}
		if d.Settings != nil {
			d.Settings.Register(mux, userAuthn)
		}
	}
	// The log line sits inside the span. Probes create no span. Recover sits inside
	// the log line, so a panic still logs one request line with status 500.
	a.handler = otelhttp.NewHandler(kitlog.HTTP(d.Log, recoverer(d.Log, mux)), Service,
		otelhttp.WithFilter(func(r *http.Request) bool {
			return r.URL.Path != "/healthz" && r.URL.Path != "/readyz"
		}))
	return a, nil
}

// authn returns the token checks of the routes. chat: user and widget tokens.
// user: user tokens only; a widget token gets 403.
func authn(user, widget *auth.Verifier) (chat, userOnly func(http.Handler) http.Handler, err error) {
	var tokens auth.TokenVerifier = user
	widgetIssuer := ""
	if widget != nil {
		if tokens, err = auth.NewMultiVerifier(user, widget); err != nil {
			return nil, nil, fmt.Errorf("app: %w", err)
		}
		widgetIssuer = widget.Issuer()
	}
	chat = func(next http.Handler) http.Handler {
		return auth.Middleware(tokens, widgetOrigin(widgetIssuer, next))
	}
	userOnly = func(next http.Handler) http.Handler {
		return auth.Middleware(tokens, onlyUser(user.Issuer(), next))
	}
	return chat, userOnly, nil
}

// widgetOrigin lets a widget token through only with a synthwerk:origin claim, and
// drops its roles and scopes: a widget token is never an admin token. The chat
// handler compares the claim with the Origin header.
func widgetOrigin(widgetIssuer string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, ok := auth.FromContext(r.Context())
		if ok && widgetIssuer != "" && p.Issuer == widgetIssuer {
			if origin, _ := p.Raw[auth.ClaimOrigin].(string); origin == "" {
				problem.Write(w, r, http.StatusForbidden, problem.CodeForbidden, "", 0)
				return
			}
			wp := *p
			wp.Roles, wp.Scopes = nil, nil
			r = r.WithContext(auth.WithPrincipal(r.Context(), &wp))
		}
		next.ServeHTTP(w, r)
	})
}

// onlyUser refuses every token that is not a user token of the user issuer, and every
// token with a synthwerk:origin claim (403). The claim check is on purpose: the claim
// marks a token as bound to a web origin (#30), and export and AI settings are never
// for an embedded page, whoever signed the token. Zitadel user tokens have no such claim.
func onlyUser(userIssuer string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, ok := auth.FromContext(r.Context())
		if ok {
			if _, widget := p.Raw[auth.ClaimOrigin]; widget || p.Issuer != userIssuer {
				problem.Write(w, r, http.StatusForbidden, problem.CodeForbidden, "", 0)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// Handler is the HTTP handler of the service.
func (a *App) Handler() http.Handler { return a.handler }

// recoverer turns a panic into a 500. The log line names the route and the request
// id only: the panic value may hold content.
func recoverer(logger *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() { //nolint:contextcheck // logs with r.Context()
			v := recover()
			if v == nil {
				return
			}
			if err, ok := v.(error); ok && errors.Is(err, http.ErrAbortHandler) {
				panic(v)
			}
			logger.ErrorContext(r.Context(), "request panicked",
				slog.String("event.name", "http.request.panicked"),
				slog.String("http.route", r.Pattern),
				slog.String("synthwerk.request_id", w.Header().Get(kitlog.RequestIDHeader)))
			problem.Write(w, r, http.StatusInternalServerError, problem.CodeInternal, "", 0)
		}()
		next.ServeHTTP(w, r)
	})
}
