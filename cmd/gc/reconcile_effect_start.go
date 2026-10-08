package main

import (
	"context"
	"errors"
	"io"
	"maps"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/worktree"
)

// The start effect (CONTRACT v5 S1, S2): bringUp, which may launch and costs
// a token, and adopt, which never launches and costs none, run one function
// under the runtime name lock. It reads the runtime fresh (O1) and picks its
// verb from S1's table, keyed by the row's token and that read. A refusal
// writes nothing. C5a2 adds the abandon table and the rollback, and C5a3
// the dead row's classification and recycle, adoption's side effects, the
// lost-commit cleanup and the pending-create StaleSelf exit.

// Start refusal causes. Each backs the row off (P4). The shared ones
// (name-busy, route-unknown, liveness-unknown, not-present) are
// reconcile_session_namelock.go's.
const (
	causeTemplate            = "template-unresolved"
	causeLivenessUnsupported = "liveness-unsupported"
	causeAlreadyRunning      = "already-running"
	causeTokenDrift          = "token-drift"
	causeNewerSelf           = "newer-self"
	causeOccupied            = "occupied"
	causeAttribution         = "attribution-unknown"
	causeDead                = "dead" // C5a3 classifies and recycles
	causePrepare             = "prepare"
	causeCommitLost          = "commit-lost"      // the premise failed; C5a3 stops the start's own runtime
	causeCommitContended     = "commit-contended" // every CAS attempt lost to another writer
)

// commitAttempts bounds the commit's CAS retries, each re-deciding the
// premise on the fresh row (legacy's CommitStartedIfCurrent retries).
const commitAttempts = 3

// startEnv is what a start effect holds beyond the pass: the endpoint
// breaker, the planner's clock, and its recorder, which receives the
// breaker's transitions only (a row's events ride its settlement).
type startEnv struct {
	Capacity *endpointCapacityGuard
	Clock    plannerClock
	Rec      events.Recorder
	Stderr   io.Writer
}

// freshClass is one fresh, corpse-aware read of a runtime name (v5 O1).
type freshClass uint8

const (
	freshUnknown     freshClass = iota // incomplete or errored: proves nothing
	freshUnsupported                   // a backend cannot answer with an error
	freshGone                          // not present
	freshDead                          // a corpse, or a zombie: running, agent dead
	freshAlive                         // running, agent not known dead
)

// freshRead is one fresh read and, for an alive runtime, whose it is. At is
// when the read was issued.
type freshRead struct {
	Class   freshClass
	Verdict identityVerdict
	At      time.Time
}

// startVerb is S1's verb.
type startVerb uint8

const (
	verbRefuse startVerb = iota
	verbLaunch
	verbNoop
	verbCommit
)

// resolveStart is v5 S1's resolution table: the verb for one fresh read of
// row's runtime, its verdict judged against row's token. An adopt never
// launches. Only a creating row commits: a runtime alive and Current under
// any other row is left as it is.
func resolveStart(adopt bool, r freshRead, row session.Info) (startVerb, string) {
	switch r.Class {
	case freshUnsupported:
		return verbRefuse, causeLivenessUnsupported
	case freshUnknown:
		return verbRefuse, causeLivenessUnknown
	case freshGone:
		if adopt {
			return verbRefuse, causeNotPresent
		}
		return verbLaunch, ""
	case freshDead:
		return verbRefuse, causeDead
	}
	switch r.Verdict {
	case identityCurrent:
		if session.State(strings.TrimSpace(row.MetadataState)) == session.StateCreating {
			return verbCommit, ""
		}
		return verbNoop, causeAlreadyRunning
	case identityStaleSelf: // arm A3 re-keys it (X1); a pending create's S7 exit is C5a3's
		return verbRefuse, causeTokenDrift
	case identityNewerSelf:
		return verbRefuse, causeNewerSelf
	case identityForeign: // a pending create's rollback is C5a2's
		return verbRefuse, causeOccupied
	}
	return verbRefuse, causeAttribution
}

