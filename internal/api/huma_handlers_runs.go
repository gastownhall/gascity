package api

import (
	"context"
	"errors"
	"log"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/gastownhall/gascity/internal/api/apierr"
	"github.com/gastownhall/gascity/internal/api/dashboardbff"
	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/molecule"
	"github.com/gastownhall/gascity/internal/runproj"
	"github.com/gastownhall/gascity/internal/session"
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

// runProjectionFreshness is the freshness contract every run read documents.
const runProjectionFreshness = "Run reads are a projection of the city event log, not a direct store read. " +
	"A change the supervisor makes (sling, cancel, its own step writes) is in the log when the write returns and reaches these reads on the projection's next poll, normally within about a second; " +
	"a successful POST /runs/{run_id}/cancel is reflected before its response. " +
	"A worker's `gc hook --claim` (the claim and the hook's own follow-up writes) and `gc bd close` record their changes in the log before the command returns, and these reads pick them up on the next poll. " +
	"No fixed bound is promised: on a slow store the recording itself takes store reads, and claims were measured visible 1 to 8 seconds after they landed. " +
	"Any other change another process writes straight to the bead store (such as a `bd close` or `bd update` run without gc), or one of those changes the worker could not record, reaches the log, and these reads, " +
	"when the supervisor first reads it back from the store: a live read of the run's steps, or its periodic store reconcile (normally every 30 to 120 seconds, by store size and latency). " +
	"From then on these reads agree with GET /bead/{id}, which serves the same supervisor view."

// RunProjectionSyncSource folds everything already appended to the city's
// event log into the warm projection before it returns. A handler that just
// wrote run state through the supervisor's cache — whose bead.* events are
// recorded synchronously — calls it so the caller's next point read reflects
// that write (read-your-writes) instead of waiting for the next poll. The bool
// is false when the projection could not be synced (unknown city, tailer not
// running, ctx done); the projection then catches up on its own.
type RunProjectionSyncSource interface {
	SyncRunProjection(context.Context, string) bool
}

// The dashboard plane is production's run projection source; a source that
// silently stopped satisfying the sync capability would quietly drop the
// cancel's read-your-writes, so pin it at compile time.
var _ interface {
	RunProjectionSource
	RunProjectionGraceSource
	RunProjectionSyncSource
} = (*dashboardbff.Plane)(nil)

// runCancelProjectionSyncTimeout bounds how long a successful cancel waits for
// the projection to fold its events. The cancel has already committed, so
// running out only forfeits read-your-writes for this one response.
const runCancelProjectionSyncTimeout = 5 * time.Second

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
	// it: a terminal run must not report a step as eternally active. A clamped
	// step reads the status dispatch closes it with (canceled under a canceled
	// run, skipped otherwise), so it never flips between terminal statuses. A step
	// that executes after the root settles (the workflow finalizer, the teardown
	// tail) is not clamped: it has not closed yet, and presenting it skipped would
	// be contradicted by its own close. A non-terminal run yields an inactive clamp
	// (raw statuses stand).
	byID := beadsByID(fold.beads)
	root, rootFound := byID[input.RunID]
	var rootPtr *beads.Bead
	if rootFound {
		rootPtr = &root
	}
	runStatus := runproj.CanonicalRunStatusForLane(lane, rootPtr, countStartedMembers(fold.beads, lane.ID))

	members := topoSortRunSteps(runMemberBeads(fold.beads, input.RunID))
	clamp := runproj.NewRunStepClamp(runStatus, members)
	out := &RunStepsOutput{}
	out.Body.RunID = input.RunID
	out.Body.Steps = make([]RunStep, 0, len(members))
	for i := range members {
		m := members[i]
		if m.ID == input.RunID {
			continue // the root is the run, not a step
		}
		status := RunStepStatus(clamp.Apply(m.ID, string(deriveRunStepStatus(m))))
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
// POST /v0/city/{cityName}/runs/{run_id}/cancel. Its first write marks the run
// root canceling (gc.cancel_requested), which stops new work at once: the claim
// gate refuses the run's steps, nested drain items included, and the
// dispatcher closes its controls. It then interrupts the current turn of each
// session holding an in-progress step of the run (interruptRunHolders, the same
// interrupt as POST /session/{id}/stop; the sessions are not killed) while it
// closes the root and its open beads with a canceled outcome. The interrupts
// do not wait for the closes, which cost a store write each. Registered with a
// 202 default status: the interrupts are best-effort and a session may take a
// moment to settle, so the wind-down is accepted, not instantaneous.
func (s *Server) humaHandleRunCancel(ctx context.Context, input *RunCancelInput) (*RunCancelOutput, error) {
	runID := strings.TrimSpace(input.RunID)
	// The run is durably canceling before any interrupt starts, so a client
	// that hangs up does not get to abandon the interrupts it implies.
	interruptCtx := context.WithoutCancel(ctx)
	var (
		interrupts  sync.WaitGroup
		interrupted []string
		failures    []RunCancelInterruptFailure
	)
	res, err := s.cancelRun(runID, func(holders []string) {
		interrupts.Add(1)
		go func() {
			defer interrupts.Done()
			interrupted, failures = s.interruptRunHolders(interruptCtx, runID, holders)
		}()
	})
	// Wait out the interrupts even when a close failed: they were issued under
	// a run already marked canceling, and the response is their only report.
	interrupts.Wait()
	if err != nil {
		// A store read/write failed mid-cancel: do NOT report a cancellation we
		// could not substantiate. 503 is retryable; the run root stays open,
		// marked canceling if the mark landed, and a retry completes the close.
		return nil, apierr.ServiceUnavailable.Msgf("run cancel failed: %v", err)
	}
	if !res.found {
		return nil, apierr.RunNotFound.Msgf("run not found: %s", runID)
	}
	if res.terminal {
		return nil, apierr.ConflictWrongState.Msgf("run %s is already terminal; nothing to cancel", runID)
	}
	s.syncRunProjectionAfterWrite(ctx)
	out := &RunCancelOutput{}
	out.Body.RunID = runID
	out.Body.Status = res.status
	out.Body.Closed = res.closed
	out.Body.Interrupted = interrupted
	out.Body.InterruptFailures = failures
	return out, nil
}

// syncRunProjectionAfterWrite gives the caller read-your-writes on the run
// projection after a write this handler made through the supervisor's cache:
// the write's bead.* events are already in the event log, and this folds them
// before the response goes out. It costs no store call — one stat and a read of
// the newly appended log bytes — and is a no-op for a source that cannot sync
// (the legacy on-disk fold re-reads the log whenever it changed).
func (s *Server) syncRunProjectionAfterWrite(ctx context.Context) {
	source, ok := s.runCensusSource.(RunProjectionSyncSource)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, runCancelProjectionSyncTimeout)
	defer cancel()
	if !source.SyncRunProjection(ctx, s.state.CityName()) {
		log.Printf("gc api: run projection did not catch up after a run write; the next read may be stale until the next poll")
	}
}

// cancelRunResult is the outcome of a cancelRun wind-down.
type cancelRunResult struct {
	found    bool      // a workflow run root with this id exists
	terminal bool      // every matching root was already terminal (nothing to cancel)
	closed   int       // beads newly closed by the cancel
	status   RunStatus // resulting run status
}

// runCancelTarget is one open run root a cancel winds down, and the IDs of the
// open workflow roots of its tree it closes, deepest first and the run root
// last.
type runCancelTarget struct {
	store     beads.Store
	root      beads.Bead
	workflows []string
}

// cancelRun winds down the run rooted at runID across every workflow store,
// closing each open root and its still-OPEN member beads with a canceled
// outcome, in three steps ordered so the run's work stops as early as it can:
//
//  1. Mark each open run root canceling: gc.cancel_requested, one write per
//     root and none when a previous attempt already marked it. From here on
//     the claim gate (molecule.WorkflowStopReason) refuses every open step of
//     the run, including the steps of drain item workflows whose own roots
//     are still open, since their ancestry reaches the marked root; the
//     dispatcher's control gate closes the run's controls; the workflow
//     finalizer settles as canceled; and the run projects "canceling".
//  2. Read the run's workflow tree (sourceworkflow.WorkflowTree: the run and
//     every drain item workflow nested under it at any depth, one member read
//     per workflow) and collect the assignees of the in-progress steps the
//     close will sweep, then hand them to interrupt, which must not block: the
//     interrupts run alongside the closes. The tree is read after the mark, so
//     a claim that landed before the mark is seen here and one after it is
//     refused by the gate.
//  3. Close each open workflow, nested drain items deepest first and the run
//     root last, reusing the workflow teardown close ordering (descendants
//     before the root, blockers before blocked) so a strict store accepts the
//     batch. Each root's close stamps the cancel marker on it too, so a nested
//     item's own controls stop as canceled.
//
// Any store read/write failure is returned so the caller reports a retryable
// 5xx rather than a phantom success. Every state a failure (or a crash) can
// leave is one a retry completes: the run root stays open until its own close,
// the last write, so the retry finds the run again instead of 409ing; if the
// mark landed the run is already stopped meanwhile; already-closed members and
// nested roots are left as they are, and the retry interrupts whoever still
// holds an in-progress step. Already-terminal runs (and already-closed members)
// are left untouched: closing a completed member would rewrite its recorded
// outcome. Each workflow's teardown tail (molecule.TeardownTailExclusion) is
// spared: it runs AFTER its root reaches a terminal state by contract, and its
// pass condition may read ROOT_OUTCOME, which this cancel's own close stamps;
// force-closing it unexecuted would strand the resources it releases.
func (s *Server) cancelRun(runID string, interrupt func(holders []string)) (cancelRunResult, error) {
	var res cancelRunResult
	var targets []runCancelTarget
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
			targets = append(targets, runCancelTarget{store: info.store, root: root})
		}
	}
	if len(targets) == 0 {
		res.terminal = res.found
		return res, nil
	}

	for _, target := range targets {
		if strings.TrimSpace(target.root.Metadata[beadmeta.CancelRequestedMetadataKey]) != "" {
			continue // marked by an earlier attempt
		}
		if err := target.store.SetMetadata(target.root.ID, beadmeta.CancelRequestedMetadataKey, "true"); err != nil {
			return res, err
		}
	}

	var holders []string
	for i := range targets {
		held, err := planRunCancel(&targets[i])
		if err != nil {
			return res, err
		}
		holders = append(holders, held...)
	}
	if len(holders) > 0 && interrupt != nil {
		interrupt(holders)
	}

	for _, target := range targets {
		for _, rootID := range target.workflows {
			n, err := cancelWorkflowRoot(target.store, rootID)
			res.closed += n
			if err != nil {
				return res, err
			}
		}
		res.status = RunStatusCanceled
	}
	return res, nil
}

