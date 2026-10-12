// Package s3test connects tests to a real S3-compatible server, such as MinIO,
// so the S3 backend and everything built on it can be exercised end to end.
//
// Tests that use it are skipped unless GOBACKUP_S3_TEST_ENDPOINT is set, which
// keeps the ordinary test suite self-contained. `make test-integration` starts a
// throwaway MinIO server and sets the environment; to use a server of your own:
//
//	export GOBACKUP_S3_TEST_ENDPOINT=http://127.0.0.1:9000
//	export AWS_ACCESS_KEY_ID=minioadmin AWS_SECRET_ACCESS_KEY=minioadmin
//	go test ./...
package s3test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"

	"github.com/senecal-jjs/cmd-line-backup/internal/backend/s3"
)

// Environment variables that configure the test server.
const (
	EnvEndpoint = "GOBACKUP_S3_TEST_ENDPOINT"
	EnvBucket   = "GOBACKUP_S3_TEST_BUCKET"
)

// DefaultBucket is used when EnvBucket is unset.
const DefaultBucket = "gobackup-test"

// Region is the region every test client uses. S3-compatible servers accept
// any region, but the SDK insists on one being set.
const Region = "us-east-1"

// Server describes the S3-compatible server under test.
type Server struct {
	Endpoint string
	Bucket   string
}

var (
	bucketMu    sync.Mutex
	bucketReady = make(map[string]error)
)

// Require returns the configured server, skipping the test if none is
// configured. The bucket is created the first time it is needed.
func Require(t testing.TB) Server {
	t.Helper()

	endpoint := os.Getenv(EnvEndpoint)
	if endpoint == "" {
		t.Skipf("set %s to run S3 integration tests (see `make test-integration`)", EnvEndpoint)
	}
	bucket := os.Getenv(EnvBucket)
	if bucket == "" {
		bucket = DefaultBucket
	}
	s := Server{Endpoint: endpoint, Bucket: bucket}

	bucketMu.Lock()
	defer bucketMu.Unlock()
	key := endpoint + "|" + bucket
	err, done := bucketReady[key]
	if !done {
		err = s.ensureBucket()
		bucketReady[key] = err
	}
	if err != nil {
		t.Fatalf("preparing bucket %s at %s: %v", bucket, endpoint, err)
	}
	return s
}

// Config returns a backend configuration for the given key prefix.
func (s Server) Config(prefix string) s3.Config {
	return s3.Config{
		Bucket:       s.Bucket,
		Prefix:       prefix,
		Region:       Region,
		Endpoint:     s.Endpoint,
		UsePathStyle: true,
	}
}

// URL returns the repository URL for a key prefix, as the CLI expects it.
func (s Server) URL(prefix string) string {
	return "s3://" + s.Bucket + "/" + prefix
}

// NewPrefix returns a key prefix unique to this test. Everything stored under it
// is deleted when the test ends, so tests never see each other's objects and a
// long-lived server does not fill up.
func (s Server) NewPrefix(t testing.TB) string {
	t.Helper()
	name := strings.NewReplacer("/", "-", " ", "_").Replace(t.Name())
	prefix := fmt.Sprintf("test-%d-%s", time.Now().UnixNano(), name)
	t.Cleanup(func() {
		if err := s.deletePrefix(prefix); err != nil {
			t.Logf("cleaning up s3://%s/%s: %v", s.Bucket, prefix, err)
		}
	})
	return prefix
}

// Open returns a backend rooted at a fresh, empty prefix, closed when the test
// ends.
func (s Server) Open(t testing.TB) *s3.Backend {
	t.Helper()
	b, err := s3.Open(context.Background(), s.Config(s.NewPrefix(t)))
	if err != nil {
		t.Fatalf("opening the S3 backend: %v", err)
	}
	t.Cleanup(func() { b.Close() })
	return b
}

// Client returns a raw S3 client for inspecting the server directly, bypassing
// the backend under test.
func (s Server) Client(t testing.TB) *awss3.Client {
	t.Helper()
	b, err := s3.Open(context.Background(), s.Config(""))
	if err != nil {
		t.Fatalf("opening an S3 client: %v", err)
	}
	return b.Client()
}

// IncompleteUploads returns the keys of multipart uploads that were started
// under prefix but never completed or aborted.
func (s Server) IncompleteUploads(t testing.TB, prefix string) []string {
	t.Helper()
	out, err := s.Client(t).ListMultipartUploads(context.Background(), &awss3.ListMultipartUploadsInput{
		Bucket: aws.String(s.Bucket),
		Prefix: aws.String(prefix + "/"),
	})
	if err != nil {
		t.Fatalf("ListMultipartUploads: %v", err)
	}
	var keys []string
	for _, u := range out.Uploads {
		keys = append(keys, aws.ToString(u.Key))
	}
	return keys
}

func (s Server) ensureBucket() error {
	b, err := s3.Open(context.Background(), s.Config(""))
	if err != nil {
		return err
	}
	client := b.Client()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	_, err = client.CreateBucket(ctx, &awss3.CreateBucketInput{Bucket: aws.String(s.Bucket)})
	if err == nil {
		return nil
	}
	// Already existing is the normal case on repeat runs.
	var owned *types.BucketAlreadyOwnedByYou
	var exists *types.BucketAlreadyExists
	if errors.As(err, &owned) || errors.As(err, &exists) {
		return nil
	}
	if _, headErr := client.HeadBucket(ctx, &awss3.HeadBucketInput{Bucket: aws.String(s.Bucket)}); headErr == nil {
		return nil
	}
	return err
}

func (s Server) deletePrefix(prefix string) error {
	b, err := s3.Open(context.Background(), s.Config(""))
	if err != nil {
		return err
	}
	client := b.Client()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	paginator := awss3.NewListObjectsV2Paginator(client, &awss3.ListObjectsV2Input{
		Bucket: aws.String(s.Bucket),
		Prefix: aws.String(prefix + "/"),
	})
	for paginator.HasMorePages() {
		page, err := paginator.NextPage(ctx)
		if err != nil {
			return err
		}
		if len(page.Contents) == 0 {
			continue
		}
		objects := make([]types.ObjectIdentifier, 0, len(page.Contents))
		for _, obj := range page.Contents {
			objects = append(objects, types.ObjectIdentifier{Key: obj.Key})
		}
		if _, err := client.DeleteObjects(ctx, &awss3.DeleteObjectsInput{
			Bucket: aws.String(s.Bucket),
			Delete: &types.Delete{Objects: objects, Quiet: aws.Bool(true)},
		}); err != nil {
			return err
		}
	}
	return nil
}
