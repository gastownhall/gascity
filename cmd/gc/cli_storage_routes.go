package main

// One-shot CLI storage routing: the same binding the controller serves, or a
// store that refuses.
//
// storage_boot.go decides where a long-running controller serves each
// coordination class from. This file decides it for the other half of the
// program — `gc mail`, `gc nudge`, `gc sling`, `gc formula`, `gc prime`, the
// control dispatcher's one-shot arm — which open the work store themselves and
// never build a CityRuntime.
//
// Both tiers take the same verdict from the same gate, and that is the whole
// content of this file. A one-shot command reading the work store while the
// controller reads the binding is two ledgers for one city, and every symptom of
// it — mail sent and never delivered, a nudge queued into a store nothing polls,
// a graph the CLI cannot see — looks like a bug in the feature rather than in
// the routing.
//
// # Once per process
//
// The gate resolves a plan and opens a database. A one-shot command touches
// several class resolvers, so the resolution is memoized per city and the open
// happens at most once, whichever call site arrives first. The routes are closed
// where the process ends (main.go), which is the only place that knows the last
// call site has run.
//
// # It resolves the config itself
//
// The call sites disagree about what config they hold: some pass the city config
// their command loaded, one passes nil because it only ever had a path, and two
// pass whatever a load that ignores its own error returned. Where a city serves
// its coordination classes from is a property of the CITY, not of the caller's
// snapshot, so this file reads city.toml once and answers from that — otherwise
// the store a migrated city serves would depend on which call site got there
// first.
//
// # A refused city gets a store that refuses
//
// The verdict is three-valued exactly as it is at boot: serve, bypass, or
// refuse. What differs is how a refusal is DELIVERED, and the two obvious ways
// are both closed. A class resolver has no error to return — sixty-odd call
// sites could not carry one — and terminating the process is not this program's
// to do: the invocation lifecycle must keep control until run returns, which
// TestProductMetricsExitBypassCensus enforces against exactly four reviewed
// sites.
//
// So the refusal is delivered as the store. The five infrastructure classes
// route at refusedClassStore, whose every operation returns the boot refusal —
// the same sentence, from the same function, naming the same command — and the
// reason is printed once when the verdict is taken, so an operator sees it even
// on a path that discards the store error. Work is untouched: it stays on the
// work ledger, where a refused city's work still belongs.
//
// What this must never be is the work store. That is the answer that looks like
// success and reads the ledger the relocated class was moved off, and it is the
// defect this file exists to close.
//
// The command the refusal names is exempt, and structurally rather than by a
// carve-out: `gc storage migrate` and `gc storage status` resolve their own
// target and read it directly (cmd_storage.go), so they reach no class resolver
// and enter nothing here. That is what keeps the remedy runnable on exactly the
// city the refusal is about, and TestStorageStatusStaysOffTheOneShotFunnel
// fails if either one ever starts routing through a class instead.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/coordclass"
	"github.com/gastownhall/gascity/internal/fsys"
)

// cliStorageLogPrefix is what a one-shot command calls itself in a storage
// refusal. The controller says "gc start"; there is no single command name on
// this side, and the sentence that matters names the remedy rather than the
// caller.
const cliStorageLogPrefix = "gc"

// cliStorageStderr is where the one-shot gate's diagnostics and its refusal are
// written. It is the process's own stderr because a one-shot command's stderr is
// the process's; tests point it somewhere they can read.
var cliStorageStderr io.Writer = os.Stderr

// cliStorageRoutesEntry is one city's resolved answer, computed at most once.
type cliStorageRoutesEntry struct {
	once   sync.Once
	routes *storageRoutes
}

var (
	cliStorageRoutesMu     sync.Mutex
	cliStorageRoutesByCity map[string]*cliStorageRoutesEntry
)

// cliStorageRoutes returns the routes a one-shot CLI command must serve this
// city's relocated coordination classes from.
//
// Nil is the identity answer every city has today: no [storage] section, or one
// that leaves every class on the reserved work binding. It is what the class
// resolvers in class_store.go already take for granted, so a city that relocates
// nothing reaches none of the machinery below.
//
// A city this build cannot serve gets routes whose stores refuse. See
// refusedClassStore.
func cliStorageRoutes(cityPath string) *storageRoutes {
	if cityPath == "" {
		return nil
	}
	entry := cliStorageRoutesEntryFor(filepath.Clean(cityPath))
	entry.once.Do(func() { entry.routes = resolveCLIStorageRoutes(cityPath) })
	return entry.routes
}

