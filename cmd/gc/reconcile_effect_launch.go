package main

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"strconv"
	"strings"
	"sync"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/config"
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
// FactsCommit premise) and a Call that clears the #46 episode; a start that
// did not commit, its abandon (reconcile_effect_abandon.go).

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
	called(0, section{Premise: premiseOwnToken, Decide: commitStep}, afterStart),
	called(0, section{Premise: premiseOwnToken, Decide: abandonStep}, stopOwnRuntime),
	{Premise: premiseClose, Decide: rollbackClose},
}

// launchRow is the row, template and pass a Launch's Calls act on.
type launchRow struct {
	w     *World
	row   session.Info
	tp    TemplateParams
	agent *config.Agent // whose side effects the launch installs
}

// startTicket is the endpoint ticket the admit Call took, the verdict the
// launch leaves for it, and the verb section's row and template, on which
// the Launch's later sections key (a PreWake binding changes the row's
// template memo key).
type startTicket struct {
	t       *capacityTicket
	verdict startVerdict
	launch  launchRow
}

// admitTicket admits the endpoint ticket at the intent's endpoint, the one
// the planner gated it on, and resolves it at every exit: success once the
// commit lands.
func admitTicket(_ context.Context, c txCaps, in launchRow) (*startTicket, error) {
	t, ok := c.start.capacity.Admit(c.it.Endpoint, in.row.ID, startCandidate{info: in.row, tp: in.tp}.logicalTemplate(in.w.Env.Cfg))
	if !ok {
		return nil, errEndpointGate
	}
	st := &startTicket{t: t, verdict: verdictNotAttempted, launch: in}
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
	ticket, err := callResult[*startTicket](v)
	if err != nil {
		return preWakeFold{}, nil // Decide refuses on the ticket
	}
	tp := ticket.launch.tp
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
	base := foldInfo(v.Row, v.It.folded()) // a dead committed row's heal and accrual (A6 item 4)
	_, _, patch, class := preWakePatch(base, v.Now, f.token)
	maps.Copy(patch, class.Fold())
	next := foldInfo(foldInfo(base, patch), f.bind)
	f.repair = concretePoolWorkDirRepairPatch(startCandidate{info: next, tp: tp}, v.World.CityPath, v.World.Env.Cfg)
	next = foldInfo(next, f.repair)
	if f.transcript = transcriptOf(r, v.World, next, tp); f.transcript == sessTranscriptAbsent {
		f.reset = clearStaleResumeKeyMetadata("", nil) // its patch only
	}
	return f, nil
}

// launchPass is what the PreWake hands the launch Call: bound is the row
// its template resolves on, bound but not yet woken, as legacy binds and
// resolves at the tick before its PreWake (a pool seat's alias defers on a
// creating row).
type launchPass struct {
	launchRow
	bound      session.Info
	transcript sessTranscriptState
	ticket     *startTicket
	undo       preWakeUndo // what the PreWake wrote, for a capacity refusal's restore (S3)
	relaunched bool        // the PreWake found the row creating (#7570)
}

