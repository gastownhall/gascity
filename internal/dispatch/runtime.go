package dispatch

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

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

// ProcessOptions provides control-dispatcher execution context.
type ProcessOptions struct {
	CityPath           string
	StorePath          string
	FormulaSearchPaths []string
	PrepareFragment    func(*formula.FragmentRecipe, beads.Bead) error
	RecycleSession     func(beads.Bead) error
	Tracef             func(format string, args ...any)
}

var (
	errFinalizePending  = errors.New("workflow finalize pending")
	errScopeBodyMissing = errors.New("scope body missing")
)

// ErrControlPending reports that a control bead is not yet processable but
// should be retried later.
var ErrControlPending = errors.New("workflow control pending")

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
		// A control bead that is not open — typically stuck at in_progress
		// after a rogue `bd update --status in_progress` from a worker —
		// can silently strand an entire workflow because the serve loop
		// treats the no-op return as a successful processed cycle. Emit a
		// specific trace line so the skip is visible in the dispatcher
		// trace log instead of looking identical to a processed cycle.
		// See bug investigation on workflow ga-ttn5z where 20+ minutes of
		// processing cycles silently no-op'd because ga-fw2fm had been
		// moved to in_progress by its implement-change worker.
		opts.tracef("process-control bead=%s kind=%s skip reason=bead_not_open status=%s",
			bead.ID, bead.Metadata["gc.kind"], bead.Status)
		return ControlResult{}, nil
	}

	switch bead.Metadata["gc.kind"] {
	case "retry":
		return processRetryControl(store, bead, opts)
	case "ralph":
		return processRalphControl(store, bead, opts)
	case "check":
		return processRalphCheck(store, bead, opts)
	case "retry-eval":
		return processRetryEval(store, bead, opts)
	case "fanout":
		return processFanout(store, bead, opts)
	case "scope-check":
		return processScopeCheck(store, bead, opts)
	case "workflow-finalize":
		return processWorkflowFinalize(store, bead)
	default:
		return ControlResult{}, fmt.Errorf("%s: unsupported control bead kind %q", bead.ID, bead.Metadata["gc.kind"])
	}
}

func (opts ProcessOptions) tracef(format string, args ...any) {
	if opts.Tracef == nil {
		return
	}
	opts.Tracef(format, args...)
}

func tracePhase[T any](opts ProcessOptions, beadID, phase string, fn func() (T, error)) (T, error) {
	var zero T
	start := time.Now()
	opts.tracef("scope-check bead=%s phase=%s start", beadID, phase)
	result, err := fn()
	if err != nil {
		opts.tracef("scope-check bead=%s phase=%s err=%v dur=%s", beadID, phase, err, time.Since(start))
		return zero, err
	}
	opts.tracef("scope-check bead=%s phase=%s ok dur=%s", beadID, phase, time.Since(start))
	return result, nil
}

func tracePhaseErr(opts ProcessOptions, beadID, phase string, fn func() error) error {
	_, err := tracePhase(opts, beadID, phase, func() (struct{}, error) {
		return struct{}{}, fn()
	})
	return err
}

