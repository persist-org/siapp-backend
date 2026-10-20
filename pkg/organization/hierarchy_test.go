package organization_test

import (
	"net/http"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"

	"siapp/pkg/access"
	"siapp/pkg/organization"
	"siapp/pkg/platform/clock"
	"siapp/pkg/platform/database"
	"siapp/pkg/platform/testkit"
)

// row is a branch, client, project or team as the API returns it.
type row map[string]any

func (r row) str(key string) string {
	s, _ := r[key].(string)
	return s
}

func (e *env) mustCreate(token, path string, body map[string]any) row {
	e.t.Helper()
	res := e.api(token, http.MethodPost, path, body)
	require.Equal(e.t, http.StatusCreated, res.Status, "POST %s: %s", path, res.Raw)
	return data[row](e.t, res)
}

// expect asserts a refusal: its status, its ErrorCode and its reason.
func expect(t *testing.T, res reply, status int, code, reason string) {
	t.Helper()
	require.Equal(t, status, res.Status, "body: %s", res.Raw)
	gotCode, gotReason := res.reason(t)
	require.Equal(t, code, gotCode)
	require.Equal(t, reason, gotReason)
}

// tree creates region OS{n} with one branch ({n}K), client ({n}K-IBMI) and security project
// ({n}K-IBMI-S) below it, and returns the four codes.
func (e *env) tree(admin string, n int) (region, branch, client, project string) {
	e.t.Helper()
	num := strconv.Itoa(n)
	region = "OS" + num
	require.Equal(e.t, http.StatusCreated, e.create(admin, region, "Outsourcing "+num).Status)
	branch = e.mustCreate(admin, "/admin/branches", map[string]any{
		"region_code": region, "company_letter": "K", "description": "Cabang " + num,
	}).str("code")
	client = e.mustCreate(admin, "/admin/clients", map[string]any{
		"branch_code": branch, "company_prefix": "IBMI", "description": "Klien " + num,
	}).str("code")
	project = e.mustCreate(admin, "/admin/projects", map[string]any{
		"client_code": client, "business_line": "security", "description": "Proyek " + num, "timezone": "Asia/Jakarta",
	}).str("code")
	return region, branch, client, project
}

// execInTenant runs one statement as the application role in an organization, so the table
// constraints answer it and nothing in between.
func execInTenant(t *testing.T, org int64, sql string) error {
	t.Helper()
	return database.WithTenantTx(t.Context(), testkit.Pool(t), org, func(tx pgx.Tx) error {
		_, err := tx.Exec(t.Context(), sql)
		return err
	})
}

func (e *env) newTeam(token, project string) reply {
	e.t.Helper()
	return e.api(token, http.MethodPost, "/admin/teams", map[string]any{"project_code": project, "description": "Regu"})
}

func listedCodes(t *testing.T, res reply) []string {
	t.Helper()
	require.Equal(t, http.StatusOK, res.Status, "body: %s", res.Raw)
	out := []string{}
	for _, r := range data[[]row](t, res) {
		out = append(out, r.str("code"))
	}
	return out
}

func TestTheChainIsBuiltFromCodes(t *testing.T) {
	e := newEnv(t)
	org := testkit.NewOrg(t)
	admin := e.person(org, "ORG_ADMIN", "")

	_, branch, client, project := e.tree(admin, 1)
	require.Equal(t, "1K", branch)
	require.Equal(t, "1K-IBMI", client)
	require.Equal(t, "1K-IBMI-S", project)

	t.Run("each level reads back with its parent's code", func(t *testing.T) {
		b := data[row](t, e.api(admin, http.MethodGet, "/admin/branches/1K", nil))
		require.Equal(t, "OS1", b.str("region_code"))
		require.Equal(t, "K", b.str("company_letter"))
		require.Equal(t, "Cabang 1", b.str("description"))
		require.Equal(t, true, b["is_active"])

		c := data[row](t, e.api(admin, http.MethodGet, "/admin/clients/1K-IBMI", nil))
		require.Equal(t, "1K", c.str("branch_code"))
		require.Equal(t, "IBMI", c.str("company_prefix"))

		p := data[row](t, e.api(admin, http.MethodGet, "/admin/projects/1K-IBMI-S", nil))
		require.Equal(t, "1K-IBMI", p.str("client_code"))
		require.Equal(t, "security", p.str("business_line"))
		require.Equal(t, "Asia/Jakarta", p.str("timezone"))
		require.EqualValues(t, 150, p["attendance_radius_meters"], "the default radius")
		require.Equal(t, false, p["configured"], "no coordinates yet")
	})

	t.Run("teams are numbered from 1 and a number is never reused", func(t *testing.T) {
		s1 := data[row](t, e.newTeam(admin, project))
		s2 := data[row](t, e.newTeam(admin, project))
		require.Equal(t, "1K-IBMI-S1", s1.str("code"))
		require.Equal(t, "1K-IBMI-S2", s2.str("code"))
		require.Equal(t, project, s1.str("project_code"))
		require.Equal(t, true, s1["validate_shift"], "validated by default")
		require.Equal(t, false, s1["reject_attendance_after_tolerance"])

		require.Equal(t, http.StatusOK, e.api(admin, http.MethodDelete, "/admin/teams/1K-IBMI-S2", nil).Status)
		s3 := data[row](t, e.newTeam(admin, project))
		require.Equal(t, "1K-IBMI-S3", s3.str("code"), "S2 is not handed out again")

		require.Equal(t, []string{"1K-IBMI-S1", "1K-IBMI-S3"}, listedCodes(t, e.api(admin, http.MethodGet, "/admin/teams", nil)))
		require.Equal(t, http.StatusNotFound, e.api(admin, http.MethodGet, "/admin/teams/1K-IBMI-S2", nil).Status)
	})

	t.Run("the list is in number order, not text order", func(t *testing.T) {
		for range 8 { // S4 .. S11
			require.Equal(t, http.StatusCreated, e.newTeam(admin, project).Status)
		}
		codes := listedCodes(t, e.raw(admin, http.MethodGet, "/admin/teams?project_code=1K-IBMI-S", nil))
		require.Equal(t, []string{"1K-IBMI-S1", "1K-IBMI-S3", "1K-IBMI-S4", "1K-IBMI-S5", "1K-IBMI-S6", "1K-IBMI-S7",
			"1K-IBMI-S8", "1K-IBMI-S9", "1K-IBMI-S10", "1K-IBMI-S11"}, codes)
	})
}

