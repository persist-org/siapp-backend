package patrol

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"siapp/internal/db"
	"siapp/pkg/access"
	"siapp/pkg/platform/apperr"
	"siapp/pkg/platform/auth"
	"siapp/pkg/platform/database"
	"siapp/pkg/platform/geo"
	"siapp/pkg/platform/idem"
	"siapp/pkg/platform/timeutil"
)

// MaxRoutePoints is how many points one request may carry (POST /patrols/{id}/points and the last
// batch of /end).
const MaxRoutePoints = 200

// maxStoredAccuracy is the largest accuracy a point is stored with. The column is numeric(8,2), and a
// phone with no fix may report anything: a larger value is stored as this one.
const maxStoredAccuracy = 100000.0

// maxPointFuture is how far ahead of the server's clock a point's time may be: a phone's clock is
// never exact, but a point from the future is a broken clock.
const maxPointFuture = 5 * time.Minute

// Route statuses.
const (
	RouteActive    = "ACTIVE"
	RouteEnded     = "ENDED"
	RouteAutoEnded = "AUTO_ENDED"
)

// StartInput is the body of POST /patrols/start. local_id (which is also the Idempotency-Key) and
// started_at are declared here like a scan's, so that a validation error names them as the client
// sent them.
type StartInput struct {
	LocalID          string    `json:"local_id" validate:"required,uuid"`
	StartedAt        time.Time `json:"started_at" validate:"required"`
	ProjectCode      string    `json:"project_code" validate:"required,notblank,max=100"`
	Latitude         *float64  `json:"latitude" validate:"required,gte=-90,lte=90"`
	Longitude        *float64  `json:"longitude" validate:"required,gte=-180,lte=180"`
	AccuracyMeters   *float64  `json:"accuracy_meters" validate:"required,gte=0,lte=100000"`
	SubmittedOffline bool      `json:"submitted_offline"`
}

// Offline is the offline fields of the start, for idem.OfflineCreate's checks.
func (in StartInput) Offline() idem.OfflineCreate {
	return idem.OfflineCreate{LocalID: in.LocalID, OccurredAt: in.StartedAt}
}

// PointInput is one position the app recorded while walking. The app numbers its points: seq is what
// makes a retried batch harmless.
type PointInput struct {
	Seq            *int32    `json:"seq" validate:"required,gte=0"`
	LoggedAt       time.Time `json:"logged_at" validate:"required"`
	Latitude       *float64  `json:"latitude" validate:"required,gte=-90,lte=90"`
	Longitude      *float64  `json:"longitude" validate:"required,gte=-180,lte=180"`
	AccuracyMeters *float64  `json:"accuracy_meters" validate:"required,gte=0"`
}

// PointsInput is the body of POST /patrols/{id}/points. The size is not a validation tag: more than
// MaxRoutePoints is PATROL_POINTS_TOO_LARGE, a reason of its own.
type PointsInput struct {
	Points []PointInput `json:"points" validate:"required,min=1,dive"`
}

// EndInput is the body of POST /patrols/{id}/end: the moment it ended, and optionally the last batch
// of points.
type EndInput struct {
	EndedAt time.Time    `json:"ended_at" validate:"required"`
	Points  []PointInput `json:"points" validate:"omitempty,dive"`
}

// PointsResult says what became of a batch: how many points were new, and how many had been
// received before (a retry).
type PointsResult struct {
	Accepted   int
	Duplicates int
}

// Route is a patrol as the list shows it.
type Route struct {
	ID               int64
	LocalID          string
	PersonnelID      int64
	PersonnelName    string
	TeamCode         string
	ProjectCode      string
	Status           string
	StartedAt        time.Time
	EndedAt          *time.Time
	WorkDate         timeutil.Date
	DistanceMeters   int
	PointCount       int
	ScanCount        int
	SubmittedOffline bool
	LastPointAt      time.Time
}

// RouteScan is a scan of a patrol as a pin on its line.
type RouteScan struct {
	ID         int64
	ZoneID     int64
	ZoneName   string
	OccurredAt time.Time
	Latitude   float64
	Longitude  float64
	Status     string
	Subject    string
}

