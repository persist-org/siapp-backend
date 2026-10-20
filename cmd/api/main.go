// Command api runs the SiAPP HTTP API. It only wires dependencies together.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	// Embeds the IANA timezone database into the binary. The distroless runtime image has
	// no /usr/share/zoneinfo, and without this time.LoadLocation("Asia/Jayapura") fails
	// there, which would break every business-date calculation in project timezones.
	_ "time/tzdata"

	"github.com/redis/go-redis/v9"

	"siapp/pkg/platform/auth"
	"siapp/pkg/platform/clock"
	"siapp/pkg/platform/config"
	"siapp/pkg/platform/database"
	"siapp/pkg/platform/httpx"
	"siapp/pkg/platform/personel"
)

const shutdownTimeout = 20 * time.Second

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: cfg.LogLevel}))
	slog.SetDefault(logger)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Dev convenience only; config refuses it in production. It uses the owner DSN, which
	// the API otherwise never touches.
	if cfg.MigrateOnStart {
		logger.Info("applying migrations (MIGRATE_ON_START)")
		if err := database.Migrate(ctx, cfg.MigrateDSN, "up"); err != nil {
			return err
		}
	}

	pool, err := database.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()

	redisOpts, err := redis.ParseURL(cfg.RedisURL)
	if err != nil {
		// The message of a parse error can quote the URL, which holds a password.
		return fmt.Errorf("REDIS_URL cannot be parsed, check its format")
	}
	rdb := redis.NewClient(redisOpts)
	defer func() { _ = rdb.Close() }()

	personelClient, err := personel.New(personel.Config{
		BaseURL: cfg.PersonelBaseURL,
		Key:     cfg.PersonelIntegrationKey,
		Group: personel.Group{
			CompanyIDs: cfg.GroupCompanyIDs, OrganizationID: cfg.GroupOrganizationID, OrganizationName: cfg.GroupOrganizationName,
		},
		Logger: logger,
	})
	if err != nil {
		return fmt.Errorf("personel client: %w", err)
	}
	tokens, err := auth.NewTokens([]byte(cfg.JWTSecret), clock.System{})
	if err != nil {
		return err
	}

	router := newRouter(deps{
		Logger:      logger,
		CORSOrigins: cfg.CORSOrigins,
		Pool:        pool,
		Redis:       rdb,
		RedisPrefix: config.RedisKeyPrefix,
		Personel:    personelClient,
		Tokens:      tokens,
		Clock:       clock.System{},
		ClientIP:    httpx.ClientIPFunc(cfg.ClientIPHeader),
	})
	srv := httpx.NewServer(cfg.HTTPAddr, router)

	serveErr := make(chan error, 1)
	go func() {
		logger.Info("http server listening", "addr", cfg.HTTPAddr, "env", cfg.AppEnv)
		serveErr <- srv.ListenAndServe()
	}()

	select {
	case err := <-serveErr:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return fmt.Errorf("serve http: %w", err)
	case <-ctx.Done():
	}

	// Stop accepting new connections and let in-flight requests finish.
	logger.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("shutdown http server: %w", err)
	}
	logger.Info("shutdown complete")
	return nil
}
