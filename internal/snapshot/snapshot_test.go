package snapshot

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

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

func sampleTree() *Tree {
	sub := crypto.ID{9}
	return NewTree([]*Node{
		{
			Name: "notes.txt", Type: NodeFile, Mode: 0o644, Size: 12,
			ModTime: time.Unix(1700000000, 0).UTC(),
			Content: []crypto.ID{{1}, {2}},
		},
		{Name: "subdir", Type: NodeDir, Mode: 0o755, ModTime: time.Unix(1700000001, 0).UTC(), Subtree: &sub},
		{Name: "link", Type: NodeSymlink, Mode: 0o777, ModTime: time.Unix(1700000002, 0).UTC(), LinkTarget: "notes.txt"},
	})
}

func TestTreeRoundTrip(t *testing.T) {
	tree := sampleTree()

	encoded, err := EncodeTree(tree)
	if err != nil {
		t.Fatalf("EncodeTree: %v", err)
	}
	got, err := DecodeTree(encoded)
	if err != nil {
		t.Fatalf("DecodeTree: %v", err)
	}

	if len(got.Nodes) != len(tree.Nodes) {
		t.Fatalf("decoded %d nodes, want %d", len(got.Nodes), len(tree.Nodes))
	}
	for i, want := range tree.Nodes {
		g := got.Nodes[i]
		if g.Name != want.Name || g.Type != want.Type || g.Mode != want.Mode {
			t.Errorf("node %d = %+v, want %+v", i, g, want)
		}
		if !g.ModTime.Equal(want.ModTime) {
			t.Errorf("node %d ModTime = %v, want %v", i, g.ModTime, want.ModTime)
		}
		if len(g.Content) != len(want.Content) {
			t.Errorf("node %d has %d chunks, want %d", i, len(g.Content), len(want.Content))
		}
	}
}

// TestTreeEncodingIsDeterministic is a deduplication requirement, not a
// cosmetic one: an unchanged directory must serialize to identical bytes so its
// tree blob is recognized as already stored. Filesystems return entries in
// arbitrary order, so encoding has to impose its own.
func TestTreeEncodingIsDeterministic(t *testing.T) {
	nodes := sampleTree().Nodes

	forward, err := EncodeTree(&Tree{Nodes: nodes})
	if err != nil {
		t.Fatalf("EncodeTree: %v", err)
	}

	reversed := make([]*Node, len(nodes))
	for i, n := range nodes {
		reversed[len(nodes)-1-i] = n
	}
	backward, err := EncodeTree(&Tree{Nodes: reversed})
	if err != nil {
		t.Fatalf("EncodeTree: %v", err)
	}

	if !bytes.Equal(forward, backward) {
		t.Error("the same directory encoded differently depending on entry order, which would break deduplication")
	}
}

