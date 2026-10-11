package main

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"strconv"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
)

// The start's abandon table and C3's rollback (CONTRACT v5 S3, C3, S7; plan
// C5a2). A start that did not commit writes its class's patch by CAS under
// its own token and still settles failed with its cause (an abandon,
// txStep.Fail with a Write):
//   - a capacity refusal restores what PreWake wrote (commitStep);
//   - a prepare failure clears last_woke_at and accrues #46;
//   - a terminal provider error parks the row (START-028); a rate-limit
//     screen quarantines it (START-022/029);
//   - a pending create tears down its own runtime and rolls the row back,
//     accruing #46 (START-031/033); a configured named one that did not
//     die keeps its row and clears last_woke_at;
//   - any other failure restores PreWake, unless PreWake found the row
//     already creating (#7570's kept marker), and accrues a wake failure,
//     resetting the conversation unless its transcript is on disk
//     (START-035).
//
// A deferred outcome, an MCP persist error and a commit-time error write
// nothing: S1 adopts or launches next pass. A10's rollback is the same
// teardown and close.

// startFailure is a start that did not commit, as its commit's Decide read
// it, for the failure Call (afterStart) and the abandon.
type startFailure struct {
	cause, terminal string // the settlement's cause; the error's terminal reason (START-028)
	err             error
	pending         bool // a pending create
	configured      bool // a configured named row with its spec
	died            bool // died during startup
	peek            bool // a rate-limit screen may show: the pane is listed (START-022)
	relaunched      bool // PreWake found the row creating (#7570): its marker stays
	undo            preWakeUndo
	row             session.Info // the fresh row, for the conversation probe
	name, display   string       // the runtime name; the agent, for the quarantine metric
	episode         string       // the #46 episode key
}

// startFailureRead is what the failure Call read, past what it accrued.
type startFailureRead struct {
	startFailure
	rateLimit bool // the pane shows a provider rate-limit screen
	keeps     bool // the conversation's transcript is on disk (wakeFailureKeepsConversation)
}

// startEnd is what the commit hands its Call: the landed commit, or the
// start that failed.
type startEnd struct {
	landed *landedCommit
	failed *startFailure
}

// failStart is the start that did not commit with cause, handed to the
// failure Call and then to the abandon.
func failStart(v txView, out startOutcome, cause string, err error) txStep {
	name := strings.TrimSpace(v.Row.SessionName)
	if err == nil { // a nil Start whose runtime died: legacy's error
		err = fmt.Errorf("session %q died during startup", name)
	}
	f := startFailure{
		cause: cause, err: err, pending: v.Row.PendingCreateClaim, died: cause == causeDiedDuringStartup, relaunched: out.relaunched, undo: out.undo,
		row: v.Row, name: name, display: out.tp.DisplayName(), episode: startupHealthEpisodeKey(v.Row, name),
	}
	started := cause != causePrepare && cause != causeAttribution // a Start's own failure, which the provider screens
	if started {
		f.terminal = runtime.ProviderTerminalErrorReason(err.Error())
	}
	if env := v.World.Env; env != nil && env.Cfg != nil {
		f.configured = configuredNamedSessionBeadHasSpecInfo(v.Row, env.Cfg, config.EffectiveCityName(env.Cfg, ""))
		f.peek = started && f.terminal == "" && env.Cfg.Session.Provider != "subprocess" && v.RT.Class != rtAbsent
	}
	return txStep{Pass: startEnd{failed: &f}}
}

// readStartFailure is the failure Call's reads and the #46 accrual (a lost
// accrual only delays its quarantine, v5 §13): the screen peek, and for the
// classes that accrue a wake failure the transcript probe.
func readStartFailure(ctx context.Context, c txCaps, f startFailure) startFailureRead {
	r := startFailureRead{startFailure: f}
	if f.peek {
		r.rateLimit = runtime.ContainsProviderRateLimitScreen(peekPane(ctx, c.start.sp, f.name))
	}
	switch {
	case r.rateLimit, f.terminal != "", f.cause == causeAttribution && errors.Is(f.err, runtime.ErrSessionExists):
	case f.pending, f.cause == causePrepare, f.cause == causeAttribution:
		_ = c.episode.fail(f.episode, f.err, c.start.clock.Now())
	default:
		r.keeps = wakeFailureKeepsConversation(f.row)
	}
	return r
}

