// Package erasure deletes a user's llm rows when identity reports the account as
// deleted (synthwerk#41). It is a JetStream durable consumer of
// dev.synthwerk.identity.user.deleted.v1 on the stream EVT_IDENTITY.
//
//   - Identity owns EVT_IDENTITY. llm never creates it: while it is missing, Run
//     retries with backoff and logs WARN. The service keeps running.
//   - One event = one store transaction: the user's messages, conversations and own
//     quota rows in every org, plus the org quota rows of the closed orgs.
//   - Ack after the commit. A database error: Nak with a delay, the event comes
//     back. A malformed event: Term, it never comes back.
//   - A repeat of the event deletes nothing and acks: the erase is idempotent.
//   - Logs hold the event id and row counts. Never the user sub, never content.
package erasure

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"regexp"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/VelimirMueller/synthwerk-contracts/go/kit/cloudevents"

	"github.com/VelimirMueller/synthwerk-llm/internal/store"
)

// Names of the fact and its place on the bus (synthwerk-contracts events/catalog.yaml).
const (
	EventType  = "dev.synthwerk.identity.user.deleted.v1"
	Subject    = "evt.identity.user.deleted.v1"
	StreamName = "EVT_IDENTITY"
	// Durable is the consumer name. One per service: all replicas share it, so
	// each event reaches one replica.
	Durable = "llm-user-deleted"
)

// Limits of the data schema (events/identity/user.deleted.v1.schema.json).
const maxDeletedOrgs = 100

var (
	subRe = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)
	orgRe = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
)

// ErrMalformed means the event can never be processed. The consumer terminates it.
var ErrMalformed = errors.New("erasure: malformed event")

// DeletedOrg is one org that closed with the account.
type DeletedOrg struct {
	OrgID        string `json:"org_id"`
	ZitadelOrgID string `json:"zitadel_org_id"`
}

// Data is the data of user.deleted.v1.
type Data struct {
	UserSub     string       `json:"user_sub"`
	DeletedOrgs []DeletedOrg `json:"deleted_orgs"`
}

// ZitadelOrgIDs returns the token org ids of the closed orgs: the tenant ids of llm.
func (d Data) ZitadelOrgIDs() []string {
	out := make([]string, 0, len(d.DeletedOrgs))
	for _, o := range d.DeletedOrgs {
		out = append(out, o.ZitadelOrgID)
	}
	return out
}

// Parse decodes and checks one CloudEvent of type EventType. Every error wraps
// ErrMalformed and names the rule, never a value.
func Parse(raw []byte) (cloudevents.Event, Data, error) {
	var ev cloudevents.Event
	if err := json.Unmarshal(raw, &ev); err != nil {
		return cloudevents.Event{}, Data{}, fmt.Errorf("%w: envelope is not JSON", ErrMalformed)
	}
	if err := ev.Validate(); err != nil {
		return cloudevents.Event{}, Data{}, fmt.Errorf("%w: envelope is not a valid CloudEvent", ErrMalformed)
	}
	if ev.Type != EventType {
		return cloudevents.Event{}, Data{}, fmt.Errorf("%w: type is not %s", ErrMalformed, EventType)
	}
	// The schema says additionalProperties: false.
	dec := json.NewDecoder(bytes.NewReader(ev.Data))
	dec.DisallowUnknownFields()
	var d struct {
		UserSub     *string       `json:"user_sub"`
		DeletedOrgs *[]DeletedOrg `json:"deleted_orgs"`
	}
	if err := dec.Decode(&d); err != nil {
		return cloudevents.Event{}, Data{}, fmt.Errorf("%w: data does not match the schema", ErrMalformed)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return cloudevents.Event{}, Data{}, fmt.Errorf("%w: data has trailing content", ErrMalformed)
	}
	switch {
	case d.UserSub == nil || !subRe.MatchString(*d.UserSub):
		return cloudevents.Event{}, Data{}, fmt.Errorf("%w: user_sub is missing or not [A-Za-z0-9_-]{1,128}", ErrMalformed)
	case d.DeletedOrgs == nil:
		return cloudevents.Event{}, Data{}, fmt.Errorf("%w: deleted_orgs is missing", ErrMalformed)
	case len(*d.DeletedOrgs) > maxDeletedOrgs:
		return cloudevents.Event{}, Data{}, fmt.Errorf("%w: deleted_orgs has more than %d items", ErrMalformed, maxDeletedOrgs)
	}
	for _, o := range *d.DeletedOrgs {
		id, err := uuid.Parse(o.OrgID)
		if err != nil || id.String() != o.OrgID || !orgRe.MatchString(o.ZitadelOrgID) {
			return cloudevents.Event{}, Data{}, fmt.Errorf("%w: a deleted_orgs item has a bad org_id or zitadel_org_id", ErrMalformed)
		}
	}
	return ev, Data{UserSub: *d.UserSub, DeletedOrgs: *d.DeletedOrgs}, nil
}

