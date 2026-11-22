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
	"time"

	"github.com/gastownhall/gascity/internal/testutil"
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
	waitFor(t, "the first child to spawn", f.spawnCount, 1)

	argvs := make([]string, joiners)
	for i := range argvs {
		argvs[i] = "list --json"
	}
	rest := runConcurrently(f.root, run, argvs)
	waitFor(t, "every identical caller to wait on one pending flight", gate.coalescer.pendingWaiters, joiners)
	if n := len(f.spawns()); n != 1 {
		t.Fatalf("%d children spawned while the first was alive, want 1", n)
	}

	f.releaseSeq(1)
	wantOutput(t, (<-first)[0], "1 list --json")
	waitFor(t, "the shared second child to spawn", f.spawnCount, 2)
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
	waitFor(t, "the first child to spawn", f.spawnCount, 1)
	second := runConcurrently(f.root, run, []string{"list --json"})
	waitFor(t, "the late caller to wait on a flight of its own", gate.coalescer.pendingWaiters, 1)

	f.releaseSeq(1)
	wantOutput(t, (<-first)[0], "1 list --json")
	waitFor(t, "the late caller's child to spawn", f.spawnCount, 2)
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
	// The calls reach their dirs through a symlink, as calls under a macOS
	// temp dir do (/var is /private/var), so a child's physical working
	// directory, not its PWD, names the dir it ran in.
	dirsReal, dirsLink := filepath.Join(f.root, "dirs"), filepath.Join(f.root, "dirs-link")
	for _, d := range []string{filepath.Join(dirsReal, "a"), filepath.Join(dirsReal, "b")} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(dirsReal, dirsLink); err != nil {
		t.Fatal(err)
	}
	dirA, dirB := filepath.Join(dirsLink, "a"), filepath.Join(dirsLink, "b")
	physical := func(dir string) string {
		t.Helper()
		p, err := filepath.EvalSymlinks(dir)
		if err != nil {
			t.Fatal(err)
		}
		return p
	}

	type call struct {
		dir     string
		argv    string
		env     map[string]string
		wantEnv string // a line this call's child must have dumped
	}
	calls := []call{
		{dir: dirA, argv: "list --json", wantEnv: "GC_TEST_CWD=" + physical(dirA)},
		{dir: dirB, argv: "list --json", wantEnv: "GC_TEST_CWD=" + physical(dirB)},
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
	waitFor(t, "every distinct call to spawn its own child", f.aliveCount, len(calls))
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
				t.Errorf("call %d: reading child %s dump: %v", i, seq, err)
				continue
			}
			if !strings.Contains("\n"+string(data), "\n"+want+"\n") {
				t.Errorf("call %d got child %s's output, but that child's dump lacks %q", i, seq, want)
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
	waitFor(t, "the blocking child to spawn", f.spawnCount, 1)
	a := runConcurrently(f.root, f.runner(ctxA, gate), []string{"list --json"})
	b := runConcurrently(f.root, f.runner(context.Background(), gate), []string{"list --json"})
	waitFor(t, "both waiters to join one pending flight", gate.coalescer.pendingWaiters, 2)

	f.releaseSeq(1)
	wantOutput(t, (<-blocker)[0], "1 list --json")
	// The blocker removed its marker before printing, so the one live child
	// is the shared one.
	waitFor(t, "the shared child to start", f.aliveCount, 1)
	pid := f.spawnPIDs()[1]

	cancelA()
	ra := (<-a)[0]
	if !errors.Is(ra.err, context.Canceled) {
		t.Fatalf("canceled waiter got (%q, %v), want context.Canceled", ra.out, ra.err)
	}
	if err := syscall.Kill(pid, 0); err != nil {
		t.Fatalf("the shared child (pid %d) is gone after one of two waiters canceled: %v", pid, err)
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
	waitFor(t, "the child to spawn", f.aliveCount, 1)
	pid := f.spawnPIDs()[0]

	cancel()
	if r := (<-res)[0]; !errors.Is(r.err, context.Canceled) {
		t.Fatalf("canceled caller got (%q, %v), want context.Canceled", r.out, r.err)
	}
	waitFor(t, "the abandoned child to be killed", func() bool {
		return errors.Is(syscall.Kill(pid, 0), syscall.ESRCH)
	}, true)
	waitFor(t, "the gate slot to be released", gate.running.Load, 0)
	waitFor(t, "the key to be dropped", gate.coalescer.keyCount, 0)
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
	waitFor(t, "the blocking child to spawn", f.spawnCount, 1)
	waiters := runConcurrently(f.root, f.runner(ctx, gate), []string{"list --json", "list --json"})
	waitFor(t, "both waiters to join one pending flight", gate.coalescer.pendingWaiters, 2)

	cancel()
	for _, r := range <-waiters {
		if !errors.Is(r.err, ErrBDExecSlotWait) || !errors.Is(r.err, context.Canceled) {
			t.Errorf("waiter of an abandoned flight got (%q, %v), want ErrBDExecSlotWait wrapping context.Canceled", r.out, r.err)
		}
	}
	f.releaseSeq(1)
	wantOutput(t, (<-blocker)[0], "1 list --json")
	waitFor(t, "every key to be dropped", gate.coalescer.keyCount, 0)
	if n := len(f.spawns()); n != 1 {
		t.Fatalf("%d children spawned, want 1: the abandoned flight spawned", n)
	}
	if n := gate.running.Load(); n != 0 {
		t.Fatalf("%d gate slots held after every call returned, want 0", n)
	}
}

// TestBDReadKeyFor_DistinctSpecsNeverShareAKey checks that specs differing in
// any field get distinct keys, including specs whose fields concatenate to
// the same bytes, and that equal specs share one key.
func TestBDReadKeyFor_DistinctSpecsNeverShareAKey(t *testing.T) {
	t.Parallel()
	args := []string{"list", "--json"}
	env := []string{"BEADS_DIR=/city/.beads"}
	specs := map[string]bdChildSpec{
		"base":                    {path: "/bin/bd", dir: "/city", args: args, env: env},
		"path and dir regrouped":  {path: "/bin/bd/city", dir: "", args: args, env: env},
		"dir":                     {path: "/bin/bd", dir: "/town", args: args, env: env},
		"args joined":             {path: "/bin/bd", dir: "/city", args: []string{"list--json"}, env: env},
		"args split elsewhere":    {path: "/bin/bd", dir: "/city", args: []string{"lis", "t--json"}, env: env},
		"arg moved into the env":  {path: "/bin/bd", dir: "/city", args: []string{"list"}, env: []string{"--json", "BEADS_DIR=/city/.beads"}},
		"empty arg":               {path: "/bin/bd", dir: "/city", args: []string{"list", "--json", ""}, env: env},
		"env value":               {path: "/bin/bd", dir: "/city", args: args, env: []string{"BEADS_DIR=/town/.beads"}},
		"no env":                  {path: "/bin/bd", dir: "/city", args: args},
		"credential":              {path: "/bin/bd", dir: "/city", args: args, env: []string{"BEADS_DIR=/city/.beads", "BEADS_DOLT_PASSWORD=p1"}},
		"another credential":      {path: "/bin/bd", dir: "/city", args: args, env: []string{"BEADS_DIR=/city/.beads", "BEADS_DOLT_PASSWORD=p2"}},
		"length prefix in an arg": {path: "/bin/bd", dir: "/city", args: []string{"4:list6:--json"}, env: env},
	}
	seen := map[bdReadKey]string{}
	for name, spec := range specs {
		key := bdReadKeyFor(spec)
		if prev, dup := seen[key]; dup {
			t.Errorf("specs %q and %q share a key", prev, name)
		}
		seen[key] = name
	}
	equal := bdChildSpec{path: "/bin/bd", dir: "/city", args: []string{"list", "--json"}, env: []string{"BEADS_DIR=/city/.beads"}}
	if bdReadKeyFor(equal) != bdReadKeyFor(specs["base"]) {
		t.Error("equal specs got different keys")
	}
}

// TestBDReadCoalesce_AbandonedFlightFinishesAfterItsKeyWentIdle pins the
// order join(F2), leave(F2), finish(F1), fly(F2): F2 queues behind F1's
// running child, its only waiter gives up, and F1's child is reaped before
// F2's goroutine runs. F2 must still find its key, finish without starting a
// child, and leave no key behind. The test runs F2's flight itself, so the
// order is exact.
func TestBDReadCoalesce_AbandonedFlightFinishesAfterItsKeyWentIdle(t *testing.T) {
	t.Parallel()
	c := newBDExecGate(8).coalescer
	spec := bdChildSpec{name: "bd", path: filepath.Join(t.TempDir(), "bd"), dir: t.TempDir(), args: []string{"list", "--json"}}

	f1, created := c.join(context.Background(), spec)
	if !created {
		t.Fatal("the first call joined a flight that did not exist")
	}
	t.Cleanup(f1.cancel)
	if !c.markRunning(f1) {
		t.Fatal("markRunning(F1) = false, want F1's child to start")
	}
	f2, created := c.join(context.Background(), spec)
	if !created {
		t.Fatal("a call made after F1's child started joined F1")
	}
	c.leave(f2)
	c.finish(f1)
	c.fly(f2)

	if f2.started {
		t.Fatal("the abandoned flight started a child")
	}
	if !errors.Is(f2.admitErr, context.Canceled) {
		t.Fatalf("abandoned flight admitErr = %v, want context.Canceled", f2.admitErr)
	}
	if n := c.keyCount(); n != 0 {
		t.Fatalf("%d keys left after every flight finished, want 0", n)
	}
}

// Budgets of the waiter-budget tests. The creator's and the early joiner's
// budgets are the subject: each ends while the flight still serves the last
// joiner. The last joiner's budget must outlast its child's start and exit,
// which race exec.
const (
	creatorBudget     = 100 * time.Millisecond
	earlyJoinerBudget = 300 * time.Millisecond
	lastJoinerBudget  = testutil.ExecRaceTimeout
)

// waiterResult is one caller's view of a coalesced flight.
type waiterResult struct {
	out     string
	status  string
	err     error
	elapsed time.Duration
}

// joinWaiters joins n callers to one new flight for spec without starting
// it, so every caller is a waiter before any of their budgets starts.
func joinWaiters(t *testing.T, c *bdReadCoalescer, spec bdChildSpec, n int) *bdReadFlight {
	t.Helper()
	f, created := c.join(context.Background(), spec)
	if !created {
		t.Fatal("the first caller joined a flight that already existed")
	}
	for i := 2; i <= n; i++ {
		if joined, created := c.join(context.Background(), spec); created || joined != f {
			t.Fatalf("caller %d did not join the pending flight", i)
		}
	}
	return f
}

// waitAsync waits on f for one caller that has joined it, as do waits for a
// caller of the runner: the caller's ctx ends after budget, which is also its
// per-command timeout. The result arrives once wait returns.
func waitAsync(c *bdReadCoalescer, f *bdReadFlight, spec bdChildSpec, budget time.Duration) <-chan waiterResult {
	start := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), budget)
	ch := make(chan waiterResult, 1)
	go func() {
		defer cancel()
		out, status, _, err := c.wait(context.Background(), ctx, f, spec, budget, start)
		ch <- waiterResult{out: strings.TrimSpace(string(out)), status: status, err: err, elapsed: time.Since(start)}
	}()
	return ch
}

