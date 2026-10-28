//go:build integration

// Component tests of the chat slice: the real HTTP stack, Postgres with row level
// security as a non-superuser role, NATS JetStream, the fake provider and the fake
// token issuer (kit/auth/authtest). Names follow "method - condition - outcome".
package chat_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nats-io/nats.go/jetstream"
	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/VelimirMueller/synthwerk-contracts/go/kit/cloudevents"
	"github.com/VelimirMueller/synthwerk-contracts/go/kit/outbox"

	"github.com/VelimirMueller/synthwerk-llm/internal/app"
	"github.com/VelimirMueller/synthwerk-llm/internal/provider"
	"github.com/VelimirMueller/synthwerk-llm/internal/provider/fake"
	"github.com/VelimirMueller/synthwerk-llm/internal/store"
	"github.com/VelimirMueller/synthwerk-llm/tests/support/containers"
	"github.com/VelimirMueller/synthwerk-llm/tests/support/harness"
	"github.com/VelimirMueller/synthwerk-llm/tests/support/sse"
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

func newRig(t *testing.T, p provider.Provider) *rig {
	t.Helper()
	db := env.Database(t)
	st := store.New(db)
	return &rig{H: harness.New(t, st, harness.Options{DB: db, Provider: p}), db: db, st: st}
}

// admin runs a query as the superuser on the rig's database (bypasses RLS).
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

