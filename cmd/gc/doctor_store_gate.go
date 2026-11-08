package main

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/doctor"
	"github.com/gastownhall/gascity/internal/orders"
)

// errDoctorStoreNotRunning is what a store open or order lookup answers for a
// scope whose bd-owned proxied store is stopped. Checks that walk several
// scopes already report a failed open as "<scope> skipped: ...", so the
// message carries the reason.
var errDoctorStoreNotRunning = errors.New(doctor.StoreNotRunningMessage)

// errDoctorStoreSuspended is errDoctorStoreNotRunning for a scope doctor
// leaves cold because its rig or city is suspended.
var errDoctorStoreSuspended = errors.New(doctor.StoreSuspendedMessage)

// errDoctorStoreStale is what a store open answers for a scope whose managed
// Dolt publication is stale.
var errDoctorStoreStale = errors.New(doctor.StoreStaleMessage)

// Per-scope liveness seams, variables so tests can model a proxied scope, a
// stopped or running proxy, and an idle policy without real ones.
var (
	doctorProxiedStoreNotRunning      = doctor.ProxiedStoreNotRunning
	doctorProxiedStoreScope           = doctor.ProxiedStoreScope
	doctorProxiedStoreRetiresWhenIdle = doctor.ProxiedStoreRetiresWhenIdle
)

// doctorScopeState is what the gate decided for one scope.
type doctorScopeState int

const (
	// doctorScopeReadable: read the store normally. A live proxy, a scope that
	// is not proxied, and an idle-retired scope under a running city.
	doctorScopeReadable doctorScopeState = iota
	// doctorScopeStopped: the proxy is down and doctor must not start it.
	doctorScopeStopped
	// doctorScopeSuspended: the scope's rig or city is suspended; leave it cold.
	doctorScopeSuspended
	// doctorScopeStale: the scope's managed Dolt publication is stale (a dead
	// or foreign pid, a mismatched identity); doctor must not read or repair it.
	doctorScopeStale
)

// doctorStoreGate answers, once per scope and per doctor run, whether doctor
// may read a scope's bd-owned proxied store. On the proxied path any bd read
// of a stopped store starts its proxy and Dolt child, so every store-reading
// check, and the bead-store preflight, asks the gate first:
//
//   - a suspended rig or city is never woken ("not checked: suspended", OK);
//   - on a stopped city a stopped store is never started ("not checked: store
//     not running", OK);
//   - on a running city a store whose proxy retired on its idle timeout is read,
//     which restarts it for at most one more idle period; a never-idle store
//     with no proxy is a fault and its stand-in warns.
type doctorStoreGate struct {
	mu            sync.Mutex
	state         map[string]doctorScopeState
	expectRunning bool
	suspended     func(scopeRoot string) bool
	// unavailable holds the typed verdict of a scope the gate classified as
	// stopped or stale from the managed publication, for the stand-in detail.
	unavailable map[string]*storeUnavailableError
	// cityPath and cfg name the city whose managed publication classifies a
	// non-proxied scope. Empty cityPath derives it from the scope root.
	cityPath string
	cfg      *config.City
}

// ForCity binds the gate to the city (and its loaded config, which may be nil)
// whose managed Dolt publication classifies a scope. It returns g for chaining.
func (g *doctorStoreGate) ForCity(cityPath string, cfg *config.City) *doctorStoreGate {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.cityPath, g.cfg = cityPath, cfg
	return g
}

// newDoctorStoreGate builds the gate for one doctor run. expectRunning is
// whether the city's controller is up. suspended reports whether a scope
// belongs to a suspended rig or city; nil means none is.
func newDoctorStoreGate(expectRunning bool, suspended func(scopeRoot string) bool) *doctorStoreGate {
	return &doctorStoreGate{state: map[string]doctorScopeState{}, unavailable: map[string]*storeUnavailableError{}, expectRunning: expectRunning, suspended: suspended}
}

// State reports what doctor may do with scopeRoot's store.
func (g *doctorStoreGate) State(scopeRoot string) doctorScopeState {
	key := normalizePathForCompare(scopeRoot)
	g.mu.Lock()
	defer g.mu.Unlock()
	if state, ok := g.state[key]; ok {
		return state
	}
	state := g.decide(scopeRoot)
	g.state[key] = state
	return state
}

// managedVerdict classifies a scope with no proxied store against the managed
// Dolt publication, read-only. It returns nil for an available scope and for a
// verdict that is not a stopped or stale store (a classification error is left
// to the check's own read).
func (g *doctorStoreGate) managedVerdict(scopeRoot string) *storeUnavailableError {
	cityPath := g.cityPath
	if cityPath == "" {
		cityPath = cityForStoreDir(scopeRoot)
	}
	label := observationScopeLabel(cityPath, scopeRoot, g.cfg)
	unavailable, ok := asStoreUnavailable(classifyObservationScope(cityPath, scopeRoot, label))
	if !ok {
		return nil
	}
	return unavailable
}

