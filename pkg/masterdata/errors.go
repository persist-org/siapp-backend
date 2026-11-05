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

	ReasonStakeholderTypeNotFound  = "STAKEHOLDER_TYPE_NOT_FOUND"
	ReasonStakeholderTypeCodeTaken = "STAKEHOLDER_TYPE_CODE_TAKEN"
	ReasonStakeholderTypeNameTaken = "STAKEHOLDER_TYPE_NAME_TAKEN"
	ReasonLetterheadNotFound       = "LETTERHEAD_NOT_FOUND"
	ReasonLetterheadNameTaken      = "LETTERHEAD_NAME_TAKEN"
	ReasonAppLinkNotFound          = "APP_LINK_NOT_FOUND"
	ReasonAppLinkNameTaken         = "APP_LINK_NAME_TAKEN"
	ReasonProjectNotFound          = "PROJECT_NOT_FOUND"
	ReasonNotProjectMember         = "NOT_PROJECT_MEMBER"
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

// ErrStakeholderTypeNotFound is returned for a stakeholder type that does not exist, is deleted, or
// belongs to another organization.
var ErrStakeholderTypeNotFound = apperr.New(http.StatusNotFound, apperr.CodeNotFound,
	ReasonStakeholderTypeNotFound, "Tipe stakeholder tidak ditemukan.")

// unknownStakeholderType is the answer to a request body that names a stakeholder type that is
// not there (a project contact, a panic type): the reason of ErrStakeholderTypeNotFound, but a
// 422 about the field, not a 404 about the URL.
func unknownStakeholderType(field string) error {
	return apperr.New(http.StatusUnprocessableEntity, apperr.CodeValidation, ReasonStakeholderTypeNotFound,
		"Tipe stakeholder tidak ditemukan.").WithFields(apperr.FieldError{
		Field: field, Code: "NOT_FOUND", Message: "Tipe stakeholder tidak ditemukan atau sudah dihapus.",
	})
}

// ErrStakeholderTypeCodeTaken is returned when the code is used, even by a deleted type.
var ErrStakeholderTypeCodeTaken = apperr.New(http.StatusConflict, apperr.CodeDuplicate,
	ReasonStakeholderTypeCodeTaken, "Kode tipe stakeholder sudah dipakai.")

// ErrStakeholderTypeNameTaken is returned when another type, active or not, has the name.
var ErrStakeholderTypeNameTaken = apperr.New(http.StatusConflict, apperr.CodeDuplicate,
	ReasonStakeholderTypeNameTaken, "Nama tipe stakeholder sudah dipakai (termasuk tipe nonaktif).")

// ErrLetterheadNotFound is returned for a letterhead that does not exist or is another
// organization's.
var ErrLetterheadNotFound = apperr.New(http.StatusNotFound, apperr.CodeNotFound,
	ReasonLetterheadNotFound, "Kop surat tidak ditemukan.")

// ErrLetterheadNameTaken is returned when another letterhead has the name (ignoring case).
var ErrLetterheadNameTaken = apperr.New(http.StatusConflict, apperr.CodeDuplicate,
	ReasonLetterheadNameTaken, "Nama kop surat sudah dipakai.")

// ErrAppLinkNotFound is returned for an app link that does not exist, is deleted, or is another
// organization's.
var ErrAppLinkNotFound = apperr.New(http.StatusNotFound, apperr.CodeNotFound,
	ReasonAppLinkNotFound, "Tautan aplikasi tidak ditemukan.")

// ErrAppLinkNameTaken is returned when another app link, active or not, has the name.
var ErrAppLinkNameTaken = apperr.New(http.StatusConflict, apperr.CodeDuplicate,
	ReasonAppLinkNameTaken, "Nama tautan aplikasi sudah dipakai (termasuk tautan nonaktif).")

// ErrProjectNotFound is returned for a project that does not exist, is deleted, or is another
// organization's.
var ErrProjectNotFound = apperr.New(http.StatusNotFound, apperr.CodeNotFound,
	ReasonProjectNotFound, "Proyek tidak ditemukan.")

// ErrNotProjectMember is returned to a person who does not serve in the project today. The phone
// book of a site is for the people who work there.
var ErrNotProjectMember = apperr.New(http.StatusForbidden, apperr.CodeScopeDenied,
	ReasonNotProjectMember, "Anda bukan anggota proyek ini.")

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
