// Unit tests of the fallback chain (synthwerk#56) with fake providers. Names follow
// "method - condition - outcome".
package chain_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/VelimirMueller/synthwerk-llm/internal/config"
	"github.com/VelimirMueller/synthwerk-llm/internal/provider"
	"github.com/VelimirMueller/synthwerk-llm/internal/provider/chain"
	"github.com/VelimirMueller/synthwerk-llm/internal/provider/fake"
	"github.com/VelimirMueller/synthwerk-llm/internal/provider/registry"
)

const prompt = "PROMPT-CANARY"

// clock is a manual clock for the breakers.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) Add(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

type rig struct {
	c       *chain.Chain
	logs    *bytes.Buffer
	metrics *sdkmetric.ManualReader
	clock   *clock
}

// link is a fake link that serves the alias "default" with model <name>-model.
func link(name string, p *fake.Provider) chain.Link {
	return chain.Link{Name: name, Provider: p, Region: chain.RegionEU, Models: map[string]string{"default": name + "-model"}}
}

func newRig(t *testing.T, mod func(*chain.Config), links ...chain.Link) *rig {
	t.Helper()
	r := &rig{logs: &bytes.Buffer{}, metrics: sdkmetric.NewManualReader(), clock: &clock{t: time.Unix(1_800_000_000, 0)}}
	cfg := chain.Config{
		Links:             links,
		FirstTokenTimeout: time.Second,
		BreakerFailures:   2,
		BreakerOpen:       time.Minute,
		Log:               slog.New(slog.NewJSONHandler(r.logs, nil)),
		Meter:             sdkmetric.NewMeterProvider(sdkmetric.WithReader(r.metrics)),
		Now:               r.clock.Now,
	}
	if mod != nil {
		mod(&cfg)
	}
	c, err := chain.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	r.c = c
	return r
}

func req() provider.Request {
	return provider.Request{Model: "primary-model", Alias: "default", Messages: []provider.Message{{Role: provider.RoleUser, Text: prompt}}}
}

// drain reads the whole answer. It returns the text, who answered and the error
// that ended it (nil on io.EOF).
func drain(t *testing.T, st provider.Stream) (text, by string, err error) {
	t.Helper()
	defer func() { _ = st.Close() }()
	var b strings.Builder
	for {
		p, err := st.Next()
		if errors.Is(err, io.EOF) {
			by, _ = provider.AnsweredBy(st, "")
			return b.String(), by, nil
		}
		if err != nil {
			by, _ = provider.AnsweredBy(st, "")
			return b.String(), by, err
		}
		b.WriteString(p)
	}
}

func answer(t *testing.T, r *rig, rq provider.Request) (text, by string, err error) {
	t.Helper()
	st, err := r.c.Chat(t.Context(), rq)
	if err != nil {
		return "", "", err
	}
	return drain(t, st)
}

// fallbacks returns the fallback counter points as "from>to:reason" -> count.
func (r *rig) fallbacks(t *testing.T) map[string]int64 {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := r.metrics.Collect(t.Context(), &rm); err != nil {
		t.Fatal(err)
	}
	out := map[string]int64{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != "synthwerk.llm.provider.fallbacks" {
				continue
			}
			for _, dp := range m.Data.(metricdata.Sum[int64]).DataPoints {
				from, _ := dp.Attributes.Value("gen_ai.provider.name")
				to, _ := dp.Attributes.Value("synthwerk.provider.next")
				reason, _ := dp.Attributes.Value("synthwerk.fallback.reason")
				out[from.AsString()+">"+to.AsString()+":"+reason.AsString()] += dp.Value
			}
		}
	}
	return out
}

// Chat - primary answers - fallback never called, primary's model and name.
func TestChatPrimaryAnswers(t *testing.T) {
	p, b := fake.New(0), fake.New(0)
	r := newRig(t, nil, link("primary", p), link("backup", b))
	text, by, err := answer(t, r, req())
	if err != nil || text != "echo: "+prompt || by != "primary" || b.Calls() != 0 {
		t.Fatalf("text %q err %v by %q backup calls %d", text, err, by, b.Calls())
	}
	if m := p.Requests()[0].Model; m != "primary-model" {
		t.Fatalf("primary model %q", m)
	}
}

