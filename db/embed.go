// Package db embeds the SQL migrations, so the binary and the tests can run them without
// the source tree. The sqlc-generated query code lives in internal/db.
package db

import "embed"

// Migrations holds every goose migration under the "migrations" directory.
//
//go:embed migrations/*.sql
var Migrations embed.FS
