package main

import (
	"sync"
	"sync/atomic"
	"time"

	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/reconcilekey"
	"github.com/gastownhall/gascity/internal/runtime"
)

// The observation cache (CONTRACT I3) holds runtime facts only: what the
// providers showed, keyed by runtime session name, so an ownerless runtime is
// as visible as an owned one. Session bead state is the census, not this.
//
// Every fact is three-valued (Unknown, No, Yes) and carries when it was
// observed and by whom. Three rules keep a stale or partial view from reading
// as the truth:
//
//   - A fact older than maxAge reads Unknown, at the reader's clock: a
//     published snapshot is immutable, so staleness cannot be written into it.
//   - Absence is concluded only by a complete, attested pass on the backend
//     that last listed the name, or, once a provider swap removed that
//     backend, by a pass on which every backend is complete. Any other
//     outcome leaves the name's facts alone to age out.
//   - Before a backend's first complete pass within the current provider,
//     every fact about names on it reads Unknown.
//
// The inventory lane is the only publisher; Note is the write-through for
// fresh three-outcome probes. Readers load the snapshot pointer without a
// lock, and a published snapshot's maps are never mutated.

// ObsFact is the three-outcome value of one runtime fact.
type ObsFact uint8

// Fact values. The zero value is Unknown, so an unset fact never reads as No.
const (
	ObsUnknown ObsFact = iota
	ObsNo
	ObsYes
)

func (v ObsFact) String() string {
	switch v {
	case ObsNo:
		return "no"
	case ObsYes:
		return "yes"
	default:
		return "unknown"
	}
}

func obsFactOf(b bool) ObsFact {
	if b {
		return ObsYes
	}
	return ObsNo
}

// ObsSource names who observed a fact.
type ObsSource uint8

// Fact sources. SourceProbe is for three-outcome probes only;
// SourceProviderEvent is reserved: provider events wake the lane, they never
// write facts.
const (
	SourceNone ObsSource = iota
	SourcePoller
	SourceProbe
	SourceProviderEvent
)

// FactKind selects one fact of a RuntimeObservation.
type FactKind uint8

// Fact kinds.
const (
	FactListed FactKind = iota
	FactRunning
	FactProcessAlive
	FactAttached
	FactPending
)

// Fact reasons.
const (
	// obsReasonPartialList marks a fact the last pass could not refresh
	// because its backend listed partially.
	obsReasonPartialList = "partial-list"
	// obsReasonUnattested marks a fact the last pass could not refresh
	// because its backend's listing cannot prove absence.
	obsReasonUnattested = "unattested"
	// obsReasonUnsupported marks a fact the backend cannot observe. It stays
	// Unknown, never No.
	obsReasonUnsupported = "unsupported"
	// obsReasonStale is a read-time Unknown: the fact is older than maxAge.
	obsReasonStale = "stale"
	// obsReasonUnprimed is a read-time Unknown: the name's backend has not
	// completed a pass since the lane started.
	obsReasonUnprimed = "unprimed"
	// obsReasonProviderSwapped is a read-time Unknown: the provider changed
	// and the name's backend has not completed a pass since.
	obsReasonProviderSwapped = "provider-swapped"
)

// observationRetentionPasses is how many consecutive passes a name may go
// unlisted before it is dropped.
const observationRetentionPasses = 10

// RuntimeFact is one observed fact.
type RuntimeFact struct {
	Value      ObsFact
	ObservedAt time.Time
	Source     ObsSource
	Reason     string
}

// OwnerState says whether a runtime's owning session was read.
type OwnerState uint8

// Owner states. OwnerNone is a clean read with no GC_SESSION_ID: an ownerless
// runtime, distinct from one whose attribution was never read.
const (
	OwnerUnknown OwnerState = iota
	OwnerNone
	OwnerSession
)

