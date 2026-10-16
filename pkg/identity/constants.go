package identity

import "time"

const (
	// Session lifetimes: "remember me" keeps a phone logged in for a month, otherwise a
	// shift-length session (manual §3.2.1.3). Each refresh restarts the clock (Phase 01.2).
	sessionTTLRememberMe = 30 * 24 * time.Hour
	sessionTTLDefault    = 12 * time.Hour

	// Login abuse limits. Only FAILED logins count (a wrong password or an unknown email),
	// so successful logins never use up anyone's budget; see Handler.login.
	//
	// loginFailuresPerIP is a flood guard for one network address. It is deliberately
	// generous: a whole site behind one wifi router (or one mobile carrier address) shares
	// an IP, and its guards must not block each other. It only trips when something is
	// sending wrong passwords at a rate no group of people would.
	loginFailuresPerIP = 100
	// loginFailuresPerIPIdentifier stops one client hammering one email, including emails
	// that do not exist. Other emails and other users on the same IP are unaffected.
	loginFailuresPerIPIdentifier = 10
	loginFailureWindow           = time.Minute
	// personelLoginRetryAfter is the window of Personalia's own per-email failure limit
	// (kpi-personalia-api, integrations.siapp.login_failures_per_email over 15 minutes).
	personelLoginRetryAfter = 15 * time.Minute

	// personelResyncAfter is how old the cached copy of a person may get before a refresh
	// asks Personalia again. See Usecase.Refresh for the trade-off.
	personelResyncAfter = 12 * time.Hour

	// Session cleanup. sessionRetention keeps recently dead sessions so that replaying one
	// of their refresh tokens is still recognised as reuse (AUTH_REFRESH_REUSED) and not just
	// as an unknown token. Past it a replay reads as unknown, which is safe: the token is
	// dead either way.
	sessionRetention = 30 * 24 * time.Hour
	cleanupBatchSize = 1000
	cleanupTimeout   = 10 * time.Minute

	maxUserAgentLen = 255
	maxIPLen        = 64
)