// preWakeStep is the PreWake's Decide: the ticket admitted, START-012's
// name, no hold (preWakeRefused) or kill fence, the runtime still absent; then
// PreWakePatch with the binding, the stop request cleared (D1), the repair,
// the stale-resume clear with its continuation rotation (legacy's
// commitPendingContinuationReset at the start) and the set-once key fill
// (R1). The Launch runs the verb section's template with its trigger env
// taken from the bound row.
func preWakeStep(v txView, f preWakeFold, probeErr error) txStep {
	ticket, err := callResult[*startTicket](v)
	switch {
	case errors.Is(err, errEndpointGate):
		return healInstead(v, txStep{Refuse: causeEndpointGate})
	case err != nil:
		return txStep{Fail: causeCallError, Err: err}
	case probeErr != nil && f.work != nil:
		return healInstead(v, txStep{Refuse: causeWorktree, Err: probeErr, Facts: effectFacts{Work: f.work}})
	case probeErr != nil:
		return txStep{Refuse: causeLivenessUnknown, Err: probeErr}
	case !session.IsSessionNameSyntaxValid(strings.TrimSpace(v.Row.SessionNameMetadata)):
		return healInstead(v, txStep{Refuse: causeSessionName})
	case preWakeRefused(v) || session.IsKillPendingInfo(v.Row, v.Now): // the hold the intent's own patch writes too
		return healInstead(v, txStep{Refuse: causeHeld})
	case v.RT.Class != rtAbsent:
		return txStep{Refuse: causeRuntimePresent}
	}
	tp := ticket.launch.tp
	base := foldInfo(v.Row, v.It.folded())
	patch := maps.Clone(v.It.Patch)
	if patch == nil {
		patch = session.MetadataPatch{}
	}
	_, _, wake, wakeClass := preWakePatch(base, v.Now, f.token)
	for _, more := range []session.MetadataPatch{wake, f.bind, stopVoidResiduePatch(), f.repair, f.reset} {
		maps.Copy(patch, more)
	}
	// The heal's cells and then the PreWake cell, minted at the write.
	class := v.It.Class.With(wakeClass)
	if len(f.reset) > 0 {
		epoch, _ := strconv.Atoi(patch["continuation_epoch"])
		patch["continuation_epoch"], patch["continuation_reset_pending"] = strconv.Itoa(epoch+1), ""
	}
	next := foldInfo(foldInfo(v.Row, patch), class.Fold())
	transcript := f.transcript
	if next.SessionKey == "" && tp.ResolvedProvider != nil && tp.ResolvedProvider.SessionIDFlag != "" && f.key != "" {
		patch["session_key"], transcript = f.key, sessTranscriptAbsent
		next = foldInfo(next, session.MetadataPatch{"session_key": f.key})
	}
	launch := launchRow{w: v.World, row: next, tp: boundTemplate(tp, next), agent: ticket.launch.agent}
	pass := launchPass{launchRow: launch, bound: foldInfo(base, f.bind), transcript: transcript, ticket: ticket, undo: preWakeUndoOf(v, foldClass(patch, class), len(f.reset) > 0)}
	pass.undo.token, pass.relaunched = f.token, creating(v.Row)
	return txStep{Write: patch, Class: class, Facts: effectFacts{Work: f.work}, Pass: pass}
}

// preWakeUndoOf is what a failed launch restores (S3) of the PreWake
// patch wrote on v's row: the row as the part of its patch the start keeps
// left it (intent.Kept: the heal it carries, as legacy's restore finds the
// row its tick healed first; a stranded seat's death accrual alone, which
// keeps it committed). A stale key's reset the PreWake consumed (staleKey)
// stays pending, so the next start rotates the continuation again; any other
// epoch bump is restored as legacy restores it.
func preWakeUndoOf(v txView, patch session.MetadataPatch, staleKey bool) preWakeUndo {
	row, meta := v.Row, v.Meta
	if len(v.It.Kept) > 0 {
		row, meta = foldInfo(row, v.It.Kept), v.It.Kept.Apply(meta)
	}
	u := newPreWakeUndo(row, meta, patch)
	if v, ok := u.written["continuation_reset_pending"]; ok && v == "" && staleKey {
		u.restore["continuation_reset_pending"] = "true"
	}
	return u
}

// preWakeRefused reports a hold that refuses v's PreWake (S1: not held or
// quarantined), over the row as the intent's own patch leaves it: the
// disposition's RefusesPreWake. A wait does not refuse: the allocation wakes a
// wait-held row whose wait is ready, and the hold clears only once its nudge
// reaches the running session (POOL-077/080); nor does an unparseable timer,
// which no heal clears. A17's respawn is not held by the agent heartbeat's
// hold it respawns through (v5.9i), which the premise holds the fresh row to.
func preWakeRefused(v txView) bool {
	d := session.DispositionOfInfo(foldInfo(v.Row, v.It.folded()))
	if v.It.Reason == decideHeartbeatRespawn {
		return d.RefusesHeartbeatRespawn(v.Now)
	}
	return d.RefusesPreWake(v.Now)
}