// startEffect is one admitted bringUp or adopt.
type startEffect struct {
	pass  *effectPass
	it    intent
	adopt bool
	// verify is worktree.Verify unless a test injects one.
	verify func(worktree.Spec) (worktree.Report, error)
}

func adoptEffect(p *effectPass, it intent) func(context.Context) settlement {
	return startEffect{pass: p, it: it, adopt: true}.run
}

// startAttempt is one effect's row, template, runtime name, routed leaf and
// writer.
type startAttempt struct {
	row    censusRow
	tp     TemplateParams
	name   string
	leaf   runtime.Provider
	writer fencedWriter
}

// run is S1: resolve the template and the route, take the name lock, then
// the row's session mutation lock around one section that reads fresh and
// acts on the table's verb (runLocked); a Launch continues outside it. The
// name lock is held through the commit, and past the deadline while a
// provider call runs (P3).
func (e startEffect) run(ctx context.Context) settlement {
	row, ok := e.pass.World.Census.Rows[e.it.Key]
	if !ok {
		return refused(causeRedecided)
	}
	writer, ok := e.pass.Writers[e.it.Key.Leg]
	if !ok {
		return settlement{Outcome: settledRefused, Cause: causeNoWriter, Err: errNoConditionalWriter}
	}
	res, ok := e.pass.World.Templates.lookup(row.Info)
	if !ok || res.Err != nil {
		return settlement{Outcome: settledRefused, Cause: causeTemplate, Err: res.Err}
	}
	a := startAttempt{row: row, tp: res.TP, name: strings.TrimSpace(row.Info.SessionName), writer: writer}
	if router, ok := e.pass.Runtime.(interface{ RouteACP(string) }); ok && a.tp.IsACP {
		router.RouteACP(a.name)
	}
	leaf, _, known := runtime.ResolveBackend(e.pass.Runtime, a.name)
	if !known {
		return refused(causeRouteUnknown)
	}
	a.leaf = leaf
	name, unlock, ok := lockRuntimeName(e.pass.World, row.Info)
	switch {
	case !ok && name == "":
		return refused(causeRouteUnknown)
	case !ok:
		return refused(causeNameBusy)
	}
	defer unlock()
	var s settlement
	var launch *launchPrep
	_ = session.WithSessionMutationLock(row.Info.ID, func() error {
		s, launch = e.runLocked(ctx, a)
		return nil
	})
	if launch != nil {
		return e.launch(ctx, a, launch)
	}
	return s
}

// runLocked is run's section under the row's session mutation lock: the
// fresh read, taken since the section began, and the verb's write; a
// Launch's PreWake returns the launch to run outside the section.
func (e startEffect) runLocked(ctx context.Context, a startAttempt) (settlement, *launchPrep) {
	read := e.observe(ctx, a, a.row.Info, e.pass.Start.Clock.Now())
	if ctx.Err() != nil { // the read proves nothing then
		return deadlineSettlement(ctx), nil
	}
	switch verb, cause := resolveStart(e.adopt, read, a.row.Info); verb {
	case verbLaunch:
		return e.preWake(ctx, a)
	case verbCommit:
		prepared, err := e.prepare(a, a.row.Info, sessTranscriptUnknown)
		if err != nil {
			return settlement{Outcome: settledFailed, Cause: causePrepare, Err: err}, nil
		}
		return e.commit(ctx, a, prepared, a.row.InstanceToken, read.At, nil), nil
	case verbNoop:
		return settlement{Outcome: settledNoop, Cause: cause, Noted: &notedRuntime{Name: a.name, At: read.At}}, nil
	default:
		return refused(cause), nil
	}
}

func refused(cause string) settlement { return settlement{Outcome: settledRefused, Cause: cause} }

