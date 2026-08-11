package pack

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"testing"

	"github.com/senecal-jjs/cmd-line-backup/internal/backend"
	"github.com/senecal-jjs/cmd-line-backup/internal/backend/local"
	"github.com/senecal-jjs/cmd-line-backup/internal/crypto"
)

func testKey(t *testing.T) *crypto.Key {
	t.Helper()
	k, err := crypto.GenerateKey()
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	return k
}

func testBackend(t *testing.T) backend.Backend {
	t.Helper()
	b, err := local.Open(t.TempDir())
	if err != nil {
		t.Fatalf("local.Open: %v", err)
	}
	t.Cleanup(func() { b.Close() })
	return b
}

// storePack uploads a finished pack so the reader can be tested against it.
func storePack(t *testing.T, be backend.Backend, p *Pack) {
	t.Helper()
	h := backend.Handle{Type: backend.TypePack, Name: p.ID.String()}
	if err := be.Save(context.Background(), h, bytes.NewReader(p.Data), int64(len(p.Data))); err != nil {
		t.Fatalf("Save: %v", err)
	}
}

func TestWriteAndReadBlobs(t *testing.T) {
	key := testKey(t)
	be := testBackend(t)

	w, err := NewWriter(key, CompressionZstd)
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}

	// Mix compressible and incompressible content of varying sizes.
	blobs := map[crypto.ID][]byte{}
	contents := [][]byte{
		bytes.Repeat([]byte("compressible "), 1000),
		randomBytes(50000, 1),
		[]byte("tiny"),
		{},
		bytes.Repeat([]byte{0}, 100000),
	}
	for _, content := range contents {
		id := key.ChunkID(content)
		added, err := w.Add(BlobData, id, content)
		if err != nil {
			t.Fatalf("Add: %v", err)
		}
		if !added {
			t.Fatalf("Add reported the blob as a duplicate")
		}
		blobs[id] = content
	}

	p, err := w.Finish()
	if err != nil {
		t.Fatalf("Finish: %v", err)
	}
	storePack(t, be, p)

	header, err := LoadHeader(context.Background(), be, key, p.ID)
	if err != nil {
		t.Fatalf("LoadHeader: %v", err)
	}
	if len(header.Entries) != len(blobs) {
		t.Fatalf("header has %d entries, want %d", len(header.Entries), len(blobs))
	}

	for id, want := range blobs {
		entry, ok := header.Find(id)
		if !ok {
			t.Fatalf("blob %s missing from the header", id.Short())
		}
		got, err := LoadBlob(context.Background(), be, key, p.ID, entry)
		if err != nil {
			t.Fatalf("LoadBlob(%s): %v", id.Short(), err)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("blob %s did not survive the round trip", id.Short())
		}
	}
}

// TestHeaderRecoveryReadsOnlyTheTail is the property that lets a lost index be
// rebuilt cheaply: recovering a pack's contents must not require downloading the
// whole pack.
func TestHeaderRecoveryReadsOnlyTheTail(t *testing.T) {
	key := testKey(t)
	be := testBackend(t)

	w, err := NewWriter(key, CompressionNone)
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	for i := 0; i < 8; i++ {
		content := randomBytes(1<<20, int64(i))
		if _, err := w.Add(BlobData, key.ChunkID(content), content); err != nil {
			t.Fatalf("Add: %v", err)
		}
	}
	p, err := w.Finish()
	if err != nil {
		t.Fatalf("Finish: %v", err)
	}
	storePack(t, be, p)

	counting := &countingBackend{Backend: be}
	header, err := LoadHeader(context.Background(), counting, key, p.ID)
	if err != nil {
		t.Fatalf("LoadHeader: %v", err)
	}
	if len(header.Entries) != 8 {
		t.Fatalf("recovered %d entries, want 8", len(header.Entries))
	}

	if counting.bytesRead > tailReadSize {
		t.Errorf("LoadHeader read %d bytes from an %d byte pack, want at most %d",
			counting.bytesRead, len(p.Data), tailReadSize)
	}
}

