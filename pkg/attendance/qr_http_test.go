package attendance_test

import (
	"encoding/base64"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"siapp/pkg/attendance"
)

// base64Ref builds a shift_ref by hand, the way a client that guessed the format would.
func base64Ref(team, day string, shiftID int64) string {
	return base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf("%s|%s|%d", team, day, shiftID)))
}

// danru adds the DANRU of the regu, who may show its code, and returns their id and token.
func (e *env) danru(org int64, team string) (int64, string) {
	e.t.Helper()
	id, token := e.person(org)
	e.exec(org, "INSERT INTO team_members (team_code, personnel_id, role, start_date) VALUES ($1, $2, 'DANRU', '2000-01-01')", team, id)
	return id, token
}

type qrCode struct {
	Token     string `json:"token"`
	ExpiresAt string `json:"expires_at"`
}

func (e *env) qrDisplay(token, team, direction string) reply {
	e.t.Helper()
	return e.api(token, http.MethodGet, "/attendances/qr/display?team_code="+team+"&direction="+direction, nil, "")
}

// qrCodeOf is the token a DANRU's phone shows.
func (e *env) qrCodeOf(token, team, direction string) string {
	e.t.Helper()
	r := e.qrDisplay(token, team, direction)
	require.Equal(e.t, http.StatusOK, r.Status, "body: %s", r.Raw)
	return data[qrCode](e.t, r).Token
}

// qrScan is a guard's scan: path is "check-in" or "check-out", fix is an optional latitude and longitude.
func (e *env) qrScan(w world, path, ref, token string, fix ...float64) reply {
	e.t.Helper()
	payload := map[string]any{"shift_ref": ref, "token": token}
	if len(fix) == 2 {
		payload["latitude"], payload["longitude"] = fix[0], fix[1]
	}
	return e.api(w.token, http.MethodPost, "/attendances/qr/"+path, payload, e.newKey())
}

func TestADanruShowsACodeAndAGuardChecksInAndOutWithIt(t *testing.T) {
	e := newEnv(t)
	w := e.world()
	danruID, danruToken := e.danru(w.org, "1K-IBMI-S1")
	ref := e.onlyShift(w.token).ShiftRef

	shown := e.qrDisplay(danruToken, "1K-IBMI-S1", "IN")
	require.Equal(t, http.StatusOK, shown.Status, "body: %s", shown.Raw)
	code := data[qrCode](t, shown)
	require.Equal(t, tuesday.Add(time.Minute).Format("2006-01-02T15:04:05.000Z"), code.ExpiresAt, "60 s from now")

	in := e.qrScan(w, "check-in", ref, code.Token)
	require.Equal(t, http.StatusCreated, in.Status, "body: %s", in.Raw)
	got := data[record](t, in)
	require.Equal(t, "PRESENT", got.Status)
	require.Equal(t, "QR", *got.CheckInMethod)
	require.Equal(t, 30, got.LateMinutes, "the same late count as a selfie check-in")
	require.Nil(t, got.CheckInDistance, "no fix was sent, so nothing was measured")
	require.Nil(t, got.CheckInWithinArea)
	require.Equal(t, 1, e.count(w.org, "SELECT count(*) FROM attendances WHERE check_in_qr_danru_id = $1 AND check_in_media_id IS NULL", danruID))
	require.Equal(t, 1, e.outbox(w.org))
	require.Equal(t, 1, e.count(w.org, "SELECT count(*) FROM personel_outbox WHERE (payload->>'check_in_qr_danru_id')::bigint = $1", danruID))

	e.clock.Advance(8 * time.Hour)
	out := e.qrScan(w, "check-out", ref, e.qrCodeOf(danruToken, "1K-IBMI-S1", "OUT"))
	require.Equal(t, http.StatusOK, out.Status, "body: %s", out.Raw)
	require.Equal(t, "QR", *data[record](t, out).CheckOutMethod)
	require.Equal(t, 1, e.count(w.org, "SELECT count(*) FROM attendances WHERE check_out_qr_danru_id = $1", danruID))
	require.Equal(t, []string{"CHECK_IN", "CHECK_OUT"}, e.outboxTypes(w.org))
	require.Equal(t, 1, e.count(w.org, "SELECT count(*) FROM personel_outbox WHERE (payload->>'check_out_qr_danru_id')::bigint = $1", danruID))
}