// RuntimeObservation is everything the cache knows about one runtime name.
type RuntimeObservation struct {
	SessionName string
	// Backend is the label of the backend that last listed the name: "" for a
	// single provider, "default"/"acp" or "local"/"remote" for a composite,
	// and "default/local" style paths for a nested one.
	Backend string
	// Incarnation is the provider-native runtime instance id; "" when the
	// backend cannot report one.
	Incarnation string
	// Owner is the session key from the runtime's GC_SESSION_ID when
	// OwnerState is OwnerSession.
	Owner      reconcilekey.Key
	OwnerState OwnerState
	// InstanceToken is this incarnation's GC_INSTANCE_TOKEN; "" when unknown.
	// It is a capability: never trace it.
	InstanceToken string

	// Listed: the artifact is visible in the provider listing (on tmux this
	// includes remain-on-exit corpses).
	Listed RuntimeFact
	// Running has runtime.Liveness.Running semantics: a live pane or box,
	// never a corpse.
	Running            RuntimeFact
	ProcessAlive       RuntimeFact
	Attached           RuntimeFact
	PendingInteraction RuntimeFact
	LastActivity       time.Time
	LastActivityKnown  bool
	// FirstListedAt is the first pass in this epoch that listed this
	// incarnation; LastListedAt the latest.
	FirstListedAt time.Time
	LastListedAt  time.Time
	// EnrichedAt is the listing start of the last pass that enriched the
	// name: Incarnation, the owner fields, Running and Attached are from it.
	EnrichedAt time.Time
}

func (o *RuntimeObservation) fact(kind FactKind) *RuntimeFact {
	switch kind {
	case FactListed:
		return &o.Listed
	case FactRunning:
		return &o.Running
	case FactProcessAlive:
		return &o.ProcessAlive
	case FactAttached:
		return &o.Attached
	default:
		return &o.PendingInteraction
	}
}

var allFactKinds = [...]FactKind{FactListed, FactRunning, FactProcessAlive, FactAttached, FactPending}

// InventoryOutcome classifies one backend's listing on one pass.
type InventoryOutcome uint8

// Listing outcomes (the partial-list rule).
const (
	// OutcomeComplete: error-free and attested. Concludes absence.
	OutcomeComplete InventoryOutcome = iota
	// OutcomeUnattested: error-free, but the backend cannot prove absence.
	OutcomeUnattested
	// OutcomePartial: a runtime.PartialListError. Returned names are present;
	// nothing is absent.
	OutcomePartial
	// OutcomeFailed: any other error. Concludes nothing.
	OutcomeFailed
)

func (o InventoryOutcome) String() string {
	switch o {
	case OutcomeComplete:
		return "complete"
	case OutcomeUnattested:
		return "unattested"
	case OutcomePartial:
		return "partial"
	default:
		return "failed"
	}
}

// BackendPass is one backend's listing on one pass.
type BackendPass struct {
	Label   string
	Outcome InventoryOutcome
	// Attested is runtime.ListRunningAttested on the backend, whatever the
	// outcome, so health can tell an unattested backend from a failing one.
	Attested bool
	Names    []string
	Err      error
	// ServerAbsent is runtime.IsRuntimeServerAbsent on this backend's own
	// error.
	ServerAbsent bool
}

// InventoryPass is one lane pass's listing.
type InventoryPass struct {
	Epoch       string
	Seq         uint64
	ProviderGen uint64
	// StartedAt is when the listing started. Every fact the pass observes,
	// and LastListedAt, is stamped with it: a runtime listed by this pass was
	// running no later than this instant, which is what a fence comparing
	// against a later PreWake needs.
	StartedAt  time.Time
	FinishedAt time.Time
	// MergedNames and MergedErr are exactly what Provider.ListRunning("")
	// returns for the same backend answers.
	MergedNames []string
	MergedErr   error
	Backends    []BackendPass
}

// mergedFailed reports whether the merged listing is unusable: an error that
// is not a partial list.
func (p InventoryPass) mergedFailed() bool {
	return p.MergedErr != nil && !runtime.IsPartialListError(p.MergedErr)
}

