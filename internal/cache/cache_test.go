package cache

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/senecal-jjs/cmd-line-backup/internal/crypto"
)

func TestPutAndGet(t *testing.T) {
	c, err := New(t.TempDir(), crypto.ID{1, 2, 3})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	id := crypto.ID{0xAB}
	data := []byte("cached object")
	if err := c.Put(KindIndex, id, data); err != nil {
		t.Fatalf("Put: %v", err)
	}

	got, ok := c.Get(KindIndex, id)
	if !ok {
		t.Fatal("Get reported a miss for a stored object")
	}
	if string(got) != string(data) {
		t.Errorf("Get returned %q, want %q", got, data)
	}
}

func TestKindsAreSeparate(t *testing.T) {
	c, err := New(t.TempDir(), crypto.ID{1})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	id := crypto.ID{0xAB}
	if err := c.Put(KindIndex, id, []byte("index")); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if err := c.Put(KindTree, id, []byte("tree")); err != nil {
		t.Fatalf("Put: %v", err)
	}

	if got, _ := c.Get(KindIndex, id); string(got) != "index" {
		t.Errorf("index entry = %q, want %q", got, "index")
	}
	if got, _ := c.Get(KindTree, id); string(got) != "tree" {
		t.Errorf("tree entry = %q, want %q", got, "tree")
	}
}

func TestGetMiss(t *testing.T) {
	c, err := New(t.TempDir(), crypto.ID{1})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, ok := c.Get(KindIndex, crypto.ID{0xFF}); ok {
		t.Error("Get reported a hit for an absent object")
	}
}

// TestRepositoriesAreIsolated confirms two repositories used from one machine
// cannot see each other's cached objects.
func TestRepositoriesAreIsolated(t *testing.T) {
	base := t.TempDir()
	first, err := New(base, crypto.ID{1})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	second, err := New(base, crypto.ID{2})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	id := crypto.ID{0xAB}
	if err := first.Put(KindIndex, id, []byte("first")); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if _, ok := second.Get(KindIndex, id); ok {
		t.Error("one repository's cache is visible to another")
	}
}

// TestDisabledCacheIsANoop keeps callers free of nil checks: a disabled cache
// must accept writes and always report misses.
func TestDisabledCacheIsANoop(t *testing.T) {
	c := Disabled()

	if c.Enabled() {
		t.Error("Disabled cache reports itself as enabled")
	}
	if err := c.Put(KindIndex, crypto.ID{1}, []byte("data")); err != nil {
		t.Errorf("Put on a disabled cache: %v", err)
	}
	if _, ok := c.Get(KindIndex, crypto.ID{1}); ok {
		t.Error("a disabled cache returned a hit")
	}
	if err := c.Clear(); err != nil {
		t.Errorf("Clear on a disabled cache: %v", err)
	}
}

func TestClear(t *testing.T) {
	c, err := New(t.TempDir(), crypto.ID{1})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	id := crypto.ID{0xAB}
	if err := c.Put(KindIndex, id, []byte("data")); err != nil {
		t.Fatalf("Put: %v", err)
	}

	if err := c.Clear(); err != nil {
		t.Fatalf("Clear: %v", err)
	}
	if _, ok := c.Get(KindIndex, id); ok {
		t.Error("an entry survived Clear")
	}
	// Clearing twice must be safe.
	if err := c.Clear(); err != nil {
		t.Errorf("second Clear: %v", err)
	}
}

func TestPermissionsAreOwnerOnly(t *testing.T) {
	base := t.TempDir()
	repoID := crypto.ID{1}
	c, err := New(base, repoID)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	id := crypto.ID{0xAB}
	if err := c.Put(KindIndex, id, []byte("data")); err != nil {
		t.Fatalf("Put: %v", err)
	}

	// Cached objects are encrypted, but they still reveal repository structure,
	// so they should not be world-readable.
	info, err := os.Stat(filepath.Join(base, repoID.String(), string(KindIndex), id.String()))
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("cached object permissions are %v, want 0600", perm)
	}
}

func TestOverwriteExistingEntry(t *testing.T) {
	c, err := New(t.TempDir(), crypto.ID{1})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	id := crypto.ID{0xAB}

	if err := c.Put(KindIndex, id, []byte("first")); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if err := c.Put(KindIndex, id, []byte("second")); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if got, _ := c.Get(KindIndex, id); string(got) != "second" {
		t.Errorf("entry = %q, want %q", got, "second")
	}
}

func TestDefaultDir(t *testing.T) {
	dir, err := DefaultDir()
	if err != nil {
		t.Fatalf("DefaultDir: %v", err)
	}
	if dir == "" {
		t.Error("DefaultDir returned an empty path")
	}
}
