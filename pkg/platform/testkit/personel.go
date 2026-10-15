package testkit

import (
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"siapp/pkg/platform/personel"
	"siapp/pkg/platform/personel/personeltest"
)

// NewPersonel starts the in-process fake Personalia and returns it with a real
// personel.Client pointed at it, so a module under test goes through the same HTTP code as
// production. Retries have no backoff, to keep tests fast.
//
// It is the only stand-in for Personalia: "against the stub" in the plan means this fake.
// Pass the client to the module's constructor in Deps.Mount, and use the fake's hooks
// (Deactivate, SetDomain, FailNext, Down, ...) to stage what Personalia does.
func NewPersonel(t testing.TB) (*personeltest.Fake, *personel.Client) {
	t.Helper()
	fake := personeltest.New()
	srv := httptest.NewServer(fake.Handler())
	t.Cleanup(srv.Close)

	noBackoff := time.Duration(0)
	client, err := personel.New(personel.Config{
		BaseURL: srv.URL + personeltest.BasePath,
		Key:     fake.Key,
		Backoff: &noBackoff,
	})
	require.NoError(t, err)
	return fake, client
}
