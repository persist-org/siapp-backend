package identity

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"siapp/internal/apigen"
	"siapp/pkg/platform/apperr"
	"siapp/pkg/platform/auth"
	"siapp/pkg/platform/httpx"
)

// Handler serves the identity endpoints.
type Handler struct {
	usecase  *Usecase
	clientIP func(*http.Request) string
	// limiter counts failed logins.
	limiter *httpx.FailureLimiter
}

// NewHandler returns the identity Handler. clientIP extracts the caller's address.
func NewHandler(usecase *Usecase, clientIP func(*http.Request) string, limiter *httpx.FailureLimiter) *Handler {
	return &Handler{usecase: usecase, clientIP: clientIP, limiter: limiter}
}

// Mount adds the identity routes. Every route that must work without a token is also in
// auth.PublicRoutes.
func (h *Handler) Mount(r chi.Router) {
	r.Post("/api/v1/auth/login", h.login)
}

// login checks two failure limits before Personalia is asked, and counts a failure against
// both afterwards.
//
// ⚠ Why failures only, and why two limits. Login is limited by the people who get it
// wrong, not by how many people use it: a shift change, with a hundred guards on one wifi
// network logging in within a minute, is all successes and uses up no budget. A limit on
// all requests per IP would lock that site out. Instead:
//   - per IP: 100 failures a minute stops a flood from one address;
//   - per IP and email: 10 failures a minute stops one client hammering one email, and
//     also covers emails that do not exist.
//
// Personalia's own per-email failure limit is the defence against guesses spread over many
// IPs. A caller over a limit is refused even with the right password, so the limit cannot
// be used to check guesses.
//
// Every credential failure is the same AUTH_INVALID_CREDENTIALS answer, so accounts cannot
// be enumerated.
func (h *Handler) login(w http.ResponseWriter, r *http.Request) {
	// The contract's own type decodes the body: unknown fields and wrong types are refused
	// by Decode, and the rules below are checked on the input the usecase takes.
	var req apigen.LoginRequest
	if err := httpx.Decode(r, &req); err != nil {
		httpx.Error(w, r, err)
		return
	}
	in := LoginInput{
		Email:       strings.ToLower(strings.TrimSpace(req.Identifier)),
		Password:    req.Password,
		Client:      string(req.Client),
		DeviceToken: deref(req.DeviceToken),
		RememberMe:  req.RememberMe != nil && *req.RememberMe,
		IP:          h.clientIP(r),
		UserAgent:   r.UserAgent(),
	}
	if err := httpx.Validate(&in); err != nil {
		httpx.Error(w, r, err)
		return
	}

	rules := loginFailureRules(in.IP, in.Email)
	if retryAfter, blocked := h.limiter.Check(r.Context(), rules...); blocked {
		httpx.TooManyRequests(w, r, retryAfter)
		return
	}

	result, err := h.usecase.Login(r.Context(), in)
	if err != nil {
		// Only wrong credentials count. A disabled account or a bound phone was reached
		// with the correct password, and an outage is not the caller's doing.
		if appErr, ok := errors.AsType[*apperr.Error](err); ok && appErr.Reason == ReasonInvalidCredentials {
			h.limiter.Record(r.Context(), rules...)
		}
		httpx.Error(w, r, err)
		return
	}

	// The response holds tokens: no cache, shared or private, may keep it.
	w.Header().Set("Cache-Control", "no-store")
	httpx.OK(w, r, http.StatusOK, toLoginResult(result))
}

func loginFailureRules(ip, email string) []httpx.FailureRule {
	// The email is user input of up to 254 bytes, so it goes into the Redis key as a
	// fixed-size hash. The handler has already lowercased it, so "Budi@x.com" and
	// "budi@x.com" share a bucket.
	sum := sha256.Sum256([]byte(email))
	return []httpx.FailureRule{
		{Name: "login-ip", Key: ip, Max: loginFailuresPerIP, Window: loginFailureWindow},
		{Name: "login-ip-email", Key: ip + ":" + hex.EncodeToString(sum[:]), Max: loginFailuresPerIPIdentifier, Window: loginFailureWindow},
	}
}

// toLoginResult maps a started session to the contract's LoginResult.
func toLoginResult(res LoginResult) apigen.LoginResult {
	p := res.Person
	tokenType := "Bearer"
	expiresIn := int(auth.AccessTokenTTL.Seconds())
	// The contract's Personnel has no full-name field, only first/middle/last. Personalia
	// keeps one name, so it goes in first_name: splitting it would guess wrong for names
	// with more than two parts.
	user := apigen.Personnel{
		Id:             &p.ID,
		OrganizationId: &p.OrganizationID,
		FirstName:      &p.FullName,
		IsActive:       new(true),
	}
	setIfNotEmpty(&user.Nik, p.NIK)
	setIfNotEmpty(&user.Email, p.Email)
	setIfNotEmpty(&user.Phone, p.Phone)
	setIfNotEmpty(&user.ProfilePhoto, p.PhotoURL)
	return apigen.LoginResult{
		AccessToken:  &res.AccessToken,
		RefreshToken: &res.RefreshToken,
		TokenType:    &tokenType,
		ExpiresIn:    &expiresIn,
		User:         &user,
	}
}

func setIfNotEmpty(dst **string, v string) {
	if v != "" {
		*dst = &v
	}
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
