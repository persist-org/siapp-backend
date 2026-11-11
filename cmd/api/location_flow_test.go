package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/stretchr/testify/require"

	"siapp/pkg/realtime"
)

// scheduleShift gives the person a shift from startsAt to endsAt (to the minute), worked in the
// project's timezone, Asia/Jakarta. A shift that crosses midnight is an overnight one that starts
// on the date of startsAt.
func (f *flow) scheduleShift(personnelID int64, startsAt, endsAt time.Time) {
	f.t.Helper()
	loc, err := time.LoadLocation("Asia/Jakarta")
	require.NoError(f.t, err)
	start, end := startsAt.In(loc), endsAt.In(loc)
	var shiftID int64
	f.query([]any{&shiftID}, "INSERT INTO shifts (name, start_minute, end_minute) VALUES ($1, $2, $3) RETURNING id",
		fmt.Sprintf("Jaga %d %d", personnelID, startsAt.Unix()), start.Hour()*60+start.Minute(), end.Hour()*60+end.Minute())
	f.exec("INSERT INTO schedules (personnel_id, work_date, shift_id) VALUES ($1, $2, $3)", personnelID, start.Format(time.DateOnly), shiftID)
}

// onShift schedules the person on a shift that is running now: from 30 minutes ago to 30 minutes
// ahead.
func (f *flow) onShift(personnelID int64) {
	f.t.Helper()
	f.scheduleShift(personnelID, time.Now().Add(-30*time.Minute), time.Now().Add(30*time.Minute))
}

// danru adds a DANRU of 1K-IBMI-S1 and returns their id and token.
func (f *flow) danru() (int64, string) {
	f.t.Helper()
	id := f.addPerson(900150, "DANRU00001", "Danru Satu")
	f.exec("INSERT INTO team_members (team_code, personnel_id, role, start_date) VALUES ('1K-IBMI-S1', $1, 'DANRU', current_date - 30)", id)
	return id, f.token(id, 5010)
}

type pointIn struct {
	At       time.Time
	Lat, Lng float64
}

func batch(points ...pointIn) []byte {
	type p struct {
		LoggedAt       string  `json:"logged_at"`
		Latitude       float64 `json:"latitude"`
		Longitude      float64 `json:"longitude"`
		AccuracyMeters float64 `json:"accuracy_meters"`
	}
	out := make([]p, 0, len(points))
	for _, pt := range points {
		out = append(out, p{pt.At.UTC().Format(time.RFC3339Nano), pt.Lat, pt.Lng, 5})
	}
	raw, _ := json.Marshal(map[string]any{"points": out})
	return raw
}

type uploadResult struct {
	Accepted, Rejected int
}

// upload posts a batch and returns the status, the result of a 200 and the error reason otherwise.
func (f *flow) upload(token string, body []byte) (code int, res uploadResult, reason string) {
	f.t.Helper()
	code, raw := f.do(http.MethodPost, "/api/v1/location-logs", token, body, nil)
	var env struct {
		Data  uploadResult `json:"data"`
		Error struct {
			Details map[string]any `json:"details"`
		} `json:"error"`
	}
	require.NoError(f.t, json.Unmarshal(raw, &env), string(raw))
	reason, _ = env.Error.Details["reason"].(string)
	return code, env.Data, reason
}

type positionOut struct {
	PersonnelID int64    `json:"personnel_id"`
	Online      bool     `json:"online"`
	Latitude    *float64 `json:"latitude"`
	TeamCode    string   `json:"team_code"`
}

func (f *flow) positions(token, path string) (int, []positionOut) {
	f.t.Helper()
	code, raw := f.do(http.MethodGet, path, token, nil, nil)
	if code != http.StatusOK {
		return code, nil
	}
	var env struct {
		Data []positionOut `json:"data"`
	}
	require.NoError(f.t, json.Unmarshal(raw, &env), string(raw))
	return code, env.Data
}

