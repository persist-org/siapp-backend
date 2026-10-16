package identity_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"

	"siapp/pkg/identity"
	"siapp/pkg/platform/apperr"
	"siapp/pkg/platform/auth"
	"siapp/pkg/platform/clock"
	"siapp/pkg/platform/database"
	"siapp/pkg/platform/httpx"
	"siapp/pkg/platform/personel/personeltest"
	"siapp/pkg/platform/testkit"
)

func TestMain(m *testing.M) { os.Exit(testkit.NewDatabase(m)) }

const (
	loginPath    = "/auth/login"
	whoamiPath   = "/api/v1/test/whoami"
	wrongPass    = "wrong-Pass-9876"
	phoneA       = "device-token-phone-A-0001"
	phoneB       = "device-token-phone-B-0002"
	forwardedFor = "X-Forwarded-For"
)

// syncBuffer is a log sink that is safe to read while the server is still writing to it.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

type env struct {
	t      *testing.T
	fake   *personeltest.Fake
	srv    string
	client *testkit.Client
	logs   *syncBuffer
	// revocations is the list the server checks tokens against, shared with the usecase.
	revocations *auth.Revocations
	// bodies collects every response body the helpers saw, for the "nothing leaks" test.
	bodies []string
}

// newEnv starts the real router with the identity module on a clean database, the Personel
// fake and a Redis prefix of its own. clientIPHeader is "" unless a test needs to vary the
// caller's address.
func newEnv(t *testing.T, clientIPHeader string) *env {
	t.Helper()
	testkit.Reset(t)
	fake, personelClient := testkit.NewPersonel(t)

	logs := &syncBuffer{}
	logger := slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	// Code that logs through slog.Default (httpx.Error, the Personel client) must be seen by
	// the "nothing leaks" test too. The tests of this package do not run in parallel.
	previous := slog.Default()
	slog.SetDefault(logger)
	t.Cleanup(func() { slog.SetDefault(previous) })
	revocations := testkit.NewRevocations(t, logger)
	usecase := identity.NewUsecase(testkit.Pool(t), personelClient, testkit.Tokens(t), revocations, clock.System{}, logger)
	failures := httpx.NewFailureLimiter(testkit.Redis(t), testkit.RandomPrefix(t), logger)
	handler := identity.NewHandler(usecase, httpx.ClientIPFunc(clientIPHeader), failures)

	srv := testkit.NewServer(t, testkit.Deps{Logger: logger, Revocations: revocations, Mount: func(r chi.Router) {
		handler.Mount(r)
		// A protected route of the test, to prove that a token does or does not pass.
		r.Get(whoamiPath, func(w http.ResponseWriter, r *http.Request) { httpx.OK(w, r, http.StatusOK, nil) })
	}})
	return &env{t: t, fake: fake, srv: srv.URL, client: testkit.NewClient(t, srv), logs: logs, revocations: revocations}
}

func mobile(email, device string) map[string]any {
	return map[string]any{"identifier": email, "password": personeltest.Password, "client": "MOBILE", "device_token": device}
}

func web(email string) map[string]any {
	return map[string]any{"identifier": email, "password": personeltest.Password, "client": "WEB"}
}

func withPassword(body map[string]any, password string) map[string]any {
	out := map[string]any{}
	for k, v := range body {
		out[k] = v
	}
	out["password"] = password
	return out
}

type errorBody struct {
	Code    string         `json:"code"`
	Message string         `json:"message"`
	Details map[string]any `json:"details"`
	Errors  []struct {
		Field string `json:"field"`
		Code  string `json:"code"`
	} `json:"errors"`
}

func (e errorBody) reason() string { r, _ := e.Details["reason"].(string); return r }

type loginData struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int    `json:"expires_in"`
	User         struct {
		ID             int64  `json:"id"`
		OrganizationID int64  `json:"organization_id"`
		NIK            string `json:"nik"`
		FirstName      string `json:"first_name"`
		Email          string `json:"email"`
		Phone          string `json:"phone"`
		IsActive       bool   `json:"is_active"`
	} `json:"user"`
}

type reply struct {
	Status int
	Header http.Header
	Raw    []byte
}

