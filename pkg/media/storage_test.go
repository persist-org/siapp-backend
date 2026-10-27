package media_test

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/require"

	"siapp/pkg/media"
	"siapp/pkg/media/storagetest"
	"siapp/pkg/platform/clock"
	"siapp/pkg/platform/config"
)

// The same suite, against both drivers. The local one needs no service and always runs, in a
// temporary directory; the S3 one runs against the MinIO that `make up` starts.
func TestLocalStorageConformance(t *testing.T) {
	storagetest.Run(t, func(t *testing.T) media.Storage {
		srv := httptest.NewUnstartedServer(nil)
		t.Cleanup(srv.Close)
		local, err := media.NewLocal(localConfig(t.TempDir(), "http://"+srv.Listener.Addr().String()), clock.System{})
		require.NoError(t, err)

		router := chi.NewRouter()
		media.MountBlobs(router, local)
		srv.Config.Handler = router
		srv.Start()
		return local
	})
}

func TestS3StorageConformance(t *testing.T) {
	storagetest.Run(t, func(t *testing.T) media.Storage { return newMinIO(t, clock.System{}) })
}

func TestNewKey(t *testing.T) {
	// 02:00 on 1 November in WIB is still 31 October in UTC. Keys use UTC, like every timestamp.
	now := time.Date(2026, time.November, 1, 2, 0, 0, 0, time.FixedZone("WIB", 7*3600))

	key, err := media.NewKey(12, "PATROL_SCAN", now, 345)
	require.NoError(t, err)
	require.True(t, strings.HasPrefix(key, "12/PATROL_SCAN/2026/10/345-"), key)
	require.True(t, media.ValidKey(key))

	other, err := media.NewKey(12, "PATROL_SCAN", now, 345)
	require.NoError(t, err)
	require.NotEqual(t, key, other, "the random part makes every key unguessable and unique")

	_, err = media.NewKey(12, "not a purpose", now, 345)
	require.ErrorIs(t, err, media.ErrInvalidKey)
	_, err = media.NewKey(0, "REPORT", now, 345)
	require.ErrorIs(t, err, media.ErrInvalidKey, "an organization id is positive")
}

func TestValidKey(t *testing.T) {
	random := strings.Repeat("a1", 16)
	for key, want := range map[string]bool{
		"1/REPORT/2026/10/7-" + random:                  true,
		"123/PANIC_REPORT/2026/01/99-" + random:         true,
		"1/REPORT/2026/10/7-" + random + "0":            false,
		"1/REPORT/2026/10/7-" + strings.ToUpper(random): false,
		"0/REPORT/2026/10/7-" + random:                  false,
		"1/report/2026/10/7-" + random:                  false,
		"1/REPORT/26/10/7-" + random:                    false,
		"1/REPORT/2026/00/7-" + random:                  false,
		"1/REPORT/2026/10/0-" + random:                  false,
		"1/REPORT/2026/10/7_" + random:                  false,
		"../1/REPORT/2026/10/7-" + random:               false,
		"/1/REPORT/2026/10/7-" + random:                 false,
		"1/REPORT/2026/10/7-" + random + "/":            false,
		"":                                              false,
	} {
		require.Equal(t, want, media.ValidKey(key), key)
	}
}

// ⚠ The drivers prove they can work at start-up, so a bad setting fails the boot and not the
// first photo.
func TestNewLocalChecksTheDirectory(t *testing.T) {
	t.Run("a missing folder is created, parents included", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "var", "lib", "media")
		_, err := media.NewLocal(localConfig(dir, "http://localhost:8080"), clock.System{})
		require.NoError(t, err)
		require.DirExists(t, dir)
		require.Empty(t, files(t, dir), "the probe file is gone")
	})
	t.Run("a relative folder is resolved against the working directory", func(t *testing.T) {
		wd, err := os.Getwd()
		require.NoError(t, err)
		rel := filepath.Join("testdata-media", filepath.Base(t.TempDir()))
		t.Cleanup(func() { _ = os.RemoveAll(filepath.Join(wd, "testdata-media")) })

		_, err = media.NewLocal(localConfig(rel, "http://localhost:8080"), clock.System{})
		require.NoError(t, err)
		require.DirExists(t, filepath.Join(wd, rel))
	})
	t.Run("a folder nobody can write to fails", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("root can write anywhere")
		}
		dir := t.TempDir()
		require.NoError(t, os.Chmod(dir, 0o500))       //nolint:gosec // read-only on purpose
		t.Cleanup(func() { _ = os.Chmod(dir, 0o700) }) //nolint:gosec // so TempDir can remove it

		_, err := media.NewLocal(localConfig(dir, "http://localhost:8080"), clock.System{})
		require.Error(t, err)
		require.Contains(t, err.Error(), "not writable")
	})
	t.Run("a path under a file fails", func(t *testing.T) {
		file := filepath.Join(t.TempDir(), "file")
		require.NoError(t, os.WriteFile(file, []byte("x"), 0o600))

		_, err := media.NewLocal(localConfig(filepath.Join(file, "media"), "http://localhost:8080"), clock.System{})
		require.Error(t, err)
	})
	t.Run("a short signing key fails", func(t *testing.T) {
		cfg := localConfig(t.TempDir(), "http://localhost:8080")
		cfg.LocalSigningKey = "too-short"
		_, err := media.NewLocal(cfg, clock.System{})
		require.Error(t, err)
		require.NotContains(t, err.Error(), "too-short")
	})
}

