package notifications

import (
	"time"

	"siapp/internal/apigen"
	"siapp/internal/db"
	"siapp/pkg/platform/timeutil"
)

// Type is what a notification is about (the notification_type enum).
type Type = db.NotificationType

// Priority is how urgently a notification is delivered (the notification_priority enum).
type Priority = db.NotificationPriority

// The 12 types of Tabel 7 of the manual.
const (
	TypeLeaveRequested        = db.NotificationTypeLEAVEREQUESTED
	TypeAbsenceMarked         = db.NotificationTypeABSENCEMARKED
	TypeReportSubmitted       = db.NotificationTypeREPORTSUBMITTED
	TypeInstructionReceived   = db.NotificationTypeINSTRUCTIONRECEIVED
	TypeTeamChanged           = db.NotificationTypeTEAMCHANGED
	TypePositionChanged       = db.NotificationTypePOSITIONCHANGED
	TypePatrolSubmitted       = db.NotificationTypePATROLSUBMITTED
	TypePatrolApproved        = db.NotificationTypePATROLAPPROVED
	TypeGuardSubmitted        = db.NotificationTypeGUARDSUBMITTED
	TypeGuardApproved         = db.NotificationTypeGUARDAPPROVED
	TypePanicTriggered        = db.NotificationTypePANICTRIGGERED
	TypeOfflineSubmissionSent = db.NotificationTypeOFFLINESUBMISSIONSENT
)

// The four types the manual's flows imply and Tabel 7 does not list.
const (
	TypeLeaveDecided        = db.NotificationTypeLEAVEDECIDED
	TypeInstructionFeedback = db.NotificationTypeINSTRUCTIONFEEDBACK
	TypeSwapAssigned        = db.NotificationTypeSWAPASSIGNED
	TypePanicResolved       = db.NotificationTypePANICRESOLVED
)

// Priorities. HIGH is for panic only.
const (
	PriorityNormal = db.NotificationPriorityNORMAL
	PriorityHigh   = db.NotificationPriorityHIGH
)

// AllTypes lists every Type. A test compares it with the database enum, so a type added by a
// migration and forgotten here fails there; Render's switch is kept complete by the exhaustive
// linter.
func AllTypes() []Type {
	return []Type{
		TypeLeaveRequested, TypeAbsenceMarked, TypeReportSubmitted, TypeInstructionReceived,
		TypeTeamChanged, TypePositionChanged, TypePatrolSubmitted, TypePatrolApproved,
		TypeGuardSubmitted, TypeGuardApproved, TypePanicTriggered, TypeOfflineSubmissionSent,
		TypeLeaveDecided, TypeInstructionFeedback, TypeSwapAssigned, TypePanicResolved,
	}
}

// BusinessLine is the line of business of the project a notification is about. It decides the
// labels of the text (DANRU or Supervisor, Regu or Tim). It has the values of
// projects.business_line.
type BusinessLine string

// The business lines.
const (
	LineSecurity BusinessLine = "security"
	LineCleaning BusinessLine = "cleaning"
)

// Vars are the values a template fills in. A type uses the fields it needs and ignores the
// rest; a field left empty drops its phrase from the text instead of leaving a hole in it.
//
// Names go into the rendered text, because that is what the person is shown. Nothing else about
// a person (NIK, phone) belongs here, and none of it goes into Input.Data.
type Vars struct {
	// BusinessLine is the line of the project the event happened in. Empty means security.
	BusinessLine BusinessLine
	// ActorName is who did it, or who it is about: the person asking for leave, pressing the panic
	// button, writing the instruction.
	ActorName string
	// TeamName is the regu (or tim) the event concerns.
	TeamName string
	// TeamRole is a person's new role in a regu. The text uses the business line's word for it.
	TeamRole db.TeamRole
	// PositionName is a person's new position, as Personel names it.
	PositionName string
	// Subject is what it is about in a few words: a report category, a zone, an instruction title,
	// a panic type, the kind of record an offline submission was.
	Subject string
	// Date is the business date the event is for (leave, absence, swap). The zero Date drops it.
	Date timeutil.Date
	// Approved is the outcome of LEAVE_DECIDED.
	Approved bool
}

