// Package snapshot defines gobackup's backup metadata: the directory trees that
// describe what was backed up, and the snapshot objects that name a backup run.
//
// # Trees
//
// A directory is serialized as a tree blob listing its immediate children. Each
// child either points at a list of content chunk IDs (a file) or at another tree
// blob (a subdirectory). Trees are stored in packs exactly like file data, which
// means they are encrypted, compressed, and deduplicated the same way: a
// directory that has not changed since the last backup produces an identical
// tree blob and costs nothing to store again.
//
// Determinism is therefore part of the format. Entries are sorted by name and
// timestamps are normalized to UTC so that identical directory state always
// yields identical bytes; otherwise deduplication would silently stop working.
//
// # Snapshots
//
// A snapshot names one backup run: when it happened, what paths it covered, and
// the ID of its root tree. It is written last, after every pack and index it
// depends on, so a snapshot that is visible in the repository is always complete.
package snapshot

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"sort"
	"time"

	"github.com/senecal-jjs/cmd-line-backup/internal/crypto"
)

// NodeType distinguishes the kinds of filesystem entries gobackup stores.
type NodeType string

const (
	NodeFile    NodeType = "file"
	NodeDir     NodeType = "dir"
	NodeSymlink NodeType = "symlink"
)

// Valid reports whether t is a supported node type.
func (t NodeType) Valid() bool {
	return t == NodeFile || t == NodeDir || t == NodeSymlink
}

// A Node is one entry within a directory.
type Node struct {
	Name string   `json:"name"`
	Type NodeType `json:"type"`

	Mode fs.FileMode `json:"mode"`
	UID  uint32      `json:"uid"`
	GID  uint32      `json:"gid"`
	Size uint64      `json:"size"`

	// ModTime and ChangeTime drive the incremental fast path. ModTime is
	// restored onto the extracted file; ChangeTime (the inode change time) is
	// only used for comparison, because unlike ModTime it cannot be set by
	// ordinary programs and so is much harder to spoof.
	ModTime    time.Time `json:"mtime"`
	AccessTime time.Time `json:"atime,omitzero"`
	ChangeTime time.Time `json:"ctime,omitzero"`

	// LinkTarget is the destination of a symlink.
	LinkTarget string `json:"link_target,omitempty"`

	// Content lists the chunk IDs making up a file, in order.
	Content []crypto.ID `json:"content,omitempty"`

	// Subtree is the tree blob describing a directory's children.
	Subtree *crypto.ID `json:"subtree,omitempty"`

	// Device and Inode identify the file on its filesystem. They are used to
	// detect hardlinks and to notice that a path now refers to a different file.
	Device uint64 `json:"device,omitempty"`
	Inode  uint64 `json:"inode,omitempty"`
	Links  uint64 `json:"links,omitempty"`
}

// A Tree is the serialized content of one directory.
type Tree struct {
	Nodes []*Node `json:"nodes"`
}

// ErrBadFormat reports malformed snapshot metadata.
var ErrBadFormat = errors.New("snapshot: malformed metadata")

// NewTree builds a tree from a set of nodes, sorting them into the canonical
// order required for deduplication.
func NewTree(nodes []*Node) *Tree {
	sorted := append([]*Node(nil), nodes...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Name < sorted[j].Name })
	return &Tree{Nodes: sorted}
}

// Find returns the node with the given name.
func (t *Tree) Find(name string) (*Node, bool) {
	for _, n := range t.Nodes {
		if n.Name == name {
			return n, true
		}
	}
	return nil, false
}

// EncodeTree serializes a tree.
//
// The result must depend only on the directory's contents, never on the order
// the filesystem happened to return entries in or on the machine's time zone.
func EncodeTree(t *Tree) ([]byte, error) {
	canonical := &Tree{Nodes: append([]*Node(nil), t.Nodes...)}
	sort.Slice(canonical.Nodes, func(i, j int) bool {
		return canonical.Nodes[i].Name < canonical.Nodes[j].Name
	})

	data, err := json.Marshal(canonical)
	if err != nil {
		return nil, fmt.Errorf("snapshot: encoding tree: %w", err)
	}
	return data, nil
}

// DecodeTree parses a tree blob.
func DecodeTree(data []byte) (*Tree, error) {
	var t Tree
	if err := json.Unmarshal(data, &t); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrBadFormat, err)
	}
	for i, n := range t.Nodes {
		if n == nil {
			return nil, fmt.Errorf("%w: tree entry %d is null", ErrBadFormat, i)
		}
		if n.Name == "" {
			return nil, fmt.Errorf("%w: tree entry %d has no name", ErrBadFormat, i)
		}
		if !n.Type.Valid() {
			return nil, fmt.Errorf("%w: entry %q has unknown type %q", ErrBadFormat, n.Name, n.Type)
		}
		if n.Type == NodeDir && n.Subtree == nil {
			return nil, fmt.Errorf("%w: directory %q has no subtree reference", ErrBadFormat, n.Name)
		}
	}
	return &t, nil
}

// Stats summarizes what a backup run did. They are informational only.
type Stats struct {
	FilesNew        uint64 `json:"files_new"`
	FilesChanged    uint64 `json:"files_changed"`
	FilesUnmodified uint64 `json:"files_unmodified"`
	DirsProcessed   uint64 `json:"dirs_processed"`

	// BytesProcessed is the total size of the files considered; BytesAdded is
	// how much new data actually reached the repository. The gap between them
	// is what deduplication and the unchanged-file fast path saved.
	BytesProcessed uint64 `json:"bytes_processed"`
	BytesAdded     uint64 `json:"bytes_added"`

	ChunksNew       uint64 `json:"chunks_new"`
	ChunksDuplicate uint64 `json:"chunks_duplicate"`
}

// A Snapshot records one backup run.
type Snapshot struct {
	// ID is the snapshot object's name. It is assigned when the snapshot is
	// saved and is not part of the serialized form.
	ID crypto.ID `json:"-"`

	Time     time.Time `json:"time"`
	Hostname string    `json:"hostname,omitempty"`
	Username string    `json:"username,omitempty"`
	Paths    []string  `json:"paths"`
	Tags     []string  `json:"tags,omitempty"`

	// Tree is the root directory of the snapshot.
	Tree crypto.ID `json:"tree"`

	// Parent is the snapshot this run was compared against, if any. It records
	// the incremental lineage and is what the next run looks for by default.
	Parent *crypto.ID `json:"parent,omitempty"`

	Stats Stats `json:"stats"`
}

// Validate checks that a snapshot is internally consistent.
func (s *Snapshot) Validate() error {
	if s.Time.IsZero() {
		return fmt.Errorf("%w: snapshot has no timestamp", ErrBadFormat)
	}
	if len(s.Paths) == 0 {
		return fmt.Errorf("%w: snapshot lists no paths", ErrBadFormat)
	}
	if s.Tree.IsZero() {
		return fmt.Errorf("%w: snapshot has no root tree", ErrBadFormat)
	}
	return nil
}

// encode serializes a snapshot object.
func encode(s *Snapshot) ([]byte, error) {
	data, err := json.Marshal(s)
	if err != nil {
		return nil, fmt.Errorf("snapshot: encoding snapshot: %w", err)
	}
	return data, nil
}

// decode parses a snapshot object.
func decode(data []byte, id crypto.ID) (*Snapshot, error) {
	var s Snapshot
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrBadFormat, err)
	}
	s.ID = id
	if err := s.Validate(); err != nil {
		return nil, err
	}
	return &s, nil
}
