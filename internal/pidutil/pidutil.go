// Package pidutil contains small process helpers shared across GC packages.
package pidutil

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const psZombieTimeout = 100 * time.Millisecond

// Alive reports whether a PID exists and is not a zombie.
func Alive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	if err != nil && !errors.Is(err, syscall.EPERM) {
		return false
	}
	statPath := filepath.Join("/proc", strconv.Itoa(pid), "stat")
	data, err := os.ReadFile(statPath)
	if err != nil {
		return !psReportsZombie(pid)
	}
	fields := strings.Fields(string(data))
	if len(fields) >= 3 && fields[2] == "Z" {
		return false
	}
	return true
}

func psReportsZombie(pid int) bool {
	ctx, cancel := context.WithTimeout(context.Background(), psZombieTimeout)
	defer cancel()

	out, err := exec.CommandContext(ctx, "ps", "-o", "stat=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return false
	}
	state := strings.TrimSpace(string(out))
	return strings.HasPrefix(state, "Z")
}
