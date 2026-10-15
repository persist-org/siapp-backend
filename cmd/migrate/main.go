// Command migrate applies the embedded goose migrations as the database owner.
//
//	migrate up|down|status
//
// It reads MIGRATE_DSN (siapp_owner), never DATABASE_URL: the app role cannot create tables.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"siapp/pkg/platform/database"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("usage: migrate up|down|status")
	}
	dsn := os.Getenv("MIGRATE_DSN")
	if dsn == "" {
		return fmt.Errorf("MIGRATE_DSN is required")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return database.Migrate(ctx, dsn, args[0])
}
