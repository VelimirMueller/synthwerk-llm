// Package quota is the seam for usage limits. #32 ships a no-op; #33 plugs the real
// guard in. The chat handler calls Check after auth, ownership and input checks and
// before any model call, so a refused request never reaches the provider.
package quota

import (
	"context"
	"errors"

	"github.com/VelimirMueller/synthwerk-contracts/go/kit/auth"
)

// ErrExhausted means the caller has no quota left (429 quota_exhausted, D-46).
var ErrExhausted = errors.New("quota: exhausted")

// Guard decides whether a principal may start one more answer.
type Guard interface {
	Check(ctx context.Context, p *auth.Principal, modelAlias string) error
}

// None allows everything (M2).
type None struct{}

// Check implements Guard.
func (None) Check(context.Context, *auth.Principal, string) error { return nil }
