package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"reflect"
	"runtime/debug"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
)

// The runtime inventory lane owns fleet runtime listing at patrol cadence and
// publishes the observation cache (runtime_observation_cache.go). It is the
// orders lane's shape (orders_lane.go): its own goroutine, safeTick around
// each pass, a backstop timer reset at the end of every pass, the same duty
// cycle for wakes, stopped by run()'s context and joined before shutdown.
//
// Each pass:
//
//  1. reads the provider under serviceStateMu, and bumps ProviderGen when a
//     reload swapped it;
//  2. lists every leaf backend exactly once, single-flight and bounded, and
//     rebuilds the merged result ListRunning("") would have returned from
//     the same answers;
//  3. classifies each backend's listing (complete, unattested, partial,
//     failed);
//  4. enriches names on backends that offer a batched inventory, in one
//     bounded call per backend;
//  5. reads attribution once per new incarnation, through
//     GetAllEnvironment, never GetMeta (tmux GetMeta maps non-session errors
//     to an empty value, which would cache a live runtime as ownerless);
//  6. publishes the snapshot, then reports health and traces;
//  7. pokes the reconciler for every name the pass proved gone, and detects
//     on_death edges for the worker (runtime_inventory_ondeath.go).
//
// A pass whose listing timed out, was still in flight or panicked publishes
// a failed outcome for every backend (named through Backends(), without
// listing): facts stay as they were and age out, health keeps moving toward
// unhealthy, and the failed merged error makes FreshSnapshot refuse the
// pass at once.
//
// Its legacy consumers are on_death (runtime_inventory_ondeath.go) and the
// runtime reapers (runtime_inventory_view.go): for the reapers the lane only
// nominates and filters candidates, and every Stop and close still follows
// their own fresh confirmation.
type runtimeInventoryLane struct {
	cache     *ObservationCache
	clock     clock.Clock
	interval  time.Duration
	stderr    io.Writer
	logPrefix string

	// wakeCh carries wake requests; buffered 1 so a burst is one pass.
	wakeCh chan struct{}

	// listing is set while a listing call is outstanding, and attributing
	// while an attribution read is (listMu). A pass that finds listing set
	// publishes a failed listing, and the late result is dropped; one that
	// finds attributing set reads no attribution.
	listMu      sync.Mutex
	listing     bool
	attributing bool
	// rechecking is set while an on_death re-check listing is outstanding.
	rechecking bool

	// Pass state, owned by whichever pass holds passMu.
	passMu        sync.Mutex
	seq           uint64
	providerGen   uint64
	lastProvider  runtime.Provider
	attribution   map[string]inventoryAttribution
	lastSignature string
	alerted       map[string]time.Time
	// poolDeathPrev holds the on_death handler names listed and not yet
	// proven gone (detectPoolDeathEdges).
	poolDeathPrev map[string]poolDeathSighting

	// onDeath queues on_death hooks for the worker and holds their names
	// against restarts.
	onDeath *onDeathGate
	// unattestedNoticed records the backends whose unattested listing of a
	// handler name stderr was told about.
	unattestedNoticed map[string]bool

	statusMu sync.Mutex
	status   inventoryLaneStatus

	// passHook, when set, sees every committed pass: the snapshots before and
	// after it. The v2 runtime installs its router here (setPassHook).
	passHook atomic.Pointer[func(prev, next *ObservationSnapshot)]

	cadencePasses atomic.Int64
	wakePasses    atomic.Int64
}

// setPassHook installs fn as the per-pass hook; nil removes it. Safe on a nil
// lane.
func (l *runtimeInventoryLane) setPassHook(fn func(prev, next *ObservationSnapshot)) {
	if l == nil {
		return
	}
	if fn == nil {
		l.passHook.Store(nil)
		return
	}
	l.passHook.Store(&fn)
}

