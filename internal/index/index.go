// Package index maps chunk IDs to the pack and byte range that hold them.
//
// The index is what makes deduplication and restore possible. A backup consults
// it to decide whether a chunk already exists somewhere in the repository, and a
// restore consults it to turn a list of chunk IDs into a set of ranged reads.
//
// Index objects are immutable and additive: each backup run writes one new
// object describing only the packs it created, and readers merge every object
// they find. Nothing is ever rewritten, so concurrent or interrupted runs cannot
// corrupt what is already stored.
package index

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"sync"

	"github.com/senecal-jjs/cmd-line-backup/internal/backend"
	"github.com/senecal-jjs/cmd-line-backup/internal/crypto"
	"github.com/senecal-jjs/cmd-line-backup/internal/pack"
)

// Format constants for the serialized index object.
const (
	formatVersion = 1
	entrySize     = 46 // must match the pack header entry encoding
)

// ErrBadFormat reports a malformed index object.
var ErrBadFormat = errors.New("index: malformed index object")

// A Location says where a blob lives: which pack, and where inside it.
type Location struct {
	PackID crypto.ID
	Entry  pack.Entry
}

// PackEntries is the set of blobs stored in one pack.
type PackEntries struct {
	PackID  crypto.ID
	Entries []pack.Entry
}

// An Index is an in-memory, merged view of every index object in a repository.
//
// It is safe for concurrent use: the archiver queries and extends it from
// several goroutines at once.
type Index struct {
	mu       sync.RWMutex
	blobs    map[crypto.ID]Location
	packs    map[crypto.ID]int // pack ID -> number of blobs
	sourceID []crypto.ID       // index objects this view was built from
}

// New returns an empty index.
func New() *Index {
	return &Index{
		blobs: make(map[crypto.ID]Location),
		packs: make(map[crypto.ID]int),
	}
}

// AddPack records every blob in a pack.
//
// If a blob is already known, the existing location is kept. Duplicates arise
// when two backups run concurrently and both store the same new chunk; either
// copy is valid, so the first one wins and the other simply goes unreferenced.
func (i *Index) AddPack(packID crypto.ID, entries []pack.Entry) {
	i.mu.Lock()
	defer i.mu.Unlock()

	i.packs[packID] += len(entries)
	for _, e := range entries {
		if _, exists := i.blobs[e.ID]; exists {
			continue
		}
		i.blobs[e.ID] = Location{PackID: packID, Entry: e}
	}
}

// Lookup returns the location of a blob.
func (i *Index) Lookup(id crypto.ID) (Location, bool) {
	i.mu.RLock()
	defer i.mu.RUnlock()

	loc, ok := i.blobs[id]
	return loc, ok
}

// Has reports whether a blob is already stored. This is the hot path of an
// incremental backup: it is consulted once per chunk.
func (i *Index) Has(id crypto.ID) bool {
	i.mu.RLock()
	defer i.mu.RUnlock()

	_, ok := i.blobs[id]
	return ok
}

// Len returns the number of distinct blobs known to the index.
func (i *Index) Len() int {
	i.mu.RLock()
	defer i.mu.RUnlock()

	return len(i.blobs)
}

// PackIDs returns the IDs of every pack the index references.
func (i *Index) PackIDs() []crypto.ID {
	i.mu.RLock()
	defer i.mu.RUnlock()

	out := make([]crypto.ID, 0, len(i.packs))
	for id := range i.packs {
		out = append(out, id)
	}
	return out
}

// SourceIDs returns the IDs of the index objects this view was loaded from.
func (i *Index) SourceIDs() []crypto.ID {
	i.mu.RLock()
	defer i.mu.RUnlock()

	return append([]crypto.ID(nil), i.sourceID...)
}

// Each calls fn for every known blob location.
func (i *Index) Each(fn func(crypto.ID, Location) error) error {
	i.mu.RLock()
	defer i.mu.RUnlock()

	for id, loc := range i.blobs {
		if err := fn(id, loc); err != nil {
			return err
		}
	}
	return nil
}

// encode serializes a set of packs into an index object plaintext.
func encode(packs []PackEntries) []byte {
	size := 8
	for _, p := range packs {
		size += crypto.IDSize + 4 + len(p.Entries)*entrySize
	}

	buf := make([]byte, size)
	binary.LittleEndian.PutUint32(buf[0:], formatVersion)
	binary.LittleEndian.PutUint32(buf[4:], uint32(len(packs)))

	off := 8
	for _, p := range packs {
		copy(buf[off:], p.PackID[:])
		off += crypto.IDSize
		binary.LittleEndian.PutUint32(buf[off:], uint32(len(p.Entries)))
		off += 4

		for _, e := range p.Entries {
			buf[off] = byte(e.Type)
			buf[off+1] = byte(e.Compression)
			copy(buf[off+2:], e.ID[:])
			binary.LittleEndian.PutUint32(buf[off+34:], e.Offset)
			binary.LittleEndian.PutUint32(buf[off+38:], e.Length)
			binary.LittleEndian.PutUint32(buf[off+42:], e.PlainLength)
			off += entrySize
		}
	}
	return buf
}

