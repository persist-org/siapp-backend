package media_test

import (
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"siapp/pkg/media"
	"siapp/pkg/media/storagetest"
)

// Views fill the photos arrays of a record's response. The URL works as it is handed out, with
// either driver.
func TestViews(t *testing.T) {
	for _, d := range drivers {
		t.Run(d.name, func(t *testing.T) {
			f := newFlow(t, d)
			token, owner := f.person(f.orgA)
			inB, _ := f.person(f.orgB)

			first, _ := f.photo(token, "REPORT", true)
			second, _ := f.photo(token, "REPORT", true)
			pending, _ := f.photo(token, "REPORT", false)
			foreign, _ := f.photo(inB, "REPORT", true)
			require.NoError(t, f.attach(f.orgA, media.AttachInput{
				MediaIDs: []int64{first, second}, OwnerID: owner, Purpose: media.PurposeReport, EntityType: "REPORT", EntityID: 7}))

			t.Run("one URL per photo, in the order asked, each serving the photo", func(t *testing.T) {
				views := f.views(f.orgA, "REPORT", 7, []int64{second, first})
				require.Len(t, views, 2)
				require.Equal(t, []int64{second, first}, []int64{views[0].ID, views[1].ID})

				for _, v := range views {
					require.Equal(t, "image/jpeg", v.ContentType)
					require.Equal(t, media.CaptureSource("CAMERA"), v.CaptureSource)
					status, header, body := storagetest.Download(t, v.URL)
					require.Equal(t, http.StatusOK, status, v.URL)
					require.Equal(t, storagetest.JPEG(900), body)
					require.Equal(t, "image/jpeg", header.Get("Content-Type"))
				}
				require.NotEqual(t, views[0].URL, views[1].URL)
			})

			t.Run("carries the capture metadata", func(t *testing.T) {
				captured := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
				body := validBody(900)
				body["captured_at"] = captured.Format(time.RFC3339)
				status, reg := f.register(token, body)
				require.Equal(t, http.StatusCreated, status)
				uploaded, answer := storagetest.Upload(t, reg.Data.asUpload(), storagetest.JPEG(900), nil)
				require.Less(t, uploaded, 300, "%s", answer)
				status, _ = f.complete(token, reg.Data.ID)
				require.Equal(t, http.StatusOK, status)
				require.NoError(t, f.attach(f.orgA, media.AttachInput{
					MediaIDs: []int64{reg.Data.ID}, OwnerID: owner, Purpose: media.PurposeReport, EntityType: "REPORT", EntityID: 8}))

				views := f.views(f.orgA, "REPORT", 8, []int64{reg.Data.ID})
				require.Len(t, views, 1)
				require.NotNil(t, views[0].CapturedAt)
				require.True(t, captured.Equal(*views[0].CapturedAt))
			})

			t.Run("leaves out what cannot be shown instead of failing the record", func(t *testing.T) {
				unclaimed, _ := f.photo(token, "REPORT", true)
				views := f.views(f.orgA, "REPORT", 7, []int64{first, pending, foreign, 987654, unclaimed, second})
				require.Len(t, views, 2, "a photo never confirmed, another organization's, a missing one and one no record attached are skipped")
				require.Equal(t, []int64{first, second}, []int64{views[0].ID, views[1].ID})
			})

			t.Run("only the photos of the record asked for", func(t *testing.T) {
				// first and second belong to REPORT 7: another report, or another kind of record
				// with the same number, gets none of them, even when its module passes their ids.
				require.Empty(t, f.views(f.orgA, "REPORT", 8, []int64{first, second}))
				require.Empty(t, f.views(f.orgA, "PATROL_SCAN", 7, []int64{first, second}))
				views := f.views(f.orgA, "REPORT", 7, []int64{first, second})
				require.Len(t, views, 2)
			})

			t.Run("nothing asked, nothing returned", func(t *testing.T) {
				require.Empty(t, f.views(f.orgA, "REPORT", 7, nil))
				require.NotNil(t, f.views(f.orgA, "REPORT", 7, []int64{}), "an empty list, not null, so it encodes as []")
			})

			t.Run("another organization cannot see them", func(t *testing.T) {
				require.Empty(t, f.views(f.orgB, "REPORT", 7, []int64{first, second}))
			})
		})
	}
}

// Views hand out URLs that stop working: an hour for a view, so a leaked response is not a
// permanent link to a photo.
func TestViewURLsExpire(t *testing.T) {
	f := newFlow(t, drivers[0])
	token, owner := f.person(f.orgA)
	photo, _ := f.photo(token, "REPORT", true)
	require.NoError(t, f.attach(f.orgA, media.AttachInput{
		MediaIDs: []int64{photo}, OwnerID: owner, Purpose: media.PurposeReport, EntityType: "REPORT", EntityID: 1}))

	views := f.views(f.orgA, "REPORT", 1, []int64{photo})
	require.Len(t, views, 1)
	status, _, _ := storagetest.Download(t, views[0].URL)
	require.Equal(t, http.StatusOK, status)

	f.clock.Advance(59 * time.Minute)
	status, _, _ = storagetest.Download(t, views[0].URL)
	require.Equal(t, http.StatusOK, status, "still valid just before the hour")
	f.clock.Advance(2 * time.Minute)
	status, _, _ = storagetest.Download(t, views[0].URL)
	require.Equal(t, http.StatusForbidden, status, "and refused after it")
}
