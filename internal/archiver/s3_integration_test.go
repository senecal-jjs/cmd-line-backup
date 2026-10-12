package archiver_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/senecal-jjs/cmd-line-backup/internal/archiver"
	"github.com/senecal-jjs/cmd-line-backup/internal/backend"
	"github.com/senecal-jjs/cmd-line-backup/internal/backend/s3"
	"github.com/senecal-jjs/cmd-line-backup/internal/backend/s3/s3test"
	"github.com/senecal-jjs/cmd-line-backup/internal/crypto"
	"github.com/senecal-jjs/cmd-line-backup/internal/index"
	"github.com/senecal-jjs/cmd-line-backup/internal/repo"
	"github.com/senecal-jjs/cmd-line-backup/internal/restorer"
	"github.com/senecal-jjs/cmd-line-backup/internal/snapshot"
	"github.com/senecal-jjs/cmd-line-backup/internal/verify"
)

// s3Harness is a repository stored in a real S3-compatible server, used to run
// the whole backup and restore pipeline over the network rather than against
// the local backend.
type s3Harness struct {
	*harness
	server s3test.Server
	prefix string
}

func newS3Harness(t *testing.T) *s3Harness {
	t.Helper()
	server := s3test.Require(t)

	key, err := crypto.GenerateKey()
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	h := &s3Harness{
		harness: &harness{t: t, cacheDir: t.TempDir(), key: key, source: t.TempDir()},
		server:  server,
		prefix:  server.NewPrefix(t),
	}

	if _, err := repo.Init(context.Background(), h.s3Backend(), key, h.options()); err != nil {
		t.Fatalf("repo.Init: %v", err)
	}
	return h
}

func (h *s3Harness) s3Backend() *s3.Backend {
	h.t.Helper()
	be, err := s3.Open(context.Background(), h.server.Config(h.prefix))
	if err != nil {
		h.t.Fatalf("s3.Open: %v", err)
	}
	return be
}

// openS3 opens the repository the way a fresh CLI invocation would. With
// coldCache, nothing is served from the local cache, so every index, snapshot,
// and tree has to come from the server.
func (h *s3Harness) openS3(coldCache bool) *repo.Repository {
	h.t.Helper()
	opts := h.options()
	if coldCache {
		opts.NoCache = true
	}
	r, err := repo.Open(context.Background(), h.s3Backend(), h.key, opts)
	if err != nil {
		h.t.Fatalf("repo.Open: %v", err)
	}
	h.t.Cleanup(func() { r.Close() })
	return r
}

func (h *s3Harness) backupS3(ctx context.Context, progress func(archiver.Progress)) (*snapshot.Snapshot, error) {
	h.t.Helper()
	r := h.openS3(false)
	snapshots, err := r.ListSnapshots(ctx)
	if err != nil {
		h.t.Fatalf("ListSnapshots: %v", err)
	}
	opts := archiver.Options{Hostname: "testhost", Progress: progress}
	if len(snapshots) > 0 {
		opts.Parent = snapshots[0]
	}
	return archiver.New(r, opts).Snapshot(ctx, []string{h.source})
}

// writeSample fills the source with enough incompressible data for several
// packs, plus many small files and a nested directory.
func (h *s3Harness) writeSample() {
	for i := 0; i < 3; i++ {
		h.write(filepath.Join("large", fmt.Sprintf("%d.bin", i)), randomBytes(12<<20, int64(i)))
	}
	for i := 0; i < 50; i++ {
		h.write(filepath.Join("small", fmt.Sprintf("%02d", i%5), fmt.Sprintf("f%02d.txt", i)),
			[]byte(strings.Repeat(fmt.Sprintf("line %d\n", i), 100)))
	}
}

func (h *s3Harness) restoreS3(snap *snapshot.Snapshot) string {
	h.t.Helper()
	target := h.t.TempDir()
	if _, err := restorer.New(h.openS3(true), restorer.Options{}).
		RestoreSnapshot(context.Background(), snap, target); err != nil {
		h.t.Fatalf("RestoreSnapshot: %v", err)
	}
	return filepath.Join(target, h.source)
}