func TestOnlyWhoMayShowACodeGetsOne(t *testing.T) {
	e := newEnv(t)
	w := e.world()
	_, danruToken := e.danru(w.org, "1K-IBMI-S1")
	e.exec(w.org, "INSERT INTO teams (code, project_code, description) VALUES ('1K-IBMI-S2', '1K-IBMI-S', 'Regu 2')")

	t.Run("a member is refused", func(t *testing.T) {
		e.qrDisplay(w.token, "1K-IBMI-S1", "IN").requireFailure(t, http.StatusForbidden, "FORBIDDEN", "PERMISSION_DENIED")
	})
	t.Run("a DANRU cannot show the code of another regu", func(t *testing.T) {
		e.qrDisplay(danruToken, "1K-IBMI-S2", "IN").requireFailure(t, http.StatusForbidden, "FORBIDDEN", "PERMISSION_DENIED")
	})
	t.Run("nor of a regu that does not exist", func(t *testing.T) {
		e.qrDisplay(danruToken, "NOPE", "IN").requireFailure(t, http.StatusForbidden, "FORBIDDEN", "PERMISSION_DENIED")
	})
	t.Run("another organization's DANRU is no different", func(t *testing.T) {
		foreign := e.world()
		_, foreignDanru := e.danru(foreign.org, "1K-IBMI-S1")
		// Its own regu has the same code, and the code is its own organization's token (below).
		require.Equal(t, http.StatusOK, e.qrDisplay(foreignDanru, "1K-IBMI-S1", "IN").Status)
		e.qrDisplay(foreignDanru, "1K-IBMI-S2", "IN").requireFailure(t, http.StatusForbidden, "FORBIDDEN", "PERMISSION_DENIED")
	})
	t.Run("the request must name a regu and a direction", func(t *testing.T) {
		for _, path := range []string{
			"/attendances/qr/display",
			"/attendances/qr/display?team_code=1K-IBMI-S1",
			"/attendances/qr/display?direction=IN",
			"/attendances/qr/display?team_code=1K-IBMI-S1&direction=in",
			"/attendances/qr/display?team_code=1K-IBMI-S1&direction=BOTH",
		} {
			r := e.raw(danruToken, http.MethodGet, path, nil, "")
			require.Equal(t, http.StatusUnprocessableEntity, r.Status, "%s: %s", path, r.Raw)
		}
	})
	t.Run("no token at all", func(t *testing.T) {
		r := e.raw("", http.MethodGet, "/attendances/qr/display?team_code=1K-IBMI-S1&direction=IN", nil, "")
		require.Equal(t, http.StatusUnauthorized, r.Status, "body: %s", r.Raw)
	})
}

func TestACodeOfAnotherRegu(t *testing.T) {
	e := newEnv(t)
	w := e.world()
	e.exec(w.org, "INSERT INTO teams (code, project_code, description) VALUES ('1K-IBMI-S2', '1K-IBMI-S', 'Regu 2')")
	_, otherDanru := e.danru(w.org, "1K-IBMI-S2")
	ref := e.onlyShift(w.token).ShiftRef // the shift of regu 1

	e.qrScan(w, "check-in", ref, e.qrCodeOf(otherDanru, "1K-IBMI-S2", "IN")).
		requireFailure(t, http.StatusUnprocessableEntity, "UNPROCESSABLE", attendance.ReasonQRWrongTeam)
	require.Zero(t, e.count(w.org, "SELECT count(*) FROM attendances"))
	require.Zero(t, e.outbox(w.org))
}

func TestAnotherOrganizationsCodeIsInvalid(t *testing.T) {
	e := newEnv(t)
	w := e.world()
	foreign := e.world() // the same regu code, in another organization
	_, foreignDanru := e.danru(foreign.org, "1K-IBMI-S1")
	ref := e.onlyShift(w.token).ShiftRef

	e.qrScan(w, "check-in", ref, e.qrCodeOf(foreignDanru, "1K-IBMI-S1", "IN")).
		requireFailure(t, http.StatusUnprocessableEntity, "UNPROCESSABLE", attendance.ReasonQRInvalid)
	require.Zero(t, e.count(w.org, "SELECT count(*) FROM attendances"))
	require.Zero(t, e.outbox(foreign.org))
}

