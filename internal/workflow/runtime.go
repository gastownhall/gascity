package workflow

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/formula"
)

// ControlResult reports whether a control bead was processed and what it did.
type ControlResult struct {
	Processed bool
	Action    string
	Created   int
	Skipped   int
}

// ProcessOptions provides workflow-control execution context.
type ProcessOptions struct {
	CityPath           string
	FormulaSearchPaths []string
	PrepareFragment    func(*formula.FragmentRecipe, beads.Bead) error
}

var errFinalizePending = errors.New("workflow finalize pending")

// ProcessControl executes a graph.v2 control bead.
//
// The current graph.v2 runtime assumes a single controller processes a given
// workflow root at a time. The gc.* spawning/spawned state machines provide
// crash-recovery and idempotent resume, but they are not a compare-and-swap
// guard for concurrent controllers executing the same control bead.
func ProcessControl(store beads.Store, bead beads.Bead, opts ProcessOptions) (ControlResult, error) {
	if store == nil {
		return ControlResult{}, fmt.Errorf("store is nil")
	}
	if bead.Status != "open" {
		return ControlResult{}, nil
	}

	switch bead.Metadata["gc.kind"] {
	case "check":
		return processRalphCheck(store, bead, opts)
	case "fanout":
		return processFanout(store, bead, opts)
	case "scope-check":
		return processScopeCheck(store, bead)
	case "workflow-finalize":
		return processWorkflowFinalize(store, bead)
	default:
		return ControlResult{}, fmt.Errorf("%s: unsupported control bead kind %q", bead.ID, bead.Metadata["gc.kind"])
	}
}

func processScopeCheck(store beads.Store, bead beads.Bead) (ControlResult, error) {
	subjectID, err := resolveBlockingSubjectID(store, bead.ID)
	if err != nil {
		return ControlResult{}, fmt.Errorf("%s: resolving subject: %w", bead.ID, err)
	}
	subject, err := store.Get(subjectID)
	if err != nil {
		return ControlResult{}, fmt.Errorf("%s: loading subject %s: %w", bead.ID, subjectID, err)
	}

	rootID := bead.Metadata["gc.root_bead_id"]
	if rootID == "" {
		return ControlResult{}, fmt.Errorf("%s: missing gc.root_bead_id", bead.ID)
	}
	scopeRef := bead.Metadata["gc.scope_ref"]
	if scopeRef == "" {
		return ControlResult{}, fmt.Errorf("%s: missing gc.scope_ref", bead.ID)
	}
	body, err := resolveScopeBody(store, rootID, scopeRef)
	if err != nil {
		return ControlResult{}, fmt.Errorf("%s: loading scope body for %s: %w", bead.ID, scopeRef, err)
	}

	if subject.Metadata["gc.outcome"] == "fail" {
		skipped, err := skipOpenScopeMembers(store, rootID, scopeRef, bead.ID)
		if err != nil {
			return ControlResult{}, fmt.Errorf("%s: aborting scope: %w", bead.ID, err)
		}
		if err := setOutcomeAndClose(store, bead.ID, "pass"); err != nil {
			return ControlResult{}, fmt.Errorf("%s: completing control bead: %w", bead.ID, err)
		}
		if body.Status != "closed" {
			if err := setOutcomeAndClose(store, body.ID, "fail"); err != nil {
				return ControlResult{}, fmt.Errorf("%s: completing scope body: %w", body.ID, err)
			}
		}
		return ControlResult{Processed: true, Action: "scope-fail", Skipped: skipped}, nil
	}

	if err := setOutcomeAndClose(store, bead.ID, "pass"); err != nil {
		return ControlResult{}, fmt.Errorf("%s: completing control bead: %w", bead.ID, err)
	}

	remainingOpen, err := hasOpenScopeMembers(store, rootID, scopeRef)
	if err != nil {
		return ControlResult{}, fmt.Errorf("%s: checking scope completion: %w", bead.ID, err)
	}
	if !remainingOpen {
		outputJSON, err := resolveScopeOutputJSON(store, rootID, scopeRef, subject)
		if err != nil {
			return ControlResult{}, fmt.Errorf("%s: resolving scope output: %w", bead.ID, err)
		}
		if outputJSON != "" {
			if err := store.SetMetadata(body.ID, "gc.output_json", outputJSON); err != nil {
				return ControlResult{}, fmt.Errorf("%s: propagating scope output: %w", body.ID, err)
			}
		}
		bodyAfter, getErr := store.Get(body.ID)
		if getErr != nil {
			return ControlResult{}, fmt.Errorf("%s: reloading scope body: %w", body.ID, getErr)
		}
		if bodyAfter.Status != "closed" {
			if err := setOutcomeAndClose(store, body.ID, "pass"); err != nil {
				return ControlResult{}, fmt.Errorf("%s: completing scope body: %w", body.ID, err)
			}
		}
		return ControlResult{Processed: true, Action: "scope-pass"}, nil
	}

	return ControlResult{Processed: true, Action: "continue"}, nil
}

