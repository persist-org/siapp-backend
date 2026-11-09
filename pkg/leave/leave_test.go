package leave_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"

	"siapp/internal/db"
	"siapp/pkg/access"
	"siapp/pkg/leave"
	"siapp/pkg/media"
	"siapp/pkg/organization"
	"siapp/pkg/platform/auth"
	"siapp/pkg/platform/database"
	"siapp/pkg/platform/idem"
	"siapp/pkg/platform/testkit"
)

func TestMain(m *testing.M) { os.Exit(testkit.NewDatabase(m)) }

// A Tuesday, 08:30 in Jakarta.
var tuesday = time.Date(2026, 3, 10, 1, 30, 0, 0, time.UTC)

type testClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *testClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// fakeRegions is the lookup of a person's regions: the test says which person is under which.
type fakeRegions map[int64][]string

func (f fakeRegions) RegionCodesOf(_ context.Context, _ *db.Queries, personnelID int64) ([]string, error) {
	return f[personnelID], nil
}

type env struct {
	t       *testing.T
	srv     string
	client  *testkit.Client
	clock   *testClock
	service *leave.Service
	regions fakeRegions
	userID  int64
	keys    int
	mu      sync.Mutex
}

// presigner gives a photo a URL without a storage behind it.
type presigner struct{ media.Storage }

func (presigner) PresignGet(_ context.Context, key, _ string, _ time.Duration) (string, error) {
	return "https://files.test/" + key + "?sig=x", nil
}

// newEnv serves the admin group (RequireAdmin and the leave admin routes) and the field routes on
// a clean database.
func newEnv(t *testing.T) *env {
	t.Helper()
	testkit.Reset(t)
	pool := testkit.Pool(t)
	clk := &testClock{now: tuesday}
	e := &env{t: t, clock: clk, regions: fakeRegions{}}
	perms := access.NewService(pool, clk)
	e.service = leave.NewService(pool, clk)
	e.service.UseRegionLookup(e.regions)
	e.service.UseSchedule(organization.LeaveSchedule{Shifts: organization.NewShifts(pool)})
	e.service.UseAttachments(media.NewService(pool, presigner{}, clk, nil))
	e.service.UsePermissions(perms)
	h := leave.NewHandler(e.service, perms, idem.New(idem.Config{Pool: pool, Identity: auth.Identity}).Middleware(true))
	srv := testkit.NewServer(t, testkit.Deps{Mount: func(r chi.Router) {
		r.Route("/api/v1/admin", func(admin chi.Router) {
			admin.Use(access.RequireAdmin(pool))
			h.MountAdmin(admin)
		})
		h.MountField(r)
	}})
	e.srv, e.client = srv.URL, testkit.NewClient(t, srv)
	return e
}

type reply struct {
	Status int
	Raw    []byte
}

