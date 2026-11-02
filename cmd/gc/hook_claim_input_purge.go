package main

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/convoy"
	"github.com/gastownhall/gascity/internal/sourceworkflow"
)

// workflowInputState is what the claim-time input check concluded about the
// input convoy of the workflow a candidate belongs to.
type workflowInputState int

const (
	// workflowInputPending is input still to wait on, or input the check
	// could not settle: the candidate stays claimable.
	workflowInputPending workflowInputState = iota
	// workflowInputComplete is input whose tracked work is terminal or handed
	// off to another lane: the workflow is retired as skipped.
	workflowInputComplete
	// workflowInputPurged is input proven to exist in no store of the city:
	// the workflow is retired as a hard failure.
	workflowInputPurged
)

// workflowInputVerdict is the claim-time input check's answer for one
// candidate: the workflow root it resolved to ("" for a candidate outside any
// workflow), the state of that root's input, and, for purged input, the ids
// proven absent.
type workflowInputVerdict struct {
	RootID     string
	State      workflowInputState
	MissingIDs []string
}

// workflowInputAbsenceProver reports whether every one of ids is proven to
// exist in no store of the city. false is "not proven" — a hit anywhere, or
// a store the prover could not or would not read — and never authorizes a
// purge. A nil prover proves nothing.
type workflowInputAbsenceProver func(ctx context.Context, ids []string) (bool, error)

// workflowInputPurgedFailureReason is the gc.failure_reason a purged-input
// retirement stamps.
const workflowInputPurgedFailureReason = "input_purged"

// workflowInputReader is the read a workflow input check makes of a store.
type workflowInputReader interface {
	Get(id string) (beads.Bead, error)
}

// workflowSubtreeCloser is what retiring a workflow needs of the store that
// holds its root and steps.
type workflowSubtreeCloser interface {
	Get(id string) (beads.Bead, error)
	List(query beads.ListQuery) ([]beads.Bead, error)
	CloseAll(ids []string, metadata map[string]string) (int, error)
}

// workflowRootPurgeable reports whether root is one a purged input retires: a
// live graph.v2 workflow root.
func workflowRootPurgeable(root beads.Bead) bool {
	return root.Metadata[beadmeta.FormulaContractMetadataKey] == beadmeta.FormulaContractGraphV2 &&
		!convoy.IsTerminalStatus(root.Status)
}

// workflowInputAbsent reports whether every one of ids misses in work and in
// graph and prove then proves it absent from the rest of the city. A hit in
// either store is "not absent" without asking prove; a read error other than
// not-found is returned.
func workflowInputAbsent(ctx context.Context, graph, work workflowInputReader, ids []string, prove workflowInputAbsenceProver) (bool, error) {
	for _, id := range ids {
		for _, store := range []workflowInputReader{work, graph} {
			_, err := store.Get(id)
			if err == nil {
				return false, nil
			}
			if !errors.Is(err, beads.ErrNotFound) {
				return false, fmt.Errorf("reading workflow input %s: %w", id, err)
			}
		}
	}
	absent, err := prove(ctx, ids)
	if err != nil {
		return false, fmt.Errorf("proving workflow input %s absent: %w", strings.Join(ids, ", "), err)
	}
	return absent, nil
}

// unresolvedMemberIDs returns the ids of the convoy members no store the
// listing read could resolve (convoy.IsUnresolvedTrackedItem placeholders).
func unresolvedMemberIDs(members []beads.Bead) []string {
	var ids []string
	for _, m := range members {
		if convoy.IsUnresolvedTrackedItem(m) {
			ids = append(ids, m.ID)
		}
	}
	return ids
}