// listedOn maps every name a non-failed backend listed this pass to the
// first such backend, in backend order.
func (p InventoryPass) listedOn() map[string]string {
	on := make(map[string]string)
	for _, b := range p.Backends {
		if b.Outcome == OutcomeFailed {
			continue
		}
		for _, name := range b.Names {
			if _, ok := on[name]; !ok {
				on[name] = b.Label
			}
		}
	}
	return on
}

// concludesAbsent reports whether this pass proves gone a name it did not
// list, given the backend that last listed it: that backend listed
// completely, or it is no longer one of the pass's backends (the provider
// was swapped) and every backend listed completely.
func (p InventoryPass) concludesAbsent(label string) bool {
	if len(p.Backends) == 0 {
		return false
	}
	all := true
	for _, b := range p.Backends {
		if b.Label == label {
			return b.Outcome == OutcomeComplete
		}
		all = all && b.Outcome == OutcomeComplete
	}
	return all
}

// InventoryAttrs are one listed name's enrichment and attribution. A name
// with no entry keeps its enrichment facts; an entry whose Known flag is
// false records that fact as unsupported.
type InventoryAttrs struct {
	Incarnation             string
	DeadKnown, AllPanesDead bool
	AttachedKnown, Attached bool
	OwnerState              OwnerState
	OwnerID, InstanceToken  string
}

// Backend health states.
const (
	backendHealthHealthy    = "healthy"
	backendHealthDegraded   = "degraded"
	backendHealthUnhealthy  = "unhealthy"
	backendHealthUnattested = "unattested"
	backendHealthIdle       = "idle"
)

// observationUnhealthyAfter is how long an attested backend may go without a
// complete pass before it is unhealthy (C8.6 observation_unhealthy_after).
const observationUnhealthyAfter = 5 * time.Minute

// BackendHealth is one backend's observation health. P1.4 publishes it and
// gates nothing on it.
type BackendHealth struct {
	Label          string
	State          string
	LastCompleteAt time.Time
	FailingSince   time.Time
}

// nextBackendHealth folds one pass into a backend's health. everListed says
// whether the backend has listed any name this epoch: a server that is absent
// and has never held a session is idle, not failing.
func nextBackendHealth(prev BackendHealth, b BackendPass, everListed bool, now time.Time) BackendHealth {
	h := BackendHealth{Label: b.Label, LastCompleteAt: prev.LastCompleteAt, FailingSince: prev.FailingSince}
	switch {
	case b.Outcome == OutcomeComplete:
		h.State, h.LastCompleteAt, h.FailingSince = backendHealthHealthy, now, time.Time{}
	case !b.Attested:
		h.State, h.FailingSince = backendHealthUnattested, time.Time{}
	case b.ServerAbsent && !everListed:
		h.State, h.FailingSince = backendHealthIdle, time.Time{}
	default:
		if h.FailingSince.IsZero() {
			h.FailingSince = now
		}
		h.State = backendHealthDegraded
		if now.Sub(h.FailingSince) >= observationUnhealthyAfter {
			h.State = backendHealthUnhealthy
		}
	}
	return h
}

// ObservationSnapshot is one immutable published view.
type ObservationSnapshot struct {
	Epoch string
	Gen   uint64
	// GenAt is when Gen last advanced.
	GenAt     time.Time
	PassSeq   uint64
	At        time.Time
	Inventory InventoryPass
	ByName    map[string]RuntimeObservation // never mutated after publish
	Primed    map[string]bool               // per backend label
	Health    map[string]BackendHealth
}

// Fact reads one fact at the reader's clock: a fact older than maxAge, or
// about a name whose backend is not primed, reads ObsUnknown.
func (s *ObservationSnapshot) Fact(name string, kind FactKind, now time.Time, maxAge time.Duration) RuntimeFact {
	if s == nil {
		return RuntimeFact{}
	}
	obs, ok := s.ByName[name]
	if !ok {
		return RuntimeFact{}
	}
	return s.readFact(obs.Backend, *obs.fact(kind), now, maxAge)
}

