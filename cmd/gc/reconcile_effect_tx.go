package main

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync/atomic"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/session"
)

// The effect transaction (simplify/EFFECT-STRUCTURE.md §2.1): runTx alone
// takes the name and session mutation locks and CASes the row (casRow), each
// attempt reading first, so the CAS window holds only the row read and Decide,
// and beginning its write through the latch (beginWrite).

// effectSpec is one intent kind: what admission knows of it (P2-P4) and its
// effect. A kind with neither sections nor a body has none yet (no-effect).
type effectSpec struct {
	class     capClass
	bootGated bool // destructive: deferred while the boot gate is closed (P2)
	tokens    int  // debited on admission, never refunded (I9)
	needs     needs
	caps      caps
	sections  []section
	// body is the create's guarded row create (v5 R1 exception 1), the one
	// effect whose write is no row CAS; it begins it through beginWrite.
	body func(ctx context.Context, c txCaps) settlement
	// around, when set, wraps the effect (C5a1's endpoint ticket, C5c1's
	// post-close cascade): it calls run at most once, outside every lock.
	around func(ctx context.Context, a aroundCaps, run func() settlement) settlement
}

// caps are what a kind reaches beyond its row CAS, only through txCaps. A
// later capability is a bit and a txCaps field, added with its first kind.
type caps uint8

const capCreate caps = 1 // the create runner and its raw stores (v5 R1 exception 1)

// txCaps is what a body (and A4's Call) holds: the handles its spec grants,
// its latch, and the test seam; never the pass.
type txCaps struct {
	it      intent
	latch   *writeLatch
	seam    txSeamFunc
	section int         // the section's index, from 0, which seams report
	attempt int         // the CAS attempt, from 1, which seams report
	create  *createPass // capCreate
	creates *createEffects
}

// aroundCaps is what an around holds. It runs outside every lock, so it
// reaches no write and none of a Call's handles (provider start, routing,
// release, kill); C5a1's endpoint gate may add Eligible here, never Admit.
type aroundCaps struct{ it intent }

// capsFor is it's txCaps under grant.
func (p *effectPass) capsFor(it intent, grant caps, latch *writeLatch) txCaps {
	c := txCaps{it: it, latch: latch, seam: p.seam, attempt: 1}
	if grant&capCreate != 0 {
		c.create, c.creates = p.held.create, p.held.creates
	}
	return c
}

// beginWrite is a write's last check, immediately before it: the context,
// then the latch. Only a nil error lets the write begin.
func (c txCaps) beginWrite(ctx context.Context) error {
	if err := c.at(ctx, seamBeforeCAS); err != nil {
		return err
	}
	if ctx.Err() != nil {
		return context.Cause(ctx)
	}
	if !c.latch.begin() {
		return errAbandoned
	}
	return nil
}

// afterWrite is the after-write seam, for a write outside a row CAS (the
// create's).
func (c txCaps) afterWrite(ctx context.Context) error { return c.at(ctx, seamAfterWrite) }

// at runs the seam s, when one is set; its error, wrapping errInjected, ends
// the effect.
func (c txCaps) at(ctx context.Context, s txSeam) error {
	if c.seam == nil {
		return nil
	}
	if err := c.seam(ctx, s, c.it, c.section, c.attempt); err != nil {
		return fmt.Errorf("%w: %w", errInjected, err)
	}
	return nil
}

// txSeam is a point in an effect where a test, or a staging fault (H1),
// acts: pauses, runs an outside operation, or fails the effect.
type txSeam uint8

const (
	seamAfterReads   txSeam = iota + 1 // the attempt's runtime and leg reads done, before the row read
	seamAfterRowRead                   // the row read, before the premise
	seamBeforeCAS                      // a write's last check, before the context and the latch
	seamAfterWrite                     // a landed write, still under the section's lock
)

// txSeamFunc acts at a seam of one effect's section and attempt; an error
// ends the effect with cause injected: failed, or ambiguous once a write
// landed. stagingSeam, nil in production, is what only a gcstaging-tagged
// init sets (H1); bindHost arms each planner with it for its city.
type txSeamFunc func(ctx context.Context, s txSeam, it intent, section, attempt int) error

