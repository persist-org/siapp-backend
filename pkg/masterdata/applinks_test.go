package masterdata_test

import (
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"siapp/pkg/masterdata"
	"siapp/pkg/platform/testkit"
)

type appLink struct {
	ID             int64   `json:"id"`
	Name           string  `json:"name"`
	IconKey        string  `json:"icon_key"`
	AndroidPackage *string `json:"android_package"`
	IOSURLScheme   *string `json:"ios_url_scheme"`
	URL            *string `json:"url"`
	SortOrder      int     `json:"sort_order"`
	IsActive       bool    `json:"is_active"`
}

func (e *env) appLinks(token string) []appLink {
	e.t.Helper()
	r := e.api(token, http.MethodGet, "/admin/app-links", nil)
	require.Equal(e.t, http.StatusOK, r.Status, "body: %s", r.Raw)
	return data[[]appLink](e.t, r)
}

func appLinkNames(ls []appLink) (out []string) {
	for _, l := range ls {
		out = append(out, l.Name)
	}
	return out
}

func TestTheAppLinkPlaceholdersAreSeededInactiveAndOnlyOnce(t *testing.T) {
	e := newEnv(t)
	org, admin := e.seeded()

	links := e.appLinks(admin)
	require.Equal(t, []string{"E-Koperasi Janusa", "IKI"}, appLinkNames(links))
	for _, l := range links {
		require.False(t, l.IsActive, "inactive until the real links are provided (DL-16)")
		require.Nil(t, l.URL)
		require.Nil(t, l.AndroidPackage)
	}
	// The field app sees none of them.
	_, field := e.person(org, "")
	full := deltaOf[appLink](t, e.api(field, http.MethodGet, "/app-links", nil))
	require.Empty(t, full.Data)

	// An admin who deleted them is not given them back.
	for _, l := range links {
		require.Equal(t, http.StatusOK, e.api(admin, http.MethodDelete, "/admin/app-links/"+itoa(l.ID), nil).Status)
	}
	added, err := masterdata.EnsureDefaults(t.Context(), testkit.Pool(t), e.clock, org)
	require.NoError(t, err)
	require.Zero(t, added)
	require.Empty(t, e.appLinks(admin))

	// A placeholder can be filled in and switched on.
	_, admin2 := e.seeded()
	ph := e.appLinks(admin2)[0]
	on := e.api(admin2, http.MethodPatch, "/admin/app-links/"+itoa(ph.ID), map[string]any{
		"android_package": "id.co.janusa.koperasi", "url": "https://koperasi.janusa.co.id/", "is_active": true,
	})
	require.Equal(t, http.StatusOK, on.Status, "body: %s", on.Raw)
	require.True(t, data[appLink](t, on).IsActive)
}

