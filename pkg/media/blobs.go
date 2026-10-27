package media

import (
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"

	"siapp/pkg/platform/apperr"
	"siapp/pkg/platform/httpx"
)

// blobs serves the two routes that exist only when the server stores files itself
// (STORAGE_DRIVER=local). They behave like a presigned S3 URL: no bearer token, a signed and
// expiring URL, and a size cap of their own. No other route ever receives file bytes.
//
// local is nil with the other drivers. The routes are mounted all the same, and answer 404
// there: the route table (and the guard tests that read it) then does not depend on the
// environment, and a client that is somehow sent a blob URL by a deployment on S3 gets the
// plain "no such thing" answer.
type blobs struct {
	local *LocalStorage
}

// MountBlobs adds the local blob routes to r. Both are on auth.PublicRoutes and carry
// `security: []` in the contract: the signature in the URL is their authorization.
func MountBlobs(r chi.Router, storage Storage) {
	local, _ := storage.(*LocalStorage)
	b := &blobs{local: local}
	r.Put(BlobRoute, b.put)
	r.Get(BlobRoute, b.get)
}

// put receives the bytes of an upload.
//
// The checks run cheapest and most decisive first: the signature and expiry, then the signed
// content type, then the size, before a single byte of the body is read.
func (b *blobs) put(w http.ResponseWriter, r *http.Request) {
	if b.local == nil {
		httpx.Error(w, r, apperr.ErrNotFound)
		return
	}
	claims, err := b.local.verify(chi.URLParam(r, "token"), opPut)
	if err != nil {
		httpx.Error(w, r, errURLInvalid)
		return
	}
	// The header is part of what was signed: the client sends the upload_headers it was
	// given. A different type is a different upload than the one that was registered.
	if !strings.EqualFold(strings.TrimSpace(r.Header.Get("Content-Type")), claims.ContentType) {
		httpx.Error(w, r, errURLInvalid)
		return
	}
	if r.ContentLength > claims.MaxBytes {
		httpx.Error(w, r, errURLTooLarge)
		return
	}
	if r.ContentLength == 0 {
		httpx.Error(w, r, apperr.ErrBadRequest.WithMessage("Body wajib diisi."))
		return
	}

	extendDeadlines(w, blobUploadTimeout)
	// ⚠ The body is capped at the signed size, outside the API's 1 MB JSON cap: this route is
	// the only one that takes more. A client that sends no Content-Length (chunked) is still
	// stopped at the cap, which is what MaxBytesReader is for.
	body := http.MaxBytesReader(w, r.Body, claims.MaxBytes)
	if err := b.local.store(claims.Key, body); err != nil {
		if _, tooLarge := errors.AsType[*http.MaxBytesError](err); tooLarge {
			httpx.Error(w, r, errURLTooLarge)
			return
		}
		if errors.Is(err, errObjectExists) {
			httpx.Error(w, r, errAlreadyUploaded)
			return
		}
		// The client went away or sent fewer bytes than it announced: nothing was kept, and
		// nobody is left to read the answer, so this is not a server fault.
		if errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF) || isClientAbort(err) {
			slog.InfoContext(r.Context(), "media: upload interrupted, nothing kept",
				slog.String("requestId", httpx.RequestID(r.Context())))
			httpx.Error(w, r, apperr.ErrBadRequest.WithMessage("Unggahan terputus."))
			return
		}
		httpx.Error(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// get serves a photo.
func (b *blobs) get(w http.ResponseWriter, r *http.Request) {
	if b.local == nil {
		httpx.Error(w, r, apperr.ErrNotFound)
		return
	}
	claims, err := b.local.verify(chi.URLParam(r, "token"), opGet)
	if err != nil {
		httpx.Error(w, r, errURLInvalid)
		return
	}
	f, info, err := b.local.open(claims.Key)
	if err != nil {
		if errors.Is(err, ErrObjectNotFound) {
			httpx.Error(w, r, ErrNotFound)
			return
		}
		httpx.Error(w, r, err)
		return
	}
	defer func() { _ = f.Close() }()

	extendDeadlines(w, blobReadTimeout)
	h := w.Header()
	// The signed type, not a guess from the bytes: nosniff stops a browser from guessing a
	// different one, so a file can never be treated as a page. The extra policy is for the
	// same reason, in case someone opens the URL directly.
	h.Set("Content-Type", claims.ContentType)
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Content-Disposition", "inline")
	h.Set("Content-Security-Policy", "default-src 'none'; sandbox")
	// A cache may keep it only as long as the URL itself works, and only the viewer's own.
	remaining := max(time.Unix(claims.Expires, 0).Sub(b.local.clock.Now()), 0)
	h.Set("Cache-Control", "private, max-age="+strconv.FormatInt(int64(remaining/time.Second), 10))

	// ServeContent answers Range and conditional requests, so a large photo resumes.
	http.ServeContent(w, r, "", info.ModTime(), f)
}

// extendDeadlines lifts the server's JSON-sized read and write timeouts for this request. A
// server that cannot (a test recorder) simply keeps its own.
func extendDeadlines(w http.ResponseWriter, d time.Duration) {
	rc := http.NewResponseController(w)
	deadline := time.Now().Add(d)
	_ = rc.SetReadDeadline(deadline)
	_ = rc.SetWriteDeadline(deadline)
}

// isClientAbort reports a read that failed because the connection broke, which is how the
// server sees a phone that lost its signal mid-upload.
func isClientAbort(err error) bool {
	if errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.EPIPE) || errors.Is(err, net.ErrClosed) {
		return true
	}
	netErr, ok := errors.AsType[net.Error](err)
	return ok && netErr.Timeout()
}