// RouteDetail is one patrol with its line and the scans along it.
type RouteDetail struct {
	Route
	// Path is the line, in the order walked. For an ACTIVE patrol it is built from the points
	// received so far; for an ended one it is the stored path.
	Path  []RoutePoint
	Scans []RouteScan
}

// RouteResult is what Start returns. Replayed is true when the patrol had been started already: the
// caller answers 200 instead of 201.
type RouteResult struct {
	Route    Route
	Replayed bool
}

func routeOf(r db.PatrolRoute) Route {
	out := Route{
		ID: r.ID, LocalID: uuid.UUID(r.LocalID.Bytes).String(), PersonnelID: r.PersonnelID, TeamCode: r.TeamCode,
		ProjectCode: r.ProjectCode, Status: string(r.Status), StartedAt: r.StartedAt.Time, WorkDate: dateOf(r.WorkDate),
		DistanceMeters: int(r.DistanceMeters), PointCount: int(r.PointCount), ScanCount: int(r.ScanCount),
		SubmittedOffline: r.SubmittedOffline, LastPointAt: r.LastPointAt.Time,
	}
	if r.EndedAt.Valid {
		out.EndedAt = &r.EndedAt.Time
	}
	return out
}

// storedPoint is a point of the stored path, as it is written in patrol_routes.path.
type storedPoint struct {
	Lat float64   `json:"lat"`
	Lng float64   `json:"lng"`
	At  time.Time `json:"at"`
	Acc float64   `json:"acc"`
}

func encodePath(path []RoutePoint) ([]byte, error) {
	out := make([]storedPoint, 0, len(path))
	for _, p := range path {
		out = append(out, storedPoint{Lat: p.Lat, Lng: p.Lng, At: p.At.UTC(), Acc: p.Accuracy})
	}
	return json.Marshal(out)
}

// decodePath reads a stored path. A stored point has no seq: its place in the array is its order.
func decodePath(raw []byte) ([]RoutePoint, error) {
	var stored []storedPoint
	if err := json.Unmarshal(raw, &stored); err != nil {
		return nil, err
	}
	out := make([]RoutePoint, 0, len(stored))
	for i, p := range stored {
		out = append(out, RoutePoint{Seq: int32(i), At: p.At, Lat: p.Lat, Lng: p.Lng, Accuracy: p.Acc}) //nolint:gosec // a path holds at most maxPathPoints
	}
	return out, nil
}

// ---------- start ----------

// Start begins a patrol: the guard taps "start patrol" and the app records its positions from here.
//
// It needs the permission to scan (patrol.scan or guard.scan, either) and a place in the project,
// like a scan, and no other patrol running: a guard walks one round at a time
// (PATROL_ALREADY_ACTIVE). It is idempotent by local_id. The start is the first point, seq 0.
//
// ⚠ Recording starts here and stops at the end, and needs no gps_tracking_enabled of the project:
// the guard asked for it by starting a patrol. The plan flags this to be confirmed against the
// privacy contract before it goes live; the consent is the "start patrol" tap, nothing else.
func (s *Service) Start(ctx context.Context, p auth.Principal, in StartInput) (RouteResult, error) {
	res, err := s.start(ctx, p, in)
	if err == nil || !database.IsUniqueViolation(err) {
		return res, err
	}
	switch database.ConstraintName(err) {
	case "patrol_routes_local_id_uq":
		// Two starts with the same local_id at once: one insert won, the loser gets its patrol.
		return s.replayStart(ctx, p, in)
	case "patrol_routes_active_uq":
		// Usually another patrol is running. But two identical starts at once can end here too, when
		// the other insert is the same patrol: then this is a replay, not a conflict.
		if got, rerr := s.replayStart(ctx, p, in); rerr == nil {
			return got, nil
		}
		return RouteResult{}, ErrPatrolAlreadyActive
	}
	return res, err
}