func (s *ObservationSnapshot) readFact(backend string, f RuntimeFact, now time.Time, maxAge time.Duration) RuntimeFact {
	if f.Value == ObsUnknown {
		return f
	}
	if !s.Primed[backend] {
		reason := obsReasonUnprimed
		if s.Inventory.ProviderGen > 1 {
			reason = obsReasonProviderSwapped
		}
		return RuntimeFact{Value: ObsUnknown, ObservedAt: f.ObservedAt, Source: f.Source, Reason: reason}
	}
	if now.Sub(f.ObservedAt) > maxAge {
		return RuntimeFact{Value: ObsUnknown, ObservedAt: f.ObservedAt, Source: f.Source, Reason: obsReasonStale}
	}
	return f
}

// AllPrimed reports whether every backend of the latest pass has completed a
// pass within the current provider.
func (s *ObservationSnapshot) AllPrimed() bool {
	if s == nil || len(s.Primed) == 0 {
		return false
	}
	for _, primed := range s.Primed {
		if !primed {
			return false
		}
	}
	return true
}

// ObservationCache publishes ObservationSnapshots. The zero value is not
// usable; construct it with NewObservationCache.
type ObservationCache struct {
	clock  clock.Clock
	maxAge time.Duration
	epoch  string

	cur     atomic.Pointer[ObservationSnapshot]
	changed chan struct{}

	// mu serializes writers; readers never take it.
	mu sync.Mutex
	// unlisted counts each name's consecutive unlisted passes, for pruning.
	unlisted map[string]int
	// everListed records backends that listed a name this epoch, for health.
	everListed map[string]bool
}

// NewObservationCache returns an empty cache whose reads treat facts older
// than maxAge as Unknown.
func NewObservationCache(clk clock.Clock, maxAge time.Duration, epoch string) *ObservationCache {
	c := &ObservationCache{
		clock:      clk,
		maxAge:     maxAge,
		epoch:      epoch,
		changed:    make(chan struct{}, 1),
		unlisted:   make(map[string]int),
		everListed: make(map[string]bool),
	}
	c.cur.Store(&ObservationSnapshot{Epoch: epoch})
	return c
}

