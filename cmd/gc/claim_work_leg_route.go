package main

// The work-leg claim route: the claim-time half of the read federation
// `gc ready` and the hook's own work query already run.
//
// A federated work query reads every work store from the agent's primary leg,
// so that leg can SERVE a bead it does not itself hold: a rig store's bead,
// routed to a city-scoped agent, arrives as a ready candidate while the claim
// — a bd subprocess rooted in the primary leg's workspace — answers "bead not
// found" for it. The candidate then drains claims_errored on every turn, which
// is issue #9159 and the I15 asymmetry hook_cross_store.go pinned. The class
// route (claim_class_route.go) closes the same asymmetry for the relocated
// coordination-class binding; this route closes it for the WORK axis: once the
// primary write proves the id absent there, the route reads the id in the
// reader's OTHER work legs — the city work store, then the serving rigs by name
// ascending, `gc ready`'s leg order — and the first leg that holds the bead
// becomes its HOME: claim-time writes and the work_started key follow the home
// leg, through the same leg builders the fan-out read already uses, so the
// claim runs as the bd workspace that actually holds the bead.
//
// The route sits UNDER the class route and OVER the bead-events wrapper
// (base → withHookClaimBeadEvents → work-leg route → class route), so a routed
// write is still announced, and the binding stays the LAST resort: a
// work-shaped id no work leg holds escalates exactly as it did before, and
// reserved-class namespaces bypass this route entirely because the binding is
// the authority for them.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"sort"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

// errHookClaimWorkLegUnreadable is the sentinel a probe returns when a work
// leg's READ failed in a way that leaves the bead's residence there unknown:
// not a not-found (which would prove absence) but a store that did not answer.
// It names the leg. Wrapping it into the claim error tells the tiers the
// candidate's residence is UNRESOLVED — nothing was written anywhere — so the
// claim loop skips to the next candidate rather than failing the invocation,
// and legProvedAbsent treats it as an answer, never re-reading the same leg.
var errHookClaimWorkLegUnreadable = errors.New("work leg unreadable")

// hookClaimReadLeg is the read seam a proven-absence question is settled
// through: one bead by id, against one leg's dir and env. It is the ops'
// ReadWorkMeta of the layer BELOW whatever is asking, so a route's probe and
// the caller's own reads agree about what "this leg holds" means.
type hookClaimReadLeg func(ctx context.Context, dir string, env []string, beadID, assignee string) (beads.Bead, error)

// hookClaimLegProvedAbsent reports whether from, the leg a claim-time write for
// id ran against, is proved NOT to hold the bead, given the error the write
// returned. Shared by the class route's escalation and the work-leg route's
// probe: both open only on proof, and the same errors close both.
//
// The write's own not-found is the proof. Any other error is settled by
// reading the same leg through readLeg: a not-found there proves it, and a read
// that finds the bead or fails proves nothing. Four errors skip the read
// because they already answer it: a named wisp refusal is the leg saying it
// HOLDS the bead; a store that did not answer (workQueryStoreUnavailable: a
// timeout, a refused connection, contention) would only be asked again while
// it is struggling; a context that is done cannot carry a read that would mean
// anything; and a probe that already reported an unreadable LEG of this route
// settled the same question for the same reason — the residence is unknown,
// not absent, and re-reading the struggling leg cannot change that.
func hookClaimLegProvedAbsent(ctx context.Context, readLeg hookClaimReadLeg, from hookStore, id, assignee string, workErr error) bool {
	switch {
	case workErr == nil:
		return false
	case hookClaimBeadIsElsewhere(workErr):
		return true
	case readLeg == nil, hookClaimBeadIsAWisp(workErr), ctx.Err() != nil, workQueryStoreUnavailable(workErr), errors.Is(workErr, errHookClaimWorkLegUnreadable):
		return false
	}
	_, err := readLeg(ctx, from.dir, from.env, id, assignee)
	return hookClaimBeadIsElsewhere(err)
}

// workLegFrameEntry is one candidate leg of the probe frame. The dir is known
// when the frame is built — it is the rig's or the city's directory, resolved
// from config — so the primary's own leg can be identified WITHOUT building
// any environment. The env is built lazily by build, at most once, and only
// when a probe actually reads this leg: a claim whose candidate the primary
// holds, or whose id is in a reserved class namespace, never pays for one.
type workLegFrameEntry struct {
	label string
	dir   string
	build func() (hookStore, bool)
	built bool
	ok    bool
	leg   hookStore
}

