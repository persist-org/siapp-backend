package masterdata_test

import (
	"net/http"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"

	"siapp/pkg/masterdata"
	"siapp/pkg/platform/testkit"
)

// ---------- category names ----------

func TestACategoryNameIsUniquePerBusinessLine(t *testing.T) {
	e := newEnv(t)
	_, admin := e.seeded()
	post := func(body map[string]any) reply {
		return e.raw(admin, http.MethodPost, "/admin/report-categories", body)
	}
	taken := func(r reply) {
		t.Helper()
		r.requireFailure(t, http.StatusConflict, "DUPLICATE", masterdata.ReasonCategoryNameTaken)
	}

	e.createCategory(admin, "report", map[string]any{"name": "Pencurian", "business_lines": []string{"security"}})
	// The same name on another line is a separate category.
	cleaning := e.createCategory(admin, "report", map[string]any{"name": "Pencurian", "business_lines": []string{"cleaning"}})

	// The same line again, in any case or spacing, clashes.
	taken(post(map[string]any{"name": "Pencurian", "business_lines": []string{"security"}}))
	taken(post(map[string]any{"name": "  pencurian ", "business_lines": []string{"security"}}))
	taken(post(map[string]any{"name": "PENCURIAN", "business_lines": []string{"cleaning", "security"}}))
	// No lines means every line, so it overlaps both.
	taken(post(map[string]any{"name": "Pencurian"}))
	taken(post(map[string]any{"name": "Pencurian", "business_lines": []string{}}))
	// And a seeded all-lines name cannot be added for one line.
	taken(post(map[string]any{"name": "Kejadian", "business_lines": []string{"security"}}))

	// A patch that moves a category onto a taken line, or renames it onto a taken name, clashes too.
	e.raw(admin, http.MethodPatch, "/admin/report-categories/"+itoa(cleaning.ID), map[string]any{"business_lines": []string{"security"}}).
		requireFailure(t, http.StatusConflict, "DUPLICATE", masterdata.ReasonCategoryNameTaken)
	e.raw(admin, http.MethodPatch, "/admin/report-categories/"+itoa(cleaning.ID), map[string]any{"business_lines": []string{}}).
		requireFailure(t, http.StatusConflict, "DUPLICATE", masterdata.ReasonCategoryNameTaken)
	e.raw(admin, http.MethodPatch, "/admin/report-categories/"+itoa(cleaning.ID), map[string]any{"name": "kejadian"}).
		requireFailure(t, http.StatusConflict, "DUPLICATE", masterdata.ReasonCategoryNameTaken)
	// Renaming a category to its own name in another case is not a clash with itself.
	same := e.api(admin, http.MethodPatch, "/admin/report-categories/"+itoa(cleaning.ID), map[string]any{"name": "PENCURIAN"})
	require.Equal(t, http.StatusOK, same.Status, "body: %s", same.Raw)
	require.Equal(t, "PENCURIAN", data[category](t, same).Name)

	// The other kind is separate.
	e.createCategory(admin, "instruction", map[string]any{"name": "Pencurian", "urgency": "HIGH"})
}

func TestAnInactiveCategoryStillHoldsItsName(t *testing.T) {
	e := newEnv(t)
	_, admin := e.seeded()
	off := e.createCategory(admin, "report", map[string]any{"name": "Vandalisme", "is_active": false})
	require.False(t, off.IsActive)

	e.raw(admin, http.MethodPost, "/admin/report-categories", map[string]any{"name": "Vandalisme"}).
		requireFailure(t, http.StatusConflict, "DUPLICATE", masterdata.ReasonCategoryNameTaken)
	e.raw(admin, http.MethodPost, "/admin/report-categories", map[string]any{"name": "vandalisme", "is_active": false}).
		requireFailure(t, http.StatusConflict, "DUPLICATE", masterdata.ReasonCategoryNameTaken)

	// Once deleted, the name is free again.
	require.Equal(t, http.StatusOK, e.api(admin, http.MethodDelete, "/admin/report-categories/"+itoa(off.ID), nil).Status)
	e.createCategory(admin, "report", map[string]any{"name": "Vandalisme"})
}