func TestLocationIngestRules(t *testing.T) {
	f := newFlow(t)
	now := time.Now()

	t.Run("off shift nothing is stored", func(t *testing.T) {
		code, _, reason := f.upload(f.personTok, batch(pointIn{now, -6.2, 106.8}))
		require.Equal(t, http.StatusForbidden, code)
		require.Equal(t, "TRACKING_DISABLED", reason)
		var n int
		f.query([]any{&n}, "SELECT count(*) FROM location_logs")
		require.Zero(t, n)
	})

	f.onShift(f.person)

	t.Run("a project with tracking off refuses the points", func(t *testing.T) {
		f.exec("UPDATE projects SET gps_tracking_enabled = false WHERE code = '1K-IBMI-S'")
		code, _, reason := f.upload(f.personTok, batch(pointIn{now, -6.2, 106.8}))
		require.Equal(t, http.StatusForbidden, code)
		require.Equal(t, "TRACKING_DISABLED", reason)
		f.exec("UPDATE projects SET gps_tracking_enabled = true WHERE code = '1K-IBMI-S'")
	})

	t.Run("an out-of-order batch keeps the newest last location, and a duplicate is ignored", func(t *testing.T) {
		newest := now.Add(-time.Minute)
		code, res, _ := f.upload(f.personTok, batch(pointIn{newest, -6.2, 106.8}))
		require.Equal(t, http.StatusOK, code)
		require.Equal(t, uploadResult{Accepted: 1}, res)

		// Older points arrive later, one of them the same as the first batch's.
		code, res, _ = f.upload(f.personTok, batch(
			pointIn{now.Add(-5 * time.Minute), -6.5, 106.5},
			pointIn{now.Add(-3 * time.Minute), -6.4, 106.4},
			pointIn{newest, -6.2, 106.8},
		))
		require.Equal(t, http.StatusOK, code)
		require.Equal(t, uploadResult{Accepted: 3}, res, "a point sent again is accepted, so the phone can drop it")

		var rows int
		f.query([]any{&rows}, "SELECT count(*) FROM location_logs WHERE personnel_id = $1", f.person)
		require.Equal(t, 3, rows, "the duplicate is one row")
		var at time.Time
		var lat float64
		f.query([]any{&at, &lat}, "SELECT logged_at, latitude FROM personnel_last_locations WHERE personnel_id = $1", f.person)
		require.WithinDuration(t, newest, at, time.Millisecond)
		require.InDelta(t, -6.2, lat, 1e-6, "the older points did not move the person back in time")
	})

	t.Run("points outside the time window are rejected, the rest accepted", func(t *testing.T) {
		code, res, _ := f.upload(f.personTok, batch(
			pointIn{now.Add(-25 * time.Hour), -6.2, 106.8},
			pointIn{now.Add(10 * time.Minute), -6.2, 106.8},
			pointIn{now.Add(-2 * time.Minute), -6.3, 106.7},
		))
		require.Equal(t, http.StatusOK, code)
		require.Equal(t, uploadResult{Accepted: 1, Rejected: 2}, res)
	})

	t.Run("a batch over 100 points is refused whole", func(t *testing.T) {
		pts := make([]pointIn, 101)
		for i := range pts {
			pts[i] = pointIn{now.Add(-time.Duration(i+10) * time.Second), -6.2, 106.8}
		}
		code, _, reason := f.upload(f.personTok, batch(pts...))
		require.Equal(t, http.StatusUnprocessableEntity, code)
		require.Equal(t, "LOCATION_BATCH_TOO_LARGE", reason)
	})

	t.Run("a bad coordinate or an empty batch is a validation error", func(t *testing.T) {
		code, _ := f.do(http.MethodPost, "/api/v1/location-logs", f.personTok, []byte(`{"points":[{"logged_at":"`+now.UTC().Format(time.RFC3339)+`","latitude":95,"longitude":106,"accuracy_meters":5}]}`), nil)
		require.Equal(t, http.StatusUnprocessableEntity, code)
		code, _ = f.do(http.MethodPost, "/api/v1/location-logs", f.personTok, []byte(`{"points":[]}`), nil)
		require.Equal(t, http.StatusUnprocessableEntity, code)
	})
}

