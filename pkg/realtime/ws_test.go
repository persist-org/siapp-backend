package realtime_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/jackc/pgx/v5"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"

	"siapp/pkg/access"
	"siapp/pkg/notifications"
	"siapp/pkg/platform/auth"
	"siapp/pkg/platform/clock"
	"siapp/pkg/platform/database"
	"siapp/pkg/platform/testkit"
	"siapp/pkg/realtime"
)

func TestMain(m *testing.M) { os.Exit(testkit.NewDatabase(m)) }

const (
	team1 = "1K-IBMI-S1"
	team2 = "1K-IBMI-S2"
)

// The hierarchy of every organization these tests build. The same team codes exist in each
// organization, which is exactly what the isolation test needs.
const worldSQL = `
INSERT INTO regions (code, description) VALUES ('OS1', 'Outsourcing 1'), ('OS2', 'Outsourcing 2');
INSERT INTO branches (code, region_code, company_letter, description) VALUES
  ('1K', 'OS1', 'K', 'Cabang 1K'), ('2K', 'OS2', 'K', 'Cabang 2K');
INSERT INTO clients (code, branch_code, description, company_prefix) VALUES
  ('1K-IBMI', '1K', 'IBMI', 'IBMI'), ('2K-QQQQ', '2K', 'QQQQ', 'QQQQ');
INSERT INTO projects (code, client_code, description, business_line, timezone) VALUES
  ('1K-IBMI-S', '1K-IBMI', 'IBMI keamanan', 'security', 'Asia/Jakarta'),
  ('2K-QQQQ-S', '2K-QQQQ', 'QQQQ keamanan', 'security', 'Asia/Jakarta');
INSERT INTO teams (code, project_code, description, active) VALUES
  ('1K-IBMI-S1', '1K-IBMI-S', 'IBMI S1', true), ('1K-IBMI-S2', '1K-IBMI-S', 'IBMI S2', true),
  ('2K-QQQQ-S1', '2K-QQQQ-S', 'QQQQ S1', true);`

// rig is a real server with the realtime routes, on the test database and Redis.
type rig struct {
	t           *testing.T
	rdb         *redis.Client
	prefix      string
	tokens      *auth.Tokens
	revocations *auth.Revocations
	tickets     *realtime.Tickets
	publisher   *realtime.RedisPublisher
	hub         *realtime.Hub
	baseURL     string
	orgA, orgB  int64
	userID      int64
}

func quietLogger() *slog.Logger { return slog.New(slog.NewJSONHandler(io.Discard, nil)) }

func newRig(t *testing.T, cfg realtime.Config) *rig {
	t.Helper()
	testkit.Reset(t)
	r := &rig{t: t, rdb: testkit.Redis(t), prefix: testkit.RandomPrefix(t), tokens: testkit.Tokens(t)}
	r.revocations = auth.NewRevocations(r.rdb, r.prefix, quietLogger())
	r.tickets = realtime.NewTickets(r.rdb, r.prefix)
	r.publisher = realtime.NewRedisPublisher(r.rdb, r.prefix, clock.System{})
	r.baseURL, r.hub = r.server(cfg)
	r.orgA, r.orgB = r.world(), r.world()
	return r
}

// server starts one API instance: its own hub, its own subscriber, the same Redis and database.
func (r *rig) server(cfg realtime.Config) (baseURL string, hub *realtime.Hub) {
	r.t.Helper()
	hub = realtime.NewHub(quietLogger())
	require.NoError(r.t, realtime.Run(r.t.Context(), r.rdb, r.prefix, hub, quietLogger()))
	handler := realtime.NewHandler(r.tokens, r.revocations, r.tickets, hub,
		realtime.NewAuthorizer(testkit.Pool(r.t), access.NewService(testkit.Pool(r.t), clock.System{}), clock.System{}),
		clock.System{}, quietLogger(), cfg)
	srv := testkit.NewServer(r.t, testkit.Deps{Mount: handler.Mount, Revocations: r.revocations})
	return srv.URL, hub
}

func (r *rig) world() int64 {
	r.t.Helper()
	org := testkit.NewOrg(r.t)
	r.exec(org, worldSQL)
	_, err := access.EnsureDefaults(r.t.Context(), testkit.Pool(r.t), clock.System{}, org)
	require.NoError(r.t, err)
	return org
}

