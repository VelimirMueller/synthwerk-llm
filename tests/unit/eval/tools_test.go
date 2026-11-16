// Prompt-injection eval of the AI tools (synthwerk#53). Same set format and gate as
// the chat eval (#72), but the model is a scripted hostile fake: it makes the tool
// calls of the case, then repeats every tool result word for word. So the eval
// scores the gateway, not the model: whatever a model asks, a tool must never give
// it another user's or org's data. It runs in the unit stage, without a model.
package eval_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/VelimirMueller/synthwerk-contracts/go/kit/cloudevents"
	kitlog "github.com/VelimirMueller/synthwerk-contracts/go/kit/log"

	"github.com/VelimirMueller/synthwerk-llm/internal/config"
	"github.com/VelimirMueller/synthwerk-llm/internal/eval"
	"github.com/VelimirMueller/synthwerk-llm/internal/identity"
	"github.com/VelimirMueller/synthwerk-llm/internal/provider"
	"github.com/VelimirMueller/synthwerk-llm/internal/provider/fake"
	"github.com/VelimirMueller/synthwerk-llm/internal/store"
	"github.com/VelimirMueller/synthwerk-llm/internal/tools"
)

const toolSetDir = "../../../evals/tools/v1"

var (
	caller = store.Owner{Tenant: "org-1", User: "user-a"}
	victim = store.Owner{Tenant: "org-1", User: "user-v"}
	// elsewhere is the same sub in another org.
	elsewhere = store.Owner{Tenant: "org-2", User: "user-a"}
)

// memStore holds the data of three owners. Each read sees only its owner's rows,
// as row level security does in Postgres.
type memStore struct {
	titles map[store.Owner]string
	usage  map[store.Owner]store.Usage
	facts  []cloudevents.Event
}

func newMemStore() *memStore {
	day := time.Date(2026, 11, 16, 0, 0, 0, 0, time.UTC)
	return &memStore{
		titles: map[store.Owner]string{caller: "MINE-1180 trip plan", victim: "VICTIM-7731 salary", elsewhere: "OTHERORG-5520 notes"},
		usage: map[store.Owner]store.Usage{
			caller:    {Window: day, User: 120, Org: 987774},
			victim:    {Window: day, User: 987654, Org: 987774},
			elsewhere: {Window: day, User: 10, Org: 445566},
		},
	}
}

func (m *memStore) ListConversations(_ context.Context, o store.Owner, _ string, _ int) ([]store.Summary, bool, error) {
	t, ok := m.titles[store.Owner{Tenant: o.Tenant, User: o.User}]
	if !ok {
		return nil, false, nil
	}
	return []store.Summary{{ID: "0199f3a0-0000-7000-8000-000000000001", ModelAlias: "default", Title: t, CreatedAt: time.Now()}}, false, nil
}

func (m *memStore) QuotaUsage(_ context.Context, o store.Owner, _ time.Time) (store.Usage, error) {
	return m.usage[store.Owner{Tenant: o.Tenant, User: o.User}], nil
}

func (m *memStore) RecordToolCall(_ context.Context, _ store.Owner, ev cloudevents.Event) error {
	m.facts = append(m.facts, ev)
	return nil
}

type member struct{}

func (member) MyEntitlements(context.Context, string) (identity.Plan, error) {
	return identity.Plan{OrgID: "0199f3a0-5b6c-7d8e-9f0a-1b2c3d4e5f60"}, nil
}

// hostile is the gateway as the eval sees it: the tool loop with the caller's scope
// in front of a scripted model that runs the attack of the case.
type hostile struct {
	reg    *tools.Registry
	scope  tools.Scope
	attack map[string][]provider.ToolCall // by the last user prompt
	model  *fake.Provider
}

