//go:build !(darwin || linux || freebsd || openbsd || netbsd || dragonfly)

package sessionlog

import "os"

func searchRootOwnedByProcess(os.FileInfo) bool {
	return false
}
