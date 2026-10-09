package main

import (
	"context"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/proxyendpoint"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/doctor"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

// Suspension is quiescence. A suspended rig, and every scope of a suspended
// city, gets no bd call from the controller: any bd read restarts a bd-owned
// proxied scope's proxy and Dolt child, so a periodic read would keep a
// suspended scope warm for good. Once the scope's sessions have drained, the
// controller stops its pair with the same lifecycle "stop" op (`bd dolt
// stop`) gc stop runs last; resuming lets the next read restart it.
//
// For a suspended rig the skips live where each periodic reader picks its
// stores (rig caches, demand, order tracking, completions, convergence, the
// maintenance orders). For a suspended city with no running session the tick
// stops before its first phase, the order lane and the completions sweep stand
// down, and every cache pauses its reconcile (controllerState.beadsQuiescent).

// suspendedScopeRetirements remembers which suspended scopes' pairs this
// controller already stopped in the current suspension episode, so a scope is
// stopped once per suspension rather than on every tick. An episode is named
// by the suspension state's write time: any suspend or resume starts a new one,
// even one no tick observed in between, and the next tick stops every
// still-suspended scope again (`bd dolt stop` on a stopped scope exits 0). It
// is in-memory only: a restarted controller stops a still-suspended scope
// again, which is equally harmless.
type suspendedScopeRetirements struct {
	mu      sync.Mutex
	retired map[string]time.Time
}

func (r *suspendedScopeRetirements) done(root string, epoch time.Time) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	at, ok := r.retired[normalizePathForCompare(root)]
	return ok && at.Equal(epoch)
}

func (r *suspendedScopeRetirements) mark(root string, epoch time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.retired == nil {
		r.retired = map[string]time.Time{}
	}
	r.retired[normalizePathForCompare(root)] = epoch
}

// forgetResumed drops every scope that is no longer suspended.
func (r *suspendedScopeRetirements) forgetResumed(suspended beadsScopeSuspension) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for root := range r.retired {
		if !suspended.Suspended(root) {
			delete(r.retired, root)
		}
	}
}

// setBeadsQuiescent records whether the city is quiescent, for the tick and
// for every cache the controller state owns.
func (cr *CityRuntime) setBeadsQuiescent(quiescent bool) {
	cr.beadsQuiescent.Store(quiescent)
	cr.cs.setBeadsQuiescent(quiescent)
}

// enterBeadsQuiescenceIfDue decides, at the top of a tick, whether the city is
// quiescent: suspended, with no session running in its runtime provider. A
// quiescent city stops every suspended scope's pair it has not stopped yet and
// reports true, and the tick then runs no phase at all. The decision reads the
// suspension state file and the runtime provider's session list, never a bead
// store.
func (cr *CityRuntime) enterBeadsQuiescenceIfDue(ctx context.Context) bool {
	suspended := suspendedBeadsScopes(cr.cityPath, cr.cfg)
	cr.retiredScopes.forgetResumed(suspended)
	cr.repairResumedScopes(suspended)
	cr.cs.setSuspendedRigs(buildEffectiveSuspendedRigNames(cr.cfg, loadSuspensionStateBestEffort(cr.cityPath)))
	quiescent := false
	if suspended.City() {
		running, err := cr.sp.ListRunning("")
		quiescent = err == nil && len(running) == 0
	}
	cr.setBeadsQuiescent(quiescent)
	if !quiescent {
		return false
	}
	cr.retireSuspendedScopes(ctx, suspended, func(string) bool { return true })
	return true
}

// resumeRepairBlockedFlags and resumeRepairCandidates are the resume path's
// calls into gc start's one-shot is_blocked repair, seams so tests can pin
// which scopes it is made for.
var (
	resumeRepairBlockedFlags = runBlockedRepairForScopes
	resumeRepairCandidates   = blockedRepairScopes
)

