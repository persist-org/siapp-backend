package identity

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/hibiken/asynq"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"siapp/internal/db"
	"siapp/pkg/platform/clock"
	"siapp/pkg/platform/database"
)

// TaskCleanupSessions is the worker task that runs Cleaner.Cleanup.
const TaskCleanupSessions = "auth:cleanup-sessions"

// Cleaner deletes sessions that are long dead.
type Cleaner struct {
	pool  *pgxpool.Pool
	clock clock.Clock
}

// NewCleaner returns a Cleaner.
func NewCleaner(pool *pgxpool.Pool, c clock.Clock) *Cleaner {
	return &Cleaner{pool: pool, clock: c}
}

// Cleanup deletes the sessions that were revoked, or that expired, more than 30 days ago
// (see sessionRetention) and returns how many it deleted.
//
// It works one organization at a time, each in its own tenant transactions: nothing crosses
// tenants, and nothing bypasses row-level security. Rows go in batches of cleanupBatchSize,
// one transaction per batch, because one huge DELETE would hold locks on a table that every
// login and refresh writes to. A failure in one organization does not stop the others; the
// first error is returned at the end.
func (c *Cleaner) Cleanup(ctx context.Context) (int64, error) {
	var orgs []int64
	if err := database.WithoutTenant(ctx, c.pool, func(q *db.Queries) (err error) {
		orgs, err = q.ListOrganizationIDs(ctx)
		return err
	}); err != nil {
		return 0, fmt.Errorf("cleanup sessions: list organizations: %w", err)
	}

	cutoff := pgtype.Timestamptz{Time: c.clock.Now().Add(-sessionRetention), Valid: true}
	var total int64
	var firstErr error
	for _, org := range orgs {
		for _, deleteBatch := range []func(*db.Queries) (int64, error){
			func(q *db.Queries) (int64, error) {
				return q.DeleteSessionsRevokedBefore(ctx, db.DeleteSessionsRevokedBeforeParams{Cutoff: cutoff, Batch: cleanupBatchSize})
			},
			func(q *db.Queries) (int64, error) {
				return q.DeleteSessionsExpiredBefore(ctx, db.DeleteSessionsExpiredBeforeParams{Cutoff: cutoff, Batch: cleanupBatchSize})
			},
		} {
			n, err := c.deleteAll(ctx, org, deleteBatch)
			total += n
			if err != nil && firstErr == nil {
				firstErr = fmt.Errorf("cleanup sessions of organization %d: %w", org, err)
			}
		}
	}
	return total, firstErr
}

// deleteAll runs deleteBatch until a batch comes back short.
func (c *Cleaner) deleteAll(ctx context.Context, org int64, deleteBatch func(*db.Queries) (int64, error)) (int64, error) {
	var total int64
	for {
		if err := ctx.Err(); err != nil {
			return total, err
		}
		var n int64
		if err := database.WithTenant(ctx, c.pool, org, func(q *db.Queries) (err error) {
			n, err = deleteBatch(q)
			return err
		}); err != nil {
			return total, err
		}
		total += n
		if n < cleanupBatchSize {
			return total, nil
		}
	}
}

// RegisterJobs adds the identity task handlers to the worker's mux.
func RegisterJobs(mux *asynq.ServeMux, cleaner *Cleaner, logger *slog.Logger) {
	mux.HandleFunc(TaskCleanupSessions, func(ctx context.Context, _ *asynq.Task) error {
		deleted, err := cleaner.Cleanup(ctx)
		if err != nil {
			return err
		}
		logger.InfoContext(ctx, "session cleanup finished", slog.Int64("deleted", deleted))
		return nil
	})
}

// RegisterSchedule adds the identity periodic tasks to the scheduler.
func RegisterSchedule(scheduler *asynq.Scheduler) error {
	// 20:00 UTC is 03:00 in Jakarta, when the fewest guards are logging in. The scheduler
	// runs in UTC (see cmd/worker), so this reads the same everywhere.
	if _, err := scheduler.Register("0 20 * * *", asynq.NewTask(TaskCleanupSessions, nil),
		asynq.MaxRetry(3), asynq.Timeout(cleanupTimeout)); err != nil {
		return fmt.Errorf("register %s: %w", TaskCleanupSessions, err)
	}
	return nil
}
