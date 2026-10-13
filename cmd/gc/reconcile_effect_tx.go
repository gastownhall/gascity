package main

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
)

// The effect transaction (simplify/EFFECT-STRUCTURE.md §2.1): runTx alone
// takes the name and session mutation locks and CASes the row (casRow), each
// attempt reading first, so the CAS window holds only the row read and Decide,
// and beginning its write through the latch (beginWrite). A section's Call
// runs after it under the name lock alone, gated like a write.

// effectSpec is one intent kind: what admission knows of it (P2-P4) and its
// effect. A kind with neither sections nor a body has none yet (no-effect).
type effectSpec struct {
	class     capClass
	bootGated bool // destructive: deferred while the boot gate is closed (P2)
	tokens    int  // debited on admission, never refunded (I9)
	needs     needs
	needsFor  func(w *World, it intent) needs // per-intent needs, when set (drain begin's legs by reason and policy)
	caps      caps
	sections  []section
	// body is the create's guarded row create (v5 R1 exception 1), the one
	// effect whose write is no row CAS; it begins it through beginWrite.
	body func(ctx context.Context, c txCaps) settlement
	// around, when set, wraps the effect (C5a1's endpoint gate, C5c1's
	// post-close cascade): it calls run at most once, outside every lock. A
	// ticket admitted only on Launch is an onExit finalizer instead.
	around func(ctx context.Context, a aroundCaps, run func() settlement) settlement
}

// caps are what a kind reaches beyond its row CAS, only through txCaps. A
// later capability is a bit and a txCaps field, added with its first kind.
type caps uint8

const (
	capCreate        caps = 1 << iota // the create runner and its raw stores (v5 R1 exception 1)
	capReadStores                     // the read-only city and rig stores (reads)
	capProviderStart                  // the start's provider, endpoint breaker and clock (a Call's)
	capEpisode                        // the #46 startup-health episode record (a Call's)
	capRetire                         // the retired-session cleanup after a close (an around's)
	capRelease                        // the close's raw work legs, for its release Call and its around's cascade
	capProviderStop                   // the stop's composite provider and clock (a Call's)
)

// txCaps is what a body, Probe or Call holds: the handles its spec grants,
// its latch, its finalizers and the seam; never the pass.
type txCaps struct {
	it      intent
	latch   *writeLatch
	seam    txSeamFunc
	section int                       // the section's index, from 0, which seams report
	attempt int                       // the CAS attempt, from 1, which seams report
	exits   *[]func(final settlement) // onExit's, run at every exit
	create  *createPass               // capCreate
	creates *createEffects
	reads   effectReads         // capReadStores: read-only, blind writes refused
	start   startCaps           // capProviderStart
	episode startupHealthRecord // capEpisode
	stop    stopCaps            // capProviderStop
	// by is the controller, as a Call's leasing helper (a Manager start or
	// stop) is handed it, carrying the effect's lease (callWatched).
	by      session.Actor
	release *closeReleases // capRelease
}

// stopCaps are what the stop verb's Call holds: the composite provider,
// whose routed leaf the fence's verdict names and through whose hops C8.8
// confirms, and the clock.
type stopCaps struct {
	sp    runtime.Provider
	clock plannerClock
}

// startCaps are what the start's Calls hold to admit the endpoint ticket and
// start a runtime: the composite provider, which the Call routes (RouteACP)
// and resolves to its leaf itself, since capsFor runs before the name lock;
// the breaker; the recorder and stderr its transitions go to; the clock.
type startCaps struct {
	sp       runtime.Provider
	capacity *endpointCapacityGuard
	rec      events.Recorder
	stderr   io.Writer
	clock    plannerClock
}

// aroundCaps is what an around holds. It runs outside every lock, so it
// reaches no row write and none of a Call's handles (provider start,
// routing, kill), but two after-close writers: the close's cascade
// (capRelease), which writes only after its close landed, and a closed row's
// cleanup (capRetire), which writes only its waits and bindings; C5a1's
// endpoint gate may add Eligible here, never Admit.
type aroundCaps struct {
	it      intent
	reads   effectReads     // capReadStores: read-only
	release *closeReleases  // capRelease
	retire  *retiredCleanup // capRetire, on the pass's clock
}

// capsFor is it's txCaps under grant.
func (p *effectPass) capsFor(it intent, grant caps, latch *writeLatch) txCaps {
	c := txCaps{it: it, latch: latch, seam: p.seam, attempt: 1, exits: new([]func(settlement))}
	if grant&capCreate != 0 {
		c.create, c.creates = p.held.create, p.held.creates
	}
	if grant&capReadStores != 0 {
		c.reads = p.reads
	}
	if grant&capProviderStart != 0 {
		c.start = p.held.start
		c.start.sp, c.start.clock = p.Runtime, p.Clock
	}
	if grant&capEpisode != 0 {
		c.episode = p.held.episode
	}
	if grant&capRelease != 0 {
		c.release = p.held.release
	}
	if grant&capProviderStop != 0 {
		c.stop = stopCaps{sp: p.Runtime, clock: p.Clock}
	}
	return c
}