var stagingSeam func(cityPath string) txSeamFunc

// writeLatch is an effect's write-begun latch: the effect sets it
// immediately before each write (beginWrite), and the executor closes it
// when it abandons the effect. The first wins, and closing is terminal: an
// effect abandoned before its write writes nothing and settles failed; one
// abandoned after settles ambiguous, and no later beginWrite opens (P5). The
// create's follow-up writes past its row write (the marker, the failed-row
// close, the adopt stamp) do not pass through the latch.
type writeLatch struct{ state atomic.Uint32 }

const (
	latchOpen uint32 = iota
	latchBegun
	latchAbandoned      // before any write began
	latchAbandonedBegun // after one began
)

// begin opens a write; a nil latch always does.
func (l *writeLatch) begin() bool {
	return l == nil || l.state.CompareAndSwap(latchOpen, latchBegun) || l.state.Load() == latchBegun
}

// abandon closes the latch and reports whether a write had begun.
func (l *writeLatch) abandon() (begun bool) {
	if l == nil || l.state.CompareAndSwap(latchOpen, latchAbandoned) {
		return false
	}
	l.state.CompareAndSwap(latchBegun, latchAbandonedBegun)
	return l.state.Load() == latchAbandonedBegun
}

// begun reports whether a write began.
func (l *writeLatch) begun() bool {
	if l == nil {
		return false
	}
	s := l.state.Load()
	return s == latchBegun || s == latchAbandonedBegun
}

func (s effectSpec) runs() bool { return s.body != nil || len(s.sections) > 0 }

// needs are a kind's locks, fresh reads and CAS budget.
type needs struct {
	NameLock bool // held across every section
	Runtime  bool // the runtime read fresh each attempt; implies NameLock
	Attempts int  // CAS rounds per section, each deciding again; 0 is 3
}

// section is one span of an effect under the row's session mutation lock,
// behind its premise (v5 R2; tx.premise).
type section struct {
	// Decide decides on each attempt's fresh reads, under both locks, no I/O.
	Decide func(v txView) txStep
}

// txView is what Decide sees: the intent, the pass's inputs, fresh reads.
type txView struct {
	It    intent
	World *World
	Alloc *allocDecision
	Row   session.Info      // read from the backing this attempt
	Meta  map[string]string // Row's persisted metadata, which the census reads
	RT    *txRuntime        // when the spec needs Runtime
	Now   time.Time         // the attempt's since
}

// txStep is Decide's verdict: refuse (writing nothing), write, or neither.
// The effect lands if a section wrote, and is a no-op otherwise.
type txStep struct {
	Write  session.MetadataPatch
	Refuse string
	Facts  effectFacts
}

// effectFacts are what an effect learned or did besides its outcome, merged
// into every settlement it builds and applied on time or late (applyFacts).
type effectFacts struct {
	Events     []events.Event   // recorded once drained (session.woke, ...)
	Transition *drainTransition // legacy's drain telemetry
	Work       *workVerdict     // a worktree verdict for the work item's backoff
}

func (f effectFacts) empty() bool { return len(f.Events) == 0 && f.Transition == nil && f.Work == nil }

func (f *effectFacts) merge(g effectFacts) {
	f.Events = append(f.Events, g.Events...)
	f.Transition = cmp.Or(g.Transition, f.Transition)
	f.Work = cmp.Or(g.Work, f.Work)
}

// Transaction causes. A refusal backs the row off (P4).
const (
	causePremise   = "premise"    // the fresh row is not the row expected
	causeShutdown  = "shutdown"   // the context ended before its deadline
	causeAroundRun = "around-run" // an around ran its effect twice
	causeInjected  = "injected"   // a seam failed the effect (tests, staging)
)

// errInjected wraps a seam's error.
var errInjected = errors.New("v2 effect: failed at a seam")

// injected is the failure a seam's error ends the effect with.
func injected(err error) settlement {
	return settlement{Outcome: settledFailed, Cause: causeInjected, Err: err}
}

