package masterdata

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"siapp/internal/db"
	"siapp/pkg/access"
	"siapp/pkg/platform/apperr"
	"siapp/pkg/platform/audit"
	"siapp/pkg/platform/auth"
	"siapp/pkg/platform/database"
)

var (
	iconKeyPattern    = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,49}$`)
	androidPkgPattern = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_]*(\.[A-Za-z][A-Za-z0-9_]*)+$`)
	iosSchemePattern  = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9+.-]*(://[^\s]*)?$`)
)

// The longest target of each kind, in characters (the contract's limits).
const (
	maxAndroidPackage = 150
	maxIOSURLScheme   = 200
	maxAppURL         = 500
)

// unsafeSchemes are schemes that are never an app to open: a web page has its own field, and the
// rest run code or read files.
var unsafeSchemes = []string{"http", "https", "javascript", "data", "file", "vbscript", "about", "blob", "intent"}

// AppLink is a button on the home screen that opens another app (E-Koperasi Janusa, IKI). SiAPP
// only links to them (DL-16).
type AppLink struct {
	ID             int64
	OrganizationID int64
	Name           string
	// IconKey names an icon the app ships with; the server only checks its shape.
	IconKey        string
	AndroidPackage *string
	IOSURLScheme   *string
	URL            *string
	SortOrder      int
	Active         bool
	CreatedAt      time.Time
	UpdatedAt      time.Time
	deletedAt      *time.Time
}

// AppLinkInput is a request to create an app link. An active link needs at least one target
// (android_package, ios_url_scheme or url).
type AppLinkInput struct {
	Name           string  `json:"name" validate:"required,notblank,max=500"`
	IconKey        string  `json:"icon_key" validate:"required,notblank,max=100"`
	AndroidPackage *string `json:"android_package" validate:"omitempty,max=300"`
	IOSURLScheme   *string `json:"ios_url_scheme" validate:"omitempty,max=300"`
	URL            *string `json:"url" validate:"omitempty,max=1000"`
	SortOrder      *int    `json:"sort_order" validate:"omitempty,min=0,max=10000"`
	Active         *bool   `json:"is_active"`
}

// AppLinkUpdate is a request to change an app link. The three targets accept null to clear them.
type AppLinkUpdate struct {
	Name           *string          `json:"name" validate:"omitempty,notblank,max=500"`
	IconKey        *string          `json:"icon_key" validate:"omitempty,notblank,max=100"`
	AndroidPackage Optional[string] `json:"android_package"`
	IOSURLScheme   Optional[string] `json:"ios_url_scheme"`
	URL            Optional[string] `json:"url"`
	SortOrder      *int             `json:"sort_order" validate:"omitempty,min=0,max=10000"`
	Active         *bool            `json:"is_active"`
}

// targets are the three ways to open an app, as a request gave them.
type targets struct{ android, ios, url *string }

func blankToNil(s *string) *string {
	if s == nil || strings.TrimSpace(*s) == "" {
		return nil
	}
	v := strings.TrimSpace(*s)
	return &v
}

// checkTargets cleans and checks the three targets; every problem is reported at once.
func checkTargets(t targets) (targets, error) {
	var out targets
	var problems []apperr.FieldError
	bad := func(field, msg string) {
		problems = append(problems, apperr.FieldError{Field: field, Code: "INVALID", Message: msg})
	}
	if v := blankToNil(t.android); v != nil {
		if len(*v) > maxAndroidPackage || !androidPkgPattern.MatchString(*v) {
			bad("android_package", "Harus nama paket Android (maksimum 150 karakter), misalnya id.co.janusa.koperasi.")
		}
		out.android = v
	}
	if v := blankToNil(t.ios); v != nil {
		scheme, _, _ := strings.Cut(*v, ":")
		switch {
		case len(*v) > maxIOSURLScheme || !iosSchemePattern.MatchString(*v):
			bad("ios_url_scheme", "Harus URL scheme iOS (maksimum 200 karakter), misalnya koperasi atau koperasi://buka.")
		case slices.Contains(unsafeSchemes, strings.ToLower(scheme)):
			bad("ios_url_scheme", "Scheme ini tidak boleh dipakai. Untuk halaman web, isi url.")
		}
		out.ios = v
	}
	if v := blankToNil(t.url); v != nil {
		u, err := url.Parse(*v)
		if err != nil || len(*v) > maxAppURL || u.Scheme != "https" || u.Host == "" || u.User != nil || strings.ContainsAny(*v, " \t\r\n") {
			bad("url", "Harus alamat https yang lengkap (maksimum 500 karakter), misalnya https://koperasi.janusa.co.id.")
		}
		out.url = v
	}
	if len(problems) > 0 {
		return targets{}, apperr.ErrValidation.WithFields(problems...)
	}
	return out, nil
}

