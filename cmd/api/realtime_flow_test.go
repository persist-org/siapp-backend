package main

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"

	"siapp/internal/db"
	"siapp/pkg/access"
	"siapp/pkg/media"
	"siapp/pkg/platform/auth"
	"siapp/pkg/platform/clock"
	"siapp/pkg/platform/config"
	"siapp/pkg/platform/database"
	"siapp/pkg/platform/httpx"
	"siapp/pkg/platform/personel/personeltest"
	"siapp/pkg/platform/testkit"
)

const webhookSecret = "test-webhook-secret-test-webhook-secret"

// The hierarchy of the organization these flows run in.
const flowWorldSQL = `
INSERT INTO regions (code, description) VALUES ('OS1', 'Outsourcing 1');
INSERT INTO branches (code, region_code, company_letter, description) VALUES ('1K', 'OS1', 'K', 'Cabang 1K');
INSERT INTO clients (code, branch_code, description, company_prefix) VALUES ('1K-IBMI', '1K', 'IBMI', 'IBMI');
INSERT INTO projects (code, client_code, description, business_line, timezone) VALUES
  ('1K-IBMI-S', '1K-IBMI', 'IBMI keamanan', 'security', 'Asia/Jakarta');
INSERT INTO teams (code, project_code, description, active) VALUES
  ('1K-IBMI-S1', '1K-IBMI-S', 'IBMI S1', true), ('1K-IBMI-S2', '1K-IBMI-S', 'IBMI S2', true);`

// flow is the production router, built exactly as main does, with the Personel fake behind it
// and a person who is a member of 1K-IBMI-S1.
type flow struct {
	t         *testing.T
	srv       *httptest.Server
	fake      *personeltest.Fake
	tokens    *auth.Tokens
	org       int64
	person    int64 // NIKAnggota1Satu
	session   int64
	adminTok  string
	personTok string
}

func newFlow(t *testing.T) *flow {
	t.Helper()
	testkit.Reset(t)
	fake, client := testkit.NewPersonel(t)
	storage, err := media.NewLocal(config.Storage{
		LocalDir: t.TempDir(), LocalSigningKey: "flow-test-signing-key-flow-test-signing-key", PublicBaseURL: "http://localhost:8080",
	}, clock.System{})
	require.NoError(t, err)
	f := &flow{t: t, fake: fake, tokens: testkit.Tokens(t), org: personeltest.OrgSatu}
	router := newRouter(deps{
		Logger: slog.New(slog.NewJSONHandler(io.Discard, nil)), Pool: testkit.Pool(t), Redis: testkit.Redis(t),
		RedisPrefix: testkit.RandomPrefix(t), Personel: client, Tokens: f.tokens, Clock: clock.System{},
		ClientIP: httpx.ClientIPFunc(""), MediaStorage: storage, PersonelWebhookSecret: webhookSecret,
	})
	f.srv = httptest.NewServer(router)
	t.Cleanup(f.srv.Close)

	require.NoError(t, database.WithoutTenant(t.Context(), testkit.Pool(t), func(q *db.Queries) error {
		_, err := q.UpsertOrganizationSeen(t.Context(), db.UpsertOrganizationSeenParams{ID: f.org, Name: "PT Siaga Satu"})
		return err
	}))
	f.exec(flowWorldSQL)
	_, err = access.EnsureDefaults(t.Context(), testkit.Pool(t), clock.System{}, f.org)
	require.NoError(t, err)

	f.person = f.addPerson(900102, personeltest.NIKAnggota1Satu, "Anggota Satu A")
	admin := f.addPerson(900199, "ADMIN0001", "Admin Satu")
	f.exec("INSERT INTO web_role_assignments (personnel_id, role) VALUES ($1, 'ORG_ADMIN')", admin)
	f.exec("INSERT INTO team_members (team_code, personnel_id, role, start_date) VALUES ('1K-IBMI-S1', $1, 'MEMBER', current_date - 30)", f.person)
	// A membership that has not started yet, and a signed-in phone.
	f.exec("INSERT INTO team_members (team_code, personnel_id, role, start_date) VALUES ('1K-IBMI-S2', $1, 'MEMBER', current_date + 10)", f.person)
	f.session = f.addSession(f.person, "tok-person")
	f.adminTok = f.token(admin, 5001)
	f.personTok = f.token(f.person, f.session)
	return f
}

