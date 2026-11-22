//go:build !windows

package beads

import (
	"context"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
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

// keyCount returns how many keys have a pending flight or a running child.
func (c *bdReadCoalescer) keyCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.keys)
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

// TestBDReadCoalesce_DistinctCallsNeverShare covers AC-7: concurrent calls
// that differ only in dir, in one arg, in BEADS_DIR or in a credential env
// var each spawn their own child and get that child's output, and identical
// mutations and identical bd sql calls never coalesce.
func TestBDReadCoalesce_DistinctCallsNeverShare(t *testing.T) {
	t.Parallel()
	f := newFakeBD(t)
	gate := newBDExecGate(16)
	dumps := filepath.Join(f.root, "env")
	dirA, dirB := filepath.Join(f.root, "a"), filepath.Join(f.root, "b")
	for _, d := range []string{dirA, dirB} {
		if err := os.Mkdir(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}

	type call struct {
		dir     string
		argv    string
		env     map[string]string
		wantEnv string // a line this call's child must have seen in its env
	}
	calls := []call{
		{dir: dirA, argv: "list --json", wantEnv: "PWD=" + dirA},
		{dir: dirB, argv: "list --json", wantEnv: "PWD=" + dirB},
		{dir: f.root, argv: "list --json --label a"},
		{dir: f.root, argv: "list --json --label b"},
		{dir: f.root, argv: "list --json", env: map[string]string{"BEADS_DIR": dirA}, wantEnv: "BEADS_DIR=" + dirA},
		{dir: f.root, argv: "list --json", env: map[string]string{"BEADS_DIR": dirB}, wantEnv: "BEADS_DIR=" + dirB},
		{dir: f.root, argv: "show gc-1 --json", env: map[string]string{"BEADS_DOLT_PASSWORD": "p1"}, wantEnv: "BEADS_DOLT_PASSWORD=p1"},
		{dir: f.root, argv: "show gc-1 --json", env: map[string]string{"BEADS_DOLT_PASSWORD": "p2"}, wantEnv: "BEADS_DOLT_PASSWORD=p2"},
		{dir: f.root, argv: "update gc-1 --status open"},
		{dir: f.root, argv: "update gc-1 --status open"},
		{dir: f.root, argv: "sql SELECT 1"},
		{dir: f.root, argv: "sql SELECT 1"},
	}

	results := make([]gateCallResult, len(calls))
	var wg sync.WaitGroup
	for i, c := range calls {
		env := f.env()
		env["GC_TEST_ENV_DUMP"] = dumps
		maps.Copy(env, c.env)
		run := execCommandRunner(context.Background(), env, false, processEnvSnapshotExcludingNativeDoltOpen, gate)
		wg.Add(1)
		go func() {
			defer wg.Done()
			out, err := run(c.dir, "bd", strings.Fields(c.argv)...)
			results[i] = gateCallResult{argv: c.argv, out: string(out), err: err}
		}()
	}
	waitFor(t, "every distinct call to spawn its own child", func() bool { return len(f.alive()) == len(calls) })
	f.releaseAll()
	wg.Wait()

	if n := len(f.spawns()); n != len(calls) {
		t.Errorf("spawned %d children for %d distinct calls, want %d", n, len(calls), len(calls))
	}
	seen := map[string]int{}
	for i, r := range results {
		if r.err != nil {
			t.Errorf("call %d %q: %v", i, r.argv, r.err)
			continue
		}
		seq, argv, _ := strings.Cut(strings.TrimSpace(r.out), " ")
		if argv != r.argv {
			t.Errorf("call %d %q got output %q, want its own child's output", i, r.argv, r.out)
		}
		if prev, dup := seen[seq]; dup {
			t.Errorf("calls %d and %d both got child %s's output", prev, i, seq)
		}
		seen[seq] = i
		if want := calls[i].wantEnv; want != "" {
			data, err := os.ReadFile(dumps + "." + seq)
			if err != nil {
				t.Errorf("call %d: reading child %s env: %v", i, seq, err)
				continue
			}
			if !strings.Contains("\n"+string(data), "\n"+want+"\n") {
				t.Errorf("call %d got child %s's output, but that child's env lacks %q", i, seq, want)
			}
		}
	}
}

// TestBDReadCoalesce_CanceledWaiterLeavesOthersServed covers AC-8: a waiter
// that cancels returns its own cancellation at once, while the shared child
// keeps running for the waiter that remains.
func TestBDReadCoalesce_CanceledWaiterLeavesOthersServed(t *testing.T) {
	t.Parallel()
	f := newFakeBD(t)
	gate := newBDExecGate(8)
	ctxA, cancelA := context.WithCancel(context.Background())
	defer cancelA()

	blocker := runConcurrently(f.root, f.runner(context.Background(), gate), []string{"list --json"})
	waitFor(t, "the blocking child to spawn", func() bool { return len(f.spawns()) == 1 })
	a := runConcurrently(f.root, f.runner(ctxA, gate), []string{"list --json"})
	b := runConcurrently(f.root, f.runner(context.Background(), gate), []string{"list --json"})
	waitFor(t, "both waiters to join one pending flight", func() bool {
		return gate.coalescer.pendingWaiters() == 2
	})

	f.releaseSeq(1)
	wantOutput(t, (<-blocker)[0], "1 list --json")
	waitFor(t, "the shared child to spawn", func() bool { return len(f.spawns()) == 2 })

	cancelA()
	ra := (<-a)[0]
	if !errors.Is(ra.err, context.Canceled) {
		t.Fatalf("canceled waiter got (%q, %v), want context.Canceled", ra.out, ra.err)
	}
	if n := len(f.alive()); n != 1 {
		t.Fatalf("%d children alive after one of two waiters canceled, want the shared child still running", n)
	}
	f.releaseSeq(2)
	wantOutput(t, (<-b)[0], "2 list --json")
}

// TestBDReadCoalesce_LastWaiterLeavingKillsChild covers AC-8: when the only
// waiter cancels, its running child is killed, its gate slot is released and
// its key is dropped.
func TestBDReadCoalesce_LastWaiterLeavingKillsChild(t *testing.T) {
	t.Parallel()
	f := newFakeBD(t)
	gate := newBDExecGate(8)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	res := runConcurrently(f.root, f.runner(ctx, gate), []string{"list --json"})
	waitFor(t, "the child to spawn", func() bool { return len(f.alive()) == 1 })
	pid := f.spawnPIDs()[0]

	cancel()
	if r := (<-res)[0]; !errors.Is(r.err, context.Canceled) {
		t.Fatalf("canceled caller got (%q, %v), want context.Canceled", r.out, r.err)
	}
	waitFor(t, "the abandoned child to be killed", func() bool {
		return errors.Is(syscall.Kill(pid, 0), syscall.ESRCH)
	})
	waitFor(t, "the gate slot to be released", func() bool { return gate.running.Load() == 0 })
	waitFor(t, "the key to be dropped", func() bool { return gate.coalescer.keyCount() == 0 })
}

// TestBDReadCoalesce_AbandonedPendingFlightNeverSpawns covers AC-8: when every
// waiter of a flight that has not started leaves, each gets the slot-wait
// error and the flight never spawns a child.
func TestBDReadCoalesce_AbandonedPendingFlightNeverSpawns(t *testing.T) {
	t.Parallel()
	f := newFakeBD(t)
	gate := newBDExecGate(8)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	blocker := runConcurrently(f.root, f.runner(context.Background(), gate), []string{"list --json"})
	waitFor(t, "the blocking child to spawn", func() bool { return len(f.spawns()) == 1 })
	waiters := runConcurrently(f.root, f.runner(ctx, gate), []string{"list --json", "list --json"})
	waitFor(t, "both waiters to join one pending flight", func() bool {
		return gate.coalescer.pendingWaiters() == 2
	})

	cancel()
	for _, r := range <-waiters {
		if !errors.Is(r.err, ErrBDExecSlotWait) || !errors.Is(r.err, context.Canceled) {
			t.Errorf("waiter of an abandoned flight got (%q, %v), want ErrBDExecSlotWait wrapping context.Canceled", r.out, r.err)
		}
	}
	f.releaseSeq(1)
	wantOutput(t, (<-blocker)[0], "1 list --json")
	waitFor(t, "every key to be dropped", func() bool { return gate.coalescer.keyCount() == 0 })
	if n := len(f.spawns()); n != 1 {
		t.Fatalf("%d children spawned, want 1: the abandoned flight spawned", n)
	}
	if n := gate.running.Load(); n != 0 {
		t.Fatalf("%d gate slots held after every call returned, want 0", n)
	}
}
