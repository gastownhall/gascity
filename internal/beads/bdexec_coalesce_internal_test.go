//go:build !windows

package beads

import (
	"context"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"sync"
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
