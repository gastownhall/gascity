package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/api"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

// The step reminder through the controller (GET /work/active-step).
//
// Every prompt of a seat runs `gc nudge drain --inject`, and every session
// start `gc prime --hook`; both write the step reminder. Read in process it
// opens the rig store at GC_RIG_ROOT (on maintainer-city a native Dolt open,
// about 1-1.4 s of schema probes and preflight) and lists that rig by
// gc.routed_to once per identity (two scans, about 0.37 s each): about 2.2 s
// of a 3-4 s hook, for a reminder that is empty for every graph.v2 seat
// (ga-ylpbsy.6). The controller holds those stores open, so it answers the
// same resolution from them, live; the hook asks it as soon as it knows its
// city and reads in process on any failure, exactly as before.

var _ api.ActiveWorkStepReader = (*controllerState)(nil)

// ActiveWorkStep answers a session's step reminder (GET
// /v0/city/{cityName}/work/active-step) from the stores this controller holds
// open, with the resolution the hook runs in process (stepResolution).
//
// The stores are the hook's: the work store at scopeRoot (the city store, or
// the rig whose path it is) and the graph store the city's class routing gives
// that work store (resolveGraphStore over this controller's routes, which is
// what cliGraphStore gives a hook). Both are read through their live handles,
// so a CachingStore answers from its backing store and never from its cache:
// the answer is as fresh as the hook's own read, whoever wrote the beads. A
// scope this controller holds no store for is refused
// (api.ErrActiveWorkStepScopeNotHeld), and the hook opens it itself.
//
// It answers only under a config it can show is current, exactly as
// ReadyWork does (configFreshness), since the config decides which store a
// scope names. Ambiguities the hook would log to its stderr are not logged
// here: the hook that asked is the one that would have seen them.
func (cs *controllerState) ActiveWorkStep(ctx context.Context, assignees []string, scopeRoot string) (*api.ActiveWorkStep, error) {
	var step *beads.Bead
	var err error
	if !cs.freshness.read(cs.configDirty, func() {
		step, err = cs.activeWorkStep(ctx, assignees, scopeRoot)
	}) {
		return nil, api.ErrReadyWorkConfigNotCurrent
	}
	if err != nil {
		return nil, err
	}
	if step == nil {
		return nil, nil
	}
	return &api.ActiveWorkStep{ID: step.ID, Title: step.Title, Description: step.Description}, nil
}

// activeWorkStep resolves the step over the stores scopeRoot names, each bound
// to ctx where its store allows (activeWorkStepReader).
func (cs *controllerState) activeWorkStep(ctx context.Context, assignees []string, scopeRoot string) (*beads.Bead, error) {
	cs.mu.RLock()
	cfg := cs.cfg
	cityStore := cs.cityBeadStore
	rigStores := cs.beadStores // residency:allow — the hook's work store is the one store at its scope root; this picks it by path, as openWispStepStore opens it by path
	routes := cs.storageRoutes
	cityPath := cs.cityPath
	cs.mu.RUnlock()

	work := activeWorkStepScopeStore(cfg, cityPath, cityStore, rigStores, scopeRoot)
	if work == nil {
		return nil, fmt.Errorf("%w: %q", api.ErrActiveWorkStepScopeNotHeld, scopeRoot)
	}
	graph := resolveGraphStore(routes, work, cfg, cityPath, nil)
	// The one-listing question is decided on the stores, before any is bound:
	// a binding is a clone, and a clone is never the same handle as another.
	oneStore := sameStoreHandle(work, graph)
	workReader, err := activeWorkStepReader(ctx, cityPath, cfg, work)
	if err != nil {
		return nil, err
	}
	graphReader, err := activeWorkStepReader(ctx, cityPath, cfg, graph)
	if err != nil {
		return nil, err
	}
	r := stepResolution{work: workReader, graph: graphReader, logf: func(string, ...any) {}}
	return r.activeStep(oneStore, assignees)
}