func TestTwoAdminsCreatingOneNameAtOnceGetOneCategory(t *testing.T) {
	e := newEnv(t)
	_, admin := e.seeded()
	const n = 8
	statuses := make([]int, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() {
			statuses[i] = e.raw(admin, http.MethodPost, "/admin/report-categories",
				map[string]any{"name": "Serentak", "business_lines": []string{"security"}}).Status
		})
	}
	wg.Wait()
	created := 0
	for _, s := range statuses {
		require.Contains(t, []int{http.StatusCreated, http.StatusConflict}, s)
		if s == http.StatusCreated {
			created++
		}
	}
	require.Equal(t, 1, created, "statuses: %v", statuses)
}

func TestACategoryNameIsCleanedAndChecked(t *testing.T) {
	e := newEnv(t)
	_, admin := e.seeded()

	c := e.createCategory(admin, "report", map[string]any{"name": "  Banyak \t  Spasi\n "})
	require.Equal(t, "Banyak Spasi", c.Name)

	for _, bad := range []any{"", "   ", "\t\n", "Bel\u0007l", 42, nil} {
		r := e.raw(admin, http.MethodPost, "/admin/report-categories", map[string]any{"name": bad})
		require.Equal(t, http.StatusUnprocessableEntity, r.Status, "name %q: %s", bad, r.Raw)
	}
	long := ""
	for range 101 {
		long += "a"
	}
	e.raw(admin, http.MethodPost, "/admin/report-categories", map[string]any{"name": long}).
		requireFailure(t, http.StatusUnprocessableEntity, "VALIDATION_ERROR", "")
	// 100 characters once the spaces around it are gone is fine (raw: the contract's maxLength
	// counts the spaces, the server is more lenient).
	padded := e.raw(admin, http.MethodPost, "/admin/report-categories", map[string]any{"name": "   " + long[:100] + "   "})
	require.Equal(t, http.StatusCreated, padded.Status, "body: %s", padded.Raw)

	for _, body := range []map[string]any{
		{"name": "X", "business_lines": []string{"Security"}},
		{"name": "X", "business_lines": []string{""}},
		{"name": "X", "sort_order": -1},
		{"name": "X", "sort_order": 10001},
		{"name": "X", "sort_order": 1.5},
		{"name": "X", "is_active": "yes"},
	} {
		r := e.raw(admin, http.MethodPost, "/admin/report-categories", body)
		require.Equal(t, http.StatusUnprocessableEntity, r.Status, "%v: %s", body, r.Raw)
	}
	r := e.raw(admin, http.MethodPost, "/admin/report-categories", nil)
	require.Contains(t, []int{http.StatusBadRequest, http.StatusUnprocessableEntity}, r.Status, "no body: %s", r.Raw)
}

func TestACategoryWithoutASortOrderGoesLast(t *testing.T) {
	e := newEnv(t)
	_, admin := e.seeded()
	c := e.createCategory(admin, "report", map[string]any{"name": "Terakhir"})
	require.Equal(t, 4, c.SortOrder, "after the four defaults (0..3)")
	all := e.categories(admin, "/admin/report-categories")
	require.Equal(t, "Terakhir", all[len(all)-1].Name)
}

