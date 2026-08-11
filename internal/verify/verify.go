// Package verify checks that a repository is internally consistent and that its
// data can actually be restored.
//
// Two levels are offered, because they answer different questions at very
// different costs:
//
//   - The structural check confirms that every chunk referenced by every
//     snapshot resolves through the index to a pack that exists and is large
//     enough to contain it. It reads only metadata -- a listing plus the
//     snapshot and tree blobs -- so it is cheap enough to run routinely. It
//     catches the failures that actually happen: a missing pack, an interrupted
//     upload, a snapshot written against an index that never landed.
//
//   - The full check additionally downloads every pack, decrypts each blob, and
//     recomputes its keyed ID. Because the ID is an HMAC over the plaintext,
//     matching it proves the bytes are exactly what was backed up. This is the
//     only way to detect silent corruption in storage, and it costs a full
//     download of the repository.
package verify

import (
	"context"
	"fmt"
	"sort"
	"sync"

	"github.com/senecal-jjs/cmd-line-backup/internal/backend"
	"github.com/senecal-jjs/cmd-line-backup/internal/crypto"
	"github.com/senecal-jjs/cmd-line-backup/internal/index"
	"github.com/senecal-jjs/cmd-line-backup/internal/pack"
	"github.com/senecal-jjs/cmd-line-backup/internal/repo"
	"github.com/senecal-jjs/cmd-line-backup/internal/snapshot"
)

// DefaultConcurrency is how many packs are read at once in a full check.
const DefaultConcurrency = 4

// Options configure a verification run.
type Options struct {
	// ReadData enables the full check: every pack is downloaded and every blob
	// is decrypted and matched against its ID.
	ReadData bool

	// Snapshots limits the check to specific snapshots. Empty means all of them.
	Snapshots []*snapshot.Snapshot

	Concurrency int

	// Progress, if set, is called with a short description of each stage.
	Progress func(string)
}

// Report summarizes what verification found.
type Report struct {
	SnapshotsChecked int
	TreesChecked     int
	BlobsReferenced  int
	PacksReferenced  int
	PacksInStorage   int
	BytesRead        int64

	// OrphanPacks are packs no index entry refers to. They are usually the
	// residue of an interrupted backup: harmless, but wasted space.
	OrphanPacks []crypto.ID

	// Problems lists everything wrong with the repository.
	Problems []string
}

// OK reports whether verification found no problems.
func (r *Report) OK() bool { return len(r.Problems) == 0 }

func (r *Report) addProblem(format string, args ...any) {
	r.Problems = append(r.Problems, fmt.Sprintf(format, args...))
}

// Run verifies a repository.
func Run(ctx context.Context, r *repo.Repository, opts Options) (*Report, error) {
	if opts.Concurrency <= 0 {
		opts.Concurrency = DefaultConcurrency
	}

	report := &Report{}
	progress := func(msg string) {
		if opts.Progress != nil {
			opts.Progress(msg)
		}
	}

	// Inventory the packs that actually exist, with their sizes, so index
	// entries can be checked against reality without a request per lookup.
	progress("listing packs")
	packSizes := make(map[crypto.ID]int64)
	if err := r.Backend().List(ctx, backend.TypePack, func(fi backend.FileInfo) error {
		id, err := crypto.ParseID(fi.Name)
		if err != nil {
			report.addProblem("pack object %q does not have a valid ID", fi.Name)
			return nil
		}
		packSizes[id] = fi.Size
		return nil
	}); err != nil {
		return nil, err
	}
	report.PacksInStorage = len(packSizes)

	// Check the index against that inventory.
	progress("checking the index")
	referenced := make(map[crypto.ID]bool)
	if err := checkIndex(r, packSizes, referenced, report); err != nil {
		return nil, err
	}
	report.PacksReferenced = len(referenced)

	for id := range packSizes {
		if !referenced[id] {
			report.OrphanPacks = append(report.OrphanPacks, id)
		}
	}
	sort.Slice(report.OrphanPacks, func(i, j int) bool {
		return report.OrphanPacks[i].String() < report.OrphanPacks[j].String()
	})

	// Walk the snapshots and confirm every chunk they name is resolvable.
	snapshots := opts.Snapshots
	if len(snapshots) == 0 {
		var err error
		snapshots, err = r.ListSnapshots(ctx)
		if err != nil {
			return nil, err
		}
	}

	seenTrees := make(map[crypto.ID]bool)
	for _, snap := range snapshots {
		progress(fmt.Sprintf("checking snapshot %s", snap.ID.Short()))
		report.SnapshotsChecked++
		checkTree(ctx, r, snap.Tree, "/", seenTrees, report)
	}
	report.TreesChecked = len(seenTrees)

	if opts.ReadData {
		progress("reading and verifying every pack")
		if err := readAllPacks(ctx, r, packSizes, opts, report, progress); err != nil {
			return nil, err
		}
	}

	return report, nil
}

