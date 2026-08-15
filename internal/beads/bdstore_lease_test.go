package beads_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

// --- RenewLease ---

// TestBdStoreRenewLeaseRunsBdHeartbeatAsHolder proves RenewLease reaches bd's
// real lease-renewing heartbeat verb with the holder as the acting identity.
// The claim lease belongs to the bead's assignee, and bd only lets the current
// owner heartbeat, so the controller must renew AS the holder (--actor), not as
// its own identity (gas-76r). fakeRunner fails on any unexpected argv, so a nil
// error here pins the exact invocation.
func TestBdStoreRenewLeaseRunsBdHeartbeatAsHolder(t *testing.T) {
	runner := fakeRunner(map[string]struct {
		out []byte
		err error
	}{
		`bd heartbeat gas-1 --actor holder-a --json`: {
			out: []byte(`{"id":"gas-1"}`),
		},
	})
	s := beads.NewBdStore("/city", runner)
	if err := s.RenewLease("gas-1", "holder-a"); err != nil {
		t.Fatalf("RenewLease() = %v, want nil", err)
	}
}

// TestBdStoreRenewLeaseWrapsRunnerError proves a failed renewal surfaces loudly
// with the bead identity in the error. A renewal that cannot land (holder lost
// the lease, bead closed, bd down) must not be silent: the caller logs it so a
// live session losing its lease is visible (gas-654's silent-success lesson).
func TestBdStoreRenewLeaseWrapsRunnerError(t *testing.T) {
	runner := fakeRunner(map[string]struct {
		out []byte
		err error
	}{
		`bd heartbeat gas-1 --actor holder-a --json`: {
			err: fmt.Errorf("lease not found for holder"),
		},
	})
	s := beads.NewBdStore("/city", runner)
	err := s.RenewLease("gas-1", "holder-a")
	if err == nil {
		t.Fatal("RenewLease() = nil, want error")
	}
	if !strings.Contains(err.Error(), "gas-1") {
		t.Errorf("error %q does not mention the bead id", err)
	}
}

// TestBdStoreRenewLeaseRejectsEmptyHolder proves an empty holder fails fast
// without invoking bd. Without an explicit --actor, bd falls back to the
// process environment's identity, which would renew the lease as the WRONG
// actor — worse than not renewing at all.
func TestBdStoreRenewLeaseRejectsEmptyHolder(t *testing.T) {
	s := beads.NewBdStore("/city", fakeRunner(nil))
	err := s.RenewLease("gas-1", "  ")
	if err == nil {
		t.Fatal("RenewLease() = nil, want error")
	}
	if strings.Contains(err.Error(), "unexpected command") {
		t.Errorf("RenewLease reached bd despite empty holder: %v", err)
	}
}
