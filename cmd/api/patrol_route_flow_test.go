package main

import (
	"encoding/json"
	"math"
	"net/http"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"siapp/pkg/patrol"
	"siapp/pkg/platform/clock"
	"siapp/pkg/platform/testkit"
)

// metersPerDegree is an arc of one degree along the equator, which is what the flow tests use to
// place points east and north of the zone's centre.
const metersPerDegree = 111_319.49

// at places a point east and north of the centre of the zones, in meters.
func at(east, north float64) (lat, lng float64) {
	return zoneLat + north/metersPerDegree, zoneLng + east/(metersPerDegree*math.Cos(zoneLat*math.Pi/180))
}

type routeOut struct {
	ID             int64   `json:"id"`
	LocalID        string  `json:"local_id"`
	PersonnelID    int64   `json:"personnel_id"`
	PersonnelName  string  `json:"personnel_name"`
	TeamCode       string  `json:"team_code"`
	ProjectCode    string  `json:"project_code"`
	Status         string  `json:"status"`
	StartedAt      string  `json:"started_at"`
	EndedAt        *string `json:"ended_at"`
	WorkDate       string  `json:"work_date"`
	DistanceMeters int     `json:"distance_meters"`
	PointCount     int     `json:"point_count"`
	ScanCount      int     `json:"scan_count"`
	LastPointAt    string  `json:"last_point_at"`
	Route          struct {
		Type        string      `json:"type"`
		Coordinates [][]float64 `json:"coordinates"`
	} `json:"route"`
	Path []struct {
		Latitude       float64 `json:"latitude"`
		Longitude      float64 `json:"longitude"`
		At             string  `json:"at"`
		AccuracyMeters float64 `json:"accuracy_meters"`
	} `json:"path"`
	Scans []struct {
		ID       int64  `json:"id"`
		ZoneID   int64  `json:"zone_id"`
		ZoneName string `json:"zone_name"`
		Status   string `json:"status"`
		Subject  string `json:"subject"`
	} `json:"scans"`
}

// rp is a point of a patrol, east and north of the centre, logged at the given time.
func rp(seq int, when time.Time, east, north float64) map[string]any {
	lat, lng := at(east, north)
	return map[string]any{
		"seq": seq, "logged_at": when.UTC().Format(time.RFC3339Nano), "latitude": lat, "longitude": lng, "accuracy_meters": 5,
	}
}

func (f *flow) patrolDo(method, path, token string, body any, key string) (int, []byte) {
	f.t.Helper()
	var raw []byte
	if body != nil {
		raw = jsonBody(f.t, body)
	}
	headers := map[string]string{}
	if key != "" {
		headers["Idempotency-Key"] = key
	}
	return f.do(method, path, token, raw, headers)
}

func routeOf(t *testing.T, code int, raw []byte, want int) routeOut {
	t.Helper()
	require.Equal(t, want, code, string(raw))
	var env struct {
		Data routeOut `json:"data"`
	}
	require.NoError(t, json.Unmarshal(raw, &env), string(raw))
	return env.Data
}

// activeOf reads GET /patrols/active: an array of zero or one patrol.
func activeOf(t *testing.T, code int, raw []byte, want int) []routeOut {
	t.Helper()
	require.Equal(t, http.StatusOK, code, string(raw))
	var env struct {
		Data []routeOut `json:"data"`
	}
	require.NoError(t, json.Unmarshal(raw, &env), string(raw))
	require.NotNil(t, env.Data, "data must be [], not null: "+string(raw))
	require.Len(t, env.Data, want)
	return env.Data
}

// startBody is a valid start made at the given time, at the centre of the zones.
func startBody(when time.Time) map[string]any {
	lat, lng := at(0, 0)
	return map[string]any{
		"local_id": uuid.NewString(), "started_at": when.UTC().Format(time.RFC3339Nano), "project_code": "1K-IBMI-S",
		"latitude": lat, "longitude": lng, "accuracy_meters": 5,
	}
}

func (f *flow) start(token string, body map[string]any) (int, []byte) {
	f.t.Helper()
	return f.patrolDo(http.MethodPost, "/api/v1/patrols/start", token, body, body["local_id"].(string))
}

func (f *flow) mustStart(token string, when time.Time) routeOut {
	f.t.Helper()
	code, raw := f.start(token, startBody(when))
	return routeOf(f.t, code, raw, http.StatusCreated)
}

func patrolURL(id int64, tail string) string {
	return "/api/v1/patrols/" + strconv.FormatInt(id, 10) + tail
}