func (f *flow) token(personnelID, sessionID int64) string {
	f.t.Helper()
	tok, _, err := f.tokens.NewAccessToken(personnelID, f.org, sessionID)
	require.NoError(f.t, err)
	return tok
}

func (f *flow) exec(sql string, args ...any) {
	f.t.Helper()
	require.NoError(f.t, database.WithTenantTx(f.t.Context(), testkit.Pool(f.t), f.org, func(tx pgx.Tx) error {
		_, err := tx.Exec(f.t.Context(), sql, args...)
		return err
	}))
}

func (f *flow) query(dst []any, sql string, args ...any) {
	f.t.Helper()
	require.NoError(f.t, database.WithTenantTx(f.t.Context(), testkit.Pool(f.t), f.org, func(tx pgx.Tx) error {
		return tx.QueryRow(f.t.Context(), sql, args...).Scan(dst...)
	}))
}

func (f *flow) addPerson(userID int64, nik, name string) (id int64) {
	f.t.Helper()
	f.query([]any{&id}, "INSERT INTO personnels (personel_user_id, nik, full_name, status) VALUES ($1, $2, $3, 'ACTIVE') RETURNING id", userID, nik, name)
	return id
}

func (f *flow) addSession(personnelID int64, token string) (id int64) {
	f.t.Helper()
	f.query([]any{&id}, `INSERT INTO sessions (personnel_id, family_id, refresh_token_hash, client, expires_at)
		VALUES ($1, gen_random_uuid(), md5($2) || md5($2), 'MOBILE', now() + interval '1 day') RETURNING id`, personnelID, token)
	f.exec("INSERT INTO devices (personnel_id, session_id, platform, push_token) VALUES ($1, $2, 'ANDROID', $3)", personnelID, id, "push-"+token)
	return id
}

func (f *flow) do(method, path, token string, body []byte, headers map[string]string) (int, []byte) {
	f.t.Helper()
	req, err := http.NewRequestWithContext(f.t.Context(), method, f.srv.URL+path, bytes.NewReader(body))
	require.NoError(f.t, err)
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	res, err := http.DefaultClient.Do(req)
	require.NoError(f.t, err)
	defer func() { _ = res.Body.Close() }()
	out, err := io.ReadAll(res.Body)
	require.NoError(f.t, err)
	return res.StatusCode, out
}

