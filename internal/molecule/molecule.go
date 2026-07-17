// Package molecule instantiates compiled formula recipes as bead molecules
// in a Store. It composes the formula compilation layer (Layer 2) with the
// bead store (Layer 1) to implement Gas City's mechanism #7.
//
// The primary entry points are Cook (compile + instantiate) and Instantiate
// (instantiate a pre-compiled Recipe).
package molecule

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/formula"
)

// Options configures molecule instantiation.
type Options struct {
	// Title overrides the root bead's title. If empty, the formula's
	// default title (or {{title}} placeholder after substitution) is used.
	Title string

	// Vars provides variable values for {{placeholder}} substitution in
	// titles, descriptions, and notes. Formula defaults are applied first;
	// these values take precedence.
	Vars map[string]string

	// ExternalDeps wires recipe steps to already-existing bead IDs.
	// These deps are embedded at create time so readiness and assignment are
	// correct before the recipe becomes visible to workers.
	ExternalDeps []ExternalDep

	// ParentID attaches the molecule to an existing bead. When set, the
	// root bead's ParentID is set to this value.
	ParentID string

	// IdempotencyKey is set as metadata on the root bead atomically with
	// creation. Used by the convergence loop to prevent duplicate wisps
	// on crash-retry.
	IdempotencyKey string

	// PriorityOverride forces every created bead to use the given priority.
	// When nil, each step's compiled priority is used.
	PriorityOverride *int

	// PreserveRootType keeps the root bead's declared type instead of
	// coercing legacy non-workflow roots to molecule containers. Attach uses
	// this for executable sub-DAG roots such as retry attempts.
	PreserveRootType bool

	// DeferAssignees creates assignable beads without an assignee and stores
	// the intended assignee in metadata for later activation.
	DeferAssignees bool
}

const (
	graphApplyTransientRetryDelay = 500 * time.Millisecond

	// DeferredAssigneeMetadataKey stores an assignee withheld during speculative
	// molecule creation. Activating the molecule restores the value as Assignee.
	DeferredAssigneeMetadataKey = beadmeta.DeferredAssigneeMetadataKey

	// DeferredRoutedToMetadataKey stores gc.routed_to withheld during
	// speculative molecule creation.
	DeferredRoutedToMetadataKey = beadmeta.DeferredRoutedToMetadataKey

	// DeferredExecutionRoutedToMetadataKey stores gc.execution_routed_to withheld
	// during speculative molecule creation.
	DeferredExecutionRoutedToMetadataKey = beadmeta.DeferredExecutionRoutedToMetadataKey

	// DeferredTypeMetadataKey stores the bead type withheld during speculative
	// molecule creation. Speculative actionable work is created as a ready-
	// excluded type and restored on activation.
	DeferredTypeMetadataKey = beadmeta.DeferredTypeMetadataKey

	// InstantiatingMetadataKey marks graph workflows that are visible in the
	// store while sequential fallback is still wiring their dependency graph.
	InstantiatingMetadataKey = beadmeta.InstantiatingMetadataKey
)

// FragmentOptions configures instantiation of a rootless recipe fragment into
// an existing workflow root.
type FragmentOptions struct {
	// RootID is the existing workflow root bead ID to stamp onto all created
	// beads as gc.root_bead_id.
	RootID string

	// Vars provides variable values for {{placeholder}} substitution.
	Vars map[string]string

	// ExternalDeps wires fragment steps to already-existing bead IDs.
	// These deps are embedded at create time so readiness and assignment are
	// correct before the fragment becomes visible to workers.
	ExternalDeps []ExternalDep

	// PriorityOverride forces every created bead to use the given priority.
	// When nil, the existing workflow root's priority is inherited.
	PriorityOverride *int
}

// ExternalDep binds a fragment step to an already-existing bead.
type ExternalDep struct {
	StepID      string
	DependsOnID string
	Type        string
}

// Result holds the outcome of molecule instantiation.
type Result struct {
	// RootID is the store-assigned ID of the root bead.
	RootID string

	// GraphWorkflow reports whether the instantiated recipe root is a graph-first
	// workflow head instead of a legacy molecule root.
	GraphWorkflow bool

	// IDMapping maps recipe step IDs to store-assigned bead IDs.
	IDMapping map[string]string

	// Created is the total number of beads created.
	Created int
}

// FragmentResult reports the outcome of fragment instantiation.
type FragmentResult struct {
	IDMapping map[string]string
	Created   int
}

// Cook compiles a formula by name and instantiates it as a molecule.
// This is the convenience wrapper that most callers should use.
func Cook(ctx context.Context, store beads.Store, formulaName string, searchPaths []string, opts Options) (*Result, error) {
	compileVars := opts.Vars
	if compileVars == nil {
		compileVars = map[string]string{}
	}
	recipe, err := formula.CompileWithoutRuntimeVarValidation(ctx, formulaName, searchPaths, compileVars)
	if err != nil {
		return nil, fmt.Errorf("compiling formula %q: %w", formulaName, err)
	}
	if err := ValidateRecipeRuntimeVars(recipe, opts); err != nil {
		return nil, err
	}
	return Instantiate(ctx, store, recipe, opts)
}

// CookOn compiles a formula and attaches it to an existing bead.
// Shorthand for Cook with opts.ParentID set.
func CookOn(ctx context.Context, store beads.Store, formulaName string, searchPaths []string, opts Options) (*Result, error) {
	if opts.ParentID == "" {
		return nil, fmt.Errorf("CookOn requires Options.ParentID")
	}
	return Cook(ctx, store, formulaName, searchPaths, opts)
}

// AttachOptions configures graph-attach mode for late-bound DAG expansion.
type AttachOptions struct {
	// Title overrides the sub-DAG root bead's title.
	Title string

	// Vars provides variable values for {{placeholder}} substitution.
	Vars map[string]string

	// IdempotencyKey prevents duplicate Attach calls. If non-empty, Attach
	// checks for an existing sub-DAG root with this key before creating beads.
	// Stored as gc.idempotency_key on the sub-DAG root bead.
	IdempotencyKey string

	// ExpectedEpoch enables optimistic concurrency control. If > 0, Attach
	// reads gc.control_epoch from the attach bead and aborts with
	// ErrEpochConflict if it doesn't match. On success, the epoch is
	// incremented atomically with the dep wiring.
	//
	// Callers should always use IdempotencyKey together with ExpectedEpoch
	// to ensure crash-recovery correctness.
	ExpectedEpoch int
}

// ErrEpochConflict is returned when AttachOptions.ExpectedEpoch does not match
// the attach bead's gc.control_epoch. This indicates another processor already
// advanced the control bead.
var ErrEpochConflict = errors.New("attach epoch conflict")

// AttachResult holds the outcome of a graph-attach operation.
type AttachResult struct {
	// RootID is the store-assigned ID of the sub-DAG root bead.
	RootID string

	// WorkflowRootID is the gc.root_bead_id inherited from the parent workflow.
	WorkflowRootID string

	// Created is the total number of beads created in the sub-DAG.
	Created int

	// IDMapping maps recipe step IDs to store-assigned bead IDs.
	IDMapping map[string]string

	// Duplicate is true when IdempotencyKey matched an existing sub-DAG.
	// RootID and IDMapping are populated from the existing sub-DAG.
	Duplicate bool
}

