package identity_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"

	"siapp/internal/db"
	"siapp/pkg/access"
	"siapp/pkg/identity"
	"siapp/pkg/platform/auth"
	"siapp/pkg/platform/personel/personeltest"
	"siapp/pkg/platform/testkit"
)

// api calls a contract operation as the holder of token and checks request and response
// against the contract.
func (e *env) api(token, method, path string, body any) reply {
	e.t.Helper()
	e.client.Token = token
	defer func() { e.client.Token = "" }()
	res := e.client.Do(method, path, body)
	e.record(string(res.Body))
	return reply{Status: res.Status, Header: res.Header, Raw: res.Body}
}

// send makes any request, with an optional bearer token and JSON body, and no contract
// checks: for requests the contract forbids and for plain status checks.
func (e *env) send(method, path, token string, body any) reply {
	e.t.Helper()
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		require.NoError(e.t, err)
		reader = bytes.NewReader(raw)
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
	raw, err := io.ReadAll(res.Body)
	require.NoError(e.t, err)
	e.record(string(raw))
	return reply{Status: res.StatusCode, Header: res.Header, Raw: raw}
}

func itoa(v int64) string { return strconv.FormatInt(v, 10) }

func dataOf[T any](t *testing.T, r reply) T {
	t.Helper()
	require.Equal(t, http.StatusOK, r.Status, "body: %s", r.Raw)
	var env struct {
		Data T `json:"data"`
	}
	require.NoError(t, json.Unmarshal(r.Raw, &env), "body: %s", r.Raw)
	return env.Data
}

// adminToken makes the fixture an ORG_ADMIN and returns an access token from a web login.
func (e *env) adminToken(email string) string {
	e.t.Helper()
	e.makeAdmin(email)
	return e.login(web(email)).data(e.t).AccessToken
}

// fieldToken signs a fixture in on a phone, as field staff do, and returns the session.
func (e *env) fieldSession(email, device string) loginData {
	e.t.Helper()
	return e.login(mobile(email, device)).data(e.t)
}

func auditActions(t *testing.T, org int64) []string {
	t.Helper()
	var out []string
	inTenant(t, org, func(tx pgx.Tx) {
		rows, err := tx.Query(t.Context(), "SELECT action FROM audit_logs ORDER BY id")
		require.NoError(t, err)
		out, err = pgx.CollectRows(rows, pgx.RowTo[string])
		require.NoError(t, err)
	})
	return out
}

type webRoleData struct {
	PersonnelID          int64   `json:"personnel_id"`
	FullName             string  `json:"full_name"`
	NIK                  string  `json:"nik"`
	Role                 string  `json:"role"`
	ProjectIDs           []int64 `json:"project_ids"`
	GrantedByPersonnelID *int64  `json:"granted_by_personnel_id"`
}

func TestWebLoginNeedsAWebRole(t *testing.T) {
	e := newEnv(t, "")
	e.autoAdmin = false

	// ⚠ Field staff cannot use the web to get around the one-phone rule.
	res := e.login(web(personeltest.EmailDanruSatu))
	require.Equal(t, http.StatusForbidden, res.Status)
	got := res.err(t)
	require.Equal(t, "FORBIDDEN", got.Code)
	require.Equal(t, access.ReasonNotAdmin, got.reason())
	require.Equal(t, 0, countRows(t, satu, "sessions"), "no session exists")
	require.Equal(t, 0, countRows(t, satu, "personnels"), "and the refused login left nothing behind")

	// The same person signs in on a phone as usual, and is still refused on the web.
	e.fieldSession(personeltest.EmailDanruSatu, phoneA)
	require.Equal(t, http.StatusForbidden, e.login(web(personeltest.EmailDanruSatu)).Status)

	// With a role they can use the web, and without it again they cannot.
	e.makeAdmin(personeltest.EmailDanruSatu)
	e.login(web(personeltest.EmailDanruSatu)).data(t)
	execTenant(t, satu, "DELETE FROM web_role_assignments")
	require.Equal(t, http.StatusForbidden, e.login(web(personeltest.EmailDanruSatu)).Status)
}

