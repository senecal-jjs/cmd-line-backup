package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/user"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/senecal-jjs/cmd-line-backup/internal/archiver"
	"github.com/senecal-jjs/cmd-line-backup/internal/crypto"
	"github.com/senecal-jjs/cmd-line-backup/internal/repo"
	"github.com/senecal-jjs/cmd-line-backup/internal/restorer"
	"github.com/senecal-jjs/cmd-line-backup/internal/snapshot"
	"github.com/senecal-jjs/cmd-line-backup/internal/verify"
)

func runInit(ctx context.Context, args []string, out, errOut io.Writer) error {
	var g globalFlags
	fs := newFlagSet("init", errOut, `gobackup init - create a new backup repository

Usage:
  gobackup init -repo s3://bucket/prefix [-keyfile PATH]

Generates a new encryption key, protects it with a passphrase, writes it to the
keyfile, and initializes the repository. The keyfile is the only way to read the
backups: without it the data is unrecoverable, so keep a copy somewhere separate
from the repository itself.
`)
	g.register(fs)
	if err := fs.Parse(args); err != nil {
		return ErrUsage
	}

	keyfilePath := g.keyfilePath()
	if _, err := os.Stat(keyfilePath); err == nil {
		return fmt.Errorf("keyfile %s already exists; remove it or choose another path with -keyfile", keyfilePath)
	}

	passphrase, err := readPassphrase(errOut, "Enter a passphrase for the new key: ", true)
	if err != nil {
		return err
	}

	key, err := crypto.GenerateKey()
	if err != nil {
		return err
	}

	be, err := g.openBackend(ctx)
	if err != nil {
		return err
	}
	defer be.Close()

	opts, err := g.repoOptions()
	if err != nil {
		return err
	}

	// The keyfile is written before the repository exists. In the other order,
	// a failure to write the keyfile would leave behind a repository that no key
	// can ever open; this way the worst case is a stray keyfile, removed below.
	if err := key.SaveKeyfile(keyfilePath, passphrase); err != nil {
		return err
	}

	r, err := repo.Init(ctx, be, key, opts)
	if err != nil {
		if rmErr := os.Remove(keyfilePath); rmErr != nil {
			return fmt.Errorf("%w (the new keyfile %s could not be removed: %v)", err, keyfilePath, rmErr)
		}
		return err
	}

	fmt.Fprintf(out, "Created repository %s\n", be.Location())
	fmt.Fprintf(out, "Repository ID: %s\n", r.Config().ID)
	fmt.Fprintf(out, "Keyfile:       %s\n", keyfilePath)
	fmt.Fprint(out, "\nKeep the keyfile and its passphrase safe, and store them separately from\n"+
		"the repository. Neither can be recovered, and without them the backups\n"+
		"cannot be read.\n")
	return nil
}