// Attach grafts a compiled recipe as a sub-DAG onto an existing workflow bead.
// The attach bead gains a blocking dependency on the sub-DAG root, preventing
// it from closing until the sub-DAG completes. All sub-DAG beads inherit the
// parent workflow's gc.root_bead_id.
//
// This is the core primitive for late-bound DAG expansion — any agent, script,
// or workflow step can call it to expand a bead into a sub-workflow at runtime.
//
// NOTE: Attach mutates the input recipe's Steps metadata in-place, stamping
// gc.root_bead_id, gc.root_store_ref, and gc.idempotency_key onto steps.
// Callers must not reuse the recipe after calling Attach.
//
// Idempotency: if IdempotencyKey is set and a sub-DAG root with that key
// already exists under the attach bead's workflow, the existing result is
// returned with Duplicate=true and no new beads are created.
//
// Fencing: if ExpectedEpoch is set, Attach verifies the attach bead's
// gc.control_epoch matches before proceeding. On success, the epoch is
// incremented. This prevents concurrent processors from spawning duplicate
// attempts.
func Attach(ctx context.Context, store beads.Store, recipe *formula.Recipe, attachBeadID string, opts AttachOptions) (*AttachResult, error) {
	if recipe == nil {
		return nil, fmt.Errorf("recipe is nil")
	}
	if attachBeadID == "" {
		return nil, fmt.Errorf("attach bead ID is required")
	}

	parentBead, err := store.Get(attachBeadID)
	if err != nil {
		return nil, fmt.Errorf("attach bead %s: %w", attachBeadID, err)
	}

	// Resolve the sub-DAG's workflow root through the canonical run chain
	// (workflow_id -> molecule_id -> gc.root_bead_id -> the parent's own id),
	// not gc.root_bead_id alone. A wisp/source bead grafted mid-workflow carries
	// the true top root in workflow_id/molecule_id (written by sling) but no
	// gc.root_bead_id of its own; the old fallback ignored those keys and rooted
	// the whole sub-DAG at the parent's own id, stamping a WRONG gc.root_bead_id
	// onto the attempt container, scope-check, and every child. Downstream
	// reconciliation then enumerated siblings via listByWorkflowRoot(<wrong
	// root>) and burned ralph attempts (maintainer-city incident,
	// gcg-wisp-y785sz). A genuine top-level head with no run chain still
	// self-roots via its own id (ResolveRunID's selfID fallback).
	rootBeadID := beadmeta.ResolveRunID(parentBead.Metadata, attachBeadID, "")
	rootStoreRef := parentBead.Metadata[beadmeta.RootStoreRefMetadataKey]

	// Idempotency: check for existing sub-DAG with the same key.
	// This runs before epoch fencing so that crash-retries with stale epochs
	// still return the existing result instead of failing.
	if opts.IdempotencyKey != "" {
		if existing, err := findExistingAttach(store, recipe, rootBeadID, attachBeadID, opts.IdempotencyKey, opts.ExpectedEpoch); err != nil {
			return nil, fmt.Errorf("idempotency check: %w", err)
		} else if existing != nil {
			return existing, nil
		}
	}

	// Epoch fencing: verify no concurrent processor has advanced the control bead.
	// Only checked for new attaches (not duplicates, which return above).
	//
	// CONTRACT: the idempotency check above MUST keep running before this
	// fence. The authoritative fence is CAS-LAST (after Instantiate+DepAdd),
	// and its ambiguity tolerance — an ambiguous fence error leaves the
	// sub-DAG live because the retry converges through findExistingAttach —
	// is void if anyone reorders the idempotency check behind the fence.
	var fenceWriter beads.ConditionalWriter
	if opts.ExpectedEpoch > 0 {
		currentEpoch := 0
		if raw := parentBead.Metadata[beadmeta.ControlEpochMetadataKey]; raw != "" {
			currentEpoch, _ = strconv.Atoi(raw)
		}
		if currentEpoch != opts.ExpectedEpoch {
			return nil, ErrEpochConflict
		}
		// Resolve the conditional writer BEFORE any side effects: a
		// require-mode refusal on an incapable store must fail closed with
		// zero created beads and an unburned epoch, not neutralize a
		// just-materialized sub-DAG on every attempt.
		w, _, err := beads.ResolveConditionalWriter(store)
		if err != nil {
			return nil, fmt.Errorf("epoch fence on %s: %w", attachBeadID, err)
		}
		fenceWriter = w
	}
	if err := ValidateRecipeRuntimeVars(recipe, Options{Title: opts.Title, Vars: opts.Vars}); err != nil {
		return nil, fmt.Errorf("validate runtime vars: %w", err)
	}

	// Stamp every step with the parent workflow's graph metadata.
	for i := range recipe.Steps {
		if recipe.Steps[i].Metadata == nil {
			recipe.Steps[i].Metadata = make(map[string]string)
		}
		recipe.Steps[i].Metadata[beadmeta.RootBeadIDMetadataKey] = rootBeadID
		if rootStoreRef != "" {
			recipe.Steps[i].Metadata[beadmeta.RootStoreRefMetadataKey] = rootStoreRef
		}
	}

	// Stamp idempotency key on the root step.
	if opts.IdempotencyKey != "" && len(recipe.Steps) > 0 {
		if recipe.Steps[0].Metadata == nil {
			recipe.Steps[0].Metadata = make(map[string]string)
		}
		recipe.Steps[0].Metadata[beadmeta.IdempotencyKeyMetadataKey] = opts.IdempotencyKey
	}

	// Under an active fence writer, the sub-DAG is created SPECULATIVELY:
	// every bead deferred (non-runnable, assignee and routing withheld) and
	// the root marked fence-pending. Racing candidates therefore cannot be
	// claimed or run before winner selection — the fence, not creation order,
	// decides which candidate activates. The legacy path (no conditional
	// writer) keeps today's create-active behavior byte-identical.
	fencedDeferred := opts.ExpectedEpoch > 0 && fenceWriter != nil
	if fencedDeferred && len(recipe.Steps) > 0 {
		if recipe.Steps[0].Metadata == nil {
			recipe.Steps[0].Metadata = make(map[string]string)
		}
		recipe.Steps[0].Metadata[beadmeta.AttachFencePendingMetadataKey] = "true"
	}

	result, err := Instantiate(ctx, store, recipe, Options{
		Title:            opts.Title,
		Vars:             opts.Vars,
		PriorityOverride: clonePriority(parentBead.Priority),
		PreserveRootType: true,
		DeferAssignees:   fencedDeferred,
	})
	if err != nil {
		return nil, fmt.Errorf("instantiate: %w", err)
	}

	// Wire blocking dep: attach bead blocks on sub-DAG root.
	if err := store.DepAdd(attachBeadID, result.RootID, "blocks"); err != nil {
		return nil, fmt.Errorf("dep %s -> %s: %w", attachBeadID, result.RootID, err)
	}

	// Increment epoch after successful attach — the authoritative fence.
	if opts.ExpectedEpoch > 0 {
		if err := advanceAttachEpochFence(store, fenceWriter, attachBeadID, opts.ExpectedEpoch, result); err != nil {
			return nil, err
		}
		if fencedDeferred {
			// Fence committed: this candidate is the winner. Activate it. On
			// a partial activation failure the epoch has already advanced, so
			// the retry converges through findExistingAttach, which finishes
			// activating a fence-committed pending candidate idempotently.
			if err := activateAttachCandidate(store, result.RootID, result.IDMapping); err != nil {
				return nil, fmt.Errorf("activating fenced attach %s (fence committed; retry finishes activation): %w", result.RootID, err)
			}
		}
	}

	return &AttachResult{
		RootID:         result.RootID,
		WorkflowRootID: rootBeadID,
		Created:        result.Created,
		IDMapping:      result.IDMapping,
	}, nil
}

// Attach fence-pending marker states. A candidate settles exactly once:
// the marker moves one-way from "true" (pending, unclaimed) to either
// "activating" (claimed by an activator; step activation is idempotent and
// finishable by any processor) or "failed" (claimed by a neutralizer), and
// "activating" completes to "" (live). Every transition is a CAS on the
// marker, so activation and neutralization can never both win the same
// candidate — the race a plain SetMetadata leaves open (a recovery racer
// activates a candidate while its fence-losing owner neutralizes it).
const (
	attachFencePendingUnclaimed  = "true"
	attachFencePendingActivating = "activating"
	attachFencePendingFailed     = "failed"
)

// claimAttachCandidate moves rootID's fence-pending marker from → to via
// metadata CAS. It returns (true, nil) when this caller won the claim,
// (false, nil) on a clean loss (someone else settled the candidate), and an
// error only on transport/ambiguous failures. Stores without a conditional
// writer never create pending markers (fencedDeferred requires the fence
// writer), so a missing writer here means a mixed-fleet artifact; the
// fallback is the legacy racy read-check-set, which is no worse than the
// pre-claim behavior.
func claimAttachCandidate(store beads.Store, rootID, from, to string) (bool, error) {
	if writer, ok := beads.ConditionalWriterFor(store); ok {
		return writer.CompareAndSetMetadataKey(rootID, beadmeta.AttachFencePendingMetadataKey, from, to)
	}
	b, err := store.Get(rootID)
	if err != nil {
		return false, err
	}
	if b.Metadata[beadmeta.AttachFencePendingMetadataKey] != from {
		return false, nil
	}
	if err := store.SetMetadata(rootID, beadmeta.AttachFencePendingMetadataKey, to); err != nil {
		return false, err
	}
	return true, nil
}

