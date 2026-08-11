package repo

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/senecal-jjs/cmd-line-backup/internal/backend"
	"github.com/senecal-jjs/cmd-line-backup/internal/backend/local"
	"github.com/senecal-jjs/cmd-line-backup/internal/chunker"
	"github.com/senecal-jjs/cmd-line-backup/internal/crypto"
	"github.com/senecal-jjs/cmd-line-backup/internal/index"
	"github.com/senecal-jjs/cmd-line-backup/internal/pack"
	"github.com/senecal-jjs/cmd-line-backup/internal/snapshot"
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

func testOptions(t *testing.T) Options {
	t.Helper()
	return Options{CacheDir: t.TempDir(), Compression: pack.CompressionZstd}
}

func initRepo(t *testing.T) (*Repository, backend.Backend, *crypto.Key) {
	t.Helper()
	be, key := testBackend(t), testKey(t)
	r, err := Init(context.Background(), be, key, testOptions(t))
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	return r, be, key
}

func TestInitAndOpen(t *testing.T) {
	r, be, key := initRepo(t)

	cfg := r.Config()
	if cfg.Version != configVersion {
		t.Errorf("Version = %d, want %d", cfg.Version, configVersion)
	}
	if cfg.ID.IsZero() {
		t.Error("repository ID is the zero value")
	}
	if cfg.Created.IsZero() {
		t.Error("creation time was not recorded")
	}

	reopened, err := Open(context.Background(), be, key, testOptions(t))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if reopened.Config().ID != cfg.ID {
		t.Error("reopened repository has a different ID")
	}
}

func TestInitRefusesToOverwrite(t *testing.T) {
	_, be, key := initRepo(t)

	if _, err := Init(context.Background(), be, key, testOptions(t)); !errors.Is(err, ErrAlreadyInitialized) {
		t.Errorf("got %v, want ErrAlreadyInitialized", err)
	}
}

func TestOpenUninitialized(t *testing.T) {
	_, err := Open(context.Background(), testBackend(t), testKey(t), testOptions(t))
	if !errors.Is(err, ErrNotInitialized) {
		t.Errorf("got %v, want ErrNotInitialized", err)
	}
}

func TestOpenWithWrongKey(t *testing.T) {
	_, be, _ := initRepo(t)

	if _, err := Open(context.Background(), be, testKey(t), testOptions(t)); err == nil {
		t.Error("a repository opened with the wrong key")
	}
}

// TestChunkerSettingsPersist matters because chunk boundaries determine
// deduplication: a repository must always be read and written with the settings
// it was created with, not whatever the current defaults happen to be.
func TestChunkerSettingsPersist(t *testing.T) {
	be, key := testBackend(t), testKey(t)
	custom := chunker.Config{MinSize: 1024, AvgSize: 4096, MaxSize: 16384}

	opts := testOptions(t)
	opts.Chunker = custom
	if _, err := Init(context.Background(), be, key, opts); err != nil {
		t.Fatalf("Init: %v", err)
	}

	// Open with different defaults requested; the stored settings must win.
	other := testOptions(t)
	other.Chunker = chunker.Config{MinSize: 99, AvgSize: 128, MaxSize: 256}
	r, err := Open(context.Background(), be, key, other)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if got := r.ChunkerConfig(); got != custom {
		t.Errorf("chunker config = %+v, want %+v", got, custom)
	}
}

func TestSaveAndLoadBlob(t *testing.T) {
	r, _, _ := initRepo(t)
	ctx := context.Background()

	content := []byte("some file content")
	id, added, err := r.SaveBlob(ctx, pack.BlobData, content)
	if err != nil {
		t.Fatalf("SaveBlob: %v", err)
	}
	if !added {
		t.Error("SaveBlob reported a new blob as already present")
	}
	if err := r.Flush(ctx); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	got, err := r.LoadBlob(ctx, id)
	if err != nil {
		t.Fatalf("LoadBlob: %v", err)
	}
	if !bytes.Equal(got, content) {
		t.Errorf("LoadBlob returned %q, want %q", got, content)
	}
}

