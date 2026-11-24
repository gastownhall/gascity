//go:build darwin || linux || freebsd || openbsd || netbsd || dragonfly

package sessionlog

import (
	"os"
	"syscall"
)

func searchRootOwnedByProcess(info os.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && int(stat.Uid) == os.Geteuid()
}