func (s *Service) replayStart(ctx context.Context, p auth.Principal, in StartInput) (RouteResult, error) {
	var out RouteResult
	err := database.WithTenant(ctx, s.pool, p.OrganizationID, func(q *db.Queries) error {
		got, found, err := s.storedRoute(ctx, q, p, in.LocalID)
		if err != nil {
			return err
		}
		if !found {
			return errors.New("patrol: patrol vanished after a local_id conflict")
		}
		out = RouteResult{Route: got, Replayed: true}
		return nil
	})
	return out, err
}

// storedRoute returns the patrol already saved under a local_id. A local_id that belongs to another
// person is not a replay of this request.
func (s *Service) storedRoute(ctx context.Context, q *db.Queries, p auth.Principal, localID string) (Route, bool, error) {
	id, err := uuid.Parse(localID)
	if err != nil {
		return Route{}, false, apperr.ErrValidation.WithFields(apperr.FieldError{Field: "local_id", Code: "INVALID", Message: "Harus berupa UUID yang valid."})
	}
	row, err := q.GetPatrolRouteByLocalID(ctx, pgtype.UUID{Bytes: id, Valid: true})
	if errors.Is(err, pgx.ErrNoRows) {
		return Route{}, false, nil
	}
	if err != nil {
		return Route{}, false, fmt.Errorf("get patrol by local id: %w", err)
	}
	if row.PersonnelID != p.PersonnelID {
		return Route{}, false, ErrPatrolLocalIDTaken
	}
	return routeOf(row), true, nil
}

// mayScan reports whether the person holds the permission to scan a zone of either type on the date.
func (s *Service) mayScan(ctx context.Context, q *db.Queries, personnelID int64, day timeutil.Date) error {
	if s.perms == nil {
		return errors.New("patrol: permissions are not plugged in")
	}
	for _, perm := range []access.Permission{access.PatrolScan, access.GuardScan} {
		res, err := s.perms.Resolve(ctx, q, personnelID, perm, day)
		if err != nil {
			return err
		}
		if res.Allowed {
			return nil
		}
	}
	return access.ErrPermissionDenied
}

func (s *Service) start(ctx context.Context, p auth.Principal, in StartInput) (RouteResult, error) {
	localID, err := uuid.Parse(in.LocalID)
	if err != nil {
		return RouteResult{}, apperr.ErrValidation.WithFields(apperr.FieldError{Field: "local_id", Code: "INVALID", Message: "Harus berupa UUID yang valid."})
	}
	now := s.clock.Now()
	at := in.StartedAt.UTC()

	var out RouteResult
	err = database.WithTenant(ctx, s.pool, p.OrganizationID, func(q *db.Queries) error {
		if got, found, err := s.storedRoute(ctx, q, p, in.LocalID); err != nil {
			return err
		} else if found {
			out = RouteResult{Route: got, Replayed: true}
			return nil
		}

		project, err := q.GetProject(ctx, in.ProjectCode)
		if errors.Is(err, pgx.ErrNoRows) {
			// A project that is not there and one the person does not serve in are answered alike.
			return errNotInProject
		}
		if err != nil {
			return fmt.Errorf("get project: %w", err)
		}
		loc, err := time.LoadLocation(project.Timezone)
		if err != nil {
			return fmt.Errorf("project %s: timezone %q: %w", project.Code, project.Timezone, err)
		}
		if err := s.mayScan(ctx, q, p.PersonnelID, timeutil.BusinessDate(at, loc)); err != nil {
			return err
		}
		// The regu and the work date from the shift rule of a scan (see place), asked for the whole
		// project: a patrol belongs to a project and may cross several zones.
		pl, err := s.placeIn(ctx, q, p, project.Code, pgtype.Text{}, loc, at)
		if errors.Is(err, ErrNotInProject) {
			return errNotInProject
		}
		if err != nil {
			return err
		}

		row, err := q.InsertPatrolRoute(ctx, db.InsertPatrolRouteParams{
			LocalID: pgtype.UUID{Bytes: localID, Valid: true}, PersonnelID: p.PersonnelID, TeamCode: pl.team,
			ProjectCode: project.Code, StartedAt: ts(at), WorkDate: pgtype.Date{Time: pl.workDate.Time(), Valid: true},
			SubmittedOffline: in.SubmittedOffline, Now: ts(now),
		})
		if err != nil {
			return fmt.Errorf("insert patrol: %w", err)
		}
		// The start is the first point, seq 0.
		if _, err := q.InsertRoutePoints(ctx, db.InsertRoutePointsParams{
			PatrolID: row.ID, Seqs: []int32{0}, LoggedAts: []pgtype.Timestamptz{ts(at)},
			Lats: []float64{*in.Latitude}, Lngs: []float64{*in.Longitude}, Accs: []float64{*in.AccuracyMeters},
		}); err != nil {
			return fmt.Errorf("insert first point: %w", err)
		}
		out = RouteResult{Route: routeOf(row)}
		return nil
	})
	return out, err
}

