//go:build integration

// Component tests of the own-data paths (synthwerk#41): GET /v1/me/export and the
// user.deleted.v1 consumer, against Postgres with row level security as a
// non-superuser role and NATS JetStream. Names follow "method - condition - outcome".
package erasure_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/VelimirMueller/synthwerk-contracts/go/kit/cloudevents"
	"github.com/VelimirMueller/synthwerk-contracts/go/llm"

	"github.com/VelimirMueller/synthwerk-llm/internal/erasure"
	"github.com/VelimirMueller/synthwerk-llm/internal/settings"
	"github.com/VelimirMueller/synthwerk-llm/internal/store"
	"github.com/VelimirMueller/synthwerk-llm/tests/support/containers"
	"github.com/VelimirMueller/synthwerk-llm/tests/support/harness"
)

var env *containers.Env

func TestMain(m *testing.M) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	var err error
	env, err = containers.Start(ctx)
	cancel()
	if err != nil {
		fmt.Fprintln(os.Stderr, "start containers:", err)
		os.Exit(1)
	}
	code := m.Run()
	env.Stop()
	os.Exit(code)
}

type rig struct {
	*harness.H
	db *pgxpool.Pool
	st *store.Store
}

func newRig(t *testing.T) *rig {
	t.Helper()
	db := env.Database(t)
	st := store.New(db)
	return &rig{H: harness.New(t, st, harness.Options{DB: db}), db: db, st: st}
}

// admin connects as the superuser to the rig's database (bypasses RLS).
func (r *rig) admin(t *testing.T) *pgx.Conn {
	t.Helper()
	u, err := url.Parse(env.PGURL)
	if err != nil {
		t.Fatal(err)
	}
	u.Path = "/" + r.db.Config().ConnConfig.Database
	c, err := pgx.Connect(t.Context(), u.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close(context.Background()) })
	return c
}

// chat makes a conversation of sub in org with one answered message.
func (r *rig) chat(t *testing.T, sub, org string) string {
	t.Helper()
	tok := r.Token(t, sub, org)
	id := r.Create(t, tok)
	if res, _ := r.Send(t, tok, id, "hello from "+sub); res.StatusCode != http.StatusOK {
		t.Fatalf("send: %d", res.StatusCode)
	}
	return id
}

// rows counts the rows per table as the superuser. owner "" with org: the org quota row.
type counts struct{ conv, msg, quota int }

func (r *rig) count(t *testing.T, owner, org string) counts {
	t.Helper()
	a := r.admin(t)
	var c counts
	q := func(table string, dst *int) {
		sql := `SELECT count(*) FROM llm.` + table + ` WHERE owner_id = $1 AND ($2 = '' OR tenant_id = $2)`
		if err := a.QueryRow(t.Context(), sql, owner, org).Scan(dst); err != nil {
			t.Fatal(err)
		}
	}
	q("conversation", &c.conv)
	q("message", &c.msg)
	q("quota_usage", &c.quota)
	return c
}

// seed: user-a chats in org-1 (twice) and org-2; user-b chats in org-1 and org-2.
// One conversation of user-a is marked deleted.
func seed(t *testing.T, r *rig) (deleted string) {
	t.Helper()
	deleted = r.chat(t, "user-a", "org-1")
	r.chat(t, "user-a", "org-1")
	r.chat(t, "user-a", "org-2")
	r.chat(t, "user-b", "org-1")
	r.chat(t, "user-b", "org-2")
	if _, err := r.admin(t).Exec(t.Context(), `UPDATE llm.conversation SET deleted_at = now() WHERE id = $1`, deleted); err != nil {
		t.Fatal(err)
	}
	return deleted
}

