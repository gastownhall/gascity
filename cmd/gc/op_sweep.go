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
// of the three ops gates the controller latched at boot is not off. A boot
// pass skipped because the city is quiescent is retried on the ticker until
// it runs.

import (
	"context"
	"errors"
	"log"
	"reflect"
	"runtime/debug"
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
	// opSweepInterval is the pass cadence while any ops gate is on.
	opSweepInterval = time.Minute
	// opSweepLimit caps the legacy ops one pass reads (and closes).
	opSweepLimit = 200
	// opSweepActor stamps the closes the sweeper writes.
	opSweepActor = "controller:op-sweep"
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
	// degraded hears every store failure the sweeper degrades past. OJ-2b
	// wires it to the ops.recording_degraded event; the default logs.
	degraded func(legacyop.Degrade)

	mu      sync.Mutex
	st      *opjournal.Store
	stOver  beads.Store
	booted  bool
	degLogs map[opSweepDegradeKey]time.Time
}

type opSweepDegradeKey struct{ stage, reason string }

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
	}
	s.degraded = s.logDegrade
	return s
}

// startOpSweep runs the op sweeper until ctx ends: a boot pass, then a pass
// every interval while any ops gate is on.
func (cs *controllerState) startOpSweep(ctx context.Context) {
	s := cs.newOpSweeper()
	go s.run(ctx)
}

// run is the sweeper's loop. It returns once the boot pass has run with every
// gate off, or when ctx ends.
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
// yet, or a gate is on.
func (s *opSweeper) wantsTicker() bool {
	s.mu.Lock()
	booted := s.booted
	s.mu.Unlock()
	return !booted || s.strongestMode() != rollout.Off
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

// pass opens the op store if it is not open and sweeps it once. A quiescent
// city is not read; the pass does not count as the boot pass.
func (s *opSweeper) pass() {
	if s.cs.storesQuiescent() {
		return
	}
	s.mu.Lock()
	s.booted = true
	s.mu.Unlock()
	st, err := s.store()
	if err != nil {
		if mode := s.strongestMode(); mode != rollout.Off {
			countOpSweepFailure()
			s.degrade(legacyop.Degrade{
				Mode: mode, Surface: legacyop.SurfaceController, Stage: legacyop.StageOpen,
				Reason: opStoreOpenReason(err), Err: err,
			})
		}
		return
	}
	stats, err := legacyop.Sweep(st, s.clk.Now(), opSweepLimit, opSweepActor)
	if stats.Closed > 0 || stats.Raced > 0 || stats.Failed > 0 || stats.Truncated || len(stats.Undecodable) > 0 {
		log.Printf("op-sweep: listed=%d closed=%d pending=%d raced=%d failed=%d truncated=%t undecodable=%v",
			stats.Listed, stats.Closed, stats.Pending, stats.Raced, stats.Failed, stats.Truncated, stats.Undecodable)
	}
	if err != nil {
		if stats.Failed == 0 {
			// A read failure; Sweep counts each failed close itself.
			countOpSweepFailure()
		}
		s.degrade(legacyop.Degrade{
			Mode: s.strongestMode(), Surface: legacyop.SurfaceController, Stage: legacyop.StageSweep,
			Reason: legacyop.DegradeStoreError, Err: err,
		})
	}
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

// sameBeadStore reports whether a and b are the same store value, without
// panicking on a store type that is not comparable.
func sameBeadStore(a, b beads.Store) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	ta, tb := reflect.TypeOf(a), reflect.TypeOf(b)
	if ta != tb || !ta.Comparable() {
		return false
	}
	return a == b
}

// opStoreOpenReason classifies an opjournal.NewStore failure as a degrade
// reason.
func opStoreOpenReason(err error) string {
	switch {
	case errors.Is(err, beads.ErrConditionalWriteUnsupported):
		return legacyop.DegradeConditionalWritesUnsupported
	case errors.Is(err, opjournal.ErrNoNamespace):
		return legacyop.DegradeNoNamespace
	default:
		return legacyop.DegradeStoreError
	}
}

// countOpSweepFailure counts a pass that could not sweep at all, beside the
// per-op counts legacyop.Sweep records.
func countOpSweepFailure() {
	telemetry.RecordOpRecord(context.Background(), "", legacyop.SurfaceController, legacyop.StageSweep, legacyop.ResultFailed, "")
}

// degrade hands d to the degrade hook.
func (s *opSweeper) degrade(d legacyop.Degrade) {
	if s.degraded != nil {
		s.degraded(d)
	}
}

// logDegrade is the default degrade hook: one log line per (stage, reason)
// per legacyop.DegradeLogInterval.
func (s *opSweeper) logDegrade(d legacyop.Degrade) {
	key := opSweepDegradeKey{stage: d.Stage, reason: d.Reason}
	now := s.clk.Now()
	s.mu.Lock()
	last, seen := s.degLogs[key]
	due := !seen || !now.Before(last.Add(legacyop.DegradeLogInterval))
	if due {
		if s.degLogs == nil {
			s.degLogs = map[opSweepDegradeKey]time.Time{}
		}
		s.degLogs[key] = now
	}
	s.mu.Unlock()
	if due {
		log.Printf("op-sweep: op journal degraded: mode=%s stage=%s reason=%s: %v", d.Mode, d.Stage, d.Reason, d.Err)
	}
}