func TestAPatchThatChangesNothingWritesNothing(t *testing.T) {
	e := newEnv(t)
	org, admin := e.seeded()
	c := e.createCategory(admin, "instruction", map[string]any{"name": "Patroli", "urgency": "LOW", "business_lines": []string{"security"}})
	pt := data[panicType](t, e.api(admin, http.MethodPost, "/admin/panic-types", map[string]any{"name": "Gempa"}))
	audits := len(e.auditActions(org))
	stamp := func(table string, id int64) (out time.Time) {
		e.inTenant(org, func(tx pgx.Tx) {
			require.NoError(t, tx.QueryRow(t.Context(), "SELECT updated_at FROM "+table+" WHERE id = $1", id).Scan(&out))
		})
		return out
	}
	catBefore, ptBefore := stamp("categories", c.ID), stamp("panic_types", pt.ID)
	e.clock.Advance(time.Minute)

	// The same values sent again are no change, whatever the spacing of the name.
	same := e.api(admin, http.MethodPatch, "/admin/instruction-categories/"+itoa(c.ID), map[string]any{
		"name": " Patroli ", "urgency": "LOW", "business_lines": []string{"security", "security"}, "sort_order": c.SortOrder, "is_active": true,
	})
	require.Equal(t, http.StatusOK, same.Status, "body: %s", same.Raw)
	require.Equal(t, http.StatusOK, e.api(admin, http.MethodPatch, "/admin/panic-types/"+itoa(pt.ID), map[string]any{"name": "Gempa", "is_active": true}).Status)
	require.Equal(t, http.StatusOK, e.api(admin, http.MethodPatch, "/admin/panic-types/"+itoa(pt.ID), map[string]any{}).Status)

	require.Len(t, e.auditActions(org), audits)
	require.True(t, catBefore.Equal(stamp("categories", c.ID)), "no updated_at bump, so no delta traffic")
	require.True(t, ptBefore.Equal(stamp("panic_types", pt.ID)))
}

func TestUrgencyOnAPatch(t *testing.T) {
	e := newEnv(t)
	_, admin := e.seeded()
	report := e.createCategory(admin, "report", map[string]any{"name": "Laporan"})
	instruction := e.createCategory(admin, "instruction", map[string]any{"name": "Perintah", "urgency": "LOW"})

	// "No urgency" on a report is what it has: accepted, nothing changes. null is "absent".
	require.Equal(t, http.StatusOK, e.raw(admin, http.MethodPatch, "/admin/report-categories/"+itoa(report.ID), map[string]any{"urgency": ""}).Status)
	require.Equal(t, http.StatusOK, e.raw(admin, http.MethodPatch, "/admin/report-categories/"+itoa(report.ID), map[string]any{"urgency": nil}).Status)
	e.raw(admin, http.MethodPatch, "/admin/report-categories/"+itoa(report.ID), map[string]any{"urgency": "HIGH"}).
		requireFailure(t, http.StatusUnprocessableEntity, "VALIDATION_ERROR", "")

	e.raw(admin, http.MethodPatch, "/admin/instruction-categories/"+itoa(instruction.ID), map[string]any{"urgency": "low"}).
		requireFailure(t, http.StatusUnprocessableEntity, "VALIDATION_ERROR", "")
	up := e.api(admin, http.MethodPatch, "/admin/instruction-categories/"+itoa(instruction.ID), map[string]any{"urgency": "HIGH"})
	require.Equal(t, http.StatusOK, up.Status, "body: %s", up.Raw)
	require.Equal(t, "HIGH", data[category](t, up).Urgency)
	// A null urgency leaves it as it is (the contract types it as a string, so raw).
	kept := e.raw(admin, http.MethodPatch, "/admin/instruction-categories/"+itoa(instruction.ID), map[string]any{"urgency": nil, "name": "Perintah Baru"})
	require.Equal(t, "HIGH", data[category](t, kept).Urgency)
}

func TestABadIDIsNotFound(t *testing.T) {
	e := newEnv(t)
	_, admin := e.seeded()
	for _, id := range []string{"0", "-1", "abc", "1.5", "99999999999999999999", "999999"} {
		e.raw(admin, http.MethodGet, "/admin/report-categories/"+id, nil).
			requireFailure(t, http.StatusNotFound, "NOT_FOUND", masterdata.ReasonCategoryNotFound)
		e.raw(admin, http.MethodPatch, "/admin/panic-types/"+id, map[string]any{"name": "X"}).
			requireFailure(t, http.StatusNotFound, "NOT_FOUND", masterdata.ReasonPanicTypeNotFound)
		e.raw(admin, http.MethodDelete, "/admin/instruction-categories/"+id, nil).
			requireFailure(t, http.StatusNotFound, "NOT_FOUND", masterdata.ReasonCategoryNotFound)
	}
}