// errAbandoned: the executor abandoned the effect before its write began.
var errAbandoned = errors.New("v2 effect: abandoned at its deadline before its write")

// withRowMutationLock is session.WithSessionMutationLock, observed by tests.
var withRowMutationLock = session.WithSessionMutationLock

// runTx runs the effect of it as spec describes, under latch.
func runTx(ctx context.Context, p *effectPass, it intent, spec effectSpec, latch *writeLatch) settlement {
	c := p.capsFor(it, spec.caps, latch)
	if spec.around == nil {
		return runSpec(ctx, p, it, spec, c)
	}
	ran := false
	return spec.around(ctx, aroundCaps{it: it}, func() settlement {
		if ran {
			return settlement{Outcome: settledFailed, Cause: causeAroundRun}
		}
		ran = true
		return runSpec(ctx, p, it, spec, c)
	})
}

// runSpec is the effect itself: the body, or the sections under the locks.
func runSpec(ctx context.Context, p *effectPass, it intent, spec effectSpec, c txCaps) settlement {
	switch {
	case spec.body != nil:
		return spec.body(ctx, c)
	case len(spec.sections) == 0:
		return refused(causeNoEffect)
	}
	row, ok := p.World.Census.Rows[it.Key]
	if !ok {
		return refused(causeRedecided)
	}
	writer, ok := p.Writers[it.Key.Leg]
	if !ok {
		return settlement{Outcome: settledRefused, Cause: causeNoWriter, Err: errNoConditionalWriter}
	}
	t := &tx{c: c, p: p, needs: spec.needs, writer: writer, expect: row.Info, basis: it.Basis, view: txView{It: it, World: p.World, Alloc: p.Alloc}}
	if spec.needs.NameLock || spec.needs.Runtime {
		name, unlock, ok := lockRuntimeName(p.World, row.Info)
		switch {
		case !ok && name == "":
			return refused(causeRouteUnknown)
		case !ok:
			return refused(causeNameBusy)
		}
		defer unlock()
		t.name = name
	}
	return t.run(ctx, spec.sections)
}

// tx is one transaction across its sections.
type tx struct {
	c      txCaps
	p      *effectPass
	needs  needs
	writer fencedWriter
	name   string       // the runtime name, when the name lock is held
	expect session.Info // the row the premise expects
	basis  rowBasis     // its incarnation and token
	landed bool         // a section's write landed
	facts  effectFacts
	view   txView
}

// run runs the sections, each under the row's mutation lock.
func (t *tx) run(ctx context.Context, sections []section) settlement {
	s := settlement{Outcome: settledNoop}
	for i, sec := range sections {
		t.c.section = i
		end := false
		_ = withRowMutationLock(t.view.It.Key.ID, func() error {
			s, end = t.section(ctx, sec)
			return nil
		})
		if end {
			break
		}
	}
	s.Facts = t.facts
	return s
}

// section runs sec's attempts; end ends the effect with the settlement.
func (t *tx) section(ctx context.Context, sec section) (_ settlement, end bool) {
	for attempt := range cmp.Or(t.needs.Attempts, 3) {
		t.c.attempt = attempt + 1
		if s, ok := t.read(ctx); !ok {
			return s, true
		}
		var step txStep
		var ended *settlement
		wrote, err := t.writer.casRow(t.view.It.Key.ID, func(row session.Info, resp session.PersistedResponse) session.MetadataPatch {
			if step, ended = t.decide(ctx, sec, row, resp.Metadata); ended != nil {
				return nil
			}
			return step.Write
		})
		switch {
		case ended != nil:
			return *ended, true
		case errors.Is(err, beads.ErrRowRefreshFenced):
			continue // a newer write owns the row: read it again
		case errors.Is(err, errNoConditionalWriter):
			return settlement{Outcome: settledRefused, Cause: causeNoWriter, Err: err}, true
		case err != nil:
			return settlement{Outcome: settledFailed, Cause: causeWrite, Err: err}, true
		case wrote:
			t.landed, t.expect = true, wroteRow(t.view.Row, step.Write)
			t.basis = rowBasisOf(t.view.It.Key, t.expect)
			t.facts.merge(step.Facts)
			if err := t.c.at(ctx, seamAfterWrite); err != nil {
				s := injected(err)
				s.Outcome = settledAmbiguous // the write landed
				return s, true
			}
			return t.done(), false
		case len(step.Write) == 0:
			t.facts.merge(step.Facts)
			return t.done(), false
		}
		// The CAS lost to another writer: read again and decide again.
	}
	return refused(causeCAS), true
}