// Chat - primary fails before the first token - the next link answers, in order.
func TestChatFallsOverInOrder(t *testing.T) {
	cases := map[string]struct {
		primary *fake.Provider
		reason  string
	}{
		"status 503":    {&fake.Provider{FailChat: true, Status: 503}, chain.ReasonStatus5xx},
		"status 429":    {&fake.Provider{FailChat: true, Status: 429}, chain.ReasonStatus429},
		"unavailable":   {&fake.Provider{FailChat: true}, chain.ReasonUnavailable},
		"broken stream": {&fake.Provider{FailStream: true}, chain.ReasonStream},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			second := &fake.Provider{FailChat: true, Status: 502}
			third := fake.New(0)
			r := newRig(t, nil, link("primary", tc.primary), link("second", second), link("third", third))
			text, by, err := answer(t, r, req())
			if err != nil || text != "echo: "+prompt || by != "third" {
				t.Fatalf("text %q err %v by %q", text, err, by)
			}
			if tc.primary.Calls() != 1 || second.Calls() != 1 || third.Calls() != 1 {
				t.Fatalf("calls %d %d %d", tc.primary.Calls(), second.Calls(), third.Calls())
			}
			if m := third.Requests()[0].Model; m != "third-model" {
				t.Fatalf("third got model %q, want its own", m)
			}
			got := r.fallbacks(t)
			if got["primary>second:"+tc.reason] != 1 || got["second>third:"+chain.ReasonStatus5xx] != 1 {
				t.Fatalf("fallback metrics %v", got)
			}
			if !strings.Contains(r.logs.String(), `"event.name":"llm.provider.fallback"`) || strings.Contains(r.logs.String(), prompt) {
				t.Fatalf("logs %s", r.logs)
			}
		})
	}
}

// Chat - primary fails after the first token - no fallback, the answer ends failed.
func TestChatNeverFallsOverAfterTheFirstToken(t *testing.T) {
	p := &fake.Provider{FailAfter: 1, Reply: func(provider.Request) []string { return []string{"a", "b"} }}
	b := fake.New(0)
	r := newRig(t, nil, link("primary", p), link("backup", b))
	text, by, err := answer(t, r, req())
	if !errors.Is(err, provider.ErrUnavailable) || text != "a" || by != "primary" || b.Calls() != 0 {
		t.Fatalf("text %q err %v by %q backup calls %d", text, err, by, b.Calls())
	}
	if len(r.fallbacks(t)) != 0 {
		t.Fatalf("fallback after a token: %v", r.fallbacks(t))
	}
}

// Chat - a 4xx other than 429 - no fallback: the request is the problem.
func TestChatClientErrorDoesNotFallOver(t *testing.T) {
	p, b := &fake.Provider{FailChat: true, Status: 400}, fake.New(0)
	r := newRig(t, nil, link("primary", p), link("backup", b))
	if _, _, err := answer(t, r, req()); !errors.Is(err, provider.ErrUnavailable) || b.Calls() != 0 {
		t.Fatalf("err %v backup calls %d", err, b.Calls())
	}
	if s := r.c.State("primary"); s != chain.StateClosed {
		t.Fatalf("a 400 changed the circuit to %s", s)
	}
}

// Chat - no first token in time - the next link answers; the last link has no such timeout.
func TestChatFirstTokenTimeout(t *testing.T) {
	slow := func() *fake.Provider { return &fake.Provider{Delay: 300 * time.Millisecond} }
	t.Run("falls over", func(t *testing.T) {
		p, b := slow(), fake.New(0)
		r := newRig(t, func(c *chain.Config) { c.FirstTokenTimeout = 50 * time.Millisecond }, link("primary", p), link("backup", b))
		text, by, err := answer(t, r, req())
		if err != nil || by != "backup" || text != "echo: "+prompt {
			t.Fatalf("text %q err %v by %q", text, err, by)
		}
		if got := r.fallbacks(t); got["primary>backup:"+chain.ReasonFirstTokenTimeout] != 1 {
			t.Fatalf("metrics %v", got)
		}
	})
	t.Run("last link waits", func(t *testing.T) {
		r := newRig(t, func(c *chain.Config) { c.FirstTokenTimeout = 50 * time.Millisecond }, link("primary", slow()))
		if text, by, err := answer(t, r, req()); err != nil || by != "primary" || text != "echo: "+prompt {
			t.Fatalf("text %q err %v by %q", text, err, by)
		}
	})
}

