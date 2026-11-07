package proctable

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/gastownhall/gascity/internal/runtime"
)

// UserManagerServiceUnit reports the systemd user-service unit that owns
// cgroup when the path is a *.service nested under the user manager
// (a path component user@<uid>.service) and is not the manager unit itself.
// A scope, a slice, the user manager, and a system service that is not
// below that manager return "", false.
//
// systemd-run --user places the transient unit directly under the manager,
// for example
// /user.slice/user-1000.slice/user@1000.service/app.slice/run-xxxx.service.
// A session pane wrapped in a tmux scope stays a sibling scope, not a
// service:
// /user.slice/user-1000.slice/user@1000.service/app.slice/tmux-spawn-<id>.scope.
func UserManagerServiceUnit(cgroup string) (string, bool) {
	cgroup = filepath.Clean(cgroup)
	if cgroup == "" || cgroup == "." || cgroup == "/" {
		return "", false
	}
	parts := strings.Split(cgroup, string(filepath.Separator))
	manager := -1
	for i, part := range parts {
		if isUserManagerUnit(part) {
			manager = i
		}
	}
	if manager < 0 {
		return "", false
	}
	var unit string
	for _, part := range parts[manager+1:] {
		if strings.HasSuffix(part, ".service") && !isUserManagerUnit(part) {
			unit = part
		}
	}
	if unit == "" {
		return "", false
	}
	return unit, true
}

// isUserManagerUnit reports whether part is the systemd user manager unit
// user@<uid>.service. Other instantiated units (foo@bar.service) are not.
func isUserManagerUnit(part string) bool {
	rest, ok := strings.CutPrefix(part, "user@")
	if !ok || !strings.HasSuffix(rest, ".service") {
		return false
	}
	uid := strings.TrimSuffix(rest, ".service")
	if uid == "" {
		return false
	}
	for _, r := range uid {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// SessionOwnUserService reports the user-manager service unit sessionID
// itself occupies, among the runtimes a scan just returned.
//
// A tracked runtime for that session that sits in a *.service names that
// unit. A tracked runtime that is not in a service (a tmux scope) means the
// session has no service of its own, so the result is empty and every nested
// *.service is foreign. With no tracked runtime, the calling process's unit
// is used when it is in one, so a subprocess session that inherited the
// reaper's cgroup is still reaped. A reaper in a scope leaves the result
// empty.
func SessionOwnUserService(runtimes []runtime.LiveRuntime, sessionID string) string {
	sessionID = strings.TrimSpace(sessionID)
	sawTracked := false
	for _, live := range runtimes {
		if sessionID == "" || strings.TrimSpace(live.SessionID) != sessionID || !live.IsTracked {
			continue
		}
		sawTracked = true
		if unit, ok := UserServiceUnitOf(live.PID); ok {
			return unit
		}
	}
	if sawTracked {
		return ""
	}
	if unit, ok := UserServiceUnitOf(os.Getpid()); ok {
		return unit
	}
	return ""
}

// ForeignUserService reports whether pid sits in a user-manager *.service
// other than sessionUnit. An empty sessionUnit means the session has no
// service of its own, so any such unit is foreign. unit is that service's
// name when pid is in one, including when it is the session's own unit
// (foreign is then false) so a reap can log it. An unreadable cgroup is
// not foreign: the caller keeps the behavior it had before this fence.
func ForeignUserService(pid int, sessionUnit string) (unit string, foreign bool) {
	unit, ok := UserServiceUnitOf(pid)
	if !ok {
		return "", false
	}
	return unit, unit != sessionUnit
}
