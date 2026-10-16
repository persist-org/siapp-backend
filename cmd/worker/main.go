// Command worker runs the background jobs: an asynq server that executes tasks and a
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

	"github.com/hibiken/asynq"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	// The scheduler's cron expressions are in UTC, see below; the embedded timezone database
	// keeps time.LoadLocation working in the distroless image like in cmd/api.
	_ "time/tzdata"

	"siapp/pkg/identity"
	"siapp/pkg/platform/audit"
	"siapp/pkg/platform/auth"
	"siapp/pkg/platform/clock"
	"siapp/pkg/platform/config"
	"siapp/pkg/platform/database"
	"siapp/pkg/platform/idem"
	"siapp/pkg/platform/personel"
)

const workerConcurrency = 5

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

	pool, err := database.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()

	redisOpt, err := asynq.ParseRedisURI(cfg.RedisURL)
	if err != nil {
		// The message of a parse error can quote the URL, which holds a password.
		return fmt.Errorf("REDIS_URL cannot be parsed, check its format")
	}

	// The resync reads Personalia and ends the sessions of people who left, so it needs the
	// Personel client and the revocation list (which lives in the same Redis as the queue).
	personelClient, err := personel.New(personel.Config{
		BaseURL: cfg.PersonelBaseURL, Key: cfg.PersonelIntegrationKey, Logger: logger,
	})
	if err != nil {
		return fmt.Errorf("personel client: %w", err)
	}
	redisClientOpts, err := redis.ParseURL(cfg.RedisURL)
	if err != nil {
		return fmt.Errorf("REDIS_URL cannot be parsed, check its format")
	}
	rdb := redis.NewClient(redisClientOpts)
	defer func() { _ = rdb.Close() }()
	resyncer := identity.NewResyncer(pool, personelClient, auth.NewRevocations(rdb, config.RedisKeyPrefix, logger), clock.System{}, logger)

	srv := asynq.NewServer(redisOpt, asynq.Config{Concurrency: workerConcurrency})
	mux := asynq.NewServeMux()
	identity.RegisterJobs(mux, identity.NewCleaner(pool, clock.System{}), logger)
	identity.RegisterResyncJob(mux, resyncer, logger)
	registerPlatformJobs(mux, pool, logger)

	// ⚠ The scheduler runs in UTC, so the cron expressions below read the same on every host.
	scheduler := asynq.NewScheduler(redisOpt, &asynq.SchedulerOpts{Location: time.UTC})
	if err := identity.RegisterSchedule(scheduler); err != nil {
		return err
	}
	if err := identity.RegisterResyncSchedule(scheduler); err != nil {
		return err
	}
	if err := registerPlatformSchedule(scheduler); err != nil {
		return err
	}

	if err := srv.Start(mux); err != nil {
		return fmt.Errorf("start worker: %w", err)
	}
	if err := scheduler.Start(); err != nil {
		srv.Shutdown()
		return fmt.Errorf("start scheduler: %w", err)
	}
	logger.Info("worker running", "env", cfg.AppEnv)

	<-ctx.Done()
	logger.Info("shutting down")
	scheduler.Shutdown()
	srv.Shutdown() // waits for tasks in flight
	logger.Info("shutdown complete")
	return nil
}

// registerPlatformJobs adds the tasks that Phase 00.4 built and left for the worker: the
// idempotency key cleanup and the monthly audit partitions.
func registerPlatformJobs(mux *asynq.ServeMux, pool *pgxpool.Pool, logger *slog.Logger) {
	mux.HandleFunc(idem.TaskCleanup, func(ctx context.Context, _ *asynq.Task) error {
		deleted, err := idem.Cleanup(ctx, pool)
		if err != nil {
			return err
		}
		logger.InfoContext(ctx, "idempotency cleanup finished", slog.Int64("deleted", deleted))
		return nil
	})
	mux.HandleFunc(audit.TaskEnsurePartitions, func(ctx context.Context, _ *asynq.Task) error {
		if err := audit.EnsurePartitions(ctx, pool, clock.System{}); err != nil {
			return err
		}
		logger.InfoContext(ctx, "audit partitions ensured")
		return nil
	})
}

// registerPlatformSchedule staggers the platform tasks after the session cleanup (20:00 UTC).
func registerPlatformSchedule(scheduler *asynq.Scheduler) error {
	if _, err := scheduler.Register("10 20 * * *", asynq.NewTask(idem.TaskCleanup, nil),
		asynq.MaxRetry(3), asynq.Timeout(10*time.Minute)); err != nil {
		return fmt.Errorf("register %s: %w", idem.TaskCleanup, err)
	}
	// The 1st of every month: the audit window is kept three months ahead, so even a few
	// missed runs leave room.
	if _, err := scheduler.Register("20 20 1 * *", asynq.NewTask(audit.TaskEnsurePartitions, nil),
		asynq.MaxRetry(3), asynq.Timeout(time.Minute)); err != nil {
		return fmt.Errorf("register %s: %w", audit.TaskEnsurePartitions, err)
	}
	return nil
}
