package media_test

import (
	"errors"
	"net/http"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"siapp/internal/db"
	"siapp/pkg/media"
	"siapp/pkg/platform/apperr"
	"siapp/pkg/platform/database"
	"siapp/pkg/platform/testkit"
)

// requireAttachError checks the reason, the code and the photo the error names.
func requireAttachError(t *testing.T, err error, wantStatus int, wantReason string, wantMediaID int64) {
	t.Helper()
	appErr, ok := errors.AsType[*apperr.Error](err)
	require.True(t, ok, "want an *apperr.Error, got %T: %v", err, err)
	require.Equal(t, wantStatus, appErr.Status)
	require.Equal(t, wantReason, appErr.Reason)
	if wantMediaID != 0 {
		require.EqualValues(t, wantMediaID, appErr.Details["media_id"], "the error names the photo")
	}
}

// Attach is what every feature module calls inside its own transaction.
func TestAttach(t *testing.T) {
	// A setup with two people in org A, one in org B, on the local driver: Attach only reads
	// and writes rows, so the driver makes no difference to it.
	setup := func(t *testing.T) (f *flow, owner, other, inB int64) {
		f = newFlow(t, drivers[0])
		_, owner = f.person(f.orgA)
		_, other = f.person(f.orgA)
		_, inB = f.person(f.orgB)
		return f, owner, other, inB
	}
	report := func(owner int64, ids ...int64) media.AttachInput {
		return media.AttachInput{MediaIDs: ids, OwnerID: owner, Purpose: media.PurposeReport, EntityType: "REPORT", EntityID: 42}
	}

	t.Run("attaches every photo to the record", func(t *testing.T) {
		f, owner, _, _ := setup(t)
		a, b, c := f.insert(f.orgA, owner, "REPORT", "UPLOADED"), f.insert(f.orgA, owner, "REPORT", "UPLOADED"), f.insert(f.orgA, owner, "REPORT", "UPLOADED")
		untouched := f.insert(f.orgA, owner, "REPORT", "UPLOADED")

		require.NoError(t, f.attach(f.orgA, report(owner, a, b, c)))

		for _, id := range []int64{a, b, c} {
			entityType, entityID := f.attachedTo(f.orgA, id)
			require.Equal(t, "REPORT", entityType)
			require.EqualValues(t, 42, entityID)
		}
		entityType, entityID := f.attachedTo(f.orgA, untouched)
		require.Empty(t, entityType, "a photo that was not named stays free")
		require.Zero(t, entityID)
	})

	t.Run("nothing to attach is not an error", func(t *testing.T) {
		f, owner, _, _ := setup(t)
		require.NoError(t, f.attach(f.orgA, report(owner)))
		require.NoError(t, f.attach(f.orgA, media.AttachInput{MediaIDs: []int64{}, OwnerID: owner, Purpose: media.PurposeReport, EntityType: "REPORT", EntityID: 1}))
	})

	t.Run("photos owned by someone else are not found", func(t *testing.T) {
		f, owner, other, _ := setup(t)
		mine := f.insert(f.orgA, owner, "REPORT", "UPLOADED")
		theirs := f.insert(f.orgA, other, "REPORT", "UPLOADED")

		err := f.attach(f.orgA, report(owner, mine, theirs))
		requireAttachError(t, err, http.StatusNotFound, "MEDIA_NOT_FOUND", theirs)

		// All or nothing: the photo that was fine is not attached either, because the module's
		// transaction rolled back.
		entityType, _ := f.attachedTo(f.orgA, mine)
		require.Empty(t, entityType)
		entityType, _ = f.attachedTo(f.orgA, theirs)
		require.Empty(t, entityType)
	})

	t.Run("photos that were never confirmed", func(t *testing.T) {
		f, owner, _, _ := setup(t)
		pending := f.insert(f.orgA, owner, "REPORT", "PENDING")

		err := f.attach(f.orgA, report(owner, pending))
		requireAttachError(t, err, http.StatusConflict, "MEDIA_NOT_UPLOADED", pending)
		entityType, _ := f.attachedTo(f.orgA, pending)
		require.Empty(t, entityType)
	})

	t.Run("a photo for another kind of record", func(t *testing.T) {
		f, owner, _, _ := setup(t)
		patrol := f.insert(f.orgA, owner, "PATROL_SCAN", "UPLOADED")

		err := f.attach(f.orgA, report(owner, patrol))
		requireAttachError(t, err, http.StatusUnprocessableEntity, "MEDIA_PURPOSE_MISMATCH", patrol)
		entityType, _ := f.attachedTo(f.orgA, patrol)
		require.Empty(t, entityType)
	})

	t.Run("a photo another record already claimed", func(t *testing.T) {
		f, owner, _, _ := setup(t)
		photo := f.insert(f.orgA, owner, "REPORT", "UPLOADED")
		require.NoError(t, f.attach(f.orgA, report(owner, photo)))

		second := report(owner, photo)
		second.EntityID = 99
		err := f.attach(f.orgA, second)
		requireAttachError(t, err, http.StatusConflict, "MEDIA_ALREADY_ATTACHED", photo)

		entityType, entityID := f.attachedTo(f.orgA, photo)
		require.Equal(t, "REPORT", entityType)
		require.EqualValues(t, 42, entityID, "the first record keeps it")

		// The same record asking again is not a quiet success either: nothing new was attached.
		requireAttachError(t, f.attach(f.orgA, report(owner, photo)), http.StatusConflict, "MEDIA_ALREADY_ATTACHED", 0)
	})

	t.Run("more photos than the limit", func(t *testing.T) {
		f, owner, _, _ := setup(t)
		var ids []int64
		for range 6 {
			ids = append(ids, f.insert(f.orgA, owner, "REPORT", "UPLOADED"))
		}

		// The default is 5.
		requireAttachError(t, f.attach(f.orgA, report(owner, ids...)), http.StatusUnprocessableEntity, "MEDIA_LIMIT_EXCEEDED", 0)
		for _, id := range ids {
			entityType, _ := f.attachedTo(f.orgA, id)
			require.Empty(t, entityType, "the limit is checked before anything is attached")
		}
		require.NoError(t, f.attach(f.orgA, report(owner, ids[:5]...)), "exactly the limit is fine")

		// A module sets its own.
		in := report(owner, ids[5])
		in.Max = 1
		in.EntityID = 43
		require.NoError(t, f.attach(f.orgA, in))
		in = report(owner, f.insert(f.orgA, owner, "REPORT", "UPLOADED"), f.insert(f.orgA, owner, "REPORT", "UPLOADED"))
		in.Max = 1
		requireAttachError(t, f.attach(f.orgA, in), http.StatusUnprocessableEntity, "MEDIA_LIMIT_EXCEEDED", 0)
	})

	t.Run("the same photo twice is a validation error", func(t *testing.T) {
		f, owner, _, _ := setup(t)
		photo := f.insert(f.orgA, owner, "REPORT", "UPLOADED")

		err := f.attach(f.orgA, report(owner, photo, photo))
		appErr, ok := errors.AsType[*apperr.Error](err)
		require.True(t, ok)
		require.Equal(t, apperr.CodeValidation, appErr.Code)
		require.Equal(t, "media_ids", appErr.Fields[0].Field)
		entityType, _ := f.attachedTo(f.orgA, photo)
		require.Empty(t, entityType)
	})

	t.Run("an id that does not exist, or another organization's", func(t *testing.T) {
		f, owner, _, inB := setup(t)
		foreign := f.insert(f.orgB, inB, "REPORT", "UPLOADED")

		requireAttachError(t, f.attach(f.orgA, report(owner, foreign)), http.StatusNotFound, "MEDIA_NOT_FOUND", foreign)
		requireAttachError(t, f.attach(f.orgA, report(owner, 987654)), http.StatusNotFound, "MEDIA_NOT_FOUND", 987654)
		// Even with the owner's id spelled as B's person: row-level security hides the row.
		requireAttachError(t, f.attach(f.orgA, report(inB, foreign)), http.StatusNotFound, "MEDIA_NOT_FOUND", foreign)

		entityType, _ := f.attachedTo(f.orgB, foreign)
		require.Empty(t, entityType, "org B's photo is untouched")
	})

	t.Run("the first photo that cannot be attached is the one named", func(t *testing.T) {
		f, owner, other, _ := setup(t)
		fine := f.insert(f.orgA, owner, "REPORT", "UPLOADED")
		wrongPurpose := f.insert(f.orgA, owner, "EMEMO", "UPLOADED")
		pending := f.insert(f.orgA, owner, "REPORT", "PENDING")
		theirs := f.insert(f.orgA, other, "REPORT", "UPLOADED")

		requireAttachError(t, f.attach(f.orgA, report(owner, fine, wrongPurpose, pending, theirs)), http.StatusUnprocessableEntity, "MEDIA_PURPOSE_MISMATCH", wrongPurpose)
		requireAttachError(t, f.attach(f.orgA, report(owner, fine, pending, wrongPurpose)), http.StatusConflict, "MEDIA_NOT_UPLOADED", pending)
		requireAttachError(t, f.attach(f.orgA, report(owner, fine, theirs, pending)), http.StatusNotFound, "MEDIA_NOT_FOUND", theirs)
	})

	t.Run("a module's rollback undoes the attach", func(t *testing.T) {
		f, owner, _, _ := setup(t)
		photo := f.insert(f.orgA, owner, "REPORT", "UPLOADED")

		err := database.WithTenant(t.Context(), testkit.Pool(t), f.orgA, func(q *db.Queries) error {
			require.NoError(t, f.service.Attach(t.Context(), q, report(owner, photo)))
			return errors.New("the record itself failed to save")
		})
		require.Error(t, err)

		entityType, _ := f.attachedTo(f.orgA, photo)
		require.Empty(t, entityType, "the photo is free again: it commits and rolls back with the record")
	})

	// ⚠ Two records claiming the same photo at the same moment: exactly one gets it.
	t.Run("two records racing for one photo", func(t *testing.T) {
		f, owner, _, _ := setup(t)
		photo := f.insert(f.orgA, owner, "REPORT", "UPLOADED")

		const racers = 8
		errs := make([]error, racers)
		var wg sync.WaitGroup
		start := make(chan struct{})
		for i := range racers {
			wg.Add(1)
			go func() {
				defer wg.Done()
				in := report(owner, photo)
				in.EntityID = int64(100 + i)
				<-start
				errs[i] = f.attach(f.orgA, in)
			}()
		}
		close(start)
		wg.Wait()

		winners := 0
		for _, err := range errs {
			if err == nil {
				winners++
				continue
			}
			requireAttachError(t, err, http.StatusConflict, "MEDIA_ALREADY_ATTACHED", photo)
		}
		require.Equal(t, 1, winners, "exactly one record got the photo")
		_, entityID := f.attachedTo(f.orgA, photo)
		require.GreaterOrEqual(t, entityID, int64(100))
	})

	t.Run("a module that forgets who the photo is for gets an error, not a client error", func(t *testing.T) {
		f, owner, _, _ := setup(t)
		photo := f.insert(f.orgA, owner, "REPORT", "UPLOADED")

		for name, in := range map[string]media.AttachInput{
			"no owner":   {MediaIDs: []int64{photo}, Purpose: media.PurposeReport, EntityType: "REPORT", EntityID: 1},
			"no record":  {MediaIDs: []int64{photo}, OwnerID: owner, Purpose: media.PurposeReport, EntityType: "REPORT"},
			"no type":    {MediaIDs: []int64{photo}, OwnerID: owner, Purpose: media.PurposeReport, EntityID: 1},
			"no purpose": {MediaIDs: []int64{photo}, OwnerID: owner, EntityType: "REPORT", EntityID: 1},
		} {
			err := f.attach(f.orgA, in)
			require.Error(t, err, name)
			_, isClient := errors.AsType[*apperr.Error](err)
			require.False(t, isClient, "%s: a programming mistake is a 500, not a client error", name)
		}
	})
}
