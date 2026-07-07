package sling

import (
	"errors"
	"fmt"
	"strings"

	"github.com/gastownhall/gascity/internal/agentutil"
	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	convoycore "github.com/gastownhall/gascity/internal/convoy"
	"github.com/gastownhall/gascity/internal/molecule"
	"github.com/gastownhall/gascity/internal/sourceworkflow"
)

// BeadFromGetters tries multiple BeadQuerier implementations and returns
// the first successful result.
func BeadFromGetters(id string, getters ...BeadQuerier) (beads.Bead, bool) {
	for _, getter := range getters {
		if getter == nil {
			continue
		}
		b, err := getter.Get(id)
		if err == nil {
			return b, true
		}
	}
	return beads.Bead{}, false
}

// CollectAttachedBeads finds all molecule/workflow attachments for a parent bead.
func CollectAttachedBeads(parent beads.Bead, store beads.Store, childQuerier BeadChildQuerier) ([]beads.Bead, error) {
	var (
		attachments []beads.Bead
		firstErr    error
	)
	seen := make(map[string]struct{})

	addByID := func(id string) {
		id = strings.TrimSpace(id)
		if id == "" || store == nil {
			return
		}
		if _, ok := seen[id]; ok {
			return
		}
		attached, err := store.Get(id)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			return
		}
		seen[id] = struct{}{}
		attachments = append(attachments, attached)
	}

	addByID(parent.Metadata[beadmeta.MoleculeIDMetadataKey])
	addByID(parent.Metadata["workflow_id"])

	if childQuerier != nil {
		children, err := childQuerier.List(beads.ListQuery{
			ParentID: parent.ID,
			Sort:     beads.SortCreatedAsc,
		})
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
		} else {
			for _, child := range children {
				if !IsAttachedRoot(child) {
					continue
				}
				if _, ok := seen[child.ID]; ok {
					continue
				}
				seen[child.ID] = struct{}{}
				attachments = append(attachments, child)
			}
		}
	}

	return attachments, firstErr
}

// AttachmentLabel returns "workflow" or "molecule" based on the bead type.
func AttachmentLabel(b beads.Bead) string {
	if IsWorkflowAttachment(b) {
		return "workflow"
	}
	return "molecule"
}

// IsAttachedRoot reports whether a bead is a workflow or molecule root.
func IsAttachedRoot(b beads.Bead) bool {
	return IsWorkflowAttachment(b) || IsMoleculeAttachment(b)
}

// IsWorkflowAttachment reports whether a bead is a graph.v2 workflow attachment.
func IsWorkflowAttachment(b beads.Bead) bool {
	return strings.EqualFold(strings.TrimSpace(b.Metadata[beadmeta.KindMetadataKey]), beadmeta.KindWorkflow) ||
		strings.EqualFold(strings.TrimSpace(b.Metadata[beadmeta.FormulaContractMetadataKey]), beadmeta.FormulaContractGraphV2)
}

// IsMoleculeAttachment reports whether a bead is a molecule attachment.
func IsMoleculeAttachment(b beads.Bead) bool {
	return strings.EqualFold(strings.TrimSpace(b.Type), "molecule")
}

// FindBlockingMolecule checks if the bead has any open attached molecule
// or wisp children. Returns the blocking attachment's label and ID, or
// empty strings if none. Read-only -- does not auto-burn.
func FindBlockingMolecule(q BeadQuerier, beadID string, store beads.Store) (label, id string) {
	parent, ok := BeadFromGetters(beadID, q, store)
	if !ok {
		return "", ""
	}
	var childQuerier BeadChildQuerier
	if cq, ok := q.(BeadChildQuerier); ok {
		childQuerier = cq
	} else if cq, ok := any(store).(BeadChildQuerier); ok {
		childQuerier = cq
	}
	attachments, err := CollectAttachedBeads(parent, store, childQuerier)
	if err != nil && len(attachments) == 0 {
		return "", ""
	}
	for _, attached := range attachments {
		if attached.Status != "closed" {
			return AttachmentLabel(attached), attached.ID
		}
	}
	return "", ""
}

// HasMoleculeChildren reports whether the bead has any open attached
// molecule or wisp children. Read-only -- does not auto-burn.
func HasMoleculeChildren(q BeadQuerier, beadID string, store beads.Store) bool {
	label, _ := FindBlockingMolecule(q, beadID, store)
	return label != ""
}

// CloseAttachedSubtree closes an attached workflow or molecule root and any
// open descendants beneath it.
func CloseAttachedSubtree(store beads.Store, attached beads.Bead) (int, error) {
	if store == nil {
		return 0, fmt.Errorf("store unavailable")
	}
	if IsWorkflowAttachment(attached) {
		return sourceworkflow.CloseWorkflowSubtree(store, attached.ID)
	}
	return molecule.CloseSubtree(store, attached.ID)
}