// TestDeduplication is the core promise: storing the same content twice must
// not store it twice.
func TestDeduplication(t *testing.T) {
	r, _, _ := initRepo(t)
	ctx := context.Background()
	content := []byte("duplicated content")

	firstID, added, err := r.SaveBlob(ctx, pack.BlobData, content)
	if err != nil || !added {
		t.Fatalf("first SaveBlob: added=%v err=%v", added, err)
	}

	secondID, added, err := r.SaveBlob(ctx, pack.BlobData, content)
	if err != nil {
		t.Fatalf("second SaveBlob: %v", err)
	}
	if added {
		t.Error("SaveBlob stored the same content twice")
	}
	if firstID != secondID {
		t.Error("the same content produced different blob IDs")
	}

	if err := r.Flush(ctx); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	// And again after a flush, when the blob is known through the index rather
	// than the open pack.
	_, added, err = r.SaveBlob(ctx, pack.BlobData, content)
	if err != nil {
		t.Fatalf("third SaveBlob: %v", err)
	}
	if added {
		t.Error("SaveBlob re-stored content that was already in the index")
	}
}

// TestDeduplicationAcrossSessions checks that a later run recognizes data stored
// by an earlier one, which is what makes the second backup of a dataset cheap.
func TestDeduplicationAcrossSessions(t *testing.T) {
	be, key := testBackend(t), testKey(t)
	ctx := context.Background()

	first, err := Init(ctx, be, key, testOptions(t))
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	content := []byte("content stored in the first session")
	id, _, err := first.SaveBlob(ctx, pack.BlobData, content)
	if err != nil {
		t.Fatalf("SaveBlob: %v", err)
	}
	if err := first.Flush(ctx); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	second, err := Open(ctx, be, key, testOptions(t))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	gotID, added, err := second.SaveBlob(ctx, pack.BlobData, content)
	if err != nil {
		t.Fatalf("SaveBlob: %v", err)
	}
	if added {
		t.Error("a later session re-stored content that already existed")
	}
	if gotID != id {
		t.Error("blob IDs differ between sessions")
	}
}

// TestPackRollover exercises the size-based pack boundary and confirms every
// blob remains retrievable across several packs.
func TestPackRollover(t *testing.T) {
	r, be, _ := initRepo(t)
	ctx := context.Background()

	// Incompressible payloads, so the pack really does grow.
	const blobSize = 1 << 20
	var ids []crypto.ID
	contents := map[crypto.ID][]byte{}
	for i := 0; i < 40; i++ {
		content := randomBytes(blobSize, int64(i))
		id, _, err := r.SaveBlob(ctx, pack.BlobData, content)
		if err != nil {
			t.Fatalf("SaveBlob: %v", err)
		}
		ids = append(ids, id)
		contents[id] = content
	}
	if err := r.Flush(ctx); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	var packCount int
	if err := be.List(ctx, backend.TypePack, func(backend.FileInfo) error {
		packCount++
		return nil
	}); err != nil {
		t.Fatalf("List: %v", err)
	}
	// 40 MB of data at a 16 MB target must span several packs.
	if packCount < 2 {
		t.Errorf("40 MB of blobs produced %d packs, want at least 2", packCount)
	}

	for _, id := range ids {
		got, err := r.LoadBlob(ctx, id)
		if err != nil {
			t.Fatalf("LoadBlob(%s): %v", id.Short(), err)
		}
		if !bytes.Equal(got, contents[id]) {
			t.Errorf("blob %s came back wrong", id.Short())
		}
	}
}

// TestWriteOrdering verifies the ordering guarantee that makes interrupted
// backups safe: a snapshot is only written after the index, which is only
// written after the packs it describes.
func TestWriteOrdering(t *testing.T) {
	be, key := testBackend(t), testKey(t)
	ctx := context.Background()

	recorder := &orderRecorder{Backend: be}
	r, err := Init(ctx, recorder, key, testOptions(t))
	if err != nil {
		t.Fatalf("Init: %v", err)
	}

	if _, _, err := r.SaveBlob(ctx, pack.BlobData, []byte("content")); err != nil {
		t.Fatalf("SaveBlob: %v", err)
	}
	if err := r.Flush(ctx); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	if _, err := r.SaveSnapshot(ctx, &snapshot.Snapshot{
		Time: time.Now().UTC(), Paths: []string{"/x"}, Tree: crypto.ID{1},
	}); err != nil {
		t.Fatalf("SaveSnapshot: %v", err)
	}

	packAt, indexAt, snapAt := -1, -1, -1
	for i, t := range recorder.order {
		switch t {
		case backend.TypePack:
			if packAt < 0 {
				packAt = i
			}
		case backend.TypeIndex:
			if indexAt < 0 {
				indexAt = i
			}
		case backend.TypeSnapshot:
			if snapAt < 0 {
				snapAt = i
			}
		}
	}

	if packAt < 0 || indexAt < 0 || snapAt < 0 {
		t.Fatalf("not all object types were written: %v", recorder.order)
	}
	if !(packAt < indexAt && indexAt < snapAt) {
		t.Errorf("writes happened in the order %v; packs must precede the index, which must precede the snapshot",
			recorder.order)
	}
}