// wantOwnDeadline checks that a waiter gave up at its own deadline, never
// earlier, with the error a direct call under the same budget reports: a
// slot-wait timeout while its flight had not started its child, a child
// timeout once it had.
func wantOwnDeadline(t *testing.T, who string, r waiterResult, budget time.Duration, wantStatus string) {
	t.Helper()
	if r.status != wantStatus || !IsTimeoutError(r.err) {
		t.Errorf("%s got status %q, error %v; want status %q and a timeout", who, r.status, r.err, wantStatus)
	}
	if got, want := errors.Is(r.err, ErrBDExecSlotWait), wantStatus == "slot-wait"; got != want {
		t.Errorf("%s error %v: wraps ErrBDExecSlotWait = %t, want %t", who, r.err, got, want)
	}
	if want := "timed out after " + budget.String(); r.err == nil || !strings.Contains(r.err.Error(), want) {
		t.Errorf("%s error %v, want it to contain %q", who, r.err, want)
	}
	if r.elapsed < budget {
		t.Errorf("%s gave up after %s, before its own %s budget ended", who, r.elapsed, budget)
	}
}

// TestBDReadCoalesce_QueuedFlightOutlivesEachWaiterBudget pins whose budget
// bounds a waiter while its flight queues for a read slot: the creator and an
// early joiner each give up with a slot-wait timeout at their own deadlines,
// and the flight keeps queuing for the last joiner, who gets the output of
// the child it spawns once the slot frees.
func TestBDReadCoalesce_QueuedFlightOutlivesEachWaiterBudget(t *testing.T) {
	t.Parallel()
	f := newFakeBD(t)
	gate := newBDExecGate(2) // one read slot
	c := gate.coalescer
	held := runConcurrently(f.root, f.runner(context.Background(), gate), []string{"show gc-hold --json"})
	waitFor(t, "the read slot's holder to spawn", f.aliveCount, 1)

	spec := f.spec("list --json")
	flight := joinWaiters(t, c, spec, 3)
	go c.fly(flight)
	creator := waitAsync(c, flight, spec, creatorBudget)
	early := waitAsync(c, flight, spec, earlyJoinerBudget)
	last := waitAsync(c, flight, spec, lastJoinerBudget)

	wantOwnDeadline(t, "the creator", <-creator, creatorBudget, "slot-wait")
	wantOwnDeadline(t, "the early joiner", <-early, earlyJoinerBudget, "slot-wait")
	if n := c.pendingWaiters(); n != 1 {
		t.Fatalf("%d callers wait on the queued flight after two of its three waiters gave up, want 1", n)
	}

	f.releaseSeq(1)
	wantOutput(t, (<-held)[0], "1 show gc-hold --json")
	waitFor(t, "the flight to spawn its child for the last joiner", f.spawnCount, 2)
	f.releaseSeq(2)
	if r := <-last; r.err != nil || r.out != "2 list --json" {
		t.Fatalf("the last joiner got (%q, %v), want the output of the child spawned for it", r.out, r.err)
	}
}