// resolveCLIStorageRoutes takes the verdict for one city, exactly once, and
// turns each of its three arms into routes: nil for a city that relocates
// nothing, the opened binding for one that has converged, and refusing stores
// for one this build must not serve.
//
// The config load is the only thing this adds to a city that relocates nothing:
// storageBootGate owns the bypass, so a city with no [storage] section
// constructs no registry, resolves no plan and reads no byte of a binding root
// through this path either. The bypass does read the city's served-binding
// note — one failed open on a city that never served a split — which is what
// keeps deleting the [storage] section from being a way past every hold the
// gate has.
//
// A config that will not load is treated as a city with no [storage]. By the
// time a class resolver runs, the command's own load has already failed and
// reported it; a second complaint from here would be noise, and the alternative
// — refusing every command on a city whose config cannot be read — takes a city
// that is merely misconfigured and makes it unusable.
//
// The composition is read straight from config rather than through
// loadCityConfigWithoutBuiltinPackRefresh, whose extras are wrong for a routing
// read: it stomps the process-wide feature-flag globals from whatever city it
// was handed, and this runs underneath call sites a command may reach with a
// scope of its own. Reading where the classes live must not be able to change
// what the command does.
func resolveCLIStorageRoutes(cityPath string) *storageRoutes {
	cfg, _, err := config.LoadWithIncludes(fsys.OSFS{}, filepath.Join(cityPath, "city.toml"))
	if err != nil {
		return nil
	}
	routes, err := storageBootGate(cityPath, cfg, cliStorageLogPrefix, nil, cliStorageStderr)
	if err == nil {
		// The one and only place a class store is given an emit target. A
		// one-shot command has no live event bus, so without this its writes to
		// a relocated class land in a store nothing observes; the controller
		// resolves its routes through openStorageRoutes, never here, so its
		// side stays exactly as it was. See class_store_emit.go.
		return routes.withCLIEmission(cityPath)
	}
	// Once, here, rather than at each call site: a command that discards the
	// store error still has to leave the operator holding the remedy.
	fmt.Fprintln(cliStorageStderr, err) //nolint:errcheck // best-effort stderr
	_, binding := storageSplitShapeOf(cfg.EffectiveStorage())
	return refusingStorageRoutes(binding, err)
}

// cliStorageRoutesEntryFor returns the memo slot for one city, creating it under
// the lock and resolving it outside — an open that takes a database lock must
// not be performed holding a mutex every other call site is waiting on.
func cliStorageRoutesEntryFor(cityPath string) *cliStorageRoutesEntry {
	cliStorageRoutesMu.Lock()
	defer cliStorageRoutesMu.Unlock()
	if cliStorageRoutesByCity == nil {
		cliStorageRoutesByCity = make(map[string]*cliStorageRoutesEntry, 1)
	}
	entry, ok := cliStorageRoutesByCity[cityPath]
	if !ok {
		entry = &cliStorageRoutesEntry{}
		cliStorageRoutesByCity[cityPath] = entry
	}
	return entry
}