const (
	inventoryLaneSafeTickTrigger                  = "inventory-lane"
	inventoryLaneTraceTrigger    TraceTickTrigger = "inventory"

	inventoryLaneReasonCadence = "cadence"
	inventoryLaneReasonWake    = "wake"
	inventoryLaneReasonPrime   = "prime"

	// inventoryListingBound caps one listing. It is strictly above the tmux
	// subprocess timeout (30s), so a stalled tmux fails inside its own call
	// and the other backends' answers still publish; the bound only catches
	// a backend with no timeout of its own.
	inventoryListingBound = 35 * time.Second
	// inventoryEnrichBound caps the batched inventory reads of one pass.
	inventoryEnrichBound = 10 * time.Second
	// inventoryAttributionBound caps the attribution reads of one pass. A
	// read still outstanding at the bound is abandoned, and the names left
	// unread are pending until a later pass.
	inventoryAttributionBound = 5 * time.Second
	// inventoryMinWakeGap spaces wake-triggered passes at least this far
	// apart, capping a provider event storm.
	inventoryMinWakeGap = time.Second
	// inventoryAttributionBudget caps attribution reads per pass: after a
	// restart with 150 sessions, attribution completes in three passes.
	inventoryAttributionBudget = 64
	// inventoryOwnerlessRereadPasses re-reads an ownerless runtime's
	// attribution this often, in passes.
	inventoryOwnerlessRereadPasses = 20
	// inventoryTraceHeartbeatPasses records a quiet pass this often.
	inventoryTraceHeartbeatPasses = 20
	// inventoryUnhealthyRealert repeats an unhealthy backend's alert.
	inventoryUnhealthyRealert = 15 * time.Minute
)

// Pass results, for the tick record and the pass trace.
const (
	inventoryResultPublished = "published"
	inventoryResultInFlight  = "in_flight"
	inventoryResultTimeout   = "listing_timeout"
	inventoryResultPanicked  = "listing_panicked"
	inventoryResultCanceled  = "canceled"
)

// inventoryLaneStatus is the last pass, for the tick record (statusMu).
type inventoryLaneStatus struct {
	at       time.Time
	reason   string
	ran      bool
	seq      uint64
	result   string
	duration time.Duration
}

// inventoryAttribution is one incarnation's attribution read.
type inventoryAttribution struct {
	incarnation string
	state       OwnerState
	ownerID     string
	token       string
	readSeq     uint64
}

// inventoryBackend is one leaf backend's listing.
type inventoryBackend struct {
	label    string
	provider runtime.Provider
	names    []string
	err      error
}

// inventoryListing is one listing of every leaf backend, with the merged
// result the provider's own ListRunning("") would have returned.
type inventoryListing struct {
	leaves      []inventoryBackend
	mergedNames []string
	mergedErr   error
	panicked    string
}

func newRuntimeInventoryLane(interval time.Duration, stderr io.Writer, logPrefix string) *runtimeInventoryLane {
	clk := clock.Real{}
	return &runtimeInventoryLane{
		cache:       NewObservationCache(clk, 2*interval, newInventoryEpoch()),
		clock:       clk,
		interval:    interval,
		stderr:      stderr,
		logPrefix:   logPrefix,
		wakeCh:      make(chan struct{}, 1),
		attribution: make(map[string]inventoryAttribution),
		alerted:     make(map[string]time.Time),
		onDeath:     newOnDeathGate(),

		unattestedNoticed: make(map[string]bool),
	}
}

// newInventoryEpoch returns a random 128-bit epoch for one lane start.
func newInventoryEpoch() string {
	var b [16]byte
	_, _ = rand.Read(b[:]) // crypto/rand.Read never returns an error
	return hex.EncodeToString(b[:])
}

// initRuntimeInventoryLane creates this runtime's lane. With no provider or
// no bead store there is no lane, and cr.inventoryLane stays nil.
func (cr *CityRuntime) initRuntimeInventoryLane() *runtimeInventoryLane {
	cfg, sp := cr.serviceProviderSnapshot()
	if sp == nil || cfg == nil || cr.cityBeadStore() == nil {
		return nil
	}
	cr.inventoryLane = newRuntimeInventoryLane(cfg.Daemon.PatrolIntervalDuration(), cr.stderr, cr.logPrefix)
	if cr.cs != nil {
		cr.cs.onDeathGate.Store(cr.inventoryLane.onDeath)
	}
	return cr.inventoryLane
}

// wake asks the lane for a pass. Non-blocking, and safe on a nil lane.
func (l *runtimeInventoryLane) wake() {
	if l == nil {
		return
	}
	select {
	case l.wakeCh <- struct{}{}:
	default:
	}
}

