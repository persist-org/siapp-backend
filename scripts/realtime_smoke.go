//go:build ignore

// Command realtime_smoke walks through the Phase 05.3 flow against a RUNNING API, the way you
// would by hand in Postman: log in, get a ticket, open the WebSocket, subscribe, move a person
// between teams and watch the notification arrive, then post signed Personel events.
//
//	cd ~/code/syap/siapp
//	set -a; . ./.env; set +a          # loads PERSONEL_WEBHOOK_SECRET and GROUP_ORGANIZATION_ID
//	export ADMIN_EMAIL=... ADMIN_PASSWORD=... PERSON_EMAIL=... PERSON_PASSWORD=... PERSON_NIK=...
//	export TEAM_FROM=1K-IBMI-S1 TEAM_TO=1K-IBMI-S2      # optional: enables the team-move step
//	go run scripts/realtime_smoke.go
//
// See the variable list in the usage text below (run it with no variables to print it).
// It prints a checklist and exits 1 if any check fails.
package main

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	_ "time/tzdata"

	"github.com/coder/websocket"
)

const usage = `realtime_smoke: smoke test of Phase 05.3 against a running API.

REQUIRED variables
  ADMIN_EMAIL, ADMIN_PASSWORD        a web admin (has a web role). Logs in with client=WEB.
  PERSON_EMAIL, PERSON_PASSWORD      an ordinary field person. Logs in with client=MOBILE.
  PERSON_NIK                         that person's full NIK (the webhook identifies people by NIK).
  PERSONEL_WEBHOOK_SECRET            the same value as the API's .env.

OPTIONAL variables
  BASE_URL             default http://localhost:8080
  GROUP_ORGANIZATION_ID default 1 (the organization id the webhook events name)
  PERSON_DEVICE_TOKEN  default "smoke-test-phone". The MOBILE login binds this "phone" to the
                       person; keep it the same between runs, or reset the binding as an admin.
  TEAM_FROM, TEAM_TO   team codes. The person must be a CURRENT member of TEAM_FROM and the admin's
                       scope must cover both. Setting both turns on the team-move step, which
                       CHANGES DATA: the person ends up in TEAM_TO. Leave them out to skip it.
  WEBHOOK_DEACTIVATE=1 also send employee.deactivated for PERSON_NIK. DESTRUCTIVE: the person
                       becomes INACTIVE, their sessions end and their memberships end today.
`

type smoke struct {
	base   string
	secret string
	org    int64
	http   *http.Client
	checks int
	failed int
}

func main() {
	s := &smoke{
		base:   strings.TrimRight(envOr("BASE_URL", "http://localhost:8080"), "/"),
		secret: os.Getenv("PERSONEL_WEBHOOK_SECRET"),
		http:   &http.Client{Timeout: 15 * time.Second},
	}
	org, err := strconv.ParseInt(envOr("GROUP_ORGANIZATION_ID", "1"), 10, 64)
	if err != nil {
		fatal("GROUP_ORGANIZATION_ID must be a number")
	}
	s.org = org
	var missing []string
	for _, k := range []string{"ADMIN_EMAIL", "ADMIN_PASSWORD", "PERSON_EMAIL", "PERSON_PASSWORD", "PERSON_NIK", "PERSONEL_WEBHOOK_SECRET"} {
		if os.Getenv(k) == "" {
			missing = append(missing, k)
		}
	}
	if len(missing) > 0 {
		fmt.Print(usage)
		fatal("missing: " + strings.Join(missing, ", "))
	}
	s.run()
	fmt.Printf("\n%d checks, %d failed\n", s.checks, s.failed)
	if s.failed > 0 {
		os.Exit(1)
	}
}