func (r *rig) exec(org int64, sql string, args ...any) {
	r.t.Helper()
	require.NoError(r.t, database.WithTenantTx(r.t.Context(), testkit.Pool(r.t), org, func(tx pgx.Tx) error {
		_, err := tx.Exec(r.t.Context(), sql, args...)
		return err
	}))
}

// person adds a person to an organization and returns the access token and the id.
func (r *rig) person(org int64) (token string, id int64) {
	r.t.Helper()
	r.userID++
	require.NoError(r.t, database.WithTenantTx(r.t.Context(), testkit.Pool(r.t), org, func(tx pgx.Tx) error {
		return tx.QueryRow(r.t.Context(),
			"INSERT INTO personnels (personel_user_id, full_name, status) VALUES ($1, 'Petugas Realtime', 'ACTIVE') RETURNING id",
			r.userID+5000).Scan(&id)
	}))
	return testkit.Token(r.t, org, id), id
}

func (r *rig) member(org int64, team string, person int64, role string) {
	r.t.Helper()
	r.exec(org, "INSERT INTO team_members (team_code, personnel_id, role, start_date) VALUES ($1, $2, $3::team_role, current_date - 30)", team, person, role)
}

func (r *rig) admin(org, person int64, role, region string) {
	r.t.Helper()
	r.exec(org, "INSERT INTO web_role_assignments (personnel_id, role, region_code) VALUES ($1, $2::web_role, NULLIF($3, ''))", person, role, region)
}

// dial opens a socket. A close during the handshake is reported by the first Read.
func (r *rig) dial(token, query string) *websocket.Conn {
	r.t.Helper()
	opts := &websocket.DialOptions{Subprotocols: []string{realtime.Subprotocol}}
	if token != "" {
		opts.HTTPHeader = http.Header{"Authorization": {"Bearer " + token}}
	}
	return r.dialWith(r.baseURL, query, opts)
}

func (r *rig) dialWith(baseURL, query string, opts *websocket.DialOptions) *websocket.Conn {
	r.t.Helper()
	ctx, cancel := context.WithTimeout(r.t.Context(), 5*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, strings.Replace(baseURL, "http", "ws", 1)+"/api/v1/ws"+query, opts)
	require.NoError(r.t, err)
	r.t.Cleanup(func() { _ = conn.CloseNow() })
	return conn
}

// frame is an envelope as a client reads it.
type frame struct {
	Type  string          `json:"type"`
	Room  string          `json:"room"`
	MsgID string          `json:"msg_id"`
	TS    time.Time       `json:"ts"`
	Data  json.RawMessage `json:"data"`
}

func (f frame) into(t *testing.T, v any) {
	t.Helper()
	require.NoError(t, json.Unmarshal(f.Data, v))
}

func read(t *testing.T, conn *websocket.Conn) frame {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	_, raw, err := conn.Read(ctx)
	require.NoError(t, err)
	var f frame
	require.NoError(t, json.Unmarshal(raw, &f), string(raw))
	return f
}

// readClose waits for the server to close the socket and returns the close code.
func readClose(t *testing.T, conn *websocket.Conn) websocket.StatusCode {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	for {
		_, _, err := conn.Read(ctx)
		if err == nil {
			continue // a frame; the close follows
		}
		code := websocket.CloseStatus(err)
		require.NotEqual(t, websocket.StatusCode(-1), code, "the socket ended without a close frame: %v", err)
		return code
	}
}

// expectNothingBefore proves that no frame arrived on conn before a sentinel: it publishes a
// sentinel to a room the connection is in and requires it to be the next frame. Redis keeps the
// order of what one connection receives, so anything that was on its way would come first. (A
// read with a timeout is not an option: coder/websocket closes a connection whose read times
// out.)
func (r *rig) expectNothingBefore(org int64, room string, conn *websocket.Conn) {
	r.t.Helper()
	r.publish(org, room, "sentinel", map[string]any{})
	require.Equal(r.t, "sentinel", read(r.t, conn).Type, "a frame arrived that should not have")
}

// connect dials with a token and swallows the hello.
func (r *rig) connect(token string) *websocket.Conn {
	r.t.Helper()
	conn := r.dial(token, "")
	require.Equal(r.t, "hello", read(r.t, conn).Type)
	return conn
}

type ack struct {
	Joined []string `json:"joined"`
	Denied []struct {
		Room   string `json:"room"`
		Reason string `json:"reason"`
	} `json:"denied"`
}

