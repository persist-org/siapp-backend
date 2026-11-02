package identity

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	"siapp/internal/db"
	"siapp/pkg/platform/apperr"
	"siapp/pkg/platform/auth"
	"siapp/pkg/platform/clock"
	"siapp/pkg/platform/database"
	"siapp/pkg/platform/httpx"
	"siapp/pkg/platform/personel"
)

// The Personel change events of the webhook (Phase 05.3).
const (
	EventEmployeeUpdated     = "employee.updated"
	EventEmployeeDeactivated = "employee.deactivated"
	EventPlacementChanged    = "placement.changed"
	EventPositionChanged     = "position.changed"
	// EventLeaveDecided belongs to the leave module (07.5). It is acknowledged here and left
	// for that phase to act on.
	EventLeaveDecided = "leave.decided"
)

const (
	// SignatureHeader carries the hex HMAC-SHA256 of the raw body, optionally as "sha256=<hex>".
	SignatureHeader = "X-Personel-Signature"
	// EventMaxAge is how far the event's timestamp may be from now, in either direction.
	EventMaxAge = 5 * time.Minute
	// EventDedupTTL is how long a processed event id is remembered.
	EventDedupTTL = 7 * 24 * time.Hour

	maxEventBody = 64 << 10

	reasonSignatureInvalid = "WEBHOOK_SIGNATURE_INVALID"
	reasonPayloadInvalid   = "WEBHOOK_PAYLOAD_INVALID"
)

var (
	errSignatureInvalid = apperr.New(http.StatusUnauthorized, apperr.CodeUnauthenticated, reasonSignatureInvalid,
		"Tanda tangan webhook tidak valid.")
	errEventStale = apperr.New(http.StatusUnauthorized, apperr.CodeUnauthenticated, reasonSignatureInvalid,
		"Waktu event terlalu jauh dari waktu server.")
	errPayloadInvalid = apperr.New(http.StatusBadRequest, apperr.CodeBadRequest, reasonPayloadInvalid,
		"Isi event tidak valid.")
)

// PositionNotifier tells a person that their position changed. The implementation sends
// POSITION_CHANGED; identity does not import the notification module.
type PositionNotifier interface {
	PositionChanged(ctx context.Context, organizationID, personnelID int64, positionName string)
}

// eventDirectory is the part of the Personel client the receiver needs (seam S4).
type eventDirectory interface {
	EmployeeByNIK(ctx context.Context, orgID int64, nik string) (personel.Identity, error)
}

// cacheInvalidator is the Redis read-through cache in front of Personel (personel.Cached).
type cacheInvalidator interface {
	Invalidate(ctx context.Context, orgID int64, kind, id string) error
}

// PersonelEvents receives the change events of Persist Personel: POST
// /api/v1/integrations/personel/events. It is the fast path for what the daily resync
// (Resyncer) would find within a day: a person who left loses access within seconds.
type PersonelEvents struct {
	pool        *pgxpool.Pool
	directory   eventDirectory
	cache       cacheInvalidator
	revocations *auth.Revocations
	rdb         redis.UniversalClient
	prefix      string
	secret      []byte
	notifier    PositionNotifier
	clock       clock.Clock
	logger      *slog.Logger
	repo        repo
}

// PersonelEventsConfig is what NewPersonelEvents is built from. Notifier may be nil.
type PersonelEventsConfig struct {
	Pool        *pgxpool.Pool
	Directory   eventDirectory
	Cache       cacheInvalidator
	Revocations *auth.Revocations
	Redis       redis.UniversalClient
	RedisPrefix string
	// Secret is PERSONEL_WEBHOOK_SECRET. Empty makes every event fail the signature check: a
	// receiver nobody configured must not accept anything.
	Secret   string
	Notifier PositionNotifier
	Clock    clock.Clock
	Logger   *slog.Logger
}

// NewPersonelEvents returns the receiver.
func NewPersonelEvents(c PersonelEventsConfig) *PersonelEvents {
	return &PersonelEvents{
		pool: c.Pool, directory: c.Directory, cache: c.Cache, revocations: c.Revocations, rdb: c.Redis, prefix: c.RedisPrefix,
		secret: []byte(c.Secret), notifier: c.Notifier, clock: c.Clock, logger: c.Logger,
	}
}

// Mount adds POST /api/v1/integrations/personel/events. The route is on auth.PublicRoutes: its
// authentication is the signature.
func (e *PersonelEvents) Mount(r chi.Router) {
	r.Post("/api/v1/integrations/personel/events", e.receive)
}

// event is the body Personel sends: {"event_id", "type", "organization_id", "timestamp", "data"}.
type event struct {
	EventID        string          `json:"event_id"`
	Type           string          `json:"type"`
	OrganizationID int64           `json:"organization_id"`
	Timestamp      time.Time       `json:"timestamp"`
	Data           json.RawMessage `json:"data"`
}

