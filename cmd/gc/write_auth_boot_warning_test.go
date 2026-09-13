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
	warnUnauthenticatedReadPlane(&buf, "0.0.0.0", true, false)
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
	warnUnauthenticatedReadPlane(&buf, "10.1.2.3", false, false)
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

// When a read-auth verifier is installed, city-scoped /v0/city reads are
// grant-gated, but the aggregate event feed (/v0/events*) and the /api dashboard
// plane are NOT covered by city-scoped read-auth and stay open on the same
// listener. The warning must be NARROWED — naming exactly those still-open
// surfaces — not suppressed wholesale (which would misreport the bind as fully
// hardened). Regression for the F2 over-suppression finding. The read-plane
// enumeration is posture-independent, but the warning still reports the write
// posture, which differs per grantGated (S1: read-auth branch must not drop the
// mutation-auth signal).
func TestWarnUnauthenticatedReadPlaneNarrowedWhenReadAuthInstalled(t *testing.T) {
	for _, grantGated := range []bool{true, false} {
		var buf bytes.Buffer
		warnUnauthenticatedReadPlane(&buf, "0.0.0.0", grantGated, true /*readAuthInstalled*/)
		out := buf.String()

		if strings.Count(out, "WARNING:") != 1 {
			t.Fatalf("read-auth installed (grantGated=%v): want exactly one WARNING line, got %d:\n%s", grantGated, strings.Count(out, "WARNING:"), out)
		}
		for _, want := range []string{
			"0.0.0.0",
			"/v0/events",
			"/api/",
			"rig-provisioning progress",
			"network/TLS front",
			"REQUIRED",
		} {
			if !strings.Contains(out, want) {
				t.Errorf("narrowed warning (grantGated=%v) missing %q:\n%s", grantGated, want, out)
			}
		}
		// It must acknowledge that city-scoped reads ARE gated rather than claim the
		// entire read plane is unauthenticated.
		if !strings.Contains(out, "X-GC-City-Read") {
			t.Errorf("narrowed warning (grantGated=%v) must name the city-read grant:\n%s", grantGated, out)
		}
		// The write posture must still be reported even when read-auth is installed:
		// grant-gated names the X-GC-City-Write grant, otherwise it is UNVERIFIED.
		if grantGated {
			if !strings.Contains(out, "X-GC-City-Write") {
				t.Errorf("narrowed grant-gated warning must name the write grant X-GC-City-Write:\n%s", out)
			}
			if strings.Contains(out, "UNVERIFIED") {
				t.Errorf("narrowed grant-gated warning must not claim UNVERIFIED write posture:\n%s", out)
			}
		} else {
			if !strings.Contains(out, "UNVERIFIED") {
				t.Errorf("narrowed unverified warning must say UNVERIFIED write posture:\n%s", out)
			}
			if strings.Contains(out, "X-GC-City-Write") {
				t.Errorf("narrowed unverified warning must not name the write grant X-GC-City-Write:\n%s", out)
			}
		}
	}
}
