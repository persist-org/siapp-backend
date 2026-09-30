package auth

import (
	"context"
	"log/slog"

	"github.com/redis/go-redis/v9"
)

// Revocations is the list of sessions whose access tokens must stop working now.
//
// Access tokens are stateless JWTs: verifying one needs only the signing key, not a database
// lookup. That is fast, but it means ending a session (logout, a stolen chain detected, a
// disabled user) would leave its access token working until it expires, up to 15 minutes.
// Revocations closes that gap: ending a session also writes its ID here, and Require refuses
// any token whose session ID is listed. A lookup is one Redis round trip, about 0.1 ms.
//
// An entry only has to outlive the tokens it cuts off, so it expires after revocationTTL, a
// little longer than an access token lives. Redis therefore holds only the last few minutes
// of revocations, not a history.
//
// ⚠ It fails open. If Redis cannot be reached, IsRevoked reports "not revoked" and Revoke
// gives up, both after logging the error, so the API keeps serving requests. The price is
// that during a Redis outage a revoked token works again until it expires, at most 15
// minutes: the same exposure this app had before revocation existed. Failing closed would
// turn every Redis outage into an API outage.
type Revocations struct {
	rdb    redis.UniversalClient
	prefix string
	logger *slog.Logger
}

// NewRevocations returns Revocations backed by rdb. prefix goes in front of every Redis key;
// tests use a random one so that they do not share entries.
func NewRevocations(rdb redis.UniversalClient, prefix string, logger *slog.Logger) *Revocations {
	return &Revocations{rdb: rdb, prefix: prefix, logger: logger}
}

func (r *Revocations) key(sessionID string) string { return r.prefix + "revoked-session:" + sessionID }

// Revoke lists the sessions so that their access tokens stop working immediately.
//
// It is called after the database change has committed and cannot fail the request: the
// sessions are already ended in MySQL, so their refresh tokens are dead either way.
func (r *Revocations) Revoke(ctx context.Context, sessionIDs ...string) {
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

// IsRevoked reports whether the session's access tokens have been cut off. On a Redis
// error it reports false, see the type comment.
func (r *Revocations) IsRevoked(ctx context.Context, sessionID string) bool {
	n, err := r.rdb.Exists(ctx, r.key(sessionID)).Result()
	if err != nil {
		r.logger.ErrorContext(ctx, "revocation list unavailable, treating the session as not revoked",
			slog.String("error", err.Error()),
		)
		return false
	}
	return n > 0
}
