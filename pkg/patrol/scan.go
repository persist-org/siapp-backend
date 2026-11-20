package patrol

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"siapp/internal/db"
	"siapp/pkg/access"
	"siapp/pkg/media"
	"siapp/pkg/organization"
	"siapp/pkg/platform/apperr"
	"siapp/pkg/platform/auth"
	"siapp/pkg/platform/database"
	"siapp/pkg/platform/idem"
	"siapp/pkg/platform/timeutil"
)

// lockScanning makes one person's scans of one zone take turns. The order rule and the patrol interval both
// read what the person has scanned in the zone and then write a scan, so two scans that arrive together
// must not both read before either has written: both could pass a check the second should have failed.
func lockScanning(ctx context.Context, q *db.Queries, p auth.Principal, zoneID int64) error {
	if err := q.LockPatrolKey(ctx, fmt.Sprintf("patrol:scan:%d:%d:%d", p.OrganizationID, p.PersonnelID, zoneID)); err != nil {
		return fmt.Errorf("lock scan: %w", err)
	}
	return nil
}

// checkScanInterval refuses a second scan of the checkpoint, in the zone, by the person too close to another
// one, on either side of its own moment: an offline scan can arrive after a later one. The caller holds
// lockScanning.
//
// ⚠ The patrol interval (DL-48) is project.patrol_interval_minutes, and it is per checkpoint: a guard
// scans the zone's stickers a few minutes apart, so a zone-wide interval would refuse the very walk the order
// rule asks for. Even a project with no interval refuses a scan within scanMinGap of another of the same
// checkpoint: a double tap, or a retry with a new local_id, must not make two scans of one visit.
func checkScanInterval(ctx context.Context, q *db.Queries, p auth.Principal, zoneID int64, checkpointID pgtype.UUID, project db.Project, at time.Time) error {
	interval := scanInterval(project)
	tooSoon, err := q.HasScanBetween(ctx, db.HasScanBetweenParams{
		PersonnelID: p.PersonnelID, ZoneID: zoneID, CheckpointID: checkpointID, After: ts(at.Add(-interval)), Before: ts(at.Add(interval)),
	})
	if err != nil {
		return fmt.Errorf("check patrol interval: %w", err)
	}
	if tooSoon {
		details := map[string]any{"interval_seconds": int(interval.Seconds())}
		// Phase 10.19: when the guard may try again. The same last-scan-plus-interval the field read shows as
		// next_allowed_at, so the app's countdown and this refusal cannot disagree. Left out when no counted
		// scan is found (the conflict was a scan the read does not count, e.g. one made on another work date).
		last, err := q.LastCountedScan(ctx, db.LastCountedScanParams{PersonnelID: p.PersonnelID, ZoneID: zoneID, CheckpointID: checkpointID})
		if err != nil {
			return fmt.Errorf("last counted scan: %w", err)
		}
		if t := lastScan(last); t != nil {
			details["next_allowed_at"] = nextAllowedAt(*t, interval).UTC().Format(time.RFC3339)
		}
		return ErrIntervalNotElapsed.WithDetails(details)
	}
	return nil
}

// scanInterval is the least time between two counted scans of one checkpoint by one person in the project.
func scanInterval(project db.Project) time.Duration {
	interval := scanMinGap
	if project.PatrolIntervalMinutes.Valid {
		interval = max(interval, time.Duration(project.PatrolIntervalMinutes.Int32)*time.Minute)
	}
	return interval
}

// nextAllowedAt is when a checkpoint scanned at `last` may be scanned again. The interval is measured from the
// previous scan's occurred_at (not from when it arrived), so an offline scan synced late counts from when it was
// made. The one function behind next_allowed_at on the read and in ZONE_INTERVAL_NOT_ELAPSED.
func nextAllowedAt(last time.Time, interval time.Duration) time.Time {
	return last.Add(interval)
}

// scanMinGap is the least time between two scans of one checkpoint by one person, whatever the project's
// patrol_interval_minutes says (or does not say).
const scanMinGap = 30 * time.Second

// Offline is the offline fields of the scan, for idem.OfflineCreate's checks.
func (in ScanInput) Offline() idem.OfflineCreate {
	return idem.OfflineCreate{LocalID: in.LocalID, OccurredAt: in.OccurredAt}
}

// Attacher claims uploaded photos for a record, and shows them when the record is read
// (media.Service).
type Attacher interface {
	Attach(ctx context.Context, q *db.Queries, in media.AttachInput) error
	Views(ctx context.Context, q *db.Queries, entityType string, entityID int64, ids []int64) ([]media.View, error)
}

// NoticeKind says what a Notice is about.
type NoticeKind int