func sign(body []byte) string {
	mac := hmac.New(sha256.New, []byte(webhookSecret))
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

type eventOpts struct {
	id      string
	org     int64
	at      time.Time
	signAs  string // overrides the signature
	rawBody []byte
}

// event posts a signed Personel event and returns the status code and the reason or status.
func (f *flow) event(typ string, data map[string]any, o eventOpts) (code int, result string) {
	f.t.Helper()
	if o.id == "" {
		o.id = "evt-" + typ + "-" + strconv.FormatInt(time.Now().UnixNano(), 10)
	}
	if o.org == 0 {
		o.org = f.org
	}
	if o.at.IsZero() {
		o.at = time.Now()
	}
	body := o.rawBody
	if body == nil {
		var err error
		body, err = json.Marshal(map[string]any{"event_id": o.id, "type": typ, "organization_id": o.org, "timestamp": o.at.UTC().Format(time.RFC3339), "data": data})
		require.NoError(f.t, err)
	}
	sig := sign(body)
	if o.signAs != "" {
		sig = o.signAs
	}
	code, raw := f.do(http.MethodPost, "/api/v1/integrations/personel/events", "", body, map[string]string{"X-Personel-Signature": sig})
	var env struct {
		Data  struct{ Status string } `json:"data"`
		Error struct {
			Details map[string]any `json:"details"`
		} `json:"error"`
	}
	require.NoError(f.t, json.Unmarshal(raw, &env), string(raw))
	if code == http.StatusOK {
		return code, env.Data.Status
	}
	reason, _ := env.Error.Details["reason"].(string)
	return code, reason
}

type inboxItem struct {
	Type  string `json:"type"`
	Title string `json:"title"`
	Body  string `json:"body"`
}

func (f *flow) inbox(token string) []inboxItem {
	f.t.Helper()
	code, raw := f.do(http.MethodGet, "/api/v1/notifications", token, nil, nil)
	require.Equal(f.t, http.StatusOK, code, string(raw))
	var out struct {
		Data []inboxItem `json:"data"`
	}
	require.NoError(f.t, json.Unmarshal(raw, &out))
	return out.Data
}

func TestPersonelWebhookRejectsWhatIsNotSigned(t *testing.T) {
	f := newFlow(t)
	data := map[string]any{"nik": personeltest.NIKAnggota1Satu, "position_name": "Danru"}

	for name, o := range map[string]eventOpts{
		"wrong signature":  {signAs: sign([]byte("another body"))},
		"empty signature":  {signAs: "00"},
		"not hex":          {signAs: "zz-not-hex"},
		"event too old":    {at: time.Now().Add(-6 * time.Minute)},
		"event from ahead": {at: time.Now().Add(6 * time.Minute)},
	} {
		code, reason := f.event("position.changed", data, o)
		require.Equal(t, http.StatusUnauthorized, code, name)
		require.Equal(t, "WEBHOOK_SIGNATURE_INVALID", reason, name)
	}
	var position *string
	f.query([]any{&position}, "SELECT position_name FROM personnels WHERE id = $1", f.person)
	require.Nil(t, position, "nothing was applied")

	t.Run("no signature header at all", func(t *testing.T) {
		code, _ := f.do(http.MethodPost, "/api/v1/integrations/personel/events", "", []byte(`{}`), nil)
		require.Equal(t, http.StatusUnauthorized, code)
	})
	t.Run("a bearer token is not a signature", func(t *testing.T) {
		code, _ := f.do(http.MethodPost, "/api/v1/integrations/personel/events", f.adminTok, []byte(`{}`), nil)
		require.Equal(t, http.StatusUnauthorized, code)
	})
}

func TestPersonelWebhookWithoutASecretRefusesEverything(t *testing.T) {
	testkit.Reset(t)
	_, client := testkit.NewPersonel(t)
	storage, err := media.NewLocal(config.Storage{LocalDir: t.TempDir(), LocalSigningKey: "flow-test-signing-key-flow-test-signing-key", PublicBaseURL: "http://localhost:8080"}, clock.System{})
	require.NoError(t, err)
	srv := httptest.NewServer(newRouter(deps{
		Logger: slog.New(slog.NewJSONHandler(io.Discard, nil)), Pool: testkit.Pool(t), Redis: testkit.Redis(t), RedisPrefix: testkit.RandomPrefix(t),
		Personel: client, Tokens: testkit.Tokens(t), Clock: clock.System{}, ClientIP: httpx.ClientIPFunc(""), MediaStorage: storage,
	}))
	t.Cleanup(srv.Close)

	// The signature of an EMPTY secret: what an unconfigured receiver must not accept.
	body := []byte(`{"event_id":"x","type":"employee.updated","organization_id":1,"timestamp":"2026-01-01T00:00:00Z"}`)
	mac := hmac.New(sha256.New, nil)
	mac.Write(body)
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, srv.URL+"/api/v1/integrations/personel/events", bytes.NewReader(body))
	require.NoError(t, err)
	req.Header.Set("X-Personel-Signature", hex.EncodeToString(mac.Sum(nil)))
	res, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	_ = res.Body.Close()
	require.Equal(t, http.StatusUnauthorized, res.StatusCode)
}

func TestPersonelWebhookChecksTheOrganizationAndThePayload(t *testing.T) {
	f := newFlow(t)

	code, _ := f.event("employee.updated", map[string]any{"nik": personeltest.NIKAnggota1Satu}, eventOpts{org: 424242})
	require.Equal(t, http.StatusNotFound, code, "a signed event for an organization that does not exist")

	for name, tc := range map[string]struct {
		typ  string
		data map[string]any
		o    eventOpts
	}{
		"employee without a nik":      {typ: "employee.deactivated", data: map[string]any{}},
		"position without a name":     {typ: "position.changed", data: map[string]any{"nik": "X"}},
		"placement without a project": {typ: "placement.changed", data: map[string]any{}},
		"not json":                    {typ: "x", o: eventOpts{rawBody: []byte("not json")}},
		"no event id":                 {typ: "x", o: eventOpts{rawBody: []byte(`{"type":"x","organization_id":900101,"timestamp":"` + time.Now().UTC().Format(time.RFC3339) + `"}`)}},
	} {
		code, reason := f.event(tc.typ, tc.data, tc.o)
		require.Equal(t, http.StatusBadRequest, code, name)
		require.Equal(t, "WEBHOOK_PAYLOAD_INVALID", reason, name)
	}

	// A failed event does not use up its id: the retry is processed.
	code, status := f.event("position.changed", map[string]any{"nik": personeltest.NIKAnggota1Satu, "position_name": "Danru"}, eventOpts{id: "retry-me"})
	require.Equal(t, http.StatusOK, code)
	require.Equal(t, "processed", status)
}