func TestAdminListQueryIsChecked(t *testing.T) {
	e := newEnv(t)
	_, admin := e.seeded()
	for _, q := range []string{"?is_active=maybe", "?limit=0", "?limit=101", "?page=0", "?page=x"} {
		r := e.raw(admin, http.MethodGet, "/admin/report-categories"+q, nil)
		require.Equal(t, http.StatusUnprocessableEntity, r.Status, "%s: %s", q, r.Raw)
	}
	// A page past the end is empty, with the right total.
	r := e.api(admin, http.MethodGet, "/admin/report-categories?page=50", nil)
	require.Empty(t, data[[]category](t, r))
}

// ---------- panic types ----------

func TestAPanicTypeNameIsUniqueWhateverTheCode(t *testing.T) {
	e := newEnv(t)
	_, admin := e.seeded()
	taken := func(body map[string]any) {
		t.Helper()
		e.raw(admin, http.MethodPost, "/admin/panic-types", body).
			requireFailure(t, http.StatusConflict, "DUPLICATE", masterdata.ReasonPanicTypeNameTaken)
	}

	made := data[panicType](t, e.api(admin, http.MethodPost, "/admin/panic-types", map[string]any{"name": "Banjir Bandang", "code": "BANJIR"}))
	taken(map[string]any{"name": "Banjir Bandang", "code": "BANJIRR"})
	taken(map[string]any{"name": "banjir   bandang"})
	// A seeded name, too.
	taken(map[string]any{"name": "medis", "code": "MEDIS"})

	// An inactive type still holds its name.
	require.Equal(t, http.StatusOK, e.api(admin, http.MethodPatch, "/admin/panic-types/"+itoa(made.ID), map[string]any{"is_active": false}).Status)
	taken(map[string]any{"name": "Banjir Bandang", "code": "BANJIR2"})

	// Renaming onto another type's name clashes; onto its own in another case does not.
	fire := data[[]panicType](t, e.api(admin, http.MethodGet, "/admin/panic-types?q=FIRE", nil))[0]
	e.raw(admin, http.MethodPatch, "/admin/panic-types/"+itoa(fire.ID), map[string]any{"name": "Banjir Bandang"}).
		requireFailure(t, http.StatusConflict, "DUPLICATE", masterdata.ReasonPanicTypeNameTaken)
	require.Equal(t, http.StatusOK, e.api(admin, http.MethodPatch, "/admin/panic-types/"+itoa(fire.ID), map[string]any{"name": "KEBAKARAN"}).Status)

	// Once deleted, the name is free; the code stays taken.
	require.Equal(t, http.StatusOK, e.api(admin, http.MethodDelete, "/admin/panic-types/"+itoa(made.ID), nil).Status)
	e.raw(admin, http.MethodPost, "/admin/panic-types", map[string]any{"name": "Banjir Bandang", "code": "BANJIR"}).
		requireFailure(t, http.StatusConflict, "DUPLICATE", masterdata.ReasonPanicTypeCodeTaken)
	again := e.api(admin, http.MethodPost, "/admin/panic-types", map[string]any{"name": "Banjir Bandang", "code": "BANJIR_LAGI"})
	require.Equal(t, http.StatusCreated, again.Status, "body: %s", again.Raw)
}

