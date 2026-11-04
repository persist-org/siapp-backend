package masterdata_test

import (
	"context"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"

	"siapp/internal/db"
	"siapp/pkg/masterdata"
	"siapp/pkg/media"
	"siapp/pkg/platform/database"
	"siapp/pkg/platform/testkit"
)

type letterhead struct {
	ID          int64   `json:"id"`
	Name        string  `json:"name"`
	HeaderText  *string `json:"header_text"`
	FooterText  *string `json:"footer_text"`
	LogoMediaID *int64  `json:"logo_media_id"`
	LogoURL     *string `json:"logo_url"`
	IsDefault   bool    `json:"is_default"`
}

func (e *env) letterheads(token string) []letterhead {
	e.t.Helper()
	r := e.api(token, http.MethodGet, "/admin/letterheads", nil)
	require.Equal(e.t, http.StatusOK, r.Status, "body: %s", r.Raw)
	return data[[]letterhead](e.t, r)
}

func (e *env) createLetterhead(token string, body map[string]any) letterhead {
	e.t.Helper()
	r := e.api(token, http.MethodPost, "/admin/letterheads", body)
	require.Equal(e.t, http.StatusCreated, r.Status, "body: %s", r.Raw)
	return data[letterhead](e.t, r)
}

func defaults(ls []letterhead) (out []string) {
	for _, l := range ls {
		if l.IsDefault {
			out = append(out, l.Name)
		}
	}
	return out
}

func TestTheFirstLetterheadIsTheDefaultAndThereIsOnlyOne(t *testing.T) {
	e := newEnv(t)
	org, admin := e.seeded()

	first := e.createLetterhead(admin, map[string]any{"name": "Kop Utama", "header_text": "PT Janusa\nJl. Merdeka 1", "footer_text": "  Terima kasih  "})
	require.True(t, first.IsDefault, "the first letterhead is the default")
	require.Equal(t, "PT Janusa\nJl. Merdeka 1", *first.HeaderText)
	require.Equal(t, "Terima kasih", *first.FooterText)
	require.Nil(t, first.LogoURL)

	second := e.createLetterhead(admin, map[string]any{"name": "Kop Cabang"})
	require.False(t, second.IsDefault)
	require.Equal(t, []string{"Kop Utama"}, defaults(e.letterheads(admin)))

	// A new default takes over, on a create and on a patch.
	third := e.createLetterhead(admin, map[string]any{"name": "Kop Baru", "is_default": true})
	require.True(t, third.IsDefault)
	require.Equal(t, []string{"Kop Baru"}, defaults(e.letterheads(admin)))
	made := e.api(admin, http.MethodPatch, "/admin/letterheads/"+itoa(second.ID), map[string]any{"is_default": true})
	require.Equal(t, http.StatusOK, made.Status, "body: %s", made.Raw)
	require.Equal(t, []string{"Kop Cabang"}, defaults(e.letterheads(admin)))
	require.Equal(t, "Kop Cabang", e.letterheads(admin)[0].Name, "the default comes first")

	// The default can be taken away; then there is none. The database says the same.
	require.Equal(t, http.StatusOK, e.api(admin, http.MethodPatch, "/admin/letterheads/"+itoa(second.ID), map[string]any{"is_default": false}).Status)
	require.Empty(t, defaults(e.letterheads(admin)))
	e.exec(org, "UPDATE letterheads SET is_default = true WHERE id = $1", first.ID)
	err := database.WithTenantTx(t.Context(), testkit.Pool(t), org, func(tx pgx.Tx) error {
		_, err := tx.Exec(t.Context(), "UPDATE letterheads SET is_default = true WHERE id = $1", third.ID)
		return err
	})
	require.ErrorContains(t, err, "letterheads_default_uq", "a second default is refused by the index")

	// An explicit false on the first letterhead is honoured.
	otherOrg, otherAdmin := e.seeded()
	_ = otherOrg
	none := e.createLetterhead(otherAdmin, map[string]any{"name": "Bukan Default", "is_default": false})
	require.False(t, none.IsDefault)
}

func TestTwoAdminsMakingADefaultAtOnceLeaveOne(t *testing.T) {
	e := newEnv(t)
	_, admin := e.seeded()
	e.createLetterhead(admin, map[string]any{"name": "Awal"})
	const n = 6
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() {
			e.raw(admin, http.MethodPost, "/admin/letterheads", map[string]any{"name": "Serentak " + itoa(int64(i)), "is_default": true})
		})
	}
	wg.Wait()
	require.Len(t, defaults(e.letterheads(admin)), 1)
	require.Len(t, e.letterheads(admin), n+1)
}

