// Provider conformance: every adapter passes the same checks (stream, usage,
// mid-stream error, cancel, no text in errors). #72 and new adapters reuse them.
package provider_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/VelimirMueller/synthwerk-llm/internal/provider"
	"github.com/VelimirMueller/synthwerk-llm/internal/provider/fake"
	"github.com/VelimirMueller/synthwerk-llm/internal/provider/openaicompat"
)

// mode tells an adapter under test how to behave.
type mode int

const (
	modeOK      mode = iota // answer "hello world" in two pieces with usage 7/2
	modeMidFail             // first piece, then a broken stream
	modeDown                // fails before the first piece
	modeSlow                // first piece, then waits until canceled
)

const canary = "CANARY-PROMPT-7f3a"

type adapter struct {
	name string
	make func(t *testing.T, m mode) provider.Provider
}

func adapters() []adapter {
	return []adapter{
		{"fake", func(_ *testing.T, m mode) provider.Provider {
			f := &fake.Provider{Reply: func(provider.Request) []string { return []string{"hello", " world"} }}
			switch m {
			case modeMidFail:
				f.FailAfter = 1
			case modeDown:
				f.FailChat = true
			case modeSlow:
				return slowFake{}
			case modeOK:
			}
			return f
		}},
		{"openaicompat", func(t *testing.T, m mode) provider.Provider {
			srv := httptest.NewServer(fakeOllama(t, m))
			t.Cleanup(srv.Close)
			p, err := openaicompat.New(openaicompat.Config{Name: "ollama", BaseURL: srv.URL + "/v1"})
			if err != nil {
				t.Fatal(err)
			}
			return p
		}},
	}
}

// slowFake hangs after the first piece until the context ends.
type slowFake struct{}

func (slowFake) Name() string { return "slow" }

func (slowFake) Chat(ctx context.Context, _ provider.Request) (provider.Stream, error) {
	return &slowStream{ctx: ctx}, nil
}

type slowStream struct {
	ctx  context.Context
	sent bool
}

func (s *slowStream) Next() (string, error) {
	if !s.sent {
		s.sent = true
		return "hello", nil
	}
	<-s.ctx.Done()
	return "", s.ctx.Err()
}
func (*slowStream) Result() provider.Result { return provider.Result{} }
func (*slowStream) Close() error            { return nil }

