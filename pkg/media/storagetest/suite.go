// Package storagetest is the conformance suite every media.Storage driver must pass. It speaks
// to the driver the way a client does: it asks for a presigned URL and uses it over HTTP. The
// local driver and the S3 driver (against MinIO) run the same suite, so "the app never knows
// which driver is behind it" is a tested statement. Only test code imports this package.
package storagetest

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"siapp/pkg/media"
)

// JPEG returns n bytes that start like a JPEG file, so that the local driver, which sniffs
// the bytes, recognizes them as image/jpeg. The rest is filler.
func JPEG(n int) []byte {
	head := []byte{0xFF, 0xD8, 0xFF, 0xE0, 0x00, 0x10, 'J', 'F', 'I', 'F', 0x00}
	out := make([]byte, n)
	copy(out, head)
	for i := len(head); i < n; i++ {
		out[i] = byte(i % 251)
	}
	return out
}

// Key returns a new valid key. The organization part is random, so that runs against a shared
// bucket (MinIO) never collide with each other or with leftovers.
func Key(t testing.TB) string {
	t.Helper()
	org, err := rand.Int(rand.Reader, big.NewInt(1_000_000_000))
	require.NoError(t, err)
	id, err := rand.Int(rand.Reader, big.NewInt(1_000_000_000))
	require.NoError(t, err)
	suffix := make([]byte, 16)
	_, err = rand.Read(suffix)
	require.NoError(t, err)
	key := fmt.Sprintf("%d/REPORT/2026/10/%d-%s", org.Int64()+1, id.Int64()+1, hex.EncodeToString(suffix))
	require.True(t, media.ValidKey(key), key)
	return key
}

// Upload sends body to an upload the way a client does: the method and the headers it was
// given, nothing else. It returns the response, with the body already read.
func Upload(t testing.TB, up media.Upload, body []byte, override map[string]string) (status int, answer []byte) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), up.Method, up.URL, bytes.NewReader(body))
	require.NoError(t, err)
	for name, value := range up.Headers {
		req.Header.Set(name, value)
	}
	for name, value := range override {
		req.Header.Set(name, value)
	}
	return do(t, req)
}

// Download fetches a presigned GET URL.
func Download(t testing.TB, url string) (status int, header http.Header, body []byte) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, url, nil)
	require.NoError(t, err)
	res, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = res.Body.Close() }()
	body, err = io.ReadAll(res.Body)
	require.NoError(t, err)
	return res.StatusCode, res.Header, body
}

func do(t testing.TB, req *http.Request) (int, []byte) {
	t.Helper()
	res, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = res.Body.Close() }()
	body, err := io.ReadAll(res.Body)
	require.NoError(t, err)
	return res.StatusCode, body
}