func (r reply) data(t *testing.T) loginData {
	t.Helper()
	require.Equal(t, http.StatusOK, r.Status, "body: %s", r.Raw)
	var env struct {
		Success bool      `json:"success"`
		Data    loginData `json:"data"`
	}
	require.NoError(t, json.Unmarshal(r.Raw, &env))
	require.True(t, env.Success)
	return env.Data
}

func (r reply) err(t *testing.T) errorBody {
	t.Helper()
	var env struct {
		Success bool      `json:"success"`
		Error   errorBody `json:"error"`
	}
	require.NoError(t, json.Unmarshal(r.Raw, &env), "body: %s", r.Raw)
	require.False(t, env.Success)
	return env.Error
}

// login sends a request that the contract allows, and checks the response against the
// contract too.
func (e *env) login(body map[string]any) reply {
	e.t.Helper()
	res := e.client.Do(http.MethodPost, loginPath, body)
	e.bodies = append(e.bodies, string(res.Body))
	return reply{Status: res.Status, Header: res.Header, Raw: res.Body}
}

// post sends anything, including requests the contract forbids, so that the server's own
// validation is what answers. The answer must still be a contract error envelope.
func (e *env) post(body any, headers map[string]string) reply {
	e.t.Helper()
	return e.postTo(loginPath, body, headers)
}

func (e *env) postTo(path string, body any, headers map[string]string) reply {
	e.t.Helper()
	raw, err := json.Marshal(body)
	require.NoError(e.t, err)
	return e.postRaw(path, raw, headers)
}

// postRaw sends the bytes as they are, for a request with no body at all.
func (e *env) postRaw(path string, raw []byte, headers map[string]string) reply {
	e.t.Helper()
	req, err := http.NewRequestWithContext(e.t.Context(), http.MethodPost, e.srv+"/api/v1"+path, bytes.NewReader(raw))
	require.NoError(e.t, err)
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	res, err := http.DefaultClient.Do(req)
	require.NoError(e.t, err)
	defer func() { _ = res.Body.Close() }()
	out, err := io.ReadAll(res.Body)
	require.NoError(e.t, err)
	e.bodies = append(e.bodies, string(out))
	if res.StatusCode >= 400 {
		testkit.ValidateSchema(e.t, "ErrorEnvelope", out)
	}
	return reply{Status: res.StatusCode, Header: res.Header, Raw: out}
}

// inTenant runs fn in a transaction scoped to org, the way the application reads tenant data.
func inTenant(t *testing.T, org int64, fn func(tx pgx.Tx)) {
	t.Helper()
	require.NoError(t, database.WithTenantTx(t.Context(), testkit.Pool(t), org, func(tx pgx.Tx) error {
		fn(tx)
		return nil
	}))
}

func scalar[T any](t *testing.T, org int64, query string, args ...any) T {
	t.Helper()
	var v T
	inTenant(t, org, func(tx pgx.Tx) {
		require.NoError(t, tx.QueryRow(t.Context(), query, args...).Scan(&v))
	})
	return v
}

func countRows(t *testing.T, org int64, table string) int {
	t.Helper()
	return scalar[int](t, org, "SELECT count(*) FROM "+table)
}