func TestALetterheadNameIsUniqueAndChecked(t *testing.T) {
	e := newEnv(t)
	_, admin := e.seeded()
	a := e.createLetterhead(admin, map[string]any{"name": "Kop A"})
	b := e.createLetterhead(admin, map[string]any{"name": "Kop B"})

	e.raw(admin, http.MethodPost, "/admin/letterheads", map[string]any{"name": " kop   a "}).
		requireFailure(t, http.StatusConflict, "DUPLICATE", masterdata.ReasonLetterheadNameTaken)
	e.raw(admin, http.MethodPatch, "/admin/letterheads/"+itoa(b.ID), map[string]any{"name": "KOP A"}).
		requireFailure(t, http.StatusConflict, "DUPLICATE", masterdata.ReasonLetterheadNameTaken)
	require.Equal(t, http.StatusOK, e.api(admin, http.MethodPatch, "/admin/letterheads/"+itoa(a.ID), map[string]any{"name": "KOP A"}).Status, "its own name in another case")

	for _, body := range []map[string]any{
		{"name": ""}, {"name": "   "}, {"name": "X", "header_text": "bel\u0007"}, {"name": "X", "logo_media_id": 0},
		{"name": "X", "is_default": "ya"},
	} {
		r := e.raw(admin, http.MethodPost, "/admin/letterheads", body)
		require.Equal(t, http.StatusUnprocessableEntity, r.Status, "%v: %s", body, r.Raw)
	}
	long := make([]byte, 2001)
	for i := range long {
		long[i] = 'a'
	}
	e.raw(admin, http.MethodPost, "/admin/letterheads", map[string]any{"name": "Panjang", "footer_text": string(long)}).
		requireFailure(t, http.StatusUnprocessableEntity, "VALIDATION_ERROR", "")

	// Blank text is "none", and null or "" clears an existing one.
	c := e.createLetterhead(admin, map[string]any{"name": "Kop C", "header_text": "  \n ", "footer_text": "Ada"})
	require.Nil(t, c.HeaderText)
	cleared := e.raw(admin, http.MethodPatch, "/admin/letterheads/"+itoa(c.ID), map[string]any{"footer_text": nil})
	require.Equal(t, http.StatusOK, cleared.Status, "body: %s", cleared.Raw)
	require.Nil(t, data[letterhead](t, cleared).FooterText)

	for _, id := range []string{"0", "abc", "999999"} {
		e.raw(admin, http.MethodGet, "/admin/letterheads/"+id, nil).
			requireFailure(t, http.StatusNotFound, "NOT_FOUND", masterdata.ReasonLetterheadNotFound)
	}
	_, other := e.seeded()
	e.raw(other, http.MethodGet, "/admin/letterheads/"+itoa(a.ID), nil).
		requireFailure(t, http.StatusNotFound, "NOT_FOUND", masterdata.ReasonLetterheadNotFound)
}

func TestALetterheadLogoIsAttachedOnceAndShownAsAURL(t *testing.T) {
	e := newEnv(t)
	org, admin := e.seeded()
	adminID, _ := e.person(org, "ORG_ADMIN")
	first, second := e.upload(org, adminID), e.upload(org, adminID)
	url := func(id int64) string { return "https://files.test/logo/" + itoa(id) }

	l := e.createLetterhead(admin, map[string]any{"name": "Dengan Logo", "logo_media_id": first})
	require.Equal(t, first, *l.LogoMediaID)
	require.Equal(t, url(first), *l.LogoURL)
	got := data[letterhead](t, e.api(admin, http.MethodGet, "/admin/letterheads/"+itoa(l.ID), nil))
	require.Equal(t, url(first), *got.LogoURL)
	require.Equal(t, url(first), *e.letterheads(admin)[0].LogoURL, "the list shows it too")

	// A photo that is not there, or that another record has, is refused with media's own reasons.
	e.raw(admin, http.MethodPost, "/admin/letterheads", map[string]any{"name": "Foto Hilang", "logo_media_id": 999}).
		requireFailure(t, http.StatusNotFound, "NOT_FOUND", "MEDIA_NOT_FOUND")
	e.raw(admin, http.MethodPost, "/admin/letterheads", map[string]any{"name": "Foto Dipakai", "logo_media_id": first}).
		requireFailure(t, http.StatusConflict, "CONFLICT", "MEDIA_ALREADY_ATTACHED")
	// The failed create left no letterhead behind.
	require.Len(t, e.letterheads(admin), 1)

	// Sending the same logo again is not a change: nothing attaches, nothing is written.
	calls, audits := e.logos.calls, len(e.auditActions(org))
	e.clock.Advance(time.Minute)
	same := e.api(admin, http.MethodPatch, "/admin/letterheads/"+itoa(l.ID), map[string]any{"logo_media_id": first, "name": "Dengan Logo"})
	require.Equal(t, http.StatusOK, same.Status, "body: %s", same.Raw)
	require.Equal(t, calls, e.logos.calls)
	require.Len(t, e.auditActions(org), audits)

	// Another logo replaces it; null clears it.
	swapped := e.api(admin, http.MethodPatch, "/admin/letterheads/"+itoa(l.ID), map[string]any{"logo_media_id": second})
	require.Equal(t, http.StatusOK, swapped.Status, "body: %s", swapped.Raw)
	require.Equal(t, url(second), *data[letterhead](t, swapped).LogoURL)
	gone := e.raw(admin, http.MethodPatch, "/admin/letterheads/"+itoa(l.ID), map[string]any{"logo_media_id": nil})
	require.Equal(t, http.StatusOK, gone.Status, "body: %s", gone.Raw)
	require.Nil(t, data[letterhead](t, gone).LogoMediaID)
	require.Nil(t, data[letterhead](t, gone).LogoURL)
}

