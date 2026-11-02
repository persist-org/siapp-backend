package realtime

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
	"github.com/go-chi/chi/v5"

	"siapp/pkg/platform/auth"
	"siapp/pkg/platform/clock"
	"siapp/pkg/platform/httpx"
)

// Subprotocol is the WebSocket subprotocol of the realtime doc.
const Subprotocol = "persist.v1"

// Close codes (realtime doc §3.3).
const (
	CloseBadFrame     websocket.StatusCode = 4400
	CloseUnauthorized websocket.StatusCode = 4401
	CloseIdle         websocket.StatusCode = 4408
)

const (
	defaultPingInterval = 20 * time.Second
	defaultIdleTimeout  = 60 * time.Second
	writeTimeout        = 10 * time.Second
	readLimit           = 4 << 10
	maxRoomsPerFrame    = 32
	maxRoomsPerConn     = 64
)

// Denial reasons in subscribe.ack.
const (
	DeniedInvalidRoom = "INVALID_ROOM"
	DeniedForbidden   = "FORBIDDEN"
	DeniedUnavailable = "UNAVAILABLE"
	DeniedRoomLimit   = "ROOM_LIMIT"
)

// Config tunes a Handler. The zero value is the production setting; tests shorten the timers.
type Config struct {
	PingInterval time.Duration
	IdleTimeout  time.Duration
	// OriginPatterns are the hosts a browser may connect from (the web admin's host). A client
	// without an Origin header, which is every mobile app, is always accepted.
	OriginPatterns []string
}

// OriginPatternsOf turns the CORS origins ("https://admin.example.com") into the host patterns
// the WebSocket origin check takes. A wildcard is dropped, never forwarded: a socket that any
// website could open is not what a CORS "*" for a public read route meant.
func OriginPatternsOf(origins []string) []string {
	var out []string
	for _, o := range origins {
		if u, err := url.Parse(o); err == nil && u.Host != "" {
			out = append(out, u.Host)
		}
	}
	return out
}

// Handler serves GET /api/v1/ws and POST /api/v1/ws/ticket.
type Handler struct {
	tokens      *auth.Tokens
	revocations *auth.Revocations
	tickets     *Tickets
	hub         *Hub
	authorizer  *Authorizer
	clock       clock.Clock
	logger      *slog.Logger
	cfg         Config
}

// NewHandler returns the handler.
func NewHandler(tokens *auth.Tokens, revocations *auth.Revocations, tickets *Tickets, hub *Hub, authorizer *Authorizer,
	c clock.Clock, logger *slog.Logger, cfg Config,
) *Handler {
	if cfg.PingInterval <= 0 {
		cfg.PingInterval = defaultPingInterval
	}
	if cfg.IdleTimeout <= 0 {
		cfg.IdleTimeout = defaultIdleTimeout
	}
	return &Handler{tokens: tokens, revocations: revocations, tickets: tickets, hub: hub, authorizer: authorizer, clock: c, logger: logger, cfg: cfg}
}

// Mount adds the routes. GET /api/v1/ws is on auth.PublicRoutes: it authenticates itself, by a
// Bearer token or by a ticket, and answers a failure with the close code 4401 that the realtime
// doc prescribes, which a middleware that replies 401 before the upgrade could not send.
// POST /api/v1/ws/ticket is an ordinary authenticated route.
func (h *Handler) Mount(r chi.Router) {
	r.Get("/api/v1/ws", h.serve)
	r.Post("/api/v1/ws/ticket", h.ticket)
}

// ticketResponse is the data of POST /ws/ticket.
type ticketResponse struct {
	Ticket    string `json:"ticket"`
	ExpiresIn int    `json:"expires_in"`
}

func (h *Handler) ticket(w http.ResponseWriter, r *http.Request) {
	p, ok := auth.From(r.Context())
	if !ok {
		httpx.Error(w, r, auth.ErrInvalidToken)
		return
	}
	// The access token was verified by auth.Require; it is read again only for its expiry.
	_, expiresAt, err := h.tokens.ParseAccessTokenWithExpiry(bearerOf(r))
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	ticket, err := h.tickets.Issue(r.Context(), p, expiresAt)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.OK(w, r, http.StatusOK, ticketResponse{Ticket: ticket, ExpiresIn: int(TicketTTL / time.Second)})
}

func bearerOf(r *http.Request) string {
	scheme, token, found := strings.Cut(r.Header.Get("Authorization"), " ")
	if !found || !strings.EqualFold(scheme, "Bearer") {
		return ""
	}
	return strings.TrimSpace(token)
}