// subscribe asks for rooms and returns the server's answer.
func subscribe(t *testing.T, conn *websocket.Conn, rooms ...string) ack {
	t.Helper()
	send(t, conn, map[string]any{"type": "subscribe", "data": map[string]any{"rooms": rooms}})
	f := read(t, conn)
	require.Equal(t, "subscribe.ack", f.Type)
	var a ack
	f.into(t, &a)
	return a
}

func send(t *testing.T, conn *websocket.Conn, v any) {
	t.Helper()
	raw, err := json.Marshal(v)
	require.NoError(t, err)
	require.NoError(t, conn.Write(t.Context(), websocket.MessageText, raw))
}

func (r *rig) publish(org int64, room, typ string, data any) {
	r.t.Helper()
	require.NoError(r.t, r.publisher.Publish(r.t.Context(), org, room, typ, data))
}

func TestAConnectionWithoutAValidTokenIsClosedWith4401(t *testing.T) {
	r := newRig(t, realtime.Config{})
	token, _ := r.person(r.orgA)

	for name, tok := range map[string]string{
		"no token":    "",
		"garbage":     "not-a-token",
		"bad signing": token[:len(token)-3] + "abc",
	} {
		conn := r.dial(tok, "")
		require.Equal(t, realtime.CloseUnauthorized, readClose(t, conn), name)
	}
}

func TestHelloCarriesTheServerTimeAndTheSubprotocol(t *testing.T) {
	r := newRig(t, realtime.Config{})
	token, _ := r.person(r.orgA)

	conn := r.dial(token, "")
	require.Equal(t, realtime.Subprotocol, conn.Subprotocol())
	hello := read(t, conn)

	require.Equal(t, "hello", hello.Type)
	require.NotEmpty(t, hello.MsgID)
	var data struct {
		ServerTime time.Time `json:"server_time"`
	}
	hello.into(t, &data)
	require.WithinDuration(t, time.Now(), data.ServerTime, 5*time.Second)
}

func TestATicketOpensOneConnectionOnly(t *testing.T) {
	r := newRig(t, realtime.Config{})
	token, id := r.person(r.orgA)

	ticket := func() string {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, r.baseURL+"/api/v1/ws/ticket", nil)
		require.NoError(t, err)
		req.Header.Set("Authorization", "Bearer "+token)
		res, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		defer func() { _ = res.Body.Close() }()
		require.Equal(t, http.StatusOK, res.StatusCode)
		var body struct {
			Data struct {
				Ticket    string `json:"ticket"`
				ExpiresIn int    `json:"expires_in"`
			} `json:"data"`
		}
		require.NoError(t, json.NewDecoder(res.Body).Decode(&body))
		require.Equal(t, int(realtime.TicketTTL/time.Second), body.Data.ExpiresIn)
		require.NotEmpty(t, body.Data.Ticket)
		return body.Data.Ticket
	}

	t.Run("the ticket works once, with no Authorization header", func(t *testing.T) {
		tk := ticket()
		conn := r.dial("", "?ticket="+tk)
		require.Equal(t, "hello", read(t, conn).Type)
		// It is the same person: their own room is theirs.
		require.Len(t, subscribe(t, conn, realtime.PersonalRoom(id)).Joined, 1)

		again := r.dial("", "?ticket="+tk)
		require.Equal(t, realtime.CloseUnauthorized, readClose(t, again), "a used ticket is rejected")
	})
	t.Run("an expired ticket is rejected", func(t *testing.T) {
		tk := ticket()
		require.NoError(t, r.rdb.Del(t.Context(), r.prefix+"ws-ticket:"+tk).Err()) // what the TTL does
		conn := r.dial("", "?ticket="+tk)
		require.Equal(t, realtime.CloseUnauthorized, readClose(t, conn))
	})
	t.Run("an invented ticket is rejected", func(t *testing.T) {
		conn := r.dial("", "?ticket=made-up")
		require.Equal(t, realtime.CloseUnauthorized, readClose(t, conn))
	})
	t.Run("a ticket needs an access token to be issued", func(t *testing.T) {
		res, err := http.Post(r.baseURL+"/api/v1/ws/ticket", "application/json", nil) //nolint:noctx // a one-line probe
		require.NoError(t, err)
		_ = res.Body.Close()
		require.Equal(t, http.StatusUnauthorized, res.StatusCode)
	})
}

