package auth_test

import (
	"fmt"
	"net/http"
	"sort"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"siapp/internal/db"
	"siapp/internal/platform/testkit"
)

const adminUsersPath = "/api/v1/admin/users"

type adminUserBody struct {
	ID          string  `json:"id"`
	Email       string  `json:"email"`
	Username    *string `json:"username"`
	FullName    string  `json:"fullName"`
	Phone       *string `json:"phone"`
	BirthDate   *string `json:"birthDate"`
	SystemRole  string  `json:"systemRole"`
	Status      string  `json:"status"`
	LastLoginAt *string `json:"lastLoginAt"`
	CreatedAt   string  `json:"createdAt"`
}

type userPageBody struct {
	Items      []adminUserBody `json:"items"`
	NextCursor *string         `json:"nextCursor"`
}

func newUserBody(email string) map[string]any {
	return map[string]any{
		"email": email, "fullName": "Budi Santoso", "birthDate": "1994-05-10", "systemRole": "MEMBER",
	}
}

func createUserOK(t *testing.T, admin *testkit.Client, body map[string]any) adminUserBody {
	t.Helper()
	res := admin.Do(http.MethodPost, adminUsersPath, body)
	require.Equal(t, http.StatusCreated, res.Status, "%s", res.Body)
	var user adminUserBody
	res.JSON(t, &user)
	return user
}

func TestAdminRoutesRequireAnAdmin(t *testing.T) {
	e := startEnv(t, testkit.Deps{})
	member, _ := e.asMember(t)
	target := uuid.NewString()

	calls := []struct {
		name, method, path string
		body               any
	}{
		{"create", http.MethodPost, adminUsersPath, newUserBody("x@example.com")},
		{"list", http.MethodGet, adminUsersPath, nil},
		{"get", http.MethodGet, adminUsersPath + "/" + target, nil},
		{"update", http.MethodPatch, adminUsersPath + "/" + target, map[string]any{"fullName": "X"}},
		{"disable", http.MethodPost, adminUsersPath + "/" + target + "/disable", nil},
		{"enable", http.MethodPost, adminUsersPath + "/" + target + "/enable", nil},
	}
	for _, tc := range calls {
		t.Run(tc.name+": a member gets 403", func(t *testing.T) {
			requireError(t, member.Do(tc.method, tc.path, tc.body), http.StatusForbidden, "PERMISSION_DENIED")
		})
		t.Run(tc.name+": no token gets 401", func(t *testing.T) {
			requireError(t, e.anon.DoInvalid(tc.method, tc.path, tc.body), http.StatusUnauthorized, "UNAUTHENTICATED")
		})
	}

	t.Run("a member's attempt changes nothing", func(t *testing.T) {
		var users int
		require.NoError(t, e.pool.QueryRowContext(t.Context(), "SELECT COUNT(*) FROM users").Scan(&users))
		require.Equal(t, 1, users, "only the member exists")
	})
}