// TestLargeHeaderFallback covers a pack with so many small blobs that its header
// does not fit in the single tail read.
func TestLargeHeaderFallback(t *testing.T) {
	key := testKey(t)
	be := testBackend(t)

	w, err := NewWriter(key, CompressionNone)
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}

	const count = 4000 // 4000 * 46 bytes per entry comfortably exceeds tailReadSize
	want := make(map[crypto.ID][]byte, count)
	for i := 0; i < count; i++ {
		content := []byte(fmt.Sprintf("blob number %d", i))
		id := key.ChunkID(content)
		if _, err := w.Add(BlobData, id, content); err != nil {
			t.Fatalf("Add: %v", err)
		}
		want[id] = content
	}
	p, err := w.Finish()
	if err != nil {
		t.Fatalf("Finish: %v", err)
	}
	storePack(t, be, p)

	header, err := LoadHeader(context.Background(), be, key, p.ID)
	if err != nil {
		t.Fatalf("LoadHeader: %v", err)
	}
	if len(header.Entries) != count {
		t.Fatalf("recovered %d entries, want %d", len(header.Entries), count)
	}

	for _, entry := range header.Entries {
		got, err := LoadBlob(context.Background(), be, key, p.ID, entry)
		if err != nil {
			t.Fatalf("LoadBlob: %v", err)
		}
		if !bytes.Equal(got, want[entry.ID]) {
			t.Fatalf("blob %s did not survive the round trip", entry.ID.Short())
		}
	}
}

func TestAddSkipsDuplicatesWithinAPack(t *testing.T) {
	key := testKey(t)
	w, err := NewWriter(key, CompressionNone)
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}

	content := []byte("stored once")
	id := key.ChunkID(content)

	added, err := w.Add(BlobData, id, content)
	if err != nil || !added {
		t.Fatalf("first Add: added=%v err=%v", added, err)
	}
	sizeAfterFirst := w.Size()

	added, err = w.Add(BlobData, id, content)
	if err != nil {
		t.Fatalf("second Add: %v", err)
	}
	if added {
		t.Error("Add stored a duplicate blob")
	}
	if w.Size() != sizeAfterFirst {
		t.Errorf("pack grew from %d to %d bytes on a duplicate Add", sizeAfterFirst, w.Size())
	}
	if w.Count() != 1 {
		t.Errorf("Count = %d, want 1", w.Count())
	}
	if !w.Has(id) {
		t.Error("Has reported a stored blob as absent")
	}
}

func TestCompressionIsSkippedWhenItDoesNotHelp(t *testing.T) {
	key := testKey(t)

	incompressible := randomBytes(100000, 42)
	compressible := bytes.Repeat([]byte("aaaa"), 25000)

	w, err := NewWriter(key, CompressionZstd)
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	if _, err := w.Add(BlobData, key.ChunkID(incompressible), incompressible); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if _, err := w.Add(BlobData, key.ChunkID(compressible), compressible); err != nil {
		t.Fatalf("Add: %v", err)
	}
	p, err := w.Finish()
	if err != nil {
		t.Fatalf("Finish: %v", err)
	}

	byID := map[crypto.ID]Entry{}
	for _, e := range p.Entries {
		byID[e.ID] = e
	}

	if got := byID[key.ChunkID(incompressible)].Compression; got != CompressionNone {
		t.Errorf("random data was stored with compression %v, want none", got)
	}
	if got := byID[key.ChunkID(compressible)].Compression; got != CompressionZstd {
		t.Errorf("repetitive data was stored with compression %v, want zstd", got)
	}

	// The compressible blob must actually be smaller on disk.
	e := byID[key.ChunkID(compressible)]
	if int(e.Length) >= len(compressible) {
		t.Errorf("compressed blob occupies %d bytes for %d bytes of input", e.Length, len(compressible))
	}
	if int(e.PlainLength) != len(compressible) {
		t.Errorf("PlainLength = %d, want %d", e.PlainLength, len(compressible))
	}
}

func TestCompressionDisabled(t *testing.T) {
	key := testKey(t)
	be := testBackend(t)

	w, err := NewWriter(key, CompressionNone)
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	content := bytes.Repeat([]byte("aaaa"), 25000)
	if _, err := w.Add(BlobData, key.ChunkID(content), content); err != nil {
		t.Fatalf("Add: %v", err)
	}
	p, err := w.Finish()
	if err != nil {
		t.Fatalf("Finish: %v", err)
	}
	storePack(t, be, p)

	if p.Entries[0].Compression != CompressionNone {
		t.Errorf("compression was applied despite being disabled")
	}

	got, err := LoadBlob(context.Background(), be, key, p.ID, p.Entries[0])
	if err != nil {
		t.Fatalf("LoadBlob: %v", err)
	}
	if !bytes.Equal(got, content) {
		t.Error("uncompressed blob did not survive the round trip")
	}
}

