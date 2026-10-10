package main

import (
	"context"
	"errors"
	"maps"
	"strings"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/telemetry"
	"github.com/gastownhall/gascity/internal/worktree"
)

// The start effect's Launch (CONTRACT v5 S1, S2), bringUp's sections: the
// verb section and the admit-ticket Call (SC N1: only a Launch takes the
// ticket); PreWake, decided again on the fresh row, and the launch Call
// (prepare, the FreshOnly Start, START-019's wait); the commit (S2's
// FactsCommit premise) and a Call that clears the #46 episode. Until C5a2's
// abandon table, a failure writes nothing more.

// Launch refusal and failure causes.
const (
	causeSessionName       = "session-name-invalid" // START-012
	causeHeld              = "held"                 // a hold or kill fence since the pass
	causeWorktree          = "worktree"
	causeStartError        = "start-error"
	causeStartDeferred     = "start-deferred" // C5a2's deferred class
	causeCapacity          = "capacity"
	causeDiedDuringStartup = "died-during-startup"
	causeHeldDead          = "held-dead" // ErrSessionExists over a dead runtime; C5a3 recycles
)

// errEndpointGate is the admit-ticket Call's refusal: the endpoint admits
// nothing now (closed, or its half-open probe taken).
var errEndpointGate = errors.New("v2 start: the endpoint admits no start now")

// startSections are bringUp's sections.
var startSections = []section{
	called(0, probed(prepareAdoption, verbStep(false)), admitTicket),
	called(callStart, probed(foldPreWake, preWakeStep), startLaunch),
	called(0, section{Premise: premiseOwnToken, Decide: commitStep}, afterCommit),
}

// launchRow is the row, template and pass a Launch's Calls act on.
type launchRow struct {
	w   *World
	row session.Info
	tp  TemplateParams
}

// startTicket is the endpoint ticket the admit Call took, and the verdict
// the launch leaves for it.
type startTicket struct {
	t       *capacityTicket
	verdict startVerdict
}

// admitTicket admits the endpoint ticket and resolves it at every exit:
// success once the commit lands.
func admitTicket(_ context.Context, c txCaps, in launchRow) (*startTicket, error) {
	t, ok := c.start.capacity.Admit(resolvedEndpointKey(in.tp, in.row), in.row.ID, startCandidate{info: in.row, tp: in.tp}.logicalTemplate(in.w.Env.Cfg))
	if !ok {
		return nil, errEndpointGate
	}
	st := &startTicket{t: t, verdict: verdictNotAttempted}
	c.onExit(func(final settlement) {
		if st.verdict == verdictInconclusive && final.Outcome == settledLanded {
			st.verdict = verdictSuccess
		}
		if t != nil {
			t.Resolve(st.verdict)
			t.guard.flush(c.start.rec, c.start.stderr)
		}
	})
	return st, nil
}

// preWakeFold is what the PreWake's Probe read or minted.
type preWakeFold struct {
	token, key string
	bind       session.MetadataPatch
	work       *workVerdict
	repair     session.MetadataPatch
	reset      session.MetadataPatch
	transcript sessTranscriptState
}

// verifyWorktree is worktree.Verify; a test injects its own.
var verifyWorktree = worktree.Verify

// foldPreWake is the PreWake's Probe on the expected row, which the premise
// holds the fresh one to: the binding's worktree verified (POOL-055, #34),
// the concrete-pool repair, the key's transcript probed in the launch's work
// dir (S-1), and a fresh token and key.
func foldPreWake(_ context.Context, r effectReads, v txView) (preWakeFold, error) {
	f := preWakeFold{token: session.NewInstanceToken()}
	if key, err := session.GenerateSessionKey(); err == nil {
		f.key = key
	}
	if e := v.Alloc.Snapshot.Entries[v.It.Key]; e != nil && e.Binding != nil {
		f.bind = maps.Clone(e.Binding.Patch)
		if spec := e.Binding.WorktreeSpec; spec != nil {
			report, err := verifyWorktree(*spec)
			f.work = &workVerdict{BeadID: spec.BeadID, Fingerprint: specFingerprint(*spec), Refused: err != nil}
			if err != nil {
				return f, err
			}
			if report.Path != "" {
				f.bind = maps.Clone(f.bind)
				if f.bind == nil {
					f.bind = session.MetadataPatch{}
				}
				f.bind[beadmeta.WorkDirMetadataKey], f.bind[beadmeta.LegacyWorkDirMetadataKey] = report.Path, report.Path
			}
		}
	}
	res, _ := v.World.Templates.lookup(v.Row)
	_, _, patch := preWakePatch(v.Row, v.Now, f.token)
	next := foldInfo(foldInfo(v.Row, patch), f.bind)
	f.repair = concretePoolWorkDirRepairPatch(startCandidate{info: next, tp: res.TP}, v.World.CityPath, v.World.Env.Cfg)
	next = foldInfo(next, f.repair)
	if sk := strings.TrimSpace(next.SessionKey); sk != "" {
		if dir := launchWorkDir(r, v.World, next, res.TP); dir != "" {
			present, probeable := staleResumeKeyProbe(sessionTranscriptProvider(res.TP.ResolvedProvider, next), dir, sk)
			switch {
			case probeable && present:
				f.transcript = sessTranscriptPresent
			case probeable:
				f.transcript, f.reset = sessTranscriptAbsent, clearStaleResumeKeyMetadata("", nil) // its patch only
			}
		}
	}
	return f, nil
}

