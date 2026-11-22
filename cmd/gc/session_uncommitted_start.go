package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

// uncommittedStartIdentityKind is the standing-condition kind of the
// one-per-episode warning an eviction logs when the runtime under an
// uncommitted row's name does not read as that row's own.
const uncommittedStartIdentityKind = "uncommitted_start_identity"

// evictUncommittedStart is one reconciler's invocation of
// evictUncommittedLiveStartIfDue at the session rows it decided on.
type evictUncommittedStart struct {
	ctx      context.Context
	cityPath string
	info     sessionpkg.Info
	name     string
	tp       TemplateParams
	cfg      *config.City
	sp       runtime.Provider
	dt       *drainTracker
	clk      clock.Clock
	startup  time.Duration
	stderr   io.Writer
	trace    *sessionReconcilerTraceCycle
	// attached is the attach probe under drift's settled wait-for-detach
	// rule; stop is the kill the eviction acts through (legacyAct.Stop over
	// the controller's store). Both are supplied by the reconciler so this
	// file holds no raw store: the typed-store boundary keeps them at the
	// baseline call site.
	attached func() (bool, error)
	stop     func(sessionpkg.Decided) error
}

// evictUncommittedLiveStartIfDue replaces a live runtime whose row's start
// never committed (#6417 F2): the row is desired, uncommitted
// (started_config_hash empty — a requested restart's handoff cleared it, or a
// commit that failed), still carries its committed reset cycle
// (reset_committed_at), and carries a prompt-bearing template, while a
// runtime under its name holds GC_SESSION_ID equal to the row's ID. Such a
// runtime never received this row's start — it came up in the gap the
// handoff opened without a start that consumed the cycle — and adopting it
// left the session live with its prompt stranded and nothing left to retry.
// It is the non-claim counterpart of recoverRunningPendingCreate: once the
// startup window has passed with no commit taking the name, the runtime is
// stopped (FactsLegacyUncommittedEvict, so a commit, a newer incarnation or
// an operator hold landing in flight refuses the kill), and the next tick
// starts the row and delivers its prompt.
//
// Every guard but the age window is cheap to check, and the expensive ones
// (the runtime identity read, the attachment probe, the fresh liveness read)
// are taken only when the row qualifies. A row that fails any guard stops
// nothing; a foreign or unreadable identity logs one warning per episode.
func evictUncommittedLiveStartIfDue(e evictUncommittedStart) {
	if e.sp == nil || e.clk == nil || strings.TrimSpace(e.info.ID) == "" || strings.TrimSpace(e.name) == "" {
		return
	}
	now := e.clk.Now()
	stderr := e.stderr
	if stderr == nil {
		stderr = io.Discard
	}
	// Row guards: a committed start owns its runtime, pending-create rows
	// keep their own repair arms (#2081), a row an operator suspends or
	// holds (the typed Disposition projection: an operator suspend keeps the
	// slot, a live heartbeat hold keeps the row up) is never the controller's
	// to restart, and a template that renders no prompt has nothing stranded
	// to recover.
	if strings.TrimSpace(e.info.StartedConfigHash) != "" {
		return
	}
	if shouldRollbackPendingCreateInfo(e.info) {
		return
	}
	if disp := sessionpkg.DispositionOfInfo(e.info); disp.KeepsSlot() || disp.Heartbeat.After(now) {
		return
	}
	if strings.TrimSpace(e.tp.Prompt) == "" {
		return
	}
	// Cycle guard: the uncommitted live row must be the shape #6417 leaves —
	// a row whose restart or reset cycle committed (reset_committed_at is the
	// handoff's and every session reset's stamp), killed its old runtime and
	// cleared its start witness, after which a runtime under its name came up
	// that no start consuming the cycle launched. A start that consumes the
	// cycle clears the stamp in its mint or its commit, and the Manager holds
	// every start but an operator's own resume while a handoff awaits the
	// controller's start (SESSION-START-031). A steady row that simply never
	// committed a config hash (a timeline parity fixture's live seat, a
	// session a Manager start brought up) is not this cycle's collateral: it
	// is left to the wake arms that own it.
	if strings.TrimSpace(e.info.ResetCommittedAt) == "" {
		return
	}
	// Identity guard: the runtime under the name must read as this very row
	// (GC_SESSION_ID), held to a probe budget; a runtime that is already gone
	// is benign, anything else is one warning per episode and nothing stopped.
	readCtx := e.ctx
	if readCtx == nil {
		readCtx = context.Background()
	}
	readCtx, cancel := context.WithTimeout(readCtx, startFailureProbeBudget)
	defer cancel()
	identity := readPendingCreateIdentityBounded(readCtx, e.cityPath, e.info, e.name, e.sp)
	switch identity.id {
	case runtimeTokenMatch:
	case runtimeTokenGone:
		// The runtime freed the name on its own: nothing to evict.
		return
	default:
		why := "unset or unsupported on this runtime"
		switch identity.id {
		case runtimeTokenMismatch:
			why = "another session's"
		case runtimeTokenUnverifiable:
			why = "unreadable"
		}
		logStandingCondition(e.dt, stderr, e.info.ID, uncommittedStartIdentityKind, fmt.Sprintf(
			"session reconciler: leaving the live runtime under %s alone: its GC_SESSION_ID reads as %s, so it is not the uncommitted start's to replace",
			e.name, why), now)
		return
	}
	// Fresh liveness: the runtime must still be up with its agent alive, and
	// the read carries the creation time the age window is measured from.
	live, err := runtime.ObserveLivenessSince(e.sp, e.name, drainAckStopPendingProcessNames(e.cfg, e.info), now)
	if err != nil {
		return // unreadable liveness is no basis for a kill; the next tick re-reads
	}
	if !live.Running || !live.Alive {
		e.dt.clearUncommittedLiveSight(e.info.ID)
		return
	}
	// Attachment guard: drift's settled wait-for-detach rule. An unreadable
	// attach probe counts as attached.
	if e.attached == nil || e.stop == nil {
		return
	}
	attached, attachErr := e.attached()
	if attachErr != nil || attached {
		return
	}
	// Age guard: the startup window must have passed since the newest of the
	// runtime's creation time, the row's last wake and its committed reset.
	// A provider that reports no creation time gets the first sighting of the
	// row live and uncommitted as its stand-in, kept in the drain tracker.
	created, haveCreated := uncommittedStartCreatedTime(live.ObjectCreated)
	if !haveCreated {
		created = e.dt.firstUncommittedLiveSight(e.info.ID, e.info.Generation+"/"+e.info.InstanceToken, now)
	}
	basis := created
	for _, raw := range []string{e.info.LastWokeAt, e.info.ResetCommittedAt} {
		if t, ok := uncommittedStartBasisTime(raw); ok && t.After(basis) {
			basis = t
		}
	}
	elapsed := now.Sub(basis)
	if elapsed <= e.startup {
		return
	}
	// The action: stop the occupant while the row still carries the facts the
	// decision rests on. An in-flight start holds the runtime lease (or
	// reports itself starting), so it holds this off; a benign refusal (a
	// commit or a newer incarnation landing mid-kill) needs no noise.
	d := sessionpkg.Decide(e.info, sessionpkg.FactsLegacyUncommittedEvict)
	if err := e.stop(d); err != nil {
		if runtime.IsSessionGone(err) || errors.Is(err, sessionpkg.ErrKillPremiseMoved) ||
			errors.Is(err, sessionpkg.ErrRuntimeLeaseBusy) || errors.Is(err, sessionpkg.ErrSessionStarting) {
			return
		}
		logStandingCondition(e.dt, stderr, e.info.ID, "uncommitted_start_evict_error", fmt.Sprintf(
			"session reconciler: evicting the uncommitted live start of %s: %v", e.name, err), now)
		return
	}
	fmt.Fprintf(stderr, "session reconciler: evicted %s's live runtime: uncommitted for %s and its start's prompt was never delivered\n", e.name, elapsed.Round(time.Second)) //nolint:errcheck // best-effort diagnostics
	if e.trace != nil {
		e.trace.RecordOperation(TraceSiteReconcilerUncommittedStart, TraceReasonUncommittedStart, TraceOutcomeStop, "", e.tp.TemplateName, e.name, 0, traceRecordPayload{
			"session_bead_id": e.info.ID,
			"uncommitted_for": elapsed.Round(time.Second).String(),
		})
	}
	e.dt.clearUncommittedLiveSight(e.info.ID)
}

// uncommittedStartCreatedTime parses Liveness.ObjectCreated, the runtime
// object's creation time in decimal unix seconds (tmux #{session_created}),
// reporting whether it named a time at all.
func uncommittedStartCreatedTime(raw string) (time.Time, bool) {
	secs, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
	if err != nil || secs <= 0 {
		return time.Time{}, false
	}
	return time.Unix(secs, 0), true
}

// uncommittedStartBasisTime parses one of the row's eviction basis
// timestamps (last_woke_at, reset_committed_at; RFC3339), reporting whether
// it named a time at all.
func uncommittedStartBasisTime(raw string) (time.Time, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}
