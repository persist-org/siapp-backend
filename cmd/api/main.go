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

	"siapp/internal/app"
	"siapp/internal/mail"
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

	redisOpts, err := redis.ParseURL(cfg.RedisURL)
	if err != nil {
		return fmt.Errorf("parse REDIS_URL: %w", err)
	}
	rdb := redis.NewClient(redisOpts)
	defer func() { _ = rdb.Close() }()
	if err := rdb.Ping(startCtx).Err(); err != nil {
		return fmt.Errorf("ping redis: %w", err)
	}

	// Config already checked SMTP_TLS, so the parse cannot fail here.
	tlsMode, err := mail.ParseTLSMode(cfg.SMTPTLS)
	if err != nil {
		return err
	}
	smtp, err := mail.NewSMTPSender(mail.SMTPConfig{
		Host: cfg.SMTPHost, Port: cfg.SMTPPort, Username: cfg.SMTPUsername, Password: cfg.SMTPPassword,
		From: cfg.SMTPFrom, TLS: tlsMode,
	})
	if err != nil {
		return err
	}
	mailer := mail.NewAsync(smtp, logger)

	router := app.NewRouter(app.Deps{
		Logger:         logger,
		DB:             pool,
		Redis:          rdb,
		JWTSecret:      []byte(cfg.JWTSecret),
		CORSOrigins:    cfg.CORSOrigins,
		ClientIPHeader: cfg.ClientIPHeader,
		Mailer:         mailer,
		ResetURL:       cfg.AppResetURL,
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
	// Mail is sent in the background. Now that no request can start a new delivery, give the
	// ones still under way a chance to finish, so a reset link asked for just before a
	// restart is not silently lost.
	if err := mailer.Wait(shutdownCtx); err != nil {
		logger.Warn("some emails may not have been sent before shutdown", "error", err)
	}
	logger.Info("shutdown complete")
	return nil
}
