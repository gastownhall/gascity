package api

import (
	"context"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/api/apierr"
	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/runproj"
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
}

// runFold reads the city event log, folds it into the latest bead snapshot per
// id, and keeps only run-participating beads. The result is memoized in the
// Server response cache keyed by the event log's modification time, so repeated
// polls between appends are a pure cache hit and a new append re-folds. A city
// with no event log yet yields an empty projection (a fresh city has no runs),
// not an error.
func (s *Server) runFold() (runFoldResult, error) {
	cityRoot := strings.TrimSpace(s.state.CityPath())
	if cityRoot == "" {
		return runFoldResult{}, nil
	}
	eventsPath := filepath.Join(cityRoot, ".gc", "events.jsonl")
	fi, err := os.Stat(eventsPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return runFoldResult{}, nil
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
	}
	s.storeResponse(key, index, res)
	return res, nil
}

// humaHandleRunsList is the Huma-typed handler for GET /v0/city/{cityName}/runs.
// It lists every run in the city (active, then waiting/blocked, then historical),
// newest activity first, capped by limit.
func (s *Server) humaHandleRunsList(_ context.Context, input *RunsListInput) (*RunsListOutput, error) {
	fold, err := s.runFold()
	if err != nil {
		return nil, runProjectionUnavailable(err)
	}
	summary := runproj.BuildRunSummary(fold.beads)
	byID := beadsByID(fold.beads)

	limit := normalizeRunsListLimit(input.Limit)
	lanes := allRunLanes(summary)

	out := &RunsListOutput{}
	out.Body.Runs = make([]Run, 0, len(lanes))
	for i := range lanes {
		if len(out.Body.Runs) >= limit {
			break
		}
		out.Body.Runs = append(out.Body.Runs, laneToRun(lanes[i], byID, fold.beads))
	}

	// Do not silently hide incompleteness: the projection caps the historical
	// lane list, the caller-supplied limit can drop runs, and a corrupt event
	// line can drop a run from the fold entirely.
	if summary.TotalHistorical > len(summary.HistoricalLanes) || len(lanes) > len(out.Body.Runs) {
		out.Body.Partial = true
		out.Body.PartialErrors = append(out.Body.PartialErrors,
			"run list truncated; older runs are not shown")
	}
	if fold.decodeMisses > 0 {
		out.Body.Partial = true
		out.Body.PartialErrors = append(out.Body.PartialErrors,
			"some run events could not be decoded; the list may be incomplete")
	}
	return out, nil
}

// humaHandleRunGet is the Huma-typed handler for
// GET /v0/city/{cityName}/runs/{run_id}. It resolves the single run off the fold
// via BuildRunLane, so a completed run beyond the list's historical cap is still
// retrievable (no false 404).
func (s *Server) humaHandleRunGet(_ context.Context, input *RunGetInput) (*RunGetOutput, error) {
	fold, err := s.runFold()
	if err != nil {
		return nil, runProjectionUnavailable(err)
	}
	lane, ok := runproj.BuildRunLane(fold.beads, input.RunID)
	if !ok {
		return nil, apierr.RunNotFound.Msgf("run not found: %s", input.RunID)
	}
	return &RunGetOutput{Body: laneToRun(lane, beadsByID(fold.beads), fold.beads)}, nil
}

// humaHandleRunSteps is the Huma-typed handler for
// GET /v0/city/{cityName}/runs/{run_id}/steps. Steps are the run's member beads
// (the root's children), each projected to a closed RunStepStatus.
func (s *Server) humaHandleRunSteps(_ context.Context, input *RunStepsInput) (*RunStepsOutput, error) {
	fold, err := s.runFold()
	if err != nil {
		return nil, runProjectionUnavailable(err)
	}
	if _, ok := runproj.BuildRunLane(fold.beads, input.RunID); !ok {
		return nil, apierr.RunNotFound.Msgf("run not found: %s", input.RunID)
	}

	members := runMemberBeads(fold.beads, input.RunID)
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

// laneToRun maps a projection lane to the canonical Run DTO, reading the run root
// bead (when present) for start time, target, and terminal outcome. The started
// member count (used only to split pending from active) is computed just for
// non-terminal runs.
func laneToRun(lane runproj.RunLane, byID map[string]beads.Bead, beadList []beads.Bead) Run {
	root, rootFound := byID[lane.ID]
	started := 0
	if !rootFound || !isClosedStatus(root.Status) {
		started = countStartedMembers(beadList, lane.ID)
	}
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
	if rootFound && isClosedStatus(root.Status) {
		switch strings.TrimSpace(root.Metadata[beadmeta.OutcomeMetadataKey]) {
		case beadmeta.OutcomeFail:
			return RunStatusFailed
		case beadmeta.OutcomeSkipped:
			return RunStatusSkipped
		}
		return RunStatusCompleted
	}
	switch lane.Phase {
	case "blocked":
		return RunStatusWaiting
	default: // active/in-flight
		if startedCount == 0 {
			return RunStatusPending
		}
		return RunStatusActive
	}
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