func TestARoomOfAMembersTeamIsJoinedAndOtherRoomsAreDenied(t *testing.T) {
	r := newRig(t, realtime.Config{})
	memberToken, member := r.person(r.orgA)
	r.member(r.orgA, team1, member, "MEMBER")
	conn := r.connect(memberToken)

	a := subscribe(t, conn, "team."+team1+".panic", "team."+team2+".panic", "team.nope.panic", "team."+team1+".bogus", "project.1K-IBMI-S.panic")
	require.Equal(t, []string{"team." + team1 + ".panic"}, a.Joined)
	reasons := map[string]string{}
	for _, d := range a.Denied {
		reasons[d.Room] = d.Reason
	}
	require.Equal(t, map[string]string{
		"team." + team2 + ".panic": realtime.DeniedForbidden,
		"team.nope.panic":          realtime.DeniedForbidden,
		"team." + team1 + ".bogus": realtime.DeniedInvalidRoom,
		"project.1K-IBMI-S.panic":  realtime.DeniedForbidden,
	}, reasons)

	r.publish(r.orgA, "team."+team1+".panic", "panic.alert", map[string]any{"panic_id": 9001})
	f := read(t, conn)
	require.Equal(t, "panic.alert", f.Type)
	require.Equal(t, "team."+team1+".panic", f.Room)
	require.NotEmpty(t, f.MsgID)
	require.False(t, f.TS.IsZero())
}

func TestTrackingIsForTheDanruOfTheTeam(t *testing.T) {
	r := newRig(t, realtime.Config{})
	danruToken, danru := r.person(r.orgA)
	memberToken, member := r.person(r.orgA)
	r.member(r.orgA, team1, danru, "DANRU")
	r.member(r.orgA, team1, member, "MEMBER")
	room := "team." + team1 + ".tracking"

	require.Equal(t, []string{room}, subscribe(t, r.connect(danruToken), room).Joined, "a DANRU may follow their regu")

	denied := subscribe(t, r.connect(memberToken), room)
	require.Empty(t, denied.Joined, "an ordinary member only receives panics, not everyone's position")
	require.Equal(t, realtime.DeniedForbidden, denied.Denied[0].Reason)

	// The default DANRU grant is PROJECT-wide, so the sister regu of the same project is covered;
	// a regu of another project is not.
	require.Len(t, subscribe(t, r.connect(danruToken), "team."+team2+".tracking").Joined, 1)
	other := subscribe(t, r.connect(danruToken), "team.2K-QQQQ-S1.tracking")
	require.Empty(t, other.Joined, "but not a regu of another project")
}

func TestProjectRoomsAreForAdminsWhoseScopeCoversTheProject(t *testing.T) {
	r := newRig(t, realtime.Config{})
	orgAdminToken, orgAdmin := r.person(r.orgA)
	regionToken, regionAdmin := r.person(r.orgA)
	otherRegionToken, otherRegionAdmin := r.person(r.orgA)
	fieldToken, field := r.person(r.orgA)
	r.admin(r.orgA, orgAdmin, "ORG_ADMIN", "")
	r.admin(r.orgA, regionAdmin, "REGION_ADMIN", "OS1")
	r.admin(r.orgA, otherRegionAdmin, "REGION_ADMIN", "OS2")
	r.member(r.orgA, team1, field, "DANRU")
	rooms := []string{"project.1K-IBMI-S.panic", "project.1K-IBMI-S.tracking"}

	require.Equal(t, rooms, subscribe(t, r.connect(orgAdminToken), rooms...).Joined)
	require.Equal(t, rooms, subscribe(t, r.connect(regionToken), rooms...).Joined)
	require.Empty(t, subscribe(t, r.connect(otherRegionToken), rooms...).Joined, "a REGION_ADMIN of another region")
	require.Empty(t, subscribe(t, r.connect(fieldToken), rooms...).Joined, "field staff, even a DANRU, are not web admins")
	require.Empty(t, subscribe(t, r.connect(orgAdminToken), "project.1K-NOPE-S.panic").Joined, "a project that does not exist")
}

