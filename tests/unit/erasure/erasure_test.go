package erasure_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/VelimirMueller/synthwerk-llm/internal/erasure"
	"github.com/VelimirMueller/synthwerk-llm/internal/store"
)

// example is the contract example (synthwerk-contracts events/examples), copied.
func example(t *testing.T) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/dev.synthwerk.identity.user.deleted.v1.json")
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// withData returns the example with data replaced by the JSON text data.
func withData(t *testing.T, data string) []byte {
	t.Helper()
	var ev map[string]json.RawMessage
	if err := json.Unmarshal(example(t), &ev); err != nil {
		t.Fatal(err)
	}
	ev["data"] = json.RawMessage(data)
	b, err := json.Marshal(ev)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// Parse - contract example - user sub and the Zitadel org ids.
func TestParseContractExample(t *testing.T) {
	ev, d, err := erasure.Parse(example(t))
	if err != nil {
		t.Fatal(err)
	}
	if ev.Type != erasure.EventType || d.UserSub != "396370217707541255" {
		t.Fatalf("event %s data %+v", ev.Type, d)
	}
	if got := d.ZitadelOrgIDs(); len(got) != 1 || got[0] != "396370208765350664" {
		t.Fatalf("orgs %v", got)
	}
}

// Parse - empty deleted_orgs - valid, no org ids.
func TestParseNoClosedOrgs(t *testing.T) {
	_, d, err := erasure.Parse(withData(t, `{"user_sub":"u_1","deleted_orgs":[]}`))
	if err != nil || len(d.ZitadelOrgIDs()) != 0 {
		t.Fatalf("err %v data %+v", err, d)
	}
}

// Parse - each broken event - ErrMalformed, and the error holds no value of the event.
func TestParseRejectsMalformed(t *testing.T) {
	tooMany := make([]string, 101)
	for i := range tooMany {
		tooMany[i] = `{"org_id":"019a3c20-1a2b-7c3d-8e4f-5a6b7c8d9e0f","zitadel_org_id":"o1"}`
	}
	other := strings.Replace(string(example(t)), "identity.user.deleted", "identity.user.created", 1)
	cases := map[string][]byte{
		"not json":            []byte("{nope"),
		"bad envelope":        []byte(`{"specversion":"1.0","data":{}}`),
		"other type":          []byte(other),
		"no user_sub":         withData(t, `{"deleted_orgs":[]}`),
		"empty user_sub":      withData(t, `{"user_sub":"","deleted_orgs":[]}`),
		"user_sub with comma": withData(t, `{"user_sub":"a,b","deleted_orgs":[]}`),
		"user_sub with %":     withData(t, `{"user_sub":"a%","deleted_orgs":[]}`),
		"user_sub too long":   withData(t, `{"user_sub":"`+strings.Repeat("a", 129)+`","deleted_orgs":[]}`),
		"no deleted_orgs":     withData(t, `{"user_sub":"u1"}`),
		"null deleted_orgs":   withData(t, `{"user_sub":"u1","deleted_orgs":null}`),
		"unknown field":       withData(t, `{"user_sub":"u1","deleted_orgs":[],"email":"a@b.example"}`),
		"bad zitadel org":     withData(t, `{"user_sub":"u1","deleted_orgs":[{"org_id":"019a3c20-1a2b-7c3d-8e4f-5a6b7c8d9e0f","zitadel_org_id":"a,b"}]}`),
		"bad org uuid":        withData(t, `{"user_sub":"u1","deleted_orgs":[{"org_id":"x","zitadel_org_id":"o1"}]}`),
		"too many orgs":       withData(t, `{"user_sub":"u1","deleted_orgs":[`+strings.Join(tooMany, ",")+`]}`),
	}
	for name, raw := range cases {
		_, _, err := erasure.Parse(raw)
		if !errors.Is(err, erasure.ErrMalformed) {
			t.Errorf("%s: err %v, want ErrMalformed", name, err)
			continue
		}
		if strings.Contains(err.Error(), "a@b.example") || strings.Contains(err.Error(), "a,b") {
			t.Errorf("%s: error holds a value: %v", name, err)
		}
	}
}

// fakeMsg records what the consumer did with it.
type fakeMsg struct {
	jetstream.Msg // nil: any other method panics
	data          []byte
	delivered     uint64
	mu            sync.Mutex
	acked, termed bool
	nakDelay      time.Duration
}

func (m *fakeMsg) Data() []byte { return m.data }
func (m *fakeMsg) Metadata() (*jetstream.MsgMetadata, error) {
	return &jetstream.MsgMetadata{NumDelivered: m.delivered}, nil
}
func (m *fakeMsg) Headers() nats.Header { return nats.Header{} }
func (m *fakeMsg) Ack() error           { m.mu.Lock(); m.acked = true; m.mu.Unlock(); return nil }
func (m *fakeMsg) Term() error          { m.mu.Lock(); m.termed = true; m.mu.Unlock(); return nil }
func (m *fakeMsg) NakWithDelay(d time.Duration) error {
	m.mu.Lock()
	m.nakDelay = d
	m.mu.Unlock()
	return nil
}

type fakeEraser struct {
	err   error
	user  string
	orgs  []string
	calls int
}

func (f *fakeEraser) EraseUser(_ context.Context, user string, orgs []string) (store.Erased, error) {
	f.calls++
	f.user, f.orgs = user, orgs
	return store.Erased{Messages: 2, Conversations: 1, QuotaRows: 2}, f.err
}

func newConsumer(e erasure.Eraser, logs *bytes.Buffer) *erasure.Consumer {
	logger := slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	return erasure.New(nil, e, logger, erasure.Config{RetryMin: time.Second, RetryMax: 8 * time.Second})
}

// Handle - valid event - erase with the sub and the Zitadel org ids, then ack. No sub in the logs.
func TestHandleErasesThenAcks(t *testing.T) {
	var logs bytes.Buffer
	e := &fakeEraser{}
	m := &fakeMsg{data: example(t), delivered: 1}
	newConsumer(e, &logs).Handle(t.Context(), m)
	if e.calls != 1 || e.user != "396370217707541255" || len(e.orgs) != 1 || e.orgs[0] != "396370208765350664" {
		t.Fatalf("eraser got %+v", e)
	}
	if !m.acked || m.termed || m.nakDelay != 0 {
		t.Fatalf("msg %+v", m)
	}
	if !strings.Contains(logs.String(), `"llm.erasure.done"`) || strings.Contains(logs.String(), "396370217707541255") {
		t.Fatalf("logs: %s", logs.String())
	}
}

// Handle - malformed event - Term, no erase, WARN log.
func TestHandleTermsMalformed(t *testing.T) {
	var logs bytes.Buffer
	e := &fakeEraser{}
	m := &fakeMsg{data: withData(t, `{"user_sub":"a b","deleted_orgs":[]}`), delivered: 1}
	newConsumer(e, &logs).Handle(t.Context(), m)
	if e.calls != 0 || !m.termed || m.acked {
		t.Fatalf("calls %d msg %+v", e.calls, m)
	}
	if !strings.Contains(logs.String(), `"llm.erasure.rejected"`) || !strings.Contains(logs.String(), `"level":"WARN"`) {
		t.Fatalf("logs: %s", logs.String())
	}
}

// Handle - database error - Nak with a delay that grows per delivery up to the max, no ack.
func TestHandleNaksOnStoreError(t *testing.T) {
	want := map[uint64]time.Duration{1: time.Second, 2: 2 * time.Second, 3: 4 * time.Second, 4: 8 * time.Second, 9: 8 * time.Second}
	for n, d := range want {
		var logs bytes.Buffer
		m := &fakeMsg{data: example(t), delivered: n}
		newConsumer(&fakeEraser{err: errors.New("db down")}, &logs).Handle(t.Context(), m)
		if m.acked || m.termed || m.nakDelay != d {
			t.Errorf("delivery %d: msg %+v, want nak %s", n, m, d)
		}
		if !strings.Contains(logs.String(), `"llm.erasure.failed"`) {
			t.Errorf("delivery %d: no llm.erasure.failed log line", n)
		}
	}
}

// ConsumerConfig - defaults - durable on the subject, explicit ack, all events, endless redelivery.
func TestConsumerConfig(t *testing.T) {
	c := erasure.ConsumerConfig(erasure.Config{})
	if c.Durable != "llm-user-deleted" || c.FilterSubject != "evt.identity.user.deleted.v1" ||
		c.AckPolicy != jetstream.AckExplicitPolicy || c.DeliverPolicy != jetstream.DeliverAllPolicy ||
		c.AckWait != 30*time.Second || c.MaxDeliver != -1 {
		t.Fatalf("%+v", c)
	}
}

// Consumer - before Run - not ready.
func TestReadyBeforeRun(t *testing.T) {
	var logs bytes.Buffer
	if err := newConsumer(&fakeEraser{}, &logs).Ready(t.Context()); err == nil {
		t.Fatal("ready before attach")
	}
}