func cleanIconKey(raw string) (string, error) {
	key := strings.TrimSpace(raw)
	if !iconKeyPattern.MatchString(key) {
		return "", validationError("icon_key", "Harus huruf kecil, angka, garis bawah atau tanda hubung, diawali huruf (maksimum 50).")
	}
	return key, nil
}

// ListAppLinks returns one page of the organization's app links.
func (s *Service) ListAppLinks(ctx context.Context, actor auth.Principal, scope access.AdminScope, f ListFilter) ([]AppLink, int, error) {
	if !scope.All {
		return nil, 0, access.ErrOrgAdminOnly
	}
	var out []AppLink
	var total int64
	err := database.WithTenant(ctx, s.pool, actor.OrganizationID, func(q *db.Queries) error {
		rows, err := q.ListAppLinks(ctx, db.ListAppLinksParams{
			Active: boolOrNull(f.Active), Q: textOrNull(f.Q),
			PageLimit: int32(f.Limit), PageOffset: int32(f.Offset), //nolint:gosec // bounded by ParseListQuery
		})
		if err != nil {
			return fmt.Errorf("list app links: %w", err)
		}
		for _, r := range rows {
			out = append(out, toAppLink(r))
		}
		total, err = q.CountAppLinks(ctx, db.CountAppLinksParams{Active: boolOrNull(f.Active), Q: textOrNull(f.Q)})
		if err != nil {
			return fmt.Errorf("count app links: %w", err)
		}
		return nil
	})
	return out, int(total), err
}

// GetAppLink returns one app link.
func (s *Service) GetAppLink(ctx context.Context, actor auth.Principal, scope access.AdminScope, id int64) (AppLink, error) {
	if !scope.All {
		return AppLink{}, access.ErrOrgAdminOnly
	}
	var out AppLink
	err := database.WithTenant(ctx, s.pool, actor.OrganizationID, func(q *db.Queries) error {
		row, err := q.GetAppLink(ctx, id)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrAppLinkNotFound
		}
		if err != nil {
			return fmt.Errorf("get app link: %w", err)
		}
		out = toAppLink(row)
		return nil
	})
	return out, err
}

// CreateAppLink adds an app link. Its name is taken by every link that is not deleted, active or
// not, ignoring case.
func (s *Service) CreateAppLink(ctx context.Context, actor auth.Principal, scope access.AdminScope, in AppLinkInput) (AppLink, error) {
	if !scope.All {
		return AppLink{}, access.ErrOrgAdminOnly
	}
	name, err := cleanName(in.Name)
	if err != nil {
		return AppLink{}, err
	}
	icon, err := cleanIconKey(in.IconKey)
	if err != nil {
		return AppLink{}, err
	}
	t, err := checkTargets(targets{in.AndroidPackage, in.IOSURLScheme, in.URL})
	if err != nil {
		return AppLink{}, err
	}
	active := in.Active == nil || *in.Active
	if active && t.android == nil && t.ios == nil && t.url == nil {
		return AppLink{}, errNoTarget()
	}

	var out AppLink
	err = database.WithTenant(ctx, s.pool, actor.OrganizationID, func(q *db.Queries) error {
		if err := claimAppLinkName(ctx, q, actor.OrganizationID, 0, name); err != nil {
			return err
		}
		row, err := q.InsertAppLink(ctx, db.InsertAppLinkParams{
			Name: name, IconKey: icon, AndroidPackage: textPtr(t.android), IosUrlScheme: textPtr(t.ios), Url: textPtr(t.url),
			SortOrder: intPtr(in.SortOrder), Active: active, Now: ts(s.clock.Now()),
		})
		if err != nil {
			return err
		}
		out = toAppLink(row)
		actorID := actor.PersonnelID
		return audit.Record(ctx, q, audit.Entry{
			ActorPersonnelID: &actorID, Action: "masterdata.app_link.create", EntityType: "app_link", EntityID: out.ID,
			Metadata: map[string]any{"after": appLinkMeta(out)},
		})
	})
	return out, appLinkWriteError(err, "create app link")
}

