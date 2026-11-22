//go:build !windows

package beads

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// aliveSampler records the peak number of live bd children, and of live read
// children, until halted.
type aliveSampler struct {
	peakAll, peakReads atomic.Int64
	stop               chan struct{}
	stopOnce           sync.Once
	done               chan struct{}
}

// sampleAlive starts sampling f's live children. The test's cleanup halts the
// sampler too, so a test that fails before halting it never leaves it reading
// the harness after its temp dir is gone.
func sampleAlive(f *fakeBD) *aliveSampler {
	s := &aliveSampler{stop: make(chan struct{}), done: make(chan struct{})}
	go func() {
		defer close(s.done)
		for {
			alive := f.alive()
			if n := int64(len(alive)); n > s.peakAll.Load() {
				s.peakAll.Store(n)
			}
			if n := int64(countReads(alive)); n > s.peakReads.Load() {
				s.peakReads.Store(n)
			}
			select {
			case <-s.stop:
				return
			case <-time.After(2 * time.Millisecond):
			}
		}
	}()
	f.t.Cleanup(s.halt)
	return s
}

// halt stops the sampler and waits for it to exit; calls after the first
// only wait.
func (s *aliveSampler) halt() {
	s.stopOnce.Do(func() { close(s.stop) })
	<-s.done
}

type gateCallResult struct {
	argv string
	out  string
	err  error
}

// runConcurrently issues every argv through run at once and returns the
// results once all calls have returned.
func runConcurrently(dir string, run CommandRunner, argvs []string) <-chan []gateCallResult {
	ch := make(chan []gateCallResult, 1)
	go func() {
		results := make([]gateCallResult, len(argvs))
		var wg sync.WaitGroup
		for i, argv := range argvs {
			wg.Add(1)
			go func() {
				defer wg.Done()
				out, err := run(dir, "bd", strings.Fields(argv)...)
				results[i] = gateCallResult{argv: argv, out: string(out), err: err}
			}()
		}
		wg.Wait()
		ch <- results
	}()
	return ch
}

// TestBDExecGate_BoundsChildren covers AC-1: more bd calls than gate slots,
// reads and mutations mixed with distinct arguments, never run more than N
// children at once nor more than R read children, and every call gets its own
// child's output.
func TestBDExecGate_BoundsChildren(t *testing.T) {
	t.Parallel()
	f := newFakeBD(t)
	gate := newBDExecGate(4)
	if gate.size != 4 || gate.readCap != 3 {
		t.Fatalf("gate size/readCap = %d/%d, want 4/3", gate.size, gate.readCap)
	}
	run := f.runner(context.Background(), gate)

	var argvs []string
	for i := 0; i < 10; i++ {
		argvs = append(argvs, fmt.Sprintf("show gc-r%d --json", i))
	}
	for i := 0; i < 4; i++ {
		argvs = append(argvs, fmt.Sprintf("update gc-m%d --status open", i))
	}

	sampler := sampleAlive(f)
	results := runConcurrently(f.root, run, argvs)

	waitFor(t, "every gate slot to hold a live child", f.aliveCount, gate.size)
	// Every slot is held while those children are alive, so no queued call
	// can spawn before one of them is reaped.
	if gate.total.TryAcquire(1) {
		gate.total.Release(1)
		t.Fatalf("the gate has a free slot while %d children are alive", gate.size)
	}
	if n := len(f.spawns()); n != gate.size {
		t.Fatalf("%d children spawned while all were blocked, want %d", n, gate.size)
	}
	f.releaseAll()
	got := <-results
	sampler.halt()

	if peak := sampler.peakAll.Load(); peak > int64(gate.size) {
		t.Errorf("peak live bd children = %d, want <= %d", peak, gate.size)
	}
	if peak := sampler.peakReads.Load(); peak > int64(gate.readCap) {
		t.Errorf("peak live read children = %d, want <= %d", peak, gate.readCap)
	}
	if n := len(f.spawns()); n != len(argvs) {
		t.Errorf("spawned %d children, want %d", n, len(argvs))
	}
	for _, r := range got {
		if r.err != nil {
			t.Errorf("%q: %v", r.argv, r.err)
			continue
		}
		_, argv, _ := strings.Cut(strings.TrimSpace(r.out), " ")
		if argv != r.argv {
			t.Errorf("%q got output %q, want its own child's output", r.argv, r.out)
		}
	}
}

// TestBDExecGate_VerbLanes pins the lane classification: the read verbs from
// the timeout table (plus query and blocked, issued as reads by the ready
// projection) are read-lane; unknown verbs, mutations, an empty argv and a
// leading flag (the doltlite write prefix) are mutation-lane.
func TestBDExecGate_VerbLanes(t *testing.T) {
	t.Parallel()
	readCases := [][]string{
		{"count"},
		{"list", "--json"},
		{"ready"},
		{"show", "gc-1"},
		{"sql", "select 1"},
		{"stats"},
		{"version"},
		{"query", "--json"},
		{"blocked", "--json"},
	}
	for _, args := range readCases {
		if !bdCommandIsRead(args) {
			t.Errorf("bdCommandIsRead(%q) = false, want true", strings.Join(args, " "))
		}
	}
	mutationCases := [][]string{
		nil,
		{},
		{"update", "gc-1", "--status", "closed"},
		{"close", "gc-1"},
		{"create", "--graph", "plan.json"},
		{"--dolt-auto-commit", "off", "update", "gc-1"},
		{"frobnicate"},
	}
	for _, args := range mutationCases {
		if bdCommandIsRead(args) {
			t.Errorf("bdCommandIsRead(%q) = true, want false", strings.Join(args, " "))
		}
	}
}

