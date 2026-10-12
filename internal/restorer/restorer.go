// Package restorer implements the restore side of gobackup: turning a snapshot
// back into files on disk.
//
// # Read planning
//
// The naive approach -- walk the tree and fetch each chunk as it is needed --
// issues one request per chunk, in whatever order the files happen to reference
// them. Because chunks are deduplicated and packed together, that order has
// little to do with how the data is laid out in storage, so it produces a large
// number of small, scattered requests.
//
// Instead the restorer first collects every chunk it needs, groups them by pack,
// sorts them by offset, and merges chunks that sit close together into a single
// ranged read. One request can then satisfy many chunks, which matters because
// S3 charges per request and each one carries latency.
package restorer

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"

	"github.com/senecal-jjs/cmd-line-backup/internal/backend"
	"github.com/senecal-jjs/cmd-line-backup/internal/crypto"
	"github.com/senecal-jjs/cmd-line-backup/internal/pack"
	"github.com/senecal-jjs/cmd-line-backup/internal/repo"
	"github.com/senecal-jjs/cmd-line-backup/internal/snapshot"
)

const (
	// DefaultConcurrency is how many pack reads run at once.
	DefaultConcurrency = 4

	// mergeGap is the largest run of unwanted bytes worth downloading to avoid
	// issuing a second request. Fetching a few unneeded kilobytes is cheaper
	// than another round trip.
	mergeGap = 64 << 10
)

// Options configure a restore.
type Options struct {
	// Include and Exclude are glob patterns matched against the path within the
	// snapshot. When Include is non-empty, only matching paths are restored.
	Include []string
	Exclude []string

	// Concurrency bounds parallel pack reads.
	Concurrency int

	// Overwrite allows replacing files that already exist in the target.
	Overwrite bool

	// Progress, if set, is called as files are written.
	Progress func(Progress)
}

// Progress reports how far a restore has got.
type Progress struct {
	Path          string
	FilesRestored uint64
	BytesRestored uint64
}

// Stats summarizes a completed restore.
type Stats struct {
	Files     uint64
	Dirs      uint64
	Symlinks  uint64
	Hardlinks uint64
	Bytes     uint64
}

// A Restorer extracts snapshots from a repository.
type Restorer struct {
	repo *repo.Repository
	opts Options

	// target is the absolute restore destination, retained so that every path
	// written can be re-checked for containment after symlink resolution.
	target string

	mu    sync.Mutex
	stats Stats
}

// New returns a Restorer reading from r.
func New(r *repo.Repository, opts Options) *Restorer {
	if opts.Concurrency <= 0 {
		opts.Concurrency = DefaultConcurrency
	}
	return &Restorer{repo: r, opts: opts}
}

// fileTask is one file to write, resolved from the snapshot tree.
type fileTask struct {
	path string // absolute path in the target directory
	node *snapshot.Node
}

// RestoreSnapshot writes a snapshot's contents into target.
func (r *Restorer) RestoreSnapshot(ctx context.Context, snap *snapshot.Snapshot, target string) (Stats, error) {
	absTarget, err := filepath.Abs(target)
	if err != nil {
		return Stats{}, fmt.Errorf("restorer: resolving target %s: %w", target, err)
	}
	if err := os.MkdirAll(absTarget, 0o755); err != nil {
		return Stats{}, fmt.Errorf("restorer: creating target %s: %w", absTarget, err)
	}
	r.target = absTarget

	// Pass one: create the directory skeleton, symlinks, and the list of files
	// to fill in. Directories must exist before any file can be written, and
	// their metadata is applied afterwards because writing children changes it.
	var (
		files []fileTask
		dirs  []dirTask
	)
	noop := func() error { return nil } // the target itself already exists
	if err := r.walk(ctx, snap.Tree, "", absTarget, noop, &files, &dirs); err != nil {
		return Stats{}, err
	}

	// Pass two: fetch the data with a plan that minimizes requests.
	if err := r.restoreFiles(ctx, files); err != nil {
		return Stats{}, err
	}

	// Pass three: apply directory metadata, deepest first, so that timestamps
	// set on a parent are not disturbed by writing into a child afterwards.
	sort.Slice(dirs, func(i, j int) bool { return len(dirs[i].path) > len(dirs[j].path) })
	for _, d := range dirs {
		if err := snapshot.RestoreDirMetadata(d.path, d.node); err != nil {
			return Stats{}, err
		}
	}

	return r.snapshotStats(), nil
}

