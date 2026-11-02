package realtime

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"

	"siapp/pkg/platform/auth"
)

// TicketTTL is how long a ticket can be redeemed.
const TicketTTL = 30 * time.Second

const ticketBytes = 32

// Ticket is what a redeemed ticket gives back: who asked for it, and when the access token
// that asked for it runs out (the socket is closed with 4401 then).
type Ticket struct {
	Principal auth.Principal
	ExpiresAt time.Time
}

type ticketValue struct {
	Org       int64 `json:"o"`
	Personnel int64 `json:"p"`
	Session   int64 `json:"s"`
	Expires   int64 `json:"e"` // Unix milliseconds
}

// Tickets issues single-use WebSocket tickets, kept in Redis.
//
// ⚠ Why a ticket and not ?token= as the realtime doc allows: a browser cannot set an
// Authorization header on a WebSocket, and an access token in a URL ends up in proxy and server
// logs, where it is good for 15 minutes. A ticket is good for 30 seconds (TicketTTL) and for one connection
// (it is deleted by the read that redeems it), so what a log keeps is harmless.
type Tickets struct {
	rdb    redis.UniversalClient
	prefix string
}

// NewTickets returns the ticket store.
func NewTickets(rdb redis.UniversalClient, prefix string) *Tickets {
	return &Tickets{rdb: rdb, prefix: prefix}
}

func (t *Tickets) key(ticket string) string { return t.prefix + "ws-ticket:" + ticket }

// Issue creates a ticket for p. expiresAt is the expiry of p's access token.
func (t *Tickets) Issue(ctx context.Context, p auth.Principal, expiresAt time.Time) (string, error) {
	b := make([]byte, ticketBytes)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate ws ticket: %w", err)
	}
	ticket := base64.RawURLEncoding.EncodeToString(b)
	raw, err := json.Marshal(ticketValue{Org: p.OrganizationID, Personnel: p.PersonnelID, Session: p.SessionID, Expires: expiresAt.UnixMilli()})
	if err != nil {
		return "", err
	}
	if err := t.rdb.Set(ctx, t.key(ticket), raw, TicketTTL).Err(); err != nil {
		return "", fmt.Errorf("store ws ticket: %w", err)
	}
	return ticket, nil
}

// Redeem returns the ticket's owner and deletes the ticket in the same Redis command, so two
// connections racing with one ticket cannot both win. ok is false for a ticket that is unknown,
// expired or already used.
func (t *Tickets) Redeem(ctx context.Context, ticket string) (Ticket, bool, error) {
	raw, err := t.rdb.GetDel(ctx, t.key(ticket)).Bytes()
	if errors.Is(err, redis.Nil) {
		return Ticket{}, false, nil
	}
	if err != nil {
		return Ticket{}, false, fmt.Errorf("redeem ws ticket: %w", err)
	}
	var v ticketValue
	if err := json.Unmarshal(raw, &v); err != nil {
		return Ticket{}, false, nil
	}
	return Ticket{
		Principal: auth.Principal{PersonnelID: v.Personnel, OrganizationID: v.Org, SessionID: v.Session},
		ExpiresAt: time.UnixMilli(v.Expires),
	}, true, nil
}
