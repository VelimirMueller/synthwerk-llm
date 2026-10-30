package cors_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/VelimirMueller/synthwerk-llm/internal/cors"
)

const (
	shop  = "https://shop.example"
	local = "http://localhost:3000"
)

// fakeIdentity answers like GET /internal/v1/widget-origins: listed origins are yes.
type fakeIdentity struct {
	mu     sync.Mutex
	listed []string
	asked  []string
	err    error
	block  chan struct{} // non-nil: wait until closed or the context ends
}

func (f *fakeIdentity) WidgetOriginListed(ctx context.Context, origin string) (bool, error) {
	f.mu.Lock()
	f.asked = append(f.asked, origin)
	block, err, listed := f.block, f.err, slices.Contains(f.listed, origin)
	f.mu.Unlock()
	if block != nil {
		select {
		case <-block:
		case <-ctx.Done():
			return false, ctx.Err()
		}
	}
	return listed, err
}

func (f *fakeIdentity) calls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.asked)
}

type clock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *clock) add(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

func TestAllowed(t *testing.T) {
	t.Run("Allowed - static origin - yes without a lookup", func(t *testing.T) {
		id := &fakeIdentity{}
		p := cors.New(cors.Config{Static: []string{local}, Lookup: id})
		if !p.Allowed(t.Context(), local) || len(id.calls()) != 0 {
			t.Fatalf("calls %v", id.calls())
		}
	})
	t.Run("Allowed - no lookup configured, origin not static - no", func(t *testing.T) {
		p := cors.New(cors.Config{Static: []string{local}})
		if p.Allowed(t.Context(), shop) {
			t.Fatal("allowed")
		}
	})
	t.Run("Allowed - zero config - every origin denied", func(t *testing.T) {
		if cors.New(cors.Config{}).Allowed(t.Context(), local) {
			t.Fatal("allowed")
		}
	})
	t.Run("Allowed - origin listed in identity, upper case with default port - yes, identity gets the canonical form", func(t *testing.T) {
		id := &fakeIdentity{listed: []string{shop}}
		p := cors.New(cors.Config{Lookup: id})
		if !p.Allowed(t.Context(), "HTTPS://Shop.Example:443") || !slices.Equal(id.calls(), []string{shop}) {
			t.Fatalf("calls %v", id.calls())
		}
	})
	junk := []string{"", "null", shop + "/", shop + "/path", "https://*.shop.example", "http://shop.example",
		"https://a@shop.example", "shop.example", "https://shop.example.", "https://shop.example:0443", "https://sh op.example",
		"https://shop.example\r\nX: y", "javascript://x", "https://shop%2eexample"}
	for _, o := range junk {
		t.Run(fmt.Sprintf("Allowed - invalid origin %q - no, no lookup, nothing cached", o), func(t *testing.T) {
			id := &fakeIdentity{listed: []string{shop}}
			p := cors.New(cors.Config{Lookup: id})
			if p.Allowed(t.Context(), o) || len(id.calls()) != 0 || p.Len() != 0 {
				t.Fatalf("calls %v len %d", id.calls(), p.Len())
			}
		})
	}
	t.Run("Allowed - yes and no answers - cached for the TTL, asked again after it", func(t *testing.T) {
		id := &fakeIdentity{listed: []string{shop}}
		c := &clock{now: time.Unix(1_700_000_000, 0)}
		p := cors.New(cors.Config{Lookup: id, TTL: time.Minute, Now: c.Now})
		for range 3 {
			if !p.Allowed(t.Context(), shop) || p.Allowed(t.Context(), "https://evil.test") {
				t.Fatal("wrong answer")
			}
		}
		if n := len(id.calls()); n != 2 {
			t.Fatalf("lookups %d, want 2", n)
		}
		c.add(time.Minute)
		p.Allowed(t.Context(), shop)
		p.Allowed(t.Context(), "https://evil.test")
		if n := len(id.calls()); n != 4 {
			t.Fatalf("lookups after the TTL %d, want 4", n)
		}
	})
	t.Run("Allowed - identity fails - no, the error is not cached", func(t *testing.T) {
		id := &fakeIdentity{listed: []string{shop}, err: errors.New("identity: unavailable: status 500")}
		p := cors.New(cors.Config{Lookup: id})
		if p.Allowed(t.Context(), shop) || p.Len() != 0 {
			t.Fatal("allowed or cached on error")
		}
		id.mu.Lock()
		id.err = nil
		id.mu.Unlock()
		if !p.Allowed(t.Context(), shop) || len(id.calls()) != 2 {
			t.Fatalf("calls %v", id.calls())
		}
	})
	t.Run("Allowed - identity hangs - no after the lookup timeout", func(t *testing.T) {
		id := &fakeIdentity{listed: []string{shop}, block: make(chan struct{})}
		defer close(id.block)
		p := cors.New(cors.Config{Lookup: id, LookupTimeout: 50 * time.Millisecond})
		start := time.Now()
		if p.Allowed(t.Context(), shop) || time.Since(start) > 2*time.Second {
			t.Fatalf("allowed or slow: %s", time.Since(start))
		}
	})
	t.Run("Allowed - 3000 different valid origins, bound 100 - cache never above 100", func(t *testing.T) {
		id := &fakeIdentity{}
		p := cors.New(cors.Config{Lookup: id, MaxEntries: 100})
		for i := range 3000 {
			p.Allowed(t.Context(), fmt.Sprintf("https://x%d.example", i))
			if p.Len() > 100 {
				t.Fatalf("cache %d after %d origins", p.Len(), i+1)
			}
		}
	})
	t.Run("Allowed - 100 parallel new origins, identity hangs, 4 slots - at most 4 lookups, the rest denied at once, nothing cached, slots free again", func(t *testing.T) {
		id := &fakeIdentity{listed: []string{shop}, block: make(chan struct{})}
		p := cors.New(cors.Config{Static: []string{"http://localhost:3000"}, Lookup: id, MaxLookups: 4})
		var wg sync.WaitGroup
		var yes atomic.Int32
		start := time.Now()
		for i := range 100 {
			wg.Go(func() {
				if p.Allowed(context.Background(), fmt.Sprintf("https://flood%d.example", i)) {
					yes.Add(1)
				}
			})
		}
		for len(id.calls()) < 4 {
			time.Sleep(time.Millisecond)
		}
		// The service keeps answering: the static list needs no slot.
		if !p.Allowed(t.Context(), "http://localhost:3000") {
			t.Fatal("static origin denied during the flood")
		}
		time.Sleep(20 * time.Millisecond)
		if n := len(id.calls()); n > 4 {
			t.Fatalf("%d lookups in flight, want at most 4", n)
		}
		close(id.block)
		wg.Wait()
		if yes.Load() != 0 || len(id.calls()) > 4 || p.Len() > 4 || time.Since(start) > 2*time.Second {
			t.Fatalf("yes %d lookups %d cached %d after %s", yes.Load(), len(id.calls()), p.Len(), time.Since(start))
		}
		id.mu.Lock()
		id.block = nil
		id.mu.Unlock()
		if !p.Allowed(t.Context(), shop) {
			t.Fatal("after the flood a listed origin is denied: slots not freed")
		}
	})
	t.Run("Allowed - 50 parallel requests for one new origin - one lookup", func(t *testing.T) {
		id := &fakeIdentity{listed: []string{shop}, block: make(chan struct{})}
		p := cors.New(cors.Config{Lookup: id})
		var wg sync.WaitGroup
		var yes atomic.Int32
		for range 50 {
			wg.Go(func() {
				if p.Allowed(context.Background(), shop) {
					yes.Add(1)
				}
			})
		}
		for len(id.calls()) == 0 {
			time.Sleep(time.Millisecond)
		}
		time.Sleep(20 * time.Millisecond)
		close(id.block)
		wg.Wait()
		if yes.Load() != 50 || len(id.calls()) != 1 {
			t.Fatalf("yes %d lookups %d", yes.Load(), len(id.calls()))
		}
	})
}

func preflight(t *testing.T, p *cors.Policy, method string, header ...string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodOptions, "/v1/conversations", nil)
	for i := 0; i+1 < len(header); i += 2 {
		req.Header.Add(header[i], header[i+1])
	}
	rec := httptest.NewRecorder()
	p.Preflight(method).ServeHTTP(rec, req)
	return rec
}

