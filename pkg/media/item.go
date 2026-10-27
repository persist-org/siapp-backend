package media

import (
	"time"

	"siapp/internal/apigen"
	"siapp/internal/db"
)

// Purpose is what a photo is registered for (the media_purpose enum).
type Purpose = db.MediaPurpose

// Status is where a photo is in the upload flow (the media_status enum).
type Status = db.MediaStatus

// CaptureSource is where a picture came from (the capture_source enum).
type CaptureSource = db.CaptureSource

// The purposes a photo is registered for. Feature modules pass theirs to Attach (Phase 04.2).
const (
	PurposeProfilePhoto     = db.MediaPurposePROFILEPHOTO
	PurposeAttendanceSelfie = db.MediaPurposeATTENDANCESELFIE
	PurposeReport           = db.MediaPurposeREPORT
	PurposeInstruction      = db.MediaPurposeINSTRUCTION
	PurposePatrolScan       = db.MediaPurposePATROLSCAN
	PurposePanicReport      = db.MediaPurposePANICREPORT
	PurposeAssetReport      = db.MediaPurposeASSETREPORT
	PurposeEMemo            = db.MediaPurposeEMEMO
)

// Media statuses.
const (
	StatusPending  = db.MediaStatusPENDING
	StatusUploaded = db.MediaStatusUPLOADED
)

// Media is one registered photo.
type Media struct {
	ID               int64
	OwnerPersonnelID int64
	Purpose          Purpose
	// ContentType and SizeBytes are what the owner declared, and what complete verified.
	ContentType string
	SizeBytes   int64
	StorageKey  string
	Status      Status

	CapturedAt    *time.Time
	Latitude      *float64
	Longitude     *float64
	CaptureSource CaptureSource

	CreatedAt time.Time
	UpdatedAt time.Time
}

// RegisterInput is the body of POST /media. The enums are checked by the validator; the type
// and the size are checked by the usecase, because each has its own reason code.
type RegisterInput struct {
	Purpose       string     `json:"purpose" validate:"required,oneof=PROFILE_PHOTO ATTENDANCE_SELFIE REPORT INSTRUCTION PATROL_SCAN PANIC_REPORT ASSET_REPORT EMEMO"`
	ContentType   string     `json:"content_type" validate:"required,notblank"`
	SizeBytes     int64      `json:"size_bytes" validate:"required,gte=1"`
	CapturedAt    *time.Time `json:"captured_at"`
	Latitude      *float64   `json:"latitude" validate:"omitempty,gte=-90,lte=90"`
	Longitude     *float64   `json:"longitude" validate:"omitempty,gte=-180,lte=180"`
	CaptureSource string     `json:"capture_source" validate:"required,oneof=CAMERA GALLERY UNKNOWN"`
}

// Registration is what registering a photo returns: its id and where to send the bytes.
type Registration struct {
	ID     int64
	Upload Upload
}

func registrationToAPI(r Registration) apigen.MediaUpload {
	headers := r.Upload.Headers
	if headers == nil {
		headers = map[string]string{}
	}
	return apigen.MediaUpload{
		Id:            r.ID,
		UploadUrl:     r.Upload.URL,
		UploadMethod:  apigen.MediaUploadUploadMethod(r.Upload.Method),
		UploadHeaders: headers,
		ExpiresAt:     r.Upload.ExpiresAt.UTC(),
	}
}

func mediaToAPI(m Media) apigen.Media {
	return apigen.Media{
		Id:            m.ID,
		Purpose:       apigen.MediaPurpose(m.Purpose),
		ContentType:   m.ContentType,
		SizeBytes:     m.SizeBytes,
		Status:        apigen.MediaStatus(m.Status),
		CapturedAt:    m.CapturedAt,
		Latitude:      m.Latitude,
		Longitude:     m.Longitude,
		CaptureSource: apigen.CaptureSource(m.CaptureSource),
		CreatedAt:     m.CreatedAt.UTC(),
	}
}
