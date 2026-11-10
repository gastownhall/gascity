package main

import (
	"bufio"
	"bytes"
	"fmt"
	"log"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/gastownhall/gascity/internal/userscope"
)

// City infrastructure (the managed Dolt server, its scope watchdog, and a
// forked supervisor) that a gc command run from inside an agent session
// spawns would otherwise inherit that session's cgroup: a GC_AGENT_SLICE
// pane scope or a runtime launcher's scope. A cgroup-wide kill of the agent
// then takes the city's store, or its supervisor, down with it. Such spawns
// get a transient systemd user scope of their own instead, when that scope
// cannot shorten the infrastructure's life. Every other caller (the
// supervisor, a CLI run from a login shell, a provider script the supervisor
// launched) spawns exactly as before.
const (
	// infraSliceEnv names the systemd user slice infrastructure scopes are
	// created under; empty means defaultInfraSlice.
	infraSliceEnv = "GC_INFRA_SLICE"
	// defaultInfraSlice is the slice used when infraSliceEnv is unset.
	defaultInfraSlice = "gascity-infra.slice"
	// infraSessionIdentityEnv is the session identity gc gives every agent
	// session (one of execenv.SessionScopedEnvKeys); a gc process carrying
	// it was started from inside that session.
	infraSessionIdentityEnv = "GC_SESSION_ID"
)

// infraScope decides once per process whether infrastructure spawns from a
// session can be scoped. The fallback is silent unless the operator named a
// slice explicitly. Tests replace it.
var infraScope = newInfraScope()

// newInfraScope returns the production infrastructure scope wrapper.
func newInfraScope() *userscope.Wrapper {
	return &userscope.Wrapper{
		Probe: infraScopeProbe,
		Unavailable: func(slice string, err error) {
			if strings.TrimSpace(os.Getenv(infraSliceEnv)) == "" {
				return
			}
			log.Printf("gc: %s=%q set but city infrastructure cannot be scoped; it runs in the caller's cgroup: %v", infraSliceEnv, slice, err)
		},
	}
}

// infraLingerDir is where logind records users whose user manager outlives
// their sessions. Tests replace it.
var infraLingerDir = "/var/lib/systemd/linger"

// infraProcCgroupPath is this process's cgroup membership. Tests replace it.
var infraProcCgroupPath = "/proc/self/cgroup"

// infraScopeProbe accepts user scopes for infrastructure only when moving a
// spawn into one cannot shorten its life, then checks that systemd-run and
// the user manager respond.
func infraScopeProbe(slice string) error {
	if err := infraScopeKeepsLifetime(); err != nil {
		return err
	}
	return userscope.Probe(slice)
}

// infraScopeKeepsLifetime reports nil when a user-manager scope lives at
// least as long as the caller's own cgroup: the user lingers, so the user
// manager outlives logout, or the caller already runs under
// user@<uid>.service and dies with the user manager anyway. Without either,
// the caller sits in a login-session scope that can survive logout
// (KillUserProcesses=no) while a user-manager scope stops at logout.
func infraScopeKeepsLifetime() error {
	u, err := user.Current()
	if err != nil {
		return fmt.Errorf("resolving current user: %w", err)
	}
	marker := filepath.Join(infraLingerDir, u.Username)
	if _, err := os.Stat(marker); err == nil {
		return nil
	}
	unit := "user@" + strconv.Itoa(os.Getuid()) + ".service"
	under, err := cgroupUnder(infraProcCgroupPath, unit)
	if err != nil {
		return fmt.Errorf("user %q does not linger (%s) and reading %s: %w", u.Username, marker, infraProcCgroupPath, err)
	}
	if !under {
		return fmt.Errorf("user %q does not linger (%s) and gc runs outside %s: a user scope would stop at logout", u.Username, marker, unit)
	}
	return nil
}

// cgroupUnder reports whether any cgroup path in the /proc/<pid>/cgroup
// file at path has unit as one of its components.
func cgroupUnder(path, unit string) (bool, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return false, err
	}
	sc := bufio.NewScanner(bytes.NewReader(data))
	for sc.Scan() {
		// hierarchy-ID:controller-list:cgroup-path
		parts := strings.SplitN(sc.Text(), ":", 3)
		if len(parts) != 3 {
			continue
		}
		for _, component := range strings.Split(parts[2], "/") {
			if component == unit {
				return true, nil
			}
		}
	}
	return false, sc.Err()
}

// infraSlice resolves the slice city infrastructure scopes are created under.
func infraSlice() string {
	if slice := strings.TrimSpace(os.Getenv(infraSliceEnv)); slice != "" {
		return slice
	}
	return defaultInfraSlice
}

// infraArgv returns argv wrapped in a transient user scope under the infra
// slice when this gc process runs inside an agent session and scoping
// cannot shorten the spawn's life; otherwise argv unchanged.
func infraArgv(argv []string) []string {
	if strings.TrimSpace(os.Getenv(infraSessionIdentityEnv)) == "" {
		return argv
	}
	return infraScope.Wrap(infraSlice(), argv)
}

// infraCommand is exec.Command for a city infrastructure process; see
// infraArgv.
func infraCommand(name string, args ...string) *exec.Cmd {
	argv := infraArgv(append([]string{name}, args...))
	return exec.Command(argv[0], argv[1:]...)
}