// findExistingAttach checks if a sub-DAG root with the given idempotency key
// already exists in the workflow. Returns nil if not found.
func findExistingAttach(store beads.Store, recipe *formula.Recipe, rootBeadID, attachBeadID, key string, expectedEpoch int) (*AttachResult, error) {
	all, err := store.List(beads.ListQuery{
		Metadata: map[string]string{
			beadmeta.IdempotencyKeyMetadataKey: key,
			beadmeta.RootBeadIDMetadataKey:     rootBeadID,
		},
		TierMode: beads.TierBoth,
	})
	if err != nil {
		return nil, err
	}
	// A failed root only surfaces as an error when NO live root shares the
	// key. Pre-fence, one key meant at most one sub-DAG (crash-partials);
	// with the CAS-last epoch fence, a fence LOSER's neutralized sub-DAG
	// legitimately coexists with the winner's live one under the same key,
	// and convergence depends on the retry finding the winner.
	failedRootID := ""
	var pending []beads.Bead
	var activating []beads.Bead
	for _, b := range all {
		if b.Metadata[beadmeta.IdempotencyKeyMetadataKey] != key {
			continue
		}
		if b.Metadata[beadmeta.RootBeadIDMetadataKey] != rootBeadID {
			continue
		}
		if b.Metadata[beadmeta.MoleculeFailedMetadataKey] == "true" ||
			b.Metadata[beadmeta.AttachFencePendingMetadataKey] == attachFencePendingFailed {
			if failedRootID == "" {
				failedRootID = b.ID
			}
			continue
		}
		switch b.Metadata[beadmeta.AttachFencePendingMetadataKey] {
		case attachFencePendingUnclaimed:
			// A pre-fence speculative candidate (deferred, never activated).
			// It must not be adopted as the duplicate while a live activated
			// root can exist; collect it for deterministic recovery below.
			pending = append(pending, b)
			continue
		case attachFencePendingActivating:
			// Claimed by an activator that has not completed (a crash between
			// claim and marker-clear, or a live co-activator). The claim is
			// the settlement: this candidate IS the survivor; activation is
			// idempotent and finishable by any processor.
			activating = append(activating, b)
			continue
		}
		return adoptExistingAttach(store, recipe, rootBeadID, attachBeadID, expectedEpoch, b)
	}
	if len(activating) > 0 {
		// A claimed-but-incomplete activation outranks every unclaimed
		// candidate: finishing it is convergent, while picking a different
		// winner would race the claim holder. (Two candidates can never both
		// hold an activation claim — the claim is a one-way CAS from the
		// unclaimed state, and recovery neutralizes the rest before claiming
		// its own pick.)
		sort.Slice(activating, func(i, j int) bool { return activating[i].ID < activating[j].ID })
		survivor := activating[0]
		idMapping, err := existingAttachIDMapping(store, recipe, rootBeadID, survivor)
		if err != nil {
			return nil, err
		}
		if err := activateAttachCandidate(store, survivor.ID, idMapping); err != nil {
			return nil, fmt.Errorf("finishing claimed attach candidate %s: %w", survivor.ID, err)
		}
		return adoptExistingAttach(store, recipe, rootBeadID, attachBeadID, expectedEpoch, survivor)
	}
	if len(pending) > 0 {
		// Only unclaimed pending candidates survive under this key: the fence
		// committed (or is unresolved) but no candidate was activated — an
		// ambiguous fence error, or a crash between fence and activation. The
		// epoch value cannot identify the winner (expected+1 is
		// non-identifying), so recovery is DETERMINISTIC instead: every
		// processor picks the lexicographically smallest candidate. The
		// non-winners are claimed-and-neutralized FIRST, then the winner is
		// claimed for activation; any lost claim means a racing processor
		// settled that candidate concurrently (a fence loser neutralizing its
		// own sub-DAG, or another recovery), so recovery surfaces the
		// convergent conflict and the retry re-lists the settled state.
		sort.Slice(pending, func(i, j int) bool { return pending[i].ID < pending[j].ID })
		winner := pending[0]
		for _, loser := range pending[1:] {
			won, claimErr := claimAttachCandidate(store, loser.ID, attachFencePendingUnclaimed, attachFencePendingFailed)
			if claimErr != nil {
				return nil, fmt.Errorf("claiming stale attach candidate %s: %w", loser.ID, claimErr)
			}
			if !won {
				return nil, fmt.Errorf("attach recovery lost the claim on %s (settled concurrently): %w", loser.ID, ErrEpochConflict)
			}
			if err := markFailedReporting(store, []string{loser.ID}); err != nil {
				return nil, fmt.Errorf("neutralizing stale attach candidate %s: %w", loser.ID, err)
			}
			if err := store.DepRemove(attachBeadID, loser.ID); err != nil && !errors.Is(err, beads.ErrNotFound) {
				return nil, fmt.Errorf("detaching stale attach candidate %s: %w", loser.ID, err)
			}
		}
		idMapping, err := existingAttachIDMapping(store, recipe, rootBeadID, winner)
		if err != nil {
			return nil, err
		}
		if err := activateAttachCandidate(store, winner.ID, idMapping); err != nil {
			return nil, fmt.Errorf("activating recovered attach candidate %s: %w", winner.ID, err)
		}
		return adoptExistingAttach(store, recipe, rootBeadID, attachBeadID, expectedEpoch, winner)
	}
	if failedRootID != "" {
		return nil, fmt.Errorf("existing attach root %s for idempotency key %q is marked molecule_failed", failedRootID, key)
	}
	return nil, nil
}

// adoptExistingAttach returns an existing sub-DAG root as the idempotent
// duplicate: it re-wires the blocking edge when missing, advances the epoch
// for the recovered duplicate when still needed, and rebuilds the ID mapping.
func adoptExistingAttach(store beads.Store, recipe *formula.Recipe, rootBeadID, attachBeadID string, expectedEpoch int, b beads.Bead) (*AttachResult, error) {
	deps, err := store.DepList(attachBeadID, "down")
	if err != nil {
		return nil, err
	}
	depExists := false
	for _, d := range deps {
		if d.DependsOnID == b.ID && d.Type == "blocks" {
			depExists = true
			break
		}
	}
	if !depExists {
		if err := store.DepAdd(attachBeadID, b.ID, "blocks"); err != nil {
			return nil, err
		}
	}
	if err := advanceAttachEpochIfNeeded(store, attachBeadID, expectedEpoch); err != nil {
		return nil, err
	}
	idMapping, err := existingAttachIDMapping(store, recipe, rootBeadID, b)
	if err != nil {
		return nil, err
	}
	return &AttachResult{
		RootID:         b.ID,
		WorkflowRootID: rootBeadID,
		IDMapping:      idMapping,
		Duplicate:      true,
	}, nil
}

func advanceAttachEpochIfNeeded(store beads.Store, attachBeadID string, expectedEpoch int) error {
	if expectedEpoch <= 0 {
		return nil
	}
	attachBead, err := store.Get(attachBeadID)
	if err != nil {
		return err
	}
	currentEpoch, _ := strconv.Atoi(strings.TrimSpace(attachBead.Metadata[beadmeta.ControlEpochMetadataKey]))
	if currentEpoch != expectedEpoch {
		return nil
	}
	nextEpoch := expectedEpoch + 1
	writer, _, resolveErr := beads.ResolveConditionalWriter(store)
	if resolveErr != nil {
		return fmt.Errorf("advancing epoch on %s: %w", attachBeadID, resolveErr)
	}
	if writer == nil {
		return store.SetMetadata(attachBeadID, beadmeta.ControlEpochMetadataKey, strconv.Itoa(nextEpoch))
	}
	ok, casErr := writer.CompareAndSetMetadataKey(attachBeadID, beadmeta.ControlEpochMetadataKey,
		strconv.Itoa(expectedEpoch), strconv.Itoa(nextEpoch))
	if ok {
		return nil
	}
	if casErr == nil || beads.IsPreconditionFailed(casErr) {
		// Losing this advance is benign by construction: another processor
		// advanced the epoch for the same recovered duplicate first. Re-read
		// to distinguish that from a spurious conflict on a still-stale epoch,
		// and bound the re-issue to ONE fresh attempt — a conflict that keeps
		// recurring with a stale epoch is cross-key revision interference and
		// surfaces as transient (the attach retries next tick).
		refreshed, getErr := store.Get(attachBeadID)
		if getErr != nil {
			return getErr
		}
		if current, _ := strconv.Atoi(strings.TrimSpace(refreshed.Metadata[beadmeta.ControlEpochMetadataKey])); current != expectedEpoch {
			return nil
		}
		ok2, casErr2 := writer.CompareAndSetMetadataKey(attachBeadID, beadmeta.ControlEpochMetadataKey,
			strconv.Itoa(expectedEpoch), strconv.Itoa(nextEpoch))
		if ok2 {
			return nil
		}
		if casErr2 == nil || beads.IsPreconditionFailed(casErr2) {
			// Either another processor advanced it (benign) or interference
			// persists; both resolve on the next level-triggered pass.
			return nil
		}
		return casErr2
	}
	return casErr
}

