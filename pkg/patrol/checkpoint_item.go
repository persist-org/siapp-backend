package patrol

import (
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"siapp/internal/db"
)

// DefaultCheckpointRadiusMeters is the geofence radius of a checkpoint whose point is sent without
// one. It is smaller than a zone's: a checkpoint is a sticker on one gate, not a whole post.
const DefaultCheckpointRadiusMeters = 30

// MaxCheckpointRadiusMeters is the largest geofence radius of a checkpoint. A radius of kilometres
// would make the geofence, which is what keeps a photo of the sticker from working elsewhere, mean nothing.
const MaxCheckpointRadiusMeters = 1000

// MaxCheckpointActions is how many actions one checkpoint's checklist may have: the guard does them
// on a phone while standing at the sticker.
const MaxCheckpointActions = 30

// MaxActionPhotos is the most photos one action can ask for or take.
const MaxActionPhotos = 5

var checkpointFence = fenceRules{defaultRadius: DefaultCheckpointRadiusMeters, maxRadius: MaxCheckpointRadiusMeters, subject: "Checkpoint"}

// Checkpoint is a place the guard scans, with its checklist and the zones it is in.
//
// ⚠ There is no field for the QR secret on purpose, as for Zone: nothing that serializes a
// Checkpoint can leak it. Only the QR renderer reads it, from the row.
type Checkpoint struct {
	ID           string // uuid
	ProjectCode  string
	Name         string
	Description  *string
	Latitude     *float64
	Longitude    *float64
	RadiusMeters *int
	Active       bool
	QRRotatedAt  time.Time
	Actions      []CheckpointAction
	Zones        []CheckpointZone
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// CheckpointZone is a zone a checkpoint is in, and its position there.
type CheckpointZone struct {
	ID        int64
	Name      string
	Type      string
	SortOrder int
}

// CheckpointAction is one thing the guard does and shows at a checkpoint.
type CheckpointAction struct {
	ID             string // uuid
	SortOrder      int
	Action         string
	Instruction    *string
	MinPhotos      int
	MaxPhotos      int
	IsNoteRequired bool
}

// PhotoRequired is whether the action needs at least one photo.
func (a CheckpointAction) PhotoRequired() bool { return a.MinPhotos > 0 }

func checkpointOf(c db.PatrolCheckpoint, actions []db.PatrolCheckpointAction, zones []db.ListZonesOfCheckpointsRow) Checkpoint {
	out := Checkpoint{
		ID: uuidString(c.ID), ProjectCode: c.ProjectCode, Name: c.Name, Description: textPtr(c.Description),
		Latitude: c.Latitude, Longitude: c.Longitude, RadiusMeters: intOf(c.RadiusMeters), Active: c.Active,
		QRRotatedAt: c.QrRotatedAt.Time, CreatedAt: c.CreatedAt.Time, UpdatedAt: c.UpdatedAt.Time,
		// Never nil: they are JSON arrays.
		Actions: make([]CheckpointAction, 0, len(actions)), Zones: make([]CheckpointZone, 0, len(zones)),
	}
	for _, a := range actions {
		out.Actions = append(out.Actions, actionOf(a))
	}
	for _, z := range zones {
		out.Zones = append(out.Zones, CheckpointZone{ID: z.ZoneID, Name: z.ZoneName, Type: string(z.ZoneType), SortOrder: int(z.Position)})
	}
	return out
}

func actionOf(a db.PatrolCheckpointAction) CheckpointAction {
	return CheckpointAction{
		ID: uuidString(a.ID), SortOrder: int(a.SortOrder), Action: a.Action, Instruction: textPtr(a.Instruction),
		MinPhotos: int(a.MinPhotos), MaxPhotos: int(a.MaxPhotos), IsNoteRequired: a.IsNoteRequired,
	}
}

// uuidString is the text of a uuid column.
func uuidString(id pgtype.UUID) string { return uuid.UUID(id.Bytes).String() }

// parseUUID reads the text of an id. ok is false for anything that is not a uuid: it cannot name a
// row, so the caller answers it like one that does not exist.
func parseUUID(s string) (id pgtype.UUID, ok bool) {
	u, err := uuid.Parse(s)
	if err != nil {
		return pgtype.UUID{}, false
	}
	return pgtype.UUID{Bytes: u, Valid: true}, true
}