func TestFlushWithNothingToStore(t *testing.T) {
	r, be, _ := initRepo(t)
	ctx := context.Background()

	if err := r.Flush(ctx); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	// An empty run must not leave stray objects behind.
	for _, ft := range []backend.FileType{backend.TypePack, backend.TypeIndex} {
		var count int
		if err := be.List(ctx, ft, func(backend.FileInfo) error {
			count++
			return nil
		}); err != nil {
			t.Fatalf("List: %v", err)
		}
		if count != 0 {
			t.Errorf("an empty flush wrote %d %s objects", count, ft)
		}
	}
}

func TestSaveAndLoadTree(t *testing.T) {
	r, _, _ := initRepo(t)
	ctx := context.Background()

	tree := snapshot.NewTree([]*snapshot.Node{
		{Name: "a.txt", Type: snapshot.NodeFile, Mode: 0o644, Size: 10, ModTime: time.Unix(1700000000, 0).UTC()},
		{Name: "b.txt", Type: snapshot.NodeFile, Mode: 0o644, Size: 20, ModTime: time.Unix(1700000001, 0).UTC()},
	})

	id, err := r.SaveTree(ctx, tree)
	if err != nil {
		t.Fatalf("SaveTree: %v", err)
	}
	if err := r.Flush(ctx); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	got, err := r.LoadTree(ctx, id)
	if err != nil {
		t.Fatalf("LoadTree: %v", err)
	}
	if len(got.Nodes) != 2 || got.Nodes[0].Name != "a.txt" {
		t.Errorf("tree came back as %+v", got.Nodes)
	}
}

// TestIdenticalTreesDeduplicate is why trees are stored as ordinary blobs:
// an unchanged directory costs nothing on the next backup.
func TestIdenticalTreesDeduplicate(t *testing.T) {
	r, _, _ := initRepo(t)
	ctx := context.Background()

	build := func() *snapshot.Tree {
		return snapshot.NewTree([]*snapshot.Node{
			{Name: "a.txt", Type: snapshot.NodeFile, Mode: 0o644, Size: 10, ModTime: time.Unix(1700000000, 0).UTC()},
		})
	}

	first, err := r.SaveTree(ctx, build())
	if err != nil {
		t.Fatalf("SaveTree: %v", err)
	}
	second, err := r.SaveTree(ctx, build())
	if err != nil {
		t.Fatalf("SaveTree: %v", err)
	}
	if first != second {
		t.Error("identical directories produced different tree IDs")
	}
}

// TestTreeCacheIsVerified confirms the cache is never trusted blindly: a
// corrupted local entry must be bypassed in favor of the repository.
func TestTreeCacheIsVerified(t *testing.T) {
	be, key := testBackend(t), testKey(t)
	ctx := context.Background()
	opts := testOptions(t)

	r, err := Init(ctx, be, key, opts)
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	tree := snapshot.NewTree([]*snapshot.Node{
		{Name: "a.txt", Type: snapshot.NodeFile, Mode: 0o644, ModTime: time.Unix(1700000000, 0).UTC()},
	})
	id, err := r.SaveTree(ctx, tree)
	if err != nil {
		t.Fatalf("SaveTree: %v", err)
	}
	if err := r.Flush(ctx); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	// Poison the cached copy with content that does not match its ID.
	if err := r.Cache().Put("tree", id, []byte(`{"nodes":[{"name":"attacker","type":"file"}]}`)); err != nil {
		t.Fatalf("Put: %v", err)
	}

	got, err := r.LoadTree(ctx, id)
	if err != nil {
		t.Fatalf("LoadTree: %v", err)
	}
	if len(got.Nodes) != 1 || got.Nodes[0].Name != "a.txt" {
		t.Errorf("a poisoned cache entry was served: %+v", got.Nodes)
	}
}

