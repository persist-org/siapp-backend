package httpx

import (
	"log/slog"
	"math"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-redis/redis_rate/v10"
	"github.com/redis/go-redis/v9"

	"siapp/internal/platform/apperr"
)

// RateLimiter limits requests per client with counters kept in Redis, so the limit holds
// across every API instance.
type RateLimiter struct {
	limiter *redis_rate.Limiter
	prefix  string
	logger  *slog.Logger
}

// NewRateLimiter returns a RateLimiter. prefix is put in front of every Redis key; tests
// use a random one so that they do not share counters.
func NewRateLimiter(rdb redis.UniversalClient, prefix string, logger *slog.Logger) *RateLimiter {
	return &RateLimiter{limiter: redis_rate.NewLimiter(rdb), prefix: prefix, logger: logger}
}

// PerMinute returns middleware that allows limit requests per minute for each client
// address, counting every request. name separates the counters of different endpoints.
// Over the limit the request gets 429 RATE_LIMITED with a Retry-After header.
//
// Use it for endpoints where every request is the thing to limit (forgot-password, panic).
// Login does not use it: it needs to count failures only, see FailureLimiter.
//
// ⚠ It fails open: if Redis cannot be reached the request is let through and the error is
// logged, because refusing everything whenever Redis blips would be a worse outage than
// briefly losing a rate limit.
func (l *RateLimiter) PerMinute(name string, limit int, clientIP func(*http.Request) string) func(http.Handler) http.Handler {
	rate := redis_rate.PerMinute(limit)
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			key := l.prefix + name + ":" + clientIP(r)
			res, err := l.limiter.Allow(r.Context(), key, rate)
			if err != nil {
				l.logger.ErrorContext(r.Context(), "rate limiter unavailable, allowing request",
					slog.String("limit", name),
					slog.String("error", err.Error()),
					slog.String("requestId", RequestID(r.Context())),
				)
				next.ServeHTTP(w, r)
				return
			}
			if res.Allowed == 0 {
				TooManyRequests(w, r, res.RetryAfter)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// TooManyRequests writes the standard 429 RATE_LIMITED response with a Retry-After header,
// rounded up to whole seconds and at least 1 (a header of 0 would invite an instant retry).
func TooManyRequests(w http.ResponseWriter, r *http.Request, retryAfter time.Duration) {
	seconds := max(1, int(math.Ceil(retryAfter.Seconds())))
	w.Header().Set("Retry-After", strconv.Itoa(seconds))
	Error(w, r, apperr.ErrRateLimited)
}

// ClientIPFunc returns a function that reads the caller's IP address from a request.
//
// With an empty header it uses the connection's remote address. With a header name, for a
// deployment behind a reverse proxy, it reads the real client address from that header; a
// comma-separated list (X-Forwarded-For) is read from the right, because each proxy
// appends the address it received the request from, so the last entry is the one our own
// proxy vouches for and the earlier ones are whatever the client claimed.
// A missing or unparsable value falls back to the remote address.
func ClientIPFunc(header string) func(*http.Request) string {
	return func(r *http.Request) string {
		if header != "" {
			values := strings.Split(r.Header.Get(header), ",")
			candidate := strings.TrimSpace(values[len(values)-1])
			if ip := net.ParseIP(candidate); ip != nil {
				return ip.String()
			}
		}
		host, _, err := net.SplitHostPort(r.RemoteAddr)
		if err != nil {
			return r.RemoteAddr
		}
		return host
	}
}