// checkIndex confirms that every index entry points into a pack that exists and
// is long enough to hold the blob.
func checkIndex(r *repo.Repository, packSizes map[crypto.ID]int64, referenced map[crypto.ID]bool, report *Report) error {
	return r.Index().Each(func(id crypto.ID, loc index.Location) error {
		referenced[loc.PackID] = true

		size, ok := packSizes[loc.PackID]
		if !ok {
			report.addProblem("chunk %s is stored in pack %s, which is missing from the repository",
				id.Short(), loc.PackID.Short())
			return nil
		}
		if end := int64(loc.Entry.Offset) + int64(loc.Entry.Length); end > size {
			report.addProblem("chunk %s claims bytes %d-%d of pack %s, which is only %d bytes long",
				id.Short(), loc.Entry.Offset, end, loc.PackID.Short(), size)
		}
		return nil
	})
}

// checkTree walks a snapshot's directory structure, confirming each tree can be
// read and each referenced chunk is in the index.
func checkTree(ctx context.Context, r *repo.Repository, treeID crypto.ID, path string, seen map[crypto.ID]bool, report *Report) {
	if seen[treeID] {
		// Deduplicated trees are shared between snapshots; checking one once is
		// enough and keeps verification proportional to unique data.
		return
	}
	seen[treeID] = true

	tree, err := r.LoadTree(ctx, treeID)
	if err != nil {
		report.addProblem("cannot read the directory tree for %s: %v", path, err)
		return
	}

	idx := r.Index()
	for _, node := range tree.Nodes {
		childPath := path + node.Name

		switch node.Type {
		case snapshot.NodeDir:
			if node.Subtree == nil {
				report.addProblem("directory %s has no subtree reference", childPath)
				continue
			}
			checkTree(ctx, r, *node.Subtree, childPath+"/", seen, report)

		case snapshot.NodeFile:
			for _, chunkID := range node.Content {
				report.BlobsReferenced++
				if _, ok := idx.Lookup(chunkID); !ok {
					report.addProblem("file %s references chunk %s, which is not in any index",
						childPath, chunkID.Short())
				}
			}
		}
	}
}

// readAllPacks downloads every pack and verifies each blob against its ID.
func readAllPacks(ctx context.Context, r *repo.Repository, packSizes map[crypto.ID]int64, opts Options, report *Report, progress func(string)) error {
	ids := make([]crypto.ID, 0, len(packSizes))
	for id := range packSizes {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i].String() < ids[j].String() })

	var (
		wg  sync.WaitGroup
		sem = make(chan struct{}, opts.Concurrency)
		mu  sync.Mutex
	)

	for _, packID := range ids {
		wg.Add(1)
		sem <- struct{}{}

		go func(packID crypto.ID) {
			defer wg.Done()
			defer func() { <-sem }()

			problems, bytesRead := verifyPack(ctx, r, packID)

			mu.Lock()
			defer mu.Unlock()
			report.BytesRead += bytesRead
			report.Problems = append(report.Problems, problems...)
		}(packID)
	}

	wg.Wait()

	// Concurrency makes the order nondeterministic, so sort for stable output.
	mu.Lock()
	sort.Strings(report.Problems)
	mu.Unlock()
	return nil
}

// verifyPack downloads one pack and checks every blob it contains.
func verifyPack(ctx context.Context, r *repo.Repository, packID crypto.ID) ([]string, int64) {
	var problems []string

	header, err := pack.LoadHeader(ctx, r.Backend(), r.Key(), packID)
	if err != nil {
		return []string{fmt.Sprintf("pack %s: cannot read its header: %v", packID.Short(), err)}, 0
	}

	data, err := backend.LoadAll(ctx, r.Backend(), backend.Handle{Type: backend.TypePack, Name: packID.String()})
	if err != nil {
		return []string{fmt.Sprintf("pack %s: cannot be read: %v", packID.Short(), err)}, 0
	}

	for _, entry := range header.Entries {
		end := int64(entry.Offset) + int64(entry.Length)
		if end > int64(len(data)) {
			problems = append(problems, fmt.Sprintf(
				"pack %s: blob %s extends past the end of the object", packID.Short(), entry.ID.Short()))
			continue
		}
		// DecodeBlob authenticates the ciphertext, decompresses it, and
		// recomputes the keyed chunk ID, so a match proves the bytes are
		// exactly what was backed up.
		if _, err := pack.DecodeBlob(r.Key(), entry, data[entry.Offset:end]); err != nil {
			problems = append(problems, fmt.Sprintf("pack %s: %v", packID.Short(), err))
		}
	}
	return problems, int64(len(data))
}