// PublishInventory folds one lane pass into a new snapshot and returns the
// number of observations whose facts or owner changed. The lane is its only
// caller, and the pass and attrs must not be mutated afterwards. Gen advances
// when anything flipped or a backend's primed state changed.
//
// Facts are stamped with the pass's StartedAt; the snapshot's At is its
// FinishedAt. A name whose backend failed this pass is left exactly as it
// was: it is neither refreshed nor counted toward pruning, and ages out.
func (c *ObservationCache) PublishInventory(pass InventoryPass, attrs map[string]InventoryAttrs) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	prev := c.cur.Load()
	at := pass.StartedAt
	swapped := pass.ProviderGen != prev.Inventory.ProviderGen

	byName := make(map[string]RuntimeObservation, len(prev.ByName))
	for name, obs := range prev.ByName {
		byName[name] = obs
	}

	// A name listed by two backends belongs to the first, in backend order.
	listedOn := pass.listedOn()
	unrefreshed := make(map[string]string)
	failed := make(map[string]bool)
	for _, b := range pass.Backends {
		switch b.Outcome {
		case OutcomeFailed:
			failed[b.Label] = true
			continue
		case OutcomePartial:
			unrefreshed[b.Label] = obsReasonPartialList
		case OutcomeUnattested:
			unrefreshed[b.Label] = obsReasonUnattested
		}
		if len(b.Names) > 0 {
			c.everListed[b.Label] = true
		}
	}

	flips := 0
	for name, label := range listedOn {
		old, had := byName[name]
		obs := old
		a, enriched := attrs[name]
		if !had || (enriched && old.Incarnation != "" && a.Incarnation != old.Incarnation) {
			// A new name or a new incarnation: nothing observed about the
			// previous runtime under this name carries over.
			obs = RuntimeObservation{SessionName: name, FirstListedAt: at}
		}
		obs.Backend = label
		obs.Listed = RuntimeFact{Value: ObsYes, ObservedAt: at, Source: SourcePoller}
		obs.LastListedAt = at
		if enriched {
			applyInventoryAttrs(&obs, a, at)
		}
		if observationChanged(old, obs, had) {
			flips++
		}
		byName[name] = obs
		delete(c.unlisted, name)
	}

	absent := RuntimeFact{Value: ObsNo, ObservedAt: at, Source: SourcePoller}
	for name, obs := range byName {
		if _, ok := listedOn[name]; ok {
			continue
		}
		old := obs
		switch {
		case pass.concludesAbsent(obs.Backend):
			obs.Listed, obs.Running, obs.ProcessAlive = absent, absent, absent
		case unrefreshed[obs.Backend] != "":
			obs.Listed.Reason = unrefreshed[obs.Backend]
		}
		if observationChanged(old, obs, true) {
			flips++
		}
		byName[name] = obs
		if failed[obs.Backend] {
			continue
		}
		c.unlisted[name]++
		if c.unlisted[name] >= observationRetentionPasses {
			delete(byName, name)
			delete(c.unlisted, name)
		}
	}

	primed := make(map[string]bool, len(pass.Backends))
	health := make(map[string]BackendHealth, len(pass.Backends))
	for _, b := range pass.Backends {
		primed[b.Label] = b.Outcome == OutcomeComplete || (!swapped && prev.Primed[b.Label])
		health[b.Label] = nextBackendHealth(prev.Health[b.Label], b, c.everListed[b.Label], at)
	}

	next := &ObservationSnapshot{
		Epoch:     c.epoch,
		Gen:       prev.Gen,
		GenAt:     prev.GenAt,
		PassSeq:   pass.Seq,
		At:        pass.FinishedAt,
		Inventory: pass,
		ByName:    byName,
		Primed:    primed,
		Health:    health,
	}
	c.store(next, flips > 0 || primedChanged(prev.Primed, primed), at)
	return flips
}

// primedChanged reports whether any backend's primed state differs, which
// changes what readers see even when no fact flipped.
func primedChanged(prev, next map[string]bool) bool {
	for label, p := range next {
		if prev[label] != p {
			return true
		}
	}
	for label, p := range prev {
		if p && !next[label] {
			return true
		}
	}
	return false
}

// applyInventoryAttrs records one name's enrichment and attribution.
func applyInventoryAttrs(obs *RuntimeObservation, a InventoryAttrs, at time.Time) {
	poller := func(v ObsFact) RuntimeFact { return RuntimeFact{Value: v, ObservedAt: at, Source: SourcePoller} }
	unsupported := RuntimeFact{Value: ObsUnknown, ObservedAt: at, Source: SourcePoller, Reason: obsReasonUnsupported}
	obs.Incarnation, obs.EnrichedAt = a.Incarnation, at
	obs.Running, obs.ProcessAlive = unsupported, unsupported
	if a.DeadKnown {
		obs.Running = poller(obsFactOf(!a.AllPanesDead))
		// A dead pane proves the process gone; a live pane says nothing about
		// the agent process inside it.
		if a.AllPanesDead {
			obs.ProcessAlive = poller(ObsNo)
		}
	}
	obs.Attached = unsupported
	if a.AttachedKnown {
		obs.Attached = poller(obsFactOf(a.Attached))
	}
	obs.OwnerState, obs.Owner, obs.InstanceToken = a.OwnerState, reconcilekey.Key{}, a.InstanceToken
	if a.OwnerState == OwnerSession {
		obs.Owner = reconcilekey.SessionRef(a.OwnerID, obs.SessionName)
	}
}

