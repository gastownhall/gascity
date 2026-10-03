package pidutil

import (
	"fmt"
	"testing"
)

// IsReparentedOrphan is the shared "has this process been orphaned?" test, and
// it gates kills. The naive `ppid <= 1` form is wrong under a
// `systemd --user` child-subreaper, where an orphan reparents to the user
// manager and carries a large LIVE ppid.
func TestIsReparentedOrphan(t *testing.T) {
	const subreaper = 3117
	for _, tc := range []struct {
		name         string
		ppid         int
		subreaperPID int
		want         bool
	}{
		{"reparented to init", 1, 0, true},
		{"reparented to init even with a subreaper present", 1, subreaper, true},
		{"reparented to the user subreaper", subreaper, subreaper, true},
		{"live parent on a plain-init host", 4242, 0, false},
		{"live parent with a subreaper present", 4242, subreaper, false},
		// The tmux server owning a pane root is the case that must never read as
		// orphaned, or the escalation refuses to kill the one runtime it should.
		{"tmux server parent", 400, subreaper, false},
		// A subreaper pid of 0/1 means "none detected" and must not turn every
		// live-parent process into an orphan.
		{"undetected subreaper does not match a live parent", 4242, 1, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsReparentedOrphan(tc.ppid, tc.subreaperPID); got != tc.want {
				t.Errorf("IsReparentedOrphan(%d, %d) = %v, want %v", tc.ppid, tc.subreaperPID, got, tc.want)
			}
		})
	}
}

// DetectUserSubreaperPIDWith walks the caller's own ancestry for the nearest
// `systemd` that is not pid 1 — the user manager, as distinct from the system
// manager at pid 1.
func TestDetectUserSubreaperPIDWith(t *testing.T) {
	// self(900) -> bash(800) -> systemd --user(3117) -> systemd(1)
	parents := map[int]int{900: 800, 800: 3117, 3117: 1}
	comms := map[int]string{900: "gc", 800: "bash", 3117: "systemd", 1: "systemd"}
	parentOf := func(pid int) (int, error) {
		ppid, ok := parents[pid]
		if !ok {
			return 0, fmt.Errorf("no parent for %d", pid)
		}
		return ppid, nil
	}
	commOf := func(pid int) string { return comms[pid] }

	if got := DetectUserSubreaperPIDWith(900, parentOf, commOf); got != 3117 {
		t.Errorf("DetectUserSubreaperPIDWith = %d, want 3117 (the user manager, not the system systemd at pid 1)", got)
	}

	// A plain-init host: no systemd ancestor above pid 1.
	plainParents := map[int]int{900: 800, 800: 1}
	plainComms := map[int]string{900: "gc", 800: "sshd", 1: "init"}
	plainParentOf := func(pid int) (int, error) {
		ppid, ok := plainParents[pid]
		if !ok {
			return 0, fmt.Errorf("no parent for %d", pid)
		}
		return ppid, nil
	}
	if got := DetectUserSubreaperPIDWith(900, plainParentOf, func(pid int) string { return plainComms[pid] }); got != 0 {
		t.Errorf("DetectUserSubreaperPIDWith on a plain-init host = %d, want 0", got)
	}

	// A malformed/cyclic chain must terminate rather than spin.
	cyclic := func(pid int) (int, error) { return pid, nil }
	if got := DetectUserSubreaperPIDWith(900, cyclic, func(int) string { return "gc" }); got != 0 {
		t.Errorf("DetectUserSubreaperPIDWith on a cyclic chain = %d, want 0", got)
	}
}
