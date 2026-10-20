// Package organization owns SiAPP's operating hierarchy: koordinator wilayah → kantor cabang
// → klien → proyek → regu, the people in each regu, and their shifts (module 02).
//
// ⚠ D17/D19. The hierarchy is SiAPP master data, not a copy of Personalia's org structure,
// and every row is identified by its code (OS1, 1K, 1K-IBMI, 1K-IBMI-S, 1K-IBMI-S1), never by
// a number. A code contains its parent's code, so it is never edited after creation. People
// stay in Personalia: this package only says which regu they serve in.
//
// Phase 02.1 starts it with the regions, and 02.2 adds the rest of the hierarchy.
package organization

import (
	"net/http"

	"siapp/pkg/platform/apperr"
)

// Error reasons (details.reason). A reason that has shipped is never renamed. They keep the
// names the plan (docs/plan/02-organization.md) gives them.
const (
	ReasonRegionNotFound       = "REGION_NOT_FOUND"
	ReasonBranchNotFound       = "BRANCH_NOT_FOUND"
	ReasonClientNotFound       = "CLIENT_NOT_FOUND"
	ReasonProjectNotFound      = "PROJECT_NOT_FOUND"
	ReasonTeamNotFound         = "TEAM_NOT_FOUND"
	ReasonCodeTaken            = "CODE_TAKEN"
	ReasonHasActiveChildren    = "HAS_ACTIVE_CHILDREN"
	ReasonBranchRegionMismatch = "BRANCH_REGION_MISMATCH"
	ReasonBranchLetterInvalid  = "BRANCH_LETTER_INVALID"
	ReasonClientPrefixInvalid  = "CLIENT_PREFIX_INVALID"
	ReasonProjectNotConfigured = "PROJECT_NOT_CONFIGURED"
)

// ErrRegionNotFound is returned for a koordinator wilayah that does not exist (or was deleted).
var ErrRegionNotFound = apperr.New(http.StatusNotFound, apperr.CodeNotFound,
	ReasonRegionNotFound, "Koordinator wilayah tidak ditemukan.")

// ErrCodeTaken is returned when a code is already used, including by a deleted row: a code is
// never handed out twice (D19), because records made under it still point at it.
var ErrCodeTaken = apperr.New(http.StatusConflict, apperr.CodeDuplicate,
	ReasonCodeTaken, "Kode sudah dipakai.")

// ErrHasActiveChildren is returned when something is deleted while rows below it (or admins
// scoped to it) still depend on it.
var ErrHasActiveChildren = apperr.New(http.StatusConflict, apperr.CodeConflict,
	ReasonHasActiveChildren, "Masih ada data aktif di bawahnya. Pindahkan atau hapus terlebih dahulu.")

// ErrBranchNotFound, ErrClientNotFound, ErrProjectNotFound and ErrTeamNotFound are returned
// for a row that does not exist (or was deleted), whether it is asked for by its own code or
// named as the parent of a new row.
var (
	ErrBranchNotFound = apperr.New(http.StatusNotFound, apperr.CodeNotFound,
		ReasonBranchNotFound, "Kantor cabang tidak ditemukan.")
	ErrClientNotFound = apperr.New(http.StatusNotFound, apperr.CodeNotFound,
		ReasonClientNotFound, "Klien tidak ditemukan.")
	ErrProjectNotFound = apperr.New(http.StatusNotFound, apperr.CodeNotFound,
		ReasonProjectNotFound, "Proyek tidak ditemukan.")
	ErrTeamNotFound = apperr.New(http.StatusNotFound, apperr.CodeNotFound,
		ReasonTeamNotFound, "Regu tidak ditemukan.")
)

// ErrBranchRegionMismatch is returned for a branch code whose number is not its region's
// (2K under OS1). The API builds the code itself, so only a code that arrives from outside
// (the master data import) can be wrong.
var ErrBranchRegionMismatch = apperr.New(http.StatusUnprocessableEntity, apperr.CodeValidation,
	ReasonBranchRegionMismatch, "Nomor kode cabang harus sama dengan nomor koordinator wilayahnya.")

// ErrBranchLetterInvalid is returned for a branch letter that is not K, I or P.
var ErrBranchLetterInvalid = apperr.New(http.StatusUnprocessableEntity, apperr.CodeValidation,
	ReasonBranchLetterInvalid, "Huruf PT cabang harus K, I, atau P.")

// ErrClientPrefixInvalid is returned for a company prefix that is not 2 to 10 capital
// letters or digits.
var ErrClientPrefixInvalid = apperr.New(http.StatusUnprocessableEntity, apperr.CodeValidation,
	ReasonClientPrefixInvalid, "Prefix klien harus 2 sampai 10 huruf besar atau angka.")

// ErrProjectNotConfigured is returned where a project's location is needed and has not been
// entered. A guard cannot check in at a site whose location is unknown.
var ErrProjectNotConfigured = apperr.New(http.StatusUnprocessableEntity, apperr.CodeUnprocessable,
	ReasonProjectNotConfigured, "Lokasi proyek belum diisi.")

// validationError is a 422 for one request field.
func validationError(field, message string) error {
	return apperr.ErrValidation.WithFields(apperr.FieldError{Field: field, Code: "INVALID", Message: message})
}
