package main

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/dispatch"
)

type hookHaltClaimedFunc func(ctx context.Context, dir string, env []string, bead beads.Bead, actor string) (string, bool, error)

// haltClaimedHookBead applies [workflows] fail_halts to a bead this session
// just claimed. When a step it needs closed with an outcome that halts its
// dependents, the bead is closed skipped instead of handed to the worker, and
// the caller moves on to the next candidate. The check runs after the claim so
// the bead is this session's alone while it is closed.
//
// Best-effort: an error leaves the claim standing and the step runs, as it
// would with the switch off. Finalize still refuses to pass the workflow root
// over the failed step, so a missed halt costs the work, not the verdict.
func haltClaimedHookBead(ctx context.Context, claimed beads.Bead, opts hookClaimOptions, ops hookClaimOps, dir string, stderr io.Writer) bool {
	if !opts.FailHalts || ops.HaltClaimed == nil {
		return false
	}
	haltedBy, halted, err := ops.HaltClaimed(ctx, dir, opts.Env, claimed, strings.TrimSpace(claimed.Assignee))
	if err != nil {
		fmt.Fprintf(stderr, "gc hook --claim: checking %s for a failed step it needs: %v\n", claimed.ID, err) //nolint:errcheck
		return false
	}
	if halted {
		fmt.Fprintf(stderr, "gc hook --claim: %s halted: it needs %s, which did not succeed\n", claimed.ID, haltedBy) //nolint:errcheck
	}
	return halted
}

// hookHaltClaimedWithBdStore is the production HaltClaimed seam: it reads the
// claimed bead's blockers through the same bd context the claim ran in. A bead
// outside any workflow has no needs edges to grade and is left alone without
// a store read.
func hookHaltClaimedWithBdStore(ctx context.Context, dir string, env []string, bead beads.Bead, actor string) (string, bool, error) {
	if strings.TrimSpace(bead.Metadata[beadmeta.RootBeadIDMetadataKey]) == "" {
		return "", false, nil
	}
	return dispatch.HaltIfNeedsHaltingStep(hookClaimBdStoreContext(ctx, dir, env, actor), bead)
}