// Run runs the suite. newStorage returns a fresh, ready driver for each subtest.
func Run(t *testing.T, newStorage func(t *testing.T) media.Storage) {
	t.Helper()

	// stored puts a JPEG of size bytes under a new key, and removes it when the test ends.
	stored := func(t *testing.T, s media.Storage, size int) (key string, body []byte) {
		t.Helper()
		key, body = Key(t), JPEG(size)
		t.Cleanup(func() { _ = s.Delete(t.Context(), key) })
		up, err := s.PresignPut(t.Context(), key, media.PutInput{ContentType: "image/jpeg", SizeBytes: int64(size), TTL: time.Minute})
		require.NoError(t, err)
		status, answer := Upload(t, up, body, nil)
		require.Less(t, status, 300, "upload was refused: %s", answer)
		return key, body
	}

	t.Run("presigned put, head, presigned get and delete", func(t *testing.T) {
		s := newStorage(t)
		before := time.Now()
		key, body := Key(t), JPEG(4096)
		t.Cleanup(func() { _ = s.Delete(t.Context(), key) })

		up, err := s.PresignPut(t.Context(), key, media.PutInput{ContentType: "image/jpeg", SizeBytes: int64(len(body)), TTL: time.Minute})
		require.NoError(t, err)
		require.Equal(t, http.MethodPut, up.Method)
		require.Equal(t, "image/jpeg", up.Headers["Content-Type"], "the type the client must send")
		require.WithinDuration(t, before.Add(time.Minute), up.ExpiresAt, 5*time.Second)

		_, err = s.Head(t.Context(), key)
		require.ErrorIs(t, err, media.ErrObjectNotFound, "nothing is there before the upload")

		status, answer := Upload(t, up, body, nil)
		require.Less(t, status, 300, "upload was refused: %s", answer)

		obj, err := s.Head(t.Context(), key)
		require.NoError(t, err)
		require.EqualValues(t, len(body), obj.SizeBytes)
		require.Equal(t, "image/jpeg", obj.ContentType)

		url, err := s.PresignGet(t.Context(), key, "image/jpeg", time.Minute)
		require.NoError(t, err)
		status, header, got := Download(t, url)
		require.Equal(t, http.StatusOK, status)
		require.Equal(t, body, got)
		require.Equal(t, "image/jpeg", header.Get("Content-Type"))

		require.NoError(t, s.Delete(t.Context(), key))
		_, err = s.Head(t.Context(), key)
		require.ErrorIs(t, err, media.ErrObjectNotFound)
		status, _, _ = Download(t, url)
		require.Equal(t, http.StatusNotFound, status, "a deleted photo is gone, even for a URL that has not expired")
	})

	t.Run("delete of a missing object is not an error", func(t *testing.T) {
		s := newStorage(t)
		key := Key(t)
		require.NoError(t, s.Delete(t.Context(), key))
		require.NoError(t, s.Delete(t.Context(), key), "and again")
	})

	t.Run("head of a missing object", func(t *testing.T) {
		s := newStorage(t)
		_, err := s.Head(t.Context(), Key(t))
		require.ErrorIs(t, err, media.ErrObjectNotFound)
	})

	t.Run("an expired URL is refused", func(t *testing.T) {
		s := newStorage(t)
		key, _ := stored(t, s, 1024)
		// The put is for a key with nothing behind it yet, so that the only thing wrong with it
		// later is its age (a key that has its object is refused for that reason first).
		fresh := Key(t)
		t.Cleanup(func() { _ = s.Delete(t.Context(), fresh) })

		put, err := s.PresignPut(t.Context(), fresh, media.PutInput{ContentType: "image/jpeg", SizeBytes: 1024, TTL: time.Second})
		require.NoError(t, err)
		get, err := s.PresignGet(t.Context(), key, "image/jpeg", time.Second)
		require.NoError(t, err)

		// The view works while it is alive...
		status, _, _ := Download(t, get)
		require.Equal(t, http.StatusOK, status)

		// ...and neither URL does once its second is over.
		time.Sleep(2500 * time.Millisecond)
		status, _ = Upload(t, put, JPEG(1024), nil)
		require.Equal(t, http.StatusForbidden, status, "an expired put")
		_, err = s.Head(t.Context(), fresh)
		require.ErrorIs(t, err, media.ErrObjectNotFound, "and it stored nothing")
		status, _, _ = Download(t, get)
		require.Equal(t, http.StatusForbidden, status, "an expired get")
	})

	t.Run("a key outside the key pattern is refused everywhere", func(t *testing.T) {
		s := newStorage(t)
		for name, key := range map[string]string{
			"empty":                     "",
			"parent directories":        "../../etc/passwd",
			"parent inside the key":     "1/REPORT/2026/10/../../../x",
			"absolute":                  "/etc/passwd",
			"no organization":           "REPORT/2026/10/1-" + hex.EncodeToString(make([]byte, 16)),
			"organization not a number": "acme/REPORT/2026/10/1-" + hex.EncodeToString(make([]byte, 16)),
			"month 13":                  "1/REPORT/2026/13/1-" + hex.EncodeToString(make([]byte, 16)),
			"an extra level":            "1/REPORT/2026/10/extra/1-" + hex.EncodeToString(make([]byte, 16)),
			"a short random part":       "1/REPORT/2026/10/1-abc",
			"a trailing slash":          Key(t) + "/",
			"a newline":                 Key(t) + "\n",
		} {
			t.Run(name, func(t *testing.T) {
				_, err := s.PresignPut(t.Context(), key, media.PutInput{ContentType: "image/jpeg", SizeBytes: 10, TTL: time.Minute})
				require.ErrorIs(t, err, media.ErrInvalidKey, "put")
				_, err = s.PresignGet(t.Context(), key, "image/jpeg", time.Minute)
				require.ErrorIs(t, err, media.ErrInvalidKey, "get")
				_, err = s.Head(t.Context(), key)
				require.ErrorIs(t, err, media.ErrInvalidKey, "head")
				require.ErrorIs(t, s.Delete(t.Context(), key), media.ErrInvalidKey, "delete")
			})
		}
	})

	t.Run("a different content type than the signed one is refused", func(t *testing.T) {
		s := newStorage(t)
		key := Key(t)
		t.Cleanup(func() { _ = s.Delete(t.Context(), key) })
		up, err := s.PresignPut(t.Context(), key, media.PutInput{ContentType: "image/jpeg", SizeBytes: 512, TTL: time.Minute})
		require.NoError(t, err)

		status, _ := Upload(t, up, JPEG(512), map[string]string{"Content-Type": "image/png"})
		require.Equal(t, http.StatusForbidden, status)
		_, err = s.Head(t.Context(), key)
		require.ErrorIs(t, err, media.ErrObjectNotFound, "nothing was stored")
	})

	t.Run("more bytes than were declared are refused and nothing is stored", func(t *testing.T) {
		s := newStorage(t)
		key := Key(t)
		t.Cleanup(func() { _ = s.Delete(t.Context(), key) })
		up, err := s.PresignPut(t.Context(), key, media.PutInput{ContentType: "image/jpeg", SizeBytes: 1000, TTL: time.Minute})
		require.NoError(t, err)

		status, _ := Upload(t, up, JPEG(2000), nil)
		require.GreaterOrEqual(t, status, 400, "the declared size is a cap")
		_, err = s.Head(t.Context(), key)
		require.ErrorIs(t, err, media.ErrObjectNotFound, "an oversize upload leaves no object")
	})

	// ⚠ A photo is evidence: once stored, the URL that stored it must not be able to replace it
	// (it is still valid for half an hour), or a checked and attached photo could be swapped.
	t.Run("an upload URL only creates: a second upload is refused and the photo stays", func(t *testing.T) {
		s := newStorage(t)
		key := Key(t)
		t.Cleanup(func() { _ = s.Delete(t.Context(), key) })
		up, err := s.PresignPut(t.Context(), key, media.PutInput{ContentType: "image/jpeg", SizeBytes: 800, TTL: time.Minute})
		require.NoError(t, err)

		first := JPEG(800)
		status, answer := Upload(t, up, first, nil)
		require.Less(t, status, 300, "the first upload works: %s", answer)

		second := JPEG(800)
		second[799] ^= 0xFF // same size and type, other content: a swapped photo
		status, _ = Upload(t, up, second, nil)
		require.Equal(t, http.StatusPreconditionFailed, status, "the same URL cannot overwrite, and every driver says so with 412")

		url, err := s.PresignGet(t.Context(), key, "image/jpeg", time.Minute)
		require.NoError(t, err)
		_, _, got := Download(t, url)
		require.Equal(t, first, got, "the stored photo is still the first one")
	})

	t.Run("head reports the type of the bytes, not the declared one", func(t *testing.T) {
		s := newStorage(t)
		key := Key(t)
		t.Cleanup(func() { _ = s.Delete(t.Context(), key) })
		notAnImage := bytes.Repeat([]byte("plain text, not a photo. "), 20)
		up, err := s.PresignPut(t.Context(), key, media.PutInput{ContentType: "image/jpeg", SizeBytes: int64(len(notAnImage)), TTL: time.Minute})
		require.NoError(t, err)
		status, answer := Upload(t, up, notAnImage, nil)
		require.Less(t, status, 300, "the store takes any bytes: %s", answer)

		obj, err := s.Head(t.Context(), key)
		require.NoError(t, err)
		require.EqualValues(t, len(notAnImage), obj.SizeBytes)
		require.Equal(t, "text/plain; charset=utf-8", obj.ContentType, "declared image/jpeg, but the bytes say otherwise")
	})

	t.Run("a get URL cannot be used to write", func(t *testing.T) {
		s := newStorage(t)
		key, body := stored(t, s, 700)
		get, err := s.PresignGet(t.Context(), key, "image/jpeg", time.Minute)
		require.NoError(t, err)

		status, _ := Upload(t, media.Upload{Method: http.MethodPut, URL: get, Headers: map[string]string{"Content-Type": "image/jpeg"}}, JPEG(700), nil)
		require.GreaterOrEqual(t, status, 400)

		// The photo is still the one that was uploaded.
		url, err := s.PresignGet(t.Context(), key, "image/jpeg", time.Minute)
		require.NoError(t, err)
		_, _, got := Download(t, url)
		require.Equal(t, body, got)
	})
}
