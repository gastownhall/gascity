package main

// T2 (boot-check-design.md §4.7): acceptContainmentLease's field-by-field
// acceptance matrix, plus the caller-side 10-minute age cap (carry-over 4)
// and the kill switch (containmentLeaseDisabled). Every rejection here
// takes the full check in production — none of this file decides a
// refusal (Q7).

import (
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

// TestAcceptContainmentLeaseOutcomes covers every outcome this build does
// lease (Converged, Genesis, Stranded degraded-serving), each with the
// Serving/ServesDegraded flags a real publisher would compute for it.
func TestAcceptContainmentLeaseOutcomes(t *testing.T) {
	cases := []struct {
		name           string
		outcome        infraMigrationOutcome
		serving        bool
		servesDegraded bool
		stranded       []string
	}{
		{"converged", infraMigrationConverged, true, false, nil},
		{"genesis", infraMigrationGenesis, true, false, nil},
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

// TestFetchContainmentLeaseKillSwitch (carry-over per design doc §4.5,
// Q4): GC_STORAGE_CONTAINMENT_LEASE=off makes fetchContainmentLease behave
// as though no controller answered, without touching the socket at all.
func TestFetchContainmentLeaseKillSwitch(t *testing.T) {
	t.Setenv(containmentLeaseKillSwitchEnv, "off")
	if _, ok := fetchContainmentLease(t.TempDir()); ok {
		t.Fatal("the kill switch must make every fetch fail closed")
	}
}

// TestFetchContainmentLeaseKillSwitchCaseInsensitive proves the match is
// not literal-string-only.
func TestFetchContainmentLeaseKillSwitchCaseInsensitive(t *testing.T) {
	t.Setenv(containmentLeaseKillSwitchEnv, "OFF")
	if _, ok := fetchContainmentLease(t.TempDir()); ok {
		t.Fatal("GC_STORAGE_CONTAINMENT_LEASE=OFF must disable the lease the same as lowercase")
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
