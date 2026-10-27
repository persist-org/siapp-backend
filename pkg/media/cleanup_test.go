package media_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hibiken/asynq"
	"github.com/stretchr/testify/require"

	"siapp/internal/db"
	"siapp/pkg/media"
	"siapp/pkg/platform/database"
	"siapp/pkg/platform/testkit"
)

const dayAndMore = 25 * time.Hour

func (f *flow) cleaner() *media.Cleaner {
	return media.NewCleaner(testkit.Pool(f.t), f.storage, f.clock, slog.New(slog.NewJSONHandler(io.Discard, nil)))
}

// runJob runs the worker task the way asynq does, through the handler the worker registers.
func (f *flow) runJob() error {
	f.t.Helper()
	mux := asynq.NewServeMux()
	media.RegisterJobs(mux, f.cleaner(), slog.New(slog.NewJSONHandler(io.Discard, nil)))
	return mux.ProcessTask(f.t.Context(), asynq.NewTask(media.TaskCleanup, nil))
}

func (f *flow) exists(key string) bool {
	f.t.Helper()
	_, err := f.storage.Head(f.t.Context(), key)
	if errors.Is(err, media.ErrObjectNotFound) {
		return false
	}
	require.NoError(f.t, err)
	return true
}

func (f *flow) hasRow(org, id int64) bool {
	f.t.Helper()
	for _, r := range f.rows(org) {
		if r.ID == id {
			return true
		}
	}
	return false
}

// media:cleanup removes photos nobody attached, a day after they were registered, and their
// files. It runs once per driver: the same code serves both.
func TestCleanupJob(t *testing.T) {
	for _, d := range drivers {
		t.Run(d.name, func(t *testing.T) {
			t.Run("removes an orphan, its file included, and keeps attached media", func(t *testing.T) {
				f := newFlow(t, d)
				token, owner := f.person(f.orgA)

				neverConfirmed, neverConfirmedKey := f.photo(token, "REPORT", false)
				unclaimed, unclaimedKey := f.photo(token, "REPORT", true)
				attached, attachedKey := f.photo(token, "REPORT", true)
				require.NoError(t, f.attach(f.orgA, media.AttachInput{
					MediaIDs: []int64{attached}, OwnerID: owner, Purpose: media.PurposeReport, EntityType: "REPORT", EntityID: 5}))
				for _, id := range []int64{neverConfirmed, unclaimed, attached} {
					f.age(f.orgA, id, dayAndMore)
				}
				require.True(t, f.exists(neverConfirmedKey) && f.exists(unclaimedKey) && f.exists(attachedKey), "all three files are there before the job")

				require.NoError(t, f.runJob())

				require.False(t, f.hasRow(f.orgA, neverConfirmed), "a photo never confirmed is removed...")
				require.False(t, f.exists(neverConfirmedKey), "...with its file")
				require.False(t, f.hasRow(f.orgA, unclaimed), "an uploaded photo no record claimed is removed...")
				require.False(t, f.exists(unclaimedKey), "...with its file")
				require.True(t, f.hasRow(f.orgA, attached), "an attached photo is never removed, however old")
				require.True(t, f.exists(attachedKey), "nor its file")
			})

			t.Run("keeps what is not a day old yet", func(t *testing.T) {
				f := newFlow(t, d)
				token, _ := f.person(f.orgA)
				young, youngKey := f.photo(token, "REPORT", true)
				youngPending, youngPendingKey := f.photo(token, "REPORT", false)
				f.age(f.orgA, young, 23*time.Hour)
				f.age(f.orgA, youngPending, 23*time.Hour)

				require.NoError(t, f.runJob())

				require.True(t, f.hasRow(f.orgA, young) && f.hasRow(f.orgA, youngPending), "an upload may still be in flight, or a report still being written")
				require.True(t, f.exists(youngKey) && f.exists(youngPendingKey))
			})

			t.Run("the job for org A never touches org B's media", func(t *testing.T) {
				f := newFlow(t, d)
				inA, _ := f.person(f.orgA)
				inB, _ := f.person(f.orgB)
				orphanA, keyA := f.photo(inA, "REPORT", true)
				orphanB, keyB := f.photo(inB, "REPORT", true)
				f.age(f.orgA, orphanA, dayAndMore)
				f.age(f.orgB, orphanB, dayAndMore)

				removed, err := f.cleaner().CleanupOrganization(t.Context(), f.orgA)
				require.NoError(t, err)
				require.Equal(t, 1, removed)
				require.False(t, f.hasRow(f.orgA, orphanA))
				require.False(t, f.exists(keyA))
				require.True(t, f.hasRow(f.orgB, orphanB), "org B's orphan is not org A's business")
				require.True(t, f.exists(keyB), "nor its file")

				// The whole job does take care of every organization.
				require.NoError(t, f.runJob())
				require.False(t, f.hasRow(f.orgB, orphanB))
				require.False(t, f.exists(keyB))
			})

			t.Run("a run with nothing to do is fine, and so is a second run", func(t *testing.T) {
				f := newFlow(t, d)
				require.NoError(t, f.runJob())
				token, _ := f.person(f.orgA)
				orphan, _ := f.photo(token, "REPORT", true)
				f.age(f.orgA, orphan, dayAndMore)
				require.NoError(t, f.runJob())
				require.NoError(t, f.runJob(), "the job is safe to repeat: asynq retries it")
			})

			t.Run("a file that is already gone does not stop the row from going", func(t *testing.T) {
				f := newFlow(t, d)
				token, _ := f.person(f.orgA)
				orphan, key := f.photo(token, "REPORT", true)
				f.age(f.orgA, orphan, dayAndMore)
				require.NoError(t, f.storage.Delete(t.Context(), key), "someone removed the file already")

				require.NoError(t, f.runJob())
				require.False(t, f.hasRow(f.orgA, orphan))
			})
		})
	}
}