// hookClaimWorkLegRoute is the work-axis claim route for one invocation.
//
// The frame is the reader's leg order with the primary's own leg removed. home
// memoizes, per bead id, the frame leg a probe found holding it; nowhere
// memoizes the ids every frame leg proved absent, so a later op for the same
// id returns the primary's own answer instead of re-walking the frame.
type hookClaimWorkLegRoute struct {
	primary hookStore
	frame   []workLegFrameEntry
	home    map[string]hookStore
	nowhere map[string]bool
	readLeg hookClaimReadLeg
}

// probe walks the frame in reader order, reading id through the layer below's
// read seam, and returns the first leg that holds it. The first holder is the
// copy the federated reader serves, which is the copy a claim must own. A leg
// that reads not-found is proven absent and the walk continues; a leg whose
// read fails any other way is an UNREADABLE leg: the walk stops, nothing is
// written, and the sentinel names the leg and the read error. A leg whose
// environment cannot be built is skipped, the same best-effort skip the
// fan-out's own leg builder applies.
func (r *hookClaimWorkLegRoute) probe(ctx context.Context, id, assignee string) (hookStore, bool, error) {
	if held, ok := r.home[id]; ok {
		return held, true, nil
	}
	if r.nowhere[id] {
		return hookStore{}, false, nil
	}
	for i := range r.frame {
		entry := &r.frame[i]
		if !entry.built {
			entry.leg, entry.ok = entry.build()
			entry.built = true
		}
		if !entry.ok {
			continue
		}
		_, err := r.readLeg(ctx, entry.leg.dir, entry.leg.env, id, assignee)
		switch {
		case err == nil:
			r.home[id] = entry.leg
			return entry.leg, true, nil
		case errors.Is(err, beads.ErrNotFound):
			continue
		default:
			return hookStore{}, false, fmt.Errorf("%w: leg %q: %w", errHookClaimWorkLegUnreadable, entry.label, err)
		}
	}
	r.nowhere[id] = true
	return hookStore{}, false, nil
}

// hookClaimWorkLegRouteForInvocation builds the route for one claim
// invocation, or nil when the invocation has no work-leg route to run.
//
// The route exists only for a claim scoped to its PRIMARY leg
// (hookStoresScopeToPrimary): the federated query then reads every work store
// through stores[0], so a candidate may live in a store the claim write cannot
// otherwise reach. Every other shape — the full fan-out, whose legs each claim
// on their own turns; a custom work_query, which never scoped; a city with no
// other work legs — is returned as nil, and the invocation runs the same ops
// it runs today. A store set of any other length than one is therefore nil by
// construction: the scoped branch hands claimHookWorkProvisioning exactly one
// store, and every unscoped branch hands it the fan-out.
//
// The primary's own leg is removed from the frame by agent scope when the
// agent resolves from config — the city leg for a city-scoped agent, its own
// rig for a rig-scoped one, matching hookWorkQueryStoreFanOut — and by
// matching the leg's dir against the primary's when it does not (a claim whose
// session carries no configured agent, as a direct programmatic caller's may).
// Serving rigs follow the reader's suspension rule (readyServingRigs): a
// suspended rig's store is not probed, exactly as it is not read.
func hookClaimWorkLegRouteForInvocation(cityPath string, cfg *config.City, queryEnv []string, stores []hookStore, stderr io.Writer) *hookClaimWorkLegRoute {
	if len(stores) != 1 || cfg == nil {
		return nil
	}
	rigs, err := readyServingRigs(cfg, cityPath)
	if err != nil {
		// The reader fails loud on an unreadable suspension state and so does
		// the hook's own front door; a claim that got this far will not be
		// failed by the route built beside it. Degrade to the unrouted claim,
		// saying why.
		fmt.Fprintf(stderr, "gc hook --claim: no work-leg route: %v\n", err) //nolint:errcheck // best-effort stderr
		return nil
	}
	primary := stores[0]
	route := &hookClaimWorkLegRoute{
		primary: primary,
		home:    map[string]hookStore{},
		nowhere: map[string]bool{},
	}
	overrides := map[string]string{}
	for _, key := range hookIdentityEnvKeys {
		if v := hookClaimEnvValue(queryEnv, key); v != "" {
			overrides[key] = v
		}
	}
	agentForQuery := hookClaimEnvValue(queryEnv, "GC_AGENT")
	view, resolved := workLegRouteAgentView(cfg, queryEnv)
	// The frame in reader order: the city work store first, then the serving
	// rigs by name ascending. A rig named like the city is the reader's own
	// dedupe rule and is dropped here too.
	cityName := hookStoreCityName(cityPath, loadedCityName(cfg, cityPath))
	dropCity, dropRig := false, ""
	if resolved {
		if agentIsCrossStoreEligible(view) {
			dropCity = true
		} else {
			dropRig = rigScopedHookRig(cfg, agentForQuery)
		}
	}
	type pending struct {
		label   string
		rigName string
	}
	var frame []pending
	if !dropCity {
		frame = append(frame, pending{label: "city"})
	}
	names := make([]string, 0, len(rigs))
	for _, rig := range rigs {
		if rig.Name == cityName {
			continue
		}
		names = append(names, rig.Name)
	}
	sort.Strings(names)
	for _, name := range names {
		if name == dropRig {
			continue
		}
		frame = append(frame, pending{label: "rig " + name, rigName: name})
	}
	// A claim whose session carries no configured agent (a direct programmatic
	// caller's may not) still knows its primary by dir: the primary's dir is
	// resolved from the same config the frame's dirs resolve from, so the two
	// spell one store the same way, and the leg that IS the primary drops.
	if !resolved {
		kept := frame[:0]
		for _, p := range frame {
			if filepath.Clean(workLegFrameDir(cityPath, cfg, p.rigName)) == filepath.Clean(primary.dir) {
				continue
			}
			kept = append(kept, p)
		}
		frame = kept
	}
	for _, p := range frame {
		p := p
		rigName := p.rigName
		route.frame = append(route.frame, workLegFrameEntry{
			label: p.label,
			dir:   workLegFrameDir(cityPath, cfg, rigName),
			build: func() (hookStore, bool) {
				var legs []hookStore
				if rigName == "" {
					legs = appendCityHookStore(nil, cityPath, cfg, view, overrides)
				} else {
					legs = appendOneRigHookStore(nil, cityPath, cfg, view, rigName, overrides)
				}
				if len(legs) == 0 {
					return hookStore{}, false
				}
				return legs[0], true
			},
		})
	}
	if len(route.frame) == 0 {
		return nil
	}
	return route
}

