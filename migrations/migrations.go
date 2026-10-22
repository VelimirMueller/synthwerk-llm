// Package migrations embeds the SQL migration files of the service.
package migrations

import "embed"

//go:embed *.sql
var FS embed.FS