// ⚠ Personel delivers at least once: a replayed event must not notify twice.
func TestPersonelWebhookIgnoresAReplayedEvent(t *testing.T) {
	f := newFlow(t)
	data := map[string]any{"nik": personeltest.NIKAnggota1Satu, "position_name": "Danru Utama"}

	code, status := f.event("position.changed", data, eventOpts{id: "evt-1"})
	require.Equal(t, http.StatusOK, code)
	require.Equal(t, "processed", status)
	code, status = f.event("position.changed", data, eventOpts{id: "evt-1"})
	require.Equal(t, http.StatusOK, code)
	require.Equal(t, "duplicate", status)

	inbox := f.inbox(f.personTok)
	require.Len(t, inbox, 1, "one event, one notification")
	require.Equal(t, "POSITION_CHANGED", inbox[0].Type)
	require.Contains(t, inbox[0].Body, "Danru Utama")
	var position string
	f.query([]any{&position}, "SELECT position_name FROM personnels WHERE id = $1", f.person)
	require.Equal(t, "Danru Utama", position)

	// The same id in another organization is another event.
	code, status = f.event("position.changed", data, eventOpts{id: "evt-2"})
	require.Equal(t, http.StatusOK, code)
	require.Equal(t, "processed", status)
	require.Len(t, f.inbox(f.personTok), 2)
}

func TestPersonelWebhookDeactivationEndsSessionsAndMemberships(t *testing.T) {
	f := newFlow(t)
	code, _ := f.do(http.MethodGet, "/api/v1/notifications", f.personTok, nil, nil)
	require.Equal(t, http.StatusOK, code, "the person is signed in")

	code, status := f.event("employee.deactivated", map[string]any{"nik": personeltest.NIKAnggota1Satu}, eventOpts{})
	require.Equal(t, http.StatusOK, code)
	require.Equal(t, "processed", status)

	var personStatus string
	f.query([]any{&personStatus}, "SELECT status::text FROM personnels WHERE id = $1", f.person)
	require.Equal(t, "INACTIVE", personStatus)

	var live, devices, active, planned int
	f.query([]any{&live}, "SELECT count(*) FROM sessions WHERE personnel_id = $1 AND revoked_at IS NULL", f.person)
	f.query([]any{&devices}, "SELECT count(*) FROM devices WHERE personnel_id = $1", f.person)
	f.query([]any{&active}, "SELECT count(*) FROM team_members WHERE personnel_id = $1 AND (end_date IS NULL OR end_date > current_date + 1)", f.person)
	f.query([]any{&planned}, "SELECT count(*) FROM team_members WHERE personnel_id = $1 AND start_date > current_date + 1", f.person)
	require.Zero(t, live, "every session ended")
	require.Zero(t, devices, "no push goes to a phone of someone who left")
	require.Zero(t, active, "the membership in force ended")
	require.Zero(t, planned, "the planned membership never took effect and is gone")
	var history int
	f.query([]any{&history}, "SELECT count(*) FROM team_members WHERE personnel_id = $1", f.person)
	require.Equal(t, 1, history, "the ended membership stays as history")

	code, _ = f.do(http.MethodGet, "/api/v1/notifications", f.personTok, nil, nil)
	require.Equal(t, http.StatusUnauthorized, code, "the access token died at once, not in 15 minutes")

	t.Run("a person who never signed in is nothing to change", func(t *testing.T) {
		code, status := f.event("employee.deactivated", map[string]any{"nik": "NEVER-SEEN"}, eventOpts{})
		require.Equal(t, http.StatusOK, code)
		require.Equal(t, "processed", status)
	})
}

