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

// workflowExpandedBackfillCheck repairs graph.v2 workflow roots that were
// expanded into real steps before gc.workflow_expanded was stamped at mint
// (#5901). Every latch gate — the hook claim matcher, controller demand, and
// the shell work and count queries — treats an unstamped workflow root as a
// possibly root-only workflow and keeps it claimable, so such a root, routed
// and ready for its whole run, is served to workers that can never progress it
// (#7436). --fix stamps gc.workflow_expanded=true on every open graph.v2 root
// an open workflow-finalize step names through gc.root_bead_id: the graph.v2
// compile adds that step exactly when it expands the root, and attached
// fragments strip it, so a root-only root stays unstamped and claimable.
// Finalization closes the root before its finalizer, so an open root's
// finalizer is open and reading open beads is enough. The check is idempotent.
type workflowExpandedBackfillCheck struct {
	cfg      *config.City
	cityPath string
	newStore func(string) (beads.Store, error)
}

func newWorkflowExpandedBackfillCheck(cfg *config.City, cityPath string, newStore func(string) (beads.Store, error)) *workflowExpandedBackfillCheck {
	return &workflowExpandedBackfillCheck{cfg: cfg, cityPath: cityPath, newStore: newStore}
}

func (c *workflowExpandedBackfillCheck) Name() string { return "workflow-expanded-backfill" }

func (c *workflowExpandedBackfillCheck) CanFix() bool { return true }

func (c *workflowExpandedBackfillCheck) WarmupEligible() bool { return false }

// workflowExpandedTarget is a single expanded workflow root that needs
// gc.workflow_expanded backfilled.
type workflowExpandedTarget struct {
	label    string
	store    beads.Store
	beadID   string
	assignee string
}

// workflowExpandedScope is one store the check reads.
type workflowExpandedScope struct {
	label string
	store beads.Store
}

func (c *workflowExpandedBackfillCheck) scopes() (scopes []workflowExpandedScope, skipped []string) {
	if c.newStore == nil {
		return nil, nil
	}
	paths := []struct{ label, path string }{{"city", c.cityPath}}
	if c.cfg != nil {
		for _, rig := range c.cfg.Rigs {
			if rig.Suspended || strings.TrimSpace(rig.Path) == "" {
				continue
			}
			paths = append(paths, struct{ label, path string }{"rig " + rig.Name, rig.Path})
		}
	}
	for _, p := range paths {
		if strings.TrimSpace(p.path) == "" {
			continue
		}
		store, err := c.newStore(p.path)
		if err != nil {
			skipped = append(skipped, fmt.Sprintf("%s skipped: opening bead store: %v", p.label, err))
			continue
		}
		scopes = append(scopes, workflowExpandedScope{label: p.label, store: store})
		// Split cities keep graph.v2 roots in the city-keyed graph binding. A
		// binding this process may not read is not a scope: a worker site
		// holds no copy of it, and a refused city's storage gate has already
		// printed its own refusal. That refusal says nothing about any root and
		// --fix cannot clear it, so counting it as a skip would keep this check
		// warning on every run.
		if p.label == "city" {
			graphStore := scopeGraphStore(c.cityPath, p.path, c.cfg, store)
			if _, refused := graphStore.(refusedClassStore); !refused && graphStore != nil && graphStore != store {
				scopes = append(scopes, workflowExpandedScope{label: "city graph", store: graphStore})
			}
		}
	}
	return scopes, skipped
}

func (c *workflowExpandedBackfillCheck) collect() (targets []workflowExpandedTarget, skipped []string) {
	scopes, skipped := c.scopes()
	for _, sc := range scopes {
		roots, err := sc.store.List(beads.ListQuery{Metadata: map[string]string{
			beadmeta.KindMetadataKey:            beadmeta.KindWorkflow,
			beadmeta.FormulaContractMetadataKey: beadmeta.FormulaContractGraphV2,
		}})
		if err != nil {
			skipped = append(skipped, fmt.Sprintf("%s skipped: listing workflow roots: %v", sc.label, err))
			continue
		}
		finalizers, err := sc.store.List(beads.ListQuery{Metadata: map[string]string{
			beadmeta.KindMetadataKey: beadmeta.KindWorkflowFinalize,
		}})
		if err != nil {
			skipped = append(skipped, fmt.Sprintf("%s skipped: listing workflow-finalize beads: %v", sc.label, err))
			continue
		}
		finalized := make(map[string]bool, len(finalizers))
		for _, f := range finalizers {
			if rootID := strings.TrimSpace(f.Metadata[beadmeta.RootBeadIDMetadataKey]); rootID != "" {
				finalized[rootID] = true
			}
		}
		for _, root := range roots {
			if strings.TrimSpace(root.Metadata[beadmeta.WorkflowExpandedMetadataKey]) == "true" || !finalized[root.ID] {
				continue
			}
			targets = append(targets, workflowExpandedTarget{label: sc.label, store: sc.store, beadID: root.ID, assignee: strings.TrimSpace(root.Assignee)})
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
		detail := fmt.Sprintf("%s bead %s is an expanded graph.v2 workflow root without gc.workflow_expanded", tgt.label, tgt.beadID)
		if tgt.assignee != "" {
			detail += fmt.Sprintf(" (assigned to %s)", tgt.assignee)
		}
		details = append(details, detail)
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
		fmt.Sprintf("%d expanded workflow root(s) lack gc.workflow_expanded and stay claimable", len(targets)),
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
