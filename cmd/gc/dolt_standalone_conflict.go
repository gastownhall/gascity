package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/gastownhall/gascity/internal/pidutil"
)

// standaloneBdDoltPIDPath is the path bd-standalone writes when it manages
// its own dolt server. The city-managed dolt lifecycle (gc-beads-bd.sh)
// instead writes its PID under the per-pack runtime state directory, so
// the presence of this file with a live PID is a reliable indicator that
// the operator started Dolt out-of-band via `bd dolt start` while a city
// was registered at cityPath. Both sides want exclusive write access to
// the same .beads/dolt database, so the second to start loses with a
// generic "dolt server could not start via gc helper" error.
func standaloneBdDoltPIDPath(cityPath string) string {
	return filepath.Join(cityPath, ".beads", "dolt-server.pid")
}

// detectStandaloneBdDolt reports whether a bd-standalone dolt server is
// currently holding the .beads/dolt-server lock at cityPath. The PID is
// returned when the pid file exists and contains a parseable integer,
// regardless of liveness, so callers can include it in error messages
// or stale-file warnings.
//
// Returns (0, false, nil) when no pid file is present or it is empty.
// Returns (pid, false, nil) when the file is present but the PID is dead
// (stale leftover from a previous bd-standalone session that did not
// clean up). Returns a non-nil error only on unexpected I/O failure or
// malformed pid contents.
//
// pidAliveFn is injected for tests so liveness can be stubbed without
// spawning real processes. Production callers use detectStandaloneBdDolt.
func detectStandaloneBdDoltWithAlive(cityPath string, pidAliveFn func(int) bool) (pid int, alive bool, err error) {
	raw, readErr := os.ReadFile(standaloneBdDoltPIDPath(cityPath))
	if readErr != nil {
		if errors.Is(readErr, os.ErrNotExist) {
			return 0, false, nil
		}
		return 0, false, fmt.Errorf("read %s: %w", standaloneBdDoltPIDPath(cityPath), readErr)
	}
	s := strings.TrimSpace(string(raw))
	if s == "" {
		return 0, false, nil
	}
	pid, err = strconv.Atoi(s)
	if err != nil {
		return 0, false, fmt.Errorf("parse pid in %s: %w", standaloneBdDoltPIDPath(cityPath), err)
	}
	if pid <= 0 {
		return pid, false, nil
	}
	return pid, pidAliveFn(pid), nil
}

func detectStandaloneBdDolt(cityPath string) (pid int, alive bool, err error) {
	return detectStandaloneBdDoltWithAlive(cityPath, pidutil.Alive)
}

// standaloneBdDoltConflictError formats the actionable error returned
// from startBeadsLifecycle when a bd-standalone dolt is detected. The
// message names the unblock command (`bd dolt stop`) explicitly so the
// operator does not have to guess from a generic "dolt server could not
// start" failure.
func standaloneBdDoltConflictError(cityPath string, pid int) error {
	return fmt.Errorf(
		"bd-managed dolt server is already running at %s (pid %d, lock file %s); "+
			"it holds an exclusive write lock on the .beads/dolt database that the city-managed dolt "+
			"cannot acquire. Run \"bd dolt stop\" to release the lock, then retry \"gc start\"",
		cityPath, pid, standaloneBdDoltPIDPath(cityPath),
	)
}