// The notices patrol sends.
const (
	// NoticeSubmitted tells the team's DANRU that a scan waits for a decision.
	NoticeSubmitted NoticeKind = iota
	// NoticeOfflineSent tells the author that a scan they made offline has been delivered.
	NoticeOfflineSent
	// NoticeDecided tells an author that their scans were approved or declined. One notice per
	// author per request, however many of their scans the request decided.
	NoticeDecided
	// NoticeScheduleChanged tells a person that patrol schedules of theirs were made, changed or cancelled: one
	// notice per person per request, however many schedules the request touched.
	NoticeScheduleChanged
)

// Notice is something to tell people about. cmd/api turns it into a notification.
type Notice struct {
	Kind         NoticeKind
	RecipientIDs []int64
	ZoneType     string
	BusinessLine string
	ActorName    string
	TeamCode     string
	Subject      string
	Date         timeutil.Date
	ScanID       int64
	ZoneID       int64
	// CheckpointID and CheckpointName say which checkpoint a submitted scan was of (empty for a notice that
	// covers a decision on several scans, and for scans made before the checkpoints).
	CheckpointID   string
	CheckpointName string
	// Outcome (SCANNED or SKIPPED) and LocationFlag of a submitted scan (Phase 10.17).
	Outcome      string
	LocationFlag string

	// For NoticeDecided: the outcome, what the decider wrote, and every scan it covers (ScanID is the
	// first; with more than one, the notice points at no single scan).
	Declined     bool
	DecisionNote string
	ScanIDs      []int64

	// For NoticeScheduleChanged (Subject is the zone's name): what happened (ScheduleChangeCreated, ...), the
	// schedules it covers, and their project.
	ProjectCode    string
	ScheduleChange string
	ScheduleIDs    []string
}

// Notifier sends notices. cmd/api adapts notifications.Service to it.
type Notifier interface {
	NotifyPatrol(ctx context.Context, n Notice)
}

// ScanInput is the body of POST /zones/{id}/scan: one visit to one checkpoint of the zone. local_id (which
// is also the Idempotency-Key) and occurred_at are the offline fields of idem.OfflineCreate, declared here
// rather than embedded so that a validation error names them as the client sent them ("local_id", not
// "OfflineCreate.local_id").
type ScanInput struct {
	LocalID    string    `json:"local_id" validate:"required,uuid"`
	OccurredAt time.Time `json:"occurred_at" validate:"required"`
	// CheckpointID is the checkpoint scanned; QRPayload is what its sticker held.
	CheckpointID string `json:"checkpoint_id" validate:"required,uuid"`
	QRPayload    string `json:"qr_payload" validate:"required,max=200"`
	// The position is optional (Phase 10.17): a phone with no GPS fix still proves the guard was there by the
	// QR. All three or none; none is stored as location_flag NO_FIX (see Position).
	Latitude       *float64 `json:"latitude" validate:"omitempty,gte=-90,lte=90"`
	Longitude      *float64 `json:"longitude" validate:"omitempty,gte=-180,lte=180"`
	AccuracyMeters *float64 `json:"accuracy_meters" validate:"omitempty,gte=0,lte=100000"`
	// Subject is optional: left out, it is the checkpoint's name.
	Subject *string `json:"subject" validate:"omitempty,max=200"`
	Notes   *string `json:"notes" validate:"omitempty,max=2000"`
	// DeviceCondition is the phone's condition, the default of every action that does not carry its own.
	DeviceCondition  json.RawMessage     `json:"device_condition"`
	Actions          []ActionAnswerInput `json:"actions" validate:"omitempty,max=50,dive"`
	SubmittedOffline bool                `json:"submitted_offline"`
}

// Scan is a visit to a checkpoint of a zone, as it was recorded.
type Scan struct {
	ID          int64
	LocalID     string
	ZoneID      int64
	ZoneType    string
	PersonnelID int64
	// Checkpoint is the checkpoint scanned, as the zone shows it now; its name is the one it has today.
	Checkpoint  ScanCheckpoint
	TeamCode    string
	ProjectCode string
	OccurredAt  time.Time
	ReceivedAt  time.Time
	WorkDate    timeutil.Date
	// The position is nil when the phone had no fix (LocationFlag NO_FIX).
	Latitude       *float64
	Longitude      *float64
	AccuracyMeters *float64
	// DistanceMeters is how far from the checkpoint the scan was made; nil for a checkpoint with no
	// geofence, or no position.
	DistanceMeters *int
	// LocationFlag is what the server made of the position (Phase 10.17): a mark for the DANRU, never a
	// refusal. Outcome is SCANNED or SKIPPED; a skip has a SkipReason and its own photos (Photos), and no
	// Actions.
	LocationFlag string
	Outcome      string
	SkipReason   *string
	Photos       []Photo
	Subject      string
	Notes        *string
	// Actions are the answers, one per action of the checkpoint's checklist as it was when the guard scanned
	// (from patrol_scan_actions, never from the live action). Photos are filled when a scan is read.
	Actions          []ScanAction
	Status           string
	PatrolID         *int64
	SubmittedOffline bool
	CreatedAt        time.Time

	// The decision (Phase 10.3): empty while the scan is SUBMITTED.
	DecidedByID  *int64
	DecidedAt    *time.Time
	DecisionNote *string
	// DecidedByName is filled when a scan is read (Service.present); nil while undecided.
	DecidedByName *string

	// Filled when a scan is read, not when it is stored: the author's name (Service.present).
	PersonnelName string
}