func events(t *testing.T, r *rig) []map[string]any {
	t.Helper()
	rows, err := r.admin(t).Query(t.Context(), `SELECT event FROM llm.synthwerk_outbox ORDER BY created_at`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []map[string]any
	for rows.Next() {
		var ev map[string]any
		if err := rows.Scan(&ev); err != nil {
			t.Fatal(err)
		}
		out = append(out, ev)
	}
	return out
}

// Send - fake answers 5 pieces with a delay - 5 deltas arrive one by one, then usage and done (AC 1).
func TestSendStreamsTokenByToken(t *testing.T) {
	f := &fake.Provider{Delay: 150 * time.Millisecond, Reply: func(provider.Request) []string { return []string{"a", "b", "c", "d", "e"} }}
	r := newRig(t, f)
	tok := r.Token(t, "user-a", "org-1")
	id := r.Create(t, tok)
	res, evs := r.Send(t, tok, id, "stream please")
	if res.StatusCode != http.StatusOK || res.Header.Get("Content-Type") != "text/event-stream" || res.Header.Get("X-Accel-Buffering") != "no" {
		t.Fatalf("status %d headers %v", res.StatusCode, res.Header)
	}
	if got := harness.Names(evs); !slices.Equal(got, []string{"delta", "delta", "delta", "delta", "delta", "usage", "done"}) {
		t.Fatalf("events %v", got)
	}
	if gap := evs[4].At.Sub(evs[0].At); gap < 400*time.Millisecond {
		t.Fatalf("deltas arrived within %s: the stream is buffered", gap)
	}
	for i, ev := range evs {
		if !strings.HasSuffix(ev.ID, fmt.Sprintf(":%d", i+1)) {
			t.Errorf("event %d id %q", i, ev.ID)
		}
	}
	var done struct {
		MessageID    string `json:"message_id"`
		FinishReason string `json:"finish_reason"`
	}
	_ = json.Unmarshal([]byte(evs[6].Data), &done)
	if done.FinishReason != "stop" || !strings.HasPrefix(evs[6].ID, done.MessageID+":") {
		t.Fatalf("done %+v id %q", done, evs[6].ID)
	}
	// Stored and readable.
	c := r.Do(t, http.MethodGet, "/v1/conversations/"+id, tok, nil)
	var conv struct {
		Messages []struct{ Role, Content, Status string }
	}
	_ = json.NewDecoder(c.Body).Decode(&conv)
	if len(conv.Messages) != 2 || conv.Messages[1].Content != "abcde" || conv.Messages[1].Status != "completed" || conv.Messages[0].Content != "stream please" {
		t.Fatalf("conversation %+v", conv)
	}
}

// Send - two users at once - streams never mix, other owner's id is 404 (AC 3).
func TestSendIsolatesUsers(t *testing.T) {
	f := fake.New(20 * time.Millisecond)
	r := newRig(t, f)
	tokA, tokB := r.Token(t, "user-a", "org-1"), r.Token(t, "user-b", "org-1")
	idA, idB := r.Create(t, tokA), r.Create(t, tokB)

	var wg sync.WaitGroup
	texts := map[string]string{}
	var mu sync.Mutex
	for _, u := range []struct{ tok, id, marker string }{{tokA, idA, "MARK-A"}, {tokB, idB, "MARK-B"}} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res, evs := r.Send(t, u.tok, u.id, "one two three "+u.marker)
			if res.StatusCode != http.StatusOK {
				t.Errorf("%s: status %d", u.marker, res.StatusCode)
			}
			mu.Lock()
			texts[u.marker] = harness.Text(t, evs)
			mu.Unlock()
		}()
	}
	wg.Wait()
	if !strings.Contains(texts["MARK-A"], "MARK-A") || strings.Contains(texts["MARK-A"], "MARK-B") ||
		!strings.Contains(texts["MARK-B"], "MARK-B") || strings.Contains(texts["MARK-B"], "MARK-A") {
		t.Fatalf("streams mixed: %v", texts)
	}

	// "exit" is plain content: it ends nothing and shares nothing.
	if res, _ := r.Send(t, tokA, idA, "exit"); res.StatusCode != http.StatusOK {
		t.Fatalf("exit: status %d", res.StatusCode)
	}
	reqs := f.Requests()
	last := reqs[len(reqs)-1]
	for _, m := range last.Messages {
		if strings.Contains(m.Text, "MARK-B") {
			t.Fatalf("A's model call holds B's history: %+v", last.Messages)
		}
	}
	if !slices.ContainsFunc(last.Messages, func(m provider.Message) bool { return strings.Contains(m.Text, "MARK-A") }) {
		t.Fatalf("A's history is missing: %+v", last.Messages)
	}

	unknown := "0199f3b0-1a2c-7c3d-8e4f-5a6b7c8d9e10"
	body := func(res *http.Response) string {
		var p map[string]any
		_ = json.NewDecoder(res.Body).Decode(&p)
		delete(p, "trace_id")
		raw, _ := json.Marshal(p)
		return string(raw)
	}
	other := r.Do(t, http.MethodGet, "/v1/conversations/"+idA, tokB, nil)
	none := r.Do(t, http.MethodGet, "/v1/conversations/"+unknown, tokB, nil)
	if other.StatusCode != http.StatusNotFound || none.StatusCode != http.StatusNotFound || body(other) != body(none) {
		t.Fatalf("other owner %d, unknown %d", other.StatusCode, none.StatusCode)
	}
	calls := f.Calls()
	if res, _ := r.Send(t, tokB, idA, "let me in"); res.StatusCode != http.StatusNotFound {
		t.Fatalf("send to A's conversation as B: status %d", res.StatusCode)
	}
	sameSubOtherOrg := r.Token(t, "user-a", "org-2")
	if res := r.Do(t, http.MethodGet, "/v1/conversations/"+idA, sameSubOtherOrg, nil); res.StatusCode != http.StatusNotFound {
		t.Fatalf("same sub, other org: status %d", res.StatusCode)
	}
	if f.Calls() != calls {
		t.Fatal("a refused send reached the model")
	}
}

// RawQuery - no session vars - zero rows (RLS as the service role).
func TestRawQueryWithoutSessionSeesNothing(t *testing.T) {
	r := newRig(t, nil)
	tok := r.Token(t, "user-a", "org-1")
	id := r.Create(t, tok)
	r.Send(t, tok, id, "hello")
	var n int
	if err := r.db.QueryRow(t.Context(), `SELECT count(*) FROM llm.message`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("service role without session: %d rows, err %v", n, err)
	}
	if err := r.admin(t).QueryRow(t.Context(), `SELECT count(*) FROM llm.message`).Scan(&n); err != nil || n != 2 {
		t.Fatalf("admin: %d rows, err %v", n, err)
	}
}

