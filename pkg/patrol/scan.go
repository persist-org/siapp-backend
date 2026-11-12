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

// Offline is the offline fields of the scan, for idem.OfflineCreate's checks.
func (in ScanInput) Offline() idem.OfflineCreate {
	return idem.OfflineCreate{LocalID: in.LocalID, OccurredAt: in.OccurredAt}
}

// Attacher claims uploaded photos for a record (media.Service).
type Attacher interface {
	Attach(ctx context.Context, q *db.Queries, in media.AttachInput) error
}

// NoticeKind says what a Notice is about.
type NoticeKind int

// The notices patrol sends.
const (
	// NoticeSubmitted tells the team's DANRU that a scan waits for a decision.
	NoticeSubmitted NoticeKind = iota
	// NoticeOfflineSent tells the author that a scan they made offline has been delivered.
	NoticeOfflineSent
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
}

// Notifier sends notices. cmd/api adapts notifications.Service to it.
type Notifier interface {
	NotifyPatrol(ctx context.Context, n Notice)
}

// ScanInput is the body of POST /zones/{id}/scan. local_id (which is also the Idempotency-Key) and
// occurred_at are the offline fields of idem.OfflineCreate, declared here rather than embedded so
// that a validation error names them as the client sent them ("local_id", not
// "OfflineCreate.local_id").
type ScanInput struct {
	LocalID          string        `json:"local_id" validate:"required,uuid"`
	OccurredAt       time.Time     `json:"occurred_at" validate:"required"`
	QRPayload        string        `json:"qr_payload" validate:"required,max=200"`
	Latitude         *float64      `json:"latitude" validate:"required,gte=-90,lte=90"`
	Longitude        *float64      `json:"longitude" validate:"required,gte=-180,lte=180"`
	AccuracyMeters   *float64      `json:"accuracy_meters" validate:"required,gte=0"`
	Subject          string        `json:"subject" validate:"required,notblank,max=200"`
	Notes            *string       `json:"notes" validate:"omitempty,max=2000"`
	Answers          []AnswerInput `json:"answers" validate:"omitempty,max=100,dive"`
	MediaIDs         []int64       `json:"media_ids" validate:"omitempty,max=20,dive,min=1"`
	SubmittedOffline bool          `json:"submitted_offline"`
}

// Scan is a visit to a zone, as it was recorded.
type Scan struct {
	ID             int64
	LocalID        string
	ZoneID         int64
	ZoneType       string
	PersonnelID    int64
	TeamCode       string
	ProjectCode    string
	OccurredAt     time.Time
	ReceivedAt     time.Time
	WorkDate       timeutil.Date
	Latitude       float64
	Longitude      float64
	AccuracyMeters float64
	// DistanceMeters is how far from the zone's centre the scan was made; nil for a zone with no
	// geofence.
	DistanceMeters   *int
	Subject          string
	Notes            *string
	Answers          []AnswerEntry
	Status           string
	PatrolID         *int64
	SubmittedOffline bool
	CreatedAt        time.Time
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
	}
	if r.DistanceMeters.Valid {
		d := int(r.DistanceMeters.Int32)
		out.DistanceMeters = &d
	}
	if r.PatrolID.Valid {
		out.PatrolID = &r.PatrolID.Int64
	}
	if err := json.Unmarshal(r.Answers, &out.Answers); err != nil {
		return Scan{}, fmt.Errorf("scan %d: decode answers: %w", r.ID, err)
	}
	if out.Answers == nil {
		out.Answers = []AnswerEntry{}
	}
	return out, nil
}

// visit is a person arriving at a zone: what the checks established.
type visit struct {
	zone     db.PatrolZone
	project  db.Project
	loc      *time.Location
	distance *int
}