func TestAPanicTypeCodeMadeFromTheNameIsAlwaysUsable(t *testing.T) {
	e := newEnv(t)
	_, admin := e.seeded()
	create := func(body map[string]any) panicType {
		t.Helper()
		r := e.api(admin, http.MethodPost, "/admin/panic-types", body)
		require.Equal(t, http.StatusCreated, r.Status, "body: %s", r.Raw)
		return data[panicType](t, r)
	}

	// "Fire!" makes FIRE, which the default Kebakaran has: the next free suffix is used.
	require.Equal(t, "FIRE_2", create(map[string]any{"name": "Fire!"}).Code)
	require.Equal(t, "FIRE_3", create(map[string]any{"name": "Fire?"}).Code)
	// A name that starts with a digit, or is one letter, gets a letter in front.
	require.Equal(t, "P_112", create(map[string]any{"name": "112"}).Code)
	require.Equal(t, "P_A", create(map[string]any{"name": "a"}).Code)
	// A long name is cut to 30 characters, and so is the code with its suffix.
	long := create(map[string]any{"name": "Kebakaran Hutan Dan Lahan Gambut Besar"})
	require.Len(t, long.Code, 30)
	longer := create(map[string]any{"name": "Kebakaran Hutan Dan Lahan Gambut Besar Sekali"})
	require.LessOrEqual(t, len(longer.Code), 30)
	require.NotEqual(t, long.Code, longer.Code)
	// A sent code is upper-cased and trimmed.
	require.Equal(t, "TSUNAMI", create(map[string]any{"name": "Tsunami", "code": "  tsunami "}).Code)

	// A name with nothing latin in it cannot make a code: the admin has to send one.
	e.raw(admin, http.MethodPost, "/admin/panic-types", map[string]any{"name": "火事"}).
		requireFailure(t, http.StatusUnprocessableEntity, "VALIDATION_ERROR", "")
	require.Equal(t, "KAJI", create(map[string]any{"name": "火事", "code": "KAJI"}).Code)

	for _, bad := range []string{"A", "1ABC", "AB-C", "ABCDEFGHIJKLMNOPQRSTUVWXYZ12345"} {
		r := e.raw(admin, http.MethodPost, "/admin/panic-types", map[string]any{"name": "Kode " + bad, "code": bad})
		require.Equal(t, http.StatusUnprocessableEntity, r.Status, "code %q: %s", bad, r.Raw)
	}
}

// ---------- delta sync ----------

func TestAPersonWhoChangesBusinessLineGetsTheWholeSet(t *testing.T) {
	e := newEnv(t)
	org, admin := e.seeded()
	security := e.createCategory(admin, "report", map[string]any{"name": "Khusus Satpam", "business_lines": []string{"security"}})
	cleaning := e.createCategory(admin, "report", map[string]any{"name": "Khusus Kebersihan", "business_lines": []string{"cleaning"}})

	personID, token := e.person(org, "")
	e.serve(org, personID, "security")
	e.clock.Advance(time.Minute)
	watermark := deltaOf[category](t, e.api(token, http.MethodGet, "/report-categories", nil)).Meta.ServerTime

	// Nothing changed: the delta is empty.
	e.clock.Advance(time.Minute)
	quiet := deltaOf[category](t, e.api(token, http.MethodGet, "/report-categories"+since(watermark), nil))
	require.Empty(t, quiet.Data)
	require.Empty(t, quiet.Deleted)

	// They move to a cleaning project. No category changed, yet what they are offered did.
	now := e.clock.Now()
	e.exec(org, "UPDATE team_members SET end_date = '2001-01-01', updated_at = $2 WHERE personnel_id = $1", personID, now)
	e.exec(org, `
INSERT INTO projects (code, client_code, description, business_line, timezone) VALUES ('1K-IBMI-C', '1K-IBMI', 'Proyek', 'cleaning', 'Asia/Jakarta');
INSERT INTO teams (code, project_code, description) VALUES ('1K-IBMI-C1', '1K-IBMI-C', 'Regu 1');`)
	e.exec(org, "INSERT INTO team_members (team_code, personnel_id, role, start_date, created_at, updated_at) VALUES ('1K-IBMI-C1', $1, 'MEMBER', '2002-01-01', $2, $2)", personID, now)
	e.clock.Advance(time.Minute)

	d := deltaOf[category](t, e.api(token, http.MethodGet, "/report-categories"+since(watermark), nil))
	require.Contains(t, names(d.Data), "Khusus Kebersihan")
	require.Contains(t, names(d.Data), "Kejadian", "the whole offered set, not just changes")
	require.NotContains(t, names(d.Data), "Khusus Satpam")
	require.Contains(t, deletedIDs(d), security.ID)
	require.NotContains(t, deletedIDs(d), cleaning.ID)
}