// proveInputAbsentAcrossCity proves each of ids absent from every store of the
// city at cityPath: no class binding owns it, and every store a by-id lookup
// of it would read opens and answers not-found. A hit is "not proven"; a
// binding, open or read failure is an error naming the id and the store.
func proveInputAbsentAcrossCity(ctx context.Context, cityPath string, cfg *config.City, ids []string) (bool, error) {
	if cfg == nil {
		return false, nil
	}
	for _, id := range ids {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		_, owned, err := cliByIDBindingOwner(cityPath, id)
		if err != nil {
			return false, fmt.Errorf("resolving binding owner of %s: %w", id, err)
		}
		if owned {
			return false, nil
		}
		for _, dir := range convoyStoreCandidates(cfg, cityPath, id) {
			if err := ctx.Err(); err != nil {
				return false, err
			}
			store, err := openStoreAtForCityWithConfig(dir, cityPath, cfg)
			if err != nil {
				return false, fmt.Errorf("opening store %s to look for %s: %w", dir, id, err)
			}
			if store == nil {
				return false, nil
			}
			_, err = store.Get(id)
			if err == nil {
				return false, nil
			}
			if !errors.Is(err, beads.ErrNotFound) {
				return false, fmt.Errorf("reading %s from store %s: %w", id, dir, err)
			}
		}
	}
	return true, nil
}

// failPurgedWorkflow retires the workflow rooted at rootID whose input no
// longer exists in any store: the root and every nonterminal step close as a
// hard failure with gc.failure_reason=input_purged.
func failPurgedWorkflow(ctx context.Context, store workflowSubtreeCloser, rootID string) error {
	return retireWorkflowSubtree(ctx, store, rootID, map[string]string{
		beadmeta.OutcomeMetadataKey:       beadmeta.OutcomeFail,
		beadmeta.FailureClassMetadataKey:  beadmeta.FailureClassHard,
		beadmeta.FailureReasonMetadataKey: workflowInputPurgedFailureReason,
		"close_reason":                    sourceworkflow.HookInputPurgedWorkflowCloseReason,
	})
}

// retireWorkflowSubtree closes rootID and every nonterminal step under it (by
// gc.root_bead_id, both tiers) with metadata. A step that already finished
// keeps its recorded outcome, and so does the root when it is already
// terminal. Nothing is written once ctx is done.
func retireWorkflowSubtree(ctx context.Context, store workflowSubtreeCloser, rootID string, metadata map[string]string) error {
	steps, err := store.List(beads.ListQuery{
		Metadata: map[string]string{beadmeta.RootBeadIDMetadataKey: rootID},
		TierMode: beads.TierBoth,
	})
	if err != nil {
		return fmt.Errorf("listing steps of %s: %w", rootID, err)
	}
	ids := make([]string, 0, len(steps)+1)
	seen := map[string]bool{}
	for _, step := range steps {
		if step.ID == "" || step.ID == rootID || seen[step.ID] || convoy.IsTerminalStatus(step.Status) {
			continue
		}
		seen[step.ID] = true
		ids = append(ids, step.ID)
	}
	root, err := store.Get(rootID)
	if err != nil {
		return fmt.Errorf("rechecking workflow root %s: %w", rootID, err)
	}
	if !convoy.IsTerminalStatus(root.Status) {
		ids = append(ids, rootID)
	}
	if len(ids) == 0 {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("retiring workflow %s: %w", rootID, err)
	}
	if _, err := store.CloseAll(ids, metadata); err != nil {
		return fmt.Errorf("closing workflow %s: %w", rootID, err)
	}
	return nil
}

// hookRetireWorkflowWithBdStore retires the workflow verdict names in the bd
// store at dir: as a hard failure for purged input, as skipped otherwise.
func hookRetireWorkflowWithBdStore(ctx context.Context, dir string, env []string, verdict workflowInputVerdict) error {
	store := hookClaimBdStoreContext(ctx, dir, env, "")
	if verdict.State == workflowInputPurged {
		return failPurgedWorkflow(ctx, store, verdict.RootID)
	}
	return skipDoneWorkflow(store, verdict.RootID)
}