// ⚠ Two organizations with the same team code: an event of one never reaches the other.
func TestAnEventOfOneOrganizationNeverReachesAnother(t *testing.T) {
	r := newRig(t, realtime.Config{})
	tokenA, a := r.person(r.orgA)
	tokenB, b := r.person(r.orgB)
	r.member(r.orgA, team1, a, "MEMBER")
	r.member(r.orgB, team1, b, "MEMBER") // the very same code: 1K-IBMI-S1
	room := "team." + team1 + ".panic"
	connA, connB := r.connect(tokenA), r.connect(tokenB)
	require.Len(t, subscribe(t, connA, room).Joined, 1)
	require.Len(t, subscribe(t, connB, room).Joined, 1)

	r.publish(r.orgA, room, "panic.alert", map[string]any{"panic_id": 1})

	require.Equal(t, "panic.alert", read(t, connA).Type)
	r.expectNothingBefore(r.orgB, room, connB)

	r.publish(r.orgB, room, "panic.alert", map[string]any{"panic_id": 2})
	require.Equal(t, "panic.alert", read(t, connB).Type)
	r.expectNothingBefore(r.orgA, room, connA)
}

func TestAnEventPublishedOnOneInstanceIsDeliveredOnTheOther(t *testing.T) {
	r := newRig(t, realtime.Config{})
	tokenA, a := r.person(r.orgA)
	tokenB, b := r.person(r.orgA)
	r.member(r.orgA, team1, a, "MEMBER")
	r.member(r.orgA, team1, b, "MEMBER")
	room := "team." + team1 + ".panic"

	secondURL, secondHub := r.server(realtime.Config{})
	require.NotSame(t, r.hub, secondHub)
	connA := r.connect(tokenA) // instance 1
	connB := r.dialWith(secondURL, "", &websocket.DialOptions{
		Subprotocols: []string{realtime.Subprotocol}, HTTPHeader: http.Header{"Authorization": {"Bearer " + tokenB}},
	}) // instance 2
	require.Equal(t, "hello", read(t, connB).Type)
	require.Len(t, subscribe(t, connA, room).Joined, 1)
	require.Len(t, subscribe(t, connB, room).Joined, 1)

	r.publish(r.orgA, room, "panic.alert", map[string]any{"panic_id": 7})

	require.Equal(t, "panic.alert", read(t, connA).Type)
	require.Equal(t, "panic.alert", read(t, connB).Type)
}

func TestAMemberReceivesNotificationCreatedInTheirPersonalRoom(t *testing.T) {
	r := newRig(t, realtime.Config{})
	token, me := r.person(r.orgA)
	_, other := r.person(r.orgA)
	_, actor := r.person(r.orgA)
	conn := r.connect(token)

	a := subscribe(t, conn, realtime.PersonalRoom(me), realtime.PersonalRoom(other))
	require.Equal(t, []string{realtime.PersonalRoom(me)}, a.Joined)
	require.Equal(t, realtime.DeniedForbidden, a.Denied[0].Reason, "somebody else's personal room")

	service := notifications.NewService(testkit.Pool(t), clock.System{}, quietLogger())
	service.UseDispatcher(realtime.NewNotificationDispatcher(r.publisher))
	service.Notify(auth.WithPrincipal(t.Context(), auth.Principal{OrganizationID: r.orgA, PersonnelID: actor}), notifications.Input{
		Type: notifications.TypeInstructionReceived, RecipientIDs: []int64{me, other},
		Vars:   notifications.Vars{ActorName: "Budi Santoso", Subject: "Siaga"},
		Entity: &notifications.Entity{Type: "instruction", ID: 42},
	})

	f := read(t, conn)
	require.Equal(t, "notification.created", f.Type)
	require.Equal(t, realtime.PersonalRoom(me), f.Room)
	var data map[string]any
	f.into(t, &data)
	require.NotZero(t, data["notification_id"])
	require.Equal(t, "INSTRUCTION_RECEIVED", data["type"])
	require.Equal(t, "NORMAL", data["priority"])
	require.Equal(t, "instruction", data["entity_type"])
	require.EqualValues(t, 42, data["entity_id"])
	require.NotContains(t, data, "title")
	require.NotContains(t, data, "body")
	require.NotContains(t, string(f.Data), "Budi", "no name goes through a socket")
	r.expectNothingBefore(r.orgA, realtime.PersonalRoom(me), conn) // one frame per notification
}

