// Package presence knows who is online: a person is online when they were seen within the
// last onlineWindow. Any authenticated request, WebSocket frame or location ping counts as
// being seen (Phase 08.1).
package presence

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	"siapp/internal/db"
	"siapp/pkg/platform/auth"
	"siapp/pkg/platform/clock"
	"siapp/pkg/platform/database"
)

const (
	// onlineWindow is how long after the last sign of life a person still counts as online.
	onlineWindow = 5 * time.Minute
	// persistEvery is the most often a person's last_seen_at is written to the database.
	persistEvery = 60 * time.Second
	// touchTimeout bounds the Redis (and rare database) work one request pays for presence.
	touchTimeout = 50 * time.Millisecond
	// cleanupTimeout bounds releasing the persist gate after a failed database write.
	cleanupTimeout = time.Second
)

// Status is what is known about one person: whether they are online, and when they were last
// seen (nil when never).
type Status struct {
	Online     bool
	LastSeenAt *time.Time
}

// Service keeps presence in Redis, with last_seen_at in Postgres as the memory that outlives
// the Redis key.
type Service struct {
	rdb    redis.UniversalClient
	prefix string
	pool   *pgxpool.Pool
	clock  clock.Clock
	logger *slog.Logger
}

// New returns the Service. prefix goes in front of every Redis key; tests use a random one.
func New(rdb redis.UniversalClient, prefix string, pool *pgxpool.Pool, c clock.Clock, logger *slog.Logger) *Service {
	return &Service{rdb: rdb, prefix: prefix, pool: pool, clock: c, logger: logger}
}

// ⚠ The organization is part of every key: personnel ids are only unique within an
// organization, so the same id in two organizations must not share a presence.
func (s *Service) key(orgID, personnelID int64) string {
	return s.prefix + "org:" + strconv.FormatInt(orgID, 10) + ":presence:" + strconv.FormatInt(personnelID, 10)
}

func (s *Service) persistKey(orgID, personnelID int64) string {
	return s.prefix + "org:" + strconv.FormatInt(orgID, 10) + ":presence:persist:" + strconv.FormatInt(personnelID, 10)
}

// Touch records that the person was seen now: they are online for the next onlineWindow, and
// their last_seen_at is persisted.
//
// The database write is throttled to once per persistEvery per person, gated by a Redis key
// set with NX. Writing on every request would cost one database write per request; this
// keeps last_seen_at accurate to about a minute at a fraction of the load. If the write
// fails the gate is released, so the next request tries again instead of waiting a minute.
func (s *Service) Touch(ctx context.Context, orgID, personnelID int64) error {
	now := s.clock.Now()
	if err := s.rdb.Set(ctx, s.key(orgID, personnelID), strconv.FormatInt(now.UnixMilli(), 10), onlineWindow).Err(); err != nil {
		return fmt.Errorf("presence: set: %w", err)
	}
	gate := s.persistKey(orgID, personnelID)
	first, err := s.rdb.SetNX(ctx, gate, 1, persistEvery).Result()
	if err != nil {
		return fmt.Errorf("presence: persist gate: %w", err)
	}
	if !first {
		return nil
	}
	err = database.WithTenant(ctx, s.pool, orgID, func(q *db.Queries) error {
		return q.TouchPersonnelLastSeen(ctx, db.TouchPersonnelLastSeenParams{ID: personnelID, SeenAt: ts(now)})
	})
	if err != nil {
		release, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
		defer cancel()
		_ = s.rdb.Del(release, gate).Err()
		return fmt.Errorf("presence: persist last_seen_at: %w", err)
	}
	return nil
}

// MarkOffline makes the person offline immediately (logout). Their last_seen_at stays.
func (s *Service) MarkOffline(ctx context.Context, orgID, personnelID int64) error {
	if err := s.rdb.Del(context.WithoutCancel(ctx), s.key(orgID, personnelID)).Err(); err != nil {
		return fmt.Errorf("presence: mark offline: %w", err)
	}
	return nil
}

// Many returns the status of each person, one entry per id.
func (s *Service) Many(ctx context.Context, orgID int64, ids []int64) (map[int64]Status, error) {
	var out map[int64]Status
	err := database.WithTenant(ctx, s.pool, orgID, func(q *db.Queries) error {
		var err error
		out, err = s.ManyIn(ctx, q, orgID, ids)
		return err
	})
	return out, err
}

// ManyIn is Many inside a tenant transaction the caller already holds.
//
// Online comes from Redis alone: a person whose key is gone is offline, even if the database
// saw them a minute ago (they logged out). Only when Redis cannot answer is the persisted
// last_seen_at used to judge online, accurate to about a minute.
func (s *Service) ManyIn(ctx context.Context, q *db.Queries, orgID int64, ids []int64) (map[int64]Status, error) {
	out := make(map[int64]Status, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	keys := make([]string, len(ids))
	for i, id := range ids {
		keys[i] = s.key(orgID, id)
	}
	vals, redisErr := s.rdb.MGet(ctx, keys...).Result()
	if redisErr != nil {
		s.logger.ErrorContext(ctx, "presence unavailable, falling back to last_seen_at", slog.String("error", redisErr.Error()))
	}

	var missing []int64
	for i, id := range ids {
		if redisErr == nil {
			if raw, ok := vals[i].(string); ok {
				if ms, err := strconv.ParseInt(raw, 10, 64); err == nil {
					seen := time.UnixMilli(ms).UTC()
					out[id] = Status{Online: true, LastSeenAt: &seen}
					continue
				}
			}
		}
		missing = append(missing, id)
	}
	if len(missing) == 0 {
		return out, nil
	}
	rows, err := q.ListPersonnelLastSeen(ctx, missing)
	if err != nil {
		return nil, fmt.Errorf("presence: list last_seen_at: %w", err)
	}
	now := s.clock.Now()
	for _, id := range missing {
		out[id] = Status{}
	}
	for _, r := range rows {
		if !r.LastSeenAt.Valid {
			continue
		}
		seen := r.LastSeenAt.Time.UTC()
		out[r.ID] = Status{Online: redisErr != nil && now.Sub(seen) < onlineWindow, LastSeenAt: &seen}
	}
	return out, nil
}

// Middleware touches the caller of every authenticated request. Mount it right after
// auth.Require: on a public route there is no principal and it does nothing.
//
// ⚠ It never fails the request. The work is bounded by touchTimeout, errors are logged and
// ignored. It runs inline on purpose: a Redis SET costs well under a millisecond, and doing it
// in the request avoids goroutines nobody tracks or waits for at shutdown.
func Middleware(s *Service, logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if p, ok := auth.From(r.Context()); ok {
				ctx, cancel := context.WithTimeout(r.Context(), touchTimeout)
				if err := s.Touch(ctx, p.OrganizationID, p.PersonnelID); err != nil {
					logger.WarnContext(r.Context(), "presence touch failed", slog.String("error", err.Error()))
				}
				cancel()
			}
			next.ServeHTTP(w, r)
		})
	}
}

func ts(t time.Time) pgtype.Timestamptz { return pgtype.Timestamptz{Time: t, Valid: true} }
