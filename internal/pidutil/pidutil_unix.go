//go:build !windows

package pidutil

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
)

// probeSignal sends a signal for Alive's existence probe. It is a seam so tests
// can model a process owned by another user, whose probe answers EPERM.
var probeSignal = syscall.Kill

func platformAlive(pid int) bool {
	probeErr := probeSignal(pid, 0)
	if probeErr != nil && !errors.Is(probeErr, syscall.EPERM) {
		return false
	}
	statPath := filepath.Join("/proc", strconv.Itoa(pid), "stat")
	data, err := readProcStat(statPath)
	if err != nil {
		// With /proc mounted, a missing entry after a successful kill(0)
		// means the process was reaped in between. The ps zombie probe
		// below finds nothing for a vanished PID and would report it alive.
		// After EPERM the process exists: its entry can be hidden (hidepid)
		// rather than gone, so that case keeps the ps probe.
		if probeErr == nil && errors.Is(err, fs.ErrNotExist) && procMounted() {
			return false
		}
		return !psReportsZombie(pid)
	}
	state, ok := procStatState(string(data))
	if ok && state == "Z" {
		return false
	}
	return true
}

// procMounted reports whether this host serves process state through /proc,
// so a missing /proc/<pid> entry is authoritative.
func procMounted() bool {
	_, err := os.Stat("/proc/self/stat")
	return err == nil
}

func platformStartTime(pid int) (string, error) {
	return psStartTime(pid)
}

func platformChildPIDs(parent int) ([]int, error) {
	return psChildPIDs(parent)
}
