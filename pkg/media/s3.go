package media

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"

	"siapp/pkg/platform/clock"
	"siapp/pkg/platform/config"
)

// startupCheckTimeout bounds the HeadBucket probe at boot.
const startupCheckTimeout = 15 * time.Second

// S3Storage is the driver of "minio" and "s3": files go straight from the device to object
// storage through presigned URLs, and the API never proxies file bytes. It is the only file
// in the repository that imports the AWS SDK (aws-sdk-go-v2).
//
//   - minio requires S3_ENDPOINT and forces path-style addressing (http://host:9000/bucket/key).
//   - s3 uses the AWS endpoint of S3_REGION unless S3_ENDPOINT is set, with virtual-hosted
//     style (https://bucket.s3.region.amazonaws.com/key).
//   - S3_FORCE_PATH_STYLE overrides either default.
type S3Storage struct {
	// api talks to the storage from the API's side: Head, Delete and the start-up check.
	api *s3.Client
	// presign signs URLs for the host a DEVICE can reach. See NewS3.
	presign *s3.PresignClient
	bucket  string
	clock   clock.Clock
}

var _ Storage = (*S3Storage)(nil)

// NewS3 builds the client and proves the bucket is reachable with these keys (HeadBucket), so a
// wrong bucket or wrong keys fail the boot, not the first upload.
//
// ⚠ Two hosts, one bucket. S3_PUBLIC_ENDPOINT (optional) is the host the presigned URLs are
// signed for. Inside compose the API reaches MinIO as http://minio:9000, but a phone cannot
// resolve the name "minio", so a URL signed for that host is useless to it. A presigned URL
// is only valid for the exact host it was signed for (the host is part of the signature), so
// it cannot be rewritten afterwards either. The API therefore PRESIGNS with the public host
// and TALKS to the API-side host for Head and Delete. When the two are the same, or the public
// one is not set, one client does both.
func NewS3(ctx context.Context, cfg config.Storage, clk clock.Clock) (*S3Storage, error) {
	pathStyle := cfg.Driver == config.StorageMinIO
	if cfg.S3ForcePathStyle != nil {
		pathStyle = *cfg.S3ForcePathStyle
	}
	newClient := func(endpoint string) *s3.Client {
		o := s3.Options{
			Region:       cfg.S3Region,
			Credentials:  credentials.NewStaticCredentialsProvider(cfg.S3AccessKey, cfg.S3SecretKey, ""),
			UsePathStyle: pathStyle,
			// Newer SDK versions add a CRC32 checksum to every upload by default. On a presigned
			// URL that becomes a query parameter and a header the phone would have to compute and
			// send, and MinIO and older gateways do not all accept it. Checksums are only used
			// where the operation requires one.
			RequestChecksumCalculation: aws.RequestChecksumCalculationWhenRequired,
			ResponseChecksumValidation: aws.ResponseChecksumValidationWhenRequired,
		}
		if endpoint != "" {
			o.BaseEndpoint = aws.String(endpoint)
		}
		return s3.New(o)
	}

	api := newClient(cfg.S3Endpoint)
	signer := api
	if cfg.S3PublicEndpoint != "" && cfg.S3PublicEndpoint != cfg.S3Endpoint {
		signer = newClient(cfg.S3PublicEndpoint)
	}
	s := &S3Storage{api: api, presign: s3.NewPresignClient(signer), bucket: cfg.S3Bucket, clock: clk}

	checkCtx, cancel := context.WithTimeout(ctx, startupCheckTimeout)
	defer cancel()
	if _, err := api.HeadBucket(checkCtx, &s3.HeadBucketInput{Bucket: aws.String(s.bucket)}); err != nil {
		return nil, fmt.Errorf("s3 storage: bucket %q is not reachable with these settings (check S3_ENDPOINT, S3_BUCKET and the keys): %w", s.bucket, err)
	}
	return s, nil
}