// activeWorkStepScopeStore is the store openWispStepStore opens for scopeRoot,
// from the stores this controller holds: the city store for the city (an empty
// scope root is the city), the rig store for a rig's path, nil for any other
// path.
func activeWorkStepScopeStore(cfg *config.City, cityPath string, cityStore beads.Store, rigStores map[string]beads.Store, scopeRoot string) beads.Store {
	scope := resolveStoreScopeRoot(cityPath, scopeRoot)
	if samePath(scope, resolveStoreScopeRoot(cityPath, cityPath)) {
		return cityStore
	}
	if cfg == nil {
		return nil
	}
	for _, rig := range cfg.Rigs {
		if strings.TrimSpace(rig.Path) == "" {
			continue
		}
		if samePath(resolveStoreScopeRoot(cityPath, rig.Path), scope) {
			return rigStores[rig.Name]
		}
	}
	return nil
}

// activeWorkStepReader is store's live read handle, bound to ctx when the
// store runs subprocesses (scopedStoreLike), so a request the hook gave up on
// stops its bd children.
func activeWorkStepReader(ctx context.Context, cityPath string, cfg *config.City, store beads.Store) (activeStepReader, error) {
	scoped, err := scopedStoreLike(ctx, cityPath, cfg, store)
	if err != nil {
		return nil, fmt.Errorf("binding the store to the request: %w", err)
	}
	if scoped != nil {
		store = scoped
	}
	return beads.HandlesFor(store).Live, nil
}

// activeWorkStepAPITimeout bounds how long a hook waits for the controller's
// step reminder, from the moment it starts asking, before it reads the stores
// itself. The bound covers finding the controller as well as the request:
// resolving a supervisor-managed city's client pings its sockets and lists the
// supervisor's cities, outside any deadline of its own, and before this route
// existed the hooks never contacted the supervisor at all. The controller
// answers in tenths of a second (its slowest reads are the per-identity routed
// scans, which it runs at once); past this it is hung or overloaded, and the
// hook's own read (about 2 s on maintainer-city) still fits the hook's 15 s
// budget. A variable so tests can stand up a hung controller without waiting
// this long.
var activeWorkStepAPITimeout = 3 * time.Second

// activeWorkStepAPIClient resolves the controller a hook's step reminder asks,
// or nil with the reason it reads in process. A worker site dials no
// controller. Otherwise it is the routing `gc ready` uses for a read whose local
// path is expensive: a supervisor-managed city answers on the supervisor's port.
// A variable so tests can stand a controller up, or take it away.
var activeWorkStepAPIClient = func(cityPath string) (*api.Client, string) {
	if onWorkerSite(cityPath) {
		return nil, "worker-site"
	}
	return supervisorFallthroughAPIClient(cityPath)
}

// wispStepLookup is one hook invocation's step reminder: the controller is
// asked in the background from the moment the hook knows its city, so the
// request overlaps the hook's other work, and the answer is taken where the
// reminder is written (content). Any failure — no controller, an older one, a
// refusal, a failed read, no answer within activeWorkStepAPITimeout — reads the
// stores in process there, exactly as the hook always did. A lookup whose
// answer the hook does not need is stopped (stop).
//
// Neither taking the answer nor stopping waits past the bound. A request in
// flight is canceled; one still resolving its client, which cannot be
// canceled, is abandoned: its answer lands in a buffer nobody reads, and it
// ends with the hook's process.
type wispStepLookup struct {
	city   string
	stderr io.Writer
	cancel context.CancelFunc
	// answer carries the background request's one answer; its buffer lets an
	// abandoned request finish without a reader.
	answer chan wispStepAnswer
	// expired fires activeWorkStepAPITimeout after the lookup started.
	expired *time.Timer
	// taken is the answer once the caller has it.
	taken *wispStepAnswer
}

// wispStepAnswer is how a step reminder lookup ended: the controller's step
// (nil for none) when it answered, and the GC_DEBUG route line either way.
type wispStepAnswer struct {
	answered bool
	step     *api.ActiveWorkStep
	route    string
	reason   string
	err      error
}

