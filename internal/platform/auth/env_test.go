package auth_test

import (
	"database/sql"
	"net/http/httptest"
	"regexp"
	"testing"

	"github.com/stretchr/testify/require"

	"siapp/internal/db"
	"siapp/internal/mail"
	"siapp/internal/platform/testkit"
)

// env is a server on a clean database with the whoami route and a recording mailer.
type env struct {
	srv    *httptest.Server
	anon   *testkit.Client
	pool   *sql.DB
	mailer *mail.FakeSender
}

func startEnv(t *testing.T, deps testkit.Deps) *env {
	t.Helper()
	pool := testkit.DB(t)
	testkit.Reset(t, pool)

	mailer := mail.NewFakeSender()
	deps.DB = pool
	deps.ExtraRoutes = whoami
	if deps.Mailer == nil {
		deps.Mailer = mailer
	} else if fake, ok := deps.Mailer.(*mail.FakeSender); ok {
		mailer = fake
	}
	srv := testkit.NewServer(t, deps)
	return &env{srv: srv, anon: testkit.NewClient(t, srv), pool: pool, mailer: mailer}
}

// asAdmin creates an admin and returns a client logged in as them.
func (e *env) asAdmin(t *testing.T) (*testkit.Client, testkit.User) {
	t.Helper()
	admin := testkit.CreateUser(t, e.pool, testkit.UserOpts{Role: db.UsersSystemRoleADMIN, FullName: "Admin Test"})
	return e.anon.Login(admin.Email, admin.Password), admin
}

// asMember creates an ordinary user and returns a client logged in as them.
func (e *env) asMember(t *testing.T) (*testkit.Client, testkit.User) {
	t.Helper()
	member := testkit.CreateUser(t, e.pool, testkit.UserOpts{})
	return e.anon.Login(member.Email, member.Password), member
}

type auditRow struct {
	ActorID    sql.NullString
	Action     string
	EntityType string
	EntityID   string
	Metadata   sql.NullString
}

// auditRows returns the audit rows with this action, oldest first.
func auditRows(t *testing.T, pool *sql.DB, action string) []auditRow {
	t.Helper()
	rows, err := pool.QueryContext(t.Context(), `
		SELECT actor_id, action, entity_type, entity_id, metadata
		FROM audit_logs WHERE action = ? ORDER BY created_at, id`, action)
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()

	var out []auditRow
	for rows.Next() {
		var r auditRow
		require.NoError(t, rows.Scan(&r.ActorID, &r.Action, &r.EntityType, &r.EntityID, &r.Metadata))
		out = append(out, r)
	}
	require.NoError(t, rows.Err())
	return out
}

var tokenInLink = regexp.MustCompile(`token=([A-Za-z0-9_-]+)`)

// resetTokenOf extracts the reset token from the link in an email.
func resetTokenOf(t *testing.T, msg mail.Message) string {
	t.Helper()
	m := tokenInLink.FindStringSubmatch(msg.Text)
	require.Len(t, m, 2, "no reset link in the email: %s", msg.Text)
	return m[1]
}
