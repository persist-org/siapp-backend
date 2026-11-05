package masterdata_test

import (
	"context"
	"net/http"
	"slices"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"

	"siapp/internal/db"
	"siapp/pkg/masterdata"
	"siapp/pkg/media"
	"siapp/pkg/platform/auth"
	"siapp/pkg/platform/testkit"
)

// fakeLogos stands in for the media module: it knows which photos are "uploaded", lets each be
// attached once, and answers a URL for the ones attached to the record asked about.
type fakeLogos struct {
	mu       sync.Mutex
	uploaded map[int64]bool
	attached map[int64]string // media id -> "TYPE:id"
	calls    int
}

func newFakeLogos() *fakeLogos {
	return &fakeLogos{uploaded: map[int64]bool{}, attached: map[int64]string{}}
}

// upload makes a photo exist, as if the caller had registered and confirmed it. The row is real,
// because a letterhead's logo is a foreign key to the media table.
func (e *env) upload(org, owner int64) int64 {
	e.t.Helper()
	var id int64
	e.inTenant(org, func(tx pgx.Tx) {
		require.NoError(e.t, tx.QueryRow(e.t.Context(), `
INSERT INTO media (owner_personnel_id, purpose, content_type, size_bytes, storage_key, status, capture_source)
VALUES ($1, 'LETTERHEAD_LOGO', 'image/png', 1000, 'k/' || gen_random_uuid()::text, 'UPLOADED', 'UNKNOWN') RETURNING id`, owner).Scan(&id))
	})
	e.logos.mu.Lock()
	defer e.logos.mu.Unlock()
	e.logos.uploaded[id] = true
	return id
}

func (f *fakeLogos) Attach(_ context.Context, _ *db.Queries, in media.AttachInput) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	for _, id := range in.MediaIDs {
		switch {
		case !f.uploaded[id]:
			return media.ErrNotFound
		case in.Purpose != media.PurposeLetterheadLogo:
			return media.ErrPurposeMismatch
		case f.attached[id] != "":
			return media.ErrAlreadyAttached
		}
		f.attached[id] = in.EntityType + ":" + strconv.FormatInt(in.EntityID, 10)
	}
	return nil
}

func (f *fakeLogos) Detach(_ context.Context, _ *db.Queries, entityType string, entityID int64, keep []int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	owner := entityType + ":" + strconv.FormatInt(entityID, 10)
	for id, at := range f.attached {
		if at == owner && !slices.Contains(keep, id) {
			delete(f.attached, id)
		}
	}
	return nil
}

// attachedTo says what a photo is attached to ("" for nothing).
func (f *fakeLogos) attachedTo(id int64) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.attached[id]
}

func (f *fakeLogos) Views(_ context.Context, _ *db.Queries, entityType string, entityID int64, ids []int64) ([]media.View, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []media.View
	for _, id := range ids {
		if f.attached[id] == entityType+":"+strconv.FormatInt(entityID, 10) {
			out = append(out, media.View{ID: id, URL: "https://files.test/logo/" + strconv.FormatInt(id, 10)})
		}
	}
	return out, nil
}

type stakeholder struct {
	ID           int64   `json:"id"`
	Code         string  `json:"code"`
	Name         string  `json:"name"`
	DefaultPhone *string `json:"default_phone"`
	SortOrder    int     `json:"sort_order"`
	IsActive     bool    `json:"is_active"`
}

func (e *env) stakeholders(token string) []stakeholder {
	e.t.Helper()
	r := e.api(token, http.MethodGet, "/admin/stakeholder-types", nil)
	require.Equal(e.t, http.StatusOK, r.Status, "body: %s", r.Raw)
	return data[[]stakeholder](e.t, r)
}

func (e *env) stakeholderByCode(token, code string) stakeholder {
	e.t.Helper()
	for _, s := range e.stakeholders(token) {
		if s.Code == code {
			return s
		}
	}
	e.t.Fatalf("no stakeholder type %s", code)
	return stakeholder{}
}

func phoneOf(s stakeholder) string {
	if s.DefaultPhone == nil {
		return "<nil>"
	}
	return *s.DefaultPhone
}

// ---------- seeding ----------

