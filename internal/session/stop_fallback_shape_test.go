package session

import (
	"context"
	"errors"
	"testing"
)

// TestStopFallbackKeepsTheActorsBusyShape: a Kill handed a lease on
// another runtime refuses it (not busy), so leaseForStop falls back to the
// name's flock; with that flock held by another holder the controller gets
// the bare ErrRuntimeLeaseBusy at once and an operator or agent
// ErrSessionStarting, and nothing is stopped. Deterministic: no race needed.
func TestStopFallbackKeepsTheActorsBusyShape(t *testing.T) {
	for _, c := range []struct {
		kind     ActorKind
		starting bool
	}{{ActorOperator, true}, {ActorAgent, true}, {ActorController, false}} {
		m := newManagerLeaseFixture(t)
		if err := m.start(m.operator(nil)); err != nil {
			t.Fatal(err)
		}
		held := m.hold(t)
		other, err := TryRuntimeLease(nil, RuntimeLeaseRequest{City: m.city, Name: "another-runtime"})
		if err != nil {
			t.Fatal(err)
		}
		by := m.as(c.kind)
		by.Lease = other
		err = m.mgr.Kill(context.Background(), by, m.info.ID)
		if c.starting {
			if !errors.Is(err, ErrSessionStarting) {
				t.Errorf("kind %d: kill falling back under a held flock = %v, want ErrSessionStarting", c.kind, err)
			}
		} else if !errors.Is(err, ErrRuntimeLeaseBusy) || errors.Is(err, ErrSessionStarting) {
			t.Errorf("kind %d: kill falling back under a held flock = %v, want the bare ErrRuntimeLeaseBusy", c.kind, err)
		}
		if len(m.sp.stops) != 0 {
			t.Errorf("kind %d: stopped under another holder's flock (stops %q)", c.kind, m.sp.stops)
		}
		other.Release()
		held.Release()
	}
}
