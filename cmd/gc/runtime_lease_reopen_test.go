package main

import (
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/session"
)

// TestReopenNamedSessionBatchClearsTheRuntimeLease: a reopened row holds no
// runtime lease, so a close that left the record cannot hand it back to its
// old holder (SESSION-RUNTIME-012).
func TestReopenNamedSessionBatchClearsTheRuntimeLease(t *testing.T) {
	for _, state := range []string{"active", "stopped"} {
		batch := reopenNamedSessionBatch(state, "", time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC))
		for k, v := range session.RuntimeLeaseClearPatch() {
			if got, ok := batch[k]; !ok || got != v {
				t.Errorf("reopen to %s: %s = %q (set %v), want cleared", state, k, got, ok)
			}
		}
		if _, ok := batch[session.RuntimeLeaseEpochKey]; ok {
			t.Errorf("reopen to %s writes the epoch, which only grows", state)
		}
	}
}