// ScanCheckpoint is the checkpoint a scan was made at.
type ScanCheckpoint struct {
	ID   string // uuid
	Name string
	// SortOrder is its place in the scan's zone now; nil once it has left that zone (or the scan was just made
	// and the place is not read again).
	SortOrder *int
}

// ScanAction is one answered action of a scan, as the scan kept it: the action as it was (so renaming it
// later changes nothing here) and what the guard gave.
type ScanAction struct {
	ActionID    string // uuid
	Action      string
	Instruction *string
	Note        *string
	// DeviceCondition is the phone's condition when it was answered, as the app sent it; nil when it sent none.
	// Evidence for whoever reviews the scan, not a verdict (see devicecondition.go).
	DeviceCondition json.RawMessage
	// Photos are this action's photos as signed URLs that expire in about an hour, so they are never stored:
	// a reader loads the scan again for fresh ones. Nil on the answer to the scan that was just made.
	Photos []Photo
}

// Photo is a photo of a scan as a reader sees it: a signed URL that expires in about an hour, so it
// is never stored, and a reader loads the scan again for a fresh one.
type Photo struct {
	ID            int64
	URL           string
	ContentType   string
	CapturedAt    *time.Time
	CaptureSource string
}

// ScanResult is what Scan returns. Replayed is true when the scan had been stored already: the
// caller answers 200 instead of 201 and nothing was sent to anybody.
type ScanResult struct {
	Scan     Scan
	Replayed bool
}

func scanOf(r db.PatrolScan) (Scan, error) {
	out := Scan{
		ID: r.ID, LocalID: uuid.UUID(r.LocalID.Bytes).String(), ZoneID: r.ZoneID, ZoneType: string(r.ZoneType),
		PersonnelID: r.PersonnelID, TeamCode: r.TeamCode, ProjectCode: r.ProjectCode, OccurredAt: r.OccurredAt.Time,
		ReceivedAt: r.ReceivedAt.Time, WorkDate: timeutil.NewDate(r.WorkDate.Time.Year(), r.WorkDate.Time.Month(), r.WorkDate.Time.Day()),
		Latitude: r.Latitude, Longitude: r.Longitude, AccuracyMeters: r.AccuracyMeters, Subject: r.Subject,
		Notes: textPtr(r.Notes), Status: string(r.Status), SubmittedOffline: r.SubmittedOffline, CreatedAt: r.CreatedAt.Time,
		LocationFlag: string(r.LocationFlag), Outcome: string(r.Outcome),
	}
	if r.SkipReason.Valid {
		reason := string(r.SkipReason.PatrolSkipReason)
		out.SkipReason = &reason
	}
	out.Checkpoint.ID = uuidString(r.CheckpointID)
	if r.DistanceMeters.Valid {
		d := int(r.DistanceMeters.Int32)
		out.DistanceMeters = &d
	}
	if r.PatrolID.Valid {
		out.PatrolID = &r.PatrolID.Int64
	}
	if r.DecidedByID.Valid {
		out.DecidedByID = &r.DecidedByID.Int64
	}
	if r.DecidedAt.Valid {
		out.DecidedAt = &r.DecidedAt.Time
	}
	out.DecisionNote = textPtr(r.DecisionNote)
	out.Actions = []ScanAction{}
	return out, nil
}

// visit is a person arriving at a checkpoint of a zone: what the checks established.
type visit struct {
	zone       db.PatrolZone
	project    db.Project
	checkpoint db.PatrolCheckpoint
	// position is the checkpoint's place in the zone (from 1).
	position int
	loc      *time.Location
	distance *int
}

// arrive runs the first two checks a scan, a check-in and a check-out have in common, in this order: the
// checkpoint must be one of the zone's, and the QR code must be the checkpoint's. The zone and its project
// are already loaded.
//
// ⚠ The link comes first and answers CHECKPOINT_NOT_IN_ZONE for a checkpoint that does not exist, is
// switched off or is another zone's alike. Then the QR: a request with a wrong code learns nothing about
// the caller's permission or position.
func (s *Service) arrive(ctx context.Context, q *db.Queries, p auth.Principal, zone db.PatrolZone, project db.Project,
	checkpointID, qrPayload string, at time.Time,
) (visit, error) {
	return s.reach(ctx, q, p, zone, project, checkpointID, &qrPayload, at)
}

