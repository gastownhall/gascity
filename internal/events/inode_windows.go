//go:build windows

package events

import "io/fs"

// inodeOf is the Windows fallback for the watcher's rotation detector.
// Windows surfaces no equivalent of inode through fs.FileInfo, so we
// approximate with size+modtime: the watcher uses a non-zero return
// value to mean "different file" only when both mtime and size match
// would coincidentally collide, which is unlikely in practice and
// short-circuited by the test+CI matrix being Linux/macOS-only today.
func inodeOf(info fs.FileInfo) uint64 {
	mt := info.ModTime().UnixNano()
	return uint64(mt) ^ uint64(info.Size())
}