func TestACodeExpiresAfterAMinute(t *testing.T) {
	// 08:30:00 is when the code is shown; the tolerance is five seconds.
	for _, tc := range []struct {
		name  string
		after time.Duration
		ok    bool
	}{
		{"just shown", 0, true},
		{"a minute later", time.Minute, true},
		{"inside the tolerance", time.Minute + 5*time.Second, true},
		{"past the tolerance", time.Minute + 6*time.Second, false},
		{"much later", time.Hour, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			w := e.world()
			_, danruToken := e.danru(w.org, "1K-IBMI-S1")
			ref := e.onlyShift(w.token).ShiftRef
			code := e.qrCodeOf(danruToken, "1K-IBMI-S1", "IN")

			e.clock.Advance(tc.after)
			r := e.qrScan(w, "check-in", ref, code)
			if tc.ok {
				require.Equal(t, http.StatusCreated, r.Status, "body: %s", r.Raw)
				return
			}
			r.requireFailure(t, http.StatusUnprocessableEntity, "UNPROCESSABLE", attendance.ReasonQRExpired)
			require.Zero(t, e.count(w.org, "SELECT count(*) FROM attendances"))
		})
	}
}

func TestACodeOfTheOtherDirectionIsInvalid(t *testing.T) {
	e := newEnv(t)
	w := e.world()
	_, danruToken := e.danru(w.org, "1K-IBMI-S1")
	ref := e.onlyShift(w.token).ShiftRef
	in, out := e.qrCodeOf(danruToken, "1K-IBMI-S1", "IN"), e.qrCodeOf(danruToken, "1K-IBMI-S1", "OUT")

	e.qrScan(w, "check-in", ref, out).requireFailure(t, http.StatusUnprocessableEntity, "UNPROCESSABLE", attendance.ReasonQRInvalid)
	require.Equal(t, http.StatusCreated, e.qrScan(w, "check-in", ref, in).Status)
	e.qrScan(w, "check-out", ref, in).requireFailure(t, http.StatusUnprocessableEntity, "UNPROCESSABLE", attendance.ReasonQRInvalid)
	require.Equal(t, http.StatusOK, e.qrScan(w, "check-out", ref, out).Status)
}

func TestAForgedCodeIsInvalid(t *testing.T) {
	e := newEnv(t)
	w := e.world()
	_, danruToken := e.danru(w.org, "1K-IBMI-S1")
	ref := e.onlyShift(w.token).ShiftRef
	good := e.qrCodeOf(danruToken, "1K-IBMI-S1", "IN")
	body, sig, _ := strings.Cut(good, ".")

	flip := func(s string) string {
		if s[0] == 'A' {
			return "B" + s[1:]
		}
		return "A" + s[1:]
	}
	for name, token := range map[string]string{
		"garbage":          "not-a-token",
		"a changed body":   flip(body) + "." + sig,
		"a changed MAC":    body + "." + flip(sig),
		"the MAC dropped":  body,
		"two tokens glued": good + "." + good,
	} {
		t.Run(name, func(t *testing.T) {
			e.qrScan(w, "check-in", ref, token).requireFailure(t, http.StatusUnprocessableEntity, "UNPROCESSABLE", attendance.ReasonQRInvalid)
		})
	}
	require.Zero(t, e.count(w.org, "SELECT count(*) FROM attendances"))

	t.Run("the real one still works", func(t *testing.T) {
		require.Equal(t, http.StatusCreated, e.qrScan(w, "check-in", ref, good).Status)
	})
}