func TestBlobTypesArePreserved(t *testing.T) {
	key := testKey(t)
	be := testBackend(t)

	w, err := NewWriter(key, CompressionNone)
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	data := []byte("file content")
	tree := []byte("directory metadata")
	dataID, treeID := key.ChunkID(data), key.ChunkID(tree)

	if _, err := w.Add(BlobData, dataID, data); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if _, err := w.Add(BlobTree, treeID, tree); err != nil {
		t.Fatalf("Add: %v", err)
	}
	p, err := w.Finish()
	if err != nil {
		t.Fatalf("Finish: %v", err)
	}
	storePack(t, be, p)

	header, err := LoadHeader(context.Background(), be, key, p.ID)
	if err != nil {
		t.Fatalf("LoadHeader: %v", err)
	}
	for _, e := range header.Entries {
		want := BlobData
		if e.ID == treeID {
			want = BlobTree
		}
		if e.Type != want {
			t.Errorf("blob %s has type %v, want %v", e.ID.Short(), e.Type, want)
		}
	}
}

// TestBlobIsBoundToItsIdentity checks the additional authenticated data: a
// sealed blob must not decrypt when presented under a different ID or type, so
// an attacker cannot rearrange valid blobs within a repository.
func TestBlobIsBoundToItsIdentity(t *testing.T) {
	key := testKey(t)
	be := testBackend(t)

	w, err := NewWriter(key, CompressionNone)
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	content := []byte("some content")
	id := key.ChunkID(content)
	if _, err := w.Add(BlobData, id, content); err != nil {
		t.Fatalf("Add: %v", err)
	}
	p, err := w.Finish()
	if err != nil {
		t.Fatalf("Finish: %v", err)
	}
	storePack(t, be, p)

	entry := p.Entries[0]
	sealed := p.Data[entry.Offset : entry.Offset+entry.Length]

	wrongID := entry
	wrongID.ID = key.ChunkID([]byte("different content"))
	if _, err := DecodeBlob(key, wrongID, sealed); err == nil {
		t.Error("a blob decoded under the wrong ID")
	}

	wrongType := entry
	wrongType.Type = BlobTree
	if _, err := DecodeBlob(key, wrongType, sealed); err == nil {
		t.Error("a blob decoded under the wrong type")
	}
}

// TestHeaderIsBoundToItsPack prevents a header from being transplanted between
// packs, which would make the index describe the wrong bytes.
func TestHeaderIsBoundToItsPack(t *testing.T) {
	key := testKey(t)

	makePack := func(content string) *Pack {
		w, err := NewWriter(key, CompressionNone)
		if err != nil {
			t.Fatalf("NewWriter: %v", err)
		}
		if _, err := w.Add(BlobData, key.ChunkID([]byte(content)), []byte(content)); err != nil {
			t.Fatalf("Add: %v", err)
		}
		p, err := w.Finish()
		if err != nil {
			t.Fatalf("Finish: %v", err)
		}
		return p
	}

	first, second := makePack("one"), makePack("two")

	headerLen, err := decodeFooter(first.Data[len(first.Data)-footerSize:])
	if err != nil {
		t.Fatalf("decodeFooter: %v", err)
	}
	sealedHeader := first.Data[len(first.Data)-footerSize-int(headerLen) : len(first.Data)-footerSize]

	if _, err := DecodeHeader(key, first.ID, sealedHeader); err != nil {
		t.Fatalf("header failed to decode under its own pack ID: %v", err)
	}
	if _, err := DecodeHeader(key, second.ID, sealedHeader); err == nil {
		t.Error("a header decoded under a different pack's ID")
	}
}

func TestCorruptedPackIsDetected(t *testing.T) {
	key := testKey(t)

	build := func() *Pack {
		w, err := NewWriter(key, CompressionNone)
		if err != nil {
			t.Fatalf("NewWriter: %v", err)
		}
		content := randomBytes(10000, 7)
		if _, err := w.Add(BlobData, key.ChunkID(content), content); err != nil {
			t.Fatalf("Add: %v", err)
		}
		p, err := w.Finish()
		if err != nil {
			t.Fatalf("Finish: %v", err)
		}
		return p
	}

	tests := []struct {
		name    string
		corrupt func(p *Pack)
	}{
		{"blob body", func(p *Pack) { p.Data[100] ^= 0xFF }},
		{"header", func(p *Pack) { p.Data[len(p.Data)-footerSize-5] ^= 0xFF }},
		{"footer magic", func(p *Pack) { p.Data[len(p.Data)-footerSize+4] ^= 0xFF }},
		{"header length", func(p *Pack) { p.Data[len(p.Data)-footerSize] ^= 0xFF }},
		{"version", func(p *Pack) { p.Data[len(p.Data)-1] ^= 0xFF }},
		{"truncated", func(p *Pack) { p.Data = p.Data[:len(p.Data)-4] }},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			be := testBackend(t)
			p := build()
			tt.corrupt(p)
			storePack(t, be, p)

			if _, err := LoadHeader(context.Background(), be, key, p.ID); err != nil {
				return // detected at header load, which is fine
			}
			// Otherwise the damage must be caught when the blob is read.
			header, err := LoadHeader(context.Background(), be, key, p.ID)
			if err != nil {
				return
			}
			for _, entry := range header.Entries {
				if _, err := LoadBlob(context.Background(), be, key, p.ID, entry); err != nil {
					return
				}
			}
			t.Error("corruption went undetected")
		})
	}
}