func TestTeamNumbersAreDistinctWhenAdminsRace(t *testing.T) {
	e := newEnv(t)
	admin := e.person(testkit.NewOrg(t), "ORG_ADMIN", "")
	_, _, _, project := e.tree(admin, 1)

	const racers = 6
	codes := make([]string, racers)
	var wg sync.WaitGroup
	for i := range racers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res := e.raw(admin, http.MethodPost, "/admin/teams", map[string]any{"project_code": project, "description": "Regu"})
			if res.Status == http.StatusCreated {
				codes[i] = data[row](t, res).str("code")
			}
		}()
	}
	wg.Wait()

	require.ElementsMatch(t, []string{
		"1K-IBMI-S1", "1K-IBMI-S2", "1K-IBMI-S3", "1K-IBMI-S4", "1K-IBMI-S5", "1K-IBMI-S6",
	}, codes, "every request got its own number")
}

func TestAClientHasOneProjectPerBusinessLine(t *testing.T) {
	e := newEnv(t)
	admin := e.person(testkit.NewOrg(t), "ORG_ADMIN", "")
	_, _, client, _ := e.tree(admin, 1)

	again := e.api(admin, http.MethodPost, "/admin/projects", map[string]any{
		"client_code": client, "business_line": "security", "description": "Dobel", "timezone": "Asia/Jakarta",
	})
	expect(t, again, http.StatusConflict, "DUPLICATE", organization.ReasonCodeTaken)

	cleaning := e.mustCreate(admin, "/admin/projects", map[string]any{
		"client_code": client, "business_line": "cleaning", "description": "Kebersihan", "timezone": "Asia/Jakarta",
	})
	require.Equal(t, "1K-IBMI-C", cleaning.str("code"))

	require.Equal(t, []string{"1K-IBMI-C"}, listedCodes(t, e.raw(admin, http.MethodGet, "/admin/projects?business_line=cleaning", nil)))
	require.Equal(t, []string{"1K-IBMI-S"}, listedCodes(t, e.raw(admin, http.MethodGet, "/admin/projects?business_line=security", nil)))
	require.Equal(t, http.StatusUnprocessableEntity, e.raw(admin, http.MethodGet, "/admin/projects?business_line=janitor", nil).Status)
}

