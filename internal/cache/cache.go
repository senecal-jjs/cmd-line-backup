// Package cache stores repository objects on the local disk so that repeated
// backups do not re-download data that cannot have changed.
//
// The cache is strictly an optimization. Every entry is reproducible from the
// repository, deleting the cache directory only costs time, and a corrupted or
// unreadable entry is silently ignored rather than reported. Correctness always
// comes from the remote repository; nothing is ever read from the cache that has
// not been authenticated against the repository key when it is used.
//
// Only immutable objects are cached: index objects, snapshot objects, and tree
// blobs, each named by an ID that never refers to different contents. There is
// therefore no invalidation problem to solve.
package cache

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/senecal-jjs/cmd-line-backup/internal/crypto"
)

// Kind separates the namespaces within a cache directory.
type Kind string

const (
	// KindIndex holds sealed index objects. These are the biggest win: without
	// them every backup would re-download the entire index just to learn which
	// chunks already exist.
	KindIndex Kind = "index"
	// KindTree holds decoded directory metadata, which the incremental
	// fast path walks on every run.
	KindTree Kind = "tree"
	// KindSnapshot holds sealed snapshot objects. Every command lists the
	// snapshots, so caching them saves a request per snapshot per run.
	KindSnapshot Kind = "snapshot"
)

// Cache is a directory of cached repository objects. The zero value is a
// disabled cache whose operations are all no-ops, which keeps callers free of
// nil checks.
type Cache struct {
	dir string
}

// Disabled returns a cache that stores nothing.
func Disabled() *Cache { return &Cache{} }

// New opens the cache for a repository, creating the directory if needed.
//
// Each repository gets its own subdirectory keyed by repository ID, so several
// repositories can be used from one machine without their contents colliding.
func New(baseDir string, repoID crypto.ID) (*Cache, error) {
	if baseDir == "" {
		var err error
		baseDir, err = DefaultDir()
		if err != nil {
			return nil, err
		}
	}

	dir := filepath.Join(baseDir, repoID.String())
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("cache: creating %s: %w", dir, err)
	}
	return &Cache{dir: dir}, nil
}

// DefaultDir returns the platform-appropriate base cache directory.
func DefaultDir() (string, error) {
	base, err := os.UserCacheDir()
	if err != nil {
		return "", fmt.Errorf("cache: locating the user cache directory: %w", err)
	}
	return filepath.Join(base, "gobackup"), nil
}

// Dir returns the cache directory, or an empty string if the cache is disabled.
func (c *Cache) Dir() string { return c.dir }

// Enabled reports whether the cache stores anything.
func (c *Cache) Enabled() bool { return c.dir != "" }

func (c *Cache) path(kind Kind, id crypto.ID) string {
	return filepath.Join(c.dir, string(kind), id.String())
}

// Get returns a cached object. A miss, an unreadable file, or a disabled cache
// are all reported the same way, because in every case the caller simply falls
// back to the repository.
func (c *Cache) Get(kind Kind, id crypto.ID) ([]byte, bool) {
	if !c.Enabled() {
		return nil, false
	}
	data, err := os.ReadFile(c.path(kind, id))
	if err != nil {
		return nil, false
	}
	return data, true
}

// Put stores an object, writing through a temporary file so a concurrent reader
// never sees a partial entry.
//
// Failures are returned for the caller to log, but they are never fatal: a
// backup that cannot write its cache is slower, not wrong.
func (c *Cache) Put(kind Kind, id crypto.ID, data []byte) error {
	if !c.Enabled() {
		return nil
	}

	dir := filepath.Join(c.dir, string(kind))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("cache: creating %s: %w", dir, err)
	}

	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return fmt.Errorf("cache: creating a temporary file: %w", err)
	}
	tmpName := tmp.Name()
	defer func() {
		tmp.Close()
		os.Remove(tmpName)
	}()

	if _, err := tmp.Write(data); err != nil {
		return fmt.Errorf("cache: writing %s: %w", id.Short(), err)
	}
	if err := tmp.Chmod(0o600); err != nil {
		return fmt.Errorf("cache: setting permissions: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("cache: closing a temporary file: %w", err)
	}
	if err := os.Rename(tmpName, c.path(kind, id)); err != nil {
		return fmt.Errorf("cache: publishing %s: %w", id.Short(), err)
	}
	return nil
}

// Clear removes every cached object for this repository.
func (c *Cache) Clear() error {
	if !c.Enabled() {
		return nil
	}
	if err := os.RemoveAll(c.dir); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("cache: clearing %s: %w", c.dir, err)
	}
	return nil
}
