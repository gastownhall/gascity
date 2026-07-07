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
// It is a projection-layer print: no domain logic and no change to boot control
// flow. Both serve seams call this one helper so the warning string is
// single-sourced (and pinned by write_auth_boot_warning_test.go).
func warnUnauthenticatedReadPlane(w io.Writer, bind string, grantGated bool) {
	posture := "UNVERIFIED — no write-auth verify key is set; mutations are gated ONLY by the network front (write_auth_allow_unverified acknowledged)"
	if grantGated {
		posture = "grant-gated — every mutation requires a signed X-GC-City-Write grant"
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
