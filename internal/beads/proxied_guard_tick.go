package beads

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beads/proxyendpoint"
)

// The guard tick is the only thing watching a LONG-LIVED proxied handle between
// its reads.
//
// A one-shot command does not need it: the open happens, a few reads follow, the
// process exits, and admission's evidence cannot go stale inside that window in
// any way a read would not itself surface. A controller store is the opposite —
// it is held for the process lifetime, and three things can change under it
// without any read failing at all:
//
//  1. bd replaces its proxy (an operator `bd dolt stop`, an idle expiry, a
//     crash-and-restart). The record names a new generation; the pooled socket
//     may still be connected to the OLD one.
//  2. The scope's root is moved and recreated at the same path (design U22).
//     root_id is path-derived, so the new proxy has the SAME root identity, and
//     the old pooled socket keeps serving the MOVED database without an error to
//     notice. This is the hazard no read can see.
//  3. Somebody migrates the database — bd's own `bd migrate`, or a newer bd
//     opening it — while gc holds a handle whose linked library pins the pair of
//     cursors admission checked.
//
// Rows 1 and 2 are answered by RE-PINNING, not by demoting (design U22,
// 648-654): a bd proxy restart is ordinary operation, and a store that fell to
// the bd front door for the rest of the process on every restart would make the
// whole lane worthless on a Finite-idle city. When the new generation does not
// admit on the tick that sees it, the handle stands down NON-terminally and a
// later tick's recovery re-pins it (council pr2 E-S1): on row 2 the old socket
// keeps serving the moved database, so "leave it and ask again" was serving the
// wrong database for an interval. "A later tick", not "the next": the recovery
// is one probe session per tick, so the handle reads through the bd front door
// until one of those sessions admits — on the loaded box that made the first
// one indeterminate, that can be many intervals (see checkGeneration for what
// that costs against the read-path re-pin it replaced). Row 3 is the one
// demotion the design asks for (563-565), because a cursor pair that moved is a
// fact about the database that re-pinning cannot change.
//
// # Why the tick does not open the library WHILE THE LEAF IS SERVING
//
// A re-pin needs a fresh library handle against the new generation's port, and
// opening one means the hermetic env window: a process-global environment
// mutation under nativeDoltOpenEnvMu. While the native leaf is still serving the
// tick deliberately does NOT perform it. It re-runs ADMISSION (file reads plus
// one probe session — no library, no fork) and then marks the native handle's
// pool stale, so the next READ reconnects through the injected reopen hook on
// the CALLER's goroutine, which is where every other library open in this
// package already happens. A background goroutine that mutated the process
// environment at an arbitrary moment would be a new class of hazard for every
// concurrent scope open and bd-child env build in the process, to save a re-pin
// a few milliseconds.
//
// There is exactly one state where that argument does not hold, and the tick
// does open the library there: a handle that has ALREADY stood down
// non-terminally. Its native leaf is nil, so every read is on the bd leaf and
// the reopen hook cannot be reached by anything — there is no caller's goroutine
// to defer to, only the choice between a recovery and none. See recoverNative.
//
// # What the tick may spend
//
// Per tick: two 200-byte file reads (the record, and the root identity behind
// Validate), one probe session, and — every proxiedGuardOwnerEvery-th tick — a
// /proc read. It holds NO ProviderOps: a tick never forks bd. While the leaf is
// SERVING, an escalation rung is the read path's to spend, through the reopen
// hook, where a caller is waiting for an answer and the cost is attributable.
// Once it has stood down there is no read on the native leaf, so nothing in
// this handle spends one; see recoverNative.
//
// A RECOVERY tick (recoverNative, for a handle that stood down non-terminally)
// is held to the same session budget, and that is a contract on the installed
// NativeLeafReopener rather than something this file can enforce: cmd/gc's
// re-admits with AdmissionInput.ProbeOnce — one pass, at most ONE probe session,
// no sleeps, no drain wait and no no-greeting ladder, because the next tick is
// the retry (council pr2 D-F5). Only when that admits does the tick also open
// the library, inside the hermetic window, plus the post-open re-read: two
// statements on one pinned connection of the new pool (council pr2 E-S3). Before the cap it could cost nine probe sessions
// and ~6s of sleeps per tick against a silent proxy, and the full 60s drain
// against a refusing one, every interval.

const (
	// proxiedGuardOwnerEvery is how often the socket-owner join runs, counted in
	// ticks. It is the most expensive check and the least likely to fire: at the
	// default 15s interval it runs every 150s, which bounds the exposure to a
	// port stolen by a process gc has no other way to notice.
	proxiedGuardOwnerEvery = 10
)