func TestACodeIsBuiltFromValidInputsOnly(t *testing.T) {
	e := newEnv(t)
	admin := e.person(testkit.NewOrg(t), "ORG_ADMIN", "")
	e.tree(admin, 1)

	// Sent raw: the contract's enums and patterns would refuse several of these before the
	// server sees them, and it is the server's own validation that has to answer.
	t.Run("a company letter other than K, I or P", func(t *testing.T) {
		res := e.raw(admin, http.MethodPost, "/admin/branches", map[string]any{"region_code": "OS1", "company_letter": "X", "description": "x"})
		expect(t, res, http.StatusUnprocessableEntity, "VALIDATION_ERROR", organization.ReasonBranchLetterInvalid)
	})
	t.Run("a company prefix of the wrong shape", func(t *testing.T) {
		for _, prefix := range []string{"ibmi", "A", "ABCDEFGHIJK", "IB-MI", "IB MI"} {
			res := e.raw(admin, http.MethodPost, "/admin/clients", map[string]any{"branch_code": "1K", "company_prefix": prefix, "description": "x"})
			expect(t, res, http.StatusUnprocessableEntity, "VALIDATION_ERROR", organization.ReasonClientPrefixInvalid)
		}
	})
	t.Run("a region code of the wrong shape", func(t *testing.T) {
		res := e.raw(admin, http.MethodPost, "/admin/branches", map[string]any{"region_code": "1K", "company_letter": "K", "description": "x"})
		require.Equal(t, http.StatusUnprocessableEntity, res.Status, "body: %s", res.Raw)
		require.Contains(t, string(res.Raw), `"region_code"`)
	})
	t.Run("a blank description or a missing field", func(t *testing.T) {
		require.Equal(t, http.StatusUnprocessableEntity,
			e.raw(admin, http.MethodPost, "/admin/branches", map[string]any{"region_code": "OS1", "company_letter": "I", "description": "  "}).Status)
		require.Equal(t, http.StatusUnprocessableEntity,
			e.raw(admin, http.MethodPost, "/admin/teams", map[string]any{"project_code": "1K-IBMI-S"}).Status)
	})
	t.Run("a business line, timezone or point that does not fit", func(t *testing.T) {
		base := func(extra map[string]any) map[string]any {
			body := map[string]any{"client_code": "1K-IBMI", "business_line": "cleaning", "description": "x", "timezone": "Asia/Jakarta"}
			for k, v := range extra {
				body[k] = v
			}
			return body
		}
		for name, body := range map[string]map[string]any{
			"business line":             base(map[string]any{"business_line": "janitor"}),
			"timezone":                  base(map[string]any{"timezone": "Europe/Paris"}),
			"latitude only":             base(map[string]any{"latitude": -6.2}),
			"longitude only":            base(map[string]any{"longitude": 106.8}),
			"latitude out of range":     base(map[string]any{"latitude": 91.0, "longitude": 106.8}),
			"zero radius":               base(map[string]any{"attendance_radius_meters": 0}),
			"a contact without a phone": base(map[string]any{"emergency_contacts": []map[string]any{{"type": "Polisi", "name": "Polsek"}}}),
		} {
			res := e.raw(admin, http.MethodPost, "/admin/projects", body)
			require.Equal(t, http.StatusUnprocessableEntity, res.Status, "%s: %s", name, res.Raw)
		}
	})
	t.Run("a parent that does not exist", func(t *testing.T) {
		expect(t, e.api(admin, http.MethodPost, "/admin/branches", map[string]any{"region_code": "OS9", "company_letter": "K", "description": "x"}),
			http.StatusNotFound, "NOT_FOUND", organization.ReasonRegionNotFound)
		expect(t, e.api(admin, http.MethodPost, "/admin/clients", map[string]any{"branch_code": "9K", "company_prefix": "ABCD", "description": "x"}),
			http.StatusNotFound, "NOT_FOUND", organization.ReasonBranchNotFound)
		expect(t, e.api(admin, http.MethodPost, "/admin/projects", map[string]any{"client_code": "1K-NONE", "business_line": "security", "description": "x", "timezone": "Asia/Jakarta"}),
			http.StatusNotFound, "NOT_FOUND", organization.ReasonClientNotFound)
		expect(t, e.newTeam(admin, "1K-IBMI-C"), http.StatusNotFound, "NOT_FOUND", organization.ReasonProjectNotFound)
	})
	t.Run("a code that is taken", func(t *testing.T) {
		expect(t, e.api(admin, http.MethodPost, "/admin/branches", map[string]any{"region_code": "OS1", "company_letter": "K", "description": "dobel"}),
			http.StatusConflict, "DUPLICATE", organization.ReasonCodeTaken)
		expect(t, e.api(admin, http.MethodPost, "/admin/clients", map[string]any{"branch_code": "1K", "company_prefix": "IBMI", "description": "dobel"}),
			http.StatusConflict, "DUPLICATE", organization.ReasonCodeTaken)
	})
	t.Run("a row that does not exist", func(t *testing.T) {
		for _, path := range []string{"/admin/branches/9K", "/admin/clients/9K-NONE", "/admin/projects/9K-NONE-S", "/admin/teams/9K-NONE-S1"} {
			for _, method := range []string{http.MethodGet, http.MethodDelete} {
				require.Equal(t, http.StatusNotFound, e.api(admin, method, path, nil).Status, method+" "+path)
			}
		}
	})
}

