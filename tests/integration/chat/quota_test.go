//go:build integration

package chat_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/VelimirMueller/synthwerk-llm/internal/chat"
	"github.com/VelimirMueller/synthwerk-llm/internal/provider"
	"github.com/VelimirMueller/synthwerk-llm/internal/provider/fake"
	"github.com/VelimirMueller/synthwerk-llm/internal/quota"
	"github.com/VelimirMueller/synthwerk-llm/internal/store"
	"github.com/VelimirMueller/synthwerk-llm/tests/support/harness"
	"github.com/VelimirMueller/synthwerk-llm/tests/support/sse"
)

// Every quota test books 100 tokens for the answer. The harness system prompt
// "You are a test." (3 tokens) and the message "hi" (1 token) make each booking
// 104 tokens; the fake reports the same input estimate as its usage.
const (
	reserve = 100
	booking = 104
)

func newQuotaRig(t *testing.T, p provider.Provider, l quota.Limits) *rig {
	t.Helper()
	db := env.Database(t)
	st := store.New(db)
	h := harness.New(t, st, harness.Options{
		DB: db, Provider: p, Quota: quota.Static(l),
		Chat: func(c *chat.Config) { c.QuotaReserveTokens = reserve },
	})
	return &rig{H: h, db: db, st: st}
}

// usage reads one quota row of org-1 for today as the superuser. owner "" is the org row.
func usage(t *testing.T, r *rig, owner string) (used, reserved int64) {
	t.Helper()
	win, _ := quota.Window(time.Now())
	err := r.admin(t).QueryRow(t.Context(),
		`SELECT used, reserved FROM llm.quota_usage WHERE tenant_id = 'org-1' AND owner_id = $1 AND window_start = $2`,
		owner, win).Scan(&used, &reserved)
	if err != nil {
		t.Fatalf("quota row %q: %v", owner, err)
	}
	return used, reserved
}

type quotaProblem struct {
	Type   string `json:"type"`
	Title  string `json:"title"`
	Status int    `json:"status"`
	Code   string `json:"code"`
	Quota  *struct {
		Scope    string    `json:"scope"`
		Unit     string    `json:"unit"`
		Limit    int64     `json:"limit"`
		ResetsAt time.Time `json:"resets_at"`
	} `json:"quota"`
}

// refused checks a 429 quota_exhausted with the typed body and a Retry-After that
// ends at the reset. It returns the scope.
func refused(t *testing.T, res *http.Response, limit int64) string {
	t.Helper()
	var p quotaProblem
	if err := json.NewDecoder(res.Body).Decode(&p); err != nil {
		t.Fatal(err)
	}
	if res.StatusCode != http.StatusTooManyRequests || p.Code != "quota_exhausted" || p.Status != 429 ||
		p.Type != "https://schemas.synthwerk.dev/problems/quota_exhausted" || p.Title != "Quota exhausted" ||
		!strings.HasPrefix(res.Header.Get("Content-Type"), "application/problem+json") {
		t.Fatalf("status %d problem %+v", res.StatusCode, p)
	}
	_, next := quota.Window(time.Now())
	if p.Quota == nil || p.Quota.Unit != "tokens" || p.Quota.Limit != limit || !p.Quota.ResetsAt.Equal(next) {
		t.Fatalf("quota member %+v, want limit %d resets %s", p.Quota, limit, next)
	}
	ra, err := strconv.Atoi(res.Header.Get("Retry-After"))
	want := int(time.Until(next).Seconds())
	if want < 10 { // the window may have turned between the send and here
		want = ra
	}
	if err != nil || ra < 1 || ra < want-5 || ra > want+5 || ra > 86400 {
		t.Fatalf("Retry-After %q, want about %d", res.Header.Get("Retry-After"), want)
	}
	return p.Quota.Scope
}

// send posts "hi" to a new conversation of tok.
func send(t *testing.T, r *rig, tok string) *http.Response {
	t.Helper()
	return r.Do(t, http.MethodPost, "/v1/conversations/"+r.Create(t, tok)+"/messages", tok, map[string]string{"content": "hi"})
}