// decode parses an index object plaintext.
func decode(data []byte) ([]PackEntries, error) {
	if len(data) < 8 {
		return nil, fmt.Errorf("%w: object is only %d bytes", ErrBadFormat, len(data))
	}
	if v := binary.LittleEndian.Uint32(data[0:]); v != formatVersion {
		return nil, fmt.Errorf("index: unsupported index format version %d, this build understands version %d",
			v, formatVersion)
	}

	packCount := binary.LittleEndian.Uint32(data[4:])
	off := 8

	// The declared count is attacker-controlled, so it must be checked against
	// what the object could actually hold before it is used to size an
	// allocation. Every pack costs at least an ID and an entry count in the
	// encoding, which gives a hard upper bound derived from the data itself
	// rather than an arbitrary cap. Skipping this check is enough for a corrupt
	// or hostile index object to request hundreds of gigabytes and take the
	// process down with it.
	const minPackSize = crypto.IDSize + 4
	maxPacks := (len(data) - 8) / minPackSize
	if uint64(packCount) > uint64(maxPacks) {
		return nil, fmt.Errorf("%w: object declares %d packs, but it is only large enough for %d",
			ErrBadFormat, packCount, maxPacks)
	}
	packs := make([]PackEntries, 0, packCount)

	for p := uint32(0); p < packCount; p++ {
		if off+crypto.IDSize+4 > len(data) {
			return nil, fmt.Errorf("%w: truncated while reading pack %d of %d", ErrBadFormat, p, packCount)
		}
		var packID crypto.ID
		copy(packID[:], data[off:])
		off += crypto.IDSize

		entryCount := binary.LittleEndian.Uint32(data[off:])
		off += 4

		// Same reasoning, and computed in 64-bit so the multiplication cannot
		// overflow into a value that looks acceptable on a 32-bit platform.
		if uint64(entryCount)*entrySize > uint64(len(data)-off) {
			return nil, fmt.Errorf("%w: pack %s claims %d entries, which do not fit in the object",
				ErrBadFormat, packID.Short(), entryCount)
		}

		entries := make([]pack.Entry, entryCount)
		for i := range entries {
			e := pack.Entry{
				Type:        pack.BlobType(data[off]),
				Compression: pack.Compression(data[off+1]),
				Offset:      binary.LittleEndian.Uint32(data[off+34:]),
				Length:      binary.LittleEndian.Uint32(data[off+38:]),
				PlainLength: binary.LittleEndian.Uint32(data[off+42:]),
			}
			copy(e.ID[:], data[off+2:off+34])
			if !e.Type.Valid() {
				return nil, fmt.Errorf("%w: unknown blob type %d", ErrBadFormat, e.Type)
			}
			if !e.Compression.Valid() {
				return nil, fmt.Errorf("%w: unknown compression %d", ErrBadFormat, e.Compression)
			}
			entries[i] = e
			off += entrySize
		}
		packs = append(packs, PackEntries{PackID: packID, Entries: entries})
	}

	if off != len(data) {
		return nil, fmt.Errorf("%w: %d unexpected trailing bytes", ErrBadFormat, len(data)-off)
	}
	return packs, nil
}

// indexAAD binds a sealed index object to its own ID.
func indexAAD(id crypto.ID) []byte {
	return append([]byte("gobackup v1 index"), id[:]...)
}

// Save writes a new index object describing the given packs and returns its ID.
// If a cache is supplied, the object is stored there too, so the run that wrote
// it does not have to download it again next time.
func Save(ctx context.Context, be backend.Backend, key *crypto.Key, packs []PackEntries, c ObjectCache) (crypto.ID, error) {
	if len(packs) == 0 {
		return crypto.ID{}, errors.New("index: refusing to save an empty index")
	}

	id, err := randomID()
	if err != nil {
		return crypto.ID{}, err
	}

	sealed, err := key.SealMeta(encode(packs), indexAAD(id))
	if err != nil {
		return crypto.ID{}, fmt.Errorf("index: sealing index object: %w", err)
	}

	h := backend.Handle{Type: backend.TypeIndex, Name: id.String()}
	if err := be.Save(ctx, h, bytes.NewReader(sealed), int64(len(sealed))); err != nil {
		return crypto.ID{}, err
	}

	if c != nil {
		_ = c.Put(id, sealed)
	}
	return id, nil
}

