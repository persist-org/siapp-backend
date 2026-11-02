// Package notifications is module 05: the in-app inbox every other module writes to through
// Service.Notify, and the endpoints a person reads it with. Phase 05.2 adds push delivery and
// Phase 05.3 the realtime socket and the Personel events; both plug in through Dispatcher.
//
// A feature module that notifies declares the one method it needs, in its own package, and
// cmd/api hands it the real Service:
//
//	type notifier interface { Notify(ctx context.Context, in notifications.Input) }
package notifications

import (
	"net/http"

	"siapp/pkg/platform/apperr"
)

// Error reasons (details.reason). A reason that has shipped is never renamed. The plan
// (docs/plan/05-notifications.md) also names WEBHOOK_SIGNATURE_INVALID: that one is Phase 05.3's.
const (
	ReasonNotFound = "NOTIFICATION_NOT_FOUND"
)

// ErrNotFound is returned for a notification id that does not exist, belongs to another
// organization, or belongs to another person. The three are not told apart, so that an id
// cannot be probed.
var ErrNotFound = apperr.New(http.StatusNotFound, apperr.CodeNotFound,
	ReasonNotFound, "Notifikasi tidak ditemukan.")
