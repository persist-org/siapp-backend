package media_test

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"siapp/pkg/media"
	"siapp/pkg/media/storagetest"
)

// The flow runs once per driver: register, PUT the bytes to the URL it was given, complete.
// Everything below the URL is the driver's business, and the assertions do not know which one
// it is.
func TestUploadFlow(t *testing.T) {
	for _, d := range drivers {
		t.Run(d.name, func(t *testing.T) {
			t.Run("register, upload, complete", func(t *testing.T) { testHappyPath(t, d) })
			t.Run("complete without an upload fails", func(t *testing.T) { testCompleteWithoutUpload(t, d) })
			t.Run("an upload of another size does not complete", func(t *testing.T) { testWrongSizeUpload(t, d) })
			t.Run("a wrong type or an oversize file is rejected", func(t *testing.T) { testRegisterRejections(t, d) })
			t.Run("another person cannot complete someone else's upload", func(t *testing.T) { testOtherPerson(t, d) })
			t.Run("cross-tenant: org B gets 404 for org A's media", func(t *testing.T) { testCrossTenant(t, d) })
			t.Run("an idempotent replay returns the same id", func(t *testing.T) { testIdempotentReplay(t, d) })
			t.Run("authentication is required", func(t *testing.T) { testAuthentication(t, d) })
			t.Run("an id that cannot exist is not found", func(t *testing.T) { testImpossibleID(t, d) })
		})
	}
}

func testHappyPath(t *testing.T, d driver) {
	f := newFlow(t, d)
	token, personnel := f.person(f.orgA)

	captured := time.Now().UTC().Add(-time.Minute).Truncate(time.Second)
	body := validBody(2048)
	body["captured_at"] = captured.Format(time.RFC3339)
	body["latitude"] = -6.2
	body["longitude"] = 106.8

	status, reg := f.register(token, body)
	require.Equal(t, http.StatusCreated, status)
	require.NotZero(t, reg.Data.ID)
	require.Equal(t, "PUT", reg.Data.UploadMethod, "always PUT: it is in the response so a future driver needs no app release")
	require.Equal(t, "image/jpeg", reg.Data.UploadHeaders["Content-Type"])
	require.NotEmpty(t, reg.Data.UploadURL)
	require.WithinDuration(t, time.Now().Add(30*time.Minute), reg.Data.ExpiresAt, time.Minute)

	// The row is PENDING, owned by the caller, in the caller's organization, with a key that
	// starts with that organization and ends in this row's id.
	rows := f.rows(f.orgA)
	require.Len(t, rows, 1)
	r := rows[0]
	require.Equal(t, reg.Data.ID, r.ID)
	require.Equal(t, f.orgA, r.Org)
	require.Equal(t, personnel, r.Owner)
	require.Equal(t, "PENDING", r.Status)
	require.Equal(t, "REPORT", r.Purpose)
	require.Equal(t, "image/jpeg", r.ContentType)
	require.EqualValues(t, 2048, r.Size)
	require.Equal(t, "CAMERA", r.Source)
	require.NotNil(t, r.CapturedAt)
	require.True(t, captured.Equal(*r.CapturedAt), "capture time is kept as sent")
	require.InDelta(t, -6.2, *r.Latitude, 1e-6)
	require.InDelta(t, 106.8, *r.Longitude, 1e-6)
	require.True(t, media.ValidKey(r.Key), r.Key)
	require.True(t, strings.HasPrefix(r.Key, itoa(f.orgA)+"/REPORT/"), "the key starts with the organization id: %s", r.Key)
	require.Contains(t, r.Key, "/"+itoa(r.ID)+"-", "the key carries the row's id")
	if f.root != "" {
		// A blob URL carries a signed token, not a path: it names no key and no directory.
		require.NotContains(t, reg.Data.UploadURL, r.Key, "the URL does not give the key away")
	}

	// Upload exactly what was registered, with exactly the headers it was given.
	uploaded, answer := storagetest.Upload(t, reg.Data.asUpload(), storagetest.JPEG(2048), nil)
	require.Less(t, uploaded, 300, "upload refused: %s", answer)

	status, done := f.complete(token, reg.Data.ID)
	require.Equal(t, http.StatusOK, status)
	require.Equal(t, "UPLOADED", done.Data.Status)
	require.Equal(t, reg.Data.ID, done.Data.ID)
	require.Equal(t, "REPORT", done.Data.Purpose)
	require.EqualValues(t, 2048, done.Data.SizeBytes)
	require.Equal(t, "CAMERA", done.Data.CaptureSource)
	require.InDelta(t, -6.2, *done.Data.Latitude, 1e-6)
	require.Equal(t, "UPLOADED", f.rows(f.orgA)[0].Status)

	// Confirming again, because the answer to the first one was lost, is not an error.
	status, again := f.complete(token, reg.Data.ID)
	require.Equal(t, http.StatusOK, status)
	require.Equal(t, "UPLOADED", again.Data.Status)
}