// launchWorkDir is the work dir prepare launches row in: the task's, the
// row's, the template's.
func launchWorkDir(r effectReads, w *World, row session.Info, tp TemplateParams) string {
	if dir := resolvePreparedTaskWorkDir(startCandidate{info: row, tp: tp}, w.CityPath, w.Env.Cfg, r.city, taskWorkDirs(w)); dir != "" {
		return dir
	}
	if dir := preparedStartSessionWorkDir(row); dir != "" {
		return resolveWorkDirAgainstCity(w.CityPath, dir)
	}
	return tp.WorkDir
}

// launchPass is what the PreWake hands the launch Call.
type launchPass struct {
	launchRow
	transcript sessTranscriptState
	ticket     *startTicket
}

// preWakeStep is the PreWake's Decide: the ticket admitted, START-012's
// name, no hold or kill fence, the runtime still absent; then PreWakePatch
// with the binding, the stop request cleared (D1), the repair, the
// stale-resume clear and the set-once key fill (R1).
func preWakeStep(v txView, f preWakeFold, probeErr error) txStep {
	ticket, err := callResult[*startTicket](v)
	switch {
	case errors.Is(err, errEndpointGate):
		return txStep{Refuse: causeEndpointGate}
	case err != nil:
		return txStep{Fail: causeCallError, Err: err}
	case probeErr != nil && f.work != nil:
		return txStep{Refuse: causeWorktree, Err: probeErr, Facts: effectFacts{Work: f.work}}
	case probeErr != nil:
		return txStep{Refuse: causeLivenessUnknown, Err: probeErr}
	case !session.IsSessionNameSyntaxValid(strings.TrimSpace(v.Row.SessionNameMetadata)):
		return txStep{Refuse: causeSessionName}
	case session.HoldsInfo(v.Row, v.Now).BlocksConsume() || session.IsKillPendingInfo(v.Row, v.Now): // TODO(R7): Disposition
		return txStep{Refuse: causeHeld}
	case v.RT.Class != rtAbsent:
		return txStep{Refuse: causeRuntimePresent}
	}
	res, _ := v.World.Templates.lookup(v.Row)
	_, _, patch := preWakePatch(v.Row, v.Now, f.token)
	for _, more := range []session.MetadataPatch{f.bind, stopVoidResiduePatch(), f.repair, f.reset} {
		maps.Copy(patch, more)
	}
	next := foldInfo(v.Row, patch)
	transcript := f.transcript
	if next.SessionKey == "" && res.TP.ResolvedProvider != nil && res.TP.ResolvedProvider.SessionIDFlag != "" && f.key != "" {
		patch["session_key"], transcript = f.key, sessTranscriptAbsent
		next = foldInfo(next, session.MetadataPatch{"session_key": f.key})
	}
	return txStep{Write: patch, Facts: effectFacts{Work: f.work}, Pass: launchPass{launchRow: launchRow{w: v.World, row: next, tp: res.TP}, transcript: transcript, ticket: ticket}}
}

// startOutcome is the launch Call's typed output for the commit.
type startOutcome struct {
	err      error
	prepared *preparedStart
	mcp      session.MetadataPatch
	health   string
	accrued  bool
}