// triggerEnvKeys are the template env keys a row's trigger stamps
// (resolveTemplateForSessionBeadInfo, sessionTriggerBeadEnv).
var triggerEnvKeys = []string{"GC_SPAWN_ORIGIN", "GC_TRIGGER_BEAD_ID", "GC_TRIGGER_WORK_BEAD_ID", "GC_TRIGGER_BEAD_STORE_REF", "GC_TRIGGER_WORK_STORE_REF"}

// boundTemplate is tp with its trigger env derived from row, as legacy binds
// and then resolves: none when the row has no trigger.
func boundTemplate(tp TemplateParams, row session.Info) TemplateParams {
	env := maps.Clone(tp.Env)
	for _, k := range triggerEnvKeys {
		delete(env, k)
	}
	if trigger := sessionTriggerBeadEnv(row); trigger != nil {
		if env == nil {
			env = make(map[string]string, len(trigger)+1)
		}
		env["GC_SPAWN_ORIGIN"] = "demand"
		maps.Copy(env, trigger)
	}
	tp.Env = env
	return tp
}

// startOutcome is the launch Call's typed output for the commit.
type startOutcome struct {
	err        error
	prepErr    error          // prepare failed, before the Start (S3's prepare class)
	undo       preWakeUndo    // the launch's, for the abandon
	relaunched bool           // the launch's
	tp         TemplateParams // the template launched
	paced      bool           // a capacity refusal the endpoint breaker paces: admitted on a ticket, the poison valve not tripped
	prepared   *preparedStart
	mcp        session.MetadataPatch
	health     string
	accrued    bool
}

// startLaunch is the launch Call: prepare of the PreWaked row, RouteACP, the
// agent's side effects (hooks and ACP route, as legacy installs them every
// tick), the routed leaf's FreshOnly Start (LL6, I24) by the deadline less
// its slack, the stale-key wait after a start with a key, and the commit's
// reads.
func startLaunch(ctx context.Context, c txCaps, in launchPass) (startOutcome, error) {
	switch res, ok := in.w.Templates.launchTemplate(in.bound, c.start.clock.Now()); { // at the tick (its prompt's beacon)
	case ok:
		in.tp, in.agent = res.TP, res.Agent
	case res.Err != nil && c.start.stderr != nil:
		fmt.Fprintf(c.start.stderr, "v2 start %s: resolving its template at launch: %v; launching the pass's\n", in.row.ID, res.Err) //nolint:errcheck // best-effort stderr
	}
	// session.woke once the commit has landed, as legacy records it after
	// its commit: at the effect's end, whether or not afterCommit ran.
	woke := events.Event{Type: events.SessionWoke, Actor: "gc", Subject: in.tp.DisplayName(), SessionID: in.row.ID}
	c.onExit(func(final settlement) {
		if final.Outcome == settledLanded && c.start.rec != nil {
			c.start.rec.Record(woke)
		}
	})
	prepared, err := prepareRow(c.reads, in.w, in.row, in.tp, in.transcript)
	if err != nil {
		return startOutcome{prepErr: err, undo: in.undo, relaunched: in.relaunched, tp: in.tp}, nil
	}
	name := strings.TrimSpace(in.row.SessionName)
	if router, ok := c.start.sp.(interface{ RouteACP(string) }); ok && in.tp.IsACP {
		router.RouteACP(name)
	}
	leaf, _, _ := runtime.ResolveBackend(c.start.sp, name)
	prepared.cfg.FreshOnly = true
	in.w.Templates.installSideEffects(in.agent, in.tp)
	if !beginStart(ctx) {
		return startOutcome{}, context.Cause(ctx) // the shutdown came first: never begin a start after it
	}
	startCtx, stop := c.start.clock.WithDeadline(ctx, c.it.Deadline.Add(-startDeadlineSlack-startFailureSlack))
	out := startOutcome{err: leaf.Start(startCtx, name, prepared.cfg), undo: in.undo, relaunched: in.relaunched, tp: in.tp, prepared: prepared}
	stop()
	in.ticket.verdict = verdictInconclusive
	switch {
	case runtime.IsProviderCapacity(out.err):
		in.ticket.verdict = verdictCapacity
		out.paced = in.ticket.t != nil && !in.ticket.t.Resolve(verdictCapacity) // resolved now, for the valve's verdict
	case out.err == nil && strings.TrimSpace(in.row.SessionKey) != "":
		t := c.start.clock.NewTimer(staleKeyDetectDelay)
		select {
		case <-t.C():
		case <-ctx.Done():
		}
		t.Stop()
	}
	if out.mcp, err = startCommitMCPPatch(prepared, in.row); err == nil && out.err == nil { // legacy persists on success only
		err = session.PersistRuntimeMCPServersSnapshot(prepared.cfg.Env["GC_CITY_PATH"], in.row.ID, prepared.cfg.MCPServers)
	}
	if err != nil {
		return out, err // the runtime may be up: S1 adopts it next pass (S3)
	}
	out.health = startupHealthEpisodeKey(in.row, name)
	out.accrued = startupHealthRecord{store: c.reads.city}.accrued(out.health)
	return out, nil
}