// authenticate returns who is connecting and when their token runs out. ok is false for any
// failure, whatever its cause: the client is told 4401 and refreshes.
func (h *Handler) authenticate(r *http.Request) (p auth.Principal, expiresAt time.Time, ok bool) {
	if ticket := r.URL.Query().Get("ticket"); ticket != "" {
		t, found, err := h.tickets.Redeem(r.Context(), ticket)
		if err != nil || !found {
			return auth.Principal{}, time.Time{}, false
		}
		p, expiresAt = t.Principal, t.ExpiresAt
	} else {
		claims, exp, err := h.tokens.ParseAccessTokenWithExpiry(bearerOf(r))
		if err != nil {
			return auth.Principal{}, time.Time{}, false
		}
		p, expiresAt = auth.Principal(claims), exp
	}
	if !expiresAt.After(h.clock.Now()) || h.revocations.IsRevoked(r.Context(), p.SessionID) {
		return auth.Principal{}, time.Time{}, false
	}
	return p, expiresAt, true
}

func (h *Handler) serve(w http.ResponseWriter, r *http.Request) {
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		Subprotocols: []string{Subprotocol}, OriginPatterns: h.cfg.OriginPatterns,
	})
	if err != nil {
		return // Accept has already answered
	}
	defer func() { _ = conn.CloseNow() }()

	p, expiresAt, ok := h.authenticate(r)
	if !ok {
		_ = conn.Close(CloseUnauthorized, "unauthorized")
		return
	}
	if conn.Subprotocol() != Subprotocol {
		_ = conn.Close(CloseBadFrame, "subprotocol "+Subprotocol+" required")
		return
	}
	conn.SetReadLimit(readLimit)

	s := &session{h: h, conn: conn, principal: p}
	s.ctx, s.cancel = context.WithCancel(context.Background())
	defer s.cancel()
	s.c = NewConn(p.OrganizationID, p.PersonnelID, func() { s.closeWith(websocket.StatusTryAgainLater, "too slow") })
	s.lastPong.Store(h.clock.Now().UnixNano())
	defer h.hub.Remove(s.c)

	s.reply("hello", "", map[string]any{
		"server_time":  h.clock.Now().UTC(),
		"capabilities": []string{"subscribe", "notification.created"},
	})
	go s.writeLoop()
	go s.keepAlive(expiresAt)
	s.readLoop()
}

// session is one accepted connection.
type session struct {
	h          *Handler
	conn       *websocket.Conn
	c          *Conn
	principal  auth.Principal
	ctx        context.Context
	cancel     context.CancelFunc
	lastPong   atomic.Int64
	rechecking atomic.Bool
	closeOnce  sync.Once
}

// closeWith ends the connection with a close code, once. It does not wait for the peer.
//
// The context is cancelled only after the close handshake is over: cancelling first would end
// the read loop, return from the handler and drop the TCP connection before the client has read
// the close code, and the client would see a broken pipe instead of 4401 or 4408.
func (s *session) closeWith(code websocket.StatusCode, reason string) {
	s.closeOnce.Do(func() {
		go func() {
			_ = s.conn.Close(code, reason)
			s.cancel()
		}()
	})
}

// reply queues a frame for this connection only. A client too slow to take its own replies is
// dropped like one too slow to take a broadcast.
func (s *session) reply(typ, room string, data any) {
	raw, err := json.Marshal(Envelope{Type: typ, Room: room, TS: s.h.clock.Now().UTC(), MsgID: newMsgID(), Data: data})
	if err != nil {
		s.h.logger.Error("realtime: encode reply", slog.String("type", typ), slog.Any("error", err))
		return
	}
	select {
	case s.c.send <- raw:
	default:
		s.c.kick()
	}
}

func (s *session) writeLoop() {
	for {
		select {
		case <-s.ctx.Done():
			return
		case frame := <-s.c.send:
			ctx, cancel := context.WithTimeout(s.ctx, writeTimeout)
			err := s.conn.Write(ctx, websocket.MessageText, frame)
			cancel()
			if err != nil {
				s.cancel()
				return
			}
		}
	}
}

// keepAlive pings every PingInterval and closes the connection with 4408 when no pong came for
// IdleTimeout, and with 4401 when the token has run out or its session was ended. The reader
// loop must be running for a pong to be seen, which it always is.
func (s *session) keepAlive(expiresAt time.Time) {
	expiry := time.NewTimer(time.Until(expiresAt))
	defer expiry.Stop()
	tick := time.NewTicker(s.h.cfg.PingInterval)
	defer tick.Stop()
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-expiry.C:
			s.closeWith(CloseUnauthorized, "token expired")
			return
		case <-tick.C:
			if time.Since(time.Unix(0, s.lastPong.Load())) > s.h.cfg.IdleTimeout {
				s.closeWith(CloseIdle, "idle timeout")
				return
			}
			if s.h.revocations.IsRevoked(s.ctx, s.principal.SessionID) {
				s.closeWith(CloseUnauthorized, "session ended")
				return
			}
			go s.recheckRooms()
			go func() {
				ctx, cancel := context.WithTimeout(s.ctx, s.h.cfg.IdleTimeout)
				defer cancel()
				if s.conn.Ping(ctx) == nil {
					s.lastPong.Store(time.Now().UnixNano())
				}
			}()
		}
	}
}