// Eraser deletes the rows. *store.Store implements it.
type Eraser interface {
	EraseUser(ctx context.Context, user string, orgs []string) (store.Erased, error)
}

// Config of the consumer. Zero values take the defaults.
type Config struct {
	// AckWait is how long JetStream waits for the ack before it sends the event
	// again. Default 30 s. One erase gets AckWait minus 5 s.
	AckWait time.Duration
	// RetryMin and RetryMax bound the backoff of a Nak and of the wait for the
	// stream. Default 1 s and 1 min.
	RetryMin, RetryMax time.Duration
}

func (c Config) withDefaults() Config {
	if c.AckWait <= 0 {
		c.AckWait = 30 * time.Second
	}
	if c.RetryMin <= 0 {
		c.RetryMin = time.Second
	}
	if c.RetryMax < c.RetryMin {
		c.RetryMax = max(time.Minute, c.RetryMin)
	}
	return c
}

// ConsumerConfig is the durable consumer llm creates or updates on EVT_IDENTITY.
// MaxDeliver -1: an erase is a legal duty, so a failing event keeps coming back
// (with the Nak backoff) until it succeeds. A malformed one is terminated instead.
func ConsumerConfig(c Config) jetstream.ConsumerConfig {
	c = c.withDefaults()
	return jetstream.ConsumerConfig{
		Durable:       Durable,
		Description:   "synthwerk-llm: delete the user's rows (synthwerk#41).",
		FilterSubject: Subject,
		DeliverPolicy: jetstream.DeliverAllPolicy,
		AckPolicy:     jetstream.AckExplicitPolicy,
		AckWait:       c.AckWait,
		MaxDeliver:    -1,
		MaxAckPending: 16,
	}
}

// Consumer runs the durable consumer.
type Consumer struct {
	js     jetstream.JetStream
	eraser Eraser
	log    *slog.Logger
	cfg    Config
	ready  atomic.Bool
}

// New returns the consumer. Call Run.
func New(js jetstream.JetStream, e Eraser, logger *slog.Logger, cfg Config) *Consumer {
	return &Consumer{js: js, eraser: e, log: logger, cfg: cfg.withDefaults()}
}

// Ready reports whether the consumer is attached to EVT_IDENTITY. For /readyz,
// not critical: chat works without it, events wait in the stream.
func (c *Consumer) Ready(context.Context) error {
	if !c.ready.Load() {
		return errors.New("user.deleted consumer not attached")
	}
	return nil
}

