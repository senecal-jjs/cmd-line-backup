package local_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/senecal-jjs/cmd-line-backup/internal/backend"
	"github.com/senecal-jjs/cmd-line-backup/internal/backend/backendtest"
	"github.com/senecal-jjs/cmd-line-backup/internal/backend/local"
)

func TestConformance(t *testing.T) {
	backendtest.RunSuite(t, func(t *testing.T) backend.Backend {
		b, err := local.Open(t.TempDir())
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		return b
	})
}

func TestOpenCreatesDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "does", "not", "exist")

	b, err := local.Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer b.Close()

	info, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if !info.IsDir() {
		t.Error("Open did not create a directory")
	}
}

// TestLayoutIsShared pins the on-disk paths, which the S3 backend mirrors. A
// repository must be movable between backends by plain file copy, so these
// locations are part of the format.
func TestLayoutIsShared(t *testing.T) {
	dir := t.TempDir()
	b, err := local.Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer b.Close()

	tests := []struct {
		handle backend.Handle
		want   string
	}{
		{backend.Handle{Type: backend.TypeConfig}, "config"},
		{backend.Handle{Type: backend.TypePack, Name: "ab12cd34"}, "packs/ab/ab12cd34"},
		{backend.Handle{Type: backend.TypeIndex, Name: "idx0001"}, "indexes/idx0001"},
		{backend.Handle{Type: backend.TypeSnapshot, Name: "snap0001"}, "snapshots/snap0001"},
		{backend.Handle{Type: backend.TypeKey, Name: "key0001"}, "keys/key0001"},
	}
	for _, tt := range tests {
		if got := tt.handle.Path(); got != tt.want {
			t.Errorf("Path() for %v = %q, want %q", tt.handle, got, tt.want)
		}

		if err := b.Save(context.Background(), tt.handle, readerOf("x"), 1); err != nil {
			t.Fatalf("Save(%v): %v", tt.handle, err)
		}
		if _, err := os.Stat(filepath.Join(dir, filepath.FromSlash(tt.want))); err != nil {
			t.Errorf("expected file at %s: %v", tt.want, err)
		}
	}
}

// TestSaveIsAtomic verifies that a failed write leaves no object behind, so a
// crash cannot publish a truncated pack that later reads would treat as valid.
func TestSaveIsAtomic(t *testing.T) {
	dir := t.TempDir()
	b, err := local.Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer b.Close()

	h := backend.Handle{Type: backend.TypePack, Name: "partial00000000"}
	// Declaring more bytes than the reader supplies simulates an interrupted upload.
	if err := b.Save(context.Background(), h, readerOf("short"), 100); err == nil {
		t.Fatal("Save succeeded despite a truncated stream")
	}

	if _, err := b.Stat(context.Background(), h); err == nil {
		t.Error("a partially written object was published")
	}

	// The temporary file must be cleaned up too.
	entries, err := os.ReadDir(filepath.Join(dir, "packs", "pa"))
	if err == nil {
		for _, e := range entries {
			t.Errorf("leftover file after failed save: %s", e.Name())
		}
	}
}

func TestPermissionsAreOwnerOnly(t *testing.T) {
	dir := t.TempDir()
	b, err := local.Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer b.Close()

	h := backend.Handle{Type: backend.TypePack, Name: "perm000000000000"}
	if err := b.Save(context.Background(), h, readerOf("data"), 4); err != nil {
		t.Fatalf("Save: %v", err)
	}

	info, err := os.Stat(filepath.Join(dir, filepath.FromSlash(h.Path())))
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("object permissions are %v, want 0600", perm)
	}
}

func TestLocation(t *testing.T) {
	dir := t.TempDir()
	b, err := local.Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer b.Close()

	if b.Location() == "" {
		t.Error("Location returned an empty string")
	}
}

func readerOf(s string) *strings.Reader { return strings.NewReader(s) }
