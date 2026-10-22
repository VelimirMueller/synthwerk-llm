// Package app wires llm: the health probes and the HTTP routes. It starts from the
// example service of the synthwerk-contracts Go kit (go/examples/service).
package app

import (
	"context"
	"log/slog"
	"net/http"

	"github.com/jackc/pgx/v5/pgconn"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"

	"github.com/VelimirMueller/synthwerk-contracts/go/kit/health"
	kitlog "github.com/VelimirMueller/synthwerk-contracts/go/kit/log"
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

// App holds the dependencies of the service.
type App struct {
	DB      DB
	Log     *slog.Logger
	Health  *health.Handler
	handler http.Handler
}

// New wires the app. Call Migrate, then Health.MarkStarted, then serve Handler.
func New(db DB, logger *slog.Logger, cfg Config) (*App, error) {
	h, err := health.New(cfg.Version, cfg.Commit,
		health.Check{Name: "postgres", Critical: true, Run: db.Ping, Detail: "postgres unreachable"},
	)
	if err != nil {
		return nil, err
	}
	a := &App{DB: db, Log: logger, Health: h}

	mux := http.NewServeMux()
	h.Register(mux)
	// The log line sits inside the span. Probes create no span.
	a.handler = otelhttp.NewHandler(kitlog.HTTP(logger, mux), Service,
		otelhttp.WithFilter(func(r *http.Request) bool {
			return r.URL.Path != "/healthz" && r.URL.Path != "/readyz"
		}))
	return a, nil
}

// Handler is the HTTP handler of the service.
func (a *App) Handler() http.Handler { return a.handler }
