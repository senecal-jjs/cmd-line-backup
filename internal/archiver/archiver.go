// Package archiver implements the backup side of gobackup: walking the
// filesystem, deciding what has changed, chunking new data, and assembling the
// directory trees that make up a snapshot.
//
// # What makes a backup incremental
//
// Two independent mechanisms keep repeat backups cheap, and they operate at
// different levels:
//
//   - The unchanged-file fast path. If a file's metadata matches the parent
//     snapshot, its chunk list is copied straight from the parent tree and the
//     file is never opened. This is what makes backing up a large, mostly-static
//     directory take seconds.
//   - Content-defined chunking plus deduplication. Files that have changed, or
//     that have no parent entry, are chunked and looked up in the index. Only
//     genuinely new chunks are uploaded, so an edit to a huge file costs only the
//     bytes around the edit, and content that moved between files costs nothing.
//
// # Ordering
//
// Trees are built bottom-up, so a directory's tree blob is written only after
// every child it references exists. The snapshot is written last, after Flush
// has stored every pack and the index. An interrupted run therefore leaves only
// unreferenced packs, never a snapshot pointing at missing data.
package archiver

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/senecal-jjs/cmd-line-backup/internal/chunker"
	"github.com/senecal-jjs/cmd-line-backup/internal/crypto"
	"github.com/senecal-jjs/cmd-line-backup/internal/pack"
	"github.com/senecal-jjs/cmd-line-backup/internal/repo"
	"github.com/senecal-jjs/cmd-line-backup/internal/snapshot"
)

// DefaultConcurrency is the number of files processed at once.
const DefaultConcurrency = 4

// Options configure a backup run.
type Options struct {
	// Excludes are glob patterns. A path is skipped when a pattern matches
	// either its full path or its base name.
	Excludes []string

	// Parent is the snapshot to compare against. When nil, every file is read
	// and chunked, though deduplication still avoids re-uploading known data.
	Parent *snapshot.Snapshot

	Hostname string
	Username string
	Tags     []string

	// Concurrency bounds how many files are read and chunked at once.
	Concurrency int

	// OnError is called for a file that cannot be read. Returning nil skips the
	// file and continues, which is usually what a backup should do about a
	// handful of unreadable files. Returning an error aborts the run. When nil,
	// errors are skipped.
	OnError func(path string, err error) error

	// Progress, if set, is called as files are processed. It may be called
	// concurrently from several goroutines, so it must be safe for concurrent
	// use.
	Progress func(Progress)
}

// Progress reports how far a backup has got.
type Progress struct {
	Path           string
	FilesProcessed uint64
	BytesProcessed uint64
	BytesAdded     uint64
}

// An Archiver creates snapshots in a repository.
type Archiver struct {
	repo *repo.Repository
	opts Options

	// sem bounds how many files are read and chunked at once across the entire
	// walk. It is created once and shared by every directory level: a per-level
	// semaphore would allow Concurrency workers at each depth, so a deeply
	// nested tree would run Concurrency x depth files in parallel, each holding
	// a multi-megabyte chunker buffer and an open file descriptor.
	sem chan struct{}

	mu    sync.Mutex
	stats snapshot.Stats

	// abort cancels the run's context with the error that ended it. The first
	// fatal failure in any worker stops every other worker and the walk itself,
	// rather than letting them carry on reading and uploading data that no
	// snapshot will ever reference.
	abort context.CancelCauseFunc

	// parentNodes maps absolute paths to the nodes recorded in the parent
	// snapshot, so the unchanged-file check is a constant-time lookup.
	parentMu    sync.Mutex
	parentNodes map[string]*snapshot.Node
}

// New returns an Archiver writing to r.
func New(r *repo.Repository, opts Options) *Archiver {
	if opts.Concurrency <= 0 {
		opts.Concurrency = DefaultConcurrency
	}
	if opts.Hostname == "" {
		opts.Hostname, _ = os.Hostname()
	}
	return &Archiver{
		repo:        r,
		opts:        opts,
		sem:         make(chan struct{}, opts.Concurrency),
		parentNodes: make(map[string]*snapshot.Node),
	}
}