// reach is arrive, with the QR optional: a nil qrPayload skips the QR check. Only a skip passes nil (Phase
// 10.17): the sticker may be the very reason the checkpoint cannot be scanned.
//
// at is the moment the guard was there (a scan's occurred_at); the zero time means "now, no grace" (check-in
// and check-out, which are online). See the QR check below for what at changes.
func (s *Service) reach(ctx context.Context, q *db.Queries, p auth.Principal, zone db.PatrolZone, project db.Project,
	checkpointID string, qrPayload *string, at time.Time,
) (visit, error) {
	v := visit{zone: zone, project: project}
	id, ok := parseUUID(checkpointID)
	if !ok {
		return v, ErrCheckpointNotInZone
	}
	position, err := q.GetZoneCheckpointLink(ctx, db.GetZoneCheckpointLinkParams{ZoneID: zone.ID, CheckpointID: id})
	if errors.Is(err, pgx.ErrNoRows) {
		return v, ErrCheckpointNotInZone
	}
	if err != nil {
		return v, fmt.Errorf("get zone checkpoint link: %w", err)
	}
	v.position = int(position)
	cp, err := q.GetPatrolCheckpoint(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return v, ErrCheckpointNotInZone
	}
	if err != nil {
		return v, fmt.Errorf("get checkpoint: %w", err)
	}
	v.checkpoint = cp
	if qrPayload != nil && !qrMatches(cp, p.OrganizationID, *qrPayload, at) {
		return v, ErrCheckpointQRInvalid
	}
	loc, err := time.LoadLocation(project.Timezone)
	if err != nil {
		return v, fmt.Errorf("project %s: timezone %q: %w", project.Code, project.Timezone, err)
	}
	v.loc = loc
	return v, nil
}

// qrMatches says whether a payload is the checkpoint's QR for a visit made at a moment.
//
// The current secret always counts. The secret the checkpoint had before its last rotation counts too, but
// only for a visit made before that rotation (at < qr_rotated_at): a scan made offline while the old sticker
// was the right one, synced after an admin rotated it. A visit after the rotation, or with no moment (at is
// zero: check-in and check-out), must carry the current secret, so a rotation still kills a leaked sticker for
// every new visit. One generation back only (a second rotation overwrites it), and the 7-day limit on
// occurred_at (idem.OfflineCreate) bounds how long ago the visit can claim to be.
//
// Both secrets are always compared, so the time taken does not say which one a payload held.
func qrMatches(cp db.PatrolCheckpoint, organizationID int64, payload string, at time.Time) bool {
	current := verifyCheckpointPayload(payload, organizationID, uuidString(cp.ID), cp.QrSecret)
	previous := false
	if cp.PreviousQrSecret.Valid {
		previous = verifyCheckpointPayload(payload, organizationID, uuidString(cp.ID), cp.PreviousQrSecret.String)
	}
	return current || (previous && !at.IsZero() && at.Before(cp.QrRotatedAt.Time))
}

// authorize is the permission and the membership a scan and a check-in need, after the QR.
//
// The permission is the one of the zone's type, judged on the date of the moment in the project's
// timezone: someone who changed regu this morning still has yesterday's scans in the old one.
func (s *Service) authorize(ctx context.Context, q *db.Queries, p auth.Principal, v visit, at time.Time) error {
	day := timeutil.BusinessDate(at, v.loc)
	perm := access.PatrolScan
	if v.zone.Type == db.PatrolZoneTypeGUARD {
		perm = access.GuardScan
	}
	if s.perms == nil {
		return errors.New("patrol: permissions are not plugged in")
	}
	res, err := s.perms.Resolve(ctx, q, p.PersonnelID, perm, day)
	if err != nil {
		return err
	}
	if !res.Allowed {
		return access.ErrPermissionDenied
	}
	_, err = s.membershipIn(ctx, q, p.PersonnelID, v.zone, day)
	return err
}

// checkGeofence refuses a position outside the checkpoint's radius (plus the capped tolerance) and records
// how far from it the position was. A checkpoint with no geofence accepts any position. Check-in and
// check-out use it (online only, so a refusal costs nothing); a scan uses flagLocation.
func checkGeofence(v visit, lat, lng, accuracy float64) (visit, error) {
	cp := v.checkpoint
	if cp.Latitude != nil && cp.Longitude != nil && cp.RadiusMeters.Valid {
		d := distanceTo(*cp.Latitude, *cp.Longitude, lat, lng)
		if !insideZone(d, float64(cp.RadiusMeters.Int32), accuracy) {
			return v, ErrCheckpointOutsideRadius.WithDetails(map[string]any{
				"distance_meters": int(d + 0.5), "radius_meters": int(cp.RadiusMeters.Int32),
			})
		}
		rounded := int(d + 0.5)
		v.distance = &rounded
	}
	return v, nil
}

