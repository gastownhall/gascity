package main

import (
	"testing"
	"time"
)

// TestProviderOpTimeoutInitGetsLongWindow guards the config-reload wedge fix:
// "init" must get the long (start/recover-class) timeout, not 30s, so a rig
// bead-store init that creates or migrates a database on a busy shared dolt
// server is not SIGKILLed mid-reload — which previously left the supervisor
// "keeping old config" so newly configured rigs never came online.
func TestProviderOpTimeoutInitGetsLongWindow(t *testing.T) {
	const long = 120 * time.Second
	const short = 30 * time.Second
	for _, op := range []string{"start", "recover", "init"} {
		if got := providerOpTimeout(op); got != long {
			t.Errorf("providerOpTimeout(%q) = %v, want %v", op, got, long)
		}
	}
	for _, op := range []string{"health", "stop", "probe", ""} {
		if got := providerOpTimeout(op); got != short {
			t.Errorf("providerOpTimeout(%q) = %v, want %v", op, got, short)
		}
	}
}

// beadsMaxOpenDuration mirrors beads' maxOpenDuration: the longest a single
// bd ping's open may take (a 45s open deadline, 5s SIGKILL confirmation and
// 5s marker clear). It is the be-6qvlzq contract; a change there reopens that
// ruling, so update this constant deliberately and tell beads and the
// architect.
const beadsMaxOpenDuration = 55 * time.Second

// proxiedReadinessOpWork is the allowance for the rest of bd ping after the
// open: one probe ping at its 10s cap plus process start, schema reads,
// SELECT 1 and kill.
const proxiedReadinessOpWork = 20 * time.Second

// A proxied scope's readiness is one `bd ping`, and that ping can legitimately
// block for up to beads' maxOpenDuration on a cold start. The generic 30s
// budget killed it partway through, so the owned proxied readiness ops get a
// floor that clears maxOpenDuration plus the work after the open, while direct
// scopes keep the generic budget unchanged.
func TestProviderOwnedOpTimeoutWidensProxiedReadiness(t *testing.T) {
	for _, op := range []string{"health", "ensure-ready", "probe"} {
		if got := providerOwnedOpTimeout(op, true); got != providerOwnedProxiedOpMinTimeout {
			t.Errorf("providerOwnedOpTimeout(%q, proxied) = %v, want %v", op, got, providerOwnedProxiedOpMinTimeout)
		}
		if got := providerOwnedOpTimeout(op, false); got != providerOpTimeout(op) {
			t.Errorf("providerOwnedOpTimeout(%q, direct) = %v, want the generic %v", op, got, providerOpTimeout(op))
		}
	}
	// start/recover/init already exceed the floor; widening must not shrink them.
	for _, op := range []string{"start", "recover", "init"} {
		if got := providerOwnedOpTimeout(op, true); got != providerOpTimeout(op) {
			t.Errorf("providerOwnedOpTimeout(%q, proxied) = %v, want %v", op, got, providerOpTimeout(op))
		}
	}
	// Retiring a proxy is a local signal, not a readiness wait.
	if got := providerOwnedOpTimeout("stop", true); got != providerOpTimeout("stop") {
		t.Errorf("providerOwnedOpTimeout(stop, proxied) = %v, want the generic %v", got, providerOpTimeout("stop"))
	}
	if want := beadsMaxOpenDuration + proxiedReadinessOpWork; providerOwnedProxiedOpMinTimeout < want {
		t.Errorf("providerOwnedProxiedOpMinTimeout = %v, want at least %v: beads' maxOpenDuration (%v) plus %v for the rest of bd ping",
			providerOwnedProxiedOpMinTimeout, want, beadsMaxOpenDuration, proxiedReadinessOpWork)
	}
	// The floor must leave the long-lived admission lane a rung above it.
	if providerOwnedProxiedOpMinTimeout >= proxiedLongLivedAdmissionBudget {
		t.Errorf("providerOwnedProxiedOpMinTimeout = %v, want below proxiedLongLivedAdmissionBudget (%v)",
			providerOwnedProxiedOpMinTimeout, proxiedLongLivedAdmissionBudget)
	}
}