// workLegRouteAgentView returns the agent config the frame's legs are built
// through and whether one RESOLVED from config. The leg builders only read the
// view's Dir (which they set themselves), so an unresolved zero view builds
// the same store coordinates a configured one does; identity travels
// separately, through the identity overrides taken from the query env.
func workLegRouteAgentView(cfg *config.City, queryEnv []string) (*config.Agent, bool) {
	for _, key := range []string{"GC_AGENT", "GC_TEMPLATE", "GC_ALIAS"} {
		name := hookClaimEnvValue(queryEnv, key)
		if name == "" {
			continue
		}
		if a, ok := resolveAgentIdentity(cfg, name, currentRigContext(cfg)); ok {
			resolved := a
			return &resolved, true
		}
	}
	return &config.Agent{}, false
}

// workLegFrameDir is the dir a frame entry's leg runs in, computed without
// building its env: the city's dir for the city leg, the rig's resolved dir
// for a rig leg — the same formula appendCityHookStore and
// appendOneRigHookStore derive a leg's dir with.
func workLegFrameDir(cityPath string, cfg *config.City, rigName string) string {
	if rigName == "" {
		return cityPath
	}
	view := &config.Agent{Dir: rigName}
	return agentCommandDir(cityPath, view, cfg.Rigs)
}

