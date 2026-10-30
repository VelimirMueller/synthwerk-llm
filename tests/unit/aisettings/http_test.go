package aisettings_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/VelimirMueller/synthwerk-contracts/go/kit/auth"
	"github.com/VelimirMueller/synthwerk-contracts/go/kit/auth/authtest"
	"github.com/VelimirMueller/synthwerk-contracts/go/kit/cloudevents"
	"github.com/VelimirMueller/synthwerk-contracts/go/llm"

	"github.com/VelimirMueller/synthwerk-llm/internal/identity"
	"github.com/VelimirMueller/synthwerk-llm/internal/settings"
	"github.com/VelimirMueller/synthwerk-llm/internal/store"
	"github.com/VelimirMueller/synthwerk-llm/tests/support/harness"
)

const (
	org      = "396370208765350663"
	orgUUID  = "0199f3a0-5b6c-7d8e-9f0a-1b2c3d4e5f60"
	path     = "/v1/ai-settings"
	secretPr = "SECRET-PROMPT-CANARY"
)

// memStore keeps one org's settings in memory and records the events. The chat
// methods fail the test: the settings routes never reach them.
type memStore struct {
	t      *testing.T
	mu     sync.Mutex
	row    *settings.Settings
	events []cloudevents.Event
	calls  int
	fail   bool
}

func (m *memStore) chat() { m.t.Error("a chat store method was called"); panic("chat store") }

func (m *memStore) CreateConversation(context.Context, store.Owner, string) (store.Conversation, error) {
	m.chat()
	return store.Conversation{}, nil
}
func (m *memStore) GetConversation(context.Context, store.Owner, string) (store.Conversation, error) {
	m.chat()
	return store.Conversation{}, nil
}
func (m *memStore) ConversationAlias(context.Context, store.Owner, string) (string, error) {
	m.chat()
	return "", nil
}
func (m *memStore) StartTurn(context.Context, store.Owner, string, string, store.Quota) (store.Turn, error) {
	m.chat()
	return store.Turn{}, nil
}
func (m *memStore) Finish(context.Context, store.Owner, store.Final, cloudevents.Event) error {
	m.chat()
	return nil
}
func (m *memStore) Export(context.Context, string, store.ExportSink) error { m.chat(); return nil }

func (m *memStore) OrgSettings(_ context.Context, o store.Owner) (*settings.Settings, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls++
	if m.fail {
		return nil, errors.New("db down")
	}
	if o.Tenant != org {
		m.t.Errorf("tenant %q, want the token org", o.Tenant)
	}
	if m.row == nil {
		return nil, nil
	}
	c := *m.row
	return &c, nil
}

func (m *memStore) SaveOrgSettings(_ context.Context, _ store.Owner, next settings.Settings, expect int64, build store.SettingsChange) (settings.Settings, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls++
	var v int64
	if m.row != nil {
		v = m.row.Version
	}
	if v != expect {
		return settings.Settings{}, false, store.ErrVersionConflict
	}
	ev, err := build(m.row, v+1)
	if err != nil {
		return settings.Settings{}, false, err
	}
	if ev == nil {
		if m.row == nil {
			return settings.Settings{}, false, nil
		}
		return *m.row, false, nil
	}
	next.Version, next.UpdatedAt = v+1, time.Date(2026, 10, 30, 10, 0, 0, 0, time.UTC)
	m.row = &next
	m.events = append(m.events, *ev)
	return next, true, nil
}

type fakePlans struct {
	plan   identity.Plan
	err    error
	bearer string
	calls  int
}

func (f *fakePlans) MyEntitlements(_ context.Context, bearer string) (identity.Plan, error) {
	f.calls++
	f.bearer = bearer
	return f.plan, f.err
}

func rig(t *testing.T, plans *fakePlans) (*harness.H, *memStore) {
	t.Helper()
	st := &memStore{t: t}
	var opts harness.Options
	if plans != nil {
		opts.Plans = plans
	}
	return harness.New(t, st, opts), st
}

func okPlans() *fakePlans {
	return &fakePlans{plan: identity.Plan{OrgID: orgUUID, UserTokensPerDay: 50000}}
}

