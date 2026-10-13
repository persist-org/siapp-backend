package personel_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"

	"siapp/pkg/platform/personel"
	"siapp/pkg/platform/personel/personeltest"
	"siapp/pkg/platform/testkit"
)

func TestCachedServesTheSecondCallFromRedis(t *testing.T) {
	ctx := context.Background()
	rdb := testkit.Redis(t)
	fake, client := testkit.NewPersonel(t)
	cached := personel.NewCached(client, fake, rdb, nil)

	orgID, nik := personeltest.OrgSatu, personeltest.NIKDanruSatu
	k := fmt.Sprintf("org:%d:personel:%s:%s", orgID, personel.KindEmployee, nik)
	require.NoError(t, rdb.Del(ctx, k).Err())
	t.Cleanup(func() { _ = rdb.Del(context.Background(), k).Err() })

	first, err := cached.EmployeeByNIK(ctx, orgID, nik)
	require.NoError(t, err)
	require.Equal(t, 1, fake.CallCount("/employees"))

	second, err := cached.EmployeeByNIK(ctx, orgID, nik)
	require.NoError(t, err)
	require.Equal(t, first, second)
	require.Equal(t, 1, fake.CallCount("/employees"), "the second call did not reach Personalia")

	ttl, err := rdb.TTL(ctx, k).Result()
	require.NoError(t, err)
	require.Positive(t, ttl)
	require.LessOrEqual(t, ttl.Minutes(), 10.0)

	// HR deactivates the person. Until a webhook arrives the cache still says active...
	fake.Deactivate(nik)
	stale, err := cached.EmployeeByNIK(ctx, orgID, nik)
	require.NoError(t, err)
	require.True(t, stale.Active)

	// ...and after Invalidate (what the webhook receiver does) the next call refetches.
	require.NoError(t, cached.Invalidate(ctx, orgID, personel.KindEmployee, nik))
	fresh, err := cached.EmployeeByNIK(ctx, orgID, nik)
	require.NoError(t, err)
	require.False(t, fresh.Active)
	require.Equal(t, 2, fake.CallCount("/employees"))
}

func TestCachedBatchFetchesOnlyTheMissing(t *testing.T) {
	ctx := context.Background()
	rdb := testkit.Redis(t)
	fake, client := testkit.NewPersonel(t)
	cached := personel.NewCached(client, fake, rdb, nil)

	orgID := personeltest.OrgDua
	keys := []string{
		fmt.Sprintf("org:%d:personel:employee:%s", orgID, personeltest.NIKDanruDua),
		fmt.Sprintf("org:%d:personel:employee:%s", orgID, personeltest.NIKAnggota1Dua),
	}
	require.NoError(t, rdb.Del(ctx, keys...).Err())
	t.Cleanup(func() { _ = rdb.Del(context.Background(), keys...).Err() })

	_, err := cached.EmployeeByNIK(ctx, orgID, personeltest.NIKDanruDua)
	require.NoError(t, err)

	got, err := cached.EmployeesByNIKs(ctx, orgID, []string{personeltest.NIKDanruDua, personeltest.NIKAnggota1Dua, "NOPE0001"})
	require.NoError(t, err)
	require.Len(t, got, 2)
	require.Equal(t, 2, fake.CallCount("/employees"), "one single-NIK call, then one batch for the one missing NIK")

	// Both are cached now; "not found" is not.
	_, err = cached.EmployeesByNIKs(ctx, orgID, []string{personeltest.NIKDanruDua, personeltest.NIKAnggota1Dua})
	require.NoError(t, err)
	require.Equal(t, 2, fake.CallCount("/employees"))
}

func TestCachedKeysAreScopedByOrganization(t *testing.T) {
	ctx := context.Background()
	rdb := testkit.Redis(t)
	fake, client := testkit.NewPersonel(t)
	cached := personel.NewCached(client, fake, rdb, nil)

	nik := personeltest.NIKDanruSatu
	k := fmt.Sprintf("org:%d:personel:employee:%s", personeltest.OrgSatu, nik)
	require.NoError(t, rdb.Del(ctx, k).Err())
	t.Cleanup(func() { _ = rdb.Del(context.Background(), k).Err() })

	_, err := cached.EmployeeByNIK(ctx, personeltest.OrgSatu, nik)
	require.NoError(t, err)
	// Another tenant asking for the same NIK must not be served org Satu's entry.
	_, err = cached.EmployeeByNIK(ctx, personeltest.OrgDua, nik)
	require.ErrorIs(t, err, personel.ErrNotFound)
}

func TestCachedErrorsAreNotCached(t *testing.T) {
	ctx := context.Background()
	rdb := testkit.Redis(t)
	fake, client := testkit.NewPersonel(t)
	cached := personel.NewCached(client, fake, rdb, nil)

	k := fmt.Sprintf("org:%d:personel:employee:%s", personeltest.OrgSatu, personeltest.NIKAnggota2Satu)
	require.NoError(t, rdb.Del(ctx, k).Err())
	t.Cleanup(func() { _ = rdb.Del(context.Background(), k).Err() })

	fake.Down()
	_, err := cached.EmployeeByNIK(ctx, personeltest.OrgSatu, personeltest.NIKAnggota2Satu)
	require.ErrorIs(t, err, personel.ErrUnavailable)
	fake.Up()
	_, err = cached.EmployeeByNIK(ctx, personeltest.OrgSatu, personeltest.NIKAnggota2Satu)
	require.NoError(t, err)
}

func TestCachedOrg(t *testing.T) {
	ctx := context.Background()
	rdb := testkit.Redis(t)
	fake, client := testkit.NewPersonel(t)
	cached := personel.NewCached(client, fake, rdb, nil)

	k := fmt.Sprintf("org:%d:personel:%s:all", personeltest.OrgSatu, personel.KindPositions)
	require.NoError(t, rdb.Del(ctx, k).Err())
	t.Cleanup(func() { _ = rdb.Del(context.Background(), k).Err() })

	first, err := cached.Positions(ctx, personeltest.OrgSatu)
	require.NoError(t, err)
	require.NotEmpty(t, first)
	fake.Down() // a cached read survives an outage
	second, err := cached.Positions(ctx, personeltest.OrgSatu)
	require.NoError(t, err)
	require.Equal(t, first, second)

	noOrg := personel.NewCached(client, nil, rdb, nil)
	_, err = noOrg.Projects(ctx, personeltest.OrgDua)
	require.ErrorIs(t, err, personel.ErrUnavailable)
}
