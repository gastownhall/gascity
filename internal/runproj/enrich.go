package runproj

import "time"

// Run-health enrichment: a faithful Go port of the TypeScript run-summary enrich
// composition (shared/src/runs/health.ts + liveness.ts, driven by the frontend
// enrichRunSummary in supervisor/runSummary.ts). BuildRunSummary produces the
// bead-derived summary with health/census in the unavailable shell;
// EnrichRunSummary layers session-derived health + census on top.
//
// Per the run-view ADR the monotonic progress/thrash marks live in the per-city
// tailer, not in this function: the caller advances them with
// AdvanceProgressMarks once per fold generation and passes the result in, so a
// request-time enrich never double-advances them.

// attemptClimbMin and thrashDetectedStreak are the default health thresholds.
// Port of TS DEFAULT_ATTEMPT_CLIMB_MIN / DEFAULT_THRASH_DETECTED_STREAK.
const (
	attemptClimbMin      = 1
	thrashDetectedStreak = 2
)

// staleLatchAfterMs is the age past which a session-less, non-progressing open
// run is demoted out of Active. Port of TS STALE_LATCH_AFTER_MS (24h).
const staleLatchAfterMs = 24 * 60 * 60 * 1000

// LaneProgressMark is the per-lane monotonic progress record the tailer carries
// across fold generations to detect thrashing. Port of TS LaneProgressMark.
type LaneProgressMark struct {
	Progress     laneProgressComparison
	ThrashStreak int
}

// laneProgressComparison is the comparable-progress union. Port of TS
// LaneProgressComparison: {status:'comparable', stepId, stageIndex, attempt} |
// {status:'not_comparable', error}.
type laneProgressComparison struct {
	Status     string // "comparable" | "not_comparable"
	StepID     string
	StageIndex int
	Attempt    int
	Error      string
}

// AdvanceProgressMarks folds the previous per-lane marks forward against the
// current lanes, incrementing a lane's thrash streak when its graph position
// stayed flat while the active step's attempt climbed. Port of TS
// advanceProgressMarks. previous may be nil (cold start).
func AdvanceProgressMarks(previous map[string]LaneProgressMark, lanes []RunLane) map[string]LaneProgressMark {
	next := make(map[string]LaneProgressMark, len(lanes))
	for _, lane := range lanes {
		progress := comparableProgress(lane)
		prior, hasPrior := previous[lane.ID]

		positionFlat := hasPrior &&
			prior.Progress.Status == "comparable" &&
			progress.Status == "comparable" &&
			prior.Progress.StepID == progress.StepID &&
			prior.Progress.StageIndex == progress.StageIndex
		climbed := hasPrior &&
			prior.Progress.Status == "comparable" &&
			progress.Status == "comparable" &&
			progress.Attempt-prior.Progress.Attempt >= attemptClimbMin

		thrashStreak := 0
		if positionFlat && climbed {
			thrashStreak = prior.ThrashStreak + 1
		}

		next[lane.ID] = LaneProgressMark{Progress: progress, ThrashStreak: thrashStreak}
	}
	return next
}

// EnrichRunSummary layers session-derived health and the city census onto a
// bead-derived RunSummary. Port of the frontend enrichRunSummary: derive per-lane
// health from the session list, split blocked lanes back out, demote stale
// session-less latches out of Active, recompute counts and census. marks are the
// tailer's advanced per-city marks (pass nil for a cold first generation); nowMs
// is the snapshot generation time used for the stale-latch demotion.
func EnrichRunSummary(s RunSummary, sessions []DashboardSession, sessionsAvailable bool, nowMs int64, marks map[string]LaneProgressMark) RunSummary {
	inFlight := make([]RunLane, 0, len(s.Lanes)+len(s.BlockedLanes))
	inFlight = append(inFlight, s.Lanes...)
	inFlight = append(inFlight, s.BlockedLanes...)

	lanes := deriveRunHealthLanes(inFlight, sessions, sessionsAvailable, marks)

	blockedLanes := make([]RunLane, 0)
	activeEnriched := make([]RunLane, 0)
	for _, lane := range lanes {
		if lane.Phase == "blocked" {
			blockedLanes = append(blockedLanes, lane)
		} else {
			activeEnriched = append(activeEnriched, lane)
		}
	}

	liveActive := make([]RunLane, 0, len(activeEnriched))
	for _, lane := range activeEnriched {
		if !isStaleSessionlessLatch(lane, nowMs, sessionsAvailable) {
			liveActive = append(liveActive, lane)
		}
	}

	censusInput := make([]RunLane, 0, len(liveActive)+len(blockedLanes))
	censusInput = append(censusInput, liveActive...)
	censusInput = append(censusInput, blockedLanes...)

	out := s
	out.TotalActive = len(liveActive)
	out.Lanes = liveActive
	out.BlockedLanes = blockedLanes
	out.RunCounts = runCounts(liveActive, len(liveActive), len(blockedLanes))
	out.Census = RunCensusState{Status: "available", Data: buildCensus(censusInput)}
	return out
}

