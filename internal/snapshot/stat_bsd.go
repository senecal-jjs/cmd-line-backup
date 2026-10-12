//go:build darwin || freebsd || netbsd || openbsd || dragonfly

package snapshot

import (
	"io/fs"
	"syscall"
)

// fillSystemInfo records the Unix metadata that change detection, ownership
// restoration, and hardlink handling rely on.
//
// The BSD-derived systems, macOS included, spell the timestamp fields
// differently from Linux, which is why this lives in its own file.
func fillSystemInfo(node *Node, fi fs.FileInfo) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return
	}

	node.UID = st.Uid
	node.GID = st.Gid
	node.Inode = st.Ino
	node.Device = uint64(st.Dev)
	node.Links = uint64(st.Nlink)
	node.ChangeTime = timespecToTime(st.Ctimespec.Sec, st.Ctimespec.Nsec)
}