// done is the settlement after a concluded step: landed once a section
// wrote, otherwise a no-op.
func (t *tx) done() settlement {
	if t.landed {
		return settlement{Outcome: settledLanded}
	}
	return settlement{Outcome: settledNoop}
}

// read is an attempt's reads before the row: the since stamp and the
// runtime, which a context that ended proves nothing by.
func (t *tx) read(ctx context.Context) (settlement, bool) {
	t.view.Now = t.p.Clock.Now()
	if t.needs.Runtime {
		rt, cause := readRuntime(ctx, t.p.Runtime, processNamesFor(t.p.World, t.expect), t.name, t.view.Now, t.p.Clock.Now)
		if cause != "" {
			return refused(cause), false
		}
		t.view.RT = rt
	}
	if err := t.c.at(ctx, seamAfterReads); err != nil {
		return injected(err), false
	}
	if ctx.Err() != nil {
		return ended(ctx), false
	}
	return settlement{}, true
}

// decide is one attempt on row, read from the backing: the premise, Decide,
// and the context, checked last. A non-nil settlement ends the effect; a
// refusal or failure carries the step's facts.
func (t *tx) decide(ctx context.Context, sec section, row session.Info, meta map[string]string) (txStep, *settlement) {
	v := t.view
	v.Row, v.Meta = row, meta
	t.view.Row = row
	if err := t.c.at(ctx, seamAfterRowRead); err != nil {
		s := injected(err)
		return txStep{}, &s
	}
	if !t.premise(row) {
		s := refused(causePremise)
		return txStep{}, &s
	}
	step := sec.Decide(v)
	var s settlement
	switch {
	case step.Refuse != "":
		s = refused(step.Refuse)
	case len(step.Write) == 0:
		return step, nil
	default:
		err := t.c.beginWrite(ctx)
		switch {
		case err == nil:
			return step, nil
		case errors.Is(err, errInjected):
			s = injected(err)
		default:
			s = ended(ctx)
		}
		return step, &s
	}
	t.facts.merge(step.Facts)
	return step, &s
}

// premise reports whether row is still the expected row (the pass's, then
// the last landed write's): open, under the locked runtime name, at its
// incarnation and token, with its lifecycle facts (LifecycleInputFromInfo).
func (t *tx) premise(row session.Info) bool {
	if row.Closed || t.name != "" && strings.TrimSpace(row.SessionName) != t.name {
		return false
	}
	return rowBasisOf(t.view.It.Key, row) == t.basis &&
		reflect.DeepEqual(session.LifecycleInputFromInfo(row), session.LifecycleInputFromInfo(t.expect))
}

// rowBasisOf is row's incarnation and token, as the census reads them.
func rowBasisOf(k rowKey, row session.Info) rowBasis {
	r := newCensusRow(k, row)
	return rowBasis{Incarnation: r.Incarnation, InstanceToken: r.InstanceToken}
}

// ended is the failure of an effect that may not write: its context ended
// (context.Cause tells its deadline from a shutdown's cancel, whatever Err
// reads under a fake clock), or the executor abandoned it first.
func ended(ctx context.Context) settlement {
	switch cause := context.Cause(ctx); {
	case cause == nil:
		return settlement{Outcome: settledFailed, Cause: causeDeadline, Err: errAbandoned}
	case !errors.Is(cause, context.DeadlineExceeded):
		return settlement{Outcome: settledFailed, Cause: canceledCause(ctx), Err: cause}
	}
	return settlement{Outcome: settledFailed, Cause: causeDeadline, Err: context.DeadlineExceeded}
}

func refused(cause string) settlement { return settlement{Outcome: settledRefused, Cause: cause} }