// Send - logs and spans - no prompt or answer text on any path (AC 4).
func TestLogsHoldNoPromptOrAnswer(t *testing.T) {
	rec := tracetest.NewSpanRecorder()
	prev := otel.GetTracerProvider()
	otel.SetTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec)))
	t.Cleanup(func() { otel.SetTracerProvider(prev) })

	const prompt, answerCanary = "PROMPT-CANARY-91c2", "ANSWER-CANARY-5d7e"
	paths := map[string]*fake.Provider{
		"success":        {Reply: func(provider.Request) []string { return []string{"x ", answerCanary} }},
		"provider down":  {FailChat: true},
		"mid-stream":     {FailAfter: 1, Reply: func(provider.Request) []string { return []string{answerCanary, " more"} }},
		"client aborted": {Delay: 300 * time.Millisecond, Reply: func(provider.Request) []string { return []string{answerCanary, " a", " b", " c"} }},
	}
	var logs []string
	for name, f := range paths {
		r := newRig(t, f)
		tok := r.Token(t, "user-a", "org-1")
		id := r.Create(t, tok)
		if name == "client aborted" {
			ctx, cancel := context.WithCancel(t.Context())
			res := r.DoCtx(ctx, t, http.MethodPost, "/v1/conversations/"+id+"/messages", tok, map[string]string{"content": prompt})
			ch := sse.Read(res.Body)
			<-ch // first delta
			cancel()
			waitStatus(t, r, "canceled")
		} else {
			r.Send(t, tok, id, prompt)
		}
		r.Do(t, http.MethodPost, "/v1/conversations/"+id+"/messages", "", map[string]string{"content": prompt})
		r.Do(t, http.MethodPost, "/v1/conversations/"+id+"/messages", "bad.token.x", map[string]string{"content": prompt})
		// The finish log line comes after the stream ends.
		deadline := time.Now().Add(5 * time.Second)
		for !strings.Contains(r.Logs.String(), `"llm.message.finished"`) && time.Now().Before(deadline) {
			time.Sleep(20 * time.Millisecond)
		}
		logs = append(logs, r.Logs.String())
		if !strings.Contains(r.Logs.String(), `"llm.message.finished"`) {
			t.Errorf("%s: no finish log line", name)
		}
	}
	all := strings.Join(logs, "\n")
	for _, c := range []string{prompt, answerCanary} {
		if strings.Contains(all, c) {
			t.Fatalf("log holds %s", c)
		}
	}
	for _, s := range rec.Ended() {
		dump := s.Name()
		for _, a := range s.Attributes() {
			dump += " " + string(a.Key) + "=" + a.Value.String()
		}
		for _, e := range s.Events() {
			dump += " " + e.Name
			for _, a := range e.Attributes {
				dump += " " + a.Value.String()
			}
		}
		if strings.Contains(dump, prompt) || strings.Contains(dump, answerCanary) {
			t.Fatalf("span holds content: %s", dump)
		}
	}
	if len(rec.Ended()) == 0 {
		t.Fatal("no spans recorded")
	}
}

