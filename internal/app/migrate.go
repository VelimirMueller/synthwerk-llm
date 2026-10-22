package app

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/VelimirMueller/synthwerk-llm/migrations"
)

// Migrate applies the SQL migrations embedded at build time.
func (a *App) Migrate(ctx context.Context) error {
	all, err := migrations.All()
	if err != nil {
		return fmt.Errorf("app: migrate: %w", err)
	}
	for _, sql := range all {
		// The simple protocol runs a file with several statements in one call.
		if _, err := a.DB.Exec(ctx, sql, pgx.QueryExecModeSimpleProtocol); err != nil {
			return fmt.Errorf("app: migrate: %w", err)
		}
	}
	return nil
}
