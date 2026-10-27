package main

// The op sweeper: the controller's closer for orphaned legacy ops
// (DES-O0API §9.2).
//
// The O0 producers (the API and the CLI) record each request as an op before
// the legacy path runs and close it after. A producer that dies in between,
// or a close that keeps failing, leaves the op open. The API adopts its own
// orphans on a same-key retry; everything else is closed here once its
// finish_by passes, as failed{deadline_in_flight}, through legacyop.Sweep. A
// lane op and an op still within its finish_by are never touched (Sweep's
// rule), and every close is revision-fenced, so racing an adopter or a late
// executor is safe. A swept op gets no terminal event; a store failure under
// a gate that is on raises ops.recording_degraded.
//
// It runs one pass at boot whatever the gates say, which catches ops left
// open after a gate was turned off, and then a pass every interval while any
// of the three ops gates the controller latched at boot is not off, or while
// the last pass left work: an op still within its finish_by, a read that
// stopped at the limit or its page cap in a pass where an op closed (more
// may wait behind it), or a retryable read, open or close failure. So with
// every gate off (the rollback path, DES-O0API §12) the ops in flight at the
// restart are still closed once their finish_by passes, and the loop then
// stops. Neither a store that cannot record ops at all (no conditional
// writes, no namespace), nor a stopped read in a pass where nothing closed (a
// head no pass gets past, closes refused for good), nor a panicking pass is
// left work with every gate off, or any would tick forever. A pass that stops
// the loop on a stopped read or a panic logs so, and gc doctor's
// ops-recording check, which scans with every gate off too once an op row
// exists, reports the expired ops left open. A boot pass skipped because the
// city is quiescent is retried on the ticker until it runs.
//
// With every gate off a pass first asks the sessions store whether any op
// row is submitted (opjournal.HasSubmittedOps, one keyed read of one row),
// and only then opens the op store, so a city that never recorded an op does
// not pay NewStore's capability probe at every controller start.
//
// Gates that are auto only by their builtin default (none set in [ops] or
// through GC_OPS_*) record only over a SQLite sessions store
// (opjournal.CheckRecordsByDefault): a pass that finds a Dolt-served or
// otherwise non-SQLite store, or one that cannot record ops at all (every
// released bd lacks --if-revision), logs that once and runs as a gates-off
// pass, so it raises no ops.recording_degraded event and still closes the
// expired ops an earlier explicit gate recorded there. The loop keeps
// ticking, as the gates are on, but while that store is still the sessions
// store a tick with nothing left to retry does nothing. A reload that
// replaces the store is opened on the next tick, so ops recorded over a
// SQLite store are swept. An explicit auto or require stays loud.

import (
	"context"
	"fmt"
	"log"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"github.com/gastownhall/gascity/internal/api"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/opjournal"
	"github.com/gastownhall/gascity/internal/opjournal/legacyop"
	"github.com/gastownhall/gascity/internal/rollout"
	"github.com/gastownhall/gascity/internal/telemetry"
)

const (
	// opSweepInterval is the pass cadence while a gate is on or the last pass
	// left work.
	opSweepInterval = time.Minute
	// opSweepLimit caps the expired legacy ops one pass closes. Ops still
	// within their finish_by do not count toward it: the read pages past them.
	opSweepLimit = 200
	// opSweepActor stamps the closes the sweeper writes.
	opSweepActor = "controller:op-sweep"
	// opSweepLogIDs caps the undecodable ids one summary line prints.
	opSweepLogIDs = 5
)