func TestLoginWorksForBothDomains(t *testing.T) {
	tests := []struct {
		name, email, nik, fullName string
	}{
		{"a siapp-only account", personeltest.EmailDanruSatu, personeltest.NIKDanruSatu, "Danru Satu"},
		{"an account that also uses Personalia", personeltest.EmailSupervisorSatu, personeltest.NIKSupervisorSatu, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t, "")
			res := e.login(web(tc.email))
			data := res.data(t)

			require.Equal(t, "Bearer", data.TokenType)
			require.Equal(t, 900, data.ExpiresIn, "the access token lives 15 minutes")
			require.True(t, strings.HasPrefix(data.RefreshToken, "900101."), "the refresh token is prefixed with the organization id")
			require.Equal(t, "no-store", res.Header.Get("Cache-Control"))

			require.NotZero(t, data.User.ID)
			require.EqualValues(t, personeltest.OrgSatu, data.User.OrganizationID)
			require.True(t, data.User.IsActive)
			require.Equal(t, identity.MaskNIK(tc.nik), data.User.NIK, "NIK is masked in the response")
			require.NotContains(t, string(res.Raw), tc.nik, "the full NIK is never sent")
			if tc.fullName != "" {
				require.Equal(t, tc.fullName, data.User.FirstName)
			}

			// The token says who and where, and its session is the row that was created.
			claims, err := testkit.Tokens(t).ParseAccessToken(data.AccessToken)
			require.NoError(t, err)
			require.Equal(t, data.User.ID, claims.PersonnelID)
			require.EqualValues(t, personeltest.OrgSatu, claims.OrganizationID)
			sessionID := scalar[int64](t, personeltest.OrgSatu, "SELECT id FROM sessions WHERE personnel_id = $1", data.User.ID)
			require.Equal(t, sessionID, claims.SessionID)

			// The person is cached, with Personalia's data and no credentials.
			require.Equal(t, tc.nik, scalar[string](t, personeltest.OrgSatu, "SELECT nik FROM personnels WHERE id = $1", data.User.ID))
			require.Equal(t, "ACTIVE", scalar[string](t, personeltest.OrgSatu, "SELECT status::text FROM personnels WHERE id = $1", data.User.ID))
			require.Equal(t, tc.email, scalar[string](t, personeltest.OrgSatu, "SELECT email FROM personnels WHERE id = $1", data.User.ID))

			// The organization was registered from Personalia's answer, as ACTIVE.
			var name, status string
			require.NoError(t, testkit.Pool(t).QueryRow(t.Context(),
				"SELECT name, status::text FROM organizations WHERE id = $1", personeltest.OrgSatu).Scan(&name, &status))
			require.Equal(t, "PT Siaga Satu (seed)", name)
			require.Equal(t, "ACTIVE", status)
		})
	}
}

func TestLoginTrimsAndLowercasesTheEmail(t *testing.T) {
	e := newEnv(t, "")
	body := web("  " + strings.ToUpper(personeltest.EmailDanruSatu) + " ")
	e.login(body).data(t)
}

func TestASecondLoginUpdatesThePerson(t *testing.T) {
	e := newEnv(t, "")
	first := e.login(web(personeltest.EmailDanruSatu)).data(t)
	require.Equal(t, "Danru Satu", first.User.FirstName)
	org := int64(personeltest.OrgSatu)
	syncedFirst := scalar[time.Time](t, org, "SELECT synced_at FROM personnels WHERE id = $1", first.User.ID)

	e.fake.Rename(personeltest.NIKDanruSatu, "Danru Satu Baru")
	second := e.login(web(personeltest.EmailDanruSatu)).data(t)

	require.Equal(t, first.User.ID, second.User.ID, "the same person, not a new row")
	require.Equal(t, "Danru Satu Baru", second.User.FirstName)
	require.Equal(t, 1, countRows(t, org, "personnels"))
	require.Equal(t, "Danru Satu Baru", scalar[string](t, org, "SELECT full_name FROM personnels WHERE id = $1", first.User.ID))
	require.True(t, scalar[time.Time](t, org, "SELECT synced_at FROM personnels WHERE id = $1", first.User.ID).After(syncedFirst))
	require.Equal(t, 2, countRows(t, org, "sessions"), "every login is its own session")
}

func TestSessionRecordsTheLoginAndOnlyTheHashOfTheRefreshToken(t *testing.T) {
	e := newEnv(t, "")
	org := int64(personeltest.OrgSatu)

	res := e.post(web(personeltest.EmailDanruSatu), map[string]string{"User-Agent": "SiAPP-Test/1.0"})
	data := res.data(t)

	require.Equal(t, auth.HashToken(data.RefreshToken),
		scalar[string](t, org, "SELECT refresh_token_hash FROM sessions WHERE personnel_id = $1", data.User.ID))
	require.Equal(t, 0, scalar[int](t, org, "SELECT count(*) FROM sessions WHERE refresh_token_hash = $1", data.RefreshToken),
		"the plain token is never stored")
	require.Equal(t, "SiAPP-Test/1.0", scalar[string](t, org, "SELECT user_agent FROM sessions WHERE personnel_id = $1", data.User.ID))
	require.Equal(t, "127.0.0.1", scalar[string](t, org, "SELECT ip FROM sessions WHERE personnel_id = $1", data.User.ID))
	require.Equal(t, "WEB", scalar[string](t, org, "SELECT client::text FROM sessions WHERE personnel_id = $1", data.User.ID))
}

