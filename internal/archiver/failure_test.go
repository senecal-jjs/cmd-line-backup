package archiver_test

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/senecal-jjs/cmd-line-backup/internal/archiver"
	"github.com/senecal-jjs/cmd-line-backup/internal/backend"
	"github.com/senecal-jjs/cmd-line-backup/internal/repo"
	"github.com/senecal-jjs/cmd-line-backup/internal/restorer"
)

// failingBackend fails one specific pack upload, simulating a transient storage
// failure partway through a backup while later uploads still succeed.
//
// Failing exactly one pack in the middle is what isolates the dangerous case: if
// every subsequent upload also failed, the final flush would fail too and the
// run would abort for that reason instead. Here the run has every opportunity to
// finish "successfully" with a snapshot that references the lost blobs.
type failingBackend struct {
	backend.Backend
	failPack int // 1-based index of the pack upload to reject
	packs    int
}

func (f *failingBackend) Save(ctx context.Context, h backend.Handle, rd io.Reader, size int64) error {
	if h.Type == backend.TypePack {
		f.packs++
		if f.packs == f.failPack {
			return errors.New("simulated storage failure")
		}
	}
	return f.Backend.Save(ctx, h, rd, size)
}

// TestFailedPackUploadDoesNotProduceABrokenSnapshot is the most important
// safety property of the whole tool.
//
// When a pack upload fails, the blobs it held are gone, but files already added
// to the snapshot tree still reference them. If the run were allowed to
// continue and write a snapshot, that snapshot would name chunks the repository
// does not contain, and the failure would only surface later during a restore.
// The backup must fail loudly instead.
func TestFailedPackUploadDoesNotProduceABrokenSnapshot(t *testing.T) {
	h := newHarness(t)

	// Enough incompressible data to fill several packs, so the failure lands in
	// the middle of the run rather than at the very end.
	for i := 0; i < 8; i++ {
		h.write(filepath.Join("data", string(rune('a'+i))+".bin"), randomBytes(6<<20, int64(i)))
	}

	// Reject the second pack only; everything before and after succeeds.
	failing := &failingBackend{Backend: h.backend(), failPack: 2}
	defer failing.Close()

	r, err := repo.Open(context.Background(), failing, h.key, h.options())
	if err != nil {
		t.Fatalf("repo.Open: %v", err)
	}

	_, err = archiver.New(r, archiver.Options{
		Hostname: "testhost",
		// The usual policy of skipping unreadable files must not swallow a
		// repository write failure.
		OnError: func(string, error) error { return nil },
	}).Snapshot(context.Background(), []string{h.source})

	if err == nil {
		t.Fatal("the backup reported success even though a pack upload failed")
	}

	// No snapshot may have been published.
	snapshots, err := h.open().ListSnapshots(context.Background())
	if err != nil {
		t.Fatalf("ListSnapshots: %v", err)
	}
	if len(snapshots) != 0 {
		t.Fatalf("%d snapshot(s) were written despite the failed upload; "+
			"such a snapshot references chunks the repository does not contain", len(snapshots))
	}
}

// TestRepositoryRefusesSnapshotAfterFailure covers the defence inside the
// repository itself, independently of how the archiver handles errors.
func TestRepositoryRefusesSnapshotAfterFailure(t *testing.T) {
	h := newHarness(t)

	failing := &failingBackend{Backend: h.backend(), failPack: 1}
	defer failing.Close()

	r, err := repo.Open(context.Background(), failing, h.key, h.options())
	if err != nil {
		t.Fatalf("repo.Open: %v", err)
	}

	// Force a pack upload, which the backend will reject.
	if _, _, err := r.SaveBlob(context.Background(), 1, randomBytes(1000, 1)); err != nil {
		t.Logf("SaveBlob: %v", err)
	}
	if err := r.Flush(context.Background()); err == nil {
		t.Fatal("Flush succeeded after a failed pack upload")
	}
	if r.Failed() == nil {
		t.Error("the repository did not record the failure")
	}
}

