package main

import (
	"context"
	"errors"
	"maps"
	"reflect"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/telemetry"
	"github.com/gastownhall/gascity/internal/worktree"
)

// The start effect's Launch (CONTRACT v5 S1): in run's lock section the
// ticket and PreWake; outside every session lock prepare and the FreshOnly
// Start (LL6, I24); in a second section the post-call read and the commit
// (S2, I3). Until C5a2's abandon table a failure writes nothing more.
const (
	causeSessionName       = "session-name-invalid" // START-012
	causeWorktree          = "worktree"
	causePreWakeContended  = "prewake-contended"
	causeStartError        = "start-error"
	causeStartDeferred     = "start-deferred" // C5a2's deferred class
	causeCapacity          = "capacity"
	causeDiedDuringStartup = "died-during-startup"
	causeHeldDead          = "held-dead" // ErrSessionExists over a dead runtime; C5a3 recycles
)

const preWakeAttempts = 3 // CAS tries, each re-deciding on the fresh row

func bringUpEffect(p *effectPass, it intent) func(context.Context) settlement {
	return startEffect{pass: p, it: it, verify: worktree.Verify}.run
}

// launchPrep is what a landed PreWake hands the launch.
type launchPrep struct {
	row        session.Info
	transcript sessTranscriptState
	ticket     *capacityTicket
	work       *workVerdict
}

// preWake is the Launch up to PreWake: START-012's name check, the endpoint
// ticket (only now, SC N1), the verified binding, and a CAS that re-decides
// on the fresh row (startable) and writes PreWakePatch with the binding,
// the stop request cleared (v5 D1), the concrete-pool repair, the
// stale-resume clear and the set-once session_key fill (R1).
func (e startEffect) preWake(ctx context.Context, a startAttempt) (settlement, *launchPrep) {
	if !session.IsSessionNameSyntaxValid(strings.TrimSpace(a.row.Info.SessionNameMetadata)) {
		return refused(causeSessionName), nil
	}
	cfg, cityPath := e.pass.World.Env.Cfg, e.pass.World.CityPath
	ticket, ok := e.pass.Start.Capacity.Admit(resolvedEndpointKey(a.tp, a.row.Info), a.row.Info.ID, startCandidate{info: a.row.Info, tp: a.tp}.logicalTemplate(cfg))
	if !ok {
		return refused(causeEndpointGate), nil
	}
	prep := &launchPrep{ticket: ticket}
	end := func(s settlement) (settlement, *launchPrep) {
		e.resolveTicket(ticket, verdictNotAttempted)
		s.Work = prep.work
		return s, nil
	}
	var binding *bindingTarget
	if entry := e.pass.Alloc.Snapshot.Entries[e.it.Key]; entry != nil {
		binding = entry.Binding
	}
	bind, err := e.verifiedBinding(binding, prep)
	if err != nil {
		return end(settlement{Outcome: settledRefused, Cause: causeWorktree, Err: err})
	}
	now, premise := e.pass.Start.Clock.Now(), false
	wrote, err := a.writer.updateMetadataFenced(a.row.Info.ID, preWakeAttempts, func(fresh session.Info, _ session.PersistedResponse) session.MetadataPatch {
		if premise = startable(fresh, a.row, now); !premise || ctx.Err() != nil {
			return nil
		}
		_, _, patch := preWakePatch(fresh, now)
		maps.Copy(patch, bind)
		maps.Copy(patch, stopVoidResiduePatch()) // both halves of the stop request (D1)
		next := foldInfo(fresh, patch)
		repair := concretePoolWorkDirRepairPatch(startCandidate{info: next, tp: a.tp}, cityPath, cfg)
		maps.Copy(patch, repair)
		next = foldInfo(next, repair)
		prep.transcript = e.foldSessionKey(a, &next, patch)
		prep.row = next
		return patch
	})
	switch {
	case wrote:
		return settlement{}, prep
	case ctx.Err() != nil:
		return end(deadlineSettlement(ctx))
	case errors.Is(err, errNoConditionalWriter):
		return end(settlement{Outcome: settledRefused, Cause: causeNoWriter, Err: err})
	case err != nil:
		return end(settlement{Outcome: settledFailed, Cause: causeWrite, Err: err})
	case !premise:
		return end(refused(causeRedecided))
	}
	return end(refused(causePreWakeContended))
}