// errNotInProject is ZONE_NOT_IN_PROJECT with words that fit a patrol, which has a project, not a zone.
var errNotInProject = ErrNotInProject.WithMessage("Anda tidak bertugas di proyek ini.")

// ---------- points ----------

// routeLockKey names a patrol for the advisory lock that makes the things that change it take turns:
// a batch of points, the end, a scan that joins it.
func routeLockKey(orgID, patrolID int64) string {
	return fmt.Sprintf("patrol:route:%d:%d", orgID, patrolID)
}

// ownRoute loads a patrol of the caller's, with the patrol's lock held. Another person's patrol, and
// one that is not there, are the same answer.
func (s *Service) ownRoute(ctx context.Context, q *db.Queries, p auth.Principal, id int64) (db.PatrolRoute, error) {
	if err := q.LockPatrolKey(ctx, routeLockKey(p.OrganizationID, id)); err != nil {
		return db.PatrolRoute{}, fmt.Errorf("lock patrol: %w", err)
	}
	row, err := q.GetPatrolRoute(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return db.PatrolRoute{}, ErrPatrolNotFound
	}
	if err != nil {
		return db.PatrolRoute{}, fmt.Errorf("get patrol: %w", err)
	}
	if row.PersonnelID != p.PersonnelID {
		return db.PatrolRoute{}, ErrPatrolNotFound
	}
	return row, nil
}

// checkPoints validates a batch and returns it as the columns the insert takes.
func (s *Service) checkPoints(points []PointInput, field string) (db.InsertRoutePointsParams, error) {
	var out db.InsertRoutePointsParams
	if len(points) > MaxRoutePoints {
		return out, ErrPointsTooLarge.WithDetails(map[string]any{"max": MaxRoutePoints})
	}
	latest := s.clock.Now().Add(maxPointFuture)
	for i, pt := range points {
		if pt.LoggedAt.After(latest) {
			return out, apperr.ErrValidation.WithFields(apperr.FieldError{
				Field: fmt.Sprintf("%s[%d].logged_at", field, i), Code: "IN_FUTURE", Message: "Tidak boleh di masa depan.",
			})
		}
		out.Seqs = append(out.Seqs, *pt.Seq)
		out.LoggedAts = append(out.LoggedAts, ts(pt.LoggedAt.UTC()))
		out.Lats = append(out.Lats, *pt.Latitude)
		out.Lngs = append(out.Lngs, *pt.Longitude)
		// ⚠ Capped, not refused: one bad fix must not make a whole batch fail, or the app would retry it
		// for ever and no later point would arrive. A point this inaccurate is dropped from the line
		// anyway (maxPointAccuracy).
		out.Accs = append(out.Accs, min(*pt.AccuracyMeters, maxStoredAccuracy))
	}
	return out, nil
}

// addPoints stores a batch on a patrol and says how many points were new.
func (s *Service) addPoints(ctx context.Context, q *db.Queries, route db.PatrolRoute, batch db.InsertRoutePointsParams) (PointsResult, error) {
	if len(batch.Seqs) == 0 {
		return PointsResult{}, nil
	}
	batch.PatrolID = route.ID
	n, err := q.InsertRoutePoints(ctx, batch)
	if err != nil {
		return PointsResult{}, fmt.Errorf("insert points: %w", err)
	}
	latest := batch.LoggedAts[0].Time
	for _, t := range batch.LoggedAts {
		if t.Time.After(latest) {
			latest = t.Time
		}
	}
	if err := q.NoteRoutePoints(ctx, db.NoteRoutePointsParams{Latest: ts(latest), Added: int32(n), Now: ts(s.clock.Now()), ID: route.ID}); err != nil { //nolint:gosec // at most MaxRoutePoints
		return PointsResult{}, fmt.Errorf("note points: %w", err)
	}
	return PointsResult{Accepted: int(n), Duplicates: len(batch.Seqs) - int(n)}, nil
}

