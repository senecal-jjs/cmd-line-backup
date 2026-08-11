// Package backend defines the storage interface that gobackup repositories are
// built on, together with the object naming scheme shared by every
// implementation.
//
// Repository objects are immutable: once written, an object is never modified.
// Deleting one is only ever done by a future garbage collector, and even then
// only for objects nothing references. That property is what lets the higher
// layers treat storage as an append-only content-addressed store and avoid
// read-modify-write races between concurrent clients.
package backend

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path"
	"strings"
)

// Errors returned by Backend implementations.
var (
	// ErrNotFound reports that the requested object does not exist.
	ErrNotFound = errors.New("backend: object not found")
	// ErrInvalidRange reports that a ranged read fell outside the object.
	ErrInvalidRange = errors.New("backend: requested range is outside the object")
)

// FileType categorizes repository objects. Each type lives in its own namespace
// within the repository.
type FileType string

const (
	// TypeConfig is the singleton repository configuration object.
	TypeConfig FileType = "config"
	// TypeKey is a passphrase-wrapped copy of the master key.
	TypeKey FileType = "key"
	// TypePack is a container holding many encrypted chunks.
	TypePack FileType = "pack"
	// TypeIndex maps chunk IDs to their location within packs.
	TypeIndex FileType = "index"
	// TypeSnapshot is the metadata describing one backup run.
	TypeSnapshot FileType = "snapshot"
)

// Valid reports whether t is a known file type.
func (t FileType) Valid() bool {
	switch t {
	case TypeConfig, TypeKey, TypePack, TypeIndex, TypeSnapshot:
		return true
	}
	return false
}

// A Handle identifies a single object in a repository.
type Handle struct {
	Type FileType
	// Name is the object's identifier, normally a hex-encoded ID. It is empty
	// for TypeConfig, which is a singleton.
	Name string
}

// String renders the handle for use in error messages.
func (h Handle) String() string {
	if h.Name == "" {
		return string(h.Type)
	}
	return fmt.Sprintf("%s/%s", h.Type, h.Name)
}

// Validate reports whether the handle is well formed.
func (h Handle) Validate() error {
	if !h.Type.Valid() {
		return fmt.Errorf("backend: unknown file type %q", h.Type)
	}
	if h.Type == TypeConfig {
		if h.Name != "" {
			return errors.New("backend: the config object must not have a name")
		}
		return nil
	}
	if h.Name == "" {
		return fmt.Errorf("backend: a %s object requires a name", h.Type)
	}
	// Names become path components, so anything that could escape the
	// repository prefix or confuse a backend has to be rejected.
	if strings.ContainsAny(h.Name, "/\\") || h.Name == "." || h.Name == ".." {
		return fmt.Errorf("backend: invalid object name %q", h.Name)
	}
	return nil
}

// Path returns the object's location relative to the repository root. Every
// backend uses this same layout, so a repository can be copied verbatim between
// storage systems, for example between S3 and a local directory.
//
// Packs are sharded into 256 directories by the first byte of their name. A
// large repository can hold hundreds of thousands of packs, and the shard keeps
// any single directory listing (or S3 key prefix) to a manageable size.
func (h Handle) Path() string {
	switch h.Type {
	case TypeConfig:
		return "config"
	case TypePack:
		return path.Join("packs", shard(h.Name), h.Name)
	case TypeIndex:
		return path.Join("indexes", h.Name)
	case TypeSnapshot:
		return path.Join("snapshots", h.Name)
	case TypeKey:
		return path.Join("keys", h.Name)
	default:
		return path.Join(string(h.Type), h.Name)
	}
}

// Prefix returns the path prefix under which objects of the given type live.
func Prefix(t FileType) string {
	switch t {
	case TypeConfig:
		return "config"
	case TypePack:
		return "packs"
	case TypeIndex:
		return "indexes"
	case TypeSnapshot:
		return "snapshots"
	case TypeKey:
		return "keys"
	default:
		return string(t)
	}
}

func shard(name string) string {
	if len(name) < 2 {
		return "00"
	}
	return name[:2]
}

// FileInfo describes a stored object.
type FileInfo struct {
	Handle
	Size int64
}

// A Backend is a flat, immutable object store.
//
// Implementations must be safe for concurrent use by multiple goroutines.
type Backend interface {
	// Location returns a human-readable description of where the repository
	// lives, for use in messages.
	Location() string

	// Save stores an object. size is the exact number of bytes rd will yield.
	//
	// Saving an object that already exists is permitted and overwrites it.
	// Because object names are either random or derived from content, this only
	// happens when identical data is written twice, so it is harmless and keeps
	// interrupted runs safely retryable.
	Save(ctx context.Context, h Handle, rd io.Reader, size int64) error

	// Load returns a reader over part of an object. offset must be
	// non-negative; a length of zero means "through the end of the object".
	// The caller must close the returned reader.
	Load(ctx context.Context, h Handle, offset, length int64) (io.ReadCloser, error)

	// Stat returns metadata about an object.
	Stat(ctx context.Context, h Handle) (FileInfo, error)

	// List calls fn for every object of the given type. Iteration stops and the
	// error is returned if fn returns one.
	List(ctx context.Context, t FileType, fn func(FileInfo) error) error

	// Remove deletes an object. Removing an object that does not exist returns
	// ErrNotFound.
	Remove(ctx context.Context, h Handle) error

	// Close releases any resources held by the backend.
	Close() error
}

// LoadAll reads an entire object into memory.
func LoadAll(ctx context.Context, b Backend, h Handle) ([]byte, error) {
	rd, err := b.Load(ctx, h, 0, 0)
	if err != nil {
		return nil, err
	}
	defer rd.Close()

	data, err := io.ReadAll(rd)
	if err != nil {
		return nil, fmt.Errorf("backend: reading %s: %w", h, err)
	}
	return data, nil
}

// LoadRange reads a byte range of an object into memory.
func LoadRange(ctx context.Context, b Backend, h Handle, offset, length int64) ([]byte, error) {
	rd, err := b.Load(ctx, h, offset, length)
	if err != nil {
		return nil, err
	}
	defer rd.Close()

	buf := make([]byte, length)
	if _, err := io.ReadFull(rd, buf); err != nil {
		return nil, fmt.Errorf("backend: reading %d bytes at offset %d of %s: %w", length, offset, h, err)
	}
	return buf, nil
}
