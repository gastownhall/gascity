package api

import (
	"context"
	"errors"
	"log"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/api/apierr"
	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/runproj"
	"github.com/gastownhall/gascity/internal/sourceworkflow"
)

// runResourcePath is the canonical Run resource URL for one run — the value a
// launch endpoint puts in its Location header when it produced an addressable run.
func runResourcePath(cityName, runID string) string {
	return "/v0/city/" + url.PathEscape(cityName) + "/runs/" + url.PathEscape(runID)
}

// runsListPath is the runs-list URL — the Location a launch endpoint uses when it
// produced no single addressable run (order dispatch, wisps, idempotent skips).
func runsListPath(cityName string) string {
	return "/v0/city/" + url.PathEscape(cityName) + "/runs"
}

// The canonical Run resource. These handlers project the city's append-only
// event log (.gc/events.jsonl) into ONE typed run shape with a closed RunStatus
// enum, converging the several run-status vocabularies the API historically
// exposed. The event log is the source of truth (the OSS-local analog of the
// hosted run projection); the bead-store scan is deliberately NOT used here — its
// per-request per-root child lookups do not perform for a hot list endpoint.

const (
	defaultRunsListLimit = 100
	maxRunsListLimit     = 500
	// runFoldCacheKeyPrefix namespaces the per-city folded-run-bead cache entry
	// in the Server response cache.
	runFoldCacheKeyPrefix = "runs:fold:"
)

// runFoldResult is the memoized output of a fold pass: the run-participating bead
// snapshots plus the count of bead events that failed to decode (a silent
// projection starve the caller surfaces as `partial`).
type runFoldResult struct {
	beads        []beads.Bead
	decodeMisses int
	ready        bool
	partial      bool
}

const runCensusPartialReason = "run projection is incomplete"

// RunCensusSource serves canonical counts from an incremental per-city
// projector. The bool is false when the requested city is unknown to the
// source.
type RunCensusSource interface {
	RunCensus(context.Context, string) (runproj.CanonicalRunCensus, bool)
}

// RunProjectionSource serves immutable bead snapshots from an incremental
// per-city projector. Production's RunCensusSource also implements this
// capability; keeping it separate preserves the narrow census contract for
// other sources and tests.
type RunProjectionSource interface {
	RunProjection(context.Context, string) (runproj.RunProjectionSnapshot, bool)
}

// RunProjectionGraceSource owns the bounded warming window for point-read
// misses that may be valid newly-slung runs not yet visible in the event fold.
type RunProjectionGraceSource interface {
	RunProjectionMissInGrace(context.Context, string, string) bool
	ForgetRunProjectionMiss(context.Context, string, string)
}

// runFold reads the warm incremental projection when the injected census source
// provides it. Direct Server users without that capability retain the legacy
// on-disk fold, memoized by event-log modification time. A city with no event
// log yet yields a ready empty projection (a fresh city has no runs), not an
// error.
func (s *Server) runFold(ctx context.Context) (runFoldResult, error) {
	if source, ok := s.runCensusSource.(RunProjectionSource); ok {
		snapshot, found := source.RunProjection(ctx, s.state.CityName())
		if !found {
			return runFoldResult{}, errors.New("run projection source unavailable")
		}
		return runFoldResult{
			beads:        snapshot.Beads,
			decodeMisses: snapshot.DecodeMisses,
			ready:        snapshot.Ready,
			partial:      snapshot.Partial || !snapshot.Ready,
		}, nil
	}

	cityRoot := strings.TrimSpace(s.state.CityPath())
	if cityRoot == "" {
		return runFoldResult{ready: true}, nil
	}
	eventsPath := filepath.Join(cityRoot, ".gc", "events.jsonl")
	fi, err := os.Stat(eventsPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return runFoldResult{ready: true}, nil
		}
		return runFoldResult{}, err
	}

	index := uint64(fi.ModTime().UnixNano())
	key := runFoldCacheKeyPrefix + s.state.CityName()
	if cached, ok := s.cachedResponse(key, index); ok {
		if res, ok := cached.(runFoldResult); ok {
			return res, nil
		}
	}

	proj := runproj.NewProjector()
	if err := proj.ColdLoad(eventsPath); err != nil {
		return runFoldResult{}, err
	}
	res := runFoldResult{
		beads:        runproj.FilterRunBeads(proj.Beads()),
		decodeMisses: proj.DecodeMisses(),
		ready:        true,
		partial:      proj.DecodeMisses() > 0,
	}
	s.storeResponse(key, index, res)
	return res, nil
}