// Snapshot backs up the given paths and returns the resulting snapshot.
func (a *Archiver) Snapshot(ctx context.Context, paths []string) (*snapshot.Snapshot, error) {
	if len(paths) == 0 {
		return nil, errors.New("archiver: no paths to back up")
	}

	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	a.abort = cancel

	cleaned := make([]string, 0, len(paths))
	for _, p := range paths {
		abs, err := snapshot.CleanPath(p)
		if err != nil {
			return nil, err
		}
		if _, err := os.Lstat(abs); err != nil {
			return nil, fmt.Errorf("archiver: %s: %w", p, err)
		}
		cleaned = append(cleaned, abs)
	}
	sort.Strings(cleaned)
	cleaned = removeNestedPaths(cleaned)

	if a.opts.Parent != nil {
		if err := a.loadParentIndex(ctx); err != nil {
			return nil, err
		}
	}

	// The snapshot root mirrors the absolute paths being backed up, so
	// /etc and /home/user/docs coexist without their names colliding and a
	// restore reproduces the original layout.
	rootID, err := a.archiveRoot(ctx, cleaned)
	if err != nil {
		// Workers stopped by abort report a bare cancellation; the error that
		// triggered it is the one worth showing.
		if cause := context.Cause(ctx); cause != nil && errors.Is(err, context.Canceled) {
			err = cause
		}
		return nil, err
	}

	// Every pack and the index must be durable before the snapshot names them.
	if err := a.repo.Flush(ctx); err != nil {
		return nil, err
	}

	snap := &snapshot.Snapshot{
		Time:     time.Now().UTC(),
		Hostname: a.opts.Hostname,
		Username: a.opts.Username,
		Paths:    cleaned,
		Tags:     a.opts.Tags,
		Tree:     rootID,
		Stats:    a.snapshotStats(),
	}
	if a.opts.Parent != nil {
		parentID := a.opts.Parent.ID
		snap.Parent = &parentID
	}

	if _, err := a.repo.SaveSnapshot(ctx, snap); err != nil {
		return nil, err
	}
	return snap, nil
}

func (a *Archiver) snapshotStats() snapshot.Stats {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.stats
}

// removeNestedPaths drops paths already covered by an ancestor, so nothing is
// archived twice.
func removeNestedPaths(sorted []string) []string {
	out := sorted[:0]
	for _, p := range sorted {
		if len(out) > 0 {
			parent := out[len(out)-1]
			if p == parent || strings.HasPrefix(p, parent+string(filepath.Separator)) {
				continue
			}
		}
		out = append(out, p)
	}
	return out
}

// loadParentIndex walks the parent snapshot's trees and records every file node
// by absolute path, so the fast path can look one up in constant time.
func (a *Archiver) loadParentIndex(ctx context.Context) error {
	return a.walkParent(ctx, a.opts.Parent.Tree, string(filepath.Separator))
}

func (a *Archiver) walkParent(ctx context.Context, treeID crypto.ID, dir string) error {
	tree, err := a.repo.LoadTree(ctx, treeID)
	if err != nil {
		// A parent snapshot that cannot be read fully is not fatal: the backup
		// falls back to reading files, which is slower but still correct.
		return nil
	}

	for _, node := range tree.Nodes {
		path := filepath.Join(dir, node.Name)

		a.parentMu.Lock()
		a.parentNodes[path] = node
		a.parentMu.Unlock()

		if node.Type == snapshot.NodeDir && node.Subtree != nil {
			if err := a.walkParent(ctx, *node.Subtree, path); err != nil {
				return err
			}
		}
	}
	return nil
}

func (a *Archiver) parentNode(path string) *snapshot.Node {
	a.parentMu.Lock()
	defer a.parentMu.Unlock()
	return a.parentNodes[path]
}

// archiveRoot builds the synthetic directory hierarchy leading to each backup
// path and returns the root tree ID.
func (a *Archiver) archiveRoot(ctx context.Context, paths []string) (crypto.ID, error) {
	root := newPathTrie()
	for _, p := range paths {
		root.insert(p)
	}
	return a.archiveTrie(ctx, root, string(filepath.Separator))
}

// pathTrie mirrors the directory structure above the paths being archived.
type pathTrie struct {
	children map[string]*pathTrie
	// terminal marks a node that corresponds to a path the user asked for, and
	// whose real contents should be archived.
	terminal bool
}

func newPathTrie() *pathTrie {
	return &pathTrie{children: make(map[string]*pathTrie)}
}

func (t *pathTrie) insert(path string) {
	node := t
	for _, part := range splitPath(path) {
		child, ok := node.children[part]
		if !ok {
			child = newPathTrie()
			node.children[part] = child
		}
		node = child
	}
	node.terminal = true
}

func splitPath(path string) []string {
	trimmed := strings.Trim(filepath.ToSlash(path), "/")
	if trimmed == "" {
		return nil
	}
	return strings.Split(trimmed, "/")
}