// statusSnapshot returns the last pass's status.
func (l *runtimeInventoryLane) statusSnapshot() inventoryLaneStatus {
	l.statusMu.Lock()
	defer l.statusMu.Unlock()
	return l.status
}

// primeNow runs one pass synchronously, before startup reconciliation.
func (cr *CityRuntime) primeNow(ctx context.Context) {
	cr.safeTick(func() {
		cr.runInventoryPass(ctx, inventoryLaneReasonPrime)
	}, inventoryLaneSafeTickTrigger)
}

// startRuntimeInventoryLane starts the lane goroutine and its on_death worker
// and returns a channel closed when both exit. The lane is paced like the
// orders lane (startPacedLane): a backstop timer one patrol interval after
// each pass ends, and wakes that wait out the duty cycle, so passes never run
// back to back.
func (cr *CityRuntime) startRuntimeInventoryLane(ctx context.Context) <-chan struct{} {
	lane := cr.inventoryLane
	workerDone := cr.startOnDeathWorker(ctx, lane)
	laneDone := startPacedLane(ctx, lane.interval, inventoryMinWakeGap, lane.wakeCh, func(wake bool) {
		reason := inventoryLaneReasonCadence
		if wake {
			reason = inventoryLaneReasonWake
			lane.wakePasses.Add(1)
		} else {
			lane.cadencePasses.Add(1)
		}
		cr.safeTick(func() {
			cr.runInventoryPass(ctx, reason)
		}, inventoryLaneSafeTickTrigger)
	})
	done := make(chan struct{})
	go func() {
		<-laneDone
		<-workerDone
		close(done)
	}()
	return done
}

// runRuntimeInventoryLane starts the lane goroutine under a child of ctx and
// returns its stop: cancel the lane, then wait for its goroutines, so a pass
// or an on_death hook in progress finishes before the caller (run()'s
// shutdown) goes on.
func (cr *CityRuntime) runRuntimeInventoryLane(ctx context.Context) (stop func()) {
	laneCtx, cancel := context.WithCancel(ctx)
	done := cr.startRuntimeInventoryLane(laneCtx)
	return func() {
		cancel()
		<-done
	}
}

// runInventoryPass is one lane pass.
func (cr *CityRuntime) runInventoryPass(ctx context.Context, reason string) {
	lane := cr.inventoryLane
	if lane == nil || ctx.Err() != nil {
		return
	}
	lane.passMu.Lock()
	defer lane.passMu.Unlock()

	cfg, sp := cr.serviceProviderSnapshot()
	if sp == nil {
		return
	}
	lane.seq++
	if !sameInventoryProvider(sp, lane.lastProvider) {
		lane.lastProvider = sp
		lane.providerGen++
	}
	report := inventoryPassReport{seq: lane.seq, providerGen: lane.providerGen, reason: reason, epoch: lane.cache.epoch}

	started := lane.clock.Now()
	listing, result := lane.listBounded(ctx, sp)
	report.result = result
	report.listing = lane.clock.Now().Sub(started)
	switch result {
	case inventoryResultPublished:
		lane.publish(ctx, listing, started, &report)
	case inventoryResultCanceled:
	default:
		if result == inventoryResultPanicked {
			fmt.Fprintf(lane.stderr, "%s: runtime inventory listing panicked: %s\n", lane.logPrefix, listing.panicked) //nolint:errcheck // best-effort stderr
		}
		lane.publishListingFailure(sp, started, result, &report)
	}
	if report.snapshot != nil {
		cr.detectPoolDeaths(lane, &report)
	}
	finished := lane.clock.Now()
	report.duration = finished.Sub(started)

	lane.statusMu.Lock()
	lane.status = inventoryLaneStatus{at: finished, reason: reason, ran: true, seq: report.seq, result: result, duration: report.duration}
	lane.statusMu.Unlock()

	if lane.traceDue(&report) {
		cr.traceInventoryPass(cfg, &report)
	}
}

