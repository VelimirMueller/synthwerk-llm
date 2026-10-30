package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/VelimirMueller/synthwerk-contracts/go/kit/cloudevents"
	"github.com/VelimirMueller/synthwerk-contracts/go/kit/outbox"

	"github.com/VelimirMueller/synthwerk-llm/internal/settings"
)

// ErrVersionConflict means the stored settings have another version than the
// caller read (409 version_conflict). Nothing changed.
var ErrVersionConflict = errors.New("store: settings version conflict")

const settingsColumns = `model_alias, allowed_models, system_prompt, max_input_chars, user_tokens_per_day, temperature, version, updated_at`

func scanSettings(row pgx.Row) (*settings.Settings, error) {
	var s settings.Settings
	if err := row.Scan(&s.Model, &s.AllowedModels, &s.SystemPrompt, &s.MaxInputChars,
		&s.UserTokensPerDay, &s.Temperature, &s.Version, &s.UpdatedAt); err != nil {
		return nil, err
	}
	return &s, nil
}

// OrgSettings returns the AI settings of the owner's org. Nil and no error: the org
// never saved settings.
func (s *Store) OrgSettings(ctx context.Context, o Owner) (*settings.Settings, error) {
	var out *settings.Settings
	err := s.withOwner(ctx, o, func(tx pgx.Tx) error {
		var err error
		out, err = scanSettings(tx.QueryRow(ctx,
			`SELECT `+settingsColumns+` FROM llm.org_settings WHERE tenant_id = $1`, o.Tenant))
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("store: org settings: %w", err)
	}
	return out, nil
}

// SettingsChange builds the change from the stored settings (nil: none) and the
// version the row gets when it is written. It returns a nil event when nothing
// changes: then nothing is written and no event goes out.
type SettingsChange func(prev *settings.Settings, version int64) (*cloudevents.Event, error)

// SaveOrgSettings replaces the AI settings of the owner's org when the stored version
// is expect (0: none stored). build sees the stored settings under a row lock and
// returns the event of the change, or nil for no change. The row and the event are
// written in one transaction (kit outbox). It returns the stored settings after the
// call and whether they changed. ErrVersionConflict: another version is stored.
func (s *Store) SaveOrgSettings(ctx context.Context, o Owner, next settings.Settings, expect int64, build SettingsChange) (settings.Settings, bool, error) {
	var saved settings.Settings
	var changed bool
	err := s.withOwner(ctx, o, func(tx pgx.Tx) error {
		prev, err := scanSettings(tx.QueryRow(ctx,
			`SELECT `+settingsColumns+` FROM llm.org_settings WHERE tenant_id = $1 FOR UPDATE`, o.Tenant))
		if errors.Is(err, pgx.ErrNoRows) {
			prev, err = nil, nil
		}
		if err != nil {
			return err
		}
		var version int64
		if prev != nil {
			version = prev.Version
		}
		if version != expect {
			return ErrVersionConflict
		}
		ev, err := build(prev, version+1)
		if err != nil {
			return err
		}
		if ev == nil {
			if prev != nil {
				saved = *prev
			}
			return nil
		}
		var row *settings.Settings
		if prev == nil {
			// Two first saves at once: the second insert finds the row and writes nothing.
			row, err = scanSettings(tx.QueryRow(ctx,
				`INSERT INTO llm.org_settings (tenant_id, model_alias, allowed_models, system_prompt, max_input_chars, user_tokens_per_day, temperature, version, updated_at)
				 VALUES ($1, $2, $3, $4, $5, $6, $7, 1, now())
				 ON CONFLICT (tenant_id) DO NOTHING
				 RETURNING `+settingsColumns,
				o.Tenant, next.Model, next.AllowedModels, next.SystemPrompt, next.MaxInputChars, next.UserTokensPerDay, next.Temperature))
		} else {
			row, err = scanSettings(tx.QueryRow(ctx,
				`UPDATE llm.org_settings SET model_alias = $2, allowed_models = $3, system_prompt = $4, max_input_chars = $5,
				        user_tokens_per_day = $6, temperature = $7, version = version + 1, updated_at = now()
				 WHERE tenant_id = $1 AND version = $8
				 RETURNING `+settingsColumns,
				o.Tenant, next.Model, next.AllowedModels, next.SystemPrompt, next.MaxInputChars, next.UserTokensPerDay, next.Temperature, version))
		}
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrVersionConflict
		}
		if err != nil {
			return err
		}
		if row.Version != version+1 {
			return fmt.Errorf("stored version %d, event version %d", row.Version, version+1)
		}
		if err := outbox.Enqueue(ctx, tx, *ev); err != nil {
			return err
		}
		saved, changed = *row, true
		return nil
	})
	if errors.Is(err, ErrVersionConflict) {
		return settings.Settings{}, false, ErrVersionConflict
	}
	if err != nil {
		return settings.Settings{}, false, fmt.Errorf("store: save org settings: %w", err)
	}
	return saved, changed, nil
}
