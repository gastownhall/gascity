package main

// T2 (boot-check-design.md §4.7): acceptContainmentLease's field-by-field
// acceptance matrix, plus the caller-side 10-minute age cap (carry-over 4)
// and the kill switch (containmentLeaseDisabled). Every rejection here
// takes the full check in production — none of this file decides a
// refusal (Q7).

import (
	"strings"
	"sync"
	"testing"
	"time"
)

// goodLeaseFixture returns a wire reply, target and basis that
// acceptContainmentLease accepts, so every T2 case can mutate exactly one
// field off of it.
func goodLeaseFixture() (containmentLeaseWire, infraBindingTarget, containmentBasis) {
	basis := containmentBasis{
		Marker:     "sha256:marker",
		Manifest:   "stamp:manifest",
		Cleared:    "absent",
		Database:   "stamp:database",
		WorkStore:  "work-store-id",
		Classifier: "classifier-digest",
	}
	target := infraBindingTarget{Binding: "infra", Database: "/city/.gc/store/infra.db"}
	wire := containmentLeaseWire{
		V:              containmentLeaseProtocolVersion,
		Binding:        target.Binding,
		Database:       target.Database,
		Outcome:        infraMigrationConverged.String(),
		Serving:        true,
		ServesDegraded: false,
		AgeMS:          1000,
		FreshMS:        60000,
		Complete:       true,
		Basis:          basis,
		ProvenBeads:    7,
	}
	return wire, target, basis
}

// TestAcceptContainmentLeaseAcceptsTheGoodFixture is the control: every T2
// mutation test below changes exactly one field off of this baseline, so
// this proves the baseline itself is accepted before anything is mutated
// off of it.
func TestAcceptContainmentLeaseAcceptsTheGoodFixture(t *testing.T) {
	wire, target, basis := goodLeaseFixture()
	report, ok := acceptContainmentLease(wire, target, basis)
	if !ok {
		t.Fatal("the unmutated fixture was rejected")
	}
	if report.Outcome != infraMigrationConverged || !report.serving() {
		t.Fatalf("report = %+v, want a serving Converged report", report)
	}
}

func TestAcceptContainmentLeaseRejections(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(wire *containmentLeaseWire, target *infraBindingTarget, basis *containmentBasis)
	}{
		{"version mismatch", func(w *containmentLeaseWire, _ *infraBindingTarget, _ *containmentBasis) {
			w.V = 0
		}},
		{"future version", func(w *containmentLeaseWire, _ *infraBindingTarget, _ *containmentBasis) {
			w.V = 2
		}},
		{"not serving, not degraded", func(w *containmentLeaseWire, _ *infraBindingTarget, _ *containmentBasis) {
			w.Serving = false
			w.ServesDegraded = false
		}},
		{"incomplete", func(w *containmentLeaseWire, _ *infraBindingTarget, _ *containmentBasis) {
			w.Complete = false
		}},
		{"wrong binding", func(w *containmentLeaseWire, _ *infraBindingTarget, _ *containmentBasis) {
			w.Binding = "other"
		}},
		{"wrong database", func(w *containmentLeaseWire, _ *infraBindingTarget, _ *containmentBasis) {
			w.Database = "/elsewhere/infra.db"
		}},
		{"empty wire basis", func(w *containmentLeaseWire, _ *infraBindingTarget, _ *containmentBasis) {
			w.Basis = containmentBasis{}
		}},
		{"basis marker differs", func(w *containmentLeaseWire, _ *infraBindingTarget, _ *containmentBasis) {
			w.Basis.Marker = "sha256:different"
		}},
		{"basis manifest differs", func(w *containmentLeaseWire, _ *infraBindingTarget, _ *containmentBasis) {
			w.Basis.Manifest = "stamp:different"
		}},
		{"basis cleared differs", func(w *containmentLeaseWire, _ *infraBindingTarget, _ *containmentBasis) {
			w.Basis.Cleared = "stamp:different"
		}},
		{"basis database stamp differs", func(w *containmentLeaseWire, _ *infraBindingTarget, _ *containmentBasis) {
			w.Basis.Database = "stamp:different"
		}},
		{"basis work-store identity differs", func(w *containmentLeaseWire, _ *infraBindingTarget, _ *containmentBasis) {
			w.Basis.WorkStore = "different-work-store"
		}},
		{"basis classifier differs", func(w *containmentLeaseWire, _ *infraBindingTarget, _ *containmentBasis) {
			w.Basis.Classifier = "different-classifier"
		}},
		{"caller's own basis is empty", func(_ *containmentLeaseWire, _ *infraBindingTarget, b *containmentBasis) {
			*b = containmentBasis{}
		}},
		{"negative age", func(w *containmentLeaseWire, _ *infraBindingTarget, _ *containmentBasis) {
			w.AgeMS = -1
		}},
		{"age exceeds its own fresh_ms", func(w *containmentLeaseWire, _ *infraBindingTarget, _ *containmentBasis) {
			w.FreshMS = 500
		}},
		{"age exceeds the caller's 10-minute cap even under a generous fresh_ms", func(w *containmentLeaseWire, _ *infraBindingTarget, _ *containmentBasis) {
			w.AgeMS = (11 * time.Minute).Milliseconds()
			w.FreshMS = (20 * time.Minute).Milliseconds()
		}},
		{"recheck failing from the work store", func(w *containmentLeaseWire, _ *infraBindingTarget, _ *containmentBasis) {
			w.RecheckFailing = true
			w.RecheckFailingFromWorkStore = true
		}},
		{"garbage outcome string", func(w *containmentLeaseWire, _ *infraBindingTarget, _ *containmentBasis) {
			w.Outcome = "quantum-superposition"
		}},
		{"empty outcome string", func(w *containmentLeaseWire, _ *infraBindingTarget, _ *containmentBasis) {
			w.Outcome = ""
		}},
		{"outcome this build never leases (not-configured)", func(w *containmentLeaseWire, _ *infraBindingTarget, _ *containmentBasis) {
			w.Outcome = infraMigrationNotConfigured.String()
		}},
		{"serving disagrees with the recomputed predicate", func(w *containmentLeaseWire, _ *infraBindingTarget, _ *containmentBasis) {
			// Genesis also serves(), so claiming Converged+Serving=false is an
			// internally inconsistent wire payload a trustworthy publisher
			// never sends.
			w.Serving = false
			w.ServesDegraded = true
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			wire, target, basis := goodLeaseFixture()
			c.mutate(&wire, &target, &basis)
			if _, ok := acceptContainmentLease(wire, target, basis); ok {
				t.Fatalf("%s: accepted a lease that must take the full check", c.name)
			}
		})
	}
}

