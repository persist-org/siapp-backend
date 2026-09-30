// Command seed creates the first administrator. It is idempotent, so it can run on every
// deploy: when an admin already exists it does nothing.
package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"siapp/internal/db"
	"siapp/internal/platform/auth"
	"siapp/internal/platform/config"
	"siapp/internal/platform/database"
)

const seedTimeout = 30 * time.Second

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.LoadSeed()
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), seedTimeout)
	defer cancel()

	pool, err := database.Open(ctx, cfg.MySQLDSN)
	if err != nil {
		return err
	}
	defer func() { _ = pool.Close() }()

	passwords := auth.NewPasswords(auth.DefaultPasswordParams)
	created, err := auth.SeedAdmin(ctx, db.New(pool), passwords, cfg.AdminEmail, cfg.AdminPassword)
	if err != nil {
		return err
	}
	if created {
		fmt.Println("seed: created the first admin")
	} else {
		fmt.Println("seed: an admin already exists, nothing to do")
	}
	return nil
}
