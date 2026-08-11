package cli_test

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/senecal-jjs/cmd-line-backup/internal/cli"
)

// run invokes the CLI the way main does and returns its output.
func run(t *testing.T, args ...string) (stdout, stderr string, err error) {
	t.Helper()

	var out, errOut bytes.Buffer
	err = cli.Run(args, &out, &errOut)
	return out.String(), errOut.String(), err
}

// env sets the environment a command needs and restores it afterwards.
func env(t *testing.T, repoDir, keyfile string) {
	t.Helper()
	t.Setenv("GOBACKUP_REPO", repoDir)
	t.Setenv("GOBACKUP_KEYFILE", keyfile)
	t.Setenv("GOBACKUP_PASSPHRASE", "test-passphrase")
	t.Setenv("GOBACKUP_CACHE_DIR", t.TempDir())
}

// setup creates an initialized repository and a source tree, returning both.
func setup(t *testing.T) (repoDir, keyfile, source string) {
	t.Helper()

	repoDir = filepath.Join(t.TempDir(), "repo")
	keyfile = filepath.Join(t.TempDir(), "gobackup.key")
	source = t.TempDir()
	env(t, repoDir, keyfile)

	if _, _, err := run(t, "init"); err != nil {
		t.Fatalf("init: %v", err)
	}

	write(t, source, "readme.txt", []byte("hello"))
	write(t, source, "docs/notes.md", []byte(strings.Repeat("note ", 2000)))
	write(t, source, "data/blob.bin", make([]byte, 1<<20))
	return repoDir, keyfile, source
}

func write(t *testing.T, root, name string, content []byte) {
	t.Helper()
	full := filepath.Join(root, name)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(full, content, 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
}

func TestUsage(t *testing.T) {
	stdout, _, err := run(t, "--help")
	if err != nil {
		t.Fatalf("--help returned %v", err)
	}
	for _, want := range []string{"init", "backup", "list", "restore", "verify", "GOBACKUP_REPO"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("usage output does not mention %q", want)
		}
	}
}

func TestNoArgumentsShowsUsage(t *testing.T) {
	_, stderr, err := run(t)
	if err != cli.ErrUsage {
		t.Errorf("got %v, want ErrUsage", err)
	}
	if !strings.Contains(stderr, "Usage:") {
		t.Error("no usage was printed")
	}
}

func TestUnknownCommand(t *testing.T) {
	_, stderr, err := run(t, "frobnicate")
	if err != cli.ErrUsage {
		t.Errorf("got %v, want ErrUsage", err)
	}
	if !strings.Contains(stderr, "unknown command") {
		t.Errorf("stderr does not explain the problem: %q", stderr)
	}
}

func TestVersion(t *testing.T) {
	stdout, _, err := run(t, "version")
	if err != nil {
		t.Fatalf("version: %v", err)
	}
	if !strings.Contains(stdout, "gobackup") {
		t.Errorf("version output = %q", stdout)
	}
}

func TestInitCreatesRepositoryAndKeyfile(t *testing.T) {
	repoDir := filepath.Join(t.TempDir(), "repo")
	keyfile := filepath.Join(t.TempDir(), "gobackup.key")
	env(t, repoDir, keyfile)

	stdout, _, err := run(t, "init")
	if err != nil {
		t.Fatalf("init: %v", err)
	}
	if !strings.Contains(stdout, "Created repository") {
		t.Errorf("init output = %q", stdout)
	}

	if _, err := os.Stat(filepath.Join(repoDir, "config")); err != nil {
		t.Errorf("the config object was not written: %v", err)
	}
	info, err := os.Stat(keyfile)
	if err != nil {
		t.Fatalf("the keyfile was not written: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("keyfile permissions are %v, want 0600", perm)
	}
}

// TestInitRefusesToClobberKeyfile guards the worst failure mode: overwriting the
// key that existing backups depend on.
func TestInitRefusesToClobberKeyfile(t *testing.T) {
	repoDir, keyfile, _ := setup(t)
	original, err := os.ReadFile(keyfile)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}

	// A second init against a new repository but the same keyfile path.
	t.Setenv("GOBACKUP_REPO", filepath.Join(t.TempDir(), "other"))
	if _, _, err := run(t, "init"); err == nil {
		t.Fatal("init overwrote an existing keyfile")
	}

	current, err := os.ReadFile(keyfile)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if !bytes.Equal(original, current) {
		t.Error("the existing keyfile was modified")
	}
	_ = repoDir
}

