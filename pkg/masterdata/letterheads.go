package masterdata

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"

	"siapp/internal/db"
	"siapp/pkg/access"
	"siapp/pkg/media"
	"siapp/pkg/platform/audit"
	"siapp/pkg/platform/auth"
	"siapp/pkg/platform/database"
)

// letterheadEntity is what a logo is attached to in the media module.
const letterheadEntity = "LETTERHEAD"

// maxLetterheadText is the longest header or footer, in characters: a few lines of a kop surat.
const maxLetterheadText = 2000

// Letterhead is a kop surat: the header, footer and logo an export puts on its pages (module 14).
type Letterhead struct {
	ID             int64
	OrganizationID int64
	Name           string
	HeaderText     *string
	FooterText     *string
	// LogoMediaID is the photo (purpose LETTERHEAD_LOGO) attached to this letterhead; LogoURL is a
	// signed URL to show it, valid for an hour and never stored.
	LogoMediaID *int64
	LogoURL     *string
	IsDefault   bool
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// LetterheadInput is a request to create a letterhead. The first letterhead of an organization is
// its default unless is_default says false.
type LetterheadInput struct {
	Name        string  `json:"name" validate:"required,notblank,max=500"`
	HeaderText  *string `json:"header_text"`
	FooterText  *string `json:"footer_text"`
	LogoMediaID *int64  `json:"logo_media_id" validate:"omitempty,min=1"`
	IsDefault   *bool   `json:"is_default"`
}

// LetterheadUpdate is a request to change a letterhead. header_text, footer_text and logo_media_id
// accept null to clear them. Making one the default makes the previous default stop being it.
type LetterheadUpdate struct {
	Name        *string          `json:"name" validate:"omitempty,notblank,max=500"`
	HeaderText  Optional[string] `json:"header_text"`
	FooterText  Optional[string] `json:"footer_text"`
	LogoMediaID Optional[int64]  `json:"logo_media_id"`
	IsDefault   *bool            `json:"is_default"`
}

// letterheadText cleans a header or footer: nil and blank are "none", line breaks are kept (a kop
// is several lines), other control characters are refused.
func letterheadText(raw *string, field string) (*string, error) {
	if raw == nil {
		return nil, nil //nolint:nilnil // nil is "no text", not a missing value
	}
	lines := strings.Split(strings.ReplaceAll(*raw, "\r\n", "\n"), "\n")
	for i, l := range lines {
		lines[i] = strings.TrimRight(l, " \t\r")
	}
	text := strings.Trim(strings.Join(lines, "\n"), "\n ")
	if text == "" {
		return nil, nil //nolint:nilnil // blank is "no text"
	}
	if utf8.RuneCountInString(text) > maxLetterheadText {
		return nil, validationError(field, "Maksimum 2000 karakter.")
	}
	if strings.IndexFunc(text, func(r rune) bool { return unicode.IsControl(r) && r != '\n' && r != '\t' }) >= 0 {
		return nil, validationError(field, "Tidak boleh berisi karakter kontrol.")
	}
	return &text, nil
}

// ListLetterheads returns one page of the organization's letterheads, the default first.
func (s *Service) ListLetterheads(ctx context.Context, actor auth.Principal, scope access.AdminScope, f ListFilter) ([]Letterhead, int, error) {
	if !scope.All {
		return nil, 0, access.ErrOrgAdminOnly
	}
	var out []Letterhead
	var total int64
	err := database.WithTenant(ctx, s.pool, actor.OrganizationID, func(q *db.Queries) error {
		rows, err := q.ListLetterheads(ctx, db.ListLetterheadsParams{
			Q: textOrNull(f.Q), PageLimit: int32(f.Limit), PageOffset: int32(f.Offset), //nolint:gosec // bounded by ParseListQuery
		})
		if err != nil {
			return fmt.Errorf("list letterheads: %w", err)
		}
		for _, r := range rows {
			l, err := s.letterheadWithLogo(ctx, q, r)
			if err != nil {
				return err
			}
			out = append(out, l)
		}
		total, err = q.CountLetterheads(ctx, textOrNull(f.Q))
		if err != nil {
			return fmt.Errorf("count letterheads: %w", err)
		}
		return nil
	})
	return out, int(total), err
}

// GetLetterhead returns one letterhead.
func (s *Service) GetLetterhead(ctx context.Context, actor auth.Principal, scope access.AdminScope, id int64) (Letterhead, error) {
	if !scope.All {
		return Letterhead{}, access.ErrOrgAdminOnly
	}
	var out Letterhead
	err := database.WithTenant(ctx, s.pool, actor.OrganizationID, func(q *db.Queries) error {
		row, err := q.GetLetterhead(ctx, id)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrLetterheadNotFound
		}
		if err != nil {
			return fmt.Errorf("get letterhead: %w", err)
		}
		out, err = s.letterheadWithLogo(ctx, q, row)
		return err
	})
	return out, err
}

