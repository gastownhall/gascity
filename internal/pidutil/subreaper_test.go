package pidutil

import (
	"errors"
	"fmt"
	"slices"
	"strings"
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

// fakeTree serves parentOf from a map, as /proc would.
type fakeTree map[int]int

func (f fakeTree) parentOf(pid int) (int, error) {
	ppid, ok := f[pid]
	if !ok {
		return 0, fmt.Errorf("no parent for %d", pid)
	}
	return ppid, nil
}

// probeReporting is a probe that observes adopter, counting its runs.
func probeReporting(adopter int, err error, runs *int) func() (int, error) {
	return func() (int, error) {
		*runs++
		return adopter, err
	}
}

// The set holds the adopter the probe observed, and nothing else: an
// ancestor is a member only when the kernel actually reparented an orphan
// of the caller's child to it.
func TestAdopterCacheDetect(t *testing.T) {
	// self(900) -> bash(800) -> process-wrapper(700) -> java(600) -> systemd --user(3117) -> init(1)
	tree := fakeTree{900: 800, 800: 700, 700: 600, 600: 3117, 3117: 1}
	for _, tc := range []struct {
		name    string
		adopter int
		probe   error
		want    []int
		wantErr string
	}{
		// bash, the caller's live parent, never set the flag: the kernel passes
		// it by, so its own children stay owned.
		{name: "nearest subreaper above a non-subreaper ancestor", adopter: 700, want: []int{700}},
		{name: "user manager", adopter: 3117, want: []int{3117}},
		{name: "no subreaper ancestor", adopter: 1},
		// The caller adopting its own orphans is never a member: it is the
		// live parent of everything it spawns.
		{name: "caller is the subreaper", adopter: 900},
		{name: "probe failed", probe: errors.New("fork: resource temporarily unavailable"), wantErr: "fork: resource temporarily unavailable"},
		{name: "adopter not an ancestor", adopter: 4242, wantErr: "not one of its ancestors"},
		{name: "adopter not visible", adopter: 0, wantErr: "no visible parent"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var c adopterCache
			runs := 0
			got, err := c.detect(900, tree.parentOf, probeReporting(tc.adopter, tc.probe, &runs))
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("detect error = %v, want it to mention %q", err, tc.wantErr)
				}
			} else if err != nil {
				t.Fatalf("detect: %v", err)
			}
			if !slices.Equal(got.PIDs(), append([]int{}, tc.want...)) {
				t.Fatalf("detect = %v, want %v", got, tc.want)
			}
			if runs != 1 {
				t.Fatalf("probe ran %d times, want once", runs)
			}
		})
	}
}

// One probe serves every call while the ancestry is unchanged; an ancestor's
// exit reparents its child, changes the chain and probes again. A failed
// probe is not remembered.
func TestAdopterCacheReprobesOnlyWhenAncestryChanges(t *testing.T) {
	tree := fakeTree{900: 800, 800: 700, 700: 1}
	var c adopterCache
	runs := 0
	probe := probeReporting(700, nil, &runs)
	for i := 0; i < 3; i++ {
		got, err := c.detect(900, tree.parentOf, probe)
		if err != nil || !got.Contains(700) {
			t.Fatalf("detect = %v, %v; want [700]", got, err)
		}
	}
	if runs != 1 {
		t.Fatalf("probe ran %d times for an unchanged ancestry, want once", runs)
	}

	// bash (800) exits: the kernel reparents the caller to the subreaper.
	delete(tree, 800)
	tree[900] = 700
	if _, err := c.detect(900, tree.parentOf, probe); err != nil {
		t.Fatalf("detect after reparent: %v", err)
	}
	if runs != 2 {
		t.Fatalf("probe ran %d times after the ancestry changed, want 2", runs)
	}

	failing := probeReporting(0, errors.New("no shell"), &runs)
	tree[900] = 800
	tree[800] = 700
	for i := 0; i < 2; i++ {
		if got, err := c.detect(900, tree.parentOf, failing); err == nil || len(got.PIDs()) != 0 {
			t.Fatalf("detect with a failing probe = %v, %v; want the empty set and an error", got, err)
		}
	}
	if runs != 4 {
		t.Fatalf("probe ran %d times, want a failed probe retried on every call (4)", runs)
	}
}

// The ancestry walk ends at pid 1, at an unreadable parent, on a loop, and at
// its depth bound, and never lists the caller.
func TestAncestry(t *testing.T) {
	for _, tc := range []struct {
		name string
		tree fakeTree
		want []int
	}{
		{"to init", fakeTree{900: 800, 800: 700, 700: 1}, []int{800, 700}},
		{"plain init host", fakeTree{900: 1}, nil},
		{"unreadable ancestor", fakeTree{900: 800, 800: 700}, []int{800, 700}},
		{"cyclic chain", fakeTree{900: 800, 800: 700, 700: 800}, []int{800, 700}},
		{"self loop", fakeTree{900: 800, 800: 900}, []int{800}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := ancestry(900, tc.tree.parentOf); !slices.Equal(got, tc.want) {
				t.Fatalf("ancestry = %v, want %v", got, tc.want)
			}
		})
	}
	reads := 0
	deep := func(pid int) (int, error) {
		reads++
		return pid + 1, nil
	}
	if got := ancestry(100, deep); len(got) != maxAncestorDepth || reads != maxAncestorDepth {
		t.Fatalf("deep ancestry = %d ancestors in %d reads, want the bound %d", len(got), reads, maxAncestorDepth)
	}
}
