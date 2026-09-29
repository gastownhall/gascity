package main

import (
	"errors"
	"os"
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

// doctorOpenStoppedStoresEnv, set to "1", turns the gate off so doctor opens
// and reads a stopped proxied store — and so starts its proxy and Dolt. It is
// an explicit opt-in for diagnosing the store-open path itself (the native
// lane's admission of a dead or absent proxy record is measured through the
// beads-store check); nothing sets it by default.
const doctorOpenStoppedStoresEnv = "GC_DOCTOR_OPEN_STOPPED_STORES"

// doctorProxiedStoreNotRunning is the per-scope liveness test. A variable so
// tests can model a stopped or running proxy without one.
var doctorProxiedStoreNotRunning = doctor.ProxiedStoreNotRunning

// doctorStoreGate answers, once per scope and per doctor run, whether a scope's
// bd-owned proxied store is stopped. Doctor must never start a server: on the
// proxied path any bd read of a stopped store starts its proxy and Dolt child,
// which gc-owned scopes keep up for good. So every store-reading check, and the
// bead-store preflight, asks the gate first and is replaced by a
// "not checked: store not running" line for a stopped scope.
type doctorStoreGate struct {
	mu       sync.Mutex
	stopped  map[string]bool
	disabled bool
}

func newDoctorStoreGate() *doctorStoreGate {
	return &doctorStoreGate{stopped: map[string]bool{}, disabled: os.Getenv(doctorOpenStoppedStoresEnv) == "1"}
}

// Stopped reports whether scopeRoot's proxied store is not running. Scopes
// that are not proxied are never stopped.
func (g *doctorStoreGate) Stopped(scopeRoot string) bool {
	if g.disabled {
		return false
	}
	key := normalizePathForCompare(scopeRoot)
	g.mu.Lock()
	defer g.mu.Unlock()
	if stopped, ok := g.stopped[key]; ok {
		return stopped
	}
	stopped := doctorProxiedStoreNotRunning(scopeRoot)
	g.stopped[key] = stopped
	return stopped
}

// Check returns c, or its not-running stand-in when any of scopes is stopped.
// labels name the scopes for the report, index for index.
func (g *doctorStoreGate) Check(c doctor.Check, scopes, labels []string) doctor.Check {
	var stopped []string
	for i, scope := range scopes {
		if g.Stopped(scope) {
			stopped = append(stopped, labels[i])
		}
	}
	if len(stopped) == 0 {
		return c
	}
	return doctor.StoreNotRunningCheck(c.Name(), stopped...)
}

// StoreFactory wraps open so a stopped scope answers errDoctorStoreNotRunning
// instead of being opened and read.
func (g *doctorStoreGate) StoreFactory(open func(string) (beads.Store, error)) func(string) (beads.Store, error) {
	return func(scopeRoot string) (beads.Store, error) {
		if g.Stopped(scopeRoot) {
			return nil, errDoctorStoreNotRunning
		}
		return open(scopeRoot)
	}
}

// OrderLastRun wraps an order-history lookup so an order whose store scope is
// stopped answers errDoctorStoreNotRunning instead of reading it.
func (g *doctorStoreGate) OrderLastRun(cityPath string, cfg *config.City, lastRun doctor.OrderFiringCurrentLastRunFunc) doctor.OrderFiringCurrentLastRunFunc {
	return func(order orders.Order) (time.Time, error) {
		if target, err := resolveOrderStoreTarget(cityPath, cfg, order); err == nil {
			if g.Stopped(target.ScopeRoot) || (legacyOrderCityFallbackNeeded(cityPath, target) && g.Stopped(cityPath)) {
				return time.Time{}, errDoctorStoreNotRunning
			}
		}
		return lastRun(order)
	}
}
