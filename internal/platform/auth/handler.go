package auth

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"

	"siapp/internal/platform/httpx"
)

// Handler serves the auth endpoints.
type Handler struct {
	service  *Service
	clientIP func(*http.Request) string
	limiter  *httpx.FailureLimiter
}

// NewHandler returns the auth Handler. clientIP extracts the caller's address.
func NewHandler(service *Service, clientIP func(*http.Request) string, limiter *httpx.FailureLimiter) *Handler {
	return &Handler{service: service, clientIP: clientIP, limiter: limiter}
}

// login checks two failure limits before the password is verified, and counts a failure
// against both afterwards.
//
// ⚠ Why failures only, and why two limits. Login is limited by the people who get it wrong,
// not by how many people use it: a shift change, with a hundred guards on one wifi network
// logging in within a minute, is all successes and uses up no budget. A limit on all
// requests per IP would lock that site out. Instead:
//   - per IP: 100 failures a minute stops a flood from one address;
//   - per IP and account name: 10 failures a minute stops one client hammering one name,
//     and, unlike the account lockout, also covers names that do not exist.
//
// The per-account lockout (Service.Login) is the defence against an attacker who spreads
// guesses over many IPs. A caller over a limit is refused even with the right password,
// so the limit cannot be used to check guesses.
func (h *Handler) login(w http.ResponseWriter, r *http.Request) {
	var req LoginRequest
	if err := httpx.Decode(r, &req); err != nil {
		httpx.Error(w, r, err)
		return
	}

	ip := h.clientIP(r)
	rules := loginFailureRules(ip, req.Identifier)
	if retryAfter, blocked := h.limiter.Check(r.Context(), rules...); blocked {
		httpx.TooManyRequests(w, r, retryAfter)
		return
	}

	result, err := h.service.Login(r.Context(), LoginInput{
		Identifier: req.Identifier,
		Password:   req.Password,
		RememberMe: req.RememberMe,
		UserAgent:  r.UserAgent(),
		IP:         ip,
	})
	if err != nil {
		// Only wrong credentials count. A locked account is already refused, and a
		// disabled one was reached with the correct password.
		if errors.Is(err, ErrInvalidCredentials) {
			h.limiter.Record(r.Context(), rules...)
		}
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, newLoginResponse(result))
}

// loginFailureRules are the limits that apply to a login attempt from ip for identifier.
func loginFailureRules(ip, identifier string) []httpx.FailureRule {
	// The identifier is user input of up to 255 bytes, so it goes into the Redis key as a
	// fixed-size hash of its normalised form; "Budi@x.com" and "budi@x.com" share a bucket.
	sum := sha256.Sum256([]byte(normalizeIdentifier(identifier)))
	return []httpx.FailureRule{
		{Name: "login-ip", Key: ip, Max: loginFailuresPerIP, Window: loginFailureWindow},
		{Name: "login-ip-account", Key: ip + ":" + hex.EncodeToString(sum[:]), Max: loginFailuresPerIPAccount, Window: loginFailureWindow},
	}
}