// peekPane is name's pane, bounded; a peek that fails or does not answer
// reads empty.
func peekPane(ctx context.Context, sp runtime.Provider, name string) string {
	leaf, _, known := runtime.ResolveBackend(sp, name)
	if sp == nil || !known || leaf == nil {
		return ""
	}
	content, _ := boundedProbeCtx(ctx, func(context.Context) string {
		s, err := leaf.Peek(name, rateLimitPeekLines)
		if err != nil {
			return ""
		}
		return s
	})
	return content
}

// abandonStep is S3's abandon on the fresh row under the start's own token
// (the premise): the class's patch, written with the start's failure. A
// pending create's rollback goes on to its teardown and close.
func abandonStep(v txView) txStep {
	f, err := callResult[startFailureRead](v)
	if err != nil {
		return txStep{Fail: causeCallError, Err: err}
	}
	abandon := func(patch session.MetadataPatch, facts effectFacts) txStep {
		return txStep{Write: patch, Fail: f.cause, Err: f.err, Facts: facts}
	}
	var facts effectFacts
	if f.pending && f.terminal == "" && errors.Is(f.err, context.DeadlineExceeded) {
		facts.Events = []events.Event{{Type: events.SessionColdStartTimeout, Actor: "controller", Subject: f.name, Message: fmt.Sprintf("session %q cold start timed out", f.name)}}
	}
	switch {
	case f.cause == causeAttribution: // deferred: S1 attributes it next pass
		return txStep{Refuse: causeAttribution, Err: f.err}
	case f.terminal != "" && !f.pending:
		return abandon(providerTerminalErrorPatch(f.terminal, v.Now), facts)
	case f.rateLimit:
		return abandon(rateLimitQuarantine(v.Row, v.Now), facts)
	case f.cause == causePrepare, f.pending && f.terminal == "" && f.configured && !f.died:
		return abandon(session.MetadataPatch{"last_woke_at": ""}, facts)
	case f.pending:
		td, cause := ownTeardown(v)
		if cause != "" {
			return txStep{Refuse: cause, Err: f.err, Facts: facts}
		}
		td.fail, td.err = f.cause, f.err
		if f.terminal != "" {
			td.more = providerTerminalErrorPatch(f.terminal, v.Now)
		}
		return txStep{Pass: td, Facts: facts}
	}
	patch, quarantined := otherFailurePatch(v.Row, v.Meta, f, v.Now)
	if quarantined {
		facts.Quarantined = f.display
	}
	return abandon(patch, facts)
}

// otherFailurePatch is S3's any-other class: PreWake restored, unless it
// found the row creating (#7570), then legacy's recordWakeFailure on a row
// still creating (an operator who killed it mid-start owns it): the
// conversation reset unless its transcript is on disk (unprobeable
// providers included), and one wake attempt more, quarantined at the
// threshold.
func otherFailurePatch(row session.Info, meta map[string]string, f startFailureRead, now time.Time) (_ session.MetadataPatch, quarantined bool) {
	patch := session.MetadataPatch{}
	if !f.relaunched {
		patch = restorePatch(f.undo, meta)
	}
	if !creating(row) {
		return patch, false
	}
	if (row.SessionKey != "" || row.StartedConfigHash != "") && !f.keeps {
		maps.Copy(patch, session.ConversationResetPatch(true))
	}
	attempts, _ := strconv.Atoi(row.WakeAttemptsMetadata)
	accrual := session.WakeFailureAccrualPatch(attempts, defaultMaxWakeAttempts, now.Add(defaultQuarantineDuration))
	maps.Copy(patch, accrual.Patch)
	maps.Copy(patch, accrual.Class(row, now).Fold())
	return patch, accrual.Quarantined
}

