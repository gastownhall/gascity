package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/reconcilekey"
	"github.com/gastownhall/gascity/internal/runtime"
)

// on_death off the tick. While the inventory lane runs, it owns pool death
// detection and the tick skips reconcilePoolDeaths:
//
//   - Detection runs on every lane pass, over the handler map published at
//     that moment and the names earlier passes saw listed as handlers, each
//     kept with the hook it had then (a reload drops dead instances from the
//     map; the tick checked deaths first). A tracked name is dead when
//     this pass proves it gone by the observation cache's rule
//     (InventoryPass.concludesAbsent): a complete, attested listing on the
//     backend that last listed it, or, once a provider swap removed that
//     backend, on every backend. A partial, failed or unattested listing
//     concludes nothing and keeps the name's last sighting. The first pass
//     has no sightings, so it draws no edges (OQ-1, as legacy).
//   - One worker runs the hooks, FIFO and serial as the tick ran them, each
//     after a fresh exact-name re-check, single-flight and bounded like the
//     lane's listing: a name that is present again is skipped; a re-check
//     error retries on the next pass and, on the second error, fires on the
//     listing that saw the name die unless the pass that requeued it listed
//     the name again.
//   - The ordering the tick gave for free (hook before that tick's restart
//     of the name) is kept by the start interlock: a name whose hook is
//     queued, waiting on a re-check or running is held in onDeathGate, and
//     executePlannedStartsTraced defers its start before any write. The
//     worker pokes the reconciler when it releases a name.

// poolDeathHookRunner runs one on_death command; production is shellRunHook.
type poolDeathHookRunner func(command, dir string, env map[string]string) (string, error)

const (
	onDeathSafeTickTrigger = "on-death-worker"
	// onDeathRecheckAttempts is how many erroring re-checks, each on its own
	// lane pass, an edge takes before it fires on its original observation.
	onDeathRecheckAttempts = 2
)

// on_death execution outcomes, for stderr and the trace.
const (
	onDeathOutcomeFired          = "fired"
	onDeathOutcomeHookFailed     = "hook_failed"
	onDeathOutcomeSkippedPresent = "skipped_present"
	onDeathOutcomeRecheckRetry   = "recheck_retry"
)

// poolDeathSighting is where and as what a handler name was last listed,
// with the hook the handler map held for it then and whether that backend's
// listing could ever prove the name gone.
type poolDeathSighting struct {
	backend     string
	incarnation string
	info        poolDeathInfo
	attested    bool
}

// deathEdge is one detected death of a handler name.
type deathEdge struct {
	name string
	// incarnation is the provider-native id the name was last listed with;
	// "" when its backend reports none.
	incarnation string
	info        poolDeathInfo
	// seq is the lane pass that detected the death.
	seq uint64
	// recheckErrors counts the erroring re-checks so far.
	recheckErrors int
}

