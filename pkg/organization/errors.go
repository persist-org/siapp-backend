// Package organization owns SiAPP's operating hierarchy: koordinator wilayah → kantor cabang
// → klien → proyek → regu, the people in each regu, and their shifts (module 02).
//
// ⚠ D17/D19. The hierarchy is SiAPP master data, not a copy of Personalia's org structure,
// and every row is identified by its code (OS1, 1K, 1K-IBMI, 1K-IBMI-S, 1K-IBMI-S1), never by
// a number. A code contains its parent's code, so it is never edited after creation. People
// stay in Personalia: this package only says which regu they serve in.
//
// Phase 02.1 starts it with the regions, 02.2 adds the rest of the hierarchy, 02.3 the
// people in each regu and 02.5 their shifts and schedules.
package organization

import (
	"net/http"

	"siapp/pkg/platform/apperr"
)

// Error reasons (details.reason). A reason that has shipped is never renamed. They keep the
// names the plan (docs/plan/02-organization.md) gives them.
const (
	ReasonRegionNotFound            = "REGION_NOT_FOUND"
	ReasonBranchNotFound            = "BRANCH_NOT_FOUND"
	ReasonClientNotFound            = "CLIENT_NOT_FOUND"
	ReasonProjectNotFound           = "PROJECT_NOT_FOUND"
	ReasonTeamNotFound              = "TEAM_NOT_FOUND"
	ReasonCodeTaken                 = "CODE_TAKEN"
	ReasonHasActiveChildren         = "HAS_ACTIVE_CHILDREN"
	ReasonBranchRegionMismatch      = "BRANCH_REGION_MISMATCH"
	ReasonBranchLetterInvalid       = "BRANCH_LETTER_INVALID"
	ReasonClientPrefixInvalid       = "CLIENT_PREFIX_INVALID"
	ReasonProjectNotConfigured      = "PROJECT_NOT_CONFIGURED"
	ReasonMembershipNotFound        = "MEMBERSHIP_NOT_FOUND"
	ReasonMembershipOverlap         = "MEMBERSHIP_OVERLAP"
	ReasonTeamHasDanru              = "TEAM_HAS_DANRU"
	ReasonPersonnelNotFound         = "PERSONNEL_NOT_FOUND"
	ReasonPersonnelInactive         = "PERSONNEL_INACTIVE"
	ReasonShiftNotFound             = "SHIFT_NOT_FOUND"
	ReasonShiftGroupNotFound        = "SHIFT_GROUP_NOT_FOUND"
	ReasonShiftInvalidTime          = "SHIFT_INVALID_TIME"
	ReasonShiftNameTaken            = "SHIFT_NAME_TAKEN"
	ReasonShiftGroupNameTaken       = "SHIFT_GROUP_NAME_TAKEN"
	ReasonShiftInUse                = "SHIFT_IN_USE"
	ReasonShiftGroupInUse           = "SHIFT_GROUP_IN_USE"
	ReasonShiftAssignmentNotFound   = "SHIFT_ASSIGNMENT_NOT_FOUND"
	ReasonScheduleNotFound          = "SCHEDULE_NOT_FOUND"
	ReasonScheduleExists            = "SCHEDULE_EXISTS"
	ReasonScheduleTimezoneAmbiguous = "SCHEDULE_TIMEZONE_AMBIGUOUS"
	ReasonNotProjectMember          = "NOT_PROJECT_MEMBER"
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

// ErrMembershipNotFound is returned for a membership that does not exist, or that has already
// ended (a move, a role change or an end applies to a membership still in force).
var ErrMembershipNotFound = apperr.New(http.StatusNotFound, apperr.CodeNotFound,
	ReasonMembershipNotFound, "Keanggotaan regu tidak ditemukan atau sudah berakhir.")

// ErrMembershipOverlap is returned when the person already serves in this regu on some of
// those dates. A person may serve in several regu at once, but not twice in the same one.
var ErrMembershipOverlap = apperr.New(http.StatusConflict, apperr.CodeConflict,
	ReasonMembershipOverlap, "Personil sudah menjadi anggota regu ini pada tanggal tersebut.")

// ErrTeamHasDanru is returned when the regu already has a DANRU on some of those dates
// (BR-RULE-02: one DANRU per regu per date).
var ErrTeamHasDanru = apperr.New(http.StatusConflict, apperr.CodeConflict,
	ReasonTeamHasDanru, "Regu ini sudah memiliki DANRU pada tanggal tersebut.")

// ErrPersonnelNotFound is returned for a NIK Personalia does not know in this organization.
var ErrPersonnelNotFound = apperr.New(http.StatusNotFound, apperr.CodeNotFound,
	ReasonPersonnelNotFound, "NIK tidak ditemukan di Personalia.")

// ErrPersonnelInactive is returned for a person Personalia reports as inactive: they cannot
// be put in a regu.
var ErrPersonnelInactive = apperr.New(http.StatusUnprocessableEntity, apperr.CodeValidation,
	ReasonPersonnelInactive, "Personil tidak aktif di Personalia dan tidak dapat ditambahkan ke regu.")

// Shift and schedule errors (Phase 02.5).
var (
	// errNoSuchPersonnel is ErrPersonnelNotFound for a personnel id in the path, which is no NIK.
	errNoSuchPersonnel = ErrPersonnelNotFound.WithMessage("Personil tidak ditemukan.")
	// errInactiveNotScheduled: a person Personalia reports as inactive has left; nothing new is
	// scheduled for them (what they already have can still be changed or removed).
	errInactiveNotScheduled = ErrPersonnelInactive.WithMessage("Personil tidak aktif di Personalia dan tidak dapat diberi jadwal baru.")

	ErrShiftNotFound = apperr.New(http.StatusNotFound, apperr.CodeNotFound,
		ReasonShiftNotFound, "Shift tidak ditemukan.")
	ErrShiftGroupNotFound = apperr.New(http.StatusNotFound, apperr.CodeNotFound,
		ReasonShiftGroupNotFound, "Shift group tidak ditemukan.")
	ErrShiftAssignmentNotFound = apperr.New(http.StatusNotFound, apperr.CodeNotFound,
		ReasonShiftAssignmentNotFound, "Personil ini belum memiliki pengaturan jadwal.")
	ErrScheduleNotFound = apperr.New(http.StatusNotFound, apperr.CodeNotFound,
		ReasonScheduleNotFound, "Jadwal tidak ditemukan.")

	// ErrShiftNameTaken and ErrShiftGroupNameTaken: names are unique among the rows that are
	// not deleted, ignoring case.
	ErrShiftNameTaken = apperr.New(http.StatusConflict, apperr.CodeDuplicate,
		ReasonShiftNameTaken, "Nama shift sudah dipakai.")
	ErrShiftGroupNameTaken = apperr.New(http.StatusConflict, apperr.CodeDuplicate,
		ReasonShiftGroupNameTaken, "Nama shift group sudah dipakai.")

	// ErrShiftInUse: a shift that a group uses, or that is scheduled from today on, would leave
	// days pointing at a shift nobody can see.
	ErrShiftInUse = apperr.New(http.StatusConflict, apperr.CodeConflict,
		ReasonShiftInUse, "Shift masih dipakai shift group atau jadwal mulai hari ini.")
	// ErrShiftGroupInUse: a person's schedule is generated from the group.
	ErrShiftGroupInUse = apperr.New(http.StatusConflict, apperr.CodeConflict,
		ReasonShiftGroupInUse, "Shift group masih dipakai pengaturan jadwal personil.")

	// ErrScheduleExists: one schedule per person per date (PJ48 refuses the same).
	ErrScheduleExists = apperr.New(http.StatusConflict, apperr.CodeConflict,
		ReasonScheduleExists, "Personil sudah memiliki jadwal pada tanggal tersebut.")

	// ErrShiftInvalidTime: a working shift needs a start and an end that differ, in HH:mm; an
	// OFF day has no times.
	ErrShiftInvalidTime = apperr.New(http.StatusUnprocessableEntity, apperr.CodeValidation,
		ReasonShiftInvalidTime, "Jam shift tidak valid: jam masuk dan jam pulang harus berformat HH:mm dan berbeda.")

	// ErrScheduleTimezoneAmbiguous: on that date the person serves in projects of different
	// timezones, so the local shift times would mean two different instants.
	ErrScheduleTimezoneAmbiguous = apperr.New(http.StatusUnprocessableEntity, apperr.CodeUnprocessable,
		ReasonScheduleTimezoneAmbiguous, "Personil bertugas di proyek dengan zona waktu berbeda pada tanggal tersebut.")
)

// ErrNotProjectMember is returned to a field user who asks about a project they serve in no
// regu of today (or one that does not exist: the two are not told apart).
var ErrNotProjectMember = apperr.New(http.StatusForbidden, apperr.CodeScopeDenied,
	ReasonNotProjectMember, "Anda bukan anggota proyek ini.")
