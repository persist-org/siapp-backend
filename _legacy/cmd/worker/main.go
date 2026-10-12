// Command worker runs the background jobs: an asynq server that executes tasks, and a
// scheduler that enqueues the periodic ones. It only wires dependencies together.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"
	_ "time/tzdata" // see cmd/api: time zones must work in a container without zoneinfo

	"github.com/hibiken/asynq"

	"siapp/internal/platform/auth"
	"siapp/internal/platform/clock"
	"siapp/internal/platform/config"
	"siapp/internal/platform/database"
)

const (
	startupTimeout = 30 * time.Second
	concurrency    = 10
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.LoadWorker()
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

	redisOpt, err := asynq.ParseRedisURI(cfg.RedisURL)
	if err != nil {
		return fmt.Errorf("parse REDIS_URL: %w", err)
	}

	// Handlers: each module registers the task types it owns.
	mux := asynq.NewServeMux()
	auth.RegisterJobs(mux, auth.NewSessionCleaner(pool, clock.System{}), logger)

	server := asynq.NewServer(redisOpt, asynq.Config{
		Concurrency: concurrency,
		Logger:      slogAdapter{logger},
	})

	// Periodic tasks. The scheduler only enqueues; the server above runs them. Cron times
	// are read in UTC so they mean the same thing on every host.
	scheduler := asynq.NewScheduler(redisOpt, &asynq.SchedulerOpts{
		Location: time.UTC,
		Logger:   slogAdapter{logger},
	})
	if err := auth.RegisterSchedule(scheduler); err != nil {
		return err
	}

	if err := server.Start(mux); err != nil {
		return fmt.Errorf("start asynq server: %w", err)
	}
	if err := scheduler.Start(); err != nil {
		server.Shutdown()
		return fmt.Errorf("start asynq scheduler: %w", err)
	}
	logger.Info("worker started")

	<-ctx.Done()
	logger.Info("worker shutting down")
	scheduler.Shutdown()
	server.Shutdown() // waits for running tasks, up to asynq's shutdown timeout
	logger.Info("worker stopped")
	return nil
}

// slogAdapter lets asynq log through slog, so worker logs are JSON like the API's.
type slogAdapter struct{ l *slog.Logger }

func (a slogAdapter) Debug(args ...any) { a.l.Debug(fmt.Sprint(args...)) }
func (a slogAdapter) Info(args ...any)  { a.l.Info(fmt.Sprint(args...)) }
func (a slogAdapter) Warn(args ...any)  { a.l.Warn(fmt.Sprint(args...)) }
func (a slogAdapter) Error(args ...any) { a.l.Error(fmt.Sprint(args...)) }
func (a slogAdapter) Fatal(args ...any) {
	a.l.Error(fmt.Sprint(args...))
	os.Exit(1)
}