// advanceAttachEpochFence advances gc.control_epoch after the sub-DAG and its
// blocking edge exist. The fence is deliberately CAS-LAST: fencing first
// would burn the epoch on a crash between the fence and Instantiate, leaving
// no idempotency record for the retry to converge on and permanently skewing
// the attempt numbering the epoch encodes. CAS-last means both racers may
// fully materialize sub-DAGs before one loses, so the loser neutralizes what
// it just created — only Attach knows the IDs — and feeds the EXISTING
// partial-attach recovery: the molecule_failed mark makes the orphan root
// discoverable (and skippable by findExistingAttach), the blocking-edge
// removal keeps the attach bead off an orphan no processor will run, and the
// returned ErrEpochConflict lets the dispatch layer classify the attempt
// hard-failed. The next level-triggered pass re-enters and converges on the
// winner's sub-DAG through the idempotency check, which runs BEFORE the fence
// by documented contract.
//
// On an AMBIGUOUS fence error the write may have committed and this racer may
// BE the winner: the epoch value is non-identifying (expected+1 is
// indistinguishable from a competitor's increment), so the sub-DAG is left
// live and the error surfaces as transient — tolerable only because the
// retry's idempotency check runs first.
func advanceAttachEpochFence(store beads.Store, writer beads.ConditionalWriter, attachBeadID string, expectedEpoch int, result *Result) error {
	nextEpoch := strconv.Itoa(expectedEpoch + 1)
	if writer == nil {
		if err := store.SetMetadata(attachBeadID, beadmeta.ControlEpochMetadataKey, nextEpoch); err != nil {
			return fmt.Errorf("incrementing epoch on %s: %w", attachBeadID, err)
		}
		return nil
	}
	ok, casErr := writer.CompareAndSetMetadataKey(attachBeadID, beadmeta.ControlEpochMetadataKey,
		strconv.Itoa(expectedEpoch), nextEpoch)
	if ok {
		return nil
	}
	if casErr != nil && !beads.IsPreconditionFailed(casErr) {
		return fmt.Errorf("incrementing epoch on %s: %w", attachBeadID, casErr)
	}
	// Genuine fence loss: a concurrent processor advanced the epoch after our
	// early check. Neutralize the losing sub-DAG. Neutralization errors are
	// PROPAGATED (still wrapped as the convergent epoch conflict): a
	// silently-unmarked candidate could be chosen by idempotency recovery.
	// The candidate stays deferred (never activated), so even when marking
	// fails it is inert — not runnable — until a later pass neutralizes it or
	// recovery deterministically resolves the survivors.
	// Claim the candidate before neutralizing: idempotency recovery on a
	// racing processor may have deterministically picked THIS candidate and
	// activated it. A lost claim means the sub-DAG is live (or being
	// activated) under someone else's adoption — neutralizing it now would
	// kill a root another caller was just handed. Skip and converge: the
	// retry adopts the live root through findExistingAttach.
	if won, claimErr := claimAttachCandidate(store, result.RootID, attachFencePendingUnclaimed, attachFencePendingFailed); claimErr != nil {
		return fmt.Errorf("epoch conflict on %s (claiming losing sub-DAG %s for neutralization failed: %w): %w",
			attachBeadID, result.RootID, claimErr, ErrEpochConflict)
	} else if !won {
		return ErrEpochConflict
	}
	createdIDs := make([]string, 0, len(result.IDMapping))
	for _, id := range result.IDMapping {
		createdIDs = append(createdIDs, id)
	}
	if markErr := markFailedReporting(store, createdIDs); markErr != nil {
		return fmt.Errorf("epoch conflict on %s (neutralizing losing sub-DAG %s failed: %w): %w",
			attachBeadID, result.RootID, markErr, ErrEpochConflict)
	}
	if depErr := store.DepRemove(attachBeadID, result.RootID); depErr != nil && !errors.Is(depErr, beads.ErrNotFound) {
		return fmt.Errorf("epoch conflict on %s (detaching losing sub-DAG %s failed: %w): %w",
			attachBeadID, result.RootID, depErr, ErrEpochConflict)
	}
	return ErrEpochConflict
}

func existingAttachIDMapping(store beads.Store, recipe *formula.Recipe, rootBeadID string, root beads.Bead) (map[string]string, error) {
	idMapping := map[string]string{}
	if recipe == nil {
		return idMapping, nil
	}
	wantedRefs := map[string][]string{}
	for i, step := range recipe.Steps {
		if i == 0 || step.IsRoot {
			idMapping[step.ID] = root.ID
			continue
		}
		for _, ref := range attachStepRefs(step) {
			wantedRefs[ref] = append(wantedRefs[ref], step.ID)
		}
	}
	if len(wantedRefs) == 0 {
		return idMapping, nil
	}
	all, err := store.List(beads.ListQuery{
		Metadata: map[string]string{beadmeta.RootBeadIDMetadataKey: rootBeadID},
		TierMode: beads.TierBoth,
	})
	if err != nil {
		return nil, err
	}
	for _, bead := range all {
		if bead.Metadata[beadmeta.MoleculeFailedMetadataKey] == "true" {
			continue
		}
		ref := strings.TrimSpace(bead.Metadata[beadmeta.StepRefMetadataKey])
		if ref == "" {
			ref = strings.TrimSpace(bead.Ref)
		}
		if ref == "" {
			continue
		}
		for _, stepID := range wantedRefs[ref] {
			if idMapping[stepID] == "" {
				idMapping[stepID] = bead.ID
			}
		}
	}
	return idMapping, nil
}

func attachStepRefs(step formula.RecipeStep) []string {
	refs := make([]string, 0, 2)
	if ref := strings.TrimSpace(step.Metadata[beadmeta.StepRefMetadataKey]); ref != "" {
		refs = append(refs, ref)
	}
	if id := strings.TrimSpace(step.ID); id != "" {
		duplicate := false
		for _, ref := range refs {
			if ref == id {
				duplicate = true
				break
			}
		}
		if !duplicate {
			refs = append(refs, id)
		}
	}
	return refs
}