func TestInitRefusesExistingRepository(t *testing.T) {
	repoDir, _, _ := setup(t)

	// Point at the same repository but a fresh keyfile path.
	t.Setenv("GOBACKUP_KEYFILE", filepath.Join(t.TempDir(), "another.key"))
	t.Setenv("GOBACKUP_REPO", repoDir)

	if _, _, err := run(t, "init"); err == nil {
		t.Error("init succeeded on a repository that already exists")
	}
}

func TestBackupListRestoreVerify(t *testing.T) {
	_, _, source := setup(t)

	stdout, _, err := run(t, "backup", "-quiet", source)
	if err != nil {
		t.Fatalf("backup: %v", err)
	}
	if !strings.Contains(stdout, "Snapshot") {
		t.Errorf("backup output = %q", stdout)
	}

	stdout, _, err = run(t, "list")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if !strings.Contains(stdout, "PATHS") || !strings.Contains(stdout, source) {
		t.Errorf("list output does not show the snapshot: %q", stdout)
	}

	stdout, _, err = run(t, "list", "-snapshot", "latest")
	if err != nil {
		t.Fatalf("list -snapshot: %v", err)
	}
	for _, want := range []string{"readme.txt", "notes.md", "blob.bin"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("file listing does not include %q:\n%s", want, stdout)
		}
	}

	target := t.TempDir()
	stdout, _, err = run(t, "restore", "-snapshot", "latest", "-target", target)
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	if !strings.Contains(stdout, "Restored") {
		t.Errorf("restore output = %q", stdout)
	}

	got, err := os.ReadFile(filepath.Join(target, source, "readme.txt"))
	if err != nil {
		t.Fatalf("reading the restored file: %v", err)
	}
	if string(got) != "hello" {
		t.Errorf("restored content = %q, want %q", got, "hello")
	}

	stdout, _, err = run(t, "verify")
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if !strings.Contains(stdout, "No problems found") {
		t.Errorf("verify output = %q", stdout)
	}

	stdout, _, err = run(t, "verify", "-read-data")
	if err != nil {
		t.Fatalf("verify -read-data: %v", err)
	}
	if !strings.Contains(stdout, "No problems found") {
		t.Errorf("verify -read-data output = %q", stdout)
	}
}

// TestSecondBackupReportsSavings checks the incremental path end to end through
// the command line, which is where a user would notice it.
func TestSecondBackupReportsSavings(t *testing.T) {
	_, _, source := setup(t)

	if _, _, err := run(t, "backup", "-quiet", source); err != nil {
		t.Fatalf("first backup: %v", err)
	}
	stdout, _, err := run(t, "backup", "-quiet", source)
	if err != nil {
		t.Fatalf("second backup: %v", err)
	}

	if !strings.Contains(stdout, "3 unchanged") {
		t.Errorf("the second backup did not report unchanged files:\n%s", stdout)
	}
	if !strings.Contains(stdout, "Uploaded:  0 B") {
		t.Errorf("the second backup uploaded data it should not have:\n%s", stdout)
	}
	if !strings.Contains(stdout, "100.0%") {
		t.Errorf("the savings figure looks wrong:\n%s", stdout)
	}
}

func TestBackupExcludes(t *testing.T) {
	_, _, source := setup(t)
	write(t, source, "debug.log", []byte("noise"))

	if _, _, err := run(t, "backup", "-quiet", "-exclude", "*.log", source); err != nil {
		t.Fatalf("backup: %v", err)
	}

	stdout, _, err := run(t, "list", "-snapshot", "latest")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if strings.Contains(stdout, "debug.log") {
		t.Errorf("an excluded file was backed up:\n%s", stdout)
	}
}

func TestBackupTags(t *testing.T) {
	_, _, source := setup(t)

	if _, _, err := run(t, "backup", "-quiet", "-tag", "nightly", "-tag", "important", source); err != nil {
		t.Fatalf("backup: %v", err)
	}
	stdout, _, err := run(t, "list")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if !strings.Contains(stdout, "nightly") || !strings.Contains(stdout, "important") {
		t.Errorf("tags are missing from the listing:\n%s", stdout)
	}
}

func TestRestoreIncludeFilter(t *testing.T) {
	_, _, source := setup(t)
	if _, _, err := run(t, "backup", "-quiet", source); err != nil {
		t.Fatalf("backup: %v", err)
	}

	target := t.TempDir()
	if _, _, err := run(t, "restore", "-snapshot", "latest", "-target", target, "-include", "*.txt"); err != nil {
		t.Fatalf("restore: %v", err)
	}

	if _, err := os.Stat(filepath.Join(target, source, "readme.txt")); err != nil {
		t.Errorf("an included file was not restored: %v", err)
	}
	if _, err := os.Stat(filepath.Join(target, source, "data/blob.bin")); err == nil {
		t.Error("a file outside the include filter was restored")
	}
}