func testCompleteWithoutUpload(t *testing.T, d driver) {
	f := newFlow(t, d)
	token, _ := f.person(f.orgA)
	_, reg := f.register(token, validBody(1024))

	status, out := f.complete(token, reg.Data.ID)
	require.Equal(t, http.StatusConflict, status)
	require.Equal(t, "CONFLICT", out.Error.Code)
	require.Equal(t, "MEDIA_NOT_UPLOADED", out.reason())
	require.Equal(t, "PENDING", f.rows(f.orgA)[0].Status, "a failed check changes nothing")

	// Uploading afterwards makes the same call succeed: the photo was only in the wrong state.
	uploaded, answer := storagetest.Upload(t, reg.Data.asUpload(), storagetest.JPEG(1024), nil)
	require.Less(t, uploaded, 300, "%s", answer)
	status, out = f.complete(token, reg.Data.ID)
	require.Equal(t, http.StatusOK, status)
	require.Equal(t, "UPLOADED", out.Data.Status)
}

// Sending fewer bytes than were registered is refused by S3 itself (the signed length) and
// accepted by the local route (it only caps); either way complete refuses the photo.
func testWrongSizeUpload(t *testing.T, d driver) {
	f := newFlow(t, d)
	token, _ := f.person(f.orgA)
	_, reg := f.register(token, validBody(1000))

	_, _ = storagetest.Upload(t, reg.Data.asUpload(), storagetest.JPEG(600), nil)

	status, out := f.complete(token, reg.Data.ID)
	require.Equal(t, http.StatusConflict, status)
	require.Equal(t, "MEDIA_NOT_UPLOADED", out.reason())
	require.Equal(t, "PENDING", f.rows(f.orgA)[0].Status)
}

func testRegisterRejections(t *testing.T, d driver) {
	f := newFlow(t, d)
	token, _ := f.person(f.orgA)

	with := func(change func(map[string]any)) map[string]any {
		body := validBody(1024)
		change(body)
		return body
	}
	key := func() map[string]string { return map[string]string{"Idempotency-Key": newIdempotencyKey()} }

	tests := []struct {
		name       string
		body       map[string]any
		headers    map[string]string
		wantStatus int
		wantCode   string
		wantReason string
		wantField  string
	}{
		{"a type that is not an image", with(func(b map[string]any) { b["content_type"] = "application/pdf" }), key(), 422, "UNPROCESSABLE", "MEDIA_TYPE_NOT_ALLOWED", ""},
		{"a gif", with(func(b map[string]any) { b["content_type"] = "image/gif" }), key(), 422, "UNPROCESSABLE", "MEDIA_TYPE_NOT_ALLOWED", ""},
		{"a file over 10 MB", with(func(b map[string]any) { b["size_bytes"] = 10<<20 + 1 }), key(), 413, "PAYLOAD_TOO_LARGE", "MEDIA_TOO_LARGE", ""},
		{"a size of zero", with(func(b map[string]any) { b["size_bytes"] = 0 }), key(), 422, "VALIDATION_ERROR", "", "size_bytes"},
		{"a negative size", with(func(b map[string]any) { b["size_bytes"] = -5 }), key(), 422, "VALIDATION_ERROR", "", "size_bytes"},
		{"an unknown purpose", with(func(b map[string]any) { b["purpose"] = "SELFIE" }), key(), 422, "VALIDATION_ERROR", "", "purpose"},
		{"no capture source", with(func(b map[string]any) { delete(b, "capture_source") }), key(), 422, "VALIDATION_ERROR", "", "capture_source"},
		{"an unknown capture source", with(func(b map[string]any) { b["capture_source"] = "DRONE" }), key(), 422, "VALIDATION_ERROR", "", "capture_source"},
		{"a latitude without a longitude", with(func(b map[string]any) { b["latitude"] = -6.2 }), key(), 422, "VALIDATION_ERROR", "", "latitude"},
		{"a latitude out of range", with(func(b map[string]any) { b["latitude"], b["longitude"] = 91.0, 106.8 }), key(), 422, "VALIDATION_ERROR", "", "latitude"},
		{"a capture time in the future", with(func(b map[string]any) {
			b["captured_at"] = time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
		}), key(), 422, "VALIDATION_ERROR", "", "captured_at"},
		{"an unknown field", with(func(b map[string]any) { b["storage_key"] = "1/REPORT/2026/10/1-x" }), key(), 422, "VALIDATION_ERROR", "", "storage_key"},
		{"no Idempotency-Key", validBody(1024), nil, 422, "VALIDATION_ERROR", "", "Idempotency-Key"},
		{"an Idempotency-Key that is not a UUID", validBody(1024), map[string]string{"Idempotency-Key": "abc"}, 422, "VALIDATION_ERROR", "", "Idempotency-Key"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			status, _, raw := f.raw(http.MethodPost, "/media", token, tc.body, tc.headers)
			require.Equal(t, tc.wantStatus, status, string(raw))
			out := decode[any](t, status, raw)
			require.Equal(t, tc.wantCode, out.Error.Code)
			require.Equal(t, tc.wantReason, out.reason())
			if tc.wantField != "" {
				require.NotEmpty(t, out.Error.Errors)
				require.Equal(t, tc.wantField, out.Error.Errors[0].Field)
			}
		})
	}
	require.Empty(t, f.rows(f.orgA), "a refused registration leaves no row")
}

