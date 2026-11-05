package masterdata

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"

	"siapp/internal/db"
	"siapp/pkg/platform/apperr"
	"siapp/pkg/platform/auth"
	"siapp/pkg/platform/database"
)

// maxContactsPerProject bounds a project's phone book: a screen of numbers for a guard in a
// hurry, not a directory.
const maxContactsPerProject = 20

// Contact is one emergency number of a project: who it is (a stakeholder type), what to call it
// ("Polsek Menteng") and the number. It is the shape stored in projects.emergency_contacts.
type Contact struct {
	StakeholderTypeID int64  `json:"stakeholder_type_id" validate:"required,min=1"`
	Name              string `json:"name" validate:"required,notblank,max=200"`
	Phone             string `json:"phone" validate:"required,notblank,max=50"`
}

// ValidateContacts checks the contacts a project is created or patched with, inside the tenant
// transaction of q, and returns them as they are to be stored: names cleaned, phones in E.164
// (or a national emergency number), in the order sent.
//
// Every problem is reported at once, as one VALIDATION_ERROR naming emergency_contacts[i].field.
// A type that is not there is reason STAKEHOLDER_TYPE_NOT_FOUND. The types are locked FOR SHARE, so
// a delete of one of them waits for this transaction (and then sees the contact), or finishes
// first (and then the type is not found here).
func ValidateContacts(ctx context.Context, q *db.Queries, contacts []Contact) ([]Contact, error) {
	if len(contacts) == 0 {
		return []Contact{}, nil
	}
	if len(contacts) > maxContactsPerProject {
		return nil, validationError("emergency_contacts", "Maksimum "+strconv.Itoa(maxContactsPerProject)+" kontak darurat per proyek.")
	}

	var problems []apperr.FieldError
	fail := func(i int, field, code, msg string) {
		problems = append(problems, apperr.FieldError{
			Field: "emergency_contacts[" + strconv.Itoa(i) + "]." + field, Code: code, Message: msg,
		})
	}

	out := make([]Contact, len(contacts))
	ids := make([]int64, 0, len(contacts))
	seen := map[string]int{}
	for i, c := range contacts {
		out[i] = Contact{StakeholderTypeID: c.StakeholderTypeID}
		if c.StakeholderTypeID < 1 {
			fail(i, "stakeholder_type_id", "INVALID", "Wajib diisi.")
		} else if !slices.Contains(ids, c.StakeholderTypeID) {
			ids = append(ids, c.StakeholderTypeID)
		}
		if name, problem := contactNameProblem(c.Name); problem != "" {
			fail(i, "name", "INVALID", problem)
		} else {
			out[i].Name = name
		}
		phone, ok := normalizePhone(c.Phone)
		if !ok {
			fail(i, "phone", "INVALID", "Harus nomor telepon internasional (+62812…), nomor lokal (0812…) atau nomor darurat nasional (110, 113, …).")
			continue
		}
		out[i].Phone = phone
		// The same number twice for one type is a slip, not a second contact.
		key := strconv.FormatInt(c.StakeholderTypeID, 10) + "|" + phone
		if first, dup := seen[key]; dup {
			fail(i, "phone", "DUPLICATE", "Nomor ini sudah ada pada kontak ke-"+strconv.Itoa(first+1)+" untuk tipe yang sama.")
		}
		seen[key] = i
	}

	if len(ids) > 0 {
		found, err := q.LockStakeholderTypesForShare(ctx, ids)
		if err != nil {
			return nil, fmt.Errorf("lock stakeholder types: %w", err)
		}
		known := make(map[int64]bool, len(found))
		for _, t := range found {
			known[t.ID] = true
		}
		var missing bool
		for i, c := range contacts {
			if c.StakeholderTypeID >= 1 && !known[c.StakeholderTypeID] {
				missing = true
				fail(i, "stakeholder_type_id", "NOT_FOUND", "Tipe stakeholder tidak ditemukan atau sudah dihapus.")
			}
		}
		if missing && len(problems) > 0 {
			return nil, apperr.New(422, apperr.CodeValidation, ReasonStakeholderTypeNotFound,
				"Ada tipe stakeholder yang tidak ditemukan.").WithFields(problems...)
		}
	}
	if len(problems) > 0 {
		return nil, apperr.ErrValidation.WithFields(problems...)
	}
	return out, nil
}

// maxContactNameLength is the longest contact name, in characters (the contract's limit).
const maxContactNameLength = 200

// contactNameProblem cleans a contact's name and says what is wrong with it, "" when nothing is.
func contactNameProblem(raw string) (name, problem string) {
	name = strings.Join(strings.Fields(raw), " ")
	switch {
	case name == "":
		return "", "Wajib diisi."
	case utf8.RuneCountInString(name) > maxContactNameLength:
		return "", "Maksimum 200 karakter."
	case strings.IndexFunc(name, unicode.IsControl) >= 0:
		return "", "Tidak boleh berisi karakter kontrol."
	}
	return name, ""
}

// ProjectContact is one number of the phone book ContactsForProject returns.
type ProjectContact struct {
	StakeholderTypeID int64
	TypeCode          string
	TypeName          string
	// Name is what the contact is called: the project's own name for it, or the type's name for a
	// number that comes from the type's default.
	Name  string
	Phone string
	// Default is true when the number is the type's default, because the project has no contact
	// of this type.
	Default bool
}

