package media_test

import (
	"bytes"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/require"

	"siapp/pkg/media"
	"siapp/pkg/media/storagetest"
	"siapp/pkg/platform/clock"
)

const blobPrefix = "/api/v1/media/blobs/"

func tokenOf(url string) string { return url[strings.LastIndex(url, "/")+1:] }

// files lists the regular files below root, relative to it.
func files(t *testing.T, root string) []string {
	t.Helper()
	var out []string
	require.NoError(t, filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			rel, _ := filepath.Rel(root, path)
			out = append(out, rel)
		}
		return nil
	}))
	return out
}

// blob sends a request to a blob URL of the test server, with no bearer token, as a phone does.
func (f *flow) blob(method, token string, body io.Reader, contentType string) (status int, header http.Header, out []byte) {
	f.t.Helper()
	req, err := http.NewRequestWithContext(f.t.Context(), method, f.srv.URL+blobPrefix+token, body)
	require.NoError(f.t, err)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	res, err := http.DefaultClient.Do(req)
	require.NoError(f.t, err)
	defer func() { _ = res.Body.Close() }()
	out, err = io.ReadAll(res.Body)
	require.NoError(f.t, err)
	return res.StatusCode, res.Header, out
}

// registered registers a JPEG of the given size and returns what the server answered.
func (f *flow) registered(token string, size int) uploadInfo {
	f.t.Helper()
	status, reg := f.register(token, validBody(size))
	require.Equal(f.t, http.StatusCreated, status)
	return reg.Data
}

func requireURLInvalid(t *testing.T, status int, raw []byte, wantStatus int) {
	t.Helper()
	require.Equal(t, wantStatus, status, string(raw))
	out := decode[any](t, status, raw)
	require.Equal(t, "MEDIA_URL_INVALID", out.reason())
}

