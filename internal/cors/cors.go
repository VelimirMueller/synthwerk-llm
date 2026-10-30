// Package cors answers CORS for the widget chat routes (infra#21). The chat widget
// runs on customer sites, so a static origin list at the edge is not enough.
//
// An origin is allowed when it is in the static list (first-party apps,
// SYNTHWERK_LLM_CORS_ORIGINS) or when identity says that an app of an active org
// lists it (GET /internal/v1/widget-origins). Everything else is denied. The
// answers of identity are cached per canonical origin for a short time.
//
// CORS only lets the browser read the answer. The real check stays in the chat
// handler: a widget token is bound to its origin (synthwerk:origin).
package cors

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"
)

// Lookup asks identity whether an app of an active org lists a canonical origin.
// *identity.Client implements it.
type Lookup interface {
	WidgetOriginListed(ctx context.Context, origin string) (bool, error)
}

// Defaults of Config.
const (
	DefaultTTL           = 60 * time.Second
	DefaultMaxEntries    = 4096
	DefaultLookupTimeout = 2 * time.Second
	// MaxAge is the Access-Control-Max-Age of an allowed preflight, in seconds.
	MaxAge = "600"
	// AllowHeaders are the request headers the widget sends.
	AllowHeaders = "Authorization, Content-Type"
	// ExposeHeaders lets the widget read the wait time of a 429.
	ExposeHeaders = "Retry-After"
)

// Config of a Policy. The zero value denies every origin.
type Config struct {
	// Static holds allowed origins in canonical form (CanonicalOrigin).
	Static []string
	// Lookup asks identity. Nil: the static list only.
	Lookup Lookup
	// TTL of a cached identity answer, yes and no. 0: DefaultTTL.
	TTL time.Duration
	// MaxEntries bounds the cache. 0: DefaultMaxEntries.
	MaxEntries int
	// LookupTimeout bounds one identity call. 0: DefaultLookupTimeout.
	LookupTimeout time.Duration
	// Now is the clock. Nil: time.Now.
	Now func() time.Time
	// Log gets one line per failed lookup. Nil: no log.
	Log *slog.Logger
}

type entry struct {
	allowed bool
	expires time.Time
}

// Policy decides which origins get CORS headers. It is safe for concurrent use.
type Policy struct {
	cfg   Config
	mu    sync.Mutex
	cache map[string]entry
	group singleflight.Group
}

// New returns a Policy. Static entries that are not canonical never match.
func New(c Config) *Policy {
	if c.TTL <= 0 {
		c.TTL = DefaultTTL
	}
	if c.MaxEntries <= 0 {
		c.MaxEntries = DefaultMaxEntries
	}
	if c.LookupTimeout <= 0 {
		c.LookupTimeout = DefaultLookupTimeout
	}
	if c.Now == nil {
		c.Now = time.Now
	}
	c.Static = slices.Clone(c.Static)
	return &Policy{cfg: c, cache: map[string]entry{}}
}

// Allowed reports whether the Origin header value may read the answer. A value
// that fails CanonicalOrigin is denied without a lookup. A failed lookup denies
// and is not cached.
func (p *Policy) Allowed(ctx context.Context, header string) bool {
	origin, err := CanonicalOrigin(header)
	if err != nil {
		return false
	}
	if slices.Contains(p.cfg.Static, origin) {
		return true
	}
	if p.cfg.Lookup == nil {
		return false
	}
	if allowed, ok := p.cached(origin); ok {
		return allowed
	}
	// One identity call per origin at a time. The call does not end with the
	// first caller's request: the others wait for the same answer.
	v, err, _ := p.group.Do(origin, func() (any, error) {
		if allowed, ok := p.cached(origin); ok {
			return allowed, nil
		}
		lctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), p.cfg.LookupTimeout)
		defer cancel()
		allowed, err := p.cfg.Lookup.WidgetOriginListed(lctx, origin)
		if err != nil {
			return false, err
		}
		p.store(origin, allowed)
		return allowed, nil
	})
	if err != nil {
		if p.cfg.Log != nil {
			p.cfg.Log.WarnContext(ctx, "CORS origin lookup failed, origin denied",
				slog.String("event.name", "llm.cors.lookup_failed"), slog.String("exception.message", err.Error()))
		}
		return false
	}
	allowed, _ := v.(bool)
	return allowed
}

func (p *Policy) cached(origin string) (allowed, ok bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.cache[origin]
	if !ok || !p.cfg.Now().Before(e.expires) {
		return false, false
	}
	return e.allowed, true
}

// store adds an answer. A full cache first drops expired entries, then arbitrary
// ones, so the cache never holds more than MaxEntries.
func (p *Policy) store(origin string, allowed bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.cfg.Now()
	if _, ok := p.cache[origin]; !ok && len(p.cache) >= p.cfg.MaxEntries {
		for k, e := range p.cache {
			if !now.Before(e.expires) {
				delete(p.cache, k)
			}
		}
		for k := range p.cache {
			if len(p.cache) < p.cfg.MaxEntries {
				break
			}
			delete(p.cache, k)
		}
	}
	p.cache[origin] = entry{allowed: allowed, expires: now.Add(p.cfg.TTL)}
}

// Len is the number of cached identity answers.
func (p *Policy) Len() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.cache)
}

// singleOrigin returns the Origin header when the request has exactly one.
func singleOrigin(r *http.Request) (string, bool) {
	v := r.Header.Values("Origin")
	if len(v) != 1 {
		return "", false
	}
	return v[0], true
}

// Wrap sets the CORS headers of an actual request, then calls next. Every answer
// gets Vary: Origin. Only an allowed origin gets Access-Control-Allow-Origin and
// Access-Control-Expose-Headers. Wrap does not wrap the ResponseWriter, so an SSE
// answer streams as before.
func (p *Policy) Wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Add("Vary", "Origin")
		if origin, ok := singleOrigin(r); ok && p.Allowed(r.Context(), origin) {
			// The value passed the strict parser: no CR, LF or space.
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Access-Control-Expose-Headers", ExposeHeaders)
		}
		next.ServeHTTP(w, r)
	})
}

// Preflight answers the CORS preflight of a route with method. It needs no token.
// 204 with the CORS headers for one allowed Origin and Access-Control-Request-Method
// equal to method; else 403 without CORS headers. Both send Vary: Origin and
// Cache-Control: no-store. No Access-Control-Allow-Credentials: the widget sends a
// Bearer token, never a cookie.
func (p *Policy) Preflight(method string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Add("Vary", "Origin")
		h.Set("Cache-Control", "no-store")
		origin, ok := singleOrigin(r)
		requested := r.Header.Values("Access-Control-Request-Method")
		if !ok || len(requested) != 1 || requested[0] != method || !p.Allowed(r.Context(), origin) {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		h.Set("Access-Control-Allow-Origin", origin)
		h.Set("Access-Control-Allow-Methods", method)
		h.Set("Access-Control-Allow-Headers", AllowHeaders)
		h.Set("Access-Control-Max-Age", MaxAge)
		w.WriteHeader(http.StatusNoContent)
	})
}

// ParseOrigins parses the comma list of SYNTHWERK_LLM_CORS_ORIGINS into canonical
// origins. Empty items are skipped. An invalid item is an error naming its index.
func ParseOrigins(list string) ([]string, error) {
	var out []string
	for i, item := range strings.Split(list, ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		c, err := CanonicalOrigin(item)
		if err != nil {
			return nil, fmt.Errorf("item %d is not an origin: use scheme://host[:port], http only for localhost", i)
		}
		if !slices.Contains(out, c) {
			out = append(out, c)
		}
	}
	return out, nil
}