func export(t *testing.T, r *rig, sub, org string) llm.LlmExport {
	t.Helper()
	res := r.Do(t, http.MethodGet, "/v1/me/export", r.Token(t, sub, org), nil)
	raw, err := io.ReadAll(res.Body)
	if err != nil || res.StatusCode != http.StatusOK {
		t.Fatalf("export: %d %v", res.StatusCode, err)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var out llm.LlmExport
	if err := dec.Decode(&out); err != nil {
		t.Fatalf("export does not decode strictly: %v", err)
	}
	for _, other := range []string{"user-a", "user-b"} {
		if other != sub && strings.Contains(string(raw), "hello from "+other) {
			t.Fatal("the export holds the peer's text")
		}
	}
	return out
}

// ExportMe - user with rows in two orgs and a peer in both - every own row, also the
// deleted conversation; no peer row, no org quota row; same result from either org's token.
func TestExportMeReturnsOnlyTheCallersRowsAcrossOrgs(t *testing.T) {
	r := newRig(t)
	deleted := seed(t, r)

	for _, org := range []string{"org-1", "org-2"} {
		got := export(t, r, "user-a", org)
		if len(got.Conversations) != 3 {
			t.Fatalf("token of %s: %d conversations, want 3", org, len(got.Conversations))
		}
		orgs := map[string]int{}
		var sawDeleted bool
		for i, c := range got.Conversations {
			orgs[c.OrgId]++
			sawDeleted = sawDeleted || c.Id.String() == deleted
			if i > 0 && c.CreatedAt.Before(got.Conversations[i-1].CreatedAt) {
				t.Fatal("conversations not oldest first")
			}
			if len(c.Messages) != 2 || c.Messages[0].Role != llm.MessageRoleUser || c.Messages[0].Content != "hello from user-a" {
				t.Fatalf("messages %+v", c.Messages)
			}
			a := c.Messages[1]
			if a.Role != llm.MessageRoleAssistant || a.Status != llm.MessageStatusCompleted || a.TokensIn == nil || a.TokensOut == nil {
				t.Fatalf("answer %+v", a)
			}
			if c.Messages[0].TokensIn != nil {
				t.Fatal("a user message has tokens")
			}
		}
		if orgs["org-1"] != 2 || orgs["org-2"] != 1 || !sawDeleted {
			t.Fatalf("orgs %v, deleted seen %v", orgs, sawDeleted)
		}
		// Own rows only: one per org for today. The org rows (owner '') never.
		if len(got.QuotaUsage) != 2 {
			t.Fatalf("quota rows %+v, want the 2 own rows", got.QuotaUsage)
		}
		var used int64
		for _, q := range got.QuotaUsage {
			used += q.Used
		}
		// Org row of org-1 holds user-a + user-b; the own rows hold user-a only.
		var orgUsed int64
		if err := r.admin(t).QueryRow(t.Context(),
			`SELECT sum(used) FROM llm.quota_usage WHERE owner_id = ''`).Scan(&orgUsed); err != nil {
			t.Fatal(err)
		}
		if used <= 0 || used >= orgUsed {
			t.Fatalf("own used %d, org used %d: the export holds an org row", used, orgUsed)
		}
	}

	// The peer sees only the peer's 2 conversations.
	if got := export(t, r, "user-b", "org-1"); len(got.Conversations) != 2 || len(got.QuotaUsage) != 2 {
		t.Fatalf("peer export %d conversations, %d quota rows", len(got.Conversations), len(got.QuotaUsage))
	}
	// A user with no rows gets two empty lists.
	if got := export(t, r, "user-new", "org-1"); len(got.Conversations) != 0 || len(got.QuotaUsage) != 0 {
		t.Fatalf("empty export %+v", got)
	}
}

// RLS - export setting left over on a pooled connection - the next transaction sees
// no org row and no conversation (a used setting reads ” later, not NULL).
func TestExportSettingDoesNotLeakToTheNextTransaction(t *testing.T) {
	r := newRig(t)
	seed(t, r)
	conn, err := r.db.Acquire(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	for _, gucs := range []string{"export_owner", "erase_owner", "erase_orgs"} {
		tx, err := conn.Begin(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(t.Context(), `SELECT set_config('synthwerk.`+gucs+`', 'user-a', true)`); err != nil {
			t.Fatal(err)
		}
		if err := tx.Commit(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	var setting string
	var n int
	if err := conn.QueryRow(t.Context(),
		`SELECT current_setting('synthwerk.export_owner', true),
		        (SELECT count(*) FROM llm.quota_usage) + (SELECT count(*) FROM llm.conversation) + (SELECT count(*) FROM llm.message)`).Scan(&setting, &n); err != nil {
		t.Fatal(err)
	}
	if setting != "" || n != 0 {
		t.Fatalf("setting %q, %d rows visible without a session", setting, n)
	}
}

// EraseUser - user with rows in two orgs, org-2 closed - every row of the user gone,
// the peer's rows and org-1's org row stay, org-2's org row gone; a repeat deletes nothing.
func TestEraseUserRemovesOwnRowsInAllOrgs(t *testing.T) {
	r := newRig(t)
	seed(t, r)
	// AI settings of both orgs (#39): only the closed org's row goes.
	for _, org := range []string{"org-1", "org-2"} {
		next := settings.Settings{Model: "default", AllowedModels: []string{"default"}, MaxInputChars: 100}
		if _, _, err := r.st.SaveOrgSettings(t.Context(), store.Owner{Tenant: org, User: "user-a"}, next, 0,
			func(*settings.Settings) (*cloudevents.Event, error) {
				ev, err := cloudevents.New(t.Context(), "dev.synthwerk.llm.ai_settings.changed.v1", "/synthwerk/llm/local", map[string]any{})
				return &ev, err
			}); err != nil {
			t.Fatal(err)
		}
	}
	peerBefore := r.count(t, "user-b", "")
	got, err := r.st.EraseUser(t.Context(), "user-a", []string{"org-2"})
	if err != nil {
		t.Fatal(err)
	}
	if got.Conversations != 3 || got.Messages != 6 || got.QuotaRows != 3 || got.SettingsRows != 1 { // 2 own + org-2's org row; org-2 settings
		t.Fatalf("erased %+v", got)
	}
	if s, err := r.st.OrgSettings(t.Context(), store.Owner{Tenant: "org-1", User: "user-b"}); err != nil || s == nil {
		t.Fatalf("org-1 settings: %v %v", s, err)
	}
	if s, err := r.st.OrgSettings(t.Context(), store.Owner{Tenant: "org-2", User: "user-b"}); err != nil || s != nil {
		t.Fatalf("org-2 settings left: %v %v", s, err)
	}
	if c := r.count(t, "user-a", ""); c != (counts{}) {
		t.Fatalf("user-a rows left: %+v", c)
	}
	if c := r.count(t, "user-b", ""); c != peerBefore {
		t.Fatalf("peer rows %+v, want %+v", c, peerBefore)
	}
	if c := r.count(t, "", "org-1"); c.quota != 1 {
		t.Fatalf("org-1 org row: %d, want 1", c.quota)
	}
	if c := r.count(t, "", "org-2"); c.quota != 0 {
		t.Fatalf("org-2 org row: %d, want 0", c.quota)
	}

	again, err := r.st.EraseUser(t.Context(), "user-a", []string{"org-2"})
	if err != nil || again != (store.Erased{}) {
		t.Fatalf("repeat: %+v, %v", again, err)
	}
	if c := r.count(t, "user-b", ""); c != peerBefore {
		t.Fatal("the repeat touched the peer")
	}
	// The peer still works: read and send in org-1.
	r.chat(t, "user-b", "org-1")
}

// EraseUser - no closed orgs - only the user's rows; every org row stays.
func TestEraseUserWithoutClosedOrgsKeepsOrgRows(t *testing.T) {
	r := newRig(t)
	seed(t, r)
	if _, err := r.st.EraseUser(t.Context(), "user-a", nil); err != nil {
		t.Fatal(err)
	}
	if c := r.count(t, "", "org-1"); c.quota != 1 {
		t.Fatal("org-1 org row gone")
	}
	if c := r.count(t, "", "org-2"); c.quota != 1 {
		t.Fatal("org-2 org row gone")
	}
}

// RLS - normal chat session - cannot delete a peer's rows or another org's rows, even
// with a WHERE that names them: the erase policy needs its own setting.
func TestChatSessionCannotDeleteAcrossOwners(t *testing.T) {
	r := newRig(t)
	seed(t, r)
	tx, err := r.db.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if _, err := tx.Exec(t.Context(), `SELECT set_config('synthwerk.tenant_id', 'org-1', true), set_config('synthwerk.owner_id', 'user-a', true)`); err != nil {
		t.Fatal(err)
	}
	tag, err := tx.Exec(t.Context(), `DELETE FROM llm.quota_usage WHERE owner_id = 'user-b' OR tenant_id = 'org-2'`)
	if err != nil {
		t.Fatal(err)
	}
	if tag.RowsAffected() != 0 {
		t.Fatalf("a chat session deleted %d foreign quota rows", tag.RowsAffected())
	}
}

func event(t *testing.T, data string) []byte {
	t.Helper()
	ev := map[string]any{
		"specversion": "1.0", "id": uuid.Must(uuid.NewV7()).String(), "source": "/synthwerk/identity/local",
		"type": erasure.EventType, "subject": "user-a", "time": time.Now().UTC().Format(time.RFC3339),
		"datacontenttype": "application/json", "actor": "user:user-a", "data": json.RawMessage(data),
	}
	b, err := json.Marshal(ev)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// Consumer - EVT_IDENTITY missing at start - waits with WARN, attaches when identity
// creates it; user.deleted erases and acks; a repeat and a malformed event leave
// nothing pending.
func TestConsumerEndToEnd(t *testing.T) {
	r := newRig(t)
	seed(t, r)
	nc := env.NATS(t)
	js, err := jetstream.New(nc)
	if err != nil {
		t.Fatal(err)
	}
	_ = js.DeleteStream(t.Context(), erasure.StreamName) // from an earlier run

	var logs harness.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, nil))
	c := erasure.New(js, r.st, logger, erasure.Config{RetryMin: 100 * time.Millisecond, RetryMax: 300 * time.Millisecond, AckWait: 10 * time.Second})
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- c.Run(ctx) }()
	defer func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("run: %v", err)
		}
	}()

	waitFor(t, "a WARN about the missing stream", func() bool {
		return strings.Contains(logs.String(), `"stream_missing"`) && strings.Contains(logs.String(), `"level":"WARN"`)
	})
	if c.Ready(t.Context()) == nil {
		t.Fatal("ready without the stream")
	}
	if _, err := js.Stream(t.Context(), erasure.StreamName); err == nil {
		t.Fatal("llm created EVT_IDENTITY")
	}

	// Identity creates its stream (as synthwerk-identity does).
	if _, err := js.CreateStream(t.Context(), jetstream.StreamConfig{Name: erasure.StreamName, Subjects: []string{"evt.identity.>"}}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = js.DeleteStream(context.Background(), erasure.StreamName) })
	waitFor(t, "the consumer to attach", func() bool { return c.Ready(t.Context()) == nil })

	good := event(t, `{"user_sub":"user-a","deleted_orgs":[{"org_id":"019a3c20-1a2b-7c3d-8e4f-5a6b7c8d9e0f","zitadel_org_id":"org-2"}]}`)
	for _, b := range [][]byte{good, event(t, `{"user_sub":"user a","deleted_orgs":[]}`), good} {
		if _, err := js.Publish(t.Context(), erasure.Subject, b); err != nil {
			t.Fatal(err)
		}
	}
	waitFor(t, "every event handled", func() bool {
		ci, err := js.Consumer(t.Context(), erasure.StreamName, erasure.Durable)
		if err != nil {
			return false
		}
		info, err := ci.Info(t.Context())
		return err == nil && info.AckFloor.Stream == 3 && info.NumAckPending == 0 && info.NumPending == 0
	})
	if c := r.count(t, "user-a", ""); c != (counts{}) {
		t.Fatalf("user-a rows left: %+v", c)
	}
	if c := r.count(t, "user-b", ""); c.conv != 2 {
		t.Fatalf("peer rows %+v", c)
	}
	if c := r.count(t, "", "org-2"); c.quota != 0 {
		t.Fatal("org-2 org row left")
	}
	if c := r.count(t, "", "org-1"); c.quota != 1 {
		t.Fatal("org-1 org row gone")
	}
	logged := logs.String()
	if strings.Count(logged, `"llm.erasure.done"`) != 2 || !strings.Contains(logged, `"llm.erasure.rejected"`) {
		t.Fatalf("logs: %s", logged)
	}
	if strings.Contains(logged, `user-a`) {
		t.Fatal("the user sub is in the logs")
	}
}

func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(50 * time.Millisecond)
	}
}
