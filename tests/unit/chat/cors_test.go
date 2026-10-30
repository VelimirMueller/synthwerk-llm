package chat_test

import (
	"context"
	"net/http"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/VelimirMueller/synthwerk-llm/internal/cors"
	"github.com/VelimirMueller/synthwerk-llm/internal/provider"
	"github.com/VelimirMueller/synthwerk-llm/internal/provider/fake"
	"github.com/VelimirMueller/synthwerk-llm/tests/support/harness"
)

// listedOrigins is identity's internal origin lookup: listed origins are yes.
type listedOrigins struct {
	mu     sync.Mutex
	listed []string
	calls  int
}

func (l *listedOrigins) WidgetOriginListed(_ context.Context, origin string) (bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.calls++
	return slices.Contains(l.listed, origin), nil
}

var widgetRoutes = []struct{ method, path string }{
	{http.MethodPost, "/v1/conversations"},
	{http.MethodGet, "/v1/conversations/" + widgetID},
	{http.MethodPost, "/v1/conversations/" + widgetID + "/messages"},
}

// Preflight of the widget chat routes (infra#21): no token needed, deny by default.
func TestWidgetRoutesPreflight(t *testing.T) {
	ids := &listedOrigins{listed: []string{shop}}
	h := harness.New(t, panicStore{t}, harness.Options{Provider: panicProvider{t},
		CORS: cors.New(cors.Config{Static: []string{"http://localhost:3000"}, Lookup: ids})})

	for _, rt := range widgetRoutes {
		t.Run("Preflight "+rt.method+" "+rt.path+" - customer origin listed in identity, no token - 204", func(t *testing.T) {
			res := h.Do(t, http.MethodOptions, rt.path, "", nil, "Origin", shop, "Access-Control-Request-Method", rt.method,
				"Access-Control-Request-Headers", "authorization,content-type")
			hd := res.Header
			if res.StatusCode != http.StatusNoContent || hd.Get("Access-Control-Allow-Origin") != shop || hd.Get("Access-Control-Allow-Methods") != rt.method ||
				hd.Get("Access-Control-Allow-Headers") != "Authorization, Content-Type" || hd.Get("Access-Control-Max-Age") != "600" ||
				hd.Get("Vary") != "Origin" || hd.Get("Cache-Control") != "no-store" || hd.Get("Access-Control-Allow-Credentials") != "" {
				t.Fatalf("%d %v", res.StatusCode, hd)
			}
		})
		t.Run("Preflight "+rt.method+" "+rt.path+" - origin in no list - 403, no CORS header", func(t *testing.T) {
			res := h.Do(t, http.MethodOptions, rt.path, "", nil, "Origin", "https://evil.test", "Access-Control-Request-Method", rt.method)
			if res.StatusCode != http.StatusForbidden || res.Header.Get("Access-Control-Allow-Origin") != "" || res.Header.Get("Vary") != "Origin" {
				t.Fatalf("%d %v", res.StatusCode, res.Header)
			}
		})
	}
	t.Run("Preflight - first-party origin of the static list - 204", func(t *testing.T) {
		res := h.Do(t, http.MethodOptions, "/v1/conversations", "", nil, "Origin", "http://localhost:3000", "Access-Control-Request-Method", "POST")
		if res.StatusCode != http.StatusNoContent || res.Header.Get("Access-Control-Allow-Origin") != "http://localhost:3000" {
			t.Fatalf("%d %v", res.StatusCode, res.Header)
		}
	})
	t.Run("Preflight - GET requested on the create route - 403", func(t *testing.T) {
		res := h.Do(t, http.MethodOptions, "/v1/conversations", "", nil, "Origin", shop, "Access-Control-Request-Method", "GET")
		if res.StatusCode != http.StatusForbidden || res.Header.Get("Access-Control-Allow-Origin") != "" {
			t.Fatalf("%d %v", res.StatusCode, res.Header)
		}
	})
	for _, path := range []string{"/v1/me/export", "/v1/ai-settings"} {
		t.Run("Preflight "+path+" - not a widget route - no CORS from llm", func(t *testing.T) {
			res := h.Do(t, http.MethodOptions, path, "", nil, "Origin", shop, "Access-Control-Request-Method", "GET")
			if res.StatusCode == http.StatusNoContent || res.Header.Get("Access-Control-Allow-Origin") != "" {
				t.Fatalf("%d %v", res.StatusCode, res.Header)
			}
		})
	}
	t.Run("Preflights above - identity asked once per origin", func(t *testing.T) {
		ids.mu.Lock()
		defer ids.mu.Unlock()
		if ids.calls != 2 { // shop and evil.test; the cache answers the rest
			t.Fatalf("identity calls %d", ids.calls)
		}
	})
}