func (s *smoke) run() {
	nik := os.Getenv("PERSON_NIK")

	step("1. Log in")
	adminToken := s.login(os.Getenv("ADMIN_EMAIL"), os.Getenv("ADMIN_PASSWORD"), "WEB", "")
	personToken := s.login(os.Getenv("PERSON_EMAIL"), os.Getenv("PERSON_PASSWORD"), "MOBILE", envOr("PERSON_DEVICE_TOKEN", "smoke-test-phone"))
	if adminToken == "" || personToken == "" {
		fatal("cannot continue without both tokens")
	}
	personID := jwtSubject(personToken)
	s.check("the person's id is in their token", personID > 0, "personnel id "+strconv.FormatInt(personID, 10))

	step("2. Ticket")
	status, body := s.call("POST", "/api/v1/ws/ticket", "", nil, nil)
	s.check("a ticket needs an access token", status == 401, fmt.Sprint(status))
	status, body = s.call("POST", "/api/v1/ws/ticket", personToken, nil, nil)
	ticket := find(body, "ticket")
	s.check("POST /ws/ticket returns a ticket", status == 200 && ticket != "", fmt.Sprintf("status %d, expires_in %s", status, find(body, "expires_in")))

	step("3. WebSocket")
	ws := s.dial("?ticket="+ticket, "")
	hello, ok := ws.next(5 * time.Second)
	s.check("a ticket opens the socket and the server says hello", ok && hello.Type == "hello", hello.Type)
	reused := s.dial("?ticket="+ticket, "")
	s.check("the same ticket a second time is closed with 4401", reused.closeCode(5*time.Second) == 4401, "")
	bad := s.dial("", "not-a-token")
	s.check("a bad token is closed with 4401", bad.closeCode(5*time.Second) == 4401, "")
	byToken := s.dial("", personToken)
	h2, ok := byToken.next(5 * time.Second)
	s.check("a Bearer token opens the socket too (mobile style)", ok && h2.Type == "hello", h2.Type)
	byToken.close()

	step("4. Subscribe")
	own := fmt.Sprintf("personnel.%d.notifications", personID)
	foreign := fmt.Sprintf("personnel.%d.notifications", personID+1)
	ws.send(map[string]any{"type": "subscribe", "data": map[string]any{"rooms": []string{own, foreign, "team.NOPE-S1.panic", "team.x.bogus"}}})
	ack, ok := ws.next(5 * time.Second)
	var a struct {
		Joined []string `json:"joined"`
		Denied []struct{ Room, Reason string }
	}
	_ = json.Unmarshal(ack.Data, &a)
	s.check("subscribe.ack arrives", ok && ack.Type == "subscribe.ack", ack.Type)
	s.check("the person's own room is joined", len(a.Joined) == 1 && a.Joined[0] == own, fmt.Sprint(a.Joined))
	reasons := map[string]string{}
	for _, d := range a.Denied {
		reasons[d.Room] = d.Reason
	}
	s.check("someone else's personal room is FORBIDDEN", reasons[foreign] == "FORBIDDEN", reasons[foreign])
	s.check("a team the person is not in is FORBIDDEN", reasons["team.NOPE-S1.panic"] == "FORBIDDEN", reasons["team.NOPE-S1.panic"])
	s.check("an invalid room name is INVALID_ROOM", reasons["team.x.bogus"] == "INVALID_ROOM", reasons["team.x.bogus"])
	ws.send(map[string]any{"type": "ping"})
	pong, ok := ws.next(5 * time.Second)
	s.check("ping gets pong", ok && pong.Type == "pong", pong.Type)

	step("5. Team move -> TEAM_CHANGED")
	from, to := os.Getenv("TEAM_FROM"), os.Getenv("TEAM_TO")
	if from == "" || to == "" {
		skip("TEAM_FROM and TEAM_TO are not set")
	} else {
		s.teamMove(adminToken, personToken, personID, from, to, ws)
	}

	step("6. Personel webhook")
	s.webhook(nik, personToken, ws)

	ws.close()
}

