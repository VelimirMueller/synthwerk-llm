//go:build integration

package chat_test

import (
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/VelimirMueller/synthwerk-llm/internal/provider"
	"github.com/VelimirMueller/synthwerk-llm/internal/provider/chain"
	"github.com/VelimirMueller/synthwerk-llm/internal/provider/fake"
	"github.com/VelimirMueller/synthwerk-llm/internal/quota"
	"github.com/VelimirMueller/synthwerk-llm/tests/support/harness"
)

// fallbackChain is a fake primary and a fake fallback "backup" for the harness alias default.
func fallbackChain(t *testing.T, primary, backup *fake.Provider) *chain.Chain {
	t.Helper()
	c, err := chain.New(chain.Config{
		Links: []chain.Link{
			{Name: "primary", Provider: primary, Region: chain.RegionEU, Models: map[string]string{"default": "test-model"}},
			{Name: "backup", Provider: backup, Region: chain.RegionEU, Models: map[string]string{"default": "backup-model"}},
		},
		Log: slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// Send - primary blocked (503) - the fallback streams the answer; event, logs and quota name it (#56).
func TestSendFallsOverToTheFallback(t *testing.T) {
	primary := &fake.Provider{FailChat: true, Status: 503}
	backup := &fake.Provider{Reply: func(provider.Request) []string { return []string{"from", " backup"} }}
	r := newQuotaRig(t, fallbackChain(t, primary, backup), quota.Limits{UserTokens: 1000})
	tok := r.Token(t, "user-a", "org-1")
	res, evs := r.Send(t, tok, r.Create(t, tok), "hi")
	if res.StatusCode != http.StatusOK || !slices.Equal(harness.Names(evs), []string{"delta", "delta", "usage", "done"}) {
		t.Fatalf("status %d events %v", res.StatusCode, harness.Names(evs))
	}
	if primary.Calls() != 1 || backup.Calls() != 1 || backup.Requests()[0].Model != "backup-model" {
		t.Fatalf("calls %d %d", primary.Calls(), backup.Calls())
	}
	waitStatus(t, r, "completed")
	evts := events(t, r)
	if len(evts) != 1 {
		t.Fatalf("events %v", evts)
	}
	data := evts[0]["data"].(map[string]any)
	if data["provider"] != "backup" || data["status"] != "completed" {
		t.Fatalf("event data %v", data)
	}
	// The quota counts the fallback's usage: its input estimate plus "from backup".
	// booking and reserve are the constants of quota_test.go: input = booking - reserve.
	const input = booking - reserve
	want := int64(data["input_tokens"].(float64) + data["output_tokens"].(float64))
	if used, reserved := usage(t, r, "user-a"); used != want || reserved != 0 || want != input+2 {
		t.Fatalf("used %d reserved %d, want %d", used, reserved, want)
	}
	logs := r.Logs.String()
	if !strings.Contains(logs, `"gen_ai.provider.name":"backup"`) || !strings.Contains(logs, `"synthwerk.provider.fallback":true`) {
		t.Fatalf("finish log does not name the fallback: %s", logs)
	}
}

// Send - primary fails after the first token - no fallback, error event, message failed.
func TestSendNoFallbackAfterTheFirstToken(t *testing.T) {
	primary := &fake.Provider{FailAfter: 1, Reply: func(provider.Request) []string { return []string{"a", "b"} }}
	backup := fake.New(0)
	r := newRig(t, fallbackChain(t, primary, backup))
	tok := r.Token(t, "user-a", "org-1")
	res, evs := r.Send(t, tok, r.Create(t, tok), "hi")
	if res.StatusCode != http.StatusOK || !slices.Equal(harness.Names(evs), []string{"delta", "error"}) || backup.Calls() != 0 {
		t.Fatalf("status %d events %v backup calls %d", res.StatusCode, harness.Names(evs), backup.Calls())
	}
	waitStatus(t, r, "failed")
}

// Send - every provider down - 503 model_unavailable, nothing charged.
func TestSendAllProvidersDown(t *testing.T) {
	r := newQuotaRig(t, fallbackChain(t, &fake.Provider{FailChat: true, Status: 503}, &fake.Provider{FailChat: true, Status: 429}), quota.Limits{UserTokens: 1000})
	tok := r.Token(t, "user-a", "org-1")
	if res := send(t, r, tok); res.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status %d", res.StatusCode)
	}
	waitStatus(t, r, "failed")
	if used, reserved := usage(t, r, "user-a"); used != 0 || reserved != 0 {
		t.Fatalf("used %d reserved %d", used, reserved)
	}
}
