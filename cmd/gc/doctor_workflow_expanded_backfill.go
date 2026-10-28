package main

import (
	"fmt"
	"sort"
	"strings"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/doctor"
)

// workflowExpandedBackfillCheck stamps gc.workflow_expanded=true on graph.v2
// workflow roots minted before the stamp existed (gc 1.4.x and earlier). An
// unstamped expanded root passes every stamp-keyed latch gate as if it were a
// root-only launch target: hookClaimMatchesRoute serves it on its route, the
// worker's routed tier offers it as "executable" demand, and the reconciler
// counts it — the worker loop #7436 describes. The mint-time stamp and its
// consumers landed together (#5901), so every root an older gc expanded is
// permanently misclassified. The evidence here is the open workflow-finalize
// child the graph.v2 compile links to the root exactly when the workflow has
// real steps, which is the same condition the mint-time stamp encodes; a
// root-only root, including one that later grew attached fragment steps, is
// never backfilled. The check is idempotent: a root already stamped true, and
// any bead that is not a graph.v2 workflow root, is left untouched.
type workflowExpandedBackfillCheck struct {
	cfg      *config.City
	cityPath string
	newStore func(string) (beads.Store, error)
}

func newWorkflowExpandedBackfillCheck(cfg *config.City, cityPath string, newStore func(string) (beads.Store, error)) *workflowExpandedBackfillCheck {
	return &workflowExpandedBackfillCheck{cfg: cfg, cityPath: cityPath, newStore: newStore}
}

func (c *workflowExpandedBackfillCheck) Name() string         { return "workflow-expanded-backfill" }
func (c *workflowExpandedBackfillCheck) CanFix() bool         { return true }
func (c *workflowExpandedBackfillCheck) WarmupEligible() bool { return false }

// backfillStampTarget is one unstamped expanded root to stamp, in the store
// that holds it. The assignee rides along so Run can mark roots already owned
// by a worker — those are the live half of the loop the stamp ends.
type backfillStampTarget struct {
	label    string
	store    beads.Store
	beadID   string
	assignee string
}

// collect walks every scope a graph.v2 workflow root can live in: the city
// work store, the city's graph-class binding on a split city, and every
// non-suspended rig with a path. A root is a target exactly when it is an
// open graph.v2 workflow root without gc.workflow_expanded whose store also
// holds an open workflow-finalize bead naming it as its gc.root_bead_id —
// finalization closes the root before it closes the finalize bead, so an
// open root's evidence is always open too. Closed beads are excluded by the
// store's default, so no status filter is needed.
func (c *workflowExpandedBackfillCheck) collect() (targets []backfillStampTarget, skipped []string) {
	collectStore := func(label string, store beads.Store) {
		roots, err := store.List(beads.ListQuery{Metadata: map[string]string{
			beadmeta.KindMetadataKey:            beadmeta.KindWorkflow,
			beadmeta.FormulaContractMetadataKey: beadmeta.FormulaContractGraphV2,
		}})
		if err != nil {
			skipped = append(skipped, fmt.Sprintf("%s skipped: listing beads: %v", label, err))
			return
		}
		if len(roots) == 0 {
			return
		}
		finalizers, err := store.List(beads.ListQuery{Metadata: map[string]string{
			beadmeta.KindMetadataKey: beadmeta.KindWorkflowFinalize,
		}})
		if err != nil {
			skipped = append(skipped, fmt.Sprintf("%s skipped: listing beads: %v", label, err))
			return
		}
		expandedRoots := make(map[string]bool, len(finalizers))
		for _, f := range finalizers {
			if rootID := strings.TrimSpace(f.Metadata[beadmeta.RootBeadIDMetadataKey]); rootID != "" {
				expandedRoots[rootID] = true
			}
		}
		for _, root := range roots {
			if strings.TrimSpace(root.Metadata[beadmeta.WorkflowExpandedMetadataKey]) == "true" {
				continue
			}
			if !expandedRoots[root.ID] {
				continue
			}
			targets = append(targets, backfillStampTarget{label: label, store: store, beadID: root.ID, assignee: root.Assignee})
		}
	}

	if c.newStore != nil && strings.TrimSpace(c.cityPath) != "" {
		cityStore, err := c.newStore(c.cityPath)
		if err != nil {
			skipped = append(skipped, fmt.Sprintf("city skipped: opening bead store: %v", err))
		} else {
			collectStore("city", cityStore)
			// On a split city the city-keyed graph binding, not the work
			// store, holds the graph.v2 roots; when nothing relocates this
			// resolves to the exact store value just collected.
			if graphStore := scopeGraphStore(c.cityPath, c.cityPath, c.cfg, cityStore); graphStore != nil && graphStore != cityStore {
				collectStore("city graph-class store", graphStore)
			}
		}
	}
	if c.cfg != nil {
		for _, rig := range c.cfg.Rigs {
			if rig.Suspended || strings.TrimSpace(rig.Path) == "" || c.newStore == nil {
				continue
			}
			rigStore, err := c.newStore(rig.Path)
			if err != nil {
				skipped = append(skipped, fmt.Sprintf("rig %s skipped: opening bead store: %v", rig.Name, err))
				continue
			}
			collectStore("rig "+rig.Name, rigStore)
		}
	}
	return targets, skipped
}

func (c *workflowExpandedBackfillCheck) Run(_ *doctor.CheckContext) *doctor.CheckResult {
	targets, skipped := c.collect()
	if len(targets) == 0 && len(skipped) == 0 {
		return okCheck(c.Name(), "no graph.v2 workflow roots need gc.workflow_expanded backfill")
	}
	details := make([]string, 0, len(targets)+len(skipped))
	for _, tgt := range targets {
		row := fmt.Sprintf("%s bead %s is an expanded graph.v2 workflow root without gc.workflow_expanded", tgt.label, tgt.beadID)
		if assignee := strings.TrimSpace(tgt.assignee); assignee != "" {
			row += fmt.Sprintf(" (assigned to %s)", assignee)
		}
		details = append(details, row)
	}
	details = append(details, skipped...)
	sort.Strings(details)
	if len(targets) == 0 {
		return warnCheck(c.Name(),
			fmt.Sprintf("gc.workflow_expanded backfill skipped %d scope(s)", len(skipped)),
			"fix bead store access, then rerun gc doctor",
			details)
	}
	return warnCheck(c.Name(),
		fmt.Sprintf("%d graph.v2 workflow root(s) need the gc.workflow_expanded stamp", len(targets)),
		"run gc doctor --fix to stamp gc.workflow_expanded on legacy expanded workflow roots",
		details)
}

func (c *workflowExpandedBackfillCheck) Fix(_ *doctor.CheckContext) error {
	targets, skipped := c.collect()
	for _, tgt := range targets {
		if err := tgt.store.SetMetadata(tgt.beadID, beadmeta.WorkflowExpandedMetadataKey, "true"); err != nil {
			return fmt.Errorf("%s bead %s: backfill gc.workflow_expanded: %w", tgt.label, tgt.beadID, err)
		}
	}
	if len(skipped) > 0 {
		return fmt.Errorf("workflow-expanded-backfill skipped %d scope(s): %s", len(skipped), strings.Join(skipped, "; "))
	}
	return nil
}
