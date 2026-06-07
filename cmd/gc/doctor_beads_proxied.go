package main

import (
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/doctor"
)

// beadsProxiedCapabilityCheck is the fail-safe visibility surface for the
// opt-in proxied-server gate (gastownhall/gascity #1978). When a city sets
// [beads] proxied=true but the resolved bd binary lacks `--proxied-server`
// external support, gascity silently falls back to direct ServerMode (the
// provider script warns once, the metadata overlay is skipped). This check
// makes that fallback visible and actionable. It is advisory, not blocking:
// the fallback keeps the city working, so it must not gate `gc start`.
//
// Lives in package main (not internal/doctor) because the bd capability probe
// (bdSupportsProxiedServer) resolves the same ${BD_BIN:-bd} the provider script
// uses, and internal/doctor cannot import package main.
type beadsProxiedCapabilityCheck struct {
	cfg *config.City
}

func newBeadsProxiedCapabilityCheck(cfg *config.City) *beadsProxiedCapabilityCheck {
	return &beadsProxiedCapabilityCheck{cfg: cfg}
}

func (c *beadsProxiedCapabilityCheck) Name() string { return "beads-proxied-capability" }

func (c *beadsProxiedCapabilityCheck) Run(_ *doctor.CheckContext) *doctor.CheckResult {
	r := &doctor.CheckResult{Name: c.Name()}
	if c.cfg == nil || !c.cfg.Beads.ProxiedEnabled() {
		r.Status = doctor.StatusOK
		r.Message = "proxied-server gate off (direct ServerMode)"
		return r
	}
	if bdSupportsProxiedServer() {
		r.Status = doctor.StatusOK
		r.Message = "proxied-server gate on; resolved bd supports connection pooling"
		return r
	}
	r.Status = doctor.StatusError
	r.Severity = doctor.SeverityAdvisory
	r.Message = "[beads] proxied=true but the resolved bd does not support --proxied-server; running in direct ServerMode"
	r.FixHint = "install a bd build with connection pooling (feat/connection-pooling) or set [beads] proxied=false in city.toml"
	return r
}

func (c *beadsProxiedCapabilityCheck) CanFix() bool { return false }

func (c *beadsProxiedCapabilityCheck) Fix(_ *doctor.CheckContext) error { return nil }

func (c *beadsProxiedCapabilityCheck) WarmupEligible() bool { return false }
