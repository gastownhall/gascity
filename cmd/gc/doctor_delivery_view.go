package main

import (
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/delivery"
	"github.com/gastownhall/gascity/internal/doctor"
)

type beadDeliveryRow struct {
	id        string
	phase     string
	age       time.Duration
	budget    time.Duration
	flag      string
	escalated string
}

type prDeliveryDoctorCheck struct {
	cityPath string
	newStore func(string) (beads.Store, error)
	rows     []beadDeliveryRow
}

func (c *prDeliveryDoctorCheck) Name() string                     { return "pr-delivery" }
func (c *prDeliveryDoctorCheck) CanFix() bool                     { return false }
func (c *prDeliveryDoctorCheck) Fix(_ *doctor.CheckContext) error { return nil }
func (c *prDeliveryDoctorCheck) WarmupEligible() bool             { return false }

func (c *prDeliveryDoctorCheck) Run(_ *doctor.CheckContext) *doctor.CheckResult {
	r := &doctor.CheckResult{Name: c.Name()}

	if c.newStore == nil {
		r.Status = doctor.StatusOK
		r.Message = "no delivery beads in flight"
		return r
	}

	store, err := c.newStore(c.cityPath)
	if err != nil {
		r.Status = doctor.StatusWarning
		r.Severity = doctor.SeverityAdvisory
		r.Message = fmt.Sprintf("pr-delivery check skipped: %v", err)
		return r
	}

	all, err := store.ListOpen()
	if err != nil {
		r.Status = doctor.StatusWarning
		r.Severity = doctor.SeverityAdvisory
		r.Message = fmt.Sprintf("pr-delivery check skipped: %v", err)
		return r
	}

	var inFlight []beads.Bead
	for _, b := range all {
		phase := strings.TrimSpace(b.Metadata[delivery.MetaKeyPhase])
		if phase == "" || delivery.IsTerminalPhase(phase) {
			continue
		}
		inFlight = append(inFlight, b)
	}

	if len(inFlight) == 0 {
		r.Status = doctor.StatusOK
		r.Message = "no delivery beads in flight"
		return r
	}

	c.rows = nil
	var stuckCount, atRiskCount int
	var details []string
	for _, b := range inFlight {
		phase := b.Metadata[delivery.MetaKeyPhase]
		age := delivery.PhaseAge(b)
		budget := delivery.PhaseBudgets[phase]
		escalated := b.Metadata[delivery.MetaKeyWardenEscalated]
		var flag string
		if budget > 0 {
			switch {
			case age > budget:
				stuckCount++
				flag = "STUCK"
			case age > budget*4/5:
				atRiskCount++
				flag = "at-risk"
			default:
				flag = "ok"
			}
		} else {
			flag = "unknown-budget"
		}
		c.rows = append(c.rows, beadDeliveryRow{
			id:        b.ID,
			phase:     phase,
			age:       age,
			budget:    budget,
			flag:      flag,
			escalated: escalated,
		})
		details = append(details, fmt.Sprintf("%s: %s %s/%s %s",
			b.ID, phase, formatDuration(age), formatDuration(budget), flag))
	}

	r.Details = details
	if stuckCount > 0 || atRiskCount > 0 {
		r.Status = doctor.StatusWarning
		r.Severity = doctor.SeverityAdvisory
	} else {
		r.Status = doctor.StatusOK
	}
	r.Message = fmt.Sprintf("%d delivery bead(s) in flight (%d stuck, %d at-risk)",
		len(inFlight), stuckCount, atRiskCount)
	return r
}

func (c *prDeliveryDoctorCheck) RenderExtras(_ *doctor.CheckContext, w io.Writer) {
	if len(c.rows) == 0 {
		return
	}
	fmt.Fprintf(w, "%-12s %-16s %-9s %-9s %s\n", "ID", "PHASE", "AGE", "BUDGET", "STATUS") //nolint:errcheck
	for _, row := range c.rows {
		fmt.Fprintf(w, "%-12s %-16s %-9s %-9s %s\n", //nolint:errcheck
			row.id, row.phase, formatDuration(row.age), formatDuration(row.budget), row.flag)
	}
	fmt.Fprintln(w, "Escalated:") //nolint:errcheck
	seen := make(map[string]bool)
	hasEscalated := false
	for _, row := range c.rows {
		if row.escalated != "" && !seen[row.id] {
			seen[row.id] = true
			fmt.Fprintf(w, "  %s  %s  escalated=%s\n", row.id, row.phase, row.escalated) //nolint:errcheck
			hasEscalated = true
		}
	}
	if !hasEscalated {
		fmt.Fprintln(w, "  (none)") //nolint:errcheck
	}
}
