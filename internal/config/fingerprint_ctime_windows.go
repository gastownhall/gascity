//go:build windows

package config

import "os"

// statCtimeNanos is the Windows fallback: Windows exposes no ctime through
// os.FileInfo, so this approximates a "did metadata change independently of
// content" signal by XORing mtime with size. This adds no discriminating
// power beyond what size+mtime already provide in the fingerprint — it is a
// documented, intentional gap, not a silent one. See inode_windows.go for
// the same fallback pattern applied to the event-watcher's rotation detector.
func statCtimeNanos(info os.FileInfo) (int64, bool) {
	return info.ModTime().UnixNano() ^ info.Size(), true
}