func TestAnAppLinkNeedsATargetAndAValidOne(t *testing.T) {
	e := newEnv(t)
	_, admin := e.seeded()
	post := func(body map[string]any) reply {
		base := map[string]any{"name": "Aplikasi Uji", "icon_key": "uji"}
		for k, v := range body {
			base[k] = v
		}
		return e.raw(admin, http.MethodPost, "/admin/app-links", base)
	}
	invalid := func(r reply, field string) {
		t.Helper()
		require.Equal(t, http.StatusUnprocessableEntity, r.Status, "body: %s", r.Raw)
		require.Contains(t, string(r.Raw), `"`+field+`"`)
	}

	// An active link needs at least one target; an inactive one may wait for it.
	invalid(post(map[string]any{}), "url")
	invalid(post(map[string]any{"url": "   ", "android_package": ""}), "url")
	inactive := post(map[string]any{"is_active": false})
	require.Equal(t, http.StatusCreated, inactive.Status, "body: %s", inactive.Raw)

	for _, bad := range []string{"http://koperasi.janusa.co.id", "koperasi.janusa.co.id", "https://", "https://user:pw@janusa.co.id", "javascript:alert(1)", "https://a b.com", "ftp://x.com"} {
		invalid(post(map[string]any{"name": "U " + bad, "url": bad}), "url")
	}
	for _, bad := range []string{"koperasi", "id", "id..co", "id.co.1abc", "id.co.janusa koperasi", "../x"} {
		invalid(post(map[string]any{"name": "A " + bad, "android_package": bad}), "android_package")
	}
	for _, bad := range []string{"http", "https://x", "javascript", "data:text/html", "file://x", "1abc", "my app", "intent://x"} {
		invalid(post(map[string]any{"name": "I " + bad, "ios_url_scheme": bad}), "ios_url_scheme")
	}
	for _, bad := range []string{"", "Koperasi", "1abc", "has space", "a/b"} {
		invalid(post(map[string]any{"name": "K " + bad, "icon_key": bad}), "icon_key")
	}
	// Every problem at once.
	both := post(map[string]any{"url": "http://x", "android_package": "bad"})
	require.Contains(t, string(both.Raw), `"url"`)
	require.Contains(t, string(both.Raw), `"android_package"`)

	ok := post(map[string]any{"name": "Lengkap", "icon_key": "lengkap", "android_package": " id.co.janusa.koperasi ", "ios_url_scheme": "koperasi://buka?x=1", "url": " https://koperasi.janusa.co.id/masuk "})
	require.Equal(t, http.StatusCreated, ok.Status, "body: %s", ok.Raw)
	l := data[appLink](t, ok)
	require.Equal(t, "id.co.janusa.koperasi", *l.AndroidPackage)
	require.Equal(t, "https://koperasi.janusa.co.id/masuk", *l.URL)
	require.True(t, l.IsActive)

	// A patch cannot take the last target from an active link, but can clear one of several.
	e.raw(admin, http.MethodPatch, "/admin/app-links/"+itoa(l.ID), map[string]any{"android_package": nil, "ios_url_scheme": nil, "url": nil}).
		requireFailure(t, http.StatusUnprocessableEntity, "VALIDATION_ERROR", "")
	cleared := e.raw(admin, http.MethodPatch, "/admin/app-links/"+itoa(l.ID), map[string]any{"android_package": nil, "ios_url_scheme": ""})
	require.Equal(t, http.StatusOK, cleared.Status, "body: %s", cleared.Raw)
	got := data[appLink](t, cleared)
	require.Nil(t, got.AndroidPackage)
	require.Nil(t, got.IOSURLScheme)
	require.NotNil(t, got.URL)
	// Deactivating lets all targets go.
	require.Equal(t, http.StatusOK, e.raw(admin, http.MethodPatch, "/admin/app-links/"+itoa(l.ID), map[string]any{"is_active": false, "url": nil}).Status)
	// ...but switching it back on does not.
	e.raw(admin, http.MethodPatch, "/admin/app-links/"+itoa(l.ID), map[string]any{"is_active": true}).
		requireFailure(t, http.StatusUnprocessableEntity, "VALIDATION_ERROR", "")
}

func TestAnAppLinkNameIsUniqueIncludingInactiveOnes(t *testing.T) {
	e := newEnv(t)
	org, admin := e.seeded()
	body := func(name string) map[string]any {
		return map[string]any{"name": name, "icon_key": "x", "url": "https://janusa.co.id"}
	}
	a := e.api(admin, http.MethodPost, "/admin/app-links", body("Portal"))
	require.Equal(t, http.StatusCreated, a.Status, "body: %s", a.Raw)
	id := data[appLink](t, a).ID

	e.raw(admin, http.MethodPost, "/admin/app-links", body(" portal ")).
		requireFailure(t, http.StatusConflict, "DUPLICATE", masterdata.ReasonAppLinkNameTaken)
	e.raw(admin, http.MethodPost, "/admin/app-links", body("iki")).
		requireFailure(t, http.StatusConflict, "DUPLICATE", masterdata.ReasonAppLinkNameTaken)
	require.Equal(t, http.StatusOK, e.api(admin, http.MethodPatch, "/admin/app-links/"+itoa(id), map[string]any{"is_active": false}).Status)
	e.raw(admin, http.MethodPost, "/admin/app-links", body("Portal")).
		requireFailure(t, http.StatusConflict, "DUPLICATE", masterdata.ReasonAppLinkNameTaken)

	// Renaming onto another's name clashes; deleting frees it.
	b := data[appLink](t, e.api(admin, http.MethodPost, "/admin/app-links", body("Portal Dua")))
	e.raw(admin, http.MethodPatch, "/admin/app-links/"+itoa(b.ID), map[string]any{"name": "PORTAL"}).
		requireFailure(t, http.StatusConflict, "DUPLICATE", masterdata.ReasonAppLinkNameTaken)
	require.Equal(t, http.StatusOK, e.api(admin, http.MethodDelete, "/admin/app-links/"+itoa(id), nil).Status)
	require.Equal(t, http.StatusOK, e.api(admin, http.MethodPatch, "/admin/app-links/"+itoa(b.ID), map[string]any{"name": "PORTAL"}).Status)

	// A change that changes nothing writes nothing.
	audits := len(e.auditActions(org))
	e.clock.Advance(time.Minute)
	require.Equal(t, http.StatusOK, e.api(admin, http.MethodPatch, "/admin/app-links/"+itoa(b.ID), map[string]any{"name": " PORTAL ", "url": "https://janusa.co.id", "is_active": true}).Status)
	require.Len(t, e.auditActions(org), audits)

	for _, id := range []string{"0", "x", "999999"} {
		e.raw(admin, http.MethodGet, "/admin/app-links/"+id, nil).
			requireFailure(t, http.StatusNotFound, "NOT_FOUND", masterdata.ReasonAppLinkNotFound)
	}
}

