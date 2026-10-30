//go:build integration

package chat_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/VelimirMueller/synthwerk-contracts/go/kit/auth"

	"github.com/VelimirMueller/synthwerk-llm/internal/identity"
	"github.com/VelimirMueller/synthwerk-llm/internal/provider"
	"github.com/VelimirMueller/synthwerk-llm/internal/provider/fake"
	"github.com/VelimirMueller/synthwerk-llm/internal/quota"
	"github.com/VelimirMueller/synthwerk-llm/internal/store"
	"github.com/VelimirMueller/synthwerk-llm/tests/support/harness"
)

const settingsOrgUUID = "0199f3a0-5b6c-7d8e-9f0a-1b2c3d4e5f60"

// plans is identity for the settings routes: every caller is an active member of the
// free plan (50000 tokens per user per day).
type plans struct{}

func (plans) MyEntitlements(context.Context, string) (identity.Plan, error) {
	return identity.Plan{OrgID: settingsOrgUUID, UserTokensPerDay: 50000}, nil
}

// recorder is a fake provider that keeps the last request.
type recorder struct {
	*fake.Provider
	mu   sync.Mutex
	last provider.Request
}

func newRecorder() *recorder {
	r := &recorder{}
	r.Provider = &fake.Provider{Reply: func(req provider.Request) []string {
		r.mu.Lock()
		defer r.mu.Unlock()
		r.last = req
		return []string{"ok"}
	}}
	return r
}

func (r *recorder) request() provider.Request {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.last
}

func settingsRig(t *testing.T, p provider.Provider, q quota.Source) *rig {
	t.Helper()
	db := env.Database(t)
	st := store.New(db)
	return &rig{H: harness.New(t, st, harness.Options{DB: db, Provider: p, Plans: plans{}, Quota: q}), db: db, st: st}
}

// put saves the AI settings of org-1 as an admin and fails the test on any other answer than 200.
func (r *rig) put(t *testing.T, change map[string]any) {
	t.Helper()
	admin := r.TokenAs(t, "admin-1", "org-1", auth.RoleAdmin)
	res := r.Do(t, http.MethodGet, "/v1/ai-settings", admin, nil)
	var cur map[string]any
	if err := json.NewDecoder(res.Body).Decode(&cur); err != nil || res.StatusCode != http.StatusOK {
		t.Fatalf("get: status %d %v", res.StatusCode, err)
	}
	body := map[string]any{
		"version": cur["version"], "model": cur["model"], "allowed_models": cur["allowed_models"],
		"system_prompt": cur["system_prompt"], "max_input_chars": cur["max_input_chars"],
		"user_tokens_per_day": cur["user_tokens_per_day"],
	}
	for k, v := range change {
		body[k] = v
	}
	if res := r.Do(t, http.MethodPut, "/v1/ai-settings", admin, body); res.StatusCode != http.StatusOK {
		t.Fatalf("put: status %d", res.StatusCode)
	}
}

// Send - org input limit changed to 10 - an 11-character message gets 413, a 10-character one streams (issue "done looks like").
func TestSendOrgInputLimitRefusesLongerPrompt(t *testing.T) {
	r := settingsRig(t, nil, nil)
	tok := r.Token(t, "user-a", "org-1")
	id := r.Create(t, tok)
	if res, _ := r.Send(t, tok, id, strings.Repeat("x", 11)); res.StatusCode != http.StatusOK {
		t.Fatalf("before the change: status %d", res.StatusCode)
	}
	r.put(t, map[string]any{"max_input_chars": 10})
	if res, _ := r.Send(t, tok, id, strings.Repeat("x", 11)); res.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("11 characters: status %d, want 413", res.StatusCode)
	}
	if res, evs := r.Send(t, tok, id, strings.Repeat("x", 10)); res.StatusCode != http.StatusOK || harness.Text(t, evs) == "" {
		t.Fatalf("10 characters: status %d", res.StatusCode)
	}
	// Another org keeps the service limit.
	other := r.Token(t, "user-c", "org-2")
	if res, _ := r.Send(t, other, r.Create(t, other), strings.Repeat("x", 11)); res.StatusCode != http.StatusOK {
		t.Fatalf("org-2: status %d", res.StatusCode)
	}
}