// outlivesShutdown is ctx for a start's Call (callWatched, before its lease
// watch), with ctx's values, ended with ctx's cause by any end of ctx (a
// provider swap, the effect's own deadline, a stop that does not preserve
// sessions), except that a preserve-sessions shutdown (errEffectShutdown)
// that lands after the provider's Start began (beginStart) is ignored. A
// provider's failed start kills the runtime it launched (tmux's
// cleanupFailedStart), so a start such a shutdown canceled mid-launch would
// kill its own runtime and the next boot would relaunch it under a new token
// instead of adopting it (§12.2 #4, START-024; staging J35 A/B). The lease
// watch, taken on the result, still ends the start on a lost lease, and
// the effect's deadline, armed on the result, ends it on time. The
// executor's closed write latch keeps a start that finishes during shutdown
// from writing. release stops the relay and the deadline.
func outlivesShutdown(ctx context.Context) (context.Context, func()) {
	g := &startGate{parent: ctx}
	// The effect's deadline is armed again on the detached context, so it
	// holds after the relay has spent itself on a preserve-sessions shutdown.
	base := context.WithoutCancel(ctx)
	var stopDeadline context.CancelFunc = func() {}
	if d, ok := ctx.Deadline(); ok {
		base, stopDeadline = context.WithDeadline(base, d)
	}
	detached, cancel := context.WithCancelCause(context.WithValue(base, startGateKey{}, g))
	g.cancel = cancel
	unrelay := context.AfterFunc(ctx, func() {
		g.mu.Lock()
		defer g.mu.Unlock()
		if cause := context.Cause(ctx); !g.begun || !errors.Is(cause, errEffectShutdown) {
			cancel(cause)
		}
	})
	return detached, func() { unrelay(); cancel(context.Canceled); stopDeadline() }
}

// startGate is outlivesShutdown's record of whether the Start began.
type startGate struct {
	mu     sync.Mutex
	parent context.Context
	cancel context.CancelCauseFunc
	begun  bool
}

type startGateKey struct{}

