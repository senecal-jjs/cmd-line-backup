// Package repo ties the storage layers together into a repository: the
// configuration object, the chunk index, the pack writer, and the local cache.
//
// It is the interface the backup, restore, and verify commands work against.
// Callers save and load blobs by content; the repository decides which pack a
// blob lands in, when a pack is full, and how the index is updated.
package repo

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/senecal-jjs/cmd-line-backup/internal/backend"
	"github.com/senecal-jjs/cmd-line-backup/internal/cache"
	"github.com/senecal-jjs/cmd-line-backup/internal/chunker"
	"github.com/senecal-jjs/cmd-line-backup/internal/crypto"
	"github.com/senecal-jjs/cmd-line-backup/internal/index"
	"github.com/senecal-jjs/cmd-line-backup/internal/pack"
)

// configVersion is the repository format version.
const configVersion = 1

// Errors returned when opening or creating repositories.
var (
	ErrAlreadyInitialized = errors.New("repo: this location already holds a repository")
	ErrNotInitialized     = errors.New("repo: no repository found at this location")
)

// Config is the repository's configuration object.
//
// It records the chunker parameters because they determine chunk boundaries:
// backing up to an existing repository with different settings would produce
// entirely different chunks and silently stop deduplicating against everything
// already stored.
type Config struct {
	Version     int           `json:"version"`
	ID          crypto.ID     `json:"id"`
	Created     time.Time     `json:"created"`
	Chunker     ChunkerConfig `json:"chunker"`
	Compression string        `json:"compression"`
}

// ChunkerConfig mirrors chunker.Config in serializable form.
type ChunkerConfig struct {
	MinSize int `json:"min_size"`
	AvgSize int `json:"avg_size"`
	MaxSize int `json:"max_size"`
}

func (c ChunkerConfig) toChunker() chunker.Config {
	return chunker.Config{MinSize: c.MinSize, AvgSize: c.AvgSize, MaxSize: c.MaxSize}
}

func fromChunker(c chunker.Config) ChunkerConfig {
	return ChunkerConfig{MinSize: c.MinSize, AvgSize: c.AvgSize, MaxSize: c.MaxSize}
}

const configAADLabel = "gobackup v1 config"

// Options control how a repository is opened.
type Options struct {
	// CacheDir is the base directory for the local cache. Empty selects the
	// platform default; use NoCache to disable caching entirely.
	CacheDir string
	NoCache  bool

	// Compression selects the algorithm for new blobs. It applies only to data
	// written from now on, since each blob records how it was compressed.
	Compression pack.Compression

	// Chunker overrides the chunk size parameters when initializing a new
	// repository. Ignored when opening an existing one, whose stored settings
	// always win.
	Chunker chunker.Config
}

// A Repository is an open gobackup repository.
type Repository struct {
	be    backend.Backend
	key   *crypto.Key
	cfg   Config
	idx   *index.Index
	cache *cache.Cache

	compression pack.Compression
	chunkerCfg  chunker.Config

	// mu guards the open pack, the list of packs written during this run, and
	// the sticky failure below.
	mu       sync.Mutex
	writer   *pack.Writer
	newPacks []index.PackEntries

	// failed records the first write failure of this session. Once a pack
	// upload fails, blobs that earlier files already reference are missing from
	// the repository, so no snapshot describing this run can be trusted. The
	// failure is therefore sticky and blocks Flush and SaveSnapshot, which
	// guarantees a broken snapshot cannot be published no matter how the caller
	// handles the original error.
	failed error
}

// Init creates a new repository at the backend's location.
func Init(ctx context.Context, be backend.Backend, key *crypto.Key, opts Options) (*Repository, error) {
	if _, err := be.Stat(ctx, backend.Handle{Type: backend.TypeConfig}); err == nil {
		return nil, fmt.Errorf("%w: %s", ErrAlreadyInitialized, be.Location())
	} else if !errors.Is(err, backend.ErrNotFound) {
		return nil, err
	}

	chunkerCfg := opts.Chunker
	if chunkerCfg == (chunker.Config{}) {
		chunkerCfg = chunker.DefaultConfig()
	}
	if err := chunkerCfg.Validate(); err != nil {
		return nil, err
	}

	var id crypto.ID
	if _, err := rand.Read(id[:]); err != nil {
		return nil, fmt.Errorf("repo: generating repository ID: %w", err)
	}

	cfg := Config{
		Version:     configVersion,
		ID:          id,
		Created:     time.Now().UTC(),
		Chunker:     fromChunker(chunkerCfg),
		Compression: opts.Compression.String(),
	}
	if err := saveConfig(ctx, be, key, cfg); err != nil {
		return nil, err
	}

	return newRepository(be, key, cfg, index.New(), opts)
}