func TestAdminRoutesAreForAdminsOnly(t *testing.T) {
	e := newEnv(t, "")
	field := e.fieldSession(personeltest.EmailAnggota1Satu, phoneA)

	for _, route := range []struct{ method, path string }{
		{http.MethodGet, "/admin/web-roles"},
		{http.MethodPut, "/admin/web-roles/1"},
		{http.MethodDelete, "/admin/web-roles/1"},
		{http.MethodDelete, "/admin/personnel/1/device-binding"},
	} {
		var body any
		if route.method == http.MethodPut {
			body = map[string]any{"role": "ORG_ADMIN"}
		}
		res := e.api(field.AccessToken, route.method, route.path, body)
		require.Equal(t, http.StatusForbidden, res.Status, "%s %s", route.method, route.path)
		require.Equal(t, access.ReasonNotAdmin, res.err(t).reason())
	}

	admin := e.adminToken(personeltest.EmailDanruSatu)
	require.Equal(t, http.StatusOK, e.api(admin, http.MethodGet, "/admin/web-roles", nil).Status)

	// The role is read on every request, never taken from the token: revoking it stops the
	// very next call, not after 15 minutes.
	execTenant(t, satu, "DELETE FROM web_role_assignments")
	require.Equal(t, http.StatusForbidden, e.api(admin, http.MethodGet, "/admin/web-roles", nil).Status)
}

func TestWebRoleManagement(t *testing.T) {
	e := newEnv(t, "")
	admin := e.adminToken(personeltest.EmailDanruSatu)
	adminID := scalar[int64](t, satu, "SELECT id FROM personnels WHERE email = $1", personeltest.EmailDanruSatu)
	member := e.fieldSession(personeltest.EmailAnggota1Satu, phoneA)
	memberID := member.User.ID
	rolePath := func(id int64) string { return "/admin/web-roles/" + itoa(id) }

	t.Run("an ORG_ADMIN grants a PROJECT_ADMIN, project ids sorted and de-duplicated", func(t *testing.T) {
		res := e.api(admin, http.MethodPut, rolePath(memberID), map[string]any{"role": "PROJECT_ADMIN", "project_ids": []int64{5, 3, 5}})
		got := dataOf[webRoleData](t, res)
		require.Equal(t, "PROJECT_ADMIN", got.Role)
		require.Equal(t, []int64{3, 5}, got.ProjectIDs)
		require.Equal(t, memberID, got.PersonnelID)
		require.Equal(t, adminID, *got.GrantedByPersonnelID)
		require.Equal(t, identity.MaskNIK(personeltest.NIKAnggota1Satu), got.NIK, "the NIK is masked")
	})

	t.Run("the list shows it, masked, with a pagination block", func(t *testing.T) {
		res := e.api(admin, http.MethodGet, "/admin/web-roles", nil)
		require.Equal(t, http.StatusOK, res.Status)
		var env struct {
			Data       []webRoleData `json:"data"`
			Pagination struct {
				Total int `json:"total"`
			} `json:"pagination"`
		}
		require.NoError(t, json.Unmarshal(res.Raw, &env))
		require.Equal(t, 2, env.Pagination.Total)
		require.Len(t, env.Data, 2)
		require.NotContains(t, string(res.Raw), personeltest.NIKAnggota1Satu)
	})

	t.Run("the shapes of the roles are enforced", func(t *testing.T) {
		for name, body := range map[string]map[string]any{
			"a PROJECT_ADMIN with no projects":   {"role": "PROJECT_ADMIN"},
			"a PROJECT_ADMIN with an empty list": {"role": "PROJECT_ADMIN", "project_ids": []int64{}},
			"an ORG_ADMIN with projects":         {"role": "ORG_ADMIN", "project_ids": []int64{4}},
			"an unknown role":                    {"role": "ROOT"},
			"a project id of zero":               {"role": "PROJECT_ADMIN", "project_ids": []int64{0}},
		} {
			// Sent raw: the contract client would refuse some of these before the server sees them.
			put := e.send(http.MethodPut, rolePath(memberID), admin, body)
			require.Equal(t, http.StatusUnprocessableEntity, put.Status, "%s: %s", name, put.Raw)
			require.Equal(t, "VALIDATION_ERROR", put.err(t).Code, name)
		}
	})

	t.Run("a person who is not in this organization is not found", func(t *testing.T) {
		res := e.api(admin, http.MethodPut, rolePath(987654), map[string]any{"role": "ORG_ADMIN"})
		require.Equal(t, http.StatusNotFound, res.Status)
	})

	t.Run("an admin cannot remove or narrow their own role", func(t *testing.T) {
		del := e.api(admin, http.MethodDelete, rolePath(adminID), nil)
		require.Equal(t, http.StatusConflict, del.Status)
		require.Equal(t, access.ReasonOwnRoleLocked, del.err(t).reason())

		narrow := e.api(admin, http.MethodPut, rolePath(adminID), map[string]any{"role": "PROJECT_ADMIN", "project_ids": []int64{1}})
		require.Equal(t, http.StatusConflict, narrow.Status)

		// ORG_ADMIN again changes nothing and is allowed.
		same := dataOf[webRoleData](t, e.api(admin, http.MethodPut, rolePath(adminID), map[string]any{"role": "ORG_ADMIN"}))
		require.Equal(t, "ORG_ADMIN", same.Role)
		require.Equal(t, "ORG_ADMIN", scalar[string](t, satu, "SELECT role::text FROM web_role_assignments WHERE personnel_id = $1", adminID))
	})

	t.Run("a PROJECT_ADMIN cannot manage roles", func(t *testing.T) {
		projectAdmin := e.login(web(personeltest.EmailAnggota1Satu)).data(t) // has the role granted above
		res := e.api(projectAdmin.AccessToken, http.MethodGet, "/admin/web-roles", nil)
		require.Equal(t, http.StatusForbidden, res.Status)
		require.Equal(t, "SCOPE_DENIED", res.err(t).Code)
		require.Equal(t, access.ReasonOrgAdminOnly, res.err(t).reason())
		require.Equal(t, http.StatusForbidden,
			e.api(projectAdmin.AccessToken, http.MethodPut, rolePath(adminID), map[string]any{"role": "ORG_ADMIN"}).Status)
	})

	t.Run("revoking a role takes effect at once, and a second revoke finds nothing", func(t *testing.T) {
		projectAdmin := e.login(web(personeltest.EmailAnggota1Satu)).data(t)
		require.Equal(t, http.StatusOK, e.api(admin, http.MethodDelete, rolePath(memberID), nil).Status)
		require.Equal(t, http.StatusForbidden, e.api(projectAdmin.AccessToken, http.MethodGet, "/admin/web-roles", nil).Status)
		require.Equal(t, http.StatusNotFound, e.api(admin, http.MethodDelete, rolePath(memberID), nil).Status)
	})

	t.Run("every grant and revoke is audited with the admin as actor", func(t *testing.T) {
		require.Equal(t, []string{"web_role.grant", "web_role.revoke"}, auditActions(t, satu))
		require.Equal(t, adminID, scalar[int64](t, satu, "SELECT actor_personnel_id FROM audit_logs WHERE action = 'web_role.grant'"))
	})
}