func testOtherPerson(t *testing.T, d driver) {
	f := newFlow(t, d)
	owner, _ := f.person(f.orgA)
	other, _ := f.person(f.orgA)
	_, reg := f.register(owner, validBody(512))
	uploaded, answer := storagetest.Upload(t, reg.Data.asUpload(), storagetest.JPEG(512), nil)
	require.Less(t, uploaded, 300, "%s", answer)

	status, out := f.complete(other, reg.Data.ID)
	require.Equal(t, http.StatusNotFound, status, "another person's photo looks like no photo at all")
	require.Equal(t, "MEDIA_NOT_FOUND", out.reason())
	require.Equal(t, "PENDING", f.rows(f.orgA)[0].Status, "and was not changed")

	status, out = f.complete(owner, reg.Data.ID)
	require.Equal(t, http.StatusOK, status)
	require.Equal(t, "UPLOADED", out.Data.Status)
}

func testCrossTenant(t *testing.T, d driver) {
	f := newFlow(t, d)
	inA, _ := f.person(f.orgA)
	inB, _ := f.person(f.orgB)
	_, reg := f.register(inA, validBody(512))
	uploaded, answer := storagetest.Upload(t, reg.Data.asUpload(), storagetest.JPEG(512), nil)
	require.Less(t, uploaded, 300, "%s", answer)

	status, out := f.complete(inB, reg.Data.ID)
	require.Equal(t, http.StatusNotFound, status)
	require.Equal(t, "NOT_FOUND", out.Error.Code)
	require.Equal(t, "MEDIA_NOT_FOUND", out.reason())
	require.Equal(t, "PENDING", f.rows(f.orgA)[0].Status, "org A's photo is unchanged")
	require.Empty(t, f.rows(f.orgB), "org B sees nothing of org A")

	// B's own registration gets a key under B, not under A.
	_, regB := f.register(inB, validBody(512))
	require.NotEqual(t, reg.Data.ID, regB.Data.ID)
	require.True(t, strings.HasPrefix(f.rows(f.orgB)[0].Key, itoa(f.orgB)+"/"))
}

func testIdempotentReplay(t *testing.T, d driver) {
	f := newFlow(t, d)
	token, _ := f.person(f.orgA)
	key := map[string]string{"Idempotency-Key": newIdempotencyKey()}

	status, _, firstRaw := f.api(token, http.MethodPost, "/media", validBody(512), key)
	require.Equal(t, http.StatusCreated, status)
	first := decode[uploadInfo](t, status, firstRaw)

	status, header, secondRaw := f.api(token, http.MethodPost, "/media", validBody(512), key)
	require.Equal(t, http.StatusCreated, status, "a replay answers what the first call answered")
	require.Equal(t, "true", header.Get("Idempotent-Replayed"))
	second := decode[uploadInfo](t, status, secondRaw)
	require.Equal(t, first.Data.ID, second.Data.ID, "the same id")
	require.Equal(t, first.Data.UploadURL, second.Data.UploadURL, "and the same URL")
	require.Len(t, f.rows(f.orgA), 1, "the replay registered nothing")

	// The same key for another request is a conflict, and registers nothing either.
	status, _, raw := f.api(token, http.MethodPost, "/media", validBody(999), key)
	require.Equal(t, http.StatusConflict, status)
	require.Equal(t, "IDEMPOTENCY_KEY_REUSED", decode[any](t, status, raw).reason())
	require.Len(t, f.rows(f.orgA), 1)
}

func testAuthentication(t *testing.T, d driver) {
	f := newFlow(t, d)
	key := map[string]string{"Idempotency-Key": newIdempotencyKey()}

	status, _, raw := f.raw(http.MethodPost, "/media", "", validBody(512), key)
	require.Equal(t, http.StatusUnauthorized, status)
	require.Equal(t, "UNAUTHENTICATED", decode[any](t, status, raw).Error.Code)

	status, _, _ = f.raw(http.MethodPost, "/media/1/complete", "", nil, nil)
	require.Equal(t, http.StatusUnauthorized, status)
}

func testImpossibleID(t *testing.T, d driver) {
	f := newFlow(t, d)
	token, _ := f.person(f.orgA)
	for _, id := range []string{"0", "-4", "abc", "99999999999999999999", "1.5"} {
		status, _, raw := f.raw(http.MethodPost, "/media/"+id+"/complete", token, nil, nil)
		require.Equal(t, http.StatusNotFound, status, id)
		require.Equal(t, "MEDIA_NOT_FOUND", decode[any](t, status, raw).reason(), id)
	}
	status, out := f.complete(token, 987654)
	require.Equal(t, http.StatusNotFound, status, "an id nobody registered")
	require.Equal(t, "MEDIA_NOT_FOUND", out.reason())
}