// UpdateAppLink changes an app link. A change that leaves every field as it was writes nothing:
// no updated_at, no audit row.
func (s *Service) UpdateAppLink(ctx context.Context, actor auth.Principal, scope access.AdminScope, id int64, in AppLinkUpdate) (AppLink, error) {
	if !scope.All {
		return AppLink{}, access.ErrOrgAdminOnly
	}
	if err := resolveAll(in.AndroidPackage.resolve("android_package"), in.IOSURLScheme.resolve("ios_url_scheme"),
		in.URL.resolve("url")); err != nil {
		return AppLink{}, err
	}
	var name *string
	if in.Name != nil {
		cleaned, err := cleanName(*in.Name)
		if err != nil {
			return AppLink{}, err
		}
		name = &cleaned
	}
	var icon *string
	if in.IconKey != nil {
		cleaned, err := cleanIconKey(*in.IconKey)
		if err != nil {
			return AppLink{}, err
		}
		icon = &cleaned
	}
	// Only the targets the request sent are checked here; the result is combined with the stored
	// ones below.
	sent := targets{}
	if in.AndroidPackage.Set {
		sent.android = in.AndroidPackage.Value
	}
	if in.IOSURLScheme.Set {
		sent.ios = in.IOSURLScheme.Value
	}
	if in.URL.Set {
		sent.url = in.URL.Value
	}
	cleanSent, err := checkTargets(sent)
	if err != nil {
		return AppLink{}, err
	}

	var out AppLink
	err = database.WithTenant(ctx, s.pool, actor.OrganizationID, func(q *db.Queries) error {
		before, err := q.GetAppLinkForUpdate(ctx, id)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrAppLinkNotFound
		}
		if err != nil {
			return fmt.Errorf("get app link: %w", err)
		}
		was := toAppLink(before)
		next := was
		if name != nil {
			next.Name = *name
		}
		if icon != nil {
			next.IconKey = *icon
		}
		if in.AndroidPackage.Set {
			next.AndroidPackage = cleanSent.android
		}
		if in.IOSURLScheme.Set {
			next.IOSURLScheme = cleanSent.ios
		}
		if in.URL.Set {
			next.URL = cleanSent.url
		}
		if in.SortOrder != nil {
			next.SortOrder = *in.SortOrder
		}
		if in.Active != nil {
			next.Active = *in.Active
		}
		if sameAppLink(was, next) {
			out = was
			return nil
		}
		if next.Active && next.AndroidPackage == nil && next.IOSURLScheme == nil && next.URL == nil {
			return errNoTarget()
		}
		if next.Name != was.Name {
			if err := claimAppLinkName(ctx, q, actor.OrganizationID, id, next.Name); err != nil {
				return err
			}
		}
		row, err := q.UpdateAppLink(ctx, db.UpdateAppLinkParams{
			ID: id, Name: next.Name, IconKey: next.IconKey, AndroidPackage: textPtr(next.AndroidPackage),
			IosUrlScheme: textPtr(next.IOSURLScheme), Url: textPtr(next.URL),
			SortOrder: int32(next.SortOrder), Active: next.Active, Now: ts(s.clock.Now()), //nolint:gosec // validated 0..10000
		})
		if err != nil {
			return err
		}
		out = toAppLink(row)
		actorID := actor.PersonnelID
		return audit.Record(ctx, q, audit.Entry{
			ActorPersonnelID: &actorID, Action: "masterdata.app_link.update", EntityType: "app_link", EntityID: id,
			Metadata: map[string]any{"before": appLinkMeta(was), "after": appLinkMeta(out)},
		})
	})
	return out, appLinkWriteError(err, "update app link")
}

// DeleteAppLink deletes an app link. The row stays, deleted and inactive, so a delta pull can send
// it to the apps as a tombstone.
func (s *Service) DeleteAppLink(ctx context.Context, actor auth.Principal, scope access.AdminScope, id int64) error {
	if !scope.All {
		return access.ErrOrgAdminOnly
	}
	return database.WithTenant(ctx, s.pool, actor.OrganizationID, func(q *db.Queries) error {
		before, err := q.GetAppLinkForUpdate(ctx, id)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrAppLinkNotFound
		}
		if err != nil {
			return fmt.Errorf("get app link: %w", err)
		}
		if _, err := q.SoftDeleteAppLink(ctx, db.SoftDeleteAppLinkParams{ID: id, Now: ts(s.clock.Now())}); err != nil {
			return fmt.Errorf("delete app link: %w", err)
		}
		actorID := actor.PersonnelID
		return audit.Record(ctx, q, audit.Entry{
			ActorPersonnelID: &actorID, Action: "masterdata.app_link.delete", EntityType: "app_link", EntityID: id,
			Metadata: map[string]any{"before": appLinkMeta(toAppLink(before))},
		})
	})
}