// PresignPut signs a PUT of exactly in.SizeBytes bytes of in.ContentType. Both are signed
// headers, so the store itself refuses a different length or type; complete checks the stored
// object again, since a signed header is a promise of the store, not of this code.
func (s *S3Storage) PresignPut(ctx context.Context, key string, in PutInput) (Upload, error) {
	if !ValidKey(key) {
		return Upload{}, ErrInvalidKey
	}
	if in.ContentType == "" || in.SizeBytes < 1 || in.TTL <= 0 {
		return Upload{}, errors.New("s3 storage: a put needs a content type, a size and a lifetime")
	}
	req, err := s.presign.PresignPutObject(ctx, &s3.PutObjectInput{
		Bucket:        aws.String(s.bucket),
		Key:           aws.String(key),
		ContentType:   aws.String(in.ContentType),
		ContentLength: aws.Int64(in.SizeBytes),
	}, s3.WithPresignExpires(in.TTL))
	if err != nil {
		return Upload{}, fmt.Errorf("presign put: %w", err)
	}

	// The client sends the headers that were signed, except the two it cannot or need not set:
	// Host comes from the URL, and Content-Length from the body it sends.
	headers := make(map[string]string, len(req.SignedHeader))
	for name, values := range req.SignedHeader {
		if strings.EqualFold(name, "host") || strings.EqualFold(name, "content-length") {
			continue
		}
		headers[name] = strings.Join(values, ",")
	}
	return Upload{
		URL:       req.URL,
		Method:    req.Method,
		Headers:   headers,
		ExpiresAt: s.clock.Now().Add(in.TTL).UTC(),
	}, nil
}

// PresignGet signs a GET that answers with the given content type, inline.
func (s *S3Storage) PresignGet(ctx context.Context, key, contentType string, ttl time.Duration) (string, error) {
	if !ValidKey(key) {
		return "", ErrInvalidKey
	}
	if contentType == "" || ttl <= 0 {
		return "", errors.New("s3 storage: a get needs a content type and a lifetime")
	}
	req, err := s.presign.PresignGetObject(ctx, &s3.GetObjectInput{
		Bucket:                     aws.String(s.bucket),
		Key:                        aws.String(key),
		ResponseContentType:        aws.String(contentType),
		ResponseContentDisposition: aws.String("inline"),
	}, s3.WithPresignExpires(ttl))
	if err != nil {
		return "", fmt.Errorf("presign get: %w", err)
	}
	return req.URL, nil
}

// Head returns the stored size and the content type the object was uploaded with.
func (s *S3Storage) Head(ctx context.Context, key string) (Object, error) {
	if !ValidKey(key) {
		return Object{}, ErrInvalidKey
	}
	out, err := s.api.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String(s.bucket), Key: aws.String(key)})
	if err != nil {
		if isNotFound(err) {
			return Object{}, ErrObjectNotFound
		}
		return Object{}, fmt.Errorf("head object: %w", err)
	}
	return Object{SizeBytes: aws.ToInt64(out.ContentLength), ContentType: aws.ToString(out.ContentType)}, nil
}

// Delete removes the object. S3 answers success for a key that does not exist, so a missing
// object is not an error here either.
func (s *S3Storage) Delete(ctx context.Context, key string) error {
	if !ValidKey(key) {
		return ErrInvalidKey
	}
	if _, err := s.api.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(s.bucket), Key: aws.String(key)}); err != nil && !isNotFound(err) {
		return fmt.Errorf("delete object: %w", err)
	}
	return nil
}

// isNotFound reports S3's "no such object" in either of its forms: a HEAD has no body, so the
// SDK reports its 404 as NotFound, while other operations carry the NoSuchKey code.
func isNotFound(err error) bool {
	if _, ok := errors.AsType[*types.NotFound](err); ok {
		return true
	}
	if _, ok := errors.AsType[*types.NoSuchKey](err); ok {
		return true
	}
	apiErr, ok := errors.AsType[smithy.APIError](err)
	return ok && (apiErr.ErrorCode() == "NotFound" || apiErr.ErrorCode() == "NoSuchKey")
}