func TestCreateUser(t *testing.T) {
	e := startEnv(t, testkit.Deps{})
	admin, adminUser := e.asAdmin(t)

	body := newUserBody("  Budi.Santoso@Example.COM ")
	body["username"] = "budi.s"
	body["phone"] = "081234567890"
	user := createUserOK(t, admin, body)

	t.Run("the response describes the new user", func(t *testing.T) {
		require.Equal(t, "budi.santoso@example.com", user.Email, "email is stored lowercase")
		require.Equal(t, "budi.s", *user.Username)
		require.Equal(t, "Budi Santoso", user.FullName)
		require.Equal(t, "081234567890", *user.Phone)
		require.Equal(t, "1994-05-10", *user.BirthDate)
		require.Equal(t, "MEMBER", user.SystemRole)
		require.Equal(t, "ACTIVE", user.Status, "active at once, no activation step")
		require.Nil(t, user.LastLoginAt)
		require.NotEmpty(t, user.CreatedAt)
	})

	t.Run("the user logs in at once with their birthdate as DDMMYYYY", func(t *testing.T) {
		res := login(e.anon, "budi.santoso@example.com", "10051994")
		require.Equal(t, http.StatusOK, res.Status, "%s", res.Body)
		var tokens loginBody
		res.JSON(t, &tokens)
		require.Equal(t, user.ID, tokens.User.ID)

		// And by username, and the token works: there is no forced password change.
		require.Equal(t, http.StatusOK, login(e.anon, "budi.s", "10051994").Status)
		require.Equal(t, http.StatusOK, statusOf(t, e.srv, tokens.AccessToken))
	})

	t.Run("the password is stored hashed and never returned", func(t *testing.T) {
		var hash string
		require.NoError(t, e.pool.QueryRowContext(t.Context(), "SELECT password_hash FROM users WHERE id = ?", user.ID).Scan(&hash))
		require.Contains(t, hash, "$argon2id$")
		require.NotContains(t, hash, "10051994")
		ok, err := testkit.Passwords.Verify("10051994", hash)
		require.NoError(t, err)
		require.True(t, ok)

		res := admin.Do(http.MethodGet, adminUsersPath+"/"+user.ID, nil)
		require.NotContains(t, strings.ToLower(string(res.Body)), "password")
		require.NotContains(t, string(res.Body), "argon2")
	})

	t.Run("the audit row names the admin and holds no secret", func(t *testing.T) {
		rows := auditRows(t, e.pool, "user.create")
		require.Len(t, rows, 1)
		require.Equal(t, adminUser.ID, rows[0].ActorID.String)
		require.Equal(t, "user", rows[0].EntityType)
		require.Equal(t, user.ID, rows[0].EntityID)
		require.JSONEq(t, `{"systemRole":"MEMBER"}`, rows[0].Metadata.String)
		for _, secret := range []string{"1994", "10051994", "argon2"} {
			require.NotContains(t, rows[0].Metadata.String, secret, "the birthdate is the password")
		}
	})

	t.Run("day and month are zero-padded in the default password", func(t *testing.T) {
		b := newUserBody("padded@example.com")
		b["birthDate"] = "2001-01-02"
		createUserOK(t, admin, b)
		require.Equal(t, http.StatusOK, login(e.anon, "padded@example.com", "02012001").Status)
	})

	t.Run("an admin can create an admin", func(t *testing.T) {
		b := newUserBody("boss@example.com")
		b["systemRole"] = "ADMIN"
		require.Equal(t, "ADMIN", createUserOK(t, admin, b).SystemRole)
	})
}

func TestCreateUserValidation(t *testing.T) {
	e := startEnv(t, testkit.Deps{})
	admin, _ := e.asAdmin(t)

	with := func(field string, value any) map[string]any {
		b := newUserBody("new@example.com")
		if value == nil {
			delete(b, field)
		} else {
			b[field] = value
		}
		return b
	}
	invalid := map[string]map[string]any{
		"missing birthDate":     with("birthDate", nil),
		"future birthDate":      with("birthDate", "2999-01-01"),
		"birthDate before 1900": with("birthDate", "1899-12-31"),
		"malformed birthDate":   with("birthDate", "10-05-1994"),
		"invalid birthDate day": with("birthDate", "1994-02-30"),
		"missing email":         with("email", nil),
		"invalid email":         with("email", "not-an-email"),
		"missing fullName":      with("fullName", nil),
		"blank fullName":        with("fullName", "   "),
		"too long fullName":     with("fullName", strings.Repeat("x", 151)),
		"username too short":    with("username", "ab"),
		"username with space":   with("username", "a b c"),
		"username with @":       with("username", "budi@home"),
		"username too long":     with("username", strings.Repeat("u", 51)),
		"unknown role":          with("systemRole", "ROOT"),
		"missing role":          with("systemRole", nil),
		"a password field":      with("password", "Sup3rSecret!"),
		"phone too long":        with("phone", strings.Repeat("1", 31)),
		"empty body":            {},
	}
	// A JSON null is different from a missing key, so it needs its own body.
	nullBirthDate := newUserBody("null@example.com")
	nullBirthDate["birthDate"] = nil
	invalid["null birthDate"] = nullBirthDate

	for name, body := range invalid {
		t.Run(name, func(t *testing.T) {
			requireError(t, admin.DoInvalid(http.MethodPost, adminUsersPath, body), http.StatusBadRequest, "VALIDATION_FAILED")
		})
	}

	t.Run("nothing was created by the failed attempts", func(t *testing.T) {
		var users int
		require.NoError(t, e.pool.QueryRowContext(t.Context(), "SELECT COUNT(*) FROM users").Scan(&users))
		require.Equal(t, 1, users, "only the admin")
		require.Empty(t, auditRows(t, e.pool, "user.create"))
	})

	t.Run("the field is named in the details", func(t *testing.T) {
		res := admin.DoInvalid(http.MethodPost, adminUsersPath, with("birthDate", nil))
		require.Contains(t, string(res.Body), `"birthDate"`)
	})
}