// restorePatch is restorePreWakeState's decide on the fresh row's
// metadata: each key PreWake wrote goes back while it still holds PreWake's
// value, so anything written since wins.
func restorePatch(u preWakeUndo, meta map[string]string) session.MetadataPatch {
	patch := make(session.MetadataPatch, len(u.restore))
	for key, value := range u.restore {
		if written := u.written[key]; meta[key] == written && value != written {
			patch[key] = value
		}
	}
	// The PreWake cell's undo, folded (the v2 deciders cannot mint).
	maps.Copy(patch, session.UndoDisposition(u.before, u.class, meta).Fold())
	return patch
}

// teardown is a rollback's own-runtime stop, when it has one, and what its
// close settles with.
type teardown struct {
	stop  *fenceVerdict // L1 alone on the own runtime (the rollback exception, F2); nil: nothing to stop
	names []string      // its template's process names, for C8.8
	fail  string        // the start's failure the close settles with; "" for A10's rollback
	err   error
	more  session.MetadataPatch // folded under the failed-create close: a terminal mark (START-028)
}

// ownTeardown is the teardown of v's pending create on its fresh read, or
// the cause that holds the rollback (rollbackRuntime).
func ownTeardown(v txView) (teardown, string) {
	stop, cause := rollbackRuntime(v.Row, v.RT)
	if cause != "" || !stop {
		return teardown{}, cause
	}
	verdict := ownRuntimeFence(v.RT, v.Row)
	return teardown{stop: &verdict, names: processNamesFor(v.World, v.Row)}, ""
}

// rollbackRuntime is C3's attribution of a pending create's fresh read: stop
// for its own runtime (O2's own-runtime test, or C8.2(a)'s bead-scoped name,
// stopped whatever its identity, SESS-091); neither for nothing running, a
// corpse (C8.8 counts it stopped) or another row's runtime (F3: rolled back
// without a stop, a stale epoch included, S7); else the cause that holds.
//
// TODO(C5a3): the corpse a rollback counts stopped stays listed (legacy
// stops the name); C5a3's corpse removal takes it, so a crash-looping pool
// create's corpses do not pile up (§12.2 #45 does not cover them).
func rollbackRuntime(row session.Info, rt *txRuntime) (stop bool, cause string) {
	switch rt.Class {
	case rtUnsupported:
		return false, causeLivenessUnsupported
	case rtAbsent, rtCorpse:
		return false, ""
	case rtUnknown:
		return false, causeLivenessUnknown
	}
	v := compareIdentity(row, rt.Identity)
	switch {
	case ownsName(row, row.SessionName), ownRuntime(v, row, rt.Identity):
		return true, ""
	case v == identityForeign, v == identityStaleSelf:
		return false, ""
	case v == identityNewerSelf:
		return false, causeNewerSelf
	}
	return false, causeAttribution
}

// stopOwnRuntime is the rollback's Call: the own runtime's fenced stop,
// confirmed (C8.8), when the teardown has one. The close reads the runtime
// fresh again, and holds the row open while its own runtime stays (ga-vcjr9).
func stopOwnRuntime(ctx context.Context, c txCaps, in teardown) (teardown, error) {
	if in.stop != nil {
		stopFenced(ctx, c.start.sp, *in.stop, in.names, c.start.clock.Now)
	}
	return in, nil
}

// rollbackClose is C3's rollback, the failed-create close, while the fresh
// row may still roll back (CanRollback) and its fresh read finds no runtime
// of its own.
func rollbackClose(v txView) txStep {
	td, err := callResult[teardown](v)
	stop, cause := rollbackRuntime(v.Row, v.RT)
	switch {
	case err != nil:
		return txStep{Fail: causeCallError, Err: err}
	case !rollbackable(v.Row):
		return txStep{Refuse: causeRedecided}
	case cause != "":
		return txStep{Refuse: cause}
	case stop:
		return txStep{Refuse: causeRuntimePresent}
	}
	return txStep{Terminal: rollbackPatch(v.Row, v.Now, td.more), Fail: td.fail, Err: td.err}
}

// rollbackable is CanRollback on the row the premise holds to the expected
// incarnation: open, not active, and still claimed (or failed-create, a
// partial rollback's retry).
func rollbackable(row session.Info) bool {
	return session.LeaseFromInfo(row).CanRollback(session.LeaseFromInfo(row))
}