// detectPoolDeathEdges applies the on_death rule to one lane pass. prev holds
// the handler names earlier passes listed and that no pass has proven gone
// since, each with the hook it had when last listed; it returns the deaths
// this pass proves, in name order, and the next prev.
//
// A dead name fires with the current map's hook while it is still a handler.
// One the map no longer holds fires with its stored hook: a reload rebuilds
// the map from the instances running at that moment, so an unlimited pool
// instance that died just before the reload is gone from it, and the tick,
// which checked deaths before reloading, fired for it. A name that is no
// longer a handler stops being tracked once it is listed again, or at once
// when its backend's listing is unattested and so could never prove it gone
// (otherwise every reload's departed instances would accumulate there).
func detectPoolDeathEdges(prev map[string]poolDeathSighting, pass InventoryPass, attrs map[string]InventoryAttrs, handlers map[string]poolDeathInfo) ([]deathEdge, map[string]poolDeathSighting) {
	if len(handlers) == 0 && len(prev) == 0 {
		return nil, prev
	}
	names := make([]string, 0, len(handlers)+len(prev))
	for name := range handlers {
		names = append(names, name)
	}
	for name := range prev {
		if _, ok := handlers[name]; !ok {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	listedOn := pass.listedOn()
	attested := make(map[string]bool, len(pass.Backends))
	for _, b := range pass.Backends {
		attested[b.Label] = b.Attested
	}
	next := make(map[string]poolDeathSighting)
	var edges []deathEdge
	for _, name := range names {
		cur, handler := handlers[name]
		if label, ok := listedOn[name]; ok {
			if handler {
				next[name] = poolDeathSighting{backend: label, incarnation: attrs[name].Incarnation, info: cur, attested: attested[label]}
			}
			continue
		}
		seen, ok := prev[name]
		switch {
		case !ok:
		case pass.concludesAbsent(seen.backend):
			info := seen.info
			if handler {
				info = cur
			}
			edges = append(edges, deathEdge{name: name, incarnation: seen.incarnation, info: info, seq: pass.Seq})
		case handler || seen.attested:
			next[name] = seen
		}
	}
	return edges, next
}

// onDeathGate is the on_death worker's queue, and the set of names the start
// interlock holds: every name with a hook queued, waiting on a re-check, or
// running. A nil gate holds nothing.
type onDeathGate struct {
	mu      sync.Mutex
	queue   []deathEdge
	waiting []deathEdge
	running *deathEdge
	held    map[string]int
	// wakeCh signals the worker; buffered 1 so a burst is one wake.
	wakeCh chan struct{}
}

func newOnDeathGate() *onDeathGate {
	return &onDeathGate{held: make(map[string]int), wakeCh: make(chan struct{}, 1)}
}

// Pending reports whether name has an on_death hook queued, waiting on a
// re-check, or running. Its start must wait.
func (g *onDeathGate) Pending(name string) bool {
	if g == nil {
		return false
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.held[name] > 0
}

// enqueue queues edges behind every earlier one. An edge for a death already
// queued or waiting, (name, incarnation), is dropped: the queued hook runs
// after both. So is one for the death a running hook is handling, when the
// incarnation identifies it.
func (g *onDeathGate) enqueue(edges []deathEdge) {
	if len(edges) == 0 {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, e := range edges {
		if g.duplicate(e) {
			continue
		}
		g.queue = append(g.queue, e)
		g.held[e.name]++
	}
	g.signal()
}

func (g *onDeathGate) duplicate(e deathEdge) bool {
	same := func(o deathEdge) bool { return o.name == e.name && o.incarnation == e.incarnation }
	if slices.ContainsFunc(g.queue, same) || slices.ContainsFunc(g.waiting, same) {
		return true
	}
	return g.running != nil && e.incarnation != "" && same(*g.running)
}

func (g *onDeathGate) signal() {
	select {
	case g.wakeCh <- struct{}{}:
	default:
	}
}

// take starts the oldest queued edge. Its name stays held until finish or
// retry.
func (g *onDeathGate) take() (deathEdge, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if len(g.queue) == 0 {
		return deathEdge{}, false
	}
	e := g.queue[0]
	g.queue = g.queue[1:]
	g.running = &e
	return e, true
}

// finish ends the running edge and reports whether its name is released.
func (g *onDeathGate) finish(e deathEdge) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.running = nil
	return g.release(e.name)
}

// retry parks the running edge until the next pass; its name stays held.
func (g *onDeathGate) retry(e deathEdge) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.running = nil
	g.waiting = append(g.waiting, e)
}

// passed requeues the edges waiting on a re-check: one lane pass went by.
func (g *onDeathGate) passed() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if len(g.waiting) == 0 {
		return
	}
	g.queue = append(g.queue, g.waiting...)
	g.waiting = nil
	g.signal()
}

// drop discards every edge not running (shutdown).
func (g *onDeathGate) drop() {
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, e := range append(g.queue, g.waiting...) {
		g.release(e.name)
	}
	g.queue, g.waiting = nil, nil
}

// release drops one hold on name and reports whether it was the last. mu
// must be held.
func (g *onDeathGate) release(name string) bool {
	g.held[name]--
	if g.held[name] > 0 {
		return false
	}
	delete(g.held, name)
	return true
}

// onDeathGate returns the start interlock, nil without an inventory lane.
func (cr *CityRuntime) onDeathGate() *onDeathGate {
	if cr.inventoryLane == nil {
		return nil
	}
	return cr.inventoryLane.onDeath
}

// publishPoolDeathHandlers installs the handler map the tick and the
// inventory lane read. The map is not mutated after.
func (cr *CityRuntime) publishPoolDeathHandlers(handlers map[string]poolDeathInfo) {
	cr.poolDeathHandlers.Store(&handlers)
}

func (cr *CityRuntime) publishedPoolDeathHandlers() map[string]poolDeathInfo {
	if h := cr.poolDeathHandlers.Load(); h != nil {
		return *h
	}
	return nil
}

// poolDeathHook returns the on_death runner.
func (cr *CityRuntime) poolDeathHook() poolDeathHookRunner {
	if cr.poolDeathHookRunner != nil {
		return cr.poolDeathHookRunner
	}
	return shellRunHook
}

// runPoolDeathHook runs one name's on_death hook and reports it on stderr
// the way the tick always has.
func runPoolDeathHook(run poolDeathHookRunner, stderr io.Writer, name string, info poolDeathInfo) error {
	out, err := run(info.Command, info.Dir, info.Env)
	if err != nil {
		fmt.Fprintf(stderr, "on_death %s: %v\n", name, err) //nolint:errcheck // best-effort stderr
	}
	// Surface only the DEFAULT hook's gc-recovery diagnostic for a bd release
	// it could not complete (the loop exits 0 even when a bd write fails, so
	// this is the only signal). A user on_death override carries no marker
	// and is left alone.
	if strings.Contains(out, config.RecoveryHookMarker) {
		fmt.Fprintf(stderr, "on_death %s: %s\n", name, strings.TrimSpace(out)) //nolint:errcheck // best-effort stderr
	}
	return err
}

// detectPoolDeaths is the lane's on_death step, after each published pass:
// requeue edges waiting on a re-check, queue this pass's deaths, then poke
// the reconciler for every name the pass proved gone. The deaths are held
// before the poke, so a tick the poke starts already defers their restarts.
// passMu must be held.
func (cr *CityRuntime) detectPoolDeaths(lane *runtimeInventoryLane, r *inventoryPassReport) {
	pass := r.snapshot.Inventory
	handlers := cr.publishedPoolDeathHandlers()
	lane.noticeUnattestedHandlers(pass, handlers)
	var edges []deathEdge
	edges, lane.poolDeathPrev = detectPoolDeathEdges(lane.poolDeathPrev, pass, r.attrs, handlers)
	lane.onDeath.passed()
	lane.onDeath.enqueue(edges)
	if len(r.gone) > 0 {
		keys := make([]reconcilekey.Key, len(r.gone))
		for i, name := range r.gone {
			keys[i] = reconcilekey.SessionNamed(name)
		}
		legacyEnqueue(cr.pokeCh, nil, keys...)
	}
}

// noticeUnattestedHandlers tells stderr, once per backend per lane, that
// handler names live on a backend whose listing cannot prove absence: their
// deaths are not detected there, so a city on such a backend alone would
// otherwise lose on_death silently.
func (l *runtimeInventoryLane) noticeUnattestedHandlers(pass InventoryPass, handlers map[string]poolDeathInfo) {
	for _, b := range pass.Backends {
		if b.Outcome != OutcomeUnattested || l.unattestedNoticed[b.Label] {
			continue
		}
		for _, name := range b.Names {
			if _, ok := handlers[name]; !ok {
				continue
			}
			l.unattestedNoticed[b.Label] = true
			fmt.Fprintf(l.stderr, "%s: on_death: backend %s cannot prove a session gone (its listing is unattested), so on_death does not fire for its sessions (%s and any others)\n", //nolint:errcheck // best-effort stderr
				l.logPrefix, inventoryLabelForTrace(b.Label), name)
			break
		}
	}
}

// startOnDeathWorker starts the worker goroutine and returns a channel closed
// when it exits. On cancellation it drops every queued edge; a hook already
// running finishes first (hooks are bounded by their own timeout).
func (cr *CityRuntime) startOnDeathWorker(ctx context.Context, lane *runtimeInventoryLane) <-chan struct{} {
	gate := lane.onDeath
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-ctx.Done():
				gate.drop()
				return
			case <-gate.wakeCh:
			}
			for ctx.Err() == nil {
				e, ok := gate.take()
				if !ok {
					break
				}
				retry := false
				cr.safeTick(func() { retry = cr.executeDeathEdge(ctx, lane, &e) }, onDeathSafeTickTrigger)
				if retry {
					gate.retry(e)
				} else if gate.finish(e) {
					legacyEnqueue(cr.pokeCh, nil, reconcilekey.SessionNamed(e.name))
				}
			}
		}
	}()
	return done
}

