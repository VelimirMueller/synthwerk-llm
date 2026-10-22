// Package migrations holds the SQL migrations of synthwerk-llm. The service
// applies all of them at startup with Exec, in file-name order (0001, 0002, ...).
package migrations

import (
	"embed"
	"fmt"
	"io/fs"
)

//go:embed *.sql
var files embed.FS

// All returns the SQL of every migration file in file-name order. The service runs
// all of them at every start, so each file must be idempotent (IF NOT EXISTS).
func All() ([]string, error) {
	names, err := fs.Glob(files, "*.sql") // sorted by name
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(names))
	for _, n := range names {
		b, err := files.ReadFile(n)
		if err != nil {
			return nil, fmt.Errorf("migrations: read %s: %w", n, err)
		}
		out = append(out, string(b))
	}
	return out, nil
}
