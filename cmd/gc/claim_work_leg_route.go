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
	frame func() ([]hookClaimWorkLeg, error)
	legs  []hookStore

	// home is the leg proved to hold each id.
	home map[string]hookStore

	// readLeg reads one bead through one leg's bd context. It is the
	// unrouted read of the layer below, so the route never recurses.
	readLeg func(ctx context.Context, dir string, env []string, beadID, assignee string) (beads.Bead, error)
}

// newHookClaimWorkLegRoute is the work-leg route for an agent whose claim is
// scoped to its primary leg. The frame is resolved on the first probe, so a
// claim the primary resolves pays nothing for it.
func newHookClaimWorkLegRoute(cityPath string, cfg *config.City, a *config.Agent, identityOverrides map[string]string) *hookClaimWorkLegRoute {
	return newHookClaimWorkLegRouteOver(func() ([]hookClaimWorkLeg, error) {
		return hookClaimWorkLegFrame(cityPath, cfg, a, identityOverrides)
	})
}

// newHookClaimWorkLegRouteOver builds the route over a frame resolver, with its
// memos live.
func newHookClaimWorkLegRouteOver(frame func() ([]hookClaimWorkLeg, error)) *hookClaimWorkLegRoute {
	return &hookClaimWorkLegRoute{
		frame: frame,
		home:  map[string]hookStore{},
	}
}

// hookClaimWorkLegFrame is the work legs a primary-scoped claim may find a
// bead the reader served in: every serving rig.
func hookClaimWorkLegFrame(cityPath string, cfg *config.City, a *config.Agent, identityOverrides map[string]string) ([]hookClaimWorkLeg, error) {
	serving, err := readyServingRigs(cfg, cityPath)
	if err != nil {
		return nil, err
	}
	var legs []hookClaimWorkLeg
	for _, rig := range serving {
		name := strings.TrimSpace(rig.Name)
		legs = append(legs, hookClaimWorkLeg{
			ref: "rig:" + name,
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
	if r.legs == nil {
		frame, err := r.frame()
		if err != nil {
			return hookStore{}, false
		}
		for _, leg := range frame {
			store, err := leg.build()
			if err != nil {
				continue
			}
			r.legs = append(r.legs, store)
		}
	}
	for _, leg := range r.legs {
		if _, err := r.readLeg(ctx, leg.dir, leg.env, id, assignee); err == nil {
			r.home[id] = leg
			return leg, true
		}
	}
	return hookStore{}, false
}

// workLegRoutedHookClaimOps returns ops whose claim-time ops fall back to the
// work leg that holds a bead the primary leg proved absent.
//
// A nil route returns ops UNCHANGED, so a claim off the primary-scoped branch
// runs the identical function values.
func workLegRoutedHookClaimOps(ops hookClaimOps, route *hookClaimWorkLegRoute) hookClaimOps {
	if route == nil {
		return ops
	}
	ops.applyDefaults()
	base := ops
	route.readLeg = base.ReadWorkMeta
	ops.WorkLegRoute = route

	ops.Claim = func(ctx context.Context, dir string, env []string, beadID, assignee string) (beads.Bead, bool, error) {
		claimed, ok, err := base.Claim(ctx, dir, env, beadID, assignee)
		if ok || !hookClaimBeadIsElsewhere(err) {
			return claimed, ok, err
		}
		home, found := route.probe(ctx, beadID, assignee)
		if !found {
			return claimed, ok, err
		}
		return base.Claim(ctx, home.dir, home.env, beadID, assignee)
	}

	ops.ReadWorkMeta = func(ctx context.Context, dir string, env []string, beadID, assignee string) (beads.Bead, error) {
		if home, ok := route.home[strings.TrimSpace(beadID)]; ok {
			return base.ReadWorkMeta(ctx, home.dir, home.env, beadID, assignee)
		}
		bead, err := base.ReadWorkMeta(ctx, dir, env, beadID, assignee)
		if !hookClaimBeadIsElsewhere(err) {
			return bead, err
		}
		home, found := route.probe(ctx, beadID, assignee)
		if !found {
			return bead, err
		}
		return base.ReadWorkMeta(ctx, home.dir, home.env, beadID, assignee)
	}

	return ops
}