func TestWebRolesStayInsideTheirOrganization(t *testing.T) {
	e := newEnv(t, "")
	adminDua := e.adminToken(personeltest.EmailDanruDua)
	inSatu := e.fieldSession(personeltest.EmailAnggota1Satu, phoneA)

	// Dua's admin does not see Satu's roles and cannot touch Satu's people.
	list := e.api(adminDua, http.MethodGet, "/admin/web-roles", nil)
	require.Contains(t, string(list.Raw), `"total":1`)
	res := e.api(adminDua, http.MethodPut, "/admin/web-roles/"+itoa(inSatu.User.ID), map[string]any{"role": "ORG_ADMIN"})
	require.Equal(t, http.StatusNotFound, res.Status, "another organization's person does not exist here")
	require.Equal(t, 0, scalar[int](t, satu, "SELECT count(*) FROM web_role_assignments WHERE personnel_id = $1", inSatu.User.ID))
}

// fakePlacements stands in for module 02 until Phase 02.2: who is placed where.
type fakePlacements map[int64][]int64

func (f fakePlacements) ProjectIDsOf(_ context.Context, _ *db.Queries, personnelID int64) ([]int64, error) {
	return f[personnelID], nil
}

func TestDeviceReset(t *testing.T) {
	e := newEnv(t, "")
	admin := e.adminToken(personeltest.EmailDanruSatu)
	adminID := scalar[int64](t, satu, "SELECT id FROM personnels WHERE email = $1", personeltest.EmailDanruSatu)

	phone := e.fieldSession(personeltest.EmailAnggota1Satu, phoneA)
	personID := phone.User.ID
	resetPath := "/admin/personnel/" + itoa(personID) + "/device-binding"
	require.Equal(t, http.StatusOK, e.api(phone.AccessToken, http.MethodPost, "/notifications/token",
		map[string]any{"firebase_token": "fcm-A", "platform": "android"}).Status)
	require.Equal(t, 1, countRows(t, satu, "device_bindings"))
	require.Equal(t, 1, countRows(t, satu, "devices"))

	res := e.api(admin, http.MethodDelete, resetPath, nil)
	require.Equal(t, http.StatusOK, res.Status)
	require.Contains(t, string(res.Raw), `"data":null`)

	// The binding is gone, their MOBILE sessions are revoked, their push token is deleted.
	require.Equal(t, 0, countRows(t, satu, "device_bindings"))
	require.Equal(t, 0, scalar[int](t, satu, "SELECT count(*) FROM sessions WHERE personnel_id = $1 AND client = 'MOBILE' AND revoked_at IS NULL", personID))
	require.Equal(t, 0, countRows(t, satu, "devices"))
	// The old phone is out at once: its access token is cut off, and it cannot refresh.
	require.Equal(t, http.StatusUnauthorized, e.whoami(phone.AccessToken))
	require.Equal(t, http.StatusUnauthorized, e.refresh(phone.RefreshToken).Status)
	// The person signs in on a new phone, which binds.
	e.fieldSession(personeltest.EmailAnggota1Satu, phoneB)
	require.Equal(t, auth.HashToken(phoneB), scalar[string](t, satu, "SELECT device_token_hash FROM device_bindings"))
	// The reset is audited with the admin as actor.
	require.Equal(t, []string{"device_binding.reset"}, auditActions(t, satu))
	require.Equal(t, adminID, scalar[int64](t, satu, "SELECT actor_personnel_id FROM audit_logs"))

	t.Run("a person with no binding is a no-op, not an error, and not audited again", func(t *testing.T) {
		execTenant(t, satu, "DELETE FROM device_bindings")
		require.Equal(t, http.StatusOK, e.api(admin, http.MethodDelete, resetPath, nil).Status)
		require.Equal(t, 1, len(auditActions(t, satu)))
	})

	t.Run("a person who does not exist is not found", func(t *testing.T) {
		require.Equal(t, http.StatusNotFound, e.api(admin, http.MethodDelete, "/admin/personnel/987654/device-binding", nil).Status)
	})
}

