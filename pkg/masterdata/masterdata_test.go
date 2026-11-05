package masterdata_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"

	"siapp/internal/db"
	"siapp/pkg/access"
	"siapp/pkg/masterdata"
	"siapp/pkg/platform/auth"
	"siapp/pkg/platform/database"
	"siapp/pkg/platform/personel"
	"siapp/pkg/platform/testkit"
)

func TestMain(m *testing.M) { os.Exit(testkit.NewDatabase(m)) }

// testClock is a clock tests move by hand, on a whole second (finer than timestamptz's
// microsecond, so a time the test holds equals the one the database gives back).
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

type env struct {
	t       *testing.T
	srv     string
	client  *testkit.Client
	clock   *testClock
	service *masterdata.Service
	logos   *fakeLogos
	userID  int64
}

// newEnv serves the admin group (RequireAdmin and the master data) and the field routes on a
// clean database. Leave types come from the in-process fake Personalia through the cache.
func newEnv(t *testing.T) *env {
	t.Helper()
	testkit.Reset(t)
	pool := testkit.Pool(t)
	fake, personelClient := testkit.NewPersonel(t)
	cache := personel.NewCached(personelClient, nil, testkit.Redis(t), nil)
	cache.UseLeave(fake)

	clk := &testClock{now: time.Now().UTC().Truncate(time.Second)}
	service := masterdata.NewService(pool, clk, cache)
	logos := newFakeLogos()
	service.UseLogoStore(logos)
	h := masterdata.NewHandler(service)
	srv := testkit.NewServer(t, testkit.Deps{Mount: func(r chi.Router) {
		r.Route("/api/v1/admin", func(admin chi.Router) {
			admin.Use(access.RequireAdmin(pool))
			h.MountAdmin(admin)
		})
		h.MountField(r)
	}})
	return &env{t: t, srv: srv.URL, client: testkit.NewClient(t, srv), clock: clk, service: service, logos: logos}
}

type reply struct {
	Status int
	Raw    []byte
}