// startWispStepLookup starts the step reminder lookup for cityPath (GC_CITY
// when empty) and the hook's identities (wispStepAssignees), against the work
// scope GC_RIG_ROOT names. stderr receives the GC_DEBUG route line, written
// from the caller's goroutine when the answer is taken.
func startWispStepLookup(cityPath string, stderr io.Writer) *wispStepLookup {
	l := &wispStepLookup{city: wispStepCityPath(cityPath), stderr: stderr}
	assignees := wispStepAssignees()
	if l.city == "" || len(assignees) == 0 {
		l.taken = &wispStepAnswer{route: "fallback", reason: "no-city-or-identity"}
		return l
	}
	// The seams are read here, on the caller's goroutine, so an abandoned
	// request never reads one a test has since restored.
	resolve, bound := activeWorkStepAPIClient, activeWorkStepAPITimeout
	ctx, cancel := context.WithTimeout(context.Background(), bound)
	l.cancel = cancel
	l.expired = time.NewTimer(bound)
	l.answer = make(chan wispStepAnswer, 1)
	city, scopeRoot := l.city, strings.TrimSpace(os.Getenv("GC_RIG_ROOT"))
	go func() {
		l.answer <- askActiveWorkStep(ctx, resolve, city, assignees, scopeRoot)
	}()
	return l
}

// askActiveWorkStep finds cityPath's controller and asks it for the step of
// assignees over the work scope at scopeRoot.
func askActiveWorkStep(ctx context.Context, resolve func(string) (*api.Client, string), cityPath string, assignees []string, scopeRoot string) wispStepAnswer {
	client, reason := resolve(cityPath)
	if client == nil {
		return wispStepAnswer{route: "fallback", reason: reason}
	}
	step, err := client.ActiveWorkStep(ctx, assignees, scopeRoot)
	if err != nil {
		return wispStepAnswer{route: "fallback", reason: "api-error", err: err}
	}
	return wispStepAnswer{answered: true, step: step, route: "api"}
}

// content is the step reminder: the controller's answer when it gave one, else
// the in-process read of the lookup's city.
func (l *wispStepLookup) content() string {
	answer := l.take()
	extra := []string(nil)
	if answer.err != nil {
		extra = []string{"error", strconv.Quote(answer.err.Error())}
	}
	logRoute(l.stderr, "step-reminder", answer.route, answer.reason, extra...)
	if answer.answered {
		if answer.step == nil {
			return ""
		}
		return formatWispStepReminder(&beads.Bead{ID: answer.step.ID, Title: answer.step.Title, Description: answer.step.Description})
	}
	return wispStepInjectionContentInProcess(l.city)
}

// answered reports whether the controller answered the lookup, step or none,
// waiting for the answer no longer than content does. It is false for a nil
// lookup.
func (l *wispStepLookup) answered() bool {
	return l != nil && l.take().answered
}

// contentFor is the step reminder for cityPath: the lookup's content when
// cityPath is the lookup's city, and a fresh wispStepInjectionContent
// otherwise (a nil lookup included), so a hook whose own resolution names a
// different city than its environment did reads the city it resolved.
func (l *wispStepLookup) contentFor(cityPath string) string {
	if l == nil || !samePath(wispStepCityPath(cityPath), l.city) {
		l.stop()
		return wispStepInjectionContent(cityPath)
	}
	return l.content()
}

// take returns the lookup's answer, waiting for it no longer than the bound
// that started with the lookup, and releases the request's context and timer.
func (l *wispStepLookup) take() wispStepAnswer {
	if l.taken != nil {
		return *l.taken
	}
	var answer wispStepAnswer
	select {
	case answer = <-l.answer:
	case <-l.expired.C:
		answer = wispStepAnswer{route: "fallback", reason: "timeout"}
	}
	l.expired.Stop()
	l.cancel()
	l.taken = &answer
	return answer
}

// stop ends a lookup whose answer is not needed: it cancels the request and
// takes what it ended with, within the same bound as content. It is safe on a
// nil lookup, after content, and more than once.
func (l *wispStepLookup) stop() {
	if l == nil {
		return
	}
	if l.cancel != nil {
		l.cancel()
	}
	l.take()
}