func TestDeviceResetLeavesWebSessionsAlone(t *testing.T) {
	e := newEnv(t, "")
	admin := e.adminToken(personeltest.EmailDanruSatu)
	adminID := scalar[int64](t, satu, "SELECT id FROM personnels WHERE email = $1", personeltest.EmailDanruSatu)
	// An admin who also has a phone.
	e.fieldSession(personeltest.EmailDanruSatu, phoneA)

	require.Equal(t, http.StatusOK, e.api(admin, http.MethodDelete, "/admin/personnel/"+itoa(adminID)+"/device-binding", nil).Status)
	require.Equal(t, http.StatusOK, e.api(admin, http.MethodGet, "/admin/web-roles", nil).Status, "only MOBILE sessions are revoked")
}

func TestAProjectAdminResetsOnlyTheirOwnProjectsPeople(t *testing.T) {
	e := newEnv(t, "")
	phone := e.fieldSession(personeltest.EmailAnggota2Satu, phoneA)
	// A person who is a PROJECT_ADMIN of project 7.
	projectAdminID := e.makeAdmin(personeltest.EmailDanruSatu)
	execTenant(t, satu, "UPDATE web_role_assignments SET role = 'PROJECT_ADMIN', project_ids = '{7}' WHERE personnel_id = $1", projectAdminID)
	admin := e.login(web(personeltest.EmailDanruSatu)).data(t).AccessToken
	resetPath := "/admin/personnel/" + itoa(phone.User.ID) + "/device-binding"

	// Until module 02 places people anywhere, nobody is in a PROJECT_ADMIN scope: fail closed.
	res := e.api(admin, http.MethodDelete, resetPath, nil)
	require.Equal(t, http.StatusForbidden, res.Status)
	require.Equal(t, "SCOPE_DENIED", res.err(t).Code)
	require.Equal(t, access.ReasonOutsideProject, res.err(t).reason())
	require.Equal(t, 1, countRows(t, satu, "device_bindings"), "nothing changed")

	// Placed at another project: still outside.
	e.usecase.UsePlacements(fakePlacements{phone.User.ID: {9}})
	require.Equal(t, http.StatusForbidden, e.api(admin, http.MethodDelete, resetPath, nil).Status)

	// Placed at one of their projects: allowed.
	e.usecase.UsePlacements(fakePlacements{phone.User.ID: {9, 7}})
	require.Equal(t, http.StatusOK, e.api(admin, http.MethodDelete, resetPath, nil).Status)
	require.Equal(t, 0, countRows(t, satu, "device_bindings"))
}

