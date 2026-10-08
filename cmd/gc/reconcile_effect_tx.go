package main

import (
	"cmp"
	"context"
	"errors"
	"reflect"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/session"
)

// The effect transaction (simplify/EFFECT-STRUCTURE.md §2.1): runTx alone
// takes the name and session mutation locks and CASes the row (casRow), each
// attempt reading first, so the CAS window holds only the row read and Decide.

// effectSpec is one intent kind: what admission knows of it (P2-P4) and its
// effect. A kind with neither sections nor a body has none yet (no-effect).
type effectSpec struct {
	class     capClass
	bootGated bool // destructive: deferred while the boot gate is closed (P2)
	tokens    int  // debited on admission, never refunded (I9)
	needs     needs
	sections  []section
	// body is the create's guarded row create (v5 R1 exception 1), the one
	// effect whose write is no row CAS.
	body func(ctx context.Context, p *effectPass, it intent) settlement
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
	causePremise  = "premise"  // the fresh row is not the row expected
	causeShutdown = "shutdown" // the context ended before its deadline
)

// withRowMutationLock is session.WithSessionMutationLock, observed by tests.
var withRowMutationLock = session.WithSessionMutationLock

// runTx runs the effect of it as spec describes.
func runTx(ctx context.Context, p *effectPass, it intent, spec effectSpec) settlement {
	switch {
	case spec.body != nil:
		return spec.body(ctx, p, it)
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
	t := &tx{p: p, needs: spec.needs, writer: writer, expect: row.Info, basis: it.Basis, view: txView{It: it, World: p.World, Alloc: p.Alloc}}
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
	for _, sec := range sections {
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
	for range cmp.Or(t.needs.Attempts, 3) {
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
	case ctx.Err() != nil:
		s = ended(ctx)
		return step, &s
	default:
		return step, nil
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

// ended is the failure of an effect whose context ended: context.Cause
// tells its deadline from a shutdown's cancel, whatever Err reads under a
// fake clock.
func ended(ctx context.Context) settlement {
	if cause := context.Cause(ctx); !errors.Is(cause, context.DeadlineExceeded) {
		return settlement{Outcome: settledFailed, Cause: causeShutdown, Err: cause}
	}
	return settlement{Outcome: settledFailed, Cause: causeDeadline, Err: context.DeadlineExceeded}
}

func refused(cause string) settlement { return settlement{Outcome: settledRefused, Cause: cause} }