// Instantiate creates beads from a pre-compiled Recipe. Use this when
// you need to inspect or modify the Recipe before instantiation.
//
// Steps are created in order (root first, then children depth-first).
// Dependencies are wired after all beads exist. On partial failure,
// already-created beads are marked with "molecule_failed" metadata
// for cleanup.
func Instantiate(ctx context.Context, store beads.Store, recipe *formula.Recipe, opts Options) (*Result, error) {
	if recipe == nil {
		return nil, fmt.Errorf("recipe is nil")
	}
	if len(recipe.Steps) == 0 {
		return nil, fmt.Errorf("recipe %q has no steps", recipe.Name)
	}
	if !opts.DeferAssignees && IsGraphApplyEnabled() {
		if applier, ok := beads.GraphApplyFor(store); ok {
			result, err := instantiateViaGraphApply(ctx, applier, recipe, opts)
			if err == nil {
				return result, nil
			}
			if !isTransientGraphApplyError(err) {
				return nil, err
			}
			graphApplyTracef("graph-apply transient-error retry recipe=%s err=%v", recipe.Name, err)
			timer := time.NewTimer(graphApplyTransientRetryDelay)
			defer timer.Stop()
			select {
			case <-timer.C:
			case <-ctx.Done():
				return nil, fmt.Errorf("retrying graph apply for recipe %q: %w", recipe.Name, ctx.Err())
			}
			result, retryErr := instantiateViaGraphApply(ctx, applier, recipe, opts)
			if retryErr == nil {
				return result, nil
			}
			if !isTransientGraphApplyError(retryErr) {
				return nil, retryErr
			}
			graphApplyTracef("graph-apply transient-error fallback recipe=%s first_err=%v retry_err=%v", recipe.Name, err, retryErr)
		} else {
			graphApplyTracef("graph-apply unavailable recipe=%s store=%T", recipe.Name, store)
		}
	}

	// Merge variable defaults from recipe with caller-provided vars.
	vars := applyVarDefaults(opts.Vars, recipe.Vars)
	priorityOverride := clonePriority(opts.PriorityOverride)

	// Build the list of beads to create.
	idMapping := make(map[string]string, len(recipe.Steps))
	createdParentByStep := make(map[string]string, len(recipe.Steps))
	var createdIDs []string
	embeddedDeps := make(map[string]bool)
	pendingAssignees := make(map[string]string)
	graphWorkflow := preservesGraphActionTypes(recipe)
	fenceGraphWorkflow := graphWorkflow && !opts.DeferAssignees
	externalDepsByStep, err := groupExternalDeps(opts.ExternalDeps)
	if err != nil {
		return nil, err
	}
	recipeParentByStep := recipeParentDeps(recipe.Deps)

	for i, step := range recipe.Steps {
		// For RootOnly recipes, only create the root bead.
		if recipe.RootOnly && i > 0 {
			break
		}

		b := stepToBead(step, vars, priorityOverride)
		if opts.DeferAssignees {
			deferBeadRouting(&b)
		}
		hasFutureBlocker := false
		for _, dep := range recipe.Deps {
			if dep.StepID != step.ID || dep.Type == "parent-child" {
				continue
			}
			dependsOnBeadID, ok := idMapping[dep.DependsOnID]
			if !ok || dependsOnBeadID == "" {
				hasFutureBlocker = true
				continue
			}
			if dep.Type == "blocks" {
				b.Needs = append(b.Needs, dependsOnBeadID)
			} else {
				b.Needs = append(b.Needs, dep.Type+":"+dependsOnBeadID)
			}
			embeddedDeps[dep.StepID+"|"+dep.DependsOnID+"|"+dep.Type] = true
		}
		for _, dep := range externalDepsByStep[step.ID] {
			if dep.Type == "parent-child" {
				continue
			}
			if dep.Type == "blocks" {
				b.Needs = append(b.Needs, dep.DependsOnID)
			} else {
				b.Needs = append(b.Needs, dep.Type+":"+dep.DependsOnID)
			}
		}
		// Root bead overrides.
		if step.IsRoot {
			if !opts.PreserveRootType && !preserveExecutableRootType(step) {
				b.Type = "molecule"
			}
			b.Ref = recipe.Name
			if opts.Title != "" {
				b.Title = formula.Substitute(opts.Title, vars)
			}
			if opts.ParentID != "" && step.Metadata[beadmeta.KindMetadataKey] != beadmeta.KindWorkflow {
				b.ParentID = opts.ParentID
			}
			if b.Metadata == nil {
				b.Metadata = make(map[string]string, 4)
			}
			if recipe.ContentHash != "" {
				b.Metadata[beadmeta.FormulaHashMetadataKey] = recipe.ContentHash
			}
			if recipe.FormulaSource != "" {
				b.Metadata[beadmeta.FormulaSourceMetadataKey] = recipe.FormulaSource
			}
			if opts.IdempotencyKey != "" {
				b.Metadata["idempotency_key"] = opts.IdempotencyKey
			}
			stampFormulaVars(vars, &b)
		} else {
			// graph.v2 workflows and their retry/Ralph attempt sub-recipes
			// use step beads as independently routable actionable work, not
			// scaffolding — skip the #1039 coercion so Ready() still surfaces
			// them for worker claim.
			if !graphWorkflow {
				b.Type = nonRootStepBeadType(b.Type)
			}
			// Non-root beads: resolve ParentID from the parent-child deps.
			for _, dep := range recipe.Deps {
				if dep.StepID == step.ID && dep.Type == "parent-child" {
					if parentBeadID, ok := idMapping[dep.DependsOnID]; ok {
						b.ParentID = parentBeadID
					}
					break
				}
			}
			for _, dep := range externalDepsByStep[step.ID] {
				if b.ParentID == "" && recipeParentByStep[step.ID] == "" && dep.Type == "parent-child" && dep.DependsOnID != "" {
					b.ParentID = dep.DependsOnID
					break
				}
			}
			// Set Ref to the step ID suffix (after the formula name prefix).
			b.Ref = step.ID
			if b.Metadata == nil {
				b.Metadata = make(map[string]string, 1)
			}
			if b.Metadata[beadmeta.StepRefMetadataKey] == "" {
				b.Metadata[beadmeta.StepRefMetadataKey] = step.ID
			}

			if graphWorkflow || step.Metadata[beadmeta.KindMetadataKey] != "" {
				if b.Metadata[beadmeta.RootBeadIDMetadataKey] == "" {
					if rootBeadID, ok := idMapping[recipe.Steps[0].ID]; ok && rootBeadID != "" {
						b.Metadata[beadmeta.RootBeadIDMetadataKey] = rootBeadID
					}
				}
			}

			// Inline Ralph attempt beads need the actual logical bead ID at runtime.
			// Stamp it during instantiation while the recipe-step -> bead mapping is live.
			if logicalStepID, ok := logicalRecipeStepID(step); ok {
				if logicalBeadID, exists := idMapping[logicalStepID]; exists {
					if b.Metadata == nil {
						b.Metadata = make(map[string]string, 1)
					}
					b.Metadata[beadmeta.LogicalBeadIDMetadataKey] = logicalBeadID
				}
			}

			// Graph-first workflows must not expose partially wired steps to
			// live workers. Create non-root beads unassigned, wire the full graph,
			// then assign them in a final pass.
			if graphWorkflow && b.Assignee != "" && hasFutureBlocker {
				pendingAssignees[step.ID] = b.Assignee
				b.Assignee = ""
			}
		}
		if fenceGraphWorkflow {
			fenceGraphWorkflowBead(&b)
		}

		// Catch unresolved {{...}} in the bead title — the field agents see
		// first. Unresolved placeholders here cause agent churn (#618).
		// Description is intentionally excluded: formulas may embed {{...}}
		// as agent-readable templates resolved at claim time.
		if strings.Contains(b.Title, "{{") {
			if residual := formula.CheckResidualVars(b.Title); len(residual) > 0 {
				markFailed(store, createdIDs)
				return nil, fmt.Errorf("step %q: bead title contains unresolved variable(s) %s — missing or misspelled --var(s)?", step.ID, strings.Join(residual, ", "))
			}
		}
		if err := validateTimeoutMetadataVars(step.ID, b.Metadata); err != nil {
			markFailed(store, createdIDs)
			return nil, err
		}

		created, err := store.Create(b)
		if err != nil {
			// Best-effort cleanup: mark already-created beads as failed.
			markFailed(store, createdIDs)
			return nil, fmt.Errorf("creating bead for step %q: %w", step.ID, err)
		}

		idMapping[step.ID] = created.ID
		createdParentByStep[step.ID] = created.ParentID
		createdIDs = append(createdIDs, created.ID)

	}

	// Wire dependencies using the IDMapping.
	if !recipe.RootOnly {
		for _, dep := range recipe.Deps {
			fromID, fromOK := idMapping[dep.StepID]
			toID, toOK := idMapping[dep.DependsOnID]
			if !fromOK || !toOK {
				continue // step was filtered out (RootOnly or condition)
			}
			if embeddedDeps[dep.StepID+"|"+dep.DependsOnID+"|"+dep.Type] {
				continue
			}
			if dep.Type == "parent-child" && createdParentByStep[dep.StepID] != toID {
				parentID := toID
				if err := store.Update(fromID, beads.UpdateOpts{ParentID: &parentID}); err != nil {
					markFailed(store, createdIDs)
					return nil, fmt.Errorf("setting parent for dep %s->%s: %w", dep.StepID, dep.DependsOnID, err)
				}
				createdParentByStep[dep.StepID] = toID
			}
			if err := store.DepAdd(fromID, toID, dep.Type); err != nil {
				markFailed(store, createdIDs)
				return nil, fmt.Errorf("wiring dep %s->%s: %w", dep.StepID, dep.DependsOnID, err)
			}
		}
	}
	for stepID, deps := range externalDepsByStep {
		if recipeParentByStep[stepID] != "" {
			continue
		}
		fromID, fromOK := idMapping[stepID]
		if !fromOK {
			continue
		}
		for _, dep := range deps {
			if dep.Type != "parent-child" {
				continue
			}
			if err := store.DepAdd(fromID, dep.DependsOnID, dep.Type); err != nil {
				markFailed(store, createdIDs)
				return nil, fmt.Errorf("wiring external dep %s->%s: %w", stepID, dep.DependsOnID, err)
			}
		}
	}

	if graphWorkflow {
		for stepID, assignee := range pendingAssignees {
			if assignee == "" {
				continue
			}
			beadID, ok := idMapping[stepID]
			if !ok || beadID == "" {
				continue
			}
			if err := store.Update(beadID, beads.UpdateOpts{Assignee: &assignee}); err != nil {
				markFailed(store, createdIDs)
				return nil, fmt.Errorf("assigning graph step %q: %w", stepID, err)
			}
		}
	}

	if fenceGraphWorkflow {
		for _, step := range recipe.Steps {
			beadID := idMapping[step.ID]
			if beadID == "" {
				continue
			}
			if err := activateFencedGraphWorkflowBead(store, beadID); err != nil {
				markFailed(store, createdIDs)
				return nil, fmt.Errorf("activating graph step %q: %w", step.ID, err)
			}
		}
	}

	rootID := ""
	if len(createdIDs) > 0 {
		rootID = createdIDs[0]
	}

	return &Result{
		RootID:        rootID,
		GraphWorkflow: graphWorkflow,
		IDMapping:     idMapping,
		Created:       len(createdIDs),
	}, nil
}