func waitStatus(t *testing.T, r *rig, status string) {
	t.Helper()
	for range 50 {
		var n int
		if err := r.admin(t).QueryRow(t.Context(), `SELECT count(*) FROM llm.message WHERE role = 'assistant' AND status = $1`, status).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n > 0 {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("no assistant message with status %s", status)
}

// Send - client aborts mid-stream - message canceled, partial content kept, event canceled.
func TestSendClientAbortKeepsPartialAnswer(t *testing.T) {
	f := &fake.Provider{Delay: 200 * time.Millisecond, Reply: func(provider.Request) []string { return []string{"part", " two", " three", " four"} }}
	r := newRig(t, f)
	tok := r.Token(t, "user-a", "org-1")
	id := r.Create(t, tok)
	ctx, cancel := context.WithCancel(t.Context())
	res := r.DoCtx(ctx, t, http.MethodPost, "/v1/conversations/"+id+"/messages", tok, map[string]string{"content": "go"})
	ch := sse.Read(res.Body)
	if ev := <-ch; ev.Event != "delta" {
		t.Fatalf("first event %q", ev.Event)
	}
	cancel()
	waitStatus(t, r, "canceled")
	var content string
	_ = r.admin(t).QueryRow(t.Context(), `SELECT content FROM llm.message WHERE role = 'assistant'`).Scan(&content)
	if !strings.HasPrefix(content, "part") || strings.Contains(content, "four") {
		t.Fatalf("content %q", content)
	}
	evs := events(t, r)
	if len(evs) != 1 || evs[0]["data"].(map[string]any)["status"] != "canceled" {
		t.Fatalf("events %v", evs)
	}
}

// Send - limits - 413 above the input limit, 429 for a third stream; no model call for either.
func TestSendLimits(t *testing.T) {
	f := &fake.Provider{Delay: time.Second, Reply: func(provider.Request) []string { return []string{"slow", " answer"} }}
	r := newRig(t, f)
	tok := r.Token(t, "user-a", "org-1")
	id := r.Create(t, tok)

	res := r.Do(t, http.MethodPost, "/v1/conversations/"+id+"/messages", tok, map[string]string{"content": strings.Repeat("ä", 8001)})
	if res.StatusCode != http.StatusRequestEntityTooLarge || f.Calls() != 0 {
		t.Fatalf("too long: status %d, calls %d", res.StatusCode, f.Calls())
	}
	if res := r.Do(t, http.MethodPost, "/v1/conversations/"+id+"/messages", tok, map[string]string{"content": ""}); res.StatusCode != http.StatusBadRequest {
		t.Fatalf("empty: status %d", res.StatusCode)
	}
	if res := r.Do(t, http.MethodPost, "/v1/conversations/"+id+"/messages", tok, map[string]any{"content": "x", "owner_id": "user-b"}); res.StatusCode != http.StatusBadRequest {
		t.Fatalf("owner in body: status %d", res.StatusCode)
	}

	for range 2 {
		res := r.Do(t, http.MethodPost, "/v1/conversations/"+id+"/messages", tok, map[string]string{"content": "x"})
		if res.StatusCode != http.StatusOK {
			t.Fatalf("stream: status %d", res.StatusCode)
		}
	}
	res = r.Do(t, http.MethodPost, "/v1/conversations/"+id+"/messages", tok, map[string]string{"content": "x"})
	var p struct{ Code string }
	_ = json.NewDecoder(res.Body).Decode(&p)
	if res.StatusCode != http.StatusTooManyRequests || p.Code != "too_many_streams" || f.Calls() != 2 {
		t.Fatalf("third stream: status %d code %q calls %d", res.StatusCode, p.Code, f.Calls())
	}
	// Another user is not limited by user-a's streams.
	tokB := r.Token(t, "user-b", "org-1")
	if res := r.Do(t, http.MethodPost, "/v1/conversations/"+r.Create(t, tokB)+"/messages", tokB, map[string]string{"content": "x"}); res.StatusCode != http.StatusOK {
		t.Fatalf("user-b: status %d", res.StatusCode)
	}
}

// Send - provider down before the first token - 503 model_unavailable, message failed, event failed.
func TestSendProviderDown(t *testing.T) {
	r := newRig(t, &fake.Provider{FailChat: true})
	tok := r.Token(t, "user-a", "org-1")
	res, _ := r.Send(t, tok, r.Create(t, tok), "hi")
	var p struct{ Code string }
	_ = json.NewDecoder(res.Body).Decode(&p)
	if res.StatusCode != http.StatusServiceUnavailable || p.Code != "model_unavailable" || res.Header.Get("Retry-After") == "" {
		t.Fatalf("status %d code %q", res.StatusCode, p.Code)
	}
	waitStatus(t, r, "failed")
	if evs := events(t, r); len(evs) != 1 || evs[0]["data"].(map[string]any)["status"] != "failed" {
		t.Fatalf("events %v", evs)
	}
}

// Send - provider fails mid-stream - error event, status stays 200, message failed.
func TestSendMidStreamFailure(t *testing.T) {
	r := newRig(t, &fake.Provider{FailAfter: 1, Reply: func(provider.Request) []string { return []string{"a", "b"} }})
	tok := r.Token(t, "user-a", "org-1")
	res, evs := r.Send(t, tok, r.Create(t, tok), "hi")
	if res.StatusCode != http.StatusOK || !slices.Equal(harness.Names(evs), []string{"delta", "error"}) {
		t.Fatalf("status %d events %v", res.StatusCode, harness.Names(evs))
	}
	if !strings.Contains(evs[1].Data, `"code":"model_unavailable"`) {
		t.Fatalf("error data %s", evs[1].Data)
	}
	waitStatus(t, r, "failed")
}

// Relay - completed message - event on EVT_LLM with orgid, actor, metadata only (AC 5).
func TestRelayPublishesCompletedFactOnEVTLLM(t *testing.T) {
	r := newRig(t, &fake.Provider{Reply: func(provider.Request) []string { return []string{"SECRET-ANSWER"} }})
	tok := r.Token(t, "user-a", "org-1")
	id := r.Create(t, tok)
	r.Send(t, tok, id, "SECRET-PROMPT")

	nc := env.NATS(t)
	js, err := jetstream.New(nc)
	if err != nil {
		t.Fatal(err)
	}
	if err := app.EnsureStream(t.Context(), js); err != nil {
		t.Fatal(err)
	}
	if err := app.EnsureStream(t.Context(), js); err != nil { // idempotent
		t.Fatal(err)
	}
	relay, err := outbox.NewRelay(r.db, js, nil, outbox.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if n, err := relay.RunOnce(t.Context()); err != nil || n != 1 {
		t.Fatalf("relay: %d, %v", n, err)
	}
	cons, err := js.OrderedConsumer(t.Context(), app.StreamName, jetstream.OrderedConsumerConfig{FilterSubjects: []string{"evt.llm.message.completed.v1"}})
	if err != nil {
		t.Fatal(err)
	}
	msg, err := cons.Next(jetstream.FetchMaxWait(5 * time.Second))
	if err != nil {
		t.Fatal(err)
	}
	raw := string(msg.Data())
	if strings.Contains(raw, "SECRET") {
		t.Fatalf("event holds text: %s", raw)
	}
	var ev cloudevents.Event
	if err := json.Unmarshal(msg.Data(), &ev); err != nil {
		t.Fatal(err)
	}
	if ev.Type != "dev.synthwerk.llm.message.completed.v1" || ev.OrgID != "org-1" || ev.Actor != "user:user-a" || ev.Source != "/synthwerk/llm/local" {
		t.Fatalf("event %+v", ev)
	}
	var data map[string]any
	_ = json.Unmarshal(ev.Data, &data)
	allowed := []string{"conversation_id", "message_id", "model", "provider", "input_tokens", "output_tokens", "latency_ms", "ttft_ms", "status"}
	for k := range data {
		if !slices.Contains(allowed, k) {
			t.Errorf("data has key %q outside the schema", k)
		}
	}
	if data["conversation_id"] != id || data["message_id"] != ev.Subject || data["status"] != "completed" || data["provider"] != "fake" || data["model"] != "default" {
		t.Fatalf("data %v", data)
	}
}

// Finish - message not streaming - rollback, no event.
func TestFinishRollbackWritesNoEvent(t *testing.T) {
	r := newRig(t, nil)
	o := store.Owner{Tenant: "org-1", User: "user-a"}
	ev, err := cloudevents.New(t.Context(), "dev.synthwerk.llm.message.completed.v1", "/synthwerk/llm/local", map[string]any{"status": "failed"})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.st.Finish(t.Context(), o, store.Final{MessageID: "0199f3b0-1a2c-7c3d-8e4f-5a6b7c8d9e10", Status: "failed"}, ev); err == nil {
		t.Fatal("want an error")
	}
	if evs := events(t, r); len(evs) != 0 {
		t.Fatalf("events %v", evs)
	}
}

// Startup - row stuck in streaming - marked failed.
func TestSweepMarksStuckAnswersFailed(t *testing.T) {
	r := newRig(t, nil)
	o := store.Owner{Tenant: "org-1", User: "user-a"}
	c, err := r.st.CreateConversation(t.Context(), o, "default")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.st.StartTurn(t.Context(), o, c.ID, "hi"); err != nil {
		t.Fatal(err)
	}
	if n, err := r.st.SweepStreaming(t.Context(), time.Hour); err != nil || n != 0 {
		t.Fatalf("a young streaming row was swept: %d, %v", n, err)
	}
	time.Sleep(50 * time.Millisecond)
	n, err := r.st.SweepStreaming(t.Context(), 10*time.Millisecond)
	if err != nil || n != 1 {
		t.Fatalf("swept %d, %v", n, err)
	}
	got, err := r.st.GetConversation(t.Context(), o, c.ID)
	if err != nil || got.Messages[1].Status != "failed" || got.Messages[0].Status != "completed" {
		t.Fatalf("conversation %+v err %v", got, err)
	}
	// Migrations run at every start: a second run must succeed.
	if err := r.App.Migrate(t.Context()); err != nil {
		t.Fatal(err)
	}
}