// closeCLIStorageRoutes releases every binding the one-shot funnel opened in this
// process. It is wired into the CLI's own exit path (main.go), which is the only
// place that knows the last call site has run.
//
// The memo is detached under the lock before anything is closed, so a second
// call closes nothing and a call that races the first cannot hand out a closed
// store: the next resolution starts from an empty memo and opens again.
//
// The DERIVED memo goes with it. cliResidencyBindings caches the class-to-store
// grouping it read out of these routes, so leaving it behind would let the next
// by-id resolution plan legs over stores this call just closed — the memo one
// layer up outliving the one it was derived from.
func closeCLIStorageRoutes() error {
	cliStorageRoutesMu.Lock()
	entries := cliStorageRoutesByCity
	cliStorageRoutesByCity = nil
	cliStorageRoutesMu.Unlock()
	resetCLIResidencyBindings()

	var errs []error
	for _, entry := range entries {
		// A resolution still in flight owns a store this close would otherwise
		// leak: sync.Once.Do returns only after the resolving call has returned,
		// so waiting on it here is what makes the opened routes visible.
		entry.once.Do(func() {})
		if err := entry.routes.close(); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// refusingStorageRoutes routes every infrastructure class at a store that
// refuses, leaving work where it is.
//
// It relocates the same set the supported split relocates — everything that is
// not work — rather than the set the config happens to name, because a refused
// city includes the arrangements this build cannot even read a class map out of.
// Under-refusing there would leave a class silently on the work store, which is
// the exact failure the refusal exists to prevent.
func refusingStorageRoutes(binding string, refusal error) *storageRoutes {
	store := refusedClassStore{err: standingStorageRefusal{err: refusal}}
	routes := &storageRoutes{stores: make(map[coordclass.Class]beads.Store), binding: binding}
	for _, class := range coordclass.Classes() {
		if class.IsInfrastructure() {
			routes.stores[class] = store
		}
	}
	return routes
}

// standingStorageRefusal marks an error as the verdict this build took about a
// CITY, rather than a fault a particular read ran into. The message is the
// refusal verbatim — wrapping adds no prefix — and the only thing the wrapper
// adds is that the two can be told apart.
//
// They are told apart because a caller can act on the difference. A refused
// city still serves its WORK beads from its work ledger, which is the whole
// reason this file leaves work unrouted; so a caller holding a work id has been
// told nothing about that id and must keep its existing path, while a caller
// holding an id only a relocated class could own has been told the one thing
// that matters. Answering both the same way turns a storage misconfiguration
// into a city where no `gc bd` write runs at all.
type standingStorageRefusal struct{ err error }

func (e standingStorageRefusal) Error() string { return e.err.Error() }
func (e standingStorageRefusal) Unwrap() error { return e.err }

// The predicate over this type is storeref.IsStandingRefusal, which matches on
// the StandingStorageRefusal() marker (declared in residency_topology.go)
// instead of on this concrete type. cmd/gc had its own errors.As spelling until
// the claim route stopped needing one: with the last production caller collapsed
// onto the resolver, a second predicate could only drift from the one the
// resolver's leg policy actually consults.

// refusedClassStore is the store a relocated class resolves to on a city this
// build must not serve: every operation fails with the refusal that says why and
// names the command that fixes it.
//
// It is deliberately a whole beads.Store and not a nil one. A nil store panics
// at the first read, and the panic names a line of Go rather than the city's
// storage state; this answers every call site — the ones that report the error
// and the ones that only stop — with the operator's own remedy.
//
// The optional capabilities below are implemented for the same reason: a caller
// that type-asserts one and finds it missing takes a fallback path, and every
// extra path is another chance to end up somewhere that does not refuse. Every
// method here, required or optional, returns the same refusal.
type refusedClassStore struct{ err error }

func (s refusedClassStore) Create(beads.Bead) (beads.Bead, error) { return beads.Bead{}, s.err }
func (s refusedClassStore) Get(string) (beads.Bead, error)        { return beads.Bead{}, s.err }
func (s refusedClassStore) Update(string, beads.UpdateOpts) error { return s.err }
func (s refusedClassStore) Close(string) error                    { return s.err }
func (s refusedClassStore) Reopen(string) error                   { return s.err }
func (s refusedClassStore) CloseAll([]string, map[string]string) (int, error) {
	return 0, s.err
}
func (s refusedClassStore) List(beads.ListQuery) ([]beads.Bead, error) { return nil, s.err }
func (s refusedClassStore) ListOpen(...string) ([]beads.Bead, error)   { return nil, s.err }
func (s refusedClassStore) Ready(...beads.ReadyQuery) ([]beads.Bead, error) {
	return nil, s.err
}

func (s refusedClassStore) Children(string, ...beads.QueryOpt) ([]beads.Bead, error) {
	return nil, s.err
}

func (s refusedClassStore) ListByLabel(string, int, ...beads.QueryOpt) ([]beads.Bead, error) {
	return nil, s.err
}

func (s refusedClassStore) ListByAssignee(string, string, int) ([]beads.Bead, error) {
	return nil, s.err
}

func (s refusedClassStore) ListByMetadata(map[string]string, int, ...beads.QueryOpt) ([]beads.Bead, error) {
	return nil, s.err
}
func (s refusedClassStore) SetMetadata(string, string, string) error { return s.err }
func (s refusedClassStore) SetMetadataBatch(string, map[string]string) error {
	return s.err
}

// Claim and ReleaseIfCurrent are the conditional-assignment pair. They are
// here for the reason the whole optional set is: the by-ID class front door
// discovers them by type assertion, and a refusing store that lacked them
// would answer "this store cannot claim" — a statement about a capability —
// where the truth is that this city must not be served at all.
func (s refusedClassStore) Claim(string, string) (beads.Bead, bool, error) { //nolint:unparam // a refusing store returns the refusal and never a value
	return beads.Bead{}, false, s.err
}

func (s refusedClassStore) ReleaseIfCurrent(string, string) (bool, error) { //nolint:unparam // a refusing store returns the refusal and never a value
	return false, s.err
}

func (s refusedClassStore) SetLocalString(string, string, string) error { return s.err }
func (s refusedClassStore) GetLocalString(string, string) (string, error) {
	return "", s.err
}
func (s refusedClassStore) Tx(string, func(beads.Tx) error) error { return s.err }
func (s refusedClassStore) Delete(string) error                   { return s.err }
func (s refusedClassStore) Ping() error                           { return s.err }
func (s refusedClassStore) DepAdd(string, string, string) error   { return s.err }
func (s refusedClassStore) DepRemove(string, string) error        { return s.err }
func (s refusedClassStore) DepList(string, string) ([]beads.Dep, error) {
	return nil, s.err
}

func (s refusedClassStore) ReadyContext(context.Context, ...beads.ReadyQuery) ([]beads.Bead, error) { //nolint:unparam // a refusing store returns the refusal and never a value
	return nil, s.err
}

func (s refusedClassStore) CreateWithStorage(beads.Bead, beads.StorageClass) (beads.Bead, error) { //nolint:unparam // a refusing store returns the refusal and never a value
	return beads.Bead{}, s.err
}

func (s refusedClassStore) ApplyGraphPlanWithStorage(context.Context, *beads.GraphApplyPlan, beads.StorageClass) (*beads.GraphApplyResult, error) { //nolint:unparam // a refusing store returns the refusal and never a value
	return nil, s.err
}

func (s refusedClassStore) WaitForParentProjection(context.Context, string, string, string) error {
	return s.err
}