func TestTheCodeAndWhatItIsBuiltFromCannotBeChanged(t *testing.T) {
	e := newEnv(t)
	admin := e.person(testkit.NewOrg(t), "ORG_ADMIN", "")
	_, _, _, project := e.tree(admin, 1)
	require.Equal(t, http.StatusCreated, e.newTeam(admin, project).Status)

	for path, fields := range map[string][]string{
		"/admin/branches/1K":        {"code", "region_code", "company_letter"},
		"/admin/clients/1K-IBMI":    {"code", "branch_code", "company_prefix"},
		"/admin/projects/1K-IBMI-S": {"code", "client_code", "business_line"},
		"/admin/teams/1K-IBMI-S1":   {"code", "project_code"},
	} {
		for _, field := range fields {
			// The request body types have no such field, and the decoder refuses unknown ones.
			res := e.raw(admin, http.MethodPatch, path, map[string]any{field: "OS9", "description": "changed"})
			require.Equal(t, http.StatusUnprocessableEntity, res.Status, "%s %s: %s", path, field, res.Raw)
		}
	}

	t.Run("the description can change", func(t *testing.T) {
		res := e.api(admin, http.MethodPatch, "/admin/branches/1K", map[string]any{"description": "Jakarta Pusat", "city": "Jakarta"})
		require.Equal(t, http.StatusOK, res.Status, "body: %s", res.Raw)
		got := data[row](t, res)
		require.Equal(t, "Jakarta Pusat", got.str("description"))
		require.Equal(t, "Jakarta", got.str("city"))
		require.Equal(t, "1K", got.str("code"))

		// One field leaves the others alone.
		res = e.api(admin, http.MethodPatch, "/admin/branches/1K", map[string]any{"is_active": false})
		got = data[row](t, res)
		require.Equal(t, "Jakarta Pusat", got.str("description"))
		require.Equal(t, "Jakarta", got.str("city"))
		require.Equal(t, false, got["is_active"])

		res = e.api(admin, http.MethodPatch, "/admin/teams/1K-IBMI-S1", map[string]any{"description": "Regu Pagi", "min_personnel": 5})
		require.Equal(t, http.StatusOK, res.Status, "body: %s", res.Raw)
		require.EqualValues(t, 5, data[row](t, res)["min_personnel"])
		require.Equal(t, "Regu Pagi", data[row](t, res).str("description"))
	})

	t.Run("a blank description is refused on a change too", func(t *testing.T) {
		require.Equal(t, http.StatusUnprocessableEntity,
			e.raw(admin, http.MethodPatch, "/admin/clients/1K-IBMI", map[string]any{"description": " "}).Status)
	})
}

func TestADeleteIsRefusedWhileActiveChildrenRemain(t *testing.T) {
	e := newEnv(t)
	org := testkit.NewOrg(t)
	admin := e.person(org, "ORG_ADMIN", "")
	_, _, _, project := e.tree(admin, 1)
	require.Equal(t, "1K-IBMI-S", project)
	require.Equal(t, http.StatusCreated, e.newTeam(admin, project).Status)

	// Bottom-up is the only order that works: each level waits for the one below it.
	for _, path := range []string{"/admin/regions/OS1", "/admin/branches/1K", "/admin/clients/1K-IBMI", "/admin/projects/1K-IBMI-S"} {
		expect(t, e.api(admin, http.MethodDelete, path, nil), http.StatusConflict, "CONFLICT", organization.ReasonHasActiveChildren)
	}
	for _, path := range []string{
		"/admin/teams/1K-IBMI-S1", "/admin/projects/1K-IBMI-S", "/admin/clients/1K-IBMI", "/admin/branches/1K", "/admin/regions/OS1",
	} {
		res := e.api(admin, http.MethodDelete, path, nil)
		require.Equal(t, http.StatusOK, res.Status, "DELETE %s: %s", path, res.Raw)
	}

	t.Run("a deleted row is hidden but keeps its code", func(t *testing.T) {
		require.Empty(t, listedCodes(t, e.api(admin, http.MethodGet, "/admin/branches", nil)))
		require.Equal(t, http.StatusNotFound, e.api(admin, http.MethodGet, "/admin/branches/1K", nil).Status)

		// The region comes back, but the branch code is still taken: records made under it
		// still point at it.
		require.Equal(t, http.StatusConflict, e.create(admin, "OS1", "Lagi").Status, "the region's code stays taken too")
		var deleted bool
		e.inTenant(org, func(tx pgx.Tx) {
			require.NoError(t, tx.QueryRow(t.Context(),
				"SELECT deleted_at IS NOT NULL AND NOT active FROM branches WHERE code = '1K'").Scan(&deleted))
		})
		require.True(t, deleted)
	})

	t.Run("a deleted parent cannot take a new child", func(t *testing.T) {
		expect(t, e.newTeam(admin, "1K-IBMI-S"), http.StatusNotFound, "NOT_FOUND", organization.ReasonProjectNotFound)
	})
}

func TestAnInactiveChildDoesNotBlockADelete(t *testing.T) {
	e := newEnv(t)
	admin := e.person(testkit.NewOrg(t), "ORG_ADMIN", "")
	_, _, _, project := e.tree(admin, 1)
	require.Equal(t, http.StatusCreated, e.newTeam(admin, project).Status)

	require.Equal(t, http.StatusOK, e.api(admin, http.MethodPatch, "/admin/teams/1K-IBMI-S1", map[string]any{"is_active": false}).Status)
	require.Equal(t, http.StatusOK, e.api(admin, http.MethodDelete, "/admin/projects/1K-IBMI-S", nil).Status)
}