func runBackup(ctx context.Context, args []string, out, errOut io.Writer) error {
	var (
		g        globalFlags
		excludes stringList
		tags     stringList
		parent   string
		quiet    bool
		conc     int
	)
	fs := newFlagSet("backup", errOut, `gobackup backup - create a snapshot

Usage:
  gobackup backup [flags] PATH...

Reads the given paths, uploads whatever is not already stored, and records a
snapshot. Files whose metadata is unchanged since the parent snapshot are not
read at all, and changed files cost only the chunks that actually differ.
`)
	g.register(fs)
	fs.Var(&excludes, "exclude", "glob pattern to skip; may be repeated")
	fs.Var(&tags, "tag", "tag to attach to the snapshot; may be repeated")
	fs.StringVar(&parent, "parent", "", "snapshot to compare against (default: the most recent matching snapshot)")
	fs.BoolVar(&quiet, "quiet", false, "suppress progress output")
	fs.IntVar(&conc, "concurrency", archiver.DefaultConcurrency, "number of files to read at once")
	if err := fs.Parse(args); err != nil {
		return ErrUsage
	}

	paths := fs.Args()
	if len(paths) == 0 {
		fmt.Fprintln(errOut, "gobackup backup: at least one path is required")
		fs.Usage()
		return ErrUsage
	}

	r, err := g.openRepository(ctx, errOut)
	if err != nil {
		return err
	}
	defer r.Close()

	hostname, _ := os.Hostname()
	username := ""
	if u, err := user.Current(); err == nil {
		username = u.Username
	}

	opts := archiver.Options{
		Excludes:    excludes,
		Tags:        tags,
		Hostname:    hostname,
		Username:    username,
		Concurrency: conc,
		OnError: func(path string, err error) error {
			// A few unreadable files should not abandon an otherwise good
			// backup, but the user needs to know they were skipped.
			fmt.Fprintf(errOut, "warning: skipping %s: %v\n", path, err)
			return nil
		},
	}

	if err := resolveParent(ctx, r, &opts, parent, paths, hostname, out, quiet); err != nil {
		return err
	}

	if !quiet {
		// The archiver calls this from its worker goroutines, so the throttle
		// state needs its own lock.
		var (
			progressMu sync.Mutex
			lastReport time.Time
		)
		opts.Progress = func(p archiver.Progress) {
			progressMu.Lock()
			defer progressMu.Unlock()

			// Throttled so progress does not flood the terminal.
			if time.Since(lastReport) < 100*time.Millisecond {
				return
			}
			lastReport = time.Now()
			fmt.Fprintf(errOut, "\r%d files, %s processed, %s new    ",
				p.FilesProcessed, humanBytes(p.BytesProcessed), humanBytes(p.BytesAdded))
		}
	}

	start := time.Now()
	snap, err := archiver.New(r, opts).Snapshot(ctx, paths)
	if !quiet {
		fmt.Fprint(errOut, "\r                                                            \r")
	}
	if err != nil {
		return err
	}

	s := snap.Stats
	fmt.Fprintf(out, "Snapshot %s created in %s\n", snap.ID.Short(), time.Since(start).Round(time.Millisecond))
	fmt.Fprintf(out, "  Files:     %d new, %d changed, %d unchanged\n", s.FilesNew, s.FilesChanged, s.FilesUnmodified)
	fmt.Fprintf(out, "  Dirs:      %d\n", s.DirsProcessed)
	fmt.Fprintf(out, "  Processed: %s\n", humanBytes(s.BytesProcessed))
	fmt.Fprintf(out, "  Uploaded:  %s\n", humanBytes(s.BytesAdded))
	if s.BytesProcessed > 0 {
		saved := 100 * (1 - float64(s.BytesAdded)/float64(s.BytesProcessed))
		fmt.Fprintf(out, "  Saved:     %.1f%% through unchanged files and deduplication\n", saved)
	}
	return nil
}

// resolveParent selects the snapshot this run compares against.
func resolveParent(ctx context.Context, r *repo.Repository, opts *archiver.Options,
	ref string, paths []string, hostname string, out io.Writer, quiet bool) error {

	if ref != "" {
		parent, err := r.FindSnapshot(ctx, ref)
		if err != nil {
			return err
		}
		opts.Parent = parent
		return nil
	}

	snapshots, err := r.ListSnapshots(ctx)
	if err != nil {
		return err
	}

	// The most recent snapshot of the same paths on the same host is the one
	// whose tree will line up, which is what makes the fast path effective.
	absPaths := make([]string, 0, len(paths))
	for _, p := range paths {
		abs, err := snapshot.CleanPath(p)
		if err != nil {
			return err
		}
		absPaths = append(absPaths, abs)
	}
	sort.Strings(absPaths)

	if parent := snapshot.FindParent(snapshots, hostname, absPaths); parent != nil {
		opts.Parent = parent
		if !quiet {
			fmt.Fprintf(out, "Comparing against snapshot %s\n", parent.ID.Short())
		}
	}
	return nil
}