// TestBDExecGate_TimeoutTableUnchanged guards the link between the lane
// allow-list and the per-command timeout table: every read verb keeps
// exactly the timeout it had before the gate existed, so admission control
// changes queueing, never budget classification.
func TestBDExecGate_TimeoutTableUnchanged(t *testing.T) {
	t.Parallel()
	want := map[string]time.Duration{
		"count":   bdReadCommandTimeout,
		"list":    bdReadCommandTimeout,
		"ready":   bdReadCommandTimeout,
		"show":    bdReadCommandTimeout,
		"sql":     bdReadCommandTimeout,
		"stats":   bdReadCommandTimeout,
		"version": bdReadCommandTimeout,
		"query":   bdQueryCommandTimeout,
		// blocked is a read by lane but keeps the generic timeout it has
		// always had.
		"blocked": bdCommandTimeout,
	}
	for verb, timeout := range want {
		if got := bdCommandTimeoutFor("bd", []string{verb}); got != timeout {
			t.Errorf("bd %s timeout = %s, want %s", verb, got, timeout)
		}
		if !bdCommandIsRead([]string{verb}) {
			t.Errorf("bd %s is a lane read but bdCommandIsRead says mutation", verb)
		}
	}
	// The read-timeout set in bdCommandTimeoutFor must be a subset of the
	// lane read set, so no command is timed out as a read but prioritized
	// as a mutation.
	for _, verb := range []string{"count", "list", "ready", "show", "sql", "stats", "version"} {
		if bdCommandTimeoutFor("bd", []string{verb}) == bdReadCommandTimeout && !bdCommandIsRead([]string{verb}) {
			t.Errorf("bd %s: read timeout but mutation lane", verb)
		}
	}
}

// TestBDExecGate_MutationProgressesWhileReadsSaturated covers AC-2: with the
// read lane full and more reads queued, bd update and bd close (also behind
// the doltlite "--dolt-auto-commit off" prefix) spawn at once while every read
// child stays blocked and no queued read spawns.
func TestBDExecGate_MutationProgressesWhileReadsSaturated(t *testing.T) {
	t.Parallel()
	f := newFakeBD(t)
	// Size 16 reserves 4 slots, one for each mutation below.
	gate := newBDExecGate(16)
	if gate.readCap != 12 {
		t.Fatalf("gate readCap = %d, want 12", gate.readCap)
	}
	run := f.runner(context.Background(), gate)

	var reads []string
	for i := 0; i < gate.readCap+3; i++ {
		reads = append(reads, fmt.Sprintf("list --json --label r%d", i))
	}
	readResults := runConcurrently(f.root, run, reads)
	waitFor(t, "the read lane to fill", f.aliveCount, gate.readCap)

	mutations := []string{
		"update gc-1 --status in_progress",
		"close gc-2",
		"--dolt-auto-commit off update gc-3 --status closed",
		"--dolt-auto-commit off close gc-4",
	}
	mutationResults := runConcurrently(f.root, run, mutations)
	waitFor(t, "every mutation child to spawn", f.spawnCount, gate.readCap+len(mutations))
	alive := f.alive()
	if n := countReads(alive); n != gate.readCap {
		t.Fatalf("%d read children alive after the mutations spawned, want all %d still blocked", n, gate.readCap)
	}
	if n := countReads(f.spawns()); n != gate.readCap {
		t.Fatalf("%d reads spawned, want %d: a queued read spawned before or with the mutations", n, gate.readCap)
	}

	f.releaseAll()
	for _, r := range append(<-mutationResults, <-readResults...) {
		if r.err != nil {
			t.Errorf("%q: %v", r.argv, r.err)
		}
	}
}

// TestBDExecGateSize covers AC-4: GC_BD_MAX_CHILDREN unset or empty gives the
// default, an integer of at least 2 is taken as is, and anything else gives
// the default with exactly one warning naming the variable.
func TestBDExecGateSize(t *testing.T) {
	t.Parallel()
	cases := []struct {
		raw      string
		want     int
		warnings int
	}{
		{raw: "", want: 16},
		{raw: "  ", want: 16},
		{raw: "2", want: 2},
		{raw: "40", want: 40},
		{raw: " 8 ", want: 8},
		{raw: "1", want: 16, warnings: 1},
		{raw: "0", want: 16, warnings: 1},
		{raw: "-4", want: 16, warnings: 1},
		{raw: "eight", want: 16, warnings: 1},
		{raw: "8.5", want: 16, warnings: 1},
	}
	for _, tc := range cases {
		var warnings []string
		got := bdExecGateSize(tc.raw, func(format string, args ...any) {
			warnings = append(warnings, fmt.Sprintf(format, args...))
		})
		if got != tc.want {
			t.Errorf("bdExecGateSize(%q) = %d, want %d", tc.raw, got, tc.want)
		}
		if len(warnings) != tc.warnings {
			t.Errorf("bdExecGateSize(%q) warned %d times (%q), want %d", tc.raw, len(warnings), warnings, tc.warnings)
		}
		for _, w := range warnings {
			if !strings.Contains(w, "GC_BD_MAX_CHILDREN") {
				t.Errorf("bdExecGateSize(%q) warning %q does not name GC_BD_MAX_CHILDREN", tc.raw, w)
			}
		}
	}
}
