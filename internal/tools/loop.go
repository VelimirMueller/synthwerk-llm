package tools

import (
	"context"
	"errors"
	"io"

	"github.com/VelimirMueller/synthwerk-llm/internal/provider"
)

// MaxRounds bounds the model calls of one answer: up to MaxRounds-1 rounds may ask
// for tools, the last round gets no tools and must answer in text.
const MaxRounds = 3

// MaxCallsPerRound bounds the tool calls the gateway runs for one model round.
// A call above it gets an error result, so every call of the model has an answer.
const MaxCallsPerRound = 4

// Chat runs one answer with tools: it calls p, runs the tool calls the model asks
// for with the scope s, and calls p again with the results, at most MaxRounds times.
// The returned stream yields the text of every round; its Result sums the usage and
// never reports FinishToolCalls. Tool results go to the model only, never to the
// client and never into the stored answer.
func (r *Registry) Chat(ctx context.Context, p provider.Provider, req provider.Request, s Scope) (provider.Stream, error) {
	defs := r.For(s)
	req.Tools = make([]provider.Tool, 0, len(defs))
	for _, d := range defs {
		req.Tools = append(req.Tools, provider.Tool{Name: d.Name, Description: d.Description, Parameters: d.Parameters})
	}
	req.Messages = append([]provider.Message(nil), req.Messages...)
	st, err := p.Chat(ctx, req)
	if err != nil {
		return nil, err
	}
	return &loop{ctx: ctx, r: r, p: p, req: req, s: s, cur: st, round: 1}, nil
}

type loop struct {
	ctx   context.Context
	r     *Registry
	p     provider.Provider
	req   provider.Request
	s     Scope
	cur   provider.Stream
	round int
	usage provider.Usage
	final provider.Result
	done  bool
	// answeredBy remembers the provider of the last round (fallback chain, #56).
	answeredBy string
	taken      bool
}

func (l *loop) Next() (string, error) {
	for {
		if l.done {
			return "", io.EOF
		}
		piece, err := l.cur.Next()
		if !errors.Is(err, io.EOF) {
			return piece, err
		}
		res := l.cur.Result()
		l.usage.InputTokens += res.Usage.InputTokens
		l.usage.OutputTokens += res.Usage.OutputTokens
		l.usage.Estimated = l.usage.Estimated || res.Usage.Estimated
		l.answeredBy, l.taken = provider.AnsweredBy(l.cur, l.p.Name())
		if res.Finish != provider.FinishToolCalls || len(res.ToolCalls) == 0 || l.round >= MaxRounds {
			l.final = provider.Result{Usage: l.usage, Finish: res.Finish}
			if l.final.Finish == provider.FinishToolCalls {
				l.final.Finish = provider.FinishStop
			}
			l.done = true
			continue
		}
		if err := l.next(res.ToolCalls); err != nil {
			l.final = provider.Result{Usage: l.usage, Finish: provider.FinishStop}
			l.done = true
			return "", err
		}
	}
}

// next runs the tool calls and starts the next model round.
func (l *loop) next(calls []provider.ToolCall) error {
	_ = l.cur.Close()
	l.req.Messages = append(l.req.Messages, provider.Message{Role: provider.RoleAssistant, ToolCalls: calls})
	for i, c := range calls {
		out := refusal("too many tool calls in one answer; this call did not run")
		if i < MaxCallsPerRound {
			out = l.r.Call(l.ctx, l.s, c.Name, c.Arguments)
		}
		l.req.Messages = append(l.req.Messages, provider.Message{Role: provider.RoleTool, ToolCallID: c.ID, Text: out})
	}
	l.round++
	if l.round >= MaxRounds {
		l.req.Tools = nil // the last round answers in text
	}
	st, err := l.p.Chat(l.ctx, l.req)
	if err != nil {
		// After the first round the answer already started: a failure ends it.
		return errors.Join(provider.ErrStream, err)
	}
	l.cur = st
	return nil
}

func (l *loop) Result() provider.Result {
	r := l.final
	if r.Finish == "" {
		r.Finish = provider.FinishStop
		r.Usage = l.usage
	}
	return r
}

func (l *loop) Close() error { return l.cur.Close() }

// Answered implements provider.Answered: the provider of the last round.
func (l *loop) Answered() (name string, taken bool) {
	if l.answeredBy == "" {
		return provider.AnsweredBy(l.cur, l.p.Name())
	}
	return l.answeredBy, l.taken
}