// Create, Get, Send from a customer origin (infra#21): CORS headers on every
// answer, the 401 included, and the answer still streams.
func TestWidgetRoutesCORSAnswers(t *testing.T) {
	st := &ownerStore{panicStore: panicStore{t}}
	f := fake.New(100 * time.Millisecond)
	f.Reply = func(provider.Request) []string { return []string{"one ", "two ", "three ", "four"} }
	h := harness.New(t, st, harness.Options{Provider: f, CORS: cors.New(cors.Config{Lookup: &listedOrigins{listed: []string{shop}}})})
	tok := h.WidgetToken(t, "user-w", "org-1", shop)

	hasCORS := func(t *testing.T, res *http.Response) {
		t.Helper()
		if res.Header.Get("Access-Control-Allow-Origin") != shop || res.Header.Get("Access-Control-Expose-Headers") != "Retry-After" || res.Header.Get("Vary") != "Origin" {
			t.Fatalf("%d %v", res.StatusCode, res.Header)
		}
	}
	t.Run("Create - no token, listed origin - 401 the widget can read", func(t *testing.T) {
		res := h.Do(t, http.MethodPost, "/v1/conversations", "", map[string]any{}, "Origin", shop)
		if res.StatusCode != http.StatusUnauthorized {
			t.Fatalf("%d", res.StatusCode)
		}
		hasCORS(t, res)
	})
	id := h.Create(t, tok, "Origin", shop)
	t.Run("Get - widget token, listed origin - 200 with CORS headers", func(t *testing.T) {
		res := h.Do(t, http.MethodGet, "/v1/conversations/"+id, tok, nil, "Origin", shop)
		if res.StatusCode != http.StatusOK {
			t.Fatalf("%d", res.StatusCode)
		}
		hasCORS(t, res)
	})
	t.Run("Send - widget token, listed origin - SSE with CORS headers, pieces arrive one by one", func(t *testing.T) {
		res, evs := h.Send(t, tok, id, "hi", "Origin", shop)
		if res.StatusCode != http.StatusOK || res.Header.Get("Content-Type") != "text/event-stream" {
			t.Fatalf("%d %v", res.StatusCode, res.Header)
		}
		hasCORS(t, res)
		var deltas []time.Time
		for _, ev := range evs {
			if ev.Event == "delta" {
				deltas = append(deltas, ev.At)
			}
		}
		if len(deltas) < 4 || deltas[len(deltas)-1].Sub(deltas[0]) < 200*time.Millisecond {
			t.Fatalf("buffered stream: deltas %d, spread %v", len(deltas), deltas)
		}
	})
	t.Run("Get - origin in no list - answer without CORS headers, Vary", func(t *testing.T) {
		res := h.Do(t, http.MethodGet, "/v1/conversations/"+id, h.Token(t, "user-w", "org-1"), nil, "Origin", "https://evil.test")
		if res.Header.Get("Access-Control-Allow-Origin") != "" || res.Header.Get("Vary") != "Origin" {
			t.Fatalf("%d %v", res.StatusCode, res.Header)
		}
	})
}

// No CORS policy wired: every origin is denied, the routes work as before.
func TestWidgetRoutesWithoutCORSPolicy(t *testing.T) {
	h := harness.New(t, panicStore{t}, harness.Options{Provider: panicProvider{t}})
	res := h.Do(t, http.MethodOptions, "/v1/conversations", "", nil, "Origin", shop, "Access-Control-Request-Method", "POST")
	if res.StatusCode != http.StatusForbidden || res.Header.Get("Access-Control-Allow-Origin") != "" {
		t.Fatalf("%d %v", res.StatusCode, res.Header)
	}
}