func TestTheDefaultStakeholdersAreSeededAndTheDefaultPanicTypesCallThem(t *testing.T) {
	e := newEnv(t)
	org, admin := e.seeded()

	var codes []string
	for _, s := range e.stakeholders(admin) {
		codes = append(codes, s.Code)
		require.Nil(t, s.DefaultPhone, "no default phone is seeded")
	}
	require.Equal(t, []string{"POLICE", "HOSPITAL", "FIRE_DEPARTMENT"}, codes)

	police := e.stakeholderByCode(admin, "POLICE")
	panics := data[[]panicType2](t, e.api(admin, http.MethodGet, "/admin/panic-types", nil))
	byCode := map[string]*int64{}
	for _, p := range panics {
		byCode[p.Code] = p.StakeholderTypeID
	}
	require.Equal(t, police.ID, *byCode["SECURITY"])
	require.NotNil(t, byCode["MEDICAL"])
	require.NotNil(t, byCode["FIRE"])
	require.Nil(t, byCode["OTHER"], "Lainnya calls nobody in particular")

	// An admin who clears a link is not given it back by the next login.
	sec := panicByCode(panics, "SECURITY")
	r := e.raw(admin, http.MethodPatch, "/admin/panic-types/"+itoa(sec.ID), map[string]any{"stakeholder_type_id": nil})
	require.Equal(t, http.StatusOK, r.Status, "body: %s", r.Raw)
	require.Nil(t, data[panicType2](t, r).StakeholderTypeID)
	added, err := masterdata.EnsureDefaults(t.Context(), testkit.Pool(t), e.clock, org)
	require.NoError(t, err)
	require.Zero(t, added)
	after := data[panicType2](t, e.api(admin, http.MethodGet, "/admin/panic-types/"+itoa(sec.ID), nil))
	require.Nil(t, after.StakeholderTypeID)
}

type panicType2 struct {
	ID                int64  `json:"id"`
	Code              string `json:"code"`
	StakeholderTypeID *int64 `json:"stakeholder_type_id"`
}

func panicByCode(all []panicType2, code string) panicType2 {
	for _, p := range all {
		if p.Code == code {
			return p
		}
	}
	return panicType2{}
}

// ---------- stakeholder types ----------

