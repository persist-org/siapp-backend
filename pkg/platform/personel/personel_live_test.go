//go:build personel_live

package personel_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"siapp/pkg/platform/personel"
	"siapp/pkg/platform/personel/personeltest"
	"siapp/pkg/platform/testkit"
)

// The contract test against a real kpi-personalia-api seeded with db/seed/40_siapp.sql:
//
//	PERSONEL_LIVE_URL=http://localhost:8081/api/v3/integrations/siapp \
//	PERSONEL_LIVE_KEY=<the key whose SHA-256 is in integrations.siapp.key_hashes> \
//	go test -tags personel_live -run Live ./pkg/platform/personel
//
// It runs the same assertions the fake's tests make, so a drift between the fake and the
// real API fails here. Run it whenever either side changes.
func liveClient(t *testing.T) *personel.Client {
	t.Helper()
	url, key := os.Getenv("PERSONEL_LIVE_URL"), os.Getenv("PERSONEL_LIVE_KEY")
	if url == "" || key == "" {
		t.Skip("PERSONEL_LIVE_URL and PERSONEL_LIVE_KEY are not set")
	}
	c, err := personel.New(personel.Config{BaseURL: url, Key: key})
	require.NoError(t, err)
	return c
}

func TestLiveLoginAndLookup(t *testing.T) {
	c := liveClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	for _, p := range []struct{ email, nik string }{
		{personeltest.EmailDanruSatu, personeltest.NIKDanruSatu},           // siapp domain
		{personeltest.EmailSupervisorSatu, personeltest.NIKSupervisorSatu}, // personalia domain
	} {
		got, err := c.Login(ctx, personel.LoginInput{Email: p.email, Password: personeltest.Password, ClientIP: "10.0.0.7", UserAgent: "siapp-live-test"})
		require.NoError(t, err, p.email)
		require.Equal(t, p.nik, got.NIK)
		require.True(t, got.Active)
		require.True(t, got.Domain.Valid())
		require.EqualValues(t, personeltest.OrgSatu, got.OrganizationID)

		looked, err := c.EmployeeByNIK(ctx, personeltest.OrgSatu, p.nik)
		require.NoError(t, err)
		require.Equal(t, got, looked, "login and lookup describe the person the same way")

		// The fake must say the same: that is the point of this test.
		_, fc := testkit.NewPersonel(t)
		fromFake, err := fc.Login(ctx, personel.LoginInput{Email: p.email, Password: personeltest.Password})
		require.NoError(t, err)
		require.Equal(t, fromFake.Domain, got.Domain)
		require.Equal(t, fromFake.OrganizationID, got.OrganizationID)
		require.Equal(t, fromFake.PositionName, got.PositionName)
	}

	_, err := c.Login(ctx, personel.LoginInput{Email: personeltest.EmailDanruSatu, Password: "definitely-wrong"})
	require.ErrorIs(t, err, personel.ErrInvalidCredentials)

	_, err = c.Login(ctx, personel.LoginInput{Email: personeltest.EmailNonaktifSatu, Password: personeltest.Password})
	require.ErrorIs(t, err, personel.ErrAccountDisabled)

	_, err = c.Login(ctx, personel.LoginInput{Email: personeltest.EmailNoNIK, Password: personeltest.Password})
	var disabled *personel.DisabledError
	require.ErrorAs(t, err, &disabled)
	require.Equal(t, personel.DisabledWorkMissing, disabled.Reason)

	got, err := c.EmployeesByNIKs(ctx, personeltest.OrgSatu, []string{personeltest.NIKNonaktifSatu, "NOPE-NOPE"})
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.False(t, got[0].Active)
}