// InstantiateFragment creates beads from a rootless recipe fragment and stamps
// them onto an existing workflow root.
func InstantiateFragment(ctx context.Context, store beads.Store, recipe *formula.FragmentRecipe, opts FragmentOptions) (*FragmentResult, error) {
	_ = ctx

	if recipe == nil {
		return nil, fmt.Errorf("recipe is nil")
	}
	if opts.RootID == "" {
		return nil, fmt.Errorf("fragment instantiation requires RootID")
	}
	if len(recipe.Steps) == 0 {
		return &FragmentResult{IDMapping: map[string]string{}}, nil
	}
	priorityOverride := clonePriority(opts.PriorityOverride)
	if priorityOverride == nil {
		root, err := store.Get(opts.RootID)
		if err != nil {
			return nil, fmt.Errorf("loading root bead %s: %w", opts.RootID, err)
		}
		priorityOverride = clonePriority(root.Priority)
	}
	if IsGraphApplyEnabled() {
		if applier, ok := beads.GraphApplyFor(store); ok {
			opts.PriorityOverride = priorityOverride
			return instantiateFragmentViaGraphApply(ctx, store, applier, recipe, opts)
		}
		graphApplyTracef("graph-apply fragment-unavailable root=%s store=%T", opts.RootID, store)
	}

	vars := applyVarDefaults(opts.Vars, recipe.Vars)
	idMapping := make(map[string]string, len(recipe.Steps))
	var createdIDs []string
	createdParentByStep := make(map[string]string, len(recipe.Steps))
	embeddedDeps := make(map[string]bool)
	pendingAssignees := make(map[string]string)
	existingLogicalBeadIDs, err := existingLogicalBeadIDIndex(store, opts.RootID)
	if err != nil {
		return nil, fmt.Errorf("indexing existing logical beads: %w", err)
	}
	externalDepsByStep, err := groupExternalDeps(opts.ExternalDeps)
	if err != nil {
		return nil, err
	}
	recipeParentByStep := recipeParentDeps(recipe.Deps)

	for _, step := range recipe.Steps {
		b := stepToBead(step, vars, priorityOverride)
		// Fragment entries stay "task" — unlike formula scaffolding steps,
		// fanout-expanded fragment beads are actionable work that pool
		// workers claim from `bd ready`. Do not apply nonRootStepBeadType
		// here (#1039).
		hasFutureBlocker := false
		for _, dep := range recipe.Deps {
			if dep.StepID != step.ID || dep.Type == "parent-child" {
				continue
			}
			dependsOnBeadID, ok := idMapping[dep.DependsOnID]
			if !ok || dependsOnBeadID == "" {
				hasFutureBlocker = true
				continue
			}
			if dep.Type == "blocks" {
				b.Needs = append(b.Needs, dependsOnBeadID)
			} else {
				b.Needs = append(b.Needs, dep.Type+":"+dependsOnBeadID)
			}
			embeddedDeps[dep.StepID+"|"+dep.DependsOnID+"|"+dep.Type] = true
		}
		for _, dep := range externalDepsByStep[step.ID] {
			if dep.Type == "parent-child" {
				continue
			}
			if dep.Type == "blocks" {
				b.Needs = append(b.Needs, dep.DependsOnID)
			} else {
				b.Needs = append(b.Needs, dep.Type+":"+dep.DependsOnID)
			}
		}
		for _, dep := range recipe.Deps {
			if dep.StepID == step.ID && dep.Type == "parent-child" {
				if parentBeadID, ok := idMapping[dep.DependsOnID]; ok {
					b.ParentID = parentBeadID
					break
				}
			}
		}
		for _, dep := range externalDepsByStep[step.ID] {
			if b.ParentID == "" && recipeParentByStep[step.ID] == "" && dep.Type == "parent-child" && dep.DependsOnID != "" {
				b.ParentID = dep.DependsOnID
				break
			}
		}

		if b.Metadata == nil {
			b.Metadata = make(map[string]string, 2)
		}
		if b.Metadata[beadmeta.StepRefMetadataKey] == "" {
			b.Metadata[beadmeta.StepRefMetadataKey] = step.ID
		}
		b.Metadata[beadmeta.RootBeadIDMetadataKey] = opts.RootID
		b.Ref = step.ID

		if logicalStepID, ok := logicalRecipeStepID(step); ok {
			if logicalBeadID, exists := idMapping[logicalStepID]; exists {
				b.Metadata[beadmeta.LogicalBeadIDMetadataKey] = logicalBeadID
			} else if logicalBeadID := existingLogicalBeadIDs[logicalStepID]; logicalBeadID != "" {
				b.Metadata[beadmeta.LogicalBeadIDMetadataKey] = logicalBeadID
			}
		}

		if b.Assignee != "" && hasFutureBlocker {
			pendingAssignees[step.ID] = b.Assignee
			b.Assignee = ""
		}

		// Same residual-var guard as Instantiate — see #618.
		if strings.Contains(b.Title, "{{") {
			if residual := formula.CheckResidualVars(b.Title); len(residual) > 0 {
				markFailed(store, createdIDs)
				return nil, fmt.Errorf("step %q: bead title contains unresolved variable(s) %s — missing or misspelled --var(s)?", step.ID, strings.Join(residual, ", "))
			}
		}
		if err := validateTimeoutMetadataVars(step.ID, b.Metadata); err != nil {
			markFailed(store, createdIDs)
			return nil, err
		}

		created, err := store.Create(b)
		if err != nil {
			markFailed(store, createdIDs)
			return nil, fmt.Errorf("creating fragment bead for step %q: %w", step.ID, err)
		}
		idMapping[step.ID] = created.ID
		createdParentByStep[step.ID] = created.ParentID
		createdIDs = append(createdIDs, created.ID)
	}

	for _, dep := range recipe.Deps {
		fromID, fromOK := idMapping[dep.StepID]
		toID, toOK := idMapping[dep.DependsOnID]
		if !fromOK || !toOK {
			continue
		}
		if embeddedDeps[dep.StepID+"|"+dep.DependsOnID+"|"+dep.Type] {
			continue
		}
		if dep.Type == "parent-child" && createdParentByStep[dep.StepID] != toID {
			parentID := toID
			if err := store.Update(fromID, beads.UpdateOpts{ParentID: &parentID}); err != nil {
				markFailed(store, createdIDs)
				return nil, fmt.Errorf("setting fragment parent for dep %s->%s: %w", dep.StepID, dep.DependsOnID, err)
			}
			createdParentByStep[dep.StepID] = toID
		}
		if err := store.DepAdd(fromID, toID, dep.Type); err != nil {
			markFailed(store, createdIDs)
			return nil, fmt.Errorf("wiring fragment dep %s->%s: %w", dep.StepID, dep.DependsOnID, err)
		}
	}
	for stepID, deps := range externalDepsByStep {
		if recipeParentByStep[stepID] != "" {
			continue
		}
		fromID, fromOK := idMapping[stepID]
		if !fromOK {
			continue
		}
		for _, dep := range deps {
			if dep.Type != "parent-child" {
				continue
			}
			if err := store.DepAdd(fromID, dep.DependsOnID, dep.Type); err != nil {
				markFailed(store, createdIDs)
				return nil, fmt.Errorf("wiring external fragment dep %s->%s: %w", stepID, dep.DependsOnID, err)
			}
		}
	}

	for stepID, assignee := range pendingAssignees {
		if assignee == "" {
			continue
		}
		beadID, ok := idMapping[stepID]
		if !ok || beadID == "" {
			continue
		}
		if err := store.Update(beadID, beads.UpdateOpts{Assignee: &assignee}); err != nil {
			markFailed(store, createdIDs)
			return nil, fmt.Errorf("assigning fragment step %q: %w", stepID, err)
		}
	}

	return &FragmentResult{
		IDMapping: idMapping,
		Created:   len(createdIDs),
	}, nil
}

func recipeParentDeps(deps []formula.RecipeDep) map[string]string {
	parents := make(map[string]string)
	for _, dep := range deps {
		if dep.Type == "parent-child" && dep.StepID != "" && dep.DependsOnID != "" && parents[dep.StepID] == "" {
			parents[dep.StepID] = dep.DependsOnID
		}
	}
	return parents
}

func groupExternalDeps(deps []ExternalDep) (map[string][]ExternalDep, error) {
	byStep := make(map[string][]ExternalDep)
	parentByStep := make(map[string]string)
	for _, dep := range deps {
		if dep.StepID == "" || dep.DependsOnID == "" {
			continue
		}
		if dep.Type == "" {
			dep.Type = "blocks"
		}
		if dep.Type == "parent-child" {
			if parentByStep[dep.StepID] != "" {
				return nil, fmt.Errorf("step %q has multiple external parent-child deps", dep.StepID)
			}
			parentByStep[dep.StepID] = dep.DependsOnID
		}
		byStep[dep.StepID] = append(byStep[dep.StepID], dep)
	}
	return byStep, nil
}