// ContactsForProject is the phone book of a project: its own contacts, plus the default phone of
// every active stakeholder type it has no contact for, with the preferred type first (0: none).
// Phase 09.1 uses it to put the number of the panic type's stakeholder on top.
//
// Inside each type the project's contacts keep the order an admin gave them; the types follow
// their sort order. A type that was deactivated after a project chose it still shows that
// project's contacts (they are still the right numbers), but is never a fallback.
//
// The organization is the caller's, from ctx (auth.From). ErrProjectNotFound for a project that
// is not there, deleted, or another organization's.
func (s *Service) ContactsForProject(ctx context.Context, projectCode string, preferredTypeID int64) ([]ProjectContact, error) {
	p, ok := auth.From(ctx)
	if !ok {
		return nil, errors.New("masterdata: ContactsForProject needs an authenticated caller in ctx")
	}
	var out []ProjectContact
	err := database.WithTenant(ctx, s.pool, p.OrganizationID, func(q *db.Queries) (err error) {
		out, err = ContactsForProjectTx(ctx, q, projectCode, preferredTypeID)
		return err
	})
	return out, err
}

// ContactsForProjectTx is ContactsForProject inside the tenant transaction of q.
func ContactsForProjectTx(ctx context.Context, q *db.Queries, projectCode string, preferredTypeID int64) ([]ProjectContact, error) {
	raw, err := q.GetProjectContacts(ctx, projectCode)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrProjectNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get project contacts: %w", err)
	}
	var stored []Contact
	// The column is written only through ValidateContacts, so a value that does not decode is a
	// bug, not input: the project then has no contacts of its own, and the defaults still show.
	if err := json.Unmarshal(raw, &stored); err != nil {
		stored = nil
	}

	ids := make([]int64, 0, len(stored))
	for _, c := range stored {
		if !slices.Contains(ids, c.StakeholderTypeID) {
			ids = append(ids, c.StakeholderTypeID)
		}
	}
	chosen, err := q.ListStakeholderTypesByIDs(ctx, ids)
	if err != nil {
		return nil, fmt.Errorf("list stakeholder types: %w", err)
	}
	active, err := q.ListActiveStakeholderTypes(ctx)
	if err != nil {
		return nil, fmt.Errorf("list active stakeholder types: %w", err)
	}

	types := map[int64]db.StakeholderType{}
	for _, t := range chosen {
		types[t.ID] = t
	}
	for _, t := range active {
		types[t.ID] = t
	}

	var out []ProjectContact
	has := map[int64]bool{}
	for _, c := range stored {
		t, ok := types[c.StakeholderTypeID]
		if !ok {
			continue // a type that was deleted cannot be: it was in use. Skip, do not fail a panic.
		}
		has[t.ID] = true
		out = append(out, ProjectContact{StakeholderTypeID: t.ID, TypeCode: t.Code, TypeName: t.Name, Name: c.Name, Phone: c.Phone})
	}
	for _, t := range active {
		if has[t.ID] || !t.DefaultPhone.Valid {
			continue
		}
		out = append(out, ProjectContact{
			StakeholderTypeID: t.ID, TypeCode: t.Code, TypeName: t.Name, Name: t.Name, Phone: t.DefaultPhone.String, Default: true,
		})
	}

	// Preferred type first, then by the types' sort order; a stable sort keeps the admin's order
	// among the contacts of one type.
	slices.SortStableFunc(out, func(a, b ProjectContact) int {
		ta, tb := types[a.StakeholderTypeID], types[b.StakeholderTypeID]
		if pa, pb := ta.ID == preferredTypeID, tb.ID == preferredTypeID; pa != pb {
			if pa {
				return -1
			}
			return 1
		}
		if c := cmp.Compare(ta.SortOrder, tb.SortOrder); c != 0 {
			return c
		}
		return cmp.Compare(ta.ID, tb.ID)
	})
	return out, nil
}

// FieldContactsOfProject is what GET /projects/{code}/emergency-contacts answers: the phone book
// of a project the caller serves in today. Anyone else is ErrNotProjectMember (a project that is
// not there is ErrProjectNotFound first, so a code cannot be probed across organizations).
func (s *Service) FieldContactsOfProject(ctx context.Context, p auth.Principal, projectCode string) ([]ProjectContact, error) {
	var out []ProjectContact
	err := database.WithTenant(ctx, s.pool, p.OrganizationID, func(q *db.Queries) error {
		if _, err := q.GetProjectContacts(ctx, projectCode); errors.Is(err, pgx.ErrNoRows) {
			return ErrProjectNotFound
		} else if err != nil {
			return fmt.Errorf("get project: %w", err)
		}
		serving, err := q.IsServingInProject(ctx, db.IsServingInProjectParams{
			PersonnelID: p.PersonnelID, ProjectCode: projectCode, Now: ts(s.clock.Now()),
		})
		if err != nil {
			return fmt.Errorf("check membership: %w", err)
		}
		if !serving {
			return ErrNotProjectMember
		}
		out, err = ContactsForProjectTx(ctx, q, projectCode, 0)
		return err
	})
	return out, err
}