func (g *doctorStoreGate) decide(scopeRoot string) doctorScopeState {
	if g.suspended != nil && g.suspended(scopeRoot) && doctorProxiedStoreScope(scopeRoot) {
		return doctorScopeSuspended
	}
	if doctorProxiedStoreNotRunning(scopeRoot) {
		if g.expectRunning && doctorProxiedStoreRetiresWhenIdle(scopeRoot) {
			return doctorScopeReadable
		}
		return doctorScopeStopped
	}
	if !doctorProxiedStoreScope(scopeRoot) {
		if verdict := g.managedVerdict(scopeRoot); verdict != nil {
			g.unavailable[normalizePathForCompare(scopeRoot)] = verdict
			if verdict.State == storeStateStopped {
				return doctorScopeStopped
			}
			return doctorScopeStale
		}
	}
	return doctorScopeReadable
}

// Skipped reports whether doctor must not read scopeRoot's store.
func (g *doctorStoreGate) Skipped(scopeRoot string) bool {
	return g.State(scopeRoot) != doctorScopeReadable
}

// Check returns c, or its stand-in when any of scopes is skipped. labels name
// the scopes for the report, index for index.
func (g *doctorStoreGate) Check(c doctor.Check, scopes, labels []string) doctor.Check {
	return g.checkOr(c, c.Name(), scopes, labels)
}

// StandIn is the stand-in for a check named name over scopes, or nil when
// every scope is readable.
func (g *doctorStoreGate) StandIn(name string, scopes, labels []string) doctor.Check {
	return g.checkOr(nil, name, scopes, labels)
}

func (g *doctorStoreGate) checkOr(c doctor.Check, name string, scopes, labels []string) doctor.Check {
	var stopped, suspended, stale, staleDetails []string
	for i, scope := range scopes {
		switch g.State(scope) {
		case doctorScopeStopped:
			stopped = append(stopped, labels[i])
		case doctorScopeSuspended:
			suspended = append(suspended, labels[i])
		case doctorScopeStale:
			stale = append(stale, labels[i])
			if verdict := g.verdict(scope); verdict != nil {
				staleDetails = append(staleDetails, verdict.Error())
			}
		}
	}
	switch {
	case len(stale) > 0:
		return doctor.StoreStaleCheck(name, strings.Join(staleDetails, "; "), stale...)
	case len(stopped) > 0:
		return doctor.StoreNotRunningCheck(name, g.expectRunning, stopped...)
	case len(suspended) > 0:
		return doctor.StoreSuspendedCheck(name, suspended...)
	}
	return c
}

// StoreFactory wraps open so a skipped scope answers errDoctorStoreNotRunning
// or errDoctorStoreSuspended instead of being opened and read.
func (g *doctorStoreGate) StoreFactory(open func(string) (beads.Store, error)) func(string) (beads.Store, error) {
	return func(scopeRoot string) (beads.Store, error) {
		if err := g.skipErr(scopeRoot); err != nil {
			return nil, err
		}
		return open(scopeRoot)
	}
}

func (g *doctorStoreGate) skipErr(scopeRoot string) error {
	switch g.State(scopeRoot) {
	case doctorScopeStopped:
		if verdict := g.verdict(scopeRoot); verdict != nil {
			return fmt.Errorf("%w (%w)", errDoctorStoreNotRunning, verdict)
		}
		return errDoctorStoreNotRunning
	case doctorScopeStale:
		if verdict := g.verdict(scopeRoot); verdict != nil {
			return fmt.Errorf("%w (%w)", errDoctorStoreStale, verdict)
		}
		return errDoctorStoreStale
	case doctorScopeSuspended:
		return errDoctorStoreSuspended
	}
	return nil
}

// verdict returns the typed managed-publication verdict recorded for a scope
// the gate classified as stopped or stale, or nil for a proxied scope.
func (g *doctorStoreGate) verdict(scopeRoot string) *storeUnavailableError {
	g.State(scopeRoot)
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.unavailable[normalizePathForCompare(scopeRoot)]
}

// OrderLastRun wraps an order-history lookup so an order whose store scope is
// skipped answers the skip error instead of reading it.
func (g *doctorStoreGate) OrderLastRun(cityPath string, cfg *config.City, lastRun doctor.OrderFiringCurrentLastRunFunc) doctor.OrderFiringCurrentLastRunFunc {
	return func(order orders.Order) (time.Time, error) {
		if target, err := resolveOrderStoreTarget(cityPath, cfg, order); err == nil {
			if err := g.skipErr(target.ScopeRoot); err != nil {
				return time.Time{}, err
			}
			if legacyOrderCityFallbackNeeded(cityPath, target) {
				if err := g.skipErr(cityPath); err != nil {
					return time.Time{}, err
				}
			}
		}
		return lastRun(order)
	}
}