// beginStart reports whether a start may begin on ctx, and from then on lets
// it outlive a preserve-sessions shutdown: false once ctx, or the context
// outlivesShutdown detached it from, has ended, so no Start begins after the
// shutdown deadline.
func beginStart(ctx context.Context) bool {
	g, ok := ctx.Value(startGateKey{}).(*startGate)
	if !ok {
		return ctx.Err() == nil
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.parent.Err() != nil {
		g.cancel(context.Cause(g.parent))
	}
	if ctx.Err() != nil {
		return false
	}
	g.begun = true
	return true
}

// commitStep is the commit's Decide on the read after the Start: only a
// runtime alive with the PreWake token commits (I3), whatever the Start
// returned, with the MCP keys and the #46 mirror cleared (the launch Call
// records session.woke once it lands). A start that did not commit is S3's:
// a deferral writes nothing; a capacity refusal restores the PreWake here;
// the other classes go to the failure Call and the abandon (failStart).
func commitStep(v txView) txStep {
	out, err := callResult[startOutcome](v)
	if err != nil {
		return txStep{Fail: causeCallError, Err: err}
	}
	if out.prepErr != nil {
		return failStart(v, out, causePrepare, out.prepErr)
	}
	died := out.err == nil || errors.Is(out.err, runtime.ErrSessionDiedDuringStartup)
	cause := runtimeCause(v.Row, v.RT)
	switch {
	case cause == "": // whatever S2's states the row is in
	case errors.Is(out.err, runtime.ErrSessionInitializing) || errors.Is(out.err, runtime.ErrRuntimeUnavailable):
		return txStep{Fail: causeStartDeferred, Err: out.err}
	case runtime.IsProviderCapacity(out.err) && out.paced:
		patch, class := restorePatch(out.undo, v.Meta)
		return txStep{Write: patch, Class: class, Fail: causeCapacity, Err: out.err}
	case runtime.IsProviderCapacity(out.err): // no ticket, or the poison valve blamed this template: legacy's failure arms, which back off
		return failStart(v, out, causeStartError, out.err)
	case died && (v.RT.Class == rtAbsent || v.RT.Class == rtCorpse || v.RT.Class == rtZombie):
		return failStart(v, out, causeDiedDuringStartup, out.err)
	case errors.Is(out.err, runtime.ErrSessionExists) && (v.RT.Class == rtCorpse || v.RT.Class == rtZombie):
		return txStep{Refuse: causeHeldDead}
	case out.err != nil && !errors.Is(out.err, runtime.ErrSessionExists) && cause == causeAttribution && v.Row.PendingCreateClaim:
		return failStart(v, out, causeAttribution, out.err) // deferred, with its #46 accrual
	case out.err != nil && !errors.Is(out.err, runtime.ErrSessionExists):
		return failStart(v, out, causeStartError, out.err)
	default:
		return txStep{Refuse: cause}
	}
	patch := startCommit(out.prepared, v.Row, v.Now)
	maps.Copy(patch, out.mcp)
	if out.accrued {
		patch[startupHealthActiveCountMetadataKey], patch[startupHealthActiveKindMetadataKey] = "0", ""
	}
	noted := &notedRuntime{Name: strings.TrimSpace(v.Row.SessionName), At: v.Now}
	return txStep{Write: patch, Last: true, Facts: effectFacts{Noted: noted}, Pass: startEnd{landed: &landedCommit{out: out, name: noted.Name, display: out.tp.DisplayName()}}}
}

// landedCommit is what a landed commit hands the after-commit Call.
type landedCommit struct {
	out           startOutcome
	name, display string
}

// afterStart is the commit section's Call: after a landed commit it clears
// the #46 episode (a lost clear only delays, v5 §13) and records legacy's
// start telemetry, and never fails the effect; after a start that did not
// commit, the failure's reads and #46 accrual (readStartFailure).
func afterStart(ctx context.Context, c txCaps, in startEnd) (startFailureRead, error) {
	if in.failed != nil {
		telemetry.RecordAgentStart(context.Background(), in.failed.name, in.failed.display, in.failed.err)
		return readStartFailure(ctx, c, *in.failed), nil
	}
	if in.landed.out.accrued {
		_ = c.episode.clear(in.landed.out.health)
	}
	telemetry.RecordAgentStart(context.Background(), in.landed.name, in.landed.display, nil)
	return startFailureRead{}, nil
}
