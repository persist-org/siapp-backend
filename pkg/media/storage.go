package media

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"time"

	"siapp/pkg/platform/clock"
	"siapp/pkg/platform/config"
)

// Storage is where photo bytes live. Feature code depends on this interface only: the
// client flow (register, PUT the bytes to the URL it is given, complete) is the same for
// every driver, and the app never knows which one is behind it.
//
// A key is always one this package generated (NewKey). Every method refuses a key that does
// not match the key pattern with ErrInvalidKey, so a driver never has to trust its caller.
type Storage interface {
	// PresignPut returns what a client needs to upload the object: a URL, the method (always
	// PUT today) and the headers it must send. The URL stops working at Upload.ExpiresAt, and
	// it only creates the object: once the photo is stored it cannot replace it.
	PresignPut(ctx context.Context, key string, in PutInput) (Upload, error)
	// PresignGet returns a URL that serves the object with the given content type until ttl
	// has passed.
	PresignGet(ctx context.Context, key, contentType string, ttl time.Duration) (string, error)
	// Head reports the size and type of a stored object, or ErrObjectNotFound. The type is
	// sniffed from the first bytes (http.DetectContentType), never taken from what the client
	// said, by every driver.
	Head(ctx context.Context, key string) (Object, error)
	// Delete removes an object. An object that is already gone is not an error, like S3.
	Delete(ctx context.Context, key string) error
}

// PutInput describes the upload a client is about to make.
type PutInput struct {
	ContentType string
	// SizeBytes is the size the client declared. The local driver caps the body at it; the S3
	// driver signs it as the Content-Length, so the store refuses any other length. complete
	// checks it again against the stored object either way.
	SizeBytes int64
	TTL       time.Duration
}

// Upload is the answer of PresignPut, ready to be sent to the client as it is.
type Upload struct {
	URL string
	// Method is always PUT. It is sent to the client so that a future driver (for example a
	// presigned POST) does not need an app release.
	Method  string
	Headers map[string]string
	// ExpiresAt is when the URL stops working.
	ExpiresAt time.Time
}

// Object is what Head knows about a stored object.
type Object struct {
	SizeBytes   int64
	ContentType string
}

// Errors of the storage drivers. They are not client errors: the usecase turns them into
// apperr values or treats them as internal failures.
var (
	// ErrObjectNotFound is returned by Head for a key with no object.
	ErrObjectNotFound = errors.New("media: object not found")
	// ErrInvalidKey is returned for a key that does not match the key pattern.
	ErrInvalidKey = errors.New("media: invalid object key")
)

// keyPattern is {organization_id}/{PURPOSE}/{yyyy}/{mm}/{id}-{random}.
//
// ⚠ Nothing in it can be "." or "..", a separator other than "/", or an absolute path, so a
// key that matches can only name a file below the storage root. LocalStorage still checks the
// resolved path (see its path method); the pattern is the first of two walls.
var keyPattern = regexp.MustCompile(`^[1-9][0-9]*/[A-Z][A-Z_]*/[0-9]{4}/(?:0[1-9]|1[0-2])/[1-9][0-9]*-[a-f0-9]{32}$`)

// ValidKey reports whether key matches the key pattern.
func ValidKey(key string) bool { return keyPattern.MatchString(key) }

// NewKey returns the storage key of a new photo: {organization_id}/{purpose}/{yyyy}/{mm}/{id}-{random}.
//
// ⚠ Two things keep tenants apart and they do different jobs, so both exist.
//   - The organization id at the start of the key separates tenants IN STORAGE: in the
//     bucket (a lifecycle rule or a bucket policy can target one organization's prefix) and,
//     for the local driver, in directories (an operator can back up, size or remove one
//     tenant's folder). It authorizes nothing: a key is not a secret and anyone who could
//     guess one would not be stopped by it.
//   - The owning record, read inside database.WithTenant, is what actually AUTHORIZES a
//     viewer. There is deliberately no endpoint that serves a photo by key or by media id:
//     the module that owns the record checks the caller's right to it, and only then asks for
//     a signed URL.
//
// The random part makes the key unguessable even for someone who knows the organization,
// purpose, month and id; it is also what keeps a re-registered id from ever reusing a key.
func NewKey(organizationID int64, purpose string, now time.Time, id int64) (string, error) {
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", fmt.Errorf("generate storage key: %w", err)
	}
	now = now.UTC()
	key := fmt.Sprintf("%d/%s/%04d/%02d/%d-%s",
		organizationID, purpose, now.Year(), int(now.Month()), id, hex.EncodeToString(random[:]))
	if !ValidKey(key) {
		return "", fmt.Errorf("%w: generated %q", ErrInvalidKey, key)
	}
	return key, nil
}

// NewStorage returns the driver STORAGE_DRIVER names, after checking that it can work.
//
// ⚠ There are two implementations, not three: "minio" and "s3" are the same driver
// (S3Storage, the only code that imports the AWS SDK) with different defaults. MinIO needs an
// explicit endpoint and path-style addressing; AWS derives the endpoint from the region and
// uses virtual-hosted style. Everything else is the S3 protocol.
//
// The check runs at start-up of the API and the worker (HeadBucket for S3, a write-and-delete
// probe for local), so a wrong bucket, wrong keys or a directory nobody can write to fail the
// boot instead of the first photo a guard takes.
func NewStorage(ctx context.Context, cfg config.Storage, clk clock.Clock, logger *slog.Logger) (Storage, error) {
	switch cfg.Driver {
	case config.StorageLocal:
		local, err := NewLocal(cfg, clk)
		if err != nil {
			return nil, err
		}
		logger.InfoContext(ctx, "media storage ready", slog.String("driver", cfg.Driver), slog.String("dir", local.root))
		return local, nil
	case config.StorageMinIO, config.StorageS3:
		s3Storage, err := NewS3(ctx, cfg, clk)
		if err != nil {
			return nil, err
		}
		logger.InfoContext(ctx, "media storage ready", slog.String("driver", cfg.Driver), slog.String("bucket", cfg.S3Bucket))
		return s3Storage, nil
	default:
		// config.Parse refuses this already; a caller that skipped it still fails closed.
		return nil, fmt.Errorf("unknown STORAGE_DRIVER %q", cfg.Driver)
	}
}
