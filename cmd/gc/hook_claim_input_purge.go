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
	"github.com/gastownhall/gascity/internal/sling"
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
// exist in no store of the city. false with no error is a store that still
// holds one of them; a proof that could not finish — a store it could not
// read, or one that could hold an id but stays unread — is an error naming
// the id and the store. Neither ever authorizes a purge. A nil prover proves
// nothing.
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

// workflowInputAbsent reports whether every one of ids misses in each of legs
// (the stores the claim reads that have not yet answered for it) and prove
// then proves it absent from the rest of the city. A hit in a leg is "not
// absent" without asking prove; a read error other than not-found, or ctx
// ending first, is returned.
func workflowInputAbsent(ctx context.Context, legs []workflowInputReader, ids []string, prove workflowInputAbsenceProver) (bool, error) {
	for _, id := range ids {
		for _, store := range legs {
			if err := ctx.Err(); err != nil {
				return false, fmt.Errorf("reading workflow input %s: %w", id, err)
			}
			_, err := store.Get(id)
			if err == nil {
				return false, nil
			}
			if !errors.Is(err, beads.ErrNotFound) {
				return false, fmt.Errorf("reading workflow input %s: %w", id, err)
			}
		}
	}
	joined := strings.Join(ids, ", ")
	if err := ctx.Err(); err != nil {
		return false, fmt.Errorf("proving workflow input %s absent: %w", joined, err)
	}
	absent, err := prove(ctx, ids)
	if err != nil {
		return false, fmt.Errorf("proving workflow input %s absent: %w", joined, err)
	}
	return absent, nil
}

// purgedIfProven returns pending as a purged verdict naming ids as the missing
// input when workflowInputAbsent proves them absent from legs and the rest of
// the city, and pending unchanged otherwise.
func purgedIfProven(ctx context.Context, pending workflowInputVerdict, legs []workflowInputReader, ids []string, prove workflowInputAbsenceProver) (workflowInputVerdict, error) {
	absent, err := workflowInputAbsent(ctx, legs, ids, prove)
	if err != nil || !absent {
		return pending, err
	}
	return workflowInputVerdict{RootID: pending.RootID, State: workflowInputPurged, MissingIDs: ids}, nil
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
// city at cityPath that could hold it: no class binding owns it, and every
// store a by-id lookup of it reads opens and answers not-found. That leaves a
// suspended rig's store unopened, to keep it quiescent (byIDStoreOpener),
// unless the id's prefix routes to that rig: an id carries the prefix of the
// store that minted it, and a by-id lookup never reads another suspended rig
// for it either. An id whose prefix routes to no store could be in any of
// them, so for it a suspended or unbound rig leaves the proof incomplete
// (workflowInputAbsenceGap). A hit is "not proven"; an incomplete proof, or a
// binding, open or read failure, is an error naming the id and the store.
func proveInputAbsentAcrossCity(ctx context.Context, cityPath string, cfg *config.City, ids []string) (bool, error) {
	return proveInputAbsentAcrossCityWith(ctx, cityPath, cfg, ids, func(dir string) (workflowInputReader, error) {
		return openStoreAtForCityWithConfig(dir, cityPath, cfg)
	})
}

// proveInputAbsentAcrossCityWith is proveInputAbsentAcrossCity reading each
// candidate store through open. The ids are one proof: the first id a store
// still holds ends it.
func proveInputAbsentAcrossCityWith(ctx context.Context, cityPath string, cfg *config.City, ids []string, open func(dir string) (workflowInputReader, error)) (bool, error) {
	if cfg == nil {
		return false, errors.New("no city config names the stores to read")
	}
	for _, id := range ids {
		held, err := workflowInputHeldInCity(ctx, cityPath, cfg, id, open)
		if err != nil || held {
			return false, err
		}
	}
	return true, nil
}

// workflowInputHeldInCity reports whether a store of the city holds id: a
// class binding that owns it, or a hit in a store the proof reads (every store
// a by-id lookup of id reads, the suspended ones it leaves unopened aside). A
// miss everywhere it reads is an error when a store that could hold id stays
// unread (workflowInputAbsenceGap).
func workflowInputHeldInCity(ctx context.Context, cityPath string, cfg *config.City, id string, open func(dir string) (workflowInputReader, error)) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	_, owned, err := cliByIDBindingOwner(cityPath, id)
	if err != nil {
		return false, fmt.Errorf("resolving binding owner of %s: %w", id, err)
	}
	if owned {
		return true, nil
	}
	asleep := byIDSuspendedStoreRoots(cfg, cityPath, id)
	for _, dir := range convoyStoreCandidates(cfg, cityPath, id) { // residency:allow — the list openConvoyStores walks for a by-id lookup of id, minus the suspended stores byIDStoreOpener leaves unopened; an absence proof reads every one, so it decides no binding, namespace or leg order
		if err := ctx.Err(); err != nil {
			return false, err
		}
		if len(asleep[normalizePathForCompare(dir)]) > 0 {
			continue
		}
		held, err := workflowInputHeldInStore(open, dir, id)
		if err != nil || held {
			return held, err
		}
	}
	return false, workflowInputAbsenceGap(cfg, cityPath, id, asleep)
}

// workflowInputHeldInStore reports whether the store at dir holds id. A store
// that will not open, opens no handle, or fails the read is an error naming
// id and dir.
func workflowInputHeldInStore(open func(dir string) (workflowInputReader, error), dir, id string) (bool, error) {
	store, err := open(dir)
	if err != nil {
		return false, fmt.Errorf("opening store %s to look for %s: %w", dir, id, err)
	}
	if store == nil {
		return false, fmt.Errorf("opening store %s to look for %s: no store handle", dir, id)
	}
	_, err = store.Get(id)
	if err == nil {
		return true, nil
	}
	if !errors.Is(err, beads.ErrNotFound) {
		return false, fmt.Errorf("reading %s from store %s: %w", id, dir, err)
	}
	return false, nil
}

// workflowInputAbsenceGap names a store that could hold id but that the
// absence proof leaves unread, as an error, or returns nil when there is
// none. A store could hold id when id's prefix routes to it; when the prefix
// routes to no store, every store of the city could. A rig with no path
// binding is unread, and so is a suspended rig in asleep (the suspended store
// roots the proof leaves unopened).
func workflowInputAbsenceGap(cfg *config.City, cityPath, id string, asleep map[string][]string) error {
	prefix := beadPrefix(cfg, id)
	routed := sling.IsHQPrefix(cfg, prefix)
	for _, rig := range cfg.Rigs {
		if prefix == "" || !strings.EqualFold(rig.EffectivePrefix(), prefix) {
			continue
		}
		routed = true
		if why := unreadRigStore(cityPath, rig, asleep); why != "" {
			return fmt.Errorf("rig %q, which %s's prefix routes to, %s", rig.Name, id, why)
		}
	}
	if routed {
		return nil
	}
	for _, rig := range cfg.Rigs {
		if why := unreadRigStore(cityPath, rig, asleep); why != "" {
			return fmt.Errorf("%s's prefix routes to no store, so rig %q could hold it, and that rig %s", id, rig.Name, why)
		}
	}
	return nil
}

// unreadRigStore says why the absence proof leaves rig's store unread, or
// returns "" when the proof reads it.
func unreadRigStore(cityPath string, rig config.Rig, asleep map[string][]string) string {
	if strings.TrimSpace(rig.Path) == "" {
		return "has no path binding, so its store cannot be read"
	}
	if len(asleep[normalizePathForCompare(resolveStoreScopeRoot(cityPath, rig.Path))]) > 0 {
		return "is suspended, so its store is not read"
	}
	return ""
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