func TestAScanMustBeWellFormed(t *testing.T) {
	e := newEnv(t)
	w := e.world()
	_, danruToken := e.danru(w.org, "1K-IBMI-S1")
	ref := e.onlyShift(w.token).ShiftRef
	code := e.qrCodeOf(danruToken, "1K-IBMI-S1", "IN")

	for name, payload := range map[string]map[string]any{
		"no token":                {"shift_ref": ref},
		"no shift_ref":            {"token": code},
		"empty token":             {"shift_ref": ref, "token": ""},
		"latitude without a lng":  {"shift_ref": ref, "token": code, "latitude": -6.2},
		"longitude without a lat": {"shift_ref": ref, "token": code, "longitude": 106.8},
		"latitude out of range":   {"shift_ref": ref, "token": code, "latitude": 91.0, "longitude": 106.8},
		"longitude out of range":  {"shift_ref": ref, "token": code, "latitude": -6.2, "longitude": 181.0},
		"negative accuracy":       {"shift_ref": ref, "token": code, "latitude": -6.2, "longitude": 106.8, "accuracy_meters": -1},
	} {
		t.Run(name, func(t *testing.T) {
			r := e.raw(w.token, http.MethodPost, "/attendances/qr/check-in", payload, e.newKey())
			require.Equal(t, http.StatusUnprocessableEntity, r.Status, "body: %s", r.Raw)
		})
	}
	require.Zero(t, e.count(w.org, "SELECT count(*) FROM attendances"))
}

func TestAShiftThatIsNotTheirsIsRefusedLikeASelfieOne(t *testing.T) {
	e := newEnv(t)
	w := e.world()
	_, danruToken := e.danru(w.org, "1K-IBMI-S1")
	code := e.qrCodeOf(danruToken, "1K-IBMI-S1", "IN")

	t.Run("a ref that is not one", func(t *testing.T) {
		e.qrScan(w, "check-in", "!!!", code).requireFailure(t, http.StatusUnprocessableEntity, "UNPROCESSABLE", attendance.ReasonShiftNotActive)
	})
	t.Run("a shift of the right regu on another day", func(t *testing.T) {
		ref := base64Ref("1K-IBMI-S1", "2026-03-11", w.shiftID)
		e.qrScan(w, "check-in", ref, code).requireFailure(t, http.StatusUnprocessableEntity, "UNPROCESSABLE", attendance.ReasonShiftNotActive)
	})
}

func TestTheGeofenceStillAppliesToAFixSentWithAScan(t *testing.T) {
	t.Run("strict refuses a fix outside the radius and writes nothing", func(t *testing.T) {
		e := newEnv(t)
		w := e.world()
		_, danruToken := e.danru(w.org, "1K-IBMI-S1")
		e.exec(w.org, "UPDATE projects SET strict_attendance_geofence = true")
		ref := e.onlyShift(w.token).ShiftRef
		code := e.qrCodeOf(danruToken, "1K-IBMI-S1", "IN")

		e.qrScan(w, "check-in", ref, code, farLat, siteLng).
			requireFailure(t, http.StatusUnprocessableEntity, "UNPROCESSABLE", attendance.ReasonOutsideArea)
		require.Zero(t, e.count(w.org, "SELECT count(*) FROM attendances"))
		require.Zero(t, e.outbox(w.org))
		// With no fix the DANRU's code is the evidence, and it works.
		require.Equal(t, http.StatusCreated, e.qrScan(w, "check-in", ref, code).Status)
	})

	t.Run("a loose geofence saves the record flagged", func(t *testing.T) {
		e := newEnv(t)
		w := e.world()
		_, danruToken := e.danru(w.org, "1K-IBMI-S1")
		ref := e.onlyShift(w.token).ShiftRef

		r := e.qrScan(w, "check-in", ref, e.qrCodeOf(danruToken, "1K-IBMI-S1", "IN"), farLat, siteLng)
		require.Equal(t, http.StatusCreated, r.Status, "body: %s", r.Raw)
		got := data[record](t, r)
		require.False(t, *got.CheckInWithinArea)
		require.NotNil(t, got.CheckInDistance)
	})

	t.Run("a fix inside the radius is measured", func(t *testing.T) {
		e := newEnv(t)
		w := e.world()
		_, danruToken := e.danru(w.org, "1K-IBMI-S1")
		ref := e.onlyShift(w.token).ShiftRef

		r := e.qrScan(w, "check-in", ref, e.qrCodeOf(danruToken, "1K-IBMI-S1", "IN"), siteLat, siteLng)
		require.Equal(t, http.StatusCreated, r.Status, "body: %s", r.Raw)
		require.True(t, *data[record](t, r).CheckInWithinArea)
	})
}

