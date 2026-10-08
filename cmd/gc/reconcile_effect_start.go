package main

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
)

// The start effect (CONTRACT v5 S1, S2): bringUp, which may launch and costs
// a token, and adopt, which never launches and costs none, run one function
// under the runtime name lock. It reads the runtime fresh (O1) and picks its
// verb from S1's table, keyed by the row's token and that read. A refusal
// writes nothing. C5a1-2 adds the Launch and registers bringUp; C5a2 adds
// the abandon table and the rollback, and C5a3 the dead row's
// classification and recycle, adoption's side effects, the lost-commit
// cleanup and the pending-create StaleSelf exit.

// Start refusal causes. Each backs the row off (P4).
const (
	causeTemplate            = "template-unresolved"
	causeRouteUnknown        = "route-unknown"
	causeNameBusy            = "name-busy"
	causeLivenessUnknown     = "liveness-unknown"
	causeLivenessUnsupported = "liveness-unsupported"
	causeAlreadyRunning      = "already-running"
	causeTokenDrift          = "token-drift"
	causeNewerSelf           = "newer-self"
	causeOccupied            = "occupied"
	causeAttribution         = "attribution-unknown"
	causeDead                = "dead" // C5a3 classifies and recycles
	causeGone                = "gone" // an adopt found no runtime
	causePrepare             = "prepare"
	causeCommitLost          = "commit-lost" // C5a3 stops the start's own runtime
)

// freshClass is one fresh, corpse-aware read of a runtime name (v5 O1).
type freshClass uint8

const (
	freshUnknown     freshClass = iota // incomplete or errored: proves nothing
	freshUnsupported                   // the leaf cannot answer with an error
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
			return verbRefuse, causeGone
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

// run is S1: resolve the template and the route, take the name lock, read
// fresh, and act on the table's verb. The lock is held through the commit,
// and past the deadline while a provider call runs (P3).
func (e startEffect) run(ctx context.Context) settlement {
	rt := e.pass.Runtime
	began := rt.Clock.Now()
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
	a := startAttempt{row: row, tp: res.TP, name: strings.TrimSpace(row.Info.SessionNameMetadata), writer: writer}
	if router, ok := rt.SP.(interface{ RouteACP(string) }); ok && a.tp.IsACP {
		router.RouteACP(a.name)
	}
	leaf, _, known := runtime.ResolveBackend(rt.SP, a.name)
	if !known {
		return refused(causeRouteUnknown)
	}
	a.leaf = leaf
	unlock := runtimeNames.tryLock(rt.CityName, a.name)
	if unlock == nil {
		return refused(causeNameBusy)
	}
	defer unlock()
	read := e.observe(ctx, a, row.Info, began)
	switch verb, cause := resolveStart(e.adopt, read, row.Info); verb {
	case verbCommit:
		prepared, err := e.prepare(a, row.Info, sessTranscriptUnknown)
		if err != nil {
			return settlement{Outcome: settledFailed, Cause: causePrepare, Err: err}
		}
		return e.commit(ctx, a, prepared, row.InstanceToken, read.At, nil)
	case verbNoop:
		return settlement{Outcome: settledNoop, Cause: cause, Noted: &notedRuntime{Name: a.name, At: read.At}}
	default:
		return refused(cause)
	}
}

func refused(cause string) settlement { return settlement{Outcome: settledRefused, Cause: cause} }

// observe is S1 step 3: a read fresh for this effect, through the routed
// leaf (LL2's ObserveLivenessBoundedSince, which takes only a read issued
// after since, never the inventory's cache), then, for an alive runtime, its
// identity judged against row's token (O2).
func (e startEffect) observe(ctx context.Context, a startAttempt, row session.Info, since time.Time) freshRead {
	r := freshRead{At: e.pass.Runtime.Clock.Now()}
	if _, ok := a.leaf.(runtime.LivenessObserverWithError); !ok {
		r.Class = freshUnsupported
		return r
	}
	l, status, err := runtime.ObserveLivenessBoundedSince(ctx, a.leaf, a.name, a.tp.Hints.ProcessNames, since, fenceProbeTimeout)
	switch {
	case status != runtime.ObservationComplete || err != nil:
		r.Class = freshUnknown
	case !l.Present():
		r.Class = freshGone
	case !l.Alive:
		r.Class = freshDead
	default:
		r.Class, r.Verdict = freshAlive, compareIdentity(row, readRuntimeIdentity(ctx, a.leaf, a.name))
	}
	return r
}

// prepare is legacy's prepare of row behind the refusing store, with the
// effect's transcript state (S-1).
func (e startEffect) prepare(a startAttempt, row session.Info, transcript sessTranscriptState) (*preparedStart, error) {
	prepared, _, err := buildPreparedStartWithTranscript(startCandidate{info: row, tp: a.tp}, e.pass.Runtime.CityPath, e.pass.World.Env.Cfg,
		blindWriteRefusingStore{inner: a.writer.store}, nil, &transcript)
	return prepared, err
}

// commit is v5 S2's commit verb: CommitStartedPatch with extra, written by
// CAS only while the fresh row is open, holds token, and is creating, active
// or awake. Holds do not veto it; the next pass drains the row. Nothing is
// written once ctx ends. read is when the fresh read that proved the
// runtime alive and Current was issued, which the drain notes (v5 O4).
func (e startEffect) commit(ctx context.Context, a startAttempt, prepared *preparedStart, token string, read time.Time, extra session.MetadataPatch) settlement {
	now := e.pass.Runtime.Clock.Now()
	var wrote bool
	err := session.WithSessionMutationLock(a.row.Info.ID, func() error {
		if err := ctx.Err(); err != nil {
			return err
		}
		var err error
		wrote, err = a.writer.updateMetadataFenced(a.row.Info.ID, 1, func(fresh session.Info, _ session.PersistedResponse) session.MetadataPatch {
			if fresh.Closed || fresh.InstanceToken != token || !commitState(fresh) || ctx.Err() != nil {
				return nil
			}
			patch := commitStartedPatch(prepared, fresh, now)
			maps.Copy(patch, extra)
			return patch
		})
		return err
	})
	switch {
	case !wrote && ctx.Err() != nil:
		return deadlineSettlement(ctx)
	case errors.Is(err, errNoConditionalWriter):
		return settlement{Outcome: settledRefused, Cause: causeNoWriter, Err: err}
	case err != nil:
		return settlement{Outcome: settledFailed, Cause: causeWrite, Err: err}
	case !wrote:
		return refused(causeCommitLost)
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

// commitStartedPatch is legacy's commit patch for prepared on the fresh row
// (commitStartResultTraced).
func commitStartedPatch(prepared *preparedStart, fresh session.Info, now time.Time) session.MetadataPatch {
	breakdown := ""
	if b, err := json.Marshal(prepared.coreBreakdown); err == nil {
		breakdown = string(b)
	}
	in := session.CommitStartedPatchInput{
		CoreHash: prepared.coreHash, LiveHash: prepared.liveHash, ProvisionHash: prepared.provisionHash,
		LaunchHash: prepared.launchHash, CoreBreakdown: breakdown,
		ConfirmState:            confirmStartCommitState(fresh.MetadataState),
		ClearSleepReason:        fresh.SleepReason != "",
		ClearPendingCreateClaim: shouldRollbackPendingCreateInfo(fresh),
		StartsAwakeInterval:     confirmPendingStart(fresh.MetadataState),
		Now:                     now,
	}
	if prepared.promptDelivered {
		in.PrimedAt, in.PromptHash = now, prepared.promptHash
	}
	return session.CommitStartedPatch(in)
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