type eventData struct {
	NIK          string `json:"nik"`
	PositionName string `json:"position_name"`
	ProjectID    int64  `json:"project_id"`
}

func (e *PersonelEvents) receive(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxEventBody))
	if err != nil {
		httpx.Error(w, r, errPayloadInvalid)
		return
	}
	if !e.validSignature(body, r.Header.Get(SignatureHeader)) {
		httpx.Error(w, r, errSignatureInvalid)
		return
	}
	var ev event
	if json.Unmarshal(body, &ev) != nil || ev.EventID == "" || len(ev.EventID) > 128 || ev.Type == "" || ev.OrganizationID < 1 || ev.Timestamp.IsZero() {
		httpx.Error(w, r, errPayloadInvalid)
		return
	}
	if d := e.clock.Now().Sub(ev.Timestamp); d > EventMaxAge || d < -EventMaxAge {
		httpx.Error(w, r, errEventStale)
		return
	}
	// ⚠ The organization comes from the event, which is signed, and is still checked against the
	// organizations table: a signed event for a tenant that does not exist is refused, and the
	// handlers below run in WithTenant of a real one.
	known, err := e.organizationExists(r.Context(), ev.OrganizationID)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	if !known {
		httpx.Error(w, r, apperr.ErrNotFound)
		return
	}

	// ⚠ Replays. Personel delivers at least once, so the same event_id can arrive twice (a
	// timeout after we processed it, a retry). Without this a replayed position.changed would
	// notify the person twice. The id is claimed BEFORE the work, atomically (SET NX), so two
	// deliveries racing each other cannot both run; if the work fails the claim is released, so
	// Personel's retry is processed and not mistaken for a replay.
	key := e.prefix + "personel-event:" + strconv.FormatInt(ev.OrganizationID, 10) + ":" + ev.EventID
	claimed, err := e.rdb.SetNX(r.Context(), key, "1", EventDedupTTL).Result()
	if err != nil {
		httpx.Error(w, r, apperr.New(http.StatusServiceUnavailable, apperr.CodeInternal, "", "Layanan sementara tidak tersedia."))
		return
	}
	if !claimed {
		httpx.OK(w, r, http.StatusOK, map[string]string{"status": "duplicate"})
		return
	}
	status, err := e.handle(r.Context(), ev)
	if err != nil {
		_ = e.rdb.Del(context.WithoutCancel(r.Context()), key).Err()
		e.logger.ErrorContext(r.Context(), "personel event failed",
			slog.String("type", ev.Type), slog.String("event_id", ev.EventID), slog.Any("error", err))
		httpx.Error(w, r, err)
		return
	}
	httpx.OK(w, r, http.StatusOK, map[string]string{"status": status})
}

func (e *PersonelEvents) validSignature(body []byte, header string) bool {
	if len(e.secret) == 0 {
		return false
	}
	got, err := hex.DecodeString(strings.TrimPrefix(strings.TrimSpace(header), "sha256="))
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, e.secret)
	mac.Write(body)
	return hmac.Equal(got, mac.Sum(nil)) // constant time
}

func (e *PersonelEvents) organizationExists(ctx context.Context, id int64) (bool, error) {
	var ids []int64
	if err := database.WithoutTenant(ctx, e.pool, func(q *db.Queries) (err error) {
		ids, err = q.ListOrganizationIDs(ctx)
		return err
	}); err != nil {
		return false, fmt.Errorf("list organizations: %w", err)
	}
	return slices.Contains(ids, id), nil
}

// handle runs one event and returns what it did: "processed" or "ignored".
func (e *PersonelEvents) handle(ctx context.Context, ev event) (string, error) {
	var d eventData
	if len(ev.Data) > 0 {
		if err := json.Unmarshal(ev.Data, &d); err != nil {
			return "", errPayloadInvalid
		}
	}
	switch ev.Type {
	case EventEmployeeUpdated, EventEmployeeDeactivated:
		if d.NIK == "" {
			return "", errPayloadInvalid
		}
		return "processed", e.employee(ctx, ev.OrganizationID, d.NIK, ev.Type == EventEmployeeDeactivated)
	case EventPositionChanged:
		if d.NIK == "" || strings.TrimSpace(d.PositionName) == "" {
			return "", errPayloadInvalid
		}
		return "processed", e.position(ctx, ev.OrganizationID, d.NIK, strings.TrimSpace(d.PositionName))
	case EventPlacementChanged:
		if d.ProjectID < 1 {
			return "", errPayloadInvalid
		}
		// The placement cache is dropped so the next read asks Personel. Telling the project
		// admins that a member is no longer placed where they serve needs a mapping from a
		// Personel project to a SiAPP project that the data model does not hold yet; the admin
		// finds the difference in the regu screens meanwhile, and nobody is removed
		// automatically: the admin decides.
		if err := e.cache.Invalidate(ctx, ev.OrganizationID, personel.KindPlacements, strconv.FormatInt(d.ProjectID, 10)); err != nil {
			return "", fmt.Errorf("invalidate placements: %w", err)
		}
		return "processed", nil
	case EventLeaveDecided:
		return "ignored", nil // Phase 07.5
	}
	e.logger.WarnContext(ctx, "personel event of an unknown type ignored", slog.String("type", ev.Type))
	return "ignored", nil
}