func (r reply) requireFailure(t *testing.T, status int, code, reason string) {
	t.Helper()
	require.Equal(t, status, r.Status, "body: %s", r.Raw)
	var e struct {
		Error struct {
			Code    string         `json:"code"`
			Details map[string]any `json:"details"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal(r.Raw, &e), "body: %s", r.Raw)
	require.Equal(t, code, e.Error.Code)
	got, _ := e.Error.Details["reason"].(string)
	require.Equal(t, reason, got)
}

func data[T any](t *testing.T, r reply) T {
	t.Helper()
	var env struct {
		Data T `json:"data"`
	}
	require.NoError(t, json.Unmarshal(r.Raw, &env), "body: %s", r.Raw)
	return env.Data
}

// api is a request checked against the contract; raw is not.
func (e *env) api(token, method, path string, body any) reply {
	e.t.Helper()
	e.client.Token = token
	defer func() { e.client.Token = "" }()
	res := e.client.Do(method, path, body)
	return reply{Status: res.Status, Raw: res.Body}
}

func (e *env) raw(token, method, path string, body any) reply {
	e.t.Helper()
	return e.rawHeaders(token, method, path, body, nil)
}

// rawHeaders is raw with extra request headers, such as Idempotency-Key.
func (e *env) rawHeaders(token, method, path string, body any, headers map[string]string) reply {
	e.t.Helper()
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		require.NoError(e.t, err)
		reader = bytes.NewReader(encoded)
	}
	req, err := http.NewRequestWithContext(e.t.Context(), method, e.srv+"/api/v1"+path, reader)
	require.NoError(e.t, err)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for name, value := range headers {
		req.Header.Set(name, value)
	}
	res, err := http.DefaultClient.Do(req)
	require.NoError(e.t, err)
	defer func() { _ = res.Body.Close() }()
	out, err := io.ReadAll(res.Body)
	require.NoError(e.t, err)
	return reply{Status: res.StatusCode, Raw: out}
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

// person adds a person and returns their id and token. A web role ("" for none) makes them an
// admin; a REGION_ADMIN is under OS1.
func (e *env) person(org int64, role string) (int64, string) {
	e.t.Helper()
	e.userID++
	var id int64
	e.inTenant(org, func(tx pgx.Tx) {
		require.NoError(e.t, tx.QueryRow(e.t.Context(),
			"INSERT INTO personnels (personel_user_id, full_name, status) VALUES ($1, $2, 'ACTIVE') RETURNING id",
			e.userID, "Orang "+strconv.FormatInt(e.userID, 10)).Scan(&id))
		switch role {
		case "ORG_ADMIN":
			_, err := tx.Exec(e.t.Context(), "INSERT INTO web_role_assignments (personnel_id, role) VALUES ($1, 'ORG_ADMIN')", id)
			require.NoError(e.t, err)
		case "REGION_ADMIN":
			_, err := tx.Exec(e.t.Context(), "INSERT INTO regions (code, description) VALUES ('OS1', 'Outsourcing 1') ON CONFLICT DO NOTHING")
			require.NoError(e.t, err)
			_, err = tx.Exec(e.t.Context(), "INSERT INTO web_role_assignments (personnel_id, role, region_code) VALUES ($1, 'REGION_ADMIN', 'OS1')", id)
			require.NoError(e.t, err)
		}
	})
	return id, testkit.Token(e.t, org, id)
}

// world is an organization with its defaults, an ORG_ADMIN, two clients each with a project and a
// regu, and a guard (MEMBER) in the first regu.
type world struct {
	org                 int64
	admin               string
	guardID             int64
	guard               string
	danruID             int64
	danru               string
	team, otherTeam     string
	client, otherClient string
}

func (e *env) world() world {
	e.t.Helper()
	org := testkit.NewOrg(e.t)
	_, err := access.EnsureDefaults(e.t.Context(), testkit.Pool(e.t), e.clock, org)
	require.NoError(e.t, err)
	_, err = leave.EnsureDefaults(e.t.Context(), testkit.Pool(e.t), e.clock, org)
	require.NoError(e.t, err)
	w := world{org: org, team: "1K-IBMI-S1", otherTeam: "1K-OTHR-S1", client: "1K-IBMI", otherClient: "1K-OTHR"}
	e.exec(org, `
INSERT INTO regions (code, description) VALUES ('OS1', 'Outsourcing 1') ON CONFLICT DO NOTHING;
INSERT INTO branches (code, region_code, company_letter, description) VALUES ('1K', 'OS1', 'K', 'Cabang 1K');
INSERT INTO clients (code, branch_code, description, company_prefix) VALUES ('1K-IBMI', '1K', 'IBMI', 'IBMI'), ('1K-OTHR', '1K', 'Lain', 'OTHR');
INSERT INTO projects (code, client_code, description, business_line, timezone, latitude, longitude)
VALUES ('1K-IBMI-S', '1K-IBMI', 'Proyek IBMI', 'security', 'Asia/Jakarta', -6.2, 106.8),
       ('1K-OTHR-S', '1K-OTHR', 'Proyek Lain', 'security', 'Asia/Jakarta', -6.2, 106.8);
INSERT INTO teams (code, project_code, description) VALUES ('1K-IBMI-S1', '1K-IBMI-S', 'Regu 1'), ('1K-OTHR-S1', '1K-OTHR-S', 'Regu Lain');`)
	_, w.admin = e.person(org, "ORG_ADMIN")
	w.guardID, w.guard = e.person(org, "")
	e.exec(org, "INSERT INTO team_members (team_code, personnel_id, role, start_date) VALUES ('1K-IBMI-S1', $1, 'MEMBER', '2000-01-01')", w.guardID)
	w.danruID, w.danru = e.person(org, "")
	e.exec(org, "UPDATE personnels SET full_name = 'Pak Danru' WHERE id = $1", w.danruID)
	e.exec(org, "INSERT INTO team_members (team_code, personnel_id, role, start_date) VALUES ('1K-IBMI-S1', $1, 'DANRU', '2000-01-01')", w.danruID)
	// A delta pull reaches back a few seconds, so later pulls must start well after the seed.
	e.clock.Advance(time.Minute)
	return w
}

type category struct {
	Code              string `json:"code"`
	Name              string `json:"name"`
	Type              string `json:"type"`
	IsAnnual          bool   `json:"is_annual"`
	SkipsDayCheck     bool   `json:"skips_day_check"`
	RequiresTimeRange bool   `json:"requires_time_range"`
	RequiresChangeDay bool   `json:"requires_change_day"`
	MakesLeaveDays    bool   `json:"makes_leave_days"`
	ExcusesLateness   bool   `json:"excuses_lateness"`
	HasQuota          bool   `json:"has_quota"`
	IsActive          bool   `json:"is_active"`
}

type quotas struct {
	DefaultDays *int `json:"default_days"`
	Clients     []struct {
		ClientCode string `json:"client_code"`
		QuotaDays  int    `json:"quota_days"`
	} `json:"clients"`
}

type allowance struct {
	CategoryCode  string `json:"category_code"`
	QuotaDays     int    `json:"quota_days"`
	UsedDays      int    `json:"used_days"`
	RemainingDays int    `json:"remaining_days"`
}

type ledgerEntry struct {
	ID           int64  `json:"id"`
	Kind         string `json:"kind"`
	Days         int    `json:"days"`
	BalanceAfter int    `json:"balance_after"`
	Note         string `json:"note"`
}

func (e *env) categories(token, query string) []category {
	e.t.Helper()
	r := e.api(token, http.MethodGet, "/admin/permission-categories"+query, nil)
	require.Equal(e.t, http.StatusOK, r.Status, "body: %s", r.Raw)
	return data[[]category](e.t, r)
}

func byCode(cs []category) map[string]category {
	out := map[string]category{}
	for _, c := range cs {
		out[c.Code] = c
	}
	return out
}

func (e *env) quota(token, category, team string) reply {
	e.t.Helper()
	path := "/permissions/quota?category_code=" + category
	if team != "" {
		path += "&team_code=" + team
	}
	return e.api(token, http.MethodGet, path, nil)
}

func (e *env) grant(w world, personnelID int64, days int) reply {
	e.t.Helper()
	return e.api(w.admin, http.MethodPost, "/admin/personnel/"+strconv.FormatInt(personnelID, 10)+"/leave-ledger",
		map[string]any{"kind": "GRANT", "days": days, "note": "Hak tahunan"})
}

func (e *env) setQuotas(w world, category string, body map[string]any) reply {
	e.t.Helper()
	return e.api(w.admin, http.MethodPut, "/admin/permission-categories/"+category+"/quotas", body)
}

func since(t time.Time) string { return "?since=" + url.QueryEscape(t.UTC().Format(time.RFC3339)) }

// ---------- categories ----------

func TestDefaultsAreSeededOnceWithTheRightFlags(t *testing.T) {
	e := newEnv(t)
	w := e.world()

	added, err := leave.EnsureDefaults(t.Context(), testkit.Pool(t), e.clock, w.org)
	require.NoError(t, err)
	require.Zero(t, added, "a second call adds nothing")

	all := e.categories(w.admin, "")
	require.Len(t, all, 17)
	got := byCode(all)

	require.Equal(t, category{Code: "ANNUAL", Name: "Cuti Tahunan", Type: "LEAVE", IsAnnual: true, MakesLeaveDays: true, HasQuota: true, IsActive: true}, got["ANNUAL"])
	require.True(t, got["MATERNITY"].SkipsDayCheck)
	require.True(t, got["OTHER_PART_DAY"].RequiresTimeRange)
	require.False(t, got["OTHER_PART_DAY"].MakesLeaveDays, "someone on a part-of-day permit can still check in")
	require.True(t, got["ON_DUTY_PART_DAY"].RequiresTimeRange)
	require.True(t, got["ON_DUTY_FULL_DAY"].ExcusesLateness)
	require.False(t, got["ON_DUTY_FULL_DAY"].MakesLeaveDays)
	require.True(t, got["CHANGE_DAY"].RequiresChangeDay)
	require.Equal(t, "SICK", got["SICK"].Type)
	require.False(t, got["SICK"].HasQuota)
	require.Len(t, e.categories(w.admin, "?type=SICK"), 1)
	require.Len(t, e.categories(w.admin, "?type=LEAVE"), 2)

	// Another organization has its own list: an edit in one does not reach the other.
	other := testkit.NewOrg(t)
	_, err = leave.EnsureDefaults(t.Context(), testkit.Pool(t), e.clock, other)
	require.NoError(t, err)
	require.Equal(t, 17, e.count(other, "SELECT count(*) FROM leave_categories"))
	require.Equal(t, 17, e.count(w.org, "SELECT count(*) FROM leave_categories"))
}

func TestAnAdminCreatesAndEditsCategories(t *testing.T) {
	e := newEnv(t)
	w := e.world()

	r := e.api(w.admin, http.MethodPost, "/admin/permission-categories", map[string]any{
		"code": "STUDY", "name": "Cuti Belajar", "type": "PERMIT",
	})
	require.Equal(t, http.StatusCreated, r.Status, "body: %s", r.Raw)
	created := data[category](t, r)
	require.True(t, created.MakesLeaveDays, "a category makes leave days unless it says not to")
	require.True(t, created.HasQuota)
	require.True(t, created.IsActive)

	// A code is taken for good, and has a shape.
	e.api(w.admin, http.MethodPost, "/admin/permission-categories", map[string]any{"code": "STUDY", "name": "Lagi", "type": "PERMIT"}).
		requireFailure(t, http.StatusConflict, "DUPLICATE", leave.ReasonCategoryCodeTaken)
	require.Equal(t, http.StatusUnprocessableEntity, e.raw(w.admin, http.MethodPost, "/admin/permission-categories",
		map[string]any{"code": "study", "name": "x", "type": "PERMIT"}).Status)

	// Flags that contradict each other are refused, whichever way they are set.
	for name, body := range map[string]map[string]any{
		"annual sick":          {"code": "A1", "name": "x", "type": "SICK", "is_annual": true},
		"annual permit":        {"code": "A2", "name": "x", "type": "PERMIT", "is_annual": true},
		"annual no quota":      {"code": "A3", "name": "x", "type": "LEAVE", "is_annual": true, "has_quota": false},
		"sick with quota":      {"code": "S1", "name": "x", "type": "SICK", "has_quota": true},
		"times and change day": {"code": "T1", "name": "x", "type": "PERMIT", "requires_time_range": true, "requires_change_day": true},
	} {
		require.Equal(t, http.StatusUnprocessableEntity, e.raw(w.admin, http.MethodPost, "/admin/permission-categories", body).Status, name)
	}

	// The same on a patch, judged on what the row would become.
	require.Equal(t, http.StatusUnprocessableEntity, e.raw(w.admin, http.MethodPatch, "/admin/permission-categories/ANNUAL", map[string]any{"has_quota": false}).Status)
	require.Equal(t, http.StatusUnprocessableEntity, e.raw(w.admin, http.MethodPatch, "/admin/permission-categories/SICK", map[string]any{"has_quota": true}).Status)

	// The code, the type and is_annual never change.
	for _, field := range []string{"code", "type", "is_annual"} {
		require.Equal(t, http.StatusUnprocessableEntity, e.raw(w.admin, http.MethodPatch, "/admin/permission-categories/STUDY",
			map[string]any{field: "x"}).Status, field)
	}

	r = e.api(w.admin, http.MethodPatch, "/admin/permission-categories/STUDY", map[string]any{"name": "Cuti Studi", "excuses_lateness": true})
	require.Equal(t, http.StatusOK, r.Status, "body: %s", r.Raw)
	require.Equal(t, "Cuti Studi", data[category](t, r).Name)
	e.api(w.admin, http.MethodPatch, "/admin/permission-categories/NOPE", map[string]any{"name": "x"}).
		requireFailure(t, http.StatusNotFound, "NOT_FOUND", leave.ReasonCategoryNotFound)

	require.Equal(t, 2, e.count(w.org, "SELECT count(*) FROM audit_logs WHERE action LIKE 'leave.category.%'"), "create and update are audited")
}

func TestOnlyAnOrgAdminManagesCategories(t *testing.T) {
	e := newEnv(t)
	w := e.world()
	_, regionAdmin := e.person(w.org, "REGION_ADMIN")

	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/admin/permission-categories"},
		{http.MethodPost, "/admin/permission-categories"},
		{http.MethodGet, "/admin/permission-categories/ANNUAL"},
		{http.MethodPatch, "/admin/permission-categories/ANNUAL"},
		{http.MethodPut, "/admin/permission-categories/UNPAID/quotas"},
		{http.MethodPut, "/admin/leave-client-settings/1K-IBMI"},
	} {
		body := map[string]any{"name": "x", "code": "XX", "type": "LEAVE", "requests_enabled": true}
		require.Equal(t, http.StatusForbidden, e.raw(w.guard, tc.method, tc.path, body).Status, "%s %s as field staff", tc.method, tc.path)
		require.Equal(t, http.StatusForbidden, e.raw(regionAdmin, tc.method, tc.path, body).Status, "%s %s as REGION_ADMIN", tc.method, tc.path)
	}
}

func TestTheFieldSeesActiveCategoriesAndTheirChanges(t *testing.T) {
	e := newEnv(t)
	w := e.world()

	r := e.api(w.guard, http.MethodGet, "/permission-categories", nil)
	require.Equal(t, http.StatusOK, r.Status, "body: %s", r.Raw)
	require.Len(t, data[[]category](t, r), 17)
	require.Len(t, data[[]category](t, e.api(w.guard, http.MethodGet, "/permission-categories?type=PERMIT", nil)), 14)
	require.Equal(t, http.StatusUnprocessableEntity, e.raw(w.guard, http.MethodGet, "/permission-categories?type=NOPE", nil).Status)
	require.Equal(t, http.StatusUnauthorized, e.raw("", http.MethodGet, "/permission-categories", nil).Status)

	var first struct {
		Meta struct {
			ServerTime time.Time `json:"server_time"`
		} `json:"meta"`
	}
	require.NoError(t, json.Unmarshal(e.api(w.guard, http.MethodGet, "/permission-categories", nil).Raw, &first))
	watermark := first.Meta.ServerTime

	// A deactivated category leaves the full list and arrives as a tombstone, by code.
	e.clock.Advance(time.Minute)
	require.Equal(t, http.StatusOK, e.api(w.admin, http.MethodPatch, "/admin/permission-categories/SPECIAL", map[string]any{"is_active": false}).Status)
	require.Len(t, data[[]category](t, e.api(w.guard, http.MethodGet, "/permission-categories", nil)), 16)

	var d struct {
		Data    []category `json:"data"`
		Deleted []struct {
			Code string `json:"code"`
		} `json:"deleted"`
	}
	r = e.api(w.guard, http.MethodGet, "/permission-categories"+since(watermark), nil)
	require.Equal(t, http.StatusOK, r.Status, "body: %s", r.Raw)
	require.NoError(t, json.Unmarshal(r.Raw, &d))
	require.Empty(t, d.Data)
	require.Len(t, d.Deleted, 1)
	require.Equal(t, "SPECIAL", d.Deleted[0].Code)

	// The two changes the test makes are told apart: one tombstone, one row to upsert.
	require.Equal(t, http.StatusOK, e.api(w.admin, http.MethodPatch, "/admin/permission-categories/MARRIAGE", map[string]any{"name": "Cuti Nikah"}).Status)
	r = e.api(w.guard, http.MethodGet, "/permission-categories"+since(watermark), nil)
	require.NoError(t, json.Unmarshal(r.Raw, &d))
	require.Len(t, d.Data, 1)
	require.Equal(t, "Cuti Nikah", d.Data[0].Name)
}

// ---------- quota ----------

func TestAQuotaIsTheClientsRowThenTheDefaultThenZero(t *testing.T) {
	e := newEnv(t)
	w := e.world()
	otherGuardID, otherGuard := e.person(w.org, "")
	e.exec(w.org, "INSERT INTO team_members (team_code, personnel_id, role, start_date) VALUES ('1K-OTHR-S1', $1, 'MEMBER', '2000-01-01')", otherGuardID)

	// No row at all is 0 days: the category cannot be requested until HR sets a quota.
	r := e.quota(w.guard, "UNPAID", "")
	require.Equal(t, http.StatusOK, r.Status, "body: %s", r.Raw)
	require.Equal(t, allowance{CategoryCode: "UNPAID"}, data[allowance](t, r))

	r = e.setQuotas(w, "UNPAID", map[string]any{"default_days": 5, "clients": []map[string]any{{"client_code": "1K-IBMI", "quota_days": 2}}})
	require.Equal(t, http.StatusOK, r.Status, "body: %s", r.Raw)
	got := data[quotas](t, r)
	require.Equal(t, 5, *got.DefaultDays)
	require.Len(t, got.Clients, 1)

	require.Equal(t, allowance{CategoryCode: "UNPAID", QuotaDays: 2, RemainingDays: 2}, data[allowance](t, e.quota(w.guard, "UNPAID", "")), "the client's own row wins")
	require.Equal(t, allowance{CategoryCode: "UNPAID", QuotaDays: 5, RemainingDays: 5}, data[allowance](t, e.quota(otherGuard, "UNPAID", "")), "a client without a row has the default")
	require.Equal(t, 2, data[allowance](t, e.quota(w.guard, "UNPAID", w.team)).QuotaDays)

	// PUT replaces: what is not sent is gone.
	r = e.setQuotas(w, "UNPAID", map[string]any{"clients": []map[string]any{{"client_code": "1K-OTHR", "quota_days": 9}}})
	require.Equal(t, http.StatusOK, r.Status, "body: %s", r.Raw)
	require.Nil(t, data[quotas](t, r).DefaultDays)
	require.Zero(t, data[allowance](t, e.quota(w.guard, "UNPAID", "")).QuotaDays)
	require.Equal(t, 9, data[allowance](t, e.quota(otherGuard, "UNPAID", "")).QuotaDays)
	detail := data[struct {
		category
		Quotas quotas `json:"quotas"`
	}](t, e.api(w.admin, http.MethodGet, "/admin/permission-categories/UNPAID", nil))
	require.Len(t, detail.Quotas.Clients, 1)

	// A client that is switched off has no days at all; one with no setting is on.
	require.Equal(t, http.StatusOK, e.api(w.admin, http.MethodPut, "/admin/leave-client-settings/1K-OTHR", map[string]any{"requests_enabled": false}).Status)
	require.Zero(t, data[allowance](t, e.quota(otherGuard, "UNPAID", "")).QuotaDays)
	require.Equal(t, http.StatusOK, e.api(w.admin, http.MethodPut, "/admin/leave-client-settings/1K-OTHR", map[string]any{"requests_enabled": true}).Status)
	require.Equal(t, 9, data[allowance](t, e.quota(otherGuard, "UNPAID", "")).QuotaDays)
	e.api(w.admin, http.MethodPut, "/admin/leave-client-settings/NOPE", map[string]any{"requests_enabled": false}).
		requireFailure(t, http.StatusNotFound, "NOT_FOUND", leave.ReasonClientNotFound)
}

func TestQuotasRefuseWhatHasNone(t *testing.T) {
	e := newEnv(t)
	w := e.world()

	// Annual leave is limited by the ledger and sick leave has no quota.
	for _, code := range []string{"ANNUAL", "SICK"} {
		e.setQuotas(w, code, map[string]any{"default_days": 3}).requireFailure(t, http.StatusUnprocessableEntity, "UNPROCESSABLE", leave.ReasonCategoryInvalid)
	}
	e.quota(w.guard, "SICK", "").requireFailure(t, http.StatusUnprocessableEntity, "UNPROCESSABLE", leave.ReasonCategoryInvalid)
	e.setQuotas(w, "NOPE", map[string]any{"default_days": 3}).requireFailure(t, http.StatusNotFound, "NOT_FOUND", leave.ReasonCategoryNotFound)
	e.quota(w.guard, "NOPE", "").requireFailure(t, http.StatusNotFound, "NOT_FOUND", leave.ReasonCategoryNotFound)

	for name, body := range map[string]map[string]any{
		"unknown client":   {"clients": []map[string]any{{"client_code": "NOPE", "quota_days": 1}}},
		"duplicate client": {"clients": []map[string]any{{"client_code": "1K-IBMI", "quota_days": 1}, {"client_code": "1K-IBMI", "quota_days": 2}}},
		"negative":         {"default_days": -1},
		"too many days":    {"default_days": 400},
	} {
		require.Equal(t, http.StatusUnprocessableEntity, e.raw(w.admin, http.MethodPut, "/admin/permission-categories/UNPAID/quotas", body).Status, name)
	}
	require.Zero(t, e.count(w.org, "SELECT count(*) FROM leave_category_quotas"), "a refused PUT keeps what was there")
}

func TestTheQuotaIsAskedInARegu(t *testing.T) {
	e := newEnv(t)
	w := e.world()

	require.Equal(t, http.StatusUnprocessableEntity, e.raw(w.guard, http.MethodGet, "/permissions/quota", nil).Status, "category_code is required")

	// Someone in no regu has no permission to ask; naming a regu one does not serve in is refused too.
	_, stranger := e.person(w.org, "")
	require.Equal(t, http.StatusForbidden, e.raw(stranger, http.MethodGet, "/permissions/quota?category_code=ANNUAL", nil).Status)
	require.Equal(t, http.StatusForbidden, e.raw(w.guard, http.MethodGet, "/permissions/quota?category_code=ANNUAL&team_code="+w.otherTeam, nil).Status)

	// Two regu: the person has to say which one.
	e.exec(w.org, "INSERT INTO team_members (team_code, personnel_id, role, start_date) VALUES ('1K-OTHR-S1', $1, 'MEMBER', '2000-01-01')", w.guardID)
	require.Equal(t, http.StatusUnprocessableEntity, e.raw(w.guard, http.MethodGet, "/permissions/quota?category_code=ANNUAL", nil).Status)
	require.Equal(t, http.StatusOK, e.raw(w.guard, http.MethodGet, "/permissions/quota?category_code=ANNUAL&team_code="+w.otherTeam, nil).Status)
	require.Equal(t, http.StatusUnauthorized, e.raw("", http.MethodGet, "/permissions/quota?category_code=ANNUAL", nil).Status)
}

// ---------- ledger ----------

func TestTheLedgerBalanceFollowsGrantsAndAdjustments(t *testing.T) {
	e := newEnv(t)
	w := e.world()

	require.Zero(t, data[allowance](t, e.quota(w.guard, "ANNUAL", "")).QuotaDays, "no grant, no balance")

	r := e.grant(w, w.guardID, 12)
	require.Equal(t, http.StatusCreated, r.Status, "body: %s", r.Raw)
	require.Equal(t, ledgerEntry{ID: data[ledgerEntry](t, r).ID, Kind: "GRANT", Days: 12, BalanceAfter: 12, Note: "Hak tahunan"}, data[ledgerEntry](t, r))
	require.Equal(t, allowance{CategoryCode: "ANNUAL", QuotaDays: 12, RemainingDays: 12}, data[allowance](t, e.quota(w.guard, "ANNUAL", "")))

	r = e.api(w.admin, http.MethodPost, "/admin/personnel/"+strconv.FormatInt(w.guardID, 10)+"/leave-ledger", map[string]any{"kind": "ADJUST", "days": -2, "note": "Koreksi"})
	require.Equal(t, http.StatusCreated, r.Status, "body: %s", r.Raw)
	require.Equal(t, 10, data[ledgerEntry](t, r).BalanceAfter)

	// The balance never goes below 0, and a refused change writes nothing.
	e.api(w.admin, http.MethodPost, "/admin/personnel/"+strconv.FormatInt(w.guardID, 10)+"/leave-ledger", map[string]any{"kind": "ADJUST", "days": -11, "note": "Terlalu banyak"}).
		requireFailure(t, http.StatusUnprocessableEntity, "UNPROCESSABLE", leave.ReasonBalanceInsufficient)
	require.Equal(t, 2, e.count(w.org, "SELECT count(*) FROM leave_ledger"))

	for name, body := range map[string]map[string]any{
		"grant of nothing":  {"kind": "GRANT", "days": 0, "note": "x"},
		"negative grant":    {"kind": "GRANT", "days": -1, "note": "x"},
		"adjust of nothing": {"kind": "ADJUST", "days": 0, "note": "x"},
		"no note":           {"kind": "GRANT", "days": 1, "note": ""},
		"other kind":        {"kind": "DEDUCT", "days": -1, "note": "x"},
	} {
		require.Equal(t, http.StatusUnprocessableEntity, e.raw(w.admin, http.MethodPost, "/admin/personnel/"+strconv.FormatInt(w.guardID, 10)+"/leave-ledger", body).Status, name)
	}

	// Newest first, and the first row's balance is the person's balance.
	r = e.api(w.admin, http.MethodGet, "/admin/personnel/"+strconv.FormatInt(w.guardID, 10)+"/leave-ledger", nil)
	require.Equal(t, http.StatusOK, r.Status, "body: %s", r.Raw)
	rows := data[[]ledgerEntry](t, r)
	require.Len(t, rows, 2)
	require.Equal(t, []string{"ADJUST", "GRANT"}, []string{rows[0].Kind, rows[1].Kind})
	require.Equal(t, 10, rows[0].BalanceAfter)
	require.Equal(t, 2, e.count(w.org, "SELECT count(*) FROM audit_logs WHERE action LIKE 'leave.ledger.%'"))

	e.api(w.admin, http.MethodGet, "/admin/personnel/999999/leave-ledger", nil).requireFailure(t, http.StatusNotFound, "NOT_FOUND", "")
}

func TestTheLedgerIsAppendOnly(t *testing.T) {
	e := newEnv(t)
	w := e.world()
	require.Equal(t, http.StatusCreated, e.grant(w, w.guardID, 5).Status)

	// siapp_app can insert and read, nothing else: a mistake is corrected by an ADJUST row.
	err := database.WithTenantTx(t.Context(), testkit.Pool(t), w.org, func(tx pgx.Tx) error {
		_, err := tx.Exec(t.Context(), "UPDATE leave_ledger SET days = 99")
		return err
	})
	require.ErrorContains(t, err, "permission denied")
	err = database.WithTenantTx(t.Context(), testkit.Pool(t), w.org, func(tx pgx.Tx) error {
		_, err := tx.Exec(t.Context(), "DELETE FROM leave_ledger")
		return err
	})
	require.ErrorContains(t, err, "permission denied")
}

func TestARegionAdminReachesOnlyTheirRegionsPeople(t *testing.T) {
	e := newEnv(t)
	w := e.world()
	regionAdminID, regionAdmin := e.person(w.org, "REGION_ADMIN")
	_ = regionAdminID
	outsiderID, _ := e.person(w.org, "")
	e.regions[w.guardID] = []string{"OS1"}
	e.regions[outsiderID] = []string{"OS2"}

	path := func(id int64) string { return "/admin/personnel/" + strconv.FormatInt(id, 10) + "/leave-ledger" }
	body := map[string]any{"kind": "GRANT", "days": 3, "note": "x"}

	require.Equal(t, http.StatusCreated, e.api(regionAdmin, http.MethodPost, path(w.guardID), body).Status, "a person in their region")
	require.Equal(t, http.StatusOK, e.api(regionAdmin, http.MethodGet, path(w.guardID), nil).Status)
	// Outside their region, and a person who does not exist, are the same answer.
	for _, id := range []int64{outsiderID, 999999} {
		require.Equal(t, http.StatusForbidden, e.raw(regionAdmin, http.MethodPost, path(id), body).Status)
		require.Equal(t, http.StatusForbidden, e.raw(regionAdmin, http.MethodGet, path(id), nil).Status)
	}
	require.Equal(t, 1, e.count(w.org, "SELECT count(*) FROM leave_ledger"))
}

// request inserts a bare request of annual leave of the person: a ledger row points at one.
func (e *env) request(w world, personnelID int64) (id int64) {
	e.t.Helper()
	e.inTenant(w.org, func(tx pgx.Tx) {
		require.NoError(e.t, tx.QueryRow(e.t.Context(), `
INSERT INTO leave_requests (number, personnel_id, team_code, project_code, type, category_id, start_date, end_date, working_days, reason)
SELECT 'T' || gen_random_uuid()::text, $1, '1K-IBMI-S1', '1K-IBMI-S', 'LEAVE', c.id, '2026-04-01', '2026-04-03', 3, 'Uji'
FROM leave_categories c WHERE c.code = 'ANNUAL' RETURNING id`, personnelID).Scan(&id))
	})
	return id
}

// inLockedTx runs fn the way Part B's submit will: in the tenant transaction, under the person's
// row lock.
func (e *env) inLockedTx(w world, personnelID int64, fn func(q *db.Queries)) {
	e.t.Helper()
	require.NoError(e.t, database.WithTenant(e.t.Context(), testkit.Pool(e.t), w.org, func(q *db.Queries) error {
		if _, err := q.LockPersonnel(e.t.Context(), personnelID); err != nil {
			return err
		}
		fn(q)
		return nil
	}))
}

func TestDeductAndRefundKeepTheBalanceRight(t *testing.T) {
	e := newEnv(t)
	w := e.world()
	require.Equal(t, http.StatusCreated, e.grant(w, w.guardID, 5).Status)
	ctx := t.Context()
	now := e.clock.Now()
	first, second, never := e.request(w, w.guardID), e.request(w, w.guardID), e.request(w, w.guardID)

	e.inLockedTx(w, w.guardID, func(q *db.Queries) {
		row, err := leave.Deduct(ctx, q, now, w.guardID, first, 3, "LR202603-00001")
		require.NoError(t, err)
		require.Equal(t, -3, row.Days)
		require.Equal(t, 2, row.BalanceAfter)

		// A balance that cannot cover the days is refused and writes nothing.
		_, err = leave.Deduct(ctx, q, now, w.guardID, second, 3, "too much")
		require.ErrorIs(t, err, leave.ErrBalanceInsufficient)
		bal, err := leave.Balance(ctx, q, w.guardID)
		require.NoError(t, err)
		require.Equal(t, 2, bal)
	})

	// A refund gives the days back once. A second one, and one for a request that deducted nothing, do nothing.
	e.inLockedTx(w, w.guardID, func(q *db.Queries) {
		refunded, err := leave.Refund(ctx, q, now, w.guardID, first, "Ditolak")
		require.NoError(t, err)
		require.True(t, refunded)
		again, err := leave.Refund(ctx, q, now, w.guardID, first, "Ditolak lagi")
		require.NoError(t, err)
		require.False(t, again)
		none, err := leave.Refund(ctx, q, now, w.guardID, never, "Tidak pernah dipotong")
		require.NoError(t, err)
		require.False(t, none)
		bal, err := leave.Balance(ctx, q, w.guardID)
		require.NoError(t, err)
		require.Equal(t, 5, bal)
	})
	require.Equal(t, 3, e.count(w.org, "SELECT count(*) FROM leave_ledger"), "grant, deduct, refund")

	// A request is deducted once: the database refuses a second deduction of the same one.
	require.Error(t, database.WithTenant(ctx, testkit.Pool(t), w.org, func(q *db.Queries) error {
		_, err := leave.Deduct(ctx, q, now, w.guardID, first, 1, "again")
		return err
	}))
	require.Equal(t, 3, e.count(w.org, "SELECT count(*) FROM leave_ledger"))
}

func TestTwoDeductionsOfTheLastDaysLetOneThrough(t *testing.T) {
	e := newEnv(t)
	w := e.world()
	require.Equal(t, http.StatusCreated, e.grant(w, w.guardID, 3).Status)

	results := make(chan error, 2)
	var wg sync.WaitGroup
	for _, requestID := range []int64{e.request(w, w.guardID), e.request(w, w.guardID)} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results <- database.WithTenant(t.Context(), testkit.Pool(t), w.org, func(q *db.Queries) error {
				if _, err := q.LockPersonnel(t.Context(), w.guardID); err != nil {
					return err
				}
				_, err := leave.Deduct(t.Context(), q, e.clock.Now(), w.guardID, requestID, 3, "parallel")
				return err
			})
		}()
	}
	wg.Wait()
	close(results)
	var failed int
	for err := range results {
		if err != nil {
			require.ErrorIs(t, err, leave.ErrBalanceInsufficient)
			failed++
		}
	}
	require.Equal(t, 1, failed, "exactly one deduction gets the days")
	require.Equal(t, 0, e.count(w.org, "SELECT balance_after FROM leave_ledger ORDER BY id DESC LIMIT 1"))
}

// ---------- approver ----------

func TestTheApproverIsTheDanruOfTheRegu(t *testing.T) {
	e := newEnv(t)
	w := e.world()

	r := e.api(w.guard, http.MethodGet, "/permissions/approver", nil)
	require.Equal(t, http.StatusOK, r.Status, "body: %s", r.Raw)
	got := data[struct {
		PersonnelID int64  `json:"personnel_id"`
		Name        string `json:"name"`
	}](t, r)
	require.Equal(t, w.danruID, got.PersonnelID)
	require.Equal(t, "Pak Danru", got.Name)
	require.Equal(t, http.StatusOK, e.raw(w.guard, http.MethodGet, "/permissions/approver?team_code="+w.team, nil).Status)

	// The DANRU's own request has no DANRU above it: the admins decide it.
	e.api(w.danru, http.MethodGet, "/permissions/approver", nil).requireFailure(t, http.StatusNotFound, "NOT_FOUND", leave.ReasonNoApprover)

	// A regu without a DANRU today is the same.
	e.exec(w.org, "UPDATE team_members SET end_date = '2026-03-01' WHERE role = 'DANRU'")
	e.api(w.guard, http.MethodGet, "/permissions/approver", nil).requireFailure(t, http.StatusNotFound, "NOT_FOUND", leave.ReasonNoApprover)

	// And a regu the person does not serve in is not theirs to ask about.
	require.Equal(t, http.StatusForbidden, e.raw(w.guard, http.MethodGet, "/permissions/approver?team_code="+w.otherTeam, nil).Status)
	require.Equal(t, http.StatusUnauthorized, e.raw("", http.MethodGet, "/permissions/approver", nil).Status)
}