// publish classifies, enriches and attributes one listing and publishes it.
func (l *runtimeInventoryLane) publish(ctx context.Context, listing inventoryListing, started time.Time, report *inventoryPassReport) {
	backends := make([]BackendPass, len(listing.leaves))
	for i, leaf := range listing.leaves {
		backends[i] = classifyInventoryBackend(leaf)
	}

	phase := l.clock.Now()
	attrs, hosts, enrichErrs := l.enrich(ctx, listing.leaves, backends)
	report.enrich = l.clock.Now().Sub(phase)
	report.enrichErrors = enrichErrs

	phase = l.clock.Now()
	report.attributionReads, report.attributionPending = l.attribute(ctx, attrs, hosts)
	report.attribute = l.clock.Now().Sub(phase)

	pass := InventoryPass{
		Epoch:       l.cache.epoch,
		Seq:         l.seq,
		ProviderGen: l.providerGen,
		StartedAt:   started,
		FinishedAt:  l.clock.Now(),
		MergedNames: listing.mergedNames,
		MergedErr:   listing.mergedErr,
		Backends:    backends,
	}
	l.commit(pass, attrs, report)
}

// publishListingFailure publishes a pass whose listing produced no answer:
// every backend failed with the same error, so no fact moves, health keeps
// counting toward unhealthy, and FreshSnapshot refuses the pass.
func (l *runtimeInventoryLane) publishListingFailure(sp runtime.Provider, started time.Time, result string, report *inventoryPassReport) {
	err := fmt.Errorf("runtime inventory listing %s", result)
	leaves := inventoryLeaves(sp, "")
	backends := make([]BackendPass, len(leaves))
	for i, leaf := range leaves {
		leaf.err = err
		backends[i] = classifyInventoryBackend(leaf)
	}
	pass := InventoryPass{
		Epoch:       l.cache.epoch,
		Seq:         l.seq,
		ProviderGen: l.providerGen,
		StartedAt:   started,
		FinishedAt:  l.clock.Now(),
		MergedErr:   err,
		Backends:    backends,
	}
	l.commit(pass, nil, report)
}

// commit publishes one pass, prunes attribution to the published names, and
// reports health.
func (l *runtimeInventoryLane) commit(pass InventoryPass, attrs map[string]InventoryAttrs, report *inventoryPassReport) {
	before := l.cache.Snapshot()
	report.flips = l.cache.PublishInventory(pass, attrs)
	snap := l.cache.Snapshot()
	report.attrs = attrs
	report.gone = inventoryGone(before, snap)
	for name := range l.attribution {
		if _, ok := snap.ByName[name]; !ok {
			delete(l.attribution, name)
		}
	}
	report.snapshot = snap
	report.alerts = l.updateHealth(snap.Health, pass.FinishedAt)
	if hook := l.passHook.Load(); hook != nil {
		(*hook)(before, snap)
	}
}

// inventoryGone returns, in name order, the names next proves gone that
// prev showed listed.
func inventoryGone(prev, next *ObservationSnapshot) []string {
	var gone []string
	for name, obs := range next.ByName {
		if obs.Listed.Value == ObsNo && prev.ByName[name].Listed.Value == ObsYes {
			gone = append(gone, name)
		}
	}
	sort.Strings(gone)
	return gone
}

// listBounded runs one listing, single-flight and bounded. A listing still
// outstanding from an earlier pass makes this pass in-flight; a listing that
// outlives the bound is abandoned, and its late result is dropped.
func (l *runtimeInventoryLane) listBounded(ctx context.Context, sp runtime.Provider) (inventoryListing, string) {
	l.listMu.Lock()
	if l.listing {
		l.listMu.Unlock()
		return inventoryListing{}, inventoryResultInFlight
	}
	l.listing = true
	l.listMu.Unlock()

	done := make(chan inventoryListing, 1)
	go func() {
		var res inventoryListing
		defer func() {
			if r := recover(); r != nil {
				res = inventoryListing{panicked: fmt.Sprintf("%v\n%s", r, debug.Stack())}
			}
			l.listMu.Lock()
			l.listing = false
			l.listMu.Unlock()
			done <- res
		}()
		res = listInventoryBackends(sp, "")
	}()

	timer := time.NewTimer(inventoryListingBound)
	defer timer.Stop()
	select {
	case res := <-done:
		if res.panicked != "" {
			return res, inventoryResultPanicked
		}
		return res, inventoryResultPublished
	case <-timer.C:
		return inventoryListing{}, inventoryResultTimeout
	case <-ctx.Done():
		return inventoryListing{}, inventoryResultCanceled
	}
}

