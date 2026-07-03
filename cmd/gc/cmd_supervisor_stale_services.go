package main

// Stale isolated supervisor-service sweep (gascity#3896).
//
// Every `gc supervisor install` under an isolated GC_HOME writes a
// per-home service file (com.gascity.supervisor.<suffix>.plist on
// launchd, gascity-supervisor-<suffix>.service on systemd) with
// RunAtLoad / Restart=always semantics. Test and e2e harnesses that
// crash or are interrupted before their `gc supervisor uninstall`
// teardown leak that service permanently: the service manager
// resurrects it on every login even after its temp GC_HOME is deleted,
// and nothing else ever removes it. The sweep below runs on the
// install and uninstall paths and boots out gc-owned *suffixed*
// service files whose GC_HOME no longer exists. It never touches the
// default (unsuffixed) service, the current process's own service
// file, or any file whose GC_HOME cannot be parsed or still exists.

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// supervisorSystemdUnitPrefix is the shared prefix of suffixed
// (isolated-GC_HOME) supervisor units; keep in sync with
// supervisorSystemdServiceName.
const supervisorSystemdUnitPrefix = "gascity-supervisor-"

// sweepStaleIsolatedSupervisorServices removes leaked isolated-home
// supervisor services for the active platform. Failures are warnings
// on stderr; the sweep never blocks the caller.
func sweepStaleIsolatedSupervisorServices(stderr io.Writer) {
	switch supervisorRuntimeGOOS {
	case "darwin":
		sweepStaleIsolatedSupervisorLaunchd(stderr)
	case "linux":
		sweepStaleIsolatedSupervisorSystemd(stderr)
	}
}

// supervisorServiceGCHomeMissing reports whether a service file's
// GC_HOME is definitively gone. Only a clean not-exist counts:
// empty values, permission errors, and transient failures leave the
// service alone.
func supervisorServiceGCHomeMissing(gcHome string) bool {
	if strings.TrimSpace(gcHome) == "" {
		return false
	}
	_, err := os.Stat(gcHome)
	return errors.Is(err, os.ErrNotExist)
}

// sweepStaleIsolatedSupervisorLaunchd boots out and removes suffixed
// com.gascity.supervisor.* launch agents whose GC_HOME no longer
// exists.
func sweepStaleIsolatedSupervisorLaunchd(stderr io.Writer) {
	home, err := os.UserHomeDir()
	if err != nil || strings.TrimSpace(home) == "" {
		return
	}
	dir := filepath.Join(home, "Library", "LaunchAgents")
	entries, err := os.ReadDir(dir)
	if err != nil {
		// Missing or unreadable LaunchAgents dir: nothing to sweep.
		return
	}
	ownPath := supervisorLaunchdPlistPath()
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if !strings.HasSuffix(name, ".plist") {
			continue
		}
		label := strings.TrimSuffix(name, ".plist")
		// Only suffixed labels: the default com.gascity.supervisor
		// agent is the user's real supervisor and is never swept.
		if !strings.HasPrefix(label, defaultSupervisorLaunchdLabel+".") {
			continue
		}
		path := filepath.Join(dir, name)
		if samePath(path, ownPath) {
			continue
		}
		// legacySupervisorHome parses GC_HOME out of any gc-rendered
		// service file, not only the legacy-labeled one.
		gcHome, ok := legacySupervisorHome(path)
		if !ok || !supervisorServiceGCHomeMissing(gcHome) {
			continue
		}
		_ = supervisorLaunchctlRun("unload", path)
		_ = supervisorLaunchctlRun("disable", supervisorLaunchdServiceTarget(label))
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			fmt.Fprintf(stderr, "gc supervisor: removing stale isolated supervisor plist %s: %v\n", path, err) //nolint:errcheck // best-effort stderr
			continue
		}
		fmt.Fprintf(stderr, "gc supervisor: removed stale isolated supervisor service %s (GC_HOME %s no longer exists)\n", label, gcHome) //nolint:errcheck // best-effort stderr
	}
}

// sweepStaleIsolatedSupervisorSystemd stops, disables, and removes
// suffixed gascity-supervisor-*.service user units whose GC_HOME no
// longer exists.
func sweepStaleIsolatedSupervisorSystemd(stderr io.Writer) {
	home, err := os.UserHomeDir()
	if err != nil || strings.TrimSpace(home) == "" {
		return
	}
	dir := filepath.Join(home, ".local", "share", "systemd", "user")
	entries, err := os.ReadDir(dir)
	if err != nil {
		// Missing or unreadable unit dir: nothing to sweep.
		return
	}
	ownPath := supervisorSystemdServicePath()
	removed := false
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		// Only suffixed units: the default gascity-supervisor.service
		// (no trailing dash) is the user's real supervisor and is
		// never swept.
		if !strings.HasPrefix(name, supervisorSystemdUnitPrefix) || !strings.HasSuffix(name, ".service") {
			continue
		}
		path := filepath.Join(dir, name)
		if samePath(path, ownPath) {
			continue
		}
		gcHome, ok := legacySupervisorHome(path)
		if !ok || !supervisorServiceGCHomeMissing(gcHome) {
			continue
		}
		_ = supervisorSystemctlRun("--user", "stop", name)
		_ = supervisorSystemctlRun("--user", "disable", name)
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			fmt.Fprintf(stderr, "gc supervisor: removing stale isolated supervisor unit %s: %v\n", path, err) //nolint:errcheck // best-effort stderr
			continue
		}
		removed = true
		fmt.Fprintf(stderr, "gc supervisor: removed stale isolated supervisor service %s (GC_HOME %s no longer exists)\n", name, gcHome) //nolint:errcheck // best-effort stderr
	}
	if removed {
		_ = supervisorSystemctlRun("--user", "daemon-reload")
	}
}