func TestCreateUserDuplicates(t *testing.T) {
	e := startEnv(t, testkit.Deps{})
	admin, _ := e.asAdmin(t)

	first := newUserBody("taken@example.com")
	first["username"] = "taken.name"
	createUserOK(t, admin, first)

	t.Run("the same email, in any case, is USER_EMAIL_TAKEN", func(t *testing.T) {
		b := newUserBody("TAKEN@example.com")
		requireError(t, admin.Do(http.MethodPost, adminUsersPath, b), http.StatusConflict, "USER_EMAIL_TAKEN")
	})
	t.Run("the same username, in any case, is USER_USERNAME_TAKEN", func(t *testing.T) {
		b := newUserBody("other@example.com")
		b["username"] = "TAKEN.name"
		requireError(t, admin.Do(http.MethodPost, adminUsersPath, b), http.StatusConflict, "USER_USERNAME_TAKEN")
	})
	t.Run("the failed attempts left no trace", func(t *testing.T) {
		require.Len(t, auditRows(t, e.pool, "user.create"), 1)
		var n int
		require.NoError(t, e.pool.QueryRowContext(t.Context(), "SELECT COUNT(*) FROM users WHERE email = 'other@example.com'").Scan(&n))
		require.Zero(t, n)
	})
	t.Run("the email of a deleted user can be used again", func(t *testing.T) {
		_, err := e.pool.ExecContext(t.Context(), "UPDATE users SET deleted_at = NOW(3) WHERE email = 'taken@example.com'")
		require.NoError(t, err)
		b := newUserBody("taken@example.com")
		b["username"] = "taken.name"
		createUserOK(t, admin, b)
	})
}

func TestGetUser(t *testing.T) {
	e := startEnv(t, testkit.Deps{})
	admin, _ := e.asAdmin(t)
	user := createUserOK(t, admin, newUserBody("get@example.com"))

	t.Run("an existing user", func(t *testing.T) {
		res := admin.Do(http.MethodGet, adminUsersPath+"/"+user.ID, nil)
		require.Equal(t, http.StatusOK, res.Status)
		var got adminUserBody
		res.JSON(t, &got)
		require.Equal(t, user, got)
	})
	t.Run("an unknown user", func(t *testing.T) {
		requireError(t, admin.Do(http.MethodGet, adminUsersPath+"/"+uuid.NewString(), nil), http.StatusNotFound, "USER_NOT_FOUND")
	})
	t.Run("an ID that is not a UUID is also USER_NOT_FOUND", func(t *testing.T) {
		requireError(t, admin.DoInvalid(http.MethodGet, adminUsersPath+"/not-a-uuid", nil), http.StatusNotFound, "USER_NOT_FOUND")
	})
	t.Run("a deleted user is not found", func(t *testing.T) {
		_, err := e.pool.ExecContext(t.Context(), "UPDATE users SET deleted_at = NOW(3) WHERE id = ?", user.ID)
		require.NoError(t, err)
		requireError(t, admin.Do(http.MethodGet, adminUsersPath+"/"+user.ID, nil), http.StatusNotFound, "USER_NOT_FOUND")
	})
}