// AddPoints stores a batch of positions of the caller's running patrol.
//
// It is safe to send the same batch again, or batches out of order: a point is (patrol, seq), and one
// that is there already is skipped and counted as a duplicate. A patrol that has ended takes no more
// points (PATROL_NOT_ACTIVE): an app that was offline sends what it has, then ends the patrol.
func (s *Service) AddPoints(ctx context.Context, p auth.Principal, id int64, in PointsInput) (PointsResult, error) {
	batch, err := s.checkPoints(in.Points, "points")
	if err != nil {
		return PointsResult{}, err
	}
	var out PointsResult
	err = database.WithTenant(ctx, s.pool, p.OrganizationID, func(q *db.Queries) error {
		route, err := s.ownRoute(ctx, q, p, id)
		if err != nil {
			return err
		}
		if route.Status != db.PatrolRouteStatusACTIVE {
			return ErrPatrolNotActive
		}
		out, err = s.addPoints(ctx, q, route, batch)
		return err
	})
	return out, err
}

// ---------- end ----------

// End finishes the caller's patrol: the last points, the path, the length, the count of scans, and
// the status ENDED. A retry of the same end (the same ended_at) returns the patrol as stored; any other
// end of a patrol that is over, by the guard or by patrol:auto-end, is PATROL_NOT_ACTIVE.
func (s *Service) End(ctx context.Context, p auth.Principal, id int64, in EndInput) (RouteDetail, error) {
	var out RouteDetail
	err := database.WithTenant(ctx, s.pool, p.OrganizationID, func(q *db.Queries) error {
		route, err := s.ownRoute(ctx, q, p, id)
		if err != nil {
			return err
		}
		if route.Status != db.PatrolRouteStatusACTIVE {
			// Only a retry of the very same end (the app lost the response) gets the stored patrol
			// back. Any other end of a patrol that is over is an error: the guard must not think a
			// second "end" changed anything.
			if !route.EndedAt.Valid || !sameInstant(route.EndedAt.Time, in.EndedAt) {
				return ErrPatrolNotActive
			}
		}
		if route.Status == db.PatrolRouteStatusACTIVE {
			endedAt := in.EndedAt.UTC()
			if err := idem.CheckOccurredAt(endedAt, s.clock.Now()); err != nil {
				return err
			}
			if endedAt.Before(route.StartedAt.Time) {
				return apperr.ErrValidation.WithFields(apperr.FieldError{
					Field: "ended_at", Code: "BEFORE_START", Message: "Tidak boleh sebelum waktu mulai patroli.",
				})
			}
			batch, err := s.checkPoints(in.Points, "points")
			if err != nil {
				return err
			}
			if _, err := s.addPoints(ctx, q, route, batch); err != nil {
				return err
			}
			if route, err = s.finish(ctx, q, route, db.PatrolRouteStatusENDED, endedAt); err != nil {
				return err
			}
		}
		out, err = s.detail(ctx, q, route)
		return err
	})
	return out, err
}

// sameInstant compares a stored time with one a client sent. PostgreSQL keeps microseconds and rounds
// what it gets, so a time sent with nanoseconds is the same instant when it is less than a
// microsecond away; truncating would refuse half of them.
func sameInstant(stored, sent time.Time) bool {
	d := stored.Sub(sent)
	return d > -time.Microsecond && d < time.Microsecond
}