// Send - org system prompt and temperature - the model call carries both.
func TestSendUsesOrgPromptAndTemperature(t *testing.T) {
	rec := newRecorder()
	r := settingsRig(t, rec.Provider, nil)
	r.put(t, map[string]any{"system_prompt": "You sell shoes.\nAnswer in German.", "temperature": 0.25})
	tok := r.Token(t, "user-a", "org-1")
	if res, _ := r.Send(t, tok, r.Create(t, tok), "hello"); res.StatusCode != http.StatusOK {
		t.Fatalf("status %d", res.StatusCode)
	}
	req := rec.request()
	if len(req.Messages) == 0 || req.Messages[0].Role != provider.RoleSystem || req.Messages[0].Text != "You sell shoes.\nAnswer in German." {
		t.Fatalf("messages %+v", req.Messages)
	}
	if req.Options.Temperature == nil || *req.Options.Temperature != 0.25 {
		t.Fatalf("temperature %v", req.Options.Temperature)
	}
}

// Create and Send - a model the org no longer allows - 400; the org default is used without a choice.
func TestOrgModelChoice(t *testing.T) {
	r := settingsRig(t, nil, nil)
	tok := r.Token(t, "user-a", "org-1")
	old := r.Do(t, http.MethodPost, "/v1/conversations", tok, map[string]any{"model": "default"})
	var c struct {
		ID string `json:"id"`
	}
	_ = json.NewDecoder(old.Body).Decode(&c)
	r.put(t, map[string]any{"model": "small", "allowed_models": []string{"small"}})

	if res := r.Do(t, http.MethodPost, "/v1/conversations", tok, map[string]any{"model": "default"}); res.StatusCode != http.StatusBadRequest {
		t.Fatalf("create with a refused model: status %d", res.StatusCode)
	}
	res := r.Do(t, http.MethodPost, "/v1/conversations", tok, map[string]any{})
	var created struct {
		Model string `json:"model"`
	}
	_ = json.NewDecoder(res.Body).Decode(&created)
	if res.StatusCode != http.StatusCreated || created.Model != "small" {
		t.Fatalf("create without a choice: status %d model %q", res.StatusCode, created.Model)
	}
	if res, _ := r.Send(t, tok, c.ID, "hi"); res.StatusCode != http.StatusBadRequest {
		t.Fatalf("send on a conversation with a refused model: status %d", res.StatusCode)
	}
}

// Send - org cap per user below the plan - 429 quota_exhausted with scope user and the cap as limit.
func TestSendOrgCapLowersUserQuota(t *testing.T) {
	r := settingsRig(t, nil, quota.Static{UserTokens: 50000, OrgTokens: 500000})
	r.put(t, map[string]any{"user_tokens_per_day": 1100})
	tok := r.Token(t, "user-a", "org-1")
	res, _ := r.Send(t, tok, r.Create(t, tok), strings.Repeat("word ", 100)) // ~125 tokens + 1024 reserve > 1100
	var p struct {
		Code  string `json:"code"`
		Quota struct {
			Scope string `json:"scope"`
			Limit int64  `json:"limit"`
		} `json:"quota"`
	}
	_ = json.NewDecoder(res.Body).Decode(&p)
	if res.StatusCode != http.StatusTooManyRequests || p.Code != "quota_exhausted" || p.Quota.Scope != "user" || p.Quota.Limit != 1100 {
		t.Fatalf("status %d %+v", res.StatusCode, p)
	}
}

// UpdateAiSettings - a change - row and event in one transaction; the event has no prompt text; RLS hides the row from another org.
func TestUpdateAiSettingsWritesRowAndEvent(t *testing.T) {
	r := settingsRig(t, nil, nil)
	r.put(t, map[string]any{"system_prompt": "PROMPT-CANARY", "max_input_chars": 500})
	evs := events(t, r)
	var found map[string]any
	for _, ev := range evs {
		if ev["type"] == "dev.synthwerk.llm.ai_settings.changed.v1" {
			found = ev
		}
	}
	if found == nil || found["orgid"] != settingsOrgUUID || found["subject"] != settingsOrgUUID || found["actor"] != "user:admin-1" {
		t.Fatalf("event %v", found)
	}
	raw, _ := json.Marshal(found)
	if strings.Contains(string(raw), "PROMPT-CANARY") || strings.Contains(r.Logs.String(), "PROMPT-CANARY") {
		t.Fatal("the system prompt leaked into the event or the logs")
	}
	if s, err := r.st.OrgSettings(t.Context(), store.Owner{Tenant: "org-1", User: "x"}); err != nil || s == nil || s.Version != 1 || s.MaxInputChars != 500 {
		t.Fatalf("org-1: %+v %v", s, err)
	}
	if s, err := r.st.OrgSettings(t.Context(), store.Owner{Tenant: "org-2", User: "x"}); err != nil || s != nil {
		t.Fatalf("org-2 sees %+v %v", s, err)
	}
	// RLS without session settings: nothing.
	var n int
	if err := r.db.QueryRow(t.Context(), `SELECT count(*) FROM llm.org_settings`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("rows without a session: %d %v", n, err)
	}
}