func processWorkflowFinalize(store beads.Store, bead beads.Bead) (ControlResult, error) {
	rootID := bead.Metadata["gc.root_bead_id"]
	if rootID == "" {
		return ControlResult{}, fmt.Errorf("%s: missing gc.root_bead_id", bead.ID)
	}

	outcome, err := resolveFinalizeOutcome(store, bead.ID)
	if err != nil {
		if errors.Is(err, errFinalizePending) {
			return ControlResult{}, nil
		}
		return ControlResult{}, fmt.Errorf("%s: resolving workflow outcome: %w", bead.ID, err)
	}

	if err := setOutcomeAndClose(store, bead.ID, "pass"); err != nil {
		return ControlResult{}, fmt.Errorf("%s: completing workflow finalizer: %w", bead.ID, err)
	}
	if err := setOutcomeAndClose(store, rootID, outcome); err != nil {
		return ControlResult{}, fmt.Errorf("%s: completing workflow head: %w", rootID, err)
	}
	return ControlResult{Processed: true, Action: "workflow-" + outcome}, nil
}

func reconcileTerminalScopedMember(store beads.Store, bead beads.Bead) (ControlResult, error) {
	scopeRef := bead.Metadata["gc.scope_ref"]
	if scopeRef == "" {
		return ControlResult{}, nil
	}
	rootID := bead.Metadata["gc.root_bead_id"]
	if rootID == "" {
		return ControlResult{}, fmt.Errorf("%s: missing gc.root_bead_id", bead.ID)
	}
	body, err := resolveScopeBody(store, rootID, scopeRef)
	if err != nil {
		return ControlResult{}, fmt.Errorf("%s: loading scope body for %s: %w", bead.ID, scopeRef, err)
	}

	if bead.Metadata["gc.outcome"] == "fail" {
		skipped, err := skipOpenScopeMembers(store, rootID, scopeRef, bead.ID)
		if err != nil {
			return ControlResult{}, fmt.Errorf("%s: aborting scope: %w", bead.ID, err)
		}
		if body.Status != "closed" {
			if err := setOutcomeAndClose(store, body.ID, "fail"); err != nil {
				return ControlResult{}, fmt.Errorf("%s: completing scope body: %w", body.ID, err)
			}
		}
		return ControlResult{Processed: true, Action: "scope-fail", Skipped: skipped}, nil
	}

	remainingOpen, err := hasOpenScopeMembers(store, rootID, scopeRef)
	if err != nil {
		return ControlResult{}, fmt.Errorf("%s: checking scope completion: %w", bead.ID, err)
	}
	if remainingOpen {
		return ControlResult{}, nil
	}

	bodyAfter, err := store.Get(body.ID)
	if err != nil {
		return ControlResult{}, fmt.Errorf("%s: reloading scope body: %w", body.ID, err)
	}
	if bodyAfter.Status == "closed" {
		return ControlResult{}, nil
	}
	outputJSON, err := resolveScopeOutputJSON(store, rootID, scopeRef, bead)
	if err != nil {
		return ControlResult{}, fmt.Errorf("%s: resolving scope output: %w", bead.ID, err)
	}
	if outputJSON != "" {
		if err := store.SetMetadata(body.ID, "gc.output_json", outputJSON); err != nil {
			return ControlResult{}, fmt.Errorf("%s: propagating scope output: %w", body.ID, err)
		}
	}
	if err := setOutcomeAndClose(store, body.ID, "pass"); err != nil {
		return ControlResult{}, fmt.Errorf("%s: completing scope body: %w", body.ID, err)
	}
	return ControlResult{Processed: true, Action: "scope-pass"}, nil
}

