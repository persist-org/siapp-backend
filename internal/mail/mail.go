// Package mail sends email: an interface, an SMTP implementation, a fake for tests, and the
// embedded Indonesian templates.
package mail

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

// Message is one email with a plain-text and an HTML body.
type Message struct {
	To      string
	Subject string
	Text    string
	HTML    string
}

// Sender sends messages.
//
// An implementation must never log a message's body: reset emails carry a secret link.
type Sender interface {
	Send(ctx context.Context, m Message) error
}

// FakeSender records messages instead of sending them. It is for tests.
type FakeSender struct {
	mu   sync.Mutex
	sent []Message
	// Err, when set, makes Send fail after recording nothing, to test the failure path.
	Err error
}

// NewFakeSender returns an empty FakeSender.
func NewFakeSender() *FakeSender { return &FakeSender{} }

// Send records m, or returns Err.
func (f *FakeSender) Send(_ context.Context, m Message) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.Err != nil {
		return f.Err
	}
	f.sent = append(f.sent, m)
	return nil
}

// Sent returns a copy of the recorded messages, oldest first.
func (f *FakeSender) Sent() []Message {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Message(nil), f.sent...)
}

// asyncSendTimeout bounds one background delivery.
const asyncSendTimeout = 30 * time.Second

// Async sends in the background, so the caller does not wait for the mail server.
//
// ⚠ This is about more than speed. Forgot-password answers 202 whether or not the email has
// an account, and it must take the same time either way: if a known address waited for an
// SMTP round trip (tens to hundreds of milliseconds) and an unknown one did not, the
// response time would tell an attacker which addresses are registered.
//
// The price is that a failed delivery cannot be reported to the caller. It is logged, which
// is all a request that answers "we will send it if the account exists" could do anyway.
type Async struct {
	inner  Sender
	logger *slog.Logger
	wg     sync.WaitGroup
}

// NewAsync wraps inner so that Send returns at once.
func NewAsync(inner Sender, logger *slog.Logger) *Async {
	return &Async{inner: inner, logger: logger}
}

// Send starts delivering m and returns nil immediately. The delivery does not stop if the
// request that triggered it ends.
func (a *Async) Send(ctx context.Context, m Message) error {
	ctx = context.WithoutCancel(ctx)
	a.wg.Go(func() {
		ctx, cancel := context.WithTimeout(ctx, asyncSendTimeout)
		defer cancel()
		if err := a.inner.Send(ctx, m); err != nil {
			// The address is left out of the log on purpose; only the failure is recorded.
			a.logger.ErrorContext(ctx, "sending email failed", slog.String("error", err.Error()))
		}
	})
	return nil
}

// Wait blocks until every delivery started so far has finished, or ctx ends. Call it on
// shutdown, after the HTTP server has stopped accepting requests, so queued mail is not lost.
func (a *Async) Wait(ctx context.Context) error {
	done := make(chan struct{})
	go func() {
		a.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