func (f *flow) points(token string, id int64, pts ...map[string]any) (int, []byte) {
	f.t.Helper()
	return f.patrolDo(http.MethodPost, patrolURL(id, "/points"), token, map[string]any{"points": pts}, uuid.NewString())
}

type pointsOut struct {
	Accepted   int `json:"accepted"`
	Duplicates int `json:"duplicates"`
}

func (f *flow) mustPoints(token string, id int64, pts ...map[string]any) pointsOut {
	f.t.Helper()
	code, raw := f.points(token, id, pts...)
	require.Equal(f.t, http.StatusOK, code, string(raw))
	var env struct {
		Data pointsOut `json:"data"`
	}
	require.NoError(f.t, json.Unmarshal(raw, &env), string(raw))
	return env.Data
}

func (f *flow) end(token string, id int64, when time.Time, pts ...map[string]any) (int, []byte) {
	f.t.Helper()
	body := map[string]any{"ended_at": when.UTC().Format(time.RFC3339Nano)}
	if len(pts) > 0 {
		body["points"] = pts
	}
	return f.patrolDo(http.MethodPost, patrolURL(id, "/end"), token, body, uuid.NewString())
}

func (f *flow) getPatrol(token string, id int64) (int, []byte) {
	f.t.Helper()
	return f.do(http.MethodGet, patrolURL(id, ""), token, nil, nil)
}

// walkSquare is a whole patrol: start at the centre, then a square of 50 m, back to the start. The
// points are sent OUT OF ORDER (3 and 2, then 1) and the last one rides along with the end, which is
// how an app that was offline for a while catches up.
func (f *flow) walkSquare(token string, base time.Time) routeOut {
	f.t.Helper()
	started := f.mustStart(token, base)
	f.mustPoints(token, started.ID, rp(3, base.Add(3*time.Minute), 50, 0), rp(2, base.Add(2*time.Minute), 50, 50))
	f.mustPoints(token, started.ID, rp(1, base.Add(time.Minute), 0, 50))
	code, raw := f.end(token, started.ID, base.Add(5*time.Minute), rp(4, base.Add(4*time.Minute), 0, 0))
	return routeOf(f.t, code, raw, http.StatusOK)
}

// requireSquare checks the line of walkSquare: five positions in the order walked, longitude first.
func requireSquare(t *testing.T, r routeOut) {
	t.Helper()
	want := [][2]float64{{0, 0}, {0, 50}, {50, 50}, {50, 0}, {0, 0}}
	require.Equal(t, "LineString", r.Route.Type)
	require.Len(t, r.Route.Coordinates, len(want), "the four corners and the way back")
	require.Len(t, r.Path, len(want))
	for i, w := range want {
		lat, lng := at(w[0], w[1])
		require.InDelta(t, lng, r.Route.Coordinates[i][0], 1e-6, "position %d: GeoJSON puts the longitude first", i)
		require.InDelta(t, lat, r.Route.Coordinates[i][1], 1e-6, "position %d: then the latitude", i)
		require.InDelta(t, lat, r.Path[i].Latitude, 1e-6)
		require.InDelta(t, lng, r.Path[i].Longitude, 1e-6)
	}
	require.InDelta(t, 200, r.DistanceMeters, 3, "a square of 50 m")
}