func TestSessionLifetimeFollowsRememberMe(t *testing.T) {
	tests := []struct {
		name       string
		rememberMe bool
		want       time.Duration
	}{
		{"default is a shift", false, 12 * time.Hour},
		{"remember me is a month", true, 30 * 24 * time.Hour},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t, "")
			body := web(personeltest.EmailDanruSatu)
			body["remember_me"] = tc.rememberMe
			data := e.login(body).data(t)

			seconds := scalar[float64](t, personeltest.OrgSatu,
				"SELECT EXTRACT(EPOCH FROM expires_at - created_at)::float8 FROM sessions WHERE personnel_id = $1", data.User.ID)
			require.InDelta(t, tc.want.Seconds(), seconds, 1)
		})
	}
}

func TestWrongCredentialsGetOneGenericAnswer(t *testing.T) {
	e := newEnv(t, "")
	wrongPassword := e.login(withPassword(web(personeltest.EmailDanruSatu), wrongPass))
	unknownEmail := e.login(web("nobody@siapp.test"))

	for _, res := range []reply{wrongPassword, unknownEmail} {
		require.Equal(t, http.StatusUnauthorized, res.Status)
		got := res.err(t)
		require.Equal(t, "UNAUTHENTICATED", got.Code)
		require.Equal(t, identity.ReasonInvalidCredentials, got.reason())
	}
	require.Equal(t, wrongPassword.err(t).Message, unknownEmail.err(t).Message,
		"an unknown email and a wrong password must be indistinguishable")
	require.Equal(t, 0, countRows(t, personeltest.OrgSatu, "personnels"), "a failed login writes nothing")
	require.Equal(t, 0, countRows(t, personeltest.OrgSatu, "sessions"))
}

func TestLoginFailuresAreRateLimitedPerIPAndEmail(t *testing.T) {
	e := newEnv(t, "")
	for i := range 10 {
		res := e.login(withPassword(web(personeltest.EmailDanruSatu), wrongPass))
		require.Equal(t, http.StatusUnauthorized, res.Status, "failure %d", i+1)
	}

	blocked := e.login(withPassword(web(personeltest.EmailDanruSatu), wrongPass))
	require.Equal(t, http.StatusTooManyRequests, blocked.Status, "the 11th failure is refused")
	require.Equal(t, "RATE_LIMITED", blocked.err(t).Code)
	require.NotEmpty(t, blocked.Header.Get("Retry-After"))

	// ⚠ Over the limit even the right password is refused, so the limit cannot be used to
	// check guesses.
	require.Equal(t, http.StatusTooManyRequests, e.login(web(personeltest.EmailDanruSatu)).Status)
	require.Equal(t, 10, e.fake.CallCount("/login"), "the refused attempts never reached Personalia")

	// Another email from the same address is unaffected: a site behind one router shares an IP.
	require.Equal(t, http.StatusOK, e.login(web(personeltest.EmailDanruDua)).Status)
}

func TestSuccessfulLoginsNeverUseUpTheBudget(t *testing.T) {
	e := newEnv(t, "")
	for range 15 {
		require.Equal(t, http.StatusOK, e.login(web(personeltest.EmailDanruSatu)).Status)
	}
}

func TestPersonaliaOwnLimitCoversGuessesSpreadOverManyAddresses(t *testing.T) {
	e := newEnv(t, forwardedFor)
	from := func(ip string) map[string]string { return map[string]string{forwardedFor: ip} }

	// Ten wrong passwords, each from another address: none reaches SiAPP's per-IP limits.
	for i := range 10 {
		res := e.post(withPassword(web(personeltest.EmailDanruSatu), wrongPass), from(fmt.Sprintf("10.0.0.%d", i+1)))
		require.Equal(t, http.StatusUnauthorized, res.Status)
	}
	// The 11th, from a fresh address and with the right password, is stopped by Personalia.
	res := e.post(web(personeltest.EmailDanruSatu), from("10.0.9.9"))
	require.Equal(t, http.StatusTooManyRequests, res.Status)
	require.Equal(t, "RATE_LIMITED", res.err(t).Code)
}

