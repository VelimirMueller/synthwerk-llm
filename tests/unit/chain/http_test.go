package chain_test

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/VelimirMueller/synthwerk-llm/internal/provider"
	"github.com/VelimirMueller/synthwerk-llm/internal/provider/chain"
	"github.com/VelimirMueller/synthwerk-llm/internal/provider/openaicompat"
)

// server is a fake OpenAI-compatible provider over HTTP. It counts its calls.
type server struct {
	*httptest.Server
	calls atomic.Int64
}

func newServer(t *testing.T, h func(w http.ResponseWriter, r *http.Request)) *server {
	t.Helper()
	s := &server{}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.calls.Add(1)
		h(w, r)
	}))
	t.Cleanup(s.Close)
	return s
}

// answers streams the pieces, then usage and [DONE].
func answers(pieces ...string) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for _, p := range pieces {
			_, _ = fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":%q}}]}\n\n", p)
			w.(http.Flusher).Flush()
		}
		_, _ = fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n")
		_, _ = fmt.Fprint(w, "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":7,\"completion_tokens\":2}}\n\ndata: [DONE]\n\n")
	}
}

func status(code int) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, _ *http.Request) { http.Error(w, "BODY-CANARY", code) }
}

func httpLink(t *testing.T, name, url string) chain.Link {
	t.Helper()
	p, err := openaicompat.New(openaicompat.Config{Name: name, BaseURL: url})
	if err != nil {
		t.Fatal(err)
	}
	return chain.Link{Name: name, Provider: p, Region: chain.RegionEU, Models: map[string]string{"default": name + "-model"}}
}

// closedURL is a base URL where nothing listens.
func closedURL(t *testing.T) string {
	t.Helper()
	l, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	_ = l.Close()
	return "http://" + addr
}

// Chat over HTTP - primary down in each way - the fallback streams the answer with its usage.
func TestHTTPFallsOver(t *testing.T) {
	stall := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		<-r.Context().Done() // headers, then nothing
	}
	cases := map[string]struct {
		primary func(t *testing.T) string
		reason  string
	}{
		"connection refused": {closedURL, chain.ReasonConnect},
		"503":                {func(t *testing.T) string { return newServer(t, status(503)).URL }, chain.ReasonStatus5xx},
		"429":                {func(t *testing.T) string { return newServer(t, status(429)).URL }, chain.ReasonStatus429},
		"no first token":     {func(t *testing.T) string { return newServer(t, stall).URL }, chain.ReasonFirstTokenTimeout},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			backup := newServer(t, answers("he", "llo"))
			r := newRig(t, func(c *chain.Config) { c.FirstTokenTimeout = 200 * time.Millisecond },
				httpLink(t, "primary", tc.primary(t)), httpLink(t, "backup", backup.URL))
			st, err := r.c.Chat(t.Context(), req())
			if err != nil {
				t.Fatal(err)
			}
			text, by, err := drain(t, st)
			if err != nil || text != "hello" || by != "backup" || backup.calls.Load() != 1 {
				t.Fatalf("text %q by %q err %v backup calls %d", text, by, err, backup.calls.Load())
			}
			if u := st.Result().Usage; u.InputTokens != 7 || u.OutputTokens != 2 || u.Estimated {
				t.Fatalf("usage %+v: must be the fallback's", u)
			}
			if got := r.fallbacks(t); got["primary>backup:"+tc.reason] != 1 {
				t.Fatalf("metrics %v", got)
			}
		})
	}
}

// Chat over HTTP - the connection drops after the first token - no fallback.
func TestHTTPDropAfterFirstToken(t *testing.T) {
	primary := newServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"partial\"}}]}\n\n")
		w.(http.Flusher).Flush()
		panic(http.ErrAbortHandler) // the net/http way to drop the connection on purpose
	})
	backup := newServer(t, answers("never"))
	r := newRig(t, nil, httpLink(t, "primary", primary.URL), httpLink(t, "backup", backup.URL))
	st, err := r.c.Chat(t.Context(), req())
	if err != nil {
		t.Fatal(err)
	}
	text, by, err := drain(t, st)
	if !errors.Is(err, provider.ErrStream) || text != "partial" || by != "primary" || backup.calls.Load() != 0 {
		t.Fatalf("text %q by %q err %v backup calls %d", text, by, err, backup.calls.Load())
	}
}

// Chat over HTTP - every provider down - the last error, nobody took the request.
func TestHTTPAllDown(t *testing.T) {
	a, b := newServer(t, status(503)), newServer(t, status(500))
	r := newRig(t, nil, httpLink(t, "primary", a.URL), httpLink(t, "backup", b.URL))
	_, err := r.c.Chat(t.Context(), req())
	if se, ok := errors.AsType[*provider.StatusError](err); !ok || se.Code != 500 {
		t.Fatalf("err %v", err)
	}
	if a.calls.Load() != 1 || b.calls.Load() != 1 {
		t.Fatalf("calls %d %d", a.calls.Load(), b.calls.Load())
	}
}