// proxiedOwner is the tri-state answer of the socket-owner join.
//
// Tri-state, and the third state NEVER decides. The join reads /proc, which is
// Linux-only, unreadable for another user's process, and racy against a proxy
// that is restarting — so "I could not tell" has to be a distinct answer from
// "somebody else holds the port". Collapsing them would demote a healthy city on
// every host where the read is not permitted.
type proxiedOwner int

const (
	// proxiedOwnerUndetermined means the join could not be performed. It never
	// decides anything.
	proxiedOwnerUndetermined proxiedOwner = iota
	// proxiedOwnerMatches means the pinned process holds the pinned port.
	proxiedOwnerMatches
	// proxiedOwnerForeign means some other live process holds it.
	proxiedOwnerForeign
)

// proxiedGuardStep is what one tick concluded, for a test and for a log line.
type proxiedGuardStep int

const (
	// proxiedGuardHeld: the pin still describes the world.
	proxiedGuardHeld proxiedGuardStep = iota
	// proxiedGuardRepinned: the generation moved and the handle was re-pinned to
	// the new one. The store is STILL NATIVE.
	proxiedGuardRepinned
	// proxiedGuardStoodDown: the native leaf was stood down with a verdict.
	proxiedGuardStoodDown
	// proxiedGuardUndecided: a read the decision depends on failed, so the tick
	// decided nothing. The next tick asks again.
	proxiedGuardUndecided
	// proxiedGuardStopped: there is nothing left to guard.
	proxiedGuardStopped
)

func (s proxiedGuardStep) String() string {
	switch s {
	case proxiedGuardHeld:
		return "held"
	case proxiedGuardRepinned:
		return "repinned"
	case proxiedGuardStoodDown:
		return "stood-down"
	case proxiedGuardUndecided:
		return "undecided"
	case proxiedGuardStopped:
		return "stopped"
	default:
		return "unknown"
	}
}

// proxiedGuardOptions are the guard's injected effects. Production leaves every
// field zero and takes the defaults; tests supply a ticker channel, a fake
// process table, a scripted probe and a scripted owner join, so the whole
// decision table is provable with no proxy, no database and no clock anywhere in
// the test binary.
type proxiedGuardOptions struct {
	interval     time.Duration
	ticks        <-chan time.Time
	processTable proxyendpoint.ProcessTable
	probe        func(ctx context.Context, ep proxyendpoint.Endpoint, database string) proxyendpoint.ProbeResult
	cursors      func(ctx context.Context, pin Pin) (proxyendpoint.Cursors, error)
	owner        func(ctx context.Context, pin Pin) proxiedOwner
	ownerEvery   int
	now          func() time.Time
	// onStep observes each tick's conclusion. Tests use it; production leaves it
	// nil.
	onStep func(step proxiedGuardStep)
}

func (o proxiedGuardOptions) withDefaults() proxiedGuardOptions {
	if o.interval <= 0 {
		o.interval = proxiedGuardInterval()
	}
	if o.processTable.Alive == nil {
		o.processTable = proxyendpoint.DefaultProcessTable()
	}
	if o.probe == nil {
		o.probe = proxyendpoint.ProbeEndpoint
	}
	if o.cursors == nil {
		o.cursors = proxiedGuardReadCursors
	}
	if o.owner == nil {
		o.owner = proxiedGuardSocketOwner
	}
	if o.ownerEvery == 0 {
		o.ownerEvery = proxiedGuardOwnerEvery
	}
	if o.now == nil {
		o.now = time.Now
	}
	return o
}

// proxiedGuard is one long-lived store's guard.
type proxiedGuard struct {
	store  *ProxiedStore
	opts   proxiedGuardOptions
	cancel context.CancelFunc
	// done is closed when run returns: after stop, the guard has made its
	// last tick and holds nothing.
	done chan struct{}
}

// stop cancels the guard and joins its goroutine.
func (g *proxiedGuard) stop() {
	g.cancel()
	<-g.done
}

// StartGuard starts the guard tick for a LONG-LIVED proxied store.
//
// It is the caller's decision because only the caller knows the shape: the
// controller's city and rig stores are held for the process lifetime and get a
// guard, and a one-shot CLI open does not — a ticker on a store that lives for
// 40ms is a goroutine and a probe session bought for nothing.
//
// It is idempotent per store: a second call stops the first guard before
// installing its own, so a re-open path cannot accumulate tickers.
func (s *ProxiedStore) StartGuard() {
	s.startGuard(proxiedGuardOptions{})
}