func TestInvalidRequestsAreRefusedBeforePersonaliaIsAsked(t *testing.T) {
	longDevice := strings.Repeat("d", 513)
	tests := []struct {
		name      string
		body      any
		status    int
		field     string
		fieldCode string
	}{
		{"mobile without a device token", map[string]any{"identifier": personeltest.EmailDanruSatu, "password": "x", "client": "MOBILE"},
			http.StatusUnprocessableEntity, "device_token", "REQUIRED"},
		{"mobile with an empty device token", map[string]any{"identifier": personeltest.EmailDanruSatu, "password": "x", "client": "MOBILE", "device_token": ""},
			http.StatusUnprocessableEntity, "device_token", "REQUIRED"},
		{"a device token that is too long", map[string]any{"identifier": personeltest.EmailDanruSatu, "password": "x", "client": "MOBILE", "device_token": longDevice},
			http.StatusUnprocessableEntity, "device_token", "TOO_LONG"},
		{"no client", map[string]any{"identifier": personeltest.EmailDanruSatu, "password": "x"},
			http.StatusUnprocessableEntity, "client", "REQUIRED"},
		{"an unknown client", map[string]any{"identifier": personeltest.EmailDanruSatu, "password": "x", "client": "DESKTOP"},
			http.StatusUnprocessableEntity, "client", "INVALID"},
		{"an identifier that is not an email", map[string]any{"identifier": "username01", "password": "x", "client": "WEB"},
			http.StatusUnprocessableEntity, "identifier", "INVALID"},
		{"no identifier", map[string]any{"password": "x", "client": "WEB"},
			http.StatusUnprocessableEntity, "identifier", "REQUIRED"},
		{"no password", map[string]any{"identifier": personeltest.EmailDanruSatu, "client": "WEB"},
			http.StatusUnprocessableEntity, "password", "REQUIRED"},
		{"an unknown field", map[string]any{"identifier": personeltest.EmailDanruSatu, "password": "x", "client": "WEB", "role": "ADMIN"},
			http.StatusUnprocessableEntity, "role", "INVALID"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t, "")
			res := e.post(tc.body, nil)
			require.Equal(t, tc.status, res.Status, "body: %s", res.Raw)
			got := res.err(t)
			require.Equal(t, "VALIDATION_ERROR", got.Code)
			require.Len(t, got.Errors, 1)
			require.Equal(t, tc.field, got.Errors[0].Field)
			require.Equal(t, tc.fieldCode, got.Errors[0].Code)
			require.Zero(t, e.fake.CallCount("/login"), "Personalia was not asked")
		})
	}
}

func TestADeviceTokenIsIgnoredForWebLogin(t *testing.T) {
	e := newEnv(t, "")
	body := web(personeltest.EmailDanruSatu)
	body["device_token"] = phoneA
	e.login(body).data(t)
	require.Equal(t, 0, countRows(t, personeltest.OrgSatu, "device_bindings"))
}

