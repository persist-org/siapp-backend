package personel

import (
	"sync"
	"time"

	"siapp/pkg/platform/clock"
)

const (
	// breakerThreshold consecutive failed calls open the breaker.
	breakerThreshold = 5
	// breakerCooldown is how long it stays open before one probe call is let through.
	breakerCooldown = 30 * time.Second
)

// breaker stops SiAPP from hammering a Personalia that is down, and stops every request
// from waiting out the timeout. It counts calls, not HTTP attempts: a call that failed
// after its retries is one failure. Only outages count (network error, timeout, 5xx); a
// 4xx means Personalia is up and answered.
//
// closed -> (threshold failures) -> open -> (cooldown) -> half-open: exactly one probe
// call; success closes it, failure opens it again for another cooldown.
type breaker struct {
	clock clock.Clock

	mu       sync.Mutex
	failures int
	openedAt time.Time
	open     bool
	probing  bool
}

func newBreaker(c clock.Clock) *breaker { return &breaker{clock: c} }

// allow reports whether a call may go out. When it returns true the caller must report
// the outcome with done.
func (b *breaker) allow() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.open {
		return true
	}
	if b.clock.Now().Sub(b.openedAt) < breakerCooldown || b.probing {
		return false
	}
	b.probing = true
	return true
}

// done records the outcome of a call that allow let through. outage is true for a failure
// that counts against Personalia's health.
func (b *breaker) done(outage bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.probing = false
	if !outage {
		b.failures = 0
		b.open = false
		return
	}
	b.failures++
	if b.open || b.failures >= breakerThreshold {
		b.open = true
		b.openedAt = b.clock.Now()
	}
}
