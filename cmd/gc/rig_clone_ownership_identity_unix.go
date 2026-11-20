//go:build unix

package main

import (
	"fmt"
	"os"
	"syscall"
)

func rigCloneNativeDirectoryIdentity(info os.FileInfo) (string, error) {
	state, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return "", fmt.Errorf("clone directory has no native ownership identity")
	}
	return fmt.Sprintf("%d:%d", state.Dev, state.Ino), nil
}