func TestLocationMaps(t *testing.T) {
	f := newFlow(t)
	danruID, danruTok := f.danru()
	f.onShift(f.person)

	// A second member who has a last location but is not online: no key in Redis.
	idle := f.addPerson(900160, "ANGGOTA002", "Anggota Dua")
	f.exec("INSERT INTO team_members (team_code, personnel_id, role, start_date) VALUES ('1K-IBMI-S1', $1, 'MEMBER', current_date - 30)", idle)
	f.exec("INSERT INTO personnel_last_locations (personnel_id, logged_at, latitude, longitude, accuracy_meters) VALUES ($1, now(), -6.1, 106.1, 5)", idle)

	code, _, _ := f.upload(f.personTok, batch(pointIn{time.Now().Add(-time.Minute), -6.2, 106.8}))
	require.Equal(t, http.StatusOK, code)

	t.Run("a member is refused the map endpoints", func(t *testing.T) {
		for _, path := range []string{"/api/v1/teams/1K-IBMI-S1/map", "/api/v1/personnel-tracking", "/api/v1/personnels/" + strconv.FormatInt(f.person, 10) + "/position"} {
			code, _ := f.positions(f.personTok, path)
			require.Equal(t, http.StatusForbidden, code, path)
		}
	})

	t.Run("the DANRU sees the online members only", func(t *testing.T) {
		for _, path := range []string{"/api/v1/teams/1K-IBMI-S1/map", "/api/v1/personnel-tracking", "/api/v1/personnel-tracking?project_code=1K-IBMI-S&team_code=1K-IBMI-S1"} {
			code, got := f.positions(danruTok, path)
			require.Equal(t, http.StatusOK, code, path)
			ids := []int64{}
			for _, p := range got {
				ids = append(ids, p.PersonnelID)
				require.True(t, p.Online)
				require.NotNil(t, p.Latitude)
				require.Equal(t, "1K-IBMI-S1", p.TeamCode)
			}
			require.Contains(t, ids, f.person, path)
			require.NotContains(t, ids, idle, "offline: no pin, however recent the stored point", path)
			// The DANRU's own request made them online, but they never sent a point.
			require.NotContains(t, ids, danruID, path)
		}
	})

	t.Run("a team or project outside the reach is refused", func(t *testing.T) {
		code, _ := f.positions(danruTok, "/api/v1/teams/NOPE-S9/map")
		require.Equal(t, http.StatusForbidden, code)
		code, _ = f.positions(danruTok, "/api/v1/personnel-tracking?project_code=9Z-NOPE-S")
		require.Equal(t, http.StatusForbidden, code)
	})

	t.Run("one person's position: online with a pin, offline without", func(t *testing.T) {
		code, raw := f.do(http.MethodGet, "/api/v1/personnels/"+strconv.FormatInt(f.person, 10)+"/position", danruTok, nil, nil)
		require.Equal(t, http.StatusOK, code, string(raw))
		var env struct {
			Data positionOut `json:"data"`
		}
		require.NoError(t, json.Unmarshal(raw, &env))
		require.True(t, env.Data.Online)
		require.NotNil(t, env.Data.Latitude)

		code, raw = f.do(http.MethodGet, "/api/v1/personnels/"+strconv.FormatInt(idle, 10)+"/position", danruTok, nil, nil)
		require.Equal(t, http.StatusOK, code, string(raw))
		env.Data = positionOut{}
		require.NoError(t, json.Unmarshal(raw, &env))
		require.False(t, env.Data.Online)
		require.Nil(t, env.Data.Latitude, "an offline member is not shown on the map")

		outsider := f.addPerson(900170, "LUAR000001", "Bukan Anggota")
		code, _ = f.positions(danruTok, "/api/v1/personnels/"+strconv.FormatInt(outsider, 10)+"/position")
		require.Equal(t, http.StatusForbidden, code, "a person in no regu of the DANRU's reach")
	})

	t.Run("the admin monitor", func(t *testing.T) {
		code, got := f.positions(f.adminTok, "/api/v1/admin/personnel-tracking?project_code=1K-IBMI-S")
		require.Equal(t, http.StatusOK, code)
		require.Len(t, got, 1)
		require.Equal(t, f.person, got[0].PersonnelID)
		code, _ = f.positions(f.adminTok, "/api/v1/admin/personnel-tracking")
		require.Equal(t, http.StatusUnprocessableEntity, code, "project_code is required")
		code, _ = f.positions(danruTok, "/api/v1/admin/personnel-tracking?project_code=1K-IBMI-S")
		require.Equal(t, http.StatusForbidden, code, "not an admin")
	})
}

