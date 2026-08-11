// Package s3 implements a repository backend backed by Amazon S3 or any
// S3-compatible object store.
//
// The layout matches the local backend byte for byte, so a repository can be
// copied between the two with a plain recursive copy.
package s3

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"path"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/feature/s3/manager"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"

	"github.com/senecal-jjs/cmd-line-backup/internal/backend"
)

// Defaults for the multipart upload path. Packs are around 16 MB, so most
// uploads are a single part; the part size matters for the occasional larger
// object.
const (
	defaultPartSize    = 8 << 20
	defaultConcurrency = 4
	defaultMaxAttempts = 5
)

// Config describes how to reach a repository in S3.
type Config struct {
	Bucket string
	Prefix string

	// Region is the AWS region. If empty it is taken from the environment or
	// the shared AWS configuration.
	Region string

	// Endpoint overrides the S3 endpoint, for S3-compatible services such as
	// MinIO. Empty means the standard AWS endpoints.
	Endpoint string

	// UsePathStyle selects bucket-in-path addressing rather than
	// bucket-as-subdomain. Most S3-compatible servers require it.
	UsePathStyle bool

	// StorageClass optionally sets the storage class for uploaded objects,
	// for example STANDARD_IA or GLACIER_IR.
	StorageClass string

	// MaxAttempts bounds retries of failed requests. Zero selects a default.
	MaxAttempts int

	// PartSize and Concurrency tune multipart uploads. Zero selects defaults.
	PartSize    int64
	Concurrency int
}

// ParseURL parses a repository location of the form s3://bucket/optional/prefix.
func ParseURL(raw string) (Config, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return Config{}, fmt.Errorf("s3: parsing repository URL %q: %w", raw, err)
	}
	if u.Scheme != "s3" {
		return Config{}, fmt.Errorf("s3: repository URL %q must use the s3:// scheme", raw)
	}
	if u.Host == "" {
		return Config{}, fmt.Errorf("s3: repository URL %q is missing a bucket name", raw)
	}
	return Config{
		Bucket: u.Host,
		Prefix: strings.Trim(u.Path, "/"),
	}, nil
}

// Backend stores repository objects as S3 objects.
type Backend struct {
	client   *s3.Client
	uploader *manager.Uploader
	cfg      Config
}

// Open connects to the S3 bucket described by cfg. Credentials come from the
// standard AWS sources: environment variables, the shared credentials file,
// or the instance/container role.
func Open(ctx context.Context, cfg Config) (*Backend, error) {
	if cfg.Bucket == "" {
		return nil, errors.New("s3: a bucket name is required")
	}
	if cfg.MaxAttempts == 0 {
		cfg.MaxAttempts = defaultMaxAttempts
	}
	if cfg.PartSize == 0 {
		cfg.PartSize = defaultPartSize
	}
	if cfg.Concurrency == 0 {
		cfg.Concurrency = defaultConcurrency
	}

	opts := []func(*awsconfig.LoadOptions) error{
		// The SDK's default retryer already backs off exponentially with
		// jitter and retries throttling and transient network failures, which
		// is exactly what long-running uploads need.
		awsconfig.WithRetryMaxAttempts(cfg.MaxAttempts),
	}
	if cfg.Region != "" {
		opts = append(opts, awsconfig.WithRegion(cfg.Region))
	}

	awsCfg, err := awsconfig.LoadDefaultConfig(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("s3: loading AWS configuration: %w", err)
	}

	client := s3.NewFromConfig(awsCfg, func(o *s3.Options) {
		if cfg.Endpoint != "" {
			o.BaseEndpoint = aws.String(cfg.Endpoint)
		}
		o.UsePathStyle = cfg.UsePathStyle
	})

	uploader := manager.NewUploader(client, func(u *manager.Uploader) {
		u.PartSize = cfg.PartSize
		u.Concurrency = cfg.Concurrency
	})

	return &Backend{client: client, uploader: uploader, cfg: cfg}, nil
}

