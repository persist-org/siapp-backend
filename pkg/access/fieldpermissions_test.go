package access_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"

	"siapp/pkg/access"
	"siapp/pkg/platform/auth"
	"siapp/pkg/platform/clock"
	"siapp/pkg/platform/database"
	"siapp/pkg/platform/testkit"
)

func TestMain(m *testing.M) { os.Exit(testkit.NewDatabase(m)) }

// catalogSize is the permissions of the catalog; rowsPerOrg is what a seeded organization holds.
var (
	catalogSize = len(access.Catalog())
	rowsPerOrg  = catalogSize * len(access.TeamRoles())
)

type env struct {
	t      *testing.T
	srv    string
	client *testkit.Client
	userID int64
}

// newEnv serves the admin group (RequireAdmin, the web roles and the field permissions) on a
// clean database, and checks every request and response against the contract.
func newEnv(t *testing.T) *env {
	t.Helper()
	testkit.Reset(t)
	pool := testkit.Pool(t)
	handler := access.NewHandler(access.NewWebRoles(pool, clock.System{}), access.NewFieldPermissions(pool, clock.System{}))
	srv := testkit.NewServer(t, testkit.Deps{Mount: func(r chi.Router) {
		r.Route("/api/v1/admin", func(admin chi.Router) {
			admin.Use(access.RequireAdmin(pool))
			handler.MountAdmin(admin)
		})
	}})
	return &env{t: t, srv: srv.URL, client: testkit.NewClient(t, srv)}
}

type reply struct {
	Status int
	Raw    []byte
}

