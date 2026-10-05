package main

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"strconv"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/doctor"
)

// supervisorBinaryStatusTimeout bounds the /health read the supervisor-binary
// doctor check performs.
const supervisorBinaryStatusTimeout = 3 * time.Second

var (
	// readSupervisorExeStateHook lets tests avoid process-table lookups.
	readSupervisorExeStateHook = readSupervisorExeState
	// supervisorBinaryStatusHook lets tests avoid the /health HTTP read.
	supervisorBinaryStatusHook = readSupervisorBinaryStatus
)

// gatherSupervisorBinary collects the running supervisor's executable and
// build identity for the supervisor-binary doctor check. Probe failures are
// carried on the result so the check can report them.
func gatherSupervisorBinary(pid int) doctor.SupervisorBinary {
	info := doctor.SupervisorBinary{
		LocalBuildID: commit,
		LocalVersion: version,
		RestartHint:  supervisorBinaryRestartHint(),
	}
	info.ExePath, info.ExeDeleted, info.ExeErr = readSupervisorExeStateHook(pid)
	status, err := supervisorBinaryStatusHook()
	if err != nil {
		info.StatusErr = err
		return info
	}
	info.BuildID = status.BuildID
	info.Version = status.Version
	return info
}

func readSupervisorBinaryStatus() (SupervisorStatus, error) {
	baseURL, err := supervisorAPIBaseURLHook()
	if err != nil {
		return SupervisorStatus{}, fmt.Errorf("resolving supervisor API URL: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), supervisorBinaryStatusTimeout)
	defer cancel()
	return newHTTPSupervisorClient(baseURL).Status(ctx)
}

// readSupervisorExeState reports the executable path process pid was started
// from and whether that path has since been removed from disk. On Linux the
// kernel marks an unlinked executable with a " (deleted)" suffix on
// /proc/<pid>/exe. Elsewhere (macOS has no /proc) ps reports the launch path,
// which survives the file's removal, so a missing path means the process is
// running a build that is no longer installed there.
func readSupervisorExeState(pid int) (string, bool, error) {
	if goruntime.GOOS == "linux" {
		target, err := os.Readlink(filepath.Join("/proc", strconv.Itoa(pid), "exe"))
		if err != nil {
			return "", false, err
		}
		if trimmed, ok := strings.CutSuffix(target, " (deleted)"); ok {
			return trimmed, true, nil
		}
		return target, false, nil
	}
	out, err := exec.Command("ps", "-o", "comm=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return "", false, fmt.Errorf("ps -o comm= -p %d: %w", pid, err)
	}
	path := strings.TrimSpace(string(out))
	if !filepath.IsAbs(path) {
		return path, false, fmt.Errorf("ps reported non-absolute executable %q for pid %d", path, pid)
	}
	if _, err := os.Stat(path); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return path, true, nil
		}
		return path, false, err
	}
	return path, false, nil
}

// supervisorBinaryRestartHint is the operator command that restarts the
// supervisor from the current gc binary. Stopping first matters: install is
// a no-op for an unchanged service file while the supervisor is still alive.
func supervisorBinaryRestartHint() string {
	if _, delegated, err := supervisorSystemdDelegation(); err != nil || delegated {
		return supervisorRestartGuidance()
	}
	return "run 'gc supervisor stop --wait', then 'gc supervisor install' (or 'gc supervisor start' if it does not run as a platform service)"
}