func (s *ProxiedStore) startGuard(opts proxiedGuardOptions) *proxiedGuard {
	ctx, cancel := context.WithCancel(context.Background())
	guard := &proxiedGuard{store: s, opts: opts.withDefaults(), cancel: cancel, done: make(chan struct{})}

	s.mu.Lock()
	previous := s.guard
	s.guard = guard
	s.mu.Unlock()
	// Outside the lock: a previous guard's goroutine takes mu itself, so joining
	// it while holding mu would deadlock.
	if previous != nil {
		previous.stop()
	}

	go guard.run(ctx)
	return guard
}

// run is the ticker loop. It owns no state beyond the tick counter: everything
// it decides is decided against the store's current pin, read fresh each tick,
// because a re-pin between ticks moves the very thing the next tick compares.
func (g *proxiedGuard) run(ctx context.Context) {
	defer close(g.done)

	ticks := g.opts.ticks
	if ticks == nil {
		ticker := time.NewTicker(g.opts.interval)
		defer ticker.Stop()
		ticks = ticker.C
	}
	for n := 1; ; n++ {
		select {
		case <-ctx.Done():
			return
		case _, open := <-ticks:
			if !open {
				// A closed injected channel is a test's stop signal, and in
				// production a ticker channel is never closed.
				return
			}
		}
		if g.tick(ctx, n) == proxiedGuardStopped {
			return
		}
	}
}

// tick runs the four steps, in the design's order, and stops at the first one
// that decides.
//
// The order is the order the evidence gets more expensive AND more specific: the
// record is two file reads and answers "is this still the same proxy", the
// cursors cost a session and answer "is this still the same schema", and the
// owner join costs a /proc walk and answers "is this still the same process on
// that port" — a question the first two cannot ask, because a stolen port still
// serves a record and a database.
func (g *proxiedGuard) tick(ctx context.Context, n int) proxiedGuardStep {
	step := g.tickSteps(ctx, n)
	if g.opts.onStep != nil {
		g.opts.onStep(step)
	}
	return step
}

func (g *proxiedGuard) tickSteps(ctx context.Context, n int) proxiedGuardStep {
	if err := ctx.Err(); err != nil {
		return proxiedGuardStopped
	}
	pin, native, terminal := g.store.guardState()
	if native == nil {
		if terminal {
			// A terminal stand-down is a fact a tick cannot move, and the handle
			// will never serve natively again. Nothing left to guard.
			return proxiedGuardStopped
		}
		// A non-terminal stand-down: the endpoint was in MOTION (a proxy
		// restart, a drain, a spent budget), which is not a fact about the
		// database. This arm used to return Held on the theory that "the re-pin
		// belongs to the read path (the reopen hook)". That theory was wrong,
		// and it is council A-F1: the reopen hook lives on the NATIVE handle
		// and is reached only from a read the native leaf serves, so with the
		// leaf nil every read goes to bd and the hook is unreachable for ever.
		// The tick is the only place left, so the tick does it.
		return g.recoverNative(ctx)
	}
	if !pin.Admitted() {
		return proxiedGuardStopped
	}

	// Step 1 — the record. On a generation change this RE-PINS.
	switch step := g.checkGeneration(ctx, pin, native); step {
	case proxiedGuardHeld:
		// Fall through to the cursors.
	case proxiedGuardRepinned:
		// The re-admission inside the re-pin already gated the new generation's
		// cursors, so a second session this tick would buy the same answer.
		return step
	default:
		return step
	}

	// Step 2 — the cursors. The one demotion the design asks a tick to make.
	if step := g.checkCursors(ctx, pin); step != proxiedGuardHeld {
		return step
	}

	// Step 3 — the events-journal slot. Deliberately a no-op: a grep for
	// BD_EVENTS_JOURNAL / EventsJournal over non-test internal/beads and cmd/gc
	// returns nothing, so there is no journal gate in gc for a tick to keep
	// consistent. The slot is named here rather than dropped so the next reader
	// finds the answer instead of the question.

	// Step 4 — the socket-owner join, every ownerEvery-th tick.
	if g.opts.ownerEvery > 0 && n%g.opts.ownerEvery == 0 {
		return g.checkOwner(ctx, pin)
	}
	return proxiedGuardHeld
}

