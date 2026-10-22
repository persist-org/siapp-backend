package organization_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"

	"siapp/pkg/access"
	"siapp/pkg/organization"
	"siapp/pkg/platform/clock"
	"siapp/pkg/platform/database"
	"siapp/pkg/platform/testkit"
)

func TestMain(m *testing.M) { os.Exit(testkit.NewDatabase(m)) }

type env struct {
	t      *testing.T
	srv    string
	client *testkit.Client
	userID int64
}

// newEnv serves the admin group of the real router (RequireAdmin, the regions and the web
// roles) on a clean database, and checks every request and response against the contract.
func newEnv(t *testing.T) *env { return newEnvWith(t, nil, clock.System{}) }

// newEnvWith is newEnv with the people resolver the team members use (nil where nobody is
// added by NIK) and the clock the usecases read, for tests that need a chosen "today".
func newEnvWith(t *testing.T, people organization.PeopleResolver, clk clock.Clock) *env {
	t.Helper()
	testkit.Reset(t)
	pool := testkit.Pool(t)
	regions := organization.NewHandler(organization.NewUsecases(pool, clk, people))
	roles := access.NewHandler(access.NewWebRoles(pool, clock.System{}))
	srv := testkit.NewServer(t, testkit.Deps{Mount: func(r chi.Router) {
		r.Route("/api/v1/admin", func(admin chi.Router) {
			admin.Use(access.RequireAdmin(pool))
			regions.MountAdmin(admin)
			roles.MountAdmin(admin)
		})
		regions.MountField(r)
	}})
	return &env{t: t, srv: srv.URL, client: testkit.NewClient(t, srv)}
}

type reply struct {
	Status int
	Raw    []byte
}

