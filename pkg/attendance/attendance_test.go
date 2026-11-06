package attendance_test

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"

	"siapp/pkg/access"
	"siapp/pkg/attendance"
	"siapp/pkg/media"
	"siapp/pkg/organization"
	"siapp/pkg/platform/auth"
	"siapp/pkg/platform/database"
	"siapp/pkg/platform/idem"
	"siapp/pkg/platform/testkit"
)

func TestMain(m *testing.M) { os.Exit(testkit.NewDatabase(m)) }

// A Tuesday, 08:30 in Jakarta: half an hour into the Day Shift (08:00-17:00, tolerance 15).
var tuesday = time.Date(2026, 3, 10, 1, 30, 0, 0, time.UTC)

const (
	workDate = "2026-03-10"
	// The project's point and a place 11 km from it.
	siteLat, siteLng = -6.2, 106.8
	farLat           = -6.3
)

type testClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *testClock) Set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = t
}

func (c *testClock) Advance(d time.Duration) { c.Set(c.Now().Add(d)) }

type env struct {
	t      *testing.T
	srv    string
	client *testkit.Client
	clock  *testClock
	userID int64
	keys   int
	mu     sync.Mutex
}

const qrSecret = "http-test-attendance-qr-secret-0123456789"

func newEnv(t *testing.T) *env { return newEnvWithQR(t, qrSecret) }

// newEnvWithQR is newEnv with the QR method on, or off for an empty secret.
func newEnvWithQR(t *testing.T, secret string) *env {
	t.Helper()
	testkit.Reset(t)
	pool := testkit.Pool(t)
	clk := &testClock{now: tuesday}
	perms := access.NewService(pool, clk)
	service := attendance.NewService(pool, clk, organization.NewShifts(pool), organization.NewQuery(pool),
		media.NewService(pool, nil, clk, nil), perms)
	if secret != "" {
		qr, err := attendance.NewQRTokens(secret, clk)
		require.NoError(t, err)
		service.UseQR(qr)
	}
	idempotency := idem.New(idem.Config{Pool: pool, Identity: auth.Identity})
	h := attendance.NewHandler(service, perms, idempotency.Middleware(true))
	srv := testkit.NewServer(t, testkit.Deps{Mount: h.MountField})
	return &env{t: t, srv: srv.URL, client: testkit.NewClient(t, srv), clock: clk}
}

// ---------- the world ----------

type world struct {
	org      int64
	shiftID  int64
	personID int64
	token    string
}

func (e *env) inTenant(org int64, fn func(tx pgx.Tx)) {
	e.t.Helper()
	require.NoError(e.t, database.WithTenantTx(e.t.Context(), testkit.Pool(e.t), org, func(tx pgx.Tx) error {
		fn(tx)
		return nil
	}))
}

func (e *env) exec(org int64, sql string, args ...any) {
	e.t.Helper()
	e.inTenant(org, func(tx pgx.Tx) {
		_, err := tx.Exec(e.t.Context(), sql, args...)
		require.NoError(e.t, err)
	})
}

func (e *env) count(org int64, sql string, args ...any) (n int) {
	e.t.Helper()
	e.inTenant(org, func(tx pgx.Tx) {
		require.NoError(e.t, tx.QueryRow(e.t.Context(), sql, args...).Scan(&n))
	})
	return n
}

// person adds a person of the organization and returns their id and a token.
func (e *env) person(org int64) (int64, string) {
	e.t.Helper()
	e.mu.Lock()
	e.userID++
	userID := e.userID
	e.mu.Unlock()
	var id int64
	e.inTenant(org, func(tx pgx.Tx) {
		require.NoError(e.t, tx.QueryRow(e.t.Context(),
			"INSERT INTO personnels (personel_user_id, full_name, status) VALUES ($1, 'Petugas Absen', 'ACTIVE') RETURNING id",
			userID).Scan(&id))
	})
	return id, testkit.Token(e.t, org, id)
}

// world builds an organization with the hierarchy down to the regu 1K-IBMI-S1 (a project at the
// site, 150 m radius, geofence not strict), the Day Shift, and a guard who serves in the regu and
// is scheduled on the Day Shift on workDate.
func (e *env) world() world {
	e.t.Helper()
	org := testkit.NewOrg(e.t)
	_, err := access.EnsureDefaults(e.t.Context(), testkit.Pool(e.t), e.clock, org)
	require.NoError(e.t, err)
	e.exec(org, `
INSERT INTO regions (code, description) VALUES ('OS1', 'Outsourcing 1');
INSERT INTO branches (code, region_code, company_letter, description) VALUES ('1K', 'OS1', 'K', 'Cabang 1K');
INSERT INTO clients (code, branch_code, description, company_prefix) VALUES ('1K-IBMI', '1K', 'IBMI', 'IBMI');`)
	e.exec(org, `INSERT INTO projects (code, client_code, description, business_line, timezone, latitude, longitude)
VALUES ('1K-IBMI-S', '1K-IBMI', 'Proyek IBMI', 'security', 'Asia/Jakarta', $1, $2)`, siteLat, siteLng)
	e.exec(org, "INSERT INTO teams (code, project_code, description) VALUES ('1K-IBMI-S1', '1K-IBMI-S', 'Regu 1')")
	w := world{org: org}
	e.inTenant(org, func(tx pgx.Tx) {
		require.NoError(e.t, tx.QueryRow(e.t.Context(),
			"INSERT INTO shifts (name, start_minute, end_minute, tolerance_minutes) VALUES ('Day Shift', 480, 1020, 15) RETURNING id").Scan(&w.shiftID))
	})
	w.personID, w.token = e.guard(org, w.shiftID)
	return w
}