func listUsers(t *testing.T, c *testkit.Client, query string) userPageBody {
	t.Helper()
	res := c.Do(http.MethodGet, adminUsersPath+query, nil)
	require.Equal(t, http.StatusOK, res.Status, "%s", res.Body)
	var page userPageBody
	res.JSON(t, &page)
	return page
}

func TestListUsersPagination(t *testing.T) {
	e := startEnv(t, testkit.Deps{})
	admin, _ := e.asAdmin(t)
	for i := range 7 {
		createUserOK(t, admin, newUserBody(fmt.Sprintf("user%d@example.com", i)))
	}
	const total = 8 // seven users and the admin

	t.Run("pages follow the cursor without gaps or repeats, newest first", func(t *testing.T) {
		var all []adminUserBody
		cursor := ""
		pages := 0
		for {
			query := "?limit=3"
			if cursor != "" {
				query += "&cursor=" + cursor
			}
			page := listUsers(t, admin, query)
			require.LessOrEqual(t, len(page.Items), 3)
			all = append(all, page.Items...)
			pages++
			if page.NextCursor == nil {
				break
			}
			require.Len(t, page.Items, 3, "a full page when there is a next one")
			cursor = *page.NextCursor
			require.Less(t, pages, 10, "the cursor must advance")
		}

		require.Len(t, all, total)
		require.Equal(t, 3, pages)
		seen := map[string]bool{}
		for _, u := range all {
			require.False(t, seen[u.ID], "user %s appears twice", u.Email)
			seen[u.ID] = true
		}
		isNewestFirst := sort.SliceIsSorted(all, func(i, j int) bool {
			if all[i].CreatedAt != all[j].CreatedAt {
				return all[i].CreatedAt > all[j].CreatedAt
			}
			return all[i].ID > all[j].ID
		})
		require.True(t, isNewestFirst)
	})

	t.Run("a page that ends exactly at the last row has no next cursor", func(t *testing.T) {
		page := listUsers(t, admin, "?limit=8")
		require.Len(t, page.Items, total)
		require.Nil(t, page.NextCursor)
	})
	t.Run("the default page size holds everything here", func(t *testing.T) {
		require.Len(t, listUsers(t, admin, "").Items, total)
	})
	t.Run("a limit above the maximum is clamped, not refused", func(t *testing.T) {
		require.Len(t, listUsers(t, admin, "?limit=5000").Items, total)
	})
	t.Run("bad parameters are VALIDATION_FAILED", func(t *testing.T) {
		for _, q := range []string{"?limit=0", "?limit=abc", "?cursor=garbage", "?status=BOGUS"} {
			requireError(t, admin.DoInvalid(http.MethodGet, adminUsersPath+q, nil), http.StatusBadRequest, "VALIDATION_FAILED")
		}
	})
}

