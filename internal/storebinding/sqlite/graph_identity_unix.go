//go:build unix

package sqlite

import (
	"fmt"
	"os"
	"syscall"
)

func platformFileIdentity(info os.FileInfo) string {
	if stat, ok := info.Sys().(*syscall.Stat_t); ok {
		return fmt.Sprintf("dev=%d;ino=%d", stat.Dev, stat.Ino)
	}
	return fmt.Sprintf("mode=%v;size=%d;mtime=%d", info.Mode(), info.Size(), info.ModTime().UnixNano())
}

func platformFileHasMultipleLinks(info os.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && stat.Nlink > 1
}