// Location implements backend.Backend.
func (b *Backend) Location() string {
	if b.cfg.Prefix == "" {
		return "s3://" + b.cfg.Bucket
	}
	return "s3://" + path.Join(b.cfg.Bucket, b.cfg.Prefix)
}

// key returns the full S3 object key for a handle.
func (b *Backend) key(h backend.Handle) string {
	if b.cfg.Prefix == "" {
		return h.Path()
	}
	return b.cfg.Prefix + "/" + h.Path()
}

// Save implements backend.Backend.
func (b *Backend) Save(ctx context.Context, h backend.Handle, rd io.Reader, size int64) error {
	if err := h.Validate(); err != nil {
		return err
	}

	// The uploader is happy to store whatever the reader yields. Counting the
	// bytes lets a truncated stream be rejected instead of silently producing a
	// short object that would only be discovered at restore time.
	counter := &countingReader{r: rd}

	input := &s3.PutObjectInput{
		Bucket: aws.String(b.cfg.Bucket),
		Key:    aws.String(b.key(h)),
		Body:   counter,
	}
	if b.cfg.StorageClass != "" {
		input.StorageClass = types.StorageClass(b.cfg.StorageClass)
	}

	if _, err := b.uploader.Upload(ctx, input); err != nil {
		return fmt.Errorf("s3: uploading %s: %w", h, err)
	}
	if counter.n != size {
		// The object is already stored, so remove it rather than leave a
		// corrupt object behind.
		_, _ = b.client.DeleteObject(ctx, &s3.DeleteObjectInput{
			Bucket: aws.String(b.cfg.Bucket),
			Key:    aws.String(b.key(h)),
		})
		return fmt.Errorf("s3: %s: uploaded %d bytes but %d were declared", h, counter.n, size)
	}
	return nil
}

type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

// Load implements backend.Backend.
func (b *Backend) Load(ctx context.Context, h backend.Handle, offset, length int64) (io.ReadCloser, error) {
	if err := h.Validate(); err != nil {
		return nil, err
	}
	if offset < 0 || length < 0 {
		return nil, fmt.Errorf("%w: offset %d, length %d", backend.ErrInvalidRange, offset, length)
	}

	input := &s3.GetObjectInput{
		Bucket: aws.String(b.cfg.Bucket),
		Key:    aws.String(b.key(h)),
	}
	if offset > 0 || length > 0 {
		input.Range = aws.String(rangeHeader(offset, length))
	}

	out, err := b.client.GetObject(ctx, input)
	if err != nil {
		return nil, b.translateError(err, h)
	}

	// S3 clamps a range that runs past the end of an object and returns the
	// bytes that do exist, rather than failing. A short read here therefore
	// means the caller asked for data that is not there, which for a pack read
	// signals a stale or corrupt index.
	if length > 0 && out.ContentLength != nil && *out.ContentLength != length {
		out.Body.Close()
		return nil, fmt.Errorf("%w: %s: asked for %d bytes at offset %d but only %d are available",
			backend.ErrInvalidRange, h, length, offset, *out.ContentLength)
	}
	return out.Body, nil
}

// rangeHeader builds an HTTP byte range. A length of zero means "to the end".
func rangeHeader(offset, length int64) string {
	if length == 0 {
		return fmt.Sprintf("bytes=%d-", offset)
	}
	return fmt.Sprintf("bytes=%d-%d", offset, offset+length-1)
}

// Stat implements backend.Backend.
func (b *Backend) Stat(ctx context.Context, h backend.Handle) (backend.FileInfo, error) {
	if err := h.Validate(); err != nil {
		return backend.FileInfo{}, err
	}

	out, err := b.client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(b.cfg.Bucket),
		Key:    aws.String(b.key(h)),
	})
	if err != nil {
		return backend.FileInfo{}, b.translateError(err, h)
	}

	var size int64
	if out.ContentLength != nil {
		size = *out.ContentLength
	}
	return backend.FileInfo{Handle: h, Size: size}, nil
}