func TestNewS3ChecksTheBucket(t *testing.T) {
	newS3 := func(change func(*config.Storage)) error {
		cfg := minioConfig()
		change(&cfg)
		_, err := media.NewS3(t.Context(), cfg, clock.System{})
		return err
	}
	// The baseline must work, or the failures below prove nothing.
	_ = newMinIO(t, clock.System{})

	t.Run("a bucket that does not exist", func(t *testing.T) {
		err := newS3(func(c *config.Storage) { c.S3Bucket = "no-such-bucket-for-siapp-tests" })
		require.Error(t, err)
		require.Contains(t, err.Error(), "bucket")
	})
	t.Run("wrong keys", func(t *testing.T) {
		err := newS3(func(c *config.Storage) { c.S3SecretKey = "definitely-wrong-secret-key" })
		require.Error(t, err)
		require.NotContains(t, err.Error(), "definitely-wrong-secret-key", "keys are never echoed")
	})
	t.Run("an endpoint nobody listens on", func(t *testing.T) {
		require.Error(t, newS3(func(c *config.Storage) { c.S3Endpoint = "http://127.0.0.1:1" }))
	})
}

// ⚠ Inside compose the API reaches MinIO as http://minio:9000, which a phone cannot resolve.
// URLs are signed for the public host, while Head and Delete go through the API's own.
func TestS3PresignsForThePublicEndpoint(t *testing.T) {
	cfg := minioConfig()
	cfg.S3PublicEndpoint = "http://minio.public.test:9000"
	if testing.Short() {
		t.Skip("-short set, skipping tests that need MinIO")
	}
	s, err := media.NewS3(t.Context(), cfg, clock.System{})
	require.NoError(t, err, "make up")

	key := storagetest.Key(t)
	put, err := s.PresignPut(t.Context(), key, media.PutInput{ContentType: "image/jpeg", SizeBytes: 100, TTL: time.Minute})
	require.NoError(t, err)
	require.True(t, strings.HasPrefix(put.URL, "http://minio.public.test:9000/"), "signed for the public host: %s", put.URL)
	get, err := s.PresignGet(t.Context(), key, "image/jpeg", time.Minute)
	require.NoError(t, err)
	require.True(t, strings.HasPrefix(get, "http://minio.public.test:9000/"), get)

	// The API's own calls use S3_ENDPOINT: a public host nobody can resolve here does not
	// get in the way of Head and Delete.
	_, err = s.Head(t.Context(), key)
	require.ErrorIs(t, err, media.ErrObjectNotFound)
	require.NoError(t, s.Delete(t.Context(), key))
}

func TestS3UploadHeadersAreOnlyWhatTheClientMustSend(t *testing.T) {
	s := newMinIO(t, clock.System{})
	up, err := s.PresignPut(t.Context(), storagetest.Key(t), media.PutInput{ContentType: "image/webp", SizeBytes: 321, TTL: time.Minute})
	require.NoError(t, err)

	require.Equal(t, http.MethodPut, up.Method)
	require.Equal(t, map[string]string{"Content-Type": "image/webp"}, up.Headers, "no Host, no Content-Length: the client's HTTP stack sets those")
	require.NotContains(t, up.URL, "x-amz-checksum", "no checksum for the phone to compute")
}

func TestNewStorageChoosesTheDriver(t *testing.T) {
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))

	local, err := media.NewStorage(t.Context(), localConfig(t.TempDir(), "http://localhost:8080"), clock.System{}, logger)
	require.NoError(t, err)
	require.IsType(t, &media.LocalStorage{}, local)

	_, err = media.NewStorage(t.Context(), config.Storage{Driver: "gcs"}, clock.System{}, logger)
	require.Error(t, err)

	t.Run("minio", func(t *testing.T) {
		cfg := minioConfig()
		if testing.Short() {
			t.Skip("-short set, skipping tests that need MinIO")
		}
		s3, err := media.NewStorage(t.Context(), cfg, clock.System{}, logger)
		require.NoError(t, err)
		require.IsType(t, &media.S3Storage{}, s3)
	})
}