// humaHandleRunsList is the Huma-typed handler for GET /v0/city/{cityName}/runs.
// It lists every run in the city (active, then waiting/blocked, then historical),
// newest activity first, capped by limit.
func (s *Server) humaHandleRunsList(ctx context.Context, input *RunsListInput) (*RunsListOutput, error) {
	fold, err := s.runFold(ctx)
	if err != nil {
		return nil, runProjectionUnavailable(err)
	}
	summary, censusLanes := runproj.BuildRunSummaryWithAllLanes(fold.beads)
	byID := beadsByID(fold.beads)
	startedByRun := countStartedMembersByRun(fold.beads, censusLanes)

	limit := normalizeRunsListLimit(input.Limit)
	lanes := allRunLanes(summary)
	rowCount := min(limit, len(lanes))
	projected := make([]Run, 0, rowCount)
	for i := range rowCount {
		lane := lanes[i]
		projected = append(projected, laneToRun(lane, byID, startedByRun[lane.ID]))
	}

	out := &RunsListOutput{}
	out.Body.StatusCounts = runStatusCountsFromProjection(
		runproj.CountCanonicalRunStatuses(fold.beads, censusLanes),
	)
	out.Body.Runs = projected

	// Do not silently hide incompleteness: the projection caps the historical
	// lane list, the caller-supplied limit can drop runs, and a corrupt event
	// line can drop a run from the fold entirely.
	if summary.TotalHistorical > len(summary.HistoricalLanes) || len(lanes) > len(out.Body.Runs) {
		out.Body.Partial = true
		out.Body.PartialErrors = append(out.Body.PartialErrors,
			"run list truncated; older runs are not shown")
	}
	if !fold.ready {
		out.Body.Partial = true
		out.Body.PartialErrors = append(out.Body.PartialErrors,
			"run projection is warming")
	} else if fold.partial && fold.decodeMisses == 0 {
		out.Body.Partial = true
		out.Body.PartialErrors = append(out.Body.PartialErrors,
			runCensusPartialReason)
	}
	if fold.decodeMisses > 0 {
		out.Body.Partial = true
		out.Body.PartialErrors = append(out.Body.PartialErrors,
			"some run events could not be decoded; the list may be incomplete")
	}
	return out, nil
}

func (s *Server) humaHandleRunsCensus(ctx context.Context, input *RunsCensusInput) (*RunsCensusOutput, error) {
	if s.runCensusSource == nil {
		return nil, apierr.ServiceUnavailable.Msg("run census is unavailable")
	}
	census, ok := s.runCensusSource.RunCensus(ctx, input.CityName)
	if !ok {
		return nil, apierr.ServiceUnavailable.Msg("run census is unavailable")
	}
	if !census.Ready {
		return nil, apierr.ServiceUnavailable.Msg("run census is warming")
	}
	out := &RunsCensusOutput{}
	out.Body.StatusCounts = runStatusCountsFromProjection(census.StatusCounts)
	out.Body.Partial = census.Partial
	if census.Partial {
		// The source may carry local diagnostics. The public typed endpoint exposes
		// only this closed, operator-safe reason so paths and error prose never leak.
		out.Body.PartialErrors = []string{runCensusPartialReason}
	}
	return out, nil
}

func runStatusCountsFromProjection(counts runproj.CanonicalRunStatusCounts) RunStatusCounts {
	return RunStatusCounts{
		Pending: counts.Pending, Active: counts.Active, Waiting: counts.Waiting,
		Canceling: counts.Canceling, Completed: counts.Completed, Failed: counts.Failed,
		Canceled: counts.Canceled, Skipped: counts.Skipped,
	}
}

