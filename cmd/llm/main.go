// Command llm is the LLM gateway for the Synthwerk family: conversations per user,
// answers streamed over SSE from the provider SYNTHWERK_LLM_PROVIDER names, a
// metadata-only fact per answer on EVT_LLM. It exports a user's own data
// (GET /v1/me/export) and deletes it on identity's user.deleted.v1 (EVT_IDENTITY).
//
//	llm              run the service
//	llm healthcheck  exit 0 when /readyz answers 200 (container health check)
//
// Environment:
//
//	SYNTHWERK_PLATFORM_ENV         local (default), ci, dev, stg, prd
//	SYNTHWERK_LLM_HTTP_ADDR        listen address, default :8080
//	SYNTHWERK_LLM_DATABASE_URL     Postgres URL (required, secret)
//	SYNTHWERK_LLM_NATS_URL         NATS URL (required, secret: may hold credentials)
//	SYNTHWERK_LLM_PROVIDER         ollama, openai_compat or fake (required; fake refused in stg/prd)
//	SYNTHWERK_LLM_BASE_URL         OpenAI-compatible base URL, default http://ollama:11434/v1 for ollama
//	SYNTHWERK_LLM_API_KEY[_FILE]   provider key (secret, optional)
//	SYNTHWERK_LLM_MODEL_ALIASES    alias=model list, default "default=qwen3.5:4b"
//	SYNTHWERK_LLM_OIDC_ISSUER      token issuer (required)
//	SYNTHWERK_LLM_OIDC_AUDIENCE    accepted audiences, comma list (required)
//	SYNTHWERK_LLM_JWKS_URL         key set URL; empty = OIDC discovery on the issuer
//	SYNTHWERK_LLM_WIDGET_ISSUER    iss of identity widget tokens (#30); empty = widget tokens refused
//	SYNTHWERK_LLM_WIDGET_AUDIENCE  accepted widget audiences, comma list, default synthwerk-api
//	SYNTHWERK_LLM_WIDGET_JWKS_URL  widget key set URL, default <widget issuer>/.well-known/jwks.json
//	SYNTHWERK_LLM_IDENTITY_URL     identity base URL for the AI settings (#39); empty = /v1/ai-settings answers 503
//	SYNTHWERK_LLM_QUOTA_USER_TOKENS  tokens per UTC day per user in an org, default 50000 (0 = no limit)
//	SYNTHWERK_LLM_QUOTA_ORG_TOKENS   tokens per UTC day per org, default 500000 (0 = no limit)
//	More limits: internal/config.
//	SYNTHWERK_LLM_LOG_LEVEL        DEBUG, INFO (default), WARN, ERROR
//	SYNTHWERK_LLM_LOG_FORMAT       json (default) or console (local only)
//	OTEL_EXPORTER_OTLP_ENDPOINT    collector (gRPC), for example http://otel-collector:4317. Unset = no export.
//
// Build with the release data:
//
//	go build -ldflags "-X main.version=0.1.0 -X main.commit=$(git rev-parse HEAD)" ./cmd/llm
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"golang.org/x/sync/errgroup"

	"github.com/VelimirMueller/synthwerk-contracts/go/kit/auth"
	"github.com/VelimirMueller/synthwerk-contracts/go/kit/cloudevents"
	"github.com/VelimirMueller/synthwerk-contracts/go/kit/env"
	kitlog "github.com/VelimirMueller/synthwerk-contracts/go/kit/log"
	kitotel "github.com/VelimirMueller/synthwerk-contracts/go/kit/otel"
	"github.com/VelimirMueller/synthwerk-contracts/go/kit/outbox"

	"github.com/VelimirMueller/synthwerk-llm/internal/aisettings"
	"github.com/VelimirMueller/synthwerk-llm/internal/app"
	"github.com/VelimirMueller/synthwerk-llm/internal/chat"
	"github.com/VelimirMueller/synthwerk-llm/internal/config"
	"github.com/VelimirMueller/synthwerk-llm/internal/cors"
	"github.com/VelimirMueller/synthwerk-llm/internal/erasure"
	"github.com/VelimirMueller/synthwerk-llm/internal/identity"
	"github.com/VelimirMueller/synthwerk-llm/internal/provider/registry"
	"github.com/VelimirMueller/synthwerk-llm/internal/quota"
	"github.com/VelimirMueller/synthwerk-llm/internal/store"
)

