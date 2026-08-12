package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/term"

	"github.com/senecal-jjs/cmd-line-backup/internal/backend"
	"github.com/senecal-jjs/cmd-line-backup/internal/backend/local"
	"github.com/senecal-jjs/cmd-line-backup/internal/backend/s3"
	"github.com/senecal-jjs/cmd-line-backup/internal/crypto"
	"github.com/senecal-jjs/cmd-line-backup/internal/pack"
	"github.com/senecal-jjs/cmd-line-backup/internal/repo"
)

// Environment variables recognized by every command.
const (
	envRepo       = "GOBACKUP_REPO"
	envKeyfile    = "GOBACKUP_KEYFILE"
	envPassphrase = "GOBACKUP_PASSPHRASE"
	envCacheDir   = "GOBACKUP_CACHE_DIR"
)

// globalFlags are the options every command shares.
type globalFlags struct {
	repository  string
	keyfile     string
	region      string
	endpoint    string
	pathStyle   bool
	noCache     bool
	cacheDir    string
	compression string
	maxAttempts int
}

func (g *globalFlags) register(fs *flag.FlagSet) {
	fs.StringVar(&g.repository, "repo", os.Getenv(envRepo),
		"repository location, e.g. s3://bucket/prefix or a local directory (env GOBACKUP_REPO)")
	fs.StringVar(&g.keyfile, "keyfile", os.Getenv(envKeyfile),
		"path to the encryption keyfile (env GOBACKUP_KEYFILE)")
	fs.StringVar(&g.region, "region", os.Getenv("AWS_REGION"), "AWS region")
	fs.StringVar(&g.endpoint, "endpoint", "", "custom S3 endpoint, for S3-compatible services")
	fs.BoolVar(&g.pathStyle, "path-style", false, "use path-style S3 addressing, required by most S3-compatible services")
	fs.BoolVar(&g.noCache, "no-cache", false, "do not use the local metadata cache")
	fs.StringVar(&g.cacheDir, "cache-dir", os.Getenv(envCacheDir), "local cache directory (env GOBACKUP_CACHE_DIR)")
	fs.StringVar(&g.compression, "compression", "zstd", "compression for new data: zstd or none")
	fs.IntVar(&g.maxAttempts, "max-attempts", 0,
		"retries for a failed S3 request before giving up (default: the backend's own default)")
}

// defaultKeyfile returns the conventional keyfile location.
func defaultKeyfile() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return "gobackup.key"
	}
	return filepath.Join(home, ".config", "gobackup", "gobackup.key")
}

func (g *globalFlags) keyfilePath() string {
	if g.keyfile != "" {
		return g.keyfile
	}
	return defaultKeyfile()
}

func (g *globalFlags) compressionMode() (pack.Compression, error) {
	switch strings.ToLower(g.compression) {
	case "", "zstd", "auto":
		return pack.CompressionZstd, nil
	case "none", "off":
		return pack.CompressionNone, nil
	default:
		return 0, fmt.Errorf("unknown compression %q; use zstd or none", g.compression)
	}
}

// openBackend resolves the repository location to a storage backend.
func (g *globalFlags) openBackend(ctx context.Context) (backend.Backend, error) {
	if g.repository == "" {
		return nil, fmt.Errorf("no repository specified; pass -repo or set %s", envRepo)
	}

	if strings.HasPrefix(g.repository, "s3://") {
		cfg, err := s3.ParseURL(g.repository)
		if err != nil {
			return nil, err
		}
		cfg.Region = g.region
		cfg.Endpoint = g.endpoint
		cfg.UsePathStyle = g.pathStyle
		cfg.MaxAttempts = g.maxAttempts
		return s3.Open(ctx, cfg)
	}

	// Anything else is treated as a local directory, which is useful for
	// testing and for repositories on external or network-mounted disks.
	path := strings.TrimPrefix(g.repository, "file://")
	return local.Open(path)
}

func (g *globalFlags) repoOptions() (repo.Options, error) {
	compression, err := g.compressionMode()
	if err != nil {
		return repo.Options{}, err
	}
	return repo.Options{
		CacheDir:    g.cacheDir,
		NoCache:     g.noCache,
		Compression: compression,
	}, nil
}

// openRepository resolves the flags into an open repository.
func (g *globalFlags) openRepository(ctx context.Context, errOut io.Writer) (*repo.Repository, error) {
	// The repository location and other flags are validated before the key is
	// loaded, so an obvious mistake is reported immediately instead of after
	// the user has typed a passphrase.
	be, err := g.openBackend(ctx)
	if err != nil {
		return nil, err
	}

	opts, err := g.repoOptions()
	if err != nil {
		be.Close()
		return nil, err
	}

	key, err := g.loadKey(errOut, false)
	if err != nil {
		be.Close()
		return nil, err
	}

	r, err := repo.Open(ctx, be, key, opts)
	if err != nil {
		be.Close()
		return nil, err
	}
	return r, nil
}

// loadKey reads and unwraps the keyfile.
func (g *globalFlags) loadKey(errOut io.Writer, confirm bool) (*crypto.Key, error) {
	path := g.keyfilePath()
	if _, err := os.Stat(path); err != nil {
		return nil, fmt.Errorf("keyfile %s is not readable: %w", path, err)
	}

	passphrase, err := readPassphrase(errOut, "Enter passphrase: ", confirm)
	if err != nil {
		return nil, err
	}
	return crypto.LoadKeyfile(path, passphrase)
}

// readPassphrase obtains the keyfile passphrase, preferring the environment so
// that scheduled backups can run unattended, and falling back to a terminal
// prompt that does not echo.
func readPassphrase(errOut io.Writer, prompt string, confirm bool) ([]byte, error) {
	if env := os.Getenv(envPassphrase); env != "" {
		return []byte(env), nil
	}

	fd := int(os.Stdin.Fd())
	if !term.IsTerminal(fd) {
		return nil, fmt.Errorf("no passphrase available: set %s or run with a terminal attached", envPassphrase)
	}

	fmt.Fprint(errOut, prompt)
	passphrase, err := term.ReadPassword(fd)
	fmt.Fprintln(errOut)
	if err != nil {
		return nil, fmt.Errorf("reading passphrase: %w", err)
	}
	if len(passphrase) == 0 {
		return nil, errors.New("the passphrase must not be empty")
	}

	if confirm {
		fmt.Fprint(errOut, "Confirm passphrase: ")
		again, err := term.ReadPassword(fd)
		fmt.Fprintln(errOut)
		if err != nil {
			return nil, fmt.Errorf("reading passphrase: %w", err)
		}
		if string(again) != string(passphrase) {
			return nil, errors.New("the passphrases do not match")
		}
	}
	return passphrase, nil
}

// stringList collects a flag that may be repeated.
type stringList []string

func (s *stringList) String() string { return strings.Join(*s, ",") }

func (s *stringList) Set(value string) error {
	*s = append(*s, value)
	return nil
}

// newFlagSet builds a flag set that prints usage to the given writer.
func newFlagSet(name string, out io.Writer, usage string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(out)
	fs.Usage = func() {
		fmt.Fprint(out, usage)
		fmt.Fprintln(out, "\nFlags:")
		fs.PrintDefaults()
	}
	return fs
}