func TestListUsersFilters(t *testing.T) {
	e := startEnv(t, testkit.Deps{})
	admin, _ := e.asAdmin(t)

	named := func(email, fullName, username string) adminUserBody {
		b := newUserBody(email)
		b["fullName"] = fullName
		if username != "" {
			b["username"] = username
		}
		return createUserOK(t, admin, b)
	}
	budi := named("b1@example.com", "Budi Santoso", "")
	_ = named("b2@example.com", "Siti Aminah", "sitia")
	byEmail := named("findme.by.email@example.com", "Someone Else", "")
	byUser := named("b4@example.com", "Another Person", "unique_handle")
	percent := named("b5@example.com", "Discount 50%", "")
	underscore := named("b6@example.com", "Under_score", "")

	ids := func(page userPageBody) []string {
		out := make([]string, len(page.Items))
		for i, u := range page.Items {
			out[i] = u.ID
		}
		return out
	}

	t.Run("by name, without regard to case", func(t *testing.T) {
		require.Equal(t, []string{budi.ID}, ids(listUsers(t, admin, "?q=bUDI")))
	})
	t.Run("by email", func(t *testing.T) {
		require.Equal(t, []string{byEmail.ID}, ids(listUsers(t, admin, "?q=findme.by")))
	})
	t.Run("by username", func(t *testing.T) {
		require.Equal(t, []string{byUser.ID}, ids(listUsers(t, admin, "?q=unique_handle")))
	})
	t.Run("a percent sign matches only a real percent sign", func(t *testing.T) {
		require.Equal(t, []string{percent.ID}, ids(listUsers(t, admin, "?q=%25")), "as a wildcard it would match everyone")
		require.Equal(t, []string{percent.ID}, ids(listUsers(t, admin, "?q=50%25")))
	})
	t.Run("an underscore matches only a real underscore", func(t *testing.T) {
		got := ids(listUsers(t, admin, "?q=_"))
		require.ElementsMatch(t, []string{underscore.ID, byUser.ID}, got, "Under_score and unique_handle contain one")
	})
	t.Run("no match is an empty list, not null", func(t *testing.T) {
		page := listUsers(t, admin, "?q=zzzzzz")
		require.NotNil(t, page.Items)
		require.Empty(t, page.Items)
		require.Nil(t, page.NextCursor)
	})

	t.Run("by status", func(t *testing.T) {
		_, err := e.pool.ExecContext(t.Context(), "UPDATE users SET status = 'DISABLED' WHERE id = ?", budi.ID)
		require.NoError(t, err)
		require.Equal(t, []string{budi.ID}, ids(listUsers(t, admin, "?status=DISABLED")))
		for _, u := range listUsers(t, admin, "?status=ACTIVE").Items {
			require.Equal(t, "ACTIVE", u.Status)
		}
	})
	t.Run("a filter and a cursor work together", func(t *testing.T) {
		first := listUsers(t, admin, "?q=example.com&status=ACTIVE&limit=2")
		require.Len(t, first.Items, 2)
		require.NotNil(t, first.NextCursor)
		second := listUsers(t, admin, "?q=example.com&status=ACTIVE&limit=2&cursor="+*first.NextCursor)
		for _, u := range second.Items {
			require.NotContains(t, ids(first), u.ID)
		}
	})
	t.Run("deleted users are left out", func(t *testing.T) {
		_, err := e.pool.ExecContext(t.Context(), "UPDATE users SET deleted_at = NOW(3) WHERE id = ?", byEmail.ID)
		require.NoError(t, err)
		require.Empty(t, listUsers(t, admin, "?q=findme.by").Items)
	})
}