// Entity is the record a notification is about, so the app can open it.
type Entity struct {
	Type string
	ID   int64
}

// Input is what Service.Notify takes.
type Input struct {
	Type Type
	// RecipientIDs are personnels ids. Duplicates are dropped, and so is every id that is not a
	// person of the caller's organization.
	RecipientIDs []int64
	Vars         Vars
	// Entity, when set, is what the app opens when the notification is tapped.
	Entity *Entity
	// Data is extra deep-link payload: ids and codes, never a NIK or a full name (DL-17).
	Data map[string]any
	// Priority is NORMAL when left empty.
	Priority Priority
	// ExcludePersonnelID is left out of the recipients: the person who caused the event has no
	// need to be told about their own act. 0 excludes nobody.
	ExcludePersonnelID int64
}

// Notification is one row of the inbox.
type Notification struct {
	ID             int64
	OrganizationID int64
	RecipientID    int64
	Type           Type
	Title          string
	Body           string
	EntityType     *string
	EntityID       *int64
	Data           map[string]any
	Priority       Priority
	ReadAt         *time.Time
	CreatedAt      time.Time
	UpdatedAt      time.Time
	DeletedAt      *time.Time
}

// Tombstone tells a client that caches the inbox to drop a row.
type Tombstone struct {
	ID        int64
	DeletedAt time.Time
}

// Count is the inbox counter.
type Count struct {
	Unread int
	Total  int
}

// ListInput is the query of the plain, paged list.
type ListInput struct {
	UnreadOnly bool
	Limit      int
	Offset     int
}

// Listing is a page of the plain list.
type Listing struct {
	Items []Notification
	// Total is the number of rows the filter matches, whatever the page.
	Total int
	// ServerTime is the watermark a client starts its delta sync from.
	ServerTime time.Time
}

// DeltaInput is the query of a delta pull. Cursor wins over Since when both are set: it is the
// position inside a pull that Since started.
type DeltaInput struct {
	Since  time.Time
	Cursor *Cursor
	Limit  int
}

// Changes is the answer to a delta pull.
type Changes struct {
	Upserted []Notification
	Deleted  []Tombstone
	// ServerTime is what the client sends as `since` next time.
	ServerTime time.Time
	// NextCursor is set only when HasMore is.
	NextCursor string
	HasMore    bool
}

func notificationToAPI(n Notification) apigen.Notification {
	data := n.Data
	if data == nil {
		data = map[string]any{}
	}
	out := apigen.Notification{
		Id:             n.ID,
		OrganizationId: n.OrganizationID,
		PersonnelId:    n.RecipientID,
		Type:           apigen.NotificationType(n.Type),
		Title:          n.Title,
		Body:           n.Body,
		Priority:       apigen.NotificationPriority(n.Priority),
		EntityType:     n.EntityType,
		EntityId:       n.EntityID,
		Data:           &data,
		IsRead:         n.ReadAt != nil,
		CreatedAt:      n.CreatedAt.UTC(),
		UpdatedAt:      n.UpdatedAt.UTC(),
	}
	if n.ReadAt != nil {
		t := n.ReadAt.UTC()
		out.ReadAt = &t
	}
	return out
}

func notificationsToAPI(items []Notification) []apigen.Notification {
	out := make([]apigen.Notification, len(items))
	for i, n := range items {
		out[i] = notificationToAPI(n)
	}
	return out
}

func tombstonesToAPI(items []Tombstone) []apigen.Tombstone {
	out := make([]apigen.Tombstone, len(items))
	for i, t := range items {
		out[i] = apigen.Tombstone{Id: t.ID, DeletedAt: t.DeletedAt.UTC()}
	}
	return out
}

func countToAPI(c Count) apigen.NotificationCount {
	return apigen.NotificationCount{Unread: c.Unread, Total: c.Total}
}