// CreateLetterhead adds a letterhead. Its name is unique ignoring case. A logo is a photo of
// purpose LETTERHEAD_LOGO that the caller registered and uploaded; it is attached to the new
// letterhead in the same transaction.
func (s *Service) CreateLetterhead(ctx context.Context, actor auth.Principal, scope access.AdminScope, in LetterheadInput) (Letterhead, error) {
	if !scope.All {
		return Letterhead{}, access.ErrOrgAdminOnly
	}
	name, err := cleanName(in.Name)
	if err != nil {
		return Letterhead{}, err
	}
	header, err := letterheadText(in.HeaderText, "header_text")
	if err != nil {
		return Letterhead{}, err
	}
	footer, err := letterheadText(in.FooterText, "footer_text")
	if err != nil {
		return Letterhead{}, err
	}
	if in.LogoMediaID != nil && s.logos == nil {
		return Letterhead{}, errors.New("masterdata: a letterhead logo needs the media module (UseLogoStore)")
	}

	var out Letterhead
	err = database.WithTenant(ctx, s.pool, actor.OrganizationID, func(q *db.Queries) error {
		if err := claimLetterheadName(ctx, q, actor.OrganizationID, 0, name); err != nil {
			return err
		}
		// ⚠ One default per organization. The lock serialises two admins making one at once, and
		// the first letterhead becomes the default by itself: an export needs one.
		if err := q.LockMasterName(ctx, letterheadDefaultKey(actor.OrganizationID)); err != nil {
			return fmt.Errorf("lock letterhead default: %w", err)
		}
		isDefault := in.IsDefault != nil && *in.IsDefault
		if in.IsDefault == nil {
			existing, err := q.CountLetterheads(ctx, textOrNull(""))
			if err != nil {
				return fmt.Errorf("count letterheads: %w", err)
			}
			isDefault = existing == 0
		}
		now := s.clock.Now()
		if isDefault {
			if err := q.ClearOtherDefaultLetterheads(ctx, db.ClearOtherDefaultLetterheadsParams{ID: 0, Now: ts(now)}); err != nil {
				return fmt.Errorf("clear default letterhead: %w", err)
			}
		}
		row, err := q.InsertLetterhead(ctx, db.InsertLetterheadParams{
			Name: name, HeaderText: textPtr(header), FooterText: textPtr(footer), IsDefault: isDefault, Now: ts(now),
		})
		if err != nil {
			return err
		}
		if in.LogoMediaID != nil {
			if err := s.attachLogo(ctx, q, actor, row.ID, *in.LogoMediaID); err != nil {
				return err
			}
			if row, err = q.SetLetterheadLogo(ctx, db.SetLetterheadLogoParams{ID: row.ID, LogoMediaID: int8Ptr(in.LogoMediaID)}); err != nil {
				return fmt.Errorf("set letterhead logo: %w", err)
			}
		}
		if out, err = s.letterheadWithLogo(ctx, q, row); err != nil {
			return err
		}
		actorID := actor.PersonnelID
		return audit.Record(ctx, q, audit.Entry{
			ActorPersonnelID: &actorID, Action: "masterdata.letterhead.create", EntityType: "letterhead", EntityID: out.ID,
			Metadata: map[string]any{"after": letterheadMeta(out)},
		})
	})
	return out, letterheadWriteError(err, "create letterhead")
}