type dirTask struct {
	path string
	node *snapshot.Node
}

// walk builds the directory structure and collects the files to restore.
//
// ensureParent creates the directory being walked, along with any of its own
// parents not created yet. It is called before anything is placed inside, so
// that with include filters only directories leading to restored entries are
// created, rather than the snapshot's entire directory skeleton.
func (r *Restorer) walk(ctx context.Context, treeID crypto.ID, snapPath, target string, ensureParent func() error, files *[]fileTask, dirs *[]dirTask) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	tree, err := r.repo.LoadTree(ctx, treeID)
	if err != nil {
		return err
	}

	for _, node := range tree.Nodes {
		childSnapPath := path(snapPath, node.Name)

		// A malicious or corrupt repository must not be able to write outside
		// the target directory.
		childTarget := filepath.Join(target, node.Name)
		if !within(target, childTarget) {
			return fmt.Errorf("restorer: entry %q would escape the target directory", childSnapPath)
		}

		switch node.Type {
		case snapshot.NodeDir:
			// Excluding a directory excludes everything beneath it. Its
			// children cannot be relied on to match the pattern themselves:
			// "*.d" matches "build.d" but not "build.d/file".
			if r.excluded(childSnapPath) {
				continue
			}
			if node.Subtree == nil {
				return fmt.Errorf("restorer: directory %q has no subtree", childSnapPath)
			}

			dirTarget, dirNode, created := childTarget, node, false
			ensureDir := func() error {
				if created {
					return nil
				}
				if err := ensureParent(); err != nil {
					return err
				}
				if err := r.safeMkdir(dirTarget); err != nil {
					return err
				}
				*dirs = append(*dirs, dirTask{path: dirTarget, node: dirNode})
				r.addStats(func(s *Stats) { s.Dirs++ })
				created = true
				return nil
			}

			// A directory the filters select is restored even if empty. One
			// that is merely on the way to selected entries is created only
			// when the first of them is restored.
			if r.included(childSnapPath) {
				if err := ensureDir(); err != nil {
					return err
				}
			}
			if err := r.walk(ctx, *node.Subtree, childSnapPath, childTarget, ensureDir, files, dirs); err != nil {
				return err
			}

		case snapshot.NodeSymlink:
			if !r.included(childSnapPath) {
				continue
			}
			if err := ensureParent(); err != nil {
				return err
			}
			if err := r.restoreSymlink(childTarget, node); err != nil {
				return err
			}

		case snapshot.NodeFile:
			if !r.included(childSnapPath) {
				continue
			}
			if err := ensureParent(); err != nil {
				return err
			}
			*files = append(*files, fileTask{path: childTarget, node: node})
		}
	}
	return nil
}

func (r *Restorer) restoreSymlink(target string, node *snapshot.Node) error {
	if err := checkContained(r.target, filepath.Dir(target)); err != nil {
		return err
	}
	if err := r.clearExisting(target); err != nil {
		return err
	}
	if err := os.Symlink(node.LinkTarget, target); err != nil {
		return fmt.Errorf("restorer: creating symlink %s: %w", target, err)
	}
	r.addStats(func(s *Stats) { s.Symlinks++ })
	return nil
}

// blobRead is one chunk that has to be fetched for one position in one file.
type blobRead struct {
	entry pack.Entry
	// destinations lists every (file, offset) that needs this chunk. A
	// deduplicated chunk can appear many times across a snapshot, and it should
	// only be downloaded once.
	destinations []destination
}

type destination struct {
	fileIndex int
	offset    int64
}

