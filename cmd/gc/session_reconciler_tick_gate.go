package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/worker"
)

// tickProbeBound is the longest the reconcile tick waits for one session's
// runtime to answer the loop head's probe, and for one tick kill. It is a
// package var so tests can shrink it.
var tickProbeBound = postStartObservationBound

// tickObservationKey names what the tick's bounded observations keep at one in
// flight: one session in one city, in a namespace of the tick's own. It is not
// postStartObservationKey: an async start's commit holds that key while it is
// hung, and the reconciler's repair paths for that very session
// (SESSION-RUNTIME-035) must still run on the tick, so a held start key must
// not read as a runtime that did not answer. The cost is that a wedged session
// can hold one goroutine here and one under the start key.
func tickObservationKey(cityPath, sessionName string) string {
	return "tick\x00" + cityPath + "\x00" + sessionName
}

// tickProbeAbandonBudget is how many consecutive expired waits a tick makes
// before it stops asking: a provider-wide wedge costs this many bounds, not one
// per session.
const tickProbeAbandonBudget = 3

// tickProbeGate is the tick-scoped record of which sessions' runtimes answered
// the reconcile loop's first contact with them and which did not. The tick
// goroutine is the only caller that probes, but the lock keeps the record safe
// for the abandoned observations' bookkeeping and the trace readers.
//
// A runtime that does not answer defers its session for the tick: unavailable
// means deferred, never dead (SESSION-RUNTIME-009). The gate never reads the
// answer's value, only whether there was one; every decision about a running
// or stopped session stays where it is.
type tickProbeGate struct {
	ctx      context.Context
	cityPath string
	sp       runtime.Provider

	mu          sync.Mutex
	answered    map[string]bool   // session name -> its runtime answered this tick
	deferred    map[string]string // session name -> why it is deferred this tick
	consecutive int               // expired waits in a row, reset by any answer
}

// newTickProbeGate builds the gate for one reconcile tick. A nil ctx is the
// background context.
func newTickProbeGate(ctx context.Context, cityPath string, sp runtime.Provider) *tickProbeGate {
	if ctx == nil {
		ctx = context.Background()
	}
	return &tickProbeGate{
		ctx:      ctx,
		cityPath: cityPath,
		sp:       sp,
		answered: map[string]bool{},
		deferred: map[string]string{},
	}
}

// mayReconcile reports whether the session named name may be reconciled this
// tick: its runtime answered a bounded IsRunning probe. The first call for a
// name this tick makes the probe; later calls return the same answer without a
// provider call. A name whose runtime did not answer, whose observation is
// still outstanding from an earlier tick, or that comes after the abandon
// budget ran out, is refused. A nil gate, a gate without a provider and a blank
// name are admitted: there is no runtime to ask.
func (g *tickProbeGate) mayReconcile(name string) bool {
	if g == nil || g.sp == nil {
		return true
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return true
	}
	g.mu.Lock()
	if g.answered[name] {
		g.mu.Unlock()
		return true
	}
	if _, refused := g.deferred[name]; refused {
		g.mu.Unlock()
		return false
	}
	if g.consecutive >= tickProbeAbandonBudget {
		g.deferred[name] = fmt.Sprintf("skipped without a call: %d consecutive runtimes did not answer this tick", g.consecutive)
		g.mu.Unlock()
		return false
	}
	g.mu.Unlock()

	err := g.observe(name, func() error {
		g.sp.IsRunning(name)
		return nil
	})

	g.mu.Lock()
	defer g.mu.Unlock()
	if err != nil {
		g.deferred[name] = describeUnanswered(err)
		return false
	}
	g.answered[name] = true
	return true
}

// skipNote is why the gate deferred name this tick, or "" when it did not.
func (g *tickProbeGate) skipNote(name string) string {
	if g == nil {
		return ""
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.deferred[strings.TrimSpace(name)]
}

// boundedKill runs kill, a tick kill of the session named name, as one bounded
// observation: the kill and everything the site does to the runtime right after
// it (the scrollback clear) are one serial call, held to tickProbeBound. A kill
// that does not answer in time is an error wrapping runtime.ErrRuntimeUnavailable,
// which the site treats as a failed kill, and the observation key stays held
// until the call returns, so the next tick finds it outstanding instead of
// stopping again. A nil gate runs kill unbounded.
func (g *tickProbeGate) boundedKill(name string, kill func() error) error {
	if g == nil || g.sp == nil || strings.TrimSpace(name) == "" {
		return kill()
	}
	return g.observe(name, kill)
}

// observe runs call as a bounded observation of name and keeps the abandon
// count: an expired wait adds one, an answer (of any kind) resets it, and an
// outstanding observation, which made no call and waited for nothing, changes
// neither.
func (g *tickProbeGate) observe(name string, call func() error) error {
	ctx, cancel := context.WithTimeout(g.ctx, tickProbeBound)
	defer cancel()
	_, err := worker.ObserveBounded(ctx, tickObservationKey(g.cityPath, name), func(context.Context) (struct{}, error) {
		return struct{}{}, call()
	})
	g.mu.Lock()
	defer g.mu.Unlock()
	switch {
	case errors.Is(err, worker.ErrObservationOutstanding):
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
		g.consecutive++
	default:
		g.consecutive = 0
	}
	return err
}

// describeUnanswered is the note for a session whose runtime gave no answer.
func describeUnanswered(err error) string {
	if errors.Is(err, worker.ErrObservationOutstanding) {
		return "its runtime did not answer an earlier observation, which is still outstanding"
	}
	return fmt.Sprintf("its runtime did not answer within %s", tickProbeBound)
}