func TestDeviceResetStaysInsideTheOrganization(t *testing.T) {
	e := newEnv(t, "")
	adminDua := e.adminToken(personeltest.EmailDanruDua)
	inSatu := e.fieldSession(personeltest.EmailAnggota1Satu, phoneA)

	res := e.api(adminDua, http.MethodDelete, "/admin/personnel/"+itoa(inSatu.User.ID)+"/device-binding", nil)
	require.Equal(t, http.StatusNotFound, res.Status)
	require.Equal(t, 1, countRows(t, satu, "device_bindings"), "another organization's binding is untouched")
	require.Equal(t, http.StatusOK, e.whoami(inSatu.AccessToken))
}

type meData struct {
	User struct {
		ID             int64  `json:"id"`
		OrganizationID int64  `json:"organization_id"`
		NIK            string `json:"nik"`
		FirstName      string `json:"first_name"`
	} `json:"user"`
	Organization struct {
		ID   int64  `json:"id"`
		Name string `json:"name"`
	} `json:"organization"`
	WebRole *struct {
		Role       string  `json:"role"`
		ProjectIDs []int64 `json:"project_ids"`
	} `json:"web_role"`
	Roles       []string `json:"roles"`
	Memberships []any    `json:"memberships"`
}

func TestMe(t *testing.T) {
	e := newEnv(t, "")

	t.Run("field staff: masked NIK, organization, no web role, no memberships yet", func(t *testing.T) {
		phone := e.fieldSession(personeltest.EmailAnggota1Satu, phoneA)
		res := e.api(phone.AccessToken, http.MethodGet, "/auth/me", nil)
		me := dataOf[meData](t, res)

		require.Equal(t, phone.User.ID, me.User.ID)
		require.Equal(t, identity.MaskNIK(personeltest.NIKAnggota1Satu), me.User.NIK)
		require.Equal(t, "Anggota Satu A", me.User.FirstName)
		require.EqualValues(t, personeltest.OrgSatu, me.Organization.ID)
		require.Equal(t, "PT Siaga Satu (seed)", me.Organization.Name)
		require.Nil(t, me.WebRole)
		require.Empty(t, me.Roles)
		require.NotNil(t, me.Memberships)
		require.Empty(t, me.Memberships)

		// Never the full NIK, and never anything about the phone binding.
		require.NotContains(t, string(res.Raw), personeltest.NIKAnggota1Satu)
		require.NotContains(t, string(res.Raw), auth.HashToken(phoneA))
		require.NotContains(t, string(res.Raw), phoneA)
		require.NotContains(t, string(res.Raw), "device")
	})

	t.Run("an ORG_ADMIN", func(t *testing.T) {
		me := dataOf[meData](t, e.api(e.adminToken(personeltest.EmailDanruSatu), http.MethodGet, "/auth/me", nil))
		require.NotNil(t, me.WebRole)
		require.Equal(t, "ORG_ADMIN", me.WebRole.Role)
		require.Empty(t, me.WebRole.ProjectIDs)
		require.Equal(t, []string{"ORG_ADMIN"}, me.Roles)
	})

	t.Run("a PROJECT_ADMIN lists their projects", func(t *testing.T) {
		id := e.makeAdmin(personeltest.EmailSupervisorSatu)
		execTenant(t, satu, "UPDATE web_role_assignments SET role = 'PROJECT_ADMIN', project_ids = '{4,2}' WHERE personnel_id = $1", id)
		token := e.login(web(personeltest.EmailSupervisorSatu)).data(t).AccessToken
		me := dataOf[meData](t, e.api(token, http.MethodGet, "/auth/me", nil))
		require.Equal(t, "PROJECT_ADMIN", me.WebRole.Role)
		require.Equal(t, []int64{4, 2}, me.WebRole.ProjectIDs)
	})

	t.Run("it needs a token, and a token of a person who is gone is refused", func(t *testing.T) {
		require.Equal(t, http.StatusUnauthorized, e.send(http.MethodGet, "/auth/me", "", nil).Status)
		// A genuine token for a person who is not in the cache: not a valid sign-in.
		ghost := testkit.Token(t, satu, 987654)
		require.Equal(t, http.StatusUnauthorized, e.send(http.MethodGet, "/auth/me", ghost, nil).Status)
	})
}