// restoreFiles creates every file, then fetches the data behind them.
func (r *Restorer) restoreFiles(ctx context.Context, files []fileTask) error {
	if len(files) == 0 {
		return nil
	}

	// Hardlinks: files sharing a device and inode in the snapshot are recreated
	// as links to the first one restored rather than as independent copies.
	// The links themselves are created afterwards, once their target actually
	// exists on disk.
	type linkTask struct{ existing, target string }

	linked := make(map[[2]uint64]string)
	var (
		toWrite []fileTask
		links   []linkTask
	)

	for _, f := range files {
		if f.node.IsHardlink() {
			key := [2]uint64{f.node.Device, f.node.Inode}
			if existing, ok := linked[key]; ok {
				links = append(links, linkTask{existing: existing, target: f.path})
				continue
			}
			linked[key] = f.path
		}
		toWrite = append(toWrite, f)
	}

	// Create every file up front so chunks can be written at their final
	// offsets in any order. The files are closed straight away and reopened for
	// each write: holding them all open would need one descriptor per file in
	// the snapshot, and a restore of a large tree would fail with "too many
	// open files".
	for _, task := range toWrite {
		dir := filepath.Dir(task.path)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("restorer: creating directory for %s: %w", task.path, err)
		}
		// Confirm the directory really is inside the target once symlinks are
		// resolved, so no file can be written through a redirected component.
		if err := checkContained(r.target, dir); err != nil {
			return err
		}

		// An existing symlink at the destination would otherwise be followed,
		// writing the restored content to whatever it points at.
		if fi, err := os.Lstat(task.path); err == nil && fi.Mode()&os.ModeSymlink != 0 {
			if !r.opts.Overwrite {
				return fmt.Errorf("restorer: %s already exists as a symlink; pass --overwrite to replace it", task.path)
			}
			if err := os.Remove(task.path); err != nil {
				return fmt.Errorf("restorer: replacing %s: %w", task.path, err)
			}
		}

		// O_NOFOLLOW is a second line of defence on the final component.
		flags := os.O_WRONLY | os.O_CREATE | os.O_TRUNC | syscall.O_NOFOLLOW
		if !r.opts.Overwrite {
			flags |= os.O_EXCL
		}
		f, err := os.OpenFile(task.path, flags, 0o600)
		if err != nil {
			if errors.Is(err, os.ErrExist) {
				return fmt.Errorf("restorer: %s already exists; pass --overwrite to replace it", task.path)
			}
			return fmt.Errorf("restorer: creating %s: %w", task.path, err)
		}
		if err := f.Close(); err != nil {
			return fmt.Errorf("restorer: creating %s: %w", task.path, err)
		}
	}

	if err := r.fetchAndWrite(ctx, toWrite); err != nil {
		return err
	}

	for _, task := range toWrite {
		if err := snapshot.RestoreMetadata(task.path, task.node); err != nil {
			return err
		}
		r.addStats(func(s *Stats) {
			s.Files++
			s.Bytes += task.node.Size
		})
		r.report(task.path)
	}

	// Every link target is now complete, so the links can be made.
	for _, l := range links {
		if err := r.linkFile(l.existing, l.target); err != nil {
			return err
		}
	}
	return nil
}

func (r *Restorer) linkFile(existing, target string) error {
	dir := filepath.Dir(target)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("restorer: creating directory for %s: %w", target, err)
	}
	if err := checkContained(r.target, dir); err != nil {
		return err
	}
	if err := r.clearExisting(target); err != nil {
		return err
	}
	if err := os.Link(existing, target); err != nil {
		return fmt.Errorf("restorer: linking %s to %s: %w", target, existing, err)
	}
	r.addStats(func(s *Stats) { s.Hardlinks++ })
	return nil
}