// workLegRoutedHookClaimOps returns ops whose claim-time reads and writes
// follow the work leg that HOLDS the bead, once the primary leg proves it does
// not.
//
// A nil route returns ops UNCHANGED — not a wrapper that always delegates — so
// a single-store city runs the identical function values it runs today and
// pays nothing for a seam it cannot use.
//
// Every probe-routed seam applies the same rule: run the primary write; open
// the probe only on the absence the write proved (hookClaimLegProvedAbsent);
// re-run the write through the first frame leg that holds the bead; when every
// leg proves absence, return the primary's own answer unchanged — the class
// route above this one escalates that to the binding exactly as it does today.
// Memo-routed seams (the release of an undelivered claim, an adoption
// re-stamp, the lifecycle emission, the workflow stop gate) never probe: they
// are consequences of a claim this invocation already routed, and a bead the
// route never homed is one the primary store answered for.
func workLegRoutedHookClaimOps(ops hookClaimOps, route *hookClaimWorkLegRoute) hookClaimOps {
	if route == nil {
		return ops
	}
	ops.applyDefaults()
	base := ops
	route.readLeg = base.ReadWorkMeta
	ops.WorkLegRoute = route

	ops.Claim = func(ctx context.Context, dir string, env []string, beadID, assignee string) (beads.Bead, bool, error) {
		if home, ok := route.home[beadID]; ok {
			return base.Claim(ctx, home.dir, home.env, beadID, assignee)
		}
		if bdIDIsClassReserved(beadID) {
			return base.Claim(ctx, dir, env, beadID, assignee)
		}
		claimed, ok, err := base.Claim(ctx, dir, env, beadID, assignee)
		if ok || err == nil {
			// The primary's CAS COMMITTED (ok), with or without a readback
			// error, or it refused the claim without error (a lost race):
			// ownership is resolved there and this route has no second
			// opinion to offer.
			return claimed, ok, err
		}
		if !hookClaimLegProvedAbsent(ctx, route.readLeg, hookStore{dir: dir, env: env}, beadID, assignee, err) {
			return claimed, ok, err
		}
		home, held, probeErr := route.probe(ctx, beadID, assignee)
		if probeErr != nil {
			return beads.Bead{}, false, probeErr
		}
		if !held {
			return claimed, ok, err
		}
		return base.Claim(ctx, home.dir, home.env, beadID, assignee)
	}

	ops.StampWorkMeta = func(ctx context.Context, dir string, env []string, beadID, assignee string, patch map[string]string) error {
		if home, ok := route.home[beadID]; ok {
			return base.StampWorkMeta(ctx, home.dir, home.env, beadID, assignee, patch)
		}
		if bdIDIsClassReserved(beadID) {
			return base.StampWorkMeta(ctx, dir, env, beadID, assignee, patch)
		}
		err := base.StampWorkMeta(ctx, dir, env, beadID, assignee, patch)
		if err == nil || !hookClaimLegProvedAbsent(ctx, route.readLeg, hookStore{dir: dir, env: env}, beadID, assignee, err) {
			return err
		}
		home, held, probeErr := route.probe(ctx, beadID, assignee)
		if probeErr != nil {
			return probeErr
		}
		if !held {
			return err
		}
		return base.StampWorkMeta(ctx, home.dir, home.env, beadID, assignee, patch)
	}

	ops.ReadWorkMeta = func(ctx context.Context, dir string, env []string, beadID, assignee string) (beads.Bead, error) {
		if home, ok := route.home[beadID]; ok {
			return base.ReadWorkMeta(ctx, home.dir, home.env, beadID, assignee)
		}
		if bdIDIsClassReserved(beadID) {
			return base.ReadWorkMeta(ctx, dir, env, beadID, assignee)
		}
		// A read's own not-found is the proof a write's would be settled into,
		// so an unrecognized read error is never re-read: it is the answer.
		bead, err := base.ReadWorkMeta(ctx, dir, env, beadID, assignee)
		if err == nil || !hookClaimBeadIsElsewhere(err) {
			return bead, err
		}
		home, held, probeErr := route.probe(ctx, beadID, assignee)
		if probeErr != nil {
			return beads.Bead{}, probeErr
		}
		if !held {
			return bead, err
		}
		return base.ReadWorkMeta(ctx, home.dir, home.env, beadID, assignee)
	}

	ops.AssignContinuation = func(ctx context.Context, dir string, env []string, beadID, assignee string) error {
		if home, ok := route.home[beadID]; ok {
			return base.AssignContinuation(ctx, home.dir, home.env, beadID, assignee)
		}
		if bdIDIsClassReserved(beadID) {
			return base.AssignContinuation(ctx, dir, env, beadID, assignee)
		}
		err := base.AssignContinuation(ctx, dir, env, beadID, assignee)
		if err == nil || !hookClaimLegProvedAbsent(ctx, route.readLeg, hookStore{dir: dir, env: env}, beadID, assignee, err) {
			return err
		}
		home, held, probeErr := route.probe(ctx, beadID, assignee)
		if probeErr != nil {
			return probeErr
		}
		if !held {
			return err
		}
		return base.AssignContinuation(ctx, home.dir, home.env, beadID, assignee)
	}

	ops.ReclaimStale = func(ctx context.Context, dir string, env []string, beadID string) (bool, string, error) {
		if home, ok := route.home[beadID]; ok {
			return base.ReclaimStale(ctx, home.dir, home.env, beadID)
		}
		if bdIDIsClassReserved(beadID) {
			return base.ReclaimStale(ctx, dir, env, beadID)
		}
		reclaimed, previousOwner, err := base.ReclaimStale(ctx, dir, env, beadID)
		if err == nil || !hookClaimLegProvedAbsent(ctx, route.readLeg, hookStore{dir: dir, env: env}, beadID, "", err) {
			return reclaimed, previousOwner, err
		}
		home, held, probeErr := route.probe(ctx, beadID, "")
		if probeErr != nil {
			return false, "", probeErr
		}
		if !held {
			return reclaimed, previousOwner, err
		}
		return base.ReclaimStale(ctx, home.dir, home.env, beadID)
	}

	// The release of an undelivered claim must run against the leg the claim
	// landed in, or it would clear an assignee in a store that never held one
	// and leave the real claim parked. Memo-only, never probes.
	ops.Release = func(ctx context.Context, dir string, env []string, beadID, assignee string) (bool, error) {
		if home, ok := route.home[beadID]; ok {
			return base.Release(ctx, home.dir, home.env, beadID, assignee)
		}
		return base.Release(ctx, dir, env, beadID, assignee)
	}

	ops.RestampAdopted = func(ctx context.Context, dir string, env []string, beadID, fromAssignee, toAssignee string) (bool, error) {
		if home, ok := route.home[beadID]; ok {
			return base.RestampAdopted(ctx, home.dir, home.env, beadID, fromAssignee, toAssignee)
		}
		return base.RestampAdopted(ctx, dir, env, beadID, fromAssignee, toAssignee)
	}

	// The lifecycle-start emission reads the step's workflow root, so it
	// belongs in the leg the claim landed in. Memo-only, never probes.
	ops.EmitExecutionStepStarted = func(step beads.Bead, dir string, env []string, assignee string) {
		if home, ok := route.home[step.ID]; ok {
			base.EmitExecutionStepStarted(step, home.dir, home.env, assignee)
			return
		}
		base.EmitExecutionStepStarted(step, dir, env, assignee)
	}

	// WorkflowStore is deliberately NOT wrapped here. Its result is a raw
	// beads.Store, and the typed-store boundary (storeclass) forbids a new
	// raw-store passthrough outside the residency layer — the class route's
	// identical wrapper is grandfathered baseline debt. The seam's two callers
	// are the workflow stop gates, whose ancestry reads FAIL OPEN on a miss
	// (hookClaimWorkflowStop) and run pre-claim — before this route has homed
	// anything — or post-claim for a sibling pin; an unrouted read there reads
	// the primary leg, misses a rig-held ancestry, and fails open exactly as a
	// store hiccup does today, re-checked on the next poll.

	// A continuation LIST follows the root's home when the route has one; a
	// list has no not-found to probe on, so an unrouted root is read exactly as
	// the caller asked. The class route above this one escalates an empty
	// group.
	ops.ListContinuation = func(ctx context.Context, dir string, env []string, rootID, group string) ([]beads.Bead, error) {
		if home, ok := route.home[rootID]; ok {
			return base.ListContinuation(ctx, home.dir, home.env, rootID, group)
		}
		return base.ListContinuation(ctx, dir, env, rootID, group)
	}

	// The done-workflow check and retirement read the root's ancestry in the
	// ledger the root lives in. Memo-only, never probes.
	ops.InputVerdict = func(ctx context.Context, dir string, env []string, candidate beads.Bead, identityCandidates []string, prove workflowInputAbsenceProver) (workflowInputVerdict, error) {
		if home, ok := route.home[workflowRootIDForInputCheck(candidate)]; ok {
			return base.InputVerdict(ctx, home.dir, home.env, candidate, identityCandidates, prove)
		}
		return base.InputVerdict(ctx, dir, env, candidate, identityCandidates, prove)
	}

	ops.RetireWorkflow = func(ctx context.Context, dir string, env []string, verdict workflowInputVerdict) error {
		if home, ok := route.home[verdict.RootID]; ok {
			return base.RetireWorkflow(ctx, home.dir, home.env, verdict)
		}
		return base.RetireWorkflow(ctx, dir, env, verdict)
	}

	return ops
}
