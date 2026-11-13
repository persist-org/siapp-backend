package patrol

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/hibiken/asynq"
	"github.com/jackc/pgx/v5"

	"siapp/internal/db"
	"siapp/pkg/platform/database"
)

// The worker tasks of the patrol route.
const (
	TaskAutoEnd   = "patrol:auto-end"
	TaskRetention = "patrol:retention"
)

// The cron expressions, in UTC like every schedule here: auto-end every 15 minutes, retention daily,
// staggered after the other retention tasks (location at 20:40 UTC).
const (
	AutoEndSchedule   = "*/15 * * * *"
	RetentionSchedule = "50 20 * * *"
)

const (
	// patrolStaleAfter is how long a running patrol may log nothing before it is ended for the guard.
	//
	// ⚠ A guard whose phone died, or who forgot to tap "end", must not be locked out of tomorrow's
	// round by PATROL_ALREADY_ACTIVE. Six hours is longer than a round and shorter than a shift
	// change, so a patrol is never ended under a guard who is still walking it.
	patrolStaleAfter = 6 * time.Hour
	// pointRetention is how long the raw points of an ended patrol are kept. By then the stored
	// path is the record.
	pointRetention = 7 * 24 * time.Hour
	// autoEndBatch is how many stale patrols one run ends per organization; the rest wait for the
	// next run, fifteen minutes on.
	autoEndBatch = 200
	// purgeBatch is how many patrols' points one delete removes: a patrol holds up to tens of
	// thousands of rows, and a delete of a whole month at once would hold locks for minutes.
	purgeBatch = 20
	jobTimeout = 10 * time.Minute
)

// organizations lists every organization, for a job that works one tenant at a time.
func (s *Service) organizations(ctx context.Context) ([]int64, error) {
	var orgs []int64
	err := database.WithoutTenant(ctx, s.pool, func(q *db.Queries) (err error) {
		orgs, err = q.ListOrganizationIDs(ctx)
		return err
	})
	return orgs, err
}

// AutoEndStale ends the running patrols that logged nothing for patrolStaleAfter, as AUTO_ENDED, with
// ended_at at their last point (the guard was last seen then, not when the job noticed). It returns
// how many it ended.
//
// It works one organization at a time, each patrol in a transaction of its own: nothing crosses
// tenants and nothing bypasses row-level security. A failure on one patrol does not stop the others;
// the errors are returned together, so asynq retries the task.
func (s *Service) AutoEndStale(ctx context.Context) (int, error) {
	orgs, err := s.organizations(ctx)
	if err != nil {
		return 0, fmt.Errorf("patrol auto-end: list organizations: %w", err)
	}
	cutoff := s.clock.Now().Add(-patrolStaleAfter)
	ended := 0
	var errs []error
	for _, org := range orgs {
		n, err := s.autoEndOrganization(ctx, org, cutoff)
		ended += n
		if err != nil {
			errs = append(errs, fmt.Errorf("patrol auto-end of organization %d: %w", org, err))
		}
	}
	return ended, errors.Join(errs...)
}

func (s *Service) autoEndOrganization(ctx context.Context, org int64, cutoff time.Time) (int, error) {
	var stale []int64
	if err := database.WithTenant(ctx, s.pool, org, func(q *db.Queries) (err error) {
		stale, err = q.ListStaleRoutes(ctx, db.ListStaleRoutesParams{Before: ts(cutoff), Batch: autoEndBatch})
		return err
	}); err != nil {
		return 0, err
	}
	ended := 0
	var errs []error
	for _, id := range stale {
		ok, err := s.autoEnd(ctx, org, id, cutoff)
		if err != nil {
			errs = append(errs, fmt.Errorf("patrol %d: %w", id, err))
			continue
		}
		if ok {
			ended++
		}
	}
	return ended, errors.Join(errs...)
}