func TestRestoreRequiresTarget(t *testing.T) {
	_, _, source := setup(t)
	if _, _, err := run(t, "backup", "-quiet", source); err != nil {
		t.Fatalf("backup: %v", err)
	}

	_, stderr, err := run(t, "restore", "-snapshot", "latest")
	if err != cli.ErrUsage {
		t.Errorf("got %v, want ErrUsage", err)
	}
	if !strings.Contains(stderr, "-target is required") {
		t.Errorf("stderr does not explain the problem: %q", stderr)
	}
}

func TestBackupRequiresPaths(t *testing.T) {
	setup(t)

	_, stderr, err := run(t, "backup")
	if err != cli.ErrUsage {
		t.Errorf("got %v, want ErrUsage", err)
	}
	if !strings.Contains(stderr, "path is required") {
		t.Errorf("stderr does not explain the problem: %q", stderr)
	}
}

func TestMissingRepositoryFlag(t *testing.T) {
	t.Setenv("GOBACKUP_REPO", "")
	t.Setenv("GOBACKUP_KEYFILE", filepath.Join(t.TempDir(), "key"))
	t.Setenv("GOBACKUP_PASSPHRASE", "passphrase")

	_, _, err := run(t, "list")
	if err == nil {
		t.Fatal("list succeeded without a repository")
	}
	if !strings.Contains(err.Error(), "no repository specified") {
		t.Errorf("error does not explain the problem: %v", err)
	}
}

func TestWrongPassphraseIsRejected(t *testing.T) {
	_, _, source := setup(t)
	if _, _, err := run(t, "backup", "-quiet", source); err != nil {
		t.Fatalf("backup: %v", err)
	}

	t.Setenv("GOBACKUP_PASSPHRASE", "not the right passphrase")
	_, _, err := run(t, "list")
	if err == nil {
		t.Fatal("the repository opened with the wrong passphrase")
	}
	if !strings.Contains(err.Error(), "passphrase") {
		t.Errorf("error does not mention the passphrase: %v", err)
	}
}

func TestMissingKeyfileIsReported(t *testing.T) {
	repoDir, _, _ := setup(t)
	t.Setenv("GOBACKUP_KEYFILE", filepath.Join(t.TempDir(), "absent.key"))
	t.Setenv("GOBACKUP_REPO", repoDir)

	_, _, err := run(t, "list")
	if err == nil {
		t.Fatal("a command succeeded without a keyfile")
	}
	if !strings.Contains(err.Error(), "keyfile") {
		t.Errorf("error does not mention the keyfile: %v", err)
	}
}

func TestListOnEmptyRepository(t *testing.T) {
	setup(t)

	stdout, _, err := run(t, "list")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if !strings.Contains(stdout, "no snapshots") {
		t.Errorf("list output = %q", stdout)
	}
}

func TestSnapshotPrefixResolution(t *testing.T) {
	_, _, source := setup(t)
	if _, _, err := run(t, "backup", "-quiet", source); err != nil {
		t.Fatalf("backup: %v", err)
	}

	listing, _, err := run(t, "list")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	// The second line holds the snapshot; its first field is the short ID.
	lines := strings.Split(strings.TrimSpace(listing), "\n")
	if len(lines) < 2 {
		t.Fatalf("unexpected listing:\n%s", listing)
	}
	shortID := strings.Fields(lines[1])[0]

	target := t.TempDir()
	if _, _, err := run(t, "restore", "-snapshot", shortID, "-target", target); err != nil {
		t.Fatalf("restore by short ID %q: %v", shortID, err)
	}
}

func TestCompressionNone(t *testing.T) {
	_, _, source := setup(t)

	if _, _, err := run(t, "backup", "-quiet", "-compression", "none", source); err != nil {
		t.Fatalf("backup: %v", err)
	}

	target := t.TempDir()
	if _, _, err := run(t, "restore", "-snapshot", "latest", "-target", target); err != nil {
		t.Fatalf("restore: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(target, source, "readme.txt"))
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(got) != "hello" {
		t.Errorf("restored content = %q", got)
	}
}

func TestInvalidCompressionIsRejected(t *testing.T) {
	_, _, source := setup(t)

	if _, _, err := run(t, "backup", "-quiet", "-compression", "bogus", source); err == nil {
		t.Error("an unknown compression setting was accepted")
	}
}
