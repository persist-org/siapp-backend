package httpx

import (
	"context"
	"log/slog"
	"time"

	"github.com/redis/go-redis/v9"
)

// FailureRule is one "at most Max failures per Window for this Key" limit.
type FailureRule struct {
	// Name labels the rule in logs.
	Name string
	// Key identifies who is being counted, e.g. a client IP, or an IP and an account.
	Key    string
	Max    int
	Window time.Duration
}

// FailureLimiter limits how many times something can fail, with counters in Redis so the
// limit holds across API instances. Unlike RateLimiter it counts only what the caller
// reports with Record, so successes never use up anyone's budget: a hundred guards on one
// wifi network can all log in, while a hundred wrong passwords from that network cannot.
//
// The window is fixed, not sliding: the count starts at the first failure and everything
// resets when the window ends. That is coarser than a sliding window (a burst straddling
// the boundary can briefly get up to twice Max) but is simple, cheap and easy to reason
// about, which suits an abuse brake.
//
// ⚠ It fails open: if Redis cannot be reached, Check reports "not blocked" and the error
// is logged. Refusing every login whenever Redis blips would be a worse outage than
// briefly losing this limit; the per-account lockout in Postgres still applies.
type FailureLimiter struct {
	rdb    redis.UniversalClient
	prefix string
	logger *slog.Logger
}

// NewFailureLimiter returns a FailureLimiter. prefix is put in front of every Redis key;
// tests use a random one so that they do not share counters.
func NewFailureLimiter(rdb redis.UniversalClient, prefix string, logger *slog.Logger) *FailureLimiter {
	return &FailureLimiter{rdb: rdb, prefix: prefix, logger: logger}
}

func (l *FailureLimiter) key(r FailureRule) string { return l.prefix + "fail:" + r.Name + ":" + r.Key }

// Check reports whether any rule is currently exhausted, and how long until the longest
// blocking one resets. Call it before doing the work that can fail.
func (l *FailureLimiter) Check(ctx context.Context, rules ...FailureRule) (retryAfter time.Duration, blocked bool) {
	pipe := l.rdb.Pipeline()
	counts := make([]*redis.StringCmd, len(rules))
	ttls := make([]*redis.DurationCmd, len(rules))
	for i, r := range rules {
		counts[i] = pipe.Get(ctx, l.key(r))
		ttls[i] = pipe.PTTL(ctx, l.key(r))
	}
	// Exec reports the first command error; a missing key (redis.Nil) is expected and only
	// means "no failures yet", so judge each command on its own below.
	_, _ = pipe.Exec(ctx)

	for i, r := range rules {
		n, err := counts[i].Int()
		switch {
		case err == redis.Nil:
			continue
		case err != nil:
			l.logUnavailable(ctx, r, err)
			return 0, false
		}
		if n < r.Max {
			continue
		}
		blocked = true
		if ttl := ttls[i].Val(); ttl > retryAfter {
			retryAfter = ttl
		}
	}
	return retryAfter, blocked
}

// Record counts one failure against every rule. Call it when the work failed in the way
// that should be limited (a wrong password), not for other outcomes.
func (l *FailureLimiter) Record(ctx context.Context, rules ...FailureRule) {
	for _, r := range rules {
		key := l.key(r)
		// ⚠ SET NX with an expiry, then INCR. INCR alone would create the key without a
		// TTL, and a crash between INCR and a separate EXPIRE would block that client
		// forever. SET NX creates the key with its TTL only when it is absent, and INCR
		// keeps the TTL. Both run in one transaction.
		pipe := l.rdb.TxPipeline()
		pipe.SetNX(ctx, key, 0, r.Window)
		pipe.Incr(ctx, key)
		if _, err := pipe.Exec(ctx); err != nil {
			l.logUnavailable(ctx, r, err)
		}
	}
}

func (l *FailureLimiter) logUnavailable(ctx context.Context, r FailureRule, err error) {
	l.logger.ErrorContext(ctx, "failure limiter unavailable, not limiting",
		slog.String("rule", r.Name),
		slog.String("error", err.Error()),
		slog.String("requestId", RequestID(ctx)),
	)
}