// checkGeneration compares bd's record against the pinned generation.
//
// An UNREADABLE record is not a change, for the same reason the mutation bracket
// treats it that way: bd removes the record on an orderly stop and rewrites it
// on start, so an absent record is as likely to be that window as a real move,
// and standing a handle down on it would demote a healthy store for a file that
// reappears a millisecond later. A record that is present and FAILS VALIDATION
// is different — a foreign root_id or a pre-schema-2 document means the thing at
// that path is not the proxy this handle was admitted against, and continuing to
// serve would be serving somebody else's database.
func (g *proxiedGuard) checkGeneration(ctx context.Context, pin Pin, native *NativeDoltStore) proxiedGuardStep {
	record, err := proxyendpoint.Read(pin.Root())
	if err != nil {
		return proxiedGuardUndecided
	}
	if err := proxyendpoint.Validate(record, pin.Root()); err != nil {
		switch {
		case errors.Is(err, proxyendpoint.ErrLegacyProxy):
			g.store.standDown(NewProxiedVerdictError(ProxiedVerdictLegacySchema,
				"the proxy record at the pinned root no longer carries a birth token", err))
			return proxiedGuardStoodDown
		case errors.Is(err, proxyendpoint.ErrNotOurs):
			g.store.standDown(NewProxiedVerdictError(ProxiedVerdictNotOurs,
				"the proxy record at the pinned root no longer validates for this root", err))
			return proxiedGuardStoodDown
		default:
			return proxiedGuardUndecided
		}
	}
	if proxyendpoint.NewPoolKey(record, pin.Database()).SameGeneration(pin.PoolKey()) {
		return proxiedGuardHeld
	}

	// The generation moved. Discard the memoized pass first: its whole contract
	// is that the answer cannot have changed, and leaving a contradiction in it
	// for the next open to read would be worse than not having it.
	ForgetProxiedPin(pin.ScopeRoot(), pin.Database())
	fresh, err := Admit(ctx, AdmissionInput{
		ScopeRoot:    pin.ScopeRoot(),
		Database:     pin.Database(),
		ProcessTable: g.opts.processTable,
		Probe:        g.opts.probe,
		// No Ops: a tick never forks bd. If the new generation needs a provider
		// verb, the read path spends it through the reopen hook, where a caller
		// is waiting and the cost is attributable.
		Ops:       nil,
		LongLived: true,
		Now:       g.opts.now,
		// A tick that re-admitted out of the memo it populated would be
		// asserting that nothing changed by reading its own answer back.
		SkipMemo: true,
		// One probe session, no waiting — the per-tick budget the header
		// states (council pr2 D-F5, re-pin arm). This re-admission was the
		// long-lived shape with the REAL clock, so a new generation that
		// refused ran the 60s drain on the tick goroutine, re-probing every 2s,
		// and a silent one walked the three-attempt ladder with 1s sleeps.
		// What a refusal on this one session does to the HANDLE is decided
		// below, and it is not "wait for the next tick".
		ProbeOnce: true,
	})
	if err != nil {
		if verdict, ok := ProxiedVerdictOf(err); ok && verdict.Terminal() {
			// A fact about the database, the record or the policy. Re-pinning
			// could only re-learn it.
			g.store.standDown(verdict)
			return proxiedGuardStoodDown
		}
		// Everything else stands the native leaf down NON-terminally, and a
		// later tick's recovery (recoverNative) re-admits it. This arm used to
		// leave the pin and the leaf alone and report Undecided, and that was
		// wrong on the one shape this file exists for (council pr2 E-S1).
		//
		// The record has just said the pinned generation was REPLACED. On a
		// proxy restart (row 1 of the header) the old pool's socket is dead, so
		// reads fail on their own and nothing is served from it. On a root move
		// (row 2) the OLD proxy is still alive, still serving the MOVED
		// database on the socket this pool holds, while the new one is not yet
		// accepting — bd writes its record before it binds. Every read on the
		// old socket succeeds, so no read ever reaches the reopen hook: the
		// Undecided arm kept serving the moved database until a tick found the
		// new generation serving, which is one full guard interval at best and
		// has no upper bound at all (GC_BEADS_PROXIED_GUARD_INTERVAL has no
		// ceiling). The old comment's escape hatch — "a reader that needs the
		// new generation sooner re-pins through the reopen hook" — cannot fire
		// on that shape, because the hook is reached only through a failed or
		// stale-marked read.
		//
		// Standing down is what A-F1 forbade, and A-F1 was right at the time:
		// with no recovery path a non-terminal stand-down was permanent. That
		// is no longer true: a non-terminally stood-down long-lived handle is
		// recovered by a later tick through the NativeLeafReopener (see
		// recoverNative). What this arm buys is that no read is served from a
		// generation the record says is gone.
		//
		// What it costs is NOT bounded by one interval, and an earlier version
		// of this comment said it was (round3 review). Every read goes to the bd
		// front door — one fork per read — until some tick's recovery admits,
		// and that recovery is ONE probe session with no provider ops. The
		// session that just failed to admit here is the same kind of session,
		// on the same box: when it failed because the box is loaded (the
		// budget_exhausted this arm usually sees), the next ones tend to fail
		// too, and the handle stays on bd for as long as the load lasts.
		//
		// On row 1 that is a regression against what it replaced. Before this
		// arm the old pool's socket was dead, so the first read failed, reached
		// the reopen hook and re-ran the FULL admission on the reader's
		// goroutine — every pass, the no-greeting ladder, the provider ping —
		// within the read's budget, and re-pinned. Standing the leaf down makes
		// that hook unreachable. The trade is deliberate (a read path that
		// cannot see row 2 must not be the only guard against it), and it
		// fails safe: non-terminal, and never a read from the wrong generation.
		//
		// The typed verdict is kept when there is one (draining,
		// budget_exhausted, backend_unreachable, proxy_gone), with the
		// generation move in its detail, so doctor says why. An UNTYPED error —
		// admission names every outcome it reaches, so it is a bug — stands down
		// as proxy_gone for the same reason: the pinned generation is gone
		// whatever the re-admission could not say.
		g.store.standDown(repinRefusedVerdict(pin, record, err))
		return proxiedGuardStoodDown
	}

	// The re-pin, in two moves that are deliberately NOT a library open:
	// adopt the freshly admitted pin, then mark the pool stale so the next read
	// reconnects through the reopen hook on the caller's goroutine. Until that
	// read happens no read is served at all from the old generation, which is the
	// property the root-move row asserts.
	g.store.adoptPin(fresh)
	if !native.markPoolStale() {
		// No reopen hook, or the handle closed underneath us: there is no way to
		// re-point this pool, so the honest move is a NON-terminal stand-down —
		// the next open re-admits and gets a native handle again.
		g.store.standDown(NewNonTerminalProxiedVerdictError(ProxiedVerdictProxyGone,
			"the proxy generation moved and this handle cannot re-point its pool", nil))
		return proxiedGuardStoodDown
	}
	return proxiedGuardRepinned
}

