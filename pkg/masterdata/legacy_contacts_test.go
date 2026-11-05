package masterdata_test

import (
	"encoding/json"
	"io/fs"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"

	appdb "siapp/db"
	"siapp/pkg/masterdata"
	"siapp/pkg/platform/testkit"
)

// runLegacyContactsMigration applies the Up half of migration 00021 to the (already migrated)
// test database, as its owner and in one transaction, like goose does. The migration only touches
// contacts still in the old shape, so running it after the schema is complete is the same as
// running it on a database that was at 00020.
func runLegacyContactsMigration(t *testing.T) {
	t.Helper()
	raw, err := fs.ReadFile(appdb.Migrations, appdb.MigrationsDir+"/00021_06_2_legacy_emergency_contacts.sql")
	require.NoError(t, err)
	up, _, found := strings.Cut(string(raw), "-- +goose Down")
	require.True(t, found)
	up = strings.Replace(up, "-- +goose Up", "", 1)

	tx, err := testkit.OwnerPool(t).Begin(t.Context())
	require.NoError(t, err)
	_, err = tx.Exec(t.Context(), up)
	require.NoError(t, err)
	require.NoError(t, tx.Commit(t.Context()))
}

type legacyContact struct {
	StakeholderTypeID int64  `json:"stakeholder_type_id"`
	Name              string `json:"name"`
	Phone             string `json:"phone"`
}

func (e *env) projectContacts(org int64, project string) []legacyContact {
	e.t.Helper()
	var out []legacyContact
	e.inTenant(org, func(tx pgx.Tx) {
		var raw []byte
		require.NoError(e.t, tx.QueryRow(e.t.Context(), "SELECT emergency_contacts FROM projects WHERE code = $1", project).Scan(&raw))
		require.NoError(e.t, json.Unmarshal(raw, &out))
	})
	return out
}

// stakeholderIDByName is the id and code of the live stakeholder type called name.
func (e *env) stakeholderIDByName(org int64, name string) (id int64, code string) {
	e.t.Helper()
	e.inTenant(org, func(tx pgx.Tx) {
		require.NoError(e.t, tx.QueryRow(e.t.Context(),
			"SELECT id, code FROM stakeholder_types WHERE lower(name) = lower($1) AND deleted_at IS NULL", name).Scan(&id, &code))
	})
	return id, code
}

func (e *env) panicStakeholderCode(org int64, panicCode string) *string {
	e.t.Helper()
	var code *string
	e.inTenant(org, func(tx pgx.Tx) {
		require.NoError(e.t, tx.QueryRow(e.t.Context(),
			`SELECT s.code FROM panic_types p LEFT JOIN stakeholder_types s ON s.id = p.stakeholder_type_id WHERE p.code = $1`,
			panicCode).Scan(&code))
	})
	return code
}

