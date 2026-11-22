//go:build !windows

package beads

import (
	"fmt"
	"testing"
)

// TestExecCommandRunner_BoundsConcurrentBdChildren asserts that the process
// bd spawn chokepoint does not start one child per concurrent caller (#9092).
// Every caller issues a distinct read, so coalescing cannot reduce the count:
// the live children must plateau at the process gate's read ceiling. It uses
// the process gate, so it is deliberately not parallel.
func TestExecCommandRunner_BoundsConcurrentBdChildren(t *testing.T) {
	const callers = 24
	f := newFakeBD(t)
	gate := processBDExecGate()
	readCap := gate.readCap
	if readCap >= callers {
		t.Fatalf("process read ceiling %d must be below %d callers for this test", readCap, callers)
	}
	run := ExecCommandRunnerWithEnv(f.env())

	var argvs []string
	for i := 0; i < callers; i++ {
		argvs = append(argvs, fmt.Sprintf("show gc-%d --json", i))
	}
	sampler := sampleAlive(f)
	results := runConcurrently(f.root, run, argvs)

	waitFor(t, "live bd children to reach the read ceiling", f.aliveCount, readCap)
	// Every read slot is held while those children are alive, so no other
	// caller can spawn before one of them is reaped.
	if gate.reads.TryAcquire(1) {
		gate.reads.Release(1)
		t.Fatalf("the process gate has a free read slot while %d read children are alive", readCap)
	}
	if n := f.spawnCount(); n != readCap {
		t.Errorf("%d bd children spawned while the first %d were blocked, want the read ceiling %d", n, readCap, readCap)
	}
	f.releaseAll()
	got := <-results
	sampler.halt()

	if peak := sampler.peakAll.Load(); peak > int64(readCap) {
		t.Errorf("%d concurrent bd children alive for %d concurrent callers, want at most %d: execCommandRunner has no in-flight bound", peak, callers, readCap)
	}
	for _, r := range got {
		if r.err != nil {
			t.Errorf("%q: %v", r.argv, r.err)
		}
	}
}
