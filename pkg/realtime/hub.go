package realtime

import (
	"log/slog"
	"sync"
)

// sendBuffer is how many frames one connection may have waiting.
const sendBuffer = 32

// roomKey is a room of one organization.
//
// ⚠ The organization is part of the key, not of the name the client sees: team.7.panic of
// organization 1 and team.7.panic of organization 2 are two different keys, so a frame
// delivered for one can never reach a connection of the other, whatever the clients ask for.
// Connections are also keyed by their own organization when they join, taken from the token and
// never from a frame.
type roomKey struct {
	org  int64
	name string
}

// Conn is one WebSocket connection as the hub sees it.
type Conn struct {
	Org         int64
	PersonnelID int64

	send  chan []byte
	kick  func()
	rooms map[string]struct{} // guarded by Hub.mu
}

// NewConn returns a connection. kick is called, once, when the connection cannot keep up and
// the hub drops it; it must close the socket without waiting for it.
func NewConn(org, personnelID int64, kick func()) *Conn {
	return &Conn{Org: org, PersonnelID: personnelID, send: make(chan []byte, sendBuffer), kick: kick, rooms: map[string]struct{}{}}
}

// Send is the channel the connection's writer drains.
func (c *Conn) Send() <-chan []byte { return c.send }

// Hub tracks the local connections of this API instance and the rooms they joined.
type Hub struct {
	mu     sync.RWMutex
	rooms  map[roomKey]map[*Conn]struct{}
	logger *slog.Logger
}

// NewHub returns an empty hub.
func NewHub(logger *slog.Logger) *Hub {
	return &Hub{rooms: map[roomKey]map[*Conn]struct{}{}, logger: logger}
}

// Join puts the connection in a room of its own organization.
func (h *Hub) Join(c *Conn, room string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	key := roomKey{c.Org, room}
	if h.rooms[key] == nil {
		h.rooms[key] = map[*Conn]struct{}{}
	}
	h.rooms[key][c] = struct{}{}
	c.rooms[room] = struct{}{}
}

// Leave takes the connection out of a room.
func (h *Hub) Leave(c *Conn, room string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.leave(c, room)
}

func (h *Hub) leave(c *Conn, room string) {
	key := roomKey{c.Org, room}
	delete(h.rooms[key], c)
	if len(h.rooms[key]) == 0 {
		delete(h.rooms, key)
	}
	delete(c.rooms, room)
}

// Remove takes the connection out of every room. Call it when the connection ends.
func (h *Hub) Remove(c *Conn) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for room := range c.rooms {
		h.leave(c, room)
	}
}

// RoomsOf returns the rooms a connection is in.
func (h *Hub) RoomsOf(c *Conn) []string {
	h.mu.RLock()
	defer h.mu.RUnlock()
	out := make([]string, 0, len(c.rooms))
	for room := range c.rooms {
		out = append(out, room)
	}
	return out
}

// Deliver hands a frame to every local connection in the room of the organization.
//
// ⚠ It never waits for a connection. A client whose buffer is full is dropped, not blocked on:
// one stuck phone in a poor signal area must not stall a panic broadcast to the rest of the
// regu, and the loop here holds a lock that every join and leave needs.
func (h *Hub) Deliver(org int64, room string, frame []byte) {
	h.mu.RLock()
	conns := h.rooms[roomKey{org, room}]
	var slow []*Conn
	for c := range conns {
		select {
		case c.send <- frame:
		default:
			slow = append(slow, c)
		}
	}
	h.mu.RUnlock()
	for _, c := range slow {
		h.logger.Warn("realtime: dropping a connection that cannot keep up",
			slog.Int64("organization_id", org), slog.Int64("personnel_id", c.PersonnelID))
		c.kick()
	}
}