// List implements backend.Backend.
func (b *Backend) List(ctx context.Context, t backend.FileType, fn func(backend.FileInfo) error) error {
	if !t.Valid() {
		return fmt.Errorf("backend: unknown file type %q", t)
	}

	if t == backend.TypeConfig {
		info, err := b.Stat(ctx, backend.Handle{Type: t})
		if errors.Is(err, backend.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		return fn(info)
	}

	prefix := backend.Prefix(t) + "/"
	if b.cfg.Prefix != "" {
		prefix = b.cfg.Prefix + "/" + prefix
	}

	paginator := s3.NewListObjectsV2Paginator(b.client, &s3.ListObjectsV2Input{
		Bucket: aws.String(b.cfg.Bucket),
		Prefix: aws.String(prefix),
	})
	for paginator.HasMorePages() {
		page, err := paginator.NextPage(ctx)
		if err != nil {
			return fmt.Errorf("s3: listing %s objects: %w", t, err)
		}
		for _, obj := range page.Contents {
			if obj.Key == nil {
				continue
			}
			// Object names never contain a separator, so the name is always the
			// final path component regardless of any sharding directories.
			name := path.Base(*obj.Key)
			if name == "" || name == "." || name == "/" {
				continue
			}
			var size int64
			if obj.Size != nil {
				size = *obj.Size
			}
			if err := fn(backend.FileInfo{
				Handle: backend.Handle{Type: t, Name: name},
				Size:   size,
			}); err != nil {
				return err
			}
		}
	}
	return nil
}

// Remove implements backend.Backend.
func (b *Backend) Remove(ctx context.Context, h backend.Handle) error {
	if err := h.Validate(); err != nil {
		return err
	}

	// DeleteObject reports success for keys that never existed, so existence is
	// confirmed first to give callers the documented ErrNotFound.
	if _, err := b.Stat(ctx, h); err != nil {
		return err
	}
	if _, err := b.client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(b.cfg.Bucket),
		Key:    aws.String(b.key(h)),
	}); err != nil {
		return fmt.Errorf("s3: removing %s: %w", h, err)
	}
	return nil
}

// Close implements backend.Backend.
func (b *Backend) Close() error { return nil }

// Client exposes the underlying S3 client. It exists for tests and
// administrative tooling that need operations outside the Backend interface,
// such as creating a bucket.
func (b *Backend) Client() *s3.Client { return b.client }

// translateError maps S3 API errors onto the backend package's sentinels.
func (b *Backend) translateError(err error, h backend.Handle) error {
	var noSuchKey *types.NoSuchKey
	if errors.As(err, &noSuchKey) {
		return fmt.Errorf("%w: %s", backend.ErrNotFound, h)
	}
	var notFound *types.NotFound
	if errors.As(err, &notFound) {
		return fmt.Errorf("%w: %s", backend.ErrNotFound, h)
	}

	var apiErr smithy.APIError
	if errors.As(err, &apiErr) {
		switch apiErr.ErrorCode() {
		case "NoSuchKey", "NotFound", "404":
			return fmt.Errorf("%w: %s", backend.ErrNotFound, h)
		case "InvalidRange":
			return fmt.Errorf("%w: %s", backend.ErrInvalidRange, h)
		case "NoSuchBucket":
			return fmt.Errorf("s3: bucket %s does not exist", b.cfg.Bucket)
		case "AccessDenied", "InvalidAccessKeyId", "SignatureDoesNotMatch":
			return fmt.Errorf("s3: access denied for %s; check the credentials and bucket policy: %w", h, err)
		}
	}
	return fmt.Errorf("s3: %s: %w", h, err)
}

// Ensure Backend satisfies the interface.
var _ backend.Backend = (*Backend)(nil)