// archiveTrie materializes an intermediate directory: its metadata comes from
// the real directory on disk, but only the children leading to a requested path
// are included.
func (a *Archiver) archiveTrie(ctx context.Context, t *pathTrie, dir string) (crypto.ID, error) {
	names := make([]string, 0, len(t.children))
	for name := range t.children {
		names = append(names, name)
	}
	sort.Strings(names)

	nodes := make([]*snapshot.Node, 0, len(names))
	for _, name := range names {
		child := t.children[name]
		path := filepath.Join(dir, name)

		var node *snapshot.Node
		if child.terminal {
			// The requested path itself is recorded as it is, so backing up a
			// symlink stores a symlink.
			fi, err := os.Lstat(path)
			if err != nil {
				return crypto.ID{}, fmt.Errorf("archiver: %s: %w", path, err)
			}
			node, err = a.archivePath(ctx, path, name, fi)
			if err != nil {
				return crypto.ID{}, err
			}
		} else {
			// Intermediate components are followed rather than recorded as
			// links. On macOS, for example, /var is a symlink to /private/var,
			// and a snapshot of /var/log has to treat "var" as the directory it
			// leads to, not as a link with children hanging off it.
			fi, err := os.Stat(path)
			if err != nil {
				return crypto.ID{}, fmt.Errorf("archiver: %s: %w", path, err)
			}
			node, err = a.archiveIntermediate(ctx, child, path, name, fi)
			if err != nil {
				return crypto.ID{}, err
			}
		}

		if node != nil {
			nodes = append(nodes, node)
		}
	}

	return a.saveTree(ctx, nodes)
}

func (a *Archiver) archiveIntermediate(ctx context.Context, t *pathTrie, path, name string, fi fs.FileInfo) (*snapshot.Node, error) {
	node, err := snapshot.NodeFromFileInfo(path, name, fi)
	if err != nil {
		return nil, err
	}
	subtree, err := a.archiveTrie(ctx, t, path)
	if err != nil {
		return nil, err
	}
	node.Subtree = &subtree
	return node, nil
}

// archivePath archives a real filesystem entry.
func (a *Archiver) archivePath(ctx context.Context, path, name string, fi fs.FileInfo) (*snapshot.Node, error) {
	if err := ctx.Err(); err != nil {
		return nil, fatal(err)
	}

	node, err := snapshot.NodeFromFileInfo(path, name, fi)
	if err != nil {
		// Sockets, devices, and FIFOs land here; skipping them is correct.
		return nil, a.handleError(path, err)
	}

	switch node.Type {
	case snapshot.NodeSymlink:
		return node, nil

	case snapshot.NodeDir:
		subtree, err := a.archiveDir(ctx, path)
		if err != nil {
			return nil, err
		}
		node.Subtree = &subtree
		a.addStats(func(s *snapshot.Stats) { s.DirsProcessed++ })
		return node, nil

	case snapshot.NodeFile:
		if err := a.archiveFile(ctx, path, node); err != nil {
			return nil, a.handleError(path, err)
		}
		return node, nil

	default:
		return nil, nil
	}
}

// archiveDir archives a directory's children and returns its tree ID.
func (a *Archiver) archiveDir(ctx context.Context, dir string) (crypto.ID, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if handled := a.handleError(dir, err); handled != nil {
			return crypto.ID{}, handled
		}
		// An unreadable directory is recorded as empty rather than aborting.
		return a.saveTree(ctx, nil)
	}

	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		nodes    []*snapshot.Node
		firstErr error
	)

	// collect is the single guarded path for appending a finished node, since
	// directories are handled on this goroutine while files are handled by the
	// worker pool. Any error reaching it ends the run, so it also stops every
	// other worker.
	collect := func(node *snapshot.Node, err error) {
		mu.Lock()
		defer mu.Unlock()
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			a.abort(err)
			return
		}
		if node != nil {
			nodes = append(nodes, node)
		}
	}
	failed := func() bool {
		mu.Lock()
		defer mu.Unlock()
		return firstErr != nil
	}

	// Every exit from the loop goes through wg.Wait below. Returning early would
	// leave workers running, and still writing to the repository, after the
	// backup had already reported its result.
	for _, entry := range entries {
		if failed() || ctx.Err() != nil {
			break
		}

		path := filepath.Join(dir, entry.Name())
		if a.excluded(path, entry.Name()) {
			continue
		}

		fi, err := entry.Info()
		if err != nil {
			if handled := a.handleError(path, err); handled != nil {
				collect(nil, handled)
			}
			continue
		}

		// Directories recurse on this goroutine so that the worker pool is not
		// consumed by goroutines that only wait on their children.
		if fi.IsDir() {
			collect(a.archivePath(ctx, path, entry.Name(), fi))
			continue
		}

		wg.Add(1)
		a.sem <- struct{}{}
		go func(path, name string, fi fs.FileInfo) {
			defer wg.Done()
			defer func() { <-a.sem }()
			collect(a.archivePath(ctx, path, name, fi))
		}(path, entry.Name(), fi)
	}

	wg.Wait()
	if firstErr != nil {
		return crypto.ID{}, firstErr
	}
	if err := ctx.Err(); err != nil {
		return crypto.ID{}, fatal(err)
	}

	// NewTree imposes the canonical ordering, so the order results arrived in
	// does not matter.
	return a.saveTree(ctx, nodes)
}