func runList(ctx context.Context, args []string, out, errOut io.Writer) error {
	var (
		g        globalFlags
		snapRef  string
		longForm bool
	)
	fs := newFlagSet("list", errOut, `gobackup list - list snapshots or their contents

Usage:
  gobackup list [flags]
  gobackup list -snapshot ID [-long]

With no snapshot given, lists every snapshot in the repository. With one, lists
the files it contains.
`)
	g.register(fs)
	fs.StringVar(&snapRef, "snapshot", "", "list the files in this snapshot; accepts an ID prefix or \"latest\"")
	fs.BoolVar(&longForm, "long", false, "show mode, size, and modification time")
	if err := fs.Parse(args); err != nil {
		return ErrUsage
	}

	r, err := g.openRepository(ctx, errOut)
	if err != nil {
		return err
	}
	defer r.Close()

	if snapRef != "" {
		snap, err := r.FindSnapshot(ctx, snapRef)
		if err != nil {
			return err
		}
		return listFiles(ctx, r, snap, longForm, out)
	}

	snapshots, err := r.ListSnapshots(ctx)
	if err != nil {
		return err
	}
	if len(snapshots) == 0 {
		fmt.Fprintln(out, "The repository has no snapshots.")
		return nil
	}

	fmt.Fprintf(out, "%-10s  %-19s  %-16s  %-10s  %s\n", "ID", "TIME", "HOST", "SIZE", "PATHS")
	for _, s := range snapshots {
		paths := strings.Join(s.Paths, ", ")
		if len(s.Tags) > 0 {
			paths += "  [" + strings.Join(s.Tags, ",") + "]"
		}
		fmt.Fprintf(out, "%-10s  %-19s  %-16s  %-10s  %s\n",
			s.ID.Short(), s.Time.Local().Format("2006-01-02 15:04:05"),
			truncate(s.Hostname, 16), humanBytes(s.Stats.BytesProcessed), paths)
	}
	return nil
}

// listFiles walks a snapshot and prints every entry it contains.
func listFiles(ctx context.Context, r *repo.Repository, snap *snapshot.Snapshot, longForm bool, out io.Writer) error {
	var walk func(treeID crypto.ID, prefix string) error
	walk = func(treeID crypto.ID, prefix string) error {
		tree, err := r.LoadTree(ctx, treeID)
		if err != nil {
			return err
		}
		for _, node := range tree.Nodes {
			path := prefix + node.Name

			switch node.Type {
			case snapshot.NodeDir:
				printNode(out, path+"/", node, longForm)
				if node.Subtree != nil {
					if err := walk(*node.Subtree, path+"/"); err != nil {
						return err
					}
				}
			case snapshot.NodeSymlink:
				printNode(out, path+" -> "+node.LinkTarget, node, longForm)
			default:
				printNode(out, path, node, longForm)
			}
		}
		return nil
	}
	return walk(snap.Tree, "/")
}

func printNode(out io.Writer, display string, node *snapshot.Node, longForm bool) {
	if !longForm {
		fmt.Fprintln(out, display)
		return
	}
	fmt.Fprintf(out, "%-11s %10s  %s  %s\n",
		node.FileMode().String(), humanBytes(node.Size),
		node.ModTime.Local().Format("2006-01-02 15:04:05"), display)
}

func runRestore(ctx context.Context, args []string, out, errOut io.Writer) error {
	var (
		g         globalFlags
		snapRef   string
		target    string
		include   stringList
		exclude   stringList
		overwrite bool
		conc      int
	)
	fs := newFlagSet("restore", errOut, `gobackup restore - restore files from a snapshot

Usage:
  gobackup restore -snapshot ID -target DIR [flags]

Files are written under the target directory, reproducing their original
absolute paths. Existing files are left untouched unless -overwrite is given.
`)
	g.register(fs)
	fs.StringVar(&snapRef, "snapshot", "latest", "snapshot to restore; accepts an ID prefix or \"latest\"")
	fs.StringVar(&target, "target", "", "directory to restore into (required)")
	fs.Var(&include, "include", "glob pattern to restore; may be repeated. Without any, everything is restored")
	fs.Var(&exclude, "exclude", "glob pattern to skip; may be repeated")
	fs.BoolVar(&overwrite, "overwrite", false, "replace files that already exist in the target")
	fs.IntVar(&conc, "concurrency", restorer.DefaultConcurrency, "number of parallel pack reads")
	if err := fs.Parse(args); err != nil {
		return ErrUsage
	}

	if target == "" {
		fmt.Fprintln(errOut, "gobackup restore: -target is required")
		fs.Usage()
		return ErrUsage
	}

	r, err := g.openRepository(ctx, errOut)
	if err != nil {
		return err
	}
	defer r.Close()

	snap, err := r.FindSnapshot(ctx, snapRef)
	if err != nil {
		return err
	}

	fmt.Fprintf(out, "Restoring snapshot %s from %s\n",
		snap.ID.Short(), snap.Time.Local().Format("2006-01-02 15:04:05"))

	start := time.Now()
	stats, err := restorer.New(r, restorer.Options{
		Include:     include,
		Exclude:     exclude,
		Overwrite:   overwrite,
		Concurrency: conc,
	}).RestoreSnapshot(ctx, snap, target)
	if err != nil {
		return err
	}

	fmt.Fprintf(out, "Restored %d files, %d directories, %d symlinks",
		stats.Files, stats.Dirs, stats.Symlinks)
	if stats.Hardlinks > 0 {
		fmt.Fprintf(out, ", %d hardlinks", stats.Hardlinks)
	}
	fmt.Fprintf(out, " (%s) in %s\n", humanBytes(stats.Bytes), time.Since(start).Round(time.Millisecond))
	return nil
}

