// Package idem makes unsafe requests safe to retry: the Idempotency-Key middleware, the
// offline-create request fields, and the cleanup of old keys.
//
// # The replay contract
//
// A client that is not sure its request arrived (a flaky connection, an offline queue)
// sends it again with the same Idempotency-Key. Then:
//
//   - the same key with the same request returns the stored response of the first call
//     (same status, same body) with the header "Idempotent-Replayed: true", and nothing
//     runs again: no handler, no side effects, no notifications, no outbox rows;
//   - the same key with a different request is 409 CONFLICT, reason IDEMPOTENCY_KEY_REUSED;
//   - a request that fails with 5xx, 409 or 429 is not stored, so the retry really runs;
//   - a 2xx whose response cannot be kept (not JSON, or over 256 KB) still completes the
//     key, because its side effects happened: the retry does not run again and answers
//     409 CONFLICT, reason IDEMPOTENCY_RESPONSE_NOT_STORED, with "Idempotent-Replayed:
//     true". Such a response on an idempotent route is a bug and is logged as an error. A
//     4xx that cannot be kept is not stored: it changed nothing, so the retry may run.
//
// The middleware answers a replay itself and never calls the handler, so a handler cannot
// accidentally repeat a side effect. Replayed exists for code that sits outside that path
// (a queue consumer that re-feeds a stored request): it reports whether the context was
// marked with WithReplayed, and such code must skip its side effects when it is true.
//
// The stored body is the first response's JSON as Postgres jsonb keeps it, so key order
// and whitespace can differ from the first response; the content is the same, including
// meta.request_id of the first request.
package idem

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"siapp/internal/db"
	"siapp/pkg/platform/apperr"
	"siapp/pkg/platform/database"
	"siapp/pkg/platform/httpx"
)

// Header names.
const (
	HeaderKey      = "Idempotency-Key"
	HeaderReplayed = "Idempotent-Replayed"
)

const (
	defaultWaitTimeout  = 5 * time.Second
	defaultPollInterval = 50 * time.Millisecond
	// defaultInFlightTTL is how long a request may hold its key without finishing. Beyond
	// that its process is presumed dead and another request may take the key over.
	defaultInFlightTTL = 2 * time.Minute

	maxRequestBody  = 1 << 20 // same as httpx.Decode
	maxStoredBody   = 256 << 10
	finishTimeout   = 5 * time.Second
	statusClassMask = 100
)

// Config wires the middleware.
type Config struct {
	Pool *pgxpool.Pool
	// Identity returns the organization and the acting personnel of the request. It is the
	// authenticated identity (Phase 01.1 supplies it from the access token); ok is false
	// when there is none, which answers 401. The middleware must run after authentication.
	Identity func(r *http.Request) (orgID, actorPersonnelID int64, ok bool)

	// WaitTimeout is how long a request that finds its key in flight waits for the first
	// request to finish before answering 409 IDEMPOTENCY_KEY_IN_FLIGHT. Default 5s.
	WaitTimeout time.Duration
	// PollInterval is how often it looks. Default 50ms.
	PollInterval time.Duration
	// InFlightTTL is the age after which an unfinished key is taken over. Default 2m.
	InFlightTTL time.Duration
}

// Service holds the configuration; create it once and mount its Middleware on routes.
type Service struct{ cfg Config }

// New returns a Service. It panics on a missing Pool or Identity: both are wiring mistakes.
func New(cfg Config) *Service {
	if cfg.Pool == nil || cfg.Identity == nil {
		panic("idem: Config.Pool and Config.Identity are required")
	}
	if cfg.WaitTimeout <= 0 {
		cfg.WaitTimeout = defaultWaitTimeout
	}
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = defaultPollInterval
	}
	if cfg.InFlightTTL <= 0 {
		cfg.InFlightTTL = defaultInFlightTTL
	}
	return &Service{cfg: cfg}
}