// TestCancellationAbortsTheBackup checks that a cancelled context stops the run
// rather than being treated as a skippable per-file error.
func TestCancellationAbortsTheBackup(t *testing.T) {
	h := newHarness(t)
	for i := 0; i < 8; i++ {
		h.write(filepath.Join("data", string(rune('a'+i))+".bin"), randomBytes(2<<20, int64(i)))
	}

	ctx, cancel := context.WithCancel(context.Background())
	r := h.open()

	var seen int
	_, err := archiver.New(r, archiver.Options{
		Hostname:    "testhost",
		Concurrency: 1,
		OnError:     func(string, error) error { return nil },
		Progress: func(archiver.Progress) {
			seen++
			if seen == 2 {
				cancel()
			}
		},
	}).Snapshot(ctx, []string{h.source})

	if err == nil {
		t.Fatal("a cancelled backup reported success")
	}
	cancel()
}

// TestRestoreRejectsSymlinkedTargetComponent covers the path-traversal defence:
// a pre-existing symlink inside the restore target must not redirect writes
// outside it.
func TestRestoreRejectsSymlinkedTargetComponent(t *testing.T) {
	h := newHarness(t)
	h.write("docs/secret.txt", []byte("restored content"))
	snap := h.backup()

	target := t.TempDir()
	outside := t.TempDir()

	// Plant a symlink where the restore will want to create a directory.
	planted := filepath.Join(target, h.source, "docs")
	if err := os.MkdirAll(filepath.Dir(planted), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.Symlink(outside, planted); err != nil {
		t.Fatalf("Symlink: %v", err)
	}

	r := h.open()
	if _, err := restorer.New(r, restorer.Options{Overwrite: true}).
		RestoreSnapshot(context.Background(), snap, target); err != nil {
		t.Fatalf("RestoreSnapshot: %v", err)
	}

	// Nothing may have been written through the symlink.
	if _, err := os.Stat(filepath.Join(outside, "secret.txt")); err == nil {
		t.Error("a file was written outside the restore target through a planted symlink")
	}
	// And the real file must be inside the target.
	if _, err := os.Stat(filepath.Join(target, h.source, "docs", "secret.txt")); err != nil {
		t.Errorf("the file was not restored inside the target: %v", err)
	}
}

// TestRestoreReplacesSymlinkAtFilePosition ensures a symlink sitting where a
// file should go is replaced rather than written through.
func TestRestoreReplacesSymlinkAtFilePosition(t *testing.T) {
	h := newHarness(t)
	h.write("note.txt", []byte("restored content"))
	snap := h.backup()

	target := t.TempDir()
	outside := filepath.Join(t.TempDir(), "victim.txt")
	if err := os.WriteFile(outside, []byte("must not change"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	planted := filepath.Join(target, h.source, "note.txt")
	if err := os.MkdirAll(filepath.Dir(planted), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.Symlink(outside, planted); err != nil {
		t.Fatalf("Symlink: %v", err)
	}

	r := h.open()
	if _, err := restorer.New(r, restorer.Options{Overwrite: true}).
		RestoreSnapshot(context.Background(), snap, target); err != nil {
		t.Fatalf("RestoreSnapshot: %v", err)
	}

	victim, err := os.ReadFile(outside)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(victim) != "must not change" {
		t.Error("the restore wrote through a symlink to a file outside the target")
	}

	got, err := os.ReadFile(planted)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(got) != "restored content" {
		t.Errorf("restored content = %q", got)
	}
}

// TestConcurrentProgressCallback exercises the documented contract that the
// progress callback may be invoked from several goroutines at once. Run under
// -race, this catches unsynchronized state in a callback.
func TestConcurrentProgressCallback(t *testing.T) {
	h := newHarness(t)
	for i := 0; i < 60; i++ {
		h.write(filepath.Join("many", string(rune('a'+i%26))+string(rune('a'+i/26))+".bin"),
			randomBytes(64<<10, int64(i)))
	}

	var mu struct {
		count int
	}
	var lock = make(chan struct{}, 1)
	lock <- struct{}{}

	r := h.open()
	_, err := archiver.New(r, archiver.Options{
		Hostname:    "testhost",
		Concurrency: 8,
		Progress: func(archiver.Progress) {
			<-lock
			mu.count++
			lock <- struct{}{}
		},
	}).Snapshot(context.Background(), []string{h.source})
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if mu.count == 0 {
		t.Error("the progress callback was never invoked")
	}
}