// nonRootStepBeadType returns the type to stamp on a non-root formula step
// bead. Beads typed "task" (the compiler's default — either from an explicit
// TOML `type = "task"` or an unset type) become "step" so Ready() and
// `bd ready` skip formula scaffolding (#1039). Other explicit types
// ("bug", "epic", ...) and the "gate" type produced by deferBeadRouting
// are preserved.
func nonRootStepBeadType(currentType string) string {
	if currentType == "task" {
		return "step"
	}
	return currentType
}

func preservesGraphActionTypes(recipe *formula.Recipe) bool {
	if recipe == nil || len(recipe.Steps) == 0 {
		return false
	}
	root := recipe.Steps[0]
	if root.Metadata[beadmeta.KindMetadataKey] == beadmeta.KindWorkflow {
		return true
	}
	return root.Metadata[beadmeta.AttemptMetadataKey] != "" && root.Metadata[beadmeta.StepRefMetadataKey] != ""
}

// stepToBead converts a RecipeStep to a Bead with variable substitution.
func stepToBead(step formula.RecipeStep, vars map[string]string, priorityOverride *int) beads.Bead {
	stepType := step.Type
	if stepType == "" {
		stepType = "task"
	}

	b := beads.Bead{
		Title:       formula.Substitute(step.Title, vars),
		Description: formula.Substitute(step.Description, vars),
		Type:        stepType,
		Priority:    resolveStepPriority(step, priorityOverride),
		Labels:      substituteLabels(step.Labels, vars),
		Assignee:    formula.Substitute(step.Assignee, vars),
	}

	// Merge step metadata + notes into bead metadata.
	if len(step.Metadata) > 0 || step.Notes != "" {
		b.Metadata = make(map[string]string, len(step.Metadata))
		for k, v := range step.Metadata {
			b.Metadata[k] = formula.Substitute(v, vars)
		}
		if step.Notes != "" {
			b.Metadata["notes"] = formula.Substitute(step.Notes, vars)
		}
	}

	return b
}

func preserveExecutableRootType(step formula.RecipeStep) bool {
	switch step.Metadata[beadmeta.KindMetadataKey] {
	case "workflow", "wisp":
		return true
	default:
		return false
	}
}

func validateTimeoutMetadataVars(stepID string, metadata map[string]string) error {
	for _, key := range []string{beadmeta.StepTimeoutMetadataKey, beadmeta.CheckTimeoutMetadataKey} {
		raw := metadata[key]
		if raw == "" {
			continue
		}
		if residual := formula.CheckResidualTimeoutVars(raw); len(residual) > 0 {
			return fmt.Errorf("step %q: metadata %s contains unresolved timeout variable(s) %s — missing or misspelled --var(s)?", stepID, key, strings.Join(residual, ", "))
		}
		parsed, err := time.ParseDuration(raw)
		if err != nil {
			return fmt.Errorf("step %q: metadata %s has invalid timeout %q: %w", stepID, key, raw, err)
		}
		if parsed <= 0 {
			return fmt.Errorf("step %q: metadata %s timeout must be positive, got %v", stepID, key, parsed)
		}
	}
	return nil
}

func deferBeadRouting(b *beads.Bead) {
	if !beads.IsReadyExcludedBead(*b) {
		beadType := b.Type
		if beadType == "" {
			beadType = "task"
		}
		ensureBeadMetadata(b)
		b.Metadata[DeferredTypeMetadataKey] = beadType
		b.Type = "gate"
	}
	if b.Assignee != "" {
		ensureBeadMetadata(b)
		b.Metadata[DeferredAssigneeMetadataKey] = b.Assignee
		b.Assignee = ""
	}
	deferBeadMetadataValue(b, beadmeta.RoutedToMetadataKey, DeferredRoutedToMetadataKey)
	deferBeadMetadataValue(b, beadmeta.ExecutionRoutedToMetadataKey, DeferredExecutionRoutedToMetadataKey)
}

func fenceGraphWorkflowBead(b *beads.Bead) {
	ensureBeadMetadata(b)
	b.Metadata[InstantiatingMetadataKey] = "true"
	deferBeadRouting(b)
}

func activateFencedGraphWorkflowBead(store beads.Store, id string) error {
	b, err := store.Get(id)
	if err != nil {
		return err
	}
	update := deferredRoutingActivationUpdate(b)
	if update.Assignee == nil && update.Type == nil && len(update.Metadata) == 0 {
		return nil
	}
	return store.Update(id, update)
}

func deferredRoutingActivationUpdate(b beads.Bead) beads.UpdateOpts {
	update := beads.UpdateOpts{}
	metadata := map[string]string{}
	if assignee := b.Metadata[DeferredAssigneeMetadataKey]; assignee != "" {
		if b.Assignee != assignee {
			update.Assignee = &assignee
		}
		metadata[DeferredAssigneeMetadataKey] = ""
	}
	if routedTo := b.Metadata[DeferredRoutedToMetadataKey]; routedTo != "" {
		if b.Metadata[beadmeta.RoutedToMetadataKey] != routedTo {
			metadata[beadmeta.RoutedToMetadataKey] = routedTo
		}
		metadata[DeferredRoutedToMetadataKey] = ""
	}
	if executionRoutedTo := b.Metadata[DeferredExecutionRoutedToMetadataKey]; executionRoutedTo != "" {
		if b.Metadata[beadmeta.ExecutionRoutedToMetadataKey] != executionRoutedTo {
			metadata[beadmeta.ExecutionRoutedToMetadataKey] = executionRoutedTo
		}
		metadata[DeferredExecutionRoutedToMetadataKey] = ""
	}
	if typ := b.Metadata[DeferredTypeMetadataKey]; typ != "" {
		if b.Type != typ {
			update.Type = &typ
		}
		metadata[DeferredTypeMetadataKey] = ""
	}
	if b.Metadata[InstantiatingMetadataKey] != "" {
		metadata[InstantiatingMetadataKey] = ""
	}
	if len(metadata) > 0 {
		update.Metadata = metadata
	}
	return update
}

func deferBeadMetadataValue(b *beads.Bead, sourceKey, deferredKey string) {
	if b.Metadata == nil {
		return
	}
	if value := b.Metadata[sourceKey]; value != "" {
		b.Metadata[deferredKey] = value
		delete(b.Metadata, sourceKey)
	}
}

func ensureBeadMetadata(b *beads.Bead) {
	if b.Metadata == nil {
		b.Metadata = make(map[string]string, 1)
	}
}

const formulaVarMetadataPrefix = beadmeta.FormulaVarPrefix

// stampFormulaVars records non-empty formula input vars on the root bead as
// gc.var.<name> metadata so they are discoverable from the parent alone
// without traversing sub-step descriptions.
func stampFormulaVars(vars map[string]string, b *beads.Bead) {
	if len(vars) == 0 {
		return
	}
	ensureBeadMetadata(b)
	for k, v := range vars {
		if v != "" {
			b.Metadata[formulaVarMetadataPrefix+k] = v
		}
	}
}

// substituteLabels applies variable substitution to each label.
func substituteLabels(labels []string, vars map[string]string) []string {
	if len(labels) == 0 {
		return labels
	}
	out := make([]string, len(labels))
	for i, l := range labels {
		out[i] = formula.Substitute(l, vars)
	}
	return out
}

func resolveStepPriority(step formula.RecipeStep, priorityOverride *int) *int {
	if priorityOverride != nil {
		return clonePriority(priorityOverride)
	}
	return clonePriority(step.Priority)
}

func clonePriority(v *int) *int {
	if v == nil {
		return nil
	}
	cloned := *v
	return &cloned
}

// applyVarDefaults merges formula variable defaults with caller-provided
// vars. Caller values take precedence over defaults.
func applyVarDefaults(vars map[string]string, defs map[string]*formula.VarDef) map[string]string {
	result := make(map[string]string)
	for name, def := range defs {
		if def != nil && def.Default != nil {
			result[name] = *def.Default
		}
	}
	for k, v := range vars {
		result[k] = v
	}
	return result
}