func clearAttachmentMetadata(store beads.Store, parent beads.Bead, attached beads.Bead) error {
	if store == nil || strings.TrimSpace(parent.ID) == "" || strings.TrimSpace(attached.ID) == "" {
		return nil
	}
	if strings.TrimSpace(parent.Metadata["workflow_id"]) == attached.ID {
		if err := store.SetMetadata(parent.ID, "workflow_id", ""); err != nil {
			return err
		}
	}
	if strings.TrimSpace(parent.Metadata[beadmeta.MoleculeIDMetadataKey]) == attached.ID {
		if err := store.SetMetadata(parent.ID, beadmeta.MoleculeIDMetadataKey, ""); err != nil {
			return err
		}
	}
	return nil
}

func checkNoMoleculeChildren(q BeadQuerier, beadID string, store beads.Store, result *SlingResult, allowLiveWorkflow bool) error {
	parent, ok := BeadFromGetters(beadID, q, store)
	if !ok {
		return nil
	}
	parentUnassigned := strings.TrimSpace(parent.Assignee) == ""

	var childQuerier BeadChildQuerier
	if cq, ok := q.(BeadChildQuerier); ok {
		childQuerier = cq
	} else if cq, ok := any(store).(BeadChildQuerier); ok {
		childQuerier = cq
	}
	attachments, err := CollectAttachedBeads(parent, store, childQuerier)
	if err != nil && len(attachments) == 0 {
		return nil
	}

	for _, attached := range attachments {
		if attached.Status == "closed" {
			continue
		}
		if IsWorkflowAttachment(attached) {
			if allowLiveWorkflow {
				continue
			}
			return &sourceworkflow.ConflictError{
				SourceBeadID: beadID,
				WorkflowIDs:  []string{attached.ID},
			}
		}
		if parentUnassigned && store != nil {
			if _, burnErr := CloseAttachedSubtree(store, attached); burnErr == nil {
				if clearErr := clearAttachmentMetadata(store, parent, attached); clearErr != nil {
					return clearErr
				}
				result.AutoBurned = append(result.AutoBurned, attached.ID)
				continue
			}
		}
		return fmt.Errorf("bead %s already has attached %s %s", beadID, AttachmentLabel(attached), attached.ID)
	}
	return nil
}

// CheckNoMoleculeChildren returns an error if the bead already has an attached
// molecule or wisp child that is still open. Auto-burn messages go to result.AutoBurned.
func CheckNoMoleculeChildren(q BeadQuerier, beadID string, store beads.Store, result *SlingResult) error {
	return checkNoMoleculeChildren(q, beadID, store, result, false)
}

// CheckBatchNoMoleculeChildren checks all open children for existing molecule
// attachments before any wisps are created.
func CheckBatchNoMoleculeChildren(q BeadChildQuerier, open []beads.Bead, store beads.Store, result *SlingResult) error {
	return checkBatchNoMoleculeChildren(q, open, store, result, false)
}

// CheckNoMoleculeChildrenAllowLiveWorkflow is like CheckNoMoleculeChildren
// but permits an existing live workflow attachment (used on --force graph
// launches that will supersede the existing root under the source-workflow
// lock).
func CheckNoMoleculeChildrenAllowLiveWorkflow(q BeadQuerier, beadID string, store beads.Store, result *SlingResult) error {
	return checkNoMoleculeChildren(q, beadID, store, result, true)
}

// CheckBatchNoMoleculeChildrenAllowLiveWorkflow is the batch variant of
// CheckNoMoleculeChildrenAllowLiveWorkflow.
func CheckBatchNoMoleculeChildrenAllowLiveWorkflow(q BeadChildQuerier, open []beads.Bead, store beads.Store, result *SlingResult) error {
	return checkBatchNoMoleculeChildren(q, open, store, result, true)
}

