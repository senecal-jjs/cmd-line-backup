package index

import (
	"bytes"
	"context"
	"errors"
	"runtime"
	"sync"
	"testing"

	"github.com/senecal-jjs/cmd-line-backup/internal/backend"
	"github.com/senecal-jjs/cmd-line-backup/internal/backend/local"
	"github.com/senecal-jjs/cmd-line-backup/internal/crypto"
	"github.com/senecal-jjs/cmd-line-backup/internal/pack"
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

func makeEntries(n int, seed byte) []pack.Entry {
	entries := make([]pack.Entry, n)
	for i := range entries {
		var id crypto.ID
		id[0] = seed
		id[1] = byte(i)
		id[2] = byte(i >> 8)
		entries[i] = pack.Entry{
			Type:        pack.BlobData,
			ID:          id,
			Offset:      uint32(i * 1000),
			Length:      1000,
			PlainLength: 900,
			Compression: pack.CompressionZstd,
		}
	}
	return entries
}

func TestAddAndLookup(t *testing.T) {
	idx := New()
	packID := crypto.ID{0xAA}
	entries := makeEntries(10, 1)
	idx.AddPack(packID, entries)

	if idx.Len() != 10 {
		t.Errorf("Len = %d, want 10", idx.Len())
	}

	for _, e := range entries {
		loc, ok := idx.Lookup(e.ID)
		if !ok {
			t.Fatalf("blob %s not found", e.ID.Short())
		}
		if loc.PackID != packID {
			t.Errorf("blob %s is in pack %s, want %s", e.ID.Short(), loc.PackID.Short(), packID.Short())
		}
		if loc.Entry != e {
			t.Errorf("blob %s entry = %+v, want %+v", e.ID.Short(), loc.Entry, e)
		}
		if !idx.Has(e.ID) {
			t.Errorf("Has reported blob %s as absent", e.ID.Short())
		}
	}

	if idx.Has(crypto.ID{0xFF}) {
		t.Error("Has reported an unknown blob as present")
	}
	if _, ok := idx.Lookup(crypto.ID{0xFF}); ok {
		t.Error("Lookup found an unknown blob")
	}
}

// TestFirstLocationWins documents the resolution for a blob stored twice, which
// happens when two backups run concurrently and both upload the same new chunk.
// Either copy would do, so the index keeps the one it saw first and the other is
// simply left unreferenced.
func TestFirstLocationWins(t *testing.T) {
	idx := New()
	shared := crypto.ID{0x11}

	first := crypto.ID{0xA1}
	second := crypto.ID{0xB2}
	idx.AddPack(first, []pack.Entry{{Type: pack.BlobData, ID: shared, Offset: 0, Length: 10}})
	idx.AddPack(second, []pack.Entry{{Type: pack.BlobData, ID: shared, Offset: 500, Length: 10}})

	loc, ok := idx.Lookup(shared)
	if !ok {
		t.Fatal("blob not found")
	}
	if loc.PackID != first {
		t.Errorf("blob resolved to pack %s, want the first one %s", loc.PackID.Short(), first.Short())
	}
	if idx.Len() != 1 {
		t.Errorf("Len = %d, want 1", idx.Len())
	}
	// Both packs are still referenced by the index.
	if got := len(idx.PackIDs()); got != 2 {
		t.Errorf("PackIDs returned %d packs, want 2", got)
	}
}

func TestEncodeDecodeRoundTrip(t *testing.T) {
	packs := []PackEntries{
		{PackID: crypto.ID{1}, Entries: makeEntries(5, 10)},
		{PackID: crypto.ID{2}, Entries: makeEntries(3, 20)},
		{PackID: crypto.ID{3}, Entries: []pack.Entry{{Type: pack.BlobTree, ID: crypto.ID{9}, Length: 42, PlainLength: 40}}},
	}

	got, err := decode(encode(packs))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got) != len(packs) {
		t.Fatalf("decoded %d packs, want %d", len(got), len(packs))
	}
	for i := range packs {
		if got[i].PackID != packs[i].PackID {
			t.Errorf("pack %d ID mismatch", i)
		}
		if len(got[i].Entries) != len(packs[i].Entries) {
			t.Fatalf("pack %d has %d entries, want %d", i, len(got[i].Entries), len(packs[i].Entries))
		}
		for j := range packs[i].Entries {
			if got[i].Entries[j] != packs[i].Entries[j] {
				t.Errorf("pack %d entry %d = %+v, want %+v", i, j, got[i].Entries[j], packs[i].Entries[j])
			}
		}
	}
}