func body() map[string]any {
	return map[string]any{
		"version": 0, "model": "small", "allowed_models": []string{"small", "default"},
		"system_prompt": secretPr + "\nAnswer briefly.", "max_input_chars": 100,
		"user_tokens_per_day": 20000, "temperature": 0.3,
	}
}

func problemCode(t *testing.T, res *http.Response) string {
	t.Helper()
	var p struct {
		Code   string `json:"code"`
		Detail string `json:"detail"`
	}
	if err := json.NewDecoder(res.Body).Decode(&p); err != nil {
		t.Fatal(err)
	}
	return p.Code + "|" + p.Detail
}

// GetAiSettings / UpdateAiSettings - caller is not an owner or admin - 403, no identity and no store call.
func TestRefusedCallers(t *testing.T) {
	plans := okPlans()
	h, st := rig(t, plans)
	widget := authtest.Claims("user-a", org, auth.RoleAdmin)
	widget[auth.ClaimOrigin] = "https://shop.example"
	cases := map[string]string{
		"member":   h.TokenAs(t, "user-a", org, auth.RoleMember),
		"operator": h.TokenAs(t, "user-a", org, auth.RoleOperator),
		"widget":   h.Issuer.Mint(t, widget),
		// Widget tokens of identity (#30): never an admin token, with a role claim or not.
		"identity widget": h.WidgetToken(t, "user-a", org, "https://shop.example"),
		"identity widget with admin role": h.WidgetToken(t, "user-a", org, "https://shop.example",
			map[string]any{auth.ClaimRoles: authtest.Roles(org, auth.RoleAdmin, auth.RoleOwner)}),
	}
	for name, tok := range cases {
		for _, method := range []string{http.MethodGet, http.MethodPut} {
			t.Run(method+" - "+name+" token - 403 forbidden, no identity call, no store call", func(t *testing.T) {
				res := h.Do(t, method, path, tok, body(), "Origin", "https://shop.example")
				if res.StatusCode != http.StatusForbidden || !strings.HasPrefix(problemCode(t, res), "forbidden|") {
					t.Fatalf("status %d", res.StatusCode)
				}
			})
		}
	}
	t.Run("GET - no token - 401", func(t *testing.T) {
		if res := h.Do(t, http.MethodGet, path, "", nil); res.StatusCode != http.StatusUnauthorized {
			t.Fatalf("status %d", res.StatusCode)
		}
	})
	if plans.calls != 0 || st.calls != 0 {
		t.Fatalf("identity calls %d, store calls %d, want 0", plans.calls, st.calls)
	}
}

// GET - identity answers - org, membership and plan come from identity with the caller's token.
func TestGet(t *testing.T) {
	t.Run("GetAiSettings - org never saved - defaults with version 0 and the bounds", func(t *testing.T) {
		plans := okPlans()
		h, _ := rig(t, plans)
		tok := h.TokenAs(t, "user-a", org, auth.RoleAdmin)
		res := h.Do(t, http.MethodGet, path, tok, nil)
		if res.StatusCode != http.StatusOK || res.Header.Get("Cache-Control") != "no-store" {
			t.Fatalf("status %d", res.StatusCode)
		}
		var s llm.AiSettings
		if err := json.NewDecoder(res.Body).Decode(&s); err != nil {
			t.Fatal(err)
		}
		if s.Version != 0 || s.UpdatedAt != nil || s.Model != "default" || !slices.Equal(s.AllowedModels, []string{"default", "small"}) ||
			s.MaxInputChars != 8000 || s.SystemPrompt != "" || s.Temperature != nil {
			t.Fatalf("got %+v", s)
		}
		b := s.Bounds
		if !slices.Equal(b.Models, []string{"default", "small"}) || b.MaxInputChars != 8000 || b.UserTokensPerDay != 50000 ||
			b.SystemPromptMaxChars != 4000 || b.TemperatureMax != 2 {
			t.Fatalf("bounds %+v", b)
		}
		if plans.bearer != tok {
			t.Fatal("identity did not get the caller's token")
		}
	})
	for name, c := range map[string]struct {
		plans  *fakePlans
		status int
		code   string
	}{
		"identity not configured": {nil, http.StatusServiceUnavailable, "settings_unavailable"},
		"identity down":           {&fakePlans{err: identity.ErrUnavailable}, http.StatusServiceUnavailable, "settings_unavailable"},
		"not an active member":    {&fakePlans{err: identity.ErrDenied}, http.StatusForbidden, "forbidden"},
	} {
		t.Run("GetAiSettings - "+name+" - "+c.code+", no store call", func(t *testing.T) {
			h, st := rig(t, c.plans)
			res := h.Do(t, http.MethodGet, path, h.TokenAs(t, "user-a", org, auth.RoleOwner), nil)
			if res.StatusCode != c.status || !strings.HasPrefix(problemCode(t, res), c.code+"|") || st.calls != 0 {
				t.Fatalf("status %d, store calls %d", res.StatusCode, st.calls)
			}
		})
	}
	t.Run("GetAiSettings - store fails - 500", func(t *testing.T) {
		h, st := rig(t, okPlans())
		st.fail = true
		if res := h.Do(t, http.MethodGet, path, h.TokenAs(t, "user-a", org, auth.RoleOwner), nil); res.StatusCode != http.StatusInternalServerError {
			t.Fatalf("status %d", res.StatusCode)
		}
	})
}