func TestALetterheadAnExportUsesCannotBeDeleted(t *testing.T) {
	e := newEnv(t)
	_, admin := e.seeded()
	used := e.createLetterhead(admin, map[string]any{"name": "Dipakai"})
	free := e.createLetterhead(admin, map[string]any{"name": "Bebas"})
	e.service.UseLetterheadUsage(func(_ context.Context, _ *db.Queries, id int64) (bool, error) { return id == used.ID, nil })

	e.raw(admin, http.MethodDelete, "/admin/letterheads/"+itoa(used.ID), nil).
		requireFailure(t, http.StatusConflict, "CONFLICT", masterdata.ReasonMasterInUse)
	require.Equal(t, http.StatusOK, e.api(admin, http.MethodDelete, "/admin/letterheads/"+itoa(free.ID), nil).Status)
	e.raw(admin, http.MethodDelete, "/admin/letterheads/"+itoa(free.ID), nil).
		requireFailure(t, http.StatusNotFound, "NOT_FOUND", masterdata.ReasonLetterheadNotFound)
	// The name is free again once it is gone.
	e.createLetterhead(admin, map[string]any{"name": "Bebas"})
}

func TestTheLogoPurposeExistsInTheDatabase(t *testing.T) {
	e := newEnv(t)
	org, _ := e.seeded()
	e.inTenant(org, func(tx pgx.Tx) {
		var ok bool
		require.NoError(t, tx.QueryRow(t.Context(), "SELECT 'LETTERHEAD_LOGO'::media_purpose = 'LETTERHEAD_LOGO'").Scan(&ok))
		require.True(t, ok)
	})
}

func TestALogoThatIsReplacedOrRemovedIsLetGo(t *testing.T) {
	e := newEnv(t)
	org, admin := e.seeded()
	adminID, _ := e.person(org, "ORG_ADMIN")
	first, second := e.upload(org, adminID), e.upload(org, adminID)
	l := e.createLetterhead(admin, map[string]any{"name": "Kop", "logo_media_id": first})
	owner := "LETTERHEAD:" + itoa(l.ID)
	require.Equal(t, owner, e.logos.attachedTo(first))

	// Replaced: the old photo is no longer attached, so the cleanup job can remove it.
	require.Equal(t, http.StatusOK, e.api(admin, http.MethodPatch, "/admin/letterheads/"+itoa(l.ID), map[string]any{"logo_media_id": second}).Status)
	require.Empty(t, e.logos.attachedTo(first))
	require.Equal(t, owner, e.logos.attachedTo(second))

	// Removed, then chosen again: it attaches again instead of being "already attached".
	require.Equal(t, http.StatusOK, e.raw(admin, http.MethodPatch, "/admin/letterheads/"+itoa(l.ID), map[string]any{"logo_media_id": nil}).Status)
	require.Empty(t, e.logos.attachedTo(second))
	again := e.api(admin, http.MethodPatch, "/admin/letterheads/"+itoa(l.ID), map[string]any{"logo_media_id": second})
	require.Equal(t, http.StatusOK, again.Status, "body: %s", again.Raw)
	require.Equal(t, owner, e.logos.attachedTo(second))

	// Deleted: its logo is let go too.
	require.Equal(t, http.StatusOK, e.api(admin, http.MethodDelete, "/admin/letterheads/"+itoa(l.ID), nil).Status)
	require.Empty(t, e.logos.attachedTo(second))
}

func TestARealMediaDetachReleasesOnlyTheRecordsOtherPhotos(t *testing.T) {
	e := newEnv(t)
	org, _ := e.seeded()
	adminID, _ := e.person(org, "ORG_ADMIN")
	a, b, other := e.upload(org, adminID), e.upload(org, adminID), e.upload(org, adminID)
	e.exec(org, "UPDATE media SET attached_entity_type = 'LETTERHEAD', attached_entity_id = 7 WHERE id = ANY($1)", []int64{a, b})
	e.exec(org, "UPDATE media SET attached_entity_type = 'LETTERHEAD', attached_entity_id = 8 WHERE id = $1", other)

	svc := media.NewService(testkit.Pool(t), nil, e.clock, nil)
	require.NoError(t, database.WithTenant(t.Context(), testkit.Pool(t), org, func(q *db.Queries) error {
		return svc.Detach(t.Context(), q, "LETTERHEAD", 7, []int64{b})
	}))
	attached := func(id int64) (out *int64) {
		e.inTenant(org, func(tx pgx.Tx) {
			require.NoError(t, tx.QueryRow(t.Context(), "SELECT attached_entity_id FROM media WHERE id = $1", id).Scan(&out))
		})
		return out
	}
	require.Nil(t, attached(a), "released")
	require.EqualValues(t, 7, *attached(b), "kept")
	require.EqualValues(t, 8, *attached(other), "another record's photo is untouched")

	require.Error(t, database.WithTenant(t.Context(), testkit.Pool(t), org, func(q *db.Queries) error {
		return svc.Detach(t.Context(), q, "", 7, nil)
	}))
}