func TestDecodeRejectsMalformedInput(t *testing.T) {
	valid := encode([]PackEntries{{PackID: crypto.ID{1}, Entries: makeEntries(2, 1)}})

	tests := []struct {
		name string
		data []byte
	}{
		{"empty", nil},
		{"short header", []byte{1, 2, 3}},
		{"truncated", valid[:len(valid)-10]},
		{"trailing bytes", append(bytes.Clone(valid), 0, 0)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := decode(tt.data); err == nil {
				t.Error("decode accepted malformed input")
			}
		})
	}

	// A pack count that promises far more data than the object holds must be
	// rejected rather than cause a huge allocation.
	huge := bytes.Clone(valid)
	huge[4], huge[5], huge[6], huge[7] = 0xFF, 0xFF, 0xFF, 0xFF
	if _, err := decode(huge); !errors.Is(err, ErrBadFormat) {
		t.Errorf("got %v, want ErrBadFormat for an absurd pack count", err)
	}
}

// TestDecodeRejectsBeforeAllocating checks that malformed counts are refused
// *before* they are used to size an allocation, not merely that an error comes
// back eventually.
//
// The distinction is what separates a clean rejection from a crash. A four-byte
// count field covers values up to 4294967295, and at 56 bytes per PackEntries
// that is a 240 GB request. Reserving it first and validating afterwards still
// produces the right error on a machine with lazy overcommit, so the difference
// is invisible on macOS while being fatal on a memory-limited Linux CI runner.
// Asserting on allocation, rather than on the returned error, is the only way to
// pin that down.
func TestDecodeRejectsBeforeAllocating(t *testing.T) {
	valid := encode([]PackEntries{{PackID: crypto.ID{1}, Entries: makeEntries(2, 1)}})

	tests := []struct {
		name   string
		mutate func([]byte)
	}{
		{"absurd pack count", func(b []byte) {
			b[4], b[5], b[6], b[7] = 0xFF, 0xFF, 0xFF, 0xFF
		}},
		{"absurd entry count", func(b []byte) {
			// The entry count sits just past the version, pack count, and pack ID.
			off := 8 + crypto.IDSize
			b[off], b[off+1], b[off+2], b[off+3] = 0xFF, 0xFF, 0xFF, 0xFF
		}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			data := bytes.Clone(valid)
			tt.mutate(data)

			var before, after runtime.MemStats
			runtime.GC()
			runtime.ReadMemStats(&before)

			if _, err := decode(data); !errors.Is(err, ErrBadFormat) {
				t.Fatalf("got %v, want ErrBadFormat", err)
			}

			runtime.ReadMemStats(&after)
			if grew := after.TotalAlloc - before.TotalAlloc; grew > 1<<20 {
				t.Errorf("decode allocated %d bytes while rejecting malformed input; "+
					"the size must be validated before it is allocated", grew)
			}
		})
	}
}

func TestDecodeRejectsWrongVersion(t *testing.T) {
	data := encode([]PackEntries{{PackID: crypto.ID{1}, Entries: makeEntries(1, 1)}})
	data[0] = 99
	if _, err := decode(data); err == nil {
		t.Error("decode accepted an unsupported format version")
	}
}

func TestSaveAndLoad(t *testing.T) {
	key := testKey(t)
	be := testBackend(t)
	ctx := context.Background()

	packs := []PackEntries{
		{PackID: crypto.ID{1}, Entries: makeEntries(4, 10)},
		{PackID: crypto.ID{2}, Entries: makeEntries(6, 20)},
	}
	id, err := Save(ctx, be, key, packs, nil)
	if err != nil {
		t.Fatalf("Save: %v", err)
	}

	idx, err := Load(ctx, be, key, nil)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if idx.Len() != 10 {
		t.Errorf("Len = %d, want 10", idx.Len())
	}
	if sources := idx.SourceIDs(); len(sources) != 1 || sources[0] != id {
		t.Errorf("SourceIDs = %v, want [%s]", sources, id.Short())
	}

	for _, p := range packs {
		for _, e := range p.Entries {
			loc, ok := idx.Lookup(e.ID)
			if !ok {
				t.Fatalf("blob %s missing after reload", e.ID.Short())
			}
			if loc.PackID != p.PackID || loc.Entry != e {
				t.Errorf("blob %s reloaded incorrectly", e.ID.Short())
			}
		}
	}
}

