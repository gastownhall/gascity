package dispatch

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/formula"
	"github.com/gastownhall/gascity/internal/molecule"
	"github.com/gastownhall/gascity/internal/sourceworkflow"
)

// ControlResult reports whether a control bead was processed and what it did.
type ControlResult struct {
	Processed bool
	Action    string
	Created   int
	Skipped   int
}

// SourceWorkflowStore identifies a store that may contain workflow roots for
// source-workflow singleton checks.
type SourceWorkflowStore struct {
	Store    beads.Store
	StoreRef string
}

// ProcessOptions provides control-dispatcher execution context.
type ProcessOptions struct {
	// Context optionally cancels bounded retry waits inside ProcessControl.
	Context            context.Context
	CityPath           string
	StorePath          string
	FormulaSearchPaths []string
	PrepareFragment    func(*formula.FragmentRecipe, beads.Bead) error
	PrepareRecipe      func(*formula.Recipe, beads.Bead) error
	RecycleSession     func(beads.Bead) error
	// RequiredArtifactStat checks required-artifact files. When nil, the
	// dispatcher uses os.Stat.
	RequiredArtifactStat func(path string) (os.FileInfo, error)
	// ResolveStoreRef opens the bead store identified by a gc.source_store_ref
	// value (e.g. "city:foo", "rig:alpha"). Used by processWorkflowFinalize to
	// propagate successful workflow completion across store boundaries: when
	// a graph workflow finalizes with outcome=pass, every parent source bead
	// linked via gc.source_bead_id+gc.source_store_ref is also closed in its
	// native store. May be nil - in which case cross-store propagation is
	// silently skipped (single-store callers, tests without resolvers, etc.).
	ResolveStoreRef func(ref string) (beads.Store, error)
	// SourceWorkflowLock serializes source-bead mutation with graph workflow
	// launch/recovery for the same store ref and source bead ID. May be nil
	// for single-process tests and callers without cross-store propagation.
	SourceWorkflowLock func(storeRef, sourceBeadID string, fn func() error) error
	// SourceWorkflowStores returns every store that may contain live workflow
	// roots. When set, workflow-finalize uses it to avoid closing a source bead
	// while any live root in another store still references that source.
	SourceWorkflowStores func() ([]SourceWorkflowStore, error)
	// MemberStores is the work-class store tail probed (after the primary
	// graph store) when a drain reads convoy membership. A control bead and
	// its drain item-root molecules live in the graph store, but the convoy
	// members a drain expands over are work beads that may live in a different
	// per-class store; MemberStores supplies those additional stores to
	// convoycore.Members so member Gets resolve across the class boundary.
	// Empty (the default for single-store callers) collapses the probe set to
	// the primary store, exactly matching the pre-seam single-store behavior.
	MemberStores []beads.Store
	Tracef       func(format string, args ...any)
}

var (
	errFinalizePending  = errors.New("workflow finalize pending")
	errScopeBodyMissing = errors.New("scope body missing")
)

const (
	maxSourceChainHops               = 32
	maxWorkflowFinalizeErrorMetadata = 512
	// Keep this retry window short and bounded while covering common
	// sub-second Dolt read-after-write visibility lag for newly created scope
	// bodies. When ProcessOptions.Context is set, retry waits exit promptly
	// on cancellation.
	scopeBodyResolveAttempts   = 5
	scopeBodyResolveRetryDelay = 100 * time.Millisecond
)

const workflowFinalizeErrorMetadataKey = beadmeta.LastFinalizeErrorMetadataKey

// ErrControlPending reports that a control bead is not yet processable but
// should be retried later.
var ErrControlPending = errors.New("workflow control pending")

// ErrControlGraphMalformed reports that a control bead refers to graph state
// that cannot become valid by waiting.
var ErrControlGraphMalformed = errors.New("workflow control graph malformed")

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
			bead.ID, bead.Metadata[beadmeta.KindMetadataKey], bead.Status)
		return ControlResult{}, nil
	}
	if result, handled, err := closeOrphanedControl(store, bead, opts); handled || err != nil {
		return result, err
	}

	switch bead.Metadata[beadmeta.KindMetadataKey] {
	case beadmeta.KindRetry:
		return processRetryControl(store, bead, opts)
	case beadmeta.KindRalph:
		return processRalphControl(store, bead, opts)
	case beadmeta.KindCheck:
		return processRalphCheck(store, bead, opts)
	case beadmeta.KindRetryEval:
		return processRetryEval(store, bead, opts)
	case beadmeta.KindFanout:
		return processFanout(store, bead, opts)
	case beadmeta.KindDrain:
		return processDrain(store, bead, opts)
	case beadmeta.KindScopeCheck:
		return processScopeCheck(store, bead, opts)
	case beadmeta.KindWorkflowFinalize:
		return processWorkflowFinalize(store, bead, opts)
	default:
		return ControlResult{}, fmt.Errorf("%s: unsupported control bead kind %q", bead.ID, bead.Metadata[beadmeta.KindMetadataKey])
	}
}