// admit runs the checks a scan, a check-in and a check-out have in common, in this order: the QR
// code, the permission, the membership and the geofence. The zone and its project are already
// loaded.
//
// ⚠ The QR comes first. A request with a wrong code learns nothing else about the zone, not even
// whether the caller may use it.
func (s *Service) admit(ctx context.Context, q *db.Queries, p auth.Principal, zone db.PatrolZone, project db.Project,
	qrPayload string, lat, lng, accuracy float64, at time.Time,
) (visit, error) {
	v := visit{zone: zone, project: project}
	if !verifyPayload(qrPayload, p.OrganizationID, zone.ID, zone.QrSecret) {
		return v, ErrQRInvalid
	}
	loc, err := time.LoadLocation(project.Timezone)
	if err != nil {
		return v, fmt.Errorf("project %s: timezone %q: %w", project.Code, project.Timezone, err)
	}
	v.loc = loc
	day := timeutil.BusinessDate(at, loc)

	// The permission is the one of the zone's type, judged on the date of the scan in the project's
	// timezone: someone who changed regu this morning still has yesterday's scans in the old one.
	perm := access.PatrolScan
	if zone.Type == db.PatrolZoneTypeGUARD {
		perm = access.GuardScan
	}
	if s.perms == nil {
		return v, errors.New("patrol: permissions are not plugged in")
	}
	res, err := s.perms.Resolve(ctx, q, p.PersonnelID, perm, day)
	if err != nil {
		return v, err
	}
	if !res.Allowed {
		return v, access.ErrPermissionDenied
	}

	if _, err := s.membershipIn(ctx, q, p.PersonnelID, zone, day); err != nil {
		return v, err
	}

	if zone.Latitude != nil && zone.Longitude != nil && zone.RadiusMeters.Valid {
		d := distanceTo(*zone.Latitude, *zone.Longitude, lat, lng)
		if !insideZone(d, float64(zone.RadiusMeters.Int32), accuracy) {
			return v, ErrOutsideRadius.WithDetails(map[string]any{
				"distance_meters": int(d + 0.5), "radius_meters": int(zone.RadiusMeters.Int32),
			})
		}
		rounded := int(d + 0.5)
		v.distance = &rounded
	}
	return v, nil
}