func (r reply) failure(t *testing.T) (code, reason string, fields map[string]string) {
	t.Helper()
	var body struct {
		Error struct {
			Code    string         `json:"code"`
			Details map[string]any `json:"details"`
			Errors  []struct {
				Field string `json:"field"`
				Code  string `json:"code"`
			} `json:"errors"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal(r.Raw, &body), "body: %s", r.Raw)
	reason, _ = body.Error.Details["reason"].(string)
	fields = map[string]string{}
	for _, f := range body.Error.Errors {
		fields[f.Field] = f.Code
	}
	return body.Error.Code, reason, fields
}

type grantData struct {
	Permission string `json:"permission"`
	Scope      string `json:"scope"`
}

type roleData struct {
	TeamRole string      `json:"team_role"`
	Grants   []grantData `json:"grants"`
}

func (r roleData) scopes() map[string]string {
	out := map[string]string{}
	for _, g := range r.Grants {
		out[g.Permission] = g.Scope
	}
	return out
}

type matrixData struct {
	Catalog []struct {
		Permission    string            `json:"permission"`
		Module        string            `json:"module"`
		Description   string            `json:"description"`
		BusinessLines []string          `json:"business_lines"`
		Defaults      map[string]string `json:"defaults"`
	} `json:"catalog"`
	Roles []roleData `json:"roles"`
}

func (m matrixData) role(t *testing.T, name string) roleData {
	t.Helper()
	for _, r := range m.Roles {
		if r.TeamRole == name {
			return r
		}
	}
	require.Failf(t, "role not in the matrix", "%s", name)
	return roleData{}
}

func data[T any](t *testing.T, r reply) T {
	t.Helper()
	require.Equal(t, http.StatusOK, r.Status, "body: %s", r.Raw)
	var body struct {
		Data T `json:"data"`
	}
	require.NoError(t, json.Unmarshal(r.Raw, &body), "body: %s", r.Raw)
	return body.Data
}

// api sends a request the contract allows and checks the answer against the contract too.
func (e *env) api(token, method, path string, body any) reply {
	e.t.Helper()
	e.client.Token = token
	defer func() { e.client.Token = "" }()
	res := e.client.Do(method, path, body)
	return reply{Status: res.Status, Raw: res.Body}
}

// raw sends anything, with no contract checks: for requests the contract forbids, where the
// server's own validation has to answer.
func (e *env) raw(token, method, path string, body any) reply {
	e.t.Helper()
	var reader io.Reader
	if body != nil {
		switch b := body.(type) {
		case []byte:
			reader = bytes.NewReader(b)
		default:
			encoded, err := json.Marshal(body)
			require.NoError(e.t, err)
			reader = bytes.NewReader(encoded)
		}
	}
	req, err := http.NewRequestWithContext(e.t.Context(), method, e.srv+"/api/v1"+path, reader)
	require.NoError(e.t, err)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	res, err := http.DefaultClient.Do(req)
	require.NoError(e.t, err)
	defer func() { _ = res.Body.Close() }()
	out, err := io.ReadAll(res.Body)
	require.NoError(e.t, err)
	if res.StatusCode >= http.StatusBadRequest {
		testkit.ValidateSchema(e.t, "ErrorEnvelope", out)
	}
	return reply{Status: res.StatusCode, Raw: out}
}

func (e *env) inTenant(org int64, fn func(tx pgx.Tx)) {
	e.t.Helper()
	require.NoError(e.t, database.WithTenantTx(e.t.Context(), testkit.Pool(e.t), org, func(tx pgx.Tx) error {
		fn(tx)
		return nil
	}))
}

// person adds a person to an organization with a web role ("" for none, so field staff) and
// returns a token for them. A REGION_ADMIN is of OS1, which is created.
func (e *env) person(org int64, role string) string {
	e.t.Helper()
	token, _ := e.personWithID(org, role)
	return token
}

// personWithID is person, and also returns the personnel id.
func (e *env) personWithID(org int64, role string) (token string, personnelID int64) {
	e.t.Helper()
	e.userID++
	e.inTenant(org, func(tx pgx.Tx) {
		require.NoError(e.t, tx.QueryRow(e.t.Context(),
			"INSERT INTO personnels (personel_user_id, full_name, status) VALUES ($1, 'Admin Test', 'ACTIVE') RETURNING id",
			e.userID).Scan(&personnelID))
		switch role {
		case "":
		case access.RoleRegionAdmin:
			_, err := tx.Exec(e.t.Context(), "INSERT INTO regions (code, description) VALUES ('OS1', 'Outsourcing 1') ON CONFLICT DO NOTHING")
			require.NoError(e.t, err)
			_, err = tx.Exec(e.t.Context(), "INSERT INTO web_role_assignments (personnel_id, role, region_code) VALUES ($1, 'REGION_ADMIN', 'OS1')", personnelID)
			require.NoError(e.t, err)
		default:
			_, err := tx.Exec(e.t.Context(), "INSERT INTO web_role_assignments (personnel_id, role) VALUES ($1, $2::web_role)", personnelID, role)
			require.NoError(e.t, err)
		}
	})
	return testkit.Token(e.t, org, personnelID), personnelID
}

func (e *env) matrix(token string) matrixData {
	e.t.Helper()
	return data[matrixData](e.t, e.api(token, http.MethodGet, "/admin/field-permissions", nil))
}

func (e *env) count(org int64, query string, args ...any) (n int) {
	e.t.Helper()
	e.inTenant(org, func(tx pgx.Tx) {
		require.NoError(e.t, tx.QueryRow(e.t.Context(), query, args...).Scan(&n))
	})
	return n
}

// scopeInDB reads one stored scope.
func (e *env) scopeInDB(org int64, role, permission string) (scope string, found bool) {
	e.t.Helper()
	e.inTenant(org, func(tx pgx.Tx) {
		err := tx.QueryRow(e.t.Context(),
			"SELECT scope::text FROM role_permissions WHERE team_role = $1::team_role AND permission = $2", role, permission).Scan(&scope)
		found = err == nil
		if err != nil {
			require.ErrorIs(e.t, err, pgx.ErrNoRows)
		}
	})
	return scope, found
}

// grantsBody is the full list of a role's grants: the catalog's defaults with the changes on top.
func grantsBody(role access.TeamRole, changes map[access.Permission]access.Scope) map[string]any {
	grants := make([]map[string]any, 0, catalogSize)
	for _, e := range access.Catalog() {
		scope := e.Defaults[role]
		if s, ok := changes[e.Key]; ok {
			scope = s
		}
		grants = append(grants, map[string]any{"permission": string(e.Key), "scope": string(scope)})
	}
	return map[string]any{"grants": grants}
}

// grantList is a role's defaults as the usecase takes them.
func grantList(role access.TeamRole) []access.Grant {
	grants := make([]access.Grant, 0, catalogSize)
	for _, e := range access.Catalog() {
		grants = append(grants, access.Grant{Permission: e.Key, Scope: e.Defaults[role]})
	}
	return grants
}

// uniformBody gives every permission one scope.
func uniformBody(scope access.Scope) map[string]any {
	grants := make([]map[string]any, 0, catalogSize)
	for _, e := range access.Catalog() {
		grants = append(grants, map[string]any{"permission": string(e.Key), "scope": string(scope)})
	}
	return map[string]any{"grants": grants}
}

func (e *env) audits(org int64) (rows []string) {
	e.t.Helper()
	e.inTenant(org, func(tx pgx.Tx) {
		r, err := tx.Query(e.t.Context(), "SELECT action || ' ' || entity_type || ' ' || coalesce(entity_code, '') FROM audit_logs ORDER BY id")
		require.NoError(e.t, err)
		rows, err = pgx.CollectRows(r, pgx.RowTo[string])
		require.NoError(e.t, err)
	})
	return rows
}

func TestAnOrganizationThatIsReadForTheFirstTimeGetsTheDefaults(t *testing.T) {
	e := newEnv(t)
	org := testkit.NewOrg(t)
	admin := e.person(org, access.RoleOrgAdmin)
	require.Equal(t, 0, e.count(org, "SELECT count(*) FROM role_permissions"), "nothing is seeded before the first read")

	res := e.api(admin, http.MethodGet, "/admin/field-permissions", nil)
	m := data[matrixData](t, res)

	require.Len(t, m.Catalog, catalogSize)
	require.Len(t, m.Roles, 2)
	require.Equal(t, []string{"DANRU", "MEMBER"}, []string{m.Roles[0].TeamRole, m.Roles[1].TeamRole})
	for i, entry := range access.Catalog() {
		def := m.Catalog[i]
		require.Equal(t, string(entry.Key), def.Permission, "the catalog keeps its order")
		require.NotEmpty(t, def.Module)
		require.NotEmpty(t, def.Description)
		require.Equal(t, string(entry.Defaults[access.RoleDanru]), def.Defaults["DANRU"], entry.Key)
		require.Equal(t, string(entry.Defaults[access.RoleMember]), def.Defaults["MEMBER"], entry.Key)
		for _, role := range m.Roles {
			require.Equal(t, string(entry.Key), role.Grants[i].Permission)
			require.Equal(t, def.Defaults[role.TeamRole], role.Grants[i].Scope, "%s for %s starts at its default", entry.Key, role.TeamRole)
		}
	}
	require.Equal(t, rowsPerOrg, e.count(org, "SELECT count(*) FROM role_permissions"))

	t.Run("a business line is listed, and an empty list means every line", func(t *testing.T) {
		for _, def := range m.Catalog {
			if def.Permission == "ememo.use" {
				require.Equal(t, []string{"cleaning"}, def.BusinessLines)
				continue
			}
			require.NotNil(t, def.BusinessLines, def.Permission)
			require.Empty(t, def.BusinessLines, def.Permission)
		}
		require.Contains(t, string(res.Raw), `"business_lines":[]`, "an empty list is [], never null")
	})

	t.Run("none is a stored value, so every pair has a row", func(t *testing.T) {
		scope, found := e.scopeInDB(org, "MEMBER", "attendance.leave.approve")
		require.True(t, found)
		require.Equal(t, "NONE", scope)
	})

	t.Run("reading again changes nothing", func(t *testing.T) {
		before := e.count(org, "SELECT count(*) FROM role_permissions WHERE updated_at = created_at")
		require.Equal(t, m, e.matrix(admin))
		require.Equal(t, rowsPerOrg, e.count(org, "SELECT count(*) FROM role_permissions"))
		require.Equal(t, before, e.count(org, "SELECT count(*) FROM role_permissions WHERE updated_at = created_at"))
		require.Empty(t, e.audits(org), "a read, and a seed, write no audit row")
	})
}

func TestReplacingTheGrantsOfARole(t *testing.T) {
	e := newEnv(t)
	org := testkit.NewOrg(t)
	admin := e.person(org, access.RoleOrgAdmin)
	before := e.matrix(admin)

	changes := map[access.Permission]access.Scope{
		access.AttendanceLeaveApprove: access.ScopeNone,    // was TEAM
		access.ReportView:             access.ScopeTeam,    // was PROJECT
		access.CCTVView:               access.ScopeProject, // already PROJECT: not a change
		access.AttendanceQRDisplay:    access.ScopeProject, // was TEAM
	}
	res := e.api(admin, http.MethodPut, "/admin/field-permissions/DANRU", grantsBody(access.RoleDanru, changes))
	require.Equal(t, http.StatusOK, res.Status, "body: %s", res.Raw)
	got := data[roleData](t, res)

	require.Equal(t, "DANRU", got.TeamRole)
	require.Len(t, got.Grants, catalogSize)
	for _, entry := range access.Catalog() {
		want := entry.Defaults[access.RoleDanru]
		if s, ok := changes[entry.Key]; ok {
			want = s
		}
		require.Equal(t, string(want), got.scopes()[string(entry.Key)], entry.Key)
	}

	t.Run("the matrix shows it, and the other role is untouched", func(t *testing.T) {
		after := e.matrix(admin)
		require.Equal(t, got, after.role(t, "DANRU"))
		require.Equal(t, before.role(t, "MEMBER"), after.role(t, "MEMBER"))
		require.Equal(t, before.Catalog, after.Catalog, "the catalog and its defaults never move")
		scope, _ := e.scopeInDB(org, "DANRU", "attendance.leave.approve")
		require.Equal(t, "NONE", scope, "taking a permission away is a stored NONE")
	})

	t.Run("only what changed was written, and it is audited with the old and new scope", func(t *testing.T) {
		require.Equal(t, 3, e.count(org, "SELECT count(*) FROM role_permissions WHERE updated_at > created_at"))
		require.Equal(t, []string{"field_permissions.replace field_permissions DANRU"}, e.audits(org))

		var metadata map[string]any
		e.inTenant(org, func(tx pgx.Tx) {
			var raw []byte
			require.NoError(t, tx.QueryRow(t.Context(), "SELECT metadata FROM audit_logs").Scan(&raw))
			require.NoError(t, json.Unmarshal(raw, &metadata))
		})
		require.Equal(t, "DANRU", metadata["team_role"])
		require.ElementsMatch(t, []any{
			map[string]any{"permission": "attendance.leave.approve", "before": "TEAM", "after": "NONE"},
			map[string]any{"permission": "report.view", "before": "PROJECT", "after": "TEAM"},
			map[string]any{"permission": "attendance.qr.display", "before": "TEAM", "after": "PROJECT"},
		}, metadata["changes"])
	})

	t.Run("sending the same list again changes nothing and audits nothing", func(t *testing.T) {
		again := e.api(admin, http.MethodPut, "/admin/field-permissions/DANRU", grantsBody(access.RoleDanru, changes))
		require.Equal(t, got, data[roleData](t, again))
		require.Len(t, e.audits(org), 1)
		require.Equal(t, 3, e.count(org, "SELECT count(*) FROM role_permissions WHERE updated_at > created_at"))
	})

	t.Run("every scope can be set, and set back", func(t *testing.T) {
		for _, scope := range []access.Scope{access.ScopeProject, access.ScopeSelf, access.ScopeNone, access.ScopeTeam} {
			res := e.api(admin, http.MethodPut, "/admin/field-permissions/MEMBER", uniformBody(scope))
			require.Equal(t, http.StatusOK, res.Status, "body: %s", res.Raw)
			for _, g := range data[roleData](t, res).Grants {
				require.Equal(t, string(scope), g.Scope, g.Permission)
			}
		}
	})
}

func TestAReSeedKeepsWhatAnAdminChanged(t *testing.T) {
	e := newEnv(t)
	org := testkit.NewOrg(t)
	admin := e.person(org, access.RoleOrgAdmin)
	e.matrix(admin)

	// ⚠ The admin takes a permission away from DANRU and gives MEMBER a wider one.
	changes := map[access.Permission]access.Scope{access.AttendanceLeaveApprove: access.ScopeNone}
	require.Equal(t, http.StatusOK, e.api(admin, http.MethodPut, "/admin/field-permissions/DANRU", grantsBody(access.RoleDanru, changes)).Status)
	member := map[access.Permission]access.Scope{access.CCTVView: access.ScopeSelf}
	require.Equal(t, http.StatusOK, e.api(admin, http.MethodPut, "/admin/field-permissions/MEMBER", grantsBody(access.RoleMember, member)).Status)
	edited := e.matrix(admin)

	pool := testkit.Pool(t)
	added, err := access.EnsureDefaults(t.Context(), pool, clock.System{}, org)
	require.NoError(t, err)
	require.Zero(t, added, "nothing is missing, so nothing is written")
	require.Equal(t, edited, e.matrix(admin), "the removal of a permission survives a re-seed")

	t.Run("a permission the catalog gains later reaches an organization seeded before", func(t *testing.T) {
		// Rows of a key the organization never had, as if the release had just added it.
		e.inTenant(org, func(tx pgx.Tx) {
			_, err := tx.Exec(t.Context(), "DELETE FROM role_permissions WHERE permission IN ('cctv.view', 'asset.report')")
			require.NoError(t, err)
		})
		added, err := access.EnsureDefaults(t.Context(), pool, clock.System{}, org)
		require.NoError(t, err)
		require.EqualValues(t, 4, added, "two keys, both roles")

		after := e.matrix(admin)
		require.Equal(t, "PROJECT", after.role(t, "MEMBER").scopes()["cctv.view"], "the missing row gets its default again")
		require.Equal(t, "NONE", after.role(t, "DANRU").scopes()["attendance.leave.approve"], "the admin's edit is still there")
		require.Equal(t, rowsPerOrg, e.count(org, "SELECT count(*) FROM role_permissions"))
	})

	t.Run("seeding twice at once adds each row once", func(t *testing.T) {
		fresh := testkit.NewOrg(t)
		const callers = 8
		var wg sync.WaitGroup
		results := make(chan int64, callers)
		errs := make(chan error, callers)
		for range callers {
			wg.Go(func() {
				n, err := access.EnsureDefaults(t.Context(), pool, clock.System{}, fresh)
				results <- n
				errs <- err
			})
		}
		wg.Wait()
		close(results)
		close(errs)
		for err := range errs {
			require.NoError(t, err)
		}
		var total int64
		for n := range results {
			total += n
		}
		require.EqualValues(t, rowsPerOrg, total, "every row was added by exactly one caller")
		require.Equal(t, rowsPerOrg, e.count(fresh, "SELECT count(*) FROM role_permissions"))
	})
}

func TestRefusedGrantListsWriteNothing(t *testing.T) {
	e := newEnv(t)
	org := testkit.NewOrg(t)
	admin := e.person(org, access.RoleOrgAdmin)
	seeded := e.matrix(admin)

	body := func(mutate func(grants []map[string]any) []map[string]any) map[string]any {
		grants := grantsBody(access.RoleDanru, map[access.Permission]access.Scope{access.AttendanceLeaveApprove: access.ScopeNone})["grants"].([]map[string]any)
		return map[string]any{"grants": mutate(grants)}
	}
	tests := []struct {
		name       string
		body       any
		status     int
		code       string
		reason     string
		wantFields map[string]string
	}{
		{
			name: "an unknown permission",
			body: body(func(g []map[string]any) []map[string]any {
				g[3]["permission"] = "attendance.leave.aprove"
				return g
			}),
			status: http.StatusUnprocessableEntity, code: "VALIDATION_ERROR", reason: access.ReasonPermissionUnknown,
			wantFields: map[string]string{"grants[3].permission": "UNKNOWN", "grants": "REQUIRED"},
		},
		{
			name:   "a permission left out",
			body:   body(func(g []map[string]any) []map[string]any { return g[1:] }),
			status: http.StatusUnprocessableEntity, code: "VALIDATION_ERROR",
			wantFields: map[string]string{"grants": "REQUIRED"},
		},
		{
			name:   "a permission sent twice",
			body:   body(func(g []map[string]any) []map[string]any { return append(g, g[0]) }),
			status: http.StatusUnprocessableEntity, code: "VALIDATION_ERROR",
			wantFields: map[string]string{fmt.Sprintf("grants[%d].permission", catalogSize): "DUPLICATE"},
		},
		{
			name: "a scope that is not one",
			body: body(func(g []map[string]any) []map[string]any {
				g[5]["scope"] = "EVERYTHING"
				return g
			}),
			status: http.StatusUnprocessableEntity, code: "VALIDATION_ERROR",
			wantFields: map[string]string{"grants[5].scope": "INVALID"},
		},
		{
			name: "a lower-case scope",
			body: body(func(g []map[string]any) []map[string]any {
				g[5]["scope"] = "team"
				return g
			}),
			status: http.StatusUnprocessableEntity, code: "VALIDATION_ERROR",
			wantFields: map[string]string{"grants[5].scope": "INVALID"},
		},
		{
			name: "a grant without a scope",
			body: body(func(g []map[string]any) []map[string]any {
				delete(g[0], "scope")
				return g
			}),
			status: http.StatusUnprocessableEntity, code: "VALIDATION_ERROR",
			wantFields: map[string]string{"grants[0].scope": "REQUIRED"},
		},
		{
			name: "a permission far too long",
			body: body(func(g []map[string]any) []map[string]any {
				g[0]["permission"] = strings.Repeat("a", 65)
				return g
			}),
			status: http.StatusUnprocessableEntity, code: "VALIDATION_ERROR",
			wantFields: map[string]string{"grants[0].permission": "TOO_LONG"},
		},
		{
			name: "a list far too long",
			body: body(func(g []map[string]any) []map[string]any {
				for len(g) <= 200 {
					g = append(g, g[0])
				}
				return g
			}),
			status: http.StatusUnprocessableEntity, code: "VALIDATION_ERROR",
			wantFields: map[string]string{"grants": "TOO_LONG"},
		},
		{
			name:   "an empty list",
			body:   map[string]any{"grants": []any{}},
			status: http.StatusUnprocessableEntity, code: "VALIDATION_ERROR",
			wantFields: map[string]string{"grants": "REQUIRED"},
		},
		{
			name:   "no list",
			body:   map[string]any{},
			status: http.StatusUnprocessableEntity, code: "VALIDATION_ERROR",
			wantFields: map[string]string{"grants": "REQUIRED"},
		},
		{
			name:   "a null list",
			body:   map[string]any{"grants": nil},
			status: http.StatusUnprocessableEntity, code: "VALIDATION_ERROR",
			wantFields: map[string]string{"grants": "REQUIRED"},
		},
		{
			name:   "a field nobody knows",
			body:   map[string]any{"grants": []any{}, "team_role": "MEMBER"},
			status: http.StatusUnprocessableEntity, code: "VALIDATION_ERROR",
			wantFields: map[string]string{"team_role": "INVALID"},
		},
		{
			name:   "a grant field nobody knows",
			body:   map[string]any{"grants": []any{map[string]any{"permission": "cctv.view", "scope": "TEAM", "extra": 1}}},
			status: http.StatusUnprocessableEntity, code: "VALIDATION_ERROR",
			wantFields: map[string]string{"extra": "INVALID"},
		},
		{
			name:   "a body that is not JSON",
			body:   []byte(`{"grants": [`),
			status: http.StatusBadRequest, code: "BAD_REQUEST",
		},
		{
			name:   "a NUL in a key",
			body:   []byte(`{"grants":[{"permission":"cctv.view\u0000","scope":"TEAM"}]}`),
			status: http.StatusBadRequest, code: "BAD_REQUEST",
		},
		{
			name:   "two objects",
			body:   []byte(`{"grants":[]}{"grants":[]}`),
			status: http.StatusBadRequest, code: "BAD_REQUEST",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			res := e.raw(admin, http.MethodPut, "/admin/field-permissions/DANRU", tc.body)
			require.Equal(t, tc.status, res.Status, "body: %s", res.Raw)
			code, reason, fields := res.failure(t)
			require.Equal(t, tc.code, code)
			require.Equal(t, tc.reason, reason)
			for field, fieldCode := range tc.wantFields {
				require.Equal(t, fieldCode, fields[field], "field %s in %v", field, fields)
			}
		})
	}

	t.Run("a body over 1 MB is refused", func(t *testing.T) {
		// Just over the limit: the server drains what is left and answers. A body far over it
		// would be cut off while the client is still writing, and the test would see a reset.
		huge := []byte(`{"grants":[{"permission":"` + strings.Repeat("a", 1<<20) + `","scope":"TEAM"}]}`)
		res := e.raw(admin, http.MethodPut, "/admin/field-permissions/DANRU", huge)
		require.Equal(t, http.StatusRequestEntityTooLarge, res.Status)
	})

	t.Run("an empty body is refused", func(t *testing.T) {
		res := e.raw(admin, http.MethodPut, "/admin/field-permissions/DANRU", nil)
		require.Equal(t, http.StatusBadRequest, res.Status, "body: %s", res.Raw)
	})

	require.Equal(t, seeded, e.matrix(admin), "not one refused list changed the matrix")
	require.Empty(t, e.audits(org))
}

func TestAnUnknownTeamRoleIsNotFound(t *testing.T) {
	e := newEnv(t)
	org := testkit.NewOrg(t)
	admin := e.person(org, access.RoleOrgAdmin)
	for _, role := range []string{"SUPERVISOR", "danru", "Member", "1", "ORG_ADMIN"} {
		res := e.raw(admin, http.MethodPut, "/admin/field-permissions/"+role, grantsBody(access.RoleDanru, nil))
		require.Equal(t, http.StatusNotFound, res.Status, "%s: %s", role, res.Raw)
		code, _, _ := res.failure(t)
		require.Equal(t, "NOT_FOUND", code)
	}
	require.Empty(t, e.audits(org))
}

func TestOnlyAnOrgAdminMayReadOrChangeTheMatrix(t *testing.T) {
	e := newEnv(t)
	org := testkit.NewOrg(t)
	orgAdmin := e.person(org, access.RoleOrgAdmin)
	regionAdmin := e.person(org, access.RoleRegionAdmin)
	fieldStaff := e.person(org, "")
	seeded := e.matrix(orgAdmin)

	routes := []struct {
		name, method, path string
		body               any
	}{
		{"read", http.MethodGet, "/admin/field-permissions", nil},
		{"replace", http.MethodPut, "/admin/field-permissions/DANRU", grantsBody(access.RoleDanru, map[access.Permission]access.Scope{access.CCTVView: access.ScopeNone})},
		{"replace of a role that does not exist", http.MethodPut, "/admin/field-permissions/NOPE", grantsBody(access.RoleDanru, nil)},
	}
	for _, route := range routes {
		t.Run("a REGION_ADMIN may not "+route.name, func(t *testing.T) {
			res := e.raw(regionAdmin, route.method, route.path, route.body)
			require.Equal(t, http.StatusForbidden, res.Status, "body: %s", res.Raw)
			code, reason, _ := res.failure(t)
			require.Equal(t, "ROLE_DENIED", code)
			require.Equal(t, access.ReasonOrgAdminOnly, reason)
		})
		t.Run("field staff may not "+route.name, func(t *testing.T) {
			res := e.raw(fieldStaff, route.method, route.path, route.body)
			require.Equal(t, http.StatusForbidden, res.Status, "body: %s", res.Raw)
			code, reason, _ := res.failure(t)
			require.Equal(t, "FORBIDDEN", code)
			require.Equal(t, access.ReasonNotAdmin, reason)
		})
		t.Run("nobody without a token may "+route.name, func(t *testing.T) {
			require.Equal(t, http.StatusUnauthorized, e.raw("", route.method, route.path, route.body).Status)
		})
	}
	require.Equal(t, seeded, e.matrix(orgAdmin), "none of those changed anything")
	require.Empty(t, e.audits(org))

	t.Run("an admin whose role was revoked after the middleware read it is refused", func(t *testing.T) {
		// RequireAdmin reads the role in a transaction of its own; another ORG_ADMIN may revoke
		// or narrow it before Replace runs. Replace locks and checks it again, so the usecase
		// refuses a caller whose scope says ORG_ADMIN but whose assignment no longer does.
		fields := access.NewFieldPermissions(testkit.Pool(t), clock.System{})
		_, revokedID := e.personWithID(org, "")
		_, narrowedID := e.personWithID(org, access.RoleRegionAdmin)
		stale := access.AdminScope{All: true}

		_, err := fields.Replace(t.Context(), auth.Principal{OrganizationID: org, PersonnelID: revokedID}, stale, access.RoleDanru, grantList(access.RoleDanru))
		require.ErrorIs(t, err, access.ErrNotAdmin, "no assignment left")
		_, err = fields.Replace(t.Context(), auth.Principal{OrganizationID: org, PersonnelID: narrowedID}, stale, access.RoleDanru, grantList(access.RoleDanru))
		require.ErrorIs(t, err, access.ErrOrgAdminOnly, "narrowed to a region")
		require.Equal(t, seeded, e.matrix(orgAdmin))
	})
}

func TestOneOrganizationsEditsDoNotReachAnother(t *testing.T) {
	e := newEnv(t)
	orgA, orgB := testkit.NewOrg(t), testkit.NewOrg(t)
	adminA, adminB := e.person(orgA, access.RoleOrgAdmin), e.person(orgB, access.RoleOrgAdmin)
	defaults := e.matrix(adminB)
	e.matrix(adminA)

	require.Equal(t, http.StatusOK, e.api(adminA, http.MethodPut, "/admin/field-permissions/DANRU", uniformBody(access.ScopeNone)).Status)
	require.Equal(t, http.StatusOK, e.api(adminA, http.MethodPut, "/admin/field-permissions/MEMBER", uniformBody(access.ScopeProject)).Status)

	require.Equal(t, defaults, e.matrix(adminB), "B's matrix is as it was")
	require.Equal(t, 0, e.count(orgB, "SELECT count(*) FROM role_permissions WHERE updated_at > created_at"))
	require.Empty(t, e.audits(orgB))
	require.Equal(t, rowsPerOrg, e.count(orgA, "SELECT count(*) FROM role_permissions"), "each tenant sees only its own rows")
	require.Equal(t, rowsPerOrg, e.count(orgB, "SELECT count(*) FROM role_permissions"))
	require.Equal(t, 2*rowsPerOrg, scalarAsOwner(t, "SELECT count(*) FROM role_permissions"))

	t.Run("a tenant cannot write another's rows", func(t *testing.T) {
		err := database.WithTenantTx(t.Context(), testkit.Pool(t), orgA, func(tx pgx.Tx) error {
			_, err := tx.Exec(t.Context(),
				"INSERT INTO role_permissions (organization_id, team_role, permission, scope) VALUES ($1, 'DANRU', 'cctv.extra', 'TEAM')", orgB)
			return err
		})
		require.Error(t, err, "row-level security refuses a row of another organization")
	})
}

func scalarAsOwner(t *testing.T, query string) (n int) {
	t.Helper()
	require.NoError(t, testkit.OwnerPool(t).QueryRow(t.Context(), query).Scan(&n))
	return n
}

func TestTwoAdminsSavingTheSameRoleAtOnceLeaveOneOfThem(t *testing.T) {
	e := newEnv(t)
	org := testkit.NewOrg(t)
	adminA, adminB := e.person(org, access.RoleOrgAdmin), e.person(org, access.RoleOrgAdmin)
	e.matrix(adminA)

	var wg sync.WaitGroup
	statuses := make(chan int, 2)
	for _, send := range []struct {
		token string
		scope access.Scope
	}{{adminA, access.ScopeSelf}, {adminB, access.ScopeProject}} {
		wg.Go(func() {
			statuses <- e.raw(send.token, http.MethodPut, "/admin/field-permissions/DANRU", uniformBody(send.scope)).Status
		})
	}
	wg.Wait()
	close(statuses)
	for status := range statuses {
		require.Equal(t, http.StatusOK, status, "the second save waits for the first, and neither fails")
	}

	// ⚠ One whole list won, not a mix of the two.
	got := e.matrix(adminA).role(t, "DANRU")
	first := got.Grants[0].Scope
	require.Contains(t, []string{"SELF", "PROJECT"}, first)
	for _, g := range got.Grants {
		require.Equal(t, first, g.Scope, g.Permission)
	}
	require.Len(t, e.audits(org), 2, "both saves changed something, and both are in the log")
}

func TestTheTableRefusesWhatTheCatalogWouldNeverWrite(t *testing.T) {
	newEnv(t)
	org := testkit.NewOrg(t)

	for name, sql := range map[string]string{
		"a key in capitals":        "INSERT INTO role_permissions (team_role, permission, scope) VALUES ('DANRU', 'Cctv.view', 'TEAM')",
		"a key with no dot":        "INSERT INTO role_permissions (team_role, permission, scope) VALUES ('DANRU', 'cctv', 'TEAM')",
		"a key with a space":       "INSERT INTO role_permissions (team_role, permission, scope) VALUES ('DANRU', 'cctv. view', 'TEAM')",
		"a key longer than 64":     "INSERT INTO role_permissions (team_role, permission, scope) VALUES ('DANRU', 'a." + strings.Repeat("b", 70) + "', 'TEAM')",
		"a role that is no role":   "INSERT INTO role_permissions (team_role, permission, scope) VALUES ('BOSS', 'cctv.view', 'TEAM')",
		"a scope that is no scope": "INSERT INTO role_permissions (team_role, permission, scope) VALUES ('DANRU', 'cctv.view', 'ALL')",
		"a scope of null":          "INSERT INTO role_permissions (team_role, permission, scope) VALUES ('DANRU', 'cctv.view', NULL)",
	} {
		err := database.WithTenantTx(t.Context(), testkit.Pool(t), org, func(tx pgx.Tx) error {
			_, err := tx.Exec(t.Context(), sql)
			return err
		})
		require.Error(t, err, name)
	}

	t.Run("one row per role and permission", func(t *testing.T) {
		insert := "INSERT INTO role_permissions (team_role, permission, scope) VALUES ('DANRU', 'cctv.view', 'TEAM')"
		require.NoError(t, database.WithTenantTx(t.Context(), testkit.Pool(t), org, func(tx pgx.Tx) error {
			_, err := tx.Exec(t.Context(), insert)
			return err
		}))
		require.Error(t, database.WithTenantTx(t.Context(), testkit.Pool(t), org, func(tx pgx.Tx) error {
			_, err := tx.Exec(t.Context(), insert)
			return err
		}))
	})
}

func TestARowOfARetiredPermissionIsLeftAlone(t *testing.T) {
	e := newEnv(t)
	org := testkit.NewOrg(t)
	admin := e.person(org, access.RoleOrgAdmin)
	e.matrix(admin)
	// A key an earlier release had and this one dropped: not in the catalog any more.
	e.inTenant(org, func(tx pgx.Tx) {
		_, err := tx.Exec(t.Context(), "INSERT INTO role_permissions (team_role, permission, scope) VALUES ('DANRU', 'retired.feature', 'PROJECT')")
		require.NoError(t, err)
	})

	m := e.matrix(admin)
	require.Len(t, m.role(t, "DANRU").Grants, catalogSize, "the matrix lists the catalog, not the table")
	require.NotContains(t, m.role(t, "DANRU").scopes(), "retired.feature")

	require.Equal(t, http.StatusOK, e.api(admin, http.MethodPut, "/admin/field-permissions/DANRU", grantsBody(access.RoleDanru, map[access.Permission]access.Scope{access.CCTVView: access.ScopeSelf})).Status)
	scope, found := e.scopeInDB(org, "DANRU", "retired.feature")
	require.True(t, found, "a replacement does not delete what it was not told about")
	require.Equal(t, "PROJECT", scope)

	added, err := access.EnsureDefaults(t.Context(), testkit.Pool(t), clock.System{}, org)
	require.NoError(t, err)
	require.Zero(t, added)
	require.Equal(t, rowsPerOrg+1, e.count(org, "SELECT count(*) FROM role_permissions"))
}