func TestPatrolRouteLifecycle(t *testing.T) {
	f := newFlow(t)
	base := time.Now().Add(-30 * time.Minute)

	t.Run("start, points in any order, end: a line in the order walked, longitude first", func(t *testing.T) {
		r := f.walkSquare(f.personTok, base)
		require.Equal(t, "ENDED", r.Status)
		require.NotNil(t, r.EndedAt)
		require.Equal(t, f.person, r.PersonnelID)
		require.Equal(t, "1K-IBMI-S1", r.TeamCode, "the regu comes from the membership, not from the app")
		require.Equal(t, "1K-IBMI-S", r.ProjectCode)
		require.Equal(t, 5, r.PointCount, "the start, the three before the end, and the one that rode along")
		require.Zero(t, r.ScanCount)
		requireSquare(t, r)
		require.Equal(t, "Anggota Satu A", r.PersonnelName)
	})

	t.Run("a retry of the same end returns the stored patrol, any other end of it is refused", func(t *testing.T) {
		var id int64
		var endedAt time.Time
		f.query([]any{&id, &endedAt}, "SELECT id, ended_at FROM patrol_routes ORDER BY id DESC LIMIT 1")
		code, raw := f.end(f.personTok, id, endedAt)
		r := routeOf(t, code, raw, http.StatusOK)
		require.Equal(t, "ENDED", r.Status)
		requireSquare(t, r)
		// The app may send nanoseconds the database rounded away: still the same end.
		code, raw = f.end(f.personTok, id, endedAt.Add(700*time.Nanosecond))
		require.Equal(t, "ENDED", routeOf(t, code, raw, http.StatusOK).Status)

		code, raw = f.end(f.personTok, id, endedAt.Add(time.Minute))
		reasonIs(t, code, raw, http.StatusConflict, "PATROL_NOT_ACTIVE")
		code, raw = f.getPatrol(f.personTok, id)
		requireSquare(t, routeOf(t, code, raw, http.StatusOK))
	})

	t.Run("an ended patrol takes no more points", func(t *testing.T) {
		var id int64
		f.query([]any{&id}, "SELECT id FROM patrol_routes ORDER BY id DESC LIMIT 1")
		code, raw := f.points(f.personTok, id, rp(9, time.Now(), 1, 1))
		reasonIs(t, code, raw, http.StatusConflict, "PATROL_NOT_ACTIVE")
	})

	t.Run("an offline patrol, sent days later, builds the same line", func(t *testing.T) {
		body := startBody(time.Now().Add(-72 * time.Hour))
		body["submitted_offline"] = true
		code, raw := f.start(f.personTok, body)
		started := routeOf(t, code, raw, http.StatusCreated)
		old := time.Now().Add(-72 * time.Hour)
		f.mustPoints(f.personTok, started.ID, rp(3, old.Add(3*time.Minute), 50, 0), rp(2, old.Add(2*time.Minute), 50, 50))
		f.mustPoints(f.personTok, started.ID, rp(1, old.Add(time.Minute), 0, 50))
		code, raw = f.end(f.personTok, started.ID, old.Add(5*time.Minute), rp(4, old.Add(4*time.Minute), 0, 0))
		r := routeOf(t, code, raw, http.StatusOK)
		requireSquare(t, r)
		loc, _ := time.LoadLocation("Asia/Jakarta")
		require.Equal(t, old.In(loc).Format(time.DateOnly), r.WorkDate, "the work date is the day the patrol was walked")
	})
}

func TestPatrolRouteStart(t *testing.T) {
	f := newFlow(t)
	now := time.Now()

	body := startBody(now)
	code, raw := f.start(f.personTok, body)
	first := routeOf(t, code, raw, http.StatusCreated)
	require.Equal(t, "ACTIVE", first.Status)
	require.Equal(t, 1, first.PointCount, "the start is the first point")
	require.Equal(t, 1, countOf(t, f, "SELECT count(*) FROM patrol_route_points WHERE patrol_id = $1 AND seq = 0", first.ID))

	t.Run("the same local_id is idempotent: the middleware replays the first answer", func(t *testing.T) {
		code, raw := f.start(f.personTok, body)
		again := routeOf(t, code, raw, http.StatusCreated)
		require.Equal(t, first.ID, again.ID)
		require.Equal(t, 1, countOf(t, f, "SELECT count(*) FROM patrol_routes"))
	})

	t.Run("and so is the service, when the key is gone: the stored patrol, 200", func(t *testing.T) {
		f.exec("DELETE FROM idempotency_keys")
		code, raw := f.start(f.personTok, body)
		again := routeOf(t, code, raw, http.StatusOK)
		require.Equal(t, first.ID, again.ID)
		require.Equal(t, 1, countOf(t, f, "SELECT count(*) FROM patrol_routes"))
		require.Equal(t, 1, countOf(t, f, "SELECT count(*) FROM patrol_route_points WHERE patrol_id = $1", first.ID), "no second start point")
	})

	t.Run("a second patrol while one runs is refused", func(t *testing.T) {
		code, raw := f.start(f.personTok, startBody(now))
		reasonIs(t, code, raw, http.StatusConflict, "PATROL_ALREADY_ACTIVE")
		require.Equal(t, 1, countOf(t, f, "SELECT count(*) FROM patrol_routes"))
	})

	t.Run("another person cannot take the local_id", func(t *testing.T) {
		_, otherTok := f.addMember("1K-IBMI-S1", "MEMBER", 900601, "REGURUTE01", "Anggota Rute")
		code, raw := f.start(otherTok, body)
		reasonIs(t, code, raw, http.StatusConflict, "PATROL_LOCAL_ID_TAKEN")
	})

	t.Run("the active patrol can be fetched, until it ends", func(t *testing.T) {
		code, raw := f.do(http.MethodGet, "/api/v1/patrols/active", f.personTok, nil, nil)
		active := activeOf(t, code, raw, 1)[0]
		require.Equal(t, first.ID, active.ID)
		require.Equal(t, "LineString", active.Route.Type)
		require.Len(t, active.Route.Coordinates, 2, "one point is written twice: a LineString needs two")

		code, raw = f.end(f.personTok, first.ID, now.Add(time.Minute))
		routeOf(t, code, raw, http.StatusOK)
		code, raw = f.do(http.MethodGet, "/api/v1/patrols/active", f.personTok, nil, nil)
		require.Empty(t, activeOf(t, code, raw, 0), "no patrol running is an empty array, not a 404")
	})

	t.Run("the request is checked", func(t *testing.T) {
		b := startBody(time.Now().Add(-48 * time.Hour))
		b["local_id"] = uuid.NewString()
		code, raw := f.patrolDo(http.MethodPost, "/api/v1/patrols/start", f.personTok, b, uuid.NewString())
		require.Equal(t, http.StatusUnprocessableEntity, code, "local_id must be the Idempotency-Key: "+string(raw))

		b = startBody(time.Now().Add(-8 * 24 * time.Hour))
		code, raw = f.start(f.personTok, b)
		reasonIs(t, code, raw, http.StatusUnprocessableEntity, "OCCURRED_AT_TOO_OLD")

		b = startBody(time.Now().Add(time.Hour))
		code, raw = f.start(f.personTok, b)
		reasonIs(t, code, raw, http.StatusUnprocessableEntity, "OCCURRED_AT_IN_FUTURE")

		b = startBody(now)
		delete(b, "latitude")
		code, raw = f.start(f.personTok, b)
		require.Equal(t, http.StatusUnprocessableEntity, code, string(raw))

		b = startBody(now)
		b["project_code"] = "NO-SUCH-PROJECT"
		code, raw = f.start(f.personTok, b)
		reasonIs(t, code, raw, http.StatusForbidden, "ZONE_NOT_IN_PROJECT")
	})

	t.Run("someone who serves in no regu cannot start a patrol", func(t *testing.T) {
		id := f.addPerson(900602, "TANPAREGU1", "Tanpa Regu")
		code, raw := f.start(f.token(id, f.addSession(id, "tok-noteam")), startBody(now))
		require.Equal(t, http.StatusForbidden, code, string(raw))
	})
}