// socket is a WebSocket the test holds open on the production router.
type socket struct {
	t *testing.T
	c *websocket.Conn
}

func (f *flow) dial(token string) *socket {
	f.t.Helper()
	url := "ws" + strings.TrimPrefix(f.srv.URL, "http") + "/api/v1/ws"
	c, _, err := websocket.Dial(f.t.Context(), url, &websocket.DialOptions{
		Subprotocols: []string{realtime.Subprotocol}, HTTPHeader: http.Header{"Authorization": {"Bearer " + token}},
	})
	require.NoError(f.t, err)
	f.t.Cleanup(func() { _ = c.CloseNow() })
	s := &socket{t: f.t, c: c}
	require.Equal(f.t, "hello", s.next(5 * time.Second)["type"])
	return s
}

// next returns the next frame, or nil when none arrives within wait.
func (s *socket) next(wait time.Duration) map[string]any {
	s.t.Helper()
	ctx, cancel := context.WithTimeout(s.t.Context(), wait)
	defer cancel()
	_, raw, err := s.c.Read(ctx)
	if err != nil {
		return nil
	}
	var frame map[string]any
	require.NoError(s.t, json.Unmarshal(raw, &frame))
	return frame
}

// until returns the first frame of the type within wait, skipping the others.
func (s *socket) until(typ string, wait time.Duration) map[string]any {
	s.t.Helper()
	deadline := time.Now().Add(wait)
	for time.Until(deadline) > 0 {
		frame := s.next(time.Until(deadline))
		if frame != nil && frame["type"] == typ {
			return frame
		}
	}
	return nil
}

func (s *socket) send(typ string, data any) {
	s.t.Helper()
	raw, err := json.Marshal(map[string]any{"type": typ, "data": data})
	require.NoError(s.t, err)
	require.NoError(s.t, s.c.Write(s.t.Context(), websocket.MessageText, raw))
}

func (s *socket) subscribe(rooms ...string) (joined, denied []any) {
	s.t.Helper()
	s.send("subscribe", map[string]any{"rooms": rooms})
	ack := s.until("subscribe.ack", 5*time.Second)
	require.NotNil(s.t, ack)
	data := ack["data"].(map[string]any)
	return data["joined"].([]any), data["denied"].([]any)
}