func TestPersonelWebhookEmployeeUpdatedRefreshesOrDeactivates(t *testing.T) {
	f := newFlow(t)
	f.fake.Rename(personeltest.NIKAnggota1Satu, "Nama Baru Dari Personel")

	code, _ := f.event("employee.updated", map[string]any{"nik": personeltest.NIKAnggota1Satu}, eventOpts{})
	require.Equal(t, http.StatusOK, code)
	var name, status string
	f.query([]any{&name, &status}, "SELECT full_name, status::text FROM personnels WHERE id = $1", f.person)
	require.Equal(t, "Nama Baru Dari Personel", name)
	require.Equal(t, "ACTIVE", status)

	// Personel now says the person is inactive: the same event deactivates them.
	f.fake.Deactivate(personeltest.NIKAnggota1Satu)
	code, _ = f.event("employee.updated", map[string]any{"nik": personeltest.NIKAnggota1Satu}, eventOpts{})
	require.Equal(t, http.StatusOK, code)
	f.query([]any{&name, &status}, "SELECT full_name, status::text FROM personnels WHERE id = $1", f.person)
	require.Equal(t, "INACTIVE", status)
	var live int
	f.query([]any{&live}, "SELECT count(*) FROM sessions WHERE personnel_id = $1 AND revoked_at IS NULL", f.person)
	require.Zero(t, live)
}

func TestPersonelWebhookAcknowledgesTheRest(t *testing.T) {
	f := newFlow(t)
	for typ, want := range map[string]string{
		"placement.changed": "processed",
		"leave.decided":     "ignored",
		"something.new":     "ignored",
	} {
		code, status := f.event(typ, map[string]any{"project_id": 77}, eventOpts{})
		require.Equal(t, http.StatusOK, code, typ)
		require.Equal(t, want, status, typ)
	}
}

func TestAnAdminMoveAndRoleChangeCreateInboxRows(t *testing.T) {
	f := newFlow(t)
	f.exec("DELETE FROM team_members WHERE start_date > current_date") // the planned one would overlap the move
	var membership int64
	f.query([]any{&membership}, "SELECT id FROM team_members WHERE personnel_id = $1 AND team_code = '1K-IBMI-S1'", f.person)
	today := time.Now().Format("2006-01-02")

	code, raw := f.do(http.MethodPost, "/api/v1/admin/team-members/"+strconv.FormatInt(membership, 10)+"/move", f.adminTok,
		[]byte(`{"to_team_code":"1K-IBMI-S2","effective_date":"`+today+`"}`), nil)
	require.Equal(t, http.StatusOK, code, string(raw))

	inbox := f.inbox(f.personTok)
	require.Len(t, inbox, 1)
	require.Equal(t, "TEAM_CHANGED", inbox[0].Type)
	require.Contains(t, inbox[0].Body, "1K-IBMI-S2")
	var adminRows int
	f.query([]any{&adminRows}, "SELECT count(*) FROM notifications WHERE recipient_personnel_id <> $1", f.person)
	require.Zero(t, adminRows, "the admin who did it is not told")

	var moved int64
	f.query([]any{&moved}, "SELECT id FROM team_members WHERE personnel_id = $1 AND team_code = '1K-IBMI-S2' AND end_date IS NULL AND start_date <= current_date", f.person)
	code, raw = f.do(http.MethodPost, "/api/v1/admin/team-members/"+strconv.FormatInt(moved, 10)+"/change-role", f.adminTok,
		[]byte(`{"role":"DANRU","effective_date":"`+time.Now().AddDate(0, 0, 1).Format("2006-01-02")+`"}`), nil)
	require.Equal(t, http.StatusOK, code, string(raw))

	inbox = f.inbox(f.personTok)
	require.Len(t, inbox, 2)
	types := []string{inbox[0].Type, inbox[1].Type}
	require.ElementsMatch(t, []string{"TEAM_CHANGED", "POSITION_CHANGED"}, types)

	t.Run("a rejected move notifies nobody", func(t *testing.T) {
		code, _ := f.do(http.MethodPost, "/api/v1/admin/team-members/"+strconv.FormatInt(moved, 10)+"/move", f.adminTok,
			[]byte(`{"to_team_code":"1K-IBMI-S2","effective_date":"`+today+`"}`), nil)
		require.NotEqual(t, http.StatusOK, code)
		require.Len(t, f.inbox(f.personTok), 2)
	})
}

func TestTheWebSocketRouteIsPublicAndTheTicketRouteIsNot(t *testing.T) {
	f := newFlow(t)
	// Without an Upgrade the public route answers its own error, not the middleware's 401.
	code, _ := f.do(http.MethodGet, "/api/v1/ws", "", nil, nil)
	require.NotEqual(t, http.StatusUnauthorized, code)
	code, _ = f.do(http.MethodPost, "/api/v1/ws/ticket", "", nil, nil)
	require.Equal(t, http.StatusUnauthorized, code)
	code, raw := f.do(http.MethodPost, "/api/v1/ws/ticket", f.personTok, nil, nil)
	require.Equal(t, http.StatusOK, code, string(raw))
}