func TestOneCodeServesManyGuardsButEachChecksInOnce(t *testing.T) {
	e := newEnv(t)
	w := e.world()
	_, danruToken := e.danru(w.org, "1K-IBMI-S1")
	second, secondToken := e.guard(w.org, w.shiftID)
	other := world{org: w.org, shiftID: w.shiftID, personID: second, token: secondToken}
	code := e.qrCodeOf(danruToken, "1K-IBMI-S1", "IN")

	// Not single-use: the second guard scans the same code the first one did.
	require.Equal(t, http.StatusCreated, e.qrScan(w, "check-in", e.onlyShift(w.token).ShiftRef, code).Status)
	require.Equal(t, http.StatusCreated, e.qrScan(other, "check-in", e.onlyShift(secondToken).ShiftRef, code).Status)
	require.Equal(t, 2, e.outbox(w.org))

	// What stops a repeat is the one record per shift, not the code.
	e.qrScan(w, "check-in", e.onlyShift(w.token).ShiftRef, code).
		requireFailure(t, http.StatusConflict, "DUPLICATE", attendance.ReasonAlreadyCheckedIn)
	require.Equal(t, 2, e.outbox(w.org))
}

func TestACheckOutNeedsACheckInFirst(t *testing.T) {
	e := newEnv(t)
	w := e.world()
	_, danruToken := e.danru(w.org, "1K-IBMI-S1")
	ref := e.onlyShift(w.token).ShiftRef
	out := e.qrCodeOf(danruToken, "1K-IBMI-S1", "OUT")

	e.qrScan(w, "check-out", ref, out).requireFailure(t, http.StatusUnprocessableEntity, "UNPROCESSABLE", attendance.ReasonNotCheckedIn)
	require.Zero(t, e.outbox(w.org))
}

func TestAQRCheckInIsIdempotent(t *testing.T) {
	e := newEnv(t)
	w := e.world()
	_, danruToken := e.danru(w.org, "1K-IBMI-S1")
	ref := e.onlyShift(w.token).ShiftRef
	code := e.qrCodeOf(danruToken, "1K-IBMI-S1", "IN")
	payload := map[string]any{"shift_ref": ref, "token": code}

	// Without a key the request is refused before anything is read.
	require.Equal(t, http.StatusUnprocessableEntity, e.raw(w.token, http.MethodPost, "/attendances/qr/check-in", payload, "").Status)

	key := e.newKey()
	first := e.api(w.token, http.MethodPost, "/attendances/qr/check-in", payload, key)
	require.Equal(t, http.StatusCreated, first.Status, "body: %s", first.Raw)
	again := e.api(w.token, http.MethodPost, "/attendances/qr/check-in", payload, key)
	require.Equal(t, http.StatusCreated, again.Status, "body: %s", again.Raw)
	require.JSONEq(t, string(first.Raw), string(again.Raw))
	require.Equal(t, 1, e.outbox(w.org))
}

func TestAQRCheckInFollowsTheSameRulesAsASelfieOne(t *testing.T) {
	t.Run("a previous working day left open blocks it", func(t *testing.T) {
		e := newEnv(t)
		w := e.world()
		_, danruToken := e.danru(w.org, "1K-IBMI-S1")
		ref := e.onlyShift(w.token).ShiftRef
		e.previousDay(w, "open")

		e.qrScan(w, "check-in", ref, e.qrCodeOf(danruToken, "1K-IBMI-S1", "IN")).
			requireFailure(t, http.StatusUnprocessableEntity, "UNPROCESSABLE", attendance.ReasonPreviousNotCheckedOut)
		require.Zero(t, e.outbox(w.org))
	})

	t.Run("a shift already recorded as leave is not turned into a presence", func(t *testing.T) {
		e := newEnv(t)
		w := e.world()
		_, danruToken := e.danru(w.org, "1K-IBMI-S1")
		ref := e.onlyShift(w.token).ShiftRef
		e.exec(w.org, `INSERT INTO attendances (personnel_id, team_code, project_code, work_date, shift_id, shift_starts_at, shift_ends_at, timezone, status)
VALUES ($1, '1K-IBMI-S1', '1K-IBMI-S', $2, $3, '2026-03-10T01:00:00Z', '2026-03-10T10:00:00Z', 'Asia/Jakarta', 'LEAVE')`,
			w.personID, workDate, w.shiftID)

		e.qrScan(w, "check-in", ref, e.qrCodeOf(danruToken, "1K-IBMI-S1", "IN")).
			requireFailure(t, http.StatusUnprocessableEntity, "UNPROCESSABLE", attendance.ReasonNotAllowed)
	})

	t.Run("the guard needs the attendance.checkin permission", func(t *testing.T) {
		e := newEnv(t)
		w := e.world()
		_, danruToken := e.danru(w.org, "1K-IBMI-S1")
		ref := e.onlyShift(w.token).ShiftRef
		code := e.qrCodeOf(danruToken, "1K-IBMI-S1", "IN")
		e.exec(w.org, "UPDATE role_permissions SET scope = 'NONE' WHERE permission = 'attendance.checkin' AND team_role = 'MEMBER'")

		r := e.qrScan(w, "check-in", ref, code)
		require.Equal(t, http.StatusForbidden, r.Status, "body: %s", r.Raw)
	})
}

