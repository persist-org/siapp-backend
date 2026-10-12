package auth

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"

	"siapp/internal/platform/apperr"
	"siapp/internal/platform/httpx"
)

// Handler serves the auth endpoints.
type Handler struct {
	service  *Service
	clientIP func(*http.Request) string
	// limiter counts failed logins; rates limits requests, such as forgot-password.
	limiter *httpx.FailureLimiter
	rates   *httpx.RateLimiter
}

// NewHandler returns the auth Handler. clientIP extracts the caller's address.
func NewHandler(service *Service, clientIP func(*http.Request) string, limiter *httpx.FailureLimiter, rates *httpx.RateLimiter) *Handler {
	return &Handler{service: service, clientIP: clientIP, limiter: limiter, rates: rates}
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

// refresh is public: it authenticates with the refresh token in the body, and the access
// token is usually expired by the time it is needed. Refresh tokens are 256 bits of
// randomness, so unlike passwords there is nothing to guess and no failure limit is needed.
func (h *Handler) refresh(w http.ResponseWriter, r *http.Request) {
	var req RefreshRequest
	if err := httpx.Decode(r, &req); err != nil {
		httpx.Error(w, r, err)
		return
	}
	pair, err := h.service.Refresh(r.Context(), RefreshInput{
		RefreshToken: req.RefreshToken,
		UserAgent:    r.UserAgent(),
		IP:           h.clientIP(r),
	})
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, newTokenPairResponse(pair))
}

func (h *Handler) logout(w http.ResponseWriter, r *http.Request) {
	p, ok := From(r.Context())
	if !ok {
		// Unreachable behind Require; refuse rather than act without knowing who the caller is.
		httpx.Error(w, r, apperr.ErrUnauthenticated)
		return
	}
	if err := h.service.Logout(r.Context(), p.UserID, p.SessionID); err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, LogoutResponse{Message: "Anda telah keluar."})
}

func (h *Handler) logoutAll(w http.ResponseWriter, r *http.Request) {
	p, ok := From(r.Context())
	if !ok {
		httpx.Error(w, r, apperr.ErrUnauthenticated)
		return
	}
	ended, err := h.service.LogoutAll(r.Context(), p.UserID)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, LogoutAllResponse{
		Message:       "Anda telah keluar dari semua perangkat.",
		SessionsEnded: ended,
	})
}

// forgotPassword always answers 202, whether or not the email has an account, so that it
// cannot be used to find out which addresses are registered.
//
// ⚠ Every request counts against the limits, known address or not, so that being limited
// reveals nothing either. Two limits, for two different abuses:
//   - per IP, 30 per 15 minutes: a flood guard. Generous, because a whole site can share one IP;
//   - per email address, 3 per 15 minutes, from anywhere: stops anyone from filling a victim's
//     inbox with reset mails. The price is that someone who knows an address can make it
//     unable to ask for a link for a few minutes; the links that were sent keep working.
//
// The IP limit is checked first, so a blocked IP does not use up an address's allowance.
func (h *Handler) forgotPassword(w http.ResponseWriter, r *http.Request) {
	var req ForgotPasswordRequest
	if err := httpx.Decode(r, &req); err != nil {
		httpx.Error(w, r, err)
		return
	}

	if allowed, retryAfter := h.rates.Allow(r.Context(), "forgot-ip", h.clientIP(r), httpx.PerPeriod(forgotPerIP, forgotWindow)); !allowed {
		httpx.TooManyRequests(w, r, retryAfter)
		return
	}
	sum := sha256.Sum256([]byte(normalizeIdentifier(string(req.Email))))
	if allowed, retryAfter := h.rates.Allow(r.Context(), "forgot-email", hex.EncodeToString(sum[:]), httpx.PerPeriod(forgotPerEmail, forgotWindow)); !allowed {
		httpx.TooManyRequests(w, r, retryAfter)
		return
	}

	if err := h.service.RequestPasswordReset(r.Context(), string(req.Email)); err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusAccepted, MessageResponse{
		Message: "Jika email terdaftar, tautan untuk mengatur ulang kata sandi telah dikirim.",
	})
}

// resetPassword is public: the token in the body is the credential. Tokens are 256 bits of
// randomness, so there is nothing to guess and no failure limit is needed.
func (h *Handler) resetPassword(w http.ResponseWriter, r *http.Request) {
	var req ResetPasswordRequest
	if err := httpx.Decode(r, &req); err != nil {
		httpx.Error(w, r, err)
		return
	}
	if err := h.service.ResetPassword(r.Context(), req.Token, req.NewPassword); err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, MessageResponse{Message: "Kata sandi berhasil diubah. Silakan masuk dengan kata sandi baru."})
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
