package archiver_test

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/senecal-jjs/cmd-line-backup/internal/archiver"
	"github.com/senecal-jjs/cmd-line-backup/internal/restorer"
)

// TestBackupAndRestoreRoundTrip is the headline behaviour: everything that goes
// into a snapshot comes back out byte for byte, with its metadata intact.
func TestBackupAndRestoreRoundTrip(t *testing.T) {
	h := newHarness(t)

	h.write("readme.txt", []byte("hello world"))
	h.write("empty.txt", nil)
	h.write("nested/deep/data.bin", randomBytes(3<<20, 1))
	h.write("nested/notes.md", []byte(strings.Repeat("compressible ", 5000)))
	if err := os.Symlink("readme.txt", filepath.Join(h.source, "link")); err != nil {
		t.Fatalf("Symlink: %v", err)
	}
	if err := os.Mkdir(filepath.Join(h.source, "emptydir"), 0o755); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}

	snap := h.backup()
	if snap.Stats.FilesNew != 4 {
		t.Errorf("FilesNew = %d, want 4", snap.Stats.FilesNew)
	}

	restored := h.restore(snap)
	compareTrees(t, h.source, restored)
}

// TestSecondBackupUploadsAlmostNothing is the point of the whole tool: backing
// up unchanged data again must not re-upload it.
func TestSecondBackupUploadsAlmostNothing(t *testing.T) {
	h := newHarness(t)

	h.write("big.bin", randomBytes(8<<20, 7))
	h.write("small.txt", []byte("some text"))

	first := h.backup()
	sizeAfterFirst := h.repoSize()

	second := h.backup()
	sizeAfterSecond := h.repoSize()

	if second.Stats.FilesUnmodified != 2 {
		t.Errorf("FilesUnmodified = %d, want 2; the fast path did not engage", second.Stats.FilesUnmodified)
	}
	if second.Stats.BytesAdded != 0 {
		t.Errorf("BytesAdded = %d, want 0", second.Stats.BytesAdded)
	}
	if second.Parent == nil || *second.Parent != first.ID {
		t.Error("the second snapshot did not record the first as its parent")
	}

	// The only growth should be the new snapshot object and its trees.
	growth := sizeAfterSecond - sizeAfterFirst
	if growth > 64<<10 {
		t.Errorf("the repository grew by %d bytes on an unchanged backup, want under 64 KiB", growth)
	}
}

// TestIncrementalUploadsOnlyTheChange combines the fast path with
// content-defined chunking: editing the middle of a large file must cost the
// edited region, not the whole file.
func TestIncrementalUploadsOnlyTheChange(t *testing.T) {
	h := newHarness(t)

	const size = 16 << 20
	original := randomBytes(size, 3)
	h.write("large.bin", original)
	h.backup()
	sizeAfterFirst := h.repoSize()

	// Insert a small run of bytes in the middle. Fixed-size blocks would shift
	// every subsequent block; content-defined chunking should re-cut only the
	// chunks around the insertion.
	edited := make([]byte, 0, size+1024)
	edited = append(edited, original[:size/2]...)
	edited = append(edited, randomBytes(1024, 99)...)
	edited = append(edited, original[size/2:]...)
	h.write("large.bin", edited)

	second := h.backup()
	growth := h.repoSize() - sizeAfterFirst

	if second.Stats.FilesChanged != 1 {
		t.Errorf("FilesChanged = %d, want 1", second.Stats.FilesChanged)
	}
	if second.Stats.ChunksDuplicate == 0 {
		t.Error("no chunks were recognized as duplicates; deduplication did not work")
	}

	// A whole-file re-upload would be ~16 MB. Allow generous headroom for the
	// re-cut chunks around the edit while still failing loudly if the file was
	// stored again in full.
	if growth > size/4 {
		t.Errorf("editing 1 KiB in a %d byte file grew the repository by %d bytes, want well under %d",
			size, growth, size/4)
	}

	restored := h.restore(second)
	got, err := os.ReadFile(filepath.Join(restored, "large.bin"))
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if !bytes.Equal(got, edited) {
		t.Error("the edited file did not restore correctly")
	}
}

// TestChangedFileIsDetected covers the other half of the fast path: a modified
// file must not be skipped.
func TestChangedFileIsDetected(t *testing.T) {
	h := newHarness(t)

	h.write("file.txt", []byte("original content"))
	h.backup()

	// Sleep past the filesystem's timestamp granularity so mtime really differs.
	time.Sleep(10 * time.Millisecond)
	h.write("file.txt", []byte("completely different content"))

	second := h.backup()
	if second.Stats.FilesChanged != 1 {
		t.Errorf("FilesChanged = %d, want 1", second.Stats.FilesChanged)
	}

	restored := h.restore(second)
	got, err := os.ReadFile(filepath.Join(restored, "file.txt"))
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(got) != "completely different content" {
		t.Errorf("restored content = %q, want the updated content", got)
	}
}

