package identity_test

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"

	"siapp/pkg/access"
	"siapp/pkg/platform/personel/personeltest"
)

// rowsPerOrg is what a seeded organization holds: one row per role and permission.
var rowsPerOrg = len(access.Catalog()) * len(access.TeamRoles())

func TestLoginSeedsTheFieldPermissionsOfANewOrganization(t *testing.T) {
	e := newEnv(t, "")

	t.Run("a login that Personalia refuses seeds nothing", func(t *testing.T) {
		require.Equal(t, http.StatusUnauthorized, e.login(withPassword(mobile(personeltest.EmailAnggota1Satu, phoneA), wrongPass)).Status)
		require.Equal(t, 0, countRows(t, satu, "role_permissions"))
		require.Equal(t, 0, countRows(t, satu, "organizations"))
	})

	t.Run("the first login of an organization gives it the default matrix", func(t *testing.T) {
		e.fieldSession(personeltest.EmailAnggota1Satu, phoneA)
		require.Equal(t, rowsPerOrg, countRows(t, satu, "role_permissions"))
		require.Equal(t, "TEAM", scalar[string](t, satu,
			"SELECT scope::text FROM role_permissions WHERE team_role = 'DANRU' AND permission = 'attendance.leave.approve'"))
		require.Equal(t, "NONE", scalar[string](t, satu,
			"SELECT scope::text FROM role_permissions WHERE team_role = 'MEMBER' AND permission = 'attendance.leave.approve'"))
		require.Empty(t, auditActions(t, satu), "a seed is not an admin's action: nothing is audited")
	})

	t.Run("the next login keeps an admin's change and fills in only what is missing", func(t *testing.T) {
		// An admin took leave approval away from DANRU, and a permission the catalog gained
		// since has no rows yet.
		execTenant(t, satu, "UPDATE role_permissions SET scope = 'NONE' WHERE team_role = 'DANRU' AND permission = 'attendance.leave.approve'")
		execTenant(t, satu, "DELETE FROM role_permissions WHERE permission = 'patrol.scan'")
		require.Equal(t, rowsPerOrg-2, countRows(t, satu, "role_permissions"))

		e.fieldSession(personeltest.EmailAnggota2Satu, phoneB)

		require.Equal(t, rowsPerOrg, countRows(t, satu, "role_permissions"))
		require.Equal(t, "NONE", scalar[string](t, satu,
			"SELECT scope::text FROM role_permissions WHERE team_role = 'DANRU' AND permission = 'attendance.leave.approve'"),
			"a login never hands back what an admin took away")
		require.Equal(t, "TEAM", scalar[string](t, satu,
			"SELECT scope::text FROM role_permissions WHERE team_role = 'MEMBER' AND permission = 'patrol.scan'"))
	})

	t.Run("another organization is seeded on its own", func(t *testing.T) {
		require.Equal(t, 0, countRows(t, dua, "role_permissions"))
		e.fieldSession(personeltest.EmailAnggota1Dua, phoneA)
		require.Equal(t, rowsPerOrg, countRows(t, dua, "role_permissions"))
		require.Equal(t, rowsPerOrg, countRows(t, satu, "role_permissions"))
	})
}

// ⚠ Two first logins of one organization at once both insert the same rows. They queue on the
// first row and the second finds the rows there: neither fails, and nothing is doubled.
func TestFirstLoginsOfAnOrganizationAtOnceSeedItOnce(t *testing.T) {
	e := newEnv(t, "")

	first := e.async(http.MethodPost, loginPath, "", mobile(personeltest.EmailAnggota1Satu, phoneA))
	second := e.async(http.MethodPost, loginPath, "", mobile(personeltest.EmailAnggota2Satu, phoneB))
	third := e.async(http.MethodPost, loginPath, "", mobile(personeltest.EmailDanruSatu, phoneA))

	for _, res := range []reply{await(t, first), await(t, second), await(t, third)} {
		require.Equal(t, http.StatusOK, res.Status, "body: %s", res.Raw)
	}
	require.Equal(t, rowsPerOrg, countRows(t, satu, "role_permissions"))
}