func (r reply) reason(t *testing.T) (code, reason string) {
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
	gotCode, gotReason := r.reason(t)
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

type category struct {
	ID            int64    `json:"id"`
	Name          string   `json:"name"`
	Urgency       string   `json:"urgency"`
	BusinessLines []string `json:"business_lines"`
	SortOrder     int      `json:"sort_order"`
	IsActive      bool     `json:"is_active"`
}

type panicType struct {
	ID        int64  `json:"id"`
	Code      string `json:"code"`
	Name      string `json:"name"`
	SortOrder int    `json:"sort_order"`
	IsActive  bool   `json:"is_active"`
}

type delta[T any] struct {
	Data    []T `json:"data"`
	Deleted []struct {
		ID int64 `json:"id"`
	} `json:"deleted"`
	Meta struct {
		ServerTime time.Time `json:"server_time"`
	} `json:"meta"`
}

func deltaOf[T any](t *testing.T, r reply) delta[T] {
	t.Helper()
	require.Equal(t, http.StatusOK, r.Status, "body: %s", r.Raw)
	var d delta[T]
	require.NoError(t, json.Unmarshal(r.Raw, &d), "body: %s", r.Raw)
	return d
}

func names(cs []category) (out []string) {
	for _, c := range cs {
		out = append(out, c.Name)
	}
	return out
}

func deletedIDs[T any](d delta[T]) (out []int64) {
	for _, x := range d.Deleted {
		out = append(out, x.ID)
	}
	return out
}

// api is a request checked against the contract; raw is not, for the requests the contract
// forbids and the errors it does not list for every route.
func (e *env) api(token, method, path string, body any) reply {
	e.t.Helper()
	e.client.Token = token
	defer func() { e.client.Token = "" }()
	res := e.client.Do(method, path, body)
	return reply{Status: res.Status, Raw: res.Body}
}

func (e *env) raw(token, method, path string, body any) reply {
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
	req.Header.Set("Authorization", "Bearer "+token)
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

// person adds a person to an organization and returns their id and a token. A web role ("" for
// none) makes them an admin; a REGION_ADMIN points at the region, which is created.
func (e *env) person(org int64, role string) (int64, string) {
	e.t.Helper()
	e.userID++
	var personnelID int64
	e.inTenant(org, func(tx pgx.Tx) {
		require.NoError(e.t, tx.QueryRow(e.t.Context(),
			"INSERT INTO personnels (personel_user_id, full_name, status) VALUES ($1, 'Petugas Master', 'ACTIVE') RETURNING id",
			e.userID).Scan(&personnelID))
		switch role {
		case "":
		case "ORG_ADMIN":
			_, err := tx.Exec(e.t.Context(), "INSERT INTO web_role_assignments (personnel_id, role) VALUES ($1, 'ORG_ADMIN')", personnelID)
			require.NoError(e.t, err)
		case "REGION_ADMIN":
			_, err := tx.Exec(e.t.Context(), "INSERT INTO regions (code, description) VALUES ('OS1', 'Outsourcing 1') ON CONFLICT DO NOTHING")
			require.NoError(e.t, err)
			_, err = tx.Exec(e.t.Context(), "INSERT INTO web_role_assignments (personnel_id, role, region_code) VALUES ($1, 'REGION_ADMIN', 'OS1')", personnelID)
			require.NoError(e.t, err)
		}
	})
	return personnelID, testkit.Token(e.t, org, personnelID)
}

// serve puts a person in a regu of a project of the given business line, from a long time ago.
func (e *env) serve(org, personnelID int64, line string) {
	e.t.Helper()
	suffix := map[string]string{"security": "S", "cleaning": "C"}[line]
	e.exec(org, `
INSERT INTO regions (code, description) VALUES ('OS1', 'Outsourcing 1') ON CONFLICT DO NOTHING;
INSERT INTO branches (code, region_code, company_letter, description) VALUES ('1K', 'OS1', 'K', 'Cabang 1K') ON CONFLICT DO NOTHING;
INSERT INTO clients (code, branch_code, description, company_prefix) VALUES ('1K-IBMI', '1K', 'IBMI', 'IBMI') ON CONFLICT DO NOTHING;`)
	e.exec(org, "INSERT INTO projects (code, client_code, description, business_line, timezone) VALUES ($1, '1K-IBMI', 'Proyek', $2::business_line, 'Asia/Jakarta')",
		"1K-IBMI-"+suffix, line)
	e.exec(org, "INSERT INTO teams (code, project_code, description) VALUES ($1, $2, 'Regu 1')", "1K-IBMI-"+suffix+"1", "1K-IBMI-"+suffix)
	e.exec(org, "INSERT INTO team_members (team_code, personnel_id, role, start_date) VALUES ($1, $2, 'MEMBER', '2000-01-01')", "1K-IBMI-"+suffix+"1", personnelID)
}

// seeded is an organization with its defaults and an ORG_ADMIN.
func (e *env) seeded() (org int64, admin string) {
	e.t.Helper()
	org = testkit.NewOrg(e.t)
	_, err := masterdata.EnsureDefaults(e.t.Context(), testkit.Pool(e.t), e.clock, org)
	require.NoError(e.t, err)
	_, admin = e.person(org, "ORG_ADMIN")
	// A delta pull reaches back a few seconds (the overlap), so the test's later pulls must start
	// well after the seed to see only what they change.
	e.clock.Advance(time.Minute)
	return org, admin
}

func (e *env) categories(token, path string) []category {
	e.t.Helper()
	r := e.api(token, http.MethodGet, path, nil)
	require.Equal(e.t, http.StatusOK, r.Status, "body: %s", r.Raw)
	return data[[]category](e.t, r)
}

func (e *env) createCategory(admin, kind string, body map[string]any) category {
	e.t.Helper()
	r := e.api(admin, http.MethodPost, "/admin/"+kind+"-categories", body)
	require.Equal(e.t, http.StatusCreated, r.Status, "body: %s", r.Raw)
	return data[category](e.t, r)
}

func (e *env) auditActions(org int64) (out []string) {
	e.t.Helper()
	e.inTenant(org, func(tx pgx.Tx) {
		rows, err := tx.Query(e.t.Context(), "SELECT action FROM audit_logs ORDER BY id")
		require.NoError(e.t, err)
		out, err = pgx.CollectRows(rows, pgx.RowTo[string])
		require.NoError(e.t, err)
	})
	return out
}

func since(t time.Time) string { return "?since=" + url.QueryEscape(t.UTC().Format(time.RFC3339)) }

// ---------- defaults ----------

func TestANewOrganizationGetsTheDefaultsAndASecondCallAddsNothing(t *testing.T) {
	e := newEnv(t)
	org := testkit.NewOrg(t)

	added, err := masterdata.EnsureDefaults(t.Context(), testkit.Pool(t), e.clock, org)
	require.NoError(t, err)
	// 3 stakeholder types, 4 panic types linked to 3 of them, 2 app links, 4 report and 2 instruction
	// categories.
	require.EqualValues(t, 3+4+3+2+4+2, added)
	added, err = masterdata.EnsureDefaults(t.Context(), testkit.Pool(t), e.clock, org)
	require.NoError(t, err)
	require.Zero(t, added)

	_, admin := e.person(org, "ORG_ADMIN")
	reports := e.categories(admin, "/admin/report-categories")
	require.Equal(t, []string{"Penemuan Barang", "Kejadian", "Kehilangan", "Lainnya"}, names(reports))
	instructions := e.categories(admin, "/admin/instruction-categories")
	require.Equal(t, []string{"Umum", "Pengecekan"}, names(instructions))
	require.Equal(t, "NORMAL", instructions[0].Urgency)
	require.Equal(t, "HIGH", instructions[1].Urgency)

	r := e.api(admin, http.MethodGet, "/admin/panic-types", nil)
	var codes []string
	for _, p := range data[[]panicType](t, r) {
		codes = append(codes, p.Code)
	}
	require.Equal(t, []string{"SECURITY", "MEDICAL", "FIRE", "OTHER"}, codes)

	// The next organization is its own: it has nothing until it is seeded.
	other := testkit.NewOrg(t)
	_, otherAdmin := e.person(other, "ORG_ADMIN")
	require.Empty(t, e.categories(otherAdmin, "/admin/report-categories"))
}

func TestADefaultAnAdminDeletedIsNotGivenBack(t *testing.T) {
	e := newEnv(t)
	org, admin := e.seeded()
	reports := e.categories(admin, "/admin/report-categories")
	require.Equal(t, http.StatusOK, e.api(admin, http.MethodDelete, "/admin/report-categories/"+itoa(reports[0].ID), nil).Status)

	added, err := masterdata.EnsureDefaults(t.Context(), testkit.Pool(t), e.clock, org)
	require.NoError(t, err)
	require.Zero(t, added)
	require.Len(t, e.categories(admin, "/admin/report-categories"), 3)
}

// ---------- admin: categories ----------

func TestUrgencyIsRequiredForAnInstructionAndRejectedForAReport(t *testing.T) {
	e := newEnv(t)
	org, admin := e.seeded()

	e.raw(admin, http.MethodPost, "/admin/instruction-categories", map[string]any{"name": "Tanpa urgensi"}).
		requireFailure(t, http.StatusUnprocessableEntity, "VALIDATION_ERROR", "")
	e.raw(admin, http.MethodPost, "/admin/report-categories", map[string]any{"name": "Dengan urgensi", "urgency": "HIGH"}).
		requireFailure(t, http.StatusUnprocessableEntity, "VALIDATION_ERROR", "")
	e.raw(admin, http.MethodPost, "/admin/instruction-categories", map[string]any{"name": "Salah", "urgency": "URGENT"}).
		requireFailure(t, http.StatusUnprocessableEntity, "VALIDATION_ERROR", "")

	created := e.createCategory(admin, "instruction", map[string]any{"name": "Darurat", "urgency": "HIGH"})
	require.Equal(t, "HIGH", created.Urgency)

	// A patch cannot take the urgency away from an instruction, or give one to a report.
	e.raw(admin, http.MethodPatch, "/admin/instruction-categories/"+itoa(created.ID), map[string]any{"urgency": ""}).
		requireFailure(t, http.StatusUnprocessableEntity, "VALIDATION_ERROR", "")
	report := e.createCategory(admin, "report", map[string]any{"name": "Baru"})
	e.raw(admin, http.MethodPatch, "/admin/report-categories/"+itoa(report.ID), map[string]any{"urgency": "LOW"}).
		requireFailure(t, http.StatusUnprocessableEntity, "VALIDATION_ERROR", "")

	// The CHECK is the backstop for a writer that skips the usecase.
	err := database.WithTenantTx(t.Context(), testkit.Pool(t), org, func(tx pgx.Tx) error {
		_, err := tx.Exec(t.Context(), "INSERT INTO categories (kind, name) VALUES ('INSTRUCTION', 'Langsung')")
		return err
	})
	require.ErrorContains(t, err, "categories_urgency_check")
}

func TestAnAdminManagesCategories(t *testing.T) {
	e := newEnv(t)
	org, admin := e.seeded()

	c := e.createCategory(admin, "report", map[string]any{"name": "Gangguan", "business_lines": []string{"security", "security"}, "sort_order": 9})
	require.Equal(t, []string{"security"}, c.BusinessLines, "a repeated line is stored once")
	require.True(t, c.IsActive)

	// The name is taken in the same kind on an overlapping line (no lines = all), not in the other kind.
	e.raw(admin, http.MethodPost, "/admin/report-categories", map[string]any{"name": "Gangguan"}).
		requireFailure(t, http.StatusConflict, "DUPLICATE", masterdata.ReasonCategoryNameTaken)
	e.createCategory(admin, "instruction", map[string]any{"name": "Gangguan", "urgency": "LOW"})

	patched := e.api(admin, http.MethodPatch, "/admin/report-categories/"+itoa(c.ID), map[string]any{"name": "Gangguan Keamanan", "business_lines": []string{}})
	require.Equal(t, http.StatusOK, patched.Status, "body: %s", patched.Raw)
	require.Equal(t, "Gangguan Keamanan", data[category](t, patched).Name)
	require.Empty(t, data[category](t, patched).BusinessLines, "an empty list means every business line")

	// A category of the other kind is not found under this path.
	e.raw(admin, http.MethodGet, "/admin/instruction-categories/"+itoa(c.ID), nil).
		requireFailure(t, http.StatusNotFound, "NOT_FOUND", masterdata.ReasonCategoryNotFound)
	e.raw(admin, http.MethodPatch, "/admin/instruction-categories/"+itoa(c.ID), map[string]any{"name": "X"}).
		requireFailure(t, http.StatusNotFound, "NOT_FOUND", masterdata.ReasonCategoryNotFound)

	// An unknown field, such as a kind, is refused: the path decides the kind.
	e.raw(admin, http.MethodPost, "/admin/report-categories", map[string]any{"name": "Lain", "kind": "INSTRUCTION"}).
		requireFailure(t, http.StatusUnprocessableEntity, "VALIDATION_ERROR", "")

	// An empty patch changes nothing: no audit row.
	before := len(e.auditActions(org))
	require.Equal(t, http.StatusOK, e.api(admin, http.MethodPatch, "/admin/report-categories/"+itoa(c.ID), map[string]any{}).Status)
	require.Len(t, e.auditActions(org), before)

	// A deactivated category still holds its name, so reactivating it can never clash.
	require.Equal(t, http.StatusOK, e.api(admin, http.MethodPatch, "/admin/report-categories/"+itoa(c.ID), map[string]any{"is_active": false}).Status)
	e.raw(admin, http.MethodPost, "/admin/report-categories", map[string]any{"name": "Gangguan Keamanan"}).
		requireFailure(t, http.StatusConflict, "DUPLICATE", masterdata.ReasonCategoryNameTaken)
	require.Equal(t, http.StatusOK, e.api(admin, http.MethodPatch, "/admin/report-categories/"+itoa(c.ID), map[string]any{"is_active": true}).Status)
	require.Equal(t, http.StatusOK, e.api(admin, http.MethodPatch, "/admin/report-categories/"+itoa(c.ID), map[string]any{"is_active": false}).Status)

	// Filters and paging.
	inactive := e.api(admin, http.MethodGet, "/admin/report-categories?is_active=false", nil)
	require.Equal(t, []string{"Gangguan Keamanan"}, names(data[[]category](t, inactive)))
	search := e.api(admin, http.MethodGet, "/admin/report-categories?q=kehil", nil)
	require.Equal(t, []string{"Kehilangan"}, names(data[[]category](t, search)))
	require.Len(t, data[[]category](t, e.api(admin, http.MethodGet, "/admin/report-categories?limit=2", nil)), 2)

	// Deleting hides the row from the admin and frees its name.
	require.Equal(t, http.StatusOK, e.api(admin, http.MethodDelete, "/admin/report-categories/"+itoa(c.ID), nil).Status)
	e.raw(admin, http.MethodGet, "/admin/report-categories/"+itoa(c.ID), nil).
		requireFailure(t, http.StatusNotFound, "NOT_FOUND", masterdata.ReasonCategoryNotFound)
	e.raw(admin, http.MethodDelete, "/admin/report-categories/"+itoa(c.ID), nil).
		requireFailure(t, http.StatusNotFound, "NOT_FOUND", masterdata.ReasonCategoryNotFound)
	e.createCategory(admin, "report", map[string]any{"name": "Gangguan Keamanan"})

	actions := e.auditActions(org)
	require.Contains(t, actions, "masterdata.category.create")
	require.Contains(t, actions, "masterdata.category.update")
	require.Contains(t, actions, "masterdata.category.delete")
}

func TestOnlyAnOrgAdminReachesTheMasterAdminRoutes(t *testing.T) {
	e := newEnv(t)
	org, admin := e.seeded()
	_, region := e.person(org, "REGION_ADMIN")
	_, nobody := e.person(org, "")
	c := e.categories(admin, "/admin/report-categories")[0]

	for _, tc := range []struct {
		method, path string
		body         any
	}{
		{http.MethodGet, "/admin/report-categories", nil},
		{http.MethodPost, "/admin/report-categories", map[string]any{"name": "X"}},
		{http.MethodPatch, "/admin/report-categories/" + itoa(c.ID), map[string]any{"name": "X"}},
		{http.MethodDelete, "/admin/report-categories/" + itoa(c.ID), nil},
		{http.MethodGet, "/admin/panic-types", nil},
		{http.MethodPost, "/admin/panic-types", map[string]any{"name": "X"}},
	} {
		// A REGION_ADMIN is an admin, but masters are the organization's, not a region's.
		r := e.raw(region, tc.method, tc.path, tc.body)
		require.Equal(t, http.StatusForbidden, r.Status, "%s %s: %s", tc.method, tc.path, r.Raw)
		// A person with no web role never gets into the group.
		r = e.raw(nobody, tc.method, tc.path, tc.body)
		require.Equal(t, http.StatusForbidden, r.Status, "%s %s: %s", tc.method, tc.path, r.Raw)
	}
}

func TestACategoryOfAnotherOrganizationIsNotThere(t *testing.T) {
	e := newEnv(t)
	_, adminA := e.seeded()
	_, adminB := e.seeded()
	idOfB := e.categories(adminB, "/admin/report-categories")[0].ID

	e.raw(adminA, http.MethodGet, "/admin/report-categories/"+itoa(idOfB), nil).
		requireFailure(t, http.StatusNotFound, "NOT_FOUND", masterdata.ReasonCategoryNotFound)
	e.raw(adminA, http.MethodPatch, "/admin/report-categories/"+itoa(idOfB), map[string]any{"name": "Dibajak"}).
		requireFailure(t, http.StatusNotFound, "NOT_FOUND", masterdata.ReasonCategoryNotFound)
	e.raw(adminA, http.MethodDelete, "/admin/report-categories/"+itoa(idOfB), nil).
		requireFailure(t, http.StatusNotFound, "NOT_FOUND", masterdata.ReasonCategoryNotFound)
	require.Equal(t, "Penemuan Barang", e.categories(adminB, "/admin/report-categories")[0].Name)
}

func TestAMasterSomethingPointsAtCannotBeDeleted(t *testing.T) {
	e := newEnv(t)
	_, admin := e.seeded()
	used := e.createCategory(admin, "report", map[string]any{"name": "Dipakai"})
	free := e.createCategory(admin, "report", map[string]any{"name": "Bebas"})
	usedType := data[panicType](t, e.api(admin, http.MethodPost, "/admin/panic-types", map[string]any{"name": "Dipakai"}))

	// The module that owns the reference registers the check (reports in 11.1, panic in 09).
	e.service.UseCategoryUsage(masterdata.KindReport, func(_ context.Context, _ *db.Queries, id int64) (bool, error) {
		return id == used.ID, nil
	})
	e.service.UsePanicTypeUsage(func(_ context.Context, _ *db.Queries, id int64) (bool, error) {
		return id == usedType.ID, nil
	})

	e.raw(admin, http.MethodDelete, "/admin/report-categories/"+itoa(used.ID), nil).
		requireFailure(t, http.StatusConflict, "CONFLICT", masterdata.ReasonMasterInUse)
	e.raw(admin, http.MethodDelete, "/admin/panic-types/"+itoa(usedType.ID), nil).
		requireFailure(t, http.StatusConflict, "CONFLICT", masterdata.ReasonMasterInUse)
	require.Equal(t, http.StatusOK, e.api(admin, http.MethodDelete, "/admin/report-categories/"+itoa(free.ID), nil).Status)

	// What the client offers instead: deactivate.
	off := e.api(admin, http.MethodPatch, "/admin/report-categories/"+itoa(used.ID), map[string]any{"is_active": false})
	require.Equal(t, http.StatusOK, off.Status, "body: %s", off.Raw)
	require.False(t, data[category](t, off).IsActive)
}

// ---------- admin: panic types ----------

func TestAnAdminManagesPanicTypes(t *testing.T) {
	e := newEnv(t)
	_, admin := e.seeded()

	made := e.api(admin, http.MethodPost, "/admin/panic-types", map[string]any{"name": "Banjir Bandang", "sort_order": 7})
	require.Equal(t, http.StatusCreated, made.Status, "body: %s", made.Raw)
	flood := data[panicType](t, made)
	require.Equal(t, "BANJIR_BANDANG", flood.Code, "made from the name")

	e.raw(admin, http.MethodPost, "/admin/panic-types", map[string]any{"code": "banjir_bandang", "name": "Lagi"}).
		requireFailure(t, http.StatusConflict, "DUPLICATE", masterdata.ReasonPanicTypeCodeTaken)
	e.raw(admin, http.MethodPost, "/admin/panic-types", map[string]any{"code": "1 SALAH", "name": "Lagi"}).
		requireFailure(t, http.StatusUnprocessableEntity, "VALIDATION_ERROR", "")

	// The code is the identity: a patch has no field for it.
	e.raw(admin, http.MethodPatch, "/admin/panic-types/"+itoa(flood.ID), map[string]any{"code": "LAIN"}).
		requireFailure(t, http.StatusUnprocessableEntity, "VALIDATION_ERROR", "")
	renamed := e.api(admin, http.MethodPatch, "/admin/panic-types/"+itoa(flood.ID), map[string]any{"name": "Banjir"})
	require.Equal(t, http.StatusOK, renamed.Status, "body: %s", renamed.Raw)
	require.Equal(t, "Banjir", data[panicType](t, renamed).Name)
	require.Equal(t, "BANJIR_BANDANG", data[panicType](t, renamed).Code)

	// A deleted type keeps its code taken.
	require.Equal(t, http.StatusOK, e.api(admin, http.MethodDelete, "/admin/panic-types/"+itoa(flood.ID), nil).Status)
	e.raw(admin, http.MethodGet, "/admin/panic-types/"+itoa(flood.ID), nil).
		requireFailure(t, http.StatusNotFound, "NOT_FOUND", masterdata.ReasonPanicTypeNotFound)
	e.raw(admin, http.MethodPost, "/admin/panic-types", map[string]any{"code": "BANJIR_BANDANG", "name": "Lagi"}).
		requireFailure(t, http.StatusConflict, "DUPLICATE", masterdata.ReasonPanicTypeCodeTaken)
}

// ---------- field reads and delta sync ----------

func TestAFullPullReturnsActiveRowsAndADeltaPullReturnsTombstones(t *testing.T) {
	e := newEnv(t)
	org, admin := e.seeded()
	_, field := e.person(org, "")

	full := e.api(field, http.MethodGet, "/report-categories", nil)
	require.Equal(t, http.StatusOK, full.Status, "body: %s", full.Raw)
	first := deltaOf[category](t, full)
	require.Equal(t, []string{"Penemuan Barang", "Kejadian", "Kehilangan", "Lainnya"}, names(first.Data))
	require.Empty(t, first.Deleted, "a full pull has no tombstones")
	watermark := first.Meta.ServerTime
	require.WithinDuration(t, e.clock.Now(), watermark, time.Second)

	// Later, an admin deactivates one, deletes another and adds a third.
	e.clock.Advance(time.Minute)
	deactivated, deleted := first.Data[1], first.Data[2]
	require.Equal(t, http.StatusOK, e.api(admin, http.MethodPatch, "/admin/report-categories/"+itoa(deactivated.ID), map[string]any{"is_active": false}).Status)
	require.Equal(t, http.StatusOK, e.api(admin, http.MethodDelete, "/admin/report-categories/"+itoa(deleted.ID), nil).Status)
	added := e.createCategory(admin, "report", map[string]any{"name": "Baru"})

	// The full pull now omits both, with no way to say they are gone.
	require.Equal(t, []string{"Penemuan Barang", "Lainnya", "Baru"}, names(deltaOf[category](t, e.api(field, http.MethodGet, "/report-categories", nil)).Data))

	// ⚠ The delta pull says it, in deleted[]: that is what empties the offline cache (DL-57).
	e.clock.Advance(time.Minute)
	d := deltaOf[category](t, e.api(field, http.MethodGet, "/report-categories"+since(watermark), nil))
	require.ElementsMatch(t, []int64{deactivated.ID, deleted.ID}, deletedIDs(d))
	require.Equal(t, []string{"Baru"}, names(d.Data))
	require.Equal(t, added.ID, d.Data[0].ID)
	require.True(t, d.Meta.ServerTime.After(watermark))

	// Nothing changed since: the next pull is empty, once the overlap has passed.
	e.clock.Advance(time.Minute)
	quiet := deltaOf[category](t, e.api(field, http.MethodGet, "/report-categories"+since(d.Meta.ServerTime), nil))
	require.Empty(t, quiet.Data)
	require.Empty(t, quiet.Deleted)

	e.raw(field, http.MethodGet, "/report-categories?since=yesterday", nil).
		requireFailure(t, http.StatusUnprocessableEntity, "VALIDATION_ERROR", "")
}

func TestPanicTypesAndInstructionCategoriesSyncTheSameWay(t *testing.T) {
	e := newEnv(t)
	org, admin := e.seeded()
	_, field := e.person(org, "")

	pt := deltaOf[panicType](t, e.api(field, http.MethodGet, "/panic-types", nil))
	require.Len(t, pt.Data, 4)
	ic := deltaOf[category](t, e.api(field, http.MethodGet, "/instruction-categories", nil))
	require.Equal(t, []string{"Umum", "Pengecekan"}, names(ic.Data))
	require.Equal(t, "HIGH", ic.Data[1].Urgency)

	e.clock.Advance(time.Minute)
	other := pt.Data[3]
	require.Equal(t, http.StatusOK, e.api(admin, http.MethodPatch, "/admin/panic-types/"+itoa(other.ID), map[string]any{"is_active": false}).Status)
	require.Equal(t, http.StatusOK, e.api(admin, http.MethodDelete, "/admin/instruction-categories/"+itoa(ic.Data[0].ID), nil).Status)

	ptDelta := deltaOf[panicType](t, e.api(field, http.MethodGet, "/panic-types"+since(pt.Meta.ServerTime), nil))
	require.Equal(t, []int64{other.ID}, deletedIDs(ptDelta))
	require.Empty(t, ptDelta.Data)
	icDelta := deltaOf[category](t, e.api(field, http.MethodGet, "/instruction-categories"+since(ic.Meta.ServerTime), nil))
	require.Equal(t, []int64{ic.Data[0].ID}, deletedIDs(icDelta))

	// Another organization's changes are never in it.
	_, adminB := e.seeded()
	idB := data[[]panicType](t, e.api(adminB, http.MethodGet, "/admin/panic-types", nil))[0].ID
	mark := deltaOf[panicType](t, e.api(field, http.MethodGet, "/panic-types", nil)).Meta.ServerTime
	e.clock.Advance(time.Minute)
	require.Equal(t, http.StatusOK, e.api(adminB, http.MethodPatch, "/admin/panic-types/"+itoa(idB), map[string]any{"is_active": false}).Status)
	again := deltaOf[panicType](t, e.api(field, http.MethodGet, "/panic-types"+since(mark), nil))
	require.Empty(t, again.Deleted)
	require.Empty(t, again.Data)
}

func TestCategoriesAreFilteredByTheBusinessLinesOfTheCallersProjects(t *testing.T) {
	e := newEnv(t)
	org, admin := e.seeded()
	e.createCategory(admin, "report", map[string]any{"name": "Khusus Satpam", "business_lines": []string{"security"}})
	cleaningOnly := e.createCategory(admin, "report", map[string]any{"name": "Khusus Kebersihan", "business_lines": []string{"cleaning"}})

	guardID, guard := e.person(org, "")
	e.serve(org, guardID, "security")
	cleanerID, cleaner := e.person(org, "")
	e.serve(org, cleanerID, "cleaning")
	_, drifter := e.person(org, "") // in no regu

	got := func(token string) []string {
		return names(deltaOf[category](t, e.api(token, http.MethodGet, "/report-categories", nil)).Data)
	}
	// A category with no business lines is for everybody; the rest follow the caller's projects.
	everyone := []string{"Penemuan Barang", "Kejadian", "Kehilangan", "Lainnya"}
	require.Equal(t, append(append([]string{}, everyone...), "Khusus Satpam"), got(guard))
	require.Equal(t, append(append([]string{}, everyone...), "Khusus Kebersihan"), got(cleaner))
	require.Equal(t, everyone, got(drifter))

	// A category that moves out of the caller's reach arrives in deleted[], so the cache drops it.
	e.clock.Advance(time.Minute)
	watermark := deltaOf[category](t, e.api(cleaner, http.MethodGet, "/report-categories", nil)).Meta.ServerTime
	e.clock.Advance(time.Minute)
	require.Equal(t, http.StatusOK, e.api(admin, http.MethodPatch, "/admin/report-categories/"+itoa(cleaningOnly.ID), map[string]any{"business_lines": []string{"security"}}).Status)
	d := deltaOf[category](t, e.api(cleaner, http.MethodGet, "/report-categories"+since(watermark), nil))
	require.Equal(t, []int64{cleaningOnly.ID}, deletedIDs(d))
	require.Empty(t, d.Data)
	// ...and to the guard it is a row to add.
	d = deltaOf[category](t, e.api(guard, http.MethodGet, "/report-categories"+since(watermark), nil))
	require.Equal(t, []string{"Khusus Kebersihan"}, names(d.Data))
}

func TestTheFieldRoutesNeedASignedInPerson(t *testing.T) {
	e := newEnv(t)
	for _, path := range []string{"/report-categories", "/instruction-categories", "/panic-types", "/permission-categories"} {
		r := e.raw("", http.MethodGet, path, nil)
		require.Equal(t, http.StatusUnauthorized, r.Status, "%s: %s", path, r.Raw)
	}
}

// ---------- AssertCategory ----------

func TestAssertCategory(t *testing.T) {
	e := newEnv(t)
	org, admin := e.seeded()
	report := e.categories(admin, "/admin/report-categories")[0]
	instruction := e.categories(admin, "/admin/instruction-categories")[0]

	otherOrg, otherAdmin := e.seeded()
	otherReport := e.categories(otherAdmin, "/admin/report-categories")[0]

	as := func(org int64) context.Context {
		return auth.WithPrincipal(t.Context(), auth.Principal{OrganizationID: org, PersonnelID: 1})
	}
	require.NoError(t, e.service.AssertCategory(as(org), report.ID, masterdata.KindReport))
	require.NoError(t, e.service.AssertCategory(as(org), instruction.ID, masterdata.KindInstruction))

	require.ErrorIs(t, e.service.AssertCategory(as(org), instruction.ID, masterdata.KindReport), masterdata.ErrCategoryKindMismatch)
	require.ErrorIs(t, e.service.AssertCategory(as(org), report.ID, masterdata.KindInstruction), masterdata.ErrCategoryKindMismatch)
	require.ErrorIs(t, e.service.AssertCategory(as(org), 999999, masterdata.KindReport), masterdata.ErrCategoryNotFound)
	// Another organization's category is not found, not a mismatch and not accepted.
	require.ErrorIs(t, e.service.AssertCategory(as(org), otherReport.ID, masterdata.KindReport), masterdata.ErrCategoryNotFound)
	require.NoError(t, e.service.AssertCategory(as(otherOrg), otherReport.ID, masterdata.KindReport))
	require.Error(t, e.service.AssertCategory(t.Context(), report.ID, masterdata.KindReport), "no caller in ctx")

	// A deactivated category still passes (an offline report must not be lost); a deleted one does not.
	require.Equal(t, http.StatusOK, e.api(admin, http.MethodPatch, "/admin/report-categories/"+itoa(report.ID), map[string]any{"is_active": false}).Status)
	require.NoError(t, e.service.AssertCategory(as(org), report.ID, masterdata.KindReport))
	require.Equal(t, http.StatusOK, e.api(admin, http.MethodDelete, "/admin/report-categories/"+itoa(report.ID), nil).Status)
	require.ErrorIs(t, e.service.AssertCategory(as(org), report.ID, masterdata.KindReport), masterdata.ErrCategoryNotFound)
}

// ---------- leave categories ----------

func TestPermissionCategoriesAreTheLeaveTypesOfPersonel(t *testing.T) {
	e := newEnv(t)
	org := testkit.NewOrg(t)
	_, token := e.person(org, "")

	r := e.api(token, http.MethodGet, "/permission-categories", nil)
	require.Equal(t, http.StatusOK, r.Status, "body: %s", r.Raw)
	got := data[[]struct {
		ID       int64  `json:"id"`
		Name     string `json:"name"`
		IsActive bool   `json:"is_active"`
	}](t, r)
	require.Len(t, got, 3)
	require.Equal(t, "Sakit", got[0].Name)
	require.Equal(t, "Izin", got[1].Name)
	require.Equal(t, "Cuti", got[2].Name)
	require.True(t, got[0].IsActive)

	// SiAPP stores none: there is no table, and no admin route to write one.
	e.inTenant(org, func(tx pgx.Tx) {
		var n int
		require.NoError(t, tx.QueryRow(t.Context(), "SELECT count(*) FROM information_schema.tables WHERE table_name LIKE '%leave%' OR table_name LIKE 'permission_categor%'").Scan(&n))
		require.Zero(t, n)
	})
}

func TestPermissionCategoriesAnswer503WhenPersonelHasNoLeaveSeam(t *testing.T) {
	testkit.Reset(t)
	pool := testkit.Pool(t)
	_, client := testkit.NewPersonel(t)
	cache := personel.NewCached(client, nil, testkit.Redis(t), nil) // no UseLeave
	clk := &testClock{now: time.Now().UTC().Truncate(time.Second)}
	srv := testkit.NewServer(t, testkit.Deps{Mount: masterdata.NewHandler(masterdata.NewService(pool, clk, cache)).MountField})
	org := testkit.NewOrg(t)
	e := &env{t: t, srv: srv.URL}
	r := e.raw(testkit.Token(t, org, 1), http.MethodGet, "/permission-categories", nil)
	r.requireFailure(t, http.StatusServiceUnavailable, "INTERNAL_ERROR", "PERSONEL_UNAVAILABLE")
}

func itoa(n int64) string {
	b, _ := json.Marshal(n)
	return string(b)
}
