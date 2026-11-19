//go:build linux

package proctable

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/gastownhall/gascity/internal/pidutil"
)

// rootArgv reads pid's argv from the same procfs root the scanner enumerates.
func rootArgv(pid int) ([]string, error) {
	return readArgv(scanRoot, pid)
}

// readArgv reads pid's argv from the procfs at root.
func readArgv(root string, pid int) ([]string, error) {
	data, err := os.ReadFile(filepath.Join(root, strconv.Itoa(pid), "cmdline"))
	if err != nil {
		return nil, err
	}
	trimmed := strings.TrimRight(string(data), "\x00")
	if trimmed == "" {
		return nil, nil
	}
	return pidutil.NormalizeArgv(strings.Split(trimmed, "\x00")), nil
}

func rootStartIdentity(pid int) string {
	stat, ok, err := readProcStat(filepath.Join(scanRoot, strconv.Itoa(pid), "stat"))
	if err != nil || !ok {
		return ""
	}
	return stat.startTime
}
