package identity

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/hibiken/asynq"
	"github.com/jackc/pgx/v5/pgxpool"

	"siapp/internal/db"
	"siapp/pkg/platform/auth"
	"siapp/pkg/platform/clock"
	"siapp/pkg/platform/database"
	"siapp/pkg/platform/personel"
)

// TaskResyncPeople is the worker task that runs Resyncer.Resync.
const TaskResyncPeople = "identity:resync-people"

const (
	// resyncStaleAfter is how old a cached person may get before the daily job asks
	// Personalia about them.
	resyncStaleAfter = 24 * time.Hour
	resyncBatchSize  = 200
	resyncTimeout    = 30 * time.Minute
)

// personelEmployees is the part of the Personel client the resync needs (seam S4).
type personelEmployees interface {
	EmployeesByNIKs(ctx context.Context, orgID int64, niks []string) ([]personel.Identity, error)
}

// Resyncer keeps the cache of people fresh and ends the sessions of people who left.
type Resyncer struct {
	pool        *pgxpool.Pool
	directory   personelEmployees
	revocations *auth.Revocations
	clock       clock.Clock
	logger      *slog.Logger
	repo        repo
}

// NewResyncer returns a Resyncer.
func NewResyncer(pool *pgxpool.Pool, directory personelEmployees, revocations *auth.Revocations, c clock.Clock, logger *slog.Logger) *Resyncer {
	return &Resyncer{pool: pool, directory: directory, revocations: revocations, clock: c, logger: logger}
}

// Resync re-fetches every active person whose cached copy is older than 24 hours, one
// organization at a time, in batches of 200 NIKs (one EmployeesByNIKs call per batch). It
// returns how many people it looked at.
//
// A person Personalia no longer returns, reports inactive, or reports under another company
// (or under another Personalia account for the same NIK) is marked INACTIVE and ALL their
// sessions end at once: the refresh tokens die, the access tokens go on the revocation list,
// and their push-token devices are deleted. Everyone else gets the fresh data and a new
// synced_at. People are not matched against Personalia's devices: SiAPP binds phones itself.
//
// This is the safety net. The Personalia webhook (Phase 05.3) will deliver a deactivation
// within seconds; without it, this daily job (plus the 12-hour check on refresh) is what
// bounds how long a person who left keeps access.
//
// If Personalia cannot answer, the run for that organization stops and the error is returned,
// so the scheduler retries; nobody is marked inactive on the strength of an outage. A failure
// in one organization does not stop the others; the first error is returned at the end.
func (r *Resyncer) Resync(ctx context.Context) (int64, error) {
	var orgs []int64
	if err := database.WithoutTenant(ctx, r.pool, func(q *db.Queries) (err error) {
		orgs, err = q.ListOrganizationIDs(ctx)
		return err
	}); err != nil {
		return 0, fmt.Errorf("resync people: list organizations: %w", err)
	}

	var total int64
	var firstErr error
	for _, org := range orgs {
		n, err := r.resyncOrganization(ctx, org)
		total += n
		if err != nil && firstErr == nil {
			firstErr = fmt.Errorf("resync people of organization %d: %w", org, err)
		}
	}
	return total, firstErr
}

func (r *Resyncer) resyncOrganization(ctx context.Context, org int64) (int64, error) {
	cutoff := ts(r.clock.Now().Add(-resyncStaleAfter))
	var done, afterID int64
	for {
		if err := ctx.Err(); err != nil {
			return done, err
		}
		var batch []db.Personnel
		if err := database.WithTenant(ctx, r.pool, org, func(q *db.Queries) (err error) {
			batch, err = q.ListStalePersonnel(ctx, db.ListStalePersonnelParams{Cutoff: cutoff, AfterID: afterID, Batch: resyncBatchSize})
			return err
		}); err != nil {
			return done, fmt.Errorf("list stale personnel: %w", err)
		}
		if len(batch) == 0 {
			return done, nil
		}

		niks := make([]string, 0, len(batch))
		for _, p := range batch {
			niks = append(niks, p.Nik.String)
		}
		people, err := r.directory.EmployeesByNIKs(ctx, org, niks)
		if err != nil {
			return done, fmt.Errorf("personel employee lookup: %w", err)
		}
		byNIK := make(map[string]personel.Identity, len(people))
		for _, p := range people {
			byNIK[p.NIK] = p
		}

		now := r.clock.Now()
		var cutOff []int64
		var gone int
		if err := database.WithTenant(ctx, r.pool, org, func(q *db.Queries) error {
			for _, p := range batch {
				fresh, found := byNIK[p.Nik.String]
				// Gone: not returned, inactive, employed elsewhere now, or a NIK that belongs
				// to another Personalia account than the one this person was cached from.
				if !found || !fresh.Active || fresh.OrganizationID != org || fresh.PersonelUserID != p.PersonelUserID {
					if err := r.repo.markInactive(ctx, q, p.ID, now); err != nil {
						return err
					}
					ids, err := r.repo.endAllSessions(ctx, q, p.ID, now)
					if err != nil {
						return err
					}
					cutOff = append(cutOff, ids...)
					gone++
					continue
				}
				if err := r.repo.refreshPersonnel(ctx, q, p.ID, fresh, now); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			return done, err
		}
		// After the commit, like every side effect: their access tokens die at once.
		r.revocations.Revoke(ctx, cutOff...)
		if gone > 0 {
			r.logger.InfoContext(ctx, "resync: people no longer active in Personalia were deactivated",
				slog.Int64("organization_id", org), slog.Int("people", gone))
		}

		done += int64(len(batch))
		afterID = batch[len(batch)-1].ID
	}
}

// RegisterResyncJob adds the resync task handler to the worker's mux.
func RegisterResyncJob(mux *asynq.ServeMux, resyncer *Resyncer, logger *slog.Logger) {
	mux.HandleFunc(TaskResyncPeople, func(ctx context.Context, _ *asynq.Task) error {
		looked, err := resyncer.Resync(ctx)
		if err != nil {
			return err
		}
		logger.InfoContext(ctx, "people resync finished", slog.Int64("people", looked))
		return nil
	})
}

// RegisterResyncSchedule adds the daily resync to the scheduler, after the session cleanup.
func RegisterResyncSchedule(scheduler *asynq.Scheduler) error {
	if _, err := scheduler.Register("30 20 * * *", asynq.NewTask(TaskResyncPeople, nil),
		asynq.MaxRetry(3), asynq.Timeout(resyncTimeout)); err != nil {
		return fmt.Errorf("register %s: %w", TaskResyncPeople, err)
	}
	return nil
}