func (r reply) reason(t *testing.T) (code, reason string) {
	t.Helper()
	var env struct {
		Error struct {
			Code    string         `json:"code"`
			Details map[string]any `json:"details"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal(r.Raw, &env), "body: %s", r.Raw)
	reason, _ = env.Error.Details["reason"].(string)
	return env.Error.Code, reason
}

type regionData struct {
	Code        string `json:"code"`
	Description string `json:"description"`
	IsActive    bool   `json:"is_active"`
}

func data[T any](t *testing.T, r reply) T {
	t.Helper()
	var env struct {
		Data T `json:"data"`
	}
	require.NoError(t, json.Unmarshal(r.Raw, &env), "body: %s", r.Raw)
	return env.Data
}

func codesOf(t *testing.T, r reply) []string {
	t.Helper()
	require.Equal(t, http.StatusOK, r.Status, "body: %s", r.Raw)
	out := []string{}
	for _, row := range data[[]regionData](t, r) {
		out = append(out, row.Code)
	}
	return out
}

func (e *env) api(token, method, path string, body any) reply {
	e.t.Helper()
	e.client.Token = token
	defer func() { e.client.Token = "" }()
	res := e.client.Do(method, path, body)
	return reply{Status: res.Status, Raw: res.Body}
}

// raw makes a request with no contract checks, for the requests the contract forbids: the
// server's own validation is what has to answer them.
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

// person adds a person to an organization with a web role ("" for none) and returns a token
// for them. A REGION_ADMIN points at the region, which must exist.
func (e *env) person(org int64, role, region string) string {
	e.t.Helper()
	e.userID++
	var personnelID int64
	e.inTenant(org, func(tx pgx.Tx) {
		require.NoError(e.t, tx.QueryRow(e.t.Context(),
			"INSERT INTO personnels (personel_user_id, full_name, status) VALUES ($1, 'Admin Test', 'ACTIVE') RETURNING id",
			e.userID).Scan(&personnelID))
		if role != "" {
			_, err := tx.Exec(e.t.Context(),
				"INSERT INTO web_role_assignments (personnel_id, role, region_code) VALUES ($1, $2::web_role, NULLIF($3, ''))",
				personnelID, role, region)
			require.NoError(e.t, err)
		}
	})
	return testkit.Token(e.t, org, personnelID)
}

func (e *env) inTenant(org int64, fn func(tx pgx.Tx)) {
	e.t.Helper()
	require.NoError(e.t, database.WithTenantTx(e.t.Context(), testkit.Pool(e.t), org, func(tx pgx.Tx) error {
		fn(tx)
		return nil
	}))
}

func (e *env) auditRows(org int64) (rows []string) {
	e.t.Helper()
	e.inTenant(org, func(tx pgx.Tx) {
		r, err := tx.Query(e.t.Context(),
			"SELECT action || ' ' || entity_type || ' ' || coalesce(entity_code, '') FROM audit_logs ORDER BY id")
		require.NoError(e.t, err)
		rows, err = pgx.CollectRows(r, pgx.RowTo[string])
		require.NoError(e.t, err)
	})
	return rows
}

func (e *env) create(token, code, description string) reply {
	e.t.Helper()
	return e.api(token, http.MethodPost, "/admin/regions", map[string]any{"code": code, "description": description})
}

func TestAnOrgAdminManagesRegions(t *testing.T) {
	e := newEnv(t)
	org := testkit.NewOrg(t)
	admin := e.person(org, "ORG_ADMIN", "")

	t.Run("create, then read back", func(t *testing.T) {
		res := e.create(admin, "OS1", "Outsourcing 1")
		require.Equal(t, http.StatusCreated, res.Status, "body: %s", res.Raw)
		require.Equal(t, regionData{Code: "OS1", Description: "Outsourcing 1", IsActive: true}, data[regionData](t, res))

		got := e.api(admin, http.MethodGet, "/admin/regions/OS1", nil)
		require.Equal(t, http.StatusOK, got.Status)
		require.Equal(t, "Outsourcing 1", data[regionData](t, got).Description)
	})

	t.Run("the list is in number order, not text order", func(t *testing.T) {
		require.Equal(t, http.StatusCreated, e.create(admin, "OS10", "Outsourcing 10").Status)
		require.Equal(t, http.StatusCreated, e.create(admin, "OS2", "Outsourcing 2").Status)
		list := e.api(admin, http.MethodGet, "/admin/regions", nil)
		require.Equal(t, []string{"OS1", "OS2", "OS10"}, codesOf(t, list))
		require.Contains(t, string(list.Raw), `"total":3`)
	})

	t.Run("the list filters by search text and by active, and pages", func(t *testing.T) {
		require.Equal(t, []string{"OS2"}, codesOf(t, e.raw(admin, http.MethodGet, "/admin/regions?q=outsourcing%202", nil)))
		require.Equal(t, []string{"OS1", "OS10"}, codesOf(t, e.raw(admin, http.MethodGet, "/admin/regions?q=os1", nil)), "the code is searched too")

		page := e.raw(admin, http.MethodGet, "/admin/regions?limit=2&page=2", nil)
		require.Equal(t, []string{"OS10"}, codesOf(t, page))
		require.Contains(t, string(page.Raw), `"total":3`, "the total ignores the page")
	})

	t.Run("the description and the active flag change; the list follows", func(t *testing.T) {
		res := e.api(admin, http.MethodPatch, "/admin/regions/OS2", map[string]any{"description": "Outsourcing Dua", "is_active": false})
		require.Equal(t, http.StatusOK, res.Status, "body: %s", res.Raw)
		require.Equal(t, regionData{Code: "OS2", Description: "Outsourcing Dua", IsActive: false}, data[regionData](t, res))

		require.Equal(t, []string{"OS1", "OS10"}, codesOf(t, e.raw(admin, http.MethodGet, "/admin/regions?is_active=true", nil)))
		require.Equal(t, []string{"OS2"}, codesOf(t, e.raw(admin, http.MethodGet, "/admin/regions?is_active=false", nil)))
		require.Equal(t, http.StatusUnprocessableEntity, e.raw(admin, http.MethodGet, "/admin/regions?is_active=maybe", nil).Status)
	})

	t.Run("a patch with one field leaves the other alone", func(t *testing.T) {
		res := e.api(admin, http.MethodPatch, "/admin/regions/OS1", map[string]any{"is_active": false})
		require.Equal(t, regionData{Code: "OS1", Description: "Outsourcing 1", IsActive: false}, data[regionData](t, res))
	})

	t.Run("a region that does not exist is REGION_NOT_FOUND", func(t *testing.T) {
		for _, call := range []struct {
			method string
			body   any
		}{{http.MethodGet, nil}, {http.MethodPatch, map[string]any{"description": "x"}}, {http.MethodDelete, nil}} {
			res := e.api(admin, call.method, "/admin/regions/OS99", call.body)
			code, reason := res.reason(t)
			require.Equal(t, http.StatusNotFound, res.Status, call.method)
			require.Equal(t, "NOT_FOUND", code)
			require.Equal(t, organization.ReasonRegionNotFound, reason)
		}
	})

	t.Run("every change is audited by code, with before and after", func(t *testing.T) {
		require.Equal(t, []string{
			"region.create region OS1", "region.create region OS10", "region.create region OS2",
			"region.update region OS2", "region.update region OS1",
		}, e.auditRows(org))
		var metadata string
		e.inTenant(org, func(tx pgx.Tx) {
			require.NoError(t, tx.QueryRow(t.Context(),
				"SELECT metadata::text FROM audit_logs WHERE action = 'region.update' AND entity_code = 'OS2'").Scan(&metadata))
		})
		require.JSONEq(t, `{"before":{"description":"Outsourcing 2","active":true},"after":{"description":"Outsourcing Dua","active":false}}`, metadata)
	})
}

func TestRegionInputIsValidated(t *testing.T) {
	e := newEnv(t)
	admin := e.person(testkit.NewOrg(t), "ORG_ADMIN", "")

	for name, code := range map[string]string{
		"lower case":      "os1",
		"a leading zero":  "OS01",
		"zero":            "OS0",
		"no number":       "OS",
		"another prefix":  "XX1",
		"a branch code":   "1K",
		"trailing text":   "OS1x",
		"a negative sign": "OS-1",
	} {
		// Sent raw: the contract's pattern would refuse these before the server sees them.
		res := e.raw(admin, http.MethodPost, "/admin/regions", map[string]any{"code": code, "description": "Outsourcing"})
		require.Equal(t, http.StatusUnprocessableEntity, res.Status, "%s: %s", name, res.Raw)
		got, _ := res.reason(t)
		require.Equal(t, "VALIDATION_ERROR", got, name)
	}

	for name, body := range map[string]map[string]any{
		"no description":   {"code": "OS1"},
		"a blank name":     {"code": "OS1", "description": "   "},
		"a name too long":  {"code": "OS1", "description": strings.Repeat("x", 201)},
		"an unknown field": {"code": "OS1", "description": "x", "name": "y"},
		"a numeric id":     {"id": 4, "code": "OS1", "description": "x"},
	} {
		res := e.raw(admin, http.MethodPost, "/admin/regions", body)
		require.Equal(t, http.StatusUnprocessableEntity, res.Status, "%s: %s", name, res.Raw)
	}

	t.Run("the code cannot be changed", func(t *testing.T) {
		require.Equal(t, http.StatusCreated, e.create(admin, "OS1", "Outsourcing 1").Status)
		res := e.raw(admin, http.MethodPatch, "/admin/regions/OS1", map[string]any{"code": "OS2"})
		require.Equal(t, http.StatusUnprocessableEntity, res.Status, "body: %s", res.Raw)
		require.Equal(t, "Outsourcing 1", data[regionData](t, e.api(admin, http.MethodGet, "/admin/regions/OS1", nil)).Description)
	})

	t.Run("a blank description is refused on a change too", func(t *testing.T) {
		require.Equal(t, http.StatusUnprocessableEntity,
			e.raw(admin, http.MethodPatch, "/admin/regions/OS1", map[string]any{"description": "  "}).Status)
	})
}

func TestACodeIsNeverHandedOutTwice(t *testing.T) {
	e := newEnv(t)
	org := testkit.NewOrg(t)
	admin := e.person(org, "ORG_ADMIN", "")
	require.Equal(t, http.StatusCreated, e.create(admin, "OS1", "Outsourcing 1").Status)

	dup := e.create(admin, "OS1", "Another")
	require.Equal(t, http.StatusConflict, dup.Status)
	code, reason := dup.reason(t)
	require.Equal(t, "DUPLICATE", code)
	require.Equal(t, organization.ReasonCodeTaken, reason)

	// Deleted, the region is gone from every read, but its code stays taken: records made
	// under it still point at it.
	require.Equal(t, http.StatusOK, e.api(admin, http.MethodDelete, "/admin/regions/OS1", nil).Status)
	require.Equal(t, http.StatusNotFound, e.api(admin, http.MethodGet, "/admin/regions/OS1", nil).Status)
	require.Empty(t, codesOf(t, e.api(admin, http.MethodGet, "/admin/regions", nil)))
	again := e.create(admin, "OS1", "Reborn")
	require.Equal(t, http.StatusConflict, again.Status)
	_, reason = again.reason(t)
	require.Equal(t, organization.ReasonCodeTaken, reason)

	require.Equal(t, http.StatusNotFound, e.api(admin, http.MethodDelete, "/admin/regions/OS1", nil).Status, "a second delete finds nothing")
	require.Contains(t, e.auditRows(org), "region.delete region OS1")
}

func TestARegionAdminReachesOnlyTheirRegion(t *testing.T) {
	e := newEnv(t)
	org := testkit.NewOrg(t)
	admin := e.person(org, "ORG_ADMIN", "")
	require.Equal(t, http.StatusCreated, e.create(admin, "OS1", "Outsourcing 1").Status)
	require.Equal(t, http.StatusCreated, e.create(admin, "OS2", "Outsourcing 2").Status)
	regionAdmin := e.person(org, "REGION_ADMIN", "OS1")

	t.Run("the list shows only their own region", func(t *testing.T) {
		list := e.api(regionAdmin, http.MethodGet, "/admin/regions", nil)
		require.Equal(t, []string{"OS1"}, codesOf(t, list))
		require.Contains(t, string(list.Raw), `"total":1`)
	})

	t.Run("they read their own and are refused another", func(t *testing.T) {
		require.Equal(t, http.StatusOK, e.api(regionAdmin, http.MethodGet, "/admin/regions/OS1", nil).Status)
		other := e.api(regionAdmin, http.MethodGet, "/admin/regions/OS2", nil)
		require.Equal(t, http.StatusForbidden, other.Status)
		code, reason := other.reason(t)
		require.Equal(t, "SCOPE_DENIED", code)
		require.Equal(t, access.ReasonOutsideScope, reason)
		// Whether the other region exists does not change the answer.
		require.Equal(t, http.StatusForbidden, e.api(regionAdmin, http.MethodGet, "/admin/regions/OS99", nil).Status)
	})

	t.Run("they cannot create, change or delete", func(t *testing.T) {
		for _, res := range []reply{
			e.create(regionAdmin, "OS3", "Outsourcing 3"),
			e.api(regionAdmin, http.MethodPatch, "/admin/regions/OS1", map[string]any{"description": "mine now"}),
			e.api(regionAdmin, http.MethodDelete, "/admin/regions/OS1", nil),
		} {
			require.Equal(t, http.StatusForbidden, res.Status, "body: %s", res.Raw)
			code, reason := res.reason(t)
			require.Equal(t, "ROLE_DENIED", code)
			require.Equal(t, access.ReasonOrgAdminOnly, reason)
		}
		require.Equal(t, "Outsourcing 1", data[regionData](t, e.api(admin, http.MethodGet, "/admin/regions/OS1", nil)).Description)
	})

	t.Run("a region with an admin pointing at it cannot be deleted", func(t *testing.T) {
		res := e.api(admin, http.MethodDelete, "/admin/regions/OS1", nil)
		require.Equal(t, http.StatusConflict, res.Status)
		_, reason := res.reason(t)
		require.Equal(t, organization.ReasonHasActiveChildren, reason)
		require.Equal(t, http.StatusOK, e.api(admin, http.MethodGet, "/admin/regions/OS1", nil).Status, "still there")

		// OS2 has nobody pointing at it.
		require.Equal(t, http.StatusOK, e.api(admin, http.MethodDelete, "/admin/regions/OS2", nil).Status)
	})
}

func TestAFieldPersonHasNoAccess(t *testing.T) {
	e := newEnv(t)
	org := testkit.NewOrg(t)
	field := e.person(org, "", "")

	for _, res := range []reply{
		e.api(field, http.MethodGet, "/admin/regions", nil),
		e.create(field, "OS1", "Outsourcing 1"),
	} {
		require.Equal(t, http.StatusForbidden, res.Status)
		_, reason := res.reason(t)
		require.Equal(t, access.ReasonNotAdmin, reason)
	}
}

func TestRegionsStayInsideTheirOrganization(t *testing.T) {
	e := newEnv(t)
	orgA, orgB := testkit.NewOrg(t), testkit.NewOrg(t)
	adminA, adminB := e.person(orgA, "ORG_ADMIN", ""), e.person(orgB, "ORG_ADMIN", "")
	require.Equal(t, http.StatusCreated, e.create(adminA, "OS1", "Outsourcing A").Status)

	require.Empty(t, codesOf(t, e.api(adminB, http.MethodGet, "/admin/regions", nil)), "B does not see A's regions")
	require.Equal(t, http.StatusNotFound, e.api(adminB, http.MethodGet, "/admin/regions/OS1", nil).Status)
	require.Equal(t, http.StatusNotFound, e.api(adminB, http.MethodPatch, "/admin/regions/OS1", map[string]any{"description": "stolen"}).Status)
	require.Equal(t, http.StatusNotFound, e.api(adminB, http.MethodDelete, "/admin/regions/OS1", nil).Status)

	// The same code is free in another organization.
	require.Equal(t, http.StatusCreated, e.create(adminB, "OS1", "Outsourcing B").Status)
	require.Equal(t, "Outsourcing A", data[regionData](t, e.api(adminA, http.MethodGet, "/admin/regions/OS1", nil)).Description)
}

func TestAWebRoleCanOnlyPointAtARegionThatExists(t *testing.T) {
	e := newEnv(t)
	org := testkit.NewOrg(t)
	admin := e.person(org, "ORG_ADMIN", "")
	require.Equal(t, http.StatusCreated, e.create(admin, "OS1", "Outsourcing 1").Status)

	var memberID int64
	e.inTenant(org, func(tx pgx.Tx) {
		require.NoError(t, tx.QueryRow(t.Context(),
			"INSERT INTO personnels (personel_user_id, full_name, status) VALUES (99, 'Calon Admin', 'ACTIVE') RETURNING id").Scan(&memberID))
	})
	path := "/admin/web-roles/" + itoa(memberID)

	require.Equal(t, http.StatusOK,
		e.api(admin, http.MethodPut, path, map[string]any{"role": "REGION_ADMIN", "region_code": "OS1"}).Status)

	// A deleted region is gone: nobody can be pointed at it any more.
	require.Equal(t, http.StatusCreated, e.create(admin, "OS2", "Outsourcing 2").Status)
	require.Equal(t, http.StatusOK, e.api(admin, http.MethodDelete, "/admin/regions/OS2", nil).Status)
	res := e.api(admin, http.MethodPut, path, map[string]any{"role": "REGION_ADMIN", "region_code": "OS2"})
	require.Equal(t, http.StatusUnprocessableEntity, res.Status, "body: %s", res.Raw)
	require.Contains(t, string(res.Raw), `"region_code"`)

	// And the database refuses what the API would have: a role without a region.
	err := database.WithTenantTx(t.Context(), testkit.Pool(t), org, func(tx pgx.Tx) error {
		_, err := tx.Exec(t.Context(), "UPDATE web_role_assignments SET region_code = NULL WHERE personnel_id = $1", memberID)
		return err
	})
	require.Error(t, err, "a REGION_ADMIN must keep a region")
}

func itoa(v int64) string {
	b, _ := json.Marshal(v)
	return string(b)
}