func TestPatrolRouteStartRace(t *testing.T) {
	f := newFlow(t)
	var wg sync.WaitGroup
	codes := make([]int, 5)
	for i := range codes {
		wg.Add(1)
		go func() {
			defer wg.Done()
			codes[i], _ = f.start(f.personTok, startBody(time.Now()))
		}()
	}
	wg.Wait()
	created := 0
	for _, c := range codes {
		switch c {
		case http.StatusCreated:
			created++
		case http.StatusConflict:
		default:
			t.Fatalf("unexpected status %d in %v", c, codes)
		}
	}
	require.Equal(t, 1, created, "one active patrol per person, whoever is first: %v", codes)
	require.Equal(t, 1, countOf(t, f, "SELECT count(*) FROM patrol_routes WHERE status = 'ACTIVE'"))
}

func TestPatrolRoutePoints(t *testing.T) {
	f := newFlow(t)
	now := time.Now()
	r := f.mustStart(f.personTok, now.Add(-time.Hour))

	t.Run("a retried batch and an out-of-order one do not duplicate or reorder points", func(t *testing.T) {
		got := f.mustPoints(f.personTok, r.ID, rp(2, now.Add(-50*time.Minute), 0, 20), rp(1, now.Add(-55*time.Minute), 0, 10))
		require.Equal(t, pointsOut{Accepted: 2}, got)
		got = f.mustPoints(f.personTok, r.ID, rp(1, now.Add(-55*time.Minute), 0, 10), rp(2, now.Add(-50*time.Minute), 0, 20), rp(3, now.Add(-45*time.Minute), 0, 30))
		require.Equal(t, pointsOut{Accepted: 1, Duplicates: 2}, got, "only seq 3 is new")
		require.Equal(t, 4, countOf(t, f, "SELECT count(*) FROM patrol_route_points WHERE patrol_id = $1", r.ID))
		require.Equal(t, 4, countOf(t, f, "SELECT point_count FROM patrol_routes WHERE id = $1", r.ID))

		var lat float64
		f.query([]any{&lat}, "SELECT latitude FROM patrol_route_points WHERE patrol_id = $1 AND seq = 2", r.ID)
		want, _ := at(0, 20)
		require.InDelta(t, want, lat, 1e-6, "a point is never replaced by a retry")

		var moved bool
		f.query([]any{&moved}, "SELECT last_point_at >= $1 FROM patrol_routes WHERE id = $2", now.Add(-46*time.Minute), r.ID)
		require.True(t, moved, "last_point_at follows the newest point")
	})

	t.Run("a late batch of old points does not move last_point_at back", func(t *testing.T) {
		var before time.Time
		f.query([]any{&before}, "SELECT last_point_at FROM patrol_routes WHERE id = $1", r.ID)
		f.mustPoints(f.personTok, r.ID, rp(10, now.Add(-58*time.Minute), 0, 5))
		var after time.Time
		f.query([]any{&after}, "SELECT last_point_at FROM patrol_routes WHERE id = $1", r.ID)
		require.True(t, after.Equal(before))
	})

	t.Run("200 points are taken, 201 are too large", func(t *testing.T) {
		pts := make([]map[string]any, 0, 201)
		for i := 0; i < 201; i++ {
			pts = append(pts, rp(100+i, now.Add(-30*time.Minute+time.Duration(i)*time.Second), 0, float64(i)))
		}
		code, raw := f.points(f.personTok, r.ID, pts...)
		reasonIs(t, code, raw, http.StatusUnprocessableEntity, "PATROL_POINTS_TOO_LARGE")
		require.Equal(t, 5, countOf(t, f, "SELECT count(*) FROM patrol_route_points WHERE patrol_id = $1", r.ID), "nothing of a refused batch is stored")

		got := f.mustPoints(f.personTok, r.ID, pts[:200]...)
		require.Equal(t, 200, got.Accepted)
	})

	t.Run("a point with an absurd accuracy is stored capped, and does not fail its batch", func(t *testing.T) {
		// numeric(8,2) holds less than a million: an uncapped value was a 500 on every retry.
		wild := rp(600, now, 0, 0)
		wild["accuracy_meters"] = 5_000_000
		got := f.mustPoints(f.personTok, r.ID, wild, rp(601, now, 0, 1))
		require.Equal(t, 2, got.Accepted)
		var acc float64
		f.query([]any{&acc}, "SELECT accuracy_meters::float8 FROM patrol_route_points WHERE patrol_id = $1 AND seq = 600", r.ID)
		require.InDelta(t, 100000, acc, 0.001)
	})

	t.Run("the batch is checked", func(t *testing.T) {
		code, raw := f.points(f.personTok, r.ID)
		require.Equal(t, http.StatusUnprocessableEntity, code, "no points: "+string(raw))

		code, raw = f.points(f.personTok, r.ID, rp(500, time.Now().Add(time.Hour), 0, 0))
		require.Equal(t, http.StatusUnprocessableEntity, code, "a point from the future: "+string(raw))

		bad := rp(501, now, 0, 0)
		bad["latitude"] = 95
		code, raw = f.points(f.personTok, r.ID, bad)
		require.Equal(t, http.StatusUnprocessableEntity, code, "a latitude off the globe: "+string(raw))

		noSeq := rp(0, now, 0, 0)
		delete(noSeq, "seq")
		code, raw = f.points(f.personTok, r.ID, noSeq)
		require.Equal(t, http.StatusUnprocessableEntity, code, "no seq: "+string(raw))
	})

	t.Run("another person's patrol, and one that is not there, are not found", func(t *testing.T) {
		_, otherTok := f.addMember("1K-IBMI-S1", "MEMBER", 900611, "REGURUTE02", "Anggota Rute Dua")
		code, raw := f.points(otherTok, r.ID, rp(700, now, 0, 0))
		reasonIs(t, code, raw, http.StatusNotFound, "PATROL_NOT_FOUND")
		code, raw = f.end(otherTok, r.ID, now)
		reasonIs(t, code, raw, http.StatusNotFound, "PATROL_NOT_FOUND")

		code, raw = f.points(f.personTok, 999999, rp(700, now, 0, 0))
		reasonIs(t, code, raw, http.StatusNotFound, "PATROL_NOT_FOUND")
		code, raw = f.do(http.MethodPost, "/api/v1/patrols/abc/points", f.personTok, jsonBody(t, map[string]any{"points": []any{rp(1, now, 0, 0)}}),
			map[string]string{"Idempotency-Key": uuid.NewString()})
		reasonIs(t, code, raw, http.StatusNotFound, "PATROL_NOT_FOUND")
	})

	t.Run("ending before the start is refused, and the patrol keeps running", func(t *testing.T) {
		code, raw := f.end(f.personTok, r.ID, now.Add(-2*time.Hour))
		require.Equal(t, http.StatusUnprocessableEntity, code, string(raw))
		code, raw = f.end(f.personTok, r.ID, now.Add(time.Hour))
		reasonIs(t, code, raw, http.StatusUnprocessableEntity, "OCCURRED_AT_IN_FUTURE")
		require.Equal(t, "ACTIVE", func() string {
			var s string
			f.query([]any{&s}, "SELECT status FROM patrol_routes WHERE id = $1", r.ID)
			return s
		}())
	})
}