func TestSinceAcceptsAnOffsetAndRejectsGarbage(t *testing.T) {
	e := newEnv(t)
	org, _ := e.seeded()
	_, token := e.person(org, "")
	at := e.clock.Now().In(time.FixedZone("WIB", 7*3600)).Format(time.RFC3339)

	// Encoded properly, and with the "+" sent raw (it arrives as a space).
	require.Equal(t, http.StatusOK, e.raw(token, http.MethodGet, "/panic-types?since="+url.QueryEscape(at), nil).Status)
	require.Equal(t, http.StatusOK, e.raw(token, http.MethodGet, "/panic-types?since="+at, nil).Status)
	// Fractional seconds, and a time before 1970, are fine.
	require.Equal(t, http.StatusOK, e.raw(token, http.MethodGet, "/panic-types?since=2026-01-01T00:00:00.123456Z", nil).Status)
	old := deltaOf[panicType](t, e.raw(token, http.MethodGet, "/panic-types?since=1900-01-01T00:00:00Z", nil))
	require.Len(t, old.Data, 4, "a pull from before the epoch is a pull from the start")

	for _, bad := range []string{"yesterday", "2026-01-01", "2026-13-01T00:00:00Z", "1730700000"} {
		e.raw(token, http.MethodGet, "/report-categories?since="+url.QueryEscape(bad), nil).
			requireFailure(t, http.StatusUnprocessableEntity, "VALIDATION_ERROR", "")
	}
}

func TestAWrongTypeInANullableFieldNamesTheField(t *testing.T) {
	e := newEnv(t)
	_, admin := e.seeded()
	st := e.stakeholders(admin)[0]
	pt := data[[]panicType](t, e.api(admin, http.MethodGet, "/admin/panic-types", nil))[0]
	lh := e.createLetterhead(admin, map[string]any{"name": "Kop"})
	link := e.appLinks(admin)[0]

	for _, tc := range []struct {
		path  string
		body  map[string]any
		field string
	}{
		{"/admin/stakeholder-types/" + itoa(st.ID), map[string]any{"default_phone": 110}, "default_phone"},
		{"/admin/panic-types/" + itoa(pt.ID), map[string]any{"stakeholder_type_id": "satu"}, "stakeholder_type_id"},
		{"/admin/letterheads/" + itoa(lh.ID), map[string]any{"header_text": []int{1}}, "header_text"},
		{"/admin/letterheads/" + itoa(lh.ID), map[string]any{"logo_media_id": 1.5}, "logo_media_id"},
		{"/admin/app-links/" + itoa(link.ID), map[string]any{"ios_url_scheme": true}, "ios_url_scheme"},
	} {
		r := e.raw(admin, http.MethodPatch, tc.path, tc.body)
		require.Equal(t, http.StatusUnprocessableEntity, r.Status, "%v: %s", tc.body, r.Raw)
		require.Contains(t, string(r.Raw), `"field":"`+tc.field+`"`, "%v: %s", tc.body, r.Raw)
	}
	// Two at once are both reported.
	r := e.raw(admin, http.MethodPatch, "/admin/app-links/"+itoa(link.ID), map[string]any{"url": 1, "android_package": 2})
	require.Contains(t, string(r.Raw), `"field":"url"`)
	require.Contains(t, string(r.Raw), `"field":"android_package"`)
}

func TestTwoNamesThatMakeOneCodeAtOnceBothGetACode(t *testing.T) {
	e := newEnv(t)
	_, admin := e.seeded()
	names := []string{"Banjir", "Banjir!", "banjir?", "BANJIR."}
	codes := make([]string, len(names))
	statuses := make([]int, len(names))
	var wg sync.WaitGroup
	for i, n := range names {
		wg.Go(func() {
			// Different names (the name rule ignores case and spacing, not punctuation).
			r := e.raw(admin, http.MethodPost, "/admin/panic-types", map[string]any{"name": n})
			statuses[i] = r.Status
			if r.Status == http.StatusCreated {
				codes[i] = data[panicType](t, r).Code
			}
		})
	}
	wg.Wait()
	for i, s := range statuses {
		require.Equal(t, http.StatusCreated, s, "%q", names[i])
	}
	require.ElementsMatch(t, []string{"BANJIR", "BANJIR_2", "BANJIR_3", "BANJIR_4"}, codes)
}