func TestTheFirstPhoneBindsAndAnotherIsRefused(t *testing.T) {
	e := newEnv(t, "")
	org := int64(personeltest.OrgSatu)

	first := e.login(mobile(personeltest.EmailAnggota1Satu, phoneA)).data(t)
	require.Equal(t, 1, countRows(t, org, "device_bindings"))
	require.Equal(t, auth.HashToken(phoneA),
		scalar[string](t, org, "SELECT device_token_hash FROM device_bindings WHERE personnel_id = $1", first.User.ID),
		"only the hash of the device token is stored")
	require.Equal(t, auth.HashToken(phoneA),
		scalar[string](t, org, "SELECT device_token_hash FROM sessions WHERE personnel_id = $1", first.User.ID),
		"the session remembers which phone it logged in with")
	require.Equal(t, "MOBILE", scalar[string](t, org, "SELECT client::text FROM sessions WHERE personnel_id = $1", first.User.ID))

	t.Run("the same phone logs in again", func(t *testing.T) {
		e.login(mobile(personeltest.EmailAnggota1Satu, phoneA)).data(t)
		require.Equal(t, 1, countRows(t, org, "device_bindings"))
		require.Equal(t, 2, countRows(t, org, "sessions"))
	})

	t.Run("another phone is refused and creates no session", func(t *testing.T) {
		sessionsBefore := countRows(t, org, "sessions")
		res := e.login(mobile(personeltest.EmailAnggota1Satu, phoneB))

		require.Equal(t, http.StatusForbidden, res.Status)
		got := res.err(t)
		require.Equal(t, "FORBIDDEN", got.Code)
		require.Equal(t, identity.ReasonDeviceBound, got.reason())
		require.Equal(t, "Akun Anda terhubung dengan perangkat lain. Hubungi admin untuk ganti perangkat.", got.Message)
		require.Equal(t, sessionsBefore, countRows(t, org, "sessions"))
		require.Equal(t, auth.HashToken(phoneA),
			scalar[string](t, org, "SELECT device_token_hash FROM device_bindings WHERE personnel_id = $1", first.User.ID),
			"the first phone keeps the binding")
	})

	t.Run("a bound phone is not a failed login, so it never trips the rate limit", func(t *testing.T) {
		for range 12 {
			require.Equal(t, http.StatusForbidden, e.login(mobile(personeltest.EmailAnggota1Satu, phoneB)).Status)
		}
	})

	t.Run("the first phone still logs in", func(t *testing.T) {
		e.login(mobile(personeltest.EmailAnggota1Satu, phoneA)).data(t)
	})

	t.Run("web login is not blocked by the phone binding and binds nothing", func(t *testing.T) {
		e.login(web(personeltest.EmailAnggota1Satu)).data(t)
		require.Equal(t, 1, countRows(t, org, "device_bindings"))
	})
}

func TestAWrongPasswordNeverBindsAPhone(t *testing.T) {
	e := newEnv(t, "")
	res := e.login(withPassword(mobile(personeltest.EmailAnggota2Satu, phoneA), wrongPass))
	require.Equal(t, http.StatusUnauthorized, res.Status)
	require.Equal(t, 0, countRows(t, personeltest.OrgSatu, "device_bindings"), "a guesser cannot claim a phone slot")
}

func TestWebLoginBindsNothing(t *testing.T) {
	e := newEnv(t, "")
	data := e.login(web(personeltest.EmailAnggota1Dua)).data(t)

	org := int64(personeltest.OrgDua)
	require.Equal(t, 0, countRows(t, org, "device_bindings"))
	require.Equal(t, 1, scalar[int](t, org, "SELECT count(*) FROM sessions WHERE personnel_id = $1 AND device_token_hash IS NULL AND client = 'WEB'", data.User.ID))
}

func TestTwoPhonesLoggingInTogetherBindOnlyOne(t *testing.T) {
	e := newEnv(t, "")
	results := make(chan int, 2)
	for _, device := range []string{phoneA, phoneB} {
		go func() {
			raw, err := json.Marshal(mobile(personeltest.EmailAnggota2Satu, device))
			if err != nil {
				results <- -1
				return
			}
			req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, e.srv+"/api/v1"+loginPath, bytes.NewReader(raw))
			if err != nil {
				results <- -1
				return
			}
			req.Header.Set("Content-Type", "application/json")
			r, err := http.DefaultClient.Do(req)
			if err != nil {
				results <- -1
				return
			}
			_ = r.Body.Close()
			results <- r.StatusCode
		}()
	}
	a, b := <-results, <-results
	require.ElementsMatch(t, []int{http.StatusOK, http.StatusForbidden}, []int{a, b}, "one phone wins, the other is refused")
	require.Equal(t, 1, countRows(t, personeltest.OrgSatu, "device_bindings"))
	require.Equal(t, 1, countRows(t, personeltest.OrgSatu, "sessions"))
}

