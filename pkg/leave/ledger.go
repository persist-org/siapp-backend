package leave

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"siapp/internal/db"
	"siapp/pkg/access"
	"siapp/pkg/platform/apperr"
	"siapp/pkg/platform/audit"
	"siapp/pkg/platform/auth"
	"siapp/pkg/platform/database"
)

// LedgerKind is why a ledger row exists.
type LedgerKind = db.LeaveLedgerKind

// The kinds of ledger row.
const (
	KindGrant  = db.LeaveLedgerKindGRANT
	KindDeduct = db.LeaveLedgerKindDEDUCT
	KindRefund = db.LeaveLedgerKindREFUND
	KindAdjust = db.LeaveLedgerKindADJUST
)

// LedgerEntry is one row of the ledger of annual leave. Days is signed: a deduction is negative.
type LedgerEntry struct {
	ID             int64
	PersonnelID    int64
	Kind           LedgerKind
	Days           int
	BalanceAfter   int
	LeaveRequestID *int64
	Note           string
	CreatedByID    *int64
	CreatedAt      time.Time
}

func toLedgerEntry(r db.LeaveLedger) LedgerEntry {
	return LedgerEntry{
		ID: r.ID, PersonnelID: r.PersonnelID, Kind: r.Kind, Days: int(r.Days), BalanceAfter: int(r.BalanceAfter),
		LeaveRequestID: int8Ptr(r.LeaveRequestID), Note: r.Note, CreatedByID: int8Ptr(r.CreatedByID), CreatedAt: r.CreatedAt.Time,
	}
}

// Balance returns the person's days of annual leave, inside the caller's transaction.
func Balance(ctx context.Context, q *db.Queries, personnelID int64) (int, error) {
	n, err := q.GetLeaveBalance(ctx, personnelID)
	if err != nil {
		return 0, fmt.Errorf("get leave balance: %w", err)
	}
	return int(n), nil
}

// post appends a row and returns it. The caller MUST hold the person's row lock (LockPersonnel):
// the new balance is read, changed and written, and two writers would lose an update. A change that
// takes the balance below 0 is ErrBalanceInsufficient and writes nothing.
func post(ctx context.Context, q *db.Queries, now time.Time, personnelID int64, kind LedgerKind, days int, requestID, actorID *int64, note string) (LedgerEntry, error) {
	balance, err := Balance(ctx, q, personnelID)
	if err != nil {
		return LedgerEntry{}, err
	}
	after := balance + days
	if after < 0 {
		return LedgerEntry{}, ErrBalanceInsufficient
	}
	var req, actor = pgInt8Ptr(requestID), pgInt8Ptr(actorID)
	row, err := q.InsertLeaveLedger(ctx, db.InsertLeaveLedgerParams{
		PersonnelID: personnelID, Kind: kind, Days: int32(days), BalanceAfter: int32(after), //nolint:gosec // a few thousand days at most
		LeaveRequestID: req, Note: note, CreatedByID: actor, Now: ts(now),
	})
	if err != nil {
		return LedgerEntry{}, fmt.Errorf("insert leave ledger: %w", err)
	}
	return toLedgerEntry(row), nil
}

// Deduct takes the days of a request out of the person's balance (annual leave, at submit). Under
// the person's lock; a balance that cannot cover them is ErrBalanceInsufficient. A request is
// deducted once: a second call is a unique violation, not a second deduction.
func Deduct(ctx context.Context, q *db.Queries, now time.Time, personnelID, requestID int64, days int, note string) (LedgerEntry, error) {
	if days < 1 {
		return LedgerEntry{}, errors.New("leave: a deduction is at least one day")
	}
	return post(ctx, q, now, personnelID, KindDeduct, -days, &requestID, nil, note)
}

// Refund gives back what a request deducted (annual leave, at reject or cancel). Under the
// person's lock. It is idempotent: a request already refunded, or one that deducted nothing,
// changes nothing and reports false.
func Refund(ctx context.Context, q *db.Queries, now time.Time, personnelID, requestID int64, note string) (bool, error) {
	if _, err := q.GetLeaveLedgerOfRequest(ctx, db.GetLeaveLedgerOfRequestParams{LeaveRequestID: pgInt8(requestID), Kind: KindRefund}); err == nil {
		return false, nil
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return false, fmt.Errorf("get refund: %w", err)
	}
	deducted, err := q.GetLeaveLedgerOfRequest(ctx, db.GetLeaveLedgerOfRequestParams{LeaveRequestID: pgInt8(requestID), Kind: KindDeduct})
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("get deduction: %w", err)
	}
	if _, err := post(ctx, q, now, personnelID, KindRefund, int(-deducted.Days), &requestID, nil, note); err != nil {
		return false, err
	}
	return true, nil
}

