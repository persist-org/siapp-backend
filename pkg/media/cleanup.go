package media

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/hibiken/asynq"
	"github.com/jackc/pgx/v5/pgxpool"

	"siapp/internal/db"
	"siapp/pkg/platform/clock"
	"siapp/pkg/platform/database"
)

// tempCleaner is implemented by a driver that can leave half-written files behind, which is
// only the local one: S3 never exposes a partial object.
type tempCleaner interface {
	RemoveStaleTempFiles(ctx context.Context, olderThan time.Time) (int, error)
}

// Cleaner removes photos nobody attached, and their files. It is the worker's periodic task
// media:cleanup.
type Cleaner struct {
	pool    *pgxpool.Pool
	storage Storage
	clock   clock.Clock
	logger  *slog.Logger
	repo    repo
}

// NewCleaner returns a Cleaner. storage is the same driver the API uses: with the local driver
// that means the worker must see the same directory (see LocalStorage).
func NewCleaner(pool *pgxpool.Pool, storage Storage, clk clock.Clock, logger *slog.Logger) *Cleaner {
	return &Cleaner{pool: pool, storage: storage, clock: clk, logger: logger}
}

// CleanupResult is what one run removed.
type CleanupResult struct {
	// Media is the number of photos removed, rows and files.
	Media int
	// TempFiles is the number of half-written files removed (local driver only).
	TempFiles int
}

// Cleanup runs the job for every organization, then removes the stale half-written files of
// the local driver. It works one organization at a time, each in its own tenant transactions:
// nothing crosses tenants and nothing bypasses row-level security. A failure in one
// organization does not stop the others; the first error is returned at the end, so asynq
// retries the task.
func (c *Cleaner) Cleanup(ctx context.Context) (CleanupResult, error) {
	var orgs []int64
	if err := database.WithoutTenant(ctx, c.pool, func(q *db.Queries) (err error) {
		orgs, err = q.ListOrganizationIDs(ctx)
		return err
	}); err != nil {
		return CleanupResult{}, fmt.Errorf("media cleanup: list organizations: %w", err)
	}

	var result CleanupResult
	var errs []error
	for _, org := range orgs {
		n, err := c.CleanupOrganization(ctx, org)
		result.Media += n
		if err != nil {
			errs = append(errs, fmt.Errorf("media cleanup of organization %d: %w", org, err))
		}
	}

	// The temporary files belong to no organization, they sit among every organization's
	// folders, so they are swept once per run and not once per tenant.
	if tc, ok := c.storage.(tempCleaner); ok {
		n, err := tc.RemoveStaleTempFiles(ctx, c.clock.Now().Add(-staleTempFileAge))
		result.TempFiles = n
		if err != nil {
			errs = append(errs, fmt.Errorf("media cleanup of temporary files: %w", err))
		}
	}
	return result, errors.Join(errs...)
}

// CleanupOrganization removes the unattached photos of ONE organization that were registered
// more than a day ago, and returns how many it removed. Another organization's photos are
// never visible to it: it runs inside that organization's WithTenant.
//
// "Unattached after a day" covers both kinds of waste: a PENDING photo whose upload was never
// finished or confirmed, and an UPLOADED one that no record claimed (the report was never
// sent). The upload URL lives half an hour, so nothing younger than a day can still be in
// flight. Attached photos are never touched.
func (c *Cleaner) CleanupOrganization(ctx context.Context, org int64) (int, error) {
	cutoff := c.clock.Now().Add(-orphanAge)
	total := 0
	// A file the storage refuses to delete keeps its row, and the run goes on with the rest: one
	// stuck file must not hold up every orphan behind it. Its error is reported at the end so
	// that asynq retries the task.
	var firstFileErr error
	for {
		if err := ctx.Err(); err != nil {
			return total, err
		}
		removed, batch, fileErr, err := c.removeBatch(ctx, org, cutoff)
		total += removed
		if err != nil {
			return total, err
		}
		if fileErr != nil && firstFileErr == nil {
			firstFileErr = fileErr
		}
		// A short batch is the last one. A batch where nothing could be removed (the storage
		// refused every file) also ends the run: asking again would only find the same rows.
		if batch < cleanupBatchSize || removed == 0 {
			return total, firstFileErr
		}
	}
}

// removeBatch removes one batch in one transaction and returns how many photos it removed, how
// many it found, the first file the storage refused to delete (fileErr), and err when the
// batch itself failed.
//
// ⚠ The file goes first, then the row, and the rows are locked (FOR UPDATE SKIP LOCKED) for the
// whole time. If the row went first and the file deletion failed, the file would be an orphan
// with nothing left that knows its key, forever. In this order the worst case is a file that is
// already gone while its row is still there: the next run deletes the missing file again, which
// is not an error, and then the row. The lock is what keeps an Attach that arrives right now
// from claiming a photo whose file this transaction is deleting: Attach waits for the commit,
// then finds no row.
func (c *Cleaner) removeBatch(ctx context.Context, org int64, cutoff time.Time) (removed, found int, fileErr, err error) {
	err = database.WithTenant(ctx, c.pool, org, func(q *db.Queries) error {
		stale, err := c.repo.lockStale(ctx, q, cutoff, cleanupBatchSize)
		if err != nil {
			return err
		}
		found = len(stale)

		gone := make([]int64, 0, len(stale))
		for _, m := range stale {
			if err := c.deleteFile(ctx, m.Key); err != nil {
				// Keep the row so the next run tries again, and go on with the others.
				c.logger.WarnContext(ctx, "media cleanup: delete file failed, keeping the row",
					slog.Int64("organizationId", org), slog.Int64("mediaId", m.ID), slog.String("error", err.Error()))
				if fileErr == nil {
					fileErr = fmt.Errorf("delete file of media %d: %w", m.ID, err)
				}
				continue
			}
			gone = append(gone, m.ID)
		}
		if len(gone) == 0 {
			return nil
		}
		n, err := c.repo.deleteRows(ctx, q, gone)
		removed = int(n)
		return err
	})
	if err != nil {
		return 0, found, fileErr, err
	}
	return removed, found, fileErr, nil
}

// deleteFile deletes one object with a deadline of its own. The batch holds row locks while it
// runs, and an Attach of one of those rows waits for them: a storage that hangs must cost that
// Attach seconds, not the job's whole half hour.
func (c *Cleaner) deleteFile(ctx context.Context, key string) error {
	ctx, cancel := context.WithTimeout(ctx, cleanupDeleteTimeout)
	defer cancel()
	return c.storage.Delete(ctx, key)
}

// RegisterJobs adds the media task handlers to the worker's mux.
func RegisterJobs(mux *asynq.ServeMux, cleaner *Cleaner, logger *slog.Logger) {
	mux.HandleFunc(TaskCleanup, func(ctx context.Context, _ *asynq.Task) error {
		result, err := cleaner.Cleanup(ctx)
		// Report what was removed even when something failed: the next run picks up the rest.
		logger.InfoContext(ctx, "media cleanup finished",
			slog.Int("media", result.Media), slog.Int("tempFiles", result.TempFiles))
		return err
	})
}

// RegisterSchedule adds the periodic cleanup to the scheduler.
func RegisterSchedule(scheduler *asynq.Scheduler) error {
	if _, err := scheduler.Register(CleanupSchedule, asynq.NewTask(TaskCleanup, nil),
		asynq.MaxRetry(3), asynq.Timeout(cleanupTimeout)); err != nil {
		return fmt.Errorf("register %s: %w", TaskCleanup, err)
	}
	return nil
}