// launch is the Launch after PreWake: prepare, the composite's FreshOnly
// Start within startup_timeout (the deadline less its slack), and the
// post-call check; the ticket resolves with what the attempt proved.
func (e startEffect) launch(ctx context.Context, a startAttempt, prep *launchPrep) (s settlement) {
	verdict := verdictNotAttempted
	defer func() {
		s.Work = prep.work
		e.resolveTicket(prep.ticket, verdict)
	}()
	prepared, err := e.prepare(a, prep.row, prep.transcript)
	if err != nil {
		return settlement{Outcome: settledFailed, Cause: causePrepare, Err: err}
	}
	if ctx.Err() != nil { // PreWaked, not started: S1 launches again next pass
		return deadlineSettlement(ctx)
	}
	prepared.cfg.FreshOnly = true
	startCtx, stop := e.pass.Start.Clock.WithDeadline(ctx, e.it.Deadline.Add(-startDeadlineSlack))
	startErr := e.pass.Runtime.Start(startCtx, a.name, prepared.cfg)
	stop()
	verdict = verdictInconclusive
	s = e.postCall(ctx, a, prepared, startErr)
	switch {
	case s.Outcome == settledLanded:
		verdict = verdictSuccess
	case runtime.IsProviderCapacity(startErr):
		verdict = verdictCapacity
	}
	return s
}

// resolveTicket resolves t once and flushes the breaker's transitions.
func (e startEffect) resolveTicket(t *capacityTicket, v startVerdict) {
	if t != nil {
		t.Resolve(v)
		t.guard.flush(e.pass.Start.Rec, e.pass.Start.Stderr)
	}
}

// verifiedBinding is the pass's binding patch with its worktree verified
// and stamped as the create effect does (POOL-055, #34); prep carries the
// verdict.
func (e startEffect) verifiedBinding(b *bindingTarget, prep *launchPrep) (session.MetadataPatch, error) {
	if b == nil {
		return nil, nil
	}
	patch := maps.Clone(b.Patch)
	if b.WorktreeSpec == nil {
		return patch, nil
	}
	report, err := e.verify(*b.WorktreeSpec)
	prep.work = &workVerdict{BeadID: b.WorktreeSpec.BeadID, Fingerprint: specFingerprint(*b.WorktreeSpec), Refused: err != nil}
	if err != nil {
		return nil, err
	}
	if report.Path != "" {
		if patch == nil {
			patch = session.MetadataPatch{}
		}
		patch[beadmeta.WorkDirMetadataKey], patch[beadmeta.LegacyWorkDirMetadataKey] = report.Path, report.Path
	}
	return patch, nil
}

// startable is PreWake's premise: open, the pass's lifecycle facts unchanged
// (an operator suspend or wake since is never overwritten), not held or
// quarantined, the pass's incarnation, not kill-pending.
func startable(fresh session.Info, pass censusRow, now time.Time) bool {
	r := newCensusRow(pass.Key, fresh)
	return !fresh.Closed && reflect.DeepEqual(session.LifecycleInputFromInfo(pass.Info), session.LifecycleInputFromInfo(fresh)) &&
		!timerRunning(fresh.HeldUntil, now) && !timerRunning(fresh.QuarantinedUntil, now) &&
		r.Incarnation == pass.Incarnation && r.InstanceToken == pass.InstanceToken && !session.IsKillPendingInfo(fresh, now)
}

// timerRunning reports whether the RFC 3339 time at is after now.
func timerRunning(at string, now time.Time) bool {
	t, err := time.Parse(time.RFC3339, strings.TrimSpace(at))
	return err == nil && t.After(now)
}

// foldSessionKey folds into patch what legacy's prepare writes blind (S-1):
// the stale-resume clear, probed in the launch's work dir, and the fill of
// an empty key. Prepare takes the transcript state it returns as given.
func (e startEffect) foldSessionKey(a startAttempt, next *session.Info, patch session.MetadataPatch) sessTranscriptState {
	state := sessTranscriptUnknown
	if sk := strings.TrimSpace(next.SessionKey); sk != "" {
		if dir := e.launchWorkDir(a, *next); dir != "" {
			present, probeable := staleResumeKeyProbe(sessionTranscriptProvider(a.tp.ResolvedProvider, *next), dir, sk)
			switch {
			case probeable && present:
				state = sessTranscriptPresent
			case probeable:
				state = sessTranscriptAbsent
				reset := clearStaleResumeKeyMetadata("", nil) // its patch only; this CAS writes it
				maps.Copy(patch, reset)
				*next = foldInfo(*next, reset)
			}
		}
	}
	if next.SessionKey == "" && a.tp.ResolvedProvider != nil && a.tp.ResolvedProvider.SessionIDFlag != "" {
		if key, err := session.GenerateSessionKey(); err == nil {
			patch["session_key"] = key
			*next = foldInfo(*next, session.MetadataPatch{"session_key": key})
			state = sessTranscriptAbsent
		}
	}
	return state
}