// Set by -ldflags at build time.
var (
	version = "0.1.0-dev"
	commit  = "0000000"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "healthcheck" {
		if err := healthcheck(env.Get(app.Service, "HTTP_ADDR", ":8080")); err != nil {
			fmt.Fprintln(os.Stderr, "healthcheck:", err)
			os.Exit(1)
		}
		return
	}
	if err := start(); err != nil {
		slog.Error("service stopped", slog.String("event.name", "service.failed"), slog.String("exception.message", err.Error()))
		os.Exit(1)
	}
}

func start() error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return run(ctx)
}

func run(ctx context.Context) error {
	// 1. Logger without the OTel bridge, for the setup lines.
	logOpts, err := kitlog.OptionsFromEnv(app.Service, version)
	if err != nil {
		return err
	}
	logger, err := kitlog.New(logOpts)
	if err != nil {
		return err
	}
	slog.SetDefault(logger)

	// 2. Telemetry, then the final logger that also writes to OTLP.
	tel, err := kitotel.Setup(ctx, kitotel.Config{ServiceName: logOpts.ServiceName, Version: version, Env: logOpts.Env}, logger)
	if err != nil {
		return err
	}
	defer func() {
		sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		_ = tel.Shutdown(sctx)
	}()
	logOpts.LoggerProvider = tel.LoggerProvider
	if logger, err = kitlog.New(logOpts); err != nil {
		return err
	}
	slog.SetDefault(logger)

	// 3. Config and dependencies.
	cfg, err := config.Load(os.Getenv)
	if err != nil {
		return err
	}
	dbURL := env.Get(app.Service, "DATABASE_URL", "")
	if dbURL == "" {
		return errors.New(env.Key(app.Service, "DATABASE_URL") + " is not set")
	}
	poolCfg, err := pgxpool.ParseConfig(dbURL)
	if err != nil {
		return errors.New("database url does not parse") // the error text may hold the password
	}
	// The kit outbox uses the unqualified table name; it must resolve to llm.synthwerk_outbox.
	poolCfg.ConnConfig.RuntimeParams["search_path"] = "llm"
	db, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		return errors.New("database pool does not start")
	}
	defer db.Close()

	nc, err := nats.Connect(cfg.NATSURL, nats.Name("synthwerk-"+app.Service), nats.MaxReconnects(-1))
	if err != nil {
		// The error text may hold the URL with credentials: name the class only.
		switch {
		case errors.Is(err, nats.ErrAuthorization):
			return errors.New("nats does not connect: authorization violation")
		case errors.Is(err, nats.ErrNoServers):
			return errors.New("nats does not connect: no server reachable")
		default:
			return errors.New("nats does not connect")
		}
	}
	defer nc.Close()
	js, err := jetstream.New(nc)
	if err != nil {
		return fmt.Errorf("jetstream: %w", err)
	}
	if err := app.EnsureStream(ctx, js); err != nil {
		return err
	}
	relay, err := outbox.NewRelay(db, js, logger, outbox.Config{})
	if err != nil {
		return err
	}

	verifier, err := auth.NewVerifier(auth.Config{Issuer: cfg.OIDCIssuer, Audiences: cfg.OIDCAudiences, JWKSURL: cfg.JWKSURL})
	if err != nil {
		return err
	}
	// Widget tokens of identity (#30): own issuer, key set and audience; chat routes only.
	var widgetVerifier *auth.Verifier
	if cfg.WidgetIssuer != "" {
		widgetVerifier, err = auth.NewVerifier(auth.Config{Issuer: cfg.WidgetIssuer, Audiences: cfg.WidgetAudiences, JWKSURL: cfg.WidgetJWKSURL})
		if err != nil {
			return err
		}
	} else {
		logger.WarnContext(ctx, "widget tokens are refused: no widget issuer",
			slog.String("event.name", "llm.widget.off"))
	}
	prov, err := registry.New(cfg)
	if err != nil {
		return err
	}
	source, err := cloudevents.Source(app.Service, cfg.Env)
	if err != nil {
		return err
	}
	st := store.New(db)
	chatHandler := chat.New(chat.Config{
		Aliases:             cfg.Aliases,
		DefaultAlias:        config.DefaultAlias,
		SystemPrompt:        cfg.SystemPrompt,
		MaxInputChars:       cfg.MaxInputChars,
		MaxStreamsPerOwner:  cfg.MaxStreamsPerOwner,
		MaxContextChars:     cfg.MaxContextChars,
		StreamTimeout:       cfg.StreamTimeout,
		ProviderConcurrency: cfg.ProviderConcurrency,
		QueueTimeout:        cfg.QueueTimeout,
		QuotaReserveTokens:  cfg.QuotaReserveTokens,
		Source:              source,
	}, st, prov, quota.Static{UserTokens: cfg.QuotaUserTokens, OrgTokens: cfg.QuotaOrgTokens}, logger) // entitlements replace Static with #31

	// AI settings (#39): identity checks the membership and gives the plan.
	// CORS of the widget chat routes (infra#21): identity says which origins apps list.
	var plans aisettings.Plans
	corsCfg := cors.Config{Static: cfg.CORSOrigins, Log: logger}
	if cfg.IdentityURL != "" {
		ic, err := identity.New(cfg.IdentityURL)
		if err != nil {
			return err
		}
		plans = ic
		corsCfg.Lookup = ic
	} else {
		logger.WarnContext(ctx, "AI settings routes answer 503 and widget CORS uses the static list only: identity is not configured",
			slog.String("event.name", "llm.settings.not_configured"))
	}
	settingsHandler := aisettings.New(chatHandler.Service(), st, plans, source, logger)

	// Identity owns EVT_IDENTITY: the consumer waits for it and never creates it.
	eraser := erasure.New(js, st, logger, erasure.Config{})

	a, err := app.New(app.Deps{
		DB: db, Log: logger, Chat: chatHandler, Verifier: verifier, WidgetVerifier: widgetVerifier, Settings: settingsHandler,
		NATS: func(context.Context) error {
			if !nc.IsConnected() {
				return errors.New("not connected")
			}
			return nil
		},
		Erasure: eraser.Ready,
		CORS:    cors.New(corsCfg),
	}, app.Config{Version: version, Commit: commit, Env: logOpts.Env})
	if err != nil {
		return err
	}
	if err := a.Migrate(ctx); err != nil {
		return err
	}
	// Older than one stream timeout plus a margin: no live stream is that old.
	swept, err := st.SweepStreaming(ctx, cfg.StreamTimeout+time.Minute)
	if err != nil {
		return err
	}
	if swept > 0 {
		logger.WarnContext(ctx, "unfinished answers marked failed",
			slog.String("event.name", "llm.message.swept"), slog.Int64("synthwerk.count", swept))
	}

	// 4. HTTP server. /readyz answers 503 until here.
	srv := &http.Server{
		Addr:              env.Get(app.Service, "HTTP_ADDR", ":8080"),
		Handler:           a.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		// No ReadTimeout: when it fires during a long answer, net/http cancels the
		// request context. Bodies are small and bounded (http.MaxBytesReader).
		// No WriteTimeout: answers stream for up to SYNTHWERK_LLM_STREAM_TIMEOUT. The
		// SSE writer sets a deadline per frame instead (internal/sse).
		IdleTimeout: 2 * time.Minute,
	}
	a.Health.MarkStarted()
	logger.InfoContext(ctx, "service started",
		slog.String("event.name", "service.started"),
		slog.String("server.address", srv.Addr),
		slog.String("gen_ai.provider.name", prov.Name()))

	g, gctx := errgroup.WithContext(ctx)
	g.Go(func() error { return relay.Run(gctx) })
	g.Go(func() error { return eraser.Run(gctx) })
	g.Go(func() error {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	})
	g.Go(func() error {
		<-gctx.Done()
		sctx, cancel := context.WithTimeout(context.WithoutCancel(gctx), 10*time.Second)
		defer cancel()
		return srv.Shutdown(sctx)
	})
	return g.Wait()
}

func healthcheck(addr string) error {
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("address %q: %w", addr, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	url := "http://" + net.JoinHostPort("127.0.0.1", port) + "/readyz"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	_ = res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("readyz: status %d", res.StatusCode)
	}
	return nil
}
