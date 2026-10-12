package snapshot

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"sort"
	"sync"

	"github.com/senecal-jjs/cmd-line-backup/internal/backend"
	"github.com/senecal-jjs/cmd-line-backup/internal/crypto"
)

// snapshotAAD binds a sealed snapshot to its own ID.
func snapshotAAD(id crypto.ID) []byte {
	return append([]byte("gobackup v1 snapshot"), id[:]...)
}

// Save writes a snapshot object and returns its ID.
//
// This must be the last write of a backup run. Every pack and index object the
// snapshot depends on has to be durable first, so that a snapshot appearing in
// the repository always refers to data that is actually there. An interrupted
// run leaves unreferenced packs behind, which waste space but never produce a
// snapshot that cannot be restored.
func Save(ctx context.Context, be backend.Backend, key *crypto.Key, s *Snapshot) (crypto.ID, error) {
	if err := s.Validate(); err != nil {
		return crypto.ID{}, err
	}

	var id crypto.ID
	if _, err := rand.Read(id[:]); err != nil {
		return crypto.ID{}, fmt.Errorf("snapshot: generating snapshot ID: %w", err)
	}

	plaintext, err := encode(s)
	if err != nil {
		return crypto.ID{}, err
	}
	sealed, err := key.SealMeta(plaintext, snapshotAAD(id))
	if err != nil {
		return crypto.ID{}, fmt.Errorf("snapshot: sealing snapshot: %w", err)
	}

	h := backend.Handle{Type: backend.TypeSnapshot, Name: id.String()}
	if err := be.Save(ctx, h, bytes.NewReader(sealed), int64(len(sealed))); err != nil {
		return crypto.ID{}, err
	}
	s.ID = id
	return id, nil
}

// An ObjectCache stores sealed snapshot objects locally between runs. Snapshots
// are immutable, so a cached copy never goes stale; it is still authenticated
// on every use. A nil cache means every snapshot is fetched from the repository.
type ObjectCache interface {
	Get(id crypto.ID) ([]byte, bool)
	Put(id crypto.ID, data []byte) error
}

// loadConcurrency bounds how many snapshot objects are fetched at once when
// listing a repository.
const loadConcurrency = 8

// Load reads a single snapshot, consulting the cache first.
func Load(ctx context.Context, be backend.Backend, key *crypto.Key, id crypto.ID, c ObjectCache) (*Snapshot, error) {
	if c != nil {
		if sealed, ok := c.Get(id); ok {
			if s, err := openAndDecode(key, id, sealed); err == nil {
				return s, nil
			}
			// A cached object that will not authenticate is corrupt; fall
			// through and refetch it, overwriting the bad entry below.
		}
	}

	sealed, err := backend.LoadAll(ctx, be, backend.Handle{Type: backend.TypeSnapshot, Name: id.String()})
	if err != nil {
		return nil, err
	}
	s, err := openAndDecode(key, id, sealed)
	if err != nil {
		return nil, err
	}
	if c != nil {
		// Failing to cache only costs speed on the next run.
		_ = c.Put(id, sealed)
	}
	return s, nil
}

func openAndDecode(key *crypto.Key, id crypto.ID, sealed []byte) (*Snapshot, error) {
	plaintext, err := key.OpenMeta(sealed, snapshotAAD(id))
	if err != nil {
		return nil, fmt.Errorf("snapshot %s: %w", id.Short(), err)
	}
	return decode(plaintext, id)
}

// List returns every snapshot in the repository, newest first.
//
// Every command needs the snapshot list, so uncached snapshots are fetched
// concurrently rather than one round trip at a time.
func List(ctx context.Context, be backend.Backend, key *crypto.Key, c ObjectCache) ([]*Snapshot, error) {
	var ids []crypto.ID
	err := be.List(ctx, backend.TypeSnapshot, func(fi backend.FileInfo) error {
		id, err := crypto.ParseID(fi.Name)
		if err != nil {
			return nil // not an object we wrote
		}
		ids = append(ids, id)
		return nil
	})
	if err != nil {
		return nil, err
	}

	snapshots := make([]*Snapshot, len(ids))
	var (
		wg       sync.WaitGroup
		sem      = make(chan struct{}, loadConcurrency)
		mu       sync.Mutex
		firstErr error
	)
	for i, id := range ids {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int, id crypto.ID) {
			defer wg.Done()
			defer func() { <-sem }()
			s, err := Load(ctx, be, key, id, c)
			if err != nil {
				mu.Lock()
				if firstErr == nil {
					firstErr = err
				}
				mu.Unlock()
				return
			}
			snapshots[i] = s
		}(i, id)
	}
	wg.Wait()
	if firstErr != nil {
		return nil, firstErr
	}

	sort.Slice(snapshots, func(i, j int) bool {
		if snapshots[i].Time.Equal(snapshots[j].Time) {
			return snapshots[i].ID.String() < snapshots[j].ID.String()
		}
		return snapshots[i].Time.After(snapshots[j].Time)
	})
	return snapshots, nil
}

// ErrAmbiguousID reports that a short snapshot ID matched more than one snapshot.
var ErrAmbiguousID = errors.New("snapshot: ambiguous snapshot ID")

// ErrNotFound reports that no snapshot matched.
var ErrNotFound = errors.New("snapshot: no matching snapshot")

// Find resolves a user-supplied snapshot reference. It accepts a full
// hexadecimal ID, an unambiguous prefix of one, or "latest" for the most recent
// snapshot.
func Find(ctx context.Context, be backend.Backend, key *crypto.Key, ref string, c ObjectCache) (*Snapshot, error) {
	snapshots, err := List(ctx, be, key, c)
	if err != nil {
		return nil, err
	}
	if len(snapshots) == 0 {
		return nil, fmt.Errorf("%w: the repository has no snapshots", ErrNotFound)
	}

	if ref == "" || ref == "latest" {
		return snapshots[0], nil
	}

	var matches []*Snapshot
	for _, s := range snapshots {
		if hasPrefixFold(s.ID.String(), ref) {
			matches = append(matches, s)
		}
	}

	switch len(matches) {
	case 0:
		return nil, fmt.Errorf("%w for %q", ErrNotFound, ref)
	case 1:
		return matches[0], nil
	default:
		return nil, fmt.Errorf("%w: %q matches %d snapshots; use more characters",
			ErrAmbiguousID, ref, len(matches))
	}
}

// FindParent returns the most recent snapshot that covers the same paths on the
// same host, which is the natural baseline for an incremental run.
func FindParent(snapshots []*Snapshot, hostname string, paths []string) *Snapshot {
	want := append([]string(nil), paths...)
	sort.Strings(want)

	for _, s := range snapshots { // already newest first
		if s.Hostname != hostname {
			continue
		}
		got := append([]string(nil), s.Paths...)
		sort.Strings(got)
		if equalStrings(got, want) {
			return s
		}
	}
	return nil
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func hasPrefixFold(s, prefix string) bool {
	if len(prefix) > len(s) {
		return false
	}
	for i := 0; i < len(prefix); i++ {
		if lower(s[i]) != lower(prefix[i]) {
			return false
		}
	}
	return true
}

func lower(b byte) byte {
	if b >= 'A' && b <= 'Z' {
		return b + ('a' - 'A')
	}
	return b
}