func TestAnAdminManagesStakeholderTypes(t *testing.T) {
	e := newEnv(t)
	org, admin := e.seeded()

	made := e.api(admin, http.MethodPost, "/admin/stakeholder-types", map[string]any{"name": "Satpam Gedung", "default_phone": "0812-3456-7890", "sort_order": 9})
	require.Equal(t, http.StatusCreated, made.Status, "body: %s", made.Raw)
	st := data[stakeholder](t, made)
	require.Equal(t, "SATPAM_GEDUNG", st.Code)
	require.Equal(t, "+6281234567890", phoneOf(st), "a local number is stored as E.164")

	// The name is unique ignoring case and spacing, inactive types included; the code is made free.
	e.raw(admin, http.MethodPost, "/admin/stakeholder-types", map[string]any{"name": " satpam   gedung "}).
		requireFailure(t, http.StatusConflict, "DUPLICATE", masterdata.ReasonStakeholderTypeNameTaken)
	e.raw(admin, http.MethodPost, "/admin/stakeholder-types", map[string]any{"name": "polisi"}).
		requireFailure(t, http.StatusConflict, "DUPLICATE", masterdata.ReasonStakeholderTypeNameTaken)
	e.raw(admin, http.MethodPost, "/admin/stakeholder-types", map[string]any{"name": "Lain", "code": "POLICE"}).
		requireFailure(t, http.StatusConflict, "DUPLICATE", masterdata.ReasonStakeholderTypeCodeTaken)
	other := data[stakeholder](t, e.api(admin, http.MethodPost, "/admin/stakeholder-types", map[string]any{"name": "Satpam Gedung!"}))
	require.Equal(t, "SATPAM_GEDUNG_2", other.Code)

	// Default phone: normalised, short national numbers kept, junk refused, null clears.
	for _, bad := range []string{"abc", "1234", "+0812", "08"} {
		e.raw(admin, http.MethodPost, "/admin/stakeholder-types", map[string]any{"name": "Bad " + bad, "default_phone": bad}).
			requireFailure(t, http.StatusUnprocessableEntity, "VALIDATION_ERROR", "")
	}
	pol := e.stakeholderByCode(admin, "POLICE")
	set := e.api(admin, http.MethodPatch, "/admin/stakeholder-types/"+itoa(pol.ID), map[string]any{"default_phone": "110"})
	require.Equal(t, http.StatusOK, set.Status, "body: %s", set.Raw)
	require.Equal(t, "110", phoneOf(data[stakeholder](t, set)))
	cleared := e.raw(admin, http.MethodPatch, "/admin/stakeholder-types/"+itoa(pol.ID), map[string]any{"default_phone": nil})
	require.Equal(t, http.StatusOK, cleared.Status, "body: %s", cleared.Raw)
	require.Nil(t, data[stakeholder](t, cleared).DefaultPhone)
	blank := e.api(admin, http.MethodPatch, "/admin/stakeholder-types/"+itoa(pol.ID), map[string]any{"default_phone": "110"})
	require.Equal(t, http.StatusOK, blank.Status)
	require.Equal(t, http.StatusOK, e.api(admin, http.MethodPatch, "/admin/stakeholder-types/"+itoa(pol.ID), map[string]any{"default_phone": ""}).Status)
	require.Nil(t, e.stakeholderByCode(admin, "POLICE").DefaultPhone, "blank clears too")

	// The code is the identity.
	e.raw(admin, http.MethodPatch, "/admin/stakeholder-types/"+itoa(pol.ID), map[string]any{"code": "X"}).
		requireFailure(t, http.StatusUnprocessableEntity, "VALIDATION_ERROR", "")

	// Nothing changed, nothing written.
	audits := len(e.auditActions(org))
	e.clock.Advance(time.Minute)
	require.Equal(t, http.StatusOK, e.api(admin, http.MethodPatch, "/admin/stakeholder-types/"+itoa(st.ID), map[string]any{"name": "Satpam Gedung", "default_phone": "+6281234567890", "is_active": true}).Status)
	require.Len(t, e.auditActions(org), audits)

	// A deleted type frees its name and keeps its code.
	require.Equal(t, http.StatusOK, e.api(admin, http.MethodDelete, "/admin/stakeholder-types/"+itoa(other.ID), nil).Status)
	e.raw(admin, http.MethodGet, "/admin/stakeholder-types/"+itoa(other.ID), nil).
		requireFailure(t, http.StatusNotFound, "NOT_FOUND", masterdata.ReasonStakeholderTypeNotFound)
	e.raw(admin, http.MethodPost, "/admin/stakeholder-types", map[string]any{"name": "Baru", "code": "SATPAM_GEDUNG_2"}).
		requireFailure(t, http.StatusConflict, "DUPLICATE", masterdata.ReasonStakeholderTypeCodeTaken)
	for _, id := range []string{"0", "x", "999999"} {
		e.raw(admin, http.MethodGet, "/admin/stakeholder-types/"+id, nil).
			requireFailure(t, http.StatusNotFound, "NOT_FOUND", masterdata.ReasonStakeholderTypeNotFound)
	}
}

func TestOnlyAnOrgAdminReachesTheNewMasterRoutes(t *testing.T) {
	e := newEnv(t)
	org, _ := e.seeded()
	_, region := e.person(org, "REGION_ADMIN")
	_, nobody := e.person(org, "")
	for path, body := range map[string]map[string]any{
		"/admin/stakeholder-types": {"name": "X"},
		"/admin/letterheads":       {"name": "X"},
		"/admin/app-links":         {"name": "X", "icon_key": "x", "url": "https://x.test"},
	} {
		for _, token := range []string{region, nobody} {
			r := e.raw(token, http.MethodGet, path, nil)
			require.Equal(t, http.StatusForbidden, r.Status, "%s: %s", path, r.Raw)
			r = e.raw(token, http.MethodPost, path, body)
			require.Equal(t, http.StatusForbidden, r.Status, "%s: %s", path, r.Raw)
		}
	}
}

func TestAStakeholderTypeInUseCannotBeDeleted(t *testing.T) {
	e := newEnv(t)
	org, admin := e.seeded()
	hospital := e.stakeholderByCode(admin, "HOSPITAL")
	free := data[stakeholder](t, e.api(admin, http.MethodPost, "/admin/stakeholder-types", map[string]any{"name": "Bebas"}))

	// A panic type calls it.
	e.raw(admin, http.MethodDelete, "/admin/stakeholder-types/"+itoa(hospital.ID), nil).
		requireFailure(t, http.StatusConflict, "CONFLICT", masterdata.ReasonMasterInUse)

	// A project's contact names it.
	e.serveProject(org, "1K-IBMI-S", `[{"stakeholder_type_id": `+itoa(free.ID)+`, "name": "Pos", "phone": "+628123456789"}]`)
	e.raw(admin, http.MethodDelete, "/admin/stakeholder-types/"+itoa(free.ID), nil).
		requireFailure(t, http.StatusConflict, "CONFLICT", masterdata.ReasonMasterInUse)

	// What the client offers instead.
	off := e.api(admin, http.MethodPatch, "/admin/stakeholder-types/"+itoa(free.ID), map[string]any{"is_active": false})
	require.Equal(t, http.StatusOK, off.Status, "body: %s", off.Raw)
	require.False(t, data[stakeholder](t, off).IsActive)

	// Once nothing points at it, it goes.
	e.exec(org, "UPDATE projects SET emergency_contacts = '[]' WHERE code = '1K-IBMI-S'")
	require.Equal(t, http.StatusOK, e.api(admin, http.MethodDelete, "/admin/stakeholder-types/"+itoa(free.ID), nil).Status)
}

