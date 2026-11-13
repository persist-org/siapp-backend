package patrol

import (
	"encoding/json"
	"fmt"
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

// Zone is a patrol route zone or a guard post, with its active checklist.
//
// ⚠ There is no field for the QR secret on purpose: nothing that serializes a Zone can leak it.
// Only the QR renderer reads it, from the row.
type Zone struct {
	ID            int64
	ProjectCode   string
	TeamCode      *string
	Type          string
	Name          string
	Alias         *string
	Description   *string
	PopupText     *string
	Latitude      *float64
	Longitude     *float64
	RadiusMeters  *int
	MinVisit      int
	StartDate     *timeutil.Date
	PhotoRequired bool
	Active        bool
	Checklist     []ChecklistItem
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

// ChecklistItem is one question of a zone.
type ChecklistItem struct {
	ID         int64
	SortOrder  int
	Label      string
	HelpText   *string
	AnswerType string
	// Options is set for CHOICE and nil for every other type.
	Options    []Option
	Required   bool
	PhotoMode  string
	PhotoMax   int
	NoteMode   string
	AttachWhen string
}

// Option is one choice of a CHOICE question: Value is what the app sends back.
type Option struct {
	Value string `json:"value" validate:"required,notblank,max=100"`
	Label string `json:"label" validate:"required,notblank,max=200"`
}

func zoneOf(z db.PatrolZone, items []db.PatrolChecklistItem) (Zone, error) {
	out := Zone{
		ID: z.ID, ProjectCode: z.ProjectCode, TeamCode: textPtr(z.TeamCode), Type: string(z.Type), Name: z.Name,
		Alias: textPtr(z.Alias), Description: textPtr(z.Description), PopupText: textPtr(z.PopupText),
		Latitude: z.Latitude, Longitude: z.Longitude, MinVisit: int(z.MinVisit), PhotoRequired: z.PhotoRequired,
		Active: z.Active, Checklist: make([]ChecklistItem, 0, len(items)),
		CreatedAt: z.CreatedAt.Time, UpdatedAt: z.UpdatedAt.Time,
	}
	if z.RadiusMeters.Valid {
		r := int(z.RadiusMeters.Int32)
		out.RadiusMeters = &r
	}
	if z.StartDate.Valid {
		d := timeutil.NewDate(z.StartDate.Time.Year(), z.StartDate.Time.Month(), z.StartDate.Time.Day())
		out.StartDate = &d
	}
	for _, it := range items {
		item, err := itemOf(it)
		if err != nil {
			return Zone{}, err
		}
		out.Checklist = append(out.Checklist, item)
	}
	return out, nil
}

func itemOf(it db.PatrolChecklistItem) (ChecklistItem, error) {
	out := ChecklistItem{
		ID: it.ID, SortOrder: int(it.SortOrder), Label: it.Label, HelpText: textPtr(it.HelpText),
		AnswerType: string(it.AnswerType), Required: it.Required, PhotoMode: string(it.PhotoMode),
		PhotoMax: int(it.PhotoMax), NoteMode: string(it.NoteMode), AttachWhen: string(it.AttachWhen),
	}
	if len(it.Options) > 0 {
		if err := json.Unmarshal(it.Options, &out.Options); err != nil {
			return ChecklistItem{}, fmt.Errorf("checklist item %d: decode options: %w", it.ID, err)
		}
	}
	return out, nil
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
