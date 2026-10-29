package quota_test

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/VelimirMueller/synthwerk-llm/internal/quota"
)

// Window - any time of a day - starts at UTC midnight, ends at the next one.
func TestWindowIsTheUTCDay(t *testing.T) {
	berlin := time.FixedZone("CEST", 2*60*60)
	cases := map[string]struct {
		at          time.Time
		start, next string
	}{
		"midday":           {time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC), "2026-10-29", "2026-10-30"},
		"midnight":         {time.Date(2026, 10, 29, 0, 0, 0, 0, time.UTC), "2026-10-29", "2026-10-30"},
		"last ns":          {time.Date(2026, 10, 29, 23, 59, 59, 999999999, time.UTC), "2026-10-29", "2026-10-30"},
		"other zone":       {time.Date(2026, 10, 30, 1, 0, 0, 0, berlin), "2026-10-29", "2026-10-30"},
		"end of year":      {time.Date(2026, 12, 31, 18, 0, 0, 0, time.UTC), "2026-12-31", "2027-01-01"},
		"leap day 2028":    {time.Date(2028, 2, 29, 3, 0, 0, 0, time.UTC), "2028-02-29", "2028-03-01"},
		"before leap day":  {time.Date(2028, 2, 28, 23, 0, 0, 0, time.UTC), "2028-02-28", "2028-02-29"},
		"no leap day 2027": {time.Date(2027, 2, 28, 23, 0, 0, 0, time.UTC), "2027-02-28", "2027-03-01"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			s, n := quota.Window(tc.at)
			if s.Format(time.DateOnly) != tc.start || n.Format(time.DateOnly) != tc.next || s.Location() != time.UTC || !n.Equal(s.Add(24*time.Hour)) {
				t.Fatalf("got %s .. %s", s, n)
			}
		})
	}
}

// ExceededError - wrapped or not - matches ErrExhausted; RetryAfter stays in 1..86400.
func TestExceededError(t *testing.T) {
	now := time.Date(2026, 10, 29, 23, 0, 0, 0, time.UTC)
	_, next := quota.Window(now)
	e := &quota.ExceededError{Scope: quota.ScopeOrg, Limit: 10, ResetsAt: next}
	if !errors.Is(e, quota.ErrExhausted) || !errors.Is(fmt.Errorf("store: %w", e), quota.ErrExhausted) {
		t.Fatal("does not match ErrExhausted")
	}
	if got, ok := errors.AsType[*quota.ExceededError](fmt.Errorf("x: %w", e)); !ok || got.Scope != quota.ScopeOrg {
		t.Fatal("As lost the scope")
	}
	cases := map[string]struct {
		now  time.Time
		want int
	}{
		"one hour left":      {now, 3600},
		"part of a second":   {next.Add(-300 * time.Millisecond), 1},
		"reset already past": {next.Add(time.Minute), 1},
		"more than a day":    {next.Add(-48 * time.Hour), 86400},
	}
	for name, tc := range cases {
		if got := e.RetryAfter(tc.now); got != tc.want {
			t.Errorf("%s: got %d, want %d", name, got, tc.want)
		}
	}
}

// Sources - Static gives its limits, None gives no limit.
func TestSources(t *testing.T) {
	l, err := quota.Static{UserTokens: 5, OrgTokens: 50}.Limits(t.Context(), nil)
	if err != nil || l.UserTokens != 5 || l.OrgTokens != 50 {
		t.Fatalf("static: %+v %v", l, err)
	}
	if l, err := (quota.None{}).Limits(t.Context(), nil); err != nil || l != (quota.Limits{}) {
		t.Fatalf("none: %+v %v", l, err)
	}
}