func TestAPanicTypeNamesItsStakeholderType(t *testing.T) {
	e := newEnv(t)
	org, admin := e.seeded()
	fire := e.stakeholderByCode(admin, "FIRE_DEPARTMENT")

	made := e.api(admin, http.MethodPost, "/admin/panic-types", map[string]any{"name": "Gas Bocor", "stakeholder_type_id": fire.ID})
	require.Equal(t, http.StatusCreated, made.Status, "body: %s", made.Raw)
	require.Equal(t, fire.ID, *data[panicType2](t, made).StakeholderTypeID)

	for _, id := range []int64{999999} {
		e.raw(admin, http.MethodPost, "/admin/panic-types", map[string]any{"name": "Tanpa Tipe", "stakeholder_type_id": id}).
			requireFailure(t, http.StatusUnprocessableEntity, "VALIDATION_ERROR", masterdata.ReasonStakeholderTypeNotFound)
	}
	e.raw(admin, http.MethodPost, "/admin/panic-types", map[string]any{"name": "Nol", "stakeholder_type_id": 0}).
		requireFailure(t, http.StatusUnprocessableEntity, "VALIDATION_ERROR", "")

	// Another organization's type is not there.
	otherOrg, otherAdmin := e.seeded()
	foreign := e.stakeholderByCode(otherAdmin, "POLICE")
	e.raw(admin, http.MethodPost, "/admin/panic-types", map[string]any{"name": "Asing", "stakeholder_type_id": foreign.ID}).
		requireFailure(t, http.StatusUnprocessableEntity, "VALIDATION_ERROR", masterdata.ReasonStakeholderTypeNotFound)
	_ = otherOrg

	// A deleted type cannot be chosen; a patch can move and clear the link.
	temp := data[stakeholder](t, e.api(admin, http.MethodPost, "/admin/stakeholder-types", map[string]any{"name": "Sementara"}))
	require.Equal(t, http.StatusOK, e.api(admin, http.MethodDelete, "/admin/stakeholder-types/"+itoa(temp.ID), nil).Status)
	pt := data[panicType2](t, made)
	e.raw(admin, http.MethodPatch, "/admin/panic-types/"+itoa(pt.ID), map[string]any{"stakeholder_type_id": temp.ID}).
		requireFailure(t, http.StatusUnprocessableEntity, "VALIDATION_ERROR", masterdata.ReasonStakeholderTypeNotFound)
	police := e.stakeholderByCode(admin, "POLICE")
	moved := e.api(admin, http.MethodPatch, "/admin/panic-types/"+itoa(pt.ID), map[string]any{"stakeholder_type_id": police.ID})
	require.Equal(t, police.ID, *data[panicType2](t, moved).StakeholderTypeID)
	_ = org
}

// ---------- the project phone book ----------

// serveProject makes a security project with the given emergency contacts (raw JSON).
func (e *env) serveProject(org int64, code, contacts string) {
	e.t.Helper()
	e.exec(org, `
INSERT INTO regions (code, description) VALUES ('OS1', 'Outsourcing 1') ON CONFLICT DO NOTHING;
INSERT INTO branches (code, region_code, company_letter, description) VALUES ('1K', 'OS1', 'K', 'Cabang 1K') ON CONFLICT DO NOTHING;
INSERT INTO clients (code, branch_code, description, company_prefix) VALUES ('1K-IBMI', '1K', 'IBMI', 'IBMI') ON CONFLICT DO NOTHING;`)
	e.exec(org, "INSERT INTO projects (code, client_code, description, business_line, timezone, emergency_contacts) VALUES ($1, '1K-IBMI', 'Proyek', 'security', 'Asia/Jakarta', $2::jsonb) ON CONFLICT (organization_id, code) DO UPDATE SET emergency_contacts = EXCLUDED.emergency_contacts",
		code, contacts)
}