// The routes that exist only with STORAGE_DRIVER=local. They work like a presigned S3 URL: no
// bearer token, a signed and expiring URL, and a size cap of their own.
func TestLocalBlobRoutes(t *testing.T) {
	t.Run("a tampered token is refused", func(t *testing.T) {
		f := newFlow(t, drivers[0])
		token, _ := f.person(f.orgA)
		up := f.registered(token, 1000)
		valid := tokenOf(up.UploadURL)
		payload, mac, found := strings.Cut(valid, ".")
		require.True(t, found)

		// Another key signs a token with the right shape: only the signature can tell.
		forged, err := mustLocalWithKey(t, "another-signing-key-another-signing-key!").PresignPut(t.Context(),
			f.rows(f.orgA)[0].Key, media.PutInput{ContentType: "image/jpeg", SizeBytes: 1000, TTL: time.Hour})
		require.NoError(t, err)

		flip := func(s string) string {
			i := len(s) / 2
			c := byte('A')
			if s[i] == 'A' {
				c = 'B'
			}
			return s[:i] + string(c) + s[i+1:]
		}
		for name, bad := range map[string]string{
			"a changed payload":        flip(payload) + "." + mac,
			"a changed signature":      payload + "." + flip(mac),
			"no signature":             payload,
			"an empty signature":       payload + ".",
			"a signature of nothing":   "." + mac,
			"garbage":                  "not-a-token",
			"extra text after the mac": valid + "xyz",
			"signed with another key":  tokenOf(forged.URL),
		} {
			t.Run(name, func(t *testing.T) {
				status, _, raw := f.blob(http.MethodPut, bad, bytes.NewReader(storagetest.JPEG(1000)), "image/jpeg")
				requireURLInvalid(t, status, raw, http.StatusForbidden)
				status, _, raw = f.blob(http.MethodGet, bad, nil, "")
				requireURLInvalid(t, status, raw, http.StatusForbidden)
			})
		}
		require.Empty(t, files(t, f.root), "nothing was stored")
		require.Equal(t, "PENDING", f.rows(f.orgA)[0].Status)
	})

	t.Run("an expired token is refused", func(t *testing.T) {
		f := newFlow(t, drivers[0])
		token, _ := f.person(f.orgA)
		up := f.registered(token, 1000)
		tok := tokenOf(up.UploadURL)

		// While it lives it works...
		status, _, raw := f.blob(http.MethodPut, tok, bytes.NewReader(storagetest.JPEG(1000)), "image/jpeg")
		require.Equal(t, http.StatusNoContent, status, string(raw))
		view, err := f.storage.PresignGet(t.Context(), f.rows(f.orgA)[0].Key, "image/jpeg", time.Minute)
		require.NoError(t, err)
		status, _, _ = f.blob(http.MethodGet, tokenOf(view), nil, "")
		require.Equal(t, http.StatusOK, status)

		// ...and not past its time. The upload URL lives half an hour, the view URL a minute.
		f.clock.Advance(2 * time.Minute)
		status, _, _ = f.blob(http.MethodGet, tokenOf(view), nil, "")
		require.Equal(t, http.StatusForbidden, status, "an expired get")
		f.clock.Advance(time.Hour)
		status, _, raw = f.blob(http.MethodPut, tok, bytes.NewReader(storagetest.JPEG(1000)), "image/jpeg")
		requireURLInvalid(t, status, raw, http.StatusForbidden)
	})

	t.Run("a token cannot be used for the other operation", func(t *testing.T) {
		f := newFlow(t, drivers[0])
		token, _ := f.person(f.orgA)
		up := f.registered(token, 1000)
		putToken := tokenOf(up.UploadURL)
		key := f.rows(f.orgA)[0].Key
		uploaded, answer := storagetest.Upload(t, up.asUpload(), storagetest.JPEG(1000), nil)
		require.Less(t, uploaded, 300, "%s", answer)
		getURL, err := f.storage.PresignGet(t.Context(), key, "image/jpeg", time.Hour)
		require.NoError(t, err)

		// A get token must not write...
		status, _, raw := f.blob(http.MethodPut, tokenOf(getURL), bytes.NewReader(storagetest.JPEG(1000)), "image/jpeg")
		requireURLInvalid(t, status, raw, http.StatusForbidden)
		// ...and a put token must not read.
		status, _, raw = f.blob(http.MethodGet, putToken, nil, "")
		requireURLInvalid(t, status, raw, http.StatusForbidden)
	})

	t.Run("a wrong Content-Type is refused", func(t *testing.T) {
		f := newFlow(t, drivers[0])
		token, _ := f.person(f.orgA)
		up := f.registered(token, 1000)

		for _, contentType := range []string{"image/png", "text/html", "application/octet-stream", ""} {
			status, _, raw := f.blob(http.MethodPut, tokenOf(up.UploadURL), bytes.NewReader(storagetest.JPEG(1000)), contentType)
			requireURLInvalid(t, status, raw, http.StatusForbidden)
		}
		require.Empty(t, files(t, f.root))
	})

	t.Run("a body over the signed size is refused with 413 and stores nothing", func(t *testing.T) {
		f := newFlow(t, drivers[0])
		token, _ := f.person(f.orgA)
		up := f.registered(token, 1000)

		// Announced up front: refused before a byte is read.
		status, _, raw := f.blob(http.MethodPut, tokenOf(up.UploadURL), bytes.NewReader(storagetest.JPEG(1001)), "image/jpeg")
		requireURLInvalid(t, status, raw, http.StatusRequestEntityTooLarge)

		// Not announced (chunked): stopped by the cap while it is being read.
		status, _, raw = f.blob(http.MethodPut, tokenOf(up.UploadURL), io.NopCloser(bytes.NewReader(storagetest.JPEG(50_000))), "image/jpeg")
		requireURLInvalid(t, status, raw, http.StatusRequestEntityTooLarge)

		require.Empty(t, files(t, f.root), "neither an object nor a temporary file is left")

		// The exact size still works with the same token afterwards.
		status, _, raw = f.blob(http.MethodPut, tokenOf(up.UploadURL), bytes.NewReader(storagetest.JPEG(1000)), "image/jpeg")
		require.Equal(t, http.StatusNoContent, status, string(raw))
	})

	t.Run("a second upload to the same URL is refused with 412, as S3 does", func(t *testing.T) {
		f := newFlow(t, drivers[0])
		token, _ := f.person(f.orgA)
		up := f.registered(token, 1000)

		status, _, raw := f.blob(http.MethodPut, tokenOf(up.UploadURL), bytes.NewReader(storagetest.JPEG(1000)), "image/jpeg")
		require.Equal(t, http.StatusNoContent, status, string(raw))
		status, _, raw = f.blob(http.MethodPut, tokenOf(up.UploadURL), bytes.NewReader(storagetest.JPEG(1000)), "image/jpeg")
		requireURLInvalid(t, status, raw, http.StatusPreconditionFailed)
		require.Len(t, files(t, f.root), 1, "one object, and no temporary file left by the refused one")

		// The client that lost the first answer just completes.
		status, out := f.complete(token, up.ID)
		require.Equal(t, http.StatusOK, status)
		require.Equal(t, "UPLOADED", out.Data.Status)
	})

	t.Run("an empty body is refused", func(t *testing.T) {
		f := newFlow(t, drivers[0])
		token, _ := f.person(f.orgA)
		up := f.registered(token, 1000)

		status, _, raw := f.blob(http.MethodPut, tokenOf(up.UploadURL), bytes.NewReader(nil), "image/jpeg")
		require.Equal(t, http.StatusBadRequest, status, string(raw))
		require.Empty(t, files(t, f.root))
	})

	// A body shorter than registered must not be stored: write-once would make it permanent,
	// complete would refuse its size, and every retry on the same URL would get 412.
	t.Run("a body shorter than registered is refused and the URL stays usable", func(t *testing.T) {
		f := newFlow(t, drivers[0])
		token, _ := f.person(f.orgA)
		up := f.registered(token, 1000)
		tok := tokenOf(up.UploadURL)

		// Announced shorter: refused before a byte is read.
		status, _, raw := f.blob(http.MethodPut, tok, bytes.NewReader(storagetest.JPEG(600)), "image/jpeg")
		require.Equal(t, http.StatusBadRequest, status, string(raw))
		// Not announced (chunked) and shorter, or empty: counted as it is stored.
		status, _, raw = f.blob(http.MethodPut, tok, io.NopCloser(bytes.NewReader(storagetest.JPEG(600))), "image/jpeg")
		require.Equal(t, http.StatusBadRequest, status, string(raw))
		status, _, raw = f.blob(http.MethodPut, tok, io.NopCloser(bytes.NewReader(nil)), "image/jpeg")
		require.Equal(t, http.StatusBadRequest, status, string(raw))
		require.Empty(t, files(t, f.root), "neither an object nor a temporary file is left")

		// The same URL still takes the right bytes, and the photo completes.
		status, _, raw = f.blob(http.MethodPut, tok, bytes.NewReader(storagetest.JPEG(1000)), "image/jpeg")
		require.Equal(t, http.StatusNoContent, status, string(raw))
		status, out := f.complete(token, up.ID)
		require.Equal(t, http.StatusOK, status)
		require.Equal(t, "UPLOADED", out.Data.Status)
	})

	// The offline queue retries POST /media with the same Idempotency-Key and gets the stored
	// answer: the same id and the same, by now dead, URL. The way out is a new URL for that id.
	t.Run("a replay after the URL expired gets the dead URL, and upload-url is the way out", func(t *testing.T) {
		f := newFlow(t, drivers[0])
		token, _ := f.person(f.orgA)
		key := map[string]string{"Idempotency-Key": newIdempotencyKey()}
		status, _, raw := f.api(token, http.MethodPost, "/media", validBody(1000), key)
		require.Equal(t, http.StatusCreated, status)
		first := decode[uploadInfo](t, status, raw).Data

		f.clock.Advance(31 * time.Minute)
		status, _, raw = f.api(token, http.MethodPost, "/media", validBody(1000), key)
		require.Equal(t, http.StatusCreated, status)
		replayed := decode[uploadInfo](t, status, raw).Data
		require.Equal(t, first.UploadURL, replayed.UploadURL, "a replay is the stored answer")
		status, _, raw = f.blob(http.MethodPut, tokenOf(replayed.UploadURL), bytes.NewReader(storagetest.JPEG(1000)), "image/jpeg")
		requireURLInvalid(t, status, raw, http.StatusForbidden)

		status, fresh := f.reissue(token, first.ID)
		require.Equal(t, http.StatusOK, status)
		require.Equal(t, first.ID, fresh.Data.ID)
		require.NotEqual(t, first.UploadURL, fresh.Data.UploadURL)
		require.True(t, fresh.Data.ExpiresAt.After(f.clock.Now()), "alive again")
		uploaded, answer := storagetest.Upload(t, fresh.Data.asUpload(), storagetest.JPEG(1000), nil)
		require.Less(t, uploaded, 300, "%s", answer)
		status, out := f.complete(token, first.ID)
		require.Equal(t, http.StatusOK, status)
		require.Equal(t, "UPLOADED", out.Data.Status)
	})

	t.Run("an upload cut off halfway leaves no file", func(t *testing.T) {
		f := newFlow(t, drivers[0])
		token, _ := f.person(f.orgA)
		up := f.registered(token, 2000)

		// Announce 2000 bytes, send 800 of them, and hang up, as a phone does that loses signal.
		conn, err := net.Dial("tcp", f.srv.Listener.Addr().String())
		require.NoError(t, err)
		_, err = fmt.Fprintf(conn, "PUT %s%s HTTP/1.1\r\nHost: test\r\nContent-Type: image/jpeg\r\nContent-Length: 2000\r\n\r\n", blobPrefix, tokenOf(up.UploadURL))
		require.NoError(t, err)
		_, err = conn.Write(storagetest.JPEG(800))
		require.NoError(t, err)
		require.NoError(t, conn.Close())

		// The access log writes its line when the handler is done: from then on the outcome is final.
		require.Eventually(t, func() bool { return strings.Contains(f.logs.String(), "/media/blobs/{token}") },
			5*time.Second, 20*time.Millisecond, "the server never finished the request")
		require.Empty(t, files(t, f.root), "no object and no temporary file")
		_, err = f.storage.Head(t.Context(), f.rows(f.orgA)[0].Key)
		require.ErrorIs(t, err, media.ErrObjectNotFound)

		status, out := f.complete(token, up.ID)
		require.Equal(t, http.StatusConflict, status)
		require.Equal(t, "MEDIA_NOT_UPLOADED", out.reason(), "complete cannot accept what was never finished")
	})

	t.Run("a file that is not an image but was declared image/jpeg fails complete", func(t *testing.T) {
		f := newFlow(t, drivers[0])
		token, _ := f.person(f.orgA)
		up := f.registered(token, 120)

		notAnImage := bytes.Repeat([]byte("<script>alert(1)</script>"), 5)[:120]
		status, _, raw := f.blob(http.MethodPut, tokenOf(up.UploadURL), bytes.NewReader(notAnImage), "image/jpeg")
		require.Equal(t, http.StatusNoContent, status, "the route does not look inside: complete does:\n%s", raw)

		stored, err := f.storage.Head(t.Context(), f.rows(f.orgA)[0].Key)
		require.NoError(t, err)
		require.EqualValues(t, 120, stored.SizeBytes, "the size is right...")
		require.NotEqual(t, "image/jpeg", stored.ContentType, "...but the bytes are not a JPEG, whatever the client said")

		status, out := f.complete(token, up.ID)
		require.Equal(t, http.StatusConflict, status)
		require.Equal(t, "MEDIA_NOT_UPLOADED", out.reason())
		require.Equal(t, "PENDING", f.rows(f.orgA)[0].Status)
	})

	t.Run("a photo is served with the signed type and the safe headers", func(t *testing.T) {
		f := newFlow(t, drivers[0])
		token, _ := f.person(f.orgA)
		up := f.registered(token, 3000)
		body := storagetest.JPEG(3000)
		uploaded, answer := storagetest.Upload(t, up.asUpload(), body, nil)
		require.Less(t, uploaded, 300, "%s", answer)

		url, err := f.storage.PresignGet(t.Context(), f.rows(f.orgA)[0].Key, "image/jpeg", 10*time.Minute)
		require.NoError(t, err)
		status, header, got := f.blob(http.MethodGet, tokenOf(url), nil, "")
		require.Equal(t, http.StatusOK, status)
		require.Equal(t, body, got)
		require.Equal(t, "image/jpeg", header.Get("Content-Type"))
		require.Equal(t, "nosniff", header.Get("X-Content-Type-Options"))
		require.Equal(t, "inline", header.Get("Content-Disposition"))
		require.Equal(t, "default-src 'none'; sandbox", header.Get("Content-Security-Policy"))
		// Private, and never kept longer than the URL itself works (10 minutes here).
		cacheControl := header.Get("Cache-Control")
		require.True(t, strings.HasPrefix(cacheControl, "private, max-age="), cacheControl)
		var maxAge int
		_, err = fmt.Sscanf(strings.TrimPrefix(cacheControl, "private, max-age="), "%d", &maxAge)
		require.NoError(t, err)
		require.LessOrEqual(t, maxAge, 600)
		require.Greater(t, maxAge, 500)

		// A range request is answered with that range, so a large photo can resume.
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, url, nil)
		require.NoError(t, err)
		req.Header.Set("Range", "bytes=10-19")
		res, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		defer func() { _ = res.Body.Close() }()
		part, err := io.ReadAll(res.Body)
		require.NoError(t, err)
		require.Equal(t, http.StatusPartialContent, res.StatusCode)
		require.Equal(t, body[10:20], part)
	})

	t.Run("a valid token for an object that does not exist is 404", func(t *testing.T) {
		f := newFlow(t, drivers[0])
		token, _ := f.person(f.orgA)
		f.registered(token, 1000) // registered, never uploaded

		url, err := f.storage.PresignGet(t.Context(), f.rows(f.orgA)[0].Key, "image/jpeg", time.Minute)
		require.NoError(t, err)
		status, _, raw := f.blob(http.MethodGet, tokenOf(url), nil, "")
		require.Equal(t, http.StatusNotFound, status)
		require.Equal(t, "MEDIA_NOT_FOUND", decode[any](t, status, raw).reason())
	})

	t.Run("the token never appears in the request log", func(t *testing.T) {
		f := newFlow(t, drivers[0])
		token, _ := f.person(f.orgA)
		up := f.registered(token, 800)
		putToken := tokenOf(up.UploadURL)
		uploaded, answer := storagetest.Upload(t, up.asUpload(), storagetest.JPEG(800), nil)
		require.Less(t, uploaded, 300, "%s", answer)
		getURL, err := f.storage.PresignGet(t.Context(), f.rows(f.orgA)[0].Key, "image/jpeg", time.Minute)
		require.NoError(t, err)
		status, _, _ := f.blob(http.MethodGet, tokenOf(getURL), nil, "")
		require.Equal(t, http.StatusOK, status)
		// Refused requests are logged too, and must not give the token away either.
		_, _, _ = f.blob(http.MethodPut, putToken+"tampered", bytes.NewReader(storagetest.JPEG(800)), "image/jpeg")

		logs := f.logs.String()
		require.Contains(t, logs, `"path":"/api/v1/media/blobs/{token}"`, "the line says what was asked for")
		for _, secret := range []string{putToken, tokenOf(getURL), strings.SplitN(putToken, ".", 2)[0], strings.SplitN(tokenOf(getURL), ".", 2)[1]} {
			require.NotContains(t, logs, secret)
		}
	})
}

