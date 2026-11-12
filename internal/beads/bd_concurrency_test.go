package beads

import (
	"context"
	"testing"
	"time"
)

// TestBDSemaphoreCapsInFlight verifies the read lane admits at most
// defaultBDReadConcurrency holders and queues the rest.
func TestBDSemaphoreCapsInFlight(t *testing.T) {
	slots := make(chan struct{}, defaultBDReadConcurrency)
	for i := 0; i < defaultBDReadConcurrency; i++ {
		if !tryAcquire(slots) {
			t.Fatalf("acquire %d of %d rejected: want admitted", i+1, defaultBDReadConcurrency)
		}
	}
	if tryAcquire(slots) {
		t.Fatal("acquire past cap admitted: want queued")
	}
	<-slots // release one
	if !tryAcquire(slots) {
		t.Fatal("acquire after release rejected: want admitted")
	}
}

// tryAcquire reports whether a non-blocking send into the slot channel succeeds.
func tryAcquire(slots chan struct{}) bool {
	select {
	case slots <- struct{}{}:
		return true
	default:
		return false
	}
}

// TestBDReadOnlyArgs verifies the read/write lane split: read verbs
// take the read lane, mutations and unknown verbs take the write lane.
func TestBDReadOnlyArgs(t *testing.T) {
	cases := []struct {
		args []string
		want bool
	}{
		{[]string{"list", "--json"}, true},
		{[]string{"ready", "--json"}, true},
		{[]string{"show", "gcg-123"}, true},
		{[]string{"query", "--json", "status=open"}, true},
		{[]string{"--json", "list"}, true}, // leading flags skipped
		{[]string{"update", "gcg-1", "--status=closed"}, false},
		{[]string{"close", "gcg-1"}, false},
		{[]string{"create", "x"}, false},
		{[]string{"comments", "add", "x"}, false}, // mutating subcommand
		{[]string{"dep", "add", "a", "b"}, false},
		{[]string{"purge"}, false},      // unknown -> writer lane
		{[]string{"frobulate"}, false},  // unknown -> writer lane
		{[]string{}, false},             // no verb -> writer lane
	}
	for _, tc := range cases {
		if got := bdReadOnlyArgs(tc.args); got != tc.want {
			t.Errorf("bdReadOnlyArgs(%v) = %v, want %v", tc.args, got, tc.want)
		}
	}
}

// TestAcquireSlotQueueWaitSheds verifies a caller waiting on a full
// lane sheds when the queue wait expires instead of blocking forever.
func TestAcquireSlotQueueWaitSheds(t *testing.T) {
	lane := make(chan struct{}, 1)
	lane <- struct{}{} // saturate

	done := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		defer cancel()
		_, err := acquireBDSubprocessSlotOn(ctx, lane)
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("wait on saturated lane returned nil error: want shed")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("saturated lane held caller past shed timeout")
	}
}

// TestAcquireSlotMinBudgetSheds verifies a caller whose parent deadline
// leaves less than bdMinExecBudget after admission releases the slot
// and sheds rather than spawning a doomed child.
func TestAcquireSlotMinBudgetSheds(t *testing.T) {
	lane := make(chan struct{}, 1)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := acquireBDSubprocessSlotOn(ctx, lane); err == nil {
		t.Fatal("acquire with sub-minimum budget returned nil: want shed")
	}
	select {
	case lane <- struct{}{}:
	default:
		t.Fatal("shed caller leaked its slot")
	}
}
