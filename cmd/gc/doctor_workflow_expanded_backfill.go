package main

import (
	"fmt"
	"sort"
	"strings"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/doctor"
)

// workflowExpandedBackfillCheck is the one-shot follow-up to #5901/#5900:
// graph.v2 roots expanded before gc.workflow_expanded shipped carry no stamp,
// so the gc.run_target claim fallback can still resurrect them. --fix stamps
// every unstamped workflow root that already has a real child step. A root-only
// (#2763) root is the unit of work itself and stays unstamped, and nothing here
// touches buildOnBoot. The scan is shared with the run-target backfill check,
// which already holds the scope walk and the store factory.
type workflowExpandedBackfillCheck struct {
	scan *runTargetRoutedToBackfillCheck
}

func newWorkflowExpandedBackfillCheck(scan *runTargetRoutedToBackfillCheck) *workflowExpandedBackfillCheck {
	return &workflowExpandedBackfillCheck{scan: scan}
}

func (c *workflowExpandedBackfillCheck) Name() string { return "workflow-expanded-backfill" }

func (c *workflowExpandedBackfillCheck) CanFix() bool { return true }

func (c *workflowExpandedBackfillCheck) WarmupEligible() bool { return false }

// matchUnstampedExpandedRoot selects unstamped workflow roots with a child.
// gc.routed_to is deliberately not consulted: the stamp is the topology latch
// and applies to an expanded root whatever its route.
func matchUnstampedExpandedRoot(b beads.Bead, hasChildren func() (bool, error)) (bool, error) {
	if !workflowRunTargetFallbackEligible(b) {
		return false, nil
	}
	return hasChildren()
}

func (c *workflowExpandedBackfillCheck) Run(_ *doctor.CheckContext) *doctor.CheckResult {
	targets, skipped := c.scan.collectMatching(matchUnstampedExpandedRoot)
	if len(targets) == 0 && len(skipped) == 0 {
		return okCheck(c.Name(), "no expanded workflow roots are missing gc.workflow_expanded")
	}
	details := make([]string, 0, len(targets)+len(skipped))
	for _, tgt := range targets {
		details = append(details, fmt.Sprintf("%s bead %s has child steps but no gc.workflow_expanded", tgt.label, tgt.beadID))
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
		fmt.Sprintf("%d expanded workflow root(s) lack gc.workflow_expanded", len(targets)),
		"run gc doctor --fix to stamp gc.workflow_expanded=true",
		details)
}

func (c *workflowExpandedBackfillCheck) Fix(_ *doctor.CheckContext) error {
	targets, skipped := c.scan.collectMatching(matchUnstampedExpandedRoot)
	for _, tgt := range targets {
		if err := tgt.store.SetMetadata(tgt.beadID, beadmeta.WorkflowExpandedMetadataKey, "true"); err != nil {
			return fmt.Errorf("%s bead %s: stamp gc.workflow_expanded: %w", tgt.label, tgt.beadID, err)
		}
	}
	if len(skipped) > 0 {
		return fmt.Errorf("%s skipped %d scope(s): %s", c.Name(), len(skipped), strings.Join(skipped, "; "))
	}
	return nil
}