// listInventoryBackends lists every leaf backend of sp exactly once and
// rebuilds the merged result sp.ListRunning("") returns. A composite is
// walked through runtime.BackendsProvider, which names backends without
// listing them; its merged result is runtime.MergeBackendListings over its
// backends' results, which is how auto and hybrid compute ListRunning.
// Nested leaves are labeled by path ("default/local").
func listInventoryBackends(sp runtime.Provider, label string) inventoryListing {
	composite, ok := sp.(runtime.BackendsProvider)
	if !ok {
		names, err := sp.ListRunning("")
		return inventoryListing{
			leaves:      []inventoryBackend{{label: label, provider: sp, names: names, err: err}},
			mergedNames: names,
			mergedErr:   err,
		}
	}
	var out inventoryListing
	var listings []runtime.BackendListing
	for _, b := range composite.Backends() {
		child := listInventoryBackends(b.Provider, joinInventoryLabel(label, b.Label))
		out.leaves = append(out.leaves, child.leaves...)
		listings = append(listings, runtime.BackendListing{Label: b.Label, Provider: b.Provider, Names: child.mergedNames, Err: child.mergedErr})
	}
	out.mergedNames, out.mergedErr = runtime.MergeBackendListings(listings)
	return out
}

// inventoryLeaves names every leaf backend of sp, with its label, without
// listing anything.
func inventoryLeaves(sp runtime.Provider, label string) []inventoryBackend {
	composite, ok := sp.(runtime.BackendsProvider)
	if !ok {
		return []inventoryBackend{{label: label, provider: sp}}
	}
	var leaves []inventoryBackend
	for _, b := range composite.Backends() {
		leaves = append(leaves, inventoryLeaves(b.Provider, joinInventoryLabel(label, b.Label))...)
	}
	return leaves
}

func joinInventoryLabel(parent, label string) string {
	if parent == "" {
		return label
	}
	return parent + "/" + label
}

// classifyInventoryBackend applies the partial-list rule to one backend.
func classifyInventoryBackend(leaf inventoryBackend) BackendPass {
	b := BackendPass{
		Label:    leaf.label,
		Attested: runtime.ListRunningAttested(leaf.provider),
		Names:    leaf.names,
		Err:      leaf.err,
	}
	switch {
	case leaf.err == nil && b.Attested:
		b.Outcome = OutcomeComplete
	case leaf.err == nil:
		b.Outcome = OutcomeUnattested
	case runtime.IsPartialListError(leaf.err):
		b.Outcome = OutcomePartial
		b.ServerAbsent = runtime.IsRuntimeServerAbsent(leaf.err)
	default:
		b.Outcome = OutcomeFailed
	}
	return b
}

// enrich reads the batched inventory of every backend that offers one, in
// one call per backend under one bound. A name belongs to the first backend
// that listed it. Names on a backend without an inventory get empty attrs,
// which record their enrichment facts as unsupported; names whose backend's
// inventory failed, or that the inventory omits, get none and keep theirs.
// hosts maps each enriched name to its backend, for attribution.
func (l *runtimeInventoryLane) enrich(ctx context.Context, leaves []inventoryBackend, backends []BackendPass) (attrs map[string]InventoryAttrs, hosts map[string]runtime.Provider, errs []string) {
	attrs = make(map[string]InventoryAttrs)
	hosts = make(map[string]runtime.Provider)
	ctx, cancel := context.WithTimeout(ctx, inventoryEnrichBound)
	defer cancel()
	claimed := make(map[string]bool)
	for i, leaf := range leaves {
		if backends[i].Outcome == OutcomeFailed {
			continue
		}
		var mine []string
		for _, name := range leaf.names {
			if !claimed[name] {
				claimed[name] = true
				mine = append(mine, name)
			}
		}
		if len(mine) == 0 {
			continue
		}
		inv, ok := leaf.provider.(runtime.InventoryProvider)
		if !ok {
			for _, name := range mine {
				attrs[name] = InventoryAttrs{}
			}
			continue
		}
		entries, err := inv.RuntimeInventory(ctx)
		if err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", inventoryLabelForTrace(leaf.label), err))
			continue
		}
		for _, name := range mine {
			e, ok := entries[name]
			if !ok {
				continue
			}
			attrs[name] = InventoryAttrs{
				Incarnation:   e.Incarnation,
				DeadKnown:     e.DeadKnown,
				AllPanesDead:  e.AllPanesDead,
				AttachedKnown: e.AttachedKnown,
				Attached:      e.Attached,
			}
			hosts[name] = leaf.provider
		}
	}
	return attrs, hosts, errs
}