// repinRefusedVerdict is the NON-terminal stand-down for a generation change
// whose fresh admission did not admit on this tick (council pr2 E-S1). It keeps
// the admission's own verdict when it has one and says, in the detail, which
// generation replaced which.
func repinRefusedVerdict(pin Pin, record proxyendpoint.Record, err error) *ProxiedVerdictError {
	moved := fmt.Sprintf("the proxy generation moved (%s -> %s) and the new generation did not admit on this tick; "+
		"standing down until a later tick's recovery admits it rather than serving the replaced generation",
		pin.Generation(), proxyendpoint.NewPoolKey(record, pin.Database()).Generation())
	if verdict, ok := ProxiedVerdictOf(err); ok {
		detail := moved
		if verdict.Detail != "" {
			detail += ": " + verdict.Detail
		}
		return NewNonTerminalProxiedVerdictError(verdict.Verdict, detail, err)
	}
	return NewNonTerminalProxiedVerdictError(ProxiedVerdictProxyGone, moved, err)
}

// checkCursors re-reads the database's two migration cursors and compares them
// to the pair this handle was admitted against.
//
// A FAILED read decides nothing. The session's own timeout, a proxy mid-restart
// and a loaded box all look the same from here, and none of them is evidence
// that the schema moved; treating one as drift would demote a healthy controller
// store for a slow tick.
func (g *proxiedGuard) checkCursors(ctx context.Context, pin Pin) proxiedGuardStep {
	observed, err := g.opts.cursors(ctx, pin)
	if err != nil {
		return proxiedGuardUndecided
	}
	pinned := pin.Cursors()
	if observed == pinned {
		return proxiedGuardHeld
	}
	// Drop the memoized pass before standing down. The memo's stamp fingerprints
	// proxy.pid and the sidecar, and a migration writes neither — so without
	// this, every other open in this process keeps reading an answer this tick
	// has just contradicted, for the rest of the TTL (council A-F3).
	ForgetProxiedPin(pin.ScopeRoot(), pin.Database())
	lane, dir := cursorDriftAgainst(pinned, observed)
	g.store.standDown(NewSchemaSkewVerdictError(lane, dir, fmt.Sprintf(
		"the pinned database moved from %s to %s while this handle was open", pinned, observed)))
	return proxiedGuardStoodDown
}

