package pidutil

import (
	"fmt"
	"testing"
)

// IsReparentedOrphan is the shared "has this process been orphaned?" test, and
// it gates kills. The naive `ppid <= 1` form is wrong under a child subreaper
// (`systemd --user`, Bazel's process-wrapper, a container init), where an
// orphan reparents to that subreaper and carries a large LIVE ppid.
func TestIsReparentedOrphan(t *testing.T) {
	const subreaper = 3117
	subreapers := NewSubreapers(subreaper)
	for _, tc := range []struct {
		name       string
		ppid       int
		subreapers Subreapers
		want       bool
	}{
		{"reparented to init", 1, Subreapers{}, true},
		{"reparented to init even with a subreaper present", 1, subreapers, true},
		{"reparented to the subreaper", subreaper, subreapers, true},
		{"reparented to the farther of two subreapers", 2001, NewSubreapers(subreaper, 2001), true},
		{"live parent on a plain-init host", 4242, Subreapers{}, false},
		{"live parent with a subreaper present", 4242, subreapers, false},
		// The tmux server owning a pane root is the case that must never read as
		// orphaned, or the escalation refuses to kill the one runtime it should.
		{"tmux server parent", 400, subreapers, false},
		// Pid 1 and below are never set members, so a set built from them
		// cannot turn every live-parent process into an orphan.
		{"no-key subreaper does not match a live parent", 4242, NewSubreapers(1, 0, -1), false},
		// ppid 0 is "could not read the parent", never proof of reparenting.
		{"unreported parent", 0, Subreapers{}, false},
		{"unreported parent with a subreaper present", 0, subreapers, false},
		{"unreported parent with a pid 0 key", 0, NewSubreapers(0), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsReparentedOrphan(tc.ppid, tc.subreapers); got != tc.want {
				t.Errorf("IsReparentedOrphan(%d, %v) = %v, want %v", tc.ppid, tc.subreapers, got, tc.want)
			}
		})
	}
}

// fakeTree serves parentOf and commOf from maps, as /proc would.
type fakeTree struct {
	parents map[int]int
	comms   map[int]string
}

func (f fakeTree) parentOf(pid int) (int, error) {
	ppid, ok := f.parents[pid]
	if !ok {
		return 0, fmt.Errorf("no parent for %d", pid)
	}
	return ppid, nil
}

func (f fakeTree) commOf(pid int) string { return f.comms[pid] }

// DetectSubreapersWith collects the caller's strict ancestors above pid 1: an
// orphan of a process the caller spawned is adopted by the nearest of them
// that is a subreaper, whichever process that is.
func TestDetectSubreapersWith(t *testing.T) {
	for _, tc := range []struct {
		name string
		tree fakeTree
		want []int
		not  []int
	}{
		{
			name: "user manager",
			// self(900) -> bash(800) -> systemd --user(3117) -> systemd(1)
			tree: fakeTree{
				parents: map[int]int{900: 800, 800: 3117, 3117: 1},
				comms:   map[int]string{900: "gc", 800: "bash", 3117: "systemd", 1: "systemd"},
			},
			want: []int{800, 3117},
			not:  []int{900, 1},
		},
		{
			// The Bazel local-exec topology: the test's escaped children are
			// adopted by process-wrapper, nowhere near a process named systemd.
			name: "bazel process-wrapper",
			tree: fakeTree{
				parents: map[int]int{900: 850, 850: 700, 700: 600, 600: 4280, 4280: 1},
				comms:   map[int]string{900: "acp_test", 850: "bash", 700: "process-wrapper", 600: "java", 4280: "systemd"},
			},
			want: []int{850, 700, 600, 4280},
			not:  []int{900},
		},
		{
			name: "container init",
			tree: fakeTree{
				parents: map[int]int{900: 12, 12: 1},
				comms:   map[int]string{900: "gc", 12: "tini"},
			},
			want: []int{12},
			not:  []int{900},
		},
		{
			// tmux never sets PR_SET_CHILD_SUBREAPER, and it is the live parent
			// of every pane root it serves: a controller started from a pane must
			// not read those roots as orphans.
			name: "tmux server ancestor",
			tree: fakeTree{
				parents: map[int]int{900: 800, 800: 500, 500: 3117, 3117: 1},
				comms:   map[int]string{900: "gc", 800: "bash", 500: "tmux: server", 3117: "systemd"},
			},
			want: []int{800, 3117},
			not:  []int{900, 500},
		},
		{
			name: "plain init host",
			tree: fakeTree{
				parents: map[int]int{900: 1},
				comms:   map[int]string{900: "gc"},
			},
			not: []int{900, 1},
		},
		{
			// An unreadable link keeps the ancestors read before it.
			name: "unreadable ancestor",
			tree: fakeTree{
				parents: map[int]int{900: 800, 800: 700},
				comms:   map[int]string{900: "gc", 800: "bash", 700: "sshd"},
			},
			want: []int{800, 700},
			not:  []int{900},
		},
		{
			// A malformed chain that loops must terminate, and never makes the
			// caller its own subreaper.
			name: "cyclic chain",
			tree: fakeTree{
				parents: map[int]int{900: 800, 800: 900},
				comms:   map[int]string{900: "gc", 800: "bash"},
			},
			want: []int{800},
			not:  []int{900},
		},
		{
			name: "self loop",
			tree: fakeTree{
				parents: map[int]int{900: 900},
				comms:   map[int]string{900: "gc"},
			},
			not: []int{900},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := DetectSubreapersWith(900, tc.tree.parentOf, tc.tree.commOf)
			for _, pid := range tc.want {
				if !got.Contains(pid) {
					t.Errorf("DetectSubreapersWith = %v, want it to contain %d", got, pid)
				}
			}
			for _, pid := range tc.not {
				if got.Contains(pid) {
					t.Errorf("DetectSubreapersWith = %v, want it not to contain %d", got, pid)
				}
			}
			if n := len(got.PIDs()); n != len(tc.want) {
				t.Errorf("DetectSubreapersWith = %v (%d pids), want exactly %v", got, n, tc.want)
			}
		})
	}
}

// A chain deeper than the walk's bound stops at the bound instead of reading
// /proc without end.
func TestDetectSubreapersWithIsBounded(t *testing.T) {
	reads := 0
	parentOf := func(pid int) (int, error) {
		reads++
		return pid + 1, nil
	}
	got := DetectSubreapersWith(100, parentOf, func(int) string { return "sh" })
	if reads > maxAncestorDepth {
		t.Errorf("walk read %d parents, want at most %d", reads, maxAncestorDepth)
	}
	if !got.Contains(101) {
		t.Errorf("DetectSubreapersWith = %v, want the nearest ancestor 101", got)
	}
}