type phoneBookEntry struct {
	StakeholderTypeID int64  `json:"stakeholder_type_id"`
	TypeCode          string `json:"type_code"`
	Name              string `json:"name"`
	Phone             string `json:"phone"`
	IsDefault         bool   `json:"is_default"`
}

func (e *env) phoneBook(token, project string) reply {
	e.t.Helper()
	return e.raw(token, http.MethodGet, "/projects/"+project+"/emergency-contacts", nil)
}

func TestAProjectsPhoneBookIsForItsMembers(t *testing.T) {
	e := newEnv(t)
	org, admin := e.seeded()
	police := e.stakeholderByCode(admin, "POLICE")
	hospital := e.stakeholderByCode(admin, "HOSPITAL")
	fire := e.stakeholderByCode(admin, "FIRE_DEPARTMENT")
	e.api(admin, http.MethodPatch, "/admin/stakeholder-types/"+itoa(hospital.ID), map[string]any{"default_phone": "119"})
	e.api(admin, http.MethodPatch, "/admin/stakeholder-types/"+itoa(fire.ID), map[string]any{"default_phone": "113"})

	// The project has two police contacts and none for the hospital or the fire brigade.
	e.serveProject(org, "1K-IBMI-S", `[
	  {"stakeholder_type_id": `+itoa(police.ID)+`, "name": "Polsek Menteng", "phone": "+62213840110"},
	  {"stakeholder_type_id": `+itoa(police.ID)+`, "name": "Polres Jakpus", "phone": "+62213840111"}]`)
	memberID, member := e.person(org, "")
	e.exec(org, "INSERT INTO teams (code, project_code, description) VALUES ('1K-IBMI-S1', '1K-IBMI-S', 'Regu 1')")
	e.exec(org, "INSERT INTO team_members (team_code, personnel_id, role, start_date) VALUES ('1K-IBMI-S1', $1, 'MEMBER', '2000-01-01')", memberID)

	r := e.api(member, http.MethodGet, "/projects/1K-IBMI-S/emergency-contacts", nil)
	require.Equal(t, http.StatusOK, r.Status, "body: %s", r.Raw)
	book := data[[]phoneBookEntry](t, r)
	require.Len(t, book, 4)
	require.Equal(t, "Polsek Menteng", book[0].Name, "the project's own contacts keep their order")
	require.Equal(t, "Polres Jakpus", book[1].Name)
	require.False(t, book[0].IsDefault)
	// A type with no contact of the project falls back to its default phone, in the types' order.
	require.Equal(t, "HOSPITAL", book[2].TypeCode)
	require.Equal(t, "119", book[2].Phone)
	require.True(t, book[2].IsDefault)
	require.Equal(t, "FIRE_DEPARTMENT", book[3].TypeCode)
	require.Equal(t, "113", book[3].Phone)

	// A person who does not serve there, and an admin who does not either, are denied.
	_, stranger := e.person(org, "")
	e.phoneBook(stranger, "1K-IBMI-S").requireFailure(t, http.StatusForbidden, "SCOPE_DENIED", masterdata.ReasonNotProjectMember)
	e.phoneBook(admin, "1K-IBMI-S").requireFailure(t, http.StatusForbidden, "SCOPE_DENIED", masterdata.ReasonNotProjectMember)
	// A project that is not there is 404, and so is another organization's.
	e.phoneBook(member, "1K-IBMI-X").requireFailure(t, http.StatusNotFound, "NOT_FOUND", masterdata.ReasonProjectNotFound)
	otherOrg := testkit.NewOrg(t)
	e.exec(otherOrg, "INSERT INTO regions (code, description) VALUES ('OS1', 'Outsourcing 1')")
	e.exec(otherOrg, "INSERT INTO branches (code, region_code, company_letter, description) VALUES ('1K', 'OS1', 'K', 'Cabang 1K')")
	e.exec(otherOrg, "INSERT INTO clients (code, branch_code, description, company_prefix) VALUES ('1K-ZZZZ', '1K', 'ZZZZ', 'ZZZZ')")
	e.exec(otherOrg, "INSERT INTO projects (code, client_code, description, business_line, timezone) VALUES ('1K-ZZZZ-S', '1K-ZZZZ', 'Proyek', 'security', 'Asia/Jakarta')")
	e.phoneBook(member, "1K-ZZZZ-S").requireFailure(t, http.StatusNotFound, "NOT_FOUND", masterdata.ReasonProjectNotFound)
	require.Equal(t, http.StatusUnauthorized, e.raw("", http.MethodGet, "/projects/1K-IBMI-S/emergency-contacts", nil).Status)

	// A membership that has ended no longer opens the book.
	e.exec(org, "UPDATE team_members SET end_date = '2001-01-01' WHERE personnel_id = $1", memberID)
	e.phoneBook(member, "1K-IBMI-S").requireFailure(t, http.StatusForbidden, "SCOPE_DENIED", masterdata.ReasonNotProjectMember)
	e.exec(org, "UPDATE team_members SET end_date = NULL, start_date = '2999-01-01' WHERE personnel_id = $1", memberID)
	e.phoneBook(member, "1K-IBMI-S").requireFailure(t, http.StatusForbidden, "SCOPE_DENIED", masterdata.ReasonNotProjectMember)
}