var corsHeaders = []string{"Access-Control-Allow-Origin", "Access-Control-Allow-Methods", "Access-Control-Allow-Headers",
	"Access-Control-Max-Age", "Access-Control-Allow-Credentials", "Access-Control-Expose-Headers"}

func TestPreflight(t *testing.T) {
	p := cors.New(cors.Config{Static: []string{local}, Lookup: &fakeIdentity{listed: []string{shop}}})
	t.Run("Preflight - listed origin, POST requested - 204 with the CORS headers, no credentials", func(t *testing.T) {
		rec := preflight(t, p, http.MethodPost, "Origin", shop, "Access-Control-Request-Method", "POST", "Access-Control-Request-Headers", "authorization,content-type")
		h := rec.Header()
		if rec.Code != http.StatusNoContent || h.Get("Access-Control-Allow-Origin") != shop || h.Get("Access-Control-Allow-Methods") != "POST" ||
			h.Get("Access-Control-Allow-Headers") != "Authorization, Content-Type" || h.Get("Access-Control-Max-Age") != "600" ||
			h.Get("Vary") != "Origin" || h.Get("Cache-Control") != "no-store" || h.Get("Access-Control-Allow-Credentials") != "" {
			t.Fatalf("%d %v", rec.Code, h)
		}
	})
	t.Run("Preflight - static origin - 204", func(t *testing.T) {
		if rec := preflight(t, p, http.MethodGet, "Origin", local, "Access-Control-Request-Method", "GET"); rec.Code != http.StatusNoContent {
			t.Fatalf("%d", rec.Code)
		}
	})
	refused := []struct {
		name   string
		header []string
	}{
		{"origin not listed", []string{"Origin", "https://evil.test", "Access-Control-Request-Method", "POST"}},
		{"null origin", []string{"Origin", "null", "Access-Control-Request-Method", "POST"}},
		{"no Origin", []string{"Access-Control-Request-Method", "POST"}},
		{"two Origin headers", []string{"Origin", shop, "Origin", shop, "Access-Control-Request-Method", "POST"}},
		{"no Access-Control-Request-Method", []string{"Origin", shop}},
		{"other method requested", []string{"Origin", shop, "Access-Control-Request-Method", "DELETE"}},
		{"method in lower case", []string{"Origin", shop, "Access-Control-Request-Method", "post"}},
	}
	for _, c := range refused {
		t.Run("Preflight - "+c.name+" - 403 without CORS headers, Vary, no-store", func(t *testing.T) {
			rec := preflight(t, p, http.MethodPost, c.header...)
			if rec.Code != http.StatusForbidden || rec.Header().Get("Vary") != "Origin" || rec.Header().Get("Cache-Control") != "no-store" {
				t.Fatalf("%d %v", rec.Code, rec.Header())
			}
			for _, k := range corsHeaders {
				if rec.Header().Get(k) != "" {
					t.Fatalf("%s set: %v", k, rec.Header())
				}
			}
		})
	}
}