// TestBDReadCoalesce_SharedChildOutlivesEachWaiterBudget pins whose budget
// bounds a waiter while its flight's child runs: the creator and an early
// joiner each give up with a timeout at their own deadlines, and the child
// keeps running for the last joiner, who gets its output.
func TestBDReadCoalesce_SharedChildOutlivesEachWaiterBudget(t *testing.T) {
	t.Parallel()
	f := newFakeBD(t)
	c := newBDExecGate(8).coalescer
	spec := f.spec("list --json")
	flight := joinWaiters(t, c, spec, 3)
	go c.fly(flight)
	waitFor(t, "the shared child to start", f.aliveCount, 1)
	pid := f.spawnPIDs()[0]

	creator := waitAsync(c, flight, spec, creatorBudget)
	early := waitAsync(c, flight, spec, earlyJoinerBudget)
	last := waitAsync(c, flight, spec, lastJoinerBudget)

	wantOwnDeadline(t, "the creator", <-creator, creatorBudget, "timeout")
	wantOwnDeadline(t, "the early joiner", <-early, earlyJoinerBudget, "timeout")
	if err := syscall.Kill(pid, 0); err != nil {
		t.Fatalf("the shared child (pid %d) is gone while the last joiner still waits: %v", pid, err)
	}
	f.releaseSeq(1)
	if r := <-last; r.err != nil || r.out != "1 list --json" {
		t.Fatalf("the last joiner got (%q, %v), want the shared child's output", r.out, r.err)
	}
}
