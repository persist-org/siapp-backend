// Package masterdata is module 06: the reference data the feature forms use (report and
// instruction categories, panic types, and later stakeholders, letterheads and app links).
//
// Rule for every master (DL-09): a row that something points at is deactivated, not deleted.
// DELETE on such a row is 409 CONFLICT with reason MASTER_IN_USE, and the client offers
// "non-aktifkan" (PATCH is_active=false) instead.
package masterdata

import (
	"errors"
	"net/http"

	"github.com/jackc/pgx/v5/pgconn"

	"siapp/pkg/platform/apperr"
)

// Module reasons (details.reason). A shipped reason is never renamed.
const (
	ReasonCategoryNotFound     = "CATEGORY_NOT_FOUND"
	ReasonCategoryKindMismatch = "CATEGORY_KIND_MISMATCH"
	ReasonCategoryNameTaken    = "CATEGORY_NAME_TAKEN"
	ReasonPanicTypeNotFound    = "PANIC_TYPE_NOT_FOUND"
	ReasonPanicTypeCodeTaken   = "PANIC_TYPE_CODE_TAKEN"
	ReasonPanicTypeNameTaken   = "PANIC_TYPE_NAME_TAKEN"
	ReasonMasterInUse          = "MASTER_IN_USE"
)

// ErrCategoryNotFound is returned for a category that does not exist, is deleted, or belongs to
// another organization (the same answer for all three).
var ErrCategoryNotFound = apperr.New(http.StatusNotFound, apperr.CodeNotFound,
	ReasonCategoryNotFound, "Kategori tidak ditemukan.")

// ErrCategoryKindMismatch is returned by AssertCategory when the category exists but is of the
// other kind, for example an instruction category given for a report.
var ErrCategoryKindMismatch = apperr.New(http.StatusUnprocessableEntity, apperr.CodeUnprocessable,
	ReasonCategoryKindMismatch, "Kategori ini bukan kategori untuk jenis data tersebut.")

// ErrCategoryNameTaken is returned when another category of the same kind, active or not, has the
// name on an overlapping business line (an empty list is every line).
var ErrCategoryNameTaken = apperr.New(http.StatusConflict, apperr.CodeDuplicate,
	ReasonCategoryNameTaken, "Nama kategori sudah dipakai untuk lini bisnis yang sama (termasuk kategori nonaktif).")

// ErrPanicTypeNotFound is returned for a panic type that does not exist, is deleted, or belongs
// to another organization.
var ErrPanicTypeNotFound = apperr.New(http.StatusNotFound, apperr.CodeNotFound,
	ReasonPanicTypeNotFound, "Tipe panik tidak ditemukan.")

// ErrPanicTypeCodeTaken is returned when the code is used, even by a deleted type: a code is
// never handed out twice, because a panic raised under it still points at it.
var ErrPanicTypeCodeTaken = apperr.New(http.StatusConflict, apperr.CodeDuplicate,
	ReasonPanicTypeCodeTaken, "Kode tipe panik sudah dipakai.")

// ErrPanicTypeNameTaken is returned when another panic type, active or not, has the name.
var ErrPanicTypeNameTaken = apperr.New(http.StatusConflict, apperr.CodeDuplicate,
	ReasonPanicTypeNameTaken, "Nama tipe panik sudah dipakai (termasuk tipe nonaktif).")

// ErrMasterInUse is returned when a master that other records point at is deleted.
var ErrMasterInUse = apperr.New(http.StatusConflict, apperr.CodeConflict,
	ReasonMasterInUse, "Data ini masih dipakai. Nonaktifkan saja, jangan dihapus.")

func validationError(field, message string) error {
	return apperr.ErrValidation.WithFields(apperr.FieldError{Field: field, Code: "INVALID", Message: message})
}

// uniqueConstraint is the name of the constraint or index a unique violation hit, "" otherwise.
func uniqueConstraint(err error) string {
	if pgErr, ok := errors.AsType[*pgconn.PgError](err); ok {
		return pgErr.ConstraintName
	}
	return ""
}
