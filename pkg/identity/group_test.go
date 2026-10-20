package identity_test

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"

	"siapp/pkg/identity"
	"siapp/pkg/platform/apperr"
	"siapp/pkg/platform/personel"
	"siapp/pkg/platform/personel/personeltest"
)

// D17: KPI, IDS and PCD are ONE organization. The fixtures' two companies stand in for two
// PTs of the group, and outsideCompany for any other company.
const (
	groupOrg       int64 = 7
	outsideCompany int64 = 777
)

var testGroup = personel.Group{
	CompanyIDs:       []int64{personeltest.OrgSatu, personeltest.OrgDua},
	OrganizationID:   groupOrg,
	OrganizationName: "Test Group",
}

func newGroupEnv(t *testing.T) *env {
	t.Helper()
	e := newEnvWithGroup(t, "", testGroup)
	e.autoAdmin = false // makeAdmin writes to the person's company, not to the group's organization
	return e
}

func TestEveryCompanyOfTheGroupIsOneOrganization(t *testing.T) {
	e := newGroupEnv(t)

	inSatu := e.fieldSession(personeltest.EmailDanruSatu, phoneA)
	inDua := e.fieldSession(personeltest.EmailDanruDua, phoneB)

	require.Equal(t, groupOrg, inSatu.User.OrganizationID)
	require.Equal(t, groupOrg, inDua.User.OrganizationID, "another PT of the group is the same tenant")
	require.Equal(t, 1, countRows(t, groupOrg, "organizations"), "one organization, not one per PT")
	require.Equal(t, 2, countRows(t, groupOrg, "personnels"))

	me := dataOf[meData](t, e.api(inDua.AccessToken, http.MethodGet, "/auth/me", nil))
	require.Equal(t, groupOrg, me.Organization.ID)
	require.Equal(t, "Test Group", me.Organization.Name, "the group's name, not the PT's")

	t.Run("each person keeps their own PT, for display", func(t *testing.T) {
		require.Equal(t, personeltest.OrgSatu,
			scalar[int64](t, groupOrg, "SELECT company_id FROM personnels WHERE id = $1", inSatu.User.ID))
		require.Equal(t, personeltest.OrgDua,
			scalar[int64](t, groupOrg, "SELECT company_id FROM personnels WHERE id = $1", inDua.User.ID))
		require.Equal(t, "PT Siaga Dua (seed)",
			scalar[string](t, groupOrg, "SELECT company_name FROM personnels WHERE id = $1", inDua.User.ID))
	})

	t.Run("a person of any PT is found by NIK in the one organization", func(t *testing.T) {
		people := e.people()
		for _, nik := range []string{personeltest.NIKAnggota1Satu, personeltest.NIKAnggota1Dua} {
			id, err := people.Ensure(t.Context(), groupOrg, nik)
			require.NoError(t, err)
			require.Positive(t, id)
		}
	})
}

func TestAPersonOfACompanyOutsideTheGroupCannotSignIn(t *testing.T) {
	e := newGroupEnv(t)
	e.fake.MoveToCompany(personeltest.NIKAnggota1Satu, outsideCompany)

	res := e.login(mobile(personeltest.EmailAnggota1Satu, phoneA))
	require.Equal(t, http.StatusForbidden, res.Status, "body: %s", res.Raw)
	got := res.err(t)
	require.Equal(t, "FORBIDDEN", got.Code)
	require.Equal(t, identity.ReasonNotInGroup, got.reason())

	// ⚠ Nothing about them is stored: not their row, not an organization for their company,
	// not a session or a phone binding.
	require.Equal(t, 0, countRows(t, groupOrg, "personnels"))
	require.Equal(t, 0, countRows(t, groupOrg, "sessions"))
	require.Equal(t, 0, countRows(t, groupOrg, "device_bindings"))
	require.Equal(t, 0, countRows(t, groupOrg, "organizations"), "no organization is made for a company that is not in the group")

	t.Run("the web is refused the same way", func(t *testing.T) {
		res := e.login(web(personeltest.EmailAnggota1Satu))
		require.Equal(t, http.StatusForbidden, res.Status)
		require.Equal(t, identity.ReasonNotInGroup, res.err(t).reason())
	})

	t.Run("a wrong password is still a wrong password", func(t *testing.T) {
		res := e.login(withPassword(mobile(personeltest.EmailAnggota1Satu, phoneA), wrongPass))
		require.Equal(t, http.StatusUnauthorized, res.Status)
	})

	t.Run("and a lookup by NIK does not find them", func(t *testing.T) {
		_, err := e.people().Ensure(t.Context(), groupOrg, personeltest.NIKAnggota1Satu)
		require.ErrorIs(t, err, apperr.ErrNotFound)
	})
}

func TestMovingBetweenCompaniesOfTheGroupKeepsThePerson(t *testing.T) {
	e := newGroupEnv(t)
	first := e.login(mobile(personeltest.EmailAnggota1Satu, phoneA)).data(t)
	makeStale(t, groupOrg, first.User.ID)
	e.fake.MoveToCompany(personeltest.NIKAnggota1Satu, personeltest.OrgDua)

	// The same person, in the same organization, with the same history: only the PT changed.
	res := e.refresh(first.RefreshToken)
	require.Equal(t, http.StatusOK, res.Status, "body: %s", res.Raw)
	require.Equal(t, 1, countRows(t, groupOrg, "personnels"))
	require.Equal(t, personeltest.OrgDua,
		scalar[int64](t, groupOrg, "SELECT company_id FROM personnels WHERE id = $1", first.User.ID))
}

func TestMovingOutOfTheGroupEndsTheSession(t *testing.T) {
	e := newGroupEnv(t)
	first := e.login(mobile(personeltest.EmailAnggota1Satu, phoneA)).data(t)
	makeStale(t, groupOrg, first.User.ID)
	e.fake.MoveToCompany(personeltest.NIKAnggota1Satu, outsideCompany)

	res := e.refresh(first.RefreshToken)
	require.Equal(t, http.StatusForbidden, res.Status, "body: %s", res.Raw)
	require.Equal(t, identity.ReasonAccountDisabled, res.err(t).reason())
}