func TestWrap(t *testing.T) {
	p := cors.New(cors.Config{Lookup: &fakeIdentity{listed: []string{shop}}})
	var called bool
	h := p.Wrap(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	do := func(origins ...string) *httptest.ResponseRecorder {
		called = false
		req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/conversations", nil)
		for _, o := range origins {
			req.Header.Add("Origin", o)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	t.Run("Wrap - listed origin - allow origin, expose Retry-After, Vary, handler runs", func(t *testing.T) {
		rec := do(shop)
		hd := rec.Header()
		if !called || hd.Get("Access-Control-Allow-Origin") != shop || hd.Get("Access-Control-Expose-Headers") != "Retry-After" || hd.Get("Vary") != "Origin" || hd.Get("Access-Control-Allow-Credentials") != "" {
			t.Fatalf("%v", hd)
		}
	})
	for _, c := range [][]string{{"https://evil.test"}, nil, {shop, shop}} {
		t.Run(fmt.Sprintf("Wrap - origins %v - Vary only, handler runs", c), func(t *testing.T) {
			rec := do(c...)
			if !called || rec.Header().Get("Vary") != "Origin" {
				t.Fatalf("%v", rec.Header())
			}
			for _, k := range corsHeaders {
				if rec.Header().Get(k) != "" {
					t.Fatalf("%s set: %v", k, rec.Header())
				}
			}
		})
	}
}

func TestParseOrigins(t *testing.T) {
	t.Run("ParseOrigins - list with spaces, upper case, default port, duplicate - canonical, once each", func(t *testing.T) {
		got, err := cors.ParseOrigins(" http://localhost:3000, HTTPS://App.Example:443 ,,https://app.example")
		if err != nil || !slices.Equal(got, []string{local, "https://app.example"}) {
			t.Fatalf("%v %v", got, err)
		}
	})
	t.Run("ParseOrigins - empty - no origins", func(t *testing.T) {
		if got, err := cors.ParseOrigins(""); err != nil || len(got) != 0 {
			t.Fatalf("%v %v", got, err)
		}
	})
	for _, bad := range []string{"*", "null", "https://app.example/", "http://app.example"} {
		t.Run("ParseOrigins - "+bad+" - error", func(t *testing.T) {
			if _, err := cors.ParseOrigins(local + "," + bad); err == nil {
				t.Fatal("accepted")
			}
		})
	}
}
