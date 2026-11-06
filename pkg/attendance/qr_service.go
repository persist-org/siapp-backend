package attendance

import (
	"context"
	"errors"
	"time"

	"siapp/internal/db"
	"siapp/pkg/platform/auth"
	"siapp/pkg/platform/geo"
)

// QRToken is what the display endpoint returns: the code to show and when it stops working.
type QRToken struct {
	Token     string
	ExpiresAt time.Time
}

// QRInput is the body of the QR check-in and check-out. The fix is optional: the DANRU's code is
// the evidence of this method. When it is sent it is measured like a selfie's (the project's
// geofence, strict or flagged), so a guard who has a code but is not at the site cannot hide it.
type QRInput struct {
	ShiftRef string `json:"shift_ref" validate:"required"`
	Token    string `json:"token" validate:"required"`
	// required_with comes before omitempty: after it, a latitude without a longitude would be
	// skipped as "empty" instead of refused.
	Latitude       *float64 `json:"latitude" validate:"required_with=Longitude,omitempty,gte=-90,lte=90"`
	Longitude      *float64 `json:"longitude" validate:"required_with=Latitude,omitempty,gte=-180,lte=180"`
	AccuracyMeters *float64 `json:"accuracy_meters" validate:"omitempty,gte=0,lte=100000"`
}

// QRDisplay issues the code a DANRU shows for a regu and a direction. The caller has already been
// checked to hold attendance.qr.display for the regu (the handler asks the route's resolution).
//
// ⚠ The permission is the whole authority, not the DANRU role: the matrix is configurable
// (/admin/field-permissions), so whoever an admin grants attendance.qr.display for a regu may show
// its code, and the record names that person as the QR leader (check_in_qr_danru_id). By default
// only a DANRU holds it. A grant is checked "today", and a code lives about a minute, so a grant
// taken away is felt at the next refresh of the code.
func (s *Service) QRDisplay(p auth.Principal, teamCode string, dir Direction) (QRToken, error) {
	if s.qr == nil {
		return QRToken{}, ErrQRUnavailable
	}
	token, claims := s.qr.Issue(p.OrganizationID, teamCode, p.PersonnelID, dir)
	return QRToken{Token: token, ExpiresAt: claims.ExpiresAt()}, nil
}

// QRCheckIn is POST /attendances/qr/check-in. It is the selfie check-in with another kind of
// evidence: the same checkIn runs, so the shift window, the permission, the previous working day,
// the late count and the outbox event are one piece of code, not two that drift apart.
func (s *Service) QRCheckIn(ctx context.Context, p auth.Principal, in QRInput) (Attendance, error) {
	c, err := s.qrCapture(p, in, DirectionIn)
	if err != nil {
		return Attendance{}, err
	}
	return s.checkIn(ctx, p, in.ShiftRef, c)
}

// QRCheckOut is POST /attendances/qr/check-out, on the same checkOut as the selfie one.
func (s *Service) QRCheckOut(ctx context.Context, p auth.Principal, in QRInput) (Attendance, error) {
	c, err := s.qrCapture(p, in, DirectionOut)
	if err != nil {
		return Attendance{}, err
	}
	return s.checkOut(ctx, p, in.ShiftRef, c)
}

// qrCapture verifies the token against the caller and the shift, and builds the capture.
//
// The token must be a genuine, unexpired one (QR_INVALID, QR_EXPIRED), issued in the caller's own
// organization and for this direction (a check-out code does not check in), and for the regu of the
// shift_ref (QR_WRONG_TEAM). The regu is compared before anything is read: a code of another regu is
// refused without telling whether the shift exists. Whether the shift is one the caller can check
// into now is the usecase's question (SHIFT_NOT_ACTIVE).
func (s *Service) qrCapture(p auth.Principal, in QRInput, dir Direction) (capture, error) {
	if s.qr == nil {
		return capture{}, ErrQRUnavailable
	}
	claims, err := s.qr.Verify(in.Token)
	expired := errors.Is(err, errQRExpired)
	if err != nil && !expired {
		return capture{}, ErrQRInvalid
	}
	// Organization and direction come before "expired": a code of another organization, or of the
	// other direction, is invalid however old it is, so the answer never tells a caller anything
	// about a token that is not theirs to use.
	if claims.OrganizationID != p.OrganizationID || claims.Direction != dir {
		return capture{}, ErrQRInvalid
	}
	if expired {
		return capture{}, ErrQRExpired
	}
	key, ok := parseShiftRef(in.ShiftRef)
	if !ok {
		return capture{}, ErrShiftNotActive
	}
	if key.TeamCode != claims.TeamCode {
		return capture{}, ErrQRWrongTeam
	}
	c := capture{Method: db.AttendanceMethodQR, QRDanruID: &claims.DanruID, AccuracyMeters: in.AccuracyMeters}
	if in.Latitude != nil && in.Longitude != nil {
		c.Fix = &geo.Point{Lat: *in.Latitude, Lng: *in.Longitude}
	}
	return c, nil
}
