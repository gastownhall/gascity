package main

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// sweepOrphanPIDPrefixedDirs removes <root>/<prefix><PID> dirs whose PID
// is no longer alive. Best-effort; ignores errors. Used by TestMain to
// clean leftover test fixtures from prior crashed/SIGKILL'd runs.
func sweepOrphanPIDPrefixedDirs(root, prefix string) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return
	}
	self := os.Getpid()
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasPrefix(name, prefix) {
			continue
		}
		pid, err := strconv.Atoi(strings.TrimPrefix(name, prefix))
		if err != nil || pid <= 0 || pid == self {
			continue
		}
		if pidAlive(pid) {
			continue
		}
		_ = os.RemoveAll(filepath.Join(root, name))
	}
}