// launchWorkDir is prepare's work dir: the task's, the row's, the template's.
func (e startEffect) launchWorkDir(a startAttempt, row session.Info) string {
	cityPath := e.pass.World.CityPath
	store := blindWriteRefusingStore{inner: a.writer.store}
	if dir := resolvePreparedTaskWorkDir(startCandidate{info: row, tp: a.tp}, cityPath, e.pass.World.Env.Cfg, store, e.taskWorkDirs()); dir != "" {
		return dir
	}
	if dir := preparedStartSessionWorkDir(row); dir != "" {
		return resolveWorkDirAgainstCity(cityPath, dir)
	}
	return a.tp.WorkDir
}

// taskWorkDirs is legacy's task work dir resolver over the pass's assigned
// work, only when that read was not partial (as the legacy tick builds it).
func (e startEffect) taskWorkDirs() taskWorkDirResolver {
	d := e.pass.World.Demand
	if d.StorePartial || len(d.AssignedWork) == 0 {
		return nil
	}
	return newAssignedTaskWorkDirResolver(e.pass.World.CityPath, d.AssignedWork)
}

// postCall is the post-call check: after a start with a session_key the
// stale-key wait (START-019), then one lock section from the fresh read,
// judged against the PreWake token, through the commit (I3).
func (e startEffect) postCall(ctx context.Context, a startAttempt, prepared *preparedStart, startErr error) settlement {
	row := prepared.candidate.info
	switch {
	case errors.Is(startErr, runtime.ErrSessionInitializing) || errors.Is(startErr, runtime.ErrRuntimeUnavailable):
		return settlement{Outcome: settledFailed, Cause: causeStartDeferred, Err: startErr}
	case runtime.IsProviderCapacity(startErr):
		return settlement{Outcome: settledFailed, Cause: causeCapacity, Err: startErr}
	case startErr == nil && strings.TrimSpace(row.SessionKey) != "":
		t := e.pass.Start.Clock.NewTimer(staleKeyDetectDelay)
		select {
		case <-t.C():
		case <-ctx.Done():
		}
		t.Stop()
	}
	var s settlement
	_ = session.WithSessionMutationLock(row.ID, func() error {
		s = e.postCallLocked(ctx, a, prepared, startErr)
		return nil
	})
	return s
}

func (e startEffect) postCallLocked(ctx context.Context, a startAttempt, prepared *preparedStart, startErr error) settlement {
	row := prepared.candidate.info
	read := e.observe(ctx, a, row, e.pass.Start.Clock.Now())
	if ctx.Err() != nil { // the runtime may be up: S1 adopts it next pass
		return deadlineSettlement(ctx)
	}
	verb, cause := resolveStart(true, read, row)
	died := startErr == nil || errors.Is(startErr, runtime.ErrSessionDiedDuringStartup)
	switch {
	case verb == verbCommit:
		return e.launchCommit(ctx, a, prepared, read.At)
	case died && (read.Class == freshGone || read.Class == freshDead):
		return settlement{Outcome: settledFailed, Cause: causeDiedDuringStartup, Err: startErr} // C5a2's abandon class
	case errors.Is(startErr, runtime.ErrSessionExists) && read.Class == freshDead:
		return refused(causeHeldDead)
	case startErr != nil && !errors.Is(startErr, runtime.ErrSessionExists):
		return settlement{Outcome: settledFailed, Cause: causeStartError, Err: startErr}
	}
	return refused(cause)
}

// launchCommit commits with legacy's MCP keys (persisted first) and the #46
// mirror clear in the CAS; once it lands it clears the #46 episode and
// records session.woke. An adopt commit does neither, as legacy's heal.
func (e startEffect) launchCommit(ctx context.Context, a startAttempt, prepared *preparedStart, read time.Time) settlement {
	row := prepared.candidate.info
	extra, err := startCommitMCPPatch(prepared, row)
	if err == nil {
		err = session.PersistRuntimeMCPServersSnapshot(prepared.cfg.Env["GC_CITY_PATH"], row.ID, prepared.cfg.MCPServers)
	}
	if err != nil {
		return settlement{Outcome: settledFailed, Cause: causeWrite, Err: err} // C5a2: the runtime is up; adopt it
	}
	health, healthKey := startupHealthRecord{store: a.writer.store}, startupHealthEpisodeKey(a.row.Info, a.name)
	accrued := health.accrued(healthKey)
	if accrued {
		extra[startupHealthActiveCountMetadataKey], extra[startupHealthActiveKindMetadataKey] = "0", ""
	}
	s := e.commit(ctx, a, prepared, row.InstanceToken, read, extra)
	if s.Outcome != settledLanded {
		return s
	}
	if accrued {
		_ = health.clear(healthKey) // a lost clear only delays (v5 §13)
	}
	telemetry.RecordAgentStart(context.Background(), a.name, a.tp.DisplayName(), nil)
	s.Event = &events.Event{Type: events.SessionWoke, Actor: "gc", Subject: a.tp.DisplayName(), SessionID: a.row.Info.ID}
	return s
}