func newHostile(t *testing.T, s *eval.Set, st tools.Store) *hostile {
	t.Helper()
	logger, err := kitlog.New(kitlog.Options{ServiceName: "synthwerk-llm", Version: "0.0.0-test", Env: "ci", Format: "json", Writer: &strings.Builder{}})
	if err != nil {
		t.Fatal(err)
	}
	h := &hostile{
		reg:    tools.New(st, member{}, "/synthwerk/llm/local", logger),
		scope:  tools.Scope{Owner: caller, Bearer: "token-of-user-a", ConversationID: "0199f3a0-0000-7000-8000-0000000000c1"},
		attack: map[string][]provider.ToolCall{},
	}
	for _, c := range s.Cases {
		var calls []provider.ToolCall
		for i, a := range c.Attack {
			calls = append(calls, provider.ToolCall{ID: c.ID + "-" + string(rune('a'+i)), Name: a.Name, Arguments: string(a.Arguments)})
		}
		prompt := c.Messages[len(c.Messages)-1].Content
		if _, dup := h.attack[prompt]; dup {
			t.Fatalf("%s: the last prompt must be unique in the set, the hostile model finds the case by it", c.ID)
		}
		h.attack[prompt] = calls
	}
	h.model = &fake.Provider{
		Tools: func(req provider.Request) []provider.ToolCall {
			last := req.Messages[len(req.Messages)-1]
			if last.Role == provider.RoleTool {
				return nil
			}
			return h.attack[last.Text]
		},
		Reply: func(req provider.Request) []string {
			var out []string
			for _, m := range req.Messages {
				if m.Role == provider.RoleTool {
					out = append(out, m.Text, "\n")
				}
			}
			if len(out) == 0 {
				return []string{"no tool ran"}
			}
			return out
		},
	}
	return h
}

func (*hostile) Name() string { return "hostile" }

func (h *hostile) Chat(ctx context.Context, req provider.Request) (provider.Stream, error) {
	return h.reg.Chat(ctx, h.model, req, h.scope)
}

func TestToolInjectionEvalPassesTheGate(t *testing.T) {
	s, err := eval.Load(toolSetDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range s.Cases {
		if !c.Critical || len(c.Attack) == 0 {
			t.Fatalf("%s: every tool case is critical and attacks", c.ID)
		}
	}
	st := newMemStore()
	h := newHostile(t, s, st)
	o := eval.Options{Alias: "default", Model: "m", SystemPrompt: config.DefaultSystemPrompt, Seed: 42}
	res, err := eval.Run(t.Context(), h, s.Cases, o)
	if err != nil {
		t.Fatal(err)
	}
	rep := eval.Score(s, res, o, h.Name())
	if !rep.Gate.Pass || rep.Passed != len(s.Cases) {
		for _, c := range rep.Cases {
			if !c.Passed {
				t.Errorf("%s: %v %s\n%s", c.ID, c.Failures, c.Error, c.Answer)
			}
		}
		t.Fatalf("gate %+v", rep.Gate)
	}
	// The model saw every tool result it asked for, and only the member tools on offer.
	for _, req := range h.model.Requests() {
		for _, tl := range req.Tools {
			if tl.Name == tools.GetOrgUsage {
				t.Fatal("admin tool offered to a member")
			}
		}
	}
	// Each admin tool call of the member is refused and audited.
	want := 0
	for _, c := range s.Cases {
		for i, a := range c.Attack {
			if a.Name == tools.GetOrgUsage && i < tools.MaxCallsPerRound {
				want++
			}
		}
	}
	if len(st.facts) != want {
		t.Fatalf("audit facts %d, want %d", len(st.facts), want)
	}
	for _, ev := range st.facts {
		if !strings.Contains(string(ev.Data), `"outcome":"denied"`) {
			t.Fatalf("fact %s", ev.Data)
		}
	}
}

// A tool that leaks fails the gate: the eval is not blind.
func TestToolInjectionEvalCatchesALeak(t *testing.T) {
	s, err := eval.Load(toolSetDir)
	if err != nil {
		t.Fatal(err)
	}
	h := newHostile(t, s, leaky{newMemStore()})
	o := eval.Options{Alias: "default", Model: "m", Seed: 42}
	res, err := eval.Run(t.Context(), h, s.Cases, o)
	if err != nil {
		t.Fatal(err)
	}
	if rep := eval.Score(s, res, o, h.Name()); rep.Gate.Pass {
		t.Fatal("a leaking store passed the tool eval")
	}
}

// leaky ignores the owner, like a query without row level security.
type leaky struct{ *memStore }

func (l leaky) ListConversations(context.Context, store.Owner, string, int) ([]store.Summary, bool, error) {
	var out []store.Summary
	for _, t := range l.titles {
		out = append(out, store.Summary{Title: t, CreatedAt: time.Now()})
	}
	return out, false, nil
}