func processScopeCheck(store beads.Store, bead beads.Bead, opts ProcessOptions) (ControlResult, error) {
	rootID := bead.Metadata["gc.root_bead_id"]
	scopeRef := bead.Metadata["gc.scope_ref"]
	opts.tracef("scope-check bead=%s begin root=%s scope=%s", bead.ID, rootID, scopeRef)

	subjectID, err := tracePhase(opts, bead.ID, "resolve-subject-id", func() (string, error) {
		return resolveBlockingSubjectID(store, bead.ID)
	})
	if err != nil {
		return ControlResult{}, fmt.Errorf("%s: resolving subject: %w", bead.ID, err)
	}
	subject, err := tracePhase(opts, bead.ID, "load-subject", func() (beads.Bead, error) {
		return store.Get(subjectID)
	})
	if err != nil {
		return ControlResult{}, fmt.Errorf("%s: loading subject %s: %w", bead.ID, subjectID, err)
	}
	if rootID == "" {
		return ControlResult{}, fmt.Errorf("%s: missing gc.root_bead_id", bead.ID)
	}
	if scopeRef == "" {
		return ControlResult{}, fmt.Errorf("%s: missing gc.scope_ref", bead.ID)
	}

	snapshot, err := tracePhase(opts, bead.ID, "load-snapshot", func() (scopeSnapshot, error) {
		return loadScopeSnapshot(store, rootID, scopeRef)
	})
	if err != nil {
		if errors.Is(err, errScopeBodyMissing) {
			return ControlResult{}, ErrControlPending
		}
		return ControlResult{}, fmt.Errorf("%s: loading scope snapshot for %s: %w", bead.ID, scopeRef, err)
	}
	opts.tracef("scope-check bead=%s snapshot root=%s scope=%s all=%d members=%d body=%s subject=%s outcome=%s",
		bead.ID, rootID, scopeRef, len(snapshot.all), len(snapshot.members), snapshot.body.ID, subject.ID, subject.Metadata["gc.outcome"])
	body := snapshot.body

	if isRetryAttemptSubject(subject) {
		if err := tracePhaseErr(opts, bead.ID, "close-control", func() error {
			return setOutcomeAndClose(store, bead.ID, "pass")
		}); err != nil {
			return ControlResult{}, fmt.Errorf("%s: completing retry-attempt control bead: %w", bead.ID, err)
		}
		remainingOpen := snapshot.hasOpenScopeMembers(bead.ID)
		opts.tracef("scope-check bead=%s phase=check-remaining-open remaining_open=%t ignore=%s", bead.ID, remainingOpen, bead.ID)
		if !remainingOpen {
			outputJSON, err := tracePhase(opts, bead.ID, "resolve-output", func() (string, error) {
				return snapshot.resolveScopeOutputJSON(subject)
			})
			if err != nil {
				return ControlResult{}, fmt.Errorf("%s: resolving scope output: %w", bead.ID, err)
			}
			if outputJSON != "" {
				if err := tracePhaseErr(opts, bead.ID, "write-output", func() error {
					return store.SetMetadata(body.ID, "gc.output_json", outputJSON)
				}); err != nil {
					return ControlResult{}, fmt.Errorf("%s: propagating scope output: %w", body.ID, err)
				}
			}
			bodyAfter, getErr := tracePhase(opts, bead.ID, "reload-body", func() (beads.Bead, error) {
				return store.Get(body.ID)
			})
			if getErr != nil {
				return ControlResult{}, fmt.Errorf("%s: reloading scope body: %w", body.ID, getErr)
			}
			if bodyAfter.Status != "closed" {
				if err := tracePhaseErr(opts, bead.ID, "close-body", func() error {
					return setOutcomeAndClose(store, body.ID, "pass")
				}); err != nil {
					return ControlResult{}, fmt.Errorf("%s: completing scope body: %w", body.ID, err)
				}
			}
			return ControlResult{Processed: true, Action: "scope-pass"}, nil
		}
		return ControlResult{Processed: true, Action: "continue"}, nil
	}

	// Subject must be closed before scope-check can pass. If the subject
	// is still open (e.g., a retry control waiting for its attempt), the
	// scope-check is pending. This prevents passing when the attempt bead
	// is missing or hasn't completed yet.
	if subject.Status != "closed" {
		opts.tracef("scope-check bead=%s subject=%s pending status=%s", bead.ID, subject.ID, subject.Status)
		return ControlResult{}, ErrControlPending
	}

	if subject.Metadata["gc.outcome"] == "fail" {
		skipped, err := tracePhase(opts, bead.ID, "skip-open-members", func() (int, error) {
			return snapshot.skipOpenScopeMembers(store, bead.ID)
		})
		if err != nil {
			return ControlResult{}, fmt.Errorf("%s: aborting scope: %w", bead.ID, err)
		}
		if err := tracePhaseErr(opts, bead.ID, "close-control", func() error {
			return setOutcomeAndClose(store, bead.ID, "pass")
		}); err != nil {
			return ControlResult{}, fmt.Errorf("%s: completing control bead: %w", bead.ID, err)
		}
		if body.Status != "closed" {
			if err := tracePhaseErr(opts, bead.ID, "close-body-fail", func() error {
				return setOutcomeAndClose(store, body.ID, "fail")
			}); err != nil {
				return ControlResult{}, fmt.Errorf("%s: completing scope body: %w", body.ID, err)
			}
		}
		return ControlResult{Processed: true, Action: "scope-fail", Skipped: skipped}, nil
	}

	if err := tracePhaseErr(opts, bead.ID, "close-control", func() error {
		return setOutcomeAndClose(store, bead.ID, "pass")
	}); err != nil {
		return ControlResult{}, fmt.Errorf("%s: completing control bead: %w", bead.ID, err)
	}

	remainingOpen := snapshot.hasOpenScopeMembers(bead.ID)
	opts.tracef("scope-check bead=%s phase=check-remaining-open remaining_open=%t ignore=%s", bead.ID, remainingOpen, bead.ID)
	if !remainingOpen {
		// Propagate non-gc metadata from scope members to the scope body.
		// This enables compositional metadata bubbling: attempt → retry →
		// scope → ralph → parent scope, etc.
		if err := tracePhaseErr(opts, bead.ID, "propagate-metadata", func() error {
			return snapshot.propagateScopeMemberMetadata(store, body.ID)
		}); err != nil {
			return ControlResult{}, fmt.Errorf("%s: propagating scope metadata: %w", bead.ID, err)
		}
		outputJSON, err := tracePhase(opts, bead.ID, "resolve-output", func() (string, error) {
			return snapshot.resolveScopeOutputJSON(subject)
		})
		if err != nil {
			return ControlResult{}, fmt.Errorf("%s: resolving scope output: %w", bead.ID, err)
		}
		if outputJSON != "" {
			if err := tracePhaseErr(opts, bead.ID, "write-output", func() error {
				return store.SetMetadata(body.ID, "gc.output_json", outputJSON)
			}); err != nil {
				return ControlResult{}, fmt.Errorf("%s: propagating scope output: %w", body.ID, err)
			}
		}
		bodyAfter, getErr := tracePhase(opts, bead.ID, "reload-body", func() (beads.Bead, error) {
			return store.Get(body.ID)
		})
		if getErr != nil {
			return ControlResult{}, fmt.Errorf("%s: reloading scope body: %w", body.ID, getErr)
		}
		if bodyAfter.Status != "closed" {
			if err := tracePhaseErr(opts, bead.ID, "close-body", func() error {
				return setOutcomeAndClose(store, body.ID, "pass")
			}); err != nil {
				return ControlResult{}, fmt.Errorf("%s: completing scope body: %w", body.ID, err)
			}
		}
		return ControlResult{Processed: true, Action: "scope-pass"}, nil
	}

	return ControlResult{Processed: true, Action: "continue"}, nil
}

