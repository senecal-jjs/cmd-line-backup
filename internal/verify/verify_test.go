package verify_test

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/senecal-jjs/cmd-line-backup/internal/archiver"
	"github.com/senecal-jjs/cmd-line-backup/internal/backend"
	"github.com/senecal-jjs/cmd-line-backup/internal/backend/local"
	"github.com/senecal-jjs/cmd-line-backup/internal/crypto"
	"github.com/senecal-jjs/cmd-line-backup/internal/pack"
	"github.com/senecal-jjs/cmd-line-backup/internal/repo"
	"github.com/senecal-jjs/cmd-line-backup/internal/verify"
)

type fixture struct {
	t       *testing.T
	repoDir string
	cache   string
	key     *crypto.Key
	source  string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()

	key, err := crypto.GenerateKey()
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	f := &fixture{t: t, repoDir: t.TempDir(), cache: t.TempDir(), key: key, source: t.TempDir()}

	be := f.backend()
	defer be.Close()
	if _, err := repo.Init(context.Background(), be, key, f.options()); err != nil {
		t.Fatalf("repo.Init: %v", err)
	}
	return f
}

func (f *fixture) backend() backend.Backend {
	f.t.Helper()
	be, err := local.Open(f.repoDir)
	if err != nil {
		f.t.Fatalf("local.Open: %v", err)
	}
	return be
}

func (f *fixture) options() repo.Options {
	return repo.Options{CacheDir: f.cache, Compression: pack.CompressionZstd}
}

func (f *fixture) open() *repo.Repository {
	f.t.Helper()
	r, err := repo.Open(context.Background(), f.backend(), f.key, f.options())
	if err != nil {
		f.t.Fatalf("repo.Open: %v", err)
	}
	f.t.Cleanup(func() { r.Close() })
	return r
}

// backup populates the repository with a small tree.
func (f *fixture) backup() {
	f.t.Helper()

	for name, content := range map[string][]byte{
		"a.txt":     []byte("first file"),
		"b.txt":     bytes.Repeat([]byte("second "), 1000),
		"sub/c.bin": randomBytes(2 << 20),
		"sub/d.txt": []byte("fourth"),
	} {
		full := filepath.Join(f.source, name)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			f.t.Fatalf("MkdirAll: %v", err)
		}
		if err := os.WriteFile(full, content, 0o644); err != nil {
			f.t.Fatalf("WriteFile: %v", err)
		}
	}

	r := f.open()
	if _, err := archiver.New(r, archiver.Options{Hostname: "testhost"}).
		Snapshot(context.Background(), []string{f.source}); err != nil {
		f.t.Fatalf("Snapshot: %v", err)
	}
}

// packFiles returns the paths of the pack objects on disk.
func (f *fixture) packFiles() []string {
	f.t.Helper()

	var out []string
	err := filepath.Walk(filepath.Join(f.repoDir, "packs"), func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() {
			out = append(out, p)
		}
		return nil
	})
	if err != nil {
		f.t.Fatalf("walking packs: %v", err)
	}
	return out
}

