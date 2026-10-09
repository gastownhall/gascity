package main

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
)

// The K1 nudges and orphan-release steps (CONTRACT v5 §13 K1 nudges row):
// legacy's three pool backstops and its orphaned-assignment release, run in
// the external-reads lane over the planner's last allocation summary (S-14),
// loaded once per run and never mutated. They write legacy's pacing markers,
// releasePoolAssignmentIfCurrent's fenced release and its advisory affinity
// clear (R1 exception 4). The execution backstop's drain request becomes a
// message to the planner, which arm A16 (C7b1) consumes.
//
// The steps run apart, so unlike legacy's tick the claim nudges may still see
// work the release reopened this pass; a reopened bead's slot is dead, so no
// nudge reaches it.

// executionStalledRequest is the execution backstop's drain request
// (NUDGE-022) for the planner: the row, and the generation a fresh read found
// it at, so the drain cannot land on a later incarnation.
type executionStalledRequest struct {
	ID, Generation string
	At             time.Time
}

// errNoPlannerInbox: the lane has no planner to hand a request to.
var errNoPlannerInbox = errors.New("no planner for the execution-stalled request")

// addPoolSteps registers the nudges and orphan-release steps over the
// planner's last allocation summary and its execution-stalled inbox. Call it
// before start. Each runs every pass, as legacy runs them every tick; the
// nudges pace themselves by their markers.
func (l *externalReadsLane) addPoolSteps(summary func() *allocSummary, stalled func(executionStalledRequest)) {
	l.summary, l.stalled = summary, stalled
	l.steps = append(l.steps,
		&laneStep{name: "nudges", legs: sourceNone, run: l.nudges},
		&laneStep{name: "orphan-release", legs: sourceNone, run: l.orphanRelease})
}

// laneSummary is the planner's last allocation summary, or nil when there is
// none or it is older than a recording stays fresh: a stalled planner must not
// steer nudges or releases from old demand.
func (l *externalReadsLane) laneSummary() *allocSummary {
	if l.summary == nil {
		return nil
	}
	s := l.summary()
	if s == nil || l.now().Sub(s.At) > externalReadsFreshPatrols*l.interval {
		return nil
	}
	return s
}

// nudges is the nudges step: the claim, continuation and execution
// backstops, in legacy order, over one live read of the raw session rows
// whose markers they pace by.
func (l *externalReadsLane) nudges(ctx context.Context, env externalReadsEnv) {
	s := l.laneSummary()
	if s == nil || env.Cfg == nil || env.CityStore == nil {
		return
	}
	raw, err := loadSessionBeads(env.CityStore)
	if err != nil {
		fmt.Fprintf(l.stderr, "external reads: loading sessions for the nudges: %v\n", err) //nolint:errcheck // best-effort stderr
		return
	}
	now, sessStore := l.now(), beads.SessionStore{Store: env.CityStore}
	claim := slices.Concat(s.AssignedWork, s.ReadyRouted)
	refs := make([]string, len(claim))
	copy(refs, s.AssignedStoreRefs)
	copy(refs[len(s.AssignedWork):], s.ReadyRoutedRefs)
	nudgeStalledPoolClaims(env.SP, env.Cfg, sessStore, raw, claim, refs, now, l.stderr)
	if ctx.Err() != nil {
		return
	}
	var candidates []ContinuationClaimCandidate
	partial := s.Partial
	if !partial {
		candidates, partial = selectReadyContinuationClaimCandidates(env.CityName, s.AssignedWork, s.AssignedStores, s.AssignedStoreRefs, s.ReadyAssigned)
	}
	nudgeStalledPoolContinuations(env.SP, env.Cfg, sessStore, raw, candidates, partial, now, l.stderr)
	if ctx.Err() != nil {
		return
	}
	nudgeStalledPoolExecution(env.SP, env.Cfg, sessStore, raw, s.AssignedWork, s.AssignedStores, s.AssignedStoreRefs, s.Partial, now, l.events, l.requestExecutionStalled(env), l.stderr)
}

// requestExecutionStalled is the execution backstop's requestDrain: it reads
// the row fresh, as legacy's requestExecutionStalledDrain does, and posts the
// request to the planner.
func (l *externalReadsLane) requestExecutionStalled(env externalReadsEnv) func(beads.Bead) error {
	return func(b beads.Bead) error {
		if l.stalled == nil {
			return errNoPlannerInbox
		}
		info, err := sessionFrontDoor(env.CityStore).Get(b.ID)
		if err != nil {
			return fmt.Errorf("reading session %q before requesting its drain: %w", b.ID, err)
		}
		l.stalled(executionStalledRequest{ID: info.ID, Generation: info.Generation, At: l.now()})
		return nil
	}
}

// orphanRelease is the orphan-release step: legacy's release of pool work
// whose assignee no open session holds, only over complete snapshots
// (INC-016) and sparing work the allocation would wake, then its
// bead.dead_assignee_reopened events.
func (l *externalReadsLane) orphanRelease(_ context.Context, env externalReadsEnv) {
	s := l.laneSummary()
	if s == nil || s.Partial || env.Cfg == nil || env.CityStore == nil {
		return
	}
	work := env.WorkStore
	if work == nil {
		work = env.CityStore
	}
	wake, wakeRefs := filterAssignedWorkBeadsForSessionWake(env.Cfg, env.CityPath, work, s.OpenSessions, s.AssignedWork, s.AssignedStoreRefs)
	snapshot := DesiredStateResult{AssignedWorkBeads: s.AssignedWork, AssignedWorkStores: s.AssignedStores, AssignedWorkStoreRefs: s.AssignedStoreRefs, StoreQueryPartial: s.Partial}
	released := releaseOrphanedPoolAssignmentsWhenSnapshotsComplete(work, beads.SessionStore{Store: env.CityStore}, env.Cfg, env.CityPath, s.OpenSessions, snapshot, env.RigStores, protectedWakeWorkKeys(wake, wakeRefs), nil)
	for _, r := range released {
		fmt.Fprintf(l.stderr, "released orphaned pool work: %s\n", r.ID) //nolint:errcheck // best-effort stderr
	}
	emitDeadAssigneeReopenedEvents(l.events, s.AssignedWork, released, l.now())
}