func TestLoginAsksPersonaliaExactlyOnce(t *testing.T) {
	e := newEnv(t, "")
	e.login(mobile(personeltest.EmailAnggota1Satu, phoneA)).data(t)
	require.Equal(t, 1, e.fake.CallCount("/login"))

	e.login(withPassword(mobile(personeltest.EmailAnggota1Satu, phoneA), wrongPass))
	require.Equal(t, 2, e.fake.CallCount("/login"), "a failed login is not retried: that would be a second failed attempt")

	// The fake refuses a body with a field it does not know, so the successful logins above
	// already prove that no device token is sent to Personalia.
	for _, c := range e.fake.Calls() {
		require.Equal(t, http.MethodPost, c.Method)
		require.True(t, strings.HasSuffix(c.Path, "/login"))
	}
}

func TestAccountsThatMayNotSignIn(t *testing.T) {
	tests := []struct{ name, email string }{
		{"a deactivated employee", personeltest.EmailNonaktifSatu},
		{"an account without an employee record", personeltest.EmailNoNIK},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t, "")
			res := e.login(mobile(tc.email, phoneA))

			require.Equal(t, http.StatusForbidden, res.Status)
			got := res.err(t)
			require.Equal(t, "FORBIDDEN", got.Code)
			require.Equal(t, identity.ReasonAccountDisabled, got.reason())
			require.Equal(t, 0, countRows(t, personeltest.OrgSatu, "personnels"))
			require.Equal(t, 0, countRows(t, personeltest.OrgSatu, "device_bindings"))
		})
	}
}

func TestADeactivationInPersonaliaStopsTheNextLogin(t *testing.T) {
	e := newEnv(t, "")
	e.login(web(personeltest.EmailAnggota2Satu)).data(t)

	e.fake.Deactivate(personeltest.NIKAnggota2Satu)
	res := e.login(web(personeltest.EmailAnggota2Satu))
	require.Equal(t, http.StatusForbidden, res.Status)
	require.Equal(t, identity.ReasonAccountDisabled, res.err(t).reason())
}

func TestASuspendedOrganizationCannotSignIn(t *testing.T) {
	e := newEnv(t, "")
	e.login(web(personeltest.EmailDanruSatu)).data(t)
	sessions := countRows(t, personeltest.OrgSatu, "sessions")

	_, err := testkit.OwnerPool(t).Exec(t.Context(), "UPDATE organizations SET status = 'SUSPENDED' WHERE id = $1", personeltest.OrgSatu)
	require.NoError(t, err)

	res := e.login(web(personeltest.EmailDanruSatu))
	require.Equal(t, http.StatusForbidden, res.Status)
	got := res.err(t)
	require.Equal(t, "FORBIDDEN", got.Code)
	require.Equal(t, identity.ReasonOrganizationSuspended, got.reason())
	require.Equal(t, sessions, countRows(t, personeltest.OrgSatu, "sessions"))

	// ⚠ A login must not undo the operator's decision: the upsert leaves status alone.
	var status string
	require.NoError(t, testkit.Pool(t).QueryRow(t.Context(), "SELECT status::text FROM organizations WHERE id = $1", personeltest.OrgSatu).Scan(&status))
	require.Equal(t, "SUSPENDED", status)

	t.Run("another organization is unaffected", func(t *testing.T) {
		e.login(web(personeltest.EmailDanruDua)).data(t)
	})
}

func TestPersonaliaBeingDownIsA503(t *testing.T) {
	e := newEnv(t, "")
	e.fake.Down()

	res := e.login(web(personeltest.EmailDanruSatu))
	require.Equal(t, http.StatusServiceUnavailable, res.Status)
	got := res.err(t)
	require.Equal(t, "INTERNAL_ERROR", got.Code)
	require.Equal(t, apperr.ReasonPersonelUnavailable, got.reason())
	require.Equal(t, 0, countRows(t, personeltest.OrgSatu, "sessions"))

	e.fake.Up()
	e.login(web(personeltest.EmailDanruSatu)).data(t)
}

func TestAWrongIntegrationKeyIsNeverReportedAsWrongCredentials(t *testing.T) {
	e := newEnv(t, "")
	e.fake.Key = "rotated-on-the-personalia-side"

	res := e.login(web(personeltest.EmailDanruSatu))
	require.Equal(t, http.StatusServiceUnavailable, res.Status, "a configuration error is an outage, not the person's mistake")
	require.Equal(t, apperr.ReasonPersonelUnavailable, res.err(t).reason())
}