func TestWrongKeyCannotReadPack(t *testing.T) {
	key, other := testKey(t), testKey(t)
	be := testBackend(t)

	w, err := NewWriter(key, CompressionNone)
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	content := []byte("private data")
	if _, err := w.Add(BlobData, key.ChunkID(content), content); err != nil {
		t.Fatalf("Add: %v", err)
	}
	p, err := w.Finish()
	if err != nil {
		t.Fatalf("Finish: %v", err)
	}
	storePack(t, be, p)

	if _, err := LoadHeader(context.Background(), be, other, p.ID); err == nil {
		t.Error("a pack header was readable with the wrong key")
	}
	if _, err := LoadBlob(context.Background(), be, other, p.ID, p.Entries[0]); err == nil {
		t.Error("a pack blob was readable with the wrong key")
	}
}

// TestPackLeaksNoPlaintext confirms the stored object contains none of the
// original content, since the whole point is that the storage provider learns
// nothing.
func TestPackLeaksNoPlaintext(t *testing.T) {
	key := testKey(t)

	w, err := NewWriter(key, CompressionNone)
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	secret := []byte("SUPER SECRET CONTENT THAT MUST NOT APPEAR IN STORAGE")
	id := key.ChunkID(secret)
	if _, err := w.Add(BlobData, id, secret); err != nil {
		t.Fatalf("Add: %v", err)
	}
	p, err := w.Finish()
	if err != nil {
		t.Fatalf("Finish: %v", err)
	}

	if bytes.Contains(p.Data, secret) {
		t.Error("the pack contains the plaintext")
	}
	// Chunk IDs are metadata and live in the sealed header, so they must not be
	// visible in the object either.
	if bytes.Contains(p.Data, id[:]) {
		t.Error("the pack exposes a chunk ID in the clear")
	}
}

func TestFinishRejectsEmptyPack(t *testing.T) {
	w, err := NewWriter(testKey(t), CompressionNone)
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	if _, err := w.Finish(); err == nil {
		t.Error("Finish accepted an empty pack")
	}
}

func TestWriterRejectsUseAfterFinish(t *testing.T) {
	key := testKey(t)
	w, err := NewWriter(key, CompressionNone)
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	content := []byte("x")
	if _, err := w.Add(BlobData, key.ChunkID(content), content); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if _, err := w.Finish(); err != nil {
		t.Fatalf("Finish: %v", err)
	}

	if _, err := w.Add(BlobData, key.ChunkID([]byte("y")), []byte("y")); err == nil {
		t.Error("Add succeeded after Finish")
	}
	if _, err := w.Finish(); err == nil {
		t.Error("Finish succeeded twice")
	}
}

func TestNewWriterValidatesArguments(t *testing.T) {
	if _, err := NewWriter(nil, CompressionNone); err == nil {
		t.Error("NewWriter accepted a nil key")
	}
	if _, err := NewWriter(testKey(t), Compression(99)); err == nil {
		t.Error("NewWriter accepted an unknown compression")
	}
}

func TestAddRejectsUnknownBlobType(t *testing.T) {
	key := testKey(t)
	w, err := NewWriter(key, CompressionNone)
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	if _, err := w.Add(BlobType(99), key.ChunkID([]byte("x")), []byte("x")); err == nil {
		t.Error("Add accepted an unknown blob type")
	}
}

func TestHeaderCodecRoundTrip(t *testing.T) {
	entries := []Entry{
		{Type: BlobData, ID: crypto.ID{1, 2, 3}, Offset: 0, Length: 100, PlainLength: 90, Compression: CompressionZstd},
		{Type: BlobTree, ID: crypto.ID{4, 5, 6}, Offset: 100, Length: 200, PlainLength: 200, Compression: CompressionNone},
	}

	got, err := decodeHeader(encodeHeader(entries))
	if err != nil {
		t.Fatalf("decodeHeader: %v", err)
	}
	if len(got.Entries) != len(entries) {
		t.Fatalf("got %d entries, want %d", len(got.Entries), len(entries))
	}
	for i := range entries {
		if got.Entries[i] != entries[i] {
			t.Errorf("entry %d = %+v, want %+v", i, got.Entries[i], entries[i])
		}
	}
}