type ctxKey struct{}

// WithReplayed marks ctx as serving a replay. See the package comment.
func WithReplayed(ctx context.Context) context.Context { return context.WithValue(ctx, ctxKey{}, true) }

// Replayed reports whether ctx was marked with WithReplayed.
func Replayed(ctx context.Context) bool { v, _ := ctx.Value(ctxKey{}).(bool); return v }

// request is what identifies one idempotent request.
type request struct {
	org, actor int64
	key        pgtype.UUID
	method     string
	path       string
	hash       []byte
}

func (rq request) matches(row db.IdempotencyKey) bool {
	return row.Method == rq.method && row.Path == rq.path && bytes.Equal(row.RequestHash, rq.hash)
}

// Middleware returns the middleware for routes marked idempotent. With required true a
// request without an Idempotency-Key is a VALIDATION_ERROR; with false it simply runs
// without protection.
func (s *Service) Middleware(required bool) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			s.serve(w, r, next, required)
		})
	}
}

func (s *Service) serve(w http.ResponseWriter, r *http.Request, next http.Handler, required bool) {
	raw := r.Header.Get(HeaderKey)
	if raw == "" {
		if !required {
			next.ServeHTTP(w, r)
			return
		}
		httpx.Error(w, r, apperr.ErrValidation.WithFields(apperr.FieldError{
			Field: HeaderKey, Code: "REQUIRED", Message: "Header Idempotency-Key wajib diisi.",
		}))
		return
	}
	key, err := uuid.Parse(raw)
	if err != nil {
		httpx.Error(w, r, apperr.ErrValidation.WithFields(apperr.FieldError{
			Field: HeaderKey, Code: "INVALID", Message: "Idempotency-Key harus berupa UUID.",
		}))
		return
	}
	org, actor, ok := s.cfg.Identity(r)
	if !ok {
		httpx.Error(w, r, apperr.ErrUnauthenticated)
		return
	}

	// The body is read here to hash it, then handed back to the handler.
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxRequestBody))
	if err != nil {
		if _, tooLarge := errors.AsType[*http.MaxBytesError](err); tooLarge {
			httpx.Error(w, r, apperr.ErrPayloadTooLarge)
		} else {
			httpx.Error(w, r, apperr.ErrBadRequest)
		}
		return
	}
	r.Body = io.NopCloser(bytes.NewReader(body))

	rq := request{
		org: org, actor: actor,
		key:    pgtype.UUID{Bytes: key, Valid: true},
		method: r.Method, path: r.URL.Path,
		hash: requestHash(body, r.URL.RawQuery),
	}

	stored, claimed, err := s.claim(r.Context(), rq)
	switch {
	case err != nil:
		httpx.Error(w, r, err)
	case claimed:
		s.run(w, r, next, rq)
	case !rq.matches(*stored):
		httpx.Error(w, r, reused())
	default:
		replay(w, r, *stored)
	}
}

func reused() *apperr.Error {
	return apperr.ErrConflict.
		WithReason(apperr.ReasonIdempotencyKeyReused).
		WithMessage("Idempotency-Key ini sudah dipakai untuk permintaan yang berbeda.")
}

func inFlight() *apperr.Error {
	return apperr.ErrConflict.
		WithReason(apperr.ReasonIdempotencyKeyInFlight).
		WithMessage("Permintaan dengan Idempotency-Key ini masih diproses. Coba lagi sebentar lagi.")
}

func notStored() *apperr.Error {
	return apperr.ErrConflict.
		WithReason(apperr.ReasonIdempotencyResponseNotStored).
		WithMessage("Permintaan ini sudah berhasil diproses, tetapi hasilnya tidak dapat dikirim ulang. Muat ulang data untuk melihatnya.")
}

