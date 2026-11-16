package store

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/VelimirMueller/synthwerk-contracts/go/kit/cloudevents"
	"github.com/VelimirMueller/synthwerk-contracts/go/kit/outbox"

	"github.com/VelimirMueller/synthwerk-llm/internal/quota"
)

// Usage is the token use of one window (one UTC day): used plus booked.
type Usage struct {
	Window time.Time
	// User is the owner's own row, Org the shared org row. Each is used + reserved.
	User int64
	Org  int64
}

// QuotaUsage reads the owner's own quota row and the org row of the window of now
// (AI tools, #53). Row level security limits it to these two rows: a peer's row is
// never visible. A missing row counts 0.
func (s *Store) QuotaUsage(ctx context.Context, o Owner, now time.Time) (Usage, error) {
	win, _ := quota.Window(now)
	u := Usage{Window: win}
	err := s.withOwner(ctx, o, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT COALESCE(SUM(used + reserved) FILTER (WHERE owner_id = $2), 0),
			        COALESCE(SUM(used + reserved) FILTER (WHERE owner_id = ''), 0)
			 FROM llm.quota_usage
			 WHERE tenant_id = $1 AND owner_id IN ('', $2) AND window_start = $3`,
			o.Tenant, o.User, win).Scan(&u.User, &u.Org)
	})
	if err != nil {
		return Usage{}, fmt.Errorf("store: quota usage: %w", err)
	}
	return u, nil
}

// RecordToolCall writes one audit fact (llm.toolcall.executed.v3, #53) to the kit
// outbox in its own transaction. The relay publishes it on EVT_LLM.
func (s *Store) RecordToolCall(ctx context.Context, o Owner, ev cloudevents.Event) error {
	err := s.withOwner(ctx, o, func(tx pgx.Tx) error {
		return outbox.Enqueue(ctx, tx, ev)
	})
	if err != nil {
		return fmt.Errorf("store: record tool call: %w", err)
	}
	return nil
}
