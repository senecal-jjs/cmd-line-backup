// Package local implements a filesystem-backed repository store.
//
// It is the reference implementation of backend.Backend: it backs the test
// suite for every other backend, and it lets a repository be kept on an
// external disk or NFS mount instead of S3.
package local

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/senecal-jjs/cmd-line-backup/internal/backend"
)

// Backend stores repository objects as files under a root directory.
type Backend struct {
	root string
}

// Open returns a Backend rooted at dir, creating the directory if needed.
func Open(dir string) (*Backend, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("local: resolving %s: %w", dir, err)
	}
	if err := os.MkdirAll(abs, 0o700); err != nil {
		return nil, fmt.Errorf("local: creating %s: %w", abs, err)
	}
	return &Backend{root: abs}, nil
}

// Location implements backend.Backend.
func (b *Backend) Location() string { return b.root }

func (b *Backend) filename(h backend.Handle) string {
	return filepath.Join(b.root, filepath.FromSlash(h.Path()))
}

// Save implements backend.Backend, writing through a temporary file so that a
// crash mid-write can never leave a partial object visible under its final name.
func (b *Backend) Save(ctx context.Context, h backend.Handle, rd io.Reader, size int64) error {
	if err := h.Validate(); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	final := b.filename(h)
	dir := filepath.Dir(final)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("local: creating directory for %s: %w", h, err)
	}

	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return fmt.Errorf("local: creating temporary file for %s: %w", h, err)
	}
	tmpName := tmp.Name()
	defer func() {
		tmp.Close()
		os.Remove(tmpName)
	}()

	written, err := io.Copy(tmp, rd)
	if err != nil {
		return fmt.Errorf("local: writing %s: %w", h, err)
	}
	if written != size {
		return fmt.Errorf("local: %s: wrote %d bytes but %d were declared", h, written, size)
	}
	// Backups are worthless if they are not durable, so the data reaches disk
	// before the rename publishes it.
	if err := tmp.Sync(); err != nil {
		return fmt.Errorf("local: syncing %s: %w", h, err)
	}
	if err := tmp.Chmod(0o600); err != nil {
		return fmt.Errorf("local: setting permissions on %s: %w", h, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("local: closing %s: %w", h, err)
	}
	if err := os.Rename(tmpName, final); err != nil {
		return fmt.Errorf("local: publishing %s: %w", h, err)
	}
	// The rename itself lives in the directory, so it is only durable once the
	// directory is synced too.
	if err := syncDir(dir); err != nil {
		return fmt.Errorf("local: syncing directory for %s: %w", h, err)
	}
	return nil
}

// syncDir flushes a directory's entries to disk. Some filesystems cannot sync a
// directory and report EINVAL; there is nothing more to be done on those.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	if err := d.Sync(); err != nil && !errors.Is(err, syscall.EINVAL) && !errors.Is(err, syscall.ENOTSUP) {
		return err
	}
	return nil
}

// Load implements backend.Backend.
func (b *Backend) Load(ctx context.Context, h backend.Handle, offset, length int64) (io.ReadCloser, error) {
	if err := h.Validate(); err != nil {
		return nil, err
	}
	if offset < 0 || length < 0 {
		return nil, fmt.Errorf("%w: offset %d, length %d", backend.ErrInvalidRange, offset, length)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	f, err := os.Open(b.filename(h))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("%w: %s", backend.ErrNotFound, h)
		}
		return nil, fmt.Errorf("local: opening %s: %w", h, err)
	}

	info, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, fmt.Errorf("local: inspecting %s: %w", h, err)
	}
	if offset > info.Size() || offset+length > info.Size() {
		f.Close()
		return nil, fmt.Errorf("%w: %s has %d bytes, requested %d at offset %d",
			backend.ErrInvalidRange, h, info.Size(), length, offset)
	}

	if offset > 0 {
		if _, err := f.Seek(offset, io.SeekStart); err != nil {
			f.Close()
			return nil, fmt.Errorf("local: seeking in %s: %w", h, err)
		}
	}
	if length == 0 {
		return f, nil
	}
	return readCloser{Reader: io.LimitReader(f, length), Closer: f}, nil
}

type readCloser struct {
	io.Reader
	io.Closer
}

// Stat implements backend.Backend.
func (b *Backend) Stat(ctx context.Context, h backend.Handle) (backend.FileInfo, error) {
	if err := h.Validate(); err != nil {
		return backend.FileInfo{}, err
	}
	if err := ctx.Err(); err != nil {
		return backend.FileInfo{}, err
	}

	info, err := os.Stat(b.filename(h))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return backend.FileInfo{}, fmt.Errorf("%w: %s", backend.ErrNotFound, h)
		}
		return backend.FileInfo{}, fmt.Errorf("local: inspecting %s: %w", h, err)
	}
	return backend.FileInfo{Handle: h, Size: info.Size()}, nil
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

	root := filepath.Join(b.root, backend.Prefix(t))
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) && path == root {
				return fs.SkipAll
			}
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		// Concurrent writers leave partial temporary files behind; they are not
		// repository objects.
		if strings.HasPrefix(d.Name(), ".tmp-") {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil // removed while we were walking
			}
			return err
		}
		return fn(backend.FileInfo{
			Handle: backend.Handle{Type: t, Name: d.Name()},
			Size:   info.Size(),
		})
	})
	if err != nil && !errors.Is(err, fs.SkipAll) {
		return fmt.Errorf("local: listing %s objects: %w", t, err)
	}
	return nil
}

// Remove implements backend.Backend.
func (b *Backend) Remove(ctx context.Context, h backend.Handle) error {
	if err := h.Validate(); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	if err := os.Remove(b.filename(h)); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("%w: %s", backend.ErrNotFound, h)
		}
		return fmt.Errorf("local: removing %s: %w", h, err)
	}
	return nil
}

// Close implements backend.Backend.
func (b *Backend) Close() error { return nil }

// Ensure Backend satisfies the interface.
var _ backend.Backend = (*Backend)(nil)