// opSweeper is one controller's op sweeper. Its store is opened lazily and
// cached against the sessions-class store it was opened over, so a reload
// that replaces that store reopens it.
type opSweeper struct {
	cs       *controllerState
	flags    rollout.Flags
	clk      clock.Clock
	interval time.Duration
	// newTicker starts the pass ticker; tests drive it by hand.
	newTicker func(time.Duration) (<-chan time.Time, func())
	// degraded hears every store failure the sweeper degrades past while a
	// gate is on; it never receives Mode off (with every gate off a failure
	// is only logged, so recording that is off never reads as degraded).
	// The default logs and raises ops.recording_degraded (reportDegrade).
	degraded func(legacyop.Degrade)
	// events latches and records the sweeper's ops.recording_degraded
	// events.
	events *api.OpsDegradeReporter
	// logf writes the sweeper's log lines; tests capture them.
	logf func(format string, args ...any)

	mu     sync.Mutex
	st     *opjournal.Store
	stOver beads.Store
	booted bool
	// incapableOver is the sessions-class store the last open found would
	// not record under gates that are auto only by their builtin default
	// (not SQLite, or unable to record ops at all), nil when the last open
	// did not. While it is still the
	// sessions store, passes run as if every gate were off (pass).
	incapableOver beads.Store
	// leftWork reports that the last pass left something a later pass can
	// finish; with every gate off it keeps the ticker running.
	leftWork bool
	logs     map[opSweepLogKey]time.Time
}

// opSweepLogKey keys the sweeper's rate-limited log lines.
type opSweepLogKey struct{ stage, reason string }

// newOpSweeper builds the controller's sweeper over its boot-latched gates.
func (cs *controllerState) newOpSweeper() *opSweeper {
	s := &opSweeper{
		cs:       cs,
		flags:    cs.rolloutFlags,
		clk:      clock.Real{},
		interval: clock.Backstop(opSweepInterval),
		newTicker: func(d time.Duration) (<-chan time.Time, func()) {
			t := time.NewTicker(d)
			return t.C, t.Stop
		},
		logf: log.Printf,
	}
	s.events = api.NewOpsDegradeReporter(func() events.Recorder { return cs.EventProvider() }, s.clk)
	s.degraded = s.reportDegrade
	return s
}

// reportDegrade is the default degrade hook: it logs d and raises one
// ops.recording_degraded event for each ops gate that is not off, under that
// gate's mode. A sweep serves every gate's ops, so its failure degrades each
// gate that records.
func (s *opSweeper) reportDegrade(d legacyop.Degrade) {
	s.logDegrade(d)
	for _, g := range opsGateModes(s.flags) {
		if g.mode == rollout.Off {
			continue
		}
		gd := d
		gd.Mode = g.mode
		s.events.Report(g.key, gd)
	}
}

// startOpSweep runs the op sweeper until ctx ends: a boot pass, then a pass
// every interval while a gate is on or the last pass left work.
func (cs *controllerState) startOpSweep(ctx context.Context) {
	s := cs.newOpSweeper()
	go s.run(ctx)
}

// run is the sweeper's loop. With every gate off it returns once a pass has
// run and left no work; it always returns when ctx ends.
func (s *opSweeper) run(ctx context.Context) {
	s.safePass()
	if !s.wantsTicker() {
		return
	}
	ticks, stop := s.newTicker(s.interval)
	defer stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticks:
			s.safePass()
			if !s.wantsTicker() {
				return
			}
		}
	}
}

// wantsTicker reports whether another pass is due: the boot pass has not run
// yet, the last pass left work, or a gate is on.
func (s *opSweeper) wantsTicker() bool {
	s.mu.Lock()
	booted, leftWork := s.booted, s.leftWork
	s.mu.Unlock()
	return !booted || leftWork || s.strongestMode() != rollout.Off
}

// incapableNow reports whether the sessions store is still the one the last
// open found unable to record ops under gates that are auto only by their
// builtin default.
func (s *opSweeper) incapableNow() bool {
	over := s.cs.SessionsBeadStore().Store
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.incapableOver != nil && sameBeadStore(s.incapableOver, over)
}

// opsGatesBuiltinDefaultOnly reports whether every ops gate that is on under
// flags is auto only by its builtin default, and at least one is on. Such
// gates treat a store that cannot record ops at all as off.
func opsGatesBuiltinDefaultOnly(flags rollout.Flags) bool {
	on := false
	for _, g := range opsGateModes(flags) {
		if g.mode == rollout.Off || g.mode == rollout.ModeUnset {
			continue
		}
		if !flags.BuiltinAuto(g.key) {
			return false
		}
		on = true
	}
	return on
}

// opsGateMode is one ops gate's key and resolved mode.
type opsGateMode struct {
	key  string
	mode rollout.Mode
}

