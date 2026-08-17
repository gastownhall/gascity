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
