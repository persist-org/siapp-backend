// Package attendance is module 07: what the field does about attendance. SiAPP captures the
// evidence (selfie, QR, GPS) and pushes every event to Persist Personel, which holds the official
// record (LOCK F5). This file is Phase 07.1: today's shifts and selfie check-in/out.
package attendance

import (
	"net/http"

	"siapp/pkg/platform/apperr"
)

// Module reasons (details.reason). A shipped reason is never renamed.
const (
	ReasonShiftNotActive    = "ATTENDANCE_SHIFT_NOT_ACTIVE"
	ReasonAlreadyCheckedIn  = "ATTENDANCE_ALREADY_CHECKED_IN"
	ReasonNotCheckedIn      = "ATTENDANCE_NOT_CHECKED_IN"
	ReasonNotAllowed        = "ATTENDANCE_NOT_ALLOWED"
	ReasonOutsideArea       = "ATTENDANCE_OUTSIDE_AREA"
	ReasonNotFound          = "ATTENDANCE_NOT_FOUND"
	ReasonAlreadyCheckedOut = "ATTENDANCE_ALREADY_CHECKED_OUT"
	ReasonNotCheckedOut     = "ATTENDANCE_NOT_CHECKED_OUT"

	ReasonPreviousNotCheckedIn  = "ATTENDANCE_PREVIOUS_NOT_CHECKED_IN"
	ReasonPreviousNotCheckedOut = "ATTENDANCE_PREVIOUS_NOT_CHECKED_OUT"

	// Phase 07.2: QR.
	ReasonQRInvalid     = "ATTENDANCE_QR_INVALID"
	ReasonQRExpired     = "ATTENDANCE_QR_EXPIRED"
	ReasonQRWrongTeam   = "ATTENDANCE_QR_WRONG_TEAM"
	ReasonQRUnavailable = "ATTENDANCE_QR_UNAVAILABLE"

	// Phase 07.6: DANRU absence, group present, admin correction.
	ReasonPersonnelNotScheduled = "ATTENDANCE_PERSONNEL_NOT_SCHEDULED"
)

// ErrQRInvalid is returned for a QR token that is not one the system issued: malformed, a bad
// signature, another organization's, or the code of the other direction (a check-out code scanned
// to check in). One answer for all of them, so a token cannot be used to probe which it was.
var ErrQRInvalid = apperr.New(http.StatusUnprocessableEntity, apperr.CodeUnprocessable,
	ReasonQRInvalid, "QR tidak valid. Pindai QR yang sedang ditampilkan DANRU.")

// ErrQRExpired is returned for a genuine token whose minute has passed: the guard scans again.
var ErrQRExpired = apperr.New(http.StatusUnprocessableEntity, apperr.CodeUnprocessable,
	ReasonQRExpired, "QR sudah kedaluwarsa. Pindai ulang QR yang sedang ditampilkan.")

// ErrQRWrongTeam is returned when the QR is a regu's other than the shift's: scanning the code of
// another regu does not check into this one.
var ErrQRWrongTeam = apperr.New(http.StatusUnprocessableEntity, apperr.CodeUnprocessable,
	ReasonQRWrongTeam, "QR ini milik regu lain, bukan regu shift yang dipilih.")

// ErrQRUnavailable is returned while ATTENDANCE_QR_SECRET is not set: without a key no code can be
// issued or trusted, so the QR method is off and the selfie one still works.
var ErrQRUnavailable = apperr.New(http.StatusServiceUnavailable, apperr.CodeInternal,
	ReasonQRUnavailable, "Absen QR belum tersedia. Gunakan absen selfie.")

// ErrShiftNotActive is returned when the shift_ref is not one of the shifts the person can check
// into now: malformed, another person's, another organization's, or outside its window. The same
// answer for all of them, so a shift_ref cannot be used to probe what exists.
var ErrShiftNotActive = apperr.New(http.StatusUnprocessableEntity, apperr.CodeUnprocessable,
	ReasonShiftNotActive, "Shift ini tidak tersedia untuk absensi sekarang.")

// ErrAlreadyCheckedIn is returned for a second check-in into the same shift.
var ErrAlreadyCheckedIn = apperr.New(http.StatusConflict, apperr.CodeDuplicate,
	ReasonAlreadyCheckedIn, "Anda sudah absen datang pada shift ini.")

// ErrNotCheckedIn is returned for a check-out without a check-in.
var ErrNotCheckedIn = apperr.New(http.StatusUnprocessableEntity, apperr.CodeUnprocessable,
	ReasonNotCheckedIn, "Anda belum absen datang pada shift ini.")

// ErrNotAllowed is returned for a check-in into a shift that is already recorded as leave or
// absence: the record is not the guard's to turn into a presence.
var ErrNotAllowed = apperr.New(http.StatusUnprocessableEntity, apperr.CodeUnprocessable,
	ReasonNotAllowed, "Absensi tidak dapat dilakukan: shift ini sudah tercatat izin atau mangkir.")

// ErrOutsideArea is returned when the strict geofence of the project is on and the fix is outside
// its radius.
var ErrOutsideArea = apperr.New(http.StatusUnprocessableEntity, apperr.CodeUnprocessable,
	ReasonOutsideArea, "Lokasi Anda di luar area proyek.")

// ErrPreviousNotCheckedIn is returned for a check-in while the person's previous working day was
// never attended: no record at all, or one marked absent, and no approved leave over it.
var ErrPreviousNotCheckedIn = apperr.New(http.StatusUnprocessableEntity, apperr.CodeUnprocessable,
	ReasonPreviousNotCheckedIn, "Absen belum bisa dilakukan: hari kerja sebelumnya tidak ada absen datang (mangkir) dan belum diselesaikan.")

// ErrPreviousNotCheckedOut is returned for a check-in while the person's previous working day was
// checked into but never out.
var ErrPreviousNotCheckedOut = apperr.New(http.StatusUnprocessableEntity, apperr.CodeUnprocessable,
	ReasonPreviousNotCheckedOut, "Absen belum bisa dilakukan: hari kerja sebelumnya belum absen pulang.")

// ErrNotFound is returned for an attendance that does not exist, is another person's, or belongs to
// another organization (the same answer for all three).
var ErrNotFound = apperr.New(http.StatusNotFound, apperr.CodeNotFound,
	ReasonNotFound, "Data absensi tidak ditemukan.")

// ErrAlreadyCheckedOut is returned for a check-out of a record that has one: "update pulang"
// (PATCH) is how it is changed.
var ErrAlreadyCheckedOut = apperr.New(http.StatusConflict, apperr.CodeDuplicate,
	ReasonAlreadyCheckedOut, "Anda sudah absen pulang pada shift ini. Gunakan perbarui jam pulang.")

// ErrNotCheckedOut is returned by "update pulang" for a record that has no check-out yet.
var ErrNotCheckedOut = apperr.New(http.StatusUnprocessableEntity, apperr.CodeUnprocessable,
	ReasonNotCheckedOut, "Belum ada absen pulang yang bisa diperbarui.")

// ErrPersonnelNotScheduled is returned by the DANRU absence and group-present endpoints when the
// target personnel has no shift instance of the given regu on the given work date: there is no
// shift instance to turn into a record.
var ErrPersonnelNotScheduled = apperr.New(http.StatusUnprocessableEntity, apperr.CodeUnprocessable,
	ReasonPersonnelNotScheduled, "Personil tidak terjadwal pada regu dan tanggal ini.")