func checkBatchNoMoleculeChildren(q BeadChildQuerier, open []beads.Bead, store beads.Store, result *SlingResult, allowLiveWorkflow bool) error {
	var problems []string
	// workflowConflicts tracks children whose already-attached root is a
	// live workflow. We emit a typed *sourceworkflow.ConflictError for
	// those so the CLI/API boundary returns exit 3 + the cleanup hint;
	// without this, users see a generic "cannot use --on" string and
	// never learn about `gc workflow delete-source`. The first child's
	// conflict becomes the typed payload; a combined non-typed error
	// keeps the summary message so "%d/%d" diagnostics stay readable.
	type workflowConflict struct {
		childID    string
		workflowID string
	}
	var workflowConflicts []workflowConflict
	for _, child := range open {
		attachments, err := CollectAttachedBeads(child, store, q)
		if err != nil && len(attachments) == 0 {
			continue
		}
		childUnassigned := strings.TrimSpace(child.Assignee) == ""
		for _, attached := range attachments {
			if attached.Status == "closed" {
				continue
			}
			if IsWorkflowAttachment(attached) {
				if allowLiveWorkflow {
					continue
				}
				problems = append(problems, fmt.Sprintf("%s (has %s %s)", child.ID, AttachmentLabel(attached), attached.ID))
				workflowConflicts = append(workflowConflicts, workflowConflict{childID: child.ID, workflowID: attached.ID})
				continue
			}
			if childUnassigned && store != nil {
				if _, burnErr := CloseAttachedSubtree(store, attached); burnErr == nil {
					if clearErr := clearAttachmentMetadata(store, child, attached); clearErr != nil {
						return clearErr
					}
					result.AutoBurned = append(result.AutoBurned, attached.ID)
					continue
				}
			}
			problems = append(problems, fmt.Sprintf("%s (has %s %s)", child.ID, AttachmentLabel(attached), attached.ID))
		}
	}
	if len(problems) == 0 {
		return nil
	}
	summary := fmt.Errorf("cannot use --on: beads already have attached molecules: %s",
		strings.Join(problems, ", "))
	if len(workflowConflicts) == 0 {
		return summary
	}
	// Emit one typed ConflictError per conflicted child so cleanup hints
	// stay correctly attributed. Collapsing into a single error keyed to
	// the first child misreports which source bead owns each blocking
	// workflow — users running the suggested `gc workflow delete-source
	// <first-child>` command would see unrelated workflow IDs and only
	// clean up part of the batch. Group blocking workflow IDs by child,
	// then join them alongside the summary; the CLI walks the
	// error chain to render one cleanup hint per affected child.
	conflictsByChild := make(map[string][]string, len(workflowConflicts))
	childOrder := make([]string, 0, len(workflowConflicts))
	for _, c := range workflowConflicts {
		if _, seen := conflictsByChild[c.childID]; !seen {
			childOrder = append(childOrder, c.childID)
		}
		conflictsByChild[c.childID] = append(conflictsByChild[c.childID], c.workflowID)
	}
	joined := make([]error, 0, len(childOrder)+1)
	for _, childID := range childOrder {
		joined = append(joined, &sourceworkflow.ConflictError{
			SourceBeadID: childID,
			WorkflowIDs:  conflictsByChild[childID],
		})
	}
	joined = append(joined, summary)
	return errors.Join(joined...)
}

// needsConvoyRecovery reports whether an already-routed bead should re-enter
// finalize to repair missing or closed auto-convoy membership.
//
// It fails CLOSED on a store error: rather than reporting "recovery needed"
// (which re-runs finalize and mints a duplicate auto-convoy under a transient
// store hiccup, #2987), it returns the error so callers can treat the convoy
// as already present. A read error is never evidence that recovery is needed.
func needsConvoyRecovery(q BeadQuerier, b beads.Bead, deps SlingDeps, opts BeadCheckOptions) (bool, error) {
	if opts.NoConvoy {
		return false, nil
	}
	live, err := hasLiveTrackingConvoy(deps.Store, b.ID)
	if err != nil {
		return false, err
	}
	if live {
		return false, nil
	}
	parentID := strings.TrimSpace(b.ParentID)
	if parentID == "" {
		return true, nil
	}
	if q == nil {
		return false, nil
	}
	parent, err := q.Get(parentID)
	if err != nil {
		if errors.Is(err, beads.ErrNotFound) {
			// A genuinely deleted parent is not a transient store hiccup: the
			// routed child is orphaned, so finalize must re-run to recreate its
			// missing auto-convoy. Return recovery-needed rather than failing
			// closed. Only ambiguous/transient store errors fail closed below
			// (assuming the convoy already exists, #2987).
			return true, nil
		}
		return false, fmt.Errorf("reading parent %s for convoy recovery of %s: %w", parentID, b.ID, err)
	}
	if parent.Type == "convoy" {
		return convoycore.IsTerminalStatus(parent.Status), nil
	}
	if sourceworkflow.IsWorkflowRoot(parent) {
		return false, nil
	}
	// Ordinary parent beads do not own the routing lifecycle. A routed child
	// without a live tracking convoy needs finalize to run again so the missing
	// auto-convoy can be recreated; finalize is idempotent for an already-routed
	// bead because CheckBeadState preserves the routed metadata and only repairs
	// the missing tracking attachment.
	return true, nil
}