func TestARegionAdminWorksInsideTheirRegion(t *testing.T) {
	e := newEnv(t)
	org := testkit.NewOrg(t)
	admin := e.person(org, "ORG_ADMIN", "")
	_, _, _, _ = e.tree(admin, 1)
	_, _, _, _ = e.tree(admin, 2)
	require.Equal(t, http.StatusCreated, e.newTeam(admin, "1K-IBMI-S").Status)
	require.Equal(t, http.StatusCreated, e.newTeam(admin, "2K-IBMI-S").Status)
	regionAdmin := e.person(org, "REGION_ADMIN", "OS1")

	t.Run("every list shows only their region", func(t *testing.T) {
		require.Equal(t, []string{"1K"}, listedCodes(t, e.api(regionAdmin, http.MethodGet, "/admin/branches", nil)))
		require.Equal(t, []string{"1K-IBMI"}, listedCodes(t, e.api(regionAdmin, http.MethodGet, "/admin/clients", nil)))
		require.Equal(t, []string{"1K-IBMI-S"}, listedCodes(t, e.api(regionAdmin, http.MethodGet, "/admin/projects", nil)))
		require.Equal(t, []string{"1K-IBMI-S1"}, listedCodes(t, e.api(regionAdmin, http.MethodGet, "/admin/teams", nil)))
		list := e.api(regionAdmin, http.MethodGet, "/admin/teams", nil)
		require.Contains(t, string(list.Raw), `"total":1`)
		require.Equal(t, []string{"1K", "2K"}, listedCodes(t, e.api(admin, http.MethodGet, "/admin/branches", nil)), "the ORG_ADMIN sees both")
	})

	t.Run("a row of another region is SCOPE_DENIED, whether or not it exists", func(t *testing.T) {
		for _, path := range []string{"/admin/branches/2K", "/admin/clients/2K-IBMI", "/admin/projects/2K-IBMI-S", "/admin/teams/2K-IBMI-S1", "/admin/teams/2K-IBMI-S99"} {
			for _, method := range []string{http.MethodGet, http.MethodPatch, http.MethodDelete} {
				var body any
				if method == http.MethodPatch {
					body = map[string]any{"description": "mine now"}
				}
				expect(t, e.api(regionAdmin, method, path, body), http.StatusForbidden, "SCOPE_DENIED", access.ReasonOutsideScope)
			}
		}
		require.Equal(t, "Cabang 2", data[row](t, e.api(admin, http.MethodGet, "/admin/branches/2K", nil)).str("description"), "nothing changed")
	})

	t.Run("they cannot create under another region", func(t *testing.T) {
		expect(t, e.api(regionAdmin, http.MethodPost, "/admin/branches", map[string]any{"region_code": "OS2", "company_letter": "I", "description": "x"}),
			http.StatusForbidden, "SCOPE_DENIED", access.ReasonOutsideScope)
		expect(t, e.api(regionAdmin, http.MethodPost, "/admin/clients", map[string]any{"branch_code": "2K", "company_prefix": "ABCD", "description": "x"}),
			http.StatusForbidden, "SCOPE_DENIED", access.ReasonOutsideScope)
		expect(t, e.api(regionAdmin, http.MethodPost, "/admin/projects", map[string]any{"client_code": "2K-IBMI", "business_line": "cleaning", "description": "x", "timezone": "Asia/Jakarta"}),
			http.StatusForbidden, "SCOPE_DENIED", access.ReasonOutsideScope)
		expect(t, e.newTeam(regionAdmin, "2K-IBMI-S"), http.StatusForbidden, "SCOPE_DENIED", access.ReasonOutsideScope)
	})

	t.Run("inside their region they create, change and delete", func(t *testing.T) {
		branch := e.mustCreate(regionAdmin, "/admin/branches", map[string]any{"region_code": "OS1", "company_letter": "I", "description": "Cabang IDS"})
		require.Equal(t, "1I", branch.str("code"))
		client := e.mustCreate(regionAdmin, "/admin/clients", map[string]any{"branch_code": "1I", "company_prefix": "XYZ1", "description": "Klien"})
		require.Equal(t, "1I-XYZ1", client.str("code"))
		project := e.mustCreate(regionAdmin, "/admin/projects", map[string]any{"client_code": "1I-XYZ1", "business_line": "cleaning", "description": "Proyek", "timezone": "Asia/Makassar"})
		require.Equal(t, "1I-XYZ1-C", project.str("code"))
		team := data[row](t, e.newTeam(regionAdmin, "1I-XYZ1-C"))
		require.Equal(t, "1I-XYZ1-C1", team.str("code"))

		require.Equal(t, http.StatusOK, e.api(regionAdmin, http.MethodPatch, "/admin/teams/1I-XYZ1-C1", map[string]any{"description": "Regu Malam"}).Status)
		for _, path := range []string{"/admin/teams/1I-XYZ1-C1", "/admin/projects/1I-XYZ1-C", "/admin/clients/1I-XYZ1", "/admin/branches/1I"} {
			require.Equal(t, http.StatusOK, e.api(regionAdmin, http.MethodDelete, path, nil).Status, path)
		}
	})

	t.Run("a region still cannot be managed by them", func(t *testing.T) {
		expect(t, e.create(regionAdmin, "OS3", "Outsourcing 3"), http.StatusForbidden, "ROLE_DENIED", access.ReasonOrgAdminOnly)
	})
}

