package attendance

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"siapp/pkg/platform/clock"
)

// Direction is which of the two QR codes a DANRU shows: the one for arriving or the one for leaving.
type Direction string

// The two directions. They are the values of the display endpoint's direction parameter and of the
// token's "dir".
const (
	DirectionIn  Direction = "IN"
	DirectionOut Direction = "OUT"
)

// Valid reports whether d is one of the two directions.
func (d Direction) Valid() bool { return d == DirectionIn || d == DirectionOut }

const (
	// qrTokenTTL is how long a token is valid after it is issued. The app refreshes the code about
	// every 30 s, so a code on screen is always well inside it.
	qrTokenTTL = 60 * time.Second
	// qrClockTolerance is the slack on both ends of the window: a scan that lands just after expiry
	// still works (the guard raised the phone before it ran out), and a token whose time is a few
	// seconds ahead of this instance's clock (another instance issued it) is not refused.
	qrClockTolerance = 5 * time.Second
	// qrVersion is the payload version, so a later change of the payload can be told apart.
	qrVersion = 1
	// minQRSecretBytes is the shortest ATTENDANCE_QR_SECRET QRTokens accepts: the token is only as
	// strong as the key that signs it.
	minQRSecretBytes = 32
)

// QRClaims is what a QR token says: this DANRU of this regu, in this organization, showed the code
// of this direction at this time.
type QRClaims struct {
	// OrganizationID is the tenant. It is inside the signed payload, so a token cannot be used in
	// another organization.
	OrganizationID int64
	// TeamCode is the regu whose DANRU showed the code.
	TeamCode string
	// DanruID is the personnel id of the person who showed it, stored on the record as the QR leader.
	DanruID   int64
	Direction Direction
	IssuedAt  time.Time
}

// ExpiresAt is when the token stops being valid, without the tolerance.
func (c QRClaims) ExpiresAt() time.Time { return c.IssuedAt.Add(qrTokenTTL) }

// qrPayload is the signed JSON.
type qrPayload struct {
	V     int       `json:"v"`
	Org   int64     `json:"org"`
	Team  string    `json:"team"`
	Danru int64     `json:"danru"`
	Dir   Direction `json:"dir"`
	Iat   int64     `json:"iat"`
}

// QR token errors. The service turns them into the module's answers.
var (
	// errQRMalformed: not a token of ours: wrong shape, bad MAC, unknown version. One answer for all
	// of them, so a caller cannot tell a forged MAC from a mangled payload.
	errQRMalformed = errors.New("qr token: invalid")
	// errQRExpired: a token of ours, signed correctly, whose time has passed.
	errQRExpired = errors.New("qr token: expired")
)

// QRTokens issues and verifies the QR tokens a DANRU's phone shows and a guard's phone scans.
//
// Format: base64url(payload) + "." + base64url(HMAC-SHA256(payload, ATTENDANCE_QR_SECRET)), with
// payload {"v":1,"org","team","danru","dir":"IN"|"OUT","iat"}.
//
// ⚠ Threat model. The token is STATELESS: nothing is stored when it is issued or used, so showing
// a code costs no database write and the server keeps nothing to leak. What it relies on instead:
//   - It is short-lived (60 s, plus 5 s of tolerance). A screenshot of the code, passed to someone
//     who is not at the site, stops working after about a minute; so does a code that is simply
//     photographed and posted. Being short-lived is the whole defence against replay, which is why
//     the app refreshes the code every 30 s.
//   - It is NOT single-use. Several guards may legitimately scan the same code, one after another,
//     because each can check in only once per shift: the unique key of the attendance, not the
//     token, is what stops a repeat.
//   - The organization is in the signed payload, and the service compares it with the caller's, so a
//     token issued in one tenant can never be used in another.
//   - It does not prove the guard is at the site, only that a DANRU's code reached them within a
//     minute. That is why a check-in that also sends a GPS fix is still measured against the
//     project's geofence (strict refuses it), and why the fix is optional rather than required: the
//     DANRU being there is the evidence the QR method is for.
//   - A DANRU who is removed from the regu keeps a valid token for at most that minute. The
//     membership is not re-read at scan time: it would add a query to every scan to close a window
//     of 65 seconds.
//
// The MAC is checked with hmac.Equal, in constant time, and before the payload is parsed: nothing
// an attacker sends reaches the JSON decoder unless it was signed with the key.
type QRTokens struct {
	secret []byte
	clock  clock.Clock
}