// TestLoadMergesMultipleObjects covers the additive design: every backup run
// writes its own index object and readers combine them all.
func TestLoadMergesMultipleObjects(t *testing.T) {
	key := testKey(t)
	be := testBackend(t)
	ctx := context.Background()

	for i := 0; i < 3; i++ {
		packs := []PackEntries{{PackID: crypto.ID{byte(i + 1)}, Entries: makeEntries(5, byte(i*10+1))}}
		if _, err := Save(ctx, be, key, packs, nil); err != nil {
			t.Fatalf("Save: %v", err)
		}
	}

	idx, err := Load(ctx, be, key, nil)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if idx.Len() != 15 {
		t.Errorf("Len = %d, want 15", idx.Len())
	}
	if got := len(idx.PackIDs()); got != 3 {
		t.Errorf("PackIDs returned %d packs, want 3", got)
	}
	if got := len(idx.SourceIDs()); got != 3 {
		t.Errorf("SourceIDs returned %d objects, want 3", got)
	}
}

func TestLoadOnEmptyRepository(t *testing.T) {
	idx, err := Load(context.Background(), testBackend(t), testKey(t), nil)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if idx.Len() != 0 {
		t.Errorf("Len = %d, want 0", idx.Len())
	}
}

func TestSaveRejectsEmptyInput(t *testing.T) {
	if _, err := Save(context.Background(), testBackend(t), testKey(t), nil, nil); err == nil {
		t.Error("Save accepted an empty index")
	}
}

func TestIndexIsUnreadableWithTheWrongKey(t *testing.T) {
	key, other := testKey(t), testKey(t)
	be := testBackend(t)
	ctx := context.Background()

	if _, err := Save(ctx, be, key, []PackEntries{{PackID: crypto.ID{1}, Entries: makeEntries(2, 1)}}, nil); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if _, err := Load(ctx, be, other, nil); err == nil {
		t.Error("an index object was readable with the wrong key")
	}
}

func TestTamperedIndexIsDetected(t *testing.T) {
	key := testKey(t)
	be := testBackend(t)
	ctx := context.Background()

	id, err := Save(ctx, be, key, []PackEntries{{PackID: crypto.ID{1}, Entries: makeEntries(2, 1)}}, nil)
	if err != nil {
		t.Fatalf("Save: %v", err)
	}

	h := backend.Handle{Type: backend.TypeIndex, Name: id.String()}
	data, err := backend.LoadAll(ctx, be, h)
	if err != nil {
		t.Fatalf("LoadAll: %v", err)
	}
	data[len(data)/2] ^= 0xFF
	if err := be.Save(ctx, h, bytes.NewReader(data), int64(len(data))); err != nil {
		t.Fatalf("Save: %v", err)
	}

	if _, err := Load(ctx, be, key, nil); err == nil {
		t.Error("a tampered index object was accepted")
	}
}