// LedgerInput is a change an admin makes to a person's balance.
type LedgerInput struct {
	// Kind is GRANT (days to add, more than 0) or ADJUST (a correction, positive or negative).
	Kind string `json:"kind" validate:"required,oneof=GRANT ADJUST"`
	Days int    `json:"days" validate:"required,min=-3660,max=3660"`
	Note string `json:"note" validate:"required,notblank,max=200"`
}

// checkReach lets an ORG_ADMIN through, and a REGION_ADMIN only for a person who serves in a regu
// under their region. A REGION_ADMIN gets the same answer for a person who does not exist as for
// one outside their region, so ids cannot be probed.
func (s *Service) checkReach(ctx context.Context, q *db.Queries, scope access.AdminScope, personnelID int64) error {
	_, err := q.LockPersonnel(ctx, personnelID)
	if errors.Is(err, pgx.ErrNoRows) {
		if !scope.All {
			return access.ErrOutsideAdminScope
		}
		return apperr.ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("lock personnel: %w", err)
	}
	if scope.All {
		return nil
	}
	if s.regions == nil {
		return access.ErrOutsideAdminScope
	}
	regions, err := s.regions.RegionCodesOf(ctx, q, personnelID)
	if err != nil {
		return fmt.Errorf("regions of personnel: %w", err)
	}
	if !scope.AllowsAnyRegion(regions) {
		return access.ErrOutsideAdminScope
	}
	return nil
}

// AdjustLedger grants days of annual leave to a person, or corrects the balance. The balance may
// not go below 0. Audited. Returns the new row.
func (s *Service) AdjustLedger(ctx context.Context, actor auth.Principal, scope access.AdminScope, personnelID int64, in LedgerInput) (LedgerEntry, error) {
	kind := LedgerKind(in.Kind)
	if kind == KindGrant && in.Days < 1 {
		return LedgerEntry{}, validationError("days", "Pemberian saldo harus lebih dari 0 hari.")
	}
	var out LedgerEntry
	err := database.WithTenant(ctx, s.pool, actor.OrganizationID, func(q *db.Queries) error {
		if err := s.checkReach(ctx, q, scope, personnelID); err != nil {
			return err
		}
		actorID := actor.PersonnelID
		e, err := post(ctx, q, s.clock.Now(), personnelID, kind, in.Days, nil, &actorID, in.Note)
		if err != nil {
			return err
		}
		out = e
		return audit.Record(ctx, q, audit.Entry{
			ActorPersonnelID: &actorID, Action: "leave.ledger." + lower(string(kind)), EntityType: "personnel", EntityID: personnelID,
			Metadata: map[string]any{"days": in.Days, "balance_after": e.BalanceAfter},
		})
	})
	return out, err
}

// LedgerPage is a page of a person's ledger, newest first. The first entry's BalanceAfter is the
// person's balance.
type LedgerPage struct {
	Entries []LedgerEntry
	Total   int
}

// ListLedger returns a page of a person's ledger, for an admin.
func (s *Service) ListLedger(ctx context.Context, actor auth.Principal, scope access.AdminScope, personnelID int64, limit, offset int) (LedgerPage, error) {
	var out LedgerPage
	err := database.WithTenant(ctx, s.pool, actor.OrganizationID, func(q *db.Queries) error {
		if err := s.checkReach(ctx, q, scope, personnelID); err != nil {
			return err
		}
		rows, err := q.ListLeaveLedger(ctx, db.ListLeaveLedgerParams{
			PersonnelID: personnelID, PageLimit: int32(limit), PageOffset: int32(offset), //nolint:gosec // bounded by ParseListQuery
		})
		if err != nil {
			return fmt.Errorf("list leave ledger: %w", err)
		}
		for _, r := range rows {
			out.Entries = append(out.Entries, toLedgerEntry(r))
		}
		total, err := q.CountLeaveLedger(ctx, personnelID)
		out.Total = int(total)
		return err
	})
	return out, err
}
