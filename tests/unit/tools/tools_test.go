// AI tools (synthwerk#53): the scope comes from the token, arguments are refused,
// admin calls are checked and audited, and the tool loop is bounded.
package tools_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/VelimirMueller/synthwerk-contracts/go/kit/cloudevents"
	kitlog "github.com/VelimirMueller/synthwerk-contracts/go/kit/log"

	"github.com/VelimirMueller/synthwerk-llm/internal/identity"
	"github.com/VelimirMueller/synthwerk-llm/internal/provider"
	"github.com/VelimirMueller/synthwerk-llm/internal/provider/fake"
	"github.com/VelimirMueller/synthwerk-llm/internal/quota"
	"github.com/VelimirMueller/synthwerk-llm/internal/store"
	"github.com/VelimirMueller/synthwerk-llm/internal/tools"
)

const orgUUID = "0199f3a0-5b6c-7d8e-9f0a-1b2c3d4e5f60"

type memStore struct {
	seen  []store.Owner
	facts []cloudevents.Event
}

func (m *memStore) ListConversations(_ context.Context, o store.Owner, _ string, _ int) ([]store.Summary, bool, error) {
	m.seen = append(m.seen, o)
	return []store.Summary{{Title: "own " + o.User, CreatedAt: time.Date(2026, 11, 16, 9, 0, 0, 0, time.UTC)}}, false, nil
}

func (m *memStore) QuotaUsage(_ context.Context, o store.Owner, _ time.Time) (store.Usage, error) {
	m.seen = append(m.seen, o)
	return store.Usage{Window: time.Date(2026, 11, 16, 0, 0, 0, 0, time.UTC), User: 120, Org: 900}, nil
}

func (m *memStore) RecordToolCall(_ context.Context, _ store.Owner, ev cloudevents.Event) error {
	m.facts = append(m.facts, ev)
	return nil
}

// members answers the identity check and counts the calls.
type members struct {
	err   error
	calls atomic.Int64
}

func (m *members) MyEntitlements(context.Context, string) (identity.Plan, error) {
	m.calls.Add(1)
	if m.err != nil {
		return identity.Plan{}, m.err
	}
	return identity.Plan{OrgID: orgUUID}, nil
}

func registry(t *testing.T, st *memStore, m tools.Members) (*tools.Registry, *strings.Builder) {
	t.Helper()
	logs := &strings.Builder{}
	logger, err := kitlog.New(kitlog.Options{ServiceName: "synthwerk-llm", Version: "0.0.0-test", Env: "ci", Format: "json", Writer: logs})
	if err != nil {
		t.Fatal(err)
	}
	return tools.New(st, m, "/synthwerk/llm/local", logger), logs
}

var (
	member = tools.Scope{Owner: store.Owner{Tenant: "org-1", User: "user-a"}, Bearer: "tok", ConversationID: "0199f3a0-0000-7000-8000-0000000000c1", Limits: quota.Limits{UserTokens: 5000, OrgTokens: 90000}}
	admin  = func() tools.Scope { s := member; s.Admin = true; return s }()
	widget = func() tools.Scope { s := admin; s.Owner.Origin = "https://shop.example"; return s }()
)

func TestForOffersAdminToolsToAdminsOnly(t *testing.T) {
	r, _ := registry(t, &memStore{}, &members{})
	names := func(s tools.Scope) (out []string) {
		for _, d := range r.For(s) {
			out = append(out, d.Name)
		}
		return out
	}
	if got := strings.Join(names(member), ","); got != "llm_list_conversations,llm_get_my_quota" {
		t.Fatalf("member %s", got)
	}
	if got := strings.Join(names(admin), ","); got != "llm_list_conversations,llm_get_my_quota,llm_get_org_usage" {
		t.Fatalf("admin %s", got)
	}
}

func TestUserToolsReadTheTokenOwnerAndRefuseArguments(t *testing.T) {
	st := &memStore{}
	r, logs := registry(t, st, &members{})
	for _, args := range []string{"", "null", "{}", " { } "} {
		if out := r.Call(t.Context(), member, tools.ListConversations, args); !strings.Contains(out, "own user-a") {
			t.Fatalf("args %q: %s", args, out)
		}
	}
	for _, args := range []string{`{"user_id":"user-v"}`, `["user-v"]`, `"user-v"`, `{"org_id":null}`, `not json`} {
		out := r.Call(t.Context(), member, tools.GetMyQuota, args)
		if !strings.Contains(out, "takes no arguments") {
			t.Fatalf("args %q: %s", args, out)
		}
	}
	for _, o := range st.seen {
		if o != member.Owner {
			t.Fatalf("store read for %+v", o)
		}
	}
	var q map[string]any
	if err := json.Unmarshal([]byte(r.Call(t.Context(), member, tools.GetMyQuota, "{}")), &q); err != nil ||
		q["used_tokens"] != 120.0 || q["limit_tokens"] != 5000.0 || q["window"] != "2026-11-16" {
		t.Fatalf("quota %v %v", q, err)
	}
	if out := r.Call(t.Context(), member, "llm_drop_tables", "{}"); !strings.Contains(out, "unknown tool") {
		t.Fatalf("unknown: %s", out)
	}
	if strings.Contains(logs.String(), "user-v") {
		t.Fatal("logs hold tool arguments")
	}
	if len(st.facts) != 0 {
		t.Fatalf("user tools wrote %d facts", len(st.facts))
	}
}

