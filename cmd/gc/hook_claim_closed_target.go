package main

import (
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/gastownhall/gascity/internal/beads"
)

// errHookClaimTargetClosed is the claim failure a routed candidate records
// when its claim CAS was refused and the canonical row the claim resolved is
// closed. Its usual cause is a stale served copy of work its owning store has
// finished, which refuses every seat spawned for it alike (#5857); a row closed
// between the work query and the claim reads the same. The text states only
// what the hook knows and names no seat, so the claim-loop guard's
// identical-error streak forms across the seats that keep being spawned for a
// stale copy.
var errHookClaimTargetClosed = errors.New("claim refused: the canonical row is closed")

// hookClaimTargetClosed reports whether a refused routed claim's readback is
// the closed canonical row: a claim no seat can win.
func hookClaimTargetClosed(claimed beads.Bead) bool {
	return strings.TrimSpace(claimed.Status) == "closed"
}

// reportHookClaimTargetClosed records a refused claim whose canonical row is
// closed as a claim failure, naming the row's last holder on stderr, so the
// claim-loop guard can withhold the stale row from demand.
func reportHookClaimTargetClosed(candidate, claimed beads.Bead, opts hookClaimOptions, ops hookClaimOps, stderr io.Writer) {
	holder := strings.TrimSpace(claimed.Assignee)
	if holder == "" {
		holder = "no assignee"
	}
	fmt.Fprintf(stderr, "gc hook --claim: skipping %s: %v (held by %s)\n", candidate.ID, errHookClaimTargetClosed, holder) //nolint:errcheck
	ops.emitClaimFailed(candidate.ID, opts, errHookClaimTargetClosed)
}