func TestTheQRMethodIsOffWithoutASecret(t *testing.T) {
	e := newEnvWithQR(t, "")
	w := e.world()
	_, danruToken := e.danru(w.org, "1K-IBMI-S1")
	ref := e.onlyShift(w.token).ShiftRef

	e.qrDisplay(danruToken, "1K-IBMI-S1", "IN").requireFailure(t, http.StatusServiceUnavailable, "INTERNAL_ERROR", attendance.ReasonQRUnavailable)
	e.qrScan(w, "check-in", ref, "any.token").requireFailure(t, http.StatusServiceUnavailable, "INTERNAL_ERROR", attendance.ReasonQRUnavailable)
	e.qrScan(w, "check-out", ref, "any.token").requireFailure(t, http.StatusServiceUnavailable, "INTERNAL_ERROR", attendance.ReasonQRUnavailable)
	require.Zero(t, e.outbox(w.org))

	// The selfie method is untouched.
	require.Equal(t, http.StatusCreated, e.checkIn(w, ref, siteLat, siteLng).Status)
}

func TestAnExpiredCodeOfAnotherOrganizationOrDirectionIsStillInvalid(t *testing.T) {
	e := newEnv(t)
	w := e.world()
	foreign := e.world()
	_, foreignDanru := e.danru(foreign.org, "1K-IBMI-S1")
	_, danruToken := e.danru(w.org, "1K-IBMI-S1")
	ref := e.onlyShift(w.token).ShiftRef
	foreignCode := e.qrCodeOf(foreignDanru, "1K-IBMI-S1", "IN")
	outCode := e.qrCodeOf(danruToken, "1K-IBMI-S1", "OUT")

	e.clock.Advance(time.Hour) // every code is long expired

	// Not "expired": that answer is only for a code the caller could have used.
	e.qrScan(w, "check-in", ref, foreignCode).requireFailure(t, http.StatusUnprocessableEntity, "UNPROCESSABLE", attendance.ReasonQRInvalid)
	e.qrScan(w, "check-in", ref, outCode).requireFailure(t, http.StatusUnprocessableEntity, "UNPROCESSABLE", attendance.ReasonQRInvalid)
}