// UpdateLetterhead changes a letterhead. A change that leaves every field as it was writes
// nothing: no updated_at, no audit row. Sending the logo it already has is not a change (a photo
// can be attached only once).
func (s *Service) UpdateLetterhead(ctx context.Context, actor auth.Principal, scope access.AdminScope, id int64, in LetterheadUpdate) (Letterhead, error) {
	if !scope.All {
		return Letterhead{}, access.ErrOrgAdminOnly
	}
	if err := resolveAll(in.HeaderText.resolve("header_text"), in.FooterText.resolve("footer_text"),
		in.LogoMediaID.resolve("logo_media_id")); err != nil {
		return Letterhead{}, err
	}
	var name *string
	if in.Name != nil {
		cleaned, err := cleanName(*in.Name)
		if err != nil {
			return Letterhead{}, err
		}
		name = &cleaned
	}
	var header, footer *string
	if in.HeaderText.Set {
		var err error
		if header, err = letterheadText(in.HeaderText.Value, "header_text"); err != nil {
			return Letterhead{}, err
		}
	}
	if in.FooterText.Set {
		var err error
		if footer, err = letterheadText(in.FooterText.Value, "footer_text"); err != nil {
			return Letterhead{}, err
		}
	}
	if in.LogoMediaID.Set && in.LogoMediaID.Value != nil {
		if *in.LogoMediaID.Value < 1 {
			return Letterhead{}, validationError("logo_media_id", "Harus id foto yang valid.")
		}
		if s.logos == nil {
			return Letterhead{}, errors.New("masterdata: a letterhead logo needs the media module (UseLogoStore)")
		}
	}

	var out Letterhead
	err := database.WithTenant(ctx, s.pool, actor.OrganizationID, func(q *db.Queries) error {
		// ⚠ Lock order: the advisory locks (the name, then the default) before the row. A create
		// takes them in that order and then clears the old default's row; if an update held that
		// row while it waited for the name, the two would deadlock.
		if name != nil {
			if err := q.LockMasterName(ctx, letterheadNameKey(actor.OrganizationID, *name)); err != nil {
				return fmt.Errorf("lock letterhead name: %w", err)
			}
		}
		if in.IsDefault != nil && *in.IsDefault {
			if err := q.LockMasterName(ctx, letterheadDefaultKey(actor.OrganizationID)); err != nil {
				return fmt.Errorf("lock letterhead default: %w", err)
			}
		}
		before, err := q.GetLetterheadForUpdate(ctx, id)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrLetterheadNotFound
		}
		if err != nil {
			return fmt.Errorf("get letterhead: %w", err)
		}
		was := toLetterhead(before)
		next := was
		if name != nil {
			next.Name = *name
		}
		if in.HeaderText.Set {
			next.HeaderText = header
		}
		if in.FooterText.Set {
			next.FooterText = footer
		}
		newLogo := false
		if in.LogoMediaID.Set {
			newLogo = in.LogoMediaID.Value != nil && !samePhone64(in.LogoMediaID.Value, was.LogoMediaID)
			next.LogoMediaID = in.LogoMediaID.Value
		}
		if in.IsDefault != nil {
			next.IsDefault = *in.IsDefault
		}
		if next.Name == was.Name && samePhone(next.HeaderText, was.HeaderText) && samePhone(next.FooterText, was.FooterText) &&
			samePhone64(next.LogoMediaID, was.LogoMediaID) && next.IsDefault == was.IsDefault {
			out, err = s.letterheadWithLogo(ctx, q, before)
			return err
		}

		if next.Name != was.Name {
			if err := claimLetterheadName(ctx, q, actor.OrganizationID, id, next.Name); err != nil {
				return err
			}
		}
		now := s.clock.Now()
		if next.IsDefault && !was.IsDefault {
			if err := q.ClearOtherDefaultLetterheads(ctx, db.ClearOtherDefaultLetterheadsParams{ID: id, Now: ts(now)}); err != nil {
				return fmt.Errorf("clear default letterhead: %w", err)
			}
		}
		if newLogo {
			if err := s.attachLogo(ctx, q, actor, id, *next.LogoMediaID); err != nil {
				return err
			}
		}
		// The logo it no longer shows is let go, so the cleanup job removes it, and the same photo
		// can be chosen again later.
		if was.LogoMediaID != nil && !samePhone64(next.LogoMediaID, was.LogoMediaID) && s.logos != nil {
			var keep []int64
			if next.LogoMediaID != nil {
				keep = []int64{*next.LogoMediaID}
			}
			if err := s.logos.Detach(ctx, q, letterheadEntity, id, keep); err != nil {
				return err
			}
		}
		row, err := q.UpdateLetterhead(ctx, db.UpdateLetterheadParams{
			ID: id, Name: next.Name, HeaderText: textPtr(next.HeaderText), FooterText: textPtr(next.FooterText),
			LogoMediaID: int8Ptr(next.LogoMediaID), IsDefault: next.IsDefault, Now: ts(now),
		})
		if err != nil {
			return err
		}
		if out, err = s.letterheadWithLogo(ctx, q, row); err != nil {
			return err
		}
		actorID := actor.PersonnelID
		return audit.Record(ctx, q, audit.Entry{
			ActorPersonnelID: &actorID, Action: "masterdata.letterhead.update", EntityType: "letterhead", EntityID: id,
			Metadata: map[string]any{"before": letterheadMeta(was), "after": letterheadMeta(out)},
		})
	})
	return out, letterheadWriteError(err, "update letterhead")
}