// fetchAndWrite builds the read plan and executes it.
func (r *Restorer) fetchAndWrite(ctx context.Context, files []fileTask) error {
	idx := r.repo.Index()

	// Group every needed chunk by the pack that holds it, recording all the
	// places each chunk has to be written.
	byPack := make(map[crypto.ID]map[crypto.ID]*blobRead)
	for fileIndex, task := range files {
		var offset int64
		for _, chunkID := range task.node.Content {
			loc, ok := idx.Lookup(chunkID)
			if !ok {
				return fmt.Errorf("restorer: %s references chunk %s, which is not in the index; the repository is incomplete",
					task.path, chunkID.Short())
			}

			blobs, ok := byPack[loc.PackID]
			if !ok {
				blobs = make(map[crypto.ID]*blobRead)
				byPack[loc.PackID] = blobs
			}
			read, ok := blobs[chunkID]
			if !ok {
				read = &blobRead{entry: loc.Entry}
				blobs[chunkID] = read
			}
			read.destinations = append(read.destinations, destination{fileIndex: fileIndex, offset: offset})

			offset += int64(loc.Entry.PlainLength)
		}
	}

	packIDs := make([]crypto.ID, 0, len(byPack))
	for id := range byPack {
		packIDs = append(packIDs, id)
	}
	sort.Slice(packIDs, func(i, j int) bool { return packIDs[i].String() < packIDs[j].String() })

	var (
		wg       sync.WaitGroup
		sem      = make(chan struct{}, r.opts.Concurrency)
		mu       sync.Mutex
		firstErr error
	)

	for _, packID := range packIDs {
		wg.Add(1)
		sem <- struct{}{}
		go func(packID crypto.ID, blobs map[crypto.ID]*blobRead) {
			defer wg.Done()
			defer func() { <-sem }()

			if err := r.readPack(ctx, packID, blobs, files); err != nil {
				mu.Lock()
				if firstErr == nil {
					firstErr = err
				}
				mu.Unlock()
			}
		}(packID, byPack[packID])
	}

	wg.Wait()
	return firstErr
}

