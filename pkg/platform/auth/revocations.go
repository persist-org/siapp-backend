package auth

import (
	"context"
	"log/slog"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"
)

// revocationTTL is how long a revoked session stays on the list: a little longer than the
// access tokens it has to cut off, for clock differences between hosts. Only sessions
// created within that window can still have a live token, so Redis holds the last few
// minutes of revocations, not a history.
const revocationTTL = AccessTokenTTL + time.Minute

// RecentWindow is how far back a session can have been created and still have a live
// access token. Code that ends sessions lists the ones created within it and hands them to
// Revoke.
const RecentWindow = revocationTTL

// Revocations is the list of sessions whose access tokens must stop working now.
//
// Access tokens are stateless JWTs: verifying one needs only the signing key, not a
// database lookup. That is fast, but it means ending a session (logout, a stolen chain
// detected, a disabled person) would leave its access token working until it expires, up
// to 15 minutes. Revocations closes that gap: ending a session also writes its id here, and
// Require refuses any token whose session id is listed. A lookup is one Redis round trip.
//
// ⚠ It fails open. If Redis cannot be reached, IsRevoked reports "not revoked" and Revoke
// gives up, both after logging the error, so the API keeps serving requests. The price is
// that during a Redis outage a revoked token works again until it expires, at most 15
// minutes: the exposure the app would have without a revocation list at all. Failing closed
// would turn every Redis outage into an API outage.
type Revocations struct {
	rdb    redis.UniversalClient
	prefix string
	logger *slog.Logger
}

// NewRevocations returns Revocations backed by rdb. prefix goes in front of every Redis
// key; tests use a random one so that they do not share entries.
func NewRevocations(rdb redis.UniversalClient, prefix string, logger *slog.Logger) *Revocations {
	return &Revocations{rdb: rdb, prefix: prefix, logger: logger}
}

func (r *Revocations) key(sessionID int64) string {
	return r.prefix + "revoked-session:" + strconv.FormatInt(sessionID, 10)
}

// Revoke lists the sessions so that their access tokens stop working immediately.
//
// It is called after the database change has committed and cannot fail the request: the
// sessions are already ended in Postgres, so their refresh tokens are dead either way.
func (r *Revocations) Revoke(ctx context.Context, sessionIDs ...int64) {
	if len(sessionIDs) == 0 {
		return
	}
	// The write must not be skipped because the client went away mid-request, or a logout
	// would quietly leave its token alive.
	ctx = context.WithoutCancel(ctx)

	pipe := r.rdb.Pipeline()
	for _, id := range sessionIDs {
		pipe.Set(ctx, r.key(id), 1, revocationTTL)
	}
	if _, err := pipe.Exec(ctx); err != nil {
		r.logger.ErrorContext(ctx, "could not record revoked sessions, their access tokens stay valid until they expire",
			slog.Int("sessions", len(sessionIDs)),
			slog.String("error", err.Error()),
		)
	}
}

// IsRevoked reports whether the session's access tokens have been cut off. On a Redis error
// it reports false, see the type comment.
func (r *Revocations) IsRevoked(ctx context.Context, sessionID int64) bool {
	n, err := r.rdb.Exists(ctx, r.key(sessionID)).Result()
	if err != nil {
		r.logger.ErrorContext(ctx, "revocation list unavailable, treating the session as not revoked",
			slog.String("error", err.Error()),
		)
		return false
	}
	return n > 0
}
