package main

import "github.com/gastownhall/gascity/internal/doctor"

// workflowExpandedBackfillCheck stamps gc.workflow_expanded onto already
// expanded graph.v2 roots (#5901 follow-up). RED skeleton: reports nothing.
type workflowExpandedBackfillCheck struct {
	scan *runTargetRoutedToBackfillCheck
}

func newWorkflowExpandedBackfillCheck(scan *runTargetRoutedToBackfillCheck) *workflowExpandedBackfillCheck {
	return &workflowExpandedBackfillCheck{scan: scan}
}

func (c *workflowExpandedBackfillCheck) Name() string { return "workflow-expanded-backfill" }

func (c *workflowExpandedBackfillCheck) CanFix() bool { return true }

func (c *workflowExpandedBackfillCheck) WarmupEligible() bool { return false }

func (c *workflowExpandedBackfillCheck) Run(_ *doctor.CheckContext) *doctor.CheckResult {
	return okCheck(c.Name(), "not implemented")
}

func (c *workflowExpandedBackfillCheck) Fix(_ *doctor.CheckContext) error { return nil }