// repairResumedScopes runs gc start's one-shot is_blocked repair
// (beads_blocked_repair.go) for every scope that was suspended on the previous
// tick and no longer is. gc start skipped those scopes to leave them cold; the
// repair is marker-gated, so a scope it already covered costs one probe. It
// runs at the top of the tick, before any phase starts the resumed scope's
// agents.
func (cr *CityRuntime) repairResumedScopes(suspended beadsScopeSuspension) {
	prev := cr.lastSuspension
	cr.lastSuspension = &suspended
	if prev == nil || cr.cfg == nil || gcDoltSkip() {
		return
	}
	var resumed []blockedRepairScope
	for _, scope := range resumeRepairCandidates(cr.cityPath, cr.cfg) {
		if prev.Suspended(scope.root) && !suspended.Suspended(scope.root) {
			resumed = append(resumed, scope)
		}
	}
	if len(resumed) > 0 {
		resumeRepairBlockedFlags(cr.cityPath, cr.cfg, resumed, cr.stderr, "gc resume")
	}
}

// tickRetireSuspendedRigScopes stops the pair of each suspended rig whose
// sessions have drained. It runs after the session reconcile, which is what
// stops a suspended rig's sessions. The city scope is left to
// enterBeadsQuiescenceIfDue: it is retired only once nothing runs at all.
func (cr *CityRuntime) tickRetireSuspendedRigScopes(p *tickPass) bool {
	suspended := suspendedBeadsScopes(cr.cityPath, cr.cfg)
	cr.retiredScopes.forgetResumed(suspended)
	if !suspended.anyRig() {
		return false
	}
	running, err := cr.sp.ListRunning("")
	if err != nil {
		return false
	}
	runningNames := make(map[string]bool, len(running))
	for _, name := range running {
		runningNames[name] = true
	}
	open := p.sessionBeads.OpenInfos()
	drained := func(scopeRoot string) bool {
		for _, info := range open {
			if runningNames[info.SessionName] && sessionInScope(cr.cityPath, cr.cfg, info, scopeRoot) {
				return false
			}
		}
		return true
	}
	cr.retireSuspendedScopes(p.ctx, suspended.withoutCity(), drained)
	return false
}

// suspendedScopePair reports whether scopeRoot is a bd-owned proxied scope
// and, if so, whether its proxy is running right now. It reads the proxy record
// and the process table; nothing is dialed. A variable so tests can model a
// running or stopped pair.
var suspendedScopePair = func(scopeRoot string) (proxied, live bool) {
	if !doctor.ProxiedStoreScope(scopeRoot) {
		return false, false
	}
	return true, !doctor.ProxiedStoreNotRunning(scopeRoot)
}

// retireSuspendedScopes stops the bd-owned pair of every suspended scope whose
// sessions have been drained for a whole tick, so the drain's own bead
// bookkeeping is done before the pair goes away.
//
// It converges on live state rather than remembering what it did: a proxied
// scope's pair is stopped whenever it is found running, so a late touch that
// restarted it (a straggler write, an event-driven order) is undone on the
// next tick instead of leaking a never-idle pair for good. A scope that is not
// proxied has no proxy to observe and is stopped once per suspension episode.
// A scope that is not provider-owned has no bd-owned pair. A rig that shares
// the city's proxy root is left alone unless the city is suspended too:
// stopping it would stop the pair serving the city and every other rig.
func (cr *CityRuntime) retireSuspendedScopes(ctx context.Context, suspended beadsScopeSuspension, drained func(scopeRoot string) bool) {
	for _, root := range cr.beadsScopeRoots() {
		key := normalizePathForCompare(root)
		if !suspended.Suspended(root) {
			delete(cr.drainedLastTick, key)
			continue
		}
		drainedNow := drained(root)
		settled := drainedNow && cr.drainedLastTick[key]
		if cr.drainedLastTick == nil {
			cr.drainedLastTick = map[string]bool{}
		}
		cr.drainedLastTick[key] = drainedNow
		if !settled {
			continue
		}
		owned, err := scopeProviderOwned(cr.cityPath, root)
		if err != nil {
			fmt.Fprintf(cr.stderr, "suspended scope %s: %v\n", root, err) //nolint:errcheck // best-effort stderr
			continue
		}
		if !owned {
			continue
		}
		if !samePath(root, cr.cityPath) && !suspended.City() && proxyendpoint.SharesCityRoot(cr.cityPath, root) {
			continue
		}
		proxied, live := suspendedScopePair(root)
		switch {
		case proxied && !live:
			continue
		case !proxied && cr.retiredScopes.done(root, suspended.epoch):
			continue
		}
		if err := runProviderOwnedScopeLifecycleOpContext(ctx, cr.cityPath, root, "stop"); err != nil {
			fmt.Fprintf(cr.stderr, "suspended scope %s: stopping its bd proxy: %v\n", root, err) //nolint:errcheck // best-effort stderr
		}
		cr.retiredScopes.mark(root, suspended.epoch)
	}
}

