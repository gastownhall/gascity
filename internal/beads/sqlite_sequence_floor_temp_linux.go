//go:build linux

package beads

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

func sqliteSequenceFloorTempPattern(dir, base string) (string, error) {
	ownerPrefix := base + strconv.Itoa(os.Geteuid()) + "-"
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", fmt.Errorf("reading SQLite sequence-floor directory: %w", err)
	}
	for _, entry := range entries {
		if entry.Type()&os.ModeSymlink != 0 || !strings.HasPrefix(entry.Name(), ownerPrefix) {
			continue
		}
		remainder := strings.TrimPrefix(entry.Name(), ownerPrefix)
		separator := strings.IndexByte(remainder, '-')
		if separator <= 0 {
			continue
		}
		pid, err := strconv.Atoi(remainder[:separator])
		if err != nil || pid <= 0 || sqliteSequenceFloorProcessAlive(pid) {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			return "", fmt.Errorf("stating stale SQLite sequence-floor temporary file: %w", err)
		}
		if !info.Mode().IsRegular() {
			continue
		}
		if err := os.Remove(filepath.Join(dir, entry.Name())); err != nil && !errors.Is(err, os.ErrNotExist) {
			return "", fmt.Errorf("removing stale SQLite sequence-floor temporary file: %w", err)
		}
	}
	return ownerPrefix + strconv.Itoa(os.Getpid()) + "-*", nil
}

func sqliteSequenceFloorProcessAlive(pid int) bool {
	err := unix.Kill(pid, 0)
	return err == nil || errors.Is(err, unix.EPERM)
}