// deriveRunHealthLanes returns the lanes with engine-derived health. Port of the
// lane-mapping half of TS deriveRunHealth (the census it also returns is recomputed
// by the caller after demotion, so it is not returned here).
func deriveRunHealthLanes(lanes []RunLane, sessions []DashboardSession, sessionsAvailable bool, marks map[string]LaneProgressMark) []RunLane {
	out := make([]RunLane, 0, len(lanes))
	for _, lane := range lanes {
		enriched := lane

		// Without the session list, health cannot be derived (gascity-dashboard
		// 0gww): report the lane's health as genuinely unavailable rather than a
		// degraded-but-available shell.
		if !sessionsAvailable {
			enriched.Health = RunLaneHealthState{Status: "unavailable", Error: "run session list unavailable"}
			out = append(out, enriched)
			continue
		}

		session, resolved := resolveLaneSession(lane, sessions)

		phaseConfidence := "inferred"
		if lane.FormulaStageResolved && resolved {
			phaseConfidence = "known"
		}

		thrashStreak := marks[lane.ID].ThrashStreak

		sessionState := RunLaneSessionState{Status: "unresolved", Error: "run session unresolved"}
		if resolved {
			sessionState = sessionFacts(session)
		}

		enriched.Health = RunLaneHealthState{
			Status: "available",
			Data: RunLaneHealth{
				PhaseConfidence:   phaseConfidence,
				NeedsOperator:     laneNeedsOperator(lane),
				StuckNode:         stuckNode(lane),
				ThrashingDetected: thrashStreak >= thrashDetectedStreak,
				Session:           sessionState,
			},
		}
		out = append(out, enriched)
	}
	return out
}

// laneNeedsOperator reports the structural human-gate signal. Port of TS
// laneNeedsOperator: phase 'approval' or 'blocked'. Derived from lane.phase alone
// so it stays valid during a session-list outage.
func laneNeedsOperator(lane RunLane) bool {
	return lane.Phase == "approval" || lane.Phase == "blocked"
}

// resolveLaneSession resolves the first of a lane's active assignees to a
// session. Port of TS resolveLaneSession.
func resolveLaneSession(lane RunLane, sessions []DashboardSession) (DashboardSession, bool) {
	for _, assignee := range lane.ActiveAssignees {
		if s, ok := resolveSessionForTarget(assignee, sessions); ok {
			return s, true
		}
	}
	return DashboardSession{}, false
}

// comparableProgress projects a lane's progress to the comparable shape used for
// thrash detection. Port of TS comparableProgress.
func comparableProgress(lane RunLane) laneProgressComparison {
	if lane.Progress.Status != "active_step" {
		return laneProgressComparison{Status: "not_comparable", Error: "run has no active step"}
	}
	if lane.Progress.Stage.Status != "available" {
		return laneProgressComparison{Status: "not_comparable", Error: lane.Progress.Stage.Error}
	}
	if lane.Progress.Attempt.Status != "available" {
		return laneProgressComparison{Status: "not_comparable", Error: lane.Progress.Attempt.Error}
	}
	return laneProgressComparison{
		Status:     "comparable",
		StepID:     lane.Progress.StepID,
		StageIndex: lane.Progress.Stage.Index,
		Attempt:    lane.Progress.Attempt.Value,
	}
}