// readPack fetches the needed regions of one pack and writes the chunks out.
func (r *Restorer) readPack(ctx context.Context, packID crypto.ID, blobs map[crypto.ID]*blobRead, files []fileTask) error {
	reads := make([]*blobRead, 0, len(blobs))
	for _, b := range blobs {
		reads = append(reads, b)
	}
	// Sorting by offset turns scattered lookups into a sequential sweep, which
	// is what allows neighbouring chunks to be merged into one request.
	sort.Slice(reads, func(i, j int) bool { return reads[i].entry.Offset < reads[j].entry.Offset })

	handle := backend.Handle{Type: backend.TypePack, Name: packID.String()}
	be := r.repo.Backend()
	key := r.repo.Key()

	for _, group := range groupReads(reads) {
		if err := ctx.Err(); err != nil {
			return err
		}

		start := int64(group[0].entry.Offset)
		last := group[len(group)-1].entry
		length := int64(last.Offset) + int64(last.Length) - start

		buf, err := backend.LoadRange(ctx, be, handle, start, length)
		if err != nil {
			return fmt.Errorf("restorer: reading pack %s: %w", packID.Short(), err)
		}

		for _, read := range group {
			sealed := buf[int64(read.entry.Offset)-start : int64(read.entry.Offset)-start+int64(read.entry.Length)]
			plaintext, err := pack.DecodeBlob(key, read.entry, sealed)
			if err != nil {
				return fmt.Errorf("restorer: pack %s: %w", packID.Short(), err)
			}

			for _, dst := range read.destinations {
				if err := writeAt(files[dst.fileIndex].path, plaintext, dst.offset); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// writeAt writes one chunk into a file created earlier in the restore.
// O_NOFOLLOW keeps the write from being redirected if the file was replaced by
// a symlink after it was created.
func writeAt(path string, data []byte, offset int64) error {
	f, err := os.OpenFile(path, os.O_WRONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return fmt.Errorf("restorer: opening %s: %w", path, err)
	}
	if _, err := f.WriteAt(data, offset); err != nil {
		f.Close()
		return fmt.Errorf("restorer: writing %s: %w", path, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("restorer: writing %s: %w", path, err)
	}
	return nil
}

// groupReads merges chunks that are close together into batches served by a
// single ranged read.
func groupReads(reads []*blobRead) [][]*blobRead {
	if len(reads) == 0 {
		return nil
	}

	var (
		groups  [][]*blobRead
		current = []*blobRead{reads[0]}
		end     = int64(reads[0].entry.Offset) + int64(reads[0].entry.Length)
	)

	for _, read := range reads[1:] {
		start := int64(read.entry.Offset)
		if start-end <= mergeGap {
			current = append(current, read)
			if e := start + int64(read.entry.Length); e > end {
				end = e
			}
			continue
		}
		groups = append(groups, current)
		current = []*blobRead{read}
		end = start + int64(read.entry.Length)
	}
	return append(groups, current)
}

func (r *Restorer) addStats(fn func(*Stats)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	fn(&r.stats)
}

func (r *Restorer) snapshotStats() Stats {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.stats
}

func (r *Restorer) report(p string) {
	if r.opts.Progress == nil {
		return
	}
	r.mu.Lock()
	prog := Progress{Path: p, FilesRestored: r.stats.Files, BytesRestored: r.stats.Bytes}
	r.mu.Unlock()
	r.opts.Progress(prog)
}

// included reports whether a path passes the include and exclude filters.
func (r *Restorer) included(p string) bool {
	if r.excluded(p) {
		return false
	}
	if len(r.opts.Include) == 0 {
		return true
	}
	for _, pattern := range r.opts.Include {
		if matchPath(pattern, p) {
			return true
		}
	}
	return false
}

func (r *Restorer) excluded(p string) bool {
	for _, pattern := range r.opts.Exclude {
		if matchPath(pattern, p) {
			return true
		}
	}
	return false
}

// matchPath matches a glob against a full snapshot path or its base name, and
// treats a pattern as covering everything beneath it.
func matchPath(pattern, p string) bool {
	if ok, err := filepath.Match(pattern, p); err == nil && ok {
		return true
	}
	if ok, err := filepath.Match(pattern, filepath.Base(p)); err == nil && ok {
		return true
	}
	trimmed := strings.TrimSuffix(pattern, "/")
	return p == trimmed || strings.HasPrefix(p, trimmed+"/")
}

func path(dir, name string) string {
	if dir == "" {
		return name
	}
	return dir + "/" + name
}

// within reports whether child lies inside parent, guarding against a
// repository that contains crafted names such as "..".
func within(parent, child string) bool {
	rel, err := filepath.Rel(parent, child)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// safeMkdir creates a single directory without ever following a symlink in its
// final component.
//
// The lexical `within` check is not sufficient on its own: it proves the joined
// path looks contained, but every subsequent write follows symlinks, so a
// symlink sitting at that location -- planted by another user in a shared
// target directory, left by an earlier restore, or named by a crafted
// repository -- would redirect the write outside the target. Replacing a
// non-directory here removes that redirection before anything is written
// through it.
//
// Whatever occupies the place is only removed with Overwrite; otherwise the
// restore stops rather than delete something it did not create.
func (r *Restorer) safeMkdir(path string) error {
	switch fi, err := os.Lstat(path); {
	case err == nil && fi.IsDir():
		return nil

	case err == nil:
		// A symlink or a regular file is occupying the directory's place.
		if !r.opts.Overwrite {
			return fmt.Errorf("restorer: %s already exists and is not a directory; pass -overwrite to replace it", path)
		}
		if err := os.Remove(path); err != nil {
			return fmt.Errorf("restorer: replacing %s: %w", path, err)
		}

	case !errors.Is(err, os.ErrNotExist):
		return fmt.Errorf("restorer: inspecting %s: %w", path, err)
	}

	if err := os.Mkdir(path, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
		return fmt.Errorf("restorer: creating %s: %w", path, err)
	}
	return nil
}

// clearExisting makes way for a symlink or hardlink at path. Anything already
// there is removed only with Overwrite, matching how regular files are treated.
func (r *Restorer) clearExisting(path string) error {
	if _, err := os.Lstat(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("restorer: inspecting %s: %w", path, err)
	}
	if !r.opts.Overwrite {
		return fmt.Errorf("restorer: %s already exists; pass -overwrite to replace it", path)
	}
	if err := os.Remove(path); err != nil {
		return fmt.Errorf("restorer: replacing %s: %w", path, err)
	}
	return nil
}

// checkContained verifies that a directory really resides inside the target
// after symlinks are resolved.
//
// safeMkdir protects each component as it is created, but a restore may write
// into directories it did not create, and the snapshot itself may name the same
// path as both a symlink and a directory. Resolving the finished path and
// re-checking containment closes the gap for good.
func checkContained(target, dir string) error {
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return fmt.Errorf("restorer: resolving %s: %w", dir, err)
	}
	resolvedTarget, err := filepath.EvalSymlinks(target)
	if err != nil {
		return fmt.Errorf("restorer: resolving the target directory: %w", err)
	}
	if !within(resolvedTarget, resolved) {
		return fmt.Errorf("restorer: %s resolves to %s, which is outside the restore target", dir, resolved)
	}
	return nil
}
