package app

import (
	"context"
	"fmt"

	"github.com/VelimirMueller/synthwerk-llm/migrations"
)

// Migrate applies the SQL migrations embedded at build time.
func (a *App) Migrate(ctx context.Context) error {
	data, err := migrations.FS.ReadFile("0001_init.sql")
	if err != nil {
		return fmt.Errorf("app: migrate: read: %w", err)
	}
	if _, err := a.DB.Exec(ctx, string(data)); err != nil {
		return fmt.Errorf("app: migrate: %w", err)
	}
	return nil
}
