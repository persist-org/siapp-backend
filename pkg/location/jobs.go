package location

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/hibiken/asynq"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"siapp/internal/db"
	"siapp/pkg/platform/clock"
)

// The worker tasks.
const (
	TaskPartitions = "location:partitions"
	TaskRetention  = "location:retention"
)

const (
	// locationRetention is how long location history is kept (30 days by default). The data
	// retention design (persist-ecosystem docs/08-meeting-notes/gap-designs/04-data-retention.md)
	// proposes 90; change this constant when that decision is adopted.
	locationRetention = 30 * 24 * time.Hour
	// partitionsAhead is how many months beyond the current one stay ready. The migration creates
	// the current month and the next three; the monthly task keeps the window moving.
	partitionsAhead = 3
	jobTimeout      = 5 * time.Minute
)

// The cron expressions, in UTC like every schedule here, staggered after the platform tasks
// (20:00-20:20 UTC): partitions on the 1st of every month, retention daily.
const (
	PartitionsSchedule = "30 20 1 * *"
	RetentionSchedule  = "40 20 * * *"
)

// Maintenance keeps the partitions of location_logs: ahead for the writes, and away for the
// retention.
type Maintenance struct {
	pool  *pgxpool.Pool
	clock clock.Clock
}

// NewMaintenance returns the maintenance job runner.
func NewMaintenance(pool *pgxpool.Pool, c clock.Clock) *Maintenance {
	return &Maintenance{pool: pool, clock: c}
}

// EnsurePartitions makes sure the partitions of the current month and the next partitionsAhead
// exist. It is idempotent. It runs through a SECURITY DEFINER function because the app role cannot
// create tables itself.
func (m *Maintenance) EnsurePartitions(ctx context.Context) error {
	now := m.clock.Now().UTC()
	first := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	q := db.New(m.pool)
	for i := 0; i <= partitionsAhead; i++ {
		month := first.AddDate(0, i, 0)
		if err := q.EnsureLocationPartition(ctx, pgtype.Date{Time: month, Valid: true}); err != nil {
			return fmt.Errorf("location: ensure partition %s: %w", month.Format("2006-01"), err)
		}
	}
	return nil
}

// DropExpired drops the monthly partitions that lie entirely before the retention window and
// returns how many.
//
// ⚠ Partitions are dropped instead of rows deleted: a DROP is instant, takes no row locks and
// leaves no dead tuples to vacuum, where deleting a month of GPS points would run for minutes,
// bloat the table and hammer the write path. The price is granularity: a month goes only when its
// last day is older than locationRetention, so history is kept for between 30 and about 60 days.
func (m *Maintenance) DropExpired(ctx context.Context) (int, error) {
	cutoff := m.clock.Now().Add(-locationRetention)
	n, err := db.New(m.pool).DropLocationPartitions(ctx, pgtype.Timestamptz{Time: cutoff, Valid: true})
	if err != nil {
		return 0, fmt.Errorf("location: drop expired partitions: %w", err)
	}
	return int(n), nil
}

// RegisterJobs adds the two tasks to the worker's mux.
func RegisterJobs(mux *asynq.ServeMux, m *Maintenance, logger *slog.Logger) {
	mux.HandleFunc(TaskPartitions, func(ctx context.Context, _ *asynq.Task) error {
		if err := m.EnsurePartitions(ctx); err != nil {
			return err
		}
		logger.InfoContext(ctx, "location partitions ensured")
		return nil
	})
	mux.HandleFunc(TaskRetention, func(ctx context.Context, _ *asynq.Task) error {
		dropped, err := m.DropExpired(ctx)
		if err != nil {
			return err
		}
		logger.InfoContext(ctx, "location retention finished", slog.Int("droppedPartitions", dropped))
		return nil
	})
}

// RegisterSchedule adds the periodic tasks to the scheduler.
func RegisterSchedule(scheduler *asynq.Scheduler) error {
	for _, t := range []struct{ spec, task string }{{PartitionsSchedule, TaskPartitions}, {RetentionSchedule, TaskRetention}} {
		if _, err := scheduler.Register(t.spec, asynq.NewTask(t.task, nil), asynq.MaxRetry(3), asynq.Timeout(jobTimeout)); err != nil {
			return fmt.Errorf("register %s: %w", t.task, err)
		}
	}
	return nil
}
