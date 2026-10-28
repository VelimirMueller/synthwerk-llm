// Package app wires llm: the health probes, the chat routes behind the token check,
// and the HTTP middlewares. It starts from the example service of the
// synthwerk-contracts Go kit (go/examples/service).
package app

import (
	"context"
	"errors"
	"log/slog"
	"net/http"

	"github.com/jackc/pgx/v5/pgconn"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"

	"github.com/VelimirMueller/synthwerk-contracts/go/kit/auth"
	"github.com/VelimirMueller/synthwerk-contracts/go/kit/health"
	kitlog "github.com/VelimirMueller/synthwerk-contracts/go/kit/log"

	"github.com/VelimirMueller/synthwerk-llm/internal/chat"
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
	// NATS reports the bus connection for /readyz. Nil: no check.
	NATS func(ctx context.Context) error
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
	if d.Verifier != nil {
		// Not critical: a token check fails closed (401/503) while the keys are away.
		checks = append(checks, health.Check{Name: "jwks", Run: d.Verifier.Ready, Detail: "token keys unavailable"})
	}
	h, err := health.New(cfg.Version, cfg.Commit, checks...)
	if err != nil {
		return nil, err
	}
	a := &App{DB: d.DB, Log: d.Log, Health: h}

	mux := http.NewServeMux()
	h.Register(mux)
	if d.Chat != nil {
		if d.Verifier == nil {
			return nil, errors.New("app: the chat routes need a token verifier")
		}
		d.Chat.Register(mux, func(next http.Handler) http.Handler { return auth.Middleware(d.Verifier, next) })
	}
	// The log line sits inside the span. Probes create no span. Recover sits inside
	// the log line, so a panic still logs one request line with status 500.
	a.handler = otelhttp.NewHandler(kitlog.HTTP(d.Log, recoverer(d.Log, mux)), Service,
		otelhttp.WithFilter(func(r *http.Request) bool {
			return r.URL.Path != "/healthz" && r.URL.Path != "/readyz"
		}))
	return a, nil
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