// An ObjectCache stores index objects locally between runs. It is optional;
// a nil cache simply means every index object is fetched from the repository.
type ObjectCache interface {
	Get(id crypto.ID) ([]byte, bool)
	Put(id crypto.ID, data []byte) error
}

// LoadOne reads and decodes a single index object, consulting the cache first.
func LoadOne(ctx context.Context, be backend.Backend, key *crypto.Key, id crypto.ID, c ObjectCache) ([]PackEntries, error) {
	if c != nil {
		if sealed, ok := c.Get(id); ok {
			if packs, err := openAndDecode(key, id, sealed); err == nil {
				return packs, nil
			}
			// A cached object that will not authenticate is corrupt or
			// truncated. The repository is the source of truth, so fall through
			// and refetch, overwriting the bad entry below.
		}
	}

	sealed, err := backend.LoadAll(ctx, be, backend.Handle{Type: backend.TypeIndex, Name: id.String()})
	if err != nil {
		return nil, err
	}
	packs, err := openAndDecode(key, id, sealed)
	if err != nil {
		return nil, err
	}

	if c != nil {
		// Failing to cache only costs speed on the next run.
		_ = c.Put(id, sealed)
	}
	return packs, nil
}

func openAndDecode(key *crypto.Key, id crypto.ID, sealed []byte) ([]PackEntries, error) {
	plaintext, err := key.OpenMeta(sealed, indexAAD(id))
	if err != nil {
		return nil, fmt.Errorf("index %s: %w", id.Short(), err)
	}
	packs, err := decode(plaintext)
	if err != nil {
		return nil, fmt.Errorf("index %s: %w", id.Short(), err)
	}
	return packs, nil
}

// Load reads every index object in the repository and merges them into a single
// view.
func Load(ctx context.Context, be backend.Backend, key *crypto.Key, c ObjectCache) (*Index, error) {
	var ids []crypto.ID
	err := be.List(ctx, backend.TypeIndex, func(fi backend.FileInfo) error {
		id, err := crypto.ParseID(fi.Name)
		if err != nil {
			// An unrecognizable name is not an index object we wrote; ignore it
			// rather than fail the whole run.
			return nil
		}
		ids = append(ids, id)
		return nil
	})
	if err != nil {
		return nil, err
	}

	// Every backup run adds an index object, so a repository with a cold cache
	// can have hundreds to fetch. They are downloaded concurrently, then merged
	// in listing order so the result does not depend on which finished first.
	loaded := make([][]PackEntries, len(ids))
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
			packs, err := LoadOne(ctx, be, key, id, c)
			if err != nil {
				mu.Lock()
				if firstErr == nil {
					firstErr = err
				}
				mu.Unlock()
				return
			}
			loaded[i] = packs
		}(i, id)
	}
	wg.Wait()
	if firstErr != nil {
		return nil, firstErr
	}

	idx := New()
	for i, id := range ids {
		for _, p := range loaded[i] {
			idx.AddPack(p.PackID, p.Entries)
		}
		idx.sourceID = append(idx.sourceID, id)
	}
	return idx, nil
}

// loadConcurrency bounds how many index objects are fetched at once.
const loadConcurrency = 8

// Rebuild reconstructs an index by reading the header of every pack in the
// repository.
//
// This is the recovery path that the pack format's tail header exists for: even
// if every index object is lost, the repository remains fully readable, because
// each pack describes its own contents. It costs one Stat and one small ranged
// read per pack rather than a full download.
func Rebuild(ctx context.Context, be backend.Backend, key *crypto.Key) (*Index, error) {
	var packIDs []crypto.ID
	err := be.List(ctx, backend.TypePack, func(fi backend.FileInfo) error {
		id, err := crypto.ParseID(fi.Name)
		if err != nil {
			return nil
		}
		packIDs = append(packIDs, id)
		return nil
	})
	if err != nil {
		return nil, err
	}

	idx := New()
	for _, packID := range packIDs {
		header, err := pack.LoadHeader(ctx, be, key, packID)
		if err != nil {
			return nil, fmt.Errorf("index: rebuilding from pack %s: %w", packID.Short(), err)
		}
		idx.AddPack(packID, header.Entries)
	}
	return idx, nil
}

func randomID() (crypto.ID, error) {
	var id crypto.ID
	if _, err := rand.Read(id[:]); err != nil {
		return crypto.ID{}, fmt.Errorf("index: generating index ID: %w", err)
	}
	return id, nil
}