// finish ends an ACTIVE patrol: it builds the path from the points received, counts the scans that
// joined it, and stores both with the new status. The caller holds the patrol's lock, so no point
// and no scan arrives between the reading and the writing.
func (s *Service) finish(ctx context.Context, q *db.Queries, route db.PatrolRoute, status db.PatrolRouteStatus, endedAt time.Time) (db.PatrolRoute, error) {
	raw, scans, err := s.pointsAndScans(ctx, q, route.ID)
	if err != nil {
		return db.PatrolRoute{}, err
	}
	pins := make([]geo.Point, 0, len(scans))
	for _, sc := range scans {
		pins = append(pins, geo.Point{Lat: sc.Latitude, Lng: sc.Longitude})
	}
	path, distance := BuildPath(raw, pins)
	encoded, err := encodePath(path)
	if err != nil {
		return db.PatrolRoute{}, fmt.Errorf("encode path: %w", err)
	}
	row, err := q.EndPatrolRoute(ctx, db.EndPatrolRouteParams{
		Status: status, EndedAt: ts(endedAt), Path: encoded, DistanceMeters: int32(distance + 0.5), //nolint:gosec // a patrol is not 2 million km long
		PointCount: int32(len(raw)), ScanCount: int32(len(scans)), Now: ts(s.clock.Now()), ID: route.ID, //nolint:gosec // bounded by what the phone sent
	})
	if err != nil {
		return db.PatrolRoute{}, fmt.Errorf("end patrol: %w", err)
	}
	return row, nil
}

// routePoints reads the raw points of a patrol, in the order of their seq.
func (s *Service) routePoints(ctx context.Context, q *db.Queries, id int64) ([]RoutePoint, error) {
	rows, err := q.ListRoutePoints(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("list points: %w", err)
	}
	raw := make([]RoutePoint, 0, len(rows))
	for _, r := range rows {
		raw = append(raw, RoutePoint{Seq: r.Seq, At: r.LoggedAt.Time, Lat: r.Latitude, Lng: r.Longitude, Accuracy: r.AccuracyMeters})
	}
	return raw, nil
}

// pointsAndScans reads what a path is built from: the raw points in order, and the scans of the patrol.
func (s *Service) pointsAndScans(ctx context.Context, q *db.Queries, id int64) ([]RoutePoint, []db.ListRouteScansRow, error) {
	raw, err := s.routePoints(ctx, q, id)
	if err != nil {
		return nil, nil, err
	}
	scans, err := q.ListRouteScans(ctx, pgtype.Int8{Int64: id, Valid: true})
	if err != nil {
		return nil, nil, fmt.Errorf("list patrol scans: %w", err)
	}
	return raw, scans, nil
}

// joinPatrol finds the patrol a scan made at a moment belongs to: the one the person had running in
// the project then (for an offline scan, the one that was running when it was made, which may have
// ended since). No patrol is fine: a scan outside a patrol is as valid as one inside.
//
// It takes the patrol's lock before it decides, and asks again after, so that it and End take turns:
// a scan either joins before the patrol is counted, or finds it ended and, when it was made before the
// end, is added to the count afterwards.
func (s *Service) joinPatrol(ctx context.Context, q *db.Queries, p auth.Principal, projectCode string, at time.Time) (pgtype.Int8, error) {
	find := func() (pgtype.Int8, error) {
		id, err := q.FindRouteForScan(ctx, db.FindRouteForScanParams{PersonnelID: p.PersonnelID, ProjectCode: projectCode, At: ts(at)})
		if errors.Is(err, pgx.ErrNoRows) {
			return pgtype.Int8{}, nil
		}
		if err != nil {
			return pgtype.Int8{}, fmt.Errorf("find patrol of scan: %w", err)
		}
		return pgtype.Int8{Int64: id, Valid: true}, nil
	}
	first, err := find()
	if err != nil || !first.Valid {
		return pgtype.Int8{}, err
	}
	if err := q.LockPatrolKey(ctx, routeLockKey(p.OrganizationID, first.Int64)); err != nil {
		return pgtype.Int8{}, fmt.Errorf("lock patrol: %w", err)
	}
	return find()
}

// ---------- reading ----------