// lowAccuracyMeters is the accuracy above which a fix is flagged LOW_ACCURACY: it says too little to trust
// where the guard stood. A mark for the DANRU, not a limit.
const lowAccuracyMeters = 50.0

// checkPosition is the rule of a scan's and a skip's position: all of latitude, longitude and accuracy, or
// none. A half position is the client's mistake, and it names the missing field.
func checkPosition(lat, lng, accuracy *float64) error {
	var problems []apperr.FieldError
	for _, f := range []struct {
		name string
		v    *float64
	}{{"latitude", lat}, {"longitude", lng}, {"accuracy_meters", accuracy}} {
		if f.v == nil && (lat != nil || lng != nil || accuracy != nil) {
			problems = append(problems, apperr.FieldError{Field: f.name, Code: "REQUIRED", Message: "Wajib diisi bila posisi dikirim."})
		}
	}
	if len(problems) > 0 {
		return apperr.ErrValidation.WithFields(problems...)
	}
	return nil
}

// flagLocation says what the position shows about the visit and records how far from the checkpoint it was.
// Unlike checkGeofence it never refuses (Phase 10.17): the QR is the proof the guard was there, and a refused
// scan is not stored, so it broke the order rule for the rest of an offline round. The DANRU reads the flag at
// approval.
//
// The flags, first match wins: NO_FIX (no position), OUTSIDE_RADIUS (the same test checkGeofence uses),
// LOW_ACCURACY (the fix is vague, see lowAccuracyMeters), OK. A checkpoint with no geofence is OK whenever
// there is a position.
func flagLocation(v visit, lat, lng, accuracy *float64) (visit, db.PatrolLocationFlag) {
	if lat == nil || lng == nil || accuracy == nil {
		return v, db.PatrolLocationFlagNOFIX
	}
	cp := v.checkpoint
	outside := false
	if cp.Latitude != nil && cp.Longitude != nil && cp.RadiusMeters.Valid {
		d := distanceTo(*cp.Latitude, *cp.Longitude, *lat, *lng)
		rounded := int(d + 0.5)
		v.distance = &rounded
		outside = !insideZone(d, float64(cp.RadiusMeters.Int32), *accuracy)
	}
	switch {
	case outside:
		return v, db.PatrolLocationFlagOUTSIDERADIUS
	case *accuracy > lowAccuracyMeters:
		return v, db.PatrolLocationFlagLOWACCURACY
	}
	return v, db.PatrolLocationFlagOK
}

// membershipIn returns the regu of the person that count for the zone on a date: those in the
// zone's project, and when the zone is for one regu, only that regu. None is ZONE_NOT_IN_PROJECT.
func (s *Service) membershipIn(ctx context.Context, q *db.Queries, personnelID int64, zone db.PatrolZone, day timeutil.Date) ([]string, error) {
	return s.membershipInProject(ctx, q, personnelID, zone.ProjectCode, zone.TeamCode, day)
}

// membershipInProject is membershipIn for a project and, when teamCode is set, one regu of it: a
// patrol route belongs to a project, not to a zone, so it asks with no regu.
func (s *Service) membershipInProject(ctx context.Context, q *db.Queries, personnelID int64, projectCode string,
	teamCode pgtype.Text, day timeutil.Date,
) ([]string, error) {
	rows, err := q.ListMembershipsOfPersonnelOn(ctx, db.ListMembershipsOfPersonnelOnParams{
		PersonnelID: personnelID, OnDate: pgtype.Date{Time: day.Time(), Valid: true},
	})
	if err != nil {
		return nil, fmt.Errorf("list memberships: %w", err)
	}
	var teams []string
	for _, r := range rows {
		if r.ProjectCode == projectCode && (!teamCode.Valid || r.TeamCode == teamCode.String) {
			teams = append(teams, r.TeamCode)
		}
	}
	if len(teams) == 0 {
		return nil, ErrNotInProject
	}
	return teams, nil
}

// placement is where a visit counts: the regu, the work date and the shift.
type placement struct {
	team     string
	workDate timeutil.Date
	shiftID  *int64
}

// place decides the regu and the work date of a visit at a moment.
//
// ⚠ They come from the person's shift, not from the app and not from the calendar:
// organization.LocalShifts.CurrentIn(person, at) for the zone's project. That is what makes a night-shift
// scan after midnight count for the day the shift started, the same date attendance uses.
// When no shift matches (an unscheduled round, an OFF day, a schedule that is ambiguous between two
// timezones) the visit still counts: it goes to the person's first active regu in the project, on
// the business date of the moment in the project's timezone.
func (s *Service) place(ctx context.Context, q *db.Queries, p auth.Principal, v visit, at time.Time) (placement, error) {
	return s.placeIn(ctx, q, p, v.zone.ProjectCode, v.zone.TeamCode, v.loc, at)
}