// cursorDriftAgainst names which lane moved and in which direction the DATABASE
// now sits relative to the pinned pair.
//
// It compares against the PIN rather than calling CursorsMatchPinned, because
// the pin is this handle's contract: admission proved the pair equal to the
// linked library's constants at open, and comparing against the pin says "the
// database moved under this handle" where comparing against the constants would
// also fire if the constants were the thing that moved — which cannot happen
// inside one process. The main lane is reported first when both moved, matching
// CursorsMatchPinned, because that is the lane bd's own migration gate consults.
func cursorDriftAgainst(pinned, observed proxyendpoint.Cursors) (lane, dir string) {
	switch {
	case observed.Main > pinned.Main:
		return ProxiedSkewLaneMain, ProxiedSkewDirAhead
	case observed.Main < pinned.Main:
		return ProxiedSkewLaneMain, ProxiedSkewDirBehind
	case observed.Ignored > pinned.Ignored:
		return ProxiedSkewLaneIgnored, ProxiedSkewDirAhead
	case observed.Ignored < pinned.Ignored:
		return ProxiedSkewLaneIgnored, ProxiedSkewDirBehind
	default:
		return "", ""
	}
}

// recoverNative re-opens the native read leaf of a handle that stood down
// NON-terminally, and is the only recovery such a handle has (council A-F1).
//
// # Why this one opens the library, when the rest of the file refuses to
//
// The refusal above is about a handle that is still SERVING: a re-pin there
// only needs the pool re-pointed, the reader's own goroutine is the natural
// place to do it, and a background library open would put a process-global
// environment mutation on a timer for no gain. None of that applies here.
// There is no reader to hand the work to — every read is on the bd leaf, and
// the native reopen hook is unreachable — so the choice is not "which
// goroutine" but "recovery or none". The window is still the hermetic one
// under nativeDoltOpenEnvMu, so a concurrent scope open or bd-child env build
// blocks on it rather than reading a transient value.
//
// It spends NO provider verb. The reopener admission runs with nil Ops, so the
// tick's "never forks bd" invariant survives: a recovery that needs bd to make
// its proxy healthy comes back non-terminal and the tick stays Undecided. The
// rung is NOT "spent later by the read path's reopen hook", which this comment
// used to say (round3 review): the hook is reached only from a read the native
// leaf serves, and this handle's leaf is nil. Nothing in this handle spends it;
// its reads go through the bd front door, bd's own client of its proxy, and a
// provider verb is spent only by an open that runs the full ladder — another
// command's. And it spends at most ONE probe session per tick: see "What the
// tick may spend" above, and checkGeneration for how long that can keep the
// handle on bd.
func (g *proxiedGuard) recoverNative(ctx context.Context) proxiedGuardStep {
	reopen := g.store.nativeReopener()
	if reopen == nil {
		// No recovery path was installed — a store assembled by hand, or a test
		// double. The handle keeps serving from the bd leaf, which is what a
		// demoted store is.
		return proxiedGuardHeld
	}
	native, pin, err := reopen(ctx)
	if err != nil {
		// A recovery is a library open like any other, so it can meet
		// head_moved, and it is the one open no caller is waiting on: nothing
		// but this line reports it (council pr2 E-S2).
		logProxiedHeadMoved(nil, g.store.scopeRootForPin(), ProxiedIncidentSiteGuardRecovery, err)
		verdict, ok := ProxiedVerdictOf(err)
		if ok && verdict.Terminal() {
			g.store.standDown(verdict)
			return proxiedGuardStoodDown
		}
		if ok {
			// Still demoted, for a reason newer than the one that demoted it.
			// Doctor reads the store's verdict, and "proxy_gone" for a handle
			// whose recoveries keep failing on head_moved is a stale account.
			g.store.noteDemotedVerdict(verdict)
		}
		return proxiedGuardUndecided
	}
	if native == nil || !pin.Admitted() {
		// A reopener that returned neither an error nor a usable leaf is a bug,
		// and a tick does not act on one.
		return proxiedGuardUndecided
	}
	// H10 again, with both handles in hand. A replacement leaf that minted a
	// different prefix from the bd leaf is a split whose halves disagree about
	// which beads are foreign, and it is worse than staying demoted.
	if verdict := ProxiedPrefixAgreement(pin.Database(), native, g.store.writeLeaf()); verdict != nil {
		closeNativeLeafQuietly(native)
		if typed, ok := ProxiedVerdictOf(verdict); ok {
			g.store.standDown(typed)
			return proxiedGuardStoodDown
		}
		return proxiedGuardUndecided
	}
	if !g.store.repin(native, pin) {
		// A terminal stand-down landed between the reopen and the swap, or the
		// store closed underneath us — CloseStore latches that before it stops
		// this guard, because the stop cannot interrupt a recovery that is past
		// its last ctx-sensitive step. The fresh leaf belongs to nobody.
		closeNativeLeafQuietly(native)
		return proxiedGuardStopped
	}
	return proxiedGuardRepinned
}

