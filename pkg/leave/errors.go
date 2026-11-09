// Package leave is Phase 07.5: leave, permit and sick requests (izin, cuti, sakit), owned by SiAPP
// (D2). Nothing here calls Personalia. The rules copy what kpi-personalia-api does
// (internal/modules/requests and approval); docs/plan/07-attendance.md lists where they differ on
// purpose.
//
// Part A is the groundwork: the categories (and what makes each special), their quotas, the
// per-client switch, and the ledger of annual leave. Parts B and C add the requests, the decision
// and the effect on attendance.
package leave

import (
	"errors"
	"net/http"

	"github.com/jackc/pgx/v5/pgconn"

	"siapp/pkg/platform/apperr"
)

// Module reasons (details.reason). A shipped reason is never renamed.
const (
	ReasonCategoryNotFound    = "LEAVE_CATEGORY_NOT_FOUND"
	ReasonCategoryCodeTaken   = "LEAVE_CATEGORY_CODE_TAKEN"
	ReasonCategoryInvalid     = "LEAVE_CATEGORY_INVALID"
	ReasonBalanceInsufficient = "LEAVE_BALANCE_INSUFFICIENT"
	ReasonNoApprover          = "LEAVE_NO_APPROVER"
	ReasonClientNotFound      = "LEAVE_CLIENT_NOT_FOUND"
)

// ErrCategoryNotFound is returned for a category that does not exist or is another organization's
// (the same answer for both).
var ErrCategoryNotFound = apperr.New(http.StatusNotFound, apperr.CodeNotFound,
	ReasonCategoryNotFound, "Kategori izin tidak ditemukan.")

// ErrCategoryCodeTaken is returned when the code is used: a code is never handed out twice,
// because a request made under it still points at it.
var ErrCategoryCodeTaken = apperr.New(http.StatusConflict, apperr.CodeDuplicate,
	ReasonCategoryCodeTaken, "Kode kategori izin sudah dipakai.")

// ErrCategoryInvalid is returned for a category that does not fit what is asked of it: a quota of
// sick leave, which has none, or of annual leave, whose limit is the ledger.
var ErrCategoryInvalid = apperr.New(http.StatusUnprocessableEntity, apperr.CodeUnprocessable,
	ReasonCategoryInvalid, "Kategori ini tidak sesuai untuk permintaan tersebut.")

// ErrBalanceInsufficient is returned when a change of the ledger would take the balance below 0.
var ErrBalanceInsufficient = apperr.New(http.StatusUnprocessableEntity, apperr.CodeUnprocessable,
	ReasonBalanceInsufficient, "Saldo cuti tidak mencukupi.")

// ErrNoApprover is returned when the regu has no DANRU today, or the caller is that DANRU. The
// request still works: it goes to the admins.
var ErrNoApprover = apperr.New(http.StatusNotFound, apperr.CodeNotFound,
	ReasonNoApprover, "Regu ini belum punya DANRU hari ini. Pengajuan akan diputuskan oleh admin.")

// ErrClientNotFound is returned for a client that does not exist or is another organization's.
var ErrClientNotFound = apperr.New(http.StatusNotFound, apperr.CodeNotFound,
	ReasonClientNotFound, "Klien tidak ditemukan.")

func validationError(field, message string) error {
	return apperr.ErrValidation.WithFields(apperr.FieldError{Field: field, Code: "INVALID", Message: message})
}

// uniqueConstraint is the name of the constraint or index a unique violation hit, "" otherwise.
func uniqueConstraint(err error) string {
	if pgErr, ok := errors.AsType[*pgconn.PgError](err); ok && pgErr.Code == "23505" {
		return pgErr.ConstraintName
	}
	return ""
}
