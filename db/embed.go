// Package db embeds the goose migrations so the migrate command, the API's optional
// run-on-start and the test kit all apply exactly the same files.
package db

import "embed"

// Migrations holds db/migrations/*.sql.
//
//go:embed migrations/*.sql
var Migrations embed.FS

// MigrationsDir is the directory inside Migrations.
const MigrationsDir = "migrations"