func TestContactsForProjectPutsThePreferredTypeFirst(t *testing.T) {
	e := newEnv(t)
	org, admin := e.seeded()
	police := e.stakeholderByCode(admin, "POLICE")
	hospital := e.stakeholderByCode(admin, "HOSPITAL")
	fire := e.stakeholderByCode(admin, "FIRE_DEPARTMENT")
	e.api(admin, http.MethodPatch, "/admin/stakeholder-types/"+itoa(fire.ID), map[string]any{"default_phone": "113"})
	e.serveProject(org, "1K-IBMI-S", `[
	  {"stakeholder_type_id": `+itoa(police.ID)+`, "name": "Polsek", "phone": "110"},
	  {"stakeholder_type_id": `+itoa(hospital.ID)+`, "name": "RS Harapan", "phone": "+62215550101"}]`)
	ctx := auth.WithPrincipal(t.Context(), auth.Principal{OrganizationID: org, PersonnelID: 1})

	order := func(preferred int64) (out []string) {
		got, err := e.service.ContactsForProject(ctx, "1K-IBMI-S", preferred)
		require.NoError(t, err)
		for _, c := range got {
			out = append(out, c.TypeCode+":"+c.Name)
		}
		return out
	}
	require.Equal(t, []string{"POLICE:Polsek", "HOSPITAL:RS Harapan", "FIRE_DEPARTMENT:Pemadam Kebakaran"}, order(0))
	require.Equal(t, []string{"HOSPITAL:RS Harapan", "POLICE:Polsek", "FIRE_DEPARTMENT:Pemadam Kebakaran"}, order(hospital.ID))
	require.Equal(t, []string{"FIRE_DEPARTMENT:Pemadam Kebakaran", "POLICE:Polsek", "HOSPITAL:RS Harapan"}, order(fire.ID), "a default is preferred too")
	require.Equal(t, order(0), order(999999), "an unknown preferred type changes nothing")

	// A deactivated type keeps showing the project's own contact but is never a fallback.
	e.api(admin, http.MethodPatch, "/admin/stakeholder-types/"+itoa(fire.ID), map[string]any{"is_active": false})
	e.api(admin, http.MethodPatch, "/admin/stakeholder-types/"+itoa(hospital.ID), map[string]any{"is_active": false})
	require.Equal(t, []string{"POLICE:Polsek", "HOSPITAL:RS Harapan"}, order(0))

	_, err := e.service.ContactsForProject(ctx, "NOPE", 0)
	require.ErrorIs(t, err, masterdata.ErrProjectNotFound)
	_, err = e.service.ContactsForProject(t.Context(), "1K-IBMI-S", 0)
	require.Error(t, err, "no caller in ctx")
}

func TestSeedingAtEveryLoginUsesNoIDs(t *testing.T) {
	e := newEnv(t)
	org, _ := e.seeded()
	lastValue := func() (out int64) {
		require.NoError(t, testkit.OwnerPool(t).QueryRow(t.Context(),
			"SELECT last_value FROM panic_types_id_seq").Scan(&out))
		return out
	}
	before := lastValue()
	for range 3 {
		added, err := masterdata.EnsureDefaults(t.Context(), testkit.Pool(t), e.clock, org)
		require.NoError(t, err)
		require.Zero(t, added)
	}
	require.Equal(t, before, lastValue(), "a login that finds the defaults takes no id from the sequence")
}
