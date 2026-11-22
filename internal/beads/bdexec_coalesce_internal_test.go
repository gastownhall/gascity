//go:build !windows

package beads

import (
	"context"
	"strings"
	"testing"
)

// pendingWaiters returns how many callers wait on flights that have not yet
// spawned their child.
func (c *bdReadCoalescer) pendingWaiters() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, st := range c.keys {
		if st.pending != nil {
			n += st.pending.waiters
		}
	}
	return n
}

func wantOutput(t *testing.T, r gateCallResult, want string) {
	t.Helper()
	if r.err != nil {
		t.Errorf("%q: %v", r.argv, r.err)
		return
	}
	if got := strings.TrimSpace(r.out); got != want {
		t.Errorf("%q output = %q, want %q", r.argv, got, want)
	}
}

// TestBDReadCoalesce_IdenticalWaitersShareOneChild covers AC-5: while a
// bd list --json child is alive, N identical calls cost exactly one more
// child, spawned only after the first exits, and all N get its output.
func TestBDReadCoalesce_IdenticalWaitersShareOneChild(t *testing.T) {
	t.Parallel()
	const joiners = 10
	f := newFakeBD(t)
	gate := newBDExecGate(8)
	run := f.runner(context.Background(), gate)

	first := runConcurrently(f.root, run, []string{"list --json"})
	waitFor(t, "the first child to spawn", func() bool { return len(f.spawns()) == 1 })

	argvs := make([]string, joiners)
	for i := range argvs {
		argvs[i] = "list --json"
	}
	rest := runConcurrently(f.root, run, argvs)
	waitFor(t, "every identical caller to wait on one pending flight", func() bool {
		return gate.coalescer.pendingWaiters() == joiners
	})
	if n := len(f.spawns()); n != 1 {
		t.Fatalf("%d children spawned while the first was alive, want 1", n)
	}

	f.releaseSeq(1)
	wantOutput(t, (<-first)[0], "1 list --json")
	waitFor(t, "the shared second child to spawn", func() bool { return len(f.spawns()) == 2 })
	f.releaseAll()
	for _, r := range <-rest {
		wantOutput(t, r, "2 list --json")
	}
	if n := len(f.spawns()); n != 2 {
		t.Fatalf("%d children spawned for 1+%d identical calls, want 2", n, joiners)
	}
}

// TestBDReadCoalesce_JoinsOnlyBeforeSpawn covers AC-6: a call that arrives
// after an identical child has started never takes that child's output; it
// gets a child spawned after the first exits.
func TestBDReadCoalesce_JoinsOnlyBeforeSpawn(t *testing.T) {
	t.Parallel()
	f := newFakeBD(t)
	gate := newBDExecGate(8)
	run := f.runner(context.Background(), gate)

	first := runConcurrently(f.root, run, []string{"list --json"})
	waitFor(t, "the first child to spawn", func() bool { return len(f.spawns()) == 1 })
	second := runConcurrently(f.root, run, []string{"list --json"})
	waitFor(t, "the late caller to wait on a flight of its own", func() bool {
		return gate.coalescer.pendingWaiters() == 1
	})

	f.releaseSeq(1)
	wantOutput(t, (<-first)[0], "1 list --json")
	waitFor(t, "the late caller's child to spawn", func() bool { return len(f.spawns()) == 2 })
	f.releaseAll()
	wantOutput(t, (<-second)[0], "2 list --json")
}