// cancelWorkflowRoot closes one workflow root and its open members as
// canceled, stamping the cancel-intent marker on the root alone and sparing its
// teardown tail. A nested drain item root is canceled exactly like the run
// root: it is a workflow root in its own right, so its own controls read
// gc.cancel_requested off it and its own teardown tail still runs.
func cancelWorkflowRoot(store beads.Store, rootID string) (int, error) {
	exclude, err := molecule.TeardownTailExclusion(store, rootID)
	if err != nil {
		return 0, err
	}
	return sourceworkflow.CloseWorkflowSubtreeAsExcept(
		store,
		rootID,
		beadmeta.OutcomeCanceled,
		runCanceledCloseReason,
		map[string]string{beadmeta.CancelRequestedMetadataKey: "true"},
		exclude,
	)
}

// planRunCancel reads target's workflow tree, records the open workflows the
// cancel closes (deepest first, the run root last), and returns the assignees
// of the in-progress members those closes will sweep, in first-seen order and
// possibly repeated. A nested item workflow already closed is neither closed
// again nor searched for holders. A spared teardown step is not swept, so its
// holder is not returned; the teardown tail is judged from the members the
// tree walk already read (every member of a graph workflow carries its
// gc.root_bead_id) rather than from a further subtree read, which the close
// makes for itself, so that no read beyond the walk delays the interrupts.
func planRunCancel(target *runCancelTarget) ([]string, error) {
	tree, err := sourceworkflow.WorkflowTree(target.store, target.root.ID)
	if err != nil {
		return nil, err
	}
	var holders []string
	for _, node := range tree {
		if node.Root.ID == "" || isClosedStatus(node.Root.Status) {
			continue
		}
		exclude := molecule.TeardownTailExclusionFromMembers(node.Members)
		holders = append(holders, inProgressAssignees(node.Members, exclude)...)
		target.workflows = append(target.workflows, node.Root.ID)
	}
	return holders, nil
}