// ⚠ The API's server has read and write timeouts sized for JSON (15 s and 30 s). A 10 MB photo
// over a slow mobile connection takes longer, so the blob routes lift the deadlines for their
// own request. Here the server's timeouts are one second and the upload takes about three.
func TestASlowUploadOutlivesTheServerTimeouts(t *testing.T) {
	srv := httptest.NewUnstartedServer(nil)
	srv.Config.ReadTimeout = time.Second
	srv.Config.WriteTimeout = time.Second
	t.Cleanup(srv.Close)
	local, err := media.NewLocal(localConfig(t.TempDir(), "http://"+srv.Listener.Addr().String()), clock.System{})
	require.NoError(t, err)
	router := chi.NewRouter()
	media.MountBlobs(router, local)
	srv.Config.Handler = router
	srv.Start()

	key := storagetest.Key(t)
	up, err := local.PresignPut(t.Context(), key, media.PutInput{ContentType: "image/jpeg", SizeBytes: 2000, TTL: time.Minute})
	require.NoError(t, err)

	body := storagetest.JPEG(2000)
	reader, writer := io.Pipe()
	go func() {
		_, _ = writer.Write(body[:1000])
		time.Sleep(3 * time.Second)
		_, _ = writer.Write(body[1000:])
		_ = writer.Close()
	}()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPut, up.URL, reader)
	require.NoError(t, err)
	req.ContentLength = 2000
	req.Header.Set("Content-Type", "image/jpeg")
	res, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	_ = res.Body.Close()
	require.Equal(t, http.StatusNoContent, res.StatusCode)

	stored, err := local.Head(t.Context(), key)
	require.NoError(t, err)
	require.EqualValues(t, 2000, stored.SizeBytes, "the whole photo arrived")
}

// With minio or s3 the files go straight to object storage, and the server's own blob routes
// have nothing to serve: they answer 404, not 401 and not a signature error.
func TestBlobRoutesAnswer404WithAnS3Driver(t *testing.T) {
	f := newFlow(t, drivers[1])

	status, _, raw := f.blob(http.MethodPut, "anything.at-all", strings.NewReader("x"), "image/jpeg")
	require.Equal(t, http.StatusNotFound, status, string(raw))
	require.Equal(t, "NOT_FOUND", decode[any](t, status, raw).Error.Code)
	status, _, raw = f.blob(http.MethodGet, "anything.at-all", nil, "")
	require.Equal(t, http.StatusNotFound, status, string(raw))
}

func mustLocalWithKey(t *testing.T, key string) *media.LocalStorage {
	t.Helper()
	cfg := localConfig(t.TempDir(), "http://forged.test")
	cfg.LocalSigningKey = key
	s, err := media.NewLocal(cfg, clock.System{})
	require.NoError(t, err)
	return s
}