func TestPatrolRouteScans(t *testing.T) {
	f := newFlow(t)
	z := f.fencedZone(nil, map[string]any{"name": "Pos Rute"})
	now := time.Now()

	patrolIDOf := func(scanID int64) *int64 {
		var id *int64
		f.query([]any{&id}, "SELECT patrol_id FROM patrol_scans WHERE id = $1", scanID)
		return id
	}

	t.Run("a scan outside any patrol still works", func(t *testing.T) {
		s := f.mustScan(f.personTok, z, scanBody(z))
		require.Nil(t, patrolIDOf(s.ID))
	})

	started := f.mustStart(f.personTok, now.Add(-20*time.Minute))
	var during scanOut
	t.Run("a scan during a patrol gets its patrol_id", func(t *testing.T) {
		b := scanBody(z)
		b["occurred_at"] = now.Add(-10 * time.Minute).UTC().Format(time.RFC3339Nano)
		during = f.mustScan(f.personTok, z, b)
		got := patrolIDOf(during.ID)
		require.NotNil(t, got)
		require.Equal(t, started.ID, *got)
		require.Equal(t, 1, countOf(t, f, "SELECT scan_count FROM patrol_routes WHERE id = $1", started.ID))
	})

	t.Run("the patrol shows the scan as a pin, and counts it when it ends", func(t *testing.T) {
		code, raw := f.getPatrol(f.personTok, started.ID)
		r := routeOf(t, code, raw, http.StatusOK)
		require.Len(t, r.Scans, 1)
		require.Equal(t, during.ID, r.Scans[0].ID)
		require.Equal(t, z.ID, r.Scans[0].ZoneID)
		require.Equal(t, "Pos Rute", r.Scans[0].ZoneName)
		require.Equal(t, "SUBMITTED", r.Scans[0].Status)

		code, raw = f.end(f.personTok, started.ID, now.Add(-5*time.Minute))
		r = routeOf(t, code, raw, http.StatusOK)
		require.Equal(t, 1, r.ScanCount)
	})

	t.Run("an offline scan made during the patrol joins it even after it ended", func(t *testing.T) {
		b := scanBody(z)
		b["occurred_at"] = now.Add(-15 * time.Minute).UTC().Format(time.RFC3339Nano)
		b["submitted_offline"] = true
		late := f.mustScan(f.personTok, z, b)
		got := patrolIDOf(late.ID)
		require.NotNil(t, got)
		require.Equal(t, started.ID, *got)
		require.Equal(t, 2, countOf(t, f, "SELECT scan_count FROM patrol_routes WHERE id = $1", started.ID), "counted although the patrol had ended")
	})

	t.Run("a scan made after the end does not join", func(t *testing.T) {
		s := f.mustScan(f.personTok, z, scanBody(z))
		require.Nil(t, patrolIDOf(s.ID), "the patrol ended five minutes ago")
	})
}

