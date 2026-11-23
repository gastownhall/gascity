package main

import (
	"fmt"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/proxyendpoint"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/doctor"
)

// bdSchemaDoctorCheck is gc doctor's half of leaving an existing bd
// database's migration to an operator (see bd_schema_gap.go): for the city
// and every active rig whose database the bd pack adopts on a Dolt server, it
// compares the database's schema with the linked beads library's and names
// each database that differs, with the command an operator runs. It never
// fixes anything: migrating is exactly the consent gc does not give.
type bdSchemaDoctorCheck struct {
	cfg  *config.City
	rigs []config.Rig
	// skip is set where gc runs no Dolt (GC_DOLT=skip).
	skip bool
	// scopes lists the scopes to check, and city what the notices need to
	// know about the city; bdSchemaScopesFor and newBdSchemaCity outside
	// tests.
	scopes func(cityPath string, rigs []config.Rig) []bdSchemaScope
	city   func(cityPath string, cfg *config.City) bdSchemaCity
	deps   bdSchemaDeps
}

// newBdSchemaDoctorCheck builds the check over the city and the active rigs.
func newBdSchemaDoctorCheck(cfg *config.City, rigs []config.Rig, skip bool) *bdSchemaDoctorCheck {
	return &bdSchemaDoctorCheck{
		cfg:    cfg,
		rigs:   rigs,
		skip:   skip,
		scopes: bdSchemaScopesFor,
		city:   newBdSchemaCity,
		deps:   liveBdSchemaDeps,
	}
}

// Name implements doctor.Check.
func (*bdSchemaDoctorCheck) Name() string { return "bd-schema" }

// CanFix implements doctor.Check: migrating is an operator's decision.
func (*bdSchemaDoctorCheck) CanFix() bool { return false }

// Fix implements doctor.Check.
func (*bdSchemaDoctorCheck) Fix(_ *doctor.CheckContext) error { return nil }

// WarmupEligible implements doctor.Check: gc start already prints each
// database's notice after its bead lifecycle.
func (*bdSchemaDoctorCheck) WarmupEligible() bool { return false }

// Run implements doctor.Check: OK when every database it could read is at the
// linked library's schema, else an advisory Warning whose details hold one
// notice per database that differs. A database that could not be read is a
// detail either way; the dolt-server checks report why.
func (c *bdSchemaDoctorCheck) Run(ctx *doctor.CheckContext) *doctor.CheckResult {
	r := &doctor.CheckResult{Name: c.Name(), Status: doctor.StatusOK, Severity: doctor.SeverityAdvisory}
	if c.skip {
		r.Message = "skipped (GC_DOLT=skip)"
		return r
	}
	if ctx == nil || ctx.CityPath == "" {
		r.Message = "no city"
		return r
	}
	scopes := c.scopes(ctx.CityPath, c.rigs)
	if len(scopes) == 0 {
		r.Message = "no bd database on a Dolt server to check"
		return r
	}
	main, ignored := beads.PinnedSchemaCursors()
	linked := bdSchemaCursors(proxyendpoint.Cursors{Main: main, Ignored: ignored})
	report := collectBdSchemaReport(c.city(ctx.CityPath, c.cfg), scopes, c.deps)
	var unreadable []string
	for _, u := range report.unreadable {
		unreadable = append(unreadable, fmt.Sprintf("%s: not checked: %v", u.label, u.err))
	}
	checked := len(scopes) - len(report.unreadable)
	switch {
	case len(report.gaps) > 0:
		r.Status = doctor.StatusWarning
		r.Message = fmt.Sprintf("%d bd database(s) not at %s, the schema of the beads library linked into gc", len(report.gaps), linked)
		r.Details = append(append([]string{}, report.notices...), unreadable...)
		r.FixHint = "once every client of a database is ready, an operator migrates it with the command its detail names, then reruns this check, which reads both migration lanes (see \"Upgrading an Existing City\" in the docs)"
	case checked == 0:
		r.Message = fmt.Sprintf("none of %d bd database(s) could be read; schema not checked", len(scopes))
		r.Details = unreadable
	default:
		r.Message = fmt.Sprintf("%d of %d bd database(s) checked, each at %s, the schema of the beads library linked into gc", checked, len(scopes), linked)
		r.Details = unreadable
	}
	return r
}