func TestWhoMayShowACodeIsDecidedByTheGrantAndToday(t *testing.T) {
	t.Run("a DANRU whose membership has ended gets no code", func(t *testing.T) {
		e := newEnv(t)
		w := e.world()
		danruID, danruToken := e.danru(w.org, "1K-IBMI-S1")
		require.Equal(t, http.StatusOK, e.qrDisplay(danruToken, "1K-IBMI-S1", "IN").Status)

		e.exec(w.org, "UPDATE team_members SET end_date = '2026-03-09' WHERE personnel_id = $1", danruID)
		e.qrDisplay(danruToken, "1K-IBMI-S1", "IN").requireFailure(t, http.StatusForbidden, "FORBIDDEN", "PERMISSION_DENIED")
	})

	t.Run("a deleted regu gets no code", func(t *testing.T) {
		e := newEnv(t)
		w := e.world()
		_, danruToken := e.danru(w.org, "1K-IBMI-S1")
		e.exec(w.org, "UPDATE teams SET deleted_at = now() WHERE code = '1K-IBMI-S1'")

		e.qrDisplay(danruToken, "1K-IBMI-S1", "IN").requireFailure(t, http.StatusForbidden, "FORBIDDEN", "PERMISSION_DENIED")
	})

	t.Run("the grant is the authority, not the role", func(t *testing.T) {
		// An admin who gives members attendance.qr.display lets a member show the code, and the
		// record then names that member as the QR leader. By default only a DANRU holds it.
		e := newEnv(t)
		w := e.world()
		e.exec(w.org, "UPDATE role_permissions SET scope = 'TEAM' WHERE permission = 'attendance.qr.display' AND team_role = 'MEMBER'")
		second, secondToken := e.guard(w.org, w.shiftID)
		other := world{org: w.org, shiftID: w.shiftID, personID: second, token: secondToken}
		ref := e.onlyShift(secondToken).ShiftRef

		code := e.qrCodeOf(w.token, "1K-IBMI-S1", "IN") // the first guard shows it
		require.Equal(t, http.StatusCreated, e.qrScan(other, "check-in", ref, code).Status)
		require.Equal(t, 1, e.count(w.org, "SELECT count(*) FROM attendances WHERE personnel_id = $1 AND check_in_qr_danru_id = $2", second, w.personID))
	})

	t.Run("taking the grant away stops the next code", func(t *testing.T) {
		e := newEnv(t)
		w := e.world()
		_, danruToken := e.danru(w.org, "1K-IBMI-S1")
		require.Equal(t, http.StatusOK, e.qrDisplay(danruToken, "1K-IBMI-S1", "IN").Status)

		e.exec(w.org, "UPDATE role_permissions SET scope = 'NONE' WHERE permission = 'attendance.qr.display' AND team_role = 'DANRU'")
		e.qrDisplay(danruToken, "1K-IBMI-S1", "IN").requireFailure(t, http.StatusForbidden, "FORBIDDEN", "PERMISSION_DENIED")
	})
}

func TestUpdatingAQRCheckOutWithASelfieKeepsTheOldEvidenceInTheAudit(t *testing.T) {
	e := newEnv(t)
	w := e.world()
	danruID, danruToken := e.danru(w.org, "1K-IBMI-S1")
	ref := e.onlyShift(w.token).ShiftRef
	require.Equal(t, http.StatusCreated, e.qrScan(w, "check-in", ref, e.qrCodeOf(danruToken, "1K-IBMI-S1", "IN")).Status)
	e.clock.Advance(8 * time.Hour)
	out := e.qrScan(w, "check-out", ref, e.qrCodeOf(danruToken, "1K-IBMI-S1", "OUT"))
	require.Equal(t, http.StatusOK, out.Status, "body: %s", out.Raw)
	id := data[record](t, out).ID
	require.Equal(t, 1, e.count(w.org, "SELECT count(*) FROM attendances WHERE id = $1 AND check_out_qr_danru_id = $2", id, danruID))

	e.clock.Advance(10 * time.Minute)
	patch := e.api(w.token, http.MethodPatch, fmt.Sprintf("/attendances/%d/check-out", id),
		map[string]any{"media_ids": []int64{e.selfie(w.org, w.personID)}, "latitude": siteLat, "longitude": siteLng}, e.newKey())
	require.Equal(t, http.StatusOK, patch.Status, "body: %s", patch.Raw)
	require.Equal(t, "SELFIE", *data[record](t, patch).CheckOutMethod)

	// The record now says selfie, with no QR leader; the audit still says who vouched before.
	require.Equal(t, 1, e.count(w.org, "SELECT count(*) FROM attendances WHERE id = $1 AND check_out_qr_danru_id IS NULL", id))
	require.Equal(t, 1, e.count(w.org,
		"SELECT count(*) FROM audit_logs WHERE action = 'attendance.check_out.update' AND entity_id = $1 AND (metadata->'before'->>'qr_danru_id')::bigint = $2",
		id, danruID))
	require.Equal(t, []string{"CHECK_IN", "CHECK_OUT", "CHECK_OUT_UPDATED"}, e.outboxTypes(w.org))
}
