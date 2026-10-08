package main

import (
	"context"
	"errors"
	"maps"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/telemetry"
	"github.com/gastownhall/gascity/internal/worktree"
)

// The start effect's Launch (CONTRACT v5 S1): the endpoint ticket, PreWake by
// CAS, prepare behind the refusing store, the agent's side effects, the
// routed leaf's Start with FreshOnly (LL6, I24), and the post-call check,
// which commits only a runtime that carries the PreWake token (S2, I3).
// Until C5a2's abandon table, every failure settles failed and writes
// nothing more.

// Launch refusal and failure causes.
const (
	causeWorktree          = "worktree"
	causeStartError        = "start-error"
	causeStartDeferred     = "start-deferred" // C5a2's deferred class
	causeCapacity          = "capacity"
	causeDiedDuringStartup = "died-during-startup"
	causeHeldDead          = "held-dead" // ErrSessionExists over a dead runtime; C5a3 recycles
)

func bringUpEffect(p *effectPass, it intent) func(context.Context) settlement {
	return startEffect{pass: p, it: it, verify: worktree.Verify}.run
}

// launch is S1's Launch. The endpoint ticket is taken only here, so a Noop or
// an adopt never takes a half-open endpoint's probe (SC N1).
func (e startEffect) launch(ctx context.Context, a startAttempt) settlement {
	rt := e.pass.Runtime
	cfg := e.pass.World.Env.Cfg
	ticket, ok := rt.Capacity.Admit(resolvedEndpointKey(a.tp, a.row.Info), a.row.Info.ID, startCandidate{info: a.row.Info, tp: a.tp}.logicalTemplate(cfg))
	if !ok {
		return refused(causeEndpointGate)
	}
	verdict := verdictNotAttempted
	defer func() {
		if ticket != nil {
			ticket.Resolve(verdict)
			ticket.guard.flush(rt.Rec, rt.Stderr)
		}
	}()
	var binding *bindingTarget
	if entry := e.pass.Alloc.Snapshot.Entries[e.it.Key]; entry != nil {
		binding = entry.Binding
	}
	bind, work, err := e.verifiedBinding(binding)
	if err != nil {
		return settlement{Outcome: settledRefused, Cause: causeWorktree, Work: work, Err: err}
	}
	row, transcript, end := e.preWake(ctx, a, bind)
	if end != nil {
		end.Work = work
		return *end
	}
	prepared, err := e.prepare(a, row, transcript)
	if err != nil {
		return settlement{Outcome: settledFailed, Cause: causePrepare, Work: work, Err: err}
	}
	prepared.cfg.FreshOnly = true
	// The agent's hooks and ACP route, as legacy installs them every tick;
	// a launch without them starts an agent with no hooks.
	e.pass.World.Templates.installSideEffects(a.agent, a.tp)
	startCtx, stop := rt.Clock.WithDeadline(ctx, e.it.Deadline.Add(-startDeadlineSlack))
	startErr := a.leaf.Start(startCtx, a.name, prepared.cfg)
	stop()
	s := e.postCall(ctx, a, prepared, startErr)
	switch verdict = verdictInconclusive; {
	case s.Outcome == settledLanded:
		verdict = verdictSuccess
	case runtime.IsProviderCapacity(startErr):
		verdict = verdictCapacity
	}
	s.Work = work
	return s
}

// verifiedBinding is the pass's binding patch for the row, with its
// worktree evidence verified and the work dir stamped as the create effect
// does (POOL-055, #34), and the verdict for the work item's backoff.
func (e startEffect) verifiedBinding(b *bindingTarget) (session.MetadataPatch, *workVerdict, error) {
	if b == nil {
		return nil, nil, nil
	}
	patch := maps.Clone(b.Patch)
	if b.WorktreeSpec == nil {
		return patch, nil, nil
	}
	report, err := e.verify(*b.WorktreeSpec)
	work := &workVerdict{BeadID: b.WorktreeSpec.BeadID, Fingerprint: specFingerprint(*b.WorktreeSpec), Refused: err != nil}
	if err != nil {
		return nil, work, err
	}
	if report.Path != "" {
		if patch == nil {
			patch = session.MetadataPatch{}
		}
		patch[beadmeta.WorkDirMetadataKey], patch[beadmeta.LegacyWorkDirMetadataKey] = report.Path, report.Path
	}
	return patch, work, nil
}