// recheckRooms asks the Authorizer again about every team and project room the connection joined,
// and takes it out of the ones it may no longer be in: a person moved to another regu, a DANRU
// who became an ordinary member, an admin whose role was withdrawn. Without it the permission
// is only judged at subscribe time, and such a person would keep receiving the regu's panics and
// the positions of everyone in it until the token ran out or the socket dropped.
//
// It runs on the keep-alive tick (every PingInterval, 20 s), one transaction per connection, and
// only for connections that hold such rooms. If the database cannot answer, the rooms are kept:
// a hiccup must not cut a phone off from a panic. A person who is deactivated is not left to
// this check, their session ends (see keepAlive).
func (s *session) recheckRooms() {
	if !s.rechecking.CompareAndSwap(false, true) {
		return // the previous check is still running
	}
	defer s.rechecking.Store(false)

	var rooms []Room
	for _, name := range s.h.hub.RoomsOf(s.c) {
		if room, ok := ParseRoom(name); ok && room.Scope != ScopePersonnel {
			rooms = append(rooms, room)
		}
	}
	if len(rooms) == 0 {
		return
	}
	allowed, err := s.h.authorizer.AllowedRooms(s.ctx, s.principal, rooms)
	if err != nil {
		if s.ctx.Err() == nil {
			s.h.logger.Error("realtime: re-check rooms", slog.Any("error", err))
		}
		return
	}
	var revoked []string
	for _, room := range rooms {
		if !allowed[room.String()] {
			s.h.hub.Leave(s.c, room.String())
			revoked = append(revoked, room.String())
		}
	}
	if len(revoked) > 0 {
		s.reply("unsubscribed", "", map[string]any{"rooms": revoked, "reason": DeniedForbidden})
	}
}

// frame is what a client sends.
type frame struct {
	Type string          `json:"type"`
	Data json.RawMessage `json:"data"`
}

type roomsData struct {
	Rooms []string `json:"rooms"`
}

type denial struct {
	Room   string `json:"room"`
	Reason string `json:"reason"`
}

func (s *session) readLoop() {
	for {
		_, raw, err := s.conn.Read(s.ctx)
		if err != nil {
			return
		}
		// Any frame proves the client is alive, not only the answer to a protocol ping: a client
		// or a proxy in between that does not pass control frames through but sends its own
		// {"type":"ping"} must not be closed for it.
		s.lastPong.Store(time.Now().UnixNano())
		var f frame
		if json.Unmarshal(raw, &f) != nil || f.Type == "" {
			s.reply("error", "", map[string]any{"code": CloseBadFrame, "message": "frame is not valid JSON with a type"})
			continue
		}
		switch f.Type {
		case "subscribe":
			s.subscribe(f.Data)
		case "unsubscribe":
			var d roomsData
			if json.Unmarshal(f.Data, &d) == nil {
				for _, room := range d.Rooms {
					s.h.hub.Leave(s.c, room)
				}
			}
		case "ping":
			s.reply("pong", "", map[string]any{})
		default:
			s.reply("error", "", map[string]any{"code": CloseBadFrame, "message": "unknown frame type"})
		}
	}
}

func (s *session) subscribe(raw json.RawMessage) {
	var d roomsData
	if json.Unmarshal(raw, &d) != nil || len(d.Rooms) == 0 || len(d.Rooms) > maxRoomsPerFrame {
		s.reply("error", "", map[string]any{"code": CloseBadFrame, "message": "subscribe needs 1 to 32 rooms"})
		return
	}
	joined, denied := []string{}, []denial{}
	for _, name := range d.Rooms {
		room, ok := ParseRoom(name)
		if !ok {
			denied = append(denied, denial{name, DeniedInvalidRoom})
			continue
		}
		if len(s.h.hub.RoomsOf(s.c)) >= maxRoomsPerConn {
			denied = append(denied, denial{name, DeniedRoomLimit})
			continue
		}
		allowed, err := s.h.authorizer.Allow(s.ctx, s.principal, room)
		switch {
		case err != nil:
			s.h.logger.Error("realtime: authorize", slog.Any("error", err))
			denied = append(denied, denial{name, DeniedUnavailable})
		case !allowed:
			denied = append(denied, denial{name, DeniedForbidden})
		default:
			s.h.hub.Join(s.c, room.String())
			joined = append(joined, room.String())
		}
	}
	s.reply("subscribe.ack", "", map[string]any{"joined": joined, "denied": denied})
}