func TestACutCodeDoesNotCollideWithACodeAnAdminTyped(t *testing.T) {
	e := newEnv(t)
	_, admin := e.seeded()
	create := func(body map[string]any) reply {
		return e.raw(admin, http.MethodPost, "/admin/panic-types", body)
	}
	name := "Kebakaran Hutan Dan Lahan Gambut Besar" // KEBAKARAN_HUTAN_DAN_LAHAN_GAMBU (cut to 30)
	first := create(map[string]any{"name": name})
	require.Equal(t, http.StatusCreated, first.Status, "body: %s", first.Raw)
	code := data[panicType](t, first).Code
	require.Len(t, code, 30)
	// An admin types the very code the next suffix would make.
	typed := code[:28] + "_2"
	require.Equal(t, http.StatusCreated, create(map[string]any{"name": "Diketik", "code": typed}).Status)

	next := create(map[string]any{"name": name + " Lagi"})
	require.Equal(t, http.StatusCreated, next.Status, "a request without a code never gets CODE_TAKEN: %s", next.Raw)
	got := data[panicType](t, next).Code
	require.NotEqual(t, typed, got)
	require.LessOrEqual(t, len(got), 30)
}

func TestARegionAdminIsRefusedBeforeTheBodyIsRead(t *testing.T) {
	e := newEnv(t)
	org, admin := e.seeded()
	_, region := e.person(org, "REGION_ADMIN")
	c := e.categories(admin, "/admin/report-categories")[0]
	for _, tc := range []struct{ method, path string }{
		{http.MethodPost, "/admin/report-categories"},
		{http.MethodPatch, "/admin/report-categories/" + itoa(c.ID)},
		{http.MethodPost, "/admin/stakeholder-types"},
		{http.MethodPost, "/admin/app-links"},
		{http.MethodPost, "/admin/letterheads"},
	} {
		// A body that would be a 422 for an ORG_ADMIN.
		r := e.raw(region, tc.method, tc.path, map[string]any{"nonsense": true})
		r.requireFailure(t, http.StatusForbidden, "ROLE_DENIED", "ACCESS_ORG_ADMIN_ONLY")
	}
	e.raw(region, http.MethodGet, "/admin/panic-types/abc", nil).
		requireFailure(t, http.StatusForbidden, "ROLE_DENIED", "ACCESS_ORG_ADMIN_ONLY")
}

func TestAnOrganizationWithSomeStakeholderTypesGetsTheRest(t *testing.T) {
	e := newEnv(t)
	org := testkit.NewOrg(t)
	// What migration 00021 leaves for an organization whose projects named only the police.
	e.exec(org, "INSERT INTO stakeholder_types (code, name) VALUES ('POLICE', 'Polisi')")

	_, err := masterdata.EnsureDefaults(t.Context(), testkit.Pool(t), e.clock, org)
	require.NoError(t, err)
	_, admin := e.person(org, "ORG_ADMIN")
	var codes []string
	for _, s := range e.stakeholders(admin) {
		codes = append(codes, s.Code)
	}
	require.ElementsMatch(t, []string{"POLICE", "HOSPITAL", "FIRE_DEPARTMENT"}, codes)
	police := e.stakeholderByCode(admin, "POLICE")
	security := panicByCode(data[[]panicType2](t, e.api(admin, http.MethodGet, "/admin/panic-types", nil)), "SECURITY")
	require.Equal(t, police.ID, *security.StakeholderTypeID, "the default panic types are linked to it")

	// A default an admin deleted is not given back.
	hospital := e.stakeholderByCode(admin, "HOSPITAL")
	e.exec(org, "UPDATE panic_types SET stakeholder_type_id = NULL WHERE stakeholder_type_id = $1", hospital.ID)
	require.Equal(t, http.StatusOK, e.api(admin, http.MethodDelete, "/admin/stakeholder-types/"+itoa(hospital.ID), nil).Status)
	added, err := masterdata.EnsureDefaults(t.Context(), testkit.Pool(t), e.clock, org)
	require.NoError(t, err)
	require.Zero(t, added)
	require.Len(t, e.stakeholders(admin), 2)
}
