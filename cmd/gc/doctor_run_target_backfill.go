package main

import (
	"fmt"
	"sort"
	"strings"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/coordclass"
	"github.com/gastownhall/gascity/internal/doctor"
)

// runTargetRoutedToBackfillCheck repairs graph.v2 workflow roots created before
// ga-eld2x, which carry gc.run_target but no gc.routed_to. gc.run_target is
// being deprecated as a persisted routing field; once the runtime
// demand/claim/scale readers consult only gc.routed_to, such a root is
// spawned-for by scale_check but unclaimable by the worker and silently
// idle-reaped (the #2763 failure). --fix backfills gc.routed_to := gc.run_target
// so the root is claimable via the canonical key. The check is idempotent: a
// root that already carries gc.routed_to, and any bead that is not a workflow
// root, is left untouched.
type runTargetRoutedToBackfillCheck struct {
	cfg      *config.City
	cityPath string
	newStore func(string) (beads.Store, error)
}

func newRunTargetRoutedToBackfillCheck(cfg *config.City, cityPath string, newStore func(string) (beads.Store, error)) *runTargetRoutedToBackfillCheck {
	return &runTargetRoutedToBackfillCheck{cfg: cfg, cityPath: cityPath, newStore: newStore}
}

func (c *runTargetRoutedToBackfillCheck) Name() string { return "run-target-routed-to-backfill" }

func (c *runTargetRoutedToBackfillCheck) CanFix() bool { return true }

func (c *runTargetRoutedToBackfillCheck) WarmupEligible() bool { return false }

// backfillTarget is a single workflow root that needs gc.routed_to backfilled.
type backfillTarget struct {
	label     string
	store     beads.Store
	beadID    string
	runTarget string
}

// runTargetBackfillQuery returns the query that selects workflow roots. They are the only
// pool-routed persisted beads that need gc.routed_to backfilled to stay
// claimable. Control-dispatcher and topology beads can also carry bare
// gc.run_target, but they are not claimed through the pool-demand
// gc.routed_to path. A targeted metadata query avoids a full-store scan.
func runTargetBackfillQuery() beads.ListQuery {
	return beads.ListQuery{Metadata: map[string]string{beadmeta.KindMetadataKey: beadmeta.KindWorkflow}, TierMode: beads.TierBoth}
}

// collect reads every work scope through the shared scope sweep, then the
// graph binding, and returns the workflow roots that need gc.routed_to
// backfilled, in scope order, plus a skip line for each scope it could not
// read.
func (c *runTargetRoutedToBackfillCheck) collect(ctx *doctor.CheckContext) (targets []backfillTarget, skipped []string) {
	collectFrom := func(label string, store beads.Store, items []beads.Bead) {
		for _, b := range items {
			runTarget := strings.TrimSpace(b.Metadata[beadmeta.RunTargetMetadataKey])
			if runTarget == "" || strings.TrimSpace(b.Metadata[beadmeta.RoutedToMetadataKey]) != "" {
				continue
			}
			// An expanded root is neither demanded nor claimable: routing it
			// again resurrects it (#5900, NEW-5).
			if !workflowRunTargetFallbackEligible(b) {
				continue
			}
			targets = append(targets, backfillTarget{label: label, store: store, beadID: b.ID, runTarget: runTarget})
		}
	}
	for _, sc := range sweepDoctorScopeLists(ctx, doctorWorkScopes(c.cfg, c.cityPath), c.newStore, runTargetBackfillQuery()) {
		if sc.skipped != "" {
			skipped = append(skipped, sc.skipped)
			continue
		}
		collectFrom(sc.label, sc.store, sc.items)
	}
	// Workflow roots are graph class, so on a split city they live in the
	// graph binding, which none of the scope stores above is: the check used to
	// report nothing to backfill and --fix repaired nothing there (read-audit
	// R13). A binding the city's routes refuse is reported as skipped by its
	// own refusal. An abandoned check does not open the binding at all.
	if ctx.Canceled() {
		return targets, append(skipped, doctorScopeAbandoned("graph binding"))
	}
	if graph, relocated := cliStorageRoutes(c.cityPath).storeFor(coordclass.ClassGraph); relocated && graph != nil { // residency:allow — a doctor scan of every workflow root wherever it lives; the binding is the one scope the work-scope walk above cannot reach
		items, err := graph.List(runTargetBackfillQuery())
		if err != nil {
			skipped = append(skipped, fmt.Sprintf("graph binding skipped: listing beads: %v", err))
		} else {
			collectFrom("graph binding", graph, items)
		}
	}
	return targets, skipped
}

func (c *runTargetRoutedToBackfillCheck) Run(ctx *doctor.CheckContext) *doctor.CheckResult {
	if ctx.Canceled() {
		return nil // the runner abandoned this run and never reads its result
	}
	targets, skipped := c.collect(ctx)
	if len(targets) == 0 && len(skipped) == 0 {
		return okCheck(c.Name(), "no workflow roots need gc.routed_to backfill")
	}
	details := make([]string, 0, len(targets)+len(skipped))
	for _, tgt := range targets {
		details = append(details, fmt.Sprintf("%s bead %s has gc.run_target=%q with empty gc.routed_to", tgt.label, tgt.beadID, tgt.runTarget))
	}
	details = append(details, skipped...)
	sort.Strings(details)
	if len(targets) == 0 {
		return warnCheck(c.Name(),
			fmt.Sprintf("gc.routed_to backfill skipped %d scope(s)", len(skipped)),
			"fix bead store access, then rerun gc doctor",
			details)
	}
	return warnCheck(c.Name(),
		fmt.Sprintf("%d workflow root(s) carry gc.run_target without gc.routed_to", len(targets)),
		"run gc doctor --fix to backfill gc.routed_to from gc.run_target",
		details)
}

func (c *runTargetRoutedToBackfillCheck) Fix(ctx *doctor.CheckContext) error {
	if ctx.Canceled() {
		return nil // the runner abandoned this run and never reads its error
	}
	targets, skipped := c.collect(ctx)
	for _, tgt := range targets {
		if err := tgt.store.SetMetadata(tgt.beadID, beadmeta.RoutedToMetadataKey, tgt.runTarget); err != nil {
			return fmt.Errorf("%s bead %s: backfill gc.routed_to: %w", tgt.label, tgt.beadID, err)
		}
	}
	if len(skipped) > 0 {
		return fmt.Errorf("run-target-routed-to-backfill skipped %d scope(s): %s", len(skipped), strings.Join(skipped, "; "))
	}
	return nil
}