// guard adds a MEMBER of the regu with a schedule on workDate.
func (e *env) guard(org, shiftID int64) (int64, string) {
	e.t.Helper()
	id, token := e.person(org)
	e.exec(org, "INSERT INTO team_members (team_code, personnel_id, role, start_date) VALUES ('1K-IBMI-S1', $1, 'MEMBER', '2000-01-01')", id)
	e.exec(org, "INSERT INTO schedules (personnel_id, work_date, shift_id) VALUES ($1, $2, $3)", id, workDate, shiftID)
	return id, token
}

// selfie registers an uploaded selfie owned by the person.
func (e *env) selfie(org, personnelID int64) int64 {
	e.t.Helper()
	e.mu.Lock()
	e.keys++
	key := fmt.Sprintf("test/%d", e.keys)
	e.mu.Unlock()
	var id int64
	e.inTenant(org, func(tx pgx.Tx) {
		require.NoError(e.t, tx.QueryRow(e.t.Context(), `
INSERT INTO media (owner_personnel_id, purpose, content_type, size_bytes, storage_key, status, capture_source)
VALUES ($1, 'ATTENDANCE_SELFIE', 'image/jpeg', 1000, $2, 'UPLOADED', 'CAMERA') RETURNING id`, personnelID, key).Scan(&id))
	})
	return id
}

func (e *env) outbox(org int64) int {
	return e.count(org, "SELECT count(*) FROM personel_outbox")
}

func (e *env) outboxTypes(org int64) (types []string) {
	e.t.Helper()
	e.inTenant(org, func(tx pgx.Tx) {
		rows, err := tx.Query(e.t.Context(), "SELECT payload->>'type' FROM personel_outbox ORDER BY id")
		require.NoError(e.t, err)
		var err2 error
		types, err2 = pgx.CollectRows(rows, pgx.RowTo[string])
		require.NoError(e.t, err2)
	})
	return types
}

// ---------- requests ----------

type reply struct {
	Status int
	Raw    []byte
}

