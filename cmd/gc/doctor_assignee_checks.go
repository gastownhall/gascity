package main

import (
	"fmt"
	"sort"
	"strings"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/doctor"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/suspensionstate"
)

// assigneeResolvesCheck reports open work whose assignee names nothing the city
// can route to. Such a bead reads as owned indefinitely while no process
// anywhere is able to pick it up.
//
// This check is deliberately report-only. Under a skeleton-crew posture,
// choosing a real owner is a resourcing decision, and blanking the field would
// trade one misleading label for another.
type assigneeResolvesCheck struct {
	cfg      *config.City
	cityPath string
	newStore func(string) (beads.Store, error)
}

func newAssigneeResolvesCheck(cfg *config.City, cityPath string, newStore func(string) (beads.Store, error)) *assigneeResolvesCheck {
	return &assigneeResolvesCheck{cfg: cfg, cityPath: cityPath, newStore: newStore}
}

func (c *assigneeResolvesCheck) Name() string { return "assignee-resolves" }

func (c *assigneeResolvesCheck) CanFix() bool { return false }

func (c *assigneeResolvesCheck) WarmupEligible() bool { return false }

func (c *assigneeResolvesCheck) Fix(_ *doctor.CheckContext) error { return nil }

func (c *assigneeResolvesCheck) Run(_ *doctor.CheckContext) *doctor.CheckResult {
	roster := newAssigneeRoster(c.cfg)
	if roster.Empty() {
		return warnCheck(c.Name(),
			"no agents or named sessions resolved from config, so assignees cannot be checked",
			"resolve the config load first (see city-config and packv2-import-state), then rerun gc doctor",
			nil)
	}

	var findings []string
	var skipped []string
	c.scanScope(&findings, &skipped, roster, "city", c.cityPath)
	if c.cfg != nil {
		suspState, _ := loadSuspensionState(fsys.OSFS{}, c.cityPath)
		for _, rig := range c.cfg.Rigs {
			if suspensionstate.EffectiveRigSuspended(suspState, rig.Name, rig.EffectiveSuspendedOnStart()) || strings.TrimSpace(rig.Path) == "" {
				continue
			}
			c.scanScope(&findings, &skipped, roster, "rig "+rig.Name, rig.Path)
		}
	}

	if len(findings) == 0 && len(skipped) == 0 {
		return okCheck(c.Name(), "every open bead's assignee resolves to a routable target")
	}
	details := append([]string{}, findings...)
	details = append(details, skipped...)
	sort.Strings(details)
	if len(findings) == 0 {
		return warnCheck(c.Name(),
			fmt.Sprintf("assignee check skipped %d scope(s)", len(skipped)),
			"fix bead store access, then rerun gc doctor",
			details)
	}
	summary := fmt.Sprintf("%d open bead(s) assigned to a target that does not resolve", len(findings))
	if len(skipped) > 0 {
		summary += fmt.Sprintf("; %d scope(s) skipped", len(skipped))
	}
	return warnCheck(c.Name(), summary,
		"reassign each bead to a configured agent or named session, or add the missing target to city.toml; do not clear the field without choosing an owner",
		details)
}

func (c *assigneeResolvesCheck) scanScope(findings, skipped *[]string, roster *assigneeRoster, label, path string) {
	if c.newStore == nil {
		*skipped = append(*skipped, fmt.Sprintf("%s skipped: no bead store constructor configured", label))
		return
	}
	if strings.TrimSpace(path) == "" {
		*skipped = append(*skipped, fmt.Sprintf("%s skipped: no store path resolved", label))
		return
	}
	store, err := c.newStore(path)
	if err != nil {
		*skipped = append(*skipped, fmt.Sprintf("%s skipped: opening bead store: %v", label, err))
		return
	}
	items, err := store.List(beads.ListQuery{AllowScan: true})
	if err != nil {
		*skipped = append(*skipped, fmt.Sprintf("%s skipped: listing beads: %v", label, err))
		return
	}
	for _, bead := range items {
		if !isOpenWorkStatus(bead.Status) {
			continue
		}
		assignee := strings.TrimSpace(bead.Assignee)
		if assignee == "" || roster.Resolves(assignee) {
			continue
		}
		*findings = append(*findings, fmt.Sprintf("%s bead %s has assignee=%q, which matches no agent or named session", label, bead.ID, assignee))
	}
}

func isOpenWorkStatus(status string) bool {
	switch strings.TrimSpace(status) {
	case "open", "in_progress", "blocked":
		return true
	}
	return false
}