// teamMove moves the person and waits for notification.created on their socket.
func (s *smoke) teamMove(adminToken, personToken string, personID int64, from, to string, ws *wsConn) {
	// The members list answers "who is in the team on a date" (today by default), so a membership
	// that starts tomorrow, which is what a previous run of this script leaves behind, is not in
	// today's list. Ask for the next few days too and take the open membership (no end date).
	var membership int64
	var start time.Time
	status := 0
	var body []byte
	for ahead := 0; ahead <= 7 && membership == 0; ahead++ {
		on := time.Now().AddDate(0, 0, ahead).Format("2006-01-02")
		status, body = s.call("GET", "/api/v1/admin/teams/"+from+"/members?on_date="+on, adminToken, nil, nil)
		var list struct {
			Data []struct {
				ID          int64   `json:"id"`
				PersonnelID int64   `json:"personnel_id"`
				StartDate   string  `json:"start_date"`
				EndDate     *string `json:"end_date"`
			} `json:"data"`
		}
		_ = json.Unmarshal(body, &list)
		for _, m := range list.Data {
			if m.PersonnelID == personID && m.EndDate == nil {
				membership = m.ID
				start, _ = time.Parse("2006-01-02", m.StartDate)
			}
		}
	}
	s.check("the person has an open membership in "+from, status == 200 && membership > 0,
		fmt.Sprintf("list status %d, membership id %d (if 0: the person is not in %s, try swapping TEAM_FROM and TEAM_TO)", status, membership, from))
	if membership == 0 {
		return
	}

	// The effective date must be after the membership's start date.
	jakarta, _ := time.LoadLocation("Asia/Jakarta")
	effective := time.Now().In(jakarta)
	if !start.IsZero() && !effective.After(start.AddDate(0, 0, 1)) {
		effective = start.AddDate(0, 0, 1)
	}
	status, body = s.call("POST", fmt.Sprintf("/api/v1/admin/team-members/%d/move", membership), adminToken,
		map[string]any{"to_team_code": to, "effective_date": effective.Format("2006-01-02")}, nil)
	s.check("the admin moves the person", status == 200, fmt.Sprintf("status %d %s", status, trim(body)))
	if status != 200 {
		return
	}

	f, ok := ws.until(5*time.Second, "notification.created")
	var data map[string]any
	_ = json.Unmarshal(f.Data, &data)
	s.check("notification.created arrives on the socket", ok, "")
	s.check("it is a TEAM_CHANGED with ids only", data["type"] == "TEAM_CHANGED" && data["title"] == nil && data["body"] == nil, fmt.Sprint(data))

	items := s.inbox(personToken)
	s.check("the inbox has the TEAM_CHANGED row naming "+to, anyItem(items, "TEAM_CHANGED", to), "")
}

// webhook posts signed events and checks each answer.
func (s *smoke) webhook(nik, personToken string, ws *wsConn) {
	position := "Smoke " + randomHex(3)
	id := "smoke-" + randomHex(8)
	data := map[string]any{"nik": nik, "position_name": position}

	code, res := s.event("position.changed", data, id, time.Now(), "")
	s.check("a signed position.changed is processed", code == 200 && res == "processed", fmt.Sprintf("%d %s", code, res))
	f, ok := ws.until(5*time.Second, "notification.created")
	var pushed map[string]any
	_ = json.Unmarshal(f.Data, &pushed)
	s.check("the person's open socket gets notification.created for it", ok && pushed["type"] == "POSITION_CHANGED", fmt.Sprint(pushed))

	code, res = s.event("position.changed", data, id, time.Now(), "")
	s.check("the same event_id again is a duplicate", code == 200 && res == "duplicate", fmt.Sprintf("%d %s", code, res))
	code, res = s.event("position.changed", data, "smoke-"+randomHex(8), time.Now(), "00")
	s.check("a wrong signature is 401 WEBHOOK_SIGNATURE_INVALID", code == 401 && res == "WEBHOOK_SIGNATURE_INVALID", fmt.Sprintf("%d %s", code, res))
	code, res = s.event("position.changed", data, "smoke-"+randomHex(8), time.Now().Add(-10*time.Minute), "")
	s.check("a 10-minute-old event is 401", code == 401 && res == "WEBHOOK_SIGNATURE_INVALID", fmt.Sprintf("%d %s", code, res))
	code, res = s.event("leave.decided", map[string]any{}, "smoke-"+randomHex(8), time.Now(), "")
	s.check("leave.decided is acknowledged and ignored", code == 200 && res == "ignored", fmt.Sprintf("%d %s", code, res))
	code, res = s.event("placement.changed", map[string]any{"project_id": 1}, "smoke-"+randomHex(8), time.Now(), "")
	s.check("placement.changed is processed", code == 200 && res == "processed", fmt.Sprintf("%d %s", code, res))

	items := s.inbox(personToken)
	n := 0
	for _, it := range items {
		if it.Type == "POSITION_CHANGED" && strings.Contains(it.Body, position) {
			n++
		}
	}
	s.check("the inbox has exactly ONE POSITION_CHANGED for the replayed event", n == 1, fmt.Sprint(n, " rows"))

	if os.Getenv("WEBHOOK_DEACTIVATE") == "1" {
		code, res = s.event("employee.deactivated", map[string]any{"nik": nik}, "smoke-"+randomHex(8), time.Now(), "")
		s.check("employee.deactivated is processed", code == 200 && res == "processed", fmt.Sprintf("%d %s", code, res))
		status, _ := s.call("GET", "/api/v1/notifications", personToken, nil, nil)
		s.check("the deactivated person's token stops working at once", status == 401, fmt.Sprint(status))
	} else {
		skip("WEBHOOK_DEACTIVATE=1 is not set")
	}
}