// opsGateModes lists the three ops gates with their modes under flags.
func opsGateModes(flags rollout.Flags) []opsGateMode {
	return []opsGateMode{
		{rollout.KeyOpsContent, flags.OpsContent()},
		{rollout.KeyOpsControl, flags.OpsControl()},
		{rollout.KeyOpsLifecycle, flags.OpsLifecycle()},
	}
}

// strongestMode is the strongest of the three ops gates: require over auto
// over off. An unset gate reads as off.
func (s *opSweeper) strongestMode() rollout.Mode {
	strongest := rollout.Off
	for _, m := range []rollout.Mode{s.flags.OpsContent(), s.flags.OpsControl(), s.flags.OpsLifecycle()} {
		switch m {
		case rollout.Require:
			return rollout.Require
		case rollout.Auto:
			strongest = rollout.Auto
		}
	}
	return strongest
}

// safePass runs one pass as store work and recovers a panic, so one bad row
// cannot crash the controller. With a gate on the next tick retries; with
// every gate off the panicking pass leaves no work (see pass), so the loop
// stops, and the log line says so.
func (s *opSweeper) safePass() (panicked bool) {
	defer s.cs.beginStoreWork()()
	defer func() {
		if r := recover(); r != nil {
			panicked = true
			stops := ""
			if !s.wantsTicker() {
				stops = "; every ops gate is off, so the sweeper stops until the controller restarts"
			}
			s.logf("op-sweep: pass panicked%s: %v (type=%T)\n%s", stops, r, r, debug.Stack())
		}
	}()
	s.pass()
	return false
}

// pass sweeps the op store once and records whether it left work. A
// quiescent city is not read; the pass does not count as the boot pass. A
// panicking pass leaves no work, so a bug cannot keep a gates-off ticker
// alive. While the sessions store is one an open found unable to record ops
// under the builtin-default gates (incapableNow), a pass runs as if every
// gate were off, and does nothing when the last pass left no work.
func (s *opSweeper) pass() {
	if s.cs.storesQuiescent() {
		return
	}
	incapable := s.incapableNow()
	s.mu.Lock()
	if incapable && !s.leftWork {
		s.mu.Unlock()
		return
	}
	s.booted, s.leftWork = true, false
	s.mu.Unlock()
	mode := s.strongestMode()
	if incapable {
		mode = rollout.Off
	}
	left := s.sweep(mode)
	s.mu.Lock()
	s.leftWork = left
	s.mu.Unlock()
}

// sweep runs one pass under mode, the strongest gate, and reports whether it
// left work a later pass can finish.
func (s *opSweeper) sweep(mode rollout.Mode) bool {
	if mode == rollout.Off && !s.hasStore() {
		has, err := opjournal.HasSubmittedOps(s.cs.SessionsBeadStore())
		if err != nil {
			reason := legacyop.DegradeReason(err)
			s.fail(mode, legacyop.StageSweep, reason, err)
			return opSweepRetryable(reason)
		}
		if !has {
			return false
		}
	}
	st, over, err := s.store(mode != rollout.Off && opsGatesBuiltinDefaultOnly(s.flags))
	if err != nil {
		reason := legacyop.DegradeReason(err)
		if mode != rollout.Off && legacyop.CannotRecord(reason) && opsGatesBuiltinDefaultOnly(s.flags) {
			s.mu.Lock()
			s.incapableOver = over
			s.mu.Unlock()
			if s.logDue(opSweepLogKey{stage: legacyop.StageOpen, reason: "default:" + reason}) {
				what := "the sessions store cannot record ops"
				if reason == legacyop.DegradeNotRecordedByDefault {
					what = "that default records ops only on a SQLite sessions store"
				}
				s.logf("op-sweep: the ops gates are auto by default, but %s (%s: %v); "+
					"sweeping as if every gate were off until a reload replaces it", what, reason, err)
			}
			return s.sweep(rollout.Off)
		}
		s.fail(mode, legacyop.StageOpen, reason, err)
		return opSweepRetryable(reason)
	}
	if mode != rollout.Off {
		// A pass as if every gate were off (the sessions store is one the
		// builtin default does not record on, holding ops left to sweep)
		// keeps the latch, so the ticker stops once that work is done.
		s.mu.Lock()
		s.incapableOver = nil
		s.mu.Unlock()
	}
	s.healthy(legacyop.StageOpen)
	stats, err := legacyop.Sweep(st, s.clk.Now(), opSweepLimit, opSweepActor)
	s.logSweep(stats)
	left := opSweepLeftWork(stats)
	if err != nil {
		reason := legacyop.DegradeReason(err)
		if stats.Failed == 0 {
			// A read failure; Sweep counts each failed close itself.
			countOpSweepFailure(legacyop.StageSweep, reason)
		}
		s.report(mode, legacyop.StageSweep, reason, err)
		left = left || opSweepRetryable(reason)
	} else {
		s.healthy(legacyop.StageSweep)
	}
	if mode == rollout.Off && !left && (stats.Full || stats.Truncated) {
		s.logStoppedRead(stats)
	}
	return left
}