// autoEnd ends one stale patrol, under its lock. It looks again inside the lock: the guard may have
// sent a point or ended the patrol between the list and now.
func (s *Service) autoEnd(ctx context.Context, org, id int64, cutoff time.Time) (bool, error) {
	ended := false
	err := database.WithTenant(ctx, s.pool, org, func(q *db.Queries) error {
		if err := q.LockPatrolKey(ctx, routeLockKey(org, id)); err != nil {
			return fmt.Errorf("lock patrol: %w", err)
		}
		row, err := q.GetPatrolRoute(ctx, id)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("get patrol: %w", err)
		}
		if row.Status != db.PatrolRouteStatusACTIVE || !row.LastPointAt.Time.Before(cutoff) {
			return nil
		}
		if _, err := s.finish(ctx, q, row, db.PatrolRouteStatusAUTOENDED, row.LastPointAt.Time); err != nil {
			return err
		}
		ended = true
		return nil
	})
	return ended, err
}

// PurgePoints deletes the raw points of the patrols that ended more than pointRetention ago, and
// returns how many rows it removed. Their stored path stays: it is the record, kept as long as the
// scans of the patrol.
//
// Like AutoEndStale it works one organization at a time, and a few patrols per delete.
func (s *Service) PurgePoints(ctx context.Context) (int64, error) {
	orgs, err := s.organizations(ctx)
	if err != nil {
		return 0, fmt.Errorf("patrol retention: list organizations: %w", err)
	}
	cutoff := s.clock.Now().Add(-pointRetention)
	var removed int64
	var errs []error
	for _, org := range orgs {
		n, err := s.purgeOrganization(ctx, org, cutoff)
		removed += n
		if err != nil {
			errs = append(errs, fmt.Errorf("patrol retention of organization %d: %w", org, err))
		}
	}
	return removed, errors.Join(errs...)
}

func (s *Service) purgeOrganization(ctx context.Context, org int64, cutoff time.Time) (int64, error) {
	var removed int64
	for {
		if err := ctx.Err(); err != nil {
			return removed, err
		}
		var n int64
		var found int
		if err := database.WithTenant(ctx, s.pool, org, func(q *db.Queries) error {
			ids, err := q.ListRoutesWithOldPoints(ctx, db.ListRoutesWithOldPointsParams{Before: ts(cutoff), Batch: purgeBatch})
			if err != nil {
				return err
			}
			found = len(ids)
			if found == 0 {
				return nil
			}
			n, err = q.DeleteRoutePoints(ctx, ids)
			return err
		}); err != nil {
			return removed, err
		}
		removed += n
		// A batch that came back short was the last: the patrols it held no longer list.
		if found < purgeBatch {
			return removed, nil
		}
	}
}

// RegisterJobs adds the patrol tasks to the worker's mux.
func RegisterJobs(mux *asynq.ServeMux, s *Service, logger *slog.Logger) {
	mux.HandleFunc(TaskAutoEnd, func(ctx context.Context, _ *asynq.Task) error {
		ended, err := s.AutoEndStale(ctx)
		// Report what was ended even when something failed: the next run picks up the rest.
		logger.InfoContext(ctx, "patrol auto-end finished", slog.Int("ended", ended))
		return err
	})
	mux.HandleFunc(TaskRetention, func(ctx context.Context, _ *asynq.Task) error {
		removed, err := s.PurgePoints(ctx)
		logger.InfoContext(ctx, "patrol retention finished", slog.Int64("removedPoints", removed))
		return err
	})
}

// RegisterSchedule adds the periodic patrol tasks to the scheduler.
func RegisterSchedule(scheduler *asynq.Scheduler) error {
	for _, t := range []struct{ spec, task string }{{AutoEndSchedule, TaskAutoEnd}, {RetentionSchedule, TaskRetention}} {
		if _, err := scheduler.Register(t.spec, asynq.NewTask(t.task, nil), asynq.MaxRetry(3), asynq.Timeout(jobTimeout)); err != nil {
			return fmt.Errorf("register %s: %w", t.task, err)
		}
	}
	return nil
}