// closeNativeLeafQuietly releases a leaf the tick is abandoning, off the tick's
// own path: CloseStore can block on a wedged connection.
func closeNativeLeafQuietly(native *NativeDoltStore) {
	go func() { _ = native.CloseStore() }()
}

// checkOwner asks whether the pinned process still holds the pinned port.
//
// It is the only check that can see a port STOLEN by a process that is not bd's
// proxy at all: such a port still has a record beside it (bd's, stale) and may
// well serve a database (somebody else's), so neither the record check nor the
// cursor check can tell. Undetermined never decides.
func (g *proxiedGuard) checkOwner(ctx context.Context, pin Pin) proxiedGuardStep {
	switch g.opts.owner(ctx, pin) {
	case proxiedOwnerForeign:
		g.store.standDown(NewProxiedVerdictError(ProxiedVerdictNotOurs, fmt.Sprintf(
			"port %d is no longer held by the pinned proxy process (generation %s)",
			pin.Port(), pin.Generation()), nil))
		return proxiedGuardStoodDown
	case proxiedOwnerMatches, proxiedOwnerUndetermined:
		return proxiedGuardHeld
	default:
		return proxiedGuardHeld
	}
}

// proxiedGuardReadCursors is the production cursor re-read: ONE connection to
// the pinned data port, both cursors over it, closed before this function
// returns.
//
// It uses the probe's own single-connection session rather than the library
// handle's pool, and that is a mechanism deviation with a reason: beadslib.Storage
// exposes no raw query, so the linked library cannot be asked for a cursor at all
// — while the probe reads exactly the two SELECT COALESCE(MAX(version),0) rows
// admission gated on, with the same tolerance for a cursor table that does not
// exist yet, over a handle that retains no idle connection for bd's idle watcher
// to count. The cost is what the design asked for (one connection per tick) and
// the numbers are the same numbers.
//
// The pair it returns is the EFFECTIVE pair — the raw main cursor and the
// ignored cursor after the library's reality floor — for the same reason
// admission gates on the effective pair (council A-F2): the raw ignored number
// is not what the linked library acts on, and a sentinel that disappears under a
// live handle moves the library's answer without moving the number on disk.
// Admission proved effective == raw == pinned at open, so comparing an effective
// re-read against the pin's raw pair is comparing like with like.
func proxiedGuardReadCursors(ctx context.Context, pin Pin) (proxyendpoint.Cursors, error) {
	result := proxyendpoint.Probe(ctx, proxyendpoint.DefaultProbeIO(pin.Port(), pin.Database()))
	if result.Outcome != proxyendpoint.ProbeServed {
		if result.Err != nil {
			return proxyendpoint.Cursors{}, fmt.Errorf("guard cursor read (%s): %w", result.Outcome, result.Err)
		}
		return proxyendpoint.Cursors{}, fmt.Errorf("guard cursor read: probe outcome %s", result.Outcome)
	}
	if !result.Reality.Checked() {
		// The same refusal the admission gate makes (council pr2 D-F11), as an
		// undecided tick rather than a demotion: an unevaluated reality is not
		// evidence the schema moved, and not evidence it did not.
		return proxyendpoint.Cursors{}, errors.New("guard cursor read: the session never evaluated the ignored lane's cursor reality")
	}
	return proxyendpoint.Cursors{
		Main:    result.Cursors.Main,
		Ignored: result.Reality.EffectiveIgnored(result.Cursors.Ignored),
	}, nil
}

