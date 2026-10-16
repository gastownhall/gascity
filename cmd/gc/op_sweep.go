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
// executor is safe. The sweeper emits no events.
//
// It runs one pass at boot whatever the gates say, which catches ops left
// open after a gate was turned off, and then a pass every interval while any
// of the three ops gates the controller latched at boot is not off, or while
// the last pass left work: an op still within its finish_by, a full batch
// that closed an op (more may wait behind it), a retryable read, open or
// close failure, or a truncated read. So with every gate off (the rollback
// path, DES-O0API §12) the ops in flight at the restart are still closed
// once their finish_by passes, and the loop then stops. A store that cannot
// record ops at all (no conditional writes, no namespace) is not left work
// with every gate off, or it would tick forever. A boot pass skipped because
// the city is quiescent is retried on the ticker until it runs.
//
// With every gate off a pass first asks the sessions store whether any op
// row is submitted (opjournal.HasSubmittedOps, one keyed read of one row),
// and only then opens the op store, so a city that never recorded an op does
// not pay NewStore's capability probe at every controller start.

import (
	"context"
	"fmt"
	"log"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/opjournal"
	"github.com/gastownhall/gascity/internal/opjournal/legacyop"
	"github.com/gastownhall/gascity/internal/rollout"
	"github.com/gastownhall/gascity/internal/telemetry"
)

const (
	// opSweepInterval is the pass cadence while a gate is on or the last pass
	// left work.
	opSweepInterval = time.Minute
	// opSweepLimit caps the legacy ops one pass reads (and closes).
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
	// OJ-2b wires it to the ops.recording_degraded event; the default logs.
	degraded func(legacyop.Degrade)
	// logf writes the sweeper's log lines; tests capture them.
	logf func(format string, args ...any)

	mu     sync.Mutex
	st     *opjournal.Store
	stOver beads.Store
	booted bool
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
	s.degraded = s.logDegrade
	return s
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
// cannot end the sweeper for the controller's life.
func (s *opSweeper) safePass() (panicked bool) {
	defer s.cs.beginStoreWork()()
	defer func() {
		if r := recover(); r != nil {
			panicked = true
			log.Printf("op-sweep: pass panicked: %v (type=%T)\n%s", r, r, debug.Stack())
		}
	}()
	s.pass()
	return false
}

// pass sweeps the op store once and records whether it left work. A
// quiescent city is not read; the pass does not count as the boot pass. A
// panicking pass leaves no work, so a bug cannot keep a gates-off ticker
// alive.
func (s *opSweeper) pass() {
	if s.cs.storesQuiescent() {
		return
	}
	s.mu.Lock()
	s.booted, s.leftWork = true, false
	s.mu.Unlock()
	left := s.sweep(s.strongestMode())
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
	st, err := s.store()
	if err != nil {
		reason := legacyop.DegradeReason(err)
		s.fail(mode, legacyop.StageOpen, reason, err)
		return opSweepRetryable(reason)
	}
	stats, err := legacyop.Sweep(st, s.clk.Now(), opSweepLimit, opSweepActor)
	s.logSweep(stats)
	// A full batch may have ops behind it. It counts only when an op in it
	// closed, so a batch whose every close fails for good cannot keep a
	// gates-off ticker alive.
	fullBatch := stats.Listed == opSweepLimit && stats.Closed+stats.Raced > 0
	left := stats.Pending > 0 || stats.Truncated || fullBatch
	if err != nil {
		reason := legacyop.DegradeReason(err)
		if stats.Failed == 0 {
			// A read failure; Sweep counts each failed close itself.
			countOpSweepFailure(legacyop.StageSweep, reason)
		}
		s.report(mode, legacyop.StageSweep, reason, err)
		left = left || opSweepRetryable(reason)
	}
	return left
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
	s.logf("op-sweep: listed=%d closed=%d pending=%d raced=%d failed=%d truncated=%t undecodable=%d%s",
		stats.Listed, stats.Closed, stats.Pending, stats.Raced, stats.Failed, stats.Truncated,
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
// sessions-class store when none is cached or that store was replaced.
func (s *opSweeper) store() (*opjournal.Store, error) {
	over := s.cs.SessionsBeadStore()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.st != nil && sameBeadStore(s.stOver, over.Store) {
		return s.st, nil
	}
	st, err := opjournal.NewStore(over, s.clk)
	if err != nil {
		s.st, s.stOver = nil, nil
		return nil, err
	}
	s.st, s.stOver = st, over.Store
	return st, nil
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
