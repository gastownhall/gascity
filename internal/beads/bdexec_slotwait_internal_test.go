//go:build !windows

package beads

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

// saturateGate fills every slot of gate with blocked fake bd children, reads
// up to the read ceiling and mutations for the rest, and returns a channel
// that yields their results once released.
func saturateGate(t *testing.T, f *fakeBD, gate *bdExecGate) <-chan []gateCallResult {
	t.Helper()
	var argvs []string
	for i := 0; i < gate.size; i++ {
		if i < gate.readCap {
			argvs = append(argvs, fmt.Sprintf("show gc-hold%d --json", i))
		} else {
			argvs = append(argvs, fmt.Sprintf("update gc-hold%d --status open", i))
		}
	}
	results := runConcurrently(f.root, f.runner(context.Background(), gate), argvs)
	waitFor(t, "every gate slot to hold a live child", f.aliveCount, gate.size)
	return results
}

// assertGateDrained releases the holding children and checks every slot came
// back: the gate's free slot count is what it was before the test.
func assertGateDrained(t *testing.T, f *fakeBD, gate *bdExecGate, held <-chan []gateCallResult) {
	t.Helper()
	f.releaseAll()
	for _, r := range <-held {
		if r.err != nil {
			t.Errorf("holding call %q: %v", r.argv, r.err)
		}
	}
	if n := gate.running.Load(); n != 0 {
		t.Fatalf("gate reports %d running children after every call returned", n)
	}
	if !gate.total.TryAcquire(int64(gate.size)) {
		t.Fatalf("gate total lane lost slots: cannot take all %d", gate.size)
	}
	gate.total.Release(int64(gate.size))
	if !gate.reads.TryAcquire(int64(gate.readCap)) {
		t.Fatalf("gate read lane lost slots: cannot take all %d", gate.readCap)
	}
	gate.reads.Release(int64(gate.readCap))
}

// TestBDExecGate_SlotWaitCallerDeadline covers AC-3 for a deadline: a call
// queued behind a full gate returns at its runner context's deadline without
// spawning, with an error that wraps ErrBDExecSlotWait and reads as a timeout.
func TestBDExecGate_SlotWaitCallerDeadline(t *testing.T) {
	t.Parallel()
	f := newFakeBD(t)
	gate := newBDExecGate(2)
	held := saturateGate(t, f, gate)

	for _, argv := range []string{"show gc-late --json", "update gc-late --status open"} {
		ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
		_, err := f.runner(ctx, gate)(f.root, "bd", strings.Fields(argv)...)
		cancel()
		if err == nil {
			t.Fatalf("%q succeeded behind a full gate", argv)
		}
		if !errors.Is(err, ErrBDExecSlotWait) {
			t.Errorf("%q error = %v, want it to wrap ErrBDExecSlotWait", argv, err)
		}
		if !IsTimeoutError(err) {
			t.Errorf("%q error = %v, want IsTimeoutError", argv, err)
		}
		if !strings.Contains(err.Error(), "(2/2 bd children running)") {
			t.Errorf("%q error = %v, want the running/size counts", argv, err)
		}
	}
	if n := len(f.spawns()); n != gate.size {
		t.Fatalf("%d children spawned, want only the %d holders", n, gate.size)
	}
	if n := gate.running.Load(); n != int64(gate.size) {
		t.Fatalf("gate running = %d after the timed-out waits, want %d", n, gate.size)
	}
	assertGateDrained(t, f, gate, held)
}

// TestBDExecGate_SlotWaitCanceled covers AC-3 for cancellation: a queued call
// whose parent context is canceled returns without spawning, with an error
// that wraps ErrBDExecSlotWait and context.Canceled.
func TestBDExecGate_SlotWaitCanceled(t *testing.T) {
	t.Parallel()
	f := newFakeBD(t)
	gate := newBDExecGate(2)
	held := saturateGate(t, f, gate)

	for _, argv := range []string{"show gc-late --json", "close gc-late"} {
		ctx, cancel := context.WithCancel(context.Background())
		time.AfterFunc(100*time.Millisecond, cancel)
		_, err := f.runner(ctx, gate)(f.root, "bd", strings.Fields(argv)...)
		cancel()
		if !errors.Is(err, ErrBDExecSlotWait) || !errors.Is(err, context.Canceled) {
			t.Errorf("%q error = %v, want it to wrap ErrBDExecSlotWait and context.Canceled", argv, err)
		}
	}
	if n := len(f.spawns()); n != gate.size {
		t.Fatalf("%d children spawned, want only the %d holders", n, gate.size)
	}
	assertGateDrained(t, f, gate, held)
}

// TestBDExecSlotWaitError_PerCommandTimeout checks the slot-wait deadline
// error names the budget that expired, per command, without waiting for it.
func TestBDExecSlotWaitError_PerCommandTimeout(t *testing.T) {
	t.Parallel()
	start := time.Now()
	parentDeadline, cancel := context.WithDeadline(context.Background(), start.Add(3*time.Second))
	defer cancel()
	cases := []struct {
		name   string
		parent context.Context
		args   []string
		want   string
	}{
		{"read", context.Background(), []string{"list", "--json"}, "timed out after " + bdReadCommandTimeout.String()},
		{"query", context.Background(), []string{"query", "--json"}, "timed out after " + bdQueryCommandTimeout.String()},
		{"graph", context.Background(), []string{"create", "--graph", "x"}, "timed out after " + bdGraphApplyCommandTimeout.String()},
		{"mutation", context.Background(), []string{"update", "gc-1"}, "timed out after " + bdCommandTimeout.String()},
		{"caller deadline", parentDeadline, []string{"update", "gc-1"}, "timed out after 3s (caller deadline)"},
	}
	gate := newBDExecGate(4)
	for _, tc := range cases {
		err := gate.slotWaitError(tc.parent, context.DeadlineExceeded, bdCommandTimeoutFor("bd", tc.args), start)
		if !errors.Is(err, ErrBDExecSlotWait) || !IsTimeoutError(err) {
			t.Errorf("%s: error = %v, want ErrBDExecSlotWait and a timeout", tc.name, err)
		}
		if !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: error = %q, want it to contain %q", tc.name, err, tc.want)
		}
	}
}
