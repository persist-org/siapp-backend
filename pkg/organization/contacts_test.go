package organization_test

import (
	"net/http"
	"strconv"
	"testing"

	"github.com/stretchr/testify/require"

	"siapp/pkg/masterdata"
	"siapp/pkg/platform/testkit"
)

// contact is one emergency contact of a request.
func contact(typeID int64, name, phone string) map[string]any {
	return map[string]any{"stakeholder_type_id": typeID, "name": name, "phone": phone}
}

func contactsOf(t *testing.T, res reply) []map[string]any {
	t.Helper()
	require.Equal(t, http.StatusOK, res.Status, "body: %s", res.Raw)
	var out []map[string]any
	for _, c := range data[row](t, res)["emergency_contacts"].([]any) {
		out = append(out, c.(map[string]any))
	}
	return out
}

func TestAProjectsEmergencyContactsAreValidated(t *testing.T) {
	e := newEnv(t)
	org := testkit.NewOrg(t)
	admin := e.person(org, "ORG_ADMIN", "")
	_, _, _, project := e.tree(admin, 1)
	path := "/admin/projects/" + project
	police := stakeholderType(t, org, "POLICE")
	hospital := stakeholderType(t, org, "HOSPITAL")

	patch := func(contacts ...map[string]any) reply {
		return e.raw(admin, http.MethodPatch, path, map[string]any{"emergency_contacts": contacts})
	}
	invalid := func(res reply, fields ...string) {
		t.Helper()
		require.Equal(t, http.StatusUnprocessableEntity, res.Status, "body: %s", res.Raw)
		for _, f := range fields {
			require.Contains(t, string(res.Raw), `"`+f+`"`, "body: %s", res.Raw)
		}
	}

	t.Run("a phone is stored as E.164 whatever way it was typed", func(t *testing.T) {
		res := patch(
			contact(police, "Polsek Menteng", "0812-3456-7890"),
			contact(police, "Polres", "+62 21 384 0110"),
			contact(hospital, "RS Harapan", "62215550101"),
			contact(hospital, "Ambulans", "119"),
		)
		got := contactsOf(t, res)
		require.Len(t, got, 4)
		require.Equal(t, "+6281234567890", got[0]["phone"])
		require.Equal(t, "+62213840110", got[1]["phone"])
		require.Equal(t, "+62215550101", got[2]["phone"])
		require.Equal(t, "119", got[3]["phone"], "a national emergency number stays as it is")
		require.EqualValues(t, police, got[0]["stakeholder_type_id"])
	})

	t.Run("a phone that is no number is refused, naming the contact", func(_ *testing.T) {
		for _, bad := range []string{"abc", "1234", "+0812345", "08", "0812-abc", " "} {
			invalid(patch(contact(police, "Polsek", bad)), "emergency_contacts[0].phone")
		}
		invalid(patch(contact(police, "Ok", "110"), contact(police, "Salah", "xx")), "emergency_contacts[1].phone")
	})

	t.Run("a type that is not there is refused with its own reason", func(t *testing.T) {
		for _, id := range []int64{999999, 0, -3} {
			res := patch(contact(id, "Polsek", "110"))
			require.Equal(t, http.StatusUnprocessableEntity, res.Status, "type %d: %s", id, res.Raw)
		}
		expect(t, patch(contact(999999, "Polsek", "110")), http.StatusUnprocessableEntity, "VALIDATION_ERROR", masterdata.ReasonStakeholderTypeNotFound)

		// Another organization's type is not there either.
		otherOrg := testkit.NewOrg(t)
		foreign := stakeholderType(t, otherOrg, "POLICE")
		expect(t, patch(contact(foreign, "Polsek", "110")), http.StatusUnprocessableEntity, "VALIDATION_ERROR", masterdata.ReasonStakeholderTypeNotFound)

		// Nor is a deleted one.
		gone := stakeholderType(t, org, "GONE")
		require.NoError(t, execInTenant(t, org, "UPDATE stakeholder_types SET active = false, deleted_at = now() WHERE id = "+strconv.FormatInt(gone, 10)))
		expect(t, patch(contact(gone, "Polsek", "110")), http.StatusUnprocessableEntity, "VALIDATION_ERROR", masterdata.ReasonStakeholderTypeNotFound)
	})

	t.Run("a type that was only deactivated still works", func(t *testing.T) {
		quiet := stakeholderType(t, org, "QUIET")
		require.NoError(t, execInTenant(t, org, "UPDATE stakeholder_types SET active = false WHERE id = "+strconv.FormatInt(quiet, 10)))
		require.Len(t, contactsOf(t, patch(contact(quiet, "Pos", "110"))), 1)
	})

	t.Run("the same number twice for one type is a slip", func(t *testing.T) {
		invalid(patch(contact(police, "A", "0812 3456 7890"), contact(police, "B", "+6281234567890")), "emergency_contacts[1].phone")
		// The same number under another type is a different contact.
		require.Len(t, contactsOf(t, patch(contact(police, "A", "110"), contact(hospital, "B", "110"))), 2)
	})

	t.Run("names are checked and cleaned, the list is bounded", func(t *testing.T) {
		invalid(patch(contact(police, "   ", "110")), "emergency_contacts[0].name")
		invalid(patch(contact(police, "bel\u0007", "110")), "emergency_contacts[0].name")
		got := contactsOf(t, patch(contact(police, "  Polsek \t Menteng ", "110")))
		require.Equal(t, "Polsek Menteng", got[0]["name"])

		many := make([]map[string]any, 21)
		for i := range many {
			many[i] = contact(police, "Pos "+strconv.Itoa(i), "+62812345678"+strconv.Itoa(i%10)+strconv.Itoa(i/10))
		}
		invalid(patch(many...), "emergency_contacts")
		require.Len(t, contactsOf(t, patch(many[:20]...)), 20)
	})

	t.Run("every problem is reported at once", func(_ *testing.T) {
		invalid(patch(contact(police, "\a", "abc"), contact(police, "Ok", "110"), contact(police, "Dua", "zzz")),
			"emergency_contacts[0].name", "emergency_contacts[0].phone", "emergency_contacts[2].phone")
	})

	t.Run("the old shape and unknown fields are refused", func(_ *testing.T) {
		invalid(e.raw(admin, http.MethodPatch, path, map[string]any{"emergency_contacts": []map[string]any{{"type": "Polisi", "name": "Polres", "phone": "110"}}}))
		invalid(e.raw(admin, http.MethodPatch, path, map[string]any{"emergency_contacts": []map[string]any{{"stakeholder_type_id": police, "name": "X", "phone": "110", "extra": 1}}}))
		invalid(e.raw(admin, http.MethodPatch, path, map[string]any{"emergency_contacts": []map[string]any{{"stakeholder_type_id": police, "phone": "110"}}}))
	})

	t.Run("a failed change changes nothing", func(t *testing.T) {
		before := contactsOf(t, patch(contact(police, "Tetap", "110")))
		res := e.raw(admin, http.MethodPatch, path, map[string]any{"description": "Tidak Boleh Berubah", "emergency_contacts": []map[string]any{contact(police, "X", "abc")}})
		require.Equal(t, http.StatusUnprocessableEntity, res.Status)
		got := e.api(admin, http.MethodGet, path, nil)
		require.Equal(t, http.StatusOK, got.Status, "body: %s", got.Raw)
		require.NotEqual(t, "Tidak Boleh Berubah", data[row](t, got).str("description"))
		require.Equal(t, before, contactsOf(t, got))
	})

	t.Run("contacts that are not sent are kept, an empty list clears them", func(t *testing.T) {
		contactsOf(t, patch(contact(police, "Tetap", "110")))
		kept := e.api(admin, http.MethodPatch, path, map[string]any{"description": "Nama Baru"})
		require.Len(t, contactsOf(t, kept), 1)
		require.Empty(t, contactsOf(t, e.raw(admin, http.MethodPatch, path, map[string]any{"emergency_contacts": []map[string]any{}})))
	})

	t.Run("a new project is checked the same way", func(t *testing.T) {
		client := e.mustCreate(admin, "/admin/clients", map[string]any{"branch_code": "1K", "company_prefix": "NEWC", "description": "Klien Baru"}).str("code")
		body := func(contacts ...map[string]any) map[string]any {
			return map[string]any{
				"client_code": client, "business_line": "cleaning", "description": "Proyek Baru", "timezone": "Asia/Jakarta",
				"emergency_contacts": contacts,
			}
		}
		invalid(e.raw(admin, http.MethodPost, "/admin/projects", body(contact(police, "Polsek", "abc"))), "emergency_contacts[0].phone")
		expect(t, e.raw(admin, http.MethodPost, "/admin/projects", body(contact(999999, "Polsek", "110"))),
			http.StatusUnprocessableEntity, "VALIDATION_ERROR", masterdata.ReasonStakeholderTypeNotFound)
		made := e.api(admin, http.MethodPost, "/admin/projects", body(contact(police, "Polsek", "0812 3456 7890")))
		require.Equal(t, http.StatusCreated, made.Status, "body: %s", made.Raw)
		var stored []any
		stored, _ = data[row](t, made)["emergency_contacts"].([]any)
		require.Len(t, stored, 1)
		require.Equal(t, "+6281234567890", stored[0].(map[string]any)["phone"])
	})
}
