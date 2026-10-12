//go:build linux

package snapshot

import (
	"io/fs"
	"syscall"
)

// fillSystemInfo records the Unix metadata that change detection, ownership
// restoration, and hardlink handling rely on.
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
	node.ChangeTime = timespecToTime(st.Ctim.Sec, st.Ctim.Nsec)
}
