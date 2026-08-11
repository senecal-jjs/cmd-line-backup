//go:build !linux && !darwin && !freebsd && !netbsd && !openbsd && !dragonfly

package snapshot

import "io/fs"

// fillSystemInfo is a no-op on platforms that do not expose Unix inode
// metadata. Backups still work; change detection falls back to size,
// modification time, and mode, and hardlinks are stored as independent files.
func fillSystemInfo(node *Node, fi fs.FileInfo) {}
