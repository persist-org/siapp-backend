package auth

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"

	"github.com/hibiken/asynq"

	"siapp/internal/db"
	"siapp/internal/platform/clock"
)

// TaskCleanupSessions is the asynq task type of the daily session cleanup.
const TaskCleanupSessions = "auth:cleanup-sessions"

// SessionCleaner deletes sessions that are long dead. It is separate from Service because
// the worker needs only this, not passwords or tokens.
type SessionCleaner struct {
	q     *db.Queries
	clock clock.Clock
}

// NewSessionCleaner returns a SessionCleaner.
func NewSessionCleaner(pool *sql.DB, c clock.Clock) *SessionCleaner {
	return &SessionCleaner{q: db.New(pool), clock: c}
}

// Cleanup deletes sessions that were revoked, or that expired, more than 30 days ago, and
// returns how many it deleted.
//
// The 30 days keep recently dead sessions around so that replaying one of their refresh
// tokens is still recognised as reuse (AUTH_REFRESH_REUSED) and not just as an unknown
// token. Past that window a replay reads as unknown, which is safe: the token is dead either
// way.
//
// Rows go in batches of cleanupBatchSize. One huge DELETE would hold locks on a table that
// every login and refresh writes to.
func (c *SessionCleaner) Cleanup(ctx context.Context) (int64, error) {
	cutoff := c.clock.Now().Add(-sessionRetention)
	var total int64

	// revoked_at is nullable and expires_at is not, so sqlc types the two cutoffs differently.
	revoked := func() (int64, error) {
		return c.q.DeleteSessionsRevokedBefore(ctx, db.DeleteSessionsRevokedBeforeParams{
			Cutoff: sql.NullTime{Time: cutoff, Valid: true}, Limit: cleanupBatchSize,
		})
	}
	expired := func() (int64, error) {
		return c.q.DeleteSessionsExpiredBefore(ctx, db.DeleteSessionsExpiredBeforeParams{Cutoff: cutoff, Limit: cleanupBatchSize})
	}

	for _, deleteBatch := range []func() (int64, error){revoked, expired} {
		for {
			if err := ctx.Err(); err != nil {
				return total, fmt.Errorf("cleanup sessions: %w", err)
			}
			n, err := deleteBatch()
			if err != nil {
				return total, fmt.Errorf("delete old sessions: %w", err)
			}
			total += n
			if n < cleanupBatchSize {
				break
			}
		}
	}
	return total, nil
}

// RegisterJobs adds the auth task handlers to the worker's mux.
func RegisterJobs(mux *asynq.ServeMux, cleaner *SessionCleaner, logger *slog.Logger) {
	mux.HandleFunc(TaskCleanupSessions, func(ctx context.Context, _ *asynq.Task) error {
		deleted, err := cleaner.Cleanup(ctx)
		if err != nil {
			return err
		}
		logger.InfoContext(ctx, "session cleanup finished", slog.Int64("deleted", deleted))
		return nil
	})
}

// RegisterSchedule adds the auth periodic tasks to the scheduler.
func RegisterSchedule(scheduler *asynq.Scheduler) error {
	// 20:00 UTC is 03:00 in Jakarta, when the fewest guards are logging in. The scheduler
	// runs in UTC (see cmd/worker), so this reads the same everywhere.
	_, err := scheduler.Register("0 20 * * *", asynq.NewTask(TaskCleanupSessions, nil),
		asynq.MaxRetry(3), asynq.Timeout(cleanupTimeout))
	if err != nil {
		return fmt.Errorf("register %s: %w", TaskCleanupSessions, err)
	}
	return nil
}