// Run attaches the consumer and handles events until ctx ends. A missing stream
// or a lost consumer is retried with backoff. Run returns nil when ctx ends.
func (c *Consumer) Run(ctx context.Context) error {
	wait := c.cfg.RetryMin
	for {
		cons, err := c.js.CreateOrUpdateConsumer(ctx, StreamName, ConsumerConfig(c.cfg))
		if err != nil {
			if ctx.Err() != nil {
				return nil //nolint:nilerr // shutdown, not a failure
			}
			reason := "consumer_create_failed"
			if errors.Is(err, jetstream.ErrStreamNotFound) {
				reason = "stream_missing"
			}
			c.log.WarnContext(ctx, "user.deleted consumer not attached, retrying",
				slog.String("event.name", "llm.erasure.waiting"),
				slog.String("synthwerk.reason", reason),
				slog.String("messaging.destination.name", StreamName),
				slog.Duration("synthwerk.retry_in", wait))
			if !sleep(ctx, wait) {
				return nil
			}
			wait = min(wait*2, c.cfg.RetryMax)
			continue
		}
		wait = c.cfg.RetryMin
		cc, err := cons.Consume(func(m jetstream.Msg) { c.Handle(ctx, m) },
			jetstream.ConsumeErrHandler(func(_ jetstream.ConsumeContext, err error) {
				c.log.WarnContext(ctx, "user.deleted consumer error",
					slog.String("event.name", "llm.erasure.consume_error"),
					slog.String("exception.type", fmt.Sprintf("%T", err)))
			}))
		if err != nil {
			c.log.WarnContext(ctx, "user.deleted consumer does not start, retrying",
				slog.String("event.name", "llm.erasure.waiting"),
				slog.String("synthwerk.reason", "consume_failed"))
			if !sleep(ctx, wait) {
				return nil
			}
			continue
		}
		c.ready.Store(true)
		c.log.InfoContext(ctx, "user.deleted consumer attached",
			slog.String("event.name", "llm.erasure.attached"),
			slog.String("messaging.destination.name", StreamName),
			slog.String("messaging.consumer.group.name", Durable))
		select {
		case <-ctx.Done():
			cc.Stop()
			c.ready.Store(false)
			return nil
		case <-cc.Closed():
			// The stream or the consumer went away (for example, identity re-created
			// the stream). Attach again.
			c.ready.Store(false)
		}
	}
}

// Handle processes one event: erase, then ack; Nak on a store error; Term on a
// malformed event.
func (c *Consumer) Handle(ctx context.Context, m jetstream.Msg) {
	ev, d, err := Parse(m.Data())
	if err != nil {
		c.log.WarnContext(ctx, "user.deleted event rejected",
			slog.String("event.name", "llm.erasure.rejected"),
			slog.String("error.type", "malformed"),
			slog.String("exception.message", err.Error())) // names the rule, never a value
		if err := m.Term(); err != nil {
			c.log.WarnContext(ctx, "term failed", slog.String("event.name", "llm.erasure.term_failed"))
		}
		return
	}
	ectx, cancel := context.WithTimeout(ctx, max(c.cfg.AckWait-5*time.Second, c.cfg.AckWait/2))
	defer cancel()
	n, err := c.eraser.EraseUser(ectx, d.UserSub, d.ZitadelOrgIDs())
	if err != nil {
		delay := c.backoff(m)
		c.log.WarnContext(ctx, "user.deleted erase failed, will retry",
			slog.String("event.name", "llm.erasure.failed"),
			slog.String("cloudevents.event_id", ev.ID),
			slog.Duration("synthwerk.retry_in", delay))
		if err := m.NakWithDelay(delay); err != nil {
			c.log.WarnContext(ctx, "nak failed", slog.String("event.name", "llm.erasure.nak_failed"))
		}
		return
	}
	if err := m.Ack(); err != nil {
		// The rows are gone; a redelivery deletes nothing and acks.
		c.log.WarnContext(ctx, "ack failed", slog.String("event.name", "llm.erasure.ack_failed"),
			slog.String("cloudevents.event_id", ev.ID))
		return
	}
	c.log.InfoContext(ctx, "user data erased",
		slog.String("event.name", "llm.erasure.done"),
		slog.String("cloudevents.event_id", ev.ID),
		slog.Int64("synthwerk.messages", n.Messages),
		slog.Int64("synthwerk.conversations", n.Conversations),
		slog.Int64("synthwerk.quota_rows", n.QuotaRows),
		slog.Int("synthwerk.deleted_orgs", len(d.DeletedOrgs)))
}

// backoff doubles the Nak delay with each delivery, from RetryMin up to RetryMax.
func (c *Consumer) backoff(m jetstream.Msg) time.Duration {
	n := uint64(1)
	if md, err := m.Metadata(); err == nil && md.NumDelivered > 0 {
		n = md.NumDelivered
	}
	d := c.cfg.RetryMin
	for i := uint64(1); i < n && d < c.cfg.RetryMax; i++ {
		d *= 2
	}
	return min(d, c.cfg.RetryMax)
}

func sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