// humaHandleRunGet is the Huma-typed handler for
// GET /v0/city/{cityName}/runs/{run_id}. It resolves the single run off the fold
// via BuildRunLane, so a completed run beyond the list's historical cap is still
// retrievable (no false 404).
func (s *Server) humaHandleRunGet(ctx context.Context, input *RunGetInput) (*RunGetOutput, error) {
	fold, err := s.runFold(ctx)
	if err != nil {
		return nil, runProjectionUnavailable(err)
	}
	if !fold.ready {
		return nil, apierr.ServiceUnavailable.Msg("run projection is warming")
	}
	lane, ok := runproj.BuildRunLane(fold.beads, input.RunID)
	if !ok {
		if fold.partial {
			return nil, apierr.ServiceUnavailable.Msg("run projection is incomplete")
		}
		if s.runProjectionMissInGrace(ctx, input.RunID) {
			return nil, apierr.ServiceUnavailable.Msg("run projection is warming")
		}
		return nil, apierr.RunNotFound.Msgf("run not found: %s", input.RunID)
	}
	s.forgetRunProjectionMiss(ctx, input.RunID)
	return &RunGetOutput{Body: laneToRun(lane, beadsByID(fold.beads), countStartedMembers(fold.beads, lane.ID))}, nil
}

// humaHandleRunSteps is the Huma-typed handler for
// GET /v0/city/{cityName}/runs/{run_id}/steps. Steps are the run's member beads
// (the root's children), each projected to a closed RunStepStatus.
func (s *Server) humaHandleRunSteps(ctx context.Context, input *RunStepsInput) (*RunStepsOutput, error) {
	fold, err := s.runFold(ctx)
	if err != nil {
		return nil, runProjectionUnavailable(err)
	}
	if !fold.ready {
		return nil, apierr.ServiceUnavailable.Msg("run projection is warming")
	}
	lane, ok := runproj.BuildRunLane(fold.beads, input.RunID)
	if !ok {
		if fold.partial {
			return nil, apierr.ServiceUnavailable.Msg("run projection is incomplete")
		}
		if s.runProjectionMissInGrace(ctx, input.RunID) {
			return nil, apierr.ServiceUnavailable.Msg("run projection is warming")
		}
		return nil, apierr.RunNotFound.Msgf("run not found: %s", input.RunID)
	}
	s.forgetRunProjectionMiss(ctx, input.RunID)

	// Derive the run's canonical lifecycle status exactly as laneToRun/deriveRunStatus
	// do (root-terminality wins over lingering members), then clamp each step through
	// it: a completed run must not report a step as eternally active when its close
	// event was lost. A non-terminal run yields an inactive clamp (raw statuses stand).
	byID := beadsByID(fold.beads)
	root, rootFound := byID[input.RunID]
	var rootPtr *beads.Bead
	if rootFound {
		rootPtr = &root
	}
	runStatus := runproj.CanonicalRunStatusForLane(lane, rootPtr, countStartedMembers(fold.beads, lane.ID))

	members := runMemberBeads(fold.beads, input.RunID)
	out := &RunStepsOutput{}
	out.Body.RunID = input.RunID
	out.Body.Steps = make([]RunStep, 0, len(members))
	for i := range members {
		m := members[i]
		if m.ID == input.RunID {
			continue // the root is the run, not a step
		}
		status := RunStepStatus(runproj.ClampStepStatusForRun(runStatus, string(deriveRunStepStatus(m))))
		out.Body.Steps = append(out.Body.Steps, RunStep{
			ID:       m.ID,
			Title:    runStepTitle(m),
			Status:   status,
			Kind:     m.Type,
			Assignee: strings.TrimSpace(m.Assignee),
		})
	}
	return out, nil
}

func (s *Server) runProjectionMissInGrace(ctx context.Context, runID string) bool {
	source, ok := s.runCensusSource.(RunProjectionGraceSource)
	return ok && source.RunProjectionMissInGrace(ctx, s.state.CityName(), runID)
}

func (s *Server) forgetRunProjectionMiss(ctx context.Context, runID string) {
	if source, ok := s.runCensusSource.(RunProjectionGraceSource); ok {
		source.ForgetRunProjectionMiss(ctx, s.state.CityName(), runID)
	}
}

// runCanceledCloseReason is the close_reason stamped on beads wound down by a run
// cancel, distinguishing an operator cancel from a skip-directive teardown.
const runCanceledCloseReason = "run canceled via POST /runs/{id}/cancel"