func TestTheSameNIKInTwoOrganizationsIsTwoPeople(t *testing.T) {
	e := newEnv(t, "")
	satu, dua := int64(personeltest.OrgSatu), int64(personeltest.OrgDua)

	inSatu := e.login(web(personeltest.EmailAnggota1Satu)).data(t)
	e.fake.MoveToCompany(personeltest.NIKAnggota1Satu, personeltest.OrgDua)
	inDua := e.login(web(personeltest.EmailAnggota1Satu)).data(t)

	require.NotEqual(t, inSatu.User.ID, inDua.User.ID, "two rows")
	require.EqualValues(t, satu, inSatu.User.OrganizationID)
	require.EqualValues(t, dua, inDua.User.OrganizationID)

	tokens := testkit.Tokens(t)
	claimsSatu, err := tokens.ParseAccessToken(inSatu.AccessToken)
	require.NoError(t, err)
	claimsDua, err := tokens.ParseAccessToken(inDua.AccessToken)
	require.NoError(t, err)
	require.Equal(t, satu, claimsSatu.OrganizationID)
	require.Equal(t, dua, claimsDua.OrganizationID)

	// Each token's tenant sees its own row with that NIK, and not the other's.
	nik := personeltest.NIKAnggota1Satu
	require.Equal(t, inSatu.User.ID, scalar[int64](t, claimsSatu.OrganizationID, "SELECT id FROM personnels WHERE nik = $1", nik))
	require.Equal(t, inDua.User.ID, scalar[int64](t, claimsDua.OrganizationID, "SELECT id FROM personnels WHERE nik = $1", nik))
	require.Equal(t, 1, countRows(t, satu, "personnels"))
	require.Equal(t, 1, countRows(t, dua, "personnels"))
	require.Equal(t, 0, scalar[int](t, satu, "SELECT count(*) FROM sessions WHERE personnel_id = $1", inDua.User.ID))
	require.Equal(t, 0, scalar[int](t, dua, "SELECT count(*) FROM sessions WHERE personnel_id = $1", inSatu.User.ID))
}

func TestNoCredentialsAreStored(t *testing.T) {
	testkit.Reset(t)
	var columns []string
	rows, err := testkit.OwnerPool(t).Query(t.Context(), `
		SELECT table_name || '.' || column_name FROM information_schema.columns
		WHERE table_schema = 'public' AND table_name IN ('personnels', 'sessions', 'device_bindings')
		  AND (column_name ILIKE '%password%' OR column_name ILIKE '%birth%' OR column_name ILIKE '%passwd%')`)
	require.NoError(t, err)
	columns, err = pgx.CollectRows(rows, pgx.RowTo[string])
	require.NoError(t, err)
	require.Empty(t, columns, "LOCK F2: no user store, no credentials, no birth date")
}

// ⚠ The password, the device token and the integration key must not appear anywhere SiAPP
// writes: not in a response, not in a log line.
func TestSecretsNeverAppearInResponsesOrLogs(t *testing.T) {
	e := newEnv(t, "")
	e.login(mobile(personeltest.EmailAnggota1Satu, phoneA)).data(t)
	e.login(withPassword(mobile(personeltest.EmailAnggota1Satu, phoneA), wrongPass))
	e.login(mobile(personeltest.EmailAnggota1Satu, phoneB))
	e.login(mobile(personeltest.EmailNonaktifSatu, phoneB))
	e.post(map[string]any{"identifier": "x", "password": wrongPass, "client": "MOBILE", "device_token": phoneB}, nil)
	e.fake.Down()
	e.login(mobile(personeltest.EmailAnggota2Satu, phoneB))
	e.fake.Up()

	everything := strings.Join(e.bodies, "\n") + "\n" + e.logs.String()
	require.NotEmpty(t, e.logs.String(), "the access log ran, so the check below means something")
	for name, secret := range map[string]string{
		"the password":         personeltest.Password,
		"a wrong password":     wrongPass,
		"a device token":       phoneA,
		"another device token": phoneB,
		"the integration key":  e.fake.Key,
	} {
		require.NotContains(t, everything, secret, "%s leaked into a response or a log", name)
	}
}
