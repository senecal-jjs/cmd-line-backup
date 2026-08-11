package repo

import (
	"bytes"
	"context"
	"fmt"

	"github.com/senecal-jjs/cmd-line-backup/internal/backend"
	"github.com/senecal-jjs/cmd-line-backup/internal/cache"
	"github.com/senecal-jjs/cmd-line-backup/internal/crypto"
	"github.com/senecal-jjs/cmd-line-backup/internal/index"
	"github.com/senecal-jjs/cmd-line-backup/internal/pack"
	"github.com/senecal-jjs/cmd-line-backup/internal/snapshot"
)

// SaveBlob stores a blob, deduplicating against everything already in the
// repository. It returns the blob's ID and whether it was newly stored.
//
// New blobs accumulate in an open pack, which is uploaded once it reaches the
// target size. Data is therefore written to the repository continuously during a
// backup rather than all at once at the end.
func (r *Repository) SaveBlob(ctx context.Context, blobType pack.BlobType, plaintext []byte) (crypto.ID, bool, error) {
	id := r.key.ChunkID(plaintext)
	if r.idx.Has(id) {
		return id, false, nil
	}

	full, added, err := r.addToPack(blobType, id, plaintext)
	if err != nil {
		return id, false, err
	}

	// The finished pack is uploaded outside the lock so that other goroutines
	// can keep filling the next one while this one is in flight.
	if full != nil {
		if err := r.uploadPack(ctx, full); err != nil {
			return id, added, err
		}
	}
	return id, added, nil
}

// addToPack appends a blob to the open pack, returning a finished pack when the
// blob filled it.
func (r *Repository) addToPack(blobType pack.BlobType, id crypto.ID, plaintext []byte) (*pack.Pack, bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	// Re-check under the lock: another goroutine may have stored this blob
	// since the caller's optimistic check.
	if r.idx.Has(id) {
		return nil, false, nil
	}
	if r.writer != nil && r.writer.Has(id) {
		return nil, false, nil
	}

	if r.writer == nil {
		w, err := pack.NewWriter(r.key, r.compression)
		if err != nil {
			return nil, false, err
		}
		r.writer = w
	}

	added, err := r.writer.Add(blobType, id, plaintext)
	if err != nil {
		return nil, false, err
	}

	if r.writer.Size() < pack.TargetSize {
		return nil, added, nil
	}

	finished, err := r.writer.Finish()
	if err != nil {
		return nil, added, err
	}
	r.writer = nil
	return finished, added, nil
}

// uploadPack stores a finished pack and records its contents in the index.
//
// A failure here is not recoverable within the run: the pack holds blobs that
// files already added to the snapshot tree refer to, so the run is marked failed
// and no snapshot can be written afterwards.
func (r *Repository) uploadPack(ctx context.Context, p *pack.Pack) error {
	h := backend.Handle{Type: backend.TypePack, Name: p.ID.String()}
	if err := r.be.Save(ctx, h, bytes.NewReader(p.Data), int64(len(p.Data))); err != nil {
		err = fmt.Errorf("repo: uploading pack %s: %w", p.ID.Short(), err)
		r.fail(err)
		return err
	}

	// Only after the pack is durable may its blobs be treated as stored.
	r.idx.AddPack(p.ID, p.Entries)

	r.mu.Lock()
	r.newPacks = append(r.newPacks, index.PackEntries{PackID: p.ID, Entries: p.Entries})
	r.mu.Unlock()
	return nil
}

// fail records the first write failure of the session.
func (r *Repository) fail(err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.failed == nil {
		r.failed = err
	}
}

// Failed returns the sticky write failure, if any.
func (r *Repository) Failed() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.failed
}

// LoadBlob fetches a blob by ID.
func (r *Repository) LoadBlob(ctx context.Context, id crypto.ID) ([]byte, error) {
	loc, ok := r.idx.Lookup(id)
	if !ok {
		return nil, fmt.Errorf("repo: blob %s is not in the index", id.Short())
	}
	return pack.LoadBlob(ctx, r.be, r.key, loc.PackID, loc.Entry)
}

