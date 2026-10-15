package personel_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"siapp/pkg/platform/personel"
	"siapp/pkg/platform/personel/personeltest"
	"siapp/pkg/platform/testkit"
)

func login(c personel.Auth, email, password string) (personel.Identity, error) {
	return c.Login(context.Background(), personel.LoginInput{Email: email, Password: password, ClientIP: "10.0.0.7", UserAgent: "siapp-test"})
}

func TestLoginReturnsIdentityForBothDomains(t *testing.T) {
	_, c := testkit.NewPersonel(t)

	siapp, err := login(c, personeltest.EmailAnggota1Satu, personeltest.Password)
	require.NoError(t, err)
	require.Equal(t, personel.DomainSiAPP, siapp.Domain)
	require.Equal(t, personeltest.NIKAnggota1Satu, siapp.NIK)
	require.EqualValues(t, personeltest.OrgSatu, siapp.OrganizationID, "the employing company is the tenant")
	require.EqualValues(t, personeltest.ClientMall, siapp.ClientID, "the placement client is not the tenant")
	require.Equal(t, "Anggota Satpam", siapp.PositionName)
	require.True(t, siapp.Active)
	require.Empty(t, siapp.Phone, "null in JSON is an empty string")

	both, err := login(c, personeltest.EmailSupervisorSatu, personeltest.Password)
	require.NoError(t, err)
	require.Equal(t, personel.DomainPersonalia, both.Domain)

	withPhone, err := login(c, personeltest.EmailDanruSatu, personeltest.Password)
	require.NoError(t, err)
	require.Equal(t, "0812 0000 0001", withPhone.Phone)
}

func TestLoginMapsFailuresToTypedErrors(t *testing.T) {
	fake, c := testkit.NewPersonel(t)

	_, err := login(c, personeltest.EmailDanruSatu, "wrong")
	require.ErrorIs(t, err, personel.ErrInvalidCredentials)
	_, err = login(c, "nobody@siapp.test", personeltest.Password)
	require.ErrorIs(t, err, personel.ErrInvalidCredentials, "an unknown email looks the same as a wrong password")

	_, err = login(c, personeltest.EmailNonaktifSatu, personeltest.Password)
	require.ErrorIs(t, err, personel.ErrAccountDisabled)
	var disabled *personel.DisabledError
	require.ErrorAs(t, err, &disabled)
	require.Equal(t, personel.DisabledInactive, disabled.Reason)

	_, err = login(c, personeltest.EmailNoNIK, personeltest.Password)
	require.ErrorIs(t, err, personel.ErrAccountDisabled)
	require.ErrorAs(t, err, &disabled)
	require.Equal(t, personel.DisabledWorkMissing, disabled.Reason)

	// The 11th failure is refused even with the right password.
	for range 10 {
		_, err = login(c, personeltest.EmailAnggota2Satu, "wrong")
		require.ErrorIs(t, err, personel.ErrInvalidCredentials)
	}
	_, err = login(c, personeltest.EmailAnggota2Satu, personeltest.Password)
	require.ErrorIs(t, err, personel.ErrRateLimited)

	// Empty input, or an email without "@", never reaches Personalia.
	before := fake.CallCount("/login")
	_, err = login(c, "", "")
	require.ErrorIs(t, err, personel.ErrInvalidCredentials)
	_, err = login(c, "danru.satu", personeltest.Password)
	require.ErrorIs(t, err, personel.ErrInvalidCredentials)
	require.Equal(t, before, fake.CallCount("/login"))

	// A malformed email Personalia refuses with 422 is a wrong email, not an outage.
	_, err = login(c, "danru.satu@", personeltest.Password)
	require.ErrorIs(t, err, personel.ErrInvalidCredentials)
	require.NotErrorIs(t, err, personel.ErrUnavailable)
	require.Equal(t, before+1, fake.CallCount("/login"))

	// Spaces a phone keyboard adds around the address are not the person's mistake.
	_, err = login(c, " "+personeltest.EmailDanruSatu+" ", personeltest.Password)
	require.NoError(t, err)
}