// Send - user over quota - 429 quota_exhausted with scope user, no model call, nothing stored (AC 1).
func TestQuotaUserOverQuotaIsRefusedBeforeTheModel(t *testing.T) {
	// One answer of 1000 output tokens: more than the booking, so the second send
	// only fails when the finish counted the real tokens.
	f := &fake.Provider{Reply: func(provider.Request) []string { return []string{strings.Repeat("abcd", 1000)} }}
	const limit = 1100 // fits one booking (104); 1004 used + 104 does not fit
	r := newQuotaRig(t, f, quota.Limits{UserTokens: limit})
	tok := r.Token(t, "user-a", "org-1")

	res := send(t, r, tok)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("first send: status %d", res.StatusCode)
	}
	sse.All(res.Body)
	waitStatus(t, r, "completed")
	if used, reserved := usage(t, r, "user-a"); used != booking-reserve+1000 || reserved != 0 {
		t.Fatalf("after the answer: used %d reserved %d, want %d and 0", used, reserved, booking-reserve+1000)
	}

	var before int
	_ = r.admin(t).QueryRow(t.Context(), `SELECT count(*) FROM llm.message`).Scan(&before)
	res = send(t, r, tok)
	if scope := refused(t, res, limit); scope != "user" {
		t.Fatalf("scope %q", scope)
	}
	if f.Calls() != 1 {
		t.Fatalf("model calls %d, want 1: the refused send reached the model", f.Calls())
	}
	var after int
	_ = r.admin(t).QueryRow(t.Context(), `SELECT count(*) FROM llm.message`).Scan(&after)
	if after != before {
		t.Fatalf("a refused send stored %d messages", after-before)
	}
	if used, reserved := usage(t, r, "user-a"); used != 1004 || reserved != 0 {
		t.Fatalf("a refused send booked: used %d reserved %d", used, reserved)
	}
	if !strings.Contains(r.Logs.String(), `"llm.quota.exhausted"`) {
		t.Fatal("no quota log line")
	}

	// The user quota is per user: user-b in the same org still chats.
	tokB := r.Token(t, "user-b", "org-1")
	if res := send(t, r, tokB); res.StatusCode != http.StatusOK {
		t.Fatalf("user-b: status %d", res.StatusCode)
	}
}

// Send - org over quota - 429 with scope org for another user of the org; other orgs and own rows only (AC 2).
func TestQuotaOrgOverQuotaRefusesEveryUserOfTheOrg(t *testing.T) {
	f := &fake.Provider{Reply: func(provider.Request) []string { return []string{strings.Repeat("abcd", 1000)} }}
	const limit = 1100
	r := newQuotaRig(t, f, quota.Limits{OrgTokens: limit})
	tokA, tokB := r.Token(t, "user-a", "org-1"), r.Token(t, "user-b", "org-1")

	res := send(t, r, tokA)
	sse.All(res.Body)
	waitStatus(t, r, "completed")
	if scope := refused(t, send(t, r, tokB), limit); scope != "org" {
		t.Fatalf("scope %q", scope)
	}
	if f.Calls() != 1 {
		t.Fatalf("model calls %d", f.Calls())
	}
	// Org row and user-a's row count the answer. The refused send rolled back:
	// user-b has no row at all.
	if used, _ := usage(t, r, ""); used != 1004 {
		t.Fatalf("org used %d", used)
	}
	if used, _ := usage(t, r, "user-a"); used != 1004 {
		t.Fatalf("user-a used %d (no user limit, still counted)", used)
	}
	var n int
	_ = r.admin(t).QueryRow(t.Context(), `SELECT count(*) FROM llm.quota_usage WHERE owner_id = 'user-b'`).Scan(&n)
	if n != 0 {
		t.Fatalf("user-b has %d quota rows", n)
	}
	// Another org has its own quota.
	if res := send(t, r, r.Token(t, "user-b", "org-2")); res.StatusCode != http.StatusOK {
		t.Fatalf("org-2: status %d", res.StatusCode)
	}

	// RLS: user-b's transaction sees the org row, never user-a's row.
	tx, err := r.db.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if _, err := tx.Exec(t.Context(), `SELECT set_config('synthwerk.tenant_id', 'org-1', true), set_config('synthwerk.owner_id', 'user-b', true)`); err != nil {
		t.Fatal(err)
	}
	rows, err := tx.Query(t.Context(), `SELECT owner_id FROM llm.quota_usage ORDER BY owner_id`)
	if err != nil {
		t.Fatal(err)
	}
	owners, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil || len(owners) != 1 || owners[0] != "" {
		t.Fatalf("user-b sees quota rows %q, err %v", owners, err)
	}
}

