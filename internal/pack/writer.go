package pack

import (
	"bytes"
	"crypto/rand"
	"errors"
	"fmt"
	"math"

	"github.com/senecal-jjs/cmd-line-backup/internal/crypto"
)

// A Writer assembles blobs into a single pack object in memory.
//
// Packs are limited to tens of megabytes, so buffering the whole object is both
// simple and cheap, and it lets the header be written after the blobs without
// seeking. A Writer is not safe for concurrent use; the archiver gives each
// upload goroutine its own.
type Writer struct {
	key         *crypto.Key
	compression Compression

	id      crypto.ID
	buf     bytes.Buffer
	entries []Entry
	seen    map[crypto.ID]bool
	done    bool
}

// NewWriter starts a new pack. Each pack gets a random ID rather than a
// content-derived one: the ID has to be known before the header is sealed, and
// two packs never hold the same set of chunks in practice, so there is nothing
// to gain from content addressing at this level.
func NewWriter(key *crypto.Key, compression Compression) (*Writer, error) {
	if key == nil {
		return nil, errors.New("pack: a key is required")
	}
	if !compression.Valid() {
		return nil, fmt.Errorf("pack: unknown compression %d", compression)
	}

	var id crypto.ID
	if _, err := rand.Read(id[:]); err != nil {
		return nil, fmt.Errorf("pack: generating pack ID: %w", err)
	}
	return &Writer{
		key:         key,
		compression: compression,
		id:          id,
		seen:        make(map[crypto.ID]bool),
	}, nil
}

// ID returns the pack's identifier.
func (w *Writer) ID() crypto.ID { return w.id }

// Size returns the number of bytes written so far, excluding the header and
// footer that Finish will append. The archiver uses it to decide when a pack is
// full.
func (w *Writer) Size() int { return w.buf.Len() }

// Count returns the number of blobs added so far.
func (w *Writer) Count() int { return len(w.entries) }

// Add compresses, seals, and appends a blob. It reports whether the blob was
// added; a blob whose ID is already present in this pack is skipped, since
// storing it twice would waste space for no benefit.
//
// The caller is responsible for checking the repository index first: Add only
// deduplicates within this one pack.
func (w *Writer) Add(blobType BlobType, id crypto.ID, plaintext []byte) (bool, error) {
	if w.done {
		return false, errors.New("pack: Add called after Finish")
	}
	if !blobType.Valid() {
		return false, fmt.Errorf("pack: unknown blob type %d", blobType)
	}
	if w.seen[id] {
		return false, nil
	}
	if len(plaintext) > math.MaxUint32 {
		return false, fmt.Errorf("pack: blob of %d bytes is too large", len(plaintext))
	}

	stored, used, err := compress(plaintext, w.compression)
	if err != nil {
		return false, err
	}

	sealed, err := w.key.SealData(stored, blobAAD(blobType, id))
	if err != nil {
		return false, fmt.Errorf("pack: sealing blob %s: %w", id.Short(), err)
	}

	offset := w.buf.Len()
	if offset+len(sealed) > math.MaxUint32 {
		return false, errors.New("pack: pack has grown beyond the addressable size")
	}
	if _, err := w.buf.Write(sealed); err != nil {
		return false, fmt.Errorf("pack: buffering blob: %w", err)
	}

	w.entries = append(w.entries, Entry{
		Type:        blobType,
		ID:          id,
		Offset:      uint32(offset),
		Length:      uint32(len(sealed)),
		PlainLength: uint32(len(plaintext)),
		Compression: used,
	})
	w.seen[id] = true
	return true, nil
}

// Has reports whether the pack already contains a blob with the given ID.
func (w *Writer) Has(id crypto.ID) bool { return w.seen[id] }

// A Pack is a finished pack object, ready to be uploaded.
type Pack struct {
	ID      crypto.ID
	Data    []byte
	Entries []Entry
}

// Finish seals the header, appends it with the footer, and returns the complete
// pack. The Writer must not be used afterwards.
func (w *Writer) Finish() (*Pack, error) {
	if w.done {
		return nil, errors.New("pack: Finish called twice")
	}
	if len(w.entries) == 0 {
		return nil, errors.New("pack: refusing to finish an empty pack")
	}
	w.done = true

	// The header is metadata, so it is sealed with the metadata subkey rather
	// than the one used for bulk data.
	sealedHeader, err := w.key.SealMeta(encodeHeader(w.entries), headerAAD(w.id))
	if err != nil {
		return nil, fmt.Errorf("pack: sealing header: %w", err)
	}
	if len(sealedHeader) > MaxHeaderSize {
		return nil, fmt.Errorf("pack: header of %d bytes exceeds the %d byte limit",
			len(sealedHeader), MaxHeaderSize)
	}

	if _, err := w.buf.Write(sealedHeader); err != nil {
		return nil, fmt.Errorf("pack: buffering header: %w", err)
	}
	if _, err := w.buf.Write(encodeFooter(uint32(len(sealedHeader)))); err != nil {
		return nil, fmt.Errorf("pack: buffering footer: %w", err)
	}

	return &Pack{ID: w.id, Data: w.buf.Bytes(), Entries: w.entries}, nil
}