// Chat - the client left - no fallback, no breaker failure.
func TestChatClientAbortDoesNotFallOver(t *testing.T) {
	p, b := &fake.Provider{Delay: time.Second}, fake.New(0)
	r := newRig(t, nil, link("primary", p), link("backup", b))
	ctx, cancel := context.WithCancel(t.Context())
	st, err := r.c.Chat(ctx, req())
	if err != nil {
		t.Fatal(err)
	}
	time.AfterFunc(20*time.Millisecond, cancel)
	if _, _, err := drain(t, st); !errors.Is(err, context.Canceled) || b.Calls() != 0 {
		t.Fatalf("err %v backup calls %d", err, b.Calls())
	}
	if s := r.c.State("primary"); s != chain.StateClosed {
		t.Fatalf("state %s", s)
	}
}

// Chat - alias the fallback does not serve - its chain is the primary only.
func TestChatChainPerAlias(t *testing.T) {
	p, b := &fake.Provider{FailChat: true, Status: 503}, fake.New(0)
	small := chain.Link{Name: "primary", Provider: p, Region: chain.RegionEU, Models: map[string]string{"default": "p", "small": "p-small"}}
	r := newRig(t, nil, small, link("backup", b))
	rq := req()
	rq.Alias = "small"
	if _, _, err := answer(t, r, rq); err == nil || b.Calls() != 0 {
		t.Fatalf("err %v backup calls %d: the fallback must not serve an alias it does not map", err, b.Calls())
	}
	// No alias (the eval): the primary only, with the model as given.
	rq = req()
	rq.Alias = ""
	if _, _, err := answer(t, r, rq); err == nil || b.Calls() != 0 || p.Requests()[1].Model != "primary-model" {
		t.Fatalf("err %v backup calls %d", err, b.Calls())
	}
}

// Breaker - failures in a row - open skips the link, half-open lets one probe, success closes.
func TestBreaker(t *testing.T) {
	p, b := &fake.Provider{FailChat: true, Status: 503}, fake.New(0)
	r := newRig(t, nil, link("primary", p), link("backup", b))
	for range 2 {
		if _, by, err := answer(t, r, req()); err != nil || by != "backup" {
			t.Fatalf("err %v by %q", err, by)
		}
	}
	if s := r.c.State("primary"); s != chain.StateOpen {
		t.Fatalf("after 2 failures: %s", s)
	}
	// Open: the primary is skipped.
	if _, by, err := answer(t, r, req()); err != nil || by != "backup" || p.Calls() != 2 {
		t.Fatalf("err %v by %q primary calls %d", err, by, p.Calls())
	}
	if got := r.fallbacks(t); got["primary>backup:"+chain.ReasonCircuitOpen] != 1 {
		t.Fatalf("metrics %v", got)
	}

	// Half-open: one probe at a time. The probe holds the primary, a second request skips it.
	r.clock.Add(time.Minute)
	p.FailChat = false
	probe, err := r.c.Chat(t.Context(), req())
	if err != nil || r.c.State("primary") != chain.StateHalfOpen {
		t.Fatalf("probe err %v state %s", err, r.c.State("primary"))
	}
	if _, by, err := answer(t, r, req()); err != nil || by != "backup" {
		t.Fatalf("during the probe: err %v by %q", err, by)
	}
	if _, by, err := drain(t, probe); err != nil || by != "primary" {
		t.Fatalf("probe: err %v by %q", err, by)
	}
	if s := r.c.State("primary"); s != chain.StateClosed {
		t.Fatalf("after a good probe: %s", s)
	}
	if !strings.Contains(r.logs.String(), `"event.name":"llm.provider.circuit.open"`) ||
		!strings.Contains(r.logs.String(), `"event.name":"llm.provider.circuit.closed"`) {
		t.Fatalf("circuit logs %s", r.logs)
	}
}