type scopeSnapshot struct {
	rootID   string
	scopeRef string
	all      []beads.Bead
	members  []beads.Bead
	body     beads.Bead
}

func loadScopeSnapshot(store beads.Store, rootID, scopeRef string) (scopeSnapshot, error) {
	all, err := listByWorkflowRoot(store, rootID)
	if err != nil {
		return scopeSnapshot{}, err
	}
	snapshot := scopeSnapshot{
		rootID:   rootID,
		scopeRef: scopeRef,
		all:      all,
	}
	bodyFound := false
	for _, bead := range all {
		if bead.Metadata["gc.root_bead_id"] != rootID {
			continue
		}
		if bead.Metadata["gc.scope_ref"] == scopeRef {
			snapshot.members = append(snapshot.members, bead)
		}
		if !bodyFound && bead.Metadata["gc.kind"] == "scope" && matchesScopeRef(bead, scopeRef) {
			snapshot.body = bead
			bodyFound = true
		}
	}
	if !bodyFound {
		return scopeSnapshot{}, fmt.Errorf("%w: scope %q not found under root %s", errScopeBodyMissing, scopeRef, rootID)
	}
	return snapshot, nil
}

func (s scopeSnapshot) hasOpenScopeMembers(ignoreIDs ...string) bool {
	if len(s.members) == 0 {
		return false
	}
	ignored := make(map[string]struct{}, len(ignoreIDs))
	for _, id := range ignoreIDs {
		if id == "" {
			continue
		}
		ignored[id] = struct{}{}
	}
	for _, member := range s.members {
		if member.Status != "open" {
			continue
		}
		if _, skip := ignored[member.ID]; skip {
			continue
		}
		switch member.Metadata["gc.scope_role"] {
		case "body", "teardown":
			continue
		default:
			return true
		}
	}
	return false
}

func (s scopeSnapshot) propagateScopeMemberMetadata(store beads.Store, bodyID string) error {
	batch := map[string]string{}
	for _, member := range s.members {
		if member.Status != "closed" {
			continue
		}
		switch member.Metadata["gc.scope_role"] {
		case "body", "teardown", "control":
			continue
		}
		for key, value := range member.Metadata {
			if key == "" || strings.HasPrefix(key, "gc.") {
				continue
			}
			batch[key] = value
		}
	}
	if len(batch) == 0 {
		return nil
	}
	return store.SetMetadataBatch(bodyID, batch)
}