func TestAProjectKnowsWhereItIs(t *testing.T) {
	e := newEnv(t)
	org := testkit.NewOrg(t)
	admin := e.person(org, "ORG_ADMIN", "")
	_, _, _, project := e.tree(admin, 1)
	projects := organization.NewProjects(testkit.Pool(t), clock.System{})

	t.Run("without coordinates it is not configured", func(t *testing.T) {
		_, _, _, err := projects.Location(t.Context(), org, project)
		require.ErrorIs(t, err, organization.ErrProjectNotConfigured)
		require.ErrorContains(t, err, organization.ReasonProjectNotConfigured)
	})

	t.Run("half a point is refused, also on a change", func(t *testing.T) {
		res := e.api(admin, http.MethodPatch, "/admin/projects/"+project, map[string]any{"latitude": -6.2})
		require.Equal(t, http.StatusUnprocessableEntity, res.Status, "body: %s", res.Raw)
		require.Contains(t, string(res.Raw), `"latitude"`)
	})

	t.Run("with a point it answers its timezone, point and radius", func(t *testing.T) {
		res := e.api(admin, http.MethodPatch, "/admin/projects/"+project, map[string]any{
			"latitude": -2.5337, "longitude": 140.7181, "timezone": "Asia/Jayapura", "attendance_radius_meters": 200,
			"patrol_interval_minutes": 30, "strict_attendance_geofence": true,
			"emergency_contacts": []map[string]any{{"type": "Polisi", "name": "Polres", "phone": "110"}},
		})
		require.Equal(t, http.StatusOK, res.Status, "body: %s", res.Raw)
		got := data[row](t, res)
		require.Equal(t, true, got["configured"])
		require.Equal(t, "Asia/Jayapura", got.str("timezone"))
		require.EqualValues(t, 30, got["patrol_interval_minutes"])
		require.Len(t, got["emergency_contacts"], 1)

		loc, point, radius, err := projects.Location(t.Context(), org, project)
		require.NoError(t, err)
		require.Equal(t, "Asia/Jayapura", loc.String())
		require.InDelta(t, -2.5337, point.Lat, 1e-9)
		require.InDelta(t, 140.7181, point.Lng, 1e-9)
		require.Equal(t, 200, radius)

		// The business date of 23:30 UTC is already tomorrow in WIT (UTC+9).
		now := time.Date(2026, 1, 31, 23, 30, 0, 0, time.UTC)
		require.Equal(t, 1, now.In(loc).Day())
	})

	t.Run("a change leaves the other settings alone", func(t *testing.T) {
		res := e.api(admin, http.MethodPatch, "/admin/projects/"+project, map[string]any{"description": "Renamed"})
		got := data[row](t, res)
		require.Equal(t, "Renamed", got.str("description"))
		require.Equal(t, "Asia/Jayapura", got.str("timezone"))
		require.EqualValues(t, 200, got["attendance_radius_meters"])
		require.Equal(t, true, got["configured"])
		require.Len(t, got["emergency_contacts"], 1, "the contacts are kept when none are sent")

		cleared := e.api(admin, http.MethodPatch, "/admin/projects/"+project, map[string]any{"emergency_contacts": []map[string]any{}})
		require.Empty(t, data[row](t, cleared)["emergency_contacts"], "an empty list clears them")
	})

	t.Run("its client, branch and region come back for a report", func(t *testing.T) {
		got, err := projects.Ancestors(t.Context(), org, project)
		require.NoError(t, err)
		require.Equal(t, organization.Ancestors{
			ClientCode: "1K-IBMI", ClientName: "Klien 1", BranchCode: "1K", BranchName: "Cabang 1",
			RegionCode: "OS1", RegionDescription: "Outsourcing 1",
		}, got)

		_, err = projects.Ancestors(t.Context(), org, "9K-NONE-S")
		require.ErrorIs(t, err, organization.ErrProjectNotFound)
	})

	t.Run("another organization does not find it", func(t *testing.T) {
		_, err := projects.Lookup(t.Context(), testkit.NewOrg(t), project)
		require.ErrorIs(t, err, organization.ErrProjectNotFound)
	})
}