func TestUpdateUser(t *testing.T) {
	e := startEnv(t, testkit.Deps{})
	admin, adminUser := e.asAdmin(t)
	b := newUserBody("patch@example.com")
	b["username"] = "patchy"
	b["phone"] = "0811"
	user := createUserOK(t, admin, b)
	path := adminUsersPath + "/" + user.ID

	patch := func(body map[string]any) *testkit.Response { return admin.Do(http.MethodPatch, path, body) }

	t.Run("changes every editable field", func(t *testing.T) {
		res := patch(map[string]any{
			"fullName": "  Budi S.  ", "phone": "0822", "username": "budi.new",
			"birthDate": "1990-01-01", "systemRole": "ADMIN",
		})
		require.Equal(t, http.StatusOK, res.Status, "%s", res.Body)
		var got adminUserBody
		res.JSON(t, &got)
		require.Equal(t, "Budi S.", got.FullName, "trimmed")
		require.Equal(t, "0822", *got.Phone)
		require.Equal(t, "budi.new", *got.Username)
		require.Equal(t, "1990-01-01", *got.BirthDate)
		require.Equal(t, "ADMIN", got.SystemRole)
		require.Equal(t, user.Email, got.Email, "the email is not editable")
		require.Equal(t, got, func() adminUserBody {
			var again adminUserBody
			admin.Do(http.MethodGet, path, nil).JSON(t, &again)
			return again
		}(), "persisted")
	})

	t.Run("the audit row lists what changed without the values of sensitive fields", func(t *testing.T) {
		rows := auditRows(t, e.pool, "user.update")
		require.Len(t, rows, 1)
		require.Equal(t, adminUser.ID, rows[0].ActorID.String)
		require.Equal(t, user.ID, rows[0].EntityID)
		require.JSONEq(t, `{"changed":["fullName","phone","username","birthDate","systemRole"],"systemRole":{"from":"MEMBER","to":"ADMIN"}}`, rows[0].Metadata.String)
		require.NotContains(t, rows[0].Metadata.String, "1990")
		require.NotContains(t, rows[0].Metadata.String, "0822")
	})

	t.Run("changing the birthdate does not change the password", func(t *testing.T) {
		// Created with 1994-05-10, changed above to 1990-01-01.
		require.Equal(t, http.StatusOK, login(e.anon, user.Email, "10051994").Status, "the original default still works")
		require.Equal(t, http.StatusUnauthorized, login(e.anon, user.Email, "01011990").Status, "the new date is not a password")
	})

	t.Run("an empty username or phone clears it", func(t *testing.T) {
		res := patch(map[string]any{"username": "", "phone": ""})
		require.Equal(t, http.StatusOK, res.Status)
		var got adminUserBody
		res.JSON(t, &got)
		require.Nil(t, got.Username)
		require.Nil(t, got.Phone)
	})

	t.Run("sending the current values changes nothing and is not audited", func(t *testing.T) {
		before := len(auditRows(t, e.pool, "user.update"))
		require.Equal(t, http.StatusOK, patch(map[string]any{"fullName": "Budi S.", "systemRole": "ADMIN", "birthDate": "1990-01-01"}).Status)
		require.Len(t, auditRows(t, e.pool, "user.update"), before)
	})

	t.Run("a username another user has is USER_USERNAME_TAKEN", func(t *testing.T) {
		other := newUserBody("other@example.com")
		other["username"] = "someone.else"
		createUserOK(t, admin, other)
		requireError(t, patch(map[string]any{"username": "SOMEONE.ELSE"}), http.StatusConflict, "USER_USERNAME_TAKEN")
	})

	t.Run("invalid requests are VALIDATION_FAILED", func(t *testing.T) {
		invalid := map[string]map[string]any{
			"empty body":       {},
			"unknown field":    {"email": "changed@example.com"},
			"blank fullName":   {"fullName": "  "},
			"bad username":     {"username": "a b"},
			"username with @":  {"username": "x@y.z"},
			"unknown role":     {"systemRole": "ROOT"},
			"future birthDate": {"birthDate": "2999-01-01"},
			"old birthDate":    {"birthDate": "1800-01-01"},
			"phone too long":   {"phone": strings.Repeat("1", 31)},
		}
		for name, body := range invalid {
			t.Run(name, func(t *testing.T) {
				requireError(t, admin.DoInvalid(http.MethodPatch, path, body), http.StatusBadRequest, "VALIDATION_FAILED")
			})
		}
	})

	t.Run("an unknown user", func(t *testing.T) {
		requireError(t, admin.Do(http.MethodPatch, adminUsersPath+"/"+uuid.NewString(), map[string]any{"fullName": "X"}), http.StatusNotFound, "USER_NOT_FOUND")
	})
}

func TestAdminCannotChangeTheirOwnRole(t *testing.T) {
	e := startEnv(t, testkit.Deps{})
	admin, adminUser := e.asAdmin(t)
	path := adminUsersPath + "/" + adminUser.ID

	requireError(t, admin.Do(http.MethodPatch, path, map[string]any{"systemRole": "MEMBER"}), http.StatusConflict, "CONFLICT")
	var role string
	require.NoError(t, e.pool.QueryRowContext(t.Context(), "SELECT system_role FROM users WHERE id = ?", adminUser.ID).Scan(&role))
	require.Equal(t, "ADMIN", role)

	t.Run("other changes to their own account are fine", func(t *testing.T) {
		require.Equal(t, http.StatusOK, admin.Do(http.MethodPatch, path, map[string]any{"fullName": "Renamed Admin"}).Status)
		require.Equal(t, http.StatusOK, admin.Do(http.MethodPatch, path, map[string]any{"systemRole": "ADMIN"}).Status, "keeping the role is not a change")
	})
}

