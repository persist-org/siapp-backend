package media

import "time"

const (
	// maxSizeBytes is the largest photo, 10 MB. The register request refuses a bigger
	// declared size, and both drivers refuse more bytes than were declared.
	maxSizeBytes int64 = 10 << 20

	// uploadURLTTL is how long an upload URL works. The device registers right before it
	// uploads; one that waited longer registers again.
	uploadURLTTL = 30 * time.Minute

	// maxCapturedAtSkew is how far in the future a device clock may be and still be believed
	// about when a photo was taken.
	maxCapturedAtSkew = 5 * time.Minute

	// sniffBytes is how much of a file http.DetectContentType looks at.
	sniffBytes = 512

	// blobUploadTimeout is how long one PUT to a local blob URL may take. The server's own
	// read and write timeouts (15 s and 30 s) fit a JSON API, not 10 MB over a mobile
	// connection, so the blob routes extend them for their own request only.
	blobUploadTimeout = 5 * time.Minute
	// blobReadTimeout is the same for serving a photo.
	blobReadTimeout = 2 * time.Minute

	// maxTokenLength caps what the blob routes will even try to decode.
	maxTokenLength = 2048

	// minSigningKeyBytes is the shortest key LocalStorage signs with: the size of an
	// HMAC-SHA256 key. config.Parse enforces the same for STORAGE_LOCAL_SIGNING_KEY.
	minSigningKeyBytes = 32

	// defaultAttachLimit is how many photos one record may hold when its module does not
	// say otherwise.
	defaultAttachLimit = 5

	// viewURLTTL is how long a photo URL handed out by Views works.
	viewURLTTL = time.Hour

	// orphanAge is how long a registered photo may stay unattached before the cleanup job
	// removes it, with its file.
	orphanAge = 24 * time.Hour
	// staleTempFileAge is how old a half-written file of the local driver must be before the
	// cleanup job removes it. A PUT takes minutes at the very most.
	staleTempFileAge = time.Hour
	// cleanupBatchSize is how many photos one cleanup transaction handles, so that it never
	// holds a long list of row locks while it talks to the storage.
	cleanupBatchSize = 50
	// cleanupBatchBudget bounds the time one batch spends deleting files while it holds its row
	// locks. A batch that runs out of it keeps the rest of its rows for the next run, so an
	// Attach waiting on one of them waits this long at most, not batch size times the delete
	// timeout.
	cleanupBatchBudget = 30 * time.Second
	// cleanupTimeout bounds one run of the job.
	cleanupTimeout = 30 * time.Minute
	// cleanupDeleteTimeout bounds one file deletion of the job.
	cleanupDeleteTimeout = 10 * time.Second
)

// CleanupSchedule runs the job hourly, at minute 40 UTC, clear of the other periodic tasks
// (the scheduler runs in UTC, see cmd/worker).
const CleanupSchedule = "40 * * * *"

// TaskCleanup is the worker task that runs Cleaner.Cleanup.
const TaskCleanup = "media:cleanup"

// TempFilePrefix starts the name of a file that LocalStorage is still writing. A file with
// this prefix is never an object: the cleanup job (Phase 04.2) deletes the ones an abandoned
// upload left behind.
const TempFilePrefix = ".upload-"

// BlobPath is where the local blob routes live. The upload and view URLs of the local driver
// are the API's public origin plus this path plus a token.
const BlobPath = "/api/v1/media/blobs/"

// BlobRoute is BlobPath as a router pattern.
const BlobRoute = BlobPath + "{token}"

// allowedContentTypes are the photo types the API accepts. The storage key has no extension:
// the type is data of the row, not of the name.
var allowedContentTypes = map[string]struct{}{
	"image/jpeg": {},
	"image/png":  {},
	"image/webp": {},
}