// TestAcceptContainmentLeaseRecheckFailingNotFromWorkStore is the positive
// half of the recheck_failing case (Q3): a lease marked recheck_failing for
// a reason OTHER than the work store is still accepted, within the cap.
func TestAcceptContainmentLeaseRecheckFailingNotFromWorkStore(t *testing.T) {
	wire, target, basis := goodLeaseFixture()
	wire.RecheckFailing = true
	wire.RecheckFailingFromWorkStore = false
	wire.AgeMS = (9 * time.Minute).Milliseconds()
	wire.FreshMS = (20 * time.Minute).Milliseconds()
	if _, ok := acceptContainmentLease(wire, target, basis); !ok {
		t.Fatal("a recheck_failing lease not caused by the work store, within the cap, must still be accepted")
	}
}

// TestAcceptContainmentLeaseOutcomes covers every outcome acceptContainmentLease
// accepts over the wire (Converged, Stranded degraded-serving), each with
// the Serving/ServesDegraded flags a real publisher would compute for it.
// Genesis is deliberately NOT one of them — see
// TestAcceptContainmentLeaseGenesisIsNeverAccepted — even though
// newContainmentLease publishes a lease for it on the controller side.
func TestAcceptContainmentLeaseOutcomes(t *testing.T) {
	cases := []struct {
		name           string
		outcome        infraMigrationOutcome
		serving        bool
		servesDegraded bool
		stranded       []string
	}{
		{"converged", infraMigrationConverged, true, false, nil},
		{"stranded degraded-serving", infraMigrationStranded, false, true, []string{"wk-1"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			wire, target, basis := goodLeaseFixture()
			wire.Outcome = c.outcome.String()
			wire.Serving = c.serving
			wire.ServesDegraded = c.servesDegraded
			wire.Stranded = c.stranded
			report, ok := acceptContainmentLease(wire, target, basis)
			if !ok {
				t.Fatalf("%s: rejected", c.name)
			}
			if report.Outcome != c.outcome {
				t.Fatalf("%s: Outcome = %v, want %v", c.name, report.Outcome, c.outcome)
			}
		})
	}
}

// TestAcceptContainmentLeaseStrandedNotDegradedIsRejected is the stranded
// counterpart to the matrix above: a Stranded outcome that is NOT
// degraded-serving (HeldDifferent/Retained/ClearUnfinished would be
// nonempty on the real report) must never be accepted on Serving/
// ServesDegraded alone — newContainmentLease never publishes this shape,
// but acceptContainmentLease must not assume that and skip its own check.
func TestAcceptContainmentLeaseStrandedNotDegradedIsRejected(t *testing.T) {
	wire, target, basis := goodLeaseFixture()
	wire.Outcome = infraMigrationStranded.String()
	wire.Serving = false
	wire.ServesDegraded = false
	if _, ok := acceptContainmentLease(wire, target, basis); ok {
		t.Fatal("a stranded, non-degraded lease must take the full check")
	}
}

// TestAcceptContainmentLeaseGenesisIsNeverAccepted is the NIT companion to
// TestAcceptContainmentLeaseOutcomes: Genesis is the first definitive
// verdict a city can ever carry, and a one-shot process must re-prove it
// itself rather than take a stale answer for the one outcome whose whole
// point is a transition this build never gets to repeat. Rejected exactly
// like any other outcome this build does not lease, by the same default
// case in acceptContainmentLease's switch.
func TestAcceptContainmentLeaseGenesisIsNeverAccepted(t *testing.T) {
	wire, target, basis := goodLeaseFixture()
	wire.Outcome = infraMigrationGenesis.String()
	if _, ok := acceptContainmentLease(wire, target, basis); ok {
		t.Fatal("a Genesis lease was accepted; it must always take the full check")
	}
}