// humaHandleRunCancel is the Huma-typed handler for
// POST /v0/city/{cityName}/runs/{run_id}/cancel. It stamps cancel intent on the
// run root and synchronously winds the run down — closing the root and its open
// beads with a canceled outcome so the dispatcher finds no more ready work and
// the run starves. Registered with a 202 default status: in-flight sessions
// finish their current step before idling, so the wind-down is accepted, not
// instantaneous.
func (s *Server) humaHandleRunCancel(_ context.Context, input *RunCancelInput) (*RunCancelOutput, error) {
	runID := strings.TrimSpace(input.RunID)
	res, err := s.cancelRun(runID)
	if err != nil {
		// A store read/write failed mid-cancel: do NOT report a cancellation we
		// could not substantiate. 503 is retryable; the run keeps running.
		return nil, apierr.ServiceUnavailable.Msgf("run cancel failed: %v", err)
	}
	if !res.found {
		return nil, apierr.RunNotFound.Msgf("run not found: %s", runID)
	}
	if res.terminal {
		return nil, apierr.ConflictWrongState.Msgf("run %s is already terminal; nothing to cancel", runID)
	}
	out := &RunCancelOutput{}
	out.Body.RunID = runID
	out.Body.Status = res.status
	out.Body.Closed = res.closed
	return out, nil
}

// cancelRunResult is the outcome of a cancelRun wind-down.
type cancelRunResult struct {
	found    bool      // a workflow run root with this id exists
	terminal bool      // every matching root was already terminal (nothing to cancel)
	closed   int       // beads newly closed by the cancel
	status   RunStatus // resulting run status
}

// cancelRun winds down the run rooted at runID across every workflow store,
// closing each open root and its still-OPEN member beads with a canceled
// outcome. It reuses the workflow teardown close ordering (descendants before
// the root, blockers before blocked) so a strict store accepts the batch. The
// cancel-intent marker is stamped root-only, together with the root's own close:
// on an atomic store the two commit as one transaction, so a failed close
// persists neither and never strands an open, half-marked root; on a non-atomic
// store the marker is durably recorded so the returned 5xx's retry completes the
// wind-down. Already-terminal runs (and already-closed members) are left
// untouched — closing a completed member would rewrite its recorded outcome. Any
// store read/write failure is returned so the caller reports a 5xx rather than a
// phantom success.
func (s *Server) cancelRun(runID string) (cancelRunResult, error) {
	var res cancelRunResult
	for _, info := range s.workflowStores() {
		if info.store == nil {
			continue
		}
		roots, err := findWorkflowRoots(info.store, runID)
		if err != nil {
			return res, err
		}
		for _, root := range roots {
			res.found = true
			if isClosedStatus(root.Status) {
				continue // already terminal — nothing to wind down
			}
			n, err := sourceworkflow.CloseWorkflowSubtreeAs(
				info.store,
				root.ID,
				beadmeta.OutcomeCanceled,
				runCanceledCloseReason,
				map[string]string{beadmeta.CancelRequestedMetadataKey: "true"},
			)
			if err != nil {
				return res, err
			}
			res.closed += n
			res.status = RunStatusCanceled
		}
	}
	if res.found && res.status == "" {
		res.terminal = true
	}
	return res, nil
}

// findWorkflowRoots returns the graph-workflow roots in store that match runID
// (Get by id + List by kind=workflow/workflow_id). Root membership uses
// sourceworkflow.IsWorkflowRoot (gc.kind=workflow OR gc.formula_contract=graph.v2)
// so a graph.v2-only root — one the Run resource lists but that lacks the
// gc.kind=workflow marker — is still cancellable, not a false 404. A non-NotFound
// store error is returned so the caller reports a 5xx.
func findWorkflowRoots(store beads.Store, runID string) ([]beads.Bead, error) {
	var roots []beads.Bead
	seen := map[string]bool{}
	add := func(root beads.Bead) {
		if !sourceworkflow.IsWorkflowRoot(root) || !matchesWorkflowID(root, runID) {
			return
		}
		if seen[root.ID] {
			return
		}
		seen[root.ID] = true
		roots = append(roots, root)
	}
	if root, err := store.Get(runID); err == nil {
		add(root)
	} else if !errors.Is(err, beads.ErrNotFound) {
		return nil, err
	}
	list, err := store.List(beads.ListQuery{
		Metadata: map[string]string{
			beadmeta.KindMetadataKey:       beadmeta.KindWorkflow,
			beadmeta.WorkflowIDMetadataKey: runID,
		},
		IncludeClosed: true,
	})
	if err != nil {
		return nil, err
	}
	for _, r := range list {
		add(r)
	}
	return roots, nil
}

