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
//  5. reads runtime identity env (v5 O3): new incarnations first, then
//     failed reads, then the least recently read names,
//     inventoryAttributionBudget reads per patrol interval, through
//     GetAllEnvironment where the backend batches it and GetMeta per key on
//     local sidecar backends (acp, subprocess); other backends are not read;
//  6. probes agent-process liveness every pass under each live pane, with
//     the runtime's own GT_PROCESS_NAMES, within the enrichment bound;
//  7. publishes the snapshot, then reports health and traces;
//  8. pokes the reconciler for every name the pass proved gone, and detects
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
	// while an identity env read is (listMu). A pass that finds listing set
	// publishes a failed listing, and the late result is dropped; one that
	// finds attributing set reads no identity.
	listMu      sync.Mutex
	listing     bool
	attributing bool
	// rechecking is set while an on_death re-check listing is outstanding.
	rechecking bool

	// Pass state, owned by whichever pass holds passMu.
	passMu       sync.Mutex
	seq          uint64
	providerGen  uint64
	lastProvider runtime.Provider
	identity     map[string]inventoryIdentity
	// envWindowAt starts the current patrol interval's env-read budget;
	// envWindowReads counts the reads made in it, and envWindowRefreshes
	// those that re-read a name's current incarnation.
	envWindowAt        time.Time
	envWindowReads     int
	envWindowRefreshes int
	lastSignature      string
	alerted            map[string]time.Time
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
	// inventoryEnrichBound caps the batched inventory reads and the process
	// probes of one pass together, so the worst-case pass (listing, this
	// bound and the identity reads' bound) stays within maxAge.
	inventoryEnrichBound = 10 * time.Second
	// inventoryAttributionBound caps the identity env reads of one pass. A
	// read still outstanding at the bound is abandoned, and the names left
	// unread wait for a later pass.
	inventoryAttributionBound = 5 * time.Second
	// inventoryMinWakeGap spaces wake-triggered passes at least this far
	// apart, capping a provider event storm.
	inventoryMinWakeGap = time.Second
	// inventoryAttributionBudget caps identity env reads per patrol
	// interval (v5 O3): after a restart with 150 sessions, every identity is
	// read within three intervals.
	inventoryAttributionBudget = 64
	// inventoryRefreshBudget caps the reads of an interval that refresh a
	// name's current incarnation, so a steady state leaves half the budget
	// to new incarnations seen by wake passes.
	inventoryRefreshBudget = inventoryAttributionBudget / 2
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

// inventoryIdentity is the last identity read of one name's incarnation.
type inventoryIdentity struct {
	incarnation string
	id          runtimeIdentity
}

