package database

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"

	_ "github.com/jackc/pgx/v5/stdlib" // registers the "pgx" database/sql driver for goose
	"github.com/pressly/goose/v3"

	appdb "siapp/db"
)

// Migrate runs a goose command ("up", "down" or "status") against dsn with the embedded
// migrations. dsn must be the owner's (MIGRATE_DSN): the app role cannot create tables.
// "down" reverts one migration.
func Migrate(ctx context.Context, dsn, command string) error {
	sqlDB, err := sql.Open("pgx", dsn)
	if err != nil {
		return errors.New("migrate: invalid connection URL")
	}
	defer func() { _ = sqlDB.Close() }()

	files, err := fs.Sub(appdb.Migrations, appdb.MigrationsDir)
	if err != nil {
		return fmt.Errorf("migrate: embedded migrations: %w", err)
	}
	provider, err := goose.NewProvider(goose.DialectPostgres, sqlDB, files)
	if err != nil {
		return fmt.Errorf("migrate: %w", err)
	}

	switch command {
	case "up":
		_, err = provider.Up(ctx)
	case "down":
		_, err = provider.Down(ctx)
	case "status":
		var results []*goose.MigrationStatus
		if results, err = provider.Status(ctx); err == nil {
			for _, s := range results {
				fmt.Printf("%-40s %s\n", s.Source.Path, s.State)
			}
		}
	default:
		return fmt.Errorf("migrate: unknown command %q (want up, down or status)", command)
	}
	if err != nil {
		return fmt.Errorf("migrate %s: %w", command, err)
	}
	return nil
}