// employee refreshes a person after employee.updated, or deactivates them after
// employee.deactivated or when Personel no longer reports them as an active member of this
// organization. A person who never signed in has no row here, and there is nothing to change.
func (e *PersonelEvents) employee(ctx context.Context, org int64, nik string, deactivated bool) error {
	if err := e.cache.Invalidate(ctx, org, personel.KindEmployee, nik); err != nil {
		return fmt.Errorf("invalidate employee: %w", err)
	}
	var fresh personel.Identity
	if !deactivated {
		var err error
		fresh, err = e.directory.EmployeeByNIK(ctx, org, nik)
		switch {
		case errors.Is(err, personel.ErrNotFound):
			deactivated = true
		case err != nil:
			return fmt.Errorf("personel employee lookup: %w", err)
		}
	}

	now := e.clock.Now()
	var cutOff []int64
	var gone bool
	err := database.WithTenant(ctx, e.pool, org, func(q *db.Queries) error {
		p, found, err := e.repo.personnelByNIK(ctx, q, nik)
		if err != nil || !found {
			return err
		}
		if _, err := q.LockPersonnelForSync(ctx, p.ID); err != nil {
			return fmt.Errorf("lock personnel for sync: %w", err)
		}
		if !deactivated && (!fresh.Active || fresh.OrganizationID != org || fresh.PersonelUserID != p.PersonelUserID) {
			deactivated = true
		}
		if !deactivated {
			return e.repo.refreshPersonnel(ctx, q, p.ID, fresh, now)
		}
		gone = true
		if err := e.repo.markInactive(ctx, q, p.ID, now); err != nil {
			return err
		}
		if cutOff, err = e.repo.endAllSessions(ctx, q, p.ID, now); err != nil {
			return err
		}
		return endMemberships(ctx, q, p.ID, now)
	})
	if err != nil {
		return err
	}
	// After the commit, like every side effect: their access tokens die at once.
	e.revocations.Revoke(ctx, cutOff...)
	if gone {
		e.logger.InfoContext(ctx, "personel event deactivated a person", slog.Int64("organization_id", org))
	}
	return nil
}

// endMemberships takes the person out of every regu: the memberships in force end today and the
// planned ones go away. The admin who reads the regu then sees an ended membership, with
// history, instead of a member who can no longer sign in.
func endMemberships(ctx context.Context, q *db.Queries, personnelID int64, now time.Time) error {
	at := pgtype.Timestamptz{Time: now, Valid: true}
	if _, err := q.DeletePlannedMembershipsOfPersonEverywhere(ctx, db.DeletePlannedMembershipsOfPersonEverywhereParams{PersonnelID: personnelID, Now: at}); err != nil {
		return fmt.Errorf("delete planned memberships: %w", err)
	}
	if _, err := q.EndMembershipsOfPersonEverywhere(ctx, db.EndMembershipsOfPersonEverywhereParams{PersonnelID: personnelID, Now: at}); err != nil {
		return fmt.Errorf("end memberships: %w", err)
	}
	return nil
}

// position stores the new position and tells the person.
func (e *PersonelEvents) position(ctx context.Context, org int64, nik, name string) error {
	var personnelID int64
	err := database.WithTenant(ctx, e.pool, org, func(q *db.Queries) error {
		p, found, err := e.repo.personnelByNIK(ctx, q, nik)
		if err != nil || !found || p.Status != db.PersonnelStatusACTIVE {
			return err
		}
		if err := q.UpdatePersonnelPosition(ctx, db.UpdatePersonnelPositionParams{
			ID: p.ID, PositionName: pgtype.Text{String: name, Valid: true}, Now: pgtype.Timestamptz{Time: e.clock.Now(), Valid: true},
		}); err != nil {
			return fmt.Errorf("update position: %w", err)
		}
		personnelID = p.ID
		return nil
	})
	if err != nil {
		return err
	}
	// After the commit, and never an error: a notification must not fail the event.
	if personnelID != 0 && e.notifier != nil {
		e.notifier.PositionChanged(ctx, org, personnelID, name)
	}
	return nil
}