// inProgressAssignees returns the assignees of the in-progress members of a
// workflow that a close sparing exclude would sweep: the same member set
// CloseWorkflowSubtreeAsExcept closes, narrowed to claimed work.
func inProgressAssignees(members []beads.Bead, exclude func(beads.Bead) bool) []string {
	var assignees []string
	for _, member := range members {
		if member.Status != "in_progress" || (exclude != nil && exclude(member)) {
			continue
		}
		if assignee := strings.TrimSpace(member.Assignee); assignee != "" {
			assignees = append(assignees, assignee)
		}
	}
	return assignees
}

// interruptRunHolders interrupts the current turn of each live session among
// holders — assignees of in-progress steps of a run being canceled — so a
// step that was executing (a foreground tool included) stops rather than
// running on after its run was canceled. It is the interrupt POST /session/{id}/stop
// sends (session.Manager.StopTurn); no session is killed, because a pool
// session may go on to serve other work.
//
// An assignee resolves to its session through the API's session target
// resolution (bead ID, session name, alias, configured named identity), as the
// ?assignee bead filter does. One that names no open session, or a session
// that is suspended or whose runtime is not running, has no turn to interrupt
// and is skipped.
//
// Best-effort: the run is already durably marked canceling, so a failure is
// logged and returned for the response rather than failing the cancel. Distinct sessions
// are interrupted concurrently, since a provider's interrupt can wait for the
// session to settle. Returns the interrupted session IDs, in holder order, and
// the failures.
func (s *Server) interruptRunHolders(ctx context.Context, runID string, holders []string) ([]string, []RunCancelInterruptFailure) {
	if len(holders) == 0 {
		return nil, nil
	}
	var failures []RunCancelInterruptFailure
	fail := func(sessionRef string, err error) {
		log.Printf("gc api: run %s canceled, but interrupting %s failed: %v", runID, sessionRef, err)
		failures = append(failures, RunCancelInterruptFailure{Session: sessionRef, Error: err.Error()})
	}
	var unique []string
	seenHolder := map[string]bool{}
	for _, holder := range holders {
		if !seenHolder[holder] {
			seenHolder[holder] = true
			unique = append(unique, holder)
		}
	}
	store := s.state.SessionsBeadStore().Store
	if store == nil {
		for _, holder := range unique {
			fail(holder, errors.New("no session store configured"))
		}
		return nil, failures
	}

	var ids []string
	seenID := map[string]bool{}
	for _, holder := range unique {
		id, err := s.resolveSessionTargetIDWithContext(ctx, store, holder, apiSessionResolveOptions{})
		if errors.Is(err, session.ErrSessionNotFound) {
			continue // not an open session: nothing is executing under it
		}
		if err != nil {
			fail(holder, err)
			continue
		}
		if !seenID[id] {
			seenID[id] = true
			ids = append(ids, id)
		}
	}

	sp := s.state.SessionProvider()
	results := make([]error, len(ids))
	skipped := make([]bool, len(ids))
	var wg sync.WaitGroup
	for i, id := range ids {
		wg.Add(1)
		go func() {
			defer wg.Done()
			mgr := s.sessionManager(store)
			info, err := mgr.Get(id)
			if err != nil {
				results[i] = err
				return
			}
			// StopTurn's own guard (a suspended or stopped session has no
			// turn), checked first so such a session is neither waited on nor
			// reported as interrupted.
			if info.Closed || info.State == session.StateSuspended || !sp.IsRunning(info.SessionName) {
				skipped[i] = true
				return
			}
			results[i] = mgr.StopTurn(id)
		}()
	}
	wg.Wait()

	var interrupted []string
	for i, id := range ids {
		switch {
		case skipped[i]:
		case results[i] != nil:
			fail(id, results[i])
		default:
			interrupted = append(interrupted, id)
		}
	}
	return interrupted, failures
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
