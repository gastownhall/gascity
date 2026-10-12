package main

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/api"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/telemetry"
)

// The dead row of S1's table (CONTRACT v5 S5; plan C5a3): a bringUp whose
// fresh read finds a corpse (present, pane dead) or a zombie (running,
// agent dead) under the row's name classifies the row's own dead runtime
// from its pane, then removes it by exact session object, before PreWake,
// and the verb section after it reads the name gone and launches. Identity
// is read first: the start-recycle exception stops a zombie only when it is
// the row's own (O2's own-runtime test); a corpse is removed under the
// corpse exception whoever it was, but classified only when it is the
// row's own, so no quarantine, terminal mark or crash is attributed to the
// wrong row. In legacy's order (checkRateLimitStability and the zombie
// capture):
//   - a terminal provider error parks the row (SESS-088): no kill, no launch;
//   - a rate-limit screen of a crash candidate quarantines it 30 minutes
//     (SESS-530): no kill, no launch, no accrual;
//   - other output is a crash (SESS-089): session.crashed, recorded once the
//     kill landed, so a recycle that keeps being refused records none twice;
//   - no output: the recycle alone.
//
// A peek that fails classifies nothing and refuses (peek-error). The kill
// keeps L3 (an attached pane refuses, and the start waits for the detach)
// and re-checks the object server-side (KillCorpseObject, KillZombieObject);
// a refused kill leaves the runtime, and the verb section's read refuses
// dead with a row backoff.

// Recycle refusal causes.
const (
	causePeekError    = "peek-error"
	causeAttached     = "attached"
	causeOperatorPane = "operator-pane" // the kill refused: more than one pane or window, or the pane lives again
)

// errRecycleAttached: the recycle's L3 found the pane attached.
var errRecycleAttached = errors.New("v2 start: the dead runtime's pane is attached")

// recycleSections are bringUp's first sections: the classification and the
// kill, then the death's accounting.
var recycleSections = []section{
	calledWhen(callStop, probed(peekDead, recycleStep), deadKill.found, killDead),
	{Decide: deathStep},
}

// deadPeek is what the recycle's Probe read of the row's own dead pane.
type deadPeek struct {
	read    bool // the row's own: classified
	content string
}

// peekDead peeks the dead runtime's pane when it is the row's own; nothing
// otherwise.
func peekDead(ctx context.Context, _ effectReads, v txView) (deadPeek, error) {
	if v.RT == nil || v.RT.Class != rtCorpse && v.RT.Class != rtZombie || !ownRuntime(compareIdentity(v.Row, v.RT.Identity), v.Row, v.RT.Identity) {
		return deadPeek{}, nil
	}
	content, err := v.RT.leaf.Peek(strings.TrimSpace(v.Row.SessionName), rateLimitPeekLines)
	if err != nil {
		return deadPeek{}, err
	}
	return deadPeek{read: true, content: content}, ctx.Err()
}

// deadKill is the dead runtime the recycle's Call removes, the crash it
// records once the kill landed, and whether the death is the row's own to
// account.
type deadKill struct {
	leaf    runtime.Provider
	name    string
	zombie  bool
	live    runtime.Liveness // the read's latest: its object
	now     time.Time
	crash   *events.Event
	account bool
}

// deadKilled is how the recycle's kill ended.
type deadKilled struct {
	killed, blocked, account bool
}

func (k deadKill) found() bool { return k.leaf != nil }

// recycleStep is the dead row's Decide: nothing for a runtime that is not
// dead; the classification's patch, or the kill.
func recycleStep(v txView, peek deadPeek, peekErr error) txStep {
	if v.RT.Class != rtCorpse && v.RT.Class != rtZombie {
		return txStep{Pass: deadKill{}}
	}
	ident := compareIdentity(v.Row, v.RT.Identity)
	own := ownRuntime(ident, v.Row, v.RT.Identity)
	switch {
	case !v.RT.Same:
		return txStep{Refuse: causeLivenessUnknown} // the object changed across the read
	case v.RT.Class == rtZombie && !own:
		return txStep{Refuse: identityCause(v.Row, v.RT.Identity)}
	case own && peekErr != nil:
		return txStep{Refuse: causePeekError, Err: peekErr}
	}
	k := deadKill{leaf: v.RT.leaf, name: strings.TrimSpace(v.Row.SessionName), zombie: v.RT.Class == rtZombie, live: v.RT.Again, now: v.Now, account: own}
	if !peek.read || strings.TrimSpace(peek.content) == "" {
		return txStep{Pass: k}
	}
	if screen, ok := deadScreen(v, peek.content); ok {
		if parkedTerminal(v.Row) && screen.patch["sleep_reason"] == string(session.SleepReasonProviderTerminalError) {
			if strings.TrimSpace(v.Row.WakeRequest) == string(session.WakeCauseExplicit) {
				return txStep{Pass: k} // an operator's wake: the screen it was parked on is recycled
			}
			return txStep{Done: true, Cause: causeDead} // already parked
		}
		return txStep{Write: screen.patch, Class: screen.class, Done: true}
	}
	if !k.zombie || runtime.ContainsProviderRateLimitScreen(peek.content) {
		return txStep{Pass: k} // legacy records a crash for a zombie alone, and never a rate-limit screen's
	}
	res, _ := v.World.Templates.lookup(v.Row)
	k.crash = &events.Event{
		Type: events.SessionCrashed, Actor: "gc", Subject: res.TP.DisplayName(),
		Message: truncateCrashPaneOutput(peek.content, crashEventPaneOutputMaxLines),
		Payload: api.SessionLifecyclePayloadJSON(v.Row.ID, res.TP.TemplateName, "zombie process"),
	}
	return txStep{Pass: k}
}

