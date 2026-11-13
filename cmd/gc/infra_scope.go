package main

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/gastownhall/gascity/internal/userscope"
)

// The managed Dolt server (directly or through its scope watchdog) that a gc
// command run from inside an agent session spawns would otherwise inherit
// that session's cgroup: a GC_AGENT_SLICE pane scope or a runtime launcher's
// scope. A cgroup-wide kill of the agent then takes the city's store down
// with it. Such spawns get a transient systemd user scope of their own
// instead, when that scope suits the caller (infraScopeSuitsCaller). Every
// other caller (the supervisor, a CLI run from a login shell, a provider
// script the supervisor launched) spawns exactly as before. The supervisor
// itself is never scoped: every session it starts would follow it into the
// slice.
const (
	// infraSliceEnv names the systemd user slice infrastructure scopes are
	// created under, or turns scoping off; see infraSlice.
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
			if _, named := infraSlice(); !named {
				return
			}
			log.Printf("gc: %s=%q set but city infrastructure cannot be scoped; it runs in the caller's cgroup: %v", infraSliceEnv, slice, err)
		},
	}
}

// infraLingerDir is where logind records users whose user manager outlives
// their sessions. Tests replace it.
var infraLingerDir = "/var/lib/systemd/linger"

// infraCurrentUsername resolves this process's username for the linger
// marker. Tests replace it.
var infraCurrentUsername = func() (string, error) {
	u, err := user.Current()
	if err != nil {
		return "", err
	}
	return u.Username, nil
}

// infraProcCgroupPath is this process's cgroup membership. Tests replace it.
var infraProcCgroupPath = "/proc/self/cgroup"

// infraScopeProbe accepts user scopes for infrastructure only when they suit
// the caller, then checks that systemd-run and the user manager respond. A
// caller they do not suit never runs the probe.
func infraScopeProbe(slice string) error {
	if err := infraScopeSuitsCaller(); err != nil {
		return err
	}
	return userscope.Probe(slice)
}

// infraCaller is where a gc process's cgroup sits relative to the current
// user's systemd units.
type infraCaller int

const (
	// infraCallerElsewhere is any other cgroup: a system service or slice
	// (a supervisor run as a system service, whose agents share its
	// cgroup), a container's root cgroup, or a cgroup file that could not
	// be read.
	infraCallerElsewhere infraCaller = iota
	// infraCallerLoginSession is a login session's scope,
	// user-<uid>.slice/session-<id>.scope.
	infraCallerLoginSession
	// infraCallerUserManager is anywhere under user@<uid>.service.
	infraCallerUserManager
)

// infraScopeSuitsCaller reports nil when moving a spawn out of this gc
// process's cgroup into a user-manager scope cannot shorten the spawn's life
// and does not undo where the operator placed it:
//
//   - Under user@<uid>.service (a GC_AGENT_SLICE pane scope, tmux's own pane
//     scopes, a launcher's user scope): always. The caller already dies with
//     the user manager. Decided from the uid alone, so a uid without a
//     passwd entry (containers, sandboxes) qualifies.
//   - In a login session's scope: when the user lingers. Without lingering a
//     user-manager scope stops at logout, while the session scope can
//     outlive it (KillUserProcesses=no).
//   - Anywhere else: only when GC_INFRA_SLICE names a slice and the user
//     lingers. Such a caller has no per-session cgroup to escape, and the
//     move would trade the cgroup the operator chose, with its memory
//     protection, for a user slice that has none unless the operator
//     defines it.
func infraScopeSuitsCaller() error {
	caller, cgroupErr := readInfraCaller(infraProcCgroupPath, os.Getuid())
	if caller == infraCallerUserManager {
		return nil
	}
	if _, named := infraSlice(); caller == infraCallerElsewhere && !named {
		if cgroupErr != nil {
			return fmt.Errorf("reading %s: %w", infraProcCgroupPath, cgroupErr)
		}
		return fmt.Errorf("gc runs outside the user's login sessions and user manager, and %s names no slice", infraSliceEnv)
	}
	if lingers, lingerNote := infraUserLingers(); !lingers {
		return fmt.Errorf("%s: a user scope would stop at logout", lingerNote)
	}
	return nil
}

// infraUserLingers reports whether logind's linger marker exists for the
// current user, with a note describing the outcome. A username that cannot
// be resolved counts as not lingering.
func infraUserLingers() (bool, string) {
	username, err := infraCurrentUsername()
	if err != nil {
		return false, fmt.Sprintf("current user does not linger (username unresolvable: %v)", err)
	}
	marker := filepath.Join(infraLingerDir, username)
	if _, err := os.Stat(marker); err != nil {
		return false, fmt.Sprintf("user %q does not linger (%s)", username, marker)
	}
	return true, ""
}