// observationChanged reports whether a fact value, the incarnation or the
// owner differs; a refresh of ObservedAt alone is not a change.
func observationChanged(old, next RuntimeObservation, had bool) bool {
	if !had {
		return true
	}
	for _, kind := range allFactKinds {
		if old.fact(kind).Value != next.fact(kind).Value {
			return true
		}
	}
	return old.Incarnation != next.Incarnation || old.OwnerState != next.OwnerState || old.Owner != next.Owner
}

// store publishes next, advancing Gen and signaling Changed when flipped.
// mu must be held.
func (c *ObservationCache) store(next *ObservationSnapshot, flipped bool, at time.Time) {
	if flipped {
		next.Gen++
		next.GenAt = at
	}
	c.cur.Store(next)
	if flipped {
		select {
		case c.changed <- struct{}{}:
		default:
		}
	}
}

// Note writes one fact from a fresh three-outcome probe. A write observed
// before the stored fact loses. It reports whether the fact's value flipped.
func (c *ObservationCache) Note(name string, kind FactKind, v ObsFact, at time.Time, src ObsSource, reason string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	prev := c.cur.Load()
	obs, ok := prev.ByName[name]
	if !ok {
		obs = RuntimeObservation{SessionName: name}
	}
	f := obs.fact(kind)
	if at.Before(f.ObservedAt) {
		return false
	}
	flipped := f.Value != v
	*f = RuntimeFact{Value: v, ObservedAt: at, Source: src, Reason: reason}

	next := *prev
	next.ByName = make(map[string]RuntimeObservation, len(prev.ByName)+1)
	for n, o := range prev.ByName {
		next.ByName[n] = o
	}
	next.ByName[name] = obs
	c.store(&next, flipped, at)
	return flipped
}

// Observation returns name's observation read at now: stale facts, and facts
// about names on unprimed backends, read ObsUnknown. Its Incarnation and owner
// fields are returned only for the incarnation listed now: Listed must read
// Yes, and the pass that last listed the name must also have enriched it.
// Otherwise they are cleared, so a reader never takes a stale, unprimed or
// previous incarnation's attribution as the current one.
func (s *ObservationSnapshot) Observation(name string, now time.Time, maxAge time.Duration) (RuntimeObservation, bool) {
	obs, ok := s.ByName[name]
	if !ok {
		return RuntimeObservation{}, false
	}
	for _, kind := range allFactKinds {
		f := obs.fact(kind)
		*f = s.readFact(obs.Backend, *f, now, maxAge)
	}
	if obs.Listed.Value != ObsYes || obs.EnrichedAt.IsZero() || !obs.EnrichedAt.Equal(obs.Listed.ObservedAt) {
		obs.Incarnation, obs.Owner, obs.OwnerState, obs.InstanceToken = "", reconcilekey.Key{}, OwnerUnknown, ""
	}
	return obs, true
}

// Get returns name's observation read at the cache's clock (Observation).
func (c *ObservationCache) Get(name string) (RuntimeObservation, bool) {
	return c.cur.Load().Observation(name, c.clock.Now(), c.maxAge)
}

// Snapshot returns the current immutable snapshot. Read facts through
// Snapshot.Fact to apply staleness and priming.
func (c *ObservationCache) Snapshot() *ObservationSnapshot {
	return c.cur.Load()
}

// FreshSnapshot returns the current snapshot if its pass finished within
// maxAge and its merged listing did not fail, so a reader gets the pass and
// the facts it published together. A caller that gets false lists live.
func (c *ObservationCache) FreshSnapshot(maxAge time.Duration) (*ObservationSnapshot, bool) {
	snap := c.cur.Load()
	pass := snap.Inventory
	if pass.FinishedAt.IsZero() || c.clock.Now().Sub(pass.FinishedAt) > maxAge || pass.mergedFailed() {
		return nil, false
	}
	return snap, true
}

// Changed returns a channel that receives after each Gen advance; bursts
// coalesce into one receive.
func (c *ObservationCache) Changed() <-chan struct{} {
	return c.changed
}