// DeleteLetterhead deletes a letterhead that nothing points at; one an export points at is
// MASTER_IN_USE. Deleting the default leaves the organization without one until an admin sets a
// new default.
func (s *Service) DeleteLetterhead(ctx context.Context, actor auth.Principal, scope access.AdminScope, id int64) error {
	if !scope.All {
		return access.ErrOrgAdminOnly
	}
	return database.WithTenant(ctx, s.pool, actor.OrganizationID, func(q *db.Queries) error {
		before, err := q.GetLetterheadForUpdate(ctx, id)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrLetterheadNotFound
		}
		if err != nil {
			return fmt.Errorf("get letterhead: %w", err)
		}
		for _, inUse := range s.letterheadUsage {
			used, err := inUse(ctx, q, id)
			if err != nil {
				return fmt.Errorf("check letterhead usage: %w", err)
			}
			if used {
				return ErrMasterInUse
			}
		}
		if s.logos != nil {
			if err := s.logos.Detach(ctx, q, letterheadEntity, id, nil); err != nil {
				return err
			}
		}
		if _, err := q.DeleteLetterhead(ctx, id); err != nil {
			return fmt.Errorf("delete letterhead: %w", err)
		}
		actorID := actor.PersonnelID
		return audit.Record(ctx, q, audit.Entry{
			ActorPersonnelID: &actorID, Action: "masterdata.letterhead.delete", EntityType: "letterhead", EntityID: id,
			Metadata: map[string]any{"before": letterheadMeta(toLetterhead(before))},
		})
	})
}

// attachLogo claims the photo for the letterhead: it must be the caller's own, uploaded, and
// registered for this purpose, and not attached to anything else (media.Attach says which of those
// is wrong).
func (s *Service) attachLogo(ctx context.Context, q *db.Queries, actor auth.Principal, letterheadID, mediaID int64) error {
	return s.logos.Attach(ctx, q, media.AttachInput{
		MediaIDs: []int64{mediaID}, OwnerID: actor.PersonnelID, Purpose: media.PurposeLetterheadLogo,
		EntityType: letterheadEntity, EntityID: letterheadID, Max: 1,
	})
}

// letterheadWithLogo is the letterhead with the signed URL of its logo.
func (s *Service) letterheadWithLogo(ctx context.Context, q *db.Queries, row db.Letterhead) (Letterhead, error) {
	l := toLetterhead(row)
	if l.LogoMediaID == nil || s.logos == nil {
		return l, nil
	}
	views, err := s.logos.Views(ctx, q, letterheadEntity, l.ID, []int64{*l.LogoMediaID})
	if err != nil {
		return Letterhead{}, fmt.Errorf("letterhead logo: %w", err)
	}
	if len(views) > 0 {
		url := views[0].URL
		l.LogoURL = &url
	}
	return l, nil
}

func letterheadNameKey(orgID int64, name string) string {
	return fmt.Sprintf("masterdata:letterhead:%d:%s", orgID, strings.ToLower(name))
}

func letterheadDefaultKey(orgID int64) string {
	return fmt.Sprintf("masterdata:letterhead-default:%d", orgID)
}

func claimLetterheadName(ctx context.Context, q *db.Queries, orgID, id int64, name string) error {
	if err := q.LockMasterName(ctx, letterheadNameKey(orgID, name)); err != nil {
		return fmt.Errorf("lock letterhead name: %w", err)
	}
	clash, err := q.LetterheadNameClash(ctx, db.LetterheadNameClashParams{ID: id, Name: name})
	if err != nil {
		return fmt.Errorf("check letterhead name: %w", err)
	}
	if clash {
		return ErrLetterheadNameTaken
	}
	return nil
}

func letterheadWriteError(err error, what string) error {
	switch {
	case err == nil:
		return nil
	case database.IsUniqueViolation(err) && uniqueConstraint(err) == "letterheads_name_uq":
		return ErrLetterheadNameTaken
	case isAppErr(err):
		return err
	}
	return fmt.Errorf("%s: %w", what, err)
}

func samePhone64(a, b *int64) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

func toLetterhead(r db.Letterhead) Letterhead {
	return Letterhead{
		ID: r.ID, OrganizationID: r.OrganizationID, Name: r.Name, HeaderText: textValue(r.HeaderText),
		FooterText: textValue(r.FooterText), LogoMediaID: int8Value(r.LogoMediaID), IsDefault: r.IsDefault,
		CreatedAt: r.CreatedAt.Time, UpdatedAt: r.UpdatedAt.Time,
	}
}

// letterheadMeta is what an audit row keeps of a letterhead: the texts are only said to be there.
func letterheadMeta(l Letterhead) map[string]any {
	return map[string]any{
		"name": l.Name, "has_header": l.HeaderText != nil, "has_footer": l.FooterText != nil,
		"logo_media_id": l.LogoMediaID, "is_default": l.IsDefault,
	}
}
