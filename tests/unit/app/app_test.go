package app_test

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/VelimirMueller/synthwerk-llm/internal/app"
)

type fakeDB struct{ err error }

func (f fakeDB) Ping(context.Context) error { return f.err }
func (f fakeDB) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	return pgconn.CommandTag{}, nil
}

func do(t *testing.T, h http.Handler, target string) int {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, target, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Code
}

func TestHealthProbes(t *testing.T) {
	logger := slog.New(slog.DiscardHandler)
	cfg := app.Config{Version: "0.1.0", Commit: "3f9c2e1", Env: "ci"}

	t.Run("healthz is 200", func(t *testing.T) {
		a, err := app.New(fakeDB{}, logger, cfg)
		if err != nil {
			t.Fatal(err)
		}
		if got := do(t, a.Handler(), "/healthz"); got != http.StatusOK {
			t.Fatalf("healthz = %d, want 200", got)
		}
	})

	t.Run("readyz is 503 before start", func(t *testing.T) {
		a, err := app.New(fakeDB{}, logger, cfg)
		if err != nil {
			t.Fatal(err)
		}
		if got := do(t, a.Handler(), "/readyz"); got != http.StatusServiceUnavailable {
			t.Fatalf("readyz = %d, want 503", got)
		}
	})

	t.Run("readyz is 200 when the database pings", func(t *testing.T) {
		a, err := app.New(fakeDB{}, logger, cfg)
		if err != nil {
			t.Fatal(err)
		}
		a.Health.MarkStarted()
		if got := do(t, a.Handler(), "/readyz"); got != http.StatusOK {
			t.Fatalf("readyz = %d, want 200", got)
		}
	})

	t.Run("readyz is 503 when the database is down", func(t *testing.T) {
		a, err := app.New(fakeDB{err: errors.New("unreachable")}, logger, cfg)
		if err != nil {
			t.Fatal(err)
		}
		a.Health.MarkStarted()
		if got := do(t, a.Handler(), "/readyz"); got != http.StatusServiceUnavailable {
			t.Fatalf("readyz = %d, want 503", got)
		}
	})
}
