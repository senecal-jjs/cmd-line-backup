package s3_test

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/senecal-jjs/cmd-line-backup/internal/backend"
	"github.com/senecal-jjs/cmd-line-backup/internal/backend/backendtest"
	"github.com/senecal-jjs/cmd-line-backup/internal/backend/s3"
)

// TestConformance runs the shared backend conformance suite against a real
// S3-compatible server, which is the only way to confirm that the S3 backend's
// ranged reads, listing, and error mapping actually behave like the local
// backend's.
//
// It is skipped unless an endpoint is configured. To run it against MinIO:
//
//	docker run -d -p 9000:9000 minio/minio server /data
//	export GOBACKUP_S3_TEST_ENDPOINT=http://127.0.0.1:9000
//	export AWS_ACCESS_KEY_ID=minioadmin AWS_SECRET_ACCESS_KEY=minioadmin
//	go test ./internal/backend/s3/
func TestConformance(t *testing.T) {
	endpoint := os.Getenv("GOBACKUP_S3_TEST_ENDPOINT")
	if endpoint == "" {
		t.Skip("set GOBACKUP_S3_TEST_ENDPOINT to run S3 integration tests")
	}

	bucket := os.Getenv("GOBACKUP_S3_TEST_BUCKET")
	if bucket == "" {
		bucket = "gobackup-test"
	}
	if err := ensureBucket(t, endpoint, bucket); err != nil {
		t.Fatalf("preparing bucket: %v", err)
	}

	backendtest.RunSuite(t, func(t *testing.T) backend.Backend {
		// Each test gets its own key prefix so they start from an empty
		// repository without needing to delete objects between runs.
		prefix := fmt.Sprintf("test-%d-%s", time.Now().UnixNano(), t.Name())

		b, err := s3.Open(t.Context(), s3.Config{
			Bucket:       bucket,
			Prefix:       prefix,
			Region:       "us-east-1",
			Endpoint:     endpoint,
			UsePathStyle: true,
			// A small part size makes the large-object test exercise the
			// multipart upload path rather than a single PUT.
			PartSize:    5 << 20,
			Concurrency: 2,
		})
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		return b
	})
}

func ensureBucket(t *testing.T, endpoint, bucket string) error {
	t.Helper()

	b, err := s3.Open(context.Background(), s3.Config{
		Bucket:       bucket,
		Region:       "us-east-1",
		Endpoint:     endpoint,
		UsePathStyle: true,
	})
	if err != nil {
		return err
	}
	defer b.Close()

	client := b.Client()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	_, err = client.CreateBucket(ctx, &awss3.CreateBucketInput{Bucket: aws.String(bucket)})
	if err != nil {
		// Already existing is the normal case on repeat runs.
		if _, headErr := client.HeadBucket(ctx, &awss3.HeadBucketInput{Bucket: aws.String(bucket)}); headErr == nil {
			return nil
		}
		return err
	}
	return nil
}