// NewQRTokens returns QRTokens signing with secret. A secret shorter than 32 bytes is refused:
// generate one with `openssl rand -base64 48`.
func NewQRTokens(secret string, c clock.Clock) (*QRTokens, error) {
	if len(secret) < minQRSecretBytes {
		return nil, fmt.Errorf("attendance: ATTENDANCE_QR_SECRET must be at least %d bytes", minQRSecretBytes)
	}
	return &QRTokens{secret: []byte(secret), clock: c}, nil
}

func (t *QRTokens) mac(payload []byte) []byte {
	h := hmac.New(sha256.New, t.secret)
	h.Write(payload)
	return h.Sum(nil)
}

// Issue signs a token for a DANRU of a regu showing the code of a direction now.
func (t *QRTokens) Issue(organizationID int64, teamCode string, danruID int64, dir Direction) (token string, claims QRClaims) {
	claims = QRClaims{
		OrganizationID: organizationID, TeamCode: teamCode, DanruID: danruID, Direction: dir,
		IssuedAt: t.clock.Now().UTC().Truncate(time.Second),
	}
	payload, err := json.Marshal(qrPayload{
		V: qrVersion, Org: organizationID, Team: teamCode, Danru: danruID, Dir: dir, Iat: claims.IssuedAt.Unix(),
	})
	if err != nil {
		panic(fmt.Sprintf("attendance: encode qr payload: %v", err)) // a struct of numbers and strings
	}
	enc := base64.RawURLEncoding
	return enc.EncodeToString(payload) + "." + enc.EncodeToString(t.mac(payload)), claims
}

// Verify checks a token and returns what it says. errQRMalformed (no claims) for anything that is
// not a token signed with this key; errQRExpired, WITH the claims, for a signed token whose window
// has passed (or has not yet begun by more than the tolerance).
func (t *QRTokens) Verify(token string) (QRClaims, error) {
	body, sig, ok := strings.Cut(token, ".")
	if !ok || strings.Contains(sig, ".") {
		return QRClaims{}, errQRMalformed
	}
	// Only the canonical encoding Issue writes is accepted. The decoder alone would also take a
	// last character with non-zero spare bits and skip embedded \r and \n, so several different
	// strings would carry one valid MAC: harmless to the signature, but a token must have one form.
	enc := base64.RawURLEncoding
	payload, err := enc.DecodeString(body)
	if err != nil || enc.EncodeToString(payload) != body {
		return QRClaims{}, errQRMalformed
	}
	got, err := enc.DecodeString(sig)
	if err != nil || enc.EncodeToString(got) != sig || !hmac.Equal(got, t.mac(payload)) {
		return QRClaims{}, errQRMalformed
	}

	var p qrPayload
	if err := json.Unmarshal(payload, &p); err != nil || p.V != qrVersion || p.Team == "" || !p.Dir.Valid() {
		return QRClaims{}, errQRMalformed
	}
	claims := QRClaims{
		OrganizationID: p.Org, TeamCode: p.Team, DanruID: p.Danru, Direction: p.Dir, IssuedAt: time.Unix(p.Iat, 0).UTC(),
	}
	now := t.clock.Now()
	if now.After(claims.ExpiresAt().Add(qrClockTolerance)) || now.Before(claims.IssuedAt.Add(-qrClockTolerance)) {
		// The claims come back with the error: the caller compares the organization and the
		// direction before it says "expired", so an expired token of another organization is as
		// invalid as a live one.
		return claims, errQRExpired
	}
	return claims, nil
}
