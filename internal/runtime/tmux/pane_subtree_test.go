package tmux

import (
	"errors"
	"sync"
	"testing"
	"testing/synctest"
	"time"
)

// A walk whose read stays blocked (a cmdline read of a process wedged with
// its memory map locked) holds its slot past its caller's timeout, so
// repeated probes of a wedged pane add no outstanding reads: a walk of a pid
// whose last walk has not returned, or one past the cap, fails at once and
// starts no read. The slot frees when the read returns. Kills a walk started
// per probe (one more blocked goroutine and thread every time), a slot freed
// at the caller's timeout, and a refused walk that waits before failing.
func TestPaneSubtreeWalksBoundBlockedReads(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var (
			mu      sync.Mutex
			reads   = map[string]int{}
			blocked = map[string]chan struct{}{"100": make(chan struct{}), "200": make(chan struct{})}
		)
		w := &paneSubtreeWalks{limit: 2, timeout: paneSubtreeTimeout, read: func(pid string) (processSnapshot, error) {
			mu.Lock()
			reads[pid]++
			wait := blocked[pid]
			mu.Unlock()
			if wait != nil {
				<-wait
			}
			return newProcessSnapshot([]processRuntimeState{{PID: pid}}), nil
		}}
		readsOf := func(pid string) int {
			mu.Lock()
			defer mu.Unlock()
			return reads[pid]
		}
		walk := func(pid string) (processSnapshot, time.Duration, error) {
			start := time.Now()
			processes, err := w.walk(pid)
			return processes, time.Since(start), err
		}

		// The first walk of a wedged pane times out; its read stays blocked.
		if _, took, err := walk("100"); err == nil || errors.Is(err, errPaneSubtreeBusy) || took != paneSubtreeTimeout {
			t.Fatalf("walk over a blocked read = (%v, %v), want a timeout after %v", took, err, paneSubtreeTimeout)
		}
		// Every later probe of it, while the read is blocked, fails at once.
		for range 3 {
			if _, took, err := walk("100"); !errors.Is(err, errPaneSubtreeBusy) || took != 0 {
				t.Fatalf("walk of a pid whose read is blocked = (%v, %v), want errPaneSubtreeBusy at once", took, err)
			}
		}
		if n := readsOf("100"); n != 1 {
			t.Fatalf("reads of 100 = %d, want 1: repeated timeouts must add no outstanding read", n)
		}
		// A second wedged pane takes the last slot; a third pane finds none.
		if _, _, err := walk("200"); err == nil || errors.Is(err, errPaneSubtreeBusy) {
			t.Fatalf("walk of a second blocked pane = %v, want a timeout", err)
		}
		if _, took, err := walk("300"); !errors.Is(err, errPaneSubtreeBusy) || took != 0 {
			t.Fatalf("walk past the cap = (%v, %v), want errPaneSubtreeBusy at once", took, err)
		}
		if n := readsOf("300"); n != 0 {
			t.Fatalf("reads of 300 = %d, want none past the cap", n)
		}

		// The blocked read returns: its slot frees and the pane walks again,
		// and the slot it held is free for the third pane.
		close(blocked["100"])
		synctest.Wait()
		if got, _, err := walk("100"); err != nil || len(got.byPID) != 1 {
			t.Fatalf("walk after the blocked read returned = (%v, %v), want its subtree", got.byPID, err)
		}
		if _, _, err := walk("300"); err != nil {
			t.Fatalf("walk of a third pane once a slot freed = %v, want its subtree", err)
		}
		if n := readsOf("100"); n != 2 {
			t.Fatalf("reads of 100 = %d, want 2", n)
		}
		close(blocked["200"])
		synctest.Wait()
		w.mu.Lock()
		held := len(w.running)
		w.mu.Unlock()
		if held != 0 {
			t.Fatalf("%d slots still held after every read returned, want none", held)
		}
	})
}