func resolveBlockingSubjectID(store beads.Store, beadID string) (string, error) {
	deps, err := store.DepList(beadID, "down")
	if err != nil {
		return "", err
	}
	for _, dep := range deps {
		if dep.Type == "blocks" {
			return dep.DependsOnID, nil
		}
	}
	return "", fmt.Errorf("no blocking dependency")
}

func resolveScopeBody(store beads.Store, rootID, scopeRef string) (beads.Bead, error) {
	all, err := listByWorkflowRoot(store, rootID)
	if err != nil {
		return beads.Bead{}, err
	}
	if bead, ok := findScopeBody(all, rootID, scopeRef); ok {
		return bead, nil
	}
	return beads.Bead{}, fmt.Errorf("scope %q not found under root %s", scopeRef, rootID)
}

func skipOpenScopeMembers(store beads.Store, rootID, scopeRef, skipControlID string) (int, error) {
	scopeBeads, err := listScopeMembers(store, rootID, scopeRef)
	if err != nil {
		return 0, err
	}

	pending := make(map[string]beads.Bead)
	for _, member := range scopeBeads {
		if member.ID == skipControlID || member.Status != "open" {
			continue
		}
		switch member.Metadata["gc.scope_role"] {
		case "body", "teardown":
			continue
		}
		pending[member.ID] = member
	}
	for id := range pending {
		if err := detachPendingFromSkipControl(store, id, skipControlID); err != nil {
			return 0, err
		}
	}

	skipped := 0
	for len(pending) > 0 {
		progress := false
		for _, id := range sortedPendingIDs(pending) {
			if !canSkipScopeMember(store, id, pending) {
				continue
			}
			if err := store.SetMetadataBatch(id, map[string]string{"gc.outcome": "skipped"}); err != nil {
				return skipped, err
			}
			if err := store.Close(id); err != nil {
				return skipped, fmt.Errorf("closing bead %q: %w", id, err)
			}
			delete(pending, id)
			skipped++
			progress = true
		}
		if progress {
			continue
		}
		return skipped, fmt.Errorf("unable to skip remaining scope members: %v", sortedPendingIDs(pending))
	}

	return skipped, nil
}

func detachPendingFromSkipControl(store beads.Store, beadID, skipControlID string) error {
	deps, err := store.DepList(beadID, "down")
	if err != nil {
		return err
	}
	for _, dep := range deps {
		if dep.Type != "blocks" || dep.DependsOnID != skipControlID {
			continue
		}
		if err := store.DepRemove(beadID, skipControlID); err != nil {
			return fmt.Errorf("removing blocker %s -> %s: %w", beadID, skipControlID, err)
		}
	}
	return nil
}

func canSkipScopeMember(store beads.Store, beadID string, pending map[string]beads.Bead) bool {
	deps, err := store.DepList(beadID, "down")
	if err != nil {
		return false
	}
	for _, dep := range deps {
		if dep.Type != "blocks" {
			continue
		}
		if _, blocked := pending[dep.DependsOnID]; blocked {
			return false
		}
	}
	return true
}