// Open loads an existing repository, including its index.
func Open(ctx context.Context, be backend.Backend, key *crypto.Key, opts Options) (*Repository, error) {
	cfg, err := loadConfig(ctx, be, key)
	if err != nil {
		return nil, err
	}

	r, err := newRepository(be, key, cfg, nil, opts)
	if err != nil {
		return nil, err
	}

	idx, err := index.Load(ctx, be, key, indexCache{r.cache})
	if err != nil {
		return nil, err
	}
	r.idx = idx
	return r, nil
}

func newRepository(be backend.Backend, key *crypto.Key, cfg Config, idx *index.Index, opts Options) (*Repository, error) {
	chunkerCfg := cfg.Chunker.toChunker()
	if err := chunkerCfg.Validate(); err != nil {
		return nil, fmt.Errorf("repo: the repository's stored chunker settings are invalid: %w", err)
	}

	c := cache.Disabled()
	if !opts.NoCache {
		var err error
		c, err = cache.New(opts.CacheDir, cfg.ID)
		if err != nil {
			// The cache is only ever an optimization, so a repository still
			// opens without it.
			c = cache.Disabled()
		}
	}

	return &Repository{
		be:          be,
		key:         key,
		cfg:         cfg,
		idx:         idx,
		cache:       c,
		compression: opts.Compression,
		chunkerCfg:  chunkerCfg,
	}, nil
}

// Accessors.
func (r *Repository) Backend() backend.Backend      { return r.be }
func (r *Repository) Key() *crypto.Key              { return r.key }
func (r *Repository) Config() Config                { return r.cfg }
func (r *Repository) Index() *index.Index           { return r.idx }
func (r *Repository) ChunkerConfig() chunker.Config { return r.chunkerCfg }
func (r *Repository) Cache() *cache.Cache           { return r.cache }

// Close releases the repository's resources. It does not flush pending data;
// call Flush first.
func (r *Repository) Close() error { return r.be.Close() }

func saveConfig(ctx context.Context, be backend.Backend, key *crypto.Key, cfg Config) error {
	plaintext, err := json.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("repo: encoding config: %w", err)
	}
	sealed, err := key.SealMeta(plaintext, []byte(configAADLabel))
	if err != nil {
		return fmt.Errorf("repo: sealing config: %w", err)
	}
	return be.Save(ctx, backend.Handle{Type: backend.TypeConfig}, bytes.NewReader(sealed), int64(len(sealed)))
}

func loadConfig(ctx context.Context, be backend.Backend, key *crypto.Key) (Config, error) {
	sealed, err := backend.LoadAll(ctx, be, backend.Handle{Type: backend.TypeConfig})
	if err != nil {
		if errors.Is(err, backend.ErrNotFound) {
			return Config{}, fmt.Errorf("%w: %s", ErrNotInitialized, be.Location())
		}
		return Config{}, err
	}

	plaintext, err := key.OpenMeta(sealed, []byte(configAADLabel))
	if err != nil {
		return Config{}, fmt.Errorf("repo: reading config: %w", err)
	}

	var cfg Config
	if err := json.Unmarshal(plaintext, &cfg); err != nil {
		return Config{}, fmt.Errorf("repo: parsing config: %w", err)
	}
	if cfg.Version != configVersion {
		return Config{}, fmt.Errorf("repo: repository format version %d is not supported by this build, which understands version %d",
			cfg.Version, configVersion)
	}
	return cfg, nil
}

// indexCache adapts the local cache to the interface the index package expects.
type indexCache struct{ c *cache.Cache }

func (i indexCache) Get(id crypto.ID) ([]byte, bool) { return i.c.Get(cache.KindIndex, id) }
func (i indexCache) Put(id crypto.ID, data []byte) error {
	return i.c.Put(cache.KindIndex, id, data)
}