// preWake is S1's PreWake: one CAS that re-decides on the fresh row (open,
// not held or quarantined, at the incarnation the pass saw, not
// kill-pending) and writes legacy's PreWakePatch with the pass's binding,
// both halves of the stop request cleared (v5 D1), the stale-resume clear,
// and session_key filled only while empty (R1). It returns the PreWaked row
// and its key's transcript state, or the settlement that ends the effect.
// The session mutation lock is held around this call only (S-13).
func (e startEffect) preWake(ctx context.Context, a startAttempt, bind session.MetadataPatch) (session.Info, sessTranscriptState, *settlement) {
	now := e.pass.Runtime.Clock.Now()
	var next session.Info
	transcript := sessTranscriptUnknown
	var wrote bool
	err := session.WithSessionMutationLock(a.row.Info.ID, func() error {
		var err error
		wrote, err = a.writer.updateMetadataFenced(a.row.Info.ID, 1, func(fresh session.Info, _ session.PersistedResponse) session.MetadataPatch {
			if ctx.Err() != nil || !startable(fresh, a.row, now) {
				return nil
			}
			_, _, patch := preWakePatch(fresh, now)
			maps.Copy(patch, bind)
			for _, k := range [...]string{drainIntentReasonKey, drainIntentAtKey, drainIntentIncarnationKey, session.DrainAckIncarnationKey, session.DrainAckAtKey} {
				patch[k] = ""
			}
			next = foldInfo(fresh, patch)
			transcript = e.foldSessionKey(a, &next, patch)
			return patch
		})
		return err
	})
	var end *settlement
	switch {
	case wrote:
	case ctx.Err() != nil:
		s := deadlineSettlement(ctx)
		end = &s
	case errors.Is(err, errNoConditionalWriter):
		end = &settlement{Outcome: settledRefused, Cause: causeNoWriter, Err: err}
	case err != nil:
		end = &settlement{Outcome: settledFailed, Cause: causeWrite, Err: err}
	default:
		end = &settlement{Outcome: settledRefused, Cause: causeRedecided}
	}
	return next, transcript, end
}

// startable is PreWake's premise on the fresh row.
func startable(fresh session.Info, pass censusRow, now time.Time) bool {
	r := newCensusRow(pass.Key, fresh)
	return !fresh.Closed && !timerRunning(fresh.HeldUntil, now) && !timerRunning(fresh.QuarantinedUntil, now) &&
		r.Incarnation == pass.Incarnation && r.InstanceToken == pass.InstanceToken && !session.IsKillPendingInfo(fresh, now)
}

// timerRunning reports whether the RFC 3339 time at is after now.
func timerRunning(at string, now time.Time) bool {
	t, err := time.Parse(time.RFC3339, strings.TrimSpace(at))
	return err == nil && t.After(now)
}

// foldSessionKey folds into patch and next what legacy's prepare writes
// blind (S-1): it probes the key's transcript in the work dir the launch
// will use, clears a key whose transcript is provably gone, and fills an
// empty key for a provider that takes one. It returns the transcript state,
// which prepare takes as given.
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

// launchWorkDir is the work dir prepare launches row in: the task's, else
// the row's, else the template's.
func (e startEffect) launchWorkDir(a startAttempt, row session.Info) string {
	cityPath := e.pass.World.CityPath
	store := blindWriteRefusingStore{inner: a.writer.store}
	if dir := resolvePreparedTaskWorkDir(startCandidate{info: row, tp: a.tp}, cityPath, e.pass.World.Env.Cfg, store, nil); dir != "" {
		return dir
	}
	if dir := preparedStartSessionWorkDir(row); dir != "" {
		return resolveWorkDirAgainstCity(cityPath, dir)
	}
	return a.tp.WorkDir
}

