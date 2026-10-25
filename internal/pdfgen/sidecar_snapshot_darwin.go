package pdfgen

import (
	"io/fs"
	"syscall"
)

// fileSnapshotOf extracts the identity snapshot from an Lstat or Stat result.
func fileSnapshotOf(info fs.FileInfo) (fileSnapshot, bool) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat == nil {
		return fileSnapshot{}, false
	}
	return fileSnapshot{
		device: uint64(stat.Dev), inode: stat.Ino, size: info.Size(), mode: info.Mode(),
		uid: stat.Uid, gid: stat.Gid,
		modified: stat.Mtimespec.Nano(), changed: stat.Ctimespec.Nano(),
	}, true
}
