package archiver_test

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/senecal-jjs/cmd-line-backup/internal/archiver"
	"github.com/senecal-jjs/cmd-line-backup/internal/backend"
	"github.com/senecal-jjs/cmd-line-backup/internal/backend/local"
	"github.com/senecal-jjs/cmd-line-backup/internal/crypto"
	"github.com/senecal-jjs/cmd-line-backup/internal/pack"
	"github.com/senecal-jjs/cmd-line-backup/internal/repo"
	"github.com/senecal-jjs/cmd-line-backup/internal/restorer"
	"github.com/senecal-jjs/cmd-line-backup/internal/snapshot"
)

// harness bundles a repository and the directories a test works with.
type harness struct {
	t        *testing.T
	repoDir  string
	cacheDir string
	key      *crypto.Key
	source   string
}

func newHarness(t *testing.T) *harness {
	t.Helper()

	key, err := crypto.GenerateKey()
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}

	h := &harness{
		t:        t,
		repoDir:  t.TempDir(),
		cacheDir: t.TempDir(),
		key:      key,
		source:   t.TempDir(),
	}

	be := h.backend()
	defer be.Close()
	if _, err := repo.Init(context.Background(), be, key, h.options()); err != nil {
		t.Fatalf("repo.Init: %v", err)
	}
	return h
}

func (h *harness) backend() backend.Backend {
	h.t.Helper()
	be, err := local.Open(h.repoDir)
	if err != nil {
		h.t.Fatalf("local.Open: %v", err)
	}
	return be
}

func (h *harness) options() repo.Options {
	return repo.Options{CacheDir: h.cacheDir, Compression: pack.CompressionZstd}
}

// open returns a freshly opened repository, mimicking a separate CLI invocation.
func (h *harness) open() *repo.Repository {
	h.t.Helper()
	r, err := repo.Open(context.Background(), h.backend(), h.key, h.options())
	if err != nil {
		h.t.Fatalf("repo.Open: %v", err)
	}
	h.t.Cleanup(func() { r.Close() })
	return r
}

// backup runs a backup of the source directory, using the latest snapshot as
// the parent when one exists.
func (h *harness) backup(opts ...func(*archiver.Options)) *snapshot.Snapshot {
	h.t.Helper()
	ctx := context.Background()
	r := h.open()

	snapshots, err := r.ListSnapshots(ctx)
	if err != nil {
		h.t.Fatalf("ListSnapshots: %v", err)
	}

	options := archiver.Options{Hostname: "testhost"}
	if len(snapshots) > 0 {
		options.Parent = snapshots[0]
	}
	for _, fn := range opts {
		fn(&options)
	}

	snap, err := archiver.New(r, options).Snapshot(ctx, []string{h.source})
	if err != nil {
		h.t.Fatalf("Snapshot: %v", err)
	}
	return snap
}

// restore extracts a snapshot into a new directory and returns its path.
func (h *harness) restore(snap *snapshot.Snapshot, opts ...func(*restorer.Options)) string {
	h.t.Helper()
	target := h.t.TempDir()
	r := h.open()

	options := restorer.Options{}
	for _, fn := range opts {
		fn(&options)
	}

	if _, err := restorer.New(r, options).RestoreSnapshot(context.Background(), snap, target); err != nil {
		h.t.Fatalf("RestoreSnapshot: %v", err)
	}
	// The snapshot reproduces absolute paths, so the source tree appears under
	// the target at its original location.
	return filepath.Join(target, h.source)
}

func (h *harness) write(relPath string, content []byte) {
	h.t.Helper()
	full := filepath.Join(h.source, relPath)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		h.t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(full, content, 0o644); err != nil {
		h.t.Fatalf("WriteFile: %v", err)
	}
}

// repoSize returns the total number of bytes stored in the repository, which is
// how these tests confirm that an incremental backup really did upload less.
func (h *harness) repoSize() int64 {
	h.t.Helper()
	var total int64
	err := filepath.Walk(h.repoDir, func(_ string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() {
			total += info.Size()
		}
		return nil
	})
	if err != nil {
		h.t.Fatalf("walking repository: %v", err)
	}
	return total
}

// compareTrees asserts that two directory trees have identical structure and
// content.
func compareTrees(t *testing.T, want, got string) {
	t.Helper()

	wantFiles := collectTree(t, want)
	gotFiles := collectTree(t, got)

	for path, wantInfo := range wantFiles {
		gotInfo, ok := gotFiles[path]
		if !ok {
			t.Errorf("%s is missing from the restore", path)
			continue
		}
		if wantInfo.kind != gotInfo.kind {
			t.Errorf("%s: restored as %s, want %s", path, gotInfo.kind, wantInfo.kind)
			continue
		}
		if wantInfo.kind == "file" {
			if !bytes.Equal(wantInfo.content, gotInfo.content) {
				t.Errorf("%s: content differs (%d bytes restored, %d expected)",
					path, len(gotInfo.content), len(wantInfo.content))
			}
			if wantInfo.mode != gotInfo.mode {
				t.Errorf("%s: mode %v, want %v", path, gotInfo.mode, wantInfo.mode)
			}
			if !wantInfo.modTime.Equal(gotInfo.modTime) {
				t.Errorf("%s: mtime %v, want %v", path, gotInfo.modTime, wantInfo.modTime)
			}
		}
		if wantInfo.kind == "symlink" && wantInfo.link != gotInfo.link {
			t.Errorf("%s: symlink points at %q, want %q", path, gotInfo.link, wantInfo.link)
		}
	}

	for path := range gotFiles {
		if _, ok := wantFiles[path]; !ok {
			t.Errorf("%s appeared in the restore but is not in the source", path)
		}
	}
}

type fileSnapshot struct {
	kind    string
	content []byte
	mode    os.FileMode
	modTime time.Time
	link    string
}

func collectTree(t *testing.T, root string) map[string]fileSnapshot {
	t.Helper()

	out := make(map[string]fileSnapshot)
	err := filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}

		fs := fileSnapshot{mode: info.Mode().Perm(), modTime: info.ModTime()}
		switch {
		case info.Mode()&os.ModeSymlink != 0:
			fs.kind = "symlink"
			fs.link, err = os.Readlink(p)
			if err != nil {
				return err
			}
		case info.IsDir():
			fs.kind = "dir"
		default:
			fs.kind = "file"
			fs.content, err = os.ReadFile(p)
			if err != nil {
				return err
			}
		}
		out[rel] = fs
		return nil
	})
	if err != nil {
		t.Fatalf("walking %s: %v", root, err)
	}
	return out
}