// postCall is S1's post-call check: after a start that resumed with a
// session_key, the stale-key wait on the effect's clock (START-019); then a
// fresh read judged against the PreWake token. Only a runtime alive and
// carrying that token commits (I3).
func (e startEffect) postCall(ctx context.Context, a startAttempt, prepared *preparedStart, startErr error) settlement {
	row := prepared.candidate.info
	switch {
	case errors.Is(startErr, runtime.ErrSessionInitializing) || errors.Is(startErr, runtime.ErrRuntimeUnavailable):
		return settlement{Outcome: settledFailed, Cause: causeStartDeferred, Err: startErr}
	case runtime.IsProviderCapacity(startErr):
		return settlement{Outcome: settledFailed, Cause: causeCapacity, Err: startErr}
	case startErr == nil && strings.TrimSpace(row.SessionKey) != "":
		t := e.pass.Runtime.Clock.NewTimer(staleKeyDetectDelay)
		select {
		case <-t.C():
		case <-ctx.Done():
		}
		t.Stop()
	}
	if ctx.Err() != nil { // the runtime may be up: S1 adopts it next pass
		return deadlineSettlement(ctx)
	}
	read := e.observe(ctx, a, row, e.pass.Runtime.Clock.Now())
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

// launchCommit is the commit of a launch: legacy's MCP keys and the clear of
// the row's #46 mirror ride the CAS; once it lands, the #46 episode is
// cleared and session.woke is recorded with legacy's payload. An adopt
// commit does neither (legacy's heal does neither).
func (e startEffect) launchCommit(ctx context.Context, a startAttempt, prepared *preparedStart, read time.Time) settlement {
	extra, err := commitMCP(prepared, a.tp.IsACP)
	if err != nil {
		return settlement{Outcome: settledFailed, Cause: causeWrite, Err: err} // C5a2: the runtime is up; adopt it
	}
	healthKey := startupHealthEpisodeKey(a.row.Info, a.name)
	accrued := a.writer.startupHealthAccrued(healthKey)
	if accrued {
		extra[startupHealthActiveCountMetadataKey], extra[startupHealthActiveKindMetadataKey] = "0", ""
	}
	s := e.commit(ctx, a, prepared, prepared.candidate.info.InstanceToken, read, extra)
	if s.Outcome != settledLanded {
		return s
	}
	if accrued {
		_ = a.writer.clearStartupHealth(healthKey) // a lost clear only delays (v5 §13)
	}
	telemetry.RecordAgentStart(context.Background(), a.name, a.tp.DisplayName(), nil)
	s.Event = &events.Event{Type: events.SessionWoke, Actor: "gc", Subject: a.tp.DisplayName(), SessionID: a.row.Info.ID}
	return s
}

// commitMCP is a launch commit's MCP keys as legacy's commit writes them:
// the servers snapshot, persisted for the runtime first, and the identity.
func commitMCP(prepared *preparedStart, acp bool) (session.MetadataPatch, error) {
	info, servers := prepared.candidate.info, prepared.cfg.MCPServers
	snapshot, err := session.EncodeMCPServersSnapshot(servers)
	if err != nil {
		return nil, err
	}
	if err := session.PersistRuntimeMCPServersSnapshot(prepared.cfg.Env["GC_CITY_PATH"], info.ID, servers); err != nil {
		return nil, err
	}
	patch := session.MetadataPatch{}
	if snapshot != "" || info.MCPServersSnapshot != "" {
		patch[session.MCPServersSnapshotMetadataKey] = snapshot
	}
	if acp || info.MCPIdentity != "" || info.MCPServersSnapshot != "" {
		if id := firstNonEmptyGCString(info.MCPIdentity, info.ConfiguredNamedIdentity, info.AgentName); id != "" || info.MCPIdentity != "" {
			patch[session.MCPIdentityMetadataKey] = id
		}
	}
	return patch, nil
}