// logStoppedRead logs that a gates-off pass whose read stopped at the limit
// or its page cap, with no op closed, stops the loop.
func (s *opSweeper) logStoppedRead(stats legacyop.SweepStats) {
	at := fmt.Sprintf("its limit of %d expired ops", opSweepLimit)
	if stats.Truncated {
		at = "its page cap behind a head of lane, undecodable or not yet expired legacy op rows"
	}
	s.logf("op-sweep: every ops gate is off and this pass closed no op after its read stopped at %s, "+
		"so the sweeper stops and leaves the expired legacy ops it did not close open; gc doctor's ops-recording check reports them", at)
}

// opSweepLeftWork reports whether a pass's stats show work a later pass can
// finish: pending ops, or a read that stopped at the limit or its page cap,
// which may have ops behind it. A stopped read counts only when an op
// closed, so a head no pass gets past, or a batch whose every close fails
// for good, cannot keep a gates-off ticker alive; the pass that stops the
// loop that way says so (logStoppedRead).
func opSweepLeftWork(stats legacyop.SweepStats) bool {
	progressed := stats.Closed+stats.Raced > 0
	return stats.Pending > 0 || ((stats.Full || stats.Truncated) && progressed)
}

// healthy tells the degrade reporter that the sweeper's stages just worked,
// for every ops gate, so their next failure raises a fresh event.
func (s *opSweeper) healthy(stages ...string) {
	for _, key := range []string{rollout.KeyOpsContent, rollout.KeyOpsControl, rollout.KeyOpsLifecycle} {
		s.events.Healthy(key, legacyop.SurfaceController, stages...)
	}
}

// opSweepRetryable reports whether a failure of reason may clear by itself,
// so a later pass is worth running: a store error or contention, not a store
// that lacks conditional writes, a namespace or the op bead type.
func opSweepRetryable(reason string) bool {
	return reason == legacyop.DegradeStoreError || reason == legacyop.DegradeContention
}

// fail counts a pass that could not sweep at all and reports it.
func (s *opSweeper) fail(mode rollout.Mode, stage, reason string, err error) {
	countOpSweepFailure(stage, reason)
	s.report(mode, stage, reason, err)
}

// report hands a store failure to the degrade hook while a gate is on, and
// only logs it, rate-limited, with every gate off.
func (s *opSweeper) report(mode rollout.Mode, stage, reason string, err error) {
	d := legacyop.Degrade{Mode: mode, Surface: legacyop.SurfaceController, Stage: stage, Reason: reason, Err: err}
	if mode == rollout.Off {
		s.logDegrade(d)
		return
	}
	s.degrade(d)
}