// ValidateRecipeRuntimeVars validates runtime variables for a compiled recipe.
// It checks declared formula vars, unresolved title placeholders, and avoids
// duplicating title errors for vars that were already reported as required.
func ValidateRecipeRuntimeVars(recipe *formula.Recipe, opts Options) error {
	validationVars := runtimeValidationVars(recipe, opts)
	validationErrs, missingRequired := formula.CollectVarValidationErrors(recipe.Vars, validationVars)
	titleErrs := unresolvedTitleValidationErrorsWithVars(recipe, opts, validationVars, missingRequired)
	if len(validationErrs) == 0 && len(titleErrs) == 0 {
		return nil
	}
	errs := make([]string, 0)
	errs = append(errs, validationErrs...)
	errs = append(errs, titleErrs...)
	return fmt.Errorf("variable validation failed:\n  - %s", strings.Join(errs, "\n  - "))
}

func runtimeValidationVars(recipe *formula.Recipe, opts Options) map[string]string {
	if opts.Title == "" || recipe == nil || recipe.Vars == nil {
		return opts.Vars
	}
	if _, ok := recipe.Vars["title"]; !ok {
		return opts.Vars
	}
	vars := applyVarDefaults(opts.Vars, recipe.Vars)
	result := make(map[string]string)
	for k, v := range vars {
		result[k] = v
	}
	result["title"] = formula.Substitute(opts.Title, vars)
	return result
}

func unresolvedTitleValidationErrorsWithVars(recipe *formula.Recipe, opts Options, providedVars map[string]string, missingRequired map[string]bool) []string {
	if recipe == nil || len(recipe.Steps) == 0 {
		return nil
	}
	vars := applyVarDefaults(providedVars, recipe.Vars)
	errs := make([]string, 0)
	for i, step := range recipe.Steps {
		if recipe.RootOnly && i > 0 {
			break
		}
		rawTitle := step.Title
		if step.IsRoot && opts.Title != "" {
			rawTitle = opts.Title
		}
		title := formula.Substitute(rawTitle, vars)
		if !strings.Contains(title, "{{") {
			continue
		}
		residual := formula.CheckResidualVars(title)
		unexplained := make([]string, 0, len(residual))
		for _, name := range residual {
			if missingRequired[name] {
				continue
			}
			unexplained = append(unexplained, name)
		}
		if len(unexplained) == 0 {
			continue
		}
		errs = append(errs,
			fmt.Sprintf(`step %q: bead title contains unresolved variable(s) %s — missing or misspelled --var(s)?`,
				step.ID, strings.Join(unexplained, ", ")))
	}
	return errs
}

// activateAttachCandidate promotes a fence-committed speculative sub-DAG to
// runnable: every created bead's deferred type/assignee/routing is restored
// and the root's fence-pending marker cleared (marker last, so a crash
// mid-activation leaves a pending root that recovery re-activates
// idempotently — activation updates are no-ops once applied).
func activateAttachCandidate(store beads.Store, rootID string, idMapping map[string]string) error {
	// Claim the candidate before touching any step: only the claim winner
	// (or a co-activator finishing an idempotent activation) may proceed. A
	// candidate settled as "failed" was neutralized — activating it now
	// would resurrect a sub-DAG some other processor already killed.
	if won, err := claimAttachCandidate(store, rootID, attachFencePendingUnclaimed, attachFencePendingActivating); err != nil {
		return fmt.Errorf("claiming attach candidate %s for activation: %w", rootID, err)
	} else if !won {
		b, err := store.Get(rootID)
		if err != nil {
			return fmt.Errorf("re-reading contested attach candidate %s: %w", rootID, err)
		}
		switch b.Metadata[beadmeta.AttachFencePendingMetadataKey] {
		case "":
			return nil // already fully activated
		case attachFencePendingActivating:
			// A co-activator is mid-flight; activation is idempotent, so
			// finish it here too — whoever completes last clears the marker.
		default:
			return fmt.Errorf("attach candidate %s was neutralized before activation: %w", rootID, ErrEpochConflict)
		}
	}
	ids := make([]string, 0, len(idMapping))
	for _, id := range idMapping {
		if id != "" {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	for _, id := range ids {
		if err := activateFencedGraphWorkflowBead(store, id); err != nil {
			return fmt.Errorf("activating %s: %w", id, err)
		}
	}
	if won, err := claimAttachCandidate(store, rootID, attachFencePendingActivating, ""); err != nil {
		return fmt.Errorf("clearing fence-pending marker on %s: %w", rootID, err)
	} else if !won {
		b, err := store.Get(rootID)
		if err != nil {
			return fmt.Errorf("re-reading attach candidate %s after activation: %w", rootID, err)
		}
		if b.Metadata[beadmeta.AttachFencePendingMetadataKey] != "" {
			return fmt.Errorf("attach candidate %s marker moved to %q during activation: %w",
				rootID, b.Metadata[beadmeta.AttachFencePendingMetadataKey], ErrEpochConflict)
		}
	}
	return nil
}

// markFailedReporting is markFailed with the first metadata-write error
// surfaced instead of swallowed: the fence loser path must know when its
// candidate was NOT neutralized, because an unmarked candidate could
// otherwise be selected by idempotency recovery.
func markFailedReporting(store beads.Store, ids []string) error {
	var firstErr error
	for _, id := range ids {
		if err := store.SetMetadataBatch(id, map[string]string{
			beadmeta.MoleculeFailedMetadataKey: "true",
			InstantiatingMetadataKey:           "",
		}); err != nil && firstErr == nil {
			firstErr = fmt.Errorf("marking %s molecule_failed: %w", id, err)
		}
	}
	return firstErr
}

// markFailed sets beadmeta.MoleculeFailedMetadataKey on all created beads.
// Best-effort: errors are silently ignored since we're already in an
// error path.
func markFailed(store beads.Store, ids []string) {
	for _, id := range ids {
		_ = store.SetMetadataBatch(id, map[string]string{
			beadmeta.MoleculeFailedMetadataKey: "true",
			InstantiatingMetadataKey:           "",
		})
	}
}

func logicalRecipeStepID(step formula.RecipeStep) (string, bool) {
	kind := step.Metadata[beadmeta.KindMetadataKey]
	if attempt := step.Metadata[beadmeta.AttemptMetadataKey]; attempt != "" {
		// v1 patterns: kind-specific suffix stripping.
		switch kind {
		case "run", "scope":
			if trimmed, ok := trimAttemptSuffix(step.ID, ".run."+attempt); ok {
				return trimmed, true
			}
		case "check":
			if trimmed, ok := trimAttemptSuffix(step.ID, ".check."+attempt); ok {
				return trimmed, true
			}
		case "retry-run":
			if trimmed, ok := trimAttemptSuffix(step.ID, ".run."+attempt); ok {
				return trimmed, true
			}
		case "retry-eval":
			if trimmed, ok := trimAttemptSuffix(step.ID, ".eval."+attempt); ok {
				return trimmed, true
			}
		}

		// v2 patterns: attempt/iteration suffix stripping.
		// v2 beads keep their original kind but have gc.attempt set.
		if trimmed, ok := trimAttemptSuffix(step.ID, ".attempt."+attempt); ok {
			return trimmed, true
		}
		if trimmed, ok := trimAttemptSuffix(step.ID, ".iteration."+attempt); ok {
			return trimmed, true
		}
	}
	if logicalID := step.Metadata[beadmeta.RalphStepIDMetadataKey]; logicalID != "" {
		switch kind {
		case "run", "check", "scope":
			return logicalID, true
		}
	}
	if kind != "run" && kind != "check" && kind != "scope" && kind != "retry-run" && kind != "retry-eval" {
		return "", false
	}
	for _, prefix := range []string{".run.", ".check.", ".eval."} {
		if idx := strings.LastIndex(step.ID, prefix); idx > 0 {
			return step.ID[:idx], true
		}
	}
	return "", false
}

func trimAttemptSuffix(id, suffix string) (string, bool) {
	if suffix == "" || !strings.HasSuffix(id, suffix) {
		return "", false
	}
	return strings.TrimSuffix(id, suffix), true
}

func existingLogicalBeadIDIndex(store beads.Store, rootID string) (map[string]string, error) {
	all, err := store.List(beads.ListQuery{
		Metadata: map[string]string{beadmeta.RootBeadIDMetadataKey: rootID},
		TierMode: beads.TierBoth,
	})
	if err != nil {
		return nil, err
	}
	index := make(map[string]string)
	for _, bead := range all {
		switch bead.Metadata[beadmeta.KindMetadataKey] {
		case "ralph", "retry":
		default:
			continue
		}
		if bead.ID != rootID && bead.Metadata[beadmeta.RootBeadIDMetadataKey] != rootID {
			continue
		}
		if stepRef := bead.Metadata[beadmeta.StepRefMetadataKey]; stepRef != "" {
			index[stepRef] = bead.ID
		}
		if stepID := bead.Metadata[beadmeta.StepIDMetadataKey]; stepID != "" {
			index[stepID] = bead.ID
		}
	}
	return index, nil
}