// observe is S1 step 3: a read fresh for this effect (LL2's
// ObserveLivenessBoundedSince, which takes only a read issued after since)
// on the routed leaf, whose identity, judged against row's token, pairs
// with the presence it read (O2). The composite only confirms an absence:
// a stale route can read gone on one backend while the runtime lives on
// another (mc-zndi7.24), and a leftover sidecar there can carry the token.
func (e startEffect) observe(ctx context.Context, a startAttempt, row session.Info, since time.Time) freshRead {
	r := freshRead{At: e.pass.Start.Clock.Now()}
	if _, ok := a.leaf.(runtime.LivenessObserverWithError); !ok || !threeOutcome(e.pass.Runtime) {
		r.Class = freshUnsupported
		return r
	}
	read := func(sp runtime.Provider) (runtime.Liveness, bool) {
		l, status, err := runtime.ObserveLivenessBoundedSince(ctx, sp, a.name, a.tp.Hints.ProcessNames, since, fenceProbeTimeout)
		return l, status == runtime.ObservationComplete && err == nil
	}
	l, ok := read(a.leaf)
	switch {
	case !ok:
		r.Class = freshUnknown
	case !l.Present():
		if c, ok := read(e.pass.Runtime); ok && !c.Present() {
			r.Class = freshGone
		}
	case !l.Alive:
		r.Class = freshDead
	default:
		r.Class, r.Verdict = freshAlive, compareIdentity(row, readRuntimeIdentity(ctx, a.leaf, a.name))
	}
	return r
}

// prepare is legacy's prepare of row behind the refusing store, with the
// effect's transcript state (S-1), so it writes nothing.
func (e startEffect) prepare(a startAttempt, row session.Info, transcript sessTranscriptState) (*preparedStart, error) {
	prepared, _, err := buildPreparedStartWithTranscript(startCandidate{info: row, tp: a.tp}, e.pass.World.CityPath, e.pass.World.Env.Cfg,
		blindWriteRefusingStore{inner: a.writer.store}, e.taskWorkDirs(), &transcript)
	return prepared, err
}

// commit is v5 S2's commit verb: legacy's commit patch (startCommitPatch)
// with extra, written by CAS only while the fresh row is open, holds token,
// and is creating, active or awake. Holds do not veto it; the next pass
// drains the row. A lost CAS re-decides the premise, up to commitAttempts
// times. Nothing is written once ctx ends. The caller holds the session
// mutation lock. read is when the fresh read that proved the runtime alive
// and Current was issued, which the drain notes (v5 O4).
func (e startEffect) commit(ctx context.Context, a startAttempt, prepared *preparedStart, token string, read time.Time, extra session.MetadataPatch) settlement {
	now := e.pass.Start.Clock.Now()
	premise := false
	wrote, err := a.writer.updateMetadataFenced(a.row.Info.ID, commitAttempts, func(fresh session.Info, _ session.PersistedResponse) session.MetadataPatch {
		premise = !fresh.Closed && fresh.InstanceToken == token && commitState(fresh)
		if !premise || ctx.Err() != nil {
			return nil
		}
		patch := startCommitPatch(prepared, fresh, now)
		maps.Copy(patch, extra)
		return patch
	})
	switch {
	case !wrote && ctx.Err() != nil:
		return deadlineSettlement(ctx)
	case errors.Is(err, errNoConditionalWriter):
		return settlement{Outcome: settledRefused, Cause: causeNoWriter, Err: err}
	case err != nil:
		return settlement{Outcome: settledFailed, Cause: causeWrite, Err: err}
	case !wrote && !premise:
		return refused(causeCommitLost)
	case !wrote:
		return refused(causeCommitContended)
	}
	return settlement{Outcome: settledLanded, Noted: &notedRuntime{Name: a.name, At: read}}
}

// commitState is S2's state premise. v2 never commits through
// CommitStartedIfCurrent, whose premise admits the asleep state gc session
// kill writes.
func commitState(fresh session.Info) bool {
	switch session.State(strings.TrimSpace(fresh.MetadataState)) {
	case session.StateCreating, session.StateActive, session.StateAwake:
		return true
	}
	return false
}

// deadlineSettlement is the failure of an effect whose context ended, at its
// deadline (context.Cause tells, whatever Err reads under a fake clock) or
// at shutdown.
func deadlineSettlement(ctx context.Context) settlement {
	cause := "canceled"
	if errors.Is(context.Cause(ctx), context.DeadlineExceeded) {
		cause = causeDeadline
	}
	return settlement{Outcome: settledFailed, Cause: cause, Err: context.Cause(ctx)}
}
