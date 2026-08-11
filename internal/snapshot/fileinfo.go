package snapshot

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

// NodeFromFileInfo builds a node describing a filesystem entry.
//
// path is used to read a symlink's target; name is the entry's name within its
// parent directory. Content and Subtree are filled in by the caller once the
// file has been chunked or the directory has been serialized.
func NodeFromFileInfo(path, name string, fi fs.FileInfo) (*Node, error) {
	node := &Node{
		Name: name,
		Mode: fi.Mode().Perm(),
		Size: uint64(fi.Size()),
		// Times are normalized to UTC so that the same file produces the same
		// tree bytes regardless of the machine's time zone, which deduplication
		// depends on.
		ModTime: fi.ModTime().UTC(),
	}

	switch {
	case fi.Mode()&fs.ModeSymlink != 0:
		node.Type = NodeSymlink
		target, err := os.Readlink(path)
		if err != nil {
			return nil, fmt.Errorf("snapshot: reading symlink %s: %w", path, err)
		}
		node.LinkTarget = target
		node.Size = 0

	case fi.IsDir():
		node.Type = NodeDir
		node.Size = 0

	case fi.Mode().IsRegular():
		node.Type = NodeFile

	default:
		// Sockets, devices, and FIFOs have no meaningful content to back up.
		return nil, fmt.Errorf("snapshot: %s is an unsupported file type (%s)", path, fi.Mode().Type())
	}

	fillSystemInfo(node, fi)
	return node, nil
}

// SameContent reports whether a node from a previous snapshot still describes
// the file on disk, meaning its chunk list can be reused without reading the
// file at all.
//
// This test is the heart of incremental backup. It deliberately checks more than
// modification time:
//
//   - size, because a changed length always means changed content;
//   - modification time, the usual signal;
//   - inode change time, which ordinary programs cannot set, so it catches edits
//     that deliberately or accidentally restore the old mtime;
//   - inode number, which catches a path being replaced by a different file, for
//     example when an editor writes a new file and renames it into place;
//   - mode, so permission changes are recorded.
//
// The remaining blind spot is a file modified within the timestamp resolution of
// its own metadata, which is why verify exists.
func SameContent(old *Node, current *Node) bool {
	if old == nil || current == nil {
		return false
	}
	if old.Type != NodeFile || current.Type != NodeFile {
		return false
	}
	if old.Size != current.Size {
		return false
	}
	if !old.ModTime.Equal(current.ModTime) {
		return false
	}
	if old.Mode != current.Mode {
		return false
	}
	// Inode and change time are only compared when both snapshots recorded
	// them, so a repository written on a platform that cannot supply them still
	// works, just with weaker change detection.
	if old.Inode != 0 && current.Inode != 0 && old.Inode != current.Inode {
		return false
	}
	if !old.ChangeTime.IsZero() && !current.ChangeTime.IsZero() && !old.ChangeTime.Equal(current.ChangeTime) {
		return false
	}
	return true
}

// IsHardlink reports whether a node is one of several links to the same inode.
func (n *Node) IsHardlink() bool {
	return n.Type == NodeFile && n.Links > 1 && n.Inode != 0
}

// FileMode returns the node's permissions combined with the type bits implied
// by its node type. Only permission bits are stored, since the type is already
// recorded separately, but listings want the familiar combined form.
func (n *Node) FileMode() fs.FileMode {
	switch n.Type {
	case NodeDir:
		return fs.ModeDir | n.Mode.Perm()
	case NodeSymlink:
		return fs.ModeSymlink | n.Mode.Perm()
	default:
		return n.Mode.Perm()
	}
}

// RestoreMetadata applies a node's recorded ownership, permissions, and
// timestamps to an extracted path.
//
// Ownership is attempted but not required: restoring another user's files
// without privileges is a normal situation, and failing the whole restore over
// it would be unhelpful. Timestamps are applied last, because writing file
// content updates them.
func RestoreMetadata(path string, node *Node) error {
	if node.Type == NodeSymlink {
		// A symlink's own metadata cannot be set portably, and chmod/chtimes
		// would follow the link and modify the target instead.
		return nil
	}

	if err := os.Chmod(path, node.Mode); err != nil {
		return fmt.Errorf("snapshot: setting permissions on %s: %w", path, err)
	}

	// Best effort; lack of privilege is expected and not an error.
	_ = os.Chown(path, int(node.UID), int(node.GID))

	atime := node.AccessTime
	if atime.IsZero() {
		atime = node.ModTime
	}
	if err := os.Chtimes(path, atime, node.ModTime); err != nil {
		return fmt.Errorf("snapshot: setting timestamps on %s: %w", path, err)
	}
	return nil
}

// RestoreDirMetadata is like RestoreMetadata but intended to run after a
// directory's children have been written, since creating them changes the
// directory's own timestamps.
func RestoreDirMetadata(path string, node *Node) error {
	return RestoreMetadata(path, node)
}

// CleanPath returns an absolute, lexically clean path.
func CleanPath(p string) (string, error) {
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", fmt.Errorf("snapshot: resolving %s: %w", p, err)
	}
	return filepath.Clean(abs), nil
}

// timespecToTime converts a Unix timespec to a UTC time, mapping a zero
// timespec onto the zero time so that "unknown" stays distinguishable.
func timespecToTime(sec, nsec int64) time.Time {
	if sec == 0 && nsec == 0 {
		return time.Time{}
	}
	return time.Unix(sec, nsec).UTC()
}