// TestIntegrationS3BackupAndRestore runs an initial and an incremental backup
// against S3, then restores both from a cold cache and verifies every pack.
func TestIntegrationS3BackupAndRestore(t *testing.T) {
	h := newS3Harness(t)
	ctx := context.Background()
	h.writeSample()

	first, err := h.backupS3(ctx, nil)
	if err != nil {
		t.Fatalf("first backup: %v", err)
	}
	firstTree := collectTree(t, h.source)

	// Change one small file and add another; everything else is untouched.
	h.write(filepath.Join("small", "00", "f00.txt"), []byte("changed"))
	h.write("added.txt", []byte("new file"))

	second, err := h.backupS3(ctx, nil)
	if err != nil {
		t.Fatalf("second backup: %v", err)
	}
	if second.Stats.FilesUnmodified == 0 {
		t.Error("the incremental backup did not reuse any unchanged files")
	}
	if second.Stats.BytesAdded > 1<<20 {
		t.Errorf("the incremental backup uploaded %d bytes for a two-file change", second.Stats.BytesAdded)
	}

	compareTrees(t, h.source, h.restoreS3(second))

	// The first snapshot must still restore its original content.
	if got := collectTree(t, h.restoreS3(first)); len(got) != len(firstTree) {
		t.Errorf("the first snapshot restored %d entries, want %d", len(got), len(firstTree))
	} else {
		for path, want := range firstTree {
			if !bytes.Equal(got[path].content, want.content) {
				t.Errorf("%s differs in the restore of the first snapshot", path)
			}
		}
	}

	report, err := verify.Run(ctx, h.openS3(true), verify.Options{ReadData: true})
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if !report.OK() {
		t.Errorf("verify found problems: %v", report.Problems)
	}
	if len(report.OrphanPacks) > 0 {
		t.Errorf("verify found %d orphan packs after clean backups", len(report.OrphanPacks))
	}
	if report.PacksInStorage < 2 {
		t.Errorf("only %d packs were stored; the sample should span several", report.PacksInStorage)
	}
}

// TestIntegrationS3IndexRebuild checks that the index can be reconstructed
// purely from the pack headers stored in S3, which depends on ranged reads of
// each pack's tail.
func TestIntegrationS3IndexRebuild(t *testing.T) {
	h := newS3Harness(t)
	ctx := context.Background()
	h.writeSample()
	if _, err := h.backupS3(ctx, nil); err != nil {
		t.Fatalf("backup: %v", err)
	}

	loaded := h.openS3(true).Index()
	rebuilt, err := index.Rebuild(ctx, h.s3Backend(), h.key)
	if err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	if rebuilt.Len() != loaded.Len() {
		t.Fatalf("the rebuilt index holds %d blobs, the stored one %d", rebuilt.Len(), loaded.Len())
	}
	_ = loaded.Each(func(id crypto.ID, want index.Location) error {
		if got, ok := rebuilt.Lookup(id); !ok || got != want {
			t.Errorf("blob %s: rebuilt location %+v, stored %+v", id.Short(), got, want)
		}
		return nil
	})
}

// TestIntegrationS3VerifyDetectsMissingPack deletes a pack from the bucket and
// checks that both the structural and the full check notice.
func TestIntegrationS3VerifyDetectsMissingPack(t *testing.T) {
	h := newS3Harness(t)
	ctx := context.Background()
	h.writeSample()
	if _, err := h.backupS3(ctx, nil); err != nil {
		t.Fatalf("backup: %v", err)
	}

	be := h.s3Backend()
	var victim backend.Handle
	if err := be.List(ctx, backend.TypePack, func(fi backend.FileInfo) error {
		victim = fi.Handle
		return errors.New("stop") // the first pack will do
	}); err == nil || victim.Name == "" {
		t.Fatalf("no pack found to delete (List: %v)", err)
	}
	if err := be.Remove(ctx, victim); err != nil {
		t.Fatalf("Remove: %v", err)
	}

	for _, readData := range []bool{false, true} {
		report, err := verify.Run(ctx, h.openS3(true), verify.Options{ReadData: readData})
		if err != nil {
			t.Fatalf("verify (read data %v): %v", readData, err)
		}
		if report.OK() {
			t.Errorf("verify (read data %v) did not notice the missing pack", readData)
		}
	}
}

// TestIntegrationS3InterruptedBackup cancels a backup partway through and
// checks that it leaves no snapshot and no incomplete multipart uploads, and
// that the repository is still fully usable afterwards.
func TestIntegrationS3InterruptedBackup(t *testing.T) {
	h := newS3Harness(t)
	h.writeSample()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_, err := h.backupS3(ctx, func(archiver.Progress) { cancel() })
	if err == nil {
		t.Fatal("the cancelled backup reported success")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("the cancelled backup returned %v, want a cancellation error", err)
	}

	snapshots, err := h.openS3(true).ListSnapshots(context.Background())
	if err != nil {
		t.Fatalf("ListSnapshots: %v", err)
	}
	if len(snapshots) != 0 {
		t.Errorf("%d snapshot(s) were written by the cancelled backup", len(snapshots))
	}
	if uploads := h.server.IncompleteUploads(t, h.prefix); len(uploads) > 0 {
		t.Errorf("the cancelled backup left incomplete multipart uploads: %v", uploads)
	}

	// A fresh run completes and restores correctly, whatever the interrupted
	// one left behind.
	snap, err := h.backupS3(context.Background(), nil)
	if err != nil {
		t.Fatalf("backup after interruption: %v", err)
	}
	compareTrees(t, h.source, h.restoreS3(snap))
}
