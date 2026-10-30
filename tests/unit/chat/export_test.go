package chat_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/VelimirMueller/synthwerk-contracts/go/kit/auth"
	"github.com/VelimirMueller/synthwerk-contracts/go/kit/auth/authtest"
	"github.com/VelimirMueller/synthwerk-contracts/go/llm"

	"github.com/VelimirMueller/synthwerk-llm/internal/store"
	"github.com/VelimirMueller/synthwerk-llm/tests/support/harness"
)

// exportStore serves Export from fixed rows; the other methods are not called.
type exportStore struct {
	panicStore
	user  string // the sub Export got
	convs []store.ExportConversation
	msgs  map[string][]store.ExportMessage
	quota []store.ExportQuota
	// failAt > 0 returns errFail before the n-th sink call. failAt = -1 fails before any.
	failAt int
}

var errFail = errors.New("db down")

func (s *exportStore) Export(_ context.Context, user string, sink store.ExportSink) error {
	s.user = user
	calls := 0
	step := func() error {
		calls++
		if s.failAt == -1 || (s.failAt > 0 && calls >= s.failAt) {
			return errFail
		}
		return nil
	}
	if s.failAt == -1 {
		return step()
	}
	for _, c := range s.convs {
		if err := step(); err != nil {
			return err
		}
		if err := sink.Conversation(c); err != nil {
			return err
		}
		for _, m := range s.msgs[c.ID] {
			if err := step(); err != nil {
				return err
			}
			if err := sink.Message(m); err != nil {
				return err
			}
		}
	}
	for _, q := range s.quota {
		if err := step(); err != nil {
			return err
		}
		if err := sink.Quota(q); err != nil {
			return err
		}
	}
	return nil
}

func ptr[T any](v T) *T { return &v }

func fixture(t *testing.T) *exportStore {
	t0 := time.Date(2026, 10, 23, 9, 0, 0, 0, time.UTC)
	return &exportStore{
		panicStore: panicStore{t},
		convs: []store.ExportConversation{
			{ID: "6f1c2d3e-4a5b-4c6d-8e7f-9a0b1c2d3e4f", Tenant: "org-1", ModelAlias: "default", CreatedAt: t0},
			{ID: "0199f3b0-0000-7c3d-8e4f-5a6b7c8d9e00", Tenant: "org-2", ModelAlias: "small", CreatedAt: t0.Add(time.Hour)},
		},
		msgs: map[string][]store.ExportMessage{
			"6f1c2d3e-4a5b-4c6d-8e7f-9a0b1c2d3e4f": {
				{Message: store.Message{ID: "0199f3b0-1a2b-7c3d-8e4f-5a6b7c8d9e0f", Role: "user", Content: "What does pulse check?", Status: "completed", CreatedAt: t0.Add(5 * time.Second)}},
				{Message: store.Message{ID: "0199f3b0-1a2c-7c3d-8e4f-5a6b7c8d9e10", Role: "assistant", Content: "Pulse reads /readyz.", Status: "completed", CreatedAt: t0.Add(6 * time.Second)}, TokensIn: ptr[int32](12), TokensOut: ptr[int32](9)},
			},
		},
		quota: []store.ExportQuota{
			{Tenant: "org-1", WindowStart: t0.Truncate(24 * time.Hour), Used: 21},
			{Tenant: "org-2", WindowStart: t0.Truncate(24 * time.Hour), Used: 0},
		},
	}
}

func decodeExport(t *testing.T, body io.Reader) llm.LlmExport {
	t.Helper()
	raw, err := io.ReadAll(body)
	if err != nil {
		t.Fatal(err)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var out llm.LlmExport
	if err := dec.Decode(&out); err != nil {
		t.Fatalf("body does not decode strictly to LlmExport: %v\n%s", err, raw)
	}
	if out.Conversations == nil || out.QuotaUsage == nil {
		t.Fatalf("a required list is null: %s", raw)
	}
	return out
}

// ExportMe - user token - 200 LlmExport with every row in order, owner = token sub.
func TestExportMeReturnsTheContractShape(t *testing.T) {
	st := fixture(t)
	h := harness.New(t, st, harness.Options{})
	res := h.Do(t, http.MethodGet, "/v1/me/export", h.Token(t, "user-a", "org-1"), nil)
	if res.StatusCode != http.StatusOK || res.Header.Get("Content-Type") != "application/json" || res.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("status %d headers %v", res.StatusCode, res.Header)
	}
	got := decodeExport(t, res.Body)
	if st.user != "user-a" {
		t.Fatalf("store got user %q, want the token sub", st.user)
	}
	if len(got.Conversations) != 2 || got.Conversations[0].OrgId != "org-1" || got.Conversations[1].OrgId != "org-2" {
		t.Fatalf("conversations %+v", got.Conversations)
	}
	c0, c1 := got.Conversations[0], got.Conversations[1]
	if c0.Model != "default" || len(c0.Messages) != 2 || c1.Messages == nil || len(c1.Messages) != 0 {
		t.Fatalf("messages %+v / %+v", c0, c1)
	}
	if m := c0.Messages[0]; m.Role != llm.MessageRoleUser || m.TokensIn != nil || m.TokensOut != nil || m.Content != "What does pulse check?" {
		t.Fatalf("user message %+v", m)
	}
	if m := c0.Messages[1]; *m.TokensIn != 12 || *m.TokensOut != 9 || m.Status != llm.MessageStatusCompleted {
		t.Fatalf("assistant message %+v", m)
	}
	if len(got.QuotaUsage) != 2 || got.QuotaUsage[0].Used != 21 || got.QuotaUsage[0].WindowStart.String() != "2026-10-23" {
		t.Fatalf("quota %+v", got.QuotaUsage)
	}
}