// FieldAppLinks is the full pull of the field app: the active links, and the server time the app
// starts its delta pulls from, taken before the read.
func (s *Service) FieldAppLinks(ctx context.Context, p auth.Principal) ([]AppLink, time.Time, error) {
	serverTime := s.clock.Now()
	var out []AppLink
	err := database.WithTenant(ctx, s.pool, p.OrganizationID, func(q *db.Queries) error {
		rows, err := q.ListActiveAppLinks(ctx)
		if err != nil {
			return fmt.Errorf("list active app links: %w", err)
		}
		for _, r := range rows {
			out = append(out, toAppLink(r))
		}
		return nil
	})
	return out, serverTime, err
}

// FieldAppLinkChanges is the delta pull. ⚠ DL-57: a deleted or deactivated link is sent in
// Deleted, so the offline cache can drop it (the full pull returns active rows only).
func (s *Service) FieldAppLinkChanges(ctx context.Context, p auth.Principal, since time.Time) (Changes[AppLink], error) {
	out := Changes[AppLink]{ServerTime: s.clock.Now()}
	err := database.WithTenant(ctx, s.pool, p.OrganizationID, func(q *db.Queries) error {
		rows, err := q.ListAppLinkChanges(ctx, ts(sinceBound(since)))
		if err != nil {
			return fmt.Errorf("list app link changes: %w", err)
		}
		for _, r := range rows {
			l := toAppLink(r)
			if !r.Active || r.DeletedAt.Valid {
				gone := l.UpdatedAt
				if l.deletedAt != nil {
					gone = *l.deletedAt
				}
				out.Deleted = append(out.Deleted, Tombstone{ID: l.ID, DeletedAt: gone})
				continue
			}
			out.Upserted = append(out.Upserted, l)
		}
		return nil
	})
	return out, err
}

func claimAppLinkName(ctx context.Context, q *db.Queries, orgID, id int64, name string) error {
	if err := q.LockMasterName(ctx, fmt.Sprintf("masterdata:app_link:%d:%s", orgID, strings.ToLower(name))); err != nil {
		return fmt.Errorf("lock app link name: %w", err)
	}
	clash, err := q.AppLinkNameClash(ctx, db.AppLinkNameClashParams{ID: id, Name: name})
	if err != nil {
		return fmt.Errorf("check app link name: %w", err)
	}
	if clash {
		return ErrAppLinkNameTaken
	}
	return nil
}

func appLinkWriteError(err error, what string) error {
	switch {
	case err == nil:
		return nil
	case database.IsUniqueViolation(err):
		return ErrAppLinkNameTaken
	case isAppErr(err):
		return err
	}
	return fmt.Errorf("%s: %w", what, err)
}

func errNoTarget() error {
	return validationError("url", "Tautan yang aktif butuh minimal satu tujuan: android_package, ios_url_scheme atau url.")
}

func sameAppLink(a, b AppLink) bool {
	return a.Name == b.Name && a.IconKey == b.IconKey && samePhone(a.AndroidPackage, b.AndroidPackage) &&
		samePhone(a.IOSURLScheme, b.IOSURLScheme) && samePhone(a.URL, b.URL) && a.SortOrder == b.SortOrder && a.Active == b.Active
}

func toAppLink(r db.AppLink) AppLink {
	l := AppLink{
		ID: r.ID, OrganizationID: r.OrganizationID, Name: r.Name, IconKey: r.IconKey,
		AndroidPackage: textValue(r.AndroidPackage), IOSURLScheme: textValue(r.IosUrlScheme), URL: textValue(r.Url),
		SortOrder: int(r.SortOrder), Active: r.Active, CreatedAt: r.CreatedAt.Time, UpdatedAt: r.UpdatedAt.Time,
	}
	if r.DeletedAt.Valid {
		d := r.DeletedAt.Time
		l.deletedAt = &d
	}
	return l
}

func appLinkMeta(l AppLink) map[string]any {
	return map[string]any{
		"name": l.Name, "icon_key": l.IconKey, "has_android": l.AndroidPackage != nil, "has_ios": l.IOSURLScheme != nil,
		"has_url": l.URL != nil, "sort_order": l.SortOrder, "active": l.Active,
	}
}
