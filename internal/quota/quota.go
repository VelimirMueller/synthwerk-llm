// Package quota limits the tokens of one user and of one org per window (#33).
//
// The window is one UTC day. A send books an estimate before the model call (the
// history, the new message and a reserve for the answer) and settles the real
// token count when the answer ends. The store does both in Postgres, in the
// transaction of the turn and of the finish, so concurrent streams of one user or
// one org can never book more than the limit (internal/store).
//
// The limits come from a Source. Static reads them from config now. When identity
// serves entitlements (#31), a Source that reads the org's plan replaces it; the
// chat handler and the store stay the same.
package quota

import (
	"context"
	"errors"
	"math"
	"time"

	"github.com/VelimirMueller/synthwerk-contracts/go/kit/auth"
)

// ErrExhausted means the caller has no quota left (429 quota_exhausted, D-46).
// Every *ExceededError matches it with errors.Is.
var ErrExhausted = errors.New("quota: exhausted")

// Scope is the owner of a quota (contracts QuotaExceeded.scope).
type Scope string

// Scopes.
const (
	ScopeUser Scope = "user"
	ScopeOrg  Scope = "org"
)

// Limits are tokens per window. 0 means no limit for that scope; usage is still
// counted.
type Limits struct {
	UserTokens int64
	OrgTokens  int64
}

// Source returns the limits that apply to a principal.
type Source interface {
	Limits(ctx context.Context, p *auth.Principal) (Limits, error)
}

// Static gives every org and every user the same limits (config).
type Static Limits

// Limits implements Source.
func (s Static) Limits(context.Context, *auth.Principal) (Limits, error) { return Limits(s), nil }

// None has no limits. Usage is still counted.
type None struct{}

// Limits implements Source.
func (None) Limits(context.Context, *auth.Principal) (Limits, error) { return Limits{}, nil }

// Window returns the window that holds t: its start and the start of the next one
// (UTC midnight).
func Window(t time.Time) (start, end time.Time) {
	u := t.UTC()
	start = time.Date(u.Year(), u.Month(), u.Day(), 0, 0, 0, 0, time.UTC)
	return start, start.AddDate(0, 0, 1)
}

// ExceededError names the quota that ran out.
type ExceededError struct {
	Scope Scope
	// Limit is the tokens per window of the scope.
	Limit int64
	// ResetsAt is the start of the next window.
	ResetsAt time.Time
}

func (e *ExceededError) Error() string { return "quota: " + string(e.Scope) + " quota exhausted" }

// Is makes errors.Is(err, ErrExhausted) true.
func (*ExceededError) Is(target error) bool { return target == ErrExhausted }

// RetryAfter is the number of whole seconds from now to the reset, 1 to 86400
// (the Retry-After range of the contract).
func (e *ExceededError) RetryAfter(now time.Time) int {
	s := math.Ceil(e.ResetsAt.Sub(now).Seconds())
	return int(min(max(s, 1), 86400))
}