// ---- HTTP ------------------------------------------------------------------------------

func (s *smoke) call(method, path, token string, body any, headers map[string]string) (int, []byte) {
	var rd io.Reader
	switch b := body.(type) {
	case nil:
	case []byte:
		rd = bytes.NewReader(b)
	default:
		raw, err := json.Marshal(b)
		if err != nil {
			fatal(err.Error())
		}
		rd = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, s.base+path, rd)
	if err != nil {
		fatal(err.Error())
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	res, err := s.http.Do(req)
	if err != nil {
		fatal(fmt.Sprintf("%s %s: %v (is the API running at %s?)", method, path, err, s.base))
	}
	defer func() { _ = res.Body.Close() }()
	out, _ := io.ReadAll(res.Body)
	return res.StatusCode, out
}

func (s *smoke) login(email, password, client, device string) string {
	payload := map[string]any{"identifier": email, "password": password, "client": client}
	if device != "" {
		payload["device_token"] = device
	}
	status, body := s.call("POST", "/api/v1/auth/login", "", payload, nil)
	token := find(body, "access_token")
	s.check(fmt.Sprintf("%s logs in as %s", email, client), status == 200 && token != "", fmt.Sprintf("status %d %s", status, trimIf(status != 200, body)))
	return token
}

type inboxItem struct {
	Type string `json:"type"`
	Body string `json:"body"`
}

func (s *smoke) inbox(token string) []inboxItem {
	_, body := s.call("GET", "/api/v1/notifications", token, nil, nil)
	var out struct {
		Data []inboxItem `json:"data"`
	}
	_ = json.Unmarshal(body, &out)
	return out.Data
}

func anyItem(items []inboxItem, typ, bodyPart string) bool {
	for _, it := range items {
		if it.Type == typ && strings.Contains(it.Body, bodyPart) {
			return true
		}
	}
	return false
}

// event posts a Personel event signed with the webhook secret. sigOverride replaces the
// signature. It returns the status code and the result status (or the error reason).
func (s *smoke) event(typ string, data map[string]any, eventID string, at time.Time, sigOverride string) (int, string) {
	raw, _ := json.Marshal(map[string]any{
		"event_id": eventID, "type": typ, "organization_id": s.org,
		"timestamp": at.UTC().Format(time.RFC3339), "data": data,
	})
	mac := hmac.New(sha256.New, []byte(s.secret))
	mac.Write(raw)
	sig := hex.EncodeToString(mac.Sum(nil))
	if sigOverride != "" {
		sig = sigOverride
	}
	status, body := s.call("POST", "/api/v1/integrations/personel/events", "", raw, map[string]string{"X-Personel-Signature": sig})
	if status == 200 {
		return status, find(body, "status")
	}
	var e struct {
		Error struct {
			Details map[string]any `json:"details"`
		} `json:"error"`
	}
	_ = json.Unmarshal(body, &e)
	reason, _ := e.Error.Details["reason"].(string)
	return status, reason
}

// ---- WebSocket -------------------------------------------------------------------------

type frame struct {
	Type string          `json:"type"`
	Room string          `json:"room"`
	Data json.RawMessage `json:"data"`
}

// wsConn reads frames in a goroutine, so waiting for one with a timeout never closes the socket
// (coder/websocket closes a connection whose read context expires).
type wsConn struct {
	c      *websocket.Conn
	frames chan frame
	closed chan websocket.StatusCode
}

func (s *smoke) dial(query, bearer string) *wsConn {
	opts := &websocket.DialOptions{Subprotocols: []string{"persist.v1"}}
	if bearer != "" {
		opts.HTTPHeader = http.Header{"Authorization": {"Bearer " + bearer}}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, _, err := websocket.Dial(ctx, strings.Replace(s.base, "http", "ws", 1)+"/api/v1/ws"+query, opts)
	if err != nil {
		fatal("websocket dial: " + err.Error())
	}
	w := &wsConn{c: c, frames: make(chan frame, 64), closed: make(chan websocket.StatusCode, 1)}
	go func() {
		for {
			_, raw, err := c.Read(context.Background())
			if err != nil {
				w.closed <- websocket.CloseStatus(err)
				return
			}
			var f frame
			if json.Unmarshal(raw, &f) == nil {
				w.frames <- f
			}
		}
	}()
	return w
}

func (w *wsConn) send(v any) {
	raw, _ := json.Marshal(v)
	if err := w.c.Write(context.Background(), websocket.MessageText, raw); err != nil {
		fatal("websocket write: " + err.Error())
	}
}

func (w *wsConn) next(timeout time.Duration) (frame, bool) {
	select {
	case f := <-w.frames:
		return f, true
	case <-time.After(timeout):
		return frame{}, false
	}
}

// until skips frames until one of the type arrives.
func (w *wsConn) until(timeout time.Duration, typ string) (frame, bool) {
	deadline := time.Now().Add(timeout)
	for {
		f, ok := w.next(time.Until(deadline))
		if !ok {
			return frame{}, false
		}
		if f.Type == typ {
			return f, true
		}
	}
}

func (w *wsConn) closeCode(timeout time.Duration) websocket.StatusCode {
	select {
	case code := <-w.closed:
		return code
	case <-time.After(timeout):
		return -1
	}
}

func (w *wsConn) close() { _ = w.c.Close(websocket.StatusNormalClosure, "bye") }

// ---- helpers ---------------------------------------------------------------------------

func (s *smoke) check(name string, ok bool, detail string) {
	s.checks++
	mark := "PASS"
	if !ok {
		mark = "FAIL"
		s.failed++
	}
	if detail != "" && (!ok || len(detail) < 120) {
		fmt.Printf("  [%s] %s  (%s)\n", mark, name, detail)
		return
	}
	fmt.Printf("  [%s] %s\n", mark, name)
}

func step(name string) { fmt.Printf("\n== %s\n", name) }
func skip(why string)  { fmt.Printf("  [SKIP] %s\n", why) }

func fatal(msg string) {
	fmt.Fprintln(os.Stderr, "error:", msg)
	os.Exit(1)
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func trim(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > 200 {
		s = s[:200] + "..."
	}
	return s
}

func trimIf(cond bool, b []byte) string {
	if cond {
		return trim(b)
	}
	return ""
}

// find returns the first string (or number) value stored under key anywhere in a JSON document.
func find(raw []byte, key string) string {
	var v any
	if json.Unmarshal(raw, &v) != nil {
		return ""
	}
	var walk func(any) string
	walk = func(n any) string {
		switch t := n.(type) {
		case map[string]any:
			if x, ok := t[key]; ok {
				switch y := x.(type) {
				case string:
					return y
				case float64:
					return strconv.FormatFloat(y, 'f', -1, 64)
				}
			}
			for _, c := range t {
				if r := walk(c); r != "" {
					return r
				}
			}
		case []any:
			for _, c := range t {
				if r := walk(c); r != "" {
					return r
				}
			}
		}
		return ""
	}
	return walk(v)
}

// jwtSubject reads the sub claim (the personnel id) of an access token. It does not verify the
// token: the API did when it issued it.
func jwtSubject(token string) int64 {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return 0
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return 0
	}
	var claims struct {
		Sub string `json:"sub"`
	}
	if json.Unmarshal(raw, &claims) != nil {
		return 0
	}
	id, _ := strconv.ParseInt(claims.Sub, 10, 64)
	return id
}
