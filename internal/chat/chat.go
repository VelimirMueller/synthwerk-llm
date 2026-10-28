// Package chat serves the chat contract (synthwerk-contracts openapi/llm): create and
// read a conversation, send a message and stream the answer as SSE.
//
// Refusal order of a send (spec #32 §3.3). The model call is the last step:
//  1. auth.Middleware: no valid token → 401 before this package runs.
//  2. origin claim differs from the Origin header → 403.
//  3. body not valid → 400; conversation not the caller's → 404; input too long → 413.
//  4. stream slot of the owner → 429; quota guard → 429 (no-op in M2).
//  5. store the turn, wait for a model slot (503), call the provider.
//
// Logs carry metadata only: never the prompt, the answer or provider error text.
package chat

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/VelimirMueller/synthwerk-contracts/go/kit/cloudevents"

	"github.com/VelimirMueller/synthwerk-llm/internal/provider"
	"github.com/VelimirMueller/synthwerk-llm/internal/quota"
	"github.com/VelimirMueller/synthwerk-llm/internal/store"
)

// EventType is the fact the gateway sends for every finished answer.
const EventType = "dev.synthwerk.llm.message.completed.v1"

const dataSchema = "https://schemas.synthwerk.dev/events/llm/message.completed.v1.json"

// Store is the persistence the handler needs. *store.Store implements it.
type Store interface {
	CreateConversation(ctx context.Context, o store.Owner, alias string) (store.Conversation, error)
	GetConversation(ctx context.Context, o store.Owner, id string) (store.Conversation, error)
	ConversationAlias(ctx context.Context, o store.Owner, id string) (string, error)
	StartTurn(ctx context.Context, o store.Owner, convID, text string) (store.Turn, error)
	Finish(ctx context.Context, o store.Owner, f store.Final, ev cloudevents.Event) error
}

// Config of the handler.
type Config struct {
	// Aliases maps a model alias to the provider model name.
	Aliases map[string]string
	// DefaultAlias for a new conversation without a choice.
	DefaultAlias string
	// SystemPrompt goes first in every model call. Empty: none.
	SystemPrompt string
	// MaxInputChars, MaxStreamsPerOwner, MaxContextChars: see package config.
	MaxInputChars      int
	MaxStreamsPerOwner int
	MaxContextChars    int
	// StreamTimeout bounds one answer.
	StreamTimeout time.Duration
	// ProviderConcurrency and QueueTimeout: model slots and the wait for one.
	ProviderConcurrency int
	QueueTimeout        time.Duration
	// Heartbeat is the SSE ping interval. Default 15 s.
	Heartbeat time.Duration
	// Source is the CloudEvents source, /synthwerk/llm/<env>.
	Source string
}

// Handler serves the chat routes.
type Handler struct {
	cfg      Config
	store    Store
	provider provider.Provider
	guard    quota.Guard
	log      *slog.Logger

	slots chan struct{}
	mu    sync.Mutex
	open  map[store.Owner]int
}

// New returns the handler. guard nil: quota.None.
func New(cfg Config, st Store, p provider.Provider, guard quota.Guard, logger *slog.Logger) *Handler {
	if guard == nil {
		guard = quota.None{}
	}
	if cfg.Heartbeat <= 0 {
		cfg.Heartbeat = 15 * time.Second
	}
	if cfg.ProviderConcurrency <= 0 {
		cfg.ProviderConcurrency = 1
	}
	if cfg.MaxStreamsPerOwner <= 0 {
		cfg.MaxStreamsPerOwner = 2
	}
	if cfg.StreamTimeout <= 0 {
		cfg.StreamTimeout = 120 * time.Second
	}
	if cfg.MaxContextChars <= 0 {
		cfg.MaxContextChars = 24000
	}
	if cfg.QueueTimeout <= 0 {
		cfg.QueueTimeout = 10 * time.Second
	}
	return &Handler{
		cfg: cfg, store: st, provider: p, guard: guard, log: logger,
		slots: make(chan struct{}, cfg.ProviderConcurrency),
		open:  map[store.Owner]int{},
	}
}

// acquireStream takes one stream slot of the owner. ok false: the owner is at the max.
func (h *Handler) acquireStream(o store.Owner) (release func(), ok bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.open[o] >= h.cfg.MaxStreamsPerOwner {
		return nil, false
	}
	h.open[o]++
	return func() {
		h.mu.Lock()
		defer h.mu.Unlock()
		if h.open[o]--; h.open[o] <= 0 {
			delete(h.open, o)
		}
	}, true
}

// acquireModel waits for a model slot up to the queue timeout.
func (h *Handler) acquireModel(ctx context.Context) (release func(), ok bool) {
	t := time.NewTimer(h.cfg.QueueTimeout)
	defer t.Stop()
	select {
	case h.slots <- struct{}{}:
		return func() { <-h.slots }, true
	case <-t.C:
		return nil, false
	case <-ctx.Done():
		return nil, false
	}
}

// history turns stored messages into model messages: the system prompt, then the
// newest turns that fit MaxContextChars, then the new user text. It stops at the
// first turn that does not fit, so the model never sees a history with a gap.
func (h *Handler) history(past []store.Message, text string) []provider.Message {
	budget := max(0, h.cfg.MaxContextChars-len([]rune(text)))
	var keep []provider.Message
	for i := len(past) - 1; i >= 0; i-- {
		m := past[i]
		if m.Content == "" {
			continue // an answer that failed before its first piece
		}
		n := len([]rune(m.Content))
		if n > budget {
			break
		}
		budget -= n
		keep = append(keep, provider.Message{Role: provider.Role(m.Role), Text: m.Content})
	}
	out := make([]provider.Message, 0, len(keep)+2)
	if h.cfg.SystemPrompt != "" {
		out = append(out, provider.Message{Role: provider.RoleSystem, Text: h.cfg.SystemPrompt})
	}
	for i := len(keep) - 1; i >= 0; i-- {
		out = append(out, keep[i])
	}
	return append(out, provider.Message{Role: provider.RoleUser, Text: text})
}
