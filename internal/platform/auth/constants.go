package auth

import "time"

const (
	accessTokenTTL    = 15 * time.Minute
	refreshTokenBytes = 32

	// Session lifetimes: "remember me" keeps a phone logged in for a month, otherwise a
	// shift-length session.
	sessionTTLRememberMe = 30 * 24 * time.Hour
	sessionTTLDefault    = 12 * time.Hour

	// Lockout: after maxFailedLogins consecutive failures the account is locked for
	// lockoutDuration.
	maxFailedLogins = 5
	lockoutDuration = 15 * time.Minute

	// Login abuse limits. Only FAILED logins count (a wrong password or an unknown user), so
	// successful logins never use up anyone's budget; see Handler.login.
	//
	// loginFailuresPerIP is a flood guard for one network address. It is deliberately
	// generous: a whole site behind one wifi router (or one mobile carrier address) shares
	// an IP, and they must not block each other. It only trips when something is sending
	// wrong passwords at a rate no group of people would.
	loginFailuresPerIP = 100
	// loginFailuresPerIPAccount stops one client hammering one account name (including
	// names that do not exist, which the per-account lockout cannot cover). Other accounts
	// and other users on the same IP are unaffected.
	loginFailuresPerIPAccount = 10
	loginFailureWindow        = time.Minute

	maxUserAgentLen = 255
	dummyPassword   = "not-a-real-password"
)
