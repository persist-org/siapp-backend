package identity

import (
	"time"

	"siapp/internal/db"
	"siapp/pkg/platform/pii"
)

// Client names, as in the contract's LoginRequest.client.
const (
	ClientMobile = "MOBILE"
	ClientWeb    = "WEB"
)

// LoginInput is a login request. The json tags carry the contract's field names, so the
// validation errors name the field the client sent (Email is the contract's `identifier`).
// IP and UserAgent come from the HTTP request, never from the body.
type LoginInput struct {
	// Email is the Personalia account email, trimmed and lowercased by the handler.
	Email string `json:"identifier" validate:"required,email,max=254"`
	// Password is passed to Personalia and nowhere else: it is never stored, hashed or logged.
	Password string `json:"password" validate:"required,max=256"`
	Client   string `json:"client" validate:"required,oneof=MOBILE WEB"`
	// DeviceToken identifies the phone. Required for MOBILE; ignored for WEB.
	DeviceToken string `json:"device_token" validate:"required_if=Client MOBILE,max=512"`
	RememberMe  bool   `json:"remember_me"`

	IP        string `json:"-"`
	UserAgent string `json:"-"`
}

// Person is the signed-in person as the login response shows them. NIK is masked: login is
// not one of the places a phase unmasks it.
type Person struct {
	ID             int64
	OrganizationID int64
	NIK            string // masked, empty when the person has none
	FullName       string
	Email          string
	Phone          string
	PhotoURL       string
}

// LoginResult is a started session: SiAPP's own token pair and who it belongs to.
type LoginResult struct {
	AccessToken  string
	RefreshToken string
	Person       Person
}

func personFrom(p db.Personnel) Person {
	return Person{
		ID:             p.ID,
		OrganizationID: p.OrganizationID,
		NIK:            MaskNIK(p.Nik.String),
		FullName:       p.FullName,
		Email:          p.Email.String,
		Phone:          p.Phone.String,
		PhotoURL:       p.PhotoUrl.String,
	}
}

// MaskNIK hides all but the last 4 characters of a NIK (`************1234`); see pii.MaskNIK.
func MaskNIK(nik string) string { return pii.MaskNIK(nik) }

// sessionTTL is how long a session lives from login or from each refresh.
func sessionTTL(rememberMe bool) time.Duration {
	if rememberMe {
		return sessionTTLRememberMe
	}
	return sessionTTLDefault
}

func truncate(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	// Cut on a rune boundary: a half character would be invalid UTF-8 for Postgres.
	cut := limit
	for cut > 0 && !isRuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}

func isRuneStart(b byte) bool { return b&0xC0 != 0x80 }