func TestPatrolRouteVisibility(t *testing.T) {
	f := newFlow(t)
	danruID, danruTok := f.danru()
	_, s2DanruTok := f.addMember("1K-IBMI-S2", "DANRU", 900621, "DANRUDUA02", "Danru Dua Rute")
	otherID, otherTok := f.addMember("1K-IBMI-S1", "MEMBER", 900622, "REGURUTE03", "Anggota Lain")

	mine := f.walkSquare(f.personTok, time.Now().Add(-40*time.Minute))
	theirs := f.walkSquare(otherTok, time.Now().Add(-40*time.Minute))
	require.NotEqual(t, mine.ID, theirs.ID)

	read := func(token string, id int64) int {
		code, _ := f.getPatrol(token, id)
		return code
	}
	listIDs := func(token, query string) []int64 {
		code, raw := f.do(http.MethodGet, "/api/v1/patrols"+query, token, nil, nil)
		require.Equal(t, http.StatusOK, code, string(raw))
		var env struct {
			Data []routeOut `json:"data"`
		}
		require.NoError(t, json.Unmarshal(raw, &env), string(raw))
		ids := make([]int64, 0, len(env.Data))
		for _, r := range env.Data {
			require.Empty(t, r.Route.Coordinates, "the list leaves the line out")
			require.NotEmpty(t, r.PersonnelName)
			ids = append(ids, r.ID)
		}
		return ids
	}

	t.Run("a member reads only their own patrol", func(t *testing.T) {
		require.Equal(t, http.StatusOK, read(f.personTok, mine.ID))
		require.Equal(t, http.StatusNotFound, read(f.personTok, theirs.ID), "a colleague's patrol is not there for them")
		require.Equal(t, []int64{mine.ID}, listIDs(f.personTok, ""))
		require.Equal(t, []int64{theirs.ID}, listIDs(otherTok, ""))
	})

	t.Run("the DANRU of the regu reads every patrol of it", func(t *testing.T) {
		require.Equal(t, http.StatusOK, read(danruTok, mine.ID))
		require.Equal(t, http.StatusOK, read(danruTok, theirs.ID))
		require.ElementsMatch(t, []int64{mine.ID, theirs.ID}, listIDs(danruTok, ""))
		require.Equal(t, []int64{theirs.ID}, listIDs(danruTok, "?personnel_id="+strconv.FormatInt(otherID, 10)))
		require.ElementsMatch(t, []int64{mine.ID, theirs.ID}, listIDs(danruTok, "?team_code=1K-IBMI-S1&status=ENDED"))
		require.Empty(t, listIDs(danruTok, "?status=ACTIVE"))
	})

	t.Run("the DANRU of another regu gets 404 and an empty list", func(t *testing.T) {
		code, raw := f.getPatrol(s2DanruTok, mine.ID)
		reasonIs(t, code, raw, http.StatusNotFound, "PATROL_NOT_FOUND")
		require.Empty(t, listIDs(s2DanruTok, ""))
		require.Empty(t, listIDs(s2DanruTok, "?team_code=1K-IBMI-S1"), "a regu outside the reach narrows to nothing")
	})

	t.Run("filters by work date, and bad filters are refused", func(t *testing.T) {
		loc, _ := time.LoadLocation("Asia/Jakarta")
		today := time.Now().In(loc).Format(time.DateOnly)
		require.Equal(t, []int64{mine.ID}, listIDs(f.personTok, "?work_date="+today))
		require.Empty(t, listIDs(f.personTok, "?work_date=2020-01-01"))
		for _, q := range []string{"?status=NOPE", "?work_date=yesterday", "?personnel_id=abc"} {
			code, raw := f.do(http.MethodGet, "/api/v1/patrols"+q, f.personTok, nil, nil)
			require.Equal(t, http.StatusUnprocessableEntity, code, q+": "+string(raw))
		}
	})

	t.Run("the list and the detail judge a patrol on the same date", func(t *testing.T) {
		// The DANRU took the regu over today: a patrol of last week is neither listed nor readable,
		// and one of today is both. The list used to show the old one while its detail was a 404.
		old := f.walkSquare(f.personTok, time.Now().Add(-50*time.Minute))
		f.exec("UPDATE patrol_routes SET work_date = current_date - 7 WHERE id = $1", old.ID)
		f.exec("UPDATE team_members SET start_date = current_date WHERE personnel_id = $1 AND role = 'DANRU'", danruID)
		defer f.exec("UPDATE team_members SET start_date = current_date - 30 WHERE personnel_id = $1 AND role = 'DANRU'", danruID)

		require.Equal(t, http.StatusNotFound, read(danruTok, old.ID))
		require.NotContains(t, listIDs(danruTok, ""), old.ID)
		require.Equal(t, http.StatusOK, read(danruTok, theirs.ID))
		require.Contains(t, listIDs(danruTok, ""), theirs.ID)
		require.Contains(t, listIDs(f.personTok, ""), old.ID, "the guard still sees their own")
	})

	t.Run("a patrol that does not exist, and a bad id, are not found", func(t *testing.T) {
		require.Equal(t, http.StatusNotFound, read(f.personTok, 999999))
		code, _ := f.do(http.MethodGet, "/api/v1/patrols/abc", f.personTok, nil, nil)
		require.Equal(t, http.StatusNotFound, code)
	})

	t.Run("an active patrol is watched as it grows", func(t *testing.T) {
		started := f.mustStart(f.personTok, time.Now().Add(-10*time.Minute))
		code, raw := f.getPatrol(danruTok, started.ID)
		before := routeOf(t, code, raw, http.StatusOK)
		require.Equal(t, "ACTIVE", before.Status)
		f.mustPoints(f.personTok, started.ID, rp(1, time.Now().Add(-8*time.Minute), 0, 40), rp(2, time.Now().Add(-6*time.Minute), 40, 40))
		code, raw = f.getPatrol(danruTok, started.ID)
		after := routeOf(t, code, raw, http.StatusOK)
		require.Greater(t, len(after.Route.Coordinates), len(before.Route.Coordinates), "the line grew")
		require.Equal(t, 3, after.PointCount)
	})
}