// startLaunch is the launch Call: prepare of the PreWaked row, RouteACP, the
// routed leaf's FreshOnly Start (LL6, I24) by the deadline less its slack,
// the stale-key wait after a start with a key, and the commit's reads.
func startLaunch(ctx context.Context, c txCaps, in launchPass) (startOutcome, error) {
	prepared, err := prepareRow(c.reads, in.w, in.row, in.tp, in.transcript)
	if err != nil {
		return startOutcome{}, err
	}
	name := strings.TrimSpace(in.row.SessionName)
	if router, ok := c.start.sp.(interface{ RouteACP(string) }); ok && in.tp.IsACP {
		router.RouteACP(name)
	}
	leaf, _, _ := runtime.ResolveBackend(c.start.sp, name)
	prepared.cfg.FreshOnly = true
	startCtx, stop := c.start.clock.WithDeadline(ctx, c.it.Deadline.Add(-startDeadlineSlack))
	out := startOutcome{err: leaf.Start(startCtx, name, prepared.cfg), prepared: prepared}
	stop()
	in.ticket.verdict = verdictInconclusive
	switch {
	case runtime.IsProviderCapacity(out.err):
		in.ticket.verdict = verdictCapacity
	case out.err == nil && strings.TrimSpace(in.row.SessionKey) != "":
		t := c.start.clock.NewTimer(staleKeyDetectDelay)
		select {
		case <-t.C():
		case <-ctx.Done():
		}
		t.Stop()
	}
	if out.mcp, err = startCommitMCPPatch(prepared, in.row); err == nil {
		err = session.PersistRuntimeMCPServersSnapshot(prepared.cfg.Env["GC_CITY_PATH"], in.row.ID, prepared.cfg.MCPServers)
	}
	if err != nil {
		return out, err // C5a2: the runtime may be up; the next pass adopts it
	}
	out.health = startupHealthEpisodeKey(in.row, name)
	out.accrued = startupHealthRecord{store: c.reads.city}.accrued(out.health)
	return out, nil
}

// commitStep is the commit's Decide on the read after the Start: only a
// runtime alive with the PreWake token commits (I3), with the MCP keys, the
// #46 mirror cleared and session.woke; a death is died during startup (S3).
func commitStep(v txView) txStep {
	out, err := callResult[startOutcome](v)
	switch {
	case err != nil:
		return txStep{Fail: causeCallError, Err: err}
	case errors.Is(out.err, runtime.ErrSessionInitializing) || errors.Is(out.err, runtime.ErrRuntimeUnavailable):
		return txStep{Fail: causeStartDeferred, Err: out.err}
	case runtime.IsProviderCapacity(out.err):
		return txStep{Fail: causeCapacity, Err: out.err}
	}
	died := out.err == nil || errors.Is(out.err, runtime.ErrSessionDiedDuringStartup)
	switch {
	case v.RT.Alive() && compareIdentity(v.Row, v.RT.Identity) == identityCurrent: // whatever S2's states the row is in
	case died && (v.RT.Class == rtAbsent || v.RT.Class == rtCorpse || v.RT.Class == rtZombie):
		return txStep{Fail: causeDiedDuringStartup, Err: out.err} // C5a2's abandon class
	case errors.Is(out.err, runtime.ErrSessionExists) && (v.RT.Class == rtCorpse || v.RT.Class == rtZombie):
		return txStep{Refuse: causeHeldDead}
	case out.err != nil && !errors.Is(out.err, runtime.ErrSessionExists):
		return txStep{Fail: causeStartError, Err: out.err}
	default:
		return txStep{Refuse: verbStep(true)(v, nil, nil).Refuse}
	}
	patch := startCommitPatch(out.prepared, v.Row, v.Now)
	maps.Copy(patch, out.mcp)
	if out.accrued {
		patch[startupHealthActiveCountMetadataKey], patch[startupHealthActiveKindMetadataKey] = "0", ""
	}
	res, _ := v.World.Templates.lookup(v.Row)
	woke := events.Event{Type: events.SessionWoke, Actor: "gc", Subject: res.TP.DisplayName(), SessionID: v.Row.ID}
	noted := &notedRuntime{Name: strings.TrimSpace(v.Row.SessionName), At: v.Now}
	return txStep{Write: patch, Facts: effectFacts{Events: []events.Event{woke}, Noted: noted}, Pass: landedCommit{out: out, name: noted.Name, display: res.TP.DisplayName()}}
}

// landedCommit is what a landed commit hands the after-commit Call.
type landedCommit struct {
	out           startOutcome
	name, display string
}

// afterCommit clears the #46 episode (a lost clear only delays, v5 §13) and
// records legacy's start telemetry; it never fails the effect.
func afterCommit(_ context.Context, c txCaps, in landedCommit) (struct{}, error) {
	if in.out.accrued {
		_ = c.episode.clear(in.out.health)
	}
	telemetry.RecordAgentStart(context.Background(), in.name, in.display, nil)
	return struct{}{}, nil
}