// inventoryBackend is one leaf backend's listing. confirmedDead is a
// server-absent listing whose server the backend confirmed dead right after.
type inventoryBackend struct {
	label         string
	provider      runtime.Provider
	names         []string
	err           error
	confirmedDead bool
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
		cache:     NewObservationCache(clk, 2*interval, newInventoryEpoch()),
		clock:     clk,
		interval:  interval,
		stderr:    stderr,
		logPrefix: logPrefix,
		wakeCh:    make(chan struct{}, 1),
		identity:  make(map[string]inventoryIdentity),
		alerted:   make(map[string]time.Time),
		onDeath:   newOnDeathGate(),

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
		lane.publish(ctx, listing, started, probeConcurrency(cfg), &report)
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

// probeConcurrency is the process probes' concurrency for cfg.
func probeConcurrency(cfg *config.City) int {
	if cfg == nil {
		return (&config.DaemonConfig{}).ProbeConcurrencyOrDefault()
	}
	return cfg.Daemon.ProbeConcurrencyOrDefault()
}

// publish classifies, enriches, reads identity for and probes one listing,
// and publishes it.
func (l *runtimeInventoryLane) publish(ctx context.Context, listing inventoryListing, started time.Time, concurrency int, report *inventoryPassReport) {
	backends := make([]BackendPass, len(listing.leaves))
	for i, leaf := range listing.leaves {
		backends[i] = classifyInventoryBackend(leaf)
	}

	// The batched inventory reads and the process probes share one bound.
	enrichCtx, cancel := context.WithTimeout(ctx, inventoryEnrichBound)
	defer cancel()
	phase := l.clock.Now()
	attrs, hosts, enrichErrs := l.enrich(enrichCtx, listing.leaves, backends)
	report.enrich = l.clock.Now().Sub(phase)
	report.enrichErrors = enrichErrs

	phase = l.clock.Now()
	report.envReads, report.envBacklog = l.readIdentities(ctx, attrs, hosts)
	report.envRead = l.clock.Now().Sub(phase)

	phase = l.clock.Now()
	report.processProbes = probeProcesses(enrichCtx, attrs, hosts, concurrency)
	report.processProbe = l.clock.Now().Sub(phase)

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

// commit publishes one pass, prunes identity reads to the names it listed,
// and reports health. A name its backend failed to list keeps an
// incarnation-keyed read, which a relisting re-checks; a read with no
// incarnation (a sidecar backend) cannot be re-checked and is dropped, so a
// reused name never inherits a previous runtime's identity.
func (l *runtimeInventoryLane) commit(pass InventoryPass, attrs map[string]InventoryAttrs, report *inventoryPassReport) {
	before := l.cache.Snapshot()
	report.flips = l.cache.PublishInventory(pass, attrs)
	snap := l.cache.Snapshot()
	report.attrs = attrs
	report.gone = inventoryGone(before, snap)
	listed, failed := pass.listedOn(), make(map[string]bool)
	for _, b := range pass.Backends {
		failed[b.Label] = b.Outcome == OutcomeFailed
	}
	for name, got := range l.identity {
		if _, ok := listed[name]; ok {
			continue
		}
		if obs, ok := snap.ByName[name]; ok && failed[obs.Backend] && got.incarnation != "" {
			continue
		}
		delete(l.identity, name)
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
		// Asked right after the failing listing, and kept for this pass only.
		dead := false
		if confirmer, ok := sp.(runtime.ServerDeathConfirmer); ok && runtime.IsRuntimeServerAbsent(err) {
			dead = confirmer.ServerConfirmedDead()
		}
		return inventoryListing{
			leaves:      []inventoryBackend{{label: label, provider: sp, names: names, err: err, confirmedDead: dead}},
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
		b.ConfirmedDead = b.ServerAbsent && leaf.confirmedDead
	default:
		b.Outcome = OutcomeFailed
	}
	return b
}

// enrich reads the batched inventory of every backend that offers one, in
// one call per backend under ctx's bound. A name belongs to the first backend
// that listed it. Names on a backend without an inventory get empty attrs,
// which record their enrichment facts as unsupported; names whose backend's
// inventory failed, or that the inventory omits, get none and keep theirs.
// hosts maps each enriched name to its backend, for the identity read and
// the process probe.
func (l *runtimeInventoryLane) enrich(ctx context.Context, leaves []inventoryBackend, backends []BackendPass) (attrs map[string]InventoryAttrs, hosts map[string]runtime.Provider, errs []string) {
	attrs = make(map[string]InventoryAttrs)
	hosts = make(map[string]runtime.Provider)
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
				hosts[name] = leaf.provider
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

// readIdentities fills the identity of every enriched name on a backend
// whose identity can be read (identityReadable) from the lane's identity
// reads (v5 O3). It reads new incarnations first, then failed reads of the
// current incarnation, then the least recently read, within the patrol
// interval's inventoryAttributionBudget (refreshes within
// inventoryRefreshBudget) and inventoryAttributionBound. A read error leaves
// the name's identity unknown; a name not read this pass keeps its current
// incarnation's last read (commit prunes the reads of unlisted names). The
// reapers' owner fact is derived from the same read, and only where it was
// always set: an incarnation on a backend with a batched environment read.
// It returns the reads made and the names left without a good read.
func (l *runtimeInventoryLane) readIdentities(ctx context.Context, attrs map[string]InventoryAttrs, hosts map[string]runtime.Provider) (reads, backlog int) {
	ctx, cancel := context.WithTimeout(ctx, inventoryAttributionBound)
	defer cancel()
	if now := l.clock.Now(); l.envWindowAt.IsZero() || now.Sub(l.envWindowAt) >= l.interval {
		l.envWindowAt, l.envWindowReads, l.envWindowRefreshes = now, 0, 0
	}
	// rank orders the reads: a new incarnation (0), a failed read of the
	// current one (1), then a refresh (2).
	rank := func(name string) int {
		got, ok := l.identity[name]
		switch {
		case !ok || got.incarnation != attrs[name].Incarnation:
			return 0
		case !got.id.Known:
			return 1
		}
		return 2
	}
	names := make([]string, 0, len(hosts))
	for name, host := range hosts {
		if identityReadable(host) {
			names = append(names, name)
		}
	}
	sort.Slice(names, func(i, j int) bool {
		a, b := names[i], names[j]
		if ra, rb := rank(a), rank(b); ra != rb {
			return ra < rb
		}
		if ra, rb := l.identity[a].id.ReadAt, l.identity[b].id.ReadAt; !ra.Equal(rb) {
			return ra.Before(rb)
		}
		return a < b
	})
	for _, name := range names {
		refresh := rank(name) == 2
		if l.envWindowReads >= inventoryAttributionBudget || (refresh && l.envWindowRefreshes >= inventoryRefreshBudget) || ctx.Err() != nil {
			break
		}
		id, ok := l.readIdentityBounded(ctx, hosts[name], name)
		if !ok {
			break
		}
		reads++
		l.envWindowReads++
		if refresh {
			l.envWindowRefreshes++
		}
		l.identity[name] = inventoryIdentity{incarnation: attrs[name].Incarnation, id: id}
	}
	for _, name := range names {
		if rank(name) < 2 {
			backlog++
		}
		got, ok := l.identity[name]
		if !ok || got.incarnation != attrs[name].Incarnation {
			continue
		}
		a := attrs[name]
		a.Identity = got.id
		if _, batch := hosts[name].(runtime.EnvironmentBatchProvider); batch && a.Incarnation != "" && got.id.Known {
			a.OwnerState = OwnerNone
			if got.id.SessionID != "" {
				a.OwnerState, a.OwnerID, a.InstanceToken = OwnerSession, got.id.SessionID, got.id.Token
			}
		}
		attrs[name] = a
	}
	return reads, backlog
}

// identityReadable reports whether leaf's identity env is cheap and local to
// read: a batched environment read (tmux), or a local sidecar (acp,
// subprocess). Other leaves' GetMeta reaches a pod, a host or a script, so
// their identity is never read, and stays unknown.
func identityReadable(leaf runtime.Provider) bool {
	if _, ok := leaf.(runtime.EnvironmentBatchProvider); ok {
		return true
	}
	sidecar, ok := leaf.(runtime.IdentitySidecarProvider)
	return ok && sidecar.LocalIdentitySidecar()
}

// readIdentityBounded reads one name's identity on its own goroutine, so a
// wedged read cannot hold the pass past ctx. It reports false, and reads
// nothing, while an earlier abandoned read is still outstanding.
func (l *runtimeInventoryLane) readIdentityBounded(ctx context.Context, leaf runtime.Provider, name string) (runtimeIdentity, bool) {
	l.listMu.Lock()
	if l.attributing {
		l.listMu.Unlock()
		return runtimeIdentity{}, false
	}
	l.attributing = true
	l.listMu.Unlock()

	at := l.clock.Now()
	done := make(chan runtimeIdentity, 1)
	go func() {
		var got runtimeIdentity
		defer func() {
			// A panicking read is a failed read: the identity stays unknown.
			_ = recover()
			l.listMu.Lock()
			l.attributing = false
			l.listMu.Unlock()
			got.ReadAt = at
			done <- got
		}()
		got = readIdentityEnv(leaf, name)
	}()
	select {
	case got := <-done:
		return got, true
	case <-ctx.Done():
		return runtimeIdentity{}, false
	}
}

// identityEnvKeys are the env keys an identity read returns, in
// readIdentityEnv's order. No drain-ack key is read (v5 O3).
var identityEnvKeys = [...]string{"GC_SESSION_ID", "GC_RUNTIME_EPOCH", "GC_INSTANCE_TOKEN", "GT_PROCESS_NAMES"}

// readRuntimeIdentity is the fresh identity read (v5 O2, I11) the start,
// stop and close effects pair with compareIdentity: one read of name's
// identity env on leaf, bounded at fenceProbeTimeout. A read that errors,
// panics or outlives its bound, or a leaf whose identity is not readable
// (identityReadable; a composite such as auto included: pass the leaf), is
// not Known. ReadAt is left to the caller.
func readRuntimeIdentity(ctx context.Context, leaf runtime.Provider, name string) runtimeIdentity {
	ctx, cancel := context.WithTimeout(ctx, fenceProbeTimeout)
	defer cancel()
	done := make(chan runtimeIdentity, 1)
	go func() {
		var got runtimeIdentity
		defer func() {
			_ = recover()
			done <- got
		}()
		got = readIdentityEnv(leaf, name)
	}()
	select {
	case got := <-done:
		return got
	case <-ctx.Done():
		return runtimeIdentity{}
	}
}

// readIdentityEnv reads name's identity env from leaf: one GetAllEnvironment
// where the leaf batches it (tmux), otherwise GetMeta per key on a local
// sidecar (acp, subprocess). Any error, or a leaf whose identity is not
// readable, leaves the identity unknown.
//
// A sidecar is one file per key, re-seeded by clearing every key and then
// writing each. Its keys are read twice, and only two equal reads count: a
// clear or write between them changes a key both reads cover, so equal reads
// never mix two seeds. A seed still writing can still show its keys not yet
// written as empty (C4c1: a straddled read of a re-seeding sidecar may read
// the row's token with no session ID). LL3 lists a subprocess runtime only
// after its seed completes, so this needs a re-seed under a listed name.
func readIdentityEnv(leaf runtime.Provider, name string) runtimeIdentity {
	var v [len(identityEnvKeys)]string
	if batch, ok := leaf.(runtime.EnvironmentBatchProvider); ok {
		vars, err := batch.GetAllEnvironment(name)
		if err != nil {
			return runtimeIdentity{}
		}
		for i, key := range identityEnvKeys {
			v[i] = strings.TrimSpace(vars[key])
		}
	} else {
		if !identityReadable(leaf) {
			return runtimeIdentity{}
		}
		first, ok := readSidecarIdentity(leaf, name)
		if !ok {
			return runtimeIdentity{}
		}
		if v, ok = readSidecarIdentity(leaf, name); !ok || v != first {
			return runtimeIdentity{}
		}
	}
	id := runtimeIdentity{Known: true, SessionID: v[0], Epoch: v[1], Token: v[2]}
	for _, pn := range strings.Split(v[3], ",") {
		if pn = strings.TrimSpace(pn); pn != "" {
			id.ProcessNames = append(id.ProcessNames, pn)
		}
	}
	return id
}

// readSidecarIdentity reads the identity keys through GetMeta, once each.
func readSidecarIdentity(leaf runtime.Provider, name string) (v [len(identityEnvKeys)]string, ok bool) {
	for i, key := range identityEnvKeys {
		val, err := leaf.GetMeta(name, key)
		if err != nil {
			return v, false
		}
		v[i] = strings.TrimSpace(val)
	}
	return v, true
}

// probeProcesses probes agent-process liveness for every enriched name whose
// pane is alive and whose identity carries GT_PROCESS_NAMES, on a leaf that
// reports failed observations (v5 O3), every pass, at most concurrency at a
// time, within ctx (the pass's enrichment bound). Only a complete,
// error-free answer with a live pane, for the incarnation the pass enriched
// (probedIncarnation), is known; anything else leaves the fact unknown.
// Other names are not probed, and their fact stays unsupported. It returns
// the probes made.
func probeProcesses(ctx context.Context, attrs map[string]InventoryAttrs, hosts map[string]runtime.Provider, concurrency int) int {
	var names []string
	for name, a := range attrs {
		if _, ok := hosts[name].(runtime.LivenessObserverWithError); ok && a.DeadKnown && !a.AllPanesDead && len(a.Identity.ProcessNames) > 0 {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	type answer struct {
		name         string
		known, alive bool
	}
	answers := make(chan answer, len(names))
	sem := make(chan struct{}, max(concurrency, 1))
	for _, name := range names {
		sem <- struct{}{}
		go func(leaf runtime.Provider, a InventoryAttrs) {
			defer func() { <-sem }()
			lv, status, err := runtime.ObserveLivenessBounded(ctx, leaf, name, a.Identity.ProcessNames, fenceProbeTimeout)
			known := status == runtime.ObservationComplete && err == nil && lv.Running && probedIncarnation(a.Incarnation, lv)
			answers <- answer{name: name, known: known, alive: lv.Alive}
		}(hosts[name], attrs[name])
	}
	for range names {
		got := <-answers
		a := attrs[got.name]
		a.ProcessProbed, a.ProcessKnown, a.ProcessAlive = true, got.known, got.alive
		attrs[got.name] = a
	}
	return len(names)
}

// probedIncarnation reports whether a probe answered for incarnation: the
// object it observed (ObjectID, ObjectCreated) leads the incarnation, as in
// tmux's "#{session_id}:#{session_created}:#{pane_pid}". A probe that names
// no object, or another one, answered for some other runtime.
func probedIncarnation(incarnation string, lv runtime.Liveness) bool {
	if lv.ObjectID == "" || lv.ObjectCreated == "" {
		return false
	}
	key := lv.ObjectID + ":" + lv.ObjectCreated
	return incarnation == key || strings.HasPrefix(incarnation, key+":")
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
	epoch         string
	seq           uint64
	providerGen   uint64
	reason        string
	result        string
	duration      time.Duration
	listing       time.Duration
	enrich        time.Duration
	envRead       time.Duration
	processProbe  time.Duration
	enrichErrors  []string
	envReads      int
	envBacklog    int
	processProbes int
	flips         int
	alerts        []string
	snapshot      *ObservationSnapshot
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
		"inventory_attribute_ms":  r.envRead.Milliseconds(),
		"inventory_flips":         r.flips,
		"inventory_attr_reads":    r.envReads,
		"inventory_attr_pending":  r.envBacklog,
		"inventory_probes":        r.processProbes,
		"inventory_probe_ms":      r.processProbe.Milliseconds(),
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