// placeIn is place for a project and, when teamCode is set, one regu of it: what a patrol route
// asks, with no zone and so no regu.
func (s *Service) placeIn(ctx context.Context, q *db.Queries, p auth.Principal, projectCode string, teamCode pgtype.Text,
	loc *time.Location, at time.Time,
) (placement, error) {
	day := timeutil.BusinessDate(at, loc)
	teams, err := s.membershipInProject(ctx, q, p.PersonnelID, projectCode, teamCode, day)
	if err != nil {
		return placement{}, err
	}
	pl := placement{team: teams[0], workDate: day}

	if s.shifts == nil {
		return pl, nil
	}
	instances, err := s.shifts.CurrentIn(ctx, q, p.PersonnelID, at)
	switch {
	case errors.Is(err, organization.ErrScheduleTimezoneAmbiguous):
		return pl, nil
	case err != nil:
		return placement{}, fmt.Errorf("current shifts: %w", err)
	}
	for _, inst := range instances {
		if !slices.Contains(inst.ProjectCodes, projectCode) {
			continue
		}
		// The shift's regu that is also a regu this zone counts for.
		for _, tc := range inst.TeamCodes {
			if slices.Contains(teams, tc) {
				shift := inst.ShiftID
				return placement{team: tc, workDate: inst.WorkDate, shiftID: &shift}, nil
			}
		}
	}
	return pl, nil
}

// Scan records a visit to a checkpoint of a zone.
//
// It is idempotent by local_id: a scan sent again returns the stored one and sends nothing. The
// Idempotency-Key middleware answers most replays itself; this covers the rest (the key expired, or
// a client that changed its key but not its local_id).
//
// The checks run in this order, and the first that fails is the answer: that the checkpoint is the zone's,
// its QR, the permission, the membership, the geofence (a flag, not a refusal), the order of the round, the
// patrol interval, then the actions and their photos.
func (s *Service) Scan(ctx context.Context, p auth.Principal, zoneID int64, in ScanInput) (ScanResult, error) {
	res, err := s.scan(ctx, p, zoneID, in)
	// Two requests with the same local_id at once: both found nothing, one insert won. The loser
	// gets the winner's scan, which is what a replay is.
	if err != nil && database.IsUniqueViolation(err) && database.ConstraintName(err) == "patrol_scans_local_id_uq" {
		return s.replay(ctx, p, zoneID, in)
	}
	return res, err
}

func (s *Service) replay(ctx context.Context, p auth.Principal, zoneID int64, in ScanInput) (ScanResult, error) {
	var out ScanResult
	err := database.WithTenant(ctx, s.pool, p.OrganizationID, func(q *db.Queries) error {
		got, found, err := s.stored(ctx, q, p, zoneID, in.LocalID)
		if err != nil {
			return err
		}
		if !found {
			return errors.New("patrol: scan vanished after a local_id conflict")
		}
		out = ScanResult{Scan: got, Replayed: true}
		return nil
	})
	return out, err
}

// stored returns the scan already saved under a local_id. A local_id that belongs to another person
// or another zone is not a replay of this request.
func (s *Service) stored(ctx context.Context, q *db.Queries, p auth.Principal, zoneID int64, localID string) (Scan, bool, error) {
	id, err := uuid.Parse(localID)
	if err != nil {
		return Scan{}, false, apperr.ErrValidation.WithFields(apperr.FieldError{Field: "local_id", Code: "INVALID", Message: "Harus berupa UUID yang valid."})
	}
	row, err := q.GetPatrolScanByLocalID(ctx, pgtype.UUID{Bytes: id, Valid: true})
	if errors.Is(err, pgx.ErrNoRows) {
		return Scan{}, false, nil
	}
	if err != nil {
		return Scan{}, false, fmt.Errorf("get scan by local id: %w", err)
	}
	if row.PersonnelID != p.PersonnelID || row.ZoneID != zoneID {
		return Scan{}, false, ErrLocalIDTaken
	}
	scan, err := scanOf(row)
	return scan, true, err
}