func TestLoginCutsDeviceDetailsToPersonaliaLimits(t *testing.T) {
	_, c := testkit.NewPersonel(t)

	// The fake refuses values over Personalia's limits with 422, like the real API.
	_, err := c.Login(context.Background(), personel.LoginInput{
		Email: personeltest.EmailDanruSatu, Password: personeltest.Password,
		ClientIP: strings.Repeat("1", 100), UserAgent: strings.Repeat("é", 600),
	})
	require.NoError(t, err, "over-long device details must not block the login")
}

func TestA422ThatIsNotThePersonsInputIsUnavailable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = w.Write([]byte(`{"data":null,"error":{"user_agent":["too long"]},"message":"Data tidak valid."}`))
	}))
	t.Cleanup(srv.Close)
	c, err := personel.New(personel.Config{BaseURL: srv.URL, Key: "k"})
	require.NoError(t, err)

	_, err = login(c, personeltest.EmailDanruSatu, personeltest.Password)
	require.ErrorIs(t, err, personel.ErrUnavailable, "our request broke the contract: not the person's fault")
	require.NotErrorIs(t, err, personel.ErrInvalidCredentials)
}

func TestLoginIsNeverRetried(t *testing.T) {
	fake, c := testkit.NewPersonel(t)

	fake.FailNext(503)
	_, err := login(c, personeltest.EmailDanruSatu, personeltest.Password)
	require.ErrorIs(t, err, personel.ErrUnavailable)
	require.Equal(t, 1, fake.CallCount("/login"), "a retry would be a second attempt against the account")

	// And the next login works: the failed one did not count against the account.
	_, err = login(c, personeltest.EmailDanruSatu, personeltest.Password)
	require.NoError(t, err)
	require.Equal(t, 2, fake.CallCount("/login"))
}

func TestWrongIntegrationKeyIsAConfigurationErrorNotBadCredentials(t *testing.T) {
	fake := personeltest.New()
	srv := httptest.NewServer(fake.Handler())
	t.Cleanup(srv.Close)
	zero := time.Duration(0)
	c, err := personel.New(personel.Config{BaseURL: srv.URL + personeltest.BasePath, Key: "not-the-key", Backoff: &zero})
	require.NoError(t, err)

	_, err = login(c, personeltest.EmailDanruSatu, personeltest.Password)
	require.ErrorIs(t, err, personel.ErrUnavailable)
	require.NotErrorIs(t, err, personel.ErrInvalidCredentials)

	_, err = c.EmployeeByNIK(context.Background(), personeltest.OrgSatu, personeltest.NIKDanruSatu)
	require.ErrorIs(t, err, personel.ErrUnavailable)
	require.NotErrorIs(t, err, personel.ErrNotFound)
	require.Equal(t, 1, fake.CallCount("/employees"), "a configuration error is not retried")
}

func TestNewRejectsBadConfig(t *testing.T) {
	_, err := personel.New(personel.Config{BaseURL: "localhost:8081", Key: "k"})
	require.Error(t, err)
	_, err = personel.New(personel.Config{BaseURL: "http://localhost:8081", Key: ""})
	require.Error(t, err)
}

