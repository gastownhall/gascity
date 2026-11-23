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
// closed. The row the work query served is a stale copy of work its owning
// store has finished, so every seat spawned for it is refused the same way.
// The text names no seat, so the claim-loop guard's identical-error streak
// forms across the seats that keep being spawned for it.
var errHookClaimTargetClosed = errors.New("claim refused: the row the claim resolves is closed in its owning store; the served row is a stale copy")

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