// PUT - a change - stored, version 1, one event with the full state and no prompt text.
func TestPut(t *testing.T) {
	t.Run("UpdateAiSettings - valid change - 200 version 1, one event without prompt text", func(t *testing.T) {
		h, st := rig(t, okPlans())
		tok := h.TokenAs(t, "user-a", org, auth.RoleAdmin)
		res := h.Do(t, http.MethodPut, path, tok, body())
		if res.StatusCode != http.StatusOK {
			t.Fatalf("status %d %s", res.StatusCode, problemCode(t, res))
		}
		var s llm.AiSettings
		if err := json.NewDecoder(res.Body).Decode(&s); err != nil {
			t.Fatal(err)
		}
		if s.Version != 1 || s.UpdatedAt == nil || s.Model != "small" || s.MaxInputChars != 100 || s.Temperature == nil || *s.Temperature != 0.3 {
			t.Fatalf("got %+v", s)
		}
		if len(st.events) != 1 {
			t.Fatalf("events %d", len(st.events))
		}
		ev := st.events[0]
		raw, _ := json.Marshal(ev)
		if strings.Contains(string(raw), secretPr) || strings.Contains(h.Logs.String(), secretPr) {
			t.Fatal("the system prompt leaked into the event or the logs")
		}
		if ev.Type != "dev.synthwerk.llm.ai_settings.changed.v1" || ev.Subject != orgUUID || ev.OrgID != orgUUID || ev.Actor != "user:user-a" {
			t.Fatalf("envelope %+v", ev)
		}
		var d map[string]any
		if err := json.Unmarshal(ev.Data, &d); err != nil {
			t.Fatal(err)
		}
		if d["org_id"] != orgUUID || d["version"] != float64(1) || d["model"] != "small" || d["max_input_chars"] != float64(100) ||
			d["system_prompt_chars"] != float64(len([]rune(secretPr+"\nAnswer briefly."))) || d["temperature"] != 0.3 ||
			strings.Join(toStrings(d["changed"]), ",") != "model,system_prompt,max_input_chars,user_tokens_per_day,temperature" { // allowed_models equals the defaults
			t.Fatalf("data %v", d)
		}
	})
	t.Run("UpdateAiSettings - same values again - 200, same version, no second event", func(t *testing.T) {
		h, st := rig(t, okPlans())
		tok := h.TokenAs(t, "user-a", org, auth.RoleOwner)
		h.Do(t, http.MethodPut, path, tok, body())
		b := body()
		b["version"] = 1
		res := h.Do(t, http.MethodPut, path, tok, b)
		var s llm.AiSettings
		_ = json.NewDecoder(res.Body).Decode(&s)
		if res.StatusCode != http.StatusOK || s.Version != 1 || len(st.events) != 1 {
			t.Fatalf("status %d version %d events %d", res.StatusCode, s.Version, len(st.events))
		}
	})
	t.Run("UpdateAiSettings - the defaults on a new org - 200 version 0, no event", func(t *testing.T) {
		h, st := rig(t, okPlans())
		res := h.Do(t, http.MethodPut, path, h.TokenAs(t, "user-a", org, auth.RoleOwner), map[string]any{
			"version": 0, "model": "default", "allowed_models": []string{"default", "small"}, "system_prompt": "",
			"max_input_chars": 8000, "user_tokens_per_day": 0,
		})
		var s llm.AiSettings
		_ = json.NewDecoder(res.Body).Decode(&s)
		if res.StatusCode != http.StatusOK || s.Version != 0 || len(st.events) != 0 {
			t.Fatalf("status %d version %d events %d", res.StatusCode, s.Version, len(st.events))
		}
	})
	t.Run("UpdateAiSettings - stale version - 409 version_conflict, nothing changed", func(t *testing.T) {
		h, st := rig(t, okPlans())
		tok := h.TokenAs(t, "user-a", org, auth.RoleOwner)
		h.Do(t, http.MethodPut, path, tok, body())
		b := body()
		b["max_input_chars"] = 50
		res := h.Do(t, http.MethodPut, path, tok, b) // still version 0
		if res.StatusCode != http.StatusConflict || !strings.HasPrefix(problemCode(t, res), "version_conflict|") || st.row.MaxInputChars != 100 {
			t.Fatalf("status %d", res.StatusCode)
		}
	})

	bad := map[string]struct {
		edit   func(map[string]any)
		detail string
	}{
		"model outside the service allow-list": {func(b map[string]any) { b["model"] = "gpt-4o" }, "model:"},
		"allowed model the service lacks":      {func(b map[string]any) { b["allowed_models"] = []string{"small", "llama"} }, "allowed_models:"},
		"cap above the plan":                   {func(b map[string]any) { b["user_tokens_per_day"] = 50001 }, "user_tokens_per_day:"},
		"control character in the prompt":      {func(b map[string]any) { b["system_prompt"] = "a\u0007b" }, "system_prompt:"},
		"input limit above the service":        {func(b map[string]any) { b["max_input_chars"] = 8001 }, "max_input_chars:"},
		"temperature above 2":                  {func(b map[string]any) { b["temperature"] = 2.5 }, "temperature:"},
	}
	for name, c := range bad {
		t.Run("UpdateAiSettings - "+name+" - 400 names the field, no store call", func(t *testing.T) {
			h, st := rig(t, okPlans())
			b := body()
			c.edit(b)
			res := h.Do(t, http.MethodPut, path, h.TokenAs(t, "user-a", org, auth.RoleOwner), b)
			if got := problemCode(t, res); res.StatusCode != http.StatusBadRequest || !strings.HasPrefix(got, "invalid_request|"+c.detail) || st.calls != 0 {
				t.Fatalf("status %d %q store calls %d", res.StatusCode, got, st.calls)
			}
		})
	}
	shape := map[string]func(map[string]any){
		"unknown field":         func(b map[string]any) { b["provider"] = "openai" },
		"missing system_prompt": func(b map[string]any) { delete(b, "system_prompt") },
		"negative version":      func(b map[string]any) { b["version"] = -1 },
	}
	for name, edit := range shape {
		t.Run("UpdateAiSettings - "+name+" - 400 before identity", func(t *testing.T) {
			plans := okPlans()
			h, _ := rig(t, plans)
			b := body()
			edit(b)
			res := h.Do(t, http.MethodPut, path, h.TokenAs(t, "user-a", org, auth.RoleOwner), b)
			if res.StatusCode != http.StatusBadRequest || plans.calls != 0 {
				t.Fatalf("status %d identity calls %d", res.StatusCode, plans.calls)
			}
		})
	}
}

func toStrings(v any) []string {
	var out []string
	for _, x := range v.([]any) {
		out = append(out, x.(string))
	}
	return out
}
