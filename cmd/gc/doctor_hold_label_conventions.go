package main

import (
	"fmt"
	"sort"
	"strings"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/doctor"
)

// retiredHoldLabels are hold/blocked labels retired by the canonical hold
// taxonomy decided in ga-tug8ry.1. Only hold:mayor and hold:external remain
// sanctioned; any live (non-closed) bead still carrying one of these is
// convention drift.
var retiredHoldLabels = []string{
	"arch-hold",
	"blocked",
	"blocked-by-operator",
	"blocked-on-external",
	"blocked-on-upstream",
	"blocked-prereq",
	"human-hold",
	"human",
	"on-hold",
}

// holdLabelConventionsFixHint mirrors ga-tug8ry.1's disposition table.
// engdocs/contributors/hold-label-conventions.md does not exist on
// origin/main yet, so the hint must stand on its own rather than point at it.
const holdLabelConventionsFixHint = "Retired hold/blocked label in use (ga-tug8ry.1 taxonomy): " +
	"arch-hold and blocked-prereq retire with no migration; blocked retires in favor of the " +
	"native status field; blocked-by-operator, blocked-on-upstream, human-hold, and human " +
	"migrate to hold:mayor; blocked-on-external migrates to hold:external; on-hold retires as " +
	"already-superseded. Set a sanctioned hold label with " +
	"'bd set-state <id> hold=mayor|external --reason \"...\"'."

// holdLabelConventionsCheck flags live use of retired hold/blocked labels
// within a single bead store scope (a city or a rig). It classifies bead
// content through the typed beads.Store interface, so it lives in cmd/gc
// alongside backlogDepthCheck rather than in internal/doctor.
type holdLabelConventionsCheck struct {
	dir      string
	label    string
	newStore func(string) (beads.Store, error)
}

func newHoldLabelConventionsCheck(dir, label string, newStore func(string) (beads.Store, error)) *holdLabelConventionsCheck {
	return &holdLabelConventionsCheck{dir: dir, label: label, newStore: newStore}
}

func (c *holdLabelConventionsCheck) Name() string { return "hold-label-conventions:" + c.label }

func (c *holdLabelConventionsCheck) CanFix() bool { return false }

// Fix is a no-op: retired labels disperse to different remediation targets
// (several to hold:mayor, one to hold:external, several retire with no
// migration, bare "blocked" moves to the native status field), so no single
// mechanical fix applies uniformly.
func (c *holdLabelConventionsCheck) Fix(_ *doctor.CheckContext) error { return nil }

func (c *holdLabelConventionsCheck) WarmupEligible() bool { return false }

func (c *holdLabelConventionsCheck) Run(_ *doctor.CheckContext) *doctor.CheckResult {
	res := &doctor.CheckResult{Name: c.Name(), Severity: doctor.SeverityAdvisory}

	if c.newStore == nil || strings.TrimSpace(c.dir) == "" {
		res.Status = doctor.StatusWarning
		res.Message = fmt.Sprintf("hold-label conventions unknown for %s: no bead store configured", c.label)
		return res
	}

	store, err := c.newStore(c.dir)
	if err != nil {
		res.Status = doctor.StatusWarning
		res.Message = fmt.Sprintf("hold-label conventions unknown for %s: opening bead store: %v", c.label, err)
		return res
	}

	var details []string
	var queryErrs []string
	for _, label := range retiredHoldLabels {
		found, err := store.ListByLabel(label, 0)
		if err != nil {
			queryErrs = append(queryErrs, fmt.Sprintf("querying label %q: %v", label, err))
			continue
		}
		for _, b := range found {
			details = append(details, fmt.Sprintf("retired label %q on %s %q", label, b.ID, b.Title))
		}
	}
	sort.Strings(details)
	sort.Strings(queryErrs)

	switch {
	case len(details) > 0:
		res.Status = doctor.StatusError
		res.Message = fmt.Sprintf("%d retired hold/blocked label use(s) found in %s", len(details), c.label)
		details = append(details, queryErrs...)
		res.Details = details
		res.FixHint = holdLabelConventionsFixHint
	case len(queryErrs) > 0:
		res.Status = doctor.StatusWarning
		res.Message = fmt.Sprintf("hold-label conventions check for %s hit %d label-query error(s)", c.label, len(queryErrs))
		res.Details = queryErrs
	default:
		res.Status = doctor.StatusOK
		res.Message = fmt.Sprintf("no retired hold/blocked labels found in %s", c.label)
	}

	return res
}
