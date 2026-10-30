//go:build integration

package chat_test

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/VelimirMueller/synthwerk-llm/internal/cors"
	"github.com/VelimirMueller/synthwerk-llm/internal/identity"
	"github.com/VelimirMueller/synthwerk-llm/internal/provider/fake"
	"github.com/VelimirMueller/synthwerk-llm/internal/store"
	"github.com/VelimirMueller/synthwerk-llm/tests/support/harness"
)

// Widget chat from a customer origin (infra#21): the real identity client asks a
// stub of GET /internal/v1/widget-origins; Postgres stores the conversation.
func TestWidgetChatFromCustomerOrigin(t *testing.T) {
	const shop = "https://shop.example"
	var asked atomic.Int32
	idSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked.Add(1)
		if r.URL.Path != "/internal/v1/widget-origins" || r.Header.Get("Authorization") != "" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if r.URL.Query().Get("origin") == shop {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(idSrv.Close)
	ic, err := identity.New(idSrv.URL)
	if err != nil {
		t.Fatal(err)
	}
	db := env.Database(t)
	h := harness.New(t, store.New(db), harness.Options{DB: db, Provider: fake.New(20 * time.Millisecond),
		CORS: cors.New(cors.Config{Lookup: ic})})
	tok := h.WidgetToken(t, "user-w", "org-1", shop)

	t.Run("Preflight, Create, Send - listed customer origin - 204, 201, SSE, all with the origin allowed", func(t *testing.T) {
		pre := h.Do(t, http.MethodOptions, "/v1/conversations", "", nil, "Origin", shop, "Access-Control-Request-Method", "POST")
		if pre.StatusCode != http.StatusNoContent || pre.Header.Get("Access-Control-Allow-Origin") != shop {
			t.Fatalf("preflight %d %v", pre.StatusCode, pre.Header)
		}
		res := h.Do(t, http.MethodPost, "/v1/conversations", tok, map[string]any{}, "Origin", shop)
		if res.StatusCode != http.StatusCreated || res.Header.Get("Access-Control-Allow-Origin") != shop {
			t.Fatalf("create %d %v", res.StatusCode, res.Header)
		}
		id := h.Create(t, tok, "Origin", shop)
		sres, evs := h.Send(t, tok, id, "hello there", "Origin", shop)
		if sres.StatusCode != http.StatusOK || sres.Header.Get("Access-Control-Allow-Origin") != shop || harness.Text(t, evs) == "" {
			t.Fatalf("send %d %v %v", sres.StatusCode, sres.Header, harness.Names(evs))
		}
		if n := asked.Load(); n != 1 {
			t.Fatalf("identity asked %d times, want 1 (cache)", n)
		}
	})
	t.Run("Preflight - origin identity does not list - 403", func(t *testing.T) {
		pre := h.Do(t, http.MethodOptions, "/v1/conversations", "", nil, "Origin", "https://evil.test", "Access-Control-Request-Method", "POST")
		if pre.StatusCode != http.StatusForbidden || pre.Header.Get("Access-Control-Allow-Origin") != "" {
			t.Fatalf("preflight %d %v", pre.StatusCode, pre.Header)
		}
	})
}