// failingDelete is a storage whose Delete refuses one key, as an object store that is down for
// a moment does.
type failingDelete struct {
	media.Storage
	key string
}

func (s failingDelete) Delete(ctx context.Context, key string) error {
	if key == s.key {
		return errors.New("storage unavailable")
	}
	return s.Storage.Delete(ctx, key)
}

func TestCleanupKeepsTheRowWhenTheFileCannotBeDeleted(t *testing.T) {
	f := newFlow(t, drivers[0])
	token, _ := f.person(f.orgA)
	stuck, stuckKey := f.photo(token, "REPORT", true)
	fine, fineKey := f.photo(token, "REPORT", true)
	f.age(f.orgA, stuck, dayAndMore)
	f.age(f.orgA, fine, dayAndMore)
	f.storage = failingDelete{Storage: f.storage, key: stuckKey}

	err := f.runJob()
	require.Error(t, err, "the task fails, so asynq retries it")

	require.True(t, f.hasRow(f.orgA, stuck), "the row stays: dropping it would leave a file nothing knows about")
	require.True(t, f.exists(stuckKey))
	require.False(t, f.hasRow(f.orgA, fine), "one stuck file does not hold up the others")
	require.False(t, f.exists(fineKey))
}

// ⚠ An Attach that is running at the same moment as the cleanup must not lose its photo to it.
// The cleanup locks the rows it is about to delete and skips rows another transaction holds.
func TestCleanupSkipsAPhotoThatIsBeingAttached(t *testing.T) {
	f := newFlow(t, drivers[0])
	token, owner := f.person(f.orgA)
	photo, key := f.photo(token, "REPORT", true)
	f.age(f.orgA, photo, dayAndMore)

	err := database.WithTenant(t.Context(), testkit.Pool(t), f.orgA, func(q *db.Queries) error {
		require.NoError(t, f.service.Attach(t.Context(), q, media.AttachInput{
			MediaIDs: []int64{photo}, OwnerID: owner, Purpose: media.PurposeReport, EntityType: "REPORT", EntityID: 9}))

		// The record has not committed yet, so the photo still LOOKS orphaned to everyone else.
		removed, err := f.cleaner().CleanupOrganization(t.Context(), f.orgA)
		require.NoError(t, err)
		require.Zero(t, removed, "the cleanup waits its turn instead of deleting a photo that is being claimed")
		return nil
	})
	require.NoError(t, err)

	require.NoError(t, f.runJob())
	require.True(t, f.hasRow(f.orgA, photo), "once attached, the photo is safe")
	require.True(t, f.exists(key))
	entityType, entityID := f.attachedTo(f.orgA, photo)
	require.Equal(t, "REPORT", entityType)
	require.EqualValues(t, 9, entityID)
}

// With the local driver the job also removes the half-written files of abandoned uploads, once
// they are an hour old.
func TestCleanupRemovesStaleTemporaryFiles(t *testing.T) {
	f := newFlow(t, drivers[0])
	token, _ := f.person(f.orgA)
	photo, key := f.photo(token, "REPORT", true)

	dir := filepath.Join(f.root, filepath.FromSlash(filepath.Dir(key)))
	stale := filepath.Join(dir, media.TempFilePrefix+"crashed-upload")
	fresh := filepath.Join(dir, media.TempFilePrefix+"still-writing")
	lookalike := filepath.Join(dir, "not-"+media.TempFilePrefix+"temp")
	for _, name := range []string{stale, fresh, lookalike} {
		require.NoError(t, os.WriteFile(name, []byte("partial"), 0o600))
	}
	old := time.Now().Add(-2 * time.Hour)
	require.NoError(t, os.Chtimes(stale, old, old))
	require.NoError(t, os.Chtimes(lookalike, old, old))

	result, err := f.cleaner().Cleanup(t.Context())
	require.NoError(t, err)
	require.Equal(t, 1, result.TempFiles)

	require.NoFileExists(t, stale, "an abandoned upload's leftovers are removed")
	require.FileExists(t, fresh, "a file an upload may still be writing is not")
	require.FileExists(t, lookalike, "only files with the temporary prefix are ever touched")
	require.True(t, f.hasRow(f.orgA, photo))
	require.True(t, f.exists(key), "and the objects themselves are not")
}

// With S3 there is nothing like it to remove, and the job says so with a zero.
func TestCleanupHasNoTemporaryFilesWithS3(t *testing.T) {
	f := newFlow(t, drivers[1])
	result, err := f.cleaner().Cleanup(t.Context())
	require.NoError(t, err)
	require.Zero(t, result.TempFiles)
}

// The task is registered with the scheduler, hourly. Registering does not connect to Redis.
func TestCleanupSchedule(t *testing.T) {
	require.Equal(t, "media:cleanup", media.TaskCleanup)
	require.Equal(t, "40 * * * *", media.CleanupSchedule, "hourly, UTC, clear of the other periodic tasks")

	scheduler := asynq.NewScheduler(asynq.RedisClientOpt{Addr: "localhost:6379"}, &asynq.SchedulerOpts{Location: time.UTC})
	require.NoError(t, media.RegisterSchedule(scheduler))
}
