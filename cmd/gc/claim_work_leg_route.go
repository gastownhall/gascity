package main

// Claim-time write routing across the WORK legs of a federated claim.
//
// On a city whose generated work query reads through the federated `gc ready`,
// `gc hook --claim` builds and claims through ONE leg: the agent's primary
// (hookStoresScopeToPrimary, hookWorkQueryPrimaryStore). The reader that leg
// runs serves every work leg's beads — the city work store and every serving
// rig — so a candidate another work leg holds reaches a claim that is a bd
// subprocess rooted in the primary's workspace, which cannot resolve it. This
// file is the route that closes that: once the primary PROVES a work-shaped id
// absent, the other work legs are read for it, in the reader's own order, and
// the op re-runs in the first leg that holds it.
//
// It sits on the hookClaimOps seam UNDER the class route
// (claim_class_route.go), so the relocated binding is still consulted last:
// the class route's not-found escalation only sees an absence the whole work
// axis has proved.

import (
	"context"
	"strings"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

// hookClaimWorkLeg is one work leg of the route's frame: the store it names
// and how to build the bd context that reaches it.
type hookClaimWorkLeg struct {
	// ref names the leg ("city:<name>" or "rig:<name>"), for diagnostics.
	ref   string
	build func() (hookStore, error)
}

// hookClaimWorkLegRoute routes a claim-time op for a bead the primary leg
// proved absent to the work leg that holds it.
//
// home and nowhere are per-invocation memos, like the class route's: one
// `gc hook --claim` is one process, and the claim path is sequential.
type hookClaimWorkLegRoute struct {
	// frame lists the work legs other than the primary, in the reader's order.
	// It is resolved once, on the first probe; each leg's bd context is built
	// on the first probe that reaches it (built), so a probe pays only for the
	// legs up to the holder.
	frame    func() ([]hookClaimWorkLeg, error)
	legs     []hookClaimWorkLeg
	framed   bool
	built    map[string]hookStore
	unusable map[string]bool

	// reserved reports an id inside a relocated class's reserved namespace,
	// which the class route answers for and this route never probes; nil
	// when the city relocates no class.
	reserved func(id string) bool

	// home is the leg proved to hold each id.
	home map[string]hookStore

	// readLeg reads one bead through one leg's bd context. It is the
	// unrouted read of the layer below, so the route never recurses.
	readLeg func(ctx context.Context, dir string, env []string, beadID, assignee string) (beads.Bead, error)
}

// newHookClaimWorkLegRoute is the work-leg route for an agent whose claim is
// scoped to the primary leg primaryRef names (primaryHookStoreRef). The frame
// is resolved on the first probe, so a claim the primary resolves pays nothing
// for it.
func newHookClaimWorkLegRoute(cityPath string, cfg *config.City, a *config.Agent, primaryRef string, identityOverrides map[string]string) *hookClaimWorkLegRoute {
	return newHookClaimWorkLegRouteOver(func() ([]hookClaimWorkLeg, error) {
		return hookClaimWorkLegFrame(cityPath, cfg, a, primaryRef, identityOverrides)
	})
}

// newHookClaimWorkLegRouteOver builds the route over a frame resolver, with its
// memos live.
func newHookClaimWorkLegRouteOver(frame func() ([]hookClaimWorkLeg, error)) *hookClaimWorkLegRoute {
	return &hookClaimWorkLegRoute{
		frame:    frame,
		built:    map[string]hookStore{},
		unusable: map[string]bool{},
		home:     map[string]hookStore{},
	}
}

// hookClaimWorkLegFrame is the work legs a primary-scoped claim may find a
// bead the reader served in: the city work store, then every serving rig. The
// leg primaryRef names is the primary itself, which has already answered, so
// it is left out; the class binding is the class route's to consult.
func hookClaimWorkLegFrame(cityPath string, cfg *config.City, a *config.Agent, primaryRef string, identityOverrides map[string]string) ([]hookClaimWorkLeg, error) {
	serving, err := readyServingRigs(cfg, cityPath)
	if err != nil {
		return nil, err
	}
	var legs []hookClaimWorkLeg
	cityRef := "city:" + hookStoreCityName(cityPath, loadedCityName(cfg, cityPath))
	if cityRef != primaryRef {
		legs = append(legs, hookClaimWorkLeg{
			ref: cityRef,
			build: func() (hookStore, error) {
				return cityHookStore(cityPath, cfg, a, identityOverrides)
			},
		})
	}
	for _, rig := range serving {
		name := strings.TrimSpace(rig.Name)
		ref := "rig:" + name
		if ref == primaryRef {
			continue
		}
		legs = append(legs, hookClaimWorkLeg{
			ref: ref,
			build: func() (hookStore, error) {
				return rigHookStore(cityPath, cfg, a, name, identityOverrides)
			},
		})
	}
	return legs, nil
}

// probe reads id through each work leg of the frame in order and returns the
// first that holds it, memoized in home. found=false means no leg could be
// shown to hold it.
func (r *hookClaimWorkLegRoute) probe(ctx context.Context, id, assignee string) (hookStore, bool) {
	id = strings.TrimSpace(id)
	if home, ok := r.home[id]; ok {
		return home, true
	}
	if !r.framed {
		frame, err := r.frame()
		if err != nil {
			return hookStore{}, false
		}
		r.legs, r.framed = frame, true
	}
	for _, leg := range r.legs {
		store, ok := r.leg(leg)
		if !ok {
			continue
		}
		if _, err := r.readLeg(ctx, store.dir, store.env, id, assignee); err == nil {
			r.home[id] = store
			return store, true
		}
	}
	return hookStore{}, false
}

// leg is the bd context that reaches leg, built on first use.
func (r *hookClaimWorkLegRoute) leg(leg hookClaimWorkLeg) (hookStore, bool) {
	if store, ok := r.built[leg.ref]; ok {
		return store, true
	}
	if r.unusable[leg.ref] {
		return hookStore{}, false
	}
	store, err := leg.build()
	if err != nil {
		r.unusable[leg.ref] = true
		return hookStore{}, false
	}
	r.built[leg.ref] = store
	return store, true
}

// homeOf is the leg the route proved holds id, if it did.
func (r *hookClaimWorkLegRoute) homeOf(id string) (hookStore, bool) {
	if r == nil {
		return hookStore{}, false
	}
	home, ok := r.home[strings.TrimSpace(id)]
	return home, ok
}

// at is the dir and env an op on id runs with: its home leg's once the route
// proved one, else the caller's.
func (r *hookClaimWorkLegRoute) at(dir string, env []string, id string) (string, []string) {
	if home, ok := r.homeOf(id); ok {
		return home.dir, home.env
	}
	return dir, env
}

// elsewhere is the leg to re-run an op on id in after the primary answered it
// with err: the work leg that holds id, when err proves the primary lacks it
// and a probe finds it. Once a home is known the op already ran there, so it
// is never re-run. An id in a relocated class's reserved namespace is the
// class route's to find, so it is never probed.
func (r *hookClaimWorkLegRoute) elsewhere(ctx context.Context, id, assignee string, err error) (hookStore, bool) {
	if _, known := r.homeOf(id); known || !hookClaimBeadIsElsewhere(err) {
		return hookStore{}, false
	}
	if r.reserved != nil && r.reserved(strings.TrimSpace(id)) {
		return hookStore{}, false
	}
	return r.probe(ctx, id, assignee)
}

// workLegRoutedHookClaimOps returns ops whose claim-time ops fall back to the
// work leg that holds a bead the primary leg proved absent.
//
// classesRelocated says the city relocates a class (hookClaimClassesRelocated),
// so ids in the reserved class namespaces are left to the class route.
//
// A nil route returns ops UNCHANGED, so a claim off the primary-scoped branch
// runs the identical function values.
func workLegRoutedHookClaimOps(ops hookClaimOps, route *hookClaimWorkLegRoute, classesRelocated bool) hookClaimOps {
	if route == nil {
		return ops
	}
	ops.applyDefaults()
	base := ops
	route.readLeg = base.ReadWorkMeta
	if classesRelocated {
		route.reserved = bdIDIsClassReserved
	}
	ops.WorkLegRoute = route

	// The ops that resolve a bead by id: the home leg once known, else the
	// primary first and the holding work leg on its proven absence. A claim
	// the primary made (ok) is its answer, whatever error rides with it.
	ops.Claim = func(ctx context.Context, dir string, env []string, beadID, assignee string) (beads.Bead, bool, error) {
		dir, env = route.at(dir, env, beadID)
		claimed, ok, err := base.Claim(ctx, dir, env, beadID, assignee)
		if ok {
			return claimed, ok, err
		}
		if home, found := route.elsewhere(ctx, beadID, assignee, err); found {
			return base.Claim(ctx, home.dir, home.env, beadID, assignee)
		}
		return claimed, ok, err
	}

	ops.ReadWorkMeta = func(ctx context.Context, dir string, env []string, beadID, assignee string) (beads.Bead, error) {
		dir, env = route.at(dir, env, beadID)
		bead, err := base.ReadWorkMeta(ctx, dir, env, beadID, assignee)
		if home, found := route.elsewhere(ctx, beadID, assignee, err); found {
			return base.ReadWorkMeta(ctx, home.dir, home.env, beadID, assignee)
		}
		return bead, err
	}

	ops.StampWorkMeta = func(ctx context.Context, dir string, env []string, beadID, assignee string, patch map[string]string) error {
		dir, env = route.at(dir, env, beadID)
		err := base.StampWorkMeta(ctx, dir, env, beadID, assignee, patch)
		if home, found := route.elsewhere(ctx, beadID, assignee, err); found {
			return base.StampWorkMeta(ctx, home.dir, home.env, beadID, assignee, patch)
		}
		return err
	}

	ops.AssignContinuation = func(ctx context.Context, dir string, env []string, beadID, assignee string) error {
		dir, env = route.at(dir, env, beadID)
		err := base.AssignContinuation(ctx, dir, env, beadID, assignee)
		if home, found := route.elsewhere(ctx, beadID, "", err); found {
			return base.AssignContinuation(ctx, home.dir, home.env, beadID, assignee)
		}
		return err
	}

	ops.ReclaimStale = func(ctx context.Context, dir string, env []string, beadID string) (bool, string, error) {
		dir, env = route.at(dir, env, beadID)
		reclaimed, previousOwner, err := base.ReclaimStale(ctx, dir, env, beadID)
		if home, found := route.elsewhere(ctx, beadID, "", err); found {
			return base.ReclaimStale(ctx, home.dir, home.env, beadID)
		}
		return reclaimed, previousOwner, err
	}

	// The ops that only ever act on a claim already made: the home leg, or
	// the caller's dir unchanged. They never probe; the claim they act on
	// proved where the bead lives. WorkflowStore is not among them: a seam
	// that hands back a raw beads.Store is storeclass debt, so it keeps the
	// caller's dir.
	ops.Release = func(ctx context.Context, dir string, env []string, beadID, assignee string) (bool, error) {
		dir, env = route.at(dir, env, beadID)
		return base.Release(ctx, dir, env, beadID, assignee)
	}

	ops.RestampAdopted = func(ctx context.Context, dir string, env []string, beadID, fromAssignee, toAssignee string) (bool, error) {
		dir, env = route.at(dir, env, beadID)
		return base.RestampAdopted(ctx, dir, env, beadID, fromAssignee, toAssignee)
	}

	ops.EmitExecutionStepStarted = func(step beads.Bead, dir string, env []string, assignee string) {
		dir, env = route.at(dir, env, step.ID)
		base.EmitExecutionStepStarted(step, dir, env, assignee)
	}

	// The ops keyed by a workflow root: the root's home leg once the route
	// proved one, so a continuation and its root are read where they live.
	ops.ListContinuation = func(ctx context.Context, dir string, env []string, rootID, group string) ([]beads.Bead, error) {
		dir, env = route.at(dir, env, rootID)
		return base.ListContinuation(ctx, dir, env, rootID, group)
	}

	ops.InputVerdict = func(ctx context.Context, dir string, env []string, candidate beads.Bead, identityCandidates []string, prove workflowInputAbsenceProver) (workflowInputVerdict, error) {
		dir, env = route.at(dir, env, workflowRootIDForInputCheck(candidate))
		return base.InputVerdict(ctx, dir, env, candidate, identityCandidates, prove)
	}

	ops.RetireWorkflow = func(ctx context.Context, dir string, env []string, verdict workflowInputVerdict) error {
		dir, env = route.at(dir, env, verdict.RootID)
		return base.RetireWorkflow(ctx, dir, env, verdict)
	}

	return ops
}