func TestDisableAndEnableUser(t *testing.T) {
	e := startEnv(t, testkit.Deps{})
	admin, adminUser := e.asAdmin(t)
	user := createUserOK(t, admin, newUserBody("victim@example.com"))
	tokens := loginAs(t, e.anon, testkit.User{Email: user.Email, Password: "10051994"}, false)
	require.Equal(t, http.StatusOK, statusOf(t, e.srv, tokens.AccessToken), "works before being disabled")

	disable := func() *testkit.Response {
		return admin.Do(http.MethodPost, adminUsersPath+"/"+user.ID+"/disable", nil)
	}
	enable := func() *testkit.Response {
		return admin.Do(http.MethodPost, adminUsersPath+"/"+user.ID+"/enable", nil)
	}

	res := disable()
	require.Equal(t, http.StatusOK, res.Status, "%s", res.Body)
	var disabled adminUserBody
	res.JSON(t, &disabled)
	require.Equal(t, "DISABLED", disabled.Status)

	t.Run("the user is locked out at once, everywhere", func(t *testing.T) {
		require.Equal(t, http.StatusUnauthorized, statusOf(t, e.srv, tokens.AccessToken), "the access token is cut off without waiting for it to expire")
		requireError(t, doRefresh(e.anon, tokens.RefreshToken), http.StatusUnauthorized, "AUTH_REFRESH_REUSED")
		requireError(t, login(e.anon, user.Email, "10051994"), http.StatusForbidden, "AUTH_ACCOUNT_DISABLED")
		requireError(t, login(e.anon, user.Email, "wrong"), http.StatusUnauthorized, "AUTH_INVALID_CREDENTIALS")

		var active int
		require.NoError(t, e.pool.QueryRowContext(t.Context(), "SELECT COUNT(*) FROM sessions WHERE user_id = ? AND revoked_at IS NULL", user.ID).Scan(&active))
		require.Zero(t, active)
	})

	t.Run("it is audited with the admin as actor", func(t *testing.T) {
		rows := auditRows(t, e.pool, "user.disable")
		require.Len(t, rows, 1)
		require.Equal(t, adminUser.ID, rows[0].ActorID.String)
		require.Equal(t, user.ID, rows[0].EntityID)
	})

	t.Run("disabling again changes nothing and is not audited again", func(t *testing.T) {
		require.Equal(t, http.StatusOK, disable().Status)
		require.Len(t, auditRows(t, e.pool, "user.disable"), 1)
	})

	t.Run("enabling lets the user log in again", func(t *testing.T) {
		res := enable()
		require.Equal(t, http.StatusOK, res.Status)
		var enabled adminUserBody
		res.JSON(t, &enabled)
		require.Equal(t, "ACTIVE", enabled.Status)
		require.Equal(t, http.StatusOK, login(e.anon, user.Email, "10051994").Status)
		require.Len(t, auditRows(t, e.pool, "user.enable"), 1)

		require.Equal(t, http.StatusOK, enable().Status, "enabling an active user is a no-op")
		require.Len(t, auditRows(t, e.pool, "user.enable"), 1)
	})

	t.Run("an admin cannot disable their own account", func(t *testing.T) {
		requireError(t, admin.Do(http.MethodPost, adminUsersPath+"/"+adminUser.ID+"/disable", nil), http.StatusConflict, "CONFLICT")
		var status string
		require.NoError(t, e.pool.QueryRowContext(t.Context(), "SELECT status FROM users WHERE id = ?", adminUser.ID).Scan(&status))
		require.Equal(t, string(db.UsersStatusACTIVE), status)
	})

	t.Run("an unknown user", func(t *testing.T) {
		for _, action := range []string{"disable", "enable"} {
			requireError(t, admin.Do(http.MethodPost, adminUsersPath+"/"+uuid.NewString()+"/"+action, nil), http.StatusNotFound, "USER_NOT_FOUND")
		}
	})
}
