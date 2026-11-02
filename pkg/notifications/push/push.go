// Package push sends a notification to a phone. It knows nothing about notifications, people or
// tenants: the worker hands it ready messages and reads back, for each one, whether the token is
// still good.
package push

import (
	"context"
	"log/slog"
)

// Platform is the kind of app install a token belongs to.
type Platform string

// The platforms of devices.platform.
const (
	PlatformAndroid Platform = "ANDROID"
	PlatformIOS     Platform = "IOS"
	PlatformWeb     Platform = "WEB"
)

// Message is one push to one device.
type Message struct {
	Token    string
	Platform Platform
	Title    string
	Body     string
	// Data is what the app reads to deep-link. FCM only takes strings.
	Data map[string]string
	// CollapseKey makes a second push with the same key replace the first on the phone, so a
	// retried task never shows the same notification twice.
	CollapseKey string
	// High is for panic: it is delivered at once, loudly, and on the panic channel.
	//
	// ⚠ The Android channel ("panic") and the sound ("siren") must also exist in the mobile app,
	// or the phone falls back to a silent default channel. And panic cannot be muted in the
	// app's notification settings (mobile spec): the channel is created as non-mutable there.
	High bool
}

// Result is what happened to one Message.
type Result struct {
	// InvalidToken means the provider said the token is unregistered or malformed: the app was
	// uninstalled or the token rotated. The device is deleted.
	InvalidToken bool
	// Err is why the push was not accepted. Nil on success and on an invalid token.
	Err error
	// Permanent says that sending the same message again will fail the same way: a rejected
	// payload, a sender id that does not match, a disabled API. Only a failure that is NOT
	// permanent (a timeout, a 5xx, a 429) is worth a retry.
	Permanent bool
}

// Provider delivers messages.
type Provider interface {
	// Send delivers every message and returns one Result per message, in the same order. The
	// error is for a failure of the whole call, such as credentials the provider refuses.
	Send(ctx context.Context, msgs []Message) ([]Result, error)
}

// LogProvider only logs. It is used when no FCM credentials are configured and in tests, so a
// development machine runs the whole flow without a Firebase project.
type LogProvider struct {
	Logger *slog.Logger
}

// Send logs the messages. The token and the text stay out of the log: they belong to a person.
func (p LogProvider) Send(ctx context.Context, msgs []Message) ([]Result, error) {
	for _, m := range msgs {
		p.Logger.InfoContext(ctx, "push: not sent, no FCM credentials",
			slog.String("platform", string(m.Platform)), slog.String("type", m.Data["type"]),
			slog.String("notification_id", m.Data["notification_id"]), slog.Bool("high", m.High))
	}
	return make([]Result, len(msgs)), nil
}