func TestPatrolAutoEndAndRetention(t *testing.T) {
	f := newFlow(t)
	svc := patrol.NewService(testkit.Pool(t), clock.System{})

	stale := f.mustStart(f.personTok, time.Now().Add(-time.Hour))
	f.mustPoints(f.personTok, stale.ID, rp(1, time.Now().Add(-50*time.Minute), 0, 40), rp(2, time.Now().Add(-40*time.Minute), 40, 40))
	_, otherTok := f.addMember("1K-IBMI-S1", "MEMBER", 900631, "REGURUTE04", "Anggota Rute Empat")
	fresh := f.mustStart(otherTok, time.Now().Add(-time.Hour))

	// The phone died seven hours ago: shift the stale patrol's clock back.
	f.exec(`UPDATE patrol_routes SET started_at = now() - interval '8 hours', last_point_at = now() - interval '7 hours' WHERE id = $1`, stale.ID)

	t.Run("a patrol with no point for six hours is ended for the guard, and a fresh one is left alone", func(t *testing.T) {
		code, raw := f.start(f.personTok, startBody(time.Now()))
		reasonIs(t, code, raw, http.StatusConflict, "PATROL_ALREADY_ACTIVE")

		ended, err := svc.AutoEndStale(t.Context())
		require.NoError(t, err)
		require.Equal(t, 1, ended)

		code, raw = f.getPatrol(f.personTok, stale.ID)
		r := routeOf(t, code, raw, http.StatusOK)
		require.Equal(t, "AUTO_ENDED", r.Status)
		require.NotEmpty(t, r.Route.Coordinates, "the path was built from the points it had")
		var atLastPoint bool
		f.query([]any{&atLastPoint}, "SELECT ended_at = last_point_at FROM patrol_routes WHERE id = $1", stale.ID)
		require.True(t, atLastPoint, "ended where the guard was last seen, not when the job noticed")

		code, raw = f.getPatrol(otherTok, fresh.ID)
		require.Equal(t, "ACTIVE", routeOf(t, code, raw, http.StatusOK).Status)
	})

	t.Run("the guard can start a new patrol", func(_ *testing.T) {
		f.mustStart(f.personTok, time.Now())
	})

	t.Run("running it again ends nothing", func(t *testing.T) {
		ended, err := svc.AutoEndStale(t.Context())
		require.NoError(t, err)
		require.Zero(t, ended)
	})

	t.Run("ending an auto-ended patrol is refused", func(t *testing.T) {
		code, raw := f.end(f.personTok, stale.ID, time.Now())
		reasonIs(t, code, raw, http.StatusConflict, "PATROL_NOT_ACTIVE")
	})

	t.Run("retention removes the raw points of a patrol that ended a week ago, and keeps its path", func(t *testing.T) {
		// A third guard: the two before are on a patrol that still runs.
		_, walkerTok := f.addMember("1K-IBMI-S1", "MEMBER", 900632, "REGURUTE05", "Anggota Rute Lima")
		old := f.walkSquare(walkerTok, time.Now().Add(-40*time.Minute))
		recent := f.walkSquare(walkerTok, time.Now().Add(-30*time.Minute))
		f.exec(`UPDATE patrol_routes SET started_at = now() - interval '9 days', ended_at = now() - interval '8 days' WHERE id = $1`, old.ID)

		removed, err := svc.PurgePoints(t.Context())
		require.NoError(t, err)
		require.EqualValues(t, 5, removed)
		require.Zero(t, countOf(t, f, "SELECT count(*) FROM patrol_route_points WHERE patrol_id = $1", old.ID))
		require.Equal(t, 5, countOf(t, f, "SELECT count(*) FROM patrol_route_points WHERE patrol_id = $1", recent.ID), "a recent patrol keeps its points")
		require.Equal(t, 1, countOf(t, f, "SELECT count(*) FROM patrol_route_points WHERE patrol_id = $1", fresh.ID), "a running patrol too")

		code, raw := f.getPatrol(walkerTok, old.ID)
		requireSquare(t, routeOf(t, code, raw, http.StatusOK))
	})
}