// deadScreen classifies the row's own dead pane as legacy's zombie capture
// and checkRateLimitStability do: a terminal provider error parks it
// whatever its wake; a rate-limit screen quarantines a crash candidate.
func deadScreen(v txView, content string) (screenWrite, bool) {
	if reason := runtime.ProviderTerminalErrorReason(content); reason != "" {
		return screenWrite{patch: providerTerminalErrorPatch(reason, v.Now)}, true
	}
	if !runtime.ContainsProviderRateLimitScreen(content) {
		return screenWrite{}, false
	}
	_, draining := activeStop(v.World.Census.reread(v.It.Key, v.Row, v.Meta)) // the fresh row's stop keys
	facts := session.ExitFacts{LastWokeAt: v.Row.LastWokeAt, Now: v.Now, ScreenAvailable: true, Screen: session.ScreenRateLimit, DrainPending: draining}
	if cfg := v.World.Env.Cfg; cfg != nil {
		facts.SubprocessProvider = cfg.Session.Provider == "subprocess"
	}
	if session.DecideSessionExit(facts) != session.ExitRateLimitQuarantine {
		return screenWrite{}, false
	}
	patch, class := rateLimitQuarantine(v.Row, v.Now)
	return screenWrite{patch: patch, class: class}, true
}

// killDead removes k's dead runtime by exact session object when L3
// passes, and records its crash once removed; a leaf that kills by no
// session object removes nothing.
func killDead(ctx context.Context, c txCaps, k deadKill) (deadKilled, error) {
	killer, ok := k.leaf.(runtime.SessionObjectKiller)
	if !ok {
		return deadKilled{}, nil
	}
	if reason, _ := attachLeg(ctx, k.leaf, k.name, false, k.now); reason != "" {
		return deadKilled{}, errRecycleAttached
	}
	var res runtime.SessionObjectKillResult
	var err error
	if k.zombie {
		res, err = killer.KillZombieObject(k.name, k.live.ObjectID, k.live.ObjectCreated, k.live.PanePID)
	} else {
		res, err = killer.KillCorpseObject(k.name, k.live.ObjectID, k.live.ObjectCreated)
	}
	out := deadKilled{killed: res == runtime.SessionObjectKilled, blocked: res == runtime.SessionObjectLive || res == runtime.SessionObjectChanged, account: k.account}
	if out.killed && k.crash != nil {
		if c.start.rec != nil {
			c.start.rec.Record(*k.crash)
		}
		telemetry.RecordAgentCrash(context.Background(), k.crash.Subject, k.crash.Message)
	}
	return out, err
}

// deathStep accounts the row's own death once its dead runtime is removed,
// as A6's dead-runtime heal does for a runtime that is gone (SESS-531,
// SESS-537/538): the heal, and a rapid crash's wake failure or churn. A
// death that accrues ends the effect with its record, so the row restarts
// the next pass, as legacy's does (and a quarantine holds it); any other
// rides ahead of the PreWake. An attached pane or a refused kill refuses.
func deathStep(v txView) txStep {
	out, err := callResult[deadKilled](v)
	switch {
	case errors.Is(err, errRecycleAttached):
		return txStep{Refuse: causeAttached} // the recycle waits for the detach (C8.1)
	case out.blocked:
		return txStep{Refuse: causeOperatorPane}
	case !out.killed || !out.account || !committed(v.Row):
		return txStep{}
	}
	w := *v.World
	w.Now = v.Now
	patch, class, accrued := deadRuntimePatch(&rowFacts{w: &w, k: v.It.Key, row: v.World.Census.reread(v.It.Key, v.Row, v.Meta), found: true}, true) // the fresh row's stop keys: a drain's death is no crash
	return txStep{Write: patch, Class: class, Done: accrued}
}