// membershipIn returns the regu of the person that count for the zone on a date: those in the
// zone's project, and when the zone is for one regu, only that regu. None is ZONE_NOT_IN_PROJECT.
func (s *Service) membershipIn(ctx context.Context, q *db.Queries, personnelID int64, zone db.PatrolZone, day timeutil.Date) ([]string, error) {
	rows, err := q.ListMembershipsOfPersonnelOn(ctx, db.ListMembershipsOfPersonnelOnParams{
		PersonnelID: personnelID, OnDate: pgtype.Date{Time: day.Time(), Valid: true},
	})
	if err != nil {
		return nil, fmt.Errorf("list memberships: %w", err)
	}
	var teams []string
	for _, r := range rows {
		if r.ProjectCode == zone.ProjectCode && (!zone.TeamCode.Valid || r.TeamCode == zone.TeamCode.String) {
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
// organization.Shifts.Current(person, at) for the zone's project. That is what makes a night-shift
// scan after midnight count for the day the shift started, the same date attendance uses.
// When no shift matches (an unscheduled round, an OFF day, a schedule that is ambiguous between two
// timezones) the visit still counts: it goes to the person's first active regu in the project, on
// the business date of the moment in the project's timezone.
func (s *Service) place(ctx context.Context, q *db.Queries, p auth.Principal, v visit, at time.Time) (placement, error) {
	day := timeutil.BusinessDate(at, v.loc)
	teams, err := s.membershipIn(ctx, q, p.PersonnelID, v.zone, day)
	if err != nil {
		return placement{}, err
	}
	pl := placement{team: teams[0], workDate: day}

	if s.shifts == nil {
		return pl, nil
	}
	instances, err := s.shifts.Current(ctx, p.OrganizationID, p.PersonnelID, at)
	switch {
	case errors.Is(err, organization.ErrScheduleTimezoneAmbiguous):
		return pl, nil
	case err != nil:
		return placement{}, fmt.Errorf("current shifts: %w", err)
	}
	for _, inst := range instances {
		if !slices.Contains(inst.ProjectCodes, v.zone.ProjectCode) {
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

// Scan records a visit to a zone.
//
// It is idempotent by local_id: a scan sent again returns the stored one and sends nothing. The
// Idempotency-Key middleware answers most replays itself; this covers the rest (the key expired, or
// a client that changed its key but not its local_id).
//
// The checks run in this order, and the first that fails is the answer: the QR, the permission, the
// membership, the geofence, the patrol interval, then the checklist and the photos.
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

		v, err := s.admit(ctx, q, p, zone, project, in.QRPayload, *in.Latitude, *in.Longitude, *in.AccuracyMeters, at)
		if err != nil {
			return err
		}

		// ⚠ The patrol interval (DL-48): a second scan of the same zone by the same person within
		// project.patrol_interval_minutes is refused. The lock makes two scans that arrive together
		// take turns, so both cannot pass the check before either is stored.
		if project.PatrolIntervalMinutes.Valid {
			if err := q.LockPatrolKey(ctx, fmt.Sprintf("patrol:scan:%d:%d:%d", p.OrganizationID, p.PersonnelID, zoneID)); err != nil {
				return fmt.Errorf("lock scan: %w", err)
			}
			interval := time.Duration(project.PatrolIntervalMinutes.Int32) * time.Minute
			// Either side of the scan's own moment: an offline scan can arrive after a later one.
			tooSoon, err := q.HasScanBetween(ctx, db.HasScanBetweenParams{
				PersonnelID: p.PersonnelID, ZoneID: zoneID, After: ts(at.Add(-interval)), Before: ts(at.Add(interval)),
			})
			if err != nil {
				return fmt.Errorf("check patrol interval: %w", err)
			}
			if tooSoon {
				return ErrIntervalNotElapsed.WithDetails(map[string]any{"interval_minutes": int(project.PatrolIntervalMinutes.Int32)})
			}
		}

		pl, err := s.place(ctx, q, p, v, at)
		if err != nil {
			return err
		}

		items, err := q.ListAllChecklistItems(ctx, zoneID)
		if err != nil {
			return fmt.Errorf("list checklist: %w", err)
		}
		entries, answerMedia, err := checkAnswers(items, in.Answers, at)
		if err != nil {
			return err
		}
		// ⚠ photo_required: one photo in the whole scan is enough, wherever it is, even when no
		// question asks for one (mobile spec SQD §4.3).
		allMedia := append(slices.Clone(in.MediaIDs), answerMedia...)
		if zone.PhotoRequired && len(allMedia) == 0 {
			return ErrPhotoRequired
		}
		answers, err := json.Marshal(entries)
		if err != nil {
			return fmt.Errorf("encode answers: %w", err)
		}

		row, err := q.InsertPatrolScan(ctx, db.InsertPatrolScanParams{
			LocalID: pgtype.UUID{Bytes: localID, Valid: true}, ZoneID: zoneID, ZoneType: zone.Type,
			PersonnelID: p.PersonnelID, TeamCode: pl.team, ProjectCode: zone.ProjectCode,
			OccurredAt: ts(at), ReceivedAt: ts(now), WorkDate: pgtype.Date{Time: pl.workDate.Time(), Valid: true},
			Latitude: *in.Latitude, Longitude: *in.Longitude, AccuracyMeters: *in.AccuracyMeters,
			DistanceMeters: intParam(v.distance), Subject: in.Subject, Notes: textOrNull(in.Notes),
			Answers: answers, SubmittedOffline: in.SubmittedOffline,
		})
		if err != nil {
			return fmt.Errorf("insert scan: %w", err)
		}

		// Every photo of the scan, the general ones and those of the answers, in the same
		// transaction as the scan: each must belong to the caller and be unused (media.Attach).
		if s.attach == nil && len(allMedia) > 0 {
			return errors.New("patrol: photo attachments are not plugged in")
		}
		if len(allMedia) > 0 {
			if err := s.attach.Attach(ctx, q, media.AttachInput{
				MediaIDs: allMedia, OwnerID: p.PersonnelID, Purpose: media.PurposePatrolScan,
				EntityType: EntityScan, EntityID: row.ID, Max: len(allMedia),
			}); err != nil {
				return err
			}
		}

		scan, err := scanOf(row)
		if err != nil {
			return err
		}
		out = ScanResult{Scan: scan}
		notices, err = s.noticesFor(ctx, q, p, zone, project, scan)
		return err
	})
	if err != nil {
		return ScanResult{}, err
	}
	// After the commit: a notice for a scan that was rolled back would be a lie.
	s.notify(ctx, notices...)
	return out, nil
}

// EntityScan is the name of a scan as the record media and notifications point at.
const EntityScan = "PATROL_SCAN"

// noticesFor works out who to tell about a new scan: the team's DANRU (or the admins when there is
// none, or the author is the DANRU), and the author when the scan was made offline.
func (s *Service) noticesFor(ctx context.Context, q *db.Queries, p auth.Principal, zone db.PatrolZone, project db.Project, scan Scan) ([]Notice, error) {
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
