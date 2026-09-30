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

	"siapp/internal/platform/config"
	"siapp/internal/platform/database"
	"siapp/internal/platform/httpx"
)

const (
	startupTimeout  = 30 * time.Second
	shutdownTimeout = 20 * time.Second
)

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

	startCtx, cancelStart := context.WithTimeout(ctx, startupTimeout)
	defer cancelStart()
	pool, err := database.Open(startCtx, cfg.MySQLDSN)
	if err != nil {
		return err
	}
	defer func() { _ = pool.Close() }()

	if cfg.MigrateOnStart {
		if err := database.Migrate(startCtx, pool); err != nil {
			return err
		}
		logger.Info("migrations applied")
	}

	router := httpx.NewRouter(httpx.RouterConfig{Logger: logger, CORSOrigins: cfg.CORSOrigins, DB: pool})
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