// executeDeathEdge re-checks one edge's name and runs its hook. It reports
// true when the re-check errored and the edge should wait for the next pass.
func (cr *CityRuntime) executeDeathEdge(ctx context.Context, lane *runtimeInventoryLane, e *deathEdge) (retry bool) {
	started := time.Now()
	_, sp := cr.serviceProviderSnapshot()
	absent, err := lane.revalidateAbsent(ctx, sp, e.name)
	if ctx.Err() != nil {
		return true // shutting down: the worker drops the edge
	}
	if err != nil {
		e.recheckErrors++
		if e.recheckErrors < onDeathRecheckAttempts {
			fmt.Fprintf(cr.stderr, "%s: on_death %s: re-check failed, retrying next pass: %v\n", cr.logPrefix, e.name, err) //nolint:errcheck // best-effort stderr
			cr.traceOnDeath(e, onDeathOutcomeRecheckRetry, err, time.Since(started))
			return true
		}
		// The lane's latest pass, at least as new as the one that requeued
		// the edge, is the last word: a name it showed is present, otherwise
		// the death the lane saw stands.
		_, listed := lane.cache.Snapshot().Inventory.listedOn()[e.name]
		absent = !listed
		if absent {
			fmt.Fprintf(cr.stderr, "%s: on_death %s: re-check failed %d times, running the hook on the listing that saw it stop: %v\n", //nolint:errcheck // best-effort stderr
				cr.logPrefix, e.name, e.recheckErrors, err)
		}
	}
	if !absent {
		cr.traceOnDeath(e, onDeathOutcomeSkippedPresent, nil, time.Since(started))
		return false
	}
	outcome := onDeathOutcomeFired
	hookErr := runPoolDeathHook(cr.poolDeathHook(), cr.stderr, e.name, e.info)
	if hookErr != nil {
		outcome = onDeathOutcomeHookFailed
	}
	cr.traceOnDeath(e, outcome, hookErr, time.Since(started))
	return false
}