// SaveTree serializes a directory and stores it as a tree blob.
func (r *Repository) SaveTree(ctx context.Context, tree *snapshot.Tree) (crypto.ID, error) {
	plaintext, err := snapshot.EncodeTree(tree)
	if err != nil {
		return crypto.ID{}, err
	}

	id, _, err := r.SaveBlob(ctx, pack.BlobTree, plaintext)
	if err != nil {
		return crypto.ID{}, err
	}

	// Trees are walked again by the next backup's fast path, so caching them
	// locally avoids a ranged read per directory.
	_ = r.cache.Put(cache.KindTree, id, plaintext)
	return id, nil
}

// LoadTree fetches and parses a tree blob, preferring the local cache.
func (r *Repository) LoadTree(ctx context.Context, id crypto.ID) (*snapshot.Tree, error) {
	if plaintext, ok := r.cache.Get(cache.KindTree, id); ok {
		// The cache is not trusted blindly: the blob must still match the keyed
		// ID that named it, which is the same check applied to repository data.
		if r.key.VerifyChunkID(id, plaintext) {
			return snapshot.DecodeTree(plaintext)
		}
	}

	plaintext, err := r.LoadBlob(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("repo: loading tree %s: %w", id.Short(), err)
	}
	_ = r.cache.Put(cache.KindTree, id, plaintext)

	return snapshot.DecodeTree(plaintext)
}

// Flush finishes the open pack and writes an index object covering every pack
// created during this run.
//
// It must be called before the snapshot is saved. The ordering -- packs, then
// index, then snapshot -- is what guarantees that a snapshot visible in the
// repository never references data that is not there.
func (r *Repository) Flush(ctx context.Context) error {
	if err := r.Failed(); err != nil {
		return fmt.Errorf("repo: refusing to finish this run because an earlier write failed: %w", err)
	}

	r.mu.Lock()
	w := r.writer
	r.writer = nil
	r.mu.Unlock()

	if w != nil && w.Count() > 0 {
		finished, err := w.Finish()
		if err != nil {
			r.fail(err)
			return err
		}
		if err := r.uploadPack(ctx, finished); err != nil {
			return err
		}
	}

	r.mu.Lock()
	packs := r.newPacks
	r.newPacks = nil
	r.mu.Unlock()

	if len(packs) == 0 {
		// Nothing new was stored, which is the normal outcome of backing up
		// unchanged data.
		return nil
	}

	if _, err := index.Save(ctx, r.be, r.key, packs, indexCache{r.cache}); err != nil {
		r.fail(err)
		return err
	}
	return nil
}

// SaveSnapshot writes a snapshot object. Flush must have completed first.
func (r *Repository) SaveSnapshot(ctx context.Context, s *snapshot.Snapshot) (crypto.ID, error) {
	// The last line of defence against publishing a snapshot that references
	// data which never made it into the repository.
	if err := r.Failed(); err != nil {
		return crypto.ID{}, fmt.Errorf("repo: refusing to write a snapshot because an earlier write failed: %w", err)
	}
	return snapshot.Save(ctx, r.be, r.key, s)
}

// LoadSnapshot reads a snapshot by ID.
func (r *Repository) LoadSnapshot(ctx context.Context, id crypto.ID) (*snapshot.Snapshot, error) {
	return snapshot.Load(ctx, r.be, r.key, id)
}

// ListSnapshots returns every snapshot, newest first.
func (r *Repository) ListSnapshots(ctx context.Context) ([]*snapshot.Snapshot, error) {
	return snapshot.List(ctx, r.be, r.key)
}

// FindSnapshot resolves a snapshot reference: a full ID, an unambiguous prefix,
// or "latest".
func (r *Repository) FindSnapshot(ctx context.Context, ref string) (*snapshot.Snapshot, error) {
	return snapshot.Find(ctx, r.be, r.key, ref)
}