func TestMalformedFramesGetAnErrorAndTheConnectionStays(t *testing.T) {
	r := newRig(t, realtime.Config{})
	token, _ := r.person(r.orgA)
	conn := r.connect(token)

	for _, bad := range []string{`not json`, `{"data":{}}`, `{"type":"fly"}`, `{"type":"subscribe","data":{"rooms":[]}}`} {
		require.NoError(t, conn.Write(t.Context(), websocket.MessageText, []byte(bad)))
		f := read(t, conn)
		require.Equal(t, "error", f.Type, bad)
		var data struct {
			Code int `json:"code"`
		}
		f.into(t, &data)
		require.Equal(t, 4400, data.Code)
	}
	send(t, conn, map[string]any{"type": "ping"})
	require.Equal(t, "pong", read(t, conn).Type, "the connection is still alive")
}

func TestUnsubscribeStopsDelivery(t *testing.T) {
	r := newRig(t, realtime.Config{})
	token, me := r.person(r.orgA)
	r.member(r.orgA, team1, me, "MEMBER")
	conn := r.connect(token)
	personal, teamRoom := realtime.PersonalRoom(me), "team."+team1+".panic"
	require.Len(t, subscribe(t, conn, personal, teamRoom).Joined, 2)

	send(t, conn, map[string]any{"type": "unsubscribe", "data": map[string]any{"rooms": []string{personal}}})
	send(t, conn, map[string]any{"type": "ping"})
	require.Equal(t, "pong", read(t, conn).Type) // the unsubscribe was read before the ping

	r.publish(r.orgA, personal, "notification.created", map[string]any{})
	r.expectNothingBefore(r.orgA, teamRoom, conn)
}

func TestAClientThatNeverAnswersPingsIsClosedWith4408(t *testing.T) {
	r := newRig(t, realtime.Config{PingInterval: 50 * time.Millisecond, IdleTimeout: 250 * time.Millisecond})
	token, _ := r.person(r.orgA)
	// This client never reads, so it never answers a ping.
	conn := r.dial(token, "")

	time.Sleep(700 * time.Millisecond)
	require.Equal(t, realtime.CloseIdle, readClose(t, conn))
}

func TestAClientThatAnswersPingsIsKept(t *testing.T) {
	r := newRig(t, realtime.Config{PingInterval: 50 * time.Millisecond, IdleTimeout: 250 * time.Millisecond})
	token, _ := r.person(r.orgA)
	conn := r.connect(token)

	// Reading is what answers pings; keep reading through several idle timeouts.
	ctx, cancel := context.WithTimeout(t.Context(), 900*time.Millisecond)
	defer cancel()
	_, _, err := conn.Read(ctx)
	require.ErrorIs(t, err, context.DeadlineExceeded, "no close frame arrived in that time")
}

func TestTheSocketClosesWith4401WhenTheTokenRunsOut(t *testing.T) {
	r := newRig(t, realtime.Config{})
	_, me := r.person(r.orgA)
	// A ticket remembers the expiry of the access token that asked for it.
	ticket, err := r.tickets.Issue(t.Context(), auth.Principal{OrganizationID: r.orgA, PersonnelID: me, SessionID: 1}, time.Now().Add(400*time.Millisecond))
	require.NoError(t, err)

	conn := r.dial("", "?ticket="+ticket)
	require.Equal(t, "hello", read(t, conn).Type)
	require.Equal(t, realtime.CloseUnauthorized, readClose(t, conn))
}

func TestTheSocketClosesWith4401WhenTheSessionEnds(t *testing.T) {
	r := newRig(t, realtime.Config{PingInterval: 50 * time.Millisecond})
	_, me := r.person(r.orgA)
	token, _, err := r.tokens.NewAccessToken(me, r.orgA, 77)
	require.NoError(t, err)
	conn := r.connect(token)

	r.revocations.Revoke(t.Context(), 77)

	require.Equal(t, realtime.CloseUnauthorized, readClose(t, conn))
}

func TestAnAlreadyRevokedSessionCannotConnect(t *testing.T) {
	r := newRig(t, realtime.Config{})
	_, me := r.person(r.orgA)
	token, _, err := r.tokens.NewAccessToken(me, r.orgA, 78)
	require.NoError(t, err)
	r.revocations.Revoke(t.Context(), 78)

	require.Equal(t, realtime.CloseUnauthorized, readClose(t, r.dial(token, "")))
}

func TestPublishRefusesANameThatIsNotARoom(t *testing.T) {
	r := newRig(t, realtime.Config{})
	err := r.publisher.Publish(t.Context(), r.orgA, "everyone", "x", nil)
	require.Error(t, err)
	require.False(t, bytes.Contains([]byte(err.Error()), []byte("rt:")))
}