func (s scopeSnapshot) resolveScopeOutputJSON(subject beads.Bead) (string, error) {
	if outputJSON := subject.Metadata["gc.output_json"]; outputJSON != "" {
		return outputJSON, nil
	}

	var candidate string
	for _, bead := range s.members {
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

func (s scopeSnapshot) skipOpenScopeMembers(store beads.Store, skipControlID string) (int, error) {
	pending := make(map[string]beads.Bead)
	for _, member := range s.members {
		if member.ID == skipControlID || member.Status != "open" {
			continue
		}
		switch member.Metadata["gc.scope_role"] {
		case "body", "teardown":
			continue
		}
		pending[member.ID] = member
	}
	for _, member := range s.members {
		switch strings.TrimSpace(member.Metadata["gc.kind"]) {
		case "retry", "ralph":
		default:
			continue
		}
		switch member.Metadata["gc.scope_role"] {
		case "body", "teardown":
			continue
		}
		for _, candidate := range s.all {
			if candidate.Status != "open" {
				continue
			}
			if !isLogicalDescendant(member, candidate) {
				continue
			}
			pending[candidate.ID] = candidate
		}
	}

	skipped := 0
	for len(pending) > 0 {
		progress := false
		for _, id := range sortedPendingIDs(pending) {
			if !canSkipScopeMember(store, id, pending) {
				continue
			}
			status := "closed"
			if err := store.Update(id, beads.UpdateOpts{
				Status:   &status,
				Metadata: map[string]string{"gc.outcome": "skipped"},
			}); err != nil {
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

// propagateScopeMemberMetadata merges non-gc metadata from all closed scope
// members onto the scope body. Later members overwrite earlier ones for the
// same key, so the final state reflects the last step's output.
func propagateScopeMemberMetadata(store beads.Store, rootID, scopeRef, bodyID string) error {
	snapshot, err := loadScopeSnapshot(store, rootID, scopeRef)
	if err != nil {
		return err
	}
	return snapshot.propagateScopeMemberMetadata(store, bodyID)
}

func isRetryAttemptSubject(subject beads.Bead) bool {
	if subject.Metadata["gc.logical_bead_id"] == "" {
		return false
	}
	// v1 pattern: attempt beads have gc.kind "retry-run" or "retry-eval".
	switch subject.Metadata["gc.kind"] {
	case "retry-run", "retry-eval":
		return true
	}
	// v2 pattern: attempt beads keep their original kind but carry gc.attempt.
	if subject.Metadata["gc.attempt"] != "" {
		return true
	}
	return false
}

func processWorkflowFinalize(store beads.Store, bead beads.Bead) (ControlResult, error) {
	rootID := bead.Metadata["gc.root_bead_id"]
	if rootID == "" {
		return ControlResult{}, fmt.Errorf("%s: missing gc.root_bead_id", bead.ID)
	}

	outcome, err := resolveFinalizeOutcome(store, bead.ID)
	if err != nil {
		if errors.Is(err, errFinalizePending) {
			return ControlResult{}, ErrControlPending
		}
		return ControlResult{}, fmt.Errorf("%s: resolving workflow outcome: %w", bead.ID, err)
	}

	// Close the root BEFORE the finalize bead. If the root close fails and
	// the control-dispatcher crashes, the finalize bead stays open so the
	// next serve cycle will retry. Closing the finalize first would make it
	// non-retriable (ProcessControl skips closed beads), stranding the root
	// as in_progress forever.
	if err := setOutcomeAndClose(store, rootID, outcome); err != nil {
		return ControlResult{}, fmt.Errorf("%s: completing workflow head: %w", rootID, err)
	}
	if err := setOutcomeAndClose(store, bead.ID, "pass"); err != nil {
		return ControlResult{}, fmt.Errorf("%s: completing workflow finalizer: %w", bead.ID, err)
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
		if errors.Is(err, errScopeBodyMissing) {
			return ControlResult{}, ErrControlPending
		}
		return ControlResult{}, fmt.Errorf("%s: loading scope body for %s: %w", bead.ID, scopeRef, err)
	}

	if bead.Metadata["gc.outcome"] == "fail" {
		skipped, err := skipOpenScopeMembers(store, rootID, scopeRef, bead.ID)
		if err != nil {
			return ControlResult{}, fmt.Errorf("%s: aborting scope: %w", bead.ID, err)
		}
		if body.Status != "closed" {
			// Propagate non-gc.* member metadata (e.g., review.verdict) onto the
			// scope body before closing, so diagnostics survive failure auto-close.
			if err := propagateScopeMemberMetadata(store, rootID, scopeRef, body.ID); err != nil {
				return ControlResult{}, fmt.Errorf("%s: propagating scope metadata: %w", bead.ID, err)
			}
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
	if err := propagateScopeMemberMetadata(store, rootID, scopeRef, body.ID); err != nil {
		return ControlResult{}, fmt.Errorf("%s: propagating scope metadata: %w", bead.ID, err)
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
	return beads.Bead{}, fmt.Errorf("%w: scope %q not found under root %s", errScopeBodyMissing, scopeRef, rootID)
}

func skipOpenScopeMembers(store beads.Store, rootID, scopeRef, skipControlID string) (int, error) {
	snapshot, err := loadScopeSnapshot(store, rootID, scopeRef)
	if err != nil {
		return 0, err
	}
	return snapshot.skipOpenScopeMembers(store, skipControlID)
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
	snapshot, err := loadScopeSnapshot(store, rootID, scopeRef)
	if err != nil {
		return false, err
	}
	return snapshot.hasOpenScopeMembers(), nil
}

func listByWorkflowRoot(store beads.Store, rootID string) ([]beads.Bead, error) {
	all, err := store.List(beads.ListQuery{
		Metadata:      map[string]string{"gc.root_bead_id": rootID},
		IncludeClosed: true,
	})
	if err != nil {
		return nil, err
	}

	result := make([]beads.Bead, 0, len(all)+1)
	seen := make(map[string]bool, len(all)+1)
	if root, err := store.Get(rootID); err == nil {
		result = append(result, root)
		seen[root.ID] = true
	} else if !errors.Is(err, beads.ErrNotFound) {
		return nil, err
	}
	for _, bead := range all {
		if seen[bead.ID] {
			continue
		}
		result = append(result, bead)
		seen[bead.ID] = true
	}
	return result, nil
}

func isLogicalDescendant(logical, candidate beads.Bead) bool {
	if logical.ID == "" || candidate.ID == "" || logical.ID == candidate.ID {
		return false
	}
	if candidate.Metadata["gc.logical_bead_id"] == logical.ID {
		return true
	}
	for _, prefix := range []string{".run.", ".eval.", ".check.", ".iteration.", ".attempt."} {
		if strings.HasPrefix(candidate.ID, logical.ID+prefix) {
			return true
		}
	}
	logicalRef := strings.TrimSpace(logical.Metadata["gc.step_ref"])
	if logicalRef == "" {
		logicalRef = strings.TrimSpace(logical.Ref)
	}
	if logicalRef == "" {
		return false
	}
	candidateRef := strings.TrimSpace(candidate.Metadata["gc.step_ref"])
	if candidateRef == "" {
		candidateRef = strings.TrimSpace(candidate.Ref)
	}
	for _, prefix := range []string{".run.", ".eval.", ".check.", ".iteration.", ".attempt."} {
		if strings.HasPrefix(candidateRef, logicalRef+prefix) {
			return true
		}
	}
	if logicalStepID := strings.TrimSpace(logical.Metadata["gc.step_id"]); logicalStepID != "" {
		if strings.TrimSpace(candidate.Metadata["gc.ralph_step_id"]) == logicalStepID {
			return true
		}
	}
	return false
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
	status := "closed"
	return store.Update(beadID, beads.UpdateOpts{
		Status:   &status,
		Metadata: map[string]string{"gc.outcome": outcome},
	})
}

// reconcileClosedScopeMember re-reads the just-closed bead and delegates to
// reconcileTerminalScopedMember. Callers invoke it immediately after
// setOutcomeAndClose, so this relies on the store being read-after-write
// consistent (true for MemStore today). If a future store becomes eventually
// consistent, pass the in-memory closed bead directly instead of re-reading.
func reconcileClosedScopeMember(store beads.Store, beadID string) (ControlResult, error) {
	closedBead, err := store.Get(beadID)
	if err != nil {
		return ControlResult{}, fmt.Errorf("%s: reloading closed scoped member: %w", beadID, err)
	}
	if closedBead.Status != "closed" {
		return ControlResult{}, nil
	}
	return reconcileTerminalScopedMember(store, closedBead)
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
	snapshot, err := loadScopeSnapshot(store, rootID, scopeRef)
	if err != nil {
		return "", err
	}
	return snapshot.resolveScopeOutputJSON(subject)
}
