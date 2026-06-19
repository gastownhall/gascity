package sling

import "testing"

// TestSlingDepsGraphrouteDepsForwardsControlDispatcherRuntimeMissing guards the
// new field forwarding that wires the rig→city control-dispatcher fallback
// (#3454) onto the sling graph-routing path. The binding-layer behavior is
// covered by graphroute.TestControlDispatcherBinding_*; this test covers the
// SlingDeps→graphroute.Deps projection that feeds it.
func TestSlingDepsGraphrouteDepsForwardsControlDispatcherRuntimeMissing(t *testing.T) {
	var gotQN string
	deps := SlingDeps{
		ControlDispatcherRuntimeMissing: func(qn string) bool {
			gotQN = qn
			return qn == "gc-contrib/control-dispatcher"
		},
	}
	gr := deps.graphrouteDeps()
	if gr.ControlDispatcherRuntimeMissing == nil {
		t.Fatal("ControlDispatcherRuntimeMissing not forwarded into graphroute.Deps")
	}
	if !gr.ControlDispatcherRuntimeMissing("gc-contrib/control-dispatcher") {
		t.Fatal("forwarded checker should report true for a runtime-missing rig dispatcher")
	}
	if gotQN != "gc-contrib/control-dispatcher" {
		t.Fatalf("closure received %q, want the qualified name passed through verbatim", gotQN)
	}
	if gr.ControlDispatcherRuntimeMissing("gc-contrib/coder") {
		t.Fatal("forwarded checker should report false for a non-dispatcher agent")
	}
}

func TestSlingDepsGraphrouteDepsNilCheckerStaysNil(t *testing.T) {
	if (SlingDeps{}).graphrouteDeps().ControlDispatcherRuntimeMissing != nil {
		t.Fatal("nil checker must stay nil so graphroute leaves the fallback disabled")
	}
}