func (s *Service) scan(ctx context.Context, p auth.Principal, zoneID int64, in ScanInput) (ScanResult, error) {
	localID, err := uuid.Parse(in.LocalID)
	if err != nil {
		return ScanResult{}, apperr.ErrValidation.WithFields(apperr.FieldError{Field: "local_id", Code: "INVALID", Message: "Harus berupa UUID yang valid."})
	}
	// The device conditions are judged first: they need no data, and a malformed one is the client's to fix
	// whatever else is wrong.
	answers, err := withDeviceConditions(in.DeviceCondition, in.Actions)
	if err != nil {
		return ScanResult{}, err
	}
	if err := checkPosition(in.Latitude, in.Longitude, in.AccuracyMeters); err != nil {
		return ScanResult{}, err
	}
	now := s.clock.Now()
	at := in.OccurredAt.UTC()

	var (
		out     ScanResult
		notices []Notice
	)
	err = database.WithTenant(ctx, s.pool, p.OrganizationID, func(q *db.Queries) error {
		if got, found, err := s.stored(ctx, q, p, zoneID, in.LocalID); err != nil {
			return err
		} else if found {
			out = ScanResult{Scan: got, Replayed: true}
			return nil
		}

		zone, err := q.GetPatrolZone(ctx, zoneID)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrZoneNotFound
		}
		if err != nil {
			return fmt.Errorf("get zone: %w", err)
		}
		// A switched-off zone takes no more scans.
		if !zone.Active {
			return ErrZoneNotFound
		}
		project, err := q.GetProject(ctx, zone.ProjectCode)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrZoneNotFound
		}
		if err != nil {
			return fmt.Errorf("get project: %w", err)
		}

		// 1 and 2: the checkpoint is the zone's, and the QR is its own.
		v, err := s.arrive(ctx, q, p, zone, project, in.CheckpointID, in.QRPayload, at)
		if err != nil {
			return err
		}
		// 3: the permission of the zone's type, and the membership.
		if err := s.authorize(ctx, q, p, v, at); err != nil {
			return err
		}
		// 4: the position against the checkpoint's geofence. Only a flag for the DANRU (see flagLocation): a
		// refusal here would not be stored and would break the order rule for the rest of the round.
		v, flag := flagLocation(v, in.Latitude, in.Longitude, in.AccuracyMeters)
		// 5: the regu and the work date, which the order rule needs.
		pl, err := s.place(ctx, q, p, v, at)
		if err != nil {
			return err
		}

		// 6 and 7: the order of the round, and the interval, under one lock (see lockScanning).
		if err := lockScanning(ctx, q, p, zoneID); err != nil {
			return err
		}
		if err := checkOrder(ctx, q, p, zoneID, v.checkpoint.ID, pl.workDate, at); err != nil {
			return err
		}
		if err := checkScanInterval(ctx, q, p, zoneID, v.checkpoint.ID, project, at); err != nil {
			return err
		}

		// 8: the actions, as the checklist was when the guard scanned.
		all, err := q.ListAllCheckpointActions(ctx, v.checkpoint.ID)
		if err != nil {
			return fmt.Errorf("list checkpoint actions: %w", err)
		}
		entries, photos, err := checkActions(all, answers, at)
		if err != nil {
			return err
		}

		patrolID, err := s.joinPatrol(ctx, q, p, zone.ProjectCode, at)
		if err != nil {
			return err
		}

		row, err := q.InsertPatrolScan(ctx, db.InsertPatrolScanParams{
			LocalID: pgtype.UUID{Bytes: localID, Valid: true}, ZoneID: zoneID, ZoneType: zone.Type,
			PersonnelID: p.PersonnelID, TeamCode: pl.team, ProjectCode: zone.ProjectCode,
			OccurredAt: ts(at), ReceivedAt: ts(now), WorkDate: pgtype.Date{Time: pl.workDate.Time(), Valid: true},
			Latitude: in.Latitude, Longitude: in.Longitude, AccuracyMeters: in.AccuracyMeters,
			DistanceMeters: intParam(v.distance), Subject: subjectOf(in.Subject, v.checkpoint.Name), Notes: textOrNull(in.Notes),
			PatrolID: patrolID, CheckpointID: v.checkpoint.ID, SubmittedOffline: in.SubmittedOffline,
			LocationFlag: flag, Outcome: db.PatrolScanOutcomeSCANNED, SkipMediaIds: []int64{},
		})
		if err != nil {
			return fmt.Errorf("insert scan: %w", err)
		}
		if patrolID.Valid {
			if err := q.AddScanToRoute(ctx, patrolID.Int64); err != nil {
				return fmt.Errorf("count scan on patrol: %w", err)
			}
		}

		// 9: every photo of the scan, in the same transaction as the scan: each must belong to the caller
		// and be unused (media.Attach).
		if s.attach == nil && len(photos) > 0 {
			return errors.New("patrol: photo attachments are not plugged in")
		}
		if len(photos) > 0 {
			if err := s.attach.Attach(ctx, q, media.AttachInput{
				MediaIDs: photos, OwnerID: p.PersonnelID, Purpose: media.PurposePatrolScan,
				EntityType: EntityScan, EntityID: row.ID, Max: len(photos),
			}); err != nil {
				return err
			}
		}

		// 10: one row per answered action. The snapshot and the columns come from the same values.
		for _, e := range entries {
			snapshot, err := actionSnapshot(e)
			if err != nil {
				return err
			}
			if _, err := q.InsertScanAction(ctx, db.InsertScanActionParams{
				ScanID: row.ID, CheckpointID: v.checkpoint.ID, ActionID: e.action.ID, PersonnelID: p.PersonnelID,
				Note: textOrNull(e.note), MediaIds: e.mediaIDs, Snapshot: snapshot, DeviceCondition: e.deviceCondition, Now: ts(now),
			}); err != nil {
				return fmt.Errorf("insert scan action: %w", err)
			}
		}

		scan, err := scanOf(row)
		if err != nil {
			return err
		}
		// What this answer shows of the checkpoint and the actions is what the guard just sent; the photo
		// URLs are not made here (a reader asks for the scan to get them).
		scan.Checkpoint = ScanCheckpoint{ID: uuidString(v.checkpoint.ID), Name: v.checkpoint.Name, SortOrder: &v.position}
		scan.Actions = make([]ScanAction, 0, len(entries))
		for _, e := range entries {
			scan.Actions = append(scan.Actions, ScanAction{
				ActionID: uuidString(e.action.ID), Action: e.action.Action, Instruction: textPtr(e.action.Instruction),
				Note: e.note, DeviceCondition: e.deviceCondition,
			})
		}
		out = ScanResult{Scan: scan}
		notices, err = s.noticesFor(ctx, q, p, zone, project, v.checkpoint, scan)
		return err
	})
	if err != nil {
		return ScanResult{}, err
	}
	// After the commit: a notice for a scan that was rolled back would be a lie.
	s.notify(ctx, notices...)
	return out, nil
}

