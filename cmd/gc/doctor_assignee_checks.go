package main

import (
	"fmt"
	"sort"
	"strings"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/doctor"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/suspensionstate"
)

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
	roster := newAssigneeRosterAt(c.cfg, c.cityPath)
	var findings []string
	var skipped []string
	var notes []string
	for _, problem := range roster.problemsList() {
		skipped = append(skipped, "assignee roster incomplete: "+problem)
	}
	if roster.Empty() {
		return warnCheck(c.Name(),
			"no agents or named sessions resolved from config, so assignees cannot be checked",
			"resolve the config load first (see city-config and packv2-import-state), then rerun gc doctor",
			nil)
	}

	roster.addSessionIdentities(c.liveSessions(&skipped))
	c.scanScope(&findings, &skipped, roster, "city", c.cityPath)
	for _, class := range relocatedBeadClasses(c.cfg) {
		notes = append(notes, fmt.Sprintf("relocated %s store not scanned: assignee scan covers city and active rig work stores only", class.Class))
	}
	if c.cfg != nil {
		suspState, err := loadSuspensionState(fsys.OSFS{}, c.cityPath)
		if err != nil {
			skipped = append(skipped, fmt.Sprintf("suspension state skipped: %v", err))
		}
		for _, rig := range c.cfg.Rigs {
			if suspensionstate.EffectiveRigSuspended(suspState, rig.Name, rig.EffectiveSuspendedOnStart()) {
				continue
			}
			if strings.TrimSpace(rig.Path) == "" {
				skipped = append(skipped, fmt.Sprintf("rig %s skipped: no store path resolved", rig.Name))
				continue
			}
			c.scanScope(&findings, &skipped, roster, "rig "+rig.Name, rig.Path)
		}
	}

	details := append(append(append([]string{}, findings...), skipped...), notes...)
	sort.Strings(details)
	if len(findings) == 0 && len(skipped) == 0 {
		message := "every non-empty assignee on open beads in the scanned city and active rig stores resolves"
		if len(notes) > 0 {
			message += fmt.Sprintf("; %d relocated class store(s) not scanned", len(notes))
		}
		return &doctor.CheckResult{Name: c.Name(), Status: doctor.StatusOK, Message: message, Details: details}
	}
	if len(findings) == 0 {
		return warnCheck(c.Name(),
			fmt.Sprintf("assignee check incomplete: %d skipped scope(s) or roster problem(s)", len(skipped)),
			"review each skipped scope; fix access failures and rerun gc doctor",
			details)
	}
	summary := fmt.Sprintf("%d open bead(s) assigned to a target that does not resolve", len(findings))
	if len(skipped) > 0 {
		summary += fmt.Sprintf("; %d skipped scope(s) or roster problem(s)", len(skipped))
	}
	return warnCheck(c.Name(), summary,
		"reassign each bead to a configured agent or named session, or add the missing target to city.toml; do not clear the field without choosing an owner",
		details)
}

func (c *assigneeResolvesCheck) liveSessions(skipped *[]string) []session.Info {
	if c.newStore == nil || strings.TrimSpace(c.cityPath) == "" {
		return nil
	}
	store, err := c.newStore(c.cityPath)
	if err != nil {
		*skipped = append(*skipped, fmt.Sprintf("live sessions not read: opening city bead store: %v", err))
		return nil
	}
	infos, err := loadOpenSessionInfos(cliSessionStore(store, c.cfg, c.cityPath))
	if err != nil {
		*skipped = append(*skipped, fmt.Sprintf("live sessions not read: %v", err))
		return nil
	}
	return infos
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
	items, err := store.List(beads.ListQuery{AllowScan: true, TierMode: beads.TierBoth})
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
