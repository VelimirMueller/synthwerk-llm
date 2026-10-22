package app

import (
	"context"
	"fmt"

	"github.com/VelimirMueller/synthwerk-llm/migrations"
)

// Migrate applies the SQL migrations embedded at build time.
func (a *App) Migrate(ctx context.Context) error {
	all, err := migrations.All()
	if err != nil {
		return fmt.Errorf("app: migrate: %w", err)
	}
	for _, sql := range all {
		if _, err := a.DB.Exec(ctx, sql); err != nil {
			return fmt.Errorf("app: migrate: %w", err)
		}
	}
	return nil
}
