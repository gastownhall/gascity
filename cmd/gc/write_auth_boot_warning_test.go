package main

import (
	"bytes"
	"strings"
	"testing"
)

// TestWarnUnauthenticatedReadPlaneGrantGated pins the G23 boot warning string for
// a hardened, grant-gated bind: it names the bind, enumerates the unauthenticated
// read surface, states the grant-gated write posture, and demands a network front.
func TestWarnUnauthenticatedReadPlaneGrantGated(t *testing.T) {
	var buf bytes.Buffer
	warnUnauthenticatedReadPlane(&buf, "0.0.0.0", true)
	out := buf.String()

	if strings.Count(out, "WARNING:") != 1 {
		t.Fatalf("want exactly one WARNING line, got %d:\n%s", strings.Count(out, "WARNING:"), out)
	}
	for _, want := range []string{
		"0.0.0.0",
		"READ plane is UNAUTHENTICATED",
		"beads",
		"mail",
		"transcripts",
		"rig-provisioning progress",
		"grant-gated",
		"X-GC-City-Write",
		"network/TLS front",
		"REQUIRED",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("warning missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "UNVERIFIED") {
		t.Errorf("grant-gated warning must not mention UNVERIFIED:\n%s", out)
	}
}

// TestWarnUnauthenticatedReadPlaneUnverified pins the warning for the ack-knob
// (no verify key) posture: it must call the write plane UNVERIFIED so the
// operator understands mutations are gated only by the network front.
func TestWarnUnauthenticatedReadPlaneUnverified(t *testing.T) {
	var buf bytes.Buffer
	warnUnauthenticatedReadPlane(&buf, "10.1.2.3", false)
	out := buf.String()

	if !strings.Contains(out, "UNVERIFIED") {
		t.Errorf("unverified warning must say UNVERIFIED:\n%s", out)
	}
	if !strings.Contains(out, "10.1.2.3") {
		t.Errorf("warning must name the bind:\n%s", out)
	}
	if strings.Contains(out, "grant-gated") {
		t.Errorf("unverified warning must not claim grant-gated:\n%s", out)
	}
	// The read-surface enumeration is posture-independent.
	for _, want := range []string{"beads", "transcripts", "network/TLS front"} {
		if !strings.Contains(out, want) {
			t.Errorf("warning missing %q:\n%s", want, out)
		}
	}
}
