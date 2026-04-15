package main

import (
	"fmt"
	"strings"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/doctor"
	"github.com/gastownhall/gascity/internal/session"
)

type sessionModelDoctorCheck struct {
	cfg      *config.City
	cityPath string
	newStore func(string) (beads.Store, error)
}

func (c *sessionModelDoctorCheck) Name() string { return "session-model" }

func (c *sessionModelDoctorCheck) CanFix() bool { return false }

func (c *sessionModelDoctorCheck) Fix(_ *doctor.CheckContext) error { return nil }

func (c *sessionModelDoctorCheck) Run(_ *doctor.CheckContext) *doctor.CheckResult {
	r := &doctor.CheckResult{Name: c.Name(), Status: doctor.StatusOK, Message: "session ownership is consistent"}
	if c == nil || c.newStore == nil {
		return r
	}
	store, err := c.newStore(c.cityPath)
	if err != nil {
		r.Status = doctor.StatusWarning
		r.Message = fmt.Sprintf("session model diagnostics skipped: %v", err)
		return r
	}
	all, err := store.List(beads.ListQuery{AllowScan: true, IncludeClosed: true, Sort: beads.SortCreatedAsc})
	if err != nil {
		r.Status = doctor.StatusWarning
		r.Message = fmt.Sprintf("session model diagnostics skipped: %v", err)
		return r
	}

	sessionByID := make(map[string]beads.Bead)
	openSessionAlias := make(map[string][]beads.Bead)
	openSessionAliasHistory := make(map[string][]beads.Bead)
	openSessionName := make(map[string][]beads.Bead)
	for _, b := range all {
		if !session.IsSessionBeadOrRepairable(b) {
			continue
		}
		sessionByID[b.ID] = b
		if b.Status == "closed" {
			continue
		}
		if alias := strings.TrimSpace(b.Metadata["alias"]); alias != "" {
			openSessionAlias[alias] = append(openSessionAlias[alias], b)
		}
		for _, alias := range session.AliasHistory(b.Metadata) {
			openSessionAliasHistory[alias] = append(openSessionAliasHistory[alias], b)
		}
		if sn := strings.TrimSpace(b.Metadata["session_name"]); sn != "" {
			openSessionName[sn] = append(openSessionName[sn], b)
		}
	}

	var findings []string
	for _, b := range all {
		if session.IsSessionBeadOrRepairable(b) || b.Status == "closed" {
			continue
		}
		assignee := strings.TrimSpace(b.Assignee)
		if assignee != "" {
			if owner, ok := sessionByID[assignee]; ok {
				if owner.Status == "closed" {
					findings = append(findings, fmt.Sprintf("closed-bead-owner: %s is assigned to closed session bead %s", b.ID, assignee))
				} else if isRetiredSessionModelOwner(owner) {
					findings = append(findings, fmt.Sprintf("retired-bead-owner: %s is assigned to retired session bead %s", b.ID, assignee))
				}
			} else if looksLikeSessionBeadID(assignee) {
				findings = append(findings, fmt.Sprintf("missing-bead-owner: %s is assigned to missing session bead %s", b.ID, assignee))
			} else {
				matches := legacySessionTokenMatches(assignee, openSessionAlias, openSessionName)
				switch {
				case len(matches) > 1:
					findings = append(findings, fmt.Sprintf("ambiguous-legacy-session-token: %s assignee %q matches %d open sessions", b.ID, assignee, len(matches)))
				case len(matches) == 0 && config.FindAgent(c.cfg, assignee) != nil:
					findings = append(findings, fmt.Sprintf("legacy-token-matches-config-only: %s assignee %q matches config but no session", b.ID, assignee))
				case len(matches) == 0:
					historical := legacySessionTokenMatches(assignee, openSessionAliasHistory, nil)
					if len(historical) > 0 {
						findings = append(findings, fmt.Sprintf("historical-alias-owner: %s assignee %q matches retired session alias on %s; update to bead ID/current alias", b.ID, assignee, historical[0].ID))
					}
				}
			}
		}
		if routedTo := strings.TrimSpace(b.Metadata["gc.routed_to"]); routedTo != "" {
			cityName := config.EffectiveCityName(c.cfg, "")
			if config.FindAgent(c.cfg, routedTo) == nil {
				if _, ok, _ := resolveNamedSessionSpecForConfigTarget(c.cfg, cityName, routedTo, currentRigContext(c.cfg)); !ok {
					findings = append(findings, fmt.Sprintf("stale-routed-config: %s routes to missing config target %q", b.ID, routedTo))
				}
			}
		}
	}

	cityName := config.EffectiveCityName(c.cfg, "")
	for _, b := range all {
		if b.Status == "closed" || !session.IsSessionBeadOrRepairable(b) {
			continue
		}
		if spec, found, err := findConflictingNamedSessionSpecForBead(c.cfg, cityName, b); err == nil && found {
			findings = append(findings, fmt.Sprintf("configured-named-conflict: session bead %s blocks named session %q", b.ID, spec.Identity))
		}
	}

	if len(findings) == 0 {
		return r
	}
	r.Status = doctor.StatusWarning
	r.Message = fmt.Sprintf("%d session model finding(s)", len(findings))
	r.Details = findings
	return r
}

func isRetiredSessionModelOwner(b beads.Bead) bool {
	return session.LifecycleIdentityReleased(b.Status, b.Metadata)
}

func looksLikeSessionBeadID(s string) bool {
	return strings.HasPrefix(s, "gc-") || strings.HasPrefix(s, "bd-") || strings.HasPrefix(s, "mc-")
}

func legacySessionTokenMatches(token string, byAlias, bySessionName map[string][]beads.Bead) []beads.Bead {
	seen := make(map[string]bool)
	var out []beads.Bead
	for _, b := range byAlias[token] {
		if !seen[b.ID] {
			out = append(out, b)
			seen[b.ID] = true
		}
	}
	for _, b := range bySessionName[token] {
		if !seen[b.ID] {
			out = append(out, b)
			seen[b.ID] = true
		}
	}
	return out
}
