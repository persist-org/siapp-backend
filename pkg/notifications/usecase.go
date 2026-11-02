package notifications

import (
	"context"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"siapp/internal/db"
	"siapp/pkg/platform/auth"
	"siapp/pkg/platform/clock"
	"siapp/pkg/platform/database"
)

// Service is the notification module's use cases: Notify, which every other module calls, and
// the inbox a person reads. Every inbox method works on the caller's own notifications only;
// the caller is the auth.Principal it is handed, never an id from the request.
type Service struct {
	pool       *pgxpool.Pool
	clock      clock.Clock
	logger     *slog.Logger
	repo       repo
	dispatcher Dispatcher
}

// NewService returns the service. Until UseDispatcher, a notification is only written to the
// inbox.
func NewService(pool *pgxpool.Pool, clk clock.Clock, logger *slog.Logger) *Service {
	return &Service{pool: pool, clock: clk, logger: logger, dispatcher: noopDispatcher{}}
}

// UseDispatcher plugs in what delivers a notification after it is written (push, realtime). Call
// it while wiring, before the service takes requests: it is not safe to change under load. A nil
// dispatcher puts the default back.
func (s *Service) UseDispatcher(d Dispatcher) {
	if d == nil {
		d = noopDispatcher{}
	}
	s.dispatcher = d
}

// List returns a page of the caller's notifications, newest first, with the total the filter
// matches and the server time a client starts its delta sync from.
func (s *Service) List(ctx context.Context, p auth.Principal, in ListInput) (Listing, error) {
	// Taken before the queries: a row that changes while they run has an updated_at after this
	// instant, so the client's next delta pull, which starts here, still finds it.
	out := Listing{ServerTime: s.clock.Now()}
	err := database.WithTenant(ctx, s.pool, p.OrganizationID, func(q *db.Queries) error {
		items, err := s.repo.list(ctx, q, p.PersonnelID, in)
		if err != nil {
			return err
		}
		counts, err := s.repo.count(ctx, q, p.PersonnelID)
		if err != nil {
			return err
		}
		out.Items = items
		out.Total = counts.Total
		if in.UnreadOnly {
			out.Total = counts.Unread
		}
		return nil
	})
	if err != nil {
		return Listing{}, err
	}
	return out, nil
}

// Changes answers a delta pull: what changed in the caller's inbox after Cursor, or after Since
// when there is no cursor, as rows to upsert and tombstones to drop, oldest change first.
//
// One ordered stream carries both, so one cursor and one limit cover them. The stream is read
// one row past the limit to learn whether there is another page without counting anything.
func (s *Service) Changes(ctx context.Context, p auth.Principal, in DeltaInput) (Changes, error) {
	out := Changes{ServerTime: s.clock.Now()}

	var after Cursor
	if in.Cursor != nil {
		after = *in.Cursor
	} else {
		// A pull from before the epoch (a device with a broken clock) is a pull from the start.
		since := in.Since
		if since.Before(time.Unix(0, 0)) {
			since = time.Unix(0, 0)
		}
		// ⚠ See deltaOverlap: the lower bound starts a little before `since`, so a row whose
		// transaction committed after the client's last pull is not lost to a clock-tick race.
		after = Cursor{UpdatedAt: since.Add(-deltaOverlap)}
	}

	var rows []Notification
	err := database.WithTenant(ctx, s.pool, p.OrganizationID, func(q *db.Queries) (err error) {
		rows, err = s.repo.changesAfter(ctx, q, p.PersonnelID, after, in.Limit+1)
		return err
	})
	if err != nil {
		return Changes{}, err
	}

	if len(rows) > in.Limit {
		rows = rows[:in.Limit]
		last := rows[len(rows)-1]
		out.HasMore = true
		out.NextCursor = Cursor{UpdatedAt: last.UpdatedAt, ID: last.ID}.Encode()
	}
	for _, n := range rows {
		if n.DeletedAt != nil {
			out.Deleted = append(out.Deleted, Tombstone{ID: n.ID, DeletedAt: *n.DeletedAt})
			continue
		}
		out.Upserted = append(out.Upserted, n)
	}
	return out, nil
}

// Count returns the caller's unread and total notifications.
func (s *Service) Count(ctx context.Context, p auth.Principal) (Count, error) {
	var out Count
	err := database.WithTenant(ctx, s.pool, p.OrganizationID, func(q *db.Queries) (err error) {
		out, err = s.repo.count(ctx, q, p.PersonnelID)
		return err
	})
	return out, err
}

// Get returns one of the caller's notifications. ErrNotFound when it is not theirs, it does not
// exist, or it belongs to another organization.
func (s *Service) Get(ctx context.Context, p auth.Principal, id int64) (Notification, error) {
	var out Notification
	err := database.WithTenant(ctx, s.pool, p.OrganizationID, func(q *db.Queries) (err error) {
		out, err = s.repo.get(ctx, q, id, p.PersonnelID)
		return err
	})
	return out, err
}

// MarkRead marks one of the caller's notifications read and returns it. It is idempotent: a
// notification that is already read comes back as it is, with its first read_at, and is not a
// change for the delta sync. ErrNotFound as for Get.
func (s *Service) MarkRead(ctx context.Context, p auth.Principal, id int64) (Notification, error) {
	var out Notification
	err := database.WithTenant(ctx, s.pool, p.OrganizationID, func(q *db.Queries) error {
		n, updated, err := s.repo.markRead(ctx, q, id, p.PersonnelID, s.clock.Now())
		if err != nil {
			return err
		}
		if updated {
			out = n
			return nil
		}
		// Nothing to update: it was read before, or it is not the caller's. get tells which.
		out, err = s.repo.get(ctx, q, id, p.PersonnelID)
		return err
	})
	return out, err
}

// MarkAllRead marks every unread notification of the caller read and returns the counter, which
// is then zero unread. In one transaction, so the counter is the state the update left.
func (s *Service) MarkAllRead(ctx context.Context, p auth.Principal) (Count, error) {
	var out Count
	err := database.WithTenant(ctx, s.pool, p.OrganizationID, func(q *db.Queries) (err error) {
		if err = s.repo.markAllRead(ctx, q, p.PersonnelID, s.clock.Now()); err != nil {
			return err
		}
		out, err = s.repo.count(ctx, q, p.PersonnelID)
		return err
	})
	return out, err
}