func TestAdminToolCallsAreCheckedAndAudited(t *testing.T) {
	tests := []struct {
		name      string
		scope     tools.Scope
		identity  error
		args      string
		wantOut   string
		outcome   string // "": no fact
		identityN int64
	}{
		{"admin", admin, nil, "{}", `"used_tokens":900`, tools.OutcomeOK, 1},
		{"admin with arguments", admin, nil, `{"org_id":"org-2"}`, "takes no arguments", tools.OutcomeDenied, 1},
		{"member", member, nil, "{}", "owners and admins only", tools.OutcomeDenied, 1},
		{"identity refuses", admin, identity.ErrDenied, "{}", "owners and admins only", "", 1},
		{"identity down", admin, errors.New("down"), "{}", "not available now", "", 1},
		{"widget", widget, nil, "{}", "owners and admins only", "", 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			st, m := &memStore{}, &members{err: tc.identity}
			r, _ := registry(t, st, m)
			out := r.Call(t.Context(), tc.scope, tools.GetOrgUsage, tc.args)
			if !strings.Contains(out, tc.wantOut) {
				t.Fatalf("out %s", out)
			}
			if m.calls.Load() != tc.identityN {
				t.Fatalf("identity calls %d, want %d", m.calls.Load(), tc.identityN)
			}
			if tc.outcome == "" {
				if len(st.facts) != 0 {
					t.Fatalf("facts %d, want 0", len(st.facts))
				}
				return
			}
			if len(st.facts) != 1 {
				t.Fatalf("facts %d, want 1", len(st.facts))
			}
			ev := st.facts[0]
			var data map[string]any
			_ = json.Unmarshal(ev.Data, &data)
			if ev.Type != tools.EventType || ev.OrgID != orgUUID || ev.Subject != tc.scope.ConversationID || ev.Actor != "user:user-a" ||
				data["outcome"] != tc.outcome || data["org_id"] != orgUUID || data["tool"] != tools.GetOrgUsage || data["scope"] != "org" {
				t.Fatalf("fact %+v data %v", ev, data)
			}
			if len(data) != 6 {
				t.Fatalf("data keys %v", data)
			}
		})
	}
	t.Run("no identity configured", func(t *testing.T) {
		st := &memStore{}
		r, _ := registry(t, st, nil)
		if out := r.Call(t.Context(), admin, tools.GetOrgUsage, "{}"); !strings.Contains(out, "not available now") || len(st.facts) != 0 {
			t.Fatalf("out %s facts %d", out, len(st.facts))
		}
	})
}

func drain(t *testing.T, st provider.Stream) string {
	t.Helper()
	var b strings.Builder
	for {
		p, err := st.Next()
		if errors.Is(err, io.EOF) {
			return b.String()
		}
		if err != nil {
			t.Fatal(err)
		}
		b.WriteString(p)
	}
}

// A model that asks for tools forever gets MaxRounds calls; the last one has no
// tools. Usage adds up, and the finish is never tool_calls.
func TestLoopIsBounded(t *testing.T) {
	r, _ := registry(t, &memStore{}, &members{})
	calls := make([]provider.ToolCall, 6)
	for i := range calls {
		calls[i] = provider.ToolCall{ID: "c", Name: tools.ListConversations, Arguments: "{}"}
	}
	p := &fake.Provider{
		Tools: func(req provider.Request) []provider.ToolCall {
			if len(req.Tools) == 0 {
				return nil
			}
			return calls
		},
		Reply: func(provider.Request) []string { return []string{"done"} },
	}
	st, err := r.Chat(t.Context(), p, provider.Request{Messages: []provider.Message{{Role: provider.RoleUser, Text: "hi"}}}, member)
	if err != nil {
		t.Fatal(err)
	}
	if got := drain(t, st); got != "done" {
		t.Fatalf("answer %q", got)
	}
	reqs := p.Requests()
	if len(reqs) != tools.MaxRounds || len(reqs[len(reqs)-1].Tools) != 0 || len(reqs[0].Tools) != 2 {
		t.Fatalf("rounds %d", len(reqs))
	}
	// Each tool round runs at most 4 calls.
	toolTurns := 0
	for _, m := range reqs[1].Messages {
		if m.Role == provider.RoleTool {
			toolTurns++
		}
	}
	if toolTurns != 4 {
		t.Fatalf("tool turns %d, want 4", toolTurns)
	}
	res := st.Result()
	if res.Finish == provider.FinishToolCalls || res.Usage.InputTokens == 0 || res.Usage.OutputTokens == 0 {
		t.Fatalf("result %+v", res)
	}
	if name, _ := provider.AnsweredBy(st, "x"); name != fake.Name {
		t.Fatalf("answered by %q", name)
	}
	_ = st.Close()
}
