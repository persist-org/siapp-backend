// Package media is the shared photo upload service (module 04): a client registers an
// upload, sends the bytes to whichever storage the environment configures, and confirms it.
// Later phases attach uploaded photos to records and return viewable URLs.
//
// Feature code depends on the Storage interface only. The drivers are LocalStorage (the disk
// of the machine the API runs on) and S3Storage (MinIO and AWS S3).
package media

import (
	"net/http"

	"siapp/pkg/platform/apperr"
)

// Error reasons (details.reason). A reason that has shipped is never renamed. They keep the
// names the plan (docs/plan/04-media.md) gives them; Phase 04.2 adds the ones about attaching.
const (
	ReasonNotFound        = "MEDIA_NOT_FOUND"
	ReasonTypeNotAllowed  = "MEDIA_TYPE_NOT_ALLOWED"
	ReasonTooLarge        = "MEDIA_TOO_LARGE"
	ReasonNotUploaded     = "MEDIA_NOT_UPLOADED"
	ReasonURLInvalid      = "MEDIA_URL_INVALID"
	ReasonAlreadyUploaded = "MEDIA_ALREADY_UPLOADED"

	// Reasons of Attach (Phase 04.2).
	ReasonAlreadyAttached = "MEDIA_ALREADY_ATTACHED"
	ReasonPurposeMismatch = "MEDIA_PURPOSE_MISMATCH"
	ReasonLimitExceeded   = "MEDIA_LIMIT_EXCEEDED"
)

// ErrNotFound is returned for a media id that does not exist, belongs to another
// organization, or belongs to another person. The three are not told apart, so that an id
// cannot be probed.
var ErrNotFound = apperr.New(http.StatusNotFound, apperr.CodeNotFound,
	ReasonNotFound, "Foto tidak ditemukan.")

// ErrTypeNotAllowed is returned for a content type other than JPEG, PNG or WebP.
var ErrTypeNotAllowed = apperr.New(http.StatusUnprocessableEntity, apperr.CodeUnprocessable,
	ReasonTypeNotAllowed, "Jenis foto harus JPEG, PNG, atau WebP.")

// ErrTooLarge is returned for a declared size over the limit.
var ErrTooLarge = apperr.New(http.StatusRequestEntityTooLarge, apperr.CodePayloadTooLarge,
	ReasonTooLarge, "Ukuran foto melebihi 10 MB.")

// ErrNotUploaded is returned by complete when the object is missing, differs in size from
// what was registered, or is not an image of the registered type. 409: the photo is in the
// wrong state for this step, and uploading it (again) fixes that.
var ErrNotUploaded = apperr.New(http.StatusConflict, apperr.CodeConflict,
	ReasonNotUploaded, "Foto belum terunggah atau tidak sesuai dengan yang didaftarkan.")

// ErrAlreadyUploaded is returned when a new upload URL is asked for a photo that was already
// confirmed: there is nothing left to upload.
var ErrAlreadyUploaded = apperr.New(http.StatusConflict, apperr.CodeConflict,
	ReasonAlreadyUploaded, "Foto sudah terunggah dan dikonfirmasi.")

// ErrAlreadyAttached is returned by Attach for a photo that a record already claimed.
var ErrAlreadyAttached = apperr.New(http.StatusConflict, apperr.CodeConflict,
	ReasonAlreadyAttached, "Foto sudah dilampirkan ke catatan lain.")

// ErrPurposeMismatch is returned by Attach for a photo registered for another kind of record
// (a patrol photo on a report).
var ErrPurposeMismatch = apperr.New(http.StatusUnprocessableEntity, apperr.CodeUnprocessable,
	ReasonPurposeMismatch, "Tujuan foto tidak sesuai dengan catatan ini.")

// ErrLimitExceeded is returned by Attach for more photos than one record may hold.
var ErrLimitExceeded = apperr.New(http.StatusUnprocessableEntity, apperr.CodeUnprocessable,
	ReasonLimitExceeded, "Jumlah foto melebihi batas.")

// errURLInvalid is what the local blob routes answer for a token that is tampered with,
// expired, for the other operation, or used with the wrong type: all one answer, because
// telling them apart would only help someone forging tokens. 403, like the signature error
// of S3.
var errURLInvalid = apperr.New(http.StatusForbidden, apperr.CodeForbidden,
	ReasonURLInvalid, "URL tidak valid atau sudah kedaluwarsa.")

// errURLTooLarge is the same reason for a body over the signed size: 413, as the plan says.
var errURLTooLarge = apperr.New(http.StatusRequestEntityTooLarge, apperr.CodePayloadTooLarge,
	ReasonURLInvalid, "Ukuran foto melebihi yang didaftarkan.")

// errSizeMismatchAPI is what the blob route answers for a body that is not the registered size.
// Nothing was stored, so the same URL still works with the right bytes.
var errSizeMismatchAPI = apperr.ErrBadRequest.WithMessage("Ukuran unggahan harus sama dengan yang didaftarkan.")

// errAlreadyUploaded is the same reason for an upload URL whose photo is already stored: an
// upload URL works once (see LocalStorage.store). 412, the status S3 and MinIO answer for the
// same case (the failed If-None-Match), so a client handles one status whatever the driver: a
// client that retried after a lost answer just calls complete; one that wants another photo
// registers again. The code is CONFLICT: the contract's ErrorCode has no precondition code.
var errAlreadyUploaded = apperr.New(http.StatusPreconditionFailed, apperr.CodeConflict,
	ReasonURLInvalid, "Foto untuk URL ini sudah terunggah. Daftarkan ulang untuk foto lain.")
