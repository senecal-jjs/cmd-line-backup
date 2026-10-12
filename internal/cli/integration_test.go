package cli_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/senecal-jjs/cmd-line-backup/internal/backend/s3/s3test"
)

// TestIntegrationS3CLIWorkflow drives every command against an s3:// repository
// on a real S3-compatible server, exactly as a user would.
func TestIntegrationS3CLIWorkflow(t *testing.T) {
	server := s3test.Require(t)

	keyfile := filepath.Join(t.TempDir(), "gobackup.key")
	env(t, server.URL(server.NewPrefix(t)), keyfile)
	t.Setenv("AWS_REGION", s3test.Region)
	s3flags := []string{"-endpoint", server.Endpoint, "-path-style"}
	cmd := func(name string, args ...string) (string, error) {
		t.Helper()
		stdout, stderr, err := run(t, append(append([]string{name}, s3flags...), args...)...)
		if err != nil && stderr != "" {
			t.Logf("%s stderr:\n%s", name, stderr)
		}
		return stdout, err
	}

	if out, err := cmd("init"); err != nil {
		t.Fatalf("init: %v", err)
	} else if !strings.Contains(out, "Created repository s3://") {
		t.Errorf("init output = %q", out)
	}
	if _, err := cmd("init"); err == nil {
		t.Error("a second init on the same prefix succeeded")
	}

	source := t.TempDir()
	write(t, source, "readme.txt", []byte("hello"))
	write(t, source, "docs/notes.md", []byte(strings.Repeat("note ", 2000)))
	write(t, source, "data/blob.bin", make([]byte, 3<<20))

	if _, err := cmd("backup", "-quiet", source); err != nil {
		t.Fatalf("first backup: %v", err)
	}
	write(t, source, "readme.txt", []byte("hello again"))
	out, err := cmd("backup", "-quiet", "-tag", "second", source)
	if err != nil {
		t.Fatalf("second backup: %v", err)
	}
	if !strings.Contains(out, "1 changed") {
		t.Errorf("the second backup did not report exactly one changed file:\n%s", out)
	}

	out, err = cmd("list")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if n := strings.Count(out, source); n != 2 {
		t.Errorf("list shows %d snapshots, want 2:\n%s", n, out)
	}

	// Start from an empty cache so the restore reads everything from S3.
	t.Setenv("GOBACKUP_CACHE_DIR", t.TempDir())
	target := t.TempDir()
	if _, err := cmd("restore", "-snapshot", "latest", "-target", target); err != nil {
		t.Fatalf("restore: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(target, source, "readme.txt"))
	if err != nil || string(got) != "hello again" {
		t.Errorf("restored readme.txt = %q, %v; want %q", got, err, "hello again")
	}
	blob, err := os.ReadFile(filepath.Join(target, source, "data", "blob.bin"))
	if err != nil || len(blob) != 3<<20 {
		t.Errorf("restored blob.bin is %d bytes (%v), want %d", len(blob), err, 3<<20)
	}

	// Restoring again without -overwrite must refuse rather than clobber.
	if _, _, err := run(t, append(append([]string{"restore"}, s3flags...), "-target", target)...); err == nil {
		t.Error("a second restore into the same target succeeded without -overwrite")
	}

	out, err = cmd("verify", "-read-data")
	if err != nil {
		t.Fatalf("verify -read-data: %v\n%s", err, out)
	}
	if !strings.Contains(out, "No problems found") {
		t.Errorf("verify output = %q", out)
	}
}