// detail builds one patrol with its line and its scans. An ACTIVE patrol has no stored path yet, so
// the line is built from the points received so far, and a DANRU watching sees it grow.
func (s *Service) detail(ctx context.Context, q *db.Queries, row db.PatrolRoute) (RouteDetail, error) {
	out := RouteDetail{Route: routeOf(row)}
	names, err := q.ListPersonnelNames(ctx, []int64{row.PersonnelID})
	if err != nil {
		return RouteDetail{}, fmt.Errorf("list names: %w", err)
	}
	if len(names) > 0 {
		out.PersonnelName = names[0].FullName
	}

	scans, err := q.ListRouteScans(ctx, pgtype.Int8{Int64: row.ID, Valid: true})
	if err != nil {
		return RouteDetail{}, fmt.Errorf("list patrol scans: %w", err)
	}
	out.Scans = make([]RouteScan, 0, len(scans))
	pins := make([]geo.Point, 0, len(scans))
	for _, sc := range scans {
		out.Scans = append(out.Scans, RouteScan{
			ID: sc.ID, ZoneID: sc.ZoneID, ZoneName: sc.ZoneName, OccurredAt: sc.OccurredAt.Time,
			Latitude: sc.Latitude, Longitude: sc.Longitude, Status: string(sc.Status), Subject: sc.Subject,
		})
		pins = append(pins, geo.Point{Lat: sc.Latitude, Lng: sc.Longitude})
	}

	if row.Status != db.PatrolRouteStatusACTIVE {
		out.Path, err = decodePath(row.Path)
		if err != nil {
			return RouteDetail{}, fmt.Errorf("patrol %d: decode path: %w", row.ID, err)
		}
		return out, nil
	}
	raw, err := s.routePoints(ctx, q, row.ID)
	if err != nil {
		return RouteDetail{}, err
	}
	out.Path, _ = BuildPath(raw, pins)
	return out, nil
}

// Active returns the caller's running patrol, with its line so far, for the app to resume after a
// restart.
func (s *Service) Active(ctx context.Context, p auth.Principal) (*RouteDetail, error) {
	var out *RouteDetail
	err := database.WithTenant(ctx, s.pool, p.OrganizationID, func(q *db.Queries) error {
		row, err := q.GetActivePatrolRoute(ctx, p.PersonnelID)
		if errors.Is(err, pgx.ErrNoRows) {
			// No running patrol is an answer, not an error: the app asks on every start.
			return nil
		}
		if err != nil {
			return fmt.Errorf("get active patrol: %w", err)
		}
		d, err := s.detail(ctx, q, row)
		if err != nil {
			return err
		}
		out = &d
		return nil
	})
	return out, err
}

// routeReach returns the regu whose patrols the person may read as a DANRU: those of their approve
// permission for patrol zones and for guard zones, on a date (nil: today).
func (s *Service) routeReach(ctx context.Context, q *db.Queries, personnelID int64, day *timeutil.Date) ([]string, error) {
	if s.perms == nil {
		return nil, errors.New("patrol: permissions are not plugged in")
	}
	teams := []string{}
	for _, perm := range []access.Permission{access.PatrolApprove, access.GuardApprove} {
		var res access.Resolution
		var err error
		if day != nil {
			res, err = s.perms.Resolve(ctx, q, personnelID, perm, *day)
		} else {
			res, err = s.perms.ResolveToday(ctx, q, personnelID, perm)
		}
		if err != nil {
			return nil, err
		}
		if !res.Allowed || res.SelfOnly {
			continue
		}
		for _, t := range res.TeamCodes {
			if !containsString(teams, t) {
				teams = append(teams, t)
			}
		}
	}
	return teams, nil
}

// GetRoute returns one patrol with its line and its scans. The guard reads their own; a DANRU reads
// the patrols of the regu they decide for. Any other patrol is answered as if it did not exist.
func (s *Service) GetRoute(ctx context.Context, p auth.Principal, id int64) (RouteDetail, error) {
	var out RouteDetail
	err := database.WithTenant(ctx, s.pool, p.OrganizationID, func(q *db.Queries) error {
		row, err := q.GetPatrolRoute(ctx, id)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrPatrolNotFound
		}
		if err != nil {
			return fmt.Errorf("get patrol: %w", err)
		}
		if row.PersonnelID != p.PersonnelID {
			// Judged on the patrol's own work date, as a scan is: someone who moved regu this morning
			// still reads yesterday's patrols of the regu they decided for then.
			day := dateOf(row.WorkDate)
			teams, err := s.routeReach(ctx, q, p.PersonnelID, &day)
			if err != nil {
				return err
			}
			if !containsString(teams, row.TeamCode) {
				return ErrPatrolNotFound
			}
		}
		out, err = s.detail(ctx, q, row)
		return err
	})
	return out, err
}