func hasLiveTrackingConvoy(store beads.Store, itemID string) (bool, error) {
	if store == nil {
		return false, nil
	}
	convoys, err := convoycore.TrackingConvoysForItem(store, itemID)
	if err != nil {
		return false, fmt.Errorf("listing tracking convoys for %s: %w", itemID, err)
	}
	for _, convoy := range convoys {
		// These are convoys by construction, so the convoy type's Ready
		// exclusion (#3591) does not apply here — only skip convoys excluded
		// by infrastructure label (session/order-tracking bookkeeping).
		if beads.HasReadyExcludedLabel(convoy) {
			continue
		}
		if !convoycore.IsTerminalStatus(convoy.Status) {
			return true, nil
		}
	}
	return false, nil
}

// resolveConvoyRecovery maps needsConvoyRecovery onto a BeadCheckResult for an
// already-routed bead: an empty result when finalize must re-run to recreate a
// missing auto-convoy, or Idempotent otherwise. On a store error it fails
// CLOSED — assuming the convoy already exists rather than minting a duplicate
// (#2987) — and surfaces the error as a warning instead of swallowing it.
func resolveConvoyRecovery(q BeadQuerier, b beads.Bead, deps SlingDeps, opts BeadCheckOptions, beadID string) BeadCheckResult {
	needRecovery, err := needsConvoyRecovery(q, b, deps, opts)
	if err != nil {
		return BeadCheckResult{
			Idempotent: true,
			Warnings:   []string{fmt.Sprintf("warning: bead %s convoy-recovery check failed, assuming convoy exists: %v", beadID, err)},
		}
	}
	if needRecovery {
		// Prior sling set gc.routed_to but left no convoy — let finalize
		// re-run to create it and poke the controller.
		return BeadCheckResult{}
	}
	return BeadCheckResult{Idempotent: true}
}

// CheckBeadState checks whether a bead is already routed and returns a
// structured result. Best-effort: nil querier or query failure → empty result.
func CheckBeadState(q BeadQuerier, beadID string, a config.Agent, deps SlingDeps) BeadCheckResult {
	return CheckBeadStateWithOptions(q, beadID, a, deps, BeadCheckOptions{})
}

// CheckBeadStateWithOptions checks whether a bead is already routed for the
// requested route options and returns a structured result.
func CheckBeadStateWithOptions(q BeadQuerier, beadID string, a config.Agent, deps SlingDeps, opts BeadCheckOptions) BeadCheckResult {
	if q == nil {
		return BeadCheckResult{}
	}
	b, err := q.Get(beadID)
	if err != nil {
		return BeadCheckResult{}
	}

	if IsCustomSlingQuery(a) {
		return BeadCheckResult{Warnings: routedStateWarnings(b, beadID)}
	}

	target := a.QualifiedName()
	if strings.TrimSpace(b.Metadata[beadmeta.RoutedToMetadataKey]) == target {
		if b.Assignee == "" || b.Assignee == target {
			return resolveConvoyRecovery(q, b, deps, opts, beadID)
		}
		return BeadCheckResult{
			Warnings: []string{fmt.Sprintf("warning: bead %s routed to %q but assigned to %q", beadID, target, b.Assignee)},
		}
	}

	isMulti := agentutil.IsMultiSessionAgent(&a)
	if !isMulti {
		if b.Assignee == target {
			return resolveConvoyRecovery(q, b, deps, opts, beadID)
		}
		return BeadCheckResult{Warnings: routedStateWarnings(b, beadID)}
	}

	if strings.TrimSpace(b.Metadata[beadmeta.RoutedToMetadataKey]) == "" {
		poolLabel := "pool:" + target
		for _, l := range b.Labels {
			if l == poolLabel {
				return resolveConvoyRecovery(q, b, deps, opts, beadID)
			}
		}
	}
	return BeadCheckResult{Warnings: routedStateWarnings(b, beadID)}
}

// routedStateWarnings reports human-readable warnings describing any existing
// routing state on b (assignee, gc.routed_to metadata, and pool: labels) that
// would collide with a fresh sling of beadID. It returns nil when b carries no
// such state.
func routedStateWarnings(b beads.Bead, beadID string) []string {
	var warnings []string
	if b.Assignee != "" {
		warnings = append(warnings, fmt.Sprintf("warning: bead %s already assigned to %q", beadID, b.Assignee))
	}
	if routedTo := strings.TrimSpace(b.Metadata[beadmeta.RoutedToMetadataKey]); routedTo != "" {
		warnings = append(warnings, fmt.Sprintf("warning: bead %s already routed to %q", beadID, routedTo))
	}
	for _, l := range b.Labels {
		if strings.HasPrefix(l, "pool:") {
			warnings = append(warnings, fmt.Sprintf("warning: bead %s already has pool label %q", beadID, l))
		}
	}
	return warnings
}