// saveTree stores a directory tree. Failures are fatal: a tree that cannot be
// written leaves the snapshot unable to describe its own contents.
func (a *Archiver) saveTree(ctx context.Context, nodes []*snapshot.Node) (crypto.ID, error) {
	id, err := a.repo.SaveTree(ctx, snapshot.NewTree(nodes))
	if err != nil {
		return crypto.ID{}, fatal(err)
	}
	return id, nil
}

// archiveFile fills in a file node's content, reusing the parent snapshot's
// chunk list when the file is unchanged.
func (a *Archiver) archiveFile(ctx context.Context, path string, node *snapshot.Node) error {
	if old := a.parentNode(path); snapshot.SameContent(old, node) {
		// The file is untouched: reuse its chunk list without opening it. This
		// is what makes a repeat backup fast.
		node.Content = old.Content
		a.addStats(func(s *snapshot.Stats) {
			s.FilesUnmodified++
			s.BytesProcessed += node.Size
		})
		a.report(path)
		return nil
	}

	isNew := a.parentNode(path) == nil

	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

	ch, err := chunker.New(f, a.repo.ChunkerConfig())
	if err != nil {
		return err
	}

	var (
		content    []crypto.ID
		total      uint64
		added      uint64
		newChunks  uint64
		dupeChunks uint64
	)
	for {
		if err := ctx.Err(); err != nil {
			return fatal(err)
		}

		chunk, err := ch.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}

		id, stored, err := a.repo.SaveBlob(ctx, pack.BlobData, chunk.Data)
		if err != nil {
			// A repository write failure invalidates the whole run, not just
			// this file.
			return fatal(err)
		}

		content = append(content, id)
		total += uint64(chunk.Length())
		if stored {
			added += uint64(chunk.Length())
			newChunks++
		} else {
			dupeChunks++
		}
	}

	node.Content = content
	// The file may have changed size since it was stated; record what was
	// actually read so restore allocates correctly.
	node.Size = total

	a.addStats(func(s *snapshot.Stats) {
		if isNew {
			s.FilesNew++
		} else {
			s.FilesChanged++
		}
		s.BytesProcessed += total
		s.BytesAdded += added
		s.ChunksNew += newChunks
		s.ChunksDuplicate += dupeChunks
	})
	a.report(path)
	return nil
}

func (a *Archiver) addStats(fn func(*snapshot.Stats)) {
	a.mu.Lock()
	defer a.mu.Unlock()
	fn(&a.stats)
}

func (a *Archiver) report(path string) {
	if a.opts.Progress == nil {
		return
	}

	a.mu.Lock()
	p := Progress{
		Path:           path,
		FilesProcessed: a.stats.FilesNew + a.stats.FilesChanged + a.stats.FilesUnmodified,
		BytesProcessed: a.stats.BytesProcessed,
		BytesAdded:     a.stats.BytesAdded,
	}
	a.mu.Unlock()

	a.opts.Progress(p)
}

// fatalError marks a failure that must abort the whole backup rather than being
// treated as a skippable file.
//
// The distinction matters a great deal. A file that cannot be read is a local
// problem affecting only that file, and skipping it is usually what the user
// wants. A failure to write to the repository is not: by the time it surfaces,
// blobs belonging to files already added to the tree may be missing, so
// continuing would produce a snapshot that references data the repository does
// not contain. Those errors must never be swallowed by the skip policy.
type fatalError struct{ err error }

func (e fatalError) Error() string { return e.err.Error() }
func (e fatalError) Unwrap() error { return e.err }

func fatal(err error) error {
	if err == nil {
		return nil
	}
	var already fatalError
	if errors.As(err, &already) {
		return err
	}
	return fatalError{err}
}

func isFatal(err error) bool {
	var f fatalError
	return errors.As(err, &f)
}

// handleError applies the caller's error policy to a failure reading a source
// file. It returns a non-nil error only when the run should stop.
func (a *Archiver) handleError(path string, err error) error {
	// Repository failures and cancellation are never skippable.
	if isFatal(err) {
		return err
	}
	if a.opts.OnError == nil {
		return nil // skip and continue
	}
	return a.opts.OnError(path, err)
}

// excluded reports whether a path matches any exclusion pattern, comparing
// against both the full path and the base name so that "*.log" and
// "/var/cache" both work.
func (a *Archiver) excluded(path, name string) bool {
	for _, pattern := range a.opts.Excludes {
		if ok, err := filepath.Match(pattern, name); err == nil && ok {
			return true
		}
		if ok, err := filepath.Match(pattern, path); err == nil && ok {
			return true
		}
		if pattern == path || strings.HasPrefix(path, strings.TrimSuffix(pattern, "/")+string(filepath.Separator)) {
			return true
		}
	}
	return false
}