func TestHealthyRepositoryPasses(t *testing.T) {
	f := newFixture(t)
	f.backup()

	report, err := verify.Run(context.Background(), f.open(), verify.Options{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !report.OK() {
		t.Errorf("a healthy repository reported problems: %v", report.Problems)
	}
	if report.SnapshotsChecked != 1 {
		t.Errorf("SnapshotsChecked = %d, want 1", report.SnapshotsChecked)
	}
	if report.BlobsReferenced == 0 {
		t.Error("no chunk references were checked")
	}
	if report.PacksInStorage == 0 {
		t.Error("no packs were found")
	}
	if len(report.OrphanPacks) != 0 {
		t.Errorf("a fresh repository reported %d orphan packs", len(report.OrphanPacks))
	}
}

func TestReadDataPassesOnHealthyRepository(t *testing.T) {
	f := newFixture(t)
	f.backup()

	report, err := verify.Run(context.Background(), f.open(), verify.Options{ReadData: true})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !report.OK() {
		t.Errorf("a healthy repository failed the full check: %v", report.Problems)
	}
	if report.BytesRead == 0 {
		t.Error("BytesRead = 0; no data was actually read")
	}
}

// TestMissingPackIsDetected covers the failure the cheap check exists for: a
// pack that never finished uploading, or was deleted.
func TestMissingPackIsDetected(t *testing.T) {
	f := newFixture(t)
	f.backup()

	packs := f.packFiles()
	if len(packs) == 0 {
		t.Fatal("no packs were written")
	}
	if err := os.Remove(packs[0]); err != nil {
		t.Fatalf("Remove: %v", err)
	}

	report, err := verify.Run(context.Background(), f.open(), verify.Options{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if report.OK() {
		t.Fatal("a missing pack went undetected by the metadata check")
	}
	if !containsSubstring(report.Problems, "missing") {
		t.Errorf("problems do not mention the missing pack: %v", report.Problems)
	}
}

// TestTruncatedPackIsDetected covers a partially written object, which the
// metadata check catches because the index says the pack should be longer.
func TestTruncatedPackIsDetected(t *testing.T) {
	f := newFixture(t)
	f.backup()

	packs := f.packFiles()
	info, err := os.Stat(packs[0])
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if err := os.Truncate(packs[0], info.Size()/2); err != nil {
		t.Fatalf("Truncate: %v", err)
	}

	report, err := verify.Run(context.Background(), f.open(), verify.Options{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if report.OK() {
		t.Error("a truncated pack went undetected")
	}
}

// TestSilentCorruptionNeedsReadData documents the boundary between the two
// checks: flipping a bit does not change any size, so only the full check can
// find it. This is precisely why -read-data exists.
func TestSilentCorruptionNeedsReadData(t *testing.T) {
	f := newFixture(t)
	f.backup()

	packs := f.packFiles()
	data, err := os.ReadFile(packs[0])
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	data[len(data)/2] ^= 0xFF
	if err := os.WriteFile(packs[0], data, 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	cheap, err := verify.Run(context.Background(), f.open(), verify.Options{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !cheap.OK() {
		t.Log("the metadata check happened to notice the corruption, which is fine")
	}

	full, err := verify.Run(context.Background(), f.open(), verify.Options{ReadData: true})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if full.OK() {
		t.Error("bit-level corruption survived the full check")
	}
}

// TestOrphanPacksAreReported covers the residue of an interrupted backup:
// harmless, but the user should know it is there.
func TestOrphanPacksAreReported(t *testing.T) {
	f := newFixture(t)
	f.backup()

	// Write a pack that no index refers to, exactly as a crashed run would.
	w, err := pack.NewWriter(f.key, pack.CompressionNone)
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	content := []byte("data from an interrupted run")
	if _, err := w.Add(pack.BlobData, f.key.ChunkID(content), content); err != nil {
		t.Fatalf("Add: %v", err)
	}
	orphan, err := w.Finish()
	if err != nil {
		t.Fatalf("Finish: %v", err)
	}

	be := f.backend()
	defer be.Close()
	if err := be.Save(context.Background(),
		backend.Handle{Type: backend.TypePack, Name: orphan.ID.String()},
		bytes.NewReader(orphan.Data), int64(len(orphan.Data))); err != nil {
		t.Fatalf("Save: %v", err)
	}

	report, err := verify.Run(context.Background(), f.open(), verify.Options{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(report.OrphanPacks) != 1 || report.OrphanPacks[0] != orphan.ID {
		t.Errorf("OrphanPacks = %v, want the one orphan %s", report.OrphanPacks, orphan.ID.Short())
	}
	// An orphan is not a problem, just wasted space.
	if !report.OK() {
		t.Errorf("an orphan pack was reported as a problem: %v", report.Problems)
	}
}

func TestEmptyRepositoryPasses(t *testing.T) {
	f := newFixture(t)

	report, err := verify.Run(context.Background(), f.open(), verify.Options{ReadData: true})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !report.OK() {
		t.Errorf("an empty repository reported problems: %v", report.Problems)
	}
	if report.SnapshotsChecked != 0 {
		t.Errorf("SnapshotsChecked = %d, want 0", report.SnapshotsChecked)
	}
}

func containsSubstring(items []string, want string) bool {
	for _, s := range items {
		if strings.Contains(s, want) {
			return true
		}
	}
	return false
}

func randomBytes(n int) []byte {
	out := make([]byte, n)
	state := uint64(12345)
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
