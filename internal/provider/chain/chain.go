// Package chain is the fallback provider of the gateway (synthwerk#56). It holds an
// ordered list of providers, the links. The first link is the primary. The chain of
// one model alias is every link that maps the alias, in that order.
//
// Rules:
//   - Fall over only on a clear provider-side failure before the first piece: no
//     connection, status 5xx, status 429, a broken stream, or no first piece within
//     FirstTokenTimeout. Never after the first piece: the client never gets a mixed
//     or repeated answer.
//   - A 4xx other than 429, a client abort and the stream timeout never fall over.
//   - Each link has a circuit breaker: after BreakerFailures failures in a row the
//     chain skips the link for BreakerOpen, then lets one request through as a probe
//     (half-open). The last link of a chain is always tried: the breaker routes
//     around a provider, it never refuses the last one.
//   - A link outside the EU needs AllowNonEU. Rule of synthwerk docs/infrastructure.md:
//     all data stays in the EU.
//   - The answer, its usage and its quota charge are the answering link's. The
//     stream says which link answered (provider.Answered).
//
// Logs and metrics carry the provider names, the alias and the reason only.
package chain

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/VelimirMueller/synthwerk-llm/internal/provider"
)

// Region of a provider: where the prompts go.
type Region string

// Regions a link can have.
const (
	RegionEU    Region = "eu"
	RegionNonEU Region = "non_eu"
)

// Reasons of a fall-over or a failure, as logs and metrics name them.
const (
	ReasonConnect           = "connect"
	ReasonStatus5xx         = "status_5xx"
	ReasonStatus429         = "status_429"
	ReasonStatus4xx         = "status_4xx"
	ReasonStream            = "stream"
	ReasonUnavailable       = "unavailable"
	ReasonFirstTokenTimeout = "first_token_timeout"
	ReasonCircuitOpen       = "circuit_open"
	ReasonOther             = "error"
)

// Circuit states, as logs and metrics name them.
const (
	StateClosed   = "closed"
	StateOpen     = "open"
	StateHalfOpen = "half_open"
)

// Defaults of Config.
const (
	DefaultFirstTokenTimeout = 30 * time.Second
	DefaultBreakerFailures   = 3
	DefaultBreakerOpen       = 30 * time.Second
)

// meterName is the instrumentation scope of the metrics.
const meterName = "github.com/VelimirMueller/synthwerk-llm/internal/provider/chain"

// Link is one provider of the chain.
type Link struct {
	// Name in logs, metrics and the completed event, for example ollama or backup.
	Name     string
	Provider provider.Provider
	Region   Region
	// Models maps an alias to the model name of this provider. A link without the
	// alias is not in that alias's chain.
	Models map[string]string
}

// Config of the chain.
type Config struct {
	// Links in order. The first is the primary.
	Links []Link
	// FirstTokenTimeout is the max wait for the first piece of a link that has a
	// next link. Default 30 s.
	FirstTokenTimeout time.Duration
	// BreakerFailures in a row open the circuit of a link. Default 3.
	BreakerFailures int
	// BreakerOpen is how long an open circuit skips the link. Default 30 s.
	BreakerOpen time.Duration
	// AllowNonEU lets a link with RegionNonEU in. Default false.
	AllowNonEU bool
	// Log for the fall-over and circuit lines. Nil: slog.Default().
	Log *slog.Logger
	// Meter for the metrics. Nil: the global meter provider.
	Meter metric.MeterProvider
	// Now is the clock of the breakers. Nil: time.Now.
	Now func() time.Time
}

// Chain is a provider.Provider over the links.
type Chain struct {
	cfg   Config
	links []*link
	log   *slog.Logger

	fallbacks   metric.Int64Counter
	failures    metric.Int64Counter
	transitions metric.Int64Counter
}

type link struct {
	Link
	br *breaker
}

// errFirstToken is the cancel cause of an attempt without a first piece in time.
var errFirstToken = errors.New("chain: no first token in time")