func TestDirectory(t *testing.T) {
	ctx := context.Background()
	fake, c := testkit.NewPersonel(t)

	t.Run("one batch: known, unknown and inactive", func(t *testing.T) {
		got, err := c.EmployeesByNIKs(ctx, personeltest.OrgSatu, []string{
			personeltest.NIKDanruSatu, "NOPE0001", personeltest.NIKNonaktifSatu, personeltest.NIKDanruSatu,
		})
		require.NoError(t, err)
		byNIK := map[string]personel.Identity{}
		for _, id := range got {
			byNIK[id.NIK] = id
		}
		require.Len(t, byNIK, 2, "unknown omitted, duplicate collapsed")
		require.True(t, byNIK[personeltest.NIKDanruSatu].Active)
		require.False(t, byNIK[personeltest.NIKNonaktifSatu].Active, "inactive people are returned, flagged")
	})

	t.Run("another organization's people are not found", func(t *testing.T) {
		_, err := c.EmployeeByNIK(ctx, personeltest.OrgSatu, personeltest.NIKDanruDua)
		require.ErrorIs(t, err, personel.ErrNotFound)
		got, err := c.EmployeesByNIKs(ctx, personeltest.OrgSatu, []string{personeltest.NIKDanruDua})
		require.NoError(t, err)
		require.Empty(t, got)
	})

	t.Run("a person who moved company follows the move", func(t *testing.T) {
		fake.MoveToCompany(personeltest.NIKAnggota2Satu, personeltest.OrgDua)
		_, err := c.EmployeeByNIK(ctx, personeltest.OrgSatu, personeltest.NIKAnggota2Satu)
		require.ErrorIs(t, err, personel.ErrNotFound)
		id, err := c.EmployeeByNIK(ctx, personeltest.OrgDua, personeltest.NIKAnggota2Satu)
		require.NoError(t, err)
		require.EqualValues(t, personeltest.OrgDua, id.OrganizationID)
	})

	t.Run("more than 200 NIKs are split into batches", func(t *testing.T) {
		niks := make([]string, 450)
		for i := range niks {
			niks[i] = fmt.Sprintf("BULK%04d", i)
		}
		niks[0] = personeltest.NIKDanruSatu
		before := fake.CallCount("/employees")
		got, err := c.EmployeesByNIKs(ctx, personeltest.OrgSatu, niks)
		require.NoError(t, err)
		require.Len(t, got, 1)
		require.Equal(t, 3, fake.CallCount("/employees")-before, "450 NIKs are 3 requests")
	})

	t.Run("a NIK longer than Personalia accepts is not found and does not fail the batch", func(t *testing.T) {
		got, err := c.EmployeesByNIKs(ctx, personeltest.OrgSatu, []string{strings.Repeat("9", 101), personeltest.NIKDanruSatu})
		require.NoError(t, err)
		require.Len(t, got, 1)
		require.Equal(t, personeltest.NIKDanruSatu, got[0].NIK)
	})

	t.Run("no NIKs is no request", func(t *testing.T) {
		before := fake.CallCount("/employees")
		got, err := c.EmployeesByNIKs(ctx, personeltest.OrgSatu, nil)
		require.NoError(t, err)
		require.Empty(t, got)
		require.Equal(t, before, fake.CallCount("/employees"))
	})
}

func TestDirectoryRetriesAnOutageThenSucceeds(t *testing.T) {
	fake, c := testkit.NewPersonel(t)

	fake.FailNext(503)
	fake.FailNext(502)
	id, err := c.EmployeeByNIK(context.Background(), personeltest.OrgSatu, personeltest.NIKDanruSatu)
	require.NoError(t, err)
	require.Equal(t, personeltest.NIKDanruSatu, id.NIK)
	require.Equal(t, 3, fake.CallCount("/employees"), "two failures, then the third attempt succeeds")

	// Three failures in a row exhaust the retries.
	fake.FailNext(503)
	fake.FailNext(503)
	fake.FailNext(503)
	_, err = c.EmployeeByNIK(context.Background(), personeltest.OrgSatu, personeltest.NIKDanruSatu)
	require.ErrorIs(t, err, personel.ErrUnavailable)
}

// manualClock is a clock the test advances.
type manualClock struct {
	mu sync.Mutex
	t  time.Time
}

func (m *manualClock) Now() time.Time { m.mu.Lock(); defer m.mu.Unlock(); return m.t }
func (m *manualClock) Advance(d time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.t = m.t.Add(d)
}

func TestCircuitBreakerOpensAndRecovers(t *testing.T) {
	ctx := context.Background()
	fake := personeltest.New()
	srv := httptest.NewServer(fake.Handler())
	t.Cleanup(srv.Close)
	clk := &manualClock{t: time.Date(2026, 3, 10, 8, 0, 0, 0, time.UTC)}
	zero := time.Duration(0)
	c, err := personel.New(personel.Config{BaseURL: srv.URL + personeltest.BasePath, Key: fake.Key, Clock: clk, Backoff: &zero})
	require.NoError(t, err)

	lookup := func() error {
		_, err := c.EmployeeByNIK(ctx, personeltest.OrgSatu, personeltest.NIKDanruSatu)
		return err
	}

	fake.Down()
	for range 5 {
		require.ErrorIs(t, lookup(), personel.ErrUnavailable)
	}
	reached := len(fake.Calls())

	// Open: calls fail at once, without touching Personalia, logins included.
	require.ErrorIs(t, lookup(), personel.ErrUnavailable)
	_, err = login(c, personeltest.EmailDanruSatu, personeltest.Password)
	require.ErrorIs(t, err, personel.ErrUnavailable)
	require.Len(t, fake.Calls(), reached, "an open breaker sends nothing")

	// After the cooldown one probe goes out; it fails, so the breaker stays open.
	clk.Advance(31 * time.Second)
	require.ErrorIs(t, lookup(), personel.ErrUnavailable)
	require.Greater(t, len(fake.Calls()), reached)
	reached = len(fake.Calls())
	require.ErrorIs(t, lookup(), personel.ErrUnavailable)
	require.Len(t, fake.Calls(), reached, "the failed probe reopened it for another cooldown")

	// Personalia recovers; the next probe succeeds and closes the breaker.
	fake.Up()
	clk.Advance(31 * time.Second)
	require.NoError(t, lookup())
	require.NoError(t, lookup())
	_, err = login(c, personeltest.EmailDanruSatu, personeltest.Password)
	require.NoError(t, err)
}