// claim either takes the key (claimed true) or returns the completed row of the request
// that holds it. When the holder is still running it waits for it, up to WaitTimeout; when
// the holder died it takes the key over. The error is ready for httpx.Error.
func (s *Service) claim(ctx context.Context, rq request) (stored *db.IdempotencyKey, claimed bool, err error) {
	deadline := time.Now().Add(s.cfg.WaitTimeout)
	for {
		row, won, err := s.tryClaim(ctx, rq)
		if err != nil {
			return nil, false, err
		}
		if won {
			return nil, true, nil
		}
		if row != nil {
			// A different request is answered at once; the same one waits if unfinished.
			if !rq.matches(*row) || row.StatusCode.Valid {
				return row, false, nil
			}
			released, err := s.releaseStale(ctx, rq)
			if err != nil {
				return nil, false, err
			}
			if released {
				continue
			}
		}
		// Either the holder is still running, or its row vanished between our two
		// statements (it failed with 5xx and released the key): look again shortly.
		if time.Now().After(deadline) {
			return nil, false, inFlight()
		}
		select {
		case <-ctx.Done():
			return nil, false, ctx.Err()
		case <-time.After(s.cfg.PollInterval):
		}
	}
}

// tryClaim inserts the in-flight row. When the key already exists it returns that row;
// row is nil when it vanished before it could be read.
func (s *Service) tryClaim(ctx context.Context, rq request) (row *db.IdempotencyKey, won bool, err error) {
	err = database.WithTenant(ctx, s.cfg.Pool, rq.org, func(q *db.Queries) error {
		_, err := q.ClaimIdempotencyKey(ctx, db.ClaimIdempotencyKeyParams{
			ActorPersonnelID: rq.actor, Key: rq.key, Method: rq.method, Path: rq.path, RequestHash: rq.hash,
		})
		if err == nil {
			won = true
			return nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		existing, err := q.GetIdempotencyKey(ctx, db.GetIdempotencyKeyParams{ActorPersonnelID: rq.actor, Key: rq.key})
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		row = &existing
		return nil
	})
	return row, won, err
}

func (s *Service) releaseStale(ctx context.Context, rq request) (bool, error) {
	var n int64
	err := database.WithTenant(ctx, s.cfg.Pool, rq.org, func(q *db.Queries) (err error) {
		n, err = q.ReleaseStaleIdempotencyKey(ctx, db.ReleaseStaleIdempotencyKeyParams{
			ActorPersonnelID: rq.actor, Key: rq.key, StaleAfterSeconds: s.cfg.InFlightTTL.Seconds(),
		})
		return err
	})
	return n > 0, err
}

// run executes the handler as the owner of the key, streams its response to the client
// while capturing it, and stores or releases the key afterwards.
func (s *Service) run(w http.ResponseWriter, r *http.Request, next http.Handler, rq request) {
	// Finishing must not depend on the client still being connected, or an aborted
	// request would leave its key in flight until the TTL.
	finishCtx := context.WithoutCancel(r.Context())

	finished := false
	defer func() {
		if finished {
			return
		}
		// The handler panicked: nothing was stored, so let the retry run.
		s.release(finishCtx, rq)
	}()

	cw := &captureWriter{ResponseWriter: w}
	next.ServeHTTP(cw, r)
	finished = true

	if body, omitted, ok := cw.storable(); ok {
		if omitted {
			slog.ErrorContext(r.Context(), "idempotency: a 2xx response is not JSON or too large to store; replays will answer 409 IDEMPOTENCY_RESPONSE_NOT_STORED",
				slog.String("path", rq.path), slog.Int("status", cw.statusCode()),
				slog.String("requestId", httpx.RequestID(r.Context())))
		}
		err := s.complete(finishCtx, rq, cw.statusCode(), body, omitted)
		if err == nil {
			return
		}
		slog.ErrorContext(r.Context(), "idempotency: store response failed",
			slog.String("error", err.Error()), slog.String("requestId", httpx.RequestID(r.Context())))
	}
	s.release(finishCtx, rq)
}

func (s *Service) complete(ctx context.Context, rq request, status int, body []byte, omitted bool) error {
	ctx, cancel := context.WithTimeout(ctx, finishTimeout)
	defer cancel()
	return database.WithTenant(ctx, s.cfg.Pool, rq.org, func(q *db.Queries) error {
		return q.CompleteIdempotencyKey(ctx, db.CompleteIdempotencyKeyParams{
			ActorPersonnelID: rq.actor, Key: rq.key,
			StatusCode:      pgtype.Int4{Int32: int32(status), Valid: true}, //nolint:gosec // an HTTP status fits
			ResponseBody:    body,
			ResponseOmitted: omitted,
		})
	})
}

func (s *Service) release(ctx context.Context, rq request) {
	ctx, cancel := context.WithTimeout(ctx, finishTimeout)
	defer cancel()
	err := database.WithTenant(ctx, s.cfg.Pool, rq.org, func(q *db.Queries) error {
		return q.ReleaseIdempotencyKey(ctx, db.ReleaseIdempotencyKeyParams{ActorPersonnelID: rq.actor, Key: rq.key})
	})
	if err != nil {
		// The row will be taken over after InFlightTTL.
		slog.ErrorContext(ctx, "idempotency: release key failed", slog.String("error", err.Error()))
	}
}

func replay(w http.ResponseWriter, r *http.Request, row db.IdempotencyKey) {
	w.Header().Set(HeaderReplayed, "true")
	if row.ResponseOmitted {
		// The first request succeeded but its response was not kept: say so rather than
		// run the handler again.
		httpx.Error(w, r, notStored())
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(int(row.StatusCode.Int32))
	if len(row.ResponseBody) > 0 {
		_, _ = w.Write(row.ResponseBody)
	}
}

// captureWriter passes the response through and keeps a copy of it, up to maxStoredBody.
type captureWriter struct {
	http.ResponseWriter
	status   int
	buf      bytes.Buffer
	overflow bool
}

func (c *captureWriter) WriteHeader(code int) {
	if c.status == 0 {
		c.status = code
	}
	c.ResponseWriter.WriteHeader(code)
}

func (c *captureWriter) Write(b []byte) (int, error) {
	if c.status == 0 {
		c.status = http.StatusOK
	}
	if !c.overflow {
		if c.buf.Len()+len(b) > maxStoredBody {
			c.overflow = true
			c.buf.Reset()
		} else {
			c.buf.Write(b)
		}
	}
	return c.ResponseWriter.Write(b)
}

// Unwrap lets http.ResponseController reach the real writer (flushing, deadlines).
func (c *captureWriter) Unwrap() http.ResponseWriter { return c.ResponseWriter }

func (c *captureWriter) statusCode() int {
	if c.status == 0 {
		return http.StatusOK // the handler wrote nothing
	}
	return c.status
}

// storable reports whether the key is completed (ok) and returns the body to store.
// Completed: 2xx, and 4xx other than 409 and 429, because the client would get the same
// answer again. Released: 5xx, 409 and 429 (transient, a retry should run).
//
// A body that cannot be kept (not JSON, as the column is jsonb, or too large) is omitted.
// A 2xx is still completed, with omitted true and no body: its side effects happened and
// must not run again. A 4xx is released instead: it changed nothing.
func (c *captureWriter) storable() (body []byte, omitted, ok bool) {
	status := c.statusCode()
	class := status / statusClassMask
	keep := class == 2 || (class == 4 && status != http.StatusConflict && status != http.StatusTooManyRequests)
	if !keep {
		return nil, false, false
	}
	if c.overflow || (c.buf.Len() > 0 && !json.Valid(c.buf.Bytes())) {
		return nil, true, class == 2
	}
	if c.buf.Len() == 0 {
		return nil, false, true
	}
	return bytes.Clone(c.buf.Bytes()), false, true
}
