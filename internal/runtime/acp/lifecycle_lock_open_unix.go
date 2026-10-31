//go:build !windows

package acp

import (
	"os"
	"syscall"
)

func openLifecycleLockFile(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0o600)
}