func sortedPendingIDs(pending map[string]beads.Bead) []string {
	ids := make([]string, 0, len(pending))
	for id := range pending {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

func hasOpenScopeMembers(store beads.Store, rootID, scopeRef string) (bool, error) {
	scopeBeads, err := listScopeMembers(store, rootID, scopeRef)
	if err != nil {
		return false, err
	}
	for _, member := range scopeBeads {
		if member.Status != "open" {
			continue
		}
		switch member.Metadata["gc.scope_role"] {
		case "body", "teardown":
			continue
		default:
			return true, nil
		}
	}
	return false, nil
}

func listScopeMembers(store beads.Store, rootID, scopeRef string) ([]beads.Bead, error) {
	all, err := listByWorkflowRoot(store, rootID)
	if err != nil {
		return nil, err
	}
	result := make([]beads.Bead, 0)
	for _, bead := range all {
		if bead.Metadata["gc.root_bead_id"] != rootID {
			continue
		}
		if bead.Metadata["gc.scope_ref"] != scopeRef {
			continue
		}
		result = append(result, bead)
	}
	return result, nil
}

func listByWorkflowRoot(store beads.Store, rootID string) ([]beads.Bead, error) {
	all, err := store.List()
	if err != nil {
		return nil, err
	}
	result := make([]beads.Bead, 0, len(all))
	for _, bead := range all {
		if bead.ID == rootID || bead.Metadata["gc.root_bead_id"] == rootID {
			result = append(result, bead)
		}
	}
	return result, nil
}

func findScopeBody(all []beads.Bead, rootID, scopeRef string) (beads.Bead, bool) {
	for _, bead := range all {
		if bead.Metadata["gc.root_bead_id"] != rootID {
			continue
		}
		if bead.Metadata["gc.kind"] != "scope" {
			continue
		}
		if matchesScopeRef(bead, scopeRef) {
			return bead, true
		}
	}
	return beads.Bead{}, false
}

func setOutcomeAndClose(store beads.Store, beadID, outcome string) error {
	if err := store.SetMetadataBatch(beadID, map[string]string{"gc.outcome": outcome}); err != nil {
		return err
	}
	status := "closed"
	return store.Update(beadID, beads.UpdateOpts{Status: &status})
}

func matchesScopeRef(bead beads.Bead, scopeRef string) bool {
	if scopeRef == "" {
		return false
	}
	if bead.Metadata["gc.scope_ref"] == scopeRef {
		return true
	}
	stepRef := bead.Metadata["gc.step_ref"]
	return stepRef == scopeRef || strings.HasSuffix(stepRef, "."+scopeRef)
}

func resolveFinalizeOutcome(store beads.Store, beadID string) (string, error) {
	deps, err := store.DepList(beadID, "down")
	if err != nil {
		return "", err
	}
	outcome := "pass"
	for _, dep := range deps {
		if dep.Type != "blocks" {
			continue
		}
		blocker, err := store.Get(dep.DependsOnID)
		if err != nil {
			return "", err
		}
		if blocker.Status != "closed" {
			return "", fmt.Errorf("%w: blocker %s is still open", errFinalizePending, blocker.ID)
		}
		if blocker.Metadata["gc.outcome"] == "fail" {
			outcome = "fail"
		}
	}
	return outcome, nil
}

func resolveScopeOutputJSON(store beads.Store, rootID, scopeRef string, subject beads.Bead) (string, error) {
	if outputJSON := subject.Metadata["gc.output_json"]; outputJSON != "" {
		return outputJSON, nil
	}

	scopeBeads, err := listScopeMembers(store, rootID, scopeRef)
	if err != nil {
		return "", err
	}

	var candidate string
	for _, bead := range scopeBeads {
		if bead.Metadata["gc.output_json"] == "" {
			continue
		}
		switch bead.Metadata["gc.scope_role"] {
		case "body", "teardown", "control":
			continue
		}
		if candidate == "" {
			candidate = bead.Metadata["gc.output_json"]
			continue
		}
		if candidate != bead.Metadata["gc.output_json"] {
			return "", nil
		}
	}
	return candidate, nil
}