// stuckNode reports the semantic node a lane is parked on. Port of TS stuckNode.
func stuckNode(lane RunLane) RunLaneStuckNode {
	if lane.Progress.Status == "active_step" {
		return RunLaneStuckNode{Status: "available", ID: lane.Progress.StepID}
	}
	return RunLaneStuckNode{Status: "unavailable", Error: "active run step unavailable"}
}

// sessionFacts projects a resolved session into the lane's session-fact union.
// Port of TS sessionFacts.
func sessionFacts(session DashboardSession) RunLaneSessionState {
	lastActive := RunLaneSessionLastActive{Status: "unavailable", Error: "session last_active unavailable"}
	if session.LastActive != nil {
		lastActive = RunLaneSessionLastActive{Status: "available", At: *session.LastActive}
	}
	activity := RunLaneSessionActivity{Status: "unavailable", Error: "session activity unavailable"}
	if session.Activity != nil {
		activity = RunLaneSessionActivity{Status: "available", Value: *session.Activity}
	}
	return RunLaneSessionState{
		Status:     "resolved",
		LastActive: lastActive,
		Running:    RunLaneSessionRunning{Status: "available", Value: session.Running},
		Activity:   activity,
	}
}

// buildCensus tallies a threshold-independent city census from the enriched
// lanes. Port of TS buildCensus.
func buildCensus(lanes []RunLane) RunCensus {
	var byPhase RunCensusByPhase
	totalInFlight := 0
	unverifiable := 0
	knownDenominator := 0
	thrashing := 0

	for _, lane := range lanes {
		incCensusPhase(&byPhase, lane.Phase)
		if lane.Phase == "complete" {
			continue
		}
		totalInFlight++
		if lane.Health.Status == "available" && lane.Health.Data.PhaseConfidence == "known" {
			knownDenominator++
			if lane.Health.Data.ThrashingDetected {
				thrashing++
			}
		} else {
			unverifiable++
		}
	}

	return RunCensus{
		ByPhase:          byPhase,
		TotalInFlight:    totalInFlight,
		Unverifiable:     unverifiable,
		KnownDenominator: knownDenominator,
		Thrashing:        thrashing,
	}
}

func incCensusPhase(b *RunCensusByPhase, phase string) {
	switch phase {
	case "intake":
		b.Intake++
	case "implementation":
		b.Implementation++
	case "review":
		b.Review++
	case "approval":
		b.Approval++
	case "finalization":
		b.Finalization++
	case "blocked":
		b.Blocked++
	case "complete":
		b.Complete++
	case "active":
		b.Active++
	}
}

// isStaleSessionlessLatch reports whether an open, non-progressing, session-less
// lane is old enough to demote out of Active. Port of TS isStaleSessionlessLatch.
// nowMs is the snapshot generation time, not a live clock.
func isStaleSessionlessLatch(lane RunLane, nowMs int64, sessionsAvailable bool) bool {
	if !sessionsAvailable {
		return false
	}
	if lane.Phase == "complete" || lane.Phase == "blocked" {
		return false
	}
	if lane.Progress.Status == "active_step" {
		return false
	}
	if laneSessionResolved(lane) {
		return false
	}
	if lane.UpdatedAt.Status != "available" {
		return false
	}
	// A parse failure means we cannot judge staleness — mirrors the TS
	// Number.isFinite(ageMs) guard (Date.parse → NaN → not stale).
	ms, ok := millisFromTimestamp(lane.UpdatedAt.At)
	if !ok {
		return false
	}
	return nowMs-ms >= staleLatchAfterMs
}

// laneSessionResolved reports whether a lane's enriched health carries a resolved
// session. Port of TS laneSessionResolved.
func laneSessionResolved(lane RunLane) bool {
	return lane.Health.Status == "available" && lane.Health.Data.Session.Status == "resolved"
}

// millisFromTimestamp parses an RFC3339 timestamp to Unix milliseconds, reporting
// ok=false on an empty or unparseable value (the TS Number.isFinite guard).
func millisFromTimestamp(value string) (int64, bool) {
	if value == "" {
		return 0, false
	}
	t, err := time.Parse(time.RFC3339, value)
	if err != nil {
		t, err = time.Parse(time.RFC3339Nano, value)
		if err != nil {
			return 0, false
		}
	}
	return t.UnixMilli(), true
}