// New checks the config and returns the chain.
func New(cfg Config) (*Chain, error) {
	if len(cfg.Links) == 0 {
		return nil, errors.New("chain: no provider")
	}
	if cfg.FirstTokenTimeout <= 0 {
		cfg.FirstTokenTimeout = DefaultFirstTokenTimeout
	}
	if cfg.BreakerFailures <= 0 {
		cfg.BreakerFailures = DefaultBreakerFailures
	}
	if cfg.BreakerOpen <= 0 {
		cfg.BreakerOpen = DefaultBreakerOpen
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	c := &Chain{cfg: cfg, log: cfg.Log}
	if c.log == nil {
		c.log = slog.Default()
	}
	seen := map[string]bool{}
	for _, l := range cfg.Links {
		switch {
		case l.Name == "" || l.Provider == nil:
			return nil, errors.New("chain: a link needs a name and a provider")
		case seen[l.Name]:
			return nil, fmt.Errorf("chain: link %q twice", l.Name)
		case l.Region != RegionEU && l.Region != RegionNonEU:
			return nil, fmt.Errorf("chain: link %q: region must be %s or %s", l.Name, RegionEU, RegionNonEU)
		case l.Region == RegionNonEU && !cfg.AllowNonEU:
			return nil, fmt.Errorf("chain: link %q is outside the EU and non-EU providers are not allowed", l.Name)
		}
		seen[l.Name] = true
		c.links = append(c.links, &link{Link: l, br: &breaker{state: StateClosed, threshold: cfg.BreakerFailures, openFor: cfg.BreakerOpen, now: cfg.Now}})
	}
	mp := cfg.Meter
	if mp == nil {
		mp = otel.GetMeterProvider()
	}
	m := mp.Meter(meterName)
	var err error
	if c.fallbacks, err = m.Int64Counter("synthwerk.llm.provider.fallbacks", metric.WithUnit("{fallback}"),
		metric.WithDescription("Answers moved to the next provider before the first token")); err != nil {
		return nil, err
	}
	if c.failures, err = m.Int64Counter("synthwerk.llm.provider.failures", metric.WithUnit("{failure}"),
		metric.WithDescription("Provider failures before the first token")); err != nil {
		return nil, err
	}
	if c.transitions, err = m.Int64Counter("synthwerk.llm.provider.circuit.transitions", metric.WithUnit("{transition}"),
		metric.WithDescription("Circuit breaker state changes per provider")); err != nil {
		return nil, err
	}
	return c, nil
}

// Name implements provider.Provider: the name of the primary.
func (c *Chain) Name() string { return c.links[0].Name }

// State returns the circuit state of the named link, for tests and probes.
func (c *Chain) State(name string) string {
	for _, l := range c.links {
		if l.Name == name {
			return l.br.current()
		}
	}
	return ""
}

// Chat implements provider.Provider. It returns once a link took the request; the
// first Next falls over when that link fails before its first piece.
func (c *Chain) Chat(ctx context.Context, req provider.Request) (provider.Stream, error) {
	s := &stream{c: c, parent: ctx, req: req}
	if req.Alias == "" {
		s.links, s.models = c.links[:1], []string{req.Model}
	} else {
		for _, l := range c.links {
			if m, ok := l.Models[req.Alias]; ok {
				s.links, s.models = append(s.links, l), append(s.models, m)
			}
		}
	}
	if len(s.links) == 0 {
		return nil, fmt.Errorf("%w: no provider serves the alias", provider.ErrUnavailable)
	}
	if err := s.open(ctx, 0); err != nil {
		return nil, err
	}
	return s, nil
}

// classify names the reason of a failure and says whether the chain may fall over.
func classify(err error, timedOut bool) (reason string, eligible bool) {
	if timedOut {
		return ReasonFirstTokenTimeout, true
	}
	if se, ok := errors.AsType[*provider.StatusError](err); ok {
		switch {
		case se.Code == 429:
			return ReasonStatus429, true
		case se.Code >= 500:
			return ReasonStatus5xx, true
		default:
			return ReasonStatus4xx, false
		}
	}
	switch {
	case errors.Is(err, provider.ErrConnect):
		return ReasonConnect, true
	case errors.Is(err, provider.ErrStream):
		return ReasonStream, true
	case errors.Is(err, provider.ErrUnavailable):
		return ReasonUnavailable, true
	default:
		return ReasonOther, false
	}
}

// failed books a failure of l: metric, and the breaker when the chain may fall over.
func (c *Chain) failed(ctx context.Context, l *link, alias, reason string, eligible, probe bool) {
	c.failures.Add(ctx, 1, metric.WithAttributes(
		attribute.String("gen_ai.provider.name", l.Name),
		attribute.String("gen_ai.request.model", alias),
		attribute.String("synthwerk.fallback.reason", reason)))
	out := outcomeNeutral
	if eligible {
		out = outcomeFailure
	}
	c.changed(ctx, l, l.br.result(probe, out))
}

// fellOver logs and counts one move from l to next.
func (c *Chain) fellOver(ctx context.Context, l, next *link, alias, reason string) {
	attrs := []attribute.KeyValue{
		attribute.String("gen_ai.provider.name", l.Name),
		attribute.String("synthwerk.provider.next", next.Name),
		attribute.String("gen_ai.request.model", alias),
		attribute.String("synthwerk.fallback.reason", reason),
	}
	c.fallbacks.Add(ctx, 1, metric.WithAttributes(attrs...))
	c.log.LogAttrs(ctx, slog.LevelWarn, "provider fell over to the next one",
		slog.String("event.name", "llm.provider.fallback"),
		slog.String("gen_ai.provider.name", l.Name),
		slog.String("synthwerk.provider.next", next.Name),
		slog.String("gen_ai.request.model", alias),
		slog.String("synthwerk.fallback.reason", reason))
}

// changed logs and counts a circuit state change. to "": no change.
func (c *Chain) changed(ctx context.Context, l *link, to string) {
	if to == "" {
		return
	}
	c.transitions.Add(ctx, 1, metric.WithAttributes(
		attribute.String("gen_ai.provider.name", l.Name),
		attribute.String("synthwerk.circuit.state", to)))
	level := slog.LevelInfo
	if to == StateOpen {
		level = slog.LevelWarn
	}
	c.log.LogAttrs(ctx, level, "provider circuit changed",
		slog.String("event.name", "llm.provider.circuit."+to),
		slog.String("gen_ai.provider.name", l.Name),
		slog.String("synthwerk.circuit.state", to))
}

// stream is one answer over the chain. Not safe for concurrent use (provider.Stream).
type stream struct {
	c      *Chain
	parent context.Context
	req    provider.Request
	links  []*link
	models []string

	i       int // the current link
	cur     provider.Stream
	cancel  context.CancelCauseFunc
	timer   *time.Timer
	probe   bool
	started bool // the first piece (or the end) came: no fall-over any more
}

// open tries the links from index from until one takes the request. ctx is the
// stream's request context (ctx).
func (s *stream) open(ctx context.Context, from int) error {
	for i := from; i < len(s.links); i++ {
		l := s.links[i]
		last := i == len(s.links)-1
		s.i = i
		probe, to, ok := l.br.allow(last)
		s.c.changed(ctx, l, to)
		if !ok {
			s.c.fellOver(ctx, l, s.links[i+1], s.req.Alias, ReasonCircuitOpen)
			continue
		}
		actx, cancel := context.WithCancelCause(ctx)
		var timer *time.Timer
		if !last {
			timer = time.AfterFunc(s.c.cfg.FirstTokenTimeout, func() { cancel(errFirstToken) })
		}
		req := s.req
		req.Model = s.models[i]
		st, err := l.Provider.Chat(actx, req)
		if err == nil {
			s.cur, s.cancel, s.timer, s.probe = st, cancel, timer, probe
			return nil
		}
		timedOut := fired(timer)
		cancel(nil)
		if ctx.Err() != nil {
			s.c.changed(ctx, l, l.br.result(probe, outcomeNeutral))
			return err
		}
		reason, eligible := classify(err, timedOut)
		s.c.failed(ctx, l, s.req.Alias, reason, eligible, probe)
		if !eligible || last {
			return err
		}
		s.c.fellOver(ctx, l, s.links[i+1], s.req.Alias, reason)
	}
	// Not reached: the last link is always tried and returns above.
	return fmt.Errorf("%w: no provider left", provider.ErrUnavailable)
}

// fired stops t and reports whether it had fired. Nil: false.
func fired(t *time.Timer) bool {
	return t != nil && !t.Stop()
}

// Next implements provider.Stream.
func (s *stream) Next() (string, error) {
	if s.started {
		return s.cur.Next()
	}
	for {
		piece, err := s.cur.Next()
		timedOut := fired(s.timer)
		s.timer = nil
		l := s.links[s.i]
		if (err == nil || errors.Is(err, io.EOF)) && !timedOut {
			s.started = true
			s.c.changed(s.parent, l, l.br.result(s.probe, outcomeSuccess))
			return piece, err
		}
		if s.parent.Err() != nil {
			s.c.changed(s.parent, l, l.br.result(s.probe, outcomeNeutral))
			s.started = true // the client left or the stream timed out: nothing to try
			return "", err
		}
		reason, eligible := classify(err, timedOut)
		s.c.failed(s.parent, l, s.req.Alias, reason, eligible, s.probe)
		if !eligible || s.i == len(s.links)-1 {
			s.started = true
			if err == nil {
				err = provider.ErrUnavailable
			}
			return "", err
		}
		// Nothing went to the client yet: drop this link and take the next one.
		s.c.fellOver(s.parent, l, s.links[s.i+1], s.req.Alias, reason)
		_ = s.cur.Close()
		s.cancel(nil)
		s.cur = nil
		if err := s.open(s.parent, s.i+1); err != nil {
			s.started = true
			return "", err
		}
	}
}

// Result implements provider.Stream: the answering link's result.
func (s *stream) Result() provider.Result {
	if s.cur == nil {
		return provider.Result{}
	}
	return s.cur.Result()
}

// Close implements provider.Stream.
func (s *stream) Close() error {
	fired(s.timer)
	s.timer = nil
	if !s.started && s.cur != nil {
		// Closed before the first piece (client abort): free a probe slot.
		l := s.links[s.i]
		s.c.changed(s.parent, l, l.br.result(s.probe, outcomeNeutral))
		s.started = true
	}
	var err error
	if s.cur != nil {
		err = s.cur.Close()
	}
	if s.cancel != nil {
		s.cancel(nil)
	}
	return err
}

// Answered implements provider.Answered.
func (s *stream) Answered() (name string, taken bool) {
	return s.links[s.i].Name, s.cur != nil
}

type outcome int

const (
	outcomeSuccess outcome = iota
	outcomeFailure
	outcomeNeutral // no verdict on the provider: client abort, 4xx
)

// breaker is the circuit of one link.
type breaker struct {
	threshold int
	openFor   time.Duration
	now       func() time.Time

	mu      sync.Mutex
	state   string
	fails   int
	until   time.Time
	probing bool
}

func (b *breaker) current() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.state
}

// allow says whether the link may be tried now. force: the last link, always tried.
// probe: this call is the one half-open probe. to: the new state, "" when unchanged.
func (b *breaker) allow(force bool) (probe bool, to string, ok bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.state == StateOpen && !b.now().Before(b.until) {
		b.state, to = StateHalfOpen, StateHalfOpen
	}
	switch b.state {
	case StateClosed:
		return false, to, true
	case StateHalfOpen:
		if !b.probing {
			b.probing = true
			return true, to, true
		}
	}
	return false, to, force
}

// result books the outcome of a try. to: the new state, "" when unchanged.
func (b *breaker) result(probe bool, out outcome) (to string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if probe {
		b.probing = false
	}
	switch out {
	case outcomeSuccess:
		b.fails = 0
		if b.state != StateClosed {
			b.state, to = StateClosed, StateClosed
		}
	case outcomeFailure:
		b.fails++
		if b.state == StateHalfOpen || (b.state == StateClosed && b.fails >= b.threshold) {
			b.state, to = StateOpen, StateOpen
			b.until = b.now().Add(b.openFor)
		}
	case outcomeNeutral:
	}
	return to
}
