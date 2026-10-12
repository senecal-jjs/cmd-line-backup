package archiver_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/senecal-jjs/cmd-line-backup/internal/archiver"
	"github.com/senecal-jjs/cmd-line-backup/internal/repo"
	"github.com/senecal-jjs/cmd-line-backup/internal/restorer"
)

// TestRestoreManyFilesWithFewDescriptors guards against holding every restored
// file open at once, which fails as soon as a snapshot has more files than the
// process may open.
func TestRestoreManyFilesWithFewDescriptors(t *testing.T) {
	h := newHarness(t)
	const files = 300
	for i := 0; i < files; i++ {
		h.write(filepath.Join("many", fmt.Sprintf("f%03d.txt", i)), []byte(fmt.Sprintf("file %d", i)))
	}
	snap := h.backup()
	r := h.open()

	var orig syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_NOFILE, &orig); err != nil {
		t.Fatalf("Getrlimit: %v", err)
	}
	low := orig
	low.Cur = 128
	if err := syscall.Setrlimit(syscall.RLIMIT_NOFILE, &low); err != nil {
		t.Skipf("cannot lower the open file limit: %v", err)
	}
	t.Cleanup(func() { syscall.Setrlimit(syscall.RLIMIT_NOFILE, &orig) })

	target := t.TempDir()
	if _, err := restorer.New(r, restorer.Options{}).RestoreSnapshot(context.Background(), snap, target); err != nil {
		t.Fatalf("restoring %d files with a limit of %d open files: %v", files, low.Cur, err)
	}
	syscall.Setrlimit(syscall.RLIMIT_NOFILE, &orig)
	compareTrees(t, h.source, filepath.Join(target, h.source))
}

// TestRestoreDoesNotDeleteWithoutOverwrite checks that a file standing where
// the snapshot has a directory or a symlink is left alone unless -overwrite is
// given.
func TestRestoreDoesNotDeleteWithoutOverwrite(t *testing.T) {
	h := newHarness(t)
	h.write(filepath.Join("sub", "a.txt"), []byte("a"))
	if err := os.Symlink("sub/a.txt", filepath.Join(h.source, "link")); err != nil {
		t.Fatalf("Symlink: %v", err)
	}
	snap := h.backup()

	for _, name := range []string{"sub", "link"} {
		t.Run(name, func(t *testing.T) {
			target := t.TempDir()
			victim := filepath.Join(target, h.source, name)
			if err := os.MkdirAll(filepath.Dir(victim), 0o755); err != nil {
				t.Fatalf("MkdirAll: %v", err)
			}
			if err := os.WriteFile(victim, []byte("user data"), 0o644); err != nil {
				t.Fatalf("WriteFile: %v", err)
			}

			_, err := restorer.New(h.open(), restorer.Options{}).RestoreSnapshot(context.Background(), snap, target)
			if err == nil || !strings.Contains(err.Error(), "overwrite") {
				t.Fatalf("restore over an existing file returned %v, want an error suggesting -overwrite", err)
			}
			if got, err := os.ReadFile(victim); err != nil || string(got) != "user data" {
				t.Fatalf("the existing file was disturbed: %q, %v", got, err)
			}

			if _, err := restorer.New(h.open(), restorer.Options{Overwrite: true}).
				RestoreSnapshot(context.Background(), snap, target); err != nil {
				t.Fatalf("restore with Overwrite: %v", err)
			}
		})
	}
}

// TestRestoreExcludedDirectoryExcludesContents covers a glob that matches a
// directory's name but not the paths of the files inside it.
func TestRestoreExcludedDirectoryExcludesContents(t *testing.T) {
	h := newHarness(t)
	h.write(filepath.Join("build.d", "x.txt"), []byte("x"))
	h.write("keep.txt", []byte("k"))
	snap := h.backup()

	restored := h.restore(snap, func(o *restorer.Options) { o.Exclude = []string{"*.d"} })
	if _, err := os.Lstat(filepath.Join(restored, "build.d")); !os.IsNotExist(err) {
		t.Errorf("the excluded directory or its contents were restored (Lstat: %v)", err)
	}
	if _, err := os.Stat(filepath.Join(restored, "keep.txt")); err != nil {
		t.Errorf("keep.txt was not restored: %v", err)
	}
}

