package s3_test

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/senecal-jjs/cmd-line-backup/internal/backend"
	"github.com/senecal-jjs/cmd-line-backup/internal/backend/backendtest"
	"github.com/senecal-jjs/cmd-line-backup/internal/backend/s3/s3test"
)

// TestConformance runs the shared backend conformance suite against a real
// S3-compatible server, which is the only way to confirm that the S3 backend's
// ranged reads, listing, and error mapping actually behave like the local
// backend's. See package s3test for how to point it at a server.
func TestConformance(t *testing.T) {
	server := s3test.Require(t)
	backendtest.RunSuite(t, func(t *testing.T) backend.Backend {
		return server.Open(t)
	})
}

// TestIntegrationPackSizedObjectIsASinglePut checks that an object the size of
// a full pack is stored with one PutObject rather than a multipart upload. A
// multipart upload interrupted midway leaves parts behind that S3 keeps billing
// for, so the backend must never start one.
func TestIntegrationPackSizedObjectIsASinglePut(t *testing.T) {
	server := s3test.Require(t)
	b := server.Open(t)
	ctx := context.Background()

	data := bytes.Repeat([]byte("0123456789abcdef"), (20<<20)/16) // larger than a pack
	h := backend.Handle{Type: backend.TypePack, Name: strings.Repeat("ab", 32)}
	if err := b.Save(ctx, h, bytes.NewReader(data), int64(len(data))); err != nil {
		t.Fatalf("Save: %v", err)
	}

	prefix := strings.TrimPrefix(b.Location(), "s3://"+server.Bucket+"/")
	head, err := server.Client(t).HeadObject(ctx, &awss3.HeadObjectInput{
		Bucket: aws.String(server.Bucket),
		Key:    aws.String(prefix + "/" + h.Path()),
	})
	if err != nil {
		t.Fatalf("HeadObject: %v", err)
	}
	// Multipart objects carry an ETag of the form "<hash>-<parts>".
	if etag := aws.ToString(head.ETag); strings.Contains(etag, "-") {
		t.Errorf("the object was stored as a multipart upload (ETag %s)", etag)
	}
	if uploads := server.IncompleteUploads(t, prefix); len(uploads) > 0 {
		t.Errorf("incomplete multipart uploads were left behind: %v", uploads)
	}

	got, err := backend.LoadAll(ctx, b, h)
	if err != nil {
		t.Fatalf("LoadAll: %v", err)
	}
	if !bytes.Equal(got, data) {
		t.Error("the object read back differs from what was saved")
	}
}

// TestIntegrationShortBodyStoresNothing checks that a reader yielding fewer
// bytes than declared is rejected before anything reaches the server.
func TestIntegrationShortBodyStoresNothing(t *testing.T) {
	server := s3test.Require(t)
	b := server.Open(t)
	ctx := context.Background()

	h := backend.Handle{Type: backend.TypeIndex, Name: strings.Repeat("cd", 32)}
	short := strings.NewReader("only part of the data")
	if err := b.Save(ctx, h, short, 1000); err == nil {
		t.Fatal("Save accepted a body shorter than its declared size")
	}
	if _, err := b.Stat(ctx, h); !errors.Is(err, backend.ErrNotFound) {
		t.Errorf("a truncated object was stored (Stat: %v)", err)
	}
}