// laneToRun maps a projection lane to the canonical Run DTO, reading the run root
// bead (when present) for start time, target, and terminal outcome. The started
// member count (used only to split pending from active) is computed just for
// non-terminal runs.
func laneToRun(lane runproj.RunLane, byID map[string]beads.Bead, started int) Run {
	root, rootFound := byID[lane.ID]
	run := Run{
		RunID:  lane.ID,
		Title:  lane.Title,
		Status: deriveRunStatus(lane, root, rootFound, started),
	}
	if lane.Formula.Status == "known" {
		run.Formula = lane.Formula.Name
	}
	if lane.Scope.Status == "available" {
		run.Scope = RunScope{Kind: lane.Scope.Kind, Ref: lane.Scope.Ref}
	}
	if lane.UpdatedAt.Status == "available" {
		run.UpdatedAt = lane.UpdatedAt.At
	}
	if rootFound {
		if !root.CreatedAt.IsZero() {
			run.StartedAt = root.CreatedAt.UTC().Format(time.RFC3339)
		}
		run.Target = workflowProjectionTarget(root)
		run.LastError = runLastError(run.Status, root)
	}
	return run
}

// deriveRunStatus is the single site that maps a run onto the closed RunStatus
// enum. Terminal status is authoritative from the run ROOT's own closure — not
// from lane phase, which requires every grouped bead (including lingering open
// source beads) to close and would otherwise report a failed run as active
// indefinitely. Extending run lifecycle (cancellation) grows this function;
// nothing else interprets run status.
func deriveRunStatus(lane runproj.RunLane, root beads.Bead, rootFound bool, startedCount int) RunStatus {
	var rootPtr *beads.Bead
	if rootFound {
		rootPtr = &root
	}
	return RunStatus(runproj.CanonicalRunStatusForLane(lane, rootPtr, startedCount))
}

// deriveRunStepStatus maps one run-step (child bead) onto the closed
// RunStepStatus enum from its bead status and terminal outcome.
func deriveRunStepStatus(b beads.Bead) RunStepStatus {
	switch strings.TrimSpace(b.Status) {
	case "closed":
		switch strings.TrimSpace(b.Metadata[beadmeta.OutcomeMetadataKey]) {
		case beadmeta.OutcomeFail:
			return RunStepStatusFailed
		case beadmeta.OutcomeSkipped:
			return RunStepStatusSkipped
		case beadmeta.OutcomeCanceled:
			return RunStepStatusCanceled
		}
		return RunStepStatusCompleted
	case "in_progress":
		return RunStepStatusActive
	case "blocked":
		return RunStepStatusBlocked
	default:
		return RunStepStatusPending
	}
}

// runLastError returns the structured failure reason for a terminal, non-success
// run, or nil otherwise. The code prefers the actionable graph failure reason
// (gc.failure_reason, e.g. "rate_limited") that control/drain stamp on a failed
// root, so clients get the stable code they can branch on rather than the coarse
// gc.outcome; it falls back to that outcome and finally the status. The message
// carries the controller's human-readable error (gc.controller_error) when
// present, else a close-reason marker.
func runLastError(status RunStatus, root beads.Bead) *RunLastError {
	if status != RunStatusFailed && status != RunStatusCanceled {
		return nil
	}
	code := strings.TrimSpace(root.Metadata[beadmeta.FailureReasonMetadataKey])
	if code == "" {
		code = strings.TrimSpace(root.Metadata[beadmeta.OutcomeMetadataKey])
	}
	if code == "" {
		code = string(status)
	}
	message := strings.TrimSpace(root.Metadata[beadmeta.ControllerErrorMetadataKey])
	if message == "" {
		message = strings.TrimSpace(root.Metadata["close_reason"])
	}
	return &RunLastError{
		Code:    code,
		Message: message,
	}
}