func TestATeamsRelationshipOfficerIsAPersonOfTheOrganization(t *testing.T) {
	e := newEnv(t)
	orgA, orgB := testkit.NewOrg(t), testkit.NewOrg(t)
	admin := e.person(orgA, "ORG_ADMIN", "")
	_, _, _, project := e.tree(admin, 1)

	var inA, inB int64
	e.inTenant(orgA, func(tx pgx.Tx) {
		require.NoError(t, tx.QueryRow(t.Context(),
			"INSERT INTO personnels (personel_user_id, full_name, status) VALUES (501, 'Petugas A', 'ACTIVE') RETURNING id").Scan(&inA))
	})
	e.inTenant(orgB, func(tx pgx.Tx) {
		require.NoError(t, tx.QueryRow(t.Context(),
			"INSERT INTO personnels (personel_user_id, full_name, status) VALUES (502, 'Petugas B', 'ACTIVE') RETURNING id").Scan(&inB))
	})

	ok := e.api(admin, http.MethodPost, "/admin/teams", map[string]any{
		"project_code": project, "description": "Regu", "relationship_officer_personnel_id": inA})
	require.Equal(t, http.StatusCreated, ok.Status, "body: %s", ok.Raw)
	require.EqualValues(t, inA, data[row](t, ok)["relationship_officer_personnel_id"])

	other := e.api(admin, http.MethodPost, "/admin/teams", map[string]any{
		"project_code": project, "description": "Regu", "relationship_officer_personnel_id": inB})
	require.Equal(t, http.StatusUnprocessableEntity, other.Status, "another organization's person: %s", other.Raw)
	require.Contains(t, string(other.Raw), `"relationship_officer_personnel_id"`)

	missing := e.api(admin, http.MethodPatch, "/admin/teams/1K-IBMI-S1", map[string]any{"relationship_officer_personnel_id": 999999})
	require.Equal(t, http.StatusUnprocessableEntity, missing.Status, "body: %s", missing.Raw)

	// The failed create did not use up a number.
	require.Equal(t, "1K-IBMI-S2", data[row](t, e.newTeam(admin, project)).str("code"))
}

func TestTheListsFilterAndPage(t *testing.T) {
	e := newEnv(t)
	admin := e.person(testkit.NewOrg(t), "ORG_ADMIN", "")
	e.tree(admin, 1)
	e.tree(admin, 2)
	e.mustCreate(admin, "/admin/branches", map[string]any{"region_code": "OS1", "company_letter": "P", "description": "Cabang PCD"})

	require.Equal(t, []string{"1K", "1P", "2K"}, listedCodes(t, e.api(admin, http.MethodGet, "/admin/branches", nil)))
	require.Equal(t, []string{"1K", "1P"}, listedCodes(t, e.raw(admin, http.MethodGet, "/admin/branches?region_code=OS1", nil)))
	require.Equal(t, []string{"1P"}, listedCodes(t, e.raw(admin, http.MethodGet, "/admin/branches?q=pcd", nil)), "the description is searched")
	require.Equal(t, []string{"2K"}, listedCodes(t, e.raw(admin, http.MethodGet, "/admin/branches?q=2k", nil)), "the code is searched")

	page := e.raw(admin, http.MethodGet, "/admin/branches?limit=2&page=2", nil)
	require.Equal(t, []string{"2K"}, listedCodes(t, page))
	require.Contains(t, string(page.Raw), `"total":3`, "the total ignores the page")

	require.Equal(t, http.StatusOK, e.api(admin, http.MethodPatch, "/admin/branches/1P", map[string]any{"is_active": false}).Status)
	require.Equal(t, []string{"1P"}, listedCodes(t, e.raw(admin, http.MethodGet, "/admin/branches?is_active=false", nil)))
	require.Equal(t, []string{"1K", "2K"}, listedCodes(t, e.raw(admin, http.MethodGet, "/admin/branches?is_active=true", nil)))
	require.Equal(t, http.StatusUnprocessableEntity, e.raw(admin, http.MethodGet, "/admin/branches?is_active=maybe", nil).Status)

	require.Equal(t, []string{"1K-IBMI"}, listedCodes(t, e.raw(admin, http.MethodGet, "/admin/clients?branch_code=1K", nil)))
	require.Equal(t, []string{"2K-IBMI-S"}, listedCodes(t, e.raw(admin, http.MethodGet, "/admin/projects?client_code=2K-IBMI", nil)))
	require.Empty(t, listedCodes(t, e.raw(admin, http.MethodGet, "/admin/teams?project_code=1K-IBMI-S", nil)))
}

