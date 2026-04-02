package main

import (
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gastownhall/gascity/internal/api"
)

// cityView is a read-only projection of managedCity, built at snapshot time.
// API handlers receive *cityView and may read any field without synchronization.
// The controllerState pointer is safe to hold because controllerState has its
// own internal RWMutex for field-level safety.
type cityView struct {
	Name    string
	Path    string
	Started bool
	Status  string

	// controllerState is a pointer to the city's api.State implementation.
	// It is thread-safe via its own internal RWMutex.
	cs api.State

	// Tombstoned is set when the city is being torn down. Copied from
	// managedCity.tombstoned at snapshot build time.
	Tombstoned bool

	// Init/backoff status (nil if not applicable).
	InitProgress *cityInitProgress
	InitFailure  *initFailRecord
	PanicRecord  *panicRecord
}

// citySnapshot is an immutable point-in-time view of all cities.
// Rebuilt on every mutation, read lock-free via atomic.Pointer.
type citySnapshot struct {
	byName  map[string]*cityView // O(1) lookup by name
	byPath  map[string]*cityView // O(1) lookup by path
	all     []*cityView          // for ListCities iteration
	gen     uint64               // monotonic generation counter
	builtAt time.Time            // for staleness instrumentation
}

// cityRegistry owns the mutable cities map and the atomic snapshot.
// All mutation methods acquire citiesMu, mutate, rebuild, and release.
// The snapshot rebuild is always called while citiesMu is held —
// there is no TOCTOU gap between mutation and publication.
type cityRegistry struct {
	citiesMu sync.Mutex              // protects cities map only; never held by API readers
	cities   map[string]*managedCity // keyed by path
	snap     atomic.Pointer[citySnapshot]

	// init/backoff state (co-protected by citiesMu)
	initStatus   map[string]cityInitProgress
	initFailures map[string]*initFailRecord
	panicHistory map[string]*panicRecord

	gen uint64 // monotonic generation counter
}

// newCityRegistry creates a registry initialized with an empty snapshot.
func newCityRegistry() *cityRegistry {
	r := &cityRegistry{
		cities:       make(map[string]*managedCity),
		initStatus:   make(map[string]cityInitProgress),
		initFailures: make(map[string]*initFailRecord),
		panicHistory: make(map[string]*panicRecord),
	}
	// Initialize with empty snapshot to prevent nil-dereference panic
	// if an API request arrives before the first reconciliation tick.
	r.snap.Store(&citySnapshot{
		byName:  make(map[string]*cityView),
		byPath:  make(map[string]*cityView),
		all:     make([]*cityView, 0),
		gen:     0,
		builtAt: time.Now(),
	})
	return r
}

// Add inserts or replaces a city. Caller must not hold citiesMu.
func (r *cityRegistry) Add(path string, mc *managedCity) {
	r.citiesMu.Lock()
	defer r.citiesMu.Unlock()
	r.cities[path] = mc
	r.rebuildSnapshotLocked()
}

// Remove deletes a city and purges its init/backoff state.
// Caller must not hold citiesMu.
func (r *cityRegistry) Remove(path string) {
	r.citiesMu.Lock()
	defer r.citiesMu.Unlock()
	delete(r.cities, path)
	delete(r.initStatus, path)
	delete(r.initFailures, path)
	delete(r.panicHistory, path)
	r.rebuildSnapshotLocked()
}

// Has returns true if a city exists at the given path.
func (r *cityRegistry) Has(path string) bool {
	r.citiesMu.Lock()
	defer r.citiesMu.Unlock()
	_, ok := r.cities[path]
	return ok
}

// CancelCity calls cancel() on the city at path if it exists.
// Used by shutdown paths that need the cancel func without
// exposing the mutable *managedCity pointer. Returns the done
// channel for waiting, or nil if the city doesn't exist.
func (r *cityRegistry) CancelCity(path string) <-chan struct{} {
	r.citiesMu.Lock()
	defer r.citiesMu.Unlock()
	mc, ok := r.cities[path]
	if !ok {
		return nil
	}
	mc.cancel()
	return mc.done
}

// ReadCallback acquires citiesMu for read access without rebuilding
// the snapshot. Use for read-only operations (gathering lists,
// checking backoff state). For mutations, use BatchUpdate instead.
func (r *cityRegistry) ReadCallback(fn func(
	cities map[string]*managedCity,
	initStatus map[string]cityInitProgress,
	initFailures map[string]*initFailRecord,
	panicHistory map[string]*panicRecord,
),
) {
	r.citiesMu.Lock()
	defer r.citiesMu.Unlock()
	fn(r.cities, r.initStatus, r.initFailures, r.panicHistory)
}

// UpdateCallback is called by city goroutines when a field changes.
// It acquires citiesMu, applies the mutation via fn, and rebuilds.
// fn runs under citiesMu — it must not call any cityRegistry method
// (citiesMu is not reentrant).
func (r *cityRegistry) UpdateCallback(path string, fn func(mc *managedCity)) {
	r.citiesMu.Lock()
	defer r.citiesMu.Unlock()
	if mc, ok := r.cities[path]; ok {
		fn(mc)
		r.rebuildSnapshotLocked()
	}
}

// BatchUpdate acquires citiesMu once, calls fn (which may mutate
// multiple cities or the init/backoff maps), and rebuilds the snapshot
// once at the end. fn receives the internal maps for direct mutation.
func (r *cityRegistry) BatchUpdate(fn func(
	cities map[string]*managedCity,
	initStatus map[string]cityInitProgress,
	initFailures map[string]*initFailRecord,
	panicHistory map[string]*panicRecord,
),
) {
	r.citiesMu.Lock()
	defer r.citiesMu.Unlock()
	fn(r.cities, r.initStatus, r.initFailures, r.panicHistory)
	r.rebuildSnapshotLocked()
}