// attribute fills attrs' owner fields for every enriched incarnation, reading
// (GC_SESSION_ID, GC_INSTANCE_TOKEN) at most once per incarnation, at most
// inventoryAttributionBudget times per pass, and within
// inventoryAttributionBound. A read error leaves the owner Unknown and is
// retried next pass; a clean read without GC_SESSION_ID is ownerless and is
// re-read every inventoryOwnerlessRereadPasses passes. A name whose
// incarnation is not read this pass carries no owner at all, never its
// previous incarnation's. It returns the reads made and the incarnations left
// unread (budget, bound, cancellation, or a read still outstanding).
func (l *runtimeInventoryLane) attribute(ctx context.Context, attrs map[string]InventoryAttrs, hosts map[string]runtime.Provider) (reads, pending int) {
	ctx, cancel := context.WithTimeout(ctx, inventoryAttributionBound)
	defer cancel()
	names := make([]string, 0, len(hosts))
	for name := range hosts {
		if attrs[name].Incarnation != "" {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	for _, name := range names {
		a := attrs[name]
		env, ok := hosts[name].(runtime.EnvironmentBatchProvider)
		if !ok {
			continue
		}
		cached, have := l.attribution[name]
		current := have && cached.incarnation == a.Incarnation
		due := !current || cached.state == OwnerUnknown ||
			(cached.state == OwnerNone && l.seq-cached.readSeq >= inventoryOwnerlessRereadPasses)
		if due && reads < inventoryAttributionBudget && ctx.Err() == nil {
			if got, ok := l.readAttribution(ctx, env, name, a.Incarnation); ok {
				reads++
				cached, current = got, true
				l.attribution[name] = got
			} else if !current {
				pending++
			}
		} else if due && !current {
			pending++
		}
		if current {
			a.OwnerState, a.OwnerID, a.InstanceToken = cached.state, cached.ownerID, cached.token
			attrs[name] = a
		}
	}
	return reads, pending
}

// readAttribution reads one incarnation's attribution on its own goroutine,
// so a wedged read cannot hold the pass past ctx. It reports false, and reads
// nothing, while an earlier abandoned read is still outstanding.
func (l *runtimeInventoryLane) readAttribution(ctx context.Context, env runtime.EnvironmentBatchProvider, name, incarnation string) (inventoryAttribution, bool) {
	l.listMu.Lock()
	if l.attributing {
		l.listMu.Unlock()
		return inventoryAttribution{}, false
	}
	l.attributing = true
	l.listMu.Unlock()

	seq := l.seq
	done := make(chan inventoryAttribution, 1)
	go func() {
		got := inventoryAttribution{incarnation: incarnation, readSeq: seq}
		defer func() {
			// A panicking read is a failed read: the owner stays Unknown.
			_ = recover()
			l.listMu.Lock()
			l.attributing = false
			l.listMu.Unlock()
			done <- got
		}()
		got = readInventoryAttribution(env, name, incarnation, seq)
	}()
	select {
	case got := <-done:
		return got, true
	case <-ctx.Done():
		return inventoryAttribution{}, false
	}
}

func readInventoryAttribution(env runtime.EnvironmentBatchProvider, name, incarnation string, seq uint64) inventoryAttribution {
	got := inventoryAttribution{incarnation: incarnation, readSeq: seq}
	vars, err := env.GetAllEnvironment(name)
	if err != nil {
		return got
	}
	got.state = OwnerNone
	if id := strings.TrimSpace(vars["GC_SESSION_ID"]); id != "" {
		got.state, got.ownerID, got.token = OwnerSession, id, strings.TrimSpace(vars["GC_INSTANCE_TOKEN"])
	}
	return got
}

// updateHealth alerts on stderr once per unhealthy episode per backend, and
// again every inventoryUnhealthyRealert while it lasts. It returns the labels
// alerted this pass.
func (l *runtimeInventoryLane) updateHealth(health map[string]BackendHealth, now time.Time) []string {
	var alerted []string
	for _, label := range sortedHealthLabels(health) {
		h := health[label]
		if h.State != backendHealthUnhealthy {
			delete(l.alerted, label)
			continue
		}
		if last, ok := l.alerted[label]; ok && now.Sub(last) < inventoryUnhealthyRealert {
			continue
		}
		l.alerted[label] = now
		alerted = append(alerted, inventoryLabelForTrace(label))
		fmt.Fprintf(l.stderr, "%s: runtime inventory: backend %s unhealthy: no complete listing for %s (last complete %s)\n", //nolint:errcheck // best-effort stderr
			l.logPrefix, inventoryLabelForTrace(label), now.Sub(h.FailingSince).Round(time.Second), formatInventoryTime(h.LastCompleteAt))
	}
	return alerted
}

func formatInventoryTime(t time.Time) string {
	if t.IsZero() {
		return "never"
	}
	return t.UTC().Format(time.RFC3339)
}

// sameInventoryProvider reports whether sp is the provider the last pass
// read. A provider whose dynamic type is not comparable never matches.
func sameInventoryProvider(sp, last runtime.Provider) bool {
	if sp == nil || last == nil {
		return sp == last
	}
	if reflect.TypeOf(sp) != reflect.TypeOf(last) || !reflect.TypeOf(sp).Comparable() {
		return false
	}
	return sp == last
}

func sortedHealthLabels(health map[string]BackendHealth) []string {
	labels := make([]string, 0, len(health))
	for label := range health {
		labels = append(labels, label)
	}
	sort.Strings(labels)
	return labels
}

// inventoryLabelForTrace names a backend in traces and logs; a single
// provider's backend has the empty label.
func inventoryLabelForTrace(label string) string {
	if label == "" {
		return "provider"
	}
	return label
}

// inventoryPassReport is what one pass traces.
type inventoryPassReport struct {
	epoch              string
	seq                uint64
	providerGen        uint64
	reason             string
	result             string
	duration           time.Duration
	listing            time.Duration
	enrich             time.Duration
	attribute          time.Duration
	enrichErrors       []string
	attributionReads   int
	attributionPending int
	flips              int
	alerts             []string
	snapshot           *ObservationSnapshot
	// attrs is the pass's enrichment, and gone the names it proved gone.
	attrs map[string]InventoryAttrs
	gone  []string
}

// traceDue reports whether a pass is worth a trace record: a flip, a change
// in its result or any backend's outcome or health, an alert, or every
// inventoryTraceHeartbeatPasses passes.
func (l *runtimeInventoryLane) traceDue(r *inventoryPassReport) bool {
	signature := r.result
	if r.snapshot != nil {
		signature += "|" + inventoryBackendOutcomes(r.snapshot.Inventory.Backends) + "|" + inventoryHealthSummary(r.snapshot.Health)
	}
	changed := signature != l.lastSignature
	l.lastSignature = signature
	return changed || r.flips > 0 || len(r.alerts) > 0 || r.seq%inventoryTraceHeartbeatPasses == 0
}

// traceInventoryPass records one pass in its own trace cycle, like an
// orders-lane pass. The config revision is omitted: a lane pass is not a
// config-revision boundary.
func (cr *CityRuntime) traceInventoryPass(cfg *config.City, r *inventoryPassReport) {
	if cr.trace == nil {
		return
	}
	trace := cr.trace.beginCycle(sessionReconcilerTraceCycleInfo{
		TickTrigger:   string(inventoryLaneTraceTrigger),
		TriggerDetail: r.reason,
		CityPath:      cr.cityPath,
	}, cfg, nil)
	if trace == nil {
		return
	}
	trace.RecordControllerOperation(TraceSiteRuntimeInventoryPass, TraceReasonRetained, inventoryPassOutcome(r),
		"runtime_inventory_pass", r.duration, inventoryPassFields(r))
	trace.end(TraceCompletionCompleted, traceRecordPayload{"phase": "runtime_inventory"})
}

// inventoryPassOutcome is a pass record's outcome: complete when every
// backend's listing was, partial when any was not, and failed when the pass
// published nothing.
func inventoryPassOutcome(r *inventoryPassReport) TraceOutcomeCode {
	switch r.result {
	case inventoryResultPublished:
	case inventoryResultTimeout:
		return TraceOutcomeDeadlineExceeded
	case inventoryResultCanceled:
		return TraceOutcomeCanceled
	default:
		return TraceOutcomeFailed
	}
	for _, b := range r.snapshot.Inventory.Backends {
		if b.Outcome != OutcomeComplete {
			return TraceOutcomePartial
		}
	}
	return TraceOutcomeComplete
}

// inventoryPassFields are a pass record's trace fields.
func inventoryPassFields(r *inventoryPassReport) map[string]any {
	fields := map[string]any{
		"inventory_epoch":         r.epoch,
		"inventory_reason":        r.reason,
		"inventory_result":        r.result,
		"inventory_pass_seq":      r.seq,
		"inventory_provider_gen":  r.providerGen,
		"inventory_listing_ms":    r.listing.Milliseconds(),
		"inventory_enrich_ms":     r.enrich.Milliseconds(),
		"inventory_attribute_ms":  r.attribute.Milliseconds(),
		"inventory_flips":         r.flips,
		"inventory_attr_reads":    r.attributionReads,
		"inventory_attr_pending":  r.attributionPending,
		"inventory_enrich_errors": strings.Join(r.enrichErrors, "; "),
		"inventory_health_alerts": strings.Join(r.alerts, ","),
	}
	if s := r.snapshot; s != nil {
		addInventorySnapshotFields(fields, s)
		fields["inventory_names"] = len(s.Inventory.MergedNames)
	}
	return fields
}

// tickFields are the tick record's view of the lane: its liveness (the age
// and trigger of its last pass), what that pass did, and the age of the
// published snapshot and of its generation.
func (l *runtimeInventoryLane) tickFields(now time.Time) map[string]any {
	st := l.statusSnapshot()
	fields := map[string]any{}
	addBackstopAgeFields(fields, st.at, st.reason, st.ran)
	fields["inventory_last_pass_seq"] = st.seq
	fields["inventory_last_result"] = st.result
	fields["inventory_last_pass_ms"] = st.duration.Milliseconds()
	snap := l.cache.Snapshot()
	fields["inventory_epoch"] = snap.Epoch
	if !snap.At.IsZero() {
		fields["inventory_age_ms"] = now.Sub(snap.At).Milliseconds()
	}
	if !snap.GenAt.IsZero() {
		fields["inventory_gen_age_ms"] = now.Sub(snap.GenAt).Milliseconds()
	}
	addInventorySnapshotFields(fields, snap)
	return fields
}

// addInventorySnapshotFields adds a snapshot's generation, per-backend
// outcomes and health.
func addInventorySnapshotFields(fields map[string]any, s *ObservationSnapshot) {
	var partial, absent []string
	for _, b := range s.Inventory.Backends {
		if b.Outcome == OutcomePartial || b.Outcome == OutcomeFailed {
			partial = append(partial, inventoryLabelForTrace(b.Label))
		}
		if b.ServerAbsent {
			absent = append(absent, inventoryLabelForTrace(b.Label))
		}
	}
	fields["inventory_gen"] = s.Gen
	// The pass a decision reading this snapshot would use (spec §4.5).
	fields["inventory_pass_seq"] = s.PassSeq
	fields["inventory_backend_outcomes"] = inventoryBackendOutcomes(s.Inventory.Backends)
	fields["inventory_partial_backends"] = strings.Join(partial, ",")
	fields["inventory_server_absent_backends"] = strings.Join(absent, ",")
	fields["inventory_backend_health"] = inventoryHealthSummary(s.Health)
	fields["inventory_all_primed"] = s.AllPrimed()
}

// inventoryBackendOutcomes renders "label=outcome" pairs in backend order.
func inventoryBackendOutcomes(backends []BackendPass) string {
	parts := make([]string, 0, len(backends))
	for _, b := range backends {
		parts = append(parts, inventoryLabelForTrace(b.Label)+"="+b.Outcome.String())
	}
	return strings.Join(parts, ",")
}

// inventoryHealthSummary renders "label=state" pairs in label order.
func inventoryHealthSummary(health map[string]BackendHealth) string {
	parts := make([]string, 0, len(health))
	for _, label := range sortedHealthLabels(health) {
		parts = append(parts, inventoryLabelForTrace(label)+"="+health[label].State)
	}
	return strings.Join(parts, ",")
}
