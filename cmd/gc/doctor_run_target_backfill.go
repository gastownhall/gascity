package main

import (
	"fmt"
	"sort"
	"strings"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
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

func (c *runTargetRoutedToBackfillCheck) collect() (targets []backfillTarget, skipped []string) {
	scopes := []struct{ label, path string }{{"city", c.cityPath}}
	if c.cfg != nil {
		for _, rig := range c.cfg.Rigs {
			if rig.Suspended || strings.TrimSpace(rig.Path) == "" {
				continue
			}
			scopes = append(scopes, struct{ label, path string }{"rig " + rig.Name, rig.Path})
		}
	}
	for _, sc := range scopes {
		if c.newStore == nil || strings.TrimSpace(sc.path) == "" {
			continue
		}
		store, err := c.newStore(sc.path)
		if err != nil {
			skipped = append(skipped, fmt.Sprintf("%s skipped: opening bead store: %v", sc.label, err))
			continue
		}
		// Workflow roots are the only pool-routed persisted beads that need
		// gc.routed_to backfilled to stay claimable. Control-dispatcher and
		// topology beads can also carry bare gc.run_target, but they are not
		// claimed through the pool-demand gc.routed_to path. A targeted metadata
		// query avoids a full-store scan.
		items, err := store.List(beads.ListQuery{Metadata: map[string]string{"gc.kind": "workflow"}})
		if err != nil {
			skipped = append(skipped, fmt.Sprintf("%s skipped: listing beads: %v", sc.label, err))
			continue
		}
		for _, b := range items {
			runTarget := strings.TrimSpace(b.Metadata["gc.run_target"])
			if runTarget == "" || strings.TrimSpace(b.Metadata["gc.routed_to"]) != "" {
				continue
			}
			targets = append(targets, backfillTarget{label: sc.label, store: store, beadID: b.ID, runTarget: runTarget})
		}
	}
	return targets, skipped
}

func (c *runTargetRoutedToBackfillCheck) Run(_ *doctor.CheckContext) *doctor.CheckResult {
	targets, skipped := c.collect()
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

func (c *runTargetRoutedToBackfillCheck) Fix(_ *doctor.CheckContext) error {
	targets, skipped := c.collect()
	for _, tgt := range targets {
		if err := tgt.store.SetMetadata(tgt.beadID, "gc.routed_to", tgt.runTarget); err != nil {
			return fmt.Errorf("%s bead %s: backfill gc.routed_to: %w", tgt.label, tgt.beadID, err)
		}
	}
	if len(skipped) > 0 {
		return fmt.Errorf("run-target-routed-to-backfill skipped %d scope(s): %s", len(skipped), strings.Join(skipped, "; "))
	}
	return nil
}