func TestDecodeTreeRejectsMalformedInput(t *testing.T) {
	tests := []struct {
		name string
		data string
	}{
		{"not json", "{"},
		{"null node", `{"nodes":[null]}`},
		{"missing name", `{"nodes":[{"type":"file"}]}`},
		{"unknown type", `{"nodes":[{"name":"x","type":"socket"}]}`},
		{"directory without subtree", `{"nodes":[{"name":"d","type":"dir"}]}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := DecodeTree([]byte(tt.data)); err == nil {
				t.Error("DecodeTree accepted malformed input")
			}
		})
	}
}

func TestTreeFind(t *testing.T) {
	tree := sampleTree()
	if n, ok := tree.Find("notes.txt"); !ok || n.Type != NodeFile {
		t.Error("Find did not return the expected file node")
	}
	if _, ok := tree.Find("absent"); ok {
		t.Error("Find returned a node that does not exist")
	}
}

// TestSameContent covers the decision that makes incremental backup fast: when
// this returns true the file is not read at all, so it must be conservative.
func TestSameContent(t *testing.T) {
	base := func() *Node {
		return &Node{
			Name: "f", Type: NodeFile, Mode: 0o644, Size: 100,
			ModTime:    time.Unix(1700000000, 0).UTC(),
			ChangeTime: time.Unix(1700000000, 0).UTC(),
			Inode:      42,
		}
	}

	tests := []struct {
		name   string
		mutate func(*Node)
		want   bool
	}{
		{"identical", func(*Node) {}, true},
		{"different size", func(n *Node) { n.Size = 101 }, false},
		{"different mtime", func(n *Node) { n.ModTime = n.ModTime.Add(time.Second) }, false},
		{"different mode", func(n *Node) { n.Mode = 0o600 }, false},
		{"different inode", func(n *Node) { n.Inode = 43 }, false},
		{"different ctime", func(n *Node) { n.ChangeTime = n.ChangeTime.Add(time.Second) }, false},
		{"type changed", func(n *Node) { n.Type = NodeSymlink }, false},
		// Repositories written where inode data is unavailable must still work,
		// just with weaker detection.
		{"inode unknown on both sides", func(n *Node) { n.Inode = 0 }, true},
		{"ctime unknown on one side", func(n *Node) { n.ChangeTime = time.Time{} }, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			old, current := base(), base()
			tt.mutate(current)
			if tt.name == "inode unknown on both sides" {
				old.Inode = 0
			}
			if got := SameContent(old, current); got != tt.want {
				t.Errorf("SameContent = %v, want %v", got, tt.want)
			}
		})
	}

	if SameContent(nil, base()) || SameContent(base(), nil) {
		t.Error("SameContent accepted a nil node")
	}
}

// TestChangedContentWithPreservedMtime is the case plain mtime comparison gets
// wrong: an editor that rewrites a file and restores its timestamp. The inode
// and change-time checks are what catch it.
func TestChangedContentWithPreservedMtime(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "file.txt")
	if err := os.WriteFile(path, []byte("original"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	fi, err := os.Lstat(path)
	if err != nil {
		t.Fatalf("Lstat: %v", err)
	}
	before, err := NodeFromFileInfo(path, "file.txt", fi)
	if err != nil {
		t.Fatalf("NodeFromFileInfo: %v", err)
	}

	// Replace the file with different content of the same length, then restore
	// the original modification time.
	if err := os.WriteFile(path, []byte("modified"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := os.Chtimes(path, before.ModTime, before.ModTime); err != nil {
		t.Fatalf("Chtimes: %v", err)
	}

	fi, err = os.Lstat(path)
	if err != nil {
		t.Fatalf("Lstat: %v", err)
	}
	after, err := NodeFromFileInfo(path, "file.txt", fi)
	if err != nil {
		t.Fatalf("NodeFromFileInfo: %v", err)
	}

	if before.Size != after.Size || !before.ModTime.Equal(after.ModTime) {
		t.Skip("the platform did not preserve size and mtime, so there is nothing to test")
	}
	if SameContent(before, after) {
		t.Error("a rewritten file with a restored mtime was treated as unchanged")
	}
}

func TestNodeFromFileInfo(t *testing.T) {
	dir := t.TempDir()

	filePath := filepath.Join(dir, "file.txt")
	if err := os.WriteFile(filePath, []byte("hello"), 0o640); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	subdir := filepath.Join(dir, "subdir")
	if err := os.Mkdir(subdir, 0o750); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	linkPath := filepath.Join(dir, "link")
	if err := os.Symlink("file.txt", linkPath); err != nil {
		t.Fatalf("Symlink: %v", err)
	}

	tests := []struct {
		path     string
		name     string
		wantType NodeType
	}{
		{filePath, "file.txt", NodeFile},
		{subdir, "subdir", NodeDir},
		{linkPath, "link", NodeSymlink},
	}
	for _, tt := range tests {
		fi, err := os.Lstat(tt.path)
		if err != nil {
			t.Fatalf("Lstat: %v", err)
		}
		node, err := NodeFromFileInfo(tt.path, tt.name, fi)
		if err != nil {
			t.Fatalf("NodeFromFileInfo(%s): %v", tt.name, err)
		}
		if node.Type != tt.wantType {
			t.Errorf("%s has type %q, want %q", tt.name, node.Type, tt.wantType)
		}
		if node.Name != tt.name {
			t.Errorf("name = %q, want %q", node.Name, tt.name)
		}
		// Times must be UTC so tree encoding does not depend on the local zone.
		if node.ModTime.Location() != time.UTC {
			t.Errorf("%s ModTime is in %v, want UTC", tt.name, node.ModTime.Location())
		}
	}

	fi, err := os.Lstat(filePath)
	if err != nil {
		t.Fatalf("Lstat: %v", err)
	}
	node, err := NodeFromFileInfo(filePath, "file.txt", fi)
	if err != nil {
		t.Fatalf("NodeFromFileInfo: %v", err)
	}
	if node.Size != 5 {
		t.Errorf("Size = %d, want 5", node.Size)
	}
	if node.Mode.Perm() != 0o640 {
		t.Errorf("Mode = %v, want 0640", node.Mode.Perm())
	}

	fi, err = os.Lstat(linkPath)
	if err != nil {
		t.Fatalf("Lstat: %v", err)
	}
	link, err := NodeFromFileInfo(linkPath, "link", fi)
	if err != nil {
		t.Fatalf("NodeFromFileInfo: %v", err)
	}
	if link.LinkTarget != "file.txt" {
		t.Errorf("LinkTarget = %q, want %q", link.LinkTarget, "file.txt")
	}
}

func TestSnapshotSaveAndLoad(t *testing.T) {
	key := testKey(t)
	be := testBackend(t)
	ctx := context.Background()

	parent := crypto.ID{7}
	snap := &Snapshot{
		Time:     time.Now().UTC().Truncate(time.Second),
		Hostname: "testhost",
		Username: "tester",
		Paths:    []string{"/home/tester"},
		Tags:     []string{"daily"},
		Tree:     crypto.ID{5},
		Parent:   &parent,
		Stats:    Stats{FilesNew: 3, BytesAdded: 1024},
	}

	id, err := Save(ctx, be, key, snap)
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	if snap.ID != id {
		t.Error("Save did not record the assigned ID on the snapshot")
	}

	got, err := Load(ctx, be, key, id, nil)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.Hostname != snap.Hostname || got.Tree != snap.Tree || !got.Time.Equal(snap.Time) {
		t.Errorf("loaded snapshot = %+v, want %+v", got, snap)
	}
	if got.Parent == nil || *got.Parent != parent {
		t.Error("parent reference was not preserved")
	}
	if got.Stats.FilesNew != 3 || got.Stats.BytesAdded != 1024 {
		t.Error("stats were not preserved")
	}
	if got.ID != id {
		t.Error("loaded snapshot has the wrong ID")
	}
}

func TestSnapshotValidation(t *testing.T) {
	ctx := context.Background()
	be, key := testBackend(t), testKey(t)

	tests := []struct {
		name string
		snap *Snapshot
	}{
		{"no timestamp", &Snapshot{Paths: []string{"/x"}, Tree: crypto.ID{1}}},
		{"no paths", &Snapshot{Time: time.Now(), Tree: crypto.ID{1}}},
		{"no tree", &Snapshot{Time: time.Now(), Paths: []string{"/x"}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := Save(ctx, be, key, tt.snap); err == nil {
				t.Error("Save accepted an invalid snapshot")
			}
		})
	}
}

func TestListIsNewestFirst(t *testing.T) {
	key := testKey(t)
	be := testBackend(t)
	ctx := context.Background()

	base := time.Now().UTC().Truncate(time.Second)
	for i := 0; i < 3; i++ {
		snap := &Snapshot{
			Time:  base.Add(time.Duration(i) * time.Hour),
			Paths: []string{"/data"},
			Tree:  crypto.ID{byte(i + 1)},
		}
		if _, err := Save(ctx, be, key, snap); err != nil {
			t.Fatalf("Save: %v", err)
		}
	}

	snapshots, err := List(ctx, be, key, nil)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(snapshots) != 3 {
		t.Fatalf("List returned %d snapshots, want 3", len(snapshots))
	}
	for i := 1; i < len(snapshots); i++ {
		if snapshots[i-1].Time.Before(snapshots[i].Time) {
			t.Error("List did not return snapshots newest first")
		}
	}
}

func TestFind(t *testing.T) {
	key := testKey(t)
	be := testBackend(t)
	ctx := context.Background()

	base := time.Now().UTC().Truncate(time.Second)
	var ids []crypto.ID
	for i := 0; i < 3; i++ {
		snap := &Snapshot{
			Time:  base.Add(time.Duration(i) * time.Hour),
			Paths: []string{"/data"},
			Tree:  crypto.ID{byte(i + 1)},
		}
		id, err := Save(ctx, be, key, snap)
		if err != nil {
			t.Fatalf("Save: %v", err)
		}
		ids = append(ids, id)
	}

	newest := ids[2]
	for _, ref := range []string{"", "latest"} {
		got, err := Find(ctx, be, key, ref, nil)
		if err != nil {
			t.Fatalf("Find(%q): %v", ref, err)
		}
		if got.ID != newest {
			t.Errorf("Find(%q) returned %s, want the newest %s", ref, got.ID.Short(), newest.Short())
		}
	}

	// Full ID and a prefix must both resolve.
	target := ids[0]
	for _, ref := range []string{target.String(), target.String()[:8]} {
		got, err := Find(ctx, be, key, ref, nil)
		if err != nil {
			t.Fatalf("Find(%q): %v", ref, err)
		}
		if got.ID != target {
			t.Errorf("Find(%q) returned %s, want %s", ref, got.ID.Short(), target.Short())
		}
	}

	if _, err := Find(ctx, be, key, "ffffffffffffffff", nil); !errors.Is(err, ErrNotFound) {
		t.Errorf("got %v, want ErrNotFound", err)
	}
}

func TestFindOnEmptyRepository(t *testing.T) {
	if _, err := Find(context.Background(), testBackend(t), testKey(t), "latest", nil); !errors.Is(err, ErrNotFound) {
		t.Errorf("got %v, want ErrNotFound", err)
	}
}

func TestFindParent(t *testing.T) {
	base := time.Now().UTC()
	snapshots := []*Snapshot{
		{ID: crypto.ID{3}, Time: base.Add(2 * time.Hour), Hostname: "hostA", Paths: []string{"/data"}},
		{ID: crypto.ID{2}, Time: base.Add(time.Hour), Hostname: "hostB", Paths: []string{"/data"}},
		{ID: crypto.ID{1}, Time: base, Hostname: "hostA", Paths: []string{"/other"}},
	}

	if got := FindParent(snapshots, "hostA", []string{"/data"}); got == nil || got.ID != (crypto.ID{3}) {
		t.Errorf("FindParent returned %v, want the hostA /data snapshot", got)
	}
	// Path order must not matter.
	multi := []*Snapshot{{ID: crypto.ID{9}, Hostname: "h", Paths: []string{"/b", "/a"}}}
	if got := FindParent(multi, "h", []string{"/a", "/b"}); got == nil {
		t.Error("FindParent failed to match paths listed in a different order")
	}
	if got := FindParent(snapshots, "hostC", []string{"/data"}); got != nil {
		t.Error("FindParent matched a snapshot from a different host")
	}
	if got := FindParent(snapshots, "hostA", []string{"/nowhere"}); got != nil {
		t.Error("FindParent matched a snapshot covering different paths")
	}
}

func TestSnapshotIsUnreadableWithTheWrongKey(t *testing.T) {
	key, other := testKey(t), testKey(t)
	be := testBackend(t)
	ctx := context.Background()

	id, err := Save(ctx, be, key, &Snapshot{
		Time: time.Now().UTC(), Paths: []string{"/x"}, Tree: crypto.ID{1},
	})
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	if _, err := Load(ctx, be, other, id, nil); err == nil {
		t.Error("a snapshot was readable with the wrong key")
	}
}

func TestRestoreMetadata(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "file.txt")
	if err := os.WriteFile(path, []byte("content"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	want := time.Unix(1600000000, 0).UTC()
	node := &Node{Name: "file.txt", Type: NodeFile, Mode: 0o640, ModTime: want}
	if err := RestoreMetadata(path, node); err != nil {
		t.Fatalf("RestoreMetadata: %v", err)
	}

	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if fi.Mode().Perm() != 0o640 {
		t.Errorf("permissions = %v, want 0640", fi.Mode().Perm())
	}
	if !fi.ModTime().Equal(want) {
		t.Errorf("ModTime = %v, want %v", fi.ModTime(), want)
	}
}

// TestRestoreMetadataSkipsSymlinks guards against following the link and
// changing the target's metadata instead.
func TestRestoreMetadataSkipsSymlinks(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	if err := os.WriteFile(target, []byte("content"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink("target", link); err != nil {
		t.Fatalf("Symlink: %v", err)
	}

	node := &Node{Name: "link", Type: NodeSymlink, Mode: 0o777, ModTime: time.Unix(1600000000, 0).UTC()}
	if err := RestoreMetadata(link, node); err != nil {
		t.Fatalf("RestoreMetadata: %v", err)
	}

	fi, err := os.Stat(target)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("the symlink target's permissions changed to %v", fi.Mode().Perm())
	}
}

// TestTreesDoNotRecordAccessTime guards deduplication of unchanged trees: the
// access time changes whenever anything reads a file, so recording it would
// make a tree differ between runs even though nothing in it was modified.
func TestTreesDoNotRecordAccessTime(t *testing.T) {
	path := filepath.Join(t.TempDir(), "file.txt")
	if err := os.WriteFile(path, []byte("content"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	fi, err := os.Lstat(path)
	if err != nil {
		t.Fatalf("Lstat: %v", err)
	}
	node, err := NodeFromFileInfo(path, "file.txt", fi)
	if err != nil {
		t.Fatalf("NodeFromFileInfo: %v", err)
	}
	data, err := EncodeTree(NewTree([]*Node{node}))
	if err != nil {
		t.Fatalf("EncodeTree: %v", err)
	}
	if bytes.Contains(data, []byte(`"atime"`)) {
		t.Errorf("the encoded tree records an access time: %s", data)
	}

	// Trees written by earlier versions still carry the field and must decode.
	legacy := []byte(`{"nodes":[{"name":"a","type":"file","mode":420,"mtime":"2024-01-01T00:00:00Z","atime":"2024-01-02T00:00:00Z"}]}`)
	if _, err := DecodeTree(legacy); err != nil {
		t.Errorf("a tree with an atime field no longer decodes: %v", err)
	}
}

// mapCache is an in-memory ObjectCache.
type mapCache map[crypto.ID][]byte

func (m mapCache) Get(id crypto.ID) ([]byte, bool) { d, ok := m[id]; return d, ok }
func (m mapCache) Put(id crypto.ID, data []byte) error {
	m[id] = append([]byte(nil), data...)
	return nil
}

// TestSnapshotCache checks that loaded snapshots are cached, that the cached
// copy is used, and that a corrupt cached copy is ignored.
func TestSnapshotCache(t *testing.T) {
	ctx := context.Background()
	be, key := testBackend(t), testKey(t)
	snap := &Snapshot{Time: time.Now().UTC(), Paths: []string{"/data"}, Tree: crypto.ID{1}}
	id, err := Save(ctx, be, key, snap)
	if err != nil {
		t.Fatalf("Save: %v", err)
	}

	cache := mapCache{}
	if _, err := List(ctx, be, key, cache); err != nil {
		t.Fatalf("List: %v", err)
	}
	if _, ok := cache[id]; !ok {
		t.Fatal("listing did not cache the snapshot")
	}

	// With the object gone from storage, only the cache can supply it.
	if err := be.Remove(ctx, backend.Handle{Type: backend.TypeSnapshot, Name: id.String()}); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if got, err := Load(ctx, be, key, id, cache); err != nil || got.ID != id {
		t.Fatalf("Load from cache = %v, %v", got, err)
	}

	// A corrupt cached copy must not be trusted.
	cache[id][len(cache[id])-1] ^= 0xFF
	if _, err := Load(ctx, be, key, id, cache); err == nil {
		t.Error("a corrupt cached snapshot was accepted")
	}
}