func TestTheFieldAppLinksSyncLikeTheOtherMasters(t *testing.T) {
	e := newEnv(t)
	org, admin := e.seeded()
	_, field := e.person(org, "")
	mk := func(name string) appLink {
		r := e.api(admin, http.MethodPost, "/admin/app-links", map[string]any{"name": name, "icon_key": "x", "url": "https://janusa.co.id"})
		require.Equal(t, http.StatusCreated, r.Status, "body: %s", r.Raw)
		return data[appLink](t, r)
	}
	one, two := mk("Satu"), mk("Dua")

	full := deltaOf[appLink](t, e.api(field, http.MethodGet, "/app-links", nil))
	require.Equal(t, []string{"Satu", "Dua"}, appLinkNames(full.Data))
	require.Empty(t, full.Deleted)

	e.clock.Advance(time.Minute)
	require.Equal(t, http.StatusOK, e.api(admin, http.MethodPatch, "/admin/app-links/"+itoa(one.ID), map[string]any{"is_active": false}).Status)
	require.Equal(t, http.StatusOK, e.api(admin, http.MethodDelete, "/admin/app-links/"+itoa(two.ID), nil).Status)
	three := mk("Tiga")
	e.clock.Advance(time.Minute)

	d := deltaOf[appLink](t, e.api(field, http.MethodGet, "/app-links"+since(full.Meta.ServerTime), nil))
	require.ElementsMatch(t, []int64{one.ID, two.ID}, deletedIDs(d), "deactivated and deleted rows are tombstones")
	require.Equal(t, []string{"Tiga"}, appLinkNames(d.Data))
	require.Equal(t, three.ID, d.Data[0].ID)
	require.Equal(t, []string{"Tiga"}, appLinkNames(deltaOf[appLink](t, e.api(field, http.MethodGet, "/app-links", nil)).Data))

	e.raw("", http.MethodGet, "/app-links", nil)
	require.Equal(t, http.StatusUnauthorized, e.raw("", http.MethodGet, "/app-links", nil).Status)
	e.raw(field, http.MethodGet, "/app-links?since=nope", nil).
		requireFailure(t, http.StatusUnprocessableEntity, "VALIDATION_ERROR", "")
}

func TestAnAppLinkTargetIsBoundedLikeTheContractSays(t *testing.T) {
	e := newEnv(t)
	_, admin := e.seeded()
	long := func(prefix string, n int, suffix string) string {
		b := []byte(prefix)
		for len(b)+len(suffix) < n {
			b = append(b, 'a')
		}
		return string(b) + suffix
	}
	cases := map[string]string{
		"android_package": long("id.co.", 151, ""),
		"ios_url_scheme":  long("koperasi://", 201, ""),
		"url":             long("https://janusa.co.id/", 501, ""),
	}
	for field, value := range cases {
		r := e.raw(admin, http.MethodPost, "/admin/app-links", map[string]any{"name": "Panjang " + field, "icon_key": "x", field: value})
		require.Equal(t, http.StatusUnprocessableEntity, r.Status, "%s: %s", field, r.Raw)
		require.Contains(t, string(r.Raw), `"`+field+`"`)
	}
	// One character less is fine.
	ok := e.raw(admin, http.MethodPost, "/admin/app-links", map[string]any{"name": "Pas", "icon_key": "x", "url": long("https://janusa.co.id/", 500, "")})
	require.Equal(t, http.StatusCreated, ok.Status, "body: %s", ok.Raw)

	// A wrong JSON type is a 422 about the field, also for a field that can be null.
	r := e.raw(admin, http.MethodPatch, "/admin/app-links/"+itoa(data[appLink](t, ok).ID), map[string]any{"url": 42})
	require.Equal(t, http.StatusUnprocessableEntity, r.Status, "body: %s", r.Raw)
	require.Contains(t, string(r.Raw), `"url"`)
}