func TestDecodeHeaderRejectsMalformedInput(t *testing.T) {
	valid := encodeHeader([]Entry{{Type: BlobData, ID: crypto.ID{1}, Length: 10}})

	tests := []struct {
		name string
		data []byte
	}{
		{"empty", nil},
		{"short", []byte{1, 2}},
		{"truncated entries", valid[:len(valid)-5]},
		{"trailing bytes", append(bytes.Clone(valid), 0)},
		{"absurd count", []byte{0xFF, 0xFF, 0xFF, 0xFF}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := decodeHeader(tt.data); err == nil {
				t.Error("decodeHeader accepted malformed input")
			}
		})
	}

	badType := encodeHeader([]Entry{{Type: BlobType(99), ID: crypto.ID{1}}})
	if _, err := decodeHeader(badType); !errors.Is(err, ErrBadFormat) {
		t.Errorf("got %v, want ErrBadFormat for an unknown blob type", err)
	}

	badCompression := encodeHeader([]Entry{{Type: BlobData, Compression: Compression(99)}})
	if _, err := decodeHeader(badCompression); !errors.Is(err, ErrBadFormat) {
		t.Errorf("got %v, want ErrBadFormat for unknown compression", err)
	}
}

func TestDecodeFooterRejectsMalformedInput(t *testing.T) {
	if _, err := decodeFooter(make([]byte, footerSize-1)); err == nil {
		t.Error("decodeFooter accepted a short footer")
	}
	if _, err := decodeFooter(make([]byte, footerSize)); !errors.Is(err, ErrBadMagic) {
		t.Error("decodeFooter accepted a footer without the magic value")
	}

	// A header length beyond the cap must be rejected rather than cause a huge
	// allocation.
	oversized := encodeFooter(MaxHeaderSize + 1)
	if _, err := decodeFooter(oversized); !errors.Is(err, ErrBadFormat) {
		t.Errorf("got %v, want ErrBadFormat for an oversized header length", err)
	}
}

func TestLoadHeaderOnMissingPack(t *testing.T) {
	be := testBackend(t)
	if _, err := LoadHeader(context.Background(), be, testKey(t), crypto.ID{9, 9, 9}); !errors.Is(err, backend.ErrNotFound) {
		t.Errorf("got %v, want ErrNotFound", err)
	}
}

func TestBlobTypeAndCompressionStrings(t *testing.T) {
	if BlobData.String() != "data" || BlobTree.String() != "tree" {
		t.Error("unexpected BlobType strings")
	}
	if BlobType(99).Valid() {
		t.Error("BlobType(99) reported as valid")
	}
	if CompressionNone.String() != "none" || CompressionZstd.String() != "zstd" {
		t.Error("unexpected Compression strings")
	}
	if Compression(99).Valid() {
		t.Error("Compression(99) reported as valid")
	}
}

// countingBackend records how many bytes were read through it.
type countingBackend struct {
	backend.Backend
	bytesRead int64
}

func (c *countingBackend) Load(ctx context.Context, h backend.Handle, offset, length int64) (io.ReadCloser, error) {
	rd, err := c.Backend.Load(ctx, h, offset, length)
	if err != nil {
		return nil, err
	}
	return &countingReadCloser{ReadCloser: rd, n: &c.bytesRead}, nil
}

type countingReadCloser struct {
	io.ReadCloser
	n *int64
}

func (c *countingReadCloser) Read(p []byte) (int, error) {
	n, err := c.ReadCloser.Read(p)
	*c.n += int64(n)
	return n, err
}

func randomBytes(n int, seed int64) []byte {
	out := make([]byte, n)
	state := uint64(seed) + 0x9E3779B97F4A7C15
	for i := 0; i < n; i += 8 {
		state += 0x9E3779B97F4A7C15
		z := state
		z = (z ^ (z >> 30)) * 0xBF58476D1CE4E5B9
		z = (z ^ (z >> 27)) * 0x94D049BB133111EB
		z ^= z >> 31
		for j := 0; j < 8 && i+j < n; j++ {
			out[i+j] = byte(z >> (8 * j))
		}
	}
	return out
}