// readInfraCaller classifies the cgroup paths in the /proc/<pid>/cgroup file
// at path for the user uid. Of several hierarchies (cgroup v1), the one
// closest to the user manager decides.
func readInfraCaller(path string, uid int) (infraCaller, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return infraCallerElsewhere, err
	}
	caller := infraCallerElsewhere
	sc := bufio.NewScanner(bytes.NewReader(data))
	for sc.Scan() {
		// hierarchy-ID:controller-list:cgroup-path
		if parts := strings.SplitN(sc.Text(), ":", 3); len(parts) == 3 {
			caller = max(caller, classifyInfraCallerCgroup(parts[2], uid))
		}
	}
	return caller, sc.Err()
}

// classifyInfraCallerCgroup places one cgroup path relative to the user uid's
// systemd units, matching whole path components: a session scope counts only
// directly under that user's user-<uid>.slice.
func classifyInfraCallerCgroup(cgroupPath string, uid int) infraCaller {
	components := strings.Split(cgroupPath, "/")
	if slices.Contains(components, "user@"+strconv.Itoa(uid)+".service") {
		return infraCallerUserManager
	}
	i := slices.Index(components, "user-"+strconv.Itoa(uid)+".slice")
	if i < 0 || i+1 >= len(components) {
		return infraCallerElsewhere
	}
	if next := components[i+1]; strings.HasPrefix(next, "session-") && strings.HasSuffix(next, ".scope") {
		return infraCallerLoginSession
	}
	return infraCallerElsewhere
}

// infraSlice resolves GC_INFRA_SLICE: the slice city infrastructure scopes
// are created under, and whether the operator named it. Unset or empty is
// defaultInfraSlice, not named. "off", "0" or "false" in any case (as for
// the GC_INJECT_* switches) turns scoping off and returns "".
func infraSlice() (slice string, named bool) {
	value := strings.TrimSpace(os.Getenv(infraSliceEnv))
	switch strings.ToLower(value) {
	case "":
		return defaultInfraSlice, false
	case "off", "0", "false":
		return "", false
	}
	return value, true
}

// infraUnitPrefix starts the name of every transient scope unit city
// infrastructure is spawned in, so its journal records stay recognizable
// after --collect removes the unit.
const infraUnitPrefix = "gascity-infra-"

// infraUnitName returns a fresh scope unit name. It is unique per spawn: a
// fixed name would collide with a previous spawn's scope that is still
// active.
func infraUnitName() string {
	var b [8]byte
	_, _ = rand.Read(b[:]) // crypto/rand.Read never returns an error
	return infraUnitPrefix + hex.EncodeToString(b[:]) + ".scope"
}

// infraArgv returns argv wrapped in a transient user scope under the infra
// slice, described by description, when this gc process runs inside an
// agent session, GC_INFRA_SLICE does not turn scoping off and the scope
// suits the caller (infraScopeSuitsCaller), with the scope unit's name;
// otherwise argv unchanged and "".
func infraArgv(description string, argv []string) ([]string, string) {
	slice, _ := infraSlice()
	if slice == "" || strings.TrimSpace(os.Getenv(infraSessionIdentityEnv)) == "" {
		return argv, ""
	}
	scope := userscope.Scope{Slice: slice, Unit: infraUnitName(), Description: description}
	wrapped, ok := infraScope.WrapScope(scope, argv)
	if !ok {
		return argv, ""
	}
	return wrapped, scope.Unit
}

// infraCommand is exec.Command for a city infrastructure process, with the
// name of the scope unit it runs in ("" when unscoped); see infraArgv. A
// name that does not resolve stays unwrapped: wrapped, exec would resolve
// systemd-run instead and the missing executable would surface only as a
// later readiness timeout rather than at Start.
func infraCommand(description, name string, args ...string) (*exec.Cmd, string) {
	if _, err := exec.LookPath(name); err != nil {
		return exec.Command(name, args...), ""
	}
	argv, unit := infraArgv(description, append([]string{name}, args...))
	return exec.Command(argv[0], argv[1:]...), unit
}

// managedDoltInfraDescription is the scope description of a managed Dolt
// spawn; the user manager's journal records repeat it.
func managedDoltInfraDescription(cityPath string) string {
	const base = "gascity infra: managed dolt"
	if strings.TrimSpace(cityPath) == "" {
		return base
	}
	return base + " for city " + cityPath
}