func TestLoadBlobUnknownID(t *testing.T) {
	r, _, _ := initRepo(t)
	if _, err := r.LoadBlob(context.Background(), crypto.ID{0xFF}); err == nil {
		t.Error("LoadBlob succeeded for an unknown ID")
	}
}

func TestSnapshotHelpers(t *testing.T) {
	r, _, _ := initRepo(t)
	ctx := context.Background()

	snap := &snapshot.Snapshot{
		Time:     time.Now().UTC().Truncate(time.Second),
		Hostname: "host",
		Paths:    []string{"/data"},
		Tree:     crypto.ID{3},
	}
	id, err := r.SaveSnapshot(ctx, snap)
	if err != nil {
		t.Fatalf("SaveSnapshot: %v", err)
	}

	loaded, err := r.LoadSnapshot(ctx, id)
	if err != nil {
		t.Fatalf("LoadSnapshot: %v", err)
	}
	if loaded.Tree != snap.Tree {
		t.Error("snapshot round trip lost the tree reference")
	}

	list, err := r.ListSnapshots(ctx)
	if err != nil {
		t.Fatalf("ListSnapshots: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("ListSnapshots returned %d snapshots, want 1", len(list))
	}

	found, err := r.FindSnapshot(ctx, "latest")
	if err != nil {
		t.Fatalf("FindSnapshot: %v", err)
	}
	if found.ID != id {
		t.Error("FindSnapshot returned the wrong snapshot")
	}
}

// TestIndexCacheAvoidsRefetching checks the local cache actually saves work:
// reopening a repository must not download index objects it already has.
func TestIndexCacheAvoidsRefetching(t *testing.T) {
	be, key := testBackend(t), testKey(t)
	ctx := context.Background()
	opts := testOptions(t)

	r, err := Init(ctx, be, key, opts)
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	if _, _, err := r.SaveBlob(ctx, pack.BlobData, []byte("content")); err != nil {
		t.Fatalf("SaveBlob: %v", err)
	}
	if err := r.Flush(ctx); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	counting := &loadCounter{Backend: be}
	reopened, err := Open(ctx, counting, key, opts)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if reopened.Index().Len() != 1 {
		t.Fatalf("index has %d blobs, want 1", reopened.Index().Len())
	}
	if counting.indexLoads != 0 {
		t.Errorf("reopening downloaded %d index objects despite a warm cache", counting.indexLoads)
	}

	// With caching disabled the object must be fetched.
	noCache := opts
	noCache.NoCache = true
	counting.indexLoads = 0
	if _, err := Open(ctx, counting, key, noCache); err != nil {
		t.Fatalf("Open: %v", err)
	}
	if counting.indexLoads == 0 {
		t.Error("no index object was downloaded with the cache disabled")
	}
}

func TestIndexRebuildMatchesStoredIndex(t *testing.T) {
	r, be, key := initRepo(t)
	ctx := context.Background()

	for i := 0; i < 5; i++ {
		if _, _, err := r.SaveBlob(ctx, pack.BlobData, randomBytes(1000, int64(i))); err != nil {
			t.Fatalf("SaveBlob: %v", err)
		}
	}
	if err := r.Flush(ctx); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	rebuilt, err := index.Rebuild(ctx, be, key)
	if err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	if rebuilt.Len() != r.Index().Len() {
		t.Errorf("rebuilt index has %d blobs, want %d", rebuilt.Len(), r.Index().Len())
	}
}

// orderRecorder records the order in which object types are written.
type orderRecorder struct {
	backend.Backend
	order []backend.FileType
}

func (o *orderRecorder) Save(ctx context.Context, h backend.Handle, rd io.Reader, size int64) error {
	if err := o.Backend.Save(ctx, h, rd, size); err != nil {
		return err
	}
	o.order = append(o.order, h.Type)
	return nil
}

// loadCounter counts index object downloads.
type loadCounter struct {
	backend.Backend
	indexLoads int
}

func (l *loadCounter) Load(ctx context.Context, h backend.Handle, offset, length int64) (io.ReadCloser, error) {
	if h.Type == backend.TypeIndex {
		l.indexLoads++
	}
	return l.Backend.Load(ctx, h, offset, length)
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