// Send - many users of one org at once - exactly as many streams start as bookings fit.
func TestQuotaConcurrentStreamsNeverBookPastTheLimit(t *testing.T) {
	// Slow pieces keep every admitted stream open while the others book, so only
	// bookings decide. The org fits three bookings and a bit.
	f := &fake.Provider{Delay: 2 * time.Second, Reply: func(provider.Request) []string { return []string{"a", "b"} }}
	const limit = 3*booking + 50
	r := newQuotaRig(t, f, quota.Limits{OrgTokens: limit})

	const users = 8
	toks, convs := make([]string, users), make([]string, users)
	for i := range users {
		toks[i] = r.Token(t, "user-"+strconv.Itoa(i), "org-1")
		convs[i] = r.Create(t, toks[i])
	}
	var wg sync.WaitGroup
	statuses := make([]int, users)
	scopes := make([]string, users)
	start := make(chan struct{})
	for i := range users {
		wg.Go(func() {
			<-start
			res := r.Do(t, http.MethodPost, "/v1/conversations/"+convs[i]+"/messages", toks[i], map[string]string{"content": "hi"})
			statuses[i] = res.StatusCode
			if res.StatusCode == http.StatusTooManyRequests {
				var p quotaProblem
				_ = json.NewDecoder(res.Body).Decode(&p)
				if p.Quota != nil {
					scopes[i] = p.Quota.Scope
				}
				return
			}
			sse.All(res.Body)
		})
	}
	close(start)
	wg.Wait()

	ok, over := 0, 0
	for i, s := range statuses {
		switch s {
		case http.StatusOK:
			ok++
		case http.StatusTooManyRequests:
			over++
			if scopes[i] != "org" {
				t.Errorf("user-%d: scope %q", i, scopes[i])
			}
		default:
			t.Errorf("user-%d: status %d", i, s)
		}
	}
	if ok != 3 || over != users-3 || f.Calls() != 3 {
		t.Fatalf("admitted %d, refused %d, model calls %d; want 3, %d, 3", ok, over, f.Calls(), users-3)
	}
	// Settled: each answer costs its input (4) and two one-token pieces.
	if used, reserved := usage(t, r, ""); used != 3*6 || reserved != 0 {
		t.Fatalf("org used %d reserved %d, want %d and 0", used, reserved, 3*6)
	}
}

// Finish - answer ends early - the booking settles to what was spent.
func TestQuotaSettlesEarlyEnds(t *testing.T) {
	t.Run("provider down - nothing charged", func(t *testing.T) {
		r := newQuotaRig(t, &fake.Provider{FailChat: true}, quota.Limits{UserTokens: 1000})
		tok := r.Token(t, "user-a", "org-1")
		if res := send(t, r, tok); res.StatusCode != http.StatusServiceUnavailable {
			t.Fatalf("status %d", res.StatusCode)
		}
		waitStatus(t, r, "failed")
		if used, reserved := usage(t, r, "user-a"); used != 0 || reserved != 0 {
			t.Fatalf("used %d reserved %d", used, reserved)
		}
	})
	t.Run("client abort - input and partial answer charged", func(t *testing.T) {
		f := &fake.Provider{Delay: 200 * time.Millisecond, Reply: func(provider.Request) []string { return []string{"abcdefgh", " more", " more", " more"} }}
		r := newQuotaRig(t, f, quota.Limits{UserTokens: 1000})
		tok := r.Token(t, "user-a", "org-1")
		ctx, cancel := context.WithCancel(t.Context())
		res := r.DoCtx(ctx, t, http.MethodPost, "/v1/conversations/"+r.Create(t, tok)+"/messages", tok, map[string]string{"content": "hi"})
		if ev := <-sse.Read(res.Body); ev.Event != "delta" {
			t.Fatalf("first event %q", ev.Event)
		}
		cancel()
		waitStatus(t, r, "canceled")
		var content string
		_ = r.admin(t).QueryRow(t.Context(), `SELECT content FROM llm.message WHERE role = 'assistant'`).Scan(&content)
		want := int64(booking-reserve) + int64(provider.EstimateTokens(content))
		if used, reserved := usage(t, r, "user-a"); used != want || reserved != 0 {
			t.Fatalf("used %d reserved %d, want %d and 0 (partial %q)", used, reserved, want, content)
		}
	})
}

// Startup - answer stuck in streaming - sweep charges its full booking.
func TestQuotaSweepSettlesStuckBookings(t *testing.T) {
	r := newQuotaRig(t, nil, quota.Limits{})
	o := store.Owner{Tenant: "org-1", User: "user-a"}
	c, err := r.st.CreateConversation(t.Context(), o, "default")
	if err != nil {
		t.Fatal(err)
	}
	q := store.Quota{Now: time.Now(), Tokens: func([]store.Message) int64 { return 500 }}
	if _, err := r.st.StartTurn(t.Context(), o, c.ID, "hi", q); err != nil {
		t.Fatal(err)
	}
	if used, reserved := usage(t, r, "user-a"); used != 0 || reserved != 500 {
		t.Fatalf("booked: used %d reserved %d", used, reserved)
	}
	time.Sleep(50 * time.Millisecond)
	if n, err := r.st.SweepStreaming(t.Context(), 10*time.Millisecond); err != nil || n != 1 {
		t.Fatalf("swept %d, %v", n, err)
	}
	for _, owner := range []string{"", "user-a"} {
		if used, reserved := usage(t, r, owner); used != 500 || reserved != 0 {
			t.Fatalf("row %q after sweep: used %d reserved %d", owner, used, reserved)
		}
	}
}
