package main

import (
	"fmt"
	"io"
)

// warnUnauthenticatedReadPlane prints the loud G23 boot warning shared by the
// controller and supervisor serve seams. It is emitted on a non-loopback bind
// that allows mutations — the "hardened bind" that previously booted silent.
//
// The point it makes: write-auth gates MUTATIONS only. The entire READ plane is
// served with no authentication, so anyone who can reach the port can read every
// bead payload, all mail, session peeks and transcripts, and the full event
// stream — including the 202 rig-provisioning progress. The warning enumerates
// that read surface, states the write posture (grant-gated when a verify key is
// configured, else unverified-by-ack behind the network front), and requires the
// operator to put a network/TLS boundary in front of the port.
//
// readAuthInstalled reports whether InstallReadAuth put a read-grant verifier on
// the mux. Read-auth gates ONLY the typed per-city routes (/v0/city/{city}); it
// deliberately does NOT cover the supervisor-scope aggregate event feed
// (/v0/events, /v0/events/stream) nor the default-on /api/* dashboard plane, all
// served on the same listener. So when it is installed the warning is NARROWED —
// not suppressed: suppressing it wholesale would misreport a partially-hardened
// bind as fully authenticated and re-open the exact silent-hardened-bind gap this
// warning exists to prevent. The narrowed form states that city-scoped reads are
// grant-gated, names the aggregate/dashboard surfaces that still require a
// network/TLS front, and still reports the write posture so mutation auth stays
// visible even when read-auth is installed. (The write posture is separately
// enforced fail-closed at boot by InstallWriteAuth's G10 gate.)
//
// It is a projection-layer print: no domain logic and no change to boot control
// flow. Both serve seams call this one helper so the warning string is
// single-sourced (and pinned by write_auth_boot_warning_test.go).
func warnUnauthenticatedReadPlane(w io.Writer, bind string, grantGated, readAuthInstalled bool) {
	// The write posture is reported on every branch: read-auth gates the READ
	// plane, never mutations, so the operator still needs to see whether writes
	// are grant-gated or merely UNVERIFIED-by-ack behind the network front.
	posture := "UNVERIFIED — no write-auth verify key is set; mutations are gated ONLY by the network front (write_auth_allow_unverified acknowledged)"
	if grantGated {
		posture = "grant-gated — every mutation requires a signed X-GC-City-Write grant"
	}
	if readAuthInstalled {
		// City-scoped reads are grant-gated, but the aggregate event feed and the
		// /api dashboard plane are NOT covered by city-scoped read-auth and remain
		// open on the same listener. Name exactly those surfaces so the operator
		// does not read an installed read-auth key as "the whole read plane is
		// authenticated".
		_, _ = fmt.Fprintf(w, `WARNING: %s is a non-loopback bind with mutations enabled — city-scoped reads are grant-gated, but part of the READ plane is still UNAUTHENTICATED.
  Typed /v0/city/{city} reads require a signed X-GC-City-Read grant, but these surfaces do NOT and anyone who can reach this port can read them with no credential:
    - the aggregate event stream (/v0/events, /v0/events/stream), including 202 rig-provisioning progress across every city
    - the /api/* dashboard plane (per-city samplers, run detail/diff, and config reads)
  Write-auth gates MUTATIONS only (posture: %s).
  A network/TLS front (reverse proxy, private network, or firewall) is still REQUIRED for those surfaces, not optional.
`, bind, posture)
		return
	}
	_, _ = fmt.Fprintf(w, `WARNING: %s is a non-loopback bind with mutations enabled — the READ plane is UNAUTHENTICATED.
  Anyone who can reach this port can read, with no credential:
    - beads (work items and their payloads) and mail
    - session peeks and full transcripts
    - the event stream, including 202 rig-provisioning progress
  Write-auth gates MUTATIONS only (posture: %s).
  A network/TLS front (reverse proxy, private network, or firewall) is REQUIRED, not optional.
`, bind, posture)
}