// beadsScopeRoots lists the city root and every configured rig root.
func (cr *CityRuntime) beadsScopeRoots() []string {
	roots := []string{cr.cityPath}
	if cr.cfg == nil {
		return roots
	}
	for _, rig := range cr.cfg.Rigs {
		if strings.TrimSpace(rig.Path) == "" {
			continue
		}
		path := rig.Path
		if !filepath.IsAbs(path) {
			path = filepath.Join(cr.cityPath, path)
		}
		roots = append(roots, path)
	}
	return roots
}

// sessionInScope reports whether a session belongs to the rig at scopeRoot:
// its agent is configured in that rig, or it works under the rig's directory.
func sessionInScope(cityPath string, cfg *config.City, info sessionpkg.Info, scopeRoot string) bool {
	if cfg != nil {
		for i := range cfg.Agents {
			agent := &cfg.Agents[i]
			if agent.QualifiedName() != info.Template {
				continue
			}
			if rigName := configuredRigName(cityPath, agent, cfg.Rigs); rigName != "" {
				root := rigRootForName(rigName, cfg.Rigs)
				if !filepath.IsAbs(root) {
					root = filepath.Join(cityPath, root)
				}
				if samePath(root, scopeRoot) {
					return true
				}
			}
			break
		}
	}
	if info.WorkDir == "" {
		return false
	}
	rel, err := filepath.Rel(normalizePathForCompare(scopeRoot), normalizePathForCompare(info.WorkDir))
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// withoutSuspendedRigs is stores without the suspended rigs, for periodic
// readers that must leave a suspended rig's store untouched.
//
// residency:allow — filters the caller's own rig map by suspension, as
// servingRigStores does; it consults no binding, namespace or leg order.
func withoutSuspendedRigs(cityPath string, cfg *config.City, stores map[string]beads.Store) map[string]beads.Store {
	suspended := buildEffectiveSuspendedRigNames(cfg, loadSuspensionStateBestEffort(cityPath))
	if len(suspended) == 0 {
		return stores
	}
	kept := make(map[string]beads.Store, len(stores))
	for name, store := range stores {
		if !suspended[name] {
			kept[name] = store
		}
	}
	return kept
}

// retireSuspendedScopesWithoutController stops a just-suspended scope's
// bd-owned proxy and Dolt when no controller is running for the city. With a
// controller running, the controller does it once the scope's sessions have
// drained (tickRetireSuspendedRigScopes, enterBeadsQuiescenceIfDue); without
// one nothing runs in the city, so there is nothing to wait for. city stops
// every provider-owned scope of the city, as gc stop's last step does;
// otherwise each of roots is stopped, except a rig that shares the city's
// proxy root, whose pair also serves the city.
func retireSuspendedScopesWithoutController(cityPath string, roots []string, city bool, stderr io.Writer) {
	if controllerAlive(cityPath) != 0 {
		return
	}
	if city {
		if err := runProviderOwnedScopesLifecycleOp(cityPath, "stop"); err != nil {
			fmt.Fprintf(stderr, "stopping the suspended city's bd proxies: %v\n", err) //nolint:errcheck // best-effort stderr
		}
		return
	}
	for _, root := range roots {
		owned, err := scopeProviderOwned(cityPath, root)
		if err != nil {
			fmt.Fprintf(stderr, "suspended scope %s: %v\n", root, err) //nolint:errcheck // best-effort stderr
			continue
		}
		if !owned || proxyendpoint.SharesCityRoot(cityPath, root) {
			continue
		}
		if err := runProviderOwnedScopeLifecycleOpContext(context.Background(), cityPath, root, "stop"); err != nil {
			fmt.Fprintf(stderr, "stopping the suspended rig's bd proxy: %v\n", err) //nolint:errcheck // best-effort stderr
		}
	}
}