func TestMigration00021ConvertsLegacyEmergencyContacts(t *testing.T) {
	e := newEnv(t)
	org := testkit.NewOrg(t)
	// 06.1 organizations have panic types that call nobody.
	e.exec(org, `INSERT INTO panic_types (code, name, sort_order) VALUES
		('SECURITY', 'Keamanan', 0), ('MEDICAL', 'Medis', 1), ('FIRE', 'Kebakaran', 2), ('OTHER', 'Lainnya', 3)`)
	e.serveProject(org, "1K-IBMI-S", `[
		{"type": "Polisi",       "name": "Polsek A",  "phone": "0812-3456-7890"},
		{"type": "RS",           "name": "RSUD",      "phone": "(021) 555 1234"},
		{"type": "Satpam Pusat", "name": "Pos",       "phone": "+628112223334"},
		{"type": "  ",           "name": "Kosong",    "phone": "0811111111"},
		{"type": "polisi",       "name": "Polres B",  "phone": "6281234567890"},
		{"type": "Pemadam",      "name": "Damkar",    "phone": "abc"},
		{"type": "Polisi",       "name": "Terpanjang", "phone": "08123456789012"},
		{"type": "Polisi",       "name": "Kelebihan",  "phone": "081234567890123"}
	]`)

	runLegacyContactsMigration(t)

	police, policeCode := e.stakeholderIDByName(org, "Polisi")
	hospital, hospitalCode := e.stakeholderIDByName(org, "Rumah Sakit")
	fire, fireCode := e.stakeholderIDByName(org, "Pemadam Kebakaran")
	legacy, legacyCode := e.stakeholderIDByName(org, "Satpam Pusat")
	require.Equal(t, "POLICE", policeCode)
	require.Equal(t, "HOSPITAL", hospitalCode)
	require.Equal(t, "FIRE_DEPARTMENT", fireCode)
	require.Regexp(t, `^LEGACY_[0-9A-F]{10}$`, legacyCode)

	// The blank-typed contact is dropped, the order of the rest is kept, a phone that cannot be
	// converted stays as it was, and none of the results is a number the API would refuse.
	require.Equal(t, []legacyContact{
		{police, "Polsek A", "+6281234567890"},
		{hospital, "RSUD", "+62215551234"},
		{legacy, "Pos", "+628112223334"},
		{police, "Polres B", "+6281234567890"},
		{fire, "Damkar", "abc"},
		{police, "Terpanjang", "+628123456789012"},
		{police, "Kelebihan", "081234567890123"},
	}, e.projectContacts(org, "1K-IBMI-S"))
	for _, c := range e.projectContacts(org, "1K-IBMI-S")[:3] {
		require.Regexp(t, `^\+[1-9][0-9]{7,14}$`, c.Phone)
	}

	// The default panic types were never linked (06.1 could not), so the migration links them.
	for panic, want := range map[string]string{"SECURITY": "POLICE", "MEDICAL": "HOSPITAL", "FIRE": "FIRE_DEPARTMENT"} {
		got := e.panicStakeholderCode(org, panic)
		require.NotNil(t, got, panic)
		require.Equal(t, want, *got, panic)
	}
	require.Nil(t, e.panicStakeholderCode(org, "OTHER"))

	// Nothing is left to give: the defaults add no stakeholder type, and a second run changes nothing.
	added, err := masterdata.EnsureDefaults(t.Context(), testkit.Pool(t), e.clock, org)
	require.NoError(t, err)
	require.NotZero(t, added, "the app links and categories still come in")
	before := e.projectContacts(org, "1K-IBMI-S")
	runLegacyContactsMigration(t)
	require.Equal(t, before, e.projectContacts(org, "1K-IBMI-S"))
}

func TestMigration00021ReusesAnExistingStakeholderTypeAndKeepsNewContacts(t *testing.T) {
	e := newEnv(t)
	org := testkit.NewOrg(t)
	e.exec(org, "INSERT INTO stakeholder_types (code, name) VALUES ('KEPOLISIAN', 'Polisi')")
	existing, _ := e.stakeholderIDByName(org, "Polisi")
	e.serveProject(org, "1K-IBMI-S", `[
		{"type": "Polisi", "name": "Lama", "phone": "081234567890"},
		{"stakeholder_type_id": 999999, "name": "Baru", "phone": "+6281234567890"}
	]`)

	runLegacyContactsMigration(t)

	// The type with that name is reused instead of a second POLICE with a clashing name, and a
	// contact that is already in the new shape is left alone.
	require.Equal(t, []legacyContact{
		{existing, "Lama", "+6281234567890"},
		{999999, "Baru", "+6281234567890"},
	}, e.projectContacts(org, "1K-IBMI-S"))
}

// An organization whose three default stakeholder types all came from migration 00021 has nothing
// for EnsureDefaults to add there, but its default panic types still have to call them.
func TestAnOrganizationWithAllStakeholderTypesStillGetsItsPanicTypesLinked(t *testing.T) {
	e := newEnv(t)
	org := testkit.NewOrg(t)
	e.serveProject(org, "1K-IBMI-S", `[
		{"type": "Polisi",  "name": "A", "phone": "081234567890"},
		{"type": "RS",      "name": "B", "phone": "081234567891"},
		{"type": "Damkar",  "name": "C", "phone": "081234567892"}
	]`)
	runLegacyContactsMigration(t)

	// No panic type exists yet: the organization is first seen after the migration.
	_, err := masterdata.EnsureDefaults(t.Context(), testkit.Pool(t), e.clock, org)
	require.NoError(t, err)
	for panic, want := range map[string]string{"SECURITY": "POLICE", "MEDICAL": "HOSPITAL", "FIRE": "FIRE_DEPARTMENT"} {
		got := e.panicStakeholderCode(org, panic)
		require.NotNil(t, got, panic)
		require.Equal(t, want, *got, panic)
	}
	require.Nil(t, e.panicStakeholderCode(org, "OTHER"))

	// An admin who clears a link is not overruled by a later call.
	e.exec(org, "UPDATE panic_types SET stakeholder_type_id = NULL WHERE code = 'SECURITY'")
	added, err := masterdata.EnsureDefaults(t.Context(), testkit.Pool(t), e.clock, org)
	require.NoError(t, err)
	require.Zero(t, added)
	require.Nil(t, e.panicStakeholderCode(org, "SECURITY"))
}