// TestRebuildFromPacks proves the recovery property the pack tail header exists
// for: with every index object deleted, the repository is still fully readable.
func TestRebuildFromPacks(t *testing.T) {
	key := testKey(t)
	be := testBackend(t)
	ctx := context.Background()

	// Build two real packs.
	var saved []PackEntries
	want := map[crypto.ID][]byte{}
	for p := 0; p < 2; p++ {
		w, err := pack.NewWriter(key, pack.CompressionZstd)
		if err != nil {
			t.Fatalf("NewWriter: %v", err)
		}
		for i := 0; i < 5; i++ {
			content := []byte(string(rune('a'+p)) + string(rune('0'+i)) + "-content")
			id := key.ChunkID(content)
			if _, err := w.Add(pack.BlobData, id, content); err != nil {
				t.Fatalf("Add: %v", err)
			}
			want[id] = content
		}
		pk, err := w.Finish()
		if err != nil {
			t.Fatalf("Finish: %v", err)
		}
		h := backend.Handle{Type: backend.TypePack, Name: pk.ID.String()}
		if err := be.Save(ctx, h, bytes.NewReader(pk.Data), int64(len(pk.Data))); err != nil {
			t.Fatalf("Save: %v", err)
		}
		saved = append(saved, PackEntries{PackID: pk.ID, Entries: pk.Entries})
	}

	if _, err := Save(ctx, be, key, saved, nil); err != nil {
		t.Fatalf("Save index: %v", err)
	}

	// Destroy every index object, simulating total index loss.
	var indexIDs []string
	if err := be.List(ctx, backend.TypeIndex, func(fi backend.FileInfo) error {
		indexIDs = append(indexIDs, fi.Name)
		return nil
	}); err != nil {
		t.Fatalf("List: %v", err)
	}
	for _, name := range indexIDs {
		if err := be.Remove(ctx, backend.Handle{Type: backend.TypeIndex, Name: name}); err != nil {
			t.Fatalf("Remove: %v", err)
		}
	}

	empty, err := Load(ctx, be, key, nil)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if empty.Len() != 0 {
		t.Fatalf("index still has %d blobs after deleting every index object", empty.Len())
	}

	rebuilt, err := Rebuild(ctx, be, key)
	if err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	if rebuilt.Len() != len(want) {
		t.Fatalf("rebuilt index has %d blobs, want %d", rebuilt.Len(), len(want))
	}

	// The rebuilt index must be usable, not merely the right size.
	for id, content := range want {
		loc, ok := rebuilt.Lookup(id)
		if !ok {
			t.Fatalf("blob %s missing from the rebuilt index", id.Short())
		}
		got, err := pack.LoadBlob(ctx, be, key, loc.PackID, loc.Entry)
		if err != nil {
			t.Fatalf("LoadBlob(%s): %v", id.Short(), err)
		}
		if !bytes.Equal(got, content) {
			t.Errorf("blob %s read back incorrectly after rebuild", id.Short())
		}
	}
}

func TestRebuildOnEmptyRepository(t *testing.T) {
	idx, err := Rebuild(context.Background(), testBackend(t), testKey(t))
	if err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	if idx.Len() != 0 {
		t.Errorf("Len = %d, want 0", idx.Len())
	}
}

func TestEach(t *testing.T) {
	idx := New()
	idx.AddPack(crypto.ID{1}, makeEntries(5, 1))

	var count int
	if err := idx.Each(func(id crypto.ID, loc Location) error {
		count++
		if loc.PackID != (crypto.ID{1}) {
			t.Errorf("unexpected pack ID %s", loc.PackID.Short())
		}
		return nil
	}); err != nil {
		t.Fatalf("Each: %v", err)
	}
	if count != 5 {
		t.Errorf("Each visited %d blobs, want 5", count)
	}

	sentinel := errors.New("stop")
	if err := idx.Each(func(crypto.ID, Location) error { return sentinel }); !errors.Is(err, sentinel) {
		t.Errorf("Each returned %v, want the callback's error", err)
	}
}

// TestConcurrentAccess exercises the locking, since the archiver queries and
// extends the index from many goroutines at once.
//
// Readers run for the whole duration of the writes rather than as a burst of
// short-lived goroutines, so lookups genuinely overlap with insertions instead
// of mostly racing the scheduler.
func TestConcurrentAccess(t *testing.T) {
	idx := New()
	const workers = 8

	start := make(chan struct{})
	done := make(chan struct{})

	// The entries are built up front so the goroutines do nothing but exercise
	// the index itself.
	batches := make([][]pack.Entry, workers)
	for w := range batches {
		batches[w] = makeEntries(100, byte(w+1))
	}

	var writers sync.WaitGroup
	for w := 0; w < workers; w++ {
		writers.Add(1)
		go func(w int) {
			defer writers.Done()
			<-start
			idx.AddPack(crypto.ID{byte(w)}, batches[w])
		}(w)
	}

	var readers sync.WaitGroup
	for r := 0; r < workers; r++ {
		readers.Add(1)
		go func() {
			defer readers.Done()
			<-start
			for {
				select {
				case <-done:
					return
				default:
				}
				for i := 0; i < 100; i++ {
					idx.Has(crypto.ID{byte(i)})
					idx.Lookup(crypto.ID{byte(i)})
					idx.Len()
				}
			}
		}()
	}

	close(start)
	writers.Wait()
	close(done)
	readers.Wait()

	if idx.Len() != workers*100 {
		t.Errorf("Len = %d, want %d", idx.Len(), workers*100)
	}
	if got := len(idx.PackIDs()); got != workers {
		t.Errorf("PackIDs returned %d packs, want %d", got, workers)
	}
}