// fakeOllama is an OpenAI-compatible stream like Ollama's /v1/chat/completions.
func fakeOllama(t *testing.T, m mode) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			http.NotFound(w, r)
			return
		}
		var req map[string]any
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req["stream"] != true {
			t.Errorf("stream = %v, want true", req["stream"])
		}
		if m == modeDown {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = io.WriteString(w, `{"error":"model crashed while reading `+canary+`"}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fl := w.(http.Flusher)
		chunk := func(s string) { _, _ = fmt.Fprintf(w, "data: %s\n\n", s); fl.Flush() }
		chunk(`{"choices":[{"delta":{"role":"assistant","content":"hello"},"finish_reason":null}]}`)
		switch m {
		case modeMidFail:
			_, _ = io.WriteString(w, "data: {not json "+canary+"\n\n")
			return
		case modeSlow:
			<-r.Context().Done()
			return
		case modeOK, modeDown:
		}
		chunk(`{"choices":[{"delta":{"content":" world"},"finish_reason":null}]}`)
		chunk(`{"choices":[{"delta":{},"finish_reason":"stop"}]}`)
		chunk(`{"choices":[],"usage":{"prompt_tokens":7,"completion_tokens":2}}`)
		chunk(`[DONE]`)
	})
}

func req() provider.Request {
	return provider.Request{Model: "m", Messages: []provider.Message{{Role: provider.RoleUser, Text: canary}}}
}

func drain(st provider.Stream) ([]string, error) {
	var out []string
	for {
		p, err := st.Next()
		if err != nil {
			return out, err
		}
		out = append(out, p)
	}
}

func TestConformance(t *testing.T) {
	for _, a := range adapters() {
		t.Run(a.name+" - stream - pieces then EOF and usage", func(t *testing.T) {
			st, err := a.make(t, modeOK).Chat(t.Context(), req())
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = st.Close() }()
			pieces, err := drain(st)
			if !errors.Is(err, io.EOF) || strings.Join(pieces, "") != "hello world" || len(pieces) != 2 {
				t.Fatalf("pieces %q err %v", pieces, err)
			}
			res := st.Result()
			if res.Finish != provider.FinishStop || res.Usage.OutputTokens < 1 || res.Usage.InputTokens < 1 {
				t.Fatalf("result %+v", res)
			}
		})
		t.Run(a.name+" - provider down - ErrUnavailable without text", func(t *testing.T) {
			_, err := a.make(t, modeDown).Chat(t.Context(), req())
			if !errors.Is(err, provider.ErrUnavailable) {
				t.Fatalf("err = %v", err)
			}
			if strings.Contains(err.Error(), canary) || strings.Contains(err.Error(), "crashed") {
				t.Fatalf("error leaks text: %v", err)
			}
		})
		t.Run(a.name+" - mid-stream error - error after first piece, no text", func(t *testing.T) {
			st, err := a.make(t, modeMidFail).Chat(t.Context(), req())
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = st.Close() }()
			pieces, err := drain(st)
			if len(pieces) != 1 || err == nil || errors.Is(err, io.EOF) {
				t.Fatalf("pieces %q err %v", pieces, err)
			}
			if strings.Contains(err.Error(), canary) {
				t.Fatalf("error leaks text: %v", err)
			}
		})
		t.Run(a.name+" - cancel - Next returns soon", func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			st, err := a.make(t, modeSlow).Chat(ctx, req())
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = st.Close() }()
			if p, err := st.Next(); err != nil || p != "hello" {
				t.Fatalf("first piece %q %v", p, err)
			}
			done := make(chan error, 1)
			go func() { _, err := st.Next(); done <- err }()
			time.Sleep(50 * time.Millisecond)
			cancel()
			select {
			case err := <-done:
				if err == nil || errors.Is(err, io.EOF) {
					t.Fatalf("err = %v, want a cancel error", err)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("Next did not return after cancel")
			}
		})
	}
}

func TestOpenAICompatRequestShapeAndKey(t *testing.T) {
	var mu sync.Mutex
	var got map[string]any
	var auth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		auth = r.Header.Get("Authorization")
		_ = json.NewDecoder(r.Body).Decode(&got)
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"x\"},\"finish_reason\":\"length\"}]}\n\ndata: [DONE]\n\n")
	}))
	defer srv.Close()
	p, err := openaicompat.New(openaicompat.Config{BaseURL: srv.URL + "/v1/", APIKey: "k-1"})
	if err != nil {
		t.Fatal(err)
	}
	st, err := p.Chat(t.Context(), provider.Request{Model: "qwen3.5:4b", Messages: []provider.Message{{Role: provider.RoleSystem, Text: "sys"}, {Role: provider.RoleUser, Text: "hi there"}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := drain(st); !errors.Is(err, io.EOF) {
		t.Fatal(err)
	}
	srv.Close() // the handler has returned
	mu.Lock()
	defer mu.Unlock()
	if auth != "Bearer k-1" || got["model"] != "qwen3.5:4b" {
		t.Fatalf("auth %q body %v", auth, got)
	}
	if so, _ := got["stream_options"].(map[string]any); so["include_usage"] != true {
		t.Fatalf("stream_options %v", got["stream_options"])
	}
	res := st.Result()
	if res.Finish != provider.FinishLength || !res.Usage.Estimated || res.Usage.OutputTokens != 1 {
		t.Fatalf("result %+v", res)
	}
}

func TestOpenAICompatRefusesBadBaseURL(t *testing.T) {
	if _, err := openaicompat.New(openaicompat.Config{BaseURL: "ollama:11434"}); err == nil {
		t.Fatal("want an error")
	}
}
