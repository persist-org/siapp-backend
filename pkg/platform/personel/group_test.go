package personel_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"siapp/pkg/platform/personel"
	"siapp/pkg/platform/personel/personeltest"
	"siapp/pkg/platform/testkit"
)

// The group of these tests: the fixtures' two companies are two PTs of one group.
var testGroup = personel.Group{
	CompanyIDs:       []int64{personeltest.OrgSatu, personeltest.OrgDua},
	OrganizationID:   7,
	OrganizationName: "Test Group",
}

// outsideCompany is a company that is not part of testGroup.
const outsideCompany int64 = 777

func TestAGroupIsOneOrganizationAndKeepsTheCompany(t *testing.T) {
	_, c := testkit.NewGroupedPersonel(t, testGroup)

	satu, err := login(c, personeltest.EmailDanruSatu, personeltest.Password)
	require.NoError(t, err)
	dua, err := login(c, personeltest.EmailDanruDua, personeltest.Password)
	require.NoError(t, err)

	for _, id := range []personel.Identity{satu, dua} {
		require.EqualValues(t, 7, id.OrganizationID, "every PT of the group is the same tenant")
		require.Equal(t, "Test Group", id.OrganizationName)
	}
	require.EqualValues(t, personeltest.OrgSatu, satu.CompanyID, "the person's own PT is kept for display")
	require.EqualValues(t, personeltest.OrgDua, dua.CompanyID)
	require.NotEmpty(t, satu.CompanyName)
}

func TestAPersonOfACompanyOutsideTheGroupHasNoOrganization(t *testing.T) {
	fake, c := testkit.NewGroupedPersonel(t, testGroup)
	fake.MoveToCompany(personeltest.NIKAnggota1Satu, outsideCompany)

	id, err := login(c, personeltest.EmailAnggota1Satu, personeltest.Password)
	require.NoError(t, err, "the client reports who they are; refusing them is the login's job")
	require.Zero(t, id.OrganizationID)
	require.EqualValues(t, outsideCompany, id.CompanyID, "so that the refusal can tell them from a broken answer")
}

func TestADirectoryLookupNeverFindsAPersonOutsideTheGroup(t *testing.T) {
	fake, c := testkit.NewGroupedPersonel(t, testGroup)
	fake.MoveToCompany(personeltest.NIKAnggota1Satu, outsideCompany)
	ctx := context.Background()

	_, err := c.EmployeeByNIK(ctx, testGroup.OrganizationID, personeltest.NIKAnggota1Satu)
	require.ErrorIs(t, err, personel.ErrNotFound)

	// Both PTs of the group are found through the one organization.
	for _, nik := range []string{personeltest.NIKDanruSatu, personeltest.NIKDanruDua} {
		found, err := c.EmployeeByNIK(ctx, testGroup.OrganizationID, nik)
		require.NoError(t, err)
		require.EqualValues(t, testGroup.OrganizationID, found.OrganizationID)
	}

	many, err := c.EmployeesByNIKs(ctx, testGroup.OrganizationID, []string{
		personeltest.NIKDanruSatu, personeltest.NIKAnggota1Satu, personeltest.NIKDanruDua,
	})
	require.NoError(t, err)
	require.Len(t, many, 2, "the person outside the group is omitted")
}

func TestWithoutAGroupTheCompanyIsTheTenant(t *testing.T) {
	_, c := testkit.NewPersonel(t)

	id, err := login(c, personeltest.EmailDanruSatu, personeltest.Password)
	require.NoError(t, err)
	require.EqualValues(t, personeltest.OrgSatu, id.OrganizationID)
	require.EqualValues(t, personeltest.OrgSatu, id.CompanyID)
}
