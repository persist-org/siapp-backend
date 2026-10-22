package main

import (
	"os"
	"strconv"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/stretchr/testify/require"

	"siapp/internal/db"
	"siapp/pkg/access"
	"siapp/pkg/platform/database"
	"siapp/pkg/platform/personel/personeltest"
	"siapp/pkg/platform/testkit"
)

func TestMain(m *testing.M) { os.Exit(testkit.NewDatabase(m)) }

// configure points the command at the test database (as the owner role) and the Personel fake,
// the way a developer points it at their own through .env.
func configure(t *testing.T) {
	t.Helper()
	testkit.Reset(t)
	fake, _, baseURL := testkit.NewPersonelWithURL(t)
	t.Setenv("APP_ENV", "test")
	t.Setenv("DATABASE_URL", testkit.AppURL(t))
	t.Setenv("MIGRATE_DSN", testkit.OwnerURL(t))
	t.Setenv("REDIS_URL", "redis://localhost:6379/0")
	t.Setenv("JWT_SECRET", testkit.JWTSecret)
	t.Setenv("PERSONEL_BASE_URL", baseURL)
	t.Setenv("PERSONEL_INTEGRATION_KEY", fake.Key)
	// The group is the fixtures' first company only, so the second company's people are
	// outside it.
	t.Setenv("GROUP_COMPANY_IDS", strconv.FormatInt(personeltest.OrgSatu, 10))
	t.Setenv("GROUP_ORGANIZATION_ID", strconv.FormatInt(groupOrg, 10))
}

// groupOrg is the organization the test group maps to.
const groupOrg int64 = 7

func textOf(s string) pgtype.Text { return pgtype.Text{String: s, Valid: true} }

func roleOf(t *testing.T, org int64) (role string, grantedBy *int64, found bool) {
	t.Helper()
	err := database.WithTenantTx(t.Context(), testkit.Pool(t), org, func(tx pgx.Tx) error {
		rows, err := tx.Query(t.Context(), "SELECT role::text, granted_by_personnel_id FROM web_role_assignments")
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			found = true
			if err := rows.Scan(&role, &grantedBy); err != nil {
				return err
			}
		}
		return rows.Err()
	})
	require.NoError(t, err)
	return role, grantedBy, found
}

func TestGrantAdminCreatesTheFirstAdmin(t *testing.T) {
	configure(t)

	// Nobody has ever signed in: the person is fetched from Personalia by NIK first.
	require.NoError(t, run([]string{"grant-admin", "--nik", personeltest.NIKDanruSatu}))

	role, grantedBy, found := roleOf(t, groupOrg)
	require.True(t, found)
	require.Equal(t, access.RoleOrgAdmin, role)
	require.Nil(t, grantedBy, "nobody granted it: it came from the command line")

	// The person is a real, active row and an admin by the gate's own check.
	var admin bool
	require.NoError(t, database.WithTenant(t.Context(), testkit.Pool(t), groupOrg, func(q *db.Queries) error {
		p, err := q.GetPersonnelByNIK(t.Context(), textOf(personeltest.NIKDanruSatu))
		require.NoError(t, err)
		require.Equal(t, db.PersonnelStatusACTIVE, p.Status)
		_, admin, err = access.AdminScopeOf(t.Context(), q, p.ID)
		return err
	}))
	require.True(t, admin)

	// Audited as a grant with no actor.
	require.NoError(t, database.WithTenantTx(t.Context(), testkit.Pool(t), groupOrg, func(tx pgx.Tx) error {
		var action string
		var actor *int64
		require.NoError(t, tx.QueryRow(t.Context(), "SELECT action, actor_personnel_id FROM audit_logs").Scan(&action, &actor))
		require.Equal(t, "web_role.grant", action)
		require.Nil(t, actor)
		return nil
	}))

	// Running it again is safe.
	require.NoError(t, run([]string{"grant-admin", "--nik", personeltest.NIKDanruSatu}))
}

func TestGrantAdminRefusesWhatItCannotFind(t *testing.T) {
	configure(t)

	for name, args := range map[string][]string{
		"a NIK of a company outside the group": {"grant-admin", "--nik", personeltest.NIKDanruDua},
		"a NIK nobody has":                     {"grant-admin", "--nik", "NOPE0000"},
		"no flags":                             {"grant-admin"},
		"no NIK":                               {"grant-admin"},
		"an unknown command":                   {"nope"},
	} {
		require.Error(t, run(args), name)
	}
	_, _, found := roleOf(t, groupOrg)
	require.False(t, found, "nothing was granted")
}

func TestWithoutArgumentsItPrintsTheAccounts(t *testing.T) {
	require.NoError(t, run(nil))
}