// runMemberBeads returns a run's member beads: the runproj membership (root id,
// parent, gc.root_bead_id, dotted prefix) plus v1/wisp members tagged only by
// gc.molecule_id. Both the steps endpoint and the started-work count use it, so
// the two never disagree about what belongs to a run.
func runMemberBeads(beadList []beads.Bead, rootID string) []beads.Bead {
	if rootID == "" {
		return nil
	}
	members := runproj.RunMembers(beadList, rootID)
	seen := make(map[string]bool, len(members))
	for i := range members {
		seen[members[i].ID] = true
	}
	for i := range beadList {
		b := beadList[i]
		if seen[b.ID] {
			continue
		}
		if strings.TrimSpace(b.Metadata[beadmeta.MoleculeIDMetadataKey]) == rootID {
			members = append(members, b)
			seen[b.ID] = true
		}
	}
	return members
}

// countStartedMembers counts a run's member beads that have started work
// (in-progress or closed), excluding the root. A zero count on a non-terminal run
// means the run is pending; a positive count means it is active.
func countStartedMembers(beadList []beads.Bead, rootID string) int {
	n := 0
	for _, m := range runMemberBeads(beadList, rootID) {
		if m.ID == rootID {
			continue
		}
		if runStepStarted(m.Status) {
			n++
		}
	}
	return n
}

// countStartedMembersByRun indexes started membership for every projected run
// in one pass. A bead may match more than one nested dotted root; candidates
// are de-duplicated per bead to mirror runMemberBeads exactly without an
// O(runs*beads) scan on the polled list endpoint.
func countStartedMembersByRun(beadList []beads.Bead, lanes []runproj.RunLane) map[string]int {
	roots := make(map[string]struct{}, len(lanes))
	counts := make(map[string]int, len(lanes))
	for _, lane := range lanes {
		roots[lane.ID] = struct{}{}
		counts[lane.ID] = 0
	}
	for _, bead := range beadList {
		if !runStepStarted(bead.Status) {
			continue
		}
		candidates := make(map[string]struct{}, 4)
		for _, rootID := range []string{
			bead.ParentID,
			bead.Metadata[beadmeta.RootBeadIDMetadataKey],
			strings.TrimSpace(bead.Metadata[beadmeta.MoleculeIDMetadataKey]),
		} {
			if _, ok := roots[rootID]; ok {
				candidates[rootID] = struct{}{}
			}
		}
		for offset, char := range bead.ID {
			if char != '.' {
				continue
			}
			if rootID := bead.ID[:offset]; rootID != "" {
				if _, ok := roots[rootID]; ok {
					candidates[rootID] = struct{}{}
				}
			}
		}
		for rootID := range candidates {
			if bead.ID != rootID {
				counts[rootID]++
			}
		}
	}
	return counts
}

func runStepStarted(status string) bool {
	s := strings.TrimSpace(status)
	return s == "in_progress" || s == "closed"
}

func isClosedStatus(status string) bool {
	return strings.TrimSpace(status) == "closed"
}

// beadsByID indexes a bead slice by id for O(1) root lookup.
func beadsByID(beadList []beads.Bead) map[string]beads.Bead {
	byID := make(map[string]beads.Bead, len(beadList))
	for i := range beadList {
		byID[beadList[i].ID] = beadList[i]
	}
	return byID
}

// allRunLanes unions the projection's lane buckets into one list, active first,
// then waiting/blocked, then historical (each already newest-first).
func allRunLanes(summary runproj.RunSummary) []runproj.RunLane {
	lanes := make([]runproj.RunLane, 0, len(summary.Lanes)+len(summary.BlockedLanes)+len(summary.HistoricalLanes))
	lanes = append(lanes, summary.Lanes...)
	lanes = append(lanes, summary.BlockedLanes...)
	lanes = append(lanes, summary.HistoricalLanes...)
	return lanes
}

func runStepTitle(b beads.Bead) string {
	if t := strings.TrimSpace(b.Title); t != "" {
		return t
	}
	return b.ID
}

func normalizeRunsListLimit(limit int) int {
	if limit <= 0 {
		return defaultRunsListLimit
	}
	if limit > maxRunsListLimit {
		return maxRunsListLimit
	}
	return limit
}

// runProjectionUnavailable wraps a fold/read failure as a 503 — reading the event
// log is a backend availability concern the caller can retry.
func runProjectionUnavailable(err error) error {
	log.Printf("gc api: run projection unavailable: %v", err)
	return apierr.ServiceUnavailable.Msg("run projection unavailable")
}