// resetContainmentLeaseKillSwitchForTest clears containmentLeaseDisabled's
// once-per-process cache (owner decision: the switch is read once per
// process) so a test that t.Setenv's the var can observe it take effect,
// and restores the previous cache on cleanup.
func resetContainmentLeaseKillSwitchForTest(t *testing.T) {
	t.Helper()
	prev := containmentLeaseDisabledOnce
	containmentLeaseDisabledOnce = sync.OnceValue(readContainmentLeaseKillSwitch)
	t.Cleanup(func() { containmentLeaseDisabledOnce = prev })
}

// TestFetchContainmentLeaseKillSwitch (carry-over per design doc §4.5, Q4;
// owner decision 2026-11-19) serves a REAL lease over a real controller
// socket — so this proves the switch, not merely the absence of a
// controller to ask: with it off, fetchContainmentLease refuses a lease a
// caller without the switch set would accept.
func TestFetchContainmentLeaseKillSwitch(t *testing.T) {
	cityPath, _, _, lease := convergedCityWithRecheckLease(t)
	bornSplitLeaseListener(t, cityPath, lease)

	resetContainmentLeaseKillSwitchForTest(t)
	if _, ok := fetchContainmentLease(cityPath); !ok {
		t.Fatal("fixture: a real lease served over the socket, switch unset, was not fetched")
	}

	resetContainmentLeaseKillSwitchForTest(t)
	t.Setenv(containmentLeaseKillSwitchEnv, "off")
	if _, ok := fetchContainmentLease(cityPath); ok {
		t.Fatal("GC_STORAGE_CONTAINMENT_LEASE=off must refuse a lease a real controller is serving")
	}
}

// TestFetchContainmentLeaseKillSwitchExactValue proves the switch is exact:
// only "off" (case-insensitively) disables it. Any other value — including
// one that merely looks like an intent to disable it — must still fetch
// the lease a real controller is serving.
func TestFetchContainmentLeaseKillSwitchExactValue(t *testing.T) {
	cityPath, _, _, lease := convergedCityWithRecheckLease(t)
	bornSplitLeaseListener(t, cityPath, lease)

	for _, v := range []string{"OFF", "Off", "disabled", "0", "false", "true"} {
		t.Run(v, func(t *testing.T) {
			resetContainmentLeaseKillSwitchForTest(t)
			t.Setenv(containmentLeaseKillSwitchEnv, v)
			_, ok := fetchContainmentLease(cityPath)
			wantOk := !strings.EqualFold(v, "off")
			if ok != wantOk {
				t.Fatalf("%s=%q: fetch ok=%v, want %v", containmentLeaseKillSwitchEnv, v, ok, wantOk)
			}
		})
	}
}

// TestFetchContainmentLeaseGarbageReply proves a reply that does not parse
// as containmentLeaseWire is rejected, not panicked on, over a real
// socket: a controller that predates this wire shape, or one that
// answered something else on this command string entirely, must look
// exactly like "no controller."
func TestFetchContainmentLeaseGarbageReply(t *testing.T) {
	cityDir := t.TempDir()
	lis := listenControllerSocketForTest(t, cityDir)
	go acceptOnceAndReply(t, lis, "not json at all\n")
	if _, ok := fetchContainmentLease(cityDir); ok {
		t.Fatal("a reply that will not parse must be rejected, not accepted or panicked on")
	}
}

// TestAcceptContainmentLeaseAgeOverflowRejected is a NIT: an AgeMS a one-shot
// process never produced itself (18446744073710ms, about 584 years) must
// not survive the int64-millisecond-to-time.Duration conversion by
// overflowing into something small or negative that the cap then lets
// through. acceptContainmentLease now compares entirely in milliseconds.
func TestAcceptContainmentLeaseAgeOverflowRejected(t *testing.T) {
	wire, target, basis := goodLeaseFixture()
	wire.AgeMS = 18446744073710
	if _, ok := acceptContainmentLease(wire, target, basis); ok {
		t.Errorf("age_ms=%d (~584 years) was accepted", wire.AgeMS)
	}
}

// TestAcceptContainmentLeaseBothBasesEmptyRejected is a NIT: two empty
// bases are trivially "equal" by Go's struct comparison, but neither one
// says anything, so the pair must still be rejected (acceptContainmentLease
// already checks wire.Basis.empty() independently of basis.empty(), but
// this pins both being empty at once rather than just one).
func TestAcceptContainmentLeaseBothBasesEmptyRejected(t *testing.T) {
	wire, target, _ := goodLeaseFixture()
	wire.Basis = containmentBasis{}
	if _, ok := acceptContainmentLease(wire, target, containmentBasis{}); ok {
		t.Fatal("two empty bases were accepted")
	}
}