// Breaker - the probe fails - open again for another BreakerOpen.
func TestBreakerProbeFails(t *testing.T) {
	p, b := &fake.Provider{FailChat: true, Status: 503}, fake.New(0)
	r := newRig(t, nil, link("primary", p), link("backup", b))
	for range 2 {
		_, _, _ = answer(t, r, req())
	}
	r.clock.Add(time.Minute)
	if _, by, err := answer(t, r, req()); err != nil || by != "backup" || p.Calls() != 3 {
		t.Fatalf("err %v by %q primary calls %d", err, by, p.Calls())
	}
	if s := r.c.State("primary"); s != chain.StateOpen {
		t.Fatalf("after a failed probe: %s", s)
	}
}

// Breaker - the last link is open - it is still tried.
func TestBreakerNeverRefusesTheLastLink(t *testing.T) {
	p := &fake.Provider{FailChat: true, Status: 503}
	r := newRig(t, nil, link("primary", p))
	for range 3 {
		if _, _, err := answer(t, r, req()); !errors.Is(err, provider.ErrUnavailable) {
			t.Fatalf("err %v", err)
		}
	}
	if p.Calls() != 3 || r.c.State("primary") != chain.StateOpen {
		t.Fatalf("calls %d state %s", p.Calls(), r.c.State("primary"))
	}
	p.FailChat = false
	if _, _, err := answer(t, r, req()); err != nil || r.c.State("primary") != chain.StateClosed {
		t.Fatalf("err %v state %s", err, r.c.State("primary"))
	}
}

// Answered - the fallback's Chat fails after a fall-over - nobody took the request.
func TestAnsweredNobodyTook(t *testing.T) {
	p, b := &fake.Provider{FailStream: true}, &fake.Provider{FailChat: true, Status: 503}
	r := newRig(t, nil, link("primary", p), link("backup", b))
	st, err := r.c.Chat(t.Context(), req())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Next(); !errors.Is(err, provider.ErrUnavailable) {
		t.Fatalf("err %v", err)
	}
	if name, taken := provider.AnsweredBy(st, ""); name != "backup" || taken {
		t.Fatalf("answered %q taken %v", name, taken)
	}
	_ = st.Close()
}

// New - a non-EU link without AllowNonEU - refused; with it - accepted.
func TestNewDataResidency(t *testing.T) {
	us := chain.Link{Name: "us", Provider: fake.New(0), Region: chain.RegionNonEU, Models: map[string]string{"default": "m"}}
	eu := link("primary", fake.New(0))
	if _, err := chain.New(chain.Config{Links: []chain.Link{eu, us}}); err == nil || !strings.Contains(err.Error(), "outside the EU") {
		t.Fatalf("err %v", err)
	}
	if _, err := chain.New(chain.Config{Links: []chain.Link{eu, us}, AllowNonEU: true}); err != nil {
		t.Fatal(err)
	}
	bad := link("x", fake.New(0))
	bad.Region = ""
	if _, err := chain.New(chain.Config{Links: []chain.Link{bad}}); err == nil {
		t.Fatal("a link without a region was accepted")
	}
	if _, err := chain.New(chain.Config{Links: []chain.Link{eu, eu}}); err == nil {
		t.Fatal("a link name twice was accepted")
	}
}

// registry.Chain - config with one fallback - two links, the primary's name.
func TestRegistryChain(t *testing.T) {
	c, err := registry.Chain(config.Config{
		Provider: config.ProviderFake, Region: config.RegionEU, Aliases: map[string]string{"default": "m"},
		Fallbacks: []config.Fallback{{Name: "backup", Provider: config.ProviderFake, Region: config.RegionEU, Aliases: map[string]string{"default": "b"}}},
	}, slog.New(slog.DiscardHandler))
	if err != nil || c.Name() != "fake" || c.State("backup") != chain.StateClosed {
		t.Fatalf("chain %v err %v", c, err)
	}
	if _, err := registry.Chain(config.Config{
		Provider: config.ProviderFake, Region: config.RegionNonEU, Aliases: map[string]string{"default": "m"},
	}, nil); err == nil {
		t.Fatal("a non-EU primary without the opt-in was built")
	}
}