func (r reply) failure(t *testing.T) (code, reason string) {
	t.Helper()
	var e struct {
		Error struct {
			Code    string         `json:"code"`
			Details map[string]any `json:"details"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal(r.Raw, &e), "body: %s", r.Raw)
	reason, _ = e.Error.Details["reason"].(string)
	return e.Error.Code, reason
}

func (r reply) requireFailure(t *testing.T, status int, code, reason string) {
	t.Helper()
	require.Equal(t, status, r.Status, "body: %s", r.Raw)
	gotCode, gotReason := r.failure(t)
	require.Equal(t, code, gotCode)
	require.Equal(t, reason, gotReason)
}

func data[T any](t *testing.T, r reply) T {
	t.Helper()
	var env struct {
		Data T `json:"data"`
	}
	require.NoError(t, json.Unmarshal(r.Raw, &env), "body: %s", r.Raw)
	return env.Data
}

func (e *env) newKey() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.keys++
	return fmt.Sprintf("00000000-0000-4000-8000-%012d", e.keys)
}

// api is a request checked against the contract. Use it from one goroutine at a time.
func (e *env) api(token, method, path string, body any, key string) reply {
	e.t.Helper()
	e.client.Token = token
	defer func() { e.client.Token = "" }()
	var headers map[string]string
	if key != "" {
		headers = map[string]string{"Idempotency-Key": key}
	}
	res := e.client.DoWithHeaders(method, path, body, headers)
	return reply{Status: res.Status, Raw: res.Body}
}

// raw is a request outside the contract's checks, safe to call from several goroutines.
func (e *env) raw(token, method, path string, body any, key string) reply {
	e.t.Helper()
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		require.NoError(e.t, err)
		reader = bytes.NewReader(encoded)
	}
	req, err := http.NewRequestWithContext(e.t.Context(), method, e.srv+"/api/v1"+path, reader)
	require.NoError(e.t, err)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	res, err := http.DefaultClient.Do(req)
	require.NoError(e.t, err)
	defer func() { _ = res.Body.Close() }()
	out, err := io.ReadAll(res.Body)
	require.NoError(e.t, err)
	return reply{Status: res.StatusCode, Raw: out}
}

type today struct {
	ShiftRef     string  `json:"shift_ref"`
	AttendanceID *int64  `json:"attendance_id"`
	WorkDate     string  `json:"work_date"`
	ProjectName  string  `json:"project_name"`
	TeamName     string  `json:"team_name"`
	ShiftName    string  `json:"shift_name"`
	StartsAt     string  `json:"starts_at"`
	EndsAt       string  `json:"ends_at"`
	Status       *string `json:"status"`
	CheckInAt    *string `json:"check_in_at"`
	CheckOutAt   *string `json:"check_out_at"`
}

type record struct {
	ID                  int64   `json:"id"`
	TeamCode            string  `json:"team_code"`
	WorkDate            string  `json:"work_date"`
	Status              string  `json:"status"`
	CheckInAt           *string `json:"check_in_at"`
	CheckInMethod       *string `json:"check_in_method"`
	CheckInDistance     *int    `json:"check_in_distance_meters"`
	CheckInWithinArea   *bool   `json:"check_in_within_area"`
	LateMinutes         int     `json:"late_minutes"`
	CheckOutAt          *string `json:"check_out_at"`
	CheckOutMethod      *string `json:"check_out_method"`
	CheckOutUpdateCount int     `json:"check_out_update_count"`
	PersonelSyncStatus  string  `json:"personel_sync_status"`
	ShiftStartsAt       string  `json:"shift_starts_at"`
	Timezone            string  `json:"timezone"`
}

func (e *env) today(token string) []today {
	e.t.Helper()
	r := e.api(token, http.MethodGet, "/attendances/today", nil, "")
	require.Equal(e.t, http.StatusOK, r.Status, "body: %s", r.Raw)
	return data[[]today](e.t, r)
}

func (e *env) onlyShift(token string) today {
	e.t.Helper()
	shifts := e.today(token)
	require.Len(e.t, shifts, 1)
	return shifts[0]
}

func body(ref string, media int64, lat, lng float64) map[string]any {
	return map[string]any{"shift_ref": ref, "media_ids": []int64{media}, "latitude": lat, "longitude": lng, "accuracy_meters": 12.5}
}

func (e *env) checkIn(w world, ref string, lat, lng float64) reply {
	e.t.Helper()
	return e.api(w.token, http.MethodPost, "/attendances/selfie/check-in", body(ref, e.selfie(w.org, w.personID), lat, lng), e.newKey())
}

func (e *env) checkOut(w world, ref string, lat, lng float64) reply {
	e.t.Helper()
	return e.api(w.token, http.MethodPost, "/attendances/selfie/check-out", body(ref, e.selfie(w.org, w.personID), lat, lng), e.newKey())
}

// ---------- tests ----------

func TestTodayListsTheShiftOnlyInsideItsWindow(t *testing.T) {
	e := newEnv(t)
	w := e.world()

	shift := e.onlyShift(w.token)
	require.Equal(t, "Day Shift", shift.ShiftName)
	require.Equal(t, "Regu 1", shift.TeamName)
	require.Equal(t, "Proyek IBMI", shift.ProjectName)
	require.Equal(t, workDate, shift.WorkDate)
	require.Equal(t, "2026-03-10T01:00:00.000Z", shift.StartsAt)
	require.Equal(t, "2026-03-10T10:00:00.000Z", shift.EndsAt)
	require.Nil(t, shift.Status, "nothing is captured yet")
	require.Nil(t, shift.AttendanceID)

	// The window is [start - 2h, end + 4h] = [23:00Z the day before, 14:00Z].
	for at, want := range map[string]int{
		"2026-03-09T22:59:00Z": 0, "2026-03-09T23:00:00Z": 1, "2026-03-10T14:00:00Z": 1, "2026-03-10T14:01:00Z": 0,
	} {
		ts, err := time.Parse(time.RFC3339, at)
		require.NoError(t, err)
		e.clock.Set(ts)
		require.Len(t, e.today(w.token), want, at)
	}

	// Someone who serves in no regu is not scheduled anywhere that day.
	e.clock.Set(tuesday)
	_, stranger := e.person(w.org)
	e.api(stranger, http.MethodGet, "/attendances/today", nil, "").requireFailure(t, http.StatusForbidden, "FORBIDDEN", access.ReasonPermissionDenied)
}

func TestCheckInCheckOutAndUpdateEachWriteOneOutboxRow(t *testing.T) {
	e := newEnv(t)
	w := e.world()
	shift := e.onlyShift(w.token)

	in := e.checkIn(w, shift.ShiftRef, siteLat, siteLng)
	require.Equal(t, http.StatusCreated, in.Status, "body: %s", in.Raw)
	got := data[record](t, in)
	require.Equal(t, "PRESENT", got.Status)
	require.Equal(t, "SELFIE", *got.CheckInMethod)
	require.Equal(t, "1K-IBMI-S1", got.TeamCode)
	require.Equal(t, "Asia/Jakarta", got.Timezone)
	require.Equal(t, "2026-03-10T01:00:00.000Z", got.ShiftStartsAt)
	require.True(t, *got.CheckInWithinArea)
	require.Zero(t, *got.CheckInDistance)
	require.Equal(t, "PENDING", got.PersonelSyncStatus)
	require.Equal(t, 1, e.outbox(w.org))

	// The today list now shows the capture.
	shift = e.onlyShift(w.token)
	require.Equal(t, "PRESENT", *shift.Status)
	require.Equal(t, got.ID, *shift.AttendanceID)
	require.NotNil(t, shift.CheckInAt)
	require.Nil(t, shift.CheckOutAt)

	e.clock.Advance(8 * time.Hour)
	out := e.checkOut(w, shift.ShiftRef, siteLat, siteLng)
	require.Equal(t, http.StatusOK, out.Status, "body: %s", out.Raw)
	require.NotNil(t, data[record](t, out).CheckOutAt)
	require.Equal(t, 2, e.outbox(w.org))

	// "Update pulang" overwrites the check-out, counts itself and is audited.
	e.clock.Advance(10 * time.Minute)
	patch := e.api(w.token, http.MethodPatch, fmt.Sprintf("/attendances/%d/check-out", got.ID),
		map[string]any{"media_ids": []int64{e.selfie(w.org, w.personID)}, "latitude": siteLat, "longitude": siteLng}, e.newKey())
	require.Equal(t, http.StatusOK, patch.Status, "body: %s", patch.Raw)
	require.Equal(t, 1, data[record](t, patch).CheckOutUpdateCount)
	require.Equal(t, []string{"CHECK_IN", "CHECK_OUT", "CHECK_OUT_UPDATED"}, e.outboxTypes(w.org))
	require.Equal(t, 1, e.count(w.org, "SELECT count(*) FROM audit_logs WHERE action = 'attendance.check_out.update' AND entity_id = $1", got.ID))
	require.Equal(t, 3, e.count(w.org, "SELECT count(*) FROM personel_outbox WHERE status = 'PENDING' AND aggregate_id = $1", got.ID))
}

func TestADoubleCheckInFailsAndWritesOneOutboxRow(t *testing.T) {
	e := newEnv(t)
	w := e.world()
	ref := e.onlyShift(w.token).ShiftRef

	require.Equal(t, http.StatusCreated, e.checkIn(w, ref, siteLat, siteLng).Status)
	e.checkIn(w, ref, siteLat, siteLng).requireFailure(t, http.StatusConflict, "DUPLICATE", attendance.ReasonAlreadyCheckedIn)
	require.Equal(t, 1, e.outbox(w.org))
}

func TestParallelCheckInsCreateOneRecord(t *testing.T) {
	e := newEnv(t)
	w := e.world()
	ref := e.onlyShift(w.token).ShiftRef
	// Registered up front: each request brings a selfie and a key of its own.
	const parallel = 8
	selfies := make([]int64, parallel)
	for i := range selfies {
		selfies[i] = e.selfie(w.org, w.personID)
	}

	replies := make([]reply, parallel)
	var wg sync.WaitGroup
	for i := range replies {
		wg.Add(1)
		go func() {
			defer wg.Done()
			replies[i] = e.raw(w.token, http.MethodPost, "/attendances/selfie/check-in", body(ref, selfies[i], siteLat, siteLng), e.newKey())
		}()
	}
	wg.Wait()

	created := 0
	for _, r := range replies {
		switch r.Status {
		case http.StatusCreated:
			created++
		default:
			r.requireFailure(t, http.StatusConflict, "DUPLICATE", attendance.ReasonAlreadyCheckedIn)
		}
	}
	require.Equal(t, 1, created)
	require.Equal(t, 1, e.count(w.org, "SELECT count(*) FROM attendances"))
	require.Equal(t, 1, e.outbox(w.org))
	// Only the winner's selfie is attached: the others' stay free for a retry.
	require.Equal(t, 1, e.count(w.org, "SELECT count(*) FROM media WHERE attached_entity_id IS NOT NULL"))
}

func TestAnIdempotentReplayReturnsTheSameBodyAndWritesNothingMore(t *testing.T) {
	e := newEnv(t)
	w := e.world()
	ref := e.onlyShift(w.token).ShiftRef
	payload := body(ref, e.selfie(w.org, w.personID), siteLat, siteLng)
	key := e.newKey()

	first := e.api(w.token, http.MethodPost, "/attendances/selfie/check-in", payload, key)
	require.Equal(t, http.StatusCreated, first.Status, "body: %s", first.Raw)
	again := e.api(w.token, http.MethodPost, "/attendances/selfie/check-in", payload, key)
	require.Equal(t, http.StatusCreated, again.Status)
	require.Equal(t, data[record](t, first).ID, data[record](t, again).ID)
	require.Equal(t, 1, e.outbox(w.org))
	require.Equal(t, 1, e.count(w.org, "SELECT count(*) FROM attendances"))
}

func TestTheGeofence(t *testing.T) {
	t.Run("a strict geofence refuses a fix outside the radius and writes nothing", func(t *testing.T) {
		e := newEnv(t)
		w := e.world()
		e.exec(w.org, "UPDATE projects SET strict_attendance_geofence = true")
		ref := e.onlyShift(w.token).ShiftRef

		e.checkIn(w, ref, farLat, siteLng).requireFailure(t, http.StatusUnprocessableEntity, "UNPROCESSABLE", attendance.ReasonOutsideArea)
		require.Zero(t, e.count(w.org, "SELECT count(*) FROM attendances"))
		require.Zero(t, e.outbox(w.org))
		// Inside it still works.
		require.Equal(t, http.StatusCreated, e.checkIn(w, ref, siteLat, siteLng).Status)
	})

	t.Run("a loose geofence saves the record flagged", func(t *testing.T) {
		e := newEnv(t)
		w := e.world()
		ref := e.onlyShift(w.token).ShiftRef

		r := e.checkIn(w, ref, farLat, siteLng)
		require.Equal(t, http.StatusCreated, r.Status, "body: %s", r.Raw)
		got := data[record](t, r)
		require.False(t, *got.CheckInWithinArea)
		require.InDelta(t, 11120, *got.CheckInDistance, 100)
	})

	t.Run("a project without coordinates is not configured", func(t *testing.T) {
		e := newEnv(t)
		w := e.world()
		e.exec(w.org, "UPDATE projects SET latitude = NULL, longitude = NULL")
		ref := e.onlyShift(w.token).ShiftRef

		e.checkIn(w, ref, siteLat, siteLng).requireFailure(t, http.StatusUnprocessableEntity, "UNPROCESSABLE", organization.ReasonProjectNotConfigured)
	})
}

func TestALateCheckInIsAcceptedAndCountedInMinutes(t *testing.T) {
	// The Day Shift starts at 08:00 Jakarta (01:00 UTC) and its tolerance is 15 minutes.
	t.Run("30 minutes late, even when the regu's old reject toggle is on", func(t *testing.T) {
		e := newEnv(t)
		w := e.world()
		ref := e.onlyShift(w.token).ShiftRef
		e.exec(w.org, "UPDATE teams SET reject_attendance_after_tolerance = true")

		r := e.checkIn(w, ref, siteLat, siteLng) // tuesday is 08:30
		require.Equal(t, http.StatusCreated, r.Status, "body: %s", r.Raw)
		require.Equal(t, 30, data[record](t, r).LateMinutes)
		require.Equal(t, 1, e.outbox(w.org))
	})

	t.Run("inside the tolerance is on time", func(t *testing.T) {
		e := newEnv(t)
		w := e.world()
		ref := e.onlyShift(w.token).ShiftRef
		e.clock.Set(time.Date(2026, 3, 10, 1, 15, 0, 0, time.UTC)) // 08:15, exactly the tolerance

		r := e.checkIn(w, ref, siteLat, siteLng)
		require.Equal(t, http.StatusCreated, r.Status, "body: %s", r.Raw)
		require.Zero(t, data[record](t, r).LateMinutes)
	})

	t.Run("just past the tolerance counts from the start of the shift", func(t *testing.T) {
		e := newEnv(t)
		w := e.world()
		ref := e.onlyShift(w.token).ShiftRef
		e.clock.Set(time.Date(2026, 3, 10, 1, 16, 59, 0, time.UTC)) // 08:16:59

		r := e.checkIn(w, ref, siteLat, siteLng)
		require.Equal(t, http.StatusCreated, r.Status, "body: %s", r.Raw)
		require.Equal(t, 16, data[record](t, r).LateMinutes)
	})
}

func TestPhotosThatAreNotTheCallersAreRefused(t *testing.T) {
	e := newEnv(t)
	w := e.world()
	ref := e.onlyShift(w.token).ShiftRef
	otherID, _ := e.guard(w.org, w.shiftID)

	r := e.api(w.token, http.MethodPost, "/attendances/selfie/check-in", body(ref, e.selfie(w.org, otherID), siteLat, siteLng), e.newKey())
	r.requireFailure(t, http.StatusNotFound, "NOT_FOUND", media.ReasonNotFound)
	require.Zero(t, e.count(w.org, "SELECT count(*) FROM attendances"), "the record rolled back with the photo")
	require.Zero(t, e.outbox(w.org))

	// A photo that does not exist at all is the same answer, not a server error.
	r = e.api(w.token, http.MethodPost, "/attendances/selfie/check-in", body(ref, 987654, siteLat, siteLng), e.newKey())
	r.requireFailure(t, http.StatusNotFound, "NOT_FOUND", media.ReasonNotFound)
}

func TestAShiftRefOfAnotherOrganizationIsRejected(t *testing.T) {
	e := newEnv(t)
	a := e.world()
	b := e.world()
	refOfA := e.onlyShift(a.token).ShiftRef

	// b's guard is scheduled on a shift of their own organization, and holds a's ref.
	r := e.api(b.token, http.MethodPost, "/attendances/selfie/check-in", body(refOfA, e.selfie(b.org, b.personID), siteLat, siteLng), e.newKey())
	r.requireFailure(t, http.StatusUnprocessableEntity, "UNPROCESSABLE", attendance.ReasonShiftNotActive)

	// So is a ref built by hand for a shift that exists nowhere, and junk.
	made := base64.RawURLEncoding.EncodeToString([]byte("1K-IBMI-S1|" + workDate + "|" + strconv.FormatInt(a.shiftID+1000, 10)))
	for _, ref := range []string{made, "not-a-ref", "!!!"} {
		r = e.api(b.token, http.MethodPost, "/attendances/selfie/check-in", body(ref, e.selfie(b.org, b.personID), siteLat, siteLng), e.newKey())
		r.requireFailure(t, http.StatusUnprocessableEntity, "UNPROCESSABLE", attendance.ReasonShiftNotActive)
	}
	require.Zero(t, e.count(b.org, "SELECT count(*) FROM attendances"))
	require.Zero(t, e.count(a.org, "SELECT count(*) FROM attendances"))
}

func TestARequestMustBeValid(t *testing.T) {
	e := newEnv(t)
	w := e.world()
	ref := e.onlyShift(w.token).ShiftRef
	selfie := e.selfie(w.org, w.personID)

	for name, payload := range map[string]map[string]any{
		"no photo":           {"shift_ref": ref, "media_ids": []int64{}, "latitude": siteLat, "longitude": siteLng},
		"two photos":         {"shift_ref": ref, "media_ids": []int64{selfie, selfie + 1}, "latitude": siteLat, "longitude": siteLng},
		"no position":        {"shift_ref": ref, "media_ids": []int64{selfie}},
		"latitude too large": {"shift_ref": ref, "media_ids": []int64{selfie}, "latitude": 91, "longitude": siteLng},
		"no shift":           {"media_ids": []int64{selfie}, "latitude": siteLat, "longitude": siteLng},
	} {
		t.Run(name, func(t *testing.T) {
			r := e.raw(w.token, http.MethodPost, "/attendances/selfie/check-in", payload, e.newKey())
			require.Equal(t, http.StatusUnprocessableEntity, r.Status, "body: %s", r.Raw)
			code, _ := r.failure(t)
			require.Equal(t, "VALIDATION_ERROR", code)
		})
	}
	require.Zero(t, e.count(w.org, "SELECT count(*) FROM attendances"))

	// Not signed in.
	require.Equal(t, http.StatusUnauthorized, e.raw("", http.MethodGet, "/attendances/today", nil, "").Status)
}

func TestCheckOutRules(t *testing.T) {
	e := newEnv(t)
	w := e.world()
	ref := e.onlyShift(w.token).ShiftRef

	e.checkOut(w, ref, siteLat, siteLng).requireFailure(t, http.StatusUnprocessableEntity, "UNPROCESSABLE", attendance.ReasonNotCheckedIn)

	require.Equal(t, http.StatusCreated, e.checkIn(w, ref, siteLat, siteLng).Status)
	require.Equal(t, http.StatusOK, e.checkOut(w, ref, siteLat, siteLng).Status)
	e.checkOut(w, ref, siteLat, siteLng).requireFailure(t, http.StatusConflict, "DUPLICATE", attendance.ReasonAlreadyCheckedOut)
	require.Equal(t, 2, e.outbox(w.org))
}

// previousDay schedules the guard on the Day Shift the day before workDate and records what became
// of it: none (no record: the guard never came), absent, leave, open (checked in, never out) or
// done (checked in and out).
func (e *env) previousDay(w world, outcome string) {
	e.t.Helper()
	const day = "2026-03-09"
	e.exec(w.org, "INSERT INTO schedules (personnel_id, work_date, shift_id) VALUES ($1, $2, $3)", w.personID, day, w.shiftID)
	const insert = `INSERT INTO attendances (personnel_id, team_code, project_code, work_date, shift_id, shift_starts_at, shift_ends_at, timezone, status%s)
VALUES ($1, '1K-IBMI-S1', '1K-IBMI-S', $2, $3, '2026-03-09T01:00:00Z', '2026-03-09T10:00:00Z', 'Asia/Jakarta', $4::attendance_status%s)`
	switch outcome {
	case "none":
	case "ABSENT", "LEAVE":
		e.exec(w.org, fmt.Sprintf(insert, "", ""), w.personID, day, w.shiftID, outcome)
	case "open":
		e.exec(w.org, fmt.Sprintf(insert, ", check_in_at, check_in_method", ", '2026-03-09T01:05:00Z', 'SELFIE'"), w.personID, day, w.shiftID, "PRESENT")
	case "done":
		e.exec(w.org, fmt.Sprintf(insert, ", check_in_at, check_in_method, check_out_at, check_out_method",
			", '2026-03-09T01:05:00Z', 'SELFIE', '2026-03-09T10:00:00Z', 'SELFIE'"), w.personID, day, w.shiftID, "PRESENT")
	default:
		e.t.Fatalf("unknown outcome %q", outcome)
	}
}

// earlierCheckIn records a finished SiAPP check-in on a day long before workDate, with no schedule
// (so it is never the previous working day itself): the guard has a SiAPP history to be judged by.
func (e *env) earlierCheckIn(w world) {
	e.t.Helper()
	e.exec(w.org, `INSERT INTO attendances (personnel_id, team_code, project_code, work_date, shift_id, shift_starts_at, shift_ends_at, timezone, status, check_in_at, check_in_method, check_out_at, check_out_method)
VALUES ($1, '1K-IBMI-S1', '1K-IBMI-S', '2026-03-02', $2, '2026-03-02T01:00:00Z', '2026-03-02T10:00:00Z', 'Asia/Jakarta', 'PRESENT', '2026-03-02T01:05:00Z', 'SELFIE', '2026-03-02T10:00:00Z', 'SELFIE')`,
		w.personID, w.shiftID)
}

func TestACheckInNeedsTheLastWorkingDaySettled(t *testing.T) {
	refused := map[string]string{
		// A day with no record, once the guard has used SiAPP before it.
		"none": attendance.ReasonPreviousNotCheckedIn,
		// A day left open blocks even with no earlier history: the check-in that opened it is SiAPP's.
		"open": attendance.ReasonPreviousNotCheckedOut,
	}
	for outcome, reason := range refused {
		t.Run("refused after "+outcome, func(t *testing.T) {
			e := newEnv(t)
			w := e.world()
			ref := e.onlyShift(w.token).ShiftRef
			if outcome == "none" {
				e.earlierCheckIn(w)
			}
			e.previousDay(w, outcome)

			r := e.checkIn(w, ref, siteLat, siteLng)
			r.requireFailure(t, http.StatusUnprocessableEntity, "UNPROCESSABLE", reason)
			require.Contains(t, string(r.Raw), "2026-03-09")
			require.Zero(t, e.count(w.org, "SELECT count(*) FROM attendances WHERE work_date = '2026-03-10'"), "nothing was written")
			require.Zero(t, e.outbox(w.org))
		})
	}

	// A day someone decided (a leave, an absence marked by the DANRU or an admin) is settled, and so
	// is a finished one; all of them with a SiAPP history, so it is the record that lets them in.
	for _, outcome := range []string{"LEAVE", "ABSENT", "done"} {
		t.Run("allowed after "+outcome, func(t *testing.T) {
			e := newEnv(t)
			w := e.world()
			ref := e.onlyShift(w.token).ShiftRef
			e.earlierCheckIn(w)
			e.previousDay(w, outcome)

			r := e.checkIn(w, ref, siteLat, siteLng)
			require.Equal(t, http.StatusCreated, r.Status, "body: %s", r.Raw)
		})
	}

	t.Run("a missed day before the first SiAPP check-in is not judged", func(t *testing.T) {
		// Go-live, a new hire, a back-dated schedule: the day was never SiAPP's to record.
		e := newEnv(t)
		w := e.world()
		ref := e.onlyShift(w.token).ShiftRef
		e.previousDay(w, "none")

		r := e.checkIn(w, ref, siteLat, siteLng)
		require.Equal(t, http.StatusCreated, r.Status, "body: %s", r.Raw)

		// From then on the guard has a history: the next missed day blocks.
		e.exec(w.org, "INSERT INTO schedules (personnel_id, work_date, shift_id) VALUES ($1, '2026-03-11', $2), ($1, '2026-03-12', $2)", w.personID, w.shiftID)
		e.clock.Set(time.Date(2026, 3, 12, 1, 30, 0, 0, time.UTC))
		e.checkIn(w, e.onlyShift(w.token).ShiftRef, siteLat, siteLng).
			requireFailure(t, http.StatusUnprocessableEntity, "UNPROCESSABLE", attendance.ReasonPreviousNotCheckedIn)
	})

	t.Run("an OFF day in between is not a working day", func(t *testing.T) {
		e := newEnv(t)
		w := e.world()
		ref := e.onlyShift(w.token).ShiftRef
		var off int64
		e.inTenant(w.org, func(tx pgx.Tx) {
			require.NoError(t, tx.QueryRow(t.Context(),
				"INSERT INTO shifts (name, start_minute, end_minute, is_off, tolerance_minutes) VALUES ('OFF', 0, 0, true, 0) RETURNING id").Scan(&off))
		})
		e.earlierCheckIn(w)
		e.previousDay(w, "done")
		e.exec(w.org, "UPDATE schedules SET shift_id = $1 WHERE work_date = '2026-03-09'", off)
		e.exec(w.org, "INSERT INTO schedules (personnel_id, work_date, shift_id) VALUES ($1, '2026-03-08', $2)", w.personID, w.shiftID)
		// 03-09 is OFF, so 03-08 is the last working day, and it has no record.
		e.checkIn(w, ref, siteLat, siteLng).requireFailure(t, http.StatusUnprocessableEntity, "UNPROCESSABLE", attendance.ReasonPreviousNotCheckedIn)
	})

	t.Run("a repeat check-in still says already checked in", func(t *testing.T) {
		e := newEnv(t)
		w := e.world()
		ref := e.onlyShift(w.token).ShiftRef
		e.previousDay(w, "done")
		require.Equal(t, http.StatusCreated, e.checkIn(w, ref, siteLat, siteLng).Status)

		e.exec(w.org, "UPDATE attendances SET check_out_at = NULL, check_out_method = NULL WHERE work_date = '2026-03-09'")
		e.checkIn(w, ref, siteLat, siteLng).requireFailure(t, http.StatusConflict, "DUPLICATE", attendance.ReasonAlreadyCheckedIn)
	})

	t.Run("check-out is never refused for it", func(t *testing.T) {
		e := newEnv(t)
		w := e.world()
		ref := e.onlyShift(w.token).ShiftRef
		e.previousDay(w, "done")
		require.Equal(t, http.StatusCreated, e.checkIn(w, ref, siteLat, siteLng).Status)

		e.exec(w.org, "UPDATE attendances SET check_out_at = NULL, check_out_method = NULL WHERE work_date = '2026-03-09'")
		require.Equal(t, http.StatusOK, e.checkOut(w, ref, siteLat, siteLng).Status)
	})
}

func TestALeaveOrAbsenceIsNotTurnedIntoAPresence(t *testing.T) {
	for _, status := range []string{"LEAVE", "ABSENT"} {
		t.Run(status, func(t *testing.T) {
			e := newEnv(t)
			w := e.world()
			ref := e.onlyShift(w.token).ShiftRef
			e.exec(w.org, `INSERT INTO attendances (personnel_id, team_code, project_code, work_date, shift_id, shift_starts_at, shift_ends_at, timezone, status)
VALUES ($1, '1K-IBMI-S1', '1K-IBMI-S', $2, $3, '2026-03-10T01:00:00Z', '2026-03-10T10:00:00Z', 'Asia/Jakarta', $4::attendance_status)`,
				w.personID, workDate, w.shiftID, status)

			require.Equal(t, status, *e.onlyShift(w.token).Status)
			e.checkIn(w, ref, siteLat, siteLng).requireFailure(t, http.StatusUnprocessableEntity, "UNPROCESSABLE", attendance.ReasonNotAllowed)
			e.checkOut(w, ref, siteLat, siteLng).requireFailure(t, http.StatusUnprocessableEntity, "UNPROCESSABLE", attendance.ReasonNotCheckedIn)
			require.Zero(t, e.outbox(w.org))
		})
	}
}

func TestUpdatingACheckOutHasItsOwnRules(t *testing.T) {
	e := newEnv(t)
	w := e.world()
	ref := e.onlyShift(w.token).ShiftRef
	id := data[record](t, e.checkIn(w, ref, siteLat, siteLng)).ID
	patch := func(token string, id int64) reply {
		return e.api(token, http.MethodPatch, fmt.Sprintf("/attendances/%d/check-out", id),
			map[string]any{"media_ids": []int64{e.selfie(w.org, w.personID)}, "latitude": siteLat, "longitude": siteLng}, e.newKey())
	}

	// No check-out to update yet.
	patch(w.token, id).requireFailure(t, http.StatusUnprocessableEntity, "UNPROCESSABLE", attendance.ReasonNotCheckedOut)
	require.Equal(t, http.StatusOK, e.checkOut(w, ref, siteLat, siteLng).Status)

	// Another guard of the regu cannot touch it, and it does not exist for an id that is not there.
	_, other := e.guard(w.org, w.shiftID)
	patch(other, id).requireFailure(t, http.StatusNotFound, "NOT_FOUND", attendance.ReasonNotFound)
	patch(w.token, id+1000).requireFailure(t, http.StatusNotFound, "NOT_FOUND", attendance.ReasonNotFound)
	// Nor can another organization's guard.
	foreign := e.world()
	patch(foreign.token, id).requireFailure(t, http.StatusNotFound, "NOT_FOUND", attendance.ReasonNotFound)
	require.Equal(t, 2, e.outbox(w.org))

	// Once the shift's check-out window has closed the record is final.
	e.clock.Set(time.Date(2026, 3, 10, 14, 1, 0, 0, time.UTC))
	patch(w.token, id).requireFailure(t, http.StatusUnprocessableEntity, "UNPROCESSABLE", attendance.ReasonShiftNotActive)
	require.Equal(t, 2, e.outbox(w.org))
}

func TestUpdatingACheckOutIsIdempotent(t *testing.T) {
	e := newEnv(t)
	w := e.world()
	ref := e.onlyShift(w.token).ShiftRef
	id := data[record](t, e.checkIn(w, ref, siteLat, siteLng)).ID
	require.Equal(t, http.StatusOK, e.checkOut(w, ref, siteLat, siteLng).Status)
	path := fmt.Sprintf("/attendances/%d/check-out", id)
	payload := map[string]any{"media_ids": []int64{e.selfie(w.org, w.personID)}, "latitude": siteLat, "longitude": siteLng}

	// Without a key the request is refused before anything is written.
	require.Equal(t, http.StatusUnprocessableEntity, e.raw(w.token, http.MethodPatch, path, payload, "").Status)
	require.Equal(t, 2, e.outbox(w.org))
	require.Zero(t, e.count(w.org, "SELECT check_out_update_count FROM attendances"))

	key := e.newKey()
	first := e.api(w.token, http.MethodPatch, path, payload, key)
	require.Equal(t, http.StatusOK, first.Status, "body: %s", first.Raw)
	again := e.api(w.token, http.MethodPatch, path, payload, key)
	require.Equal(t, http.StatusOK, again.Status, "body: %s", again.Raw)
	require.JSONEq(t, string(first.Raw), string(again.Raw))
	require.Equal(t, 1, data[record](t, again).CheckOutUpdateCount)
	require.Equal(t, []string{"CHECK_IN", "CHECK_OUT", "CHECK_OUT_UPDATED"}, e.outboxTypes(w.org))
}

func TestTheAnswersComeInOrder(t *testing.T) {
	t.Run("a repeat check-in from outside a strict geofence is ALREADY_CHECKED_IN", func(t *testing.T) {
		e := newEnv(t)
		w := e.world()
		ref := e.onlyShift(w.token).ShiftRef
		require.Equal(t, http.StatusCreated, e.checkIn(w, ref, siteLat, siteLng).Status)
		e.exec(w.org, "UPDATE projects SET strict_attendance_geofence = true")

		e.checkIn(w, ref, farLat, siteLng).requireFailure(t, http.StatusConflict, "DUPLICATE", attendance.ReasonAlreadyCheckedIn)
	})

	t.Run("a repeat check-out from outside a strict geofence is ALREADY_CHECKED_OUT", func(t *testing.T) {
		e := newEnv(t)
		w := e.world()
		ref := e.onlyShift(w.token).ShiftRef
		require.Equal(t, http.StatusCreated, e.checkIn(w, ref, siteLat, siteLng).Status)
		require.Equal(t, http.StatusOK, e.checkOut(w, ref, siteLat, siteLng).Status)
		e.exec(w.org, "UPDATE projects SET strict_attendance_geofence = true")

		e.checkOut(w, ref, farLat, siteLng).requireFailure(t, http.StatusConflict, "DUPLICATE", attendance.ReasonAlreadyCheckedOut)
	})
}

func TestThePreviousDayIsJudgedAsAWholeDay(t *testing.T) {
	t.Run("a closed record of another shift settles it", func(t *testing.T) {
		e := newEnv(t)
		w := e.world()
		ref := e.onlyShift(w.token).ShiftRef
		var other int64
		e.inTenant(w.org, func(tx pgx.Tx) {
			require.NoError(t, tx.QueryRow(t.Context(),
				"INSERT INTO shifts (name, start_minute, end_minute, tolerance_minutes) VALUES ('Early', 420, 960, 15) RETURNING id").Scan(&other))
		})
		e.previousDay(w, "done")
		// The schedule was moved to another shift after the guard worked the first one.
		e.exec(w.org, "UPDATE schedules SET shift_id = $1 WHERE work_date = '2026-03-09'", other)

		require.Equal(t, http.StatusCreated, e.checkIn(w, ref, siteLat, siteLng).Status)
	})

	t.Run("one regu left open is open even when another was closed", func(t *testing.T) {
		e := newEnv(t)
		w := e.world()
		ref := e.onlyShift(w.token).ShiftRef
		e.exec(w.org, "INSERT INTO teams (code, project_code, description) VALUES ('1K-IBMI-S2', '1K-IBMI-S', 'Regu 2')")
		e.previousDay(w, "done")
		e.exec(w.org, `INSERT INTO attendances (personnel_id, team_code, project_code, work_date, shift_id, shift_starts_at, shift_ends_at, timezone, status, check_in_at, check_in_method)
VALUES ($1, '1K-IBMI-S2', '1K-IBMI-S', '2026-03-09', $2, '2026-03-09T01:00:00Z', '2026-03-09T10:00:00Z', 'Asia/Jakarta', 'PRESENT', '2026-03-09T01:05:00Z', 'SELFIE')`,
			w.personID, w.shiftID)

		e.checkIn(w, ref, siteLat, siteLng).requireFailure(t, http.StatusUnprocessableEntity, "UNPROCESSABLE", attendance.ReasonPreviousNotCheckedOut)
	})
}