// proxiedGuardSocketOwner joins the pinned port's listening socket to the pinned
// process through /proc.
//
// The join is deliberately inverted relative to the obvious implementation: it
// collects the listening socket inodes for the port and then looks for one of
// them among the PINNED process's own file descriptors, instead of walking every
// process in /proc to find the holder. The question is "does the process we are
// pinned to still own that port", so one /proc/<pid>/fd read answers it, and a
// tick must not pay a whole-process-table walk for that.
//
// Every read that fails yields Undetermined, and that is load-bearing: /proc is
// Linux-only, another user's fd directory is unreadable, and a proxy that is
// restarting has no listener for a moment. None of those is somebody stealing a
// port.
func proxiedGuardSocketOwner(_ context.Context, pin Pin) proxiedOwner {
	if pin.Port() <= 0 || pin.PoolKey().PID <= 0 {
		return proxiedOwnerUndetermined
	}
	inodes, read := proxiedListeningSocketInodes(pin.Port())
	if !read || len(inodes) == 0 {
		// Nothing readable, or nothing listening. "Nothing listening" is the
		// probe's business, not the owner join's.
		return proxiedOwnerUndetermined
	}
	fdDir := filepath.Join("/proc", strconv.Itoa(pin.PoolKey().PID), "fd")
	entries, err := os.ReadDir(fdDir)
	if err != nil {
		return proxiedOwnerUndetermined
	}
	for _, entry := range entries {
		target, err := os.Readlink(filepath.Join(fdDir, entry.Name()))
		if err != nil {
			continue
		}
		if !strings.HasPrefix(target, "socket:[") || !strings.HasSuffix(target, "]") {
			continue
		}
		inode := strings.TrimSuffix(strings.TrimPrefix(target, "socket:["), "]")
		if _, ok := inodes[inode]; ok {
			return proxiedOwnerMatches
		}
	}
	// The port has a listener, the pinned process's descriptors are readable, and
	// none of them is it.
	return proxiedOwnerForeign
}

// proxiedListeningSocketInodes returns the socket inodes listening on port,
// and whether any of the kernel's tables could be read at all.
func proxiedListeningSocketInodes(port int) (map[string]struct{}, bool) {
	const (
		tcpStateListen = "0A"
		// The inode column in /proc/net/tcp{,6}.
		inodeField = 9
	)
	inodes := map[string]struct{}{}
	read := false
	for _, path := range []string{"/proc/net/tcp", "/proc/net/tcp6"} {
		file, err := os.Open(path)
		if err != nil {
			continue
		}
		read = true
		scanner := bufio.NewScanner(file)
		for scanner.Scan() {
			fields := strings.Fields(scanner.Text())
			if len(fields) <= inodeField || fields[3] != tcpStateListen {
				continue
			}
			_, portHex, ok := strings.Cut(fields[1], ":")
			if !ok {
				continue
			}
			listening, err := strconv.ParseUint(portHex, 16, 16)
			if err != nil || int(listening) != port {
				continue
			}
			inodes[fields[inodeField]] = struct{}{}
		}
		_ = file.Close()
	}
	return inodes, read
}

// ---------------------------------------------------------------------------
// The store-side seams the guard needs. They live here rather than in
// proxied_store.go so the guard's blast radius is one file.
// ---------------------------------------------------------------------------

// guardState reads the three facts a tick decides from, atomically: the current
// pin, the native leaf (nil when stood down) and whether the stand-down was
// terminal. Reading them one at a time would let a re-pin land between two of
// them and produce a decision about a state that never existed.
func (s *ProxiedStore) guardState() (Pin, *NativeDoltStore, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.pin, s.native, s.terminal
}

// noteDemotedVerdict replaces the recorded reason of a NON-terminally demoted
// handle with a newer non-terminal one, so the diagnostic says why the handle is
// still on bd now rather than why it first left. It never touches a serving
// handle and never overwrites a terminal latch or records a terminal verdict:
// those go through standDown.
func (s *ProxiedStore) noteDemotedVerdict(verdict *ProxiedVerdictError) {
	if verdict == nil || verdict.Terminal() {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.native != nil || s.terminal {
		return
	}
	s.verdict = verdict
}

// adoptPin installs a freshly admitted pin on a store whose native leaf is still
// serving.
//
// It is the re-pin's first half and it is not repin(): there is no new
// *NativeDoltStore to install, because the tick does not open the library. The
// existing handle keeps serving — for exactly as long as it takes the next read
// to notice the stale mark and reconnect — while the pin, and therefore the
// generation the diagnostic reports and the mutation bracket compares against,
// is the new one.
//
// It refuses on a terminal stand-down and on an unadmitted pin, so the two
// invariants the wrapper is built on (demotion is one-way for a fact; only Admit
// mints a pin) hold here too.
func (s *ProxiedStore) adoptPin(pin Pin) bool {
	if !pin.Admitted() {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.terminal || s.closed || s.native == nil {
		return false
	}
	s.pin = pin
	s.root = pin.Root()
	s.database = pin.Database()
	s.verdict = nil
	return true
}