var (
	errOnDeathNoProvider     = errors.New("no session provider")
	errOnDeathRecheckBusy    = errors.New("an earlier re-check is still outstanding")
	errOnDeathRecheckTimeout = fmt.Errorf("re-check listing exceeded %s", inventoryListingBound)
)

// revalidateAbsent is the fresh exact-name check before a hook: absent only
// when an error-free listing for the name omits it. A listing that shows the
// name means present, whatever its error. Like the lane's own listing it is
// single-flight and bounded (k8s and ssh list without a deadline): a call
// still outstanding from an earlier re-check, or one that outlives
// inventoryListingBound or ctx, is an error, and the late result is dropped.
func (l *runtimeInventoryLane) revalidateAbsent(ctx context.Context, sp runtime.Provider, name string) (bool, error) {
	if sp == nil {
		return false, errOnDeathNoProvider
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	l.listMu.Lock()
	if l.rechecking {
		l.listMu.Unlock()
		return false, errOnDeathRecheckBusy
	}
	l.rechecking = true
	l.listMu.Unlock()

	type result struct {
		absent bool
		err    error
	}
	done := make(chan result, 1)
	go func() {
		res := result{err: errors.New("re-check listing panicked")}
		defer func() {
			_ = recover() // a panicking listing is an erroring re-check
			l.listMu.Lock()
			l.rechecking = false
			l.listMu.Unlock()
			done <- res
		}()
		names, err := sp.ListRunning(name)
		if slices.Contains(names, name) {
			res = result{}
			return
		}
		res = result{absent: err == nil, err: err}
	}()
	timer := time.NewTimer(inventoryListingBound)
	defer timer.Stop()
	select {
	case res := <-done:
		return res.absent, res.err
	case <-timer.C:
		return false, errOnDeathRecheckTimeout
	case <-ctx.Done():
		return false, ctx.Err()
	}
}

// traceOnDeath records one hook execution in its own trace cycle, like a lane
// pass.
func (cr *CityRuntime) traceOnDeath(e *deathEdge, outcome string, err error, d time.Duration) {
	if cr.trace == nil {
		return
	}
	cfg, _ := cr.serviceProviderSnapshot()
	trace := cr.trace.beginCycle(sessionReconcilerTraceCycleInfo{
		TickTrigger:   string(inventoryLaneTraceTrigger),
		TriggerDetail: "on_death",
		CityPath:      cr.cityPath,
	}, cfg, nil)
	if trace == nil {
		return
	}
	code := TraceOutcomeSuccess
	switch outcome {
	case onDeathOutcomeHookFailed:
		code = TraceOutcomeFailed
	case onDeathOutcomeSkippedPresent:
		code = TraceOutcomeSkippedPresent
	case onDeathOutcomeRecheckRetry:
		code = TraceOutcomeRetry
	}
	fields := map[string]any{
		"session_name":            e.name,
		"on_death_outcome":        outcome,
		"on_death_incarnation":    e.incarnation,
		"on_death_recheck_errors": e.recheckErrors,
		"inventory_pass_seq":      e.seq,
	}
	if err != nil {
		fields["error"] = err.Error()
	}
	trace.RecordControllerOperation(TraceSiteRuntimeInventoryOnDeath, TraceReasonRetained, code, "runtime_inventory_on_death", d, fields)
	trace.end(TraceCompletionCompleted, traceRecordPayload{"phase": "runtime_inventory_on_death"})
}