// Snapshot returns the current read-only snapshot. Lock-free.
func (r *cityRegistry) Snapshot() *citySnapshot {
	return r.snap.Load()
}

// CityState returns the api.State for a named city, or nil if not found/not running.
// Lock-free read from the atomic snapshot.
func (r *cityRegistry) CityState(name string) api.State {
	snap := r.snap.Load()
	view, ok := snap.byName[name]
	if !ok {
		return nil
	}
	if view.Tombstoned {
		return nil
	}
	if !view.Started || view.cs == nil {
		return nil
	}
	return view.cs
}

// ListCities returns info about all managed cities. Lock-free read from
// the atomic snapshot. All cities (running, initializing, and failed) are
// included in snap.all by rebuildSnapshotLocked.
func (r *cityRegistry) ListCities() []api.CityInfo {
	snap := r.snap.Load()
	out := make([]api.CityInfo, 0, len(snap.all))
	for _, v := range snap.all {
		ci := api.CityInfo{
			Name:    v.Name,
			Path:    v.Path,
			Running: v.Started,
			Status:  v.Status,
		}
		// Running cities report empty status (matches old behavior).
		if v.Started {
			ci.Status = ""
		}
		// Compute completed phases from current status for startup progress.
		if !v.Started && v.Status != "" {
			ci.PhasesCompleted = phasesCompletedBefore(v.Status)
		}
		// Init-failure cities include the error message.
		if v.InitFailure != nil {
			ci.Error = v.InitFailure.lastError
		}
		out = append(out, ci)
	}
	return out
}

// startupPhaseOrder is the ordered list of startup phases.
var startupPhaseOrder = []string{
	"loading_config",
	"starting_bead_store",
	"resolving_formulas",
	"adopting_sessions",
	"starting_agents",
}

// phasesCompletedBefore returns all phases that come before the given current phase.
func phasesCompletedBefore(current string) []string {
	for i, p := range startupPhaseOrder {
		if p == current {
			if i == 0 {
				return nil
			}
			return startupPhaseOrder[:i]
		}
	}
	return nil
}

// rebuildSnapshotLocked rebuilds the atomic snapshot from current state.
// PRECONDITION: caller holds citiesMu. Must not re-acquire citiesMu.
func (r *cityRegistry) rebuildSnapshotLocked() {
	r.gen++

	// Count total entries: cities + init-only + failure-only entries.
	totalEstimate := len(r.cities) + len(r.initStatus) + len(r.initFailures)

	snap := &citySnapshot{
		byName:  make(map[string]*cityView, totalEstimate),
		byPath:  make(map[string]*cityView, totalEstimate),
		all:     make([]*cityView, 0, totalEstimate),
		gen:     r.gen,
		builtAt: time.Now(),
	}

	// Build views for cities in the main map.
	for path, mc := range r.cities {
		view := r.toCityView(path, mc)
		snap.byPath[path] = view
		snap.byName[view.Name] = view
		snap.all = append(snap.all, view)
	}

	// Build views for init-in-progress cities not yet in the main map.
	for path, ip := range r.initStatus {
		if _, exists := snap.byPath[path]; exists {
			continue
		}
		ipCopy := ip
		view := &cityView{
			Name:         ip.name,
			Path:         path,
			Status:       ip.status,
			InitProgress: &ipCopy,
		}
		snap.byPath[path] = view
		snap.byName[view.Name] = view
		snap.all = append(snap.all, view)
	}

	// Build views for init-failure cities not yet in the main map.
	// These are NOT added to byName because their names are derived from
	// filepath.Base(path) which is not guaranteed unique and could collide
	// with running cities that use effective names from the registry.
	// They appear in snap.all (for ListCities) and snap.byPath only.
	for path, rec := range r.initFailures {
		if _, exists := snap.byPath[path]; exists {
			continue
		}
		recCopy := *rec
		view := &cityView{
			Name:        filepath.Base(path),
			Path:        path,
			Status:      "init_failed",
			InitFailure: &recCopy,
		}
		snap.byPath[path] = view
		// Deliberately NOT added to byName — see comment above.
		snap.all = append(snap.all, view)
	}

	r.snap.Store(snap)
}

// toCityView deep-copies a managedCity into an immutable cityView.
// Called under citiesMu — safe to read all managedCity fields.
func (r *cityRegistry) toCityView(path string, mc *managedCity) *cityView {
	// SAFETY: cs is a pointer to controllerState, which has its own internal
	// RWMutex protecting all field access. API handlers that receive this pointer
	// call methods like Config(), SessionProvider(), etc. which acquire cs.mu.RLock().
	// The Poke() method only does a non-blocking channel send — no managedCity access.
	var cs api.State
	if mc.cr != nil {
		cs = mc.cr.cs
	}

	v := &cityView{
		Name:       mc.name,
		Path:       path,
		Started:    mc.started,
		Status:     mc.status,
		cs:         cs,
		Tombstoned: mc.tombstoned.Load(),
	}

	if ip, ok := r.initStatus[path]; ok {
		cp := ip
		v.InitProgress = &cp
	}
	if f, ok := r.initFailures[path]; ok {
		cp := *f
		v.InitFailure = &cp
	}
	if p, ok := r.panicHistory[path]; ok {
		cp := *p
		v.PanicRecord = &cp
	}

	return v
}