// TestDeduplicationAcrossFiles confirms identical content in different files is
// stored once.
func TestDeduplicationAcrossFiles(t *testing.T) {
	h := newHarness(t)

	content := randomBytes(4<<20, 11)
	h.write("first.bin", content)
	snap := h.backup()
	sizeAfterFirst := h.repoSize()

	h.write("copies/second.bin", content)
	h.write("copies/third.bin", content)
	second := h.backup()

	growth := h.repoSize() - sizeAfterFirst
	if growth > 256<<10 {
		t.Errorf("two duplicate 4 MiB files grew the repository by %d bytes, want a negligible amount", growth)
	}
	if second.Stats.BytesAdded != 0 {
		t.Errorf("BytesAdded = %d, want 0 for duplicate content", second.Stats.BytesAdded)
	}
	_ = snap

	restored := h.restore(second)
	for _, name := range []string{"first.bin", "copies/second.bin", "copies/third.bin"} {
		got, err := os.ReadFile(filepath.Join(restored, name))
		if err != nil {
			t.Fatalf("ReadFile(%s): %v", name, err)
		}
		if !bytes.Equal(got, content) {
			t.Errorf("%s restored incorrectly", name)
		}
	}
}

func TestDeletedFileDisappearsFromNextSnapshot(t *testing.T) {
	h := newHarness(t)

	h.write("keep.txt", []byte("keep"))
	h.write("remove.txt", []byte("remove"))
	h.backup()

	if err := os.Remove(filepath.Join(h.source, "remove.txt")); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	second := h.backup()

	restored := h.restore(second)
	if _, err := os.Stat(filepath.Join(restored, "remove.txt")); err == nil {
		t.Error("a deleted file was still present in the new snapshot")
	}
	if _, err := os.Stat(filepath.Join(restored, "keep.txt")); err != nil {
		t.Errorf("a retained file went missing: %v", err)
	}

	// The old snapshot must still restore the deleted file, since snapshots are
	// independent views.
	snapshots, err := h.open().ListSnapshots(context.Background())
	if err != nil {
		t.Fatalf("ListSnapshots: %v", err)
	}
	oldRestore := h.restore(snapshots[len(snapshots)-1])
	if _, err := os.Stat(filepath.Join(oldRestore, "remove.txt")); err != nil {
		t.Errorf("the earlier snapshot lost a file it contained: %v", err)
	}
}

func TestExcludePatterns(t *testing.T) {
	h := newHarness(t)

	h.write("keep.txt", []byte("keep"))
	h.write("debug.log", []byte("noise"))
	h.write("logs/app.log", []byte("noise"))
	h.write("cache/data.bin", []byte("noise"))

	snap := h.backup(func(o *archiver.Options) {
		o.Excludes = []string{"*.log", "cache"}
	})

	restored := h.restore(snap)
	for _, excluded := range []string{"debug.log", "logs/app.log", "cache/data.bin"} {
		if _, err := os.Stat(filepath.Join(restored, excluded)); err == nil {
			t.Errorf("%s was backed up despite matching an exclusion", excluded)
		}
	}
	if _, err := os.Stat(filepath.Join(restored, "keep.txt")); err != nil {
		t.Errorf("keep.txt was excluded by mistake: %v", err)
	}
}

func TestRestoreIncludeFilter(t *testing.T) {
	h := newHarness(t)

	h.write("docs/a.txt", []byte("a"))
	h.write("docs/b.txt", []byte("b"))
	h.write("images/c.png", []byte("c"))

	snap := h.backup()
	restored := h.restore(snap, func(o *restorer.Options) {
		o.Include = []string{"*.txt"}
	})

	for _, want := range []string{"docs/a.txt", "docs/b.txt"} {
		if _, err := os.Stat(filepath.Join(restored, want)); err != nil {
			t.Errorf("%s was not restored: %v", want, err)
		}
	}
	if _, err := os.Stat(filepath.Join(restored, "images/c.png")); err == nil {
		t.Error("a file outside the include filter was restored")
	}
}