func TestLocationReachesTheDanrusSocket(t *testing.T) {
	f := newFlow(t)
	_, danruTok := f.danru()
	f.onShift(f.person)

	danru := f.dial(danruTok)
	joined, denied := danru.subscribe("team.1K-IBMI-S1.tracking", "project.1K-IBMI-S.tracking")
	require.Equal(t, []any{"team.1K-IBMI-S1.tracking"}, joined, "the project room is for admins")
	require.Len(t, denied, 1)

	// Another organization's person asks for the same room name: it is not theirs.
	other := int64(2)
	tok, _, err := f.tokens.NewAccessToken(900999, other, 5999)
	require.NoError(t, err)
	foreign := f.dial(tok)
	joinedElsewhere, deniedElsewhere := foreign.subscribe("team.1K-IBMI-S1.tracking")
	require.Empty(t, joinedElsewhere)
	require.Len(t, deniedElsewhere, 1)

	member := f.dial(f.personTok)
	_, deniedMember := member.subscribe("team.1K-IBMI-S1.tracking")
	require.Len(t, deniedMember, 1, "an ordinary member does not get the map of the regu")

	t.Run("a REST upload is broadcast to the regu's DANRU, ids only", func(t *testing.T) {
		code, _, _ := f.upload(f.personTok, batch(pointIn{time.Now().Add(-time.Second), -6.2, 106.8}))
		require.Equal(t, http.StatusOK, code)
		frame := danru.until("location.broadcast", 5*time.Second)
		require.NotNil(t, frame, "the DANRU's socket gets the position")
		require.Equal(t, "team.1K-IBMI-S1.tracking", frame["room"])
		data := frame["data"].(map[string]any)
		require.EqualValues(t, f.person, data["personnel_id"])
		require.InDelta(t, -6.2, data["latitude"], 1e-6)
		require.NotContains(t, data, "full_name", "no name goes through a socket")
		require.Nil(t, foreign.until("location.broadcast", time.Second), "another organization's socket gets nothing")
	})

	t.Run("a location.update frame goes through the same rules", func(t *testing.T) {
		// Wait out the 2 s debounce window so the next live point is broadcast too.
		time.Sleep(2100 * time.Millisecond)
		member.send("location.update", map[string]any{"latitude": -6.25, "longitude": 106.85, "accuracy_meters": 8})
		frame := danru.until("location.broadcast", 5*time.Second)
		require.NotNil(t, frame)
		require.InDelta(t, -6.25, frame["data"].(map[string]any)["latitude"], 1e-6)

		// Off shift (tracking switched off for the project) the frame is refused with a reason,
		// and the socket stays open.
		f.exec("UPDATE projects SET gps_tracking_enabled = false WHERE code = '1K-IBMI-S'")
		time.Sleep(1100 * time.Millisecond) // past the one-frame-per-second limit
		member.send("location.update", map[string]any{"latitude": -6.3, "longitude": 106.9, "accuracy_meters": 8})
		refused := member.until("error", 5*time.Second)
		require.NotNil(t, refused)
		data := refused["data"].(map[string]any)
		require.Equal(t, "TRACKING_DISABLED", data["reason"])
		require.EqualValues(t, realtime.ErrorForbidden, data["code"])
		member.send("ping", map[string]any{})
		require.NotNil(t, member.until("pong", 5*time.Second), "the socket survived the refusal")
	})
}

func TestLocationFramesAreRateLimitedAndEndWithTheSession(t *testing.T) {
	f := newFlow(t)
	_, danruTok := f.danru()
	f.onShift(f.person)
	member := f.dial(f.personTok)

	t.Run("a second location.update within a second is refused, the socket stays", func(t *testing.T) {
		frame := map[string]any{"latitude": -6.25, "longitude": 106.85, "accuracy_meters": 8}
		member.send("location.update", frame)
		member.send("location.update", frame)
		refused := member.until("error", 5*time.Second)
		require.NotNil(t, refused)
		require.EqualValues(t, realtime.ErrorTooFast, refused["data"].(map[string]any)["code"])
		member.send("ping", map[string]any{})
		require.NotNil(t, member.until("pong", 5*time.Second))
	})

	t.Run("after logout the open socket neither counts as online nor stores positions", func(t *testing.T) {
		code, raw := f.do(http.MethodPost, "/api/v1/auth/logout", f.personTok, nil, nil)
		require.Less(t, code, 300, string(raw))
		time.Sleep(1100 * time.Millisecond) // past the rate limit, so only the session can refuse
		var before int
		f.query([]any{&before}, "SELECT count(*) FROM location_logs WHERE personnel_id = $1", f.person)

		member.send("location.update", map[string]any{"latitude": -6.3, "longitude": 106.9, "accuracy_meters": 8})
		member.send("ping", map[string]any{})
		require.Nil(t, member.until("pong", 2*time.Second), "the socket is closed, not answering")

		online, _ := f.member(danruTok, "1K-IBMI-S1", f.person)
		require.False(t, online, "a frame of an ended session is not a sign of life")
		var after int
		f.query([]any{&after}, "SELECT count(*) FROM location_logs WHERE personnel_id = $1", f.person)
		require.Equal(t, before, after)
	})
}

