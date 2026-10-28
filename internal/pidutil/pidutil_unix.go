//go:build !windows

package pidutil

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
)

func platformAlive(pid int) bool {
	err := syscall.Kill(pid, 0)
	if err != nil && !errors.Is(err, syscall.EPERM) {
		return false
	}
	statPath := filepath.Join("/proc", strconv.Itoa(pid), "stat")
	data, err := os.ReadFile(statPath)
	if err != nil {
		return !psReportsZombie(pid)
	}
	state, ok := procStatState(string(data))
	if ok && state == "Z" {
		return false
	}
	return true
}

func platformStartTime(pid int) (string, error) {
	return psStartTime(pid)
}

func platformChildPIDs(parent int) ([]int, error) {
	return psChildPIDs(parent)
}