// TestRestoreIncludeCreatesOnlyNeededDirectories checks that an include filter
// does not recreate the snapshot's whole directory skeleton.
func TestRestoreIncludeCreatesOnlyNeededDirectories(t *testing.T) {
	h := newHarness(t)
	h.write(filepath.Join("docs", "a.txt"), []byte("a"))
	h.write(filepath.Join("other", "b.bin"), []byte("b"))
	if err := os.MkdirAll(filepath.Join(h.source, "empty"), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	snap := h.backup()

	restored := h.restore(snap, func(o *restorer.Options) { o.Include = []string{"*.txt"} })
	if _, err := os.Stat(filepath.Join(restored, "docs", "a.txt")); err != nil {
		t.Errorf("the included file was not restored: %v", err)
	}
	for _, dir := range []string{"other", "empty"} {
		if _, err := os.Lstat(filepath.Join(restored, dir)); !os.IsNotExist(err) {
			t.Errorf("directory %s holds nothing included but was created (Lstat: %v)", dir, err)
		}
	}
}

// TestSpecialModeBitsAreRestored covers setuid and sticky bits, which used to
// be dropped because only the permission bits were recorded.
func TestSpecialModeBitsAreRestored(t *testing.T) {
	h := newHarness(t)
	h.write(filepath.Join("shared", "tool"), []byte("#!/bin/sh\n"))
	tool := filepath.Join(h.source, "shared", "tool")
	shared := filepath.Join(h.source, "shared")
	if err := os.Chmod(tool, 0o755|os.ModeSetuid); err != nil {
		t.Fatalf("Chmod: %v", err)
	}
	if err := os.Chmod(shared, 0o777|os.ModeSticky); err != nil {
		t.Fatalf("Chmod: %v", err)
	}
	snap := h.backup()
	restored := h.restore(snap)

	for path, want := range map[string]os.FileMode{
		filepath.Join(restored, "shared", "tool"): os.ModeSetuid,
		filepath.Join(restored, "shared"):         os.ModeSticky,
	} {
		fi, err := os.Stat(path)
		if err != nil {
			t.Fatalf("Stat: %v", err)
		}
		if fi.Mode()&want == 0 {
			t.Errorf("%s has mode %v, missing %v", path, fi.Mode(), want)
		}
	}
}

// TestFailedUploadStopsTheBackup checks that a backup stops uploading once a
// pack upload has failed, rather than processing the rest of the tree for a
// snapshot that can never be written.
func TestFailedUploadStopsTheBackup(t *testing.T) {
	h := newHarness(t)
	// Enough incompressible data for several packs.
	for i := 0; i < 16; i++ {
		h.write(filepath.Join("data", fmt.Sprintf("%02d.bin", i)), randomBytes(4<<20, int64(i)))
	}

	failing := &failingBackend{Backend: h.backend(), failPack: 1}
	defer failing.Close()
	r, err := repo.Open(context.Background(), failing, h.key, h.options())
	if err != nil {
		t.Fatalf("repo.Open: %v", err)
	}

	_, err = archiver.New(r, archiver.Options{Hostname: "testhost", Concurrency: 1}).
		Snapshot(context.Background(), []string{h.source})
	if err == nil {
		t.Fatal("the backup succeeded despite a failed upload")
	}
	if !strings.Contains(err.Error(), "simulated storage failure") {
		t.Errorf("the error does not name the original failure: %v", err)
	}
	if failing.packs != 1 {
		t.Errorf("%d pack uploads were attempted; the backup should have stopped after the first failed", failing.packs)
	}
}
