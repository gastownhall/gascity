package api

import (
	"context"
	"errors"
	"net/url"
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
)

const runCensusPartialReason = "run projection is incomplete"

// RunCensusSource serves canonical counts from an incremental per-city
// projector. The bool is false when the requested city is unknown to the
// source.
type RunCensusSource interface {
	RunCensus(context.Context, string) (runproj.CanonicalRunCensus, bool)
}

// The run list/get/steps reads are served from a server-owned per-city warm
// projector (runs_projector.go): one asynchronous cold replay off the request
// path, then an incremental byte-offset tail of only newly appended events. So a
// request serves a warm read instead of re-replaying the whole history on every
// poll. It is independent of the optional census projector (RunCensusSource),
// which only serves counts and only when the dashboard is mounted.

// humaHandleRunsList is the Huma-typed handler for GET /v0/city/{cityName}/runs.
// It lists every run in the city (active, then waiting/blocked, then historical),
// newest activity first, capped by limit.
func (s *Server) humaHandleRunsList(ctx context.Context, input *RunsListInput) (*RunsListOutput, error) {
	snap, err := s.runProjection(ctx)
	if err != nil {
		return nil, runProjectionUnavailable(err)
	}
	summary, censusLanes := runproj.BuildRunSummaryWithAllLanes(snap.beads)
	byID := beadsByID(snap.beads)
	startedByRun := countStartedMembersByRun(snap.beads, censusLanes)

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
		runproj.CountCanonicalRunStatuses(snap.beads, censusLanes),
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
	if snap.decodeMisses > 0 {
		out.Body.Partial = true
		out.Body.PartialErrors = append(out.Body.PartialErrors,
			"some run events could not be decoded; the list may be incomplete")
	}
	// A cold replay still in flight (first warm-up or a post-rotation reset) means
	// the list may not yet reflect every run: report it rather than serve a
	// possibly-empty view as if it were complete.
	if !snap.ready || snap.refreshing {
		out.Body.Partial = true
		out.Body.PartialErrors = append(out.Body.PartialErrors,
			"run view is warming; the list may be incomplete")
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
	snap, err := s.runProjection(ctx)
	if err != nil {
		return nil, runProjectionUnavailable(err)
	}
	lane, ok := runproj.BuildRunLane(snap.beads, input.RunID)
	if !ok {
		return nil, runNotFoundOrWarming(snap, input.RunID)
	}
	return &RunGetOutput{Body: laneToRun(lane, beadsByID(snap.beads), countStartedMembers(snap.beads, lane.ID))}, nil
}

// humaHandleRunSteps is the Huma-typed handler for
// GET /v0/city/{cityName}/runs/{run_id}/steps. Steps are the run's member beads
// (the root's children), each projected to a closed RunStepStatus.
func (s *Server) humaHandleRunSteps(ctx context.Context, input *RunStepsInput) (*RunStepsOutput, error) {
	snap, err := s.runProjection(ctx)
	if err != nil {
		return nil, runProjectionUnavailable(err)
	}
	if _, ok := runproj.BuildRunLane(snap.beads, input.RunID); !ok {
		return nil, runNotFoundOrWarming(snap, input.RunID)
	}

	members := runMemberBeads(snap.beads, input.RunID)
	out := &RunStepsOutput{}
	out.Body.RunID = input.RunID
	out.Body.Steps = make([]RunStep, 0, len(members))
	for i := range members {
		m := members[i]
		if m.ID == input.RunID {
			continue // the root is the run, not a step
		}
		out.Body.Steps = append(out.Body.Steps, RunStep{
			ID:       m.ID,
			Title:    runStepTitle(m),
			Status:   deriveRunStepStatus(m),
			Kind:     m.Type,
			Assignee: strings.TrimSpace(m.Assignee),
		})
	}
	return out, nil
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
	return apierr.ServiceUnavailable.Msgf("run projection unavailable: %v", err)
}

// runNotFoundOrWarming maps a run absent from the projection to the honest
// status. While a cold replay is still in flight (first warm-up or a
// post-rotation reset) the fold may be incomplete, so a run that may yet appear
// is a retryable 503 rather than a terminal 404. Once the projection is warm and
// settled, an absent run is a definitive 404.
func runNotFoundOrWarming(snap runSnapshot, runID string) error {
	if !snap.ready || snap.refreshing {
		return apierr.ServiceUnavailable.Msgf("run view is warming; retry shortly: %s", runID)
	}
	return apierr.RunNotFound.Msgf("run not found: %s", runID)
}