// hangingPersonalia serves fake, except while the returned flag is set: then it answers
// nothing until the client goes away.
func hangingPersonalia(t *testing.T, fake *personeltest.Fake) (*httptest.Server, *atomic.Bool) {
	t.Helper()
	var hang atomic.Bool
	stop := make(chan struct{})
	h := fake.Handler()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hang.Load() {
			// The server notices a client that went away only once the body is read.
			_, _ = io.Copy(io.Discard, r.Body)
			select {
			case <-r.Context().Done():
			case <-stop:
			}
			return
		}
		h.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(stop) }) // runs first: Close waits for the handlers
	return srv, &hang
}

func TestCallerGivingUpDoesNotOpenTheBreaker(t *testing.T) {
	fake := personeltest.New()
	srv, hang := hangingPersonalia(t, fake)
	zero := time.Duration(0)
	c, err := personel.New(personel.Config{BaseURL: srv.URL + personeltest.BasePath, Key: fake.Key, Backoff: &zero})
	require.NoError(t, err)

	// Twice the threshold of callers whose own deadline runs out (phones closing the app
	// during login, request deadlines), and one that cancels.
	hang.Store(true)
	for range 10 {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
		_, err := c.Login(ctx, personel.LoginInput{Email: personeltest.EmailDanruSatu, Password: personeltest.Password})
		cancel()
		require.ErrorIs(t, err, personel.ErrUnavailable)
		require.ErrorIs(t, err, context.DeadlineExceeded, "the caller can tell it gave up")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = c.EmployeeByNIK(ctx, personeltest.OrgSatu, personeltest.NIKDanruSatu)
	require.ErrorIs(t, err, context.Canceled)

	hang.Store(false)
	_, err = login(c, personeltest.EmailDanruSatu, personeltest.Password)
	require.NoError(t, err, "Personalia was never down: the breaker must still be closed")
	require.Equal(t, 1, fake.CallCount("/login"))
}

func TestPersonaliaHangingPastTheClientTimeoutOpensTheBreaker(t *testing.T) {
	fake := personeltest.New()
	srv, hang := hangingPersonalia(t, fake)
	zero := time.Duration(0)
	c, err := personel.New(personel.Config{
		BaseURL: srv.URL + personeltest.BasePath, Key: fake.Key, Backoff: &zero,
		HTTPClient: &http.Client{Timeout: 20 * time.Millisecond},
	})
	require.NoError(t, err)

	// The client's own timeout leaves the caller's context alone: these are outages.
	hang.Store(true)
	for range 5 {
		_, err := login(c, personeltest.EmailDanruSatu, personeltest.Password)
		require.ErrorIs(t, err, personel.ErrUnavailable)
	}

	hang.Store(false)
	_, err = login(c, personeltest.EmailDanruSatu, personeltest.Password)
	require.ErrorIs(t, err, personel.ErrUnavailable)
	require.Zero(t, fake.CallCount("/login"), "the breaker is open: nothing was sent")
}

func TestAnAbandonedProbeDoesNotReopenTheBreaker(t *testing.T) {
	fake := personeltest.New()
	srv := httptest.NewServer(fake.Handler())
	t.Cleanup(srv.Close)
	clk := &manualClock{t: time.Date(2026, 3, 10, 8, 0, 0, 0, time.UTC)}
	zero := time.Duration(0)
	c, err := personel.New(personel.Config{BaseURL: srv.URL + personeltest.BasePath, Key: fake.Key, Clock: clk, Backoff: &zero})
	require.NoError(t, err)
	lookup := func(ctx context.Context) error {
		_, err := c.EmployeeByNIK(ctx, personeltest.OrgSatu, personeltest.NIKDanruSatu)
		return err
	}

	fake.Down()
	for range 5 {
		require.ErrorIs(t, lookup(context.Background()), personel.ErrUnavailable)
	}
	fake.Up()
	clk.Advance(31 * time.Second)

	// The probe's caller goes away: no outcome, so the probe slot is simply freed.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	require.ErrorIs(t, lookup(ctx), context.Canceled)

	require.NoError(t, lookup(context.Background()), "the next call probes at once and closes the breaker")
}

func TestClientErrorsDoNotOpenTheBreaker(t *testing.T) {
	fake, c := testkit.NewPersonel(t)
	for range 20 {
		_, err := login(c, personeltest.EmailDanruSatu, "wrong")
		if errors.Is(err, personel.ErrRateLimited) {
			continue // 429 is a 4xx too
		}
		require.ErrorIs(t, err, personel.ErrInvalidCredentials)
	}
	_, err := c.EmployeeByNIK(context.Background(), personeltest.OrgSatu, personeltest.NIKDanruSatu)
	require.NoError(t, err, "4xx answers mean Personalia is up")
	require.Positive(t, fake.CallCount("/employees"))
}

func TestDisabledErrorMatchesSentinel(t *testing.T) {
	err := error(&personel.DisabledError{Reason: personel.DisabledInactive})
	require.ErrorIs(t, err, personel.ErrAccountDisabled)
	require.NotErrorIs(t, err, personel.ErrInvalidCredentials)
}

func TestProposedSeamsAgainstTheFake(t *testing.T) {
	ctx := context.Background()
	fake, _ := testkit.NewPersonel(t)

	projects, err := fake.Projects(ctx, personeltest.OrgSatu)
	require.NoError(t, err)
	require.NotEmpty(t, projects)

	ev := personel.AttendanceEvent{Key: "k1", NIK: personeltest.NIKDanruSatu, Kind: "check_in", OccurredAt: time.Date(2026, 3, 10, 8, 0, 0, 0, time.UTC)}
	require.NoError(t, fake.PushEvents(ctx, personeltest.OrgSatu, []personel.AttendanceEvent{ev, ev}))
	require.Len(t, fake.Pushed(), 1, "the same Key is the same event")
	st, err := fake.Status(ctx, personeltest.OrgSatu, personeltest.NIKDanruSatu, ev.OccurredAt)
	require.NoError(t, err)
	require.True(t, st.CheckedIn)
	require.False(t, st.CheckedOut)

	rec, err := fake.Submit(ctx, personeltest.OrgSatu, personel.LeaveRequest{NIK: personeltest.NIKAnggota1Satu, LeaveTypeID: 2})
	require.NoError(t, err)
	require.Equal(t, personel.LeavePending, rec.State)
	require.NoError(t, fake.Decide(ctx, personeltest.OrgSatu, rec.ID, true, ""))
	approved, err := fake.List(ctx, personeltest.OrgSatu, personel.LeaveFilter{State: personel.LeaveApproved})
	require.NoError(t, err)
	require.Len(t, approved, 1)
	require.ErrorIs(t, fake.Decide(ctx, personeltest.OrgSatu, "nope", true, ""), personel.ErrNotFound)

	fake.Down()
	_, err = fake.Projects(ctx, personeltest.OrgSatu)
	require.ErrorIs(t, err, personel.ErrUnavailable)
}

func TestHooksStageWhatPersonaliaDoes(t *testing.T) {
	fake, c := testkit.NewPersonel(t)

	fake.SetDomain(personeltest.EmailAnggota1Satu, personel.DomainPersonalia)
	id, err := login(c, personeltest.EmailAnggota1Satu, personeltest.Password)
	require.NoError(t, err)
	require.Equal(t, personel.DomainPersonalia, id.Domain)

	fake.Deactivate(personeltest.NIKAnggota1Satu)
	_, err = login(c, personeltest.EmailAnggota1Satu, personeltest.Password)
	require.ErrorIs(t, err, personel.ErrAccountDisabled)
}