// RouteFilter narrows the patrol list.
type RouteFilter struct {
	ProjectCode string
	TeamCode    string
	PersonnelID int64
	WorkDate    *timeutil.Date
	Status      string
	Limit       int
	Offset      int
}

// ListRoutes returns one page of patrols, newest first, without their paths, and the total. A guard
// sees their own; a DANRU also those of the regu their approve permission reaches.
func (s *Service) ListRoutes(ctx context.Context, p auth.Principal, f RouteFilter) ([]Route, int, error) {
	var out []Route
	var total int64
	err := database.WithTenant(ctx, s.pool, p.OrganizationID, func(q *db.Queries) error {
		status := db.NullPatrolRouteStatus{PatrolRouteStatus: db.PatrolRouteStatus(f.Status), Valid: f.Status != ""}
		// ⚠ Each patrol is judged on its own work date, as GetRoute does: a DANRU is never shown in the
		// list a patrol whose detail would be a 404 (or the other way round).
		keys, err := q.ListRouteKeys(ctx, db.ListRouteKeysParams{
			ViewerID: p.PersonnelID, ProjectCode: text(f.ProjectCode), TeamCode: text(f.TeamCode),
			PersonnelID: int8Param(positive(f.PersonnelID)), WorkDate: dateParam(f.WorkDate), Status: status,
		})
		if err != nil {
			return fmt.Errorf("list patrol keys: %w", err)
		}
		reach := map[timeutil.Date][]string{}
		days, teams := []pgtype.Date{}, []string{}
		for _, k := range keys {
			day := dateOf(k.WorkDate)
			list, cached := reach[day]
			if !cached {
				if list, err = s.routeReach(ctx, q, p.PersonnelID, &day); err != nil {
					return err
				}
				reach[day] = list
			}
			if containsString(list, k.TeamCode) {
				days, teams = append(days, k.WorkDate), append(teams, k.TeamCode)
			}
		}
		rows, err := q.ListPatrolRoutes(ctx, db.ListPatrolRoutesParams{
			ViewerID: p.PersonnelID, Days: days, Teams: teams, ProjectCode: text(f.ProjectCode), TeamCode: text(f.TeamCode),
			PersonnelID: int8Param(positive(f.PersonnelID)), WorkDate: dateParam(f.WorkDate), Status: status,
			PageLimit: int32(f.Limit), PageOffset: int32(f.Offset), //nolint:gosec // bounded by ParseListQuery
		})
		if err != nil {
			return fmt.Errorf("list patrols: %w", err)
		}
		total, err = q.CountPatrolRoutes(ctx, db.CountPatrolRoutesParams{
			ViewerID: p.PersonnelID, Days: days, Teams: teams, ProjectCode: text(f.ProjectCode), TeamCode: text(f.TeamCode),
			PersonnelID: int8Param(positive(f.PersonnelID)), WorkDate: dateParam(f.WorkDate), Status: status,
		})
		if err != nil {
			return fmt.Errorf("count patrols: %w", err)
		}
		out = make([]Route, 0, len(rows))
		for _, r := range rows {
			route := routeOf(db.PatrolRoute{
				ID: r.ID, LocalID: r.LocalID, PersonnelID: r.PersonnelID, TeamCode: r.TeamCode, ProjectCode: r.ProjectCode,
				Status: r.Status, StartedAt: r.StartedAt, EndedAt: r.EndedAt, WorkDate: r.WorkDate,
				DistanceMeters: r.DistanceMeters, PointCount: r.PointCount, ScanCount: r.ScanCount,
				SubmittedOffline: r.SubmittedOffline, LastPointAt: r.LastPointAt,
			})
			route.PersonnelName = r.FullName
			out = append(out, route)
		}
		return nil
	})
	return out, int(total), err
}
