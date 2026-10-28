// Package fake is a deterministic provider for tests, CI and the local stack. By
// default it answers "echo: <last user message>" one word per piece, so a test can
// put an owner marker in the prompt and find it in the answer of that owner only.
// It counts and records its calls: a test proves "the model saw no call".
package fake

import (
	"context"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/VelimirMueller/synthwerk-llm/internal/provider"
)

// Name of the adapter.
const Name = "fake"

// Provider is the fake model. Set the fields before the first call.
type Provider struct {
	// Reply returns the answer pieces for a request. Nil: echo the last user message.
	Reply func(req provider.Request) []string
	// Delay before each piece.
	Delay time.Duration
	// FailChat makes Chat fail with this error (wrapped in ErrUnavailable).
	FailChat bool
	// FailAfter makes Next fail after this many pieces. 0: never.
	FailAfter int
	// NoUsage: the stream reports estimated usage, like a provider without numbers.
	NoUsage bool

	calls atomic.Int64
	mu    sync.Mutex
	reqs  []provider.Request
}

// New returns a fake with the echo answer and the given delay per piece.
func New(delay time.Duration) *Provider { return &Provider{Delay: delay} }

// Name implements provider.Provider.
func (*Provider) Name() string { return Name }

// Calls is the number of Chat calls so far.
func (p *Provider) Calls() int { return int(p.calls.Load()) }

// Requests returns a copy of every request so far.
func (p *Provider) Requests() []provider.Request {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]provider.Request(nil), p.reqs...)
}

// Chat implements provider.Provider.
func (p *Provider) Chat(ctx context.Context, req provider.Request) (provider.Stream, error) {
	p.calls.Add(1)
	p.mu.Lock()
	p.reqs = append(p.reqs, req)
	p.mu.Unlock()
	if p.FailChat {
		return nil, provider.ErrUnavailable
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	pieces := Echo(req)
	if p.Reply != nil {
		pieces = p.Reply(req)
	}
	return &stream{ctx: ctx, p: p, pieces: pieces, input: provider.EstimateInput(req.Messages)}, nil
}

// Echo is the default answer: "echo: " and the last user message, one word per piece.
func Echo(req provider.Request) []string {
	last := ""
	for _, m := range req.Messages {
		if m.Role == provider.RoleUser {
			last = m.Text
		}
	}
	words := strings.Fields("echo: " + last)
	out := make([]string, len(words))
	for i, w := range words {
		if i > 0 {
			w = " " + w
		}
		out[i] = w
	}
	return out
}

type stream struct {
	ctx    context.Context
	p      *Provider
	pieces []string
	next   int
	out    int
	input  int
}

func (s *stream) Next() (string, error) {
	if s.next >= len(s.pieces) {
		return "", io.EOF
	}
	if s.p.FailAfter > 0 && s.next >= s.p.FailAfter {
		return "", provider.ErrUnavailable
	}
	if s.p.Delay > 0 {
		t := time.NewTimer(s.p.Delay)
		select {
		case <-s.ctx.Done():
			t.Stop()
			return "", s.ctx.Err()
		case <-t.C:
		}
	} else if err := s.ctx.Err(); err != nil {
		return "", err
	}
	piece := s.pieces[s.next]
	s.next++
	s.out += provider.EstimateTokens(piece)
	return piece, nil
}

func (s *stream) Result() provider.Result {
	return provider.Result{
		Usage:  provider.Usage{InputTokens: s.input, OutputTokens: s.out, Estimated: s.p.NoUsage},
		Finish: provider.FinishStop,
	}
}

func (*stream) Close() error { return nil }