// logSweep logs a pass's summary: at once when it closed, raced or failed
// something, and once per legacyop.DegradeLogInterval when it only saw a
// truncated read or rows it cannot decode, which persist from pass to pass.
func (s *opSweeper) logSweep(stats legacyop.SweepStats) {
	acted := stats.Closed > 0 || stats.Raced > 0 || stats.Failed > 0
	if !acted {
		if !stats.Truncated && len(stats.Undecodable) == 0 {
			return
		}
		condition := fmt.Sprintf("truncated=%t,undecodable=%t", stats.Truncated, len(stats.Undecodable) > 0)
		if !s.logDue(opSweepLogKey{stage: "summary", reason: condition}) {
			return
		}
	}
	s.logf("op-sweep: listed=%d closed=%d pending=%d raced=%d failed=%d full=%t truncated=%t undecodable=%d%s",
		stats.Listed, stats.Closed, stats.Pending, stats.Raced, stats.Failed, stats.Full, stats.Truncated,
		len(stats.Undecodable), opSweepIDs(stats.Undecodable))
}

// opSweepIDs renders the first opSweepLogIDs ids, and how many it left out.
func opSweepIDs(ids []string) string {
	if len(ids) == 0 {
		return ""
	}
	shown := ids[:min(len(ids), opSweepLogIDs)]
	out := " [" + strings.Join(shown, " ") + "]"
	if more := len(ids) - len(shown); more > 0 {
		out += fmt.Sprintf(" (+%d more)", more)
	}
	return out
}

// hasStore reports whether an op store is cached.
func (s *opSweeper) hasStore() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.st != nil
}

// store returns the cached op store, opening it over the current
// sessions-class store when none is cached or that store was replaced, with
// the sessions-class store it is over, or a failed open was over. Under
// builtinDefault (every ops gate that is on is auto only by its builtin
// default, and the pass sweeps under it) a sessions store that is not
// SQLite-backed answers *opjournal.NotRecordedByDefaultError, cached store
// or not: that default records nothing there.
func (s *opSweeper) store(builtinDefault bool) (*opjournal.Store, beads.Store, error) {
	over := s.cs.SessionsBeadStore()
	if builtinDefault {
		if err := opjournal.CheckRecordsByDefault(over); err != nil {
			return nil, over.Store, err
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.st != nil && sameBeadStore(s.stOver, over.Store) {
		return s.st, over.Store, nil
	}
	st, err := opjournal.NewStore(over, s.clk)
	if err != nil {
		s.st, s.stOver = nil, nil
		return nil, over.Store, err
	}
	s.st, s.stOver = st, over.Store
	return st, over.Store, nil
}

// sameBeadStore reports whether a and b are the same store value. Stores are
// comparable by convention (the codebase keys maps by beads.Store); one that
// is not, which would make == panic, reads as a different store, so the op
// store is reopened over it rather than the pass failing.
func sameBeadStore(a, b beads.Store) (same bool) {
	defer func() {
		if recover() != nil {
			same = false
		}
	}()
	return a == b
}

// countOpSweepFailure counts a pass that could not sweep at all, beside the
// per-op counts legacyop.Sweep records, with the failure's degrade reason
// class in unrecorded_reason so the metric says why the sweeper fails.
func countOpSweepFailure(stage, reason string) {
	telemetry.RecordOpRecord(context.Background(), "", legacyop.SurfaceController, stage, legacyop.ResultFailed, reason)
}

// degrade hands d to the degrade hook.
func (s *opSweeper) degrade(d legacyop.Degrade) {
	if s.degraded != nil {
		s.degraded(d)
	}
}

// logDegrade is the default degrade hook, and the only report of a failure
// with every gate off: one log line per (stage, reason) per
// legacyop.DegradeLogInterval.
func (s *opSweeper) logDegrade(d legacyop.Degrade) {
	if s.logDue(opSweepLogKey{stage: d.Stage, reason: d.Reason}) {
		s.logf("op-sweep: op journal degraded: mode=%s stage=%s reason=%s: %v", d.Mode, d.Stage, d.Reason, d.Err)
	}
}

// logDue reports whether the line keyed by key is due, at most once per
// legacyop.DegradeLogInterval, and marks it logged when it is.
func (s *opSweeper) logDue(key opSweepLogKey) bool {
	now := s.clk.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	if last, seen := s.logs[key]; seen && now.Before(last.Add(legacyop.DegradeLogInterval)) {
		return false
	}
	if s.logs == nil {
		s.logs = map[opSweepLogKey]time.Time{}
	}
	s.logs[key] = now
	return true
}