func TestLocationPointsAreJudgedByWhenTheyWereRecorded(t *testing.T) {
	now := time.Now()

	t.Run("a point recorded off duty is dropped even when sent during a shift", func(t *testing.T) {
		f := newFlow(t)
		f.onShift(f.person)
		code, res, _ := f.upload(f.personTok, batch(
			pointIn{now.Add(-3 * time.Hour), -6.9, 106.9}, // before the shift started
			pointIn{now.Add(-time.Minute), -6.2, 106.8},
		))
		require.Equal(t, http.StatusOK, code)
		require.Equal(t, uploadResult{Accepted: 1, Rejected: 1}, res)
		var rows int
		f.query([]any{&rows}, "SELECT count(*) FROM location_logs WHERE personnel_id = $1", f.person)
		require.Equal(t, 1, rows, "no trail of the off-duty hours")
	})

	t.Run("a batch recorded during a shift is accepted after the shift ended, and not broadcast", func(t *testing.T) {
		f := newFlow(t)
		_, danruTok := f.danru()
		// The shift ran from three hours ago to one hour ago: nobody is on shift now.
		f.scheduleShift(f.person, now.Add(-3*time.Hour), now.Add(-time.Hour))
		danru := f.dial(danruTok)
		joined, _ := danru.subscribe("team.1K-IBMI-S1.tracking")
		require.Len(t, joined, 1)

		inside := now.Add(-2 * time.Hour)
		code, res, _ := f.upload(f.personTok, batch(
			pointIn{now.Add(-4 * time.Hour), -6.9, 106.9},  // before the shift
			pointIn{inside, -6.2, 106.8},                   // during it
			pointIn{now.Add(-30 * time.Minute), -6.8, 106}, // after it
		))
		require.Equal(t, http.StatusOK, code)
		require.Equal(t, uploadResult{Accepted: 1, Rejected: 2}, res)
		var at time.Time
		f.query([]any{&at}, "SELECT logged_at FROM personnel_last_locations WHERE personnel_id = $1", f.person)
		require.WithinDuration(t, inside, at, time.Second)
		require.Nil(t, danru.until("location.broadcast", 1500*time.Millisecond), "a late batch is history, not a live position")

		// Only a point outside every shift: nothing to track at all.
		code, _, reason := f.upload(f.personTok, batch(pointIn{now.Add(-30 * time.Minute), -6.8, 106}))
		require.Equal(t, http.StatusForbidden, code)
		require.Equal(t, "TRACKING_DISABLED", reason)
	})
}

func TestLocationUploadIsRateLimitedPerPerson(t *testing.T) {
	f := newFlow(t)
	f.onShift(f.person)
	body := batch(pointIn{time.Now().Add(-time.Minute), -6.2, 106.8})
	var last int
	for range 32 {
		last, _, _ = f.upload(f.personTok, body)
	}
	require.Equal(t, http.StatusTooManyRequests, last)
	// Another person is not affected by it.
	other := f.addPerson(900180, "ANGGOTA003", "Anggota Tiga")
	f.exec("INSERT INTO team_members (team_code, personnel_id, role, start_date) VALUES ('1K-IBMI-S1', $1, 'MEMBER', current_date - 30)", other)
	f.onShift(other)
	code, _, _ := f.upload(f.token(other, 5020), body)
	require.Equal(t, http.StatusOK, code)
}
