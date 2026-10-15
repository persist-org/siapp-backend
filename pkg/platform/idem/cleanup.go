package idem

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"siapp/internal/db"
	"siapp/pkg/platform/database"
)

// TaskCleanup is the name of the daily worker task that calls Cleanup. The worker
// (Phase 01.2) registers it.
const TaskCleanup = "idem:cleanup"

// idempotencyKeyTTL is how long a key is remembered. It is longer than the 7-day offline
// window, so a queued item that is finally sent after a week is still recognised.
const idempotencyKeyTTL = 8 * 24 * time.Hour

// Cleanup deletes the keys older than the TTL, one organization at a time, each in its own
// tenant transaction (nothing crosses tenants). It returns how many it deleted. A failure
// in one organization does not stop the others; the first error is returned at the end.
func Cleanup(ctx context.Context, pool *pgxpool.Pool) (int64, error) {
	var orgs []int64
	if err := database.WithoutTenant(ctx, pool, func(q *db.Queries) (err error) {
		orgs, err = q.ListOrganizationIDs(ctx)
		return err
	}); err != nil {
		return 0, fmt.Errorf("idem: list organizations: %w", err)
	}

	var total int64
	var firstErr error
	for _, org := range orgs {
		err := database.WithTenant(ctx, pool, org, func(q *db.Queries) error {
			n, err := q.DeleteExpiredIdempotencyKeys(ctx, idempotencyKeyTTL.Seconds())
			total += n
			return err
		})
		if err != nil && firstErr == nil {
			firstErr = fmt.Errorf("idem: cleanup organization %d: %w", org, err)
		}
	}
	return total, firstErr
}
