package realtime

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"

	"siapp/pkg/platform/clock"
)

// Envelope is every frame of the protocol (persist realtime doc §5.1). Data carries ids and
// codes only: no NIK and no full name ever goes through a socket.
type Envelope struct {
	Type  string    `json:"type"`
	Room  string    `json:"room,omitempty"`
	TS    time.Time `json:"ts"`
	MsgID string    `json:"msg_id"`
	Data  any       `json:"data"`
}

// Publisher sends an event to a room of an organization, on every API instance.
type Publisher interface {
	Publish(ctx context.Context, orgID int64, room, typ string, data any) error
}

func channelOf(prefix string, orgID int64) string {
	return prefix + "rt:" + strconv.FormatInt(orgID, 10)
}

// RedisPublisher publishes through Redis pub/sub.
//
// ⚠ Trade-off: one channel per organization (rt:{orgID}) and every instance subscribes to all of
// them with a pattern (rt:*), so every instance sees every event, including those for rooms it
// holds no connection in. That is simple and cheap at this scale (a few instances, events in
// the hundreds per second at most); the price is paid in Redis fan-out, and the day it hurts the
// next step is one channel per room. Redis pub/sub does not store anything: an instance that is
// disconnected misses events, which is why the clients resync over REST after a reconnect and
// why panic also travels as a push (05.2).
type RedisPublisher struct {
	rdb    redis.UniversalClient
	prefix string
	clock  clock.Clock
}

// NewRedisPublisher returns a publisher. prefix goes in front of the channel name like it does
// for every other Redis key.
func NewRedisPublisher(rdb redis.UniversalClient, prefix string, c clock.Clock) *RedisPublisher {
	return &RedisPublisher{rdb: rdb, prefix: prefix, clock: c}
}

// Publish implements Publisher.
func (p *RedisPublisher) Publish(ctx context.Context, orgID int64, room, typ string, data any) error {
	if _, ok := ParseRoom(room); !ok {
		return fmt.Errorf("realtime: %q is not a room", room)
	}
	raw, err := json.Marshal(Envelope{Type: typ, Room: room, TS: p.clock.Now().UTC(), MsgID: uuid.NewString(), Data: data})
	if err != nil {
		return fmt.Errorf("realtime: encode %s: %w", typ, err)
	}
	if err := p.rdb.Publish(ctx, channelOf(p.prefix, orgID), raw).Err(); err != nil {
		return fmt.Errorf("realtime: publish %s: %w", typ, err)
	}
	return nil
}

// Run subscribes to every organization's channel and delivers what arrives to the local hub. It
// returns once the subscription is confirmed (events published after that are not missed) and
// keeps running until ctx ends; go-redis reconnects by itself when Redis goes away.
func Run(ctx context.Context, rdb redis.UniversalClient, prefix string, hub *Hub, logger *slog.Logger) error {
	ps := rdb.PSubscribe(ctx, prefix+"rt:*")
	if _, err := ps.Receive(ctx); err != nil {
		_ = ps.Close()
		return fmt.Errorf("realtime: subscribe: %w", err)
	}
	go func() {
		defer func() { _ = ps.Close() }()
		ch := ps.Channel()
		for {
			select {
			case <-ctx.Done():
				return
			case msg, ok := <-ch:
				if !ok {
					return
				}
				deliver(hub, prefix, msg, logger)
			}
		}
	}()
	return nil
}

func deliver(hub *Hub, prefix string, msg *redis.Message, logger *slog.Logger) {
	org, err := strconv.ParseInt(strings.TrimPrefix(msg.Channel, prefix+"rt:"), 10, 64)
	var env struct {
		Room string `json:"room"`
	}
	if err != nil || org < 1 || json.Unmarshal([]byte(msg.Payload), &env) != nil || env.Room == "" {
		logger.Warn("realtime: ignoring a malformed event", slog.String("channel", msg.Channel))
		return
	}
	hub.Deliver(org, env.Room, []byte(msg.Payload))
}