func TestTheHierarchyIsAuditedByCode(t *testing.T) {
	e := newEnv(t)
	org := testkit.NewOrg(t)
	admin := e.person(org, "ORG_ADMIN", "")
	_, _, _, project := e.tree(admin, 1)
	require.Equal(t, http.StatusCreated, e.newTeam(admin, project).Status)
	require.Equal(t, http.StatusOK, e.api(admin, http.MethodPatch, "/admin/clients/1K-IBMI", map[string]any{"director_name": "Budi"}).Status)
	require.Equal(t, http.StatusOK, e.api(admin, http.MethodDelete, "/admin/teams/1K-IBMI-S1", nil).Status)

	require.Equal(t, []string{
		"region.create region OS1", "branch.create branch 1K", "client.create client 1K-IBMI", "project.create project 1K-IBMI-S",
		"team.create team 1K-IBMI-S1", "client.update client 1K-IBMI", "team.delete team 1K-IBMI-S1",
	}, e.auditRows(org))

	var metadata string
	e.inTenant(org, func(tx pgx.Tx) {
		require.NoError(t, tx.QueryRow(t.Context(),
			"SELECT metadata::text FROM audit_logs WHERE action = 'client.update'").Scan(&metadata))
	})
	require.Contains(t, metadata, `"before"`)
	require.Contains(t, metadata, `"after"`)
	require.Contains(t, metadata, `"director_name": "Budi"`)
}

func TestTheHierarchyStaysInsideItsOrganization(t *testing.T) {
	e := newEnv(t)
	orgA, orgB := testkit.NewOrg(t), testkit.NewOrg(t)
	adminA, adminB := e.person(orgA, "ORG_ADMIN", ""), e.person(orgB, "ORG_ADMIN", "")
	e.tree(adminA, 1)

	for _, path := range []string{"/admin/branches", "/admin/clients", "/admin/projects", "/admin/teams"} {
		require.Empty(t, listedCodes(t, e.api(adminB, http.MethodGet, path, nil)), path)
	}
	for _, path := range []string{"/admin/branches/1K", "/admin/clients/1K-IBMI", "/admin/projects/1K-IBMI-S"} {
		require.Equal(t, http.StatusNotFound, e.api(adminB, http.MethodGet, path, nil).Status, path)
		require.Equal(t, http.StatusNotFound, e.api(adminB, http.MethodDelete, path, nil).Status, path)
	}
	// B cannot hang anything under A's rows, and its own tree can use the same codes.
	expect(t, e.api(adminB, http.MethodPost, "/admin/branches", map[string]any{"region_code": "OS1", "company_letter": "K", "description": "x"}),
		http.StatusNotFound, "NOT_FOUND", organization.ReasonRegionNotFound)
	e.tree(adminB, 1)
	require.Equal(t, "Cabang 1", data[row](t, e.api(adminA, http.MethodGet, "/admin/branches/1K", nil)).str("description"))
}

func TestAFieldPersonHasNoAccessToTheHierarchy(t *testing.T) {
	e := newEnv(t)
	field := e.person(testkit.NewOrg(t), "", "")
	for _, path := range []string{"/admin/branches", "/admin/clients", "/admin/projects", "/admin/teams"} {
		res := e.api(field, http.MethodGet, path, nil)
		require.Equal(t, http.StatusForbidden, res.Status, path)
		_, reason := res.reason(t)
		require.Equal(t, access.ReasonNotAdmin, reason)
	}
}

func TestTheTablesKeepACodeAndItsPartsInStep(t *testing.T) {
	e := newEnv(t)
	org := testkit.NewOrg(t)
	admin := e.person(org, "ORG_ADMIN", "")
	e.tree(admin, 1)

	// The API builds every code, so these are what the database alone refuses: a code that
	// does not match the columns it is made of.
	for name, sql := range map[string]string{
		"a branch code that is not its region's":  "INSERT INTO branches (code, region_code, company_letter, description) VALUES ('2K', 'OS1', 'K', 'x')",
		"a branch letter that is not K, I or P":   "INSERT INTO branches (code, region_code, company_letter, description) VALUES ('1X', 'OS1', 'X', 'x')",
		"a client code that is not its branch's":  "INSERT INTO clients (code, branch_code, company_prefix, description) VALUES ('2K-ABCD', '1K', 'ABCD', 'x')",
		"a project suffix that is not its line's": "INSERT INTO projects (code, client_code, business_line, timezone, description) VALUES ('1K-IBMI-C', '1K-IBMI', 'security', 'Asia/Jakarta', 'x')",
		"a project in a zone outside Indonesia":   "INSERT INTO projects (code, client_code, business_line, timezone, description) VALUES ('1K-IBMI-C', '1K-IBMI', 'cleaning', 'Europe/Paris', 'x')",
		"half a point":                            "INSERT INTO projects (code, client_code, business_line, timezone, latitude, description) VALUES ('1K-IBMI-C', '1K-IBMI', 'cleaning', 'Asia/Jakarta', -6.2, 'x')",
		"a team with a zero number":               "INSERT INTO teams (code, project_code, description) VALUES ('1K-IBMI-S0', '1K-IBMI-S', 'x')",
		"a team of another project":               "INSERT INTO teams (code, project_code, description) VALUES ('2K-IBMI-S1', '1K-IBMI-S', 'x')",
		"a team of a project that does not exist": "INSERT INTO teams (code, project_code, description) VALUES ('9K-NONE-S1', '9K-NONE-S', 'x')",
	} {
		err := execInTenant(t, org, sql)
		require.Error(t, err, name)
	}
}