// rateLimitQuarantine is legacy's recordRateLimitQuarantine for row at now: its
// lifecycle and the Quarantine cell's quarantined_until, folded (the v2
// deciders cannot mint; the write admits it).
func rateLimitQuarantine(row session.Info, now time.Time) session.MetadataPatch {
	patch := session.RateLimitQuarantinePatch()
	maps.Copy(patch, session.CellInfo(row, session.ActorController, session.DispEvent{Kind: session.EvQuarantine, Until: now.Add(defaultRateLimitQuarantineDuration)}, now).Fold())
	return patch
}

// rollbackPatch is legacy's rollbackPendingCreateClears as one atomic close:
// last_woke_at cleared, more, then failedCreateClosePatch over them, and an
// explicit session name cleared in the same write (legacy clears it in a
// second write once the close landed).
func rollbackPatch(row session.Info, now time.Time, more session.MetadataPatch) session.MetadataPatch {
	patch := session.MetadataPatch{"last_woke_at": ""}
	maps.Copy(patch, more)
	maps.Copy(patch, failedCreateCloseLife(now))
	maps.Copy(patch, session.CellInfo(row, session.ActorController, session.DispEvent{Kind: session.EvRetire}, now).Fold())
	if strings.TrimSpace(row.SessionNameExplicit) == "true" {
		patch["session_name"] = ""
	}
	return patch
}

// rollbackSections are A10's rollback (C3): a screen on its listed dead pane
// parks the row instead (SESS-051/092); else the own runtime's stop, when
// the fresh read finds one, then the close.
var rollbackSections = []section{
	called(0, probed(paneScreen, rollbackStep), stopOwnRuntime),
	{Premise: premiseClose, Decide: rollbackClose},
}

// paneScreen is legacy's checkRateLimitStability on the rollback's fresh
// read: a crash candidate whose pane is listed but not alive is peeked; a
// terminal provider error parks it, a rate-limit screen quarantines it.
func paneScreen(ctx context.Context, _ effectReads, v txView) (session.MetadataPatch, error) {
	if v.RT == nil || v.RT.Class != rtZombie && v.RT.Class != rtCorpse || v.World.Env == nil || v.World.Env.Cfg == nil {
		return nil, nil
	}
	facts := session.ExitFacts{SubprocessProvider: v.World.Env.Cfg.Session.Provider == "subprocess", LastWokeAt: v.Row.LastWokeAt, Now: v.Now, ScreenAvailable: true}
	if session.DecideSessionExit(facts) != session.ExitGatherScreen {
		return nil, nil
	}
	content := peekPane(ctx, v.RT.leaf, strings.TrimSpace(v.Row.SessionName))
	if reason := runtime.ProviderTerminalErrorReason(content); reason != "" {
		return providerTerminalErrorPatch(reason, v.Now), nil
	}
	if facts.Screen = session.ScreenOther; runtime.ContainsProviderRateLimitScreen(content) {
		facts.Screen = session.ScreenRateLimit
	}
	if session.DecideSessionExit(facts) == session.ExitRateLimitQuarantine {
		return rateLimitQuarantine(v.Row, v.Now), nil
	}
	return nil, nil
}

// rollbackStep is the rollback's first Decide: a screen's patch, or the
// teardown of the fresh read's own runtime for the Call.
func rollbackStep(v txView, screen session.MetadataPatch, _ error) txStep {
	td, cause := ownTeardown(v)
	switch {
	case !rollbackable(v.Row):
		return txStep{Refuse: causeRedecided}
	case len(screen) > 0:
		return txStep{Write: screen, Done: true}
	case cause != "":
		return txStep{Refuse: cause}
	}
	return txStep{Pass: td}
}

// retireAround runs a start or a rollback, then, once it closed the row,
// legacy's retired-session cleanup: its waits canceled and its external
// message bindings closed.
func retireAround(_ context.Context, a aroundCaps, run func() settlement) settlement {
	s := run()
	if s.Closed && a.retire != nil {
		a.retire.cleanup(a.it.Key.ID)
	}
	return s
}