// ExportMe - user without rows - 200 with two empty lists.
func TestExportMeEmpty(t *testing.T) {
	h := harness.New(t, &exportStore{panicStore: panicStore{t}}, harness.Options{})
	res := h.Do(t, http.MethodGet, "/v1/me/export", h.Token(t, "user-a", "org-1"), nil)
	raw, _ := io.ReadAll(res.Body)
	if res.StatusCode != http.StatusOK || strings.TrimSpace(string(raw)) != `{"conversations":[],"quota_usage":[]}` {
		t.Fatalf("status %d body %s", res.StatusCode, raw)
	}
}

// ExportMe - widget token (synthwerk:origin) - 403 forbidden, no store call.
func TestExportMeWidgetTokenIsForbidden(t *testing.T) {
	h := harness.New(t, panicStore{t}, harness.Options{})
	claims := authtest.Claims("user-a", "org-1", auth.RoleMember)
	claims[auth.ClaimOrigin] = "https://shop.example"
	tok := h.Issuer.Mint(t, claims)
	// Also with the matching Origin: the export is never for a widget.
	res := h.Do(t, http.MethodGet, "/v1/me/export", tok, nil, "Origin", "https://shop.example")
	if res.StatusCode != http.StatusForbidden {
		t.Fatalf("status %d, want 403", res.StatusCode)
	}
	var p struct {
		Code string `json:"code"`
	}
	_ = json.NewDecoder(res.Body).Decode(&p)
	if p.Code != "forbidden" {
		t.Fatalf("code %q", p.Code)
	}
}

// ExportMe - store fails before the first row - 500 problem, no partial body.
func TestExportMeStoreErrorBeforeFirstRow(t *testing.T) {
	st := fixture(t)
	st.failAt = -1
	h := harness.New(t, st, harness.Options{})
	res := h.Do(t, http.MethodGet, "/v1/me/export", h.Token(t, "user-a", "org-1"), nil)
	if res.StatusCode != http.StatusInternalServerError || res.Header.Get("Content-Type") != "application/problem+json" {
		t.Fatalf("status %d", res.StatusCode)
	}
	if !strings.Contains(h.Logs.String(), `"llm.export.failed"`) {
		t.Fatal("no llm.export.failed log line")
	}
}

// tooLarge reports ErrExportTooLarge before any row.
type tooLarge struct{ panicStore }

func (tooLarge) Export(context.Context, string, store.ExportSink) error {
	return store.ErrExportTooLarge
}

// ExportMe - more rows than the contract allows - 500 before the first byte, own log event.
func TestExportMeTooLarge(t *testing.T) {
	h := harness.New(t, tooLarge{panicStore{t}}, harness.Options{})
	res := h.Do(t, http.MethodGet, "/v1/me/export", h.Token(t, "user-a", "org-1"), nil)
	if res.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status %d", res.StatusCode)
	}
	if !strings.Contains(h.Logs.String(), `"llm.export.too_large"`) {
		t.Fatal("no llm.export.too_large log line")
	}
}

// ExportMe - store fails after the first row - connection aborted, never valid JSON.
func TestExportMeStoreErrorMidStreamAborts(t *testing.T) {
	st := fixture(t)
	st.failAt = 3 // after the first conversation and its first message
	h := harness.New(t, st, harness.Options{})
	// Not h.Do: the transport error is the expected outcome.
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, h.Server.URL+"/v1/me/export", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+h.Token(t, "user-a", "org-1"))
	res, err := h.Server.Client().Do(req)
	if err == nil {
		defer func() { _ = res.Body.Close() }()
		raw, rerr := io.ReadAll(res.Body)
		if rerr == nil && json.Valid(raw) {
			t.Fatalf("a failed export ended as valid JSON: %s", raw)
		}
	}
	if !strings.Contains(h.Logs.String(), `"llm.export.aborted"`) {
		t.Fatal("no llm.export.aborted log line")
	}
	if strings.Contains(h.Logs.String(), "What does pulse check") {
		t.Fatal("content in the logs")
	}
}