func closeOrphanedControl(store beads.Store, bead beads.Bead, opts ProcessOptions) (ControlResult, bool, error) {
	if bead.Metadata[beadmeta.KindMetadataKey] == beadmeta.KindWorkflowFinalize {
		return ControlResult{}, false, nil
	}
	rootID := strings.TrimSpace(bead.Metadata[beadmeta.RootBeadIDMetadataKey])
	rootStoreRef := strings.TrimSpace(bead.Metadata[beadmeta.RootStoreRefMetadataKey])
	if rootID == "" || rootStoreRef == "" || rootID == bead.ID {
		return ControlResult{}, false, nil
	}
	if _, err := store.Get(rootID); err == nil {
		return ControlResult{}, false, nil
	} else if !errors.Is(err, beads.ErrNotFound) {
		return ControlResult{}, false, fmt.Errorf("%s: loading workflow root %s: %w", bead.ID, rootID, err)
	}

	opts.tracef("process-control bead=%s kind=%s close reason=missing_workflow_root root=%s store_ref=%s",
		bead.ID, bead.Metadata[beadmeta.KindMetadataKey], rootID, rootStoreRef)
	closeMetadata := map[string]string{
		beadmeta.OutcomeMetadataKey:           beadmeta.OutcomeFail,
		beadmeta.FailureClassMetadataKey:      beadmeta.FailureClassHard,
		beadmeta.FailureReasonMetadataKey:     "missing_workflow_root",
		beadmeta.FinalDispositionMetadataKey:  beadmeta.DispositionOrphanedWorkflow,
		beadmeta.MissingRootBeadIDMetadataKey: rootID,
	}
	clearControllerSpawnErrorMetadata(closeMetadata)
	if err := updateMetadataAndClose(store, bead.ID, closeMetadata); err != nil {
		return ControlResult{}, true, fmt.Errorf("%s: closing orphaned control: %w", bead.ID, err)
	}
	return ControlResult{Processed: true, Action: "orphaned-workflow"}, true, nil
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
	rootID := bead.Metadata[beadmeta.RootBeadIDMetadataKey]
	scopeRef := bead.Metadata[beadmeta.ScopeRefMetadataKey]
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
	body, err := tracePhase(opts, bead.ID, "resolve-body", func() (beads.Bead, error) {
		return resolveScopeBody(store, rootID, scopeRef, bead.ID, opts)
	})
	if err != nil {
		if errors.Is(err, errScopeBodyMissing) {
			return ControlResult{}, fmt.Errorf("%w: %w", ErrControlGraphMalformed, err)
		}
		return ControlResult{}, fmt.Errorf("%s: loading scope body for %s: %w", bead.ID, scopeRef, err)
	}

	if isRetryAttemptSubject(subject) {
		if subject.Status != "closed" {
			opts.tracef("scope-check bead=%s subject=%s pending status=%s", bead.ID, subject.ID, subject.Status)
			return ControlResult{}, ErrControlPending
		}
		remainingOpen, err := tracePhase(opts, bead.ID, "check-open-members", func() (bool, error) {
			return hasOpenScopeMembers(store, rootID, scopeRef, bead.ID)
		})
		if err != nil {
			return ControlResult{}, fmt.Errorf("%s: checking scope completion: %w", bead.ID, err)
		}
		opts.tracef("scope-check bead=%s phase=check-remaining-open remaining_open=%t ignore=%s", bead.ID, remainingOpen, bead.ID)
		if !remainingOpen {
			snapshot, err := loadScopeSnapshotForControl(store, rootID, scopeRef, body, subject, bead.ID, opts)
			if err != nil {
				return ControlResult{}, err
			}
			if err := closeScopeAsPassed(store, snapshot, subject, opts, bead.ID); err != nil {
				return ControlResult{}, err
			}
			if err := tracePhaseErr(opts, bead.ID, "close-control", func() error {
				return setOutcomeAndClose(store, bead.ID, beadmeta.OutcomePass)
			}); err != nil {
				return ControlResult{}, fmt.Errorf("%s: completing retry-attempt control bead: %w", bead.ID, err)
			}
			return ControlResult{Processed: true, Action: "scope-pass"}, nil
		}
		if err := tracePhaseErr(opts, bead.ID, "close-control", func() error {
			return setOutcomeAndClose(store, bead.ID, beadmeta.OutcomePass)
		}); err != nil {
			return ControlResult{}, fmt.Errorf("%s: completing retry-attempt control bead: %w", bead.ID, err)
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

	if beadOutcomeFailed(subject) {
		snapshot, err := loadScopeSnapshotForControl(store, rootID, scopeRef, body, subject, bead.ID, opts)
		if err != nil {
			return ControlResult{}, err
		}
		skipped, err := abortScope(store, snapshot, opts, bead.ID)
		if err != nil {
			return ControlResult{}, err
		}
		if err := tracePhaseErr(opts, bead.ID, "close-control", func() error {
			return setOutcomeAndClose(store, bead.ID, beadmeta.OutcomePass)
		}); err != nil {
			return ControlResult{}, fmt.Errorf("%s: completing control bead: %w", bead.ID, err)
		}
		return ControlResult{Processed: true, Action: "scope-fail", Skipped: skipped}, nil
	}

	remainingOpen, err := tracePhase(opts, bead.ID, "check-open-members", func() (bool, error) {
		return hasOpenScopeMembers(store, rootID, scopeRef, bead.ID)
	})
	if err != nil {
		return ControlResult{}, fmt.Errorf("%s: checking scope completion: %w", bead.ID, err)
	}
	opts.tracef("scope-check bead=%s phase=check-remaining-open remaining_open=%t ignore=%s", bead.ID, remainingOpen, bead.ID)
	if !remainingOpen {
		snapshot, err := loadScopeSnapshotForControl(store, rootID, scopeRef, body, subject, bead.ID, opts)
		if err != nil {
			return ControlResult{}, err
		}
		if err := closeScopeAsPassed(store, snapshot, subject, opts, bead.ID); err != nil {
			return ControlResult{}, err
		}
		if err := tracePhaseErr(opts, bead.ID, "close-control", func() error {
			return setOutcomeAndClose(store, bead.ID, beadmeta.OutcomePass)
		}); err != nil {
			return ControlResult{}, fmt.Errorf("%s: completing control bead: %w", bead.ID, err)
		}
		return ControlResult{Processed: true, Action: "scope-pass"}, nil
	}
	if err := tracePhaseErr(opts, bead.ID, "close-control", func() error {
		return setOutcomeAndClose(store, bead.ID, beadmeta.OutcomePass)
	}); err != nil {
		return ControlResult{}, fmt.Errorf("%s: completing control bead: %w", bead.ID, err)
	}

	return ControlResult{Processed: true, Action: "continue"}, nil
}

func loadScopeSnapshotForControl(store beads.Store, rootID, scopeRef string, body, subject beads.Bead, controlID string, opts ProcessOptions) (scopeSnapshot, error) {
	snapshot, err := tracePhase(opts, controlID, "load-snapshot", func() (scopeSnapshot, error) {
		return loadScopeSnapshotWithBody(store, rootID, scopeRef, body)
	})
	if err != nil {
		return scopeSnapshot{}, fmt.Errorf("%s: loading scope snapshot for %s: %w", controlID, scopeRef, err)
	}
	opts.tracef("scope-check bead=%s snapshot root=%s scope=%s all=%d members=%d body=%s subject=%s outcome=%s",
		controlID, rootID, scopeRef, len(snapshot.all), len(snapshot.members), snapshot.body.ID, subject.ID, subject.Metadata[beadmeta.OutcomeMetadataKey])
	return snapshot, nil
}

// closeScopeAsPassed runs the shared convergence sequence for a scope whose
// members have all completed successfully: propagate non-gc.* member metadata
// onto the scope body, resolve the scope's output_json from subject, then close
// the body with outcome=pass (unless it is already closed). subject supplies the
// output_json source and traceID identifies the bead driving the trace. The
// caller owns closing its own control bead. This is the single implementation
// shared by both processScopeCheck branches and reconcileTerminalScopedMember,
// so every convergence observer closes a scope identically.
func closeScopeAsPassed(store beads.Store, snapshot scopeSnapshot, subject beads.Bead, opts ProcessOptions, traceID string) error {
	bodyID := snapshot.body.ID
	// Propagate non-gc metadata from scope members to the scope body. This
	// enables compositional metadata bubbling: attempt → retry → scope →
	// ralph → parent scope, etc.
	if err := tracePhaseErr(opts, traceID, "propagate-metadata", func() error {
		return snapshot.propagateScopeMemberMetadata(store, bodyID)
	}); err != nil {
		return fmt.Errorf("%s: propagating scope metadata: %w", traceID, err)
	}
	outputJSON, err := tracePhase(opts, traceID, "resolve-output", func() (string, error) {
		return snapshot.resolveScopeOutputJSON(subject)
	})
	if err != nil {
		return fmt.Errorf("%s: resolving scope output: %w", traceID, err)
	}
	if outputJSON != "" {
		if err := tracePhaseErr(opts, traceID, "write-output", func() error {
			return store.SetMetadata(bodyID, beadmeta.OutputJSONMetadataKey, outputJSON)
		}); err != nil {
			return fmt.Errorf("%s: propagating scope output: %w", bodyID, err)
		}
	}
	bodyAfter, err := tracePhase(opts, traceID, "reload-body", func() (beads.Bead, error) {
		return store.Get(bodyID)
	})
	if err != nil {
		return fmt.Errorf("%s: reloading scope body: %w", bodyID, err)
	}
	if bodyAfter.Status != "closed" {
		if err := tracePhaseErr(opts, traceID, "close-body", func() error {
			return setOutcomeAndClose(store, bodyID, beadmeta.OutcomePass)
		}); err != nil {
			return fmt.Errorf("%s: completing scope body: %w", bodyID, err)
		}
	}
	return nil
}

// abortScope runs the shared convergence sequence for a scope whose subject
// failed: skip any still-open members, propagate non-gc.* member metadata (e.g.
// review.verdict) onto the scope body so diagnostics survive failure auto-close,
// then close the body with outcome=fail. It returns the number of members
// skipped. traceID identifies the bead driving the trace and is also the id
// ignored when skipping open members. The caller owns closing its own control
// bead. This is the single implementation shared by processScopeCheck and
// reconcileTerminalScopedMember.
func abortScope(store beads.Store, snapshot scopeSnapshot, opts ProcessOptions, traceID string) (int, error) {
	bodyID := snapshot.body.ID
	skipped, err := tracePhase(opts, traceID, "skip-open-members", func() (int, error) {
		return snapshot.skipOpenScopeMembers(store, traceID)
	})
	if err != nil {
		return 0, fmt.Errorf("%s: aborting scope: %w", traceID, err)
	}
	if err := tracePhaseErr(opts, traceID, "propagate-metadata", func() error {
		return snapshot.propagateScopeMemberMetadata(store, bodyID)
	}); err != nil {
		return 0, fmt.Errorf("%s: propagating scope metadata: %w", traceID, err)
	}
	if err := tracePhaseErr(opts, traceID, "close-body-fail", func() error {
		return setOutcomeAndClose(store, bodyID, beadmeta.OutcomeFail)
	}); err != nil {
		return 0, fmt.Errorf("%s: completing scope body: %w", bodyID, err)
	}
	return skipped, nil
}

type scopeSnapshot struct {
	rootID      string
	scopeRef    string
	all         []beads.Bead
	allComplete bool
	members     []beads.Bead
	body        beads.Bead
}

func loadScopeSnapshotWithBody(store beads.Store, rootID, scopeRef string, body beads.Bead) (scopeSnapshot, error) {
	members, err := listByWorkflowRootAndScope(store, rootID, scopeRef)
	if err != nil {
		return scopeSnapshot{}, err
	}
	snapshot := scopeSnapshot{
		rootID:   rootID,
		scopeRef: scopeRef,
		members:  members,
		body:     body,
	}
	snapshot.all = mergeScopeSnapshotBeads(snapshot.members, snapshot.body)
	return snapshot, nil
}

func listByWorkflowRootAndScope(store beads.Store, rootID, scopeRef string) ([]beads.Bead, error) {
	return beads.HandlesFor(store).Live.List(beads.ListQuery{
		Metadata: map[string]string{
			beadmeta.RootBeadIDMetadataKey: rootID,
			beadmeta.ScopeRefMetadataKey:   scopeRef,
		},
		IncludeClosed: true,
	})
}

func listActiveByWorkflowRootAndScope(store beads.Store, rootID, scopeRef string) ([]beads.Bead, error) {
	return beads.HandlesFor(store).Live.List(beads.ListQuery{
		Metadata: map[string]string{
			beadmeta.RootBeadIDMetadataKey: rootID,
			beadmeta.ScopeRefMetadataKey:   scopeRef,
		},
	})
}

func mergeScopeSnapshotBeads(members []beads.Bead, body beads.Bead) []beads.Bead {
	out := make([]beads.Bead, 0, len(members)+1)
	seen := make(map[string]struct{}, len(members)+1)
	for _, bead := range members {
		if bead.ID == "" {
			continue
		}
		if _, ok := seen[bead.ID]; ok {
			continue
		}
		out = append(out, bead)
		seen[bead.ID] = struct{}{}
	}
	if body.ID != "" {
		if _, ok := seen[body.ID]; !ok {
			out = append(out, body)
		}
	}
	return out
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
		if member.Metadata[beadmeta.KindMetadataKey] == beadmeta.KindSpec {
			continue
		}
		switch member.Metadata[beadmeta.ScopeRoleMetadataKey] {
		case "body", "teardown":
			continue
		default:
			return true
		}
	}
	return false
}

func hasOpenScopeMembers(store beads.Store, rootID, scopeRef string, ignoreIDs ...string) (bool, error) {
	members, err := listActiveByWorkflowRootAndScope(store, rootID, scopeRef)
	if err != nil {
		return false, err
	}
	return scopeSnapshot{members: members}.hasOpenScopeMembers(ignoreIDs...), nil
}

func (s scopeSnapshot) propagateScopeMemberMetadata(store beads.Store, bodyID string) error {
	batch := map[string]string{}
	for _, member := range s.members {
		if member.Status != "closed" {
			continue
		}
		switch member.Metadata[beadmeta.ScopeRoleMetadataKey] {
		case "body", "teardown", "control":
			continue
		}
		for key, value := range member.Metadata {
			if key == "" || strings.HasPrefix(key, beadmeta.Namespace) {
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
	if outputJSON := subject.Metadata[beadmeta.OutputJSONMetadataKey]; outputJSON != "" {
		return outputJSON, nil
	}

	var candidate string
	for _, bead := range s.members {
		if bead.Metadata[beadmeta.OutputJSONMetadataKey] == "" {
			continue
		}
		switch bead.Metadata[beadmeta.ScopeRoleMetadataKey] {
		case "body", "teardown", "control":
			continue
		}
		if candidate == "" {
			candidate = bead.Metadata[beadmeta.OutputJSONMetadataKey]
			continue
		}
		if candidate != bead.Metadata[beadmeta.OutputJSONMetadataKey] {
			return "", nil
		}
	}
	return candidate, nil
}

func (s scopeSnapshot) skipOpenScopeMembers(store beads.Store, skipControlID string) (int, error) {
	all := s.all
	if !s.allComplete {
		loaded, err := listByWorkflowRoot(store, s.rootID)
		if err != nil {
			return 0, err
		}
		all = loaded
	}
	pending := make(map[string]beads.Bead)
	for _, member := range s.members {
		if member.ID == skipControlID || member.Status != "open" {
			continue
		}
		if member.Metadata[beadmeta.KindMetadataKey] == beadmeta.KindSpec {
			continue
		}
		switch member.Metadata[beadmeta.ScopeRoleMetadataKey] {
		case "body", "teardown":
			continue
		}
		pending[member.ID] = member
	}
	for _, member := range s.members {
		switch strings.TrimSpace(member.Metadata[beadmeta.KindMetadataKey]) {
		case "retry", "ralph":
		default:
			continue
		}
		switch member.Metadata[beadmeta.ScopeRoleMetadataKey] {
		case "body", "teardown":
			continue
		}
		for _, candidate := range all {
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
		ids := sortedPendingIDs(pending)
		depsByID, err := loadDownDepsForScopeSkip(store, ids)
		if err != nil {
			return skipped, err
		}
		skippable := make([]string, 0, len(ids))
		for _, id := range ids {
			if preserveScopeCheckForSubject(pending[id], depsByID[id], skipControlID) {
				delete(pending, id)
				continue
			}
			if !canSkipScopeMemberWithDeps(depsByID[id], pending) {
				continue
			}
			skippable = append(skippable, id)
		}
		if len(skippable) == 0 {
			if len(pending) == 0 {
				break
			}
			return skipped, fmt.Errorf("unable to skip remaining scope members: %v", ids)
		}
		closed, err := skipScopeMembers(store, skippable)
		if err != nil {
			return skipped + closed, err
		}
		for _, id := range skippable {
			delete(pending, id)
		}
		skipped += closed
	}

	return skipped, nil
}

func preserveScopeCheckForSubject(candidate beads.Bead, deps []beads.Dep, subjectID string) bool {
	if subjectID == "" {
		return false
	}
	// Keep the failed subject's own scope-check replayable: if abort
	// reconciliation skips siblings but fails before closing the body, that
	// control bead is the idempotent recovery path.
	if candidate.Metadata[beadmeta.KindMetadataKey] != beadmeta.KindScopeCheck {
		return false
	}
	if candidate.Metadata[beadmeta.ScopeRoleMetadataKey] != "control" {
		return false
	}
	for _, dep := range deps {
		if dep.Type == "blocks" && dep.DependsOnID == subjectID {
			return true
		}
	}
	return false
}

// beadOutcomeFailed reports whether a closed bead counts as failed for
// scope-abort and outcome-aggregation purposes. gc.outcome=fail is always a
// failure. For beads that opted into gc.on_fail=abort_scope, the
// worker-result contract is fail-closed (mirroring the retry metadata
// firewall in classifyRetryAttempt): a bare close with no gc.outcome, or an
// unknown gc.outcome value, is treated as a failure rather than as success.
// Retry-managed attempt subjects are exempt — their contract violations are
// classified by retry-eval as transient retries, not scope aborts.
func beadOutcomeFailed(subject beads.Bead) bool {
	outcome := strings.TrimSpace(subject.Metadata[beadmeta.OutcomeMetadataKey])
	if outcome == beadmeta.OutcomeFail {
		return true
	}
	if strings.TrimSpace(subject.Metadata[beadmeta.OnFailMetadataKey]) != "abort_scope" || isRetryAttemptSubject(subject) {
		return false
	}
	switch outcome {
	case beadmeta.OutcomePass, beadmeta.OutcomeSkipped:
		return false
	default:
		return true
	}
}

func isRetryAttemptSubject(subject beads.Bead) bool {
	if subject.Metadata[beadmeta.LogicalBeadIDMetadataKey] == "" {
		return false
	}
	// v1 pattern: attempt beads have gc.kind "retry-run" or "retry-eval".
	switch subject.Metadata[beadmeta.KindMetadataKey] {
	case "retry-run", "retry-eval":
		return true
	}
	// v2 pattern: attempt beads keep their original kind but carry gc.attempt.
	if subject.Metadata[beadmeta.AttemptMetadataKey] != "" {
		return true
	}
	return false
}

func processWorkflowFinalize(store beads.Store, bead beads.Bead, opts ProcessOptions) (ControlResult, error) {
	rootID := bead.Metadata[beadmeta.RootBeadIDMetadataKey]
	if rootID == "" {
		return ControlResult{}, fmt.Errorf("%s: missing gc.root_bead_id", bead.ID)
	}

	outcome, err := resolveFinalizeOutcome(store, bead)
	if err != nil {
		if errors.Is(err, errFinalizePending) {
			return ControlResult{}, ErrControlPending
		}
		return ControlResult{}, fmt.Errorf("%s: resolving workflow outcome: %w", bead.ID, err)
	}

	// On success, propagate the closure across the gc.source_bead_id chain so
	// parent source beads in other stores (e.g. the city-scope "Adopt PR"
	// request that spawned a rig-scope mol-adopt-pr-v2 workflow) don't accumulate
	// as orphans. Failures intentionally leave parent sources open so a human
	// can investigate via list - the bead IS the audit handle.
	if outcome == beadmeta.OutcomePass {
		if err := preflightSourceBeadChain(store, rootID, opts); err != nil {
			return ControlResult{}, recordWorkflowFinalizeError(store, bead.ID, fmt.Errorf("%s: preflighting source bead chain: %w", rootID, err))
		}
	}
	// Close the root BEFORE the finalize bead. If the root close fails and
	// the control-dispatcher crashes, the finalize bead stays open so the
	// next serve cycle will retry. Source-chain propagation is preflighted first
	// so retryable scan failures keep the root live for singleton scans, but
	// source beads are not mutated until the root is durably closed.
	if err := setOutcomeAndClose(store, rootID, outcome); err != nil {
		if errors.Is(err, beads.ErrNotFound) {
			if closeErr := setOutcomeAndClose(store, bead.ID, beadmeta.OutcomeMissingRoot); closeErr != nil {
				return ControlResult{}, recordWorkflowFinalizeError(store, bead.ID, fmt.Errorf("%s: closing orphaned finalizer (root %s missing): %w", bead.ID, rootID, closeErr))
			}
			return ControlResult{Processed: true, Action: "workflow-missing_root"}, nil
		}
		return ControlResult{}, recordWorkflowFinalizeError(store, bead.ID, fmt.Errorf("%s: completing workflow head: %w", rootID, err))
	}
	if _, err := sourceworkflow.CloseSpecSidecarsForRoot(store, rootID, sourceworkflow.WorkflowSpecSidecarClosedReason); err != nil {
		return ControlResult{}, recordWorkflowFinalizeError(store, bead.ID, fmt.Errorf("%s: closing workflow spec sidecars: %w", rootID, err))
	}
	if outcome == beadmeta.OutcomePass {
		if err := closeSourceBeadChain(store, rootID, opts); err != nil {
			return ControlResult{}, recordWorkflowFinalizeError(store, bead.ID, fmt.Errorf("%s: closing source bead chain: %w", rootID, err))
		}
	}
	if err := setOutcomeAndClose(store, bead.ID, beadmeta.OutcomePass); err != nil {
		return ControlResult{}, recordWorkflowFinalizeError(store, bead.ID, fmt.Errorf("%s: completing workflow finalizer: %w", bead.ID, err))
	}

	// Purge the molecule-scoped artifact tree now that the workflow has
	// terminated. Artifact lifetime is anchored to the molecule, not the
	// worker worktree — see internal/molecule/artifact.go. Best-effort:
	// os.RemoveAll is idempotent, so a retry after controller crash is
	// safe. Gated on CityPath being present so tests that omit it don't
	// spuriously touch the real filesystem.
	if strings.TrimSpace(opts.CityPath) != "" {
		if err := molecule.RemoveDir(opts.CityPath, rootID); err != nil {
			opts.tracef("workflow-finalize bead=%s root=%s artifact-purge-err=%v", bead.ID, rootID, err)
		}
	}

	return ControlResult{Processed: true, Action: "workflow-" + outcome}, nil
}

func preflightSourceBeadChain(rootStore beads.Store, rootID string, opts ProcessOptions) error {
	return walkSourceBeadChain(rootStore, rootID, opts, false)
}

// closeSourceBeadChain walks gc.source_bead_id / gc.source_store_ref upward
// from the just-finalized workflow root and closes every parent source bead
// in its native store. A missing resolver for a cross-store ref, a deleted
// parent, or a cycle stops the walk as a traced no-op.
// Resolver, store read, and close failures are returned so the finalizer stays
// open for retry. This is what makes "Adopt PR" city-scope source beads
// disappear from the human-visible queue once the rig-scope workflow merges.
func closeSourceBeadChain(rootStore beads.Store, rootID string, opts ProcessOptions) error {
	return walkSourceBeadChain(rootStore, rootID, opts, true)
}

func walkSourceBeadChain(rootStore beads.Store, rootID string, opts ProcessOptions, mutate bool) error {
	currentStore := rootStore
	currentID := rootID
	currentRef := ""
	excludeRootSourceRef := ""
	resolvedStores := make(map[string]beads.Store)
	visited := make(map[string]bool)
	for hop := 0; hop < maxSourceChainHops; hop++ {
		current, err := currentStore.Get(currentID)
		if err != nil {
			if errors.Is(err, beads.ErrNotFound) {
				opts.tracef("close-source-chain root=%s stop reason=deleted_current at_id=%s ref=%s", rootID, currentID, sourceChainStoreLabel(currentRef))
				return nil
			}
			return fmt.Errorf("getting source chain bead %s in %s: %w", currentID, sourceChainStoreLabel(currentRef), err)
		}
		if currentID == rootID && currentRef == "" {
			excludeRootSourceRef = strings.TrimSpace(current.Metadata[sourceworkflow.SourceStoreRefMetadataKey])
		}
		nextID := strings.TrimSpace(current.Metadata[beadmeta.SourceBeadIDMetadataKey])
		if nextID == "" {
			opts.tracef("close-source-chain root=%s stop reason=no_source at_id=%s ref=%s", rootID, currentID, sourceChainStoreLabel(currentRef))
			return nil
		}
		nextRef := strings.TrimSpace(current.Metadata[sourceworkflow.SourceStoreRefMetadataKey])
		effectiveRef := currentRef
		nextStore := currentStore
		if nextRef != "" {
			effectiveRef = nextRef
			if opts.ResolveStoreRef == nil {
				opts.tracef("close-source-chain root=%s stop reason=missing_resolver source=%s ref=%s", rootID, nextID, sourceChainStoreLabel(effectiveRef))
				return nil
			}
			resolved, ok := resolvedStores[nextRef]
			if !ok {
				resolvedStore, err := opts.ResolveStoreRef(nextRef)
				if err != nil {
					return fmt.Errorf("resolving source store %q: %w", nextRef, err)
				}
				if resolvedStore == nil {
					return fmt.Errorf("resolving source store %q: nil store", nextRef)
				}
				resolved = resolvedStore
				resolvedStores[nextRef] = resolvedStore
			}
			nextStore = resolved
		}
		key := sourceChainKey(effectiveRef, nextID)
		if visited[key] {
			opts.tracef("close-source-chain root=%s stop reason=cycle source=%s ref=%s", rootID, nextID, sourceChainStoreLabel(effectiveRef))
			return nil
		}
		visited[key] = true

		var stopWalk bool
		loadAndClose := func() error {
			loaded, err := nextStore.Get(nextID)
			if err != nil {
				if errors.Is(err, beads.ErrNotFound) {
					opts.tracef("close-source-chain root=%s stop reason=deleted_parent source=%s ref=%s", rootID, nextID, sourceChainStoreLabel(effectiveRef))
					stopWalk = true
					return nil
				}
				return fmt.Errorf("getting source bead %s in %s: %w", nextID, sourceChainStoreLabel(effectiveRef), err)
			}
			liveRoots, err := listLiveSourceWorkflowRoots(nextStore, nextID, effectiveRef, rootID, excludeRootSourceRef, opts)
			if err != nil {
				return fmt.Errorf("listing live workflows for source bead %s in %s: %w", nextID, sourceChainStoreLabel(effectiveRef), err)
			}
			if len(liveRoots) > 0 {
				opts.tracef("close-source-chain root=%s stop reason=live_child_workflow source=%s ref=%s live_roots=%s", rootID, nextID, sourceChainStoreLabel(effectiveRef), sourceChainRootIDs(liveRoots))
				stopWalk = true
				return nil
			}
			if !mutate {
				return nil
			}
			if err := propagateSourceBeadTerminalMetadata(nextStore, loaded.ID, current.Metadata); err != nil {
				return fmt.Errorf("propagating source bead metadata %s in %s: %w", nextID, sourceChainStoreLabel(effectiveRef), err)
			}
			if loaded.Status == "closed" {
				opts.tracef("close-source-chain root=%s skip reason=already_closed source=%s ref=%s", rootID, nextID, sourceChainStoreLabel(effectiveRef))
				return nil
			}
			if err := closeSourceBeadPreservingOutcome(nextStore, loaded); err != nil {
				return fmt.Errorf("closing source bead %s in %s: %w", nextID, sourceChainStoreLabel(effectiveRef), err)
			}
			opts.tracef("close-source-chain root=%s closed source=%s ref=%s preserved_outcome=%t", rootID, nextID, sourceChainStoreLabel(effectiveRef), strings.TrimSpace(loaded.Metadata[beadmeta.OutcomeMetadataKey]) != "")
			return nil
		}
		if mutate && opts.SourceWorkflowLock != nil {
			if err := opts.SourceWorkflowLock(effectiveRef, nextID, loadAndClose); err != nil {
				return fmt.Errorf("locking source bead %s in %s: %w", nextID, sourceChainStoreLabel(effectiveRef), err)
			}
		} else if err := loadAndClose(); err != nil {
			return err
		}
		if stopWalk {
			return nil
		}
		currentStore = nextStore
		currentID = nextID
		currentRef = effectiveRef
	}
	err := fmt.Errorf("source chain depth limit reached after %d hops", maxSourceChainHops)
	opts.tracef("close-source-chain root=%s stop reason=depth_limit max_hops=%d", rootID, maxSourceChainHops)
	return err
}

func listLiveSourceWorkflowRoots(fallbackStore beads.Store, sourceBeadID, sourceStoreRef, excludeRootID, excludeRootSourceRef string, opts ProcessOptions) ([]beads.Bead, error) {
	stores, err := sourceWorkflowStoresForLiveRootScan(fallbackStore, sourceStoreRef, opts)
	if err != nil {
		return nil, err
	}
	roots := make([]beads.Bead, 0)
	seenRoots := make(map[string]struct{}, len(stores))
	visitedSources := make(map[string]struct{})
	var collect func(string, string) error
	collect = func(currentSourceID, currentSourceStoreRef string) error {
		currentSourceID = strings.TrimSpace(currentSourceID)
		if currentSourceID == "" {
			return nil
		}
		for i, info := range stores {
			if info.Store == nil {
				continue
			}
			rootStoreRef := strings.TrimSpace(info.StoreRef)
			sourceVisitKey := sourceWorkflowScanKey(rootStoreRef, currentSourceStoreRef, currentSourceID, i)
			if _, ok := visitedSources[sourceVisitKey]; ok {
				continue
			}
			visitedSources[sourceVisitKey] = struct{}{}
			matches, err := sourceworkflow.ListLiveRoots(info.Store, currentSourceID, currentSourceStoreRef, rootStoreRef)
			if err != nil {
				return fmt.Errorf("listing live workflows in %s: %w", sourceChainStoreLabel(rootStoreRef), err)
			}
			matches = withoutSourceWorkflowRoot(matches, excludeRootID, excludeRootSourceRef)
			for _, root := range matches {
				rootKey := sourceWorkflowRootKey(rootStoreRef, root.ID, i)
				if _, ok := seenRoots[rootKey]; ok {
					continue
				}
				seenRoots[rootKey] = struct{}{}
				roots = append(roots, root)
			}
			children, err := sourceWorkflowChildSources(info.Store, currentSourceID, currentSourceStoreRef, rootStoreRef)
			if err != nil {
				return fmt.Errorf("listing source workflow children in %s: %w", sourceChainStoreLabel(rootStoreRef), err)
			}
			for _, child := range children {
				if err := collect(child.ID, rootStoreRef); err != nil {
					return err
				}
			}
		}
		return nil
	}
	if err := collect(sourceBeadID, sourceStoreRef); err != nil {
		return nil, err
	}
	return roots, nil
}

func sourceWorkflowStoresForLiveRootScan(fallbackStore beads.Store, sourceStoreRef string, opts ProcessOptions) ([]SourceWorkflowStore, error) {
	if opts.SourceWorkflowStores == nil {
		return []SourceWorkflowStore{{Store: fallbackStore, StoreRef: strings.TrimSpace(sourceStoreRef)}}, nil
	}
	stores, err := opts.SourceWorkflowStores()
	if err != nil {
		return nil, err
	}
	scanned := make([]SourceWorkflowStore, 0, len(stores))
	for _, info := range stores {
		if info.Store == nil {
			continue
		}
		info.StoreRef = strings.TrimSpace(info.StoreRef)
		scanned = append(scanned, info)
	}
	if len(scanned) == 0 {
		return []SourceWorkflowStore{{Store: fallbackStore, StoreRef: strings.TrimSpace(sourceStoreRef)}}, nil
	}
	return scanned, nil
}

func sourceWorkflowChildSources(store beads.Store, sourceBeadID, sourceStoreRef, rootStoreRef string) ([]beads.Bead, error) {
	sourceBeadID = strings.TrimSpace(sourceBeadID)
	if store == nil || sourceBeadID == "" {
		return nil, nil
	}
	candidates, err := beads.HandlesFor(store).Live.List(beads.ListQuery{
		IncludeClosed: true,
		Metadata: map[string]string{
			beadmeta.SourceBeadIDMetadataKey: sourceBeadID,
		},
	})
	if err != nil {
		return nil, err
	}
	children := make([]beads.Bead, 0, len(candidates))
	for _, candidate := range candidates {
		if candidate.ID == "" || sourceworkflow.IsWorkflowRoot(candidate) {
			continue
		}
		if !sourceworkflow.WorkflowMatchesSource(candidate, sourceBeadID, sourceStoreRef, rootStoreRef) {
			continue
		}
		children = append(children, candidate)
	}
	return children, nil
}

func sourceWorkflowScanKey(rootStoreRef, sourceStoreRef, sourceBeadID string, storeIndex int) string {
	keyScope := strings.TrimSpace(rootStoreRef)
	if keyScope == "" {
		keyScope = fmt.Sprintf("store#%d", storeIndex)
	}
	return keyScope + "\x00" + strings.TrimSpace(sourceStoreRef) + "\x00" + strings.TrimSpace(sourceBeadID)
}

func sourceWorkflowRootKey(rootStoreRef, rootID string, storeIndex int) string {
	keyScope := strings.TrimSpace(rootStoreRef)
	if keyScope == "" {
		keyScope = fmt.Sprintf("store#%d", storeIndex)
	}
	return keyScope + "\x00" + strings.TrimSpace(rootID)
}

func withoutSourceWorkflowRoot(roots []beads.Bead, rootID, rootSourceStoreRef string) []beads.Bead {
	rootID = strings.TrimSpace(rootID)
	if rootID == "" || len(roots) == 0 {
		return roots
	}
	rootSourceStoreRef = strings.TrimSpace(rootSourceStoreRef)
	out := roots[:0]
	for _, root := range roots {
		if root.ID != rootID {
			out = append(out, root)
			continue
		}
		// Legacy roots may not have gc.source_store_ref. In that case the
		// exclusion is ID-only and relies on bead IDs being unique across scanned
		// stores; modern roots use the source-store ref check below.
		if rootSourceStoreRef != "" && strings.TrimSpace(root.Metadata[sourceworkflow.SourceStoreRefMetadataKey]) != rootSourceStoreRef {
			out = append(out, root)
		}
	}
	return out
}

func sourceChainKey(storeRef, beadID string) string {
	// Upward finalize walks only need the parent store and source bead. The
	// downward delete-source walk also keys by querying root store because it
	// recursively fans out across every source-workflow store.
	return strings.TrimSpace(storeRef) + "\x00" + strings.TrimSpace(beadID)
}

func sourceChainStoreLabel(storeRef string) string {
	storeRef = strings.TrimSpace(storeRef)
	if storeRef == "" {
		return "current store"
	}
	return storeRef
}

func sourceChainRootIDs(roots []beads.Bead) string {
	ids := make([]string, 0, len(roots))
	for _, root := range roots {
		if root.ID != "" {
			ids = append(ids, root.ID)
		}
	}
	sort.Strings(ids)
	return strings.Join(ids, ",")
}

func closeSourceBeadPreservingOutcome(store beads.Store, bead beads.Bead) error {
	status := "closed"
	opts := beads.UpdateOpts{Status: &status}
	if strings.TrimSpace(bead.Metadata[beadmeta.OutcomeMetadataKey]) == "" {
		opts.Metadata = map[string]string{beadmeta.OutcomeMetadataKey: beadmeta.OutcomePass}
	}
	return store.Update(bead.ID, opts)
}

func propagateSourceBeadTerminalMetadata(store beads.Store, beadID string, metadata map[string]string) error {
	batch := make(map[string]string)
	copyNonGCMetadata(batch, metadata)
	if len(batch) == 0 {
		return nil
	}
	return store.SetMetadataBatch(beadID, batch)
}

func recordWorkflowFinalizeError(store beads.Store, finalizerID string, err error) error {
	if err == nil {
		return nil
	}
	reason := strings.TrimSpace(err.Error())
	if len(reason) > maxWorkflowFinalizeErrorMetadata {
		reason = truncateWorkflowFinalizeErrorMetadata(reason)
	}
	if setErr := store.SetMetadata(finalizerID, workflowFinalizeErrorMetadataKey, reason); setErr != nil {
		return errors.Join(err, fmt.Errorf("recording workflow finalize error on %s: %w", finalizerID, setErr))
	}
	return err
}

func truncateWorkflowFinalizeErrorMetadata(reason string) string {
	limit := maxWorkflowFinalizeErrorMetadata
	if len(reason) <= limit {
		return reason
	}
	for limit > 0 && !utf8.ValidString(reason[:limit]) {
		limit--
	}
	return reason[:limit]
}

func reconcileTerminalScopedMember(store beads.Store, bead beads.Bead) (ControlResult, error) {
	return reconcileTerminalScopedMemberWithOptions(store, bead, ProcessOptions{})
}

func reconcileTerminalScopedMemberWithOptions(store beads.Store, bead beads.Bead, opts ProcessOptions) (ControlResult, error) {
	scopeRef := bead.Metadata[beadmeta.ScopeRefMetadataKey]
	if scopeRef == "" {
		return ControlResult{}, nil
	}
	rootID := bead.Metadata[beadmeta.RootBeadIDMetadataKey]
	if rootID == "" {
		return ControlResult{}, fmt.Errorf("%s: missing gc.root_bead_id", bead.ID)
	}
	body, err := resolveScopeBody(store, rootID, scopeRef, bead.ID, opts)
	if err != nil {
		if errors.Is(err, errScopeBodyMissing) {
			return ControlResult{}, fmt.Errorf("%w: %w", ErrControlGraphMalformed, err)
		}
		return ControlResult{}, fmt.Errorf("%s: loading scope body for %s: %w", bead.ID, scopeRef, err)
	}

	if beadOutcomeFailed(bead) {
		snapshot, err := loadScopeSnapshotWithBody(store, rootID, scopeRef, body)
		if err != nil {
			return ControlResult{}, fmt.Errorf("%s: loading scope snapshot for %s: %w", bead.ID, scopeRef, err)
		}
		skipped, err := abortScope(store, snapshot, opts, bead.ID)
		if err != nil {
			return ControlResult{}, err
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
	snapshot, err := loadScopeSnapshotWithBody(store, rootID, scopeRef, body)
	if err != nil {
		return ControlResult{}, fmt.Errorf("%s: loading scope snapshot for %s: %w", bead.ID, scopeRef, err)
	}
	if err := closeScopeAsPassed(store, snapshot, bead, opts, bead.ID); err != nil {
		return ControlResult{}, err
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

func resolveScopeBody(store beads.Store, rootID, scopeRef, traceID string, opts ProcessOptions) (beads.Bead, error) {
	ctx := opts.Context
	if ctx == nil {
		ctx = context.Background()
	}
	var lastErr error
	for attempt := 1; attempt <= scopeBodyResolveAttempts; attempt++ {
		bead, err := resolveScopeBodyOnce(store, rootID, scopeRef)
		if err == nil {
			opts.tracef("scope-check bead=%s resolve-body attempt=%d root=%s scope=%s result=ok body=%s", traceID, attempt, rootID, scopeRef, bead.ID)
			return bead, nil
		}
		if !errors.Is(err, errScopeBodyMissing) {
			opts.tracef("scope-check bead=%s resolve-body attempt=%d root=%s scope=%s result=error err=%v", traceID, attempt, rootID, scopeRef, err)
			return bead, err
		}
		opts.tracef("scope-check bead=%s resolve-body attempt=%d root=%s scope=%s result=retry reason=missing_body err=%v", traceID, attempt, rootID, scopeRef, err)
		lastErr = err
		if attempt < scopeBodyResolveAttempts {
			timer := time.NewTimer(scopeBodyResolveRetryDelay)
			select {
			case <-ctx.Done():
				if !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
				return beads.Bead{}, ctx.Err()
			case <-timer.C:
			}
		}
	}
	opts.tracef("scope-check bead=%s resolve-body attempts=%d root=%s scope=%s result=exhausted err=%v", traceID, scopeBodyResolveAttempts, rootID, scopeRef, lastErr)
	return beads.Bead{}, lastErr
}

func resolveScopeBodyOnce(store beads.Store, rootID, scopeRef string) (beads.Bead, error) {
	if bead, ok, err := resolveScopeBodyByRole(store, rootID, scopeRef, false); err != nil {
		return beads.Bead{}, err
	} else if ok {
		return bead, nil
	}
	if bead, ok, err := resolveScopeBodyByRole(store, rootID, scopeRef, true); err != nil {
		return beads.Bead{}, err
	} else if ok {
		return bead, nil
	}
	all, err := listByWorkflowRoot(store, rootID)
	if err != nil {
		return beads.Bead{}, err
	}
	if bead, ok := findScopeBody(all, rootID, scopeRef); ok {
		return bead, nil
	}
	return beads.Bead{}, fmt.Errorf("%w: scope %q not found under root %s", errScopeBodyMissing, scopeRef, rootID)
}

func resolveScopeBodyByRole(store beads.Store, rootID, scopeRef string, includeClosed bool) (beads.Bead, bool, error) {
	matches, err := beads.HandlesFor(store).Live.List(beads.ListQuery{
		Metadata: map[string]string{
			beadmeta.RootBeadIDMetadataKey: rootID,
			beadmeta.KindMetadataKey:       beadmeta.KindScope,
			beadmeta.ScopeRoleMetadataKey:  beadmeta.ScopeRoleBody,
		},
		IncludeClosed: includeClosed,
	})
	if err != nil {
		return beads.Bead{}, false, err
	}
	for _, bead := range matches {
		if matchesScopeRef(bead, scopeRef) {
			return bead, true, nil
		}
	}
	return beads.Bead{}, false, nil
}

func canSkipScopeMemberWithDeps(deps []beads.Dep, pending map[string]beads.Bead) bool {
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

type scopeSkipDepBatchLister interface {
	DepListBatch(ids []string) (map[string][]beads.Dep, error)
}

func loadDownDepsForScopeSkip(store beads.Store, ids []string) (map[string][]beads.Dep, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	if batch, ok := store.(scopeSkipDepBatchLister); ok {
		deps, err := batch.DepListBatch(ids)
		if err != nil {
			return nil, fmt.Errorf("batch listing scope skip deps: %w", err)
		}
		if deps == nil {
			deps = make(map[string][]beads.Dep, len(ids))
		}
		return deps, nil
	}
	depsByID := make(map[string][]beads.Dep, len(ids))
	for _, id := range ids {
		deps, err := store.DepList(id, "down")
		if err != nil {
			return nil, fmt.Errorf("listing deps for scope skip bead %q: %w", id, err)
		}
		depsByID[id] = deps
	}
	return depsByID, nil
}

type scopeSkipBatchUpdater interface {
	UpdateAll(ids []string, opts beads.UpdateOpts) (int, error)
}

func skipScopeMembers(store beads.Store, ids []string) (int, error) {
	status := "closed"
	opts := beads.UpdateOpts{
		Status:   &status,
		Metadata: map[string]string{beadmeta.OutcomeMetadataKey: beadmeta.OutcomeSkipped},
	}
	if batch, ok := store.(scopeSkipBatchUpdater); ok {
		updated, err := batch.UpdateAll(ids, opts)
		if err != nil {
			return updated, fmt.Errorf("closing skipped scope beads %v: %w", ids, err)
		}
		return updated, nil
	}
	closed := 0
	for _, id := range ids {
		if err := store.Update(id, opts); err != nil {
			return closed, fmt.Errorf("closing bead %q: %w", id, err)
		}
		closed++
	}
	return closed, nil
}

func sortedPendingIDs(pending map[string]beads.Bead) []string {
	ids := make([]string, 0, len(pending))
	for id := range pending {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

func listByWorkflowRoot(store beads.Store, rootID string) ([]beads.Bead, error) {
	all, err := beads.HandlesFor(store).Live.List(beads.ListQuery{
		Metadata:      map[string]string{beadmeta.RootBeadIDMetadataKey: rootID},
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
	if candidate.Metadata[beadmeta.LogicalBeadIDMetadataKey] == logical.ID {
		return true
	}
	for _, prefix := range []string{".run.", ".eval.", ".check.", ".iteration.", ".attempt."} {
		if strings.HasPrefix(candidate.ID, logical.ID+prefix) {
			return true
		}
	}
	logicalRef := strings.TrimSpace(logical.Metadata[beadmeta.StepRefMetadataKey])
	if logicalRef == "" {
		logicalRef = strings.TrimSpace(logical.Ref)
	}
	if logicalRef == "" {
		return false
	}
	candidateRef := strings.TrimSpace(candidate.Metadata[beadmeta.StepRefMetadataKey])
	if candidateRef == "" {
		candidateRef = strings.TrimSpace(candidate.Ref)
	}
	for _, prefix := range []string{".run.", ".eval.", ".check.", ".iteration.", ".attempt."} {
		if strings.HasPrefix(candidateRef, logicalRef+prefix) {
			return true
		}
	}
	if logicalStepID := strings.TrimSpace(logical.Metadata[beadmeta.StepIDMetadataKey]); logicalStepID != "" {
		if strings.TrimSpace(candidate.Metadata[beadmeta.RalphStepIDMetadataKey]) == logicalStepID {
			return true
		}
	}
	return false
}

func findScopeBody(all []beads.Bead, rootID, scopeRef string) (beads.Bead, bool) {
	for _, bead := range all {
		if bead.Metadata[beadmeta.RootBeadIDMetadataKey] != rootID {
			continue
		}
		if bead.Metadata[beadmeta.KindMetadataKey] != beadmeta.KindScope {
			continue
		}
		if matchesScopeRef(bead, scopeRef) {
			return bead, true
		}
	}
	return beads.Bead{}, false
}

func setOutcomeAndClose(store beads.Store, beadID, outcome string) error {
	return updateMetadataAndClose(store, beadID, map[string]string{beadmeta.OutcomeMetadataKey: outcome})
}

// ReconcileClosedScopeMember re-reads a just-closed bead and delegates to
// scope reconciliation. Callers invoke it immediately after
// setOutcomeAndClose, so this relies on the store being read-after-write
// consistent (true for MemStore today). If a future store becomes eventually
// consistent, pass the in-memory closed bead directly instead of re-reading.
func ReconcileClosedScopeMember(store beads.Store, beadID string) (ControlResult, error) {
	return reconcileClosedScopeMember(store, beadID)
}

func reconcileClosedScopeMember(store beads.Store, beadID string) (ControlResult, error) {
	return reconcileClosedScopeMemberWithOptions(store, beadID, ProcessOptions{})
}

func reconcileClosedScopeMemberWithOptions(store beads.Store, beadID string, opts ProcessOptions) (ControlResult, error) {
	closedBead, err := store.Get(beadID)
	if err != nil {
		return ControlResult{}, fmt.Errorf("%s: reloading closed scoped member: %w", beadID, err)
	}
	if closedBead.Status != "closed" {
		return ControlResult{}, nil
	}
	return reconcileTerminalScopedMemberWithOptions(store, closedBead, opts)
}

func matchesScopeRef(bead beads.Bead, scopeRef string) bool {
	if scopeRef == "" {
		return false
	}
	if bead.Metadata[beadmeta.ScopeRefMetadataKey] == scopeRef {
		return true
	}
	stepRef := bead.Metadata[beadmeta.StepRefMetadataKey]
	return stepRef == scopeRef || strings.HasSuffix(stepRef, "."+scopeRef)
}

func resolveFinalizeOutcome(store beads.Store, finalizer beads.Bead) (string, error) {
	outcome, err := resolveBlockedOutcome(store, finalizer.ID)
	if err != nil {
		return "", err
	}
	rootID := strings.TrimSpace(finalizer.Metadata[beadmeta.RootBeadIDMetadataKey])
	if outcome == beadmeta.OutcomePass && rootID != "" {
		failed, err := workflowRootHasTerminalAbortScopeFailure(store, rootID, finalizer.ID)
		if err != nil {
			return "", err
		}
		if failed {
			outcome = beadmeta.OutcomeFail
		}
	}
	return outcome, nil
}

func resolveBlockedOutcome(store beads.Store, beadID string) (string, error) {
	deps, err := store.DepList(beadID, "down")
	if err != nil {
		return "", err
	}
	outcome := beadmeta.OutcomePass
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
		if beadOutcomeFailed(blocker) {
			outcome = beadmeta.OutcomeFail
		}
	}
	return outcome, nil
}

func workflowRootHasTerminalAbortScopeFailure(store beads.Store, rootID, finalizerID string) (bool, error) {
	all, err := listByWorkflowRoot(store, rootID)
	if err != nil {
		return false, err
	}
	for _, candidate := range all {
		if candidate.ID == finalizerID {
			continue
		}
		if terminalAbortScopeFailure(candidate) {
			return true, nil
		}
	}
	return false, nil
}

func terminalAbortScopeFailure(bead beads.Bead) bool {
	if bead.Status != "closed" {
		return false
	}
	if strings.TrimSpace(bead.Metadata[beadmeta.OnFailMetadataKey]) != "abort_scope" {
		return false
	}
	if !beadOutcomeFailed(bead) {
		return false
	}
	switch strings.TrimSpace(bead.Metadata[beadmeta.FailureClassMetadataKey]) {
	case beadmeta.FailureClassTransient:
		return false
	case beadmeta.FailureClassHard:
		return true
	default:
		return !isRetryAttemptSubject(bead)
	}
}