func runVerify(ctx context.Context, args []string, out, errOut io.Writer) error {
	var (
		g        globalFlags
		readData bool
		snapRef  string
		conc     int
	)
	fs := newFlagSet("verify", errOut, `gobackup verify - check repository integrity

Usage:
  gobackup verify [flags]

By default only metadata is checked: that every chunk referenced by every
snapshot resolves to a pack that exists and is large enough to hold it. That is
cheap and catches missing or truncated packs.

With -read-data every pack is downloaded, decrypted, and matched against the
chunk IDs recomputed from its contents, which additionally detects silent
corruption in storage. That costs a full download of the repository.
`)
	g.register(fs)
	fs.BoolVar(&readData, "read-data", false, "download and verify every pack, not just metadata")
	fs.StringVar(&snapRef, "snapshot", "", "check only this snapshot (default: all)")
	fs.IntVar(&conc, "concurrency", verify.DefaultConcurrency, "number of parallel pack reads")
	if err := fs.Parse(args); err != nil {
		return ErrUsage
	}

	r, err := g.openRepository(ctx, errOut)
	if err != nil {
		return err
	}
	defer r.Close()

	opts := verify.Options{
		ReadData:    readData,
		Concurrency: conc,
		Progress:    func(msg string) { fmt.Fprintf(errOut, "%s...\n", msg) },
	}
	if snapRef != "" {
		snap, err := r.FindSnapshot(ctx, snapRef)
		if err != nil {
			return err
		}
		opts.Snapshots = []*snapshot.Snapshot{snap}
	}

	report, err := verify.Run(ctx, r, opts)
	if err != nil {
		return err
	}

	fmt.Fprintf(out, "\nSnapshots checked: %d\n", report.SnapshotsChecked)
	fmt.Fprintf(out, "Trees checked:     %d\n", report.TreesChecked)
	fmt.Fprintf(out, "Chunk references:  %d\n", report.BlobsReferenced)
	fmt.Fprintf(out, "Packs in storage:  %d (%d referenced)\n", report.PacksInStorage, report.PacksReferenced)
	if readData {
		fmt.Fprintf(out, "Data read:         %s\n", humanBytes(uint64(report.BytesRead)))
	}

	if len(report.OrphanPacks) > 0 {
		fmt.Fprintf(out, "\n%d unreferenced pack(s) are occupying space. These are normally left\n"+
			"behind by an interrupted backup and are harmless.\n", len(report.OrphanPacks))
	}

	if report.OK() {
		fmt.Fprintln(out, "\nNo problems found.")
		return nil
	}

	fmt.Fprintf(out, "\n%d problem(s) found:\n", len(report.Problems))
	for _, p := range report.Problems {
		fmt.Fprintf(out, "  - %s\n", p)
	}
	return errors.New("the repository failed verification")
}

// humanBytes formats a byte count for display.
func humanBytes(n uint64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := uint64(unit), 0
	for size := n / unit; size >= unit; size /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	if max <= 1 {
		return s[:max]
	}
	return s[:max-1] + "…"
}