func TestRestoreRefusesToOverwriteByDefault(t *testing.T) {
	h := newHarness(t)
	h.write("file.txt", []byte("content"))
	snap := h.backup()

	target := t.TempDir()
	existing := filepath.Join(target, h.source, "file.txt")
	if err := os.MkdirAll(filepath.Dir(existing), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(existing, []byte("pre-existing"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	r := h.open()
	_, err := restorer.New(r, restorer.Options{}).RestoreSnapshot(context.Background(), snap, target)
	if err == nil {
		t.Fatal("restore silently overwrote an existing file")
	}

	got, err := os.ReadFile(existing)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(got) != "pre-existing" {
		t.Error("the existing file was modified despite the restore failing")
	}

	// With --overwrite it must succeed.
	r2 := h.open()
	if _, err := restorer.New(r2, restorer.Options{Overwrite: true}).RestoreSnapshot(context.Background(), snap, target); err != nil {
		t.Fatalf("restore with Overwrite: %v", err)
	}
	got, err = os.ReadFile(existing)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(got) != "content" {
		t.Errorf("file content = %q, want the restored content", got)
	}
}

func TestHardlinksAreRecreated(t *testing.T) {
	h := newHarness(t)

	original := filepath.Join(h.source, "original.txt")
	if err := os.WriteFile(original, []byte("shared content"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := os.Link(original, filepath.Join(h.source, "hardlink.txt")); err != nil {
		t.Skipf("the filesystem does not support hardlinks: %v", err)
	}

	snap := h.backup()
	restored := h.restore(snap)

	a, err := os.Stat(filepath.Join(restored, "original.txt"))
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	b, err := os.Stat(filepath.Join(restored, "hardlink.txt"))
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if !os.SameFile(a, b) {
		t.Error("hardlinked files were restored as independent copies")
	}
}

func TestUnreadableFilesAreSkipped(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root, which can read anything")
	}
	h := newHarness(t)

	h.write("readable.txt", []byte("fine"))
	secret := filepath.Join(h.source, "secret.txt")
	if err := os.WriteFile(secret, []byte("no access"), 0o000); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	var skipped []string
	snap := h.backup(func(o *archiver.Options) {
		o.OnError = func(path string, err error) error {
			skipped = append(skipped, path)
			return nil // continue
		}
	})

	if len(skipped) == 0 {
		t.Error("no error was reported for the unreadable file")
	}
	restored := h.restore(snap)
	if _, err := os.Stat(filepath.Join(restored, "readable.txt")); err != nil {
		t.Errorf("the readable file was not backed up: %v", err)
	}
}

func TestOnErrorCanAbortTheRun(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root, which can read anything")
	}
	h := newHarness(t)

	secret := filepath.Join(h.source, "secret.txt")
	if err := os.WriteFile(secret, []byte("no access"), 0o000); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	r := h.open()
	sentinel := fmt.Errorf("aborting")
	_, err := archiver.New(r, archiver.Options{
		Hostname: "testhost",
		OnError:  func(string, error) error { return sentinel },
	}).Snapshot(context.Background(), []string{h.source})

	if err == nil {
		t.Fatal("the backup succeeded despite OnError returning an error")
	}
}

func TestEmptyDirectoriesArePreserved(t *testing.T) {
	h := newHarness(t)

	for _, dir := range []string{"empty", "nested/also-empty"} {
		if err := os.MkdirAll(filepath.Join(h.source, dir), 0o755); err != nil {
			t.Fatalf("MkdirAll: %v", err)
		}
	}

	snap := h.backup()
	restored := h.restore(snap)

	for _, dir := range []string{"empty", "nested/also-empty"} {
		info, err := os.Stat(filepath.Join(restored, dir))
		if err != nil {
			t.Errorf("%s was not restored: %v", dir, err)
			continue
		}
		if !info.IsDir() {
			t.Errorf("%s was restored as something other than a directory", dir)
		}
	}
}

func TestManySmallFiles(t *testing.T) {
	h := newHarness(t)

	const count = 500
	for i := 0; i < count; i++ {
		h.write(fmt.Sprintf("dir%02d/file%03d.txt", i%10, i), []byte(fmt.Sprintf("content of file %d", i)))
	}

	snap := h.backup()
	if snap.Stats.FilesNew != count {
		t.Errorf("FilesNew = %d, want %d", snap.Stats.FilesNew, count)
	}

	restored := h.restore(snap)
	compareTrees(t, h.source, restored)
}

func TestBackupRejectsMissingPath(t *testing.T) {
	h := newHarness(t)
	r := h.open()

	_, err := archiver.New(r, archiver.Options{}).Snapshot(
		context.Background(), []string{filepath.Join(h.source, "does-not-exist")})
	if err == nil {
		t.Error("the backup succeeded for a path that does not exist")
	}
}

func TestBackupRejectsNoPaths(t *testing.T) {
	h := newHarness(t)
	r := h.open()

	if _, err := archiver.New(r, archiver.Options{}).Snapshot(context.Background(), nil); err == nil {
		t.Error("the backup succeeded with no paths")
	}
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