// onExit registers f to run with the effect's final settlement at every
// exit, a panic included (as a failed causePanic, then re-panicking): C5a1's
// endpoint ticket, admitted only on Launch (SC N1), resolved however the
// effect ends. The last registered runs first. A finalizer runs after the
// effect has released its runtime lease and every lock: it may settle what
// the effect admitted (a ticket, a count), and must not call the runtime or
// write the row.
func (c txCaps) onExit(f func(final settlement)) { *c.exits = append(*c.exits, f) }

// exit runs every finalizer, last registered first, with s, or with a
// panic's failure; a finalizer's panic skips none of the others, and the
// first panic, the effect's own before any finalizer's, is raised again.
func (c txCaps) exit(s *settlement) {
	r := recover()
	final := *s
	if r != nil {
		final = settlement{Outcome: settledFailed, Cause: causePanic, Err: fmt.Errorf("effect panicked: %v", r)}
	}
	for i := len(*c.exits) - 1; i >= 0; i-- {
		func() {
			defer func() {
				if fr := recover(); fr != nil && r == nil {
					r = fr
				}
			}()
			(*c.exits)[i](final)
		}()
	}
	if r != nil {
		panic(r)
	}
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
	seamBeforeCall                     // before a Call's gate (the context, then the latch)
	seamAfterCall                      // a Call returned: the name lock held, the mutation lock released
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

// needs are a kind's locks, fresh reads and CAS budget. Every read implies
// the name lock, as does a Call.
type needs struct {
	NameLock  bool      // held across every section
	Runtime   bool      // the runtime read fresh each attempt
	Legs      fenceLegs // legAttach (L3), legPending (L4) on the routed leaf; legWork (L5) live
	Idle      bool      // the agent proved idle on the routed leaf
	Activity  bool      // the agent's activity read on the routed leaf (a named drift restart's deferral, the idle stop's proof)
	Content   bool      // the agent's pane read idle or not on the routed leaf (the content-clock idle stop's proof)
	Escalates bool      // a C8.5 escalation class: L3 escalates on a terminal-no-report leaf
	Attempts  int       // CAS rounds per section, each deciding again; 0 is 3
	// Lease takes the row's runtime lease record, not the name's flock
	// alone: an effect that creates, destroys or restarts a runtime, or that
	// writes the incarnation or the commit. Every section with a Call needs
	// it (the registry test).
	Lease bool
	// LeaseAt, with Lease, takes the record only just before that section
	// (the name's flock from the start), as legacy's start takes its record
	// only before PreWake: an effect that ends before it, a deferral, writes
	// nothing.
	LeaseAt int
}

func (s effectSpec) needsOf(w *World, it intent) needs {
	if s.needsFor != nil {
		return s.needsFor(w, it)
	}
	return s.needs
}

// premiseRule is what a section's fresh row must still be (v5 R2).
type premiseRule uint8

const (
	// premiseDefault: open, carrying the locked runtime name, at the
	// incarnation and token expected, with the premise facts
	// (session.FactsOf, the registry's premise keys) of the expected row: the
	// pass's, then the one the last landed section wrote. An effect that
	// reads the row's work (L5) compares its assignment identity keys too.
	// After a callStart, the facts compared are the registry's
	// FactsAfterStart site's: the started runtime writes the keys it drops.
	premiseDefault premiseRule = iota
	// premiseOwnToken (v5 S2's commit): after an earlier section's write
	// landed, the registry's FactsCommit site: open, creating, active or
	// awake, and still carrying the token that write set; holds do not veto
	// it.
	premiseOwnToken
	// premiseClose: a close section (txStep.Terminal). The default premise, and
	// no kill fence (tx.decide); a row already closed is a no-op that ends the
	// effect.
	premiseClose
	// premiseDrainBasis: a drain begin (A20). Open, carrying the locked
	// runtime name, at the basis expected, with the registry's FactsDrainBasis
	// site's facts: the drain basis, so an agent's heartbeat (held_until)
	// cannot starve the begin; its Decide re-decides on the fresh row.
	premiseDrainBasis
	// premiseBaseline: a baseline write (A8). The default premise with the
	// registry's FactsBaseline site's facts: the incarnation, the stored
	// config hash and the stop request; its Decide re-decides on the fresh
	// row.
	premiseBaseline
)

// section is one span of an effect under the row's session mutation lock,
// behind its premise (tx.premise). Build one with a Probe or a Call through probed and called, which type
// their data.
type section struct {
	Premise premiseRule
	// Decide decides on the attempt's fresh reads, under both locks, with no
	// I/O; it runs once per attempt. Every function stored here is a v2purity
	// root:
	//
	//gc:pure
	Decide func(v txView) txStep
	// Moved, when set, decides an attempt whose premise moved, which
	// otherwise refuses: v5 S2's lost commit. It sees the expected row and
	// what moved; ok false is the premise's refusal. It never writes: a step
	// that does fails the effect.
	//
	//gc:pure
	Moved func(v txView, expect session.Info, moved []string) (_ txStep, ok bool)
	probe func(ctx context.Context, r effectReads, v txView) (any, error)
	call  func(ctx context.Context, c txCaps, in any) (any, error)
	// callKind is the Call's kind (called), when it has one.
	callKind callKind
	// The kind's own Probe, Decide and Call as given to probed and called,
	// which the effect lint covers; each set by one write, never read by the
	// transaction.
	probeDef, decideDef, callDef any
	// when, set by calledWhen, decides from the concluded step's Pass
	// whether the Call runs at all: a skipped Call is no write, so it passes
	// no gate, opens no latch and runs no seam.
	when func(pass any) bool
}

// probed is a section whose Probe, a bounded read under both locks after the
// transaction's own reads (it sees the expected row), hands Decide its typed
// result. A probe past fenceProbeTimeout answers errProbeExpired. Every decide
// passed here is a v2purity root, as a section's Decide is; the probe is
// stored, reached only through a read of section.probe (or probeDef):
//
//gc:pure-param decide
//gc:stored-param probe
func probed[P any](probe func(context.Context, effectReads, txView) (P, error), decide func(txView, P, error) txStep) section {
	return section{
		probe: func(ctx context.Context, r effectReads, v txView) (any, error) { return probe(ctx, r, v) },
		Decide: func(v txView) txStep {
			p, _ := v.probe.(P)
			return decide(v, p, v.probeErr)
		},
		probeDef:  probe,
		decideDef: decide,
	}
}

// callKind is what a section's Call does to the runtime, which decides the
// premise of the sections after it.
type callKind uint8

const (
	// callStart starts a runtime: the sections after it compare the
	// registry's FactsAfterStart facts, since the started runtime writes the
	// keys that exception drops.
	callStart callKind = iota + 1
	// callStop stops one: after it the premise is the registry's
	// FactsAfterStop site's (the stop verb's finalize, v5 D3), whatever a
	// start before it ran.
	callStop
	// callWork writes work beads (a release), not the runtime: the premise
	// after it is the one before it.
	callWork
)

// called is sec with a provider Call after it, run under the name lock with
// the mutation lock released, and only like a write (the context, then the
// latch). It takes the step's Pass, typed In, and hands the next section's
// Decide its result (callResult[Out]). The call is stored, reached only
// through a read of section.call (or callDef):
//
//gc:stored-param call
func called[In, Out any](kind callKind, sec section, call func(context.Context, txCaps, In) (Out, error)) section {
	sec.callKind = kind
	sec.call = func(ctx context.Context, c txCaps, in any) (any, error) {
		typed, _ := in.(In)
		return call(ctx, c, typed)
	}
	sec.callDef = call
	return sec
}

// calledWhen is called with an opt-in skip: the Call runs only when when
// accepts the step's Pass (a corpse to remove, a release to make). By
// default a section's Call always runs, and opens the latch like a write.
// Both are stored:
//
//gc:stored-param call
//gc:stored-param when
func calledWhen[In, Out any](kind callKind, sec section, when func(In) bool, call func(context.Context, txCaps, In) (Out, error)) section {
	sec = called(kind, sec, call)
	sec.when = func(pass any) bool {
		typed, ok := pass.(In)
		return ok && when(typed)
	}
	return sec
}

// callFacts is a Call result that carries facts of what the Call did (a
// stop's session.stopped), merged as the Call returns, so a later section's
// refusal does not drop them.
type callFacts interface{ facts() effectFacts }

// callResult is the previous section's Call result and error.
func callResult[Out any](v txView) (Out, error) {
	out, _ := v.prev.(Out)
	return out, v.prevErr
}

// errProbeExpired: a section's Probe, or the live work read, ran past its
// bound.
var errProbeExpired = errors.New("v2 effect: probe expired")

// txView is what Decide sees: the intent, the pass's inputs, fresh reads.
type txView struct {
	It    intent
	World *World
	Alloc *allocDecision
	Row   session.Info      // read from the backing this attempt
	Meta  map[string]string // Row's persisted metadata, which the census reads
	RT    *txRuntime        // when the spec needs Runtime
	Fence txFence           // the legs the spec needs
	Now   time.Time         // the attempt's since
	// The section's Probe result, and the previous section's Call result
	// (probed, callResult).
	probe, prev       any
	probeErr, prevErr error
}

// txFence is the fence legs read fresh for Decide: each reason holds the
// action, "" passes (attachLeg, boundedPending); Work is L5, read live.
type txFence struct {
	Attach, Pending string
	AttachEscalate  bool      // L3 escalates (C8.5, on a terminal-no-report leaf)
	Read            fenceLegs // the legs read: one not read holds (fenceDestructive)
	Work            *txWork
	Idle            bool // proved idle: WaitForIdle, then no activity since the pass
	Activity        *activityRead
	Content         contentState // the pane's fresh snapshot (needs.Content)
}

// workHeld reports L5 holding: work read on the seat, or a read that failed
// or was not made (fails closed).
func (f txFence) workHeld() bool { return f.Work == nil || f.Work.Err != nil || !f.Work.Free }

// activityRead is the routed leaf's activity read: whether it reports
// activity for the name, and its last.
type activityRead struct {
	Reportable bool
	Last       time.Time
	Err        error
}

// txStep is Decide's verdict: refuse or fail (writing nothing), write, or
// neither. Done ends the effect after the step: landed if a section wrote,
// otherwise a no-op with Cause. Without Done, the section's Call and the
// next section run. Pass is what the section's Call takes. A Fail with a
// Write (or a Terminal) is an abandon (v5 S3): the write lands, and the
// effect ends failed with its cause. Last runs the section's Call, then
// ends the effect: a landed commit's after-commit Call, ahead of the
// sections only a failure reaches.
type txStep struct {
	Write session.MetadataPatch
	// Class is the disposition cells' keys the write carries beside Write (or,
	// in a premiseClose section, beside Terminal), minted where the section
	// writes them.
	Class session.DispositionPatch
	// Title, Type and the label deltas ride Write's CAS (LL4's
	// UpdateRowFenced): the row-metadata verb's (M1). A step that sets a
	// title or labels writes metadata too (its synced_at); a type repair
	// alone writes none, as legacy's RepairType.
	Title, Type             *string
	AddLabels, RemoveLabels []string
	Terminal                session.MetadataPatch // a premiseClose section's close, with this terminal patch; never with Write
	Refuse                  string
	Fail                    string
	Err                     error
	Done                    bool
	Last                    bool
	Cause                   string
	Facts                   effectFacts
	Pass                    any
}

// writes reports a step that writes the row: metadata, the disposition's
// cells, a terminal close, its title, type or labels.
func (s txStep) writes() bool {
	return len(s.Write) > 0 || s.Class.Len() > 0 || len(s.Terminal) > 0 || s.Title != nil || s.Type != nil || len(s.AddLabels) > 0 || len(s.RemoveLabels) > 0
}

// effectFacts are what an effect learned or did besides its outcome, merged
// into every settlement it builds and applied on time or late (applyFacts).
type effectFacts struct {
	Events     []events.Event   // recorded once drained (session.woke, ...)
	Transition *drainTransition // legacy's drain telemetry
	Work       *workVerdict     // a worktree verdict for the work item's backoff
	// Noted is a fresh read that found the row's runtime alive and Current,
	// which the planner notes in the observation cache (v5 O4).
	Noted *notedRuntime
	// Quarantined is the agent a landed death accrual quarantined, for
	// legacy's RecordAgentQuarantine.
	Quarantined string
	// IdleKilled is the agent an idle-timeout stop stopped, for legacy's
	// RecordAgentIdleKill.
	IdleKilled string
	// AliasOwed is the row whose landed alias write owes its runtime the
	// alias (A7's runtime-alias sync, M1).
	AliasOwed *rowKey
	// Stopped is a stop the stop verb performed, for legacy's stop metric.
	Stopped *agentStop
	// Classified is the (row, runtime token) a classification completed,
	// which the planner remembers so neither A12 nor the start's recycle
	// classifies a pair twice (v5 S5, X10).
	Classified *classifiedRuntime
}

// classifiedRuntime is a row's dead runtime, by its runtime token, and the
// crash its classification found, which the recycle records once its kill
// lands (a refused kill keeps it for the next attempt, which peeks no more).
type classifiedRuntime struct {
	Key   rowKey
	Token string // deadToken
	Crash *events.Event
	// Recycle marks the start's recycle's classification, which A6's
	// heals do not take (A12's alone, CONTRACT A6).
	Recycle bool
}

// notedRuntime is a runtime name a fresh read issued at At found alive.
type notedRuntime struct {
	Name string
	At   time.Time
}

func (f effectFacts) empty() bool {
	return len(f.Events) == 0 && f.Transition == nil && f.Work == nil && f.Noted == nil && f.Quarantined == "" && f.AliasOwed == nil && f.Stopped == nil && f.Classified == nil && f.IdleKilled == ""
}

func (f *effectFacts) merge(g effectFacts) {
	f.Events = append(f.Events, g.Events...)
	f.Transition = cmp.Or(g.Transition, f.Transition)
	f.Work = cmp.Or(g.Work, f.Work)
	f.Noted = cmp.Or(g.Noted, f.Noted)
	f.Quarantined = cmp.Or(g.Quarantined, f.Quarantined)
	f.AliasOwed = cmp.Or(g.AliasOwed, f.AliasOwed)
	f.Stopped = cmp.Or(g.Stopped, f.Stopped)
	f.Classified = cmp.Or(g.Classified, f.Classified)
	f.IdleKilled = cmp.Or(g.IdleKilled, f.IdleKilled)
}

// Transaction causes. A refusal backs the row off (P4).
const (
	causePremise   = "premise"    // the fresh row is not the row expected
	causeShutdown  = "shutdown"   // the context ended before its deadline
	causeAroundRun = "around-run" // an around ran its effect twice
	causeInjected  = "injected"   // a seam failed the effect (tests, staging)
	causeCallError = "call-error" // the last section's provider call failed
	causeStep      = "step"       // a verdict its section's rule forbids (a Terminal outside premiseClose, a Write in one)
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
func runTx(ctx context.Context, p *effectPass, it intent, spec effectSpec, latch *writeLatch) (s settlement) {
	c := p.capsFor(it, spec.caps, latch)
	defer c.exit(&s)
	if spec.around == nil {
		return runSpec(ctx, p, it, spec, c)
	}
	ran := false
	a := aroundCaps{it: it, reads: c.reads, release: c.release}
	if spec.caps&capRetire != 0 {
		r := p.held.retire
		r.clock = p.Clock
		a.retire = &r
	}
	return spec.around(ctx, a, func() settlement {
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
	t := &tx{c: c, p: p, needs: spec.needsOf(p.World, it), writer: writer, expect: row.Info, names: processNamesFor(p.World, row.Info), facts0: row.Facts, basis: it.Basis, view: txView{It: it, World: p.World, Alloc: p.Alloc}}
	t.needs.Lease = t.needs.Lease || spec.needs.Lease // a per-intent needsFor cannot drop the record
	if t.needs.locksName(spec) {
		var front *session.Store
		if t.needs.Lease {
			if ctx.Err() != nil { // past its deadline: write no record
				return ended(ctx)
			}
			var err error
			if front, err = writer.leaseFront(); err != nil {
				return settlement{Outcome: settledRefused, Cause: causeNoWriter, Err: err}
			}
		}
		ttl := session.RuntimeLeaseTTL(effectBudget(ctx))
		if t.needs.Lease && t.needs.LeaseAt > 0 {
			t.recordFront, t.recordTTL, front = front, ttl, nil // the flock now, the record at LeaseAt
		}
		name, lease, cause, err := lockRuntimeName(p.World, row.Info, front, ttl)
		if cause != "" {
			return settlement{Outcome: settledRefused, Cause: cause, Err: err}
		}
		defer lease.Release()
		t.name, t.lease = name, lease
		var cancel context.CancelFunc
		ctx, cancel = leaseBound(ctx, lease)
		defer cancel()
	}
	return t.run(ctx, spec.sections)
}

// leaseBound is ctx bounded by lease's record: one epoch per effect ends by
// the lease's SafeUntil, compared as remaining durations, never wall clock
// to wall clock. A flock-only lease bounds nothing.
func leaseBound(ctx context.Context, lease *session.RuntimeLease) (context.Context, context.CancelFunc) {
	if lease == nil || lease.Epoch() == 0 {
		return ctx, func() {}
	}
	return context.WithTimeoutCause(ctx, time.Until(lease.SafeUntil()), session.ErrRuntimeLeaseExpired)
}

// takeRecord takes the lease record a LeaseAt effect deferred, bounding ctx
// by it; a refusal is lockRuntimeName's.
func (t *tx) takeRecord(ctx context.Context) (context.Context, context.CancelFunc, *settlement) {
	if ctx.Err() != nil { // past its deadline: write no record, as the eager take
		s := ended(ctx)
		return ctx, func() {}, &s
	}
	req := session.RuntimeLeaseRequest{City: t.p.World.CityPath, Name: t.name, ID: t.view.It.Key.ID, TTL: t.recordTTL}
	if err := t.lease.TakeRecord(t.recordFront, req); err != nil {
		return ctx, func() {}, &settlement{Outcome: settledRefused, Cause: leaseRefusal(err), Err: err}
	}
	ctx, cancel := leaseBound(ctx, t.lease)
	return ctx, cancel, nil
}

// locksName reports an effect that holds its row's runtime name: one that
// takes the lease record, needs the name, the runtime, a fence leg or
// idleness, or has a Call.
func (n needs) locksName(spec effectSpec) bool {
	return n.Lease || n.NameLock || n.Runtime || n.Legs != 0 || n.Idle || slices.ContainsFunc(spec.sections, func(s section) bool { return s.call != nil })
}

// leaseWatchEvery is how often a Call's lease watch reads the row.
var leaseWatchEvery = 5 * time.Second

// effectBudget is what remains of ctx's deadline: the effect's own TTL. An
// effect with no deadline budgets nothing, and its lease lasts the margin.
func effectBudget(ctx context.Context) time.Duration {
	if dl, ok := ctx.Deadline(); ok {
		return time.Until(dl)
	}
	return 0
}

// tx is one transaction across its sections.
type tx struct {
	c      txCaps
	p      *effectPass
	needs  needs
	writer fencedWriter
	name   string                // the runtime name, when the name lock is held
	lease  *session.RuntimeLease // the name's lease: its flock, and with needs.Lease the row's record
	// recordFront and recordTTL are a LeaseAt effect's record to take.
	recordFront *session.Store
	recordTTL   time.Duration
	expect      session.Info  // the row the premise expects
	names       []string      // its template's process names, keyed on the pass's row
	facts0      session.Facts // its premise facts
	basis       rowBasis      // its incarnation and token
	started     bool          // a callStart ran, and no callStop since
	stopped     bool          // a callStop ran
	landed      bool          // a section's write landed
	last        bool          // the concluded step ends the effect after its Call (txStep.Last)
	facts       effectFacts
	pass        any // the concluded step's Pass, to its section's Call
	view        txView
}

// run runs the sections, each under the row's mutation lock.
func (t *tx) run(ctx context.Context, sections []section) settlement {
	s := settlement{Outcome: settledNoop}
	for i, sec := range sections {
		if t.recordFront != nil && i == t.needs.LeaseAt {
			var cancel context.CancelFunc
			var refused *settlement
			if ctx, cancel, refused = t.takeRecord(ctx); refused != nil {
				return *refused
			}
			defer cancel()
		}
		t.c.section = i
		end := false
		_ = withRowMutationLock(t.view.It.Key.ID, func() error {
			s, end = t.section(ctx, sec)
			return nil
		})
		if end {
			break
		}
		if t.view.prev, t.view.prevErr = nil, nil; sec.call == nil || sec.when != nil && !sec.when(t.pass) {
			if t.last {
				break
			}
			continue
		}
		if err := t.c.at(ctx, seamBeforeCall); err != nil {
			s = injected(err)
			break
		}
		// A provider call is a write: an effect the executor abandoned first
		// never calls, and one abandoned during it is ambiguous.
		final := i == len(sections)-1 || t.last
		if ctx.Err() != nil || !t.c.latch.begin() {
			if s.Outcome != settledLanded || !final {
				s = ended(ctx) // a landed last section stays landed: its Call never fails the effect
			}
			break
		}
		t.view.prev, t.view.prevErr = t.callWatched(ctx, sec)
		if f, ok := t.view.prev.(callFacts); ok { // what the Call did, kept whatever the later sections decide
			t.facts.merge(f.facts())
		}
		switch sec.callKind {
		case callStart:
			t.started = true
		case callStop:
			t.started, t.stopped = false, true // a stopped runtime writes nothing more
		}
		if err := t.c.at(ctx, seamAfterCall); err != nil {
			s = injected(err)
			break
		}
		if final && t.view.prevErr != nil {
			s = settlement{Outcome: settledFailed, Cause: causeCallError, Err: t.view.prevErr}
		}
		if t.last {
			break
		}
	}
	s.Facts = t.facts
	return s
}

// section runs sec's attempts; end ends the effect with the settlement.
func (t *tx) section(ctx context.Context, sec section) (_ settlement, end bool) {
	if sec.Premise == premiseClose {
		return t.closeSection(ctx, sec)
	}
	for attempt := range cmp.Or(t.needs.Attempts, 3) {
		t.c.attempt = attempt + 1
		if s, ok := t.read(ctx, sec); !ok {
			return s, true
		}
		var step txStep
		var ended *settlement
		var wroteFacts session.Facts
		wrote, err := t.writer.casRow(t.view.It.Key.ID, func(row session.Info, resp session.PersistedResponse) session.RowPatch {
			if step, ended = t.decide(ctx, sec, row, resp.Metadata); ended != nil || len(step.Write) == 0 && step.Class.Len() == 0 && step.Type == nil {
				return session.RowPatch{}
			}
			if step.Class.Len() > 0 {
				// The cells the deciders moved, minted for this row's write; a
				// lost attempt unmints (Store.UpdateMetadataFenced).
				step.Write = step.Class.Into(t.view.It.Key.ID, maps.Clone(step.Write))
			}
			wroteFacts = session.FactsOf(step.Write.Apply(resp.Metadata))
			return session.RowPatch{Metadata: step.Write, Title: step.Title, Type: step.Type, AddLabels: step.AddLabels, RemoveLabels: step.RemoveLabels}
		})
		switch {
		case ended != nil:
			return *ended, true
		case errors.Is(err, beads.ErrRowRefreshFenced):
			continue // a newer write owns the row: read it again
		case errors.Is(err, errNoConditionalWriter):
			return settlement{Outcome: settledRefused, Cause: causeNoWriter, Err: err}, true
		case errors.As(err, new(*beads.ConditionalUpdateFieldUnsupportedError)):
			return settlement{Outcome: settledRefused, Cause: causeLabelsUnguarded, Err: err}, true
		case err != nil:
			return settlement{Outcome: settledFailed, Cause: causeWrite, Err: err}, true
		case wrote:
			t.landed, t.expect, t.facts0 = true, wroteRow(t.view.Row, step.Write), wroteFacts
			t.basis = rowBasisOf(t.view.It.Key, t.expect)
			if err := t.c.at(ctx, seamAfterWrite); err != nil {
				t.facts.merge(step.Facts)
				s := injected(err)
				s.Outcome = settledAmbiguous // the write landed
				return s, true
			}
			fallthrough
		case len(step.Write) == 0:
			t.facts.merge(step.Facts)
			t.pass, t.last = step.Pass, step.Last
			return t.done(step), step.Done || step.Fail != ""
		}
		// The CAS lost to another writer: read again and decide again.
	}
	return refused(causeCAS), true
}

// closeSection runs a close section's attempts: each reads, decides, and
// closes once at the read's revision (closeRow). A landed close ends the
// effect, as a row already closed does (a no-op); a lost fence decides
// again.
func (t *tx) closeSection(ctx context.Context, sec section) (settlement, bool) {
	for attempt := range cmp.Or(t.needs.Attempts, 3) {
		t.c.attempt = attempt + 1
		if s, ok := t.read(ctx, sec); !ok {
			return s, true
		}
		var step txStep
		var ended *settlement
		read := false
		closed, err := t.writer.closeRow(t.view.It.Key.ID, func(row session.Info, resp session.PersistedResponse) (session.MetadataPatch, bool) {
			read = true
			if step, ended = t.decide(ctx, sec, row, resp.Metadata); ended != nil || len(step.Terminal) == 0 {
				return nil, false
			}
			if step.Class.Len() > 0 {
				// The close's cells, minted for this row's close.
				step.Terminal = step.Class.Into(t.view.It.Key.ID, maps.Clone(step.Terminal))
			}
			return step.Terminal, true
		})
		switch {
		case ended != nil:
			return *ended, true
		case errors.Is(err, beads.ErrRowRefreshFenced):
			continue
		case errors.Is(err, errNoConditionalWriter):
			return settlement{Outcome: settledRefused, Cause: causeNoWriter, Err: err}, true
		case err != nil:
			return settlement{Outcome: settledFailed, Cause: causeWrite, Err: err}, true
		case !read: // closed by another writer first
			return t.done(txStep{Cause: "already-closed"}), true
		case closed:
			t.landed = true
			t.facts.merge(step.Facts)
			s := t.done(step)
			if err := t.c.at(ctx, seamAfterWrite); err != nil {
				s = injected(err)
				s.Outcome = settledAmbiguous // the close landed
			}
			s.Closed = true
			return s, true
		case len(step.Terminal) == 0:
			t.facts.merge(step.Facts)
			t.pass = step.Pass
			return t.done(step), step.Done
		}
		// The close lost its fence: read again and decide again.
	}
	return refused(causeCAS), true
}

// done is the settlement after a concluded step: an abandon's failure once
// its write landed, landed once a section wrote, otherwise a no-op with the
// step's cause.
func (t *tx) done(step txStep) settlement {
	switch {
	case step.Fail != "":
		return settlement{Outcome: settledFailed, Cause: step.Fail, Err: step.Err}
	case t.landed:
		s := settlement{Outcome: settledLanded}
		if len(step.Write) > 0 { // this step's write stood in for a refusal (healInstead)
			s.Cause = step.Cause
		}
		return s
	}
	return settlement{Outcome: settledNoop, Cause: step.Cause}
}

// read is an attempt's reads before the row: the since stamp, the runtime,
// the fence legs and the Probe, which a context that ended proves nothing
// by.
func (t *tx) read(ctx context.Context, sec section) (settlement, bool) {
	t.view.Now = t.p.Clock.Now()
	if t.needs.Runtime {
		rt, cause := readRuntime(ctx, t.p.Runtime, t.names, t.name, t.view.Now, t.p.Clock.Now)
		if cause != "" {
			return refused(cause), false
		}
		t.view.RT = rt
	}
	if t.needs.Legs != 0 || t.needs.Idle {
		if cause := t.readFence(ctx); cause != "" {
			return refused(cause), false
		}
	}
	if sec.probe != nil {
		v := t.view
		v.Row = t.expect
		type result struct {
			v   any
			err error
		}
		r, ok := boundedProbeCtx(ctx, func(ctx context.Context) result { p, err := sec.probe(ctx, t.c.reads, v); return result{p, err} })
		if t.view.probe, t.view.probeErr = r.v, r.err; !ok {
			t.view.probeErr = errProbeExpired
		}
	} else {
		t.view.probe, t.view.probeErr = nil, nil // a section's Probe is its own
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
	var step txStep
	if moved := t.premise(sec.Premise, row, meta); len(moved) > 0 {
		var ok bool
		if sec.Moved != nil {
			step, ok = sec.Moved(v, t.expect, moved)
		}
		if !ok {
			s := refused(causePremise)
			s.Err = fmt.Errorf("%w: %s", errPremiseMoved, strings.Join(moved, ", "))
			return txStep{}, &s
		}
		if step.writes() || step.Done || step.Last { // nothing lands, nor ends the effect landed, on a row that moved
			s := settlement{Outcome: settledFailed, Cause: causeStep}
			return txStep{}, &s
		}
	} else {
		step = sec.Decide(v)
	}
	var s settlement
	switch {
	case step.Refuse != "":
		s = settlement{Outcome: settledRefused, Cause: step.Refuse, Err: step.Err}
	case step.Fail != "" && len(step.Write) == 0 && step.Class.Len() == 0 && len(step.Terminal) == 0:
		s = settlement{Outcome: settledFailed, Cause: step.Fail, Err: step.Err}
	case len(step.Terminal) > 0 && sec.Premise != premiseClose, sec.Premise == premiseClose && len(step.Write) > 0:
		s = settlement{Outcome: settledFailed, Cause: causeStep}
	case sec.Premise == premiseClose && session.IsKillPendingInfo(row, t.view.Now):
		// Not subsumed by the facts: a row the pass already read under an
		// honored kill fence has equal facts, and the fence still refuses.
		s = refused(causePremise)
	case len(step.Write) == 0 && step.Class.Len() == 0 && len(step.Terminal) == 0:
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

// premise reports what keeps row, persisted as meta, from being the
// expected row (the pass's, then the last landed write's) under rule: none
// when it is; otherwise the moved premise keys by name, "closed",
// "session_name", or the basis, so a refusal tells a conflict from a key
// that moves too often to belong in the premise.
func (t *tx) premise(rule premiseRule, row session.Info, meta map[string]string) (moved []string) {
	switch {
	case row.Closed:
		return []string{"closed"}
	case t.name != "" && strings.TrimSpace(row.SessionName) != t.name:
		return []string{"session_name"}
	}
	if t.lease != nil && !t.lease.HoldsMeta(meta) {
		return []string{"runtime lease"}
	}
	fresh := session.FactsOf(meta)
	switch rule {
	case premiseOwnToken:
		if !t.landed {
			return []string{"no own write"}
		}
		return session.FactsCommit.Moved(fresh, t.facts0)
	case premiseDefault, premiseClose, premiseDrainBasis, premiseBaseline:
		site := session.FactsDefault
		switch {
		case rule == premiseDrainBasis:
			site = session.FactsDrainBasis
		case rule == premiseBaseline:
			site = session.FactsBaseline
		case t.started:
			site = session.FactsAfterStart
		case t.stopped:
			site = session.FactsAfterStop
		}
		moved = site.Moved(t.compared(fresh), t.compared(t.facts0))
		if len(moved) == 0 && rowBasisOf(t.view.It.Key, row) != t.basis {
			moved = []string{"basis"}
		}
		return moved
	}
	return []string{"rule"}
}

// compared is the part of f the premise compares: the assignment identity
// keys only for an effect that reads the row's work (L5).
func (t *tx) compared(f session.Facts) session.Facts {
	if t.needs.Legs&legWork == 0 {
		f = f.Premise()
	}
	return f
}

// rowBasisOf is row's incarnation and token, as the census reads them.
func rowBasisOf(k rowKey, row session.Info) rowBasis {
	r := newCensusRow(k, row)
	return rowBasis{Incarnation: r.Incarnation, InstanceToken: r.InstanceToken}
}

// errPremiseMoved is a premise refusal's error, naming what moved.
var errPremiseMoved = errors.New("the row moved since the pass")

// callWatched runs sec's Call on the lease's watch, so a Call whose lease
// is lost, released or past SafeUntil sees its context end, and hands it the
// controller's Actor carrying the lease (txCaps.by).
func (t *tx) callWatched(ctx context.Context, sec section) (any, error) {
	c := t.c
	c.by = sessionActor(session.ActorController, t.p.World.CityPath)
	if sec.callKind == callStart {
		// Detached before the lease watch, so a lost lease still ends a Start
		// that outlives a preserve-sessions shutdown (outlivesShutdown).
		detached, release := outlivesShutdown(ctx)
		defer release()
		ctx = detached
	}
	if t.lease == nil {
		return sec.call(ctx, c, t.pass)
	}
	watched, cancel, err := t.lease.Watch(ctx, leaseWatchEvery)
	if err != nil {
		return nil, err
	}
	defer cancel()
	// A Call through a leasing helper (a Manager start or stop) runs under
	// this lease instead of finding its own flock busy.
	c.by.Lease = t.lease
	return sec.call(watched, c, t.pass)
}

// ended is the failure of an effect that may not write: its context ended
// (context.Cause tells its deadline from a shutdown's cancel, whatever Err
// reads under a fake clock), or the executor abandoned it first.
func ended(ctx context.Context) settlement {
	switch cause := context.Cause(ctx); {
	case cause == nil:
		return settlement{Outcome: settledFailed, Cause: causeDeadline, Err: errAbandoned}
	case errors.Is(cause, session.ErrRuntimeLeaseExpired):
		return settlement{Outcome: settledFailed, Cause: causeLeaseExpired, Err: cause}
	case errors.Is(cause, session.ErrRuntimeLeaseLost):
		return settlement{Outcome: settledFailed, Cause: causeLeaseLost, Err: cause}
	case !errors.Is(cause, context.DeadlineExceeded):
		return settlement{Outcome: settledFailed, Cause: canceledCause(ctx), Err: cause}
	}
	return settlement{Outcome: settledFailed, Cause: causeDeadline, Err: context.DeadlineExceeded}
}

// The lease's own ends: past its SafeUntil, or taken over.
const (
	causeLeaseExpired = "lease-expired"
	causeLeaseLost    = "lease-lost"
)

func refused(cause string) settlement { return settlement{Outcome: settledRefused, Cause: cause} }