// checkOrder applies the order rule (order.go) to a scan of a checkpoint made at a moment: the checkpoint
// can be scanned only when the round in progress has passed the one before it. The caller holds
// lockScanning.
//
// A scan counts the person's earlier scans by occurred_at, not by arrival, so a queue made offline and
// synced in occurred_at order is judged exactly as it would have been online. ⚠ The app must send a zone's
// offline scans in that order: a later one sent first is CHECKPOINT_OUT_OF_ORDER and can be sent again once
// the earlier ones have arrived.
func checkOrder(ctx context.Context, q *db.Queries, p auth.Principal, zoneID int64, checkpointID pgtype.UUID, workDate timeutil.Date, at time.Time) error {
	order, err := loadOrder(ctx, q, p.PersonnelID, zoneID, workDate, at)
	if err != nil {
		return err
	}
	i := slices.IndexFunc(order.Steps, func(st orderStep) bool { return st.CheckpointID == checkpointID })
	if i < 0 {
		return ErrCheckpointNotInZone // switched off or unlinked since the link was read
	}
	if order.Steps[i].Scannable {
		return nil
	}
	// The place the order wants first is the one before this one: this one is behind a checkpoint the round
	// has not reached.
	return ErrCheckpointOutOfOrder.WithDetails(map[string]any{"expected_checkpoint_id": uuidString(order.Steps[i-1].CheckpointID)})
}

// EntityScan is the name of a scan as the record media and notifications point at.
const EntityScan = "PATROL_SCAN"

// noticesFor works out who to tell about a new scan: the team's DANRU (or the admins when there is
// none, or the author is the DANRU), and the author when the scan was made offline.
func (s *Service) noticesFor(ctx context.Context, q *db.Queries, p auth.Principal, zone db.PatrolZone, project db.Project,
	cp db.PatrolCheckpoint, scan Scan,
) ([]Notice, error) {
	if s.notifier == nil {
		return nil, nil
	}
	name, err := q.PersonnelNameAndNIK(ctx, p.PersonnelID)
	if err != nil {
		return nil, fmt.Errorf("get personnel name: %w", err)
	}
	base := Notice{
		ZoneType: string(zone.Type), BusinessLine: string(project.BusinessLine), ActorName: name.FullName,
		TeamCode: scan.TeamCode, Subject: scan.Subject, Date: scan.WorkDate, ScanID: scan.ID, ZoneID: zone.ID,
		CheckpointID: uuidString(cp.ID), CheckpointName: cp.Name, Outcome: scan.Outcome, LocationFlag: scan.LocationFlag,
	}
	var out []Notice
	danru, err := s.perms.DanruOf(ctx, q, scan.TeamCode, p.PersonnelID, scan.WorkDate)
	if err != nil {
		return nil, err
	}
	if len(danru) > 0 {
		n := base
		n.Kind, n.RecipientIDs = NoticeSubmitted, danru
		out = append(out, n)
	}
	if scan.SubmittedOffline {
		n := base
		n.Kind, n.RecipientIDs = NoticeOfflineSent, []int64{p.PersonnelID}
		out = append(out, n)
	}
	return out, nil
}

func (s *Service) notify(ctx context.Context, notices ...Notice) {
	if s.notifier == nil {
		return
	}
	for _, n := range notices {
		if len(n.RecipientIDs) > 0 {
			s.notifier.NotifyPatrol(ctx, n)
		}
	}
}
