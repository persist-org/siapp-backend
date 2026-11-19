package patrol

import (
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"siapp/internal/db"
	"siapp/pkg/platform/timeutil"
)

// Zone types, as stored.
const (
	TypePatrol = "PATROL"
	TypeGuard  = "GUARD"
)

// DefaultRadiusMeters is the geofence radius of a zone whose point is sent without one.
const DefaultRadiusMeters = 50

// MaxRadiusMeters is the largest geofence radius of a zone.
const MaxRadiusMeters = 5000

// Zone is a patrol route zone or a guard post, with the checkpoints it holds.
//
// ⚠ There is no field for the QR secret on purpose: nothing that serializes a Zone can leak it.
// Only the QR renderer reads it, from the row.
//
// What is filled depends on the read: a list has only CheckpointCount; the admin's detail has the
// ordered Checkpoints (inactive ones too, without their checklists); the field's detail has the active
// Checkpoints with their checklists and what the caller has done today, and NextCheckpointID.
type Zone struct {
	ID           int64
	ProjectCode  string
	TeamCode     *string
	Type         string
	Name         string
	Alias        *string
	Description  *string
	PopupText    *string
	Latitude     *float64
	Longitude    *float64
	RadiusMeters *int
	MinVisit     int
	StartDate    *timeutil.Date
	Active       bool
	// CheckpointCount is how many active checkpoints the zone has: what a guard can scan.
	CheckpointCount int
	Checkpoints     []ZoneCheckpoint
	// NextCheckpointID is the checkpoint the caller should scan next (field detail only).
	NextCheckpointID *string
	CreatedAt        time.Time
	UpdatedAt        time.Time
}

// ZoneCheckpoint is a checkpoint as a zone shows it: where it stands in the zone's order.
type ZoneCheckpoint struct {
	ID           string // uuid
	Name         string
	SortOrder    int // the position in this zone, from 1
	Active       bool
	Latitude     *float64
	Longitude    *float64
	RadiusMeters *int

	// The rest is the field's detail only.
	// MyScans is how many times the caller scanned it in this zone on today's work date.
	MyScans *int
	// Scannable is whether the order rule lets the caller scan it now.
	Scannable *bool
	// Actions is its active checklist, in order. Nil means the read does not carry checklists.
	Actions []CheckpointAction
}

func zoneOf(z db.PatrolZone) Zone {
	out := Zone{
		ID: z.ID, ProjectCode: z.ProjectCode, TeamCode: textPtr(z.TeamCode), Type: string(z.Type), Name: z.Name,
		Alias: textPtr(z.Alias), Description: textPtr(z.Description), PopupText: textPtr(z.PopupText),
		Latitude: z.Latitude, Longitude: z.Longitude, MinVisit: int(z.MinVisit),
		Active: z.Active, CreatedAt: z.CreatedAt.Time, UpdatedAt: z.UpdatedAt.Time,
	}
	if z.RadiusMeters.Valid {
		r := int(z.RadiusMeters.Int32)
		out.RadiusMeters = &r
	}
	if z.StartDate.Valid {
		d := timeutil.NewDate(z.StartDate.Time.Year(), z.StartDate.Time.Month(), z.StartDate.Time.Day())
		out.StartDate = &d
	}
	return out
}

// zoneCheckpointOf is a checkpoint row with its position in the zone.
func zoneCheckpointOf(c db.PatrolCheckpoint, position int32) ZoneCheckpoint {
	return ZoneCheckpoint{
		ID: uuidString(c.ID), Name: c.Name, SortOrder: int(position), Active: c.Active,
		Latitude: c.Latitude, Longitude: c.Longitude, RadiusMeters: intOf(c.RadiusMeters),
	}
}

func ts(t time.Time) pgtype.Timestamptz { return pgtype.Timestamptz{Time: t, Valid: true} }

// textPtr turns a nullable text column into a pointer.
func textPtr(t pgtype.Text) *string {
	if !t.Valid {
		return nil
	}
	s := t.String
	return &s
}

// nonBlank trims s and returns nil when nothing is left.
func nonBlank(s *string) *string {
	if s == nil || strings.TrimSpace(*s) == "" {
		return nil
	}
	t := strings.TrimSpace(*s)
	return &t
}

// textOrNull stores a blank (or missing) text as NULL: "" and "no value" mean the same thing for
// an alias, a description or a popup text.
func textOrNull(s *string) pgtype.Text {
	if s == nil || strings.TrimSpace(*s) == "" {
		return pgtype.Text{}
	}
	return pgtype.Text{String: strings.TrimSpace(*s), Valid: true}
}

func dateParam(d *timeutil.Date) pgtype.Date {
	if d == nil {
		return pgtype.Date{}
	}
	return pgtype.Date{Time: d.Time(), Valid: true}
}

func intParam(v *int) pgtype.Int4 {
	if v == nil {
		return pgtype.Int4{}
	}
	return pgtype.Int4{Int32: int32(*v), Valid: true} //nolint:gosec // validated to 1..5000
}
