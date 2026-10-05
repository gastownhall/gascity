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

// A proxied scope's readiness is one `bd ping`, and that ping can legitimately
// block for ~45s on a cold start (beads waits up to 15s for the proxy endpoint
// and then up to 30s for the Dolt child). The generic 30s budget killed it
// partway through, so the owned proxied readiness ops get at least 60s while
// direct scopes keep the generic budget unchanged.
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
	if providerOwnedProxiedOpMinTimeout < 60*time.Second {
		t.Errorf("providerOwnedProxiedOpMinTimeout = %v, want at least 60s to cover beads' 15s+30s internal deadlines", providerOwnedProxiedOpMinTimeout)
	}
}
