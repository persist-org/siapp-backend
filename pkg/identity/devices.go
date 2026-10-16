package identity

import (
	"context"

	"siapp/internal/db"
	"siapp/pkg/platform/apperr"
	"siapp/pkg/platform/auth"
	"siapp/pkg/platform/database"
)

// DeviceInput registers the push token of the app install that is signed in. The json tags
// carry the contract's field names.
type DeviceInput struct {
	FirebaseToken string `json:"firebase_token" validate:"required,max=4096"`
	Platform      string `json:"platform" validate:"required,oneof=android ios web"`
	AppVersion    string `json:"app_version" validate:"max=64"`
}

// RegisterDevice records the caller's push token on their login chain.
//
// ⚠ The three rules of the device registry:
//   - a device follows its login chain: it hangs on the chain's LIVE session, and a refresh
//     moves it to the next one, so the app registers once, not after every refresh;
//   - a push token belongs to one device: registering a token that another device holds
//     removes the other one first (see repo.upsertDevice);
//   - a device never outlives its login: whatever ends a chain deletes its devices
//     (repo.endFamily), and the row is bound to its session by a cascading foreign key.
//
// Registering twice with the same token updates one row, which is what makes the call safe to
// repeat: an app can send it at every start.
func (u *Usecase) RegisterDevice(ctx context.Context, p auth.Principal, in DeviceInput) error {
	platform := map[string]db.DevicePlatform{
		"android": db.DevicePlatformANDROID, "ios": db.DevicePlatformIOS, "web": db.DevicePlatformWEB,
	}[in.Platform]
	now := u.clock.Now()

	return database.WithTenant(ctx, u.pool, p.OrganizationID, func(q *db.Queries) error {
		// The token names the session it was issued for, which may be older than the chain's
		// live one (a refresh happened since). A chain with no live session has ended, and
		// the caller is not signed in any more, whatever the revocation list says.
		live, found, err := u.repo.liveSession(ctx, q, p.SessionID, p.PersonnelID)
		if err != nil {
			return err
		}
		if !found {
			return apperr.ErrUnauthenticated
		}
		return u.repo.upsertDevice(ctx, q, p.PersonnelID, live, platform, in.FirebaseToken, in.AppVersion, now)
	})
}

// RemoveDevice deletes the caller's push token from their login chain. Removing a token that
// is not there is not an error: the app may call it twice, and the end state is the same.
func (u *Usecase) RemoveDevice(ctx context.Context, p auth.Principal, firebaseToken string) error {
	return database.WithTenant(ctx, u.pool, p.OrganizationID, func(q *db.Queries) error {
		return u.repo.removeChainDevice(ctx, q, p.SessionID, p.PersonnelID, firebaseToken)
	})
}
