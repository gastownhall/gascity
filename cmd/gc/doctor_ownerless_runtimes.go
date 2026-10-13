package main

import (
	"fmt"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/doctor"
	"github.com/gastownhall/gascity/internal/runtime"
)

// ownerlessRuntimesCheck is gc doctor's finding for the runtimes no session
// row owns (CONTRACT v5.10j, mc-gjc95): one with no readable GC_SESSION_ID,
// or one whose GC_SESSION_ID names a row the session store does not hold.
// The controller's tick reaps only a dead one of this city with no client
// attached; every other one stays running, and this names it with why.
type ownerlessRuntimesCheck struct {
	cfg      *config.City
	cityPath string
	newStore func(string) (beads.Store, error)
	sp       runtime.Provider // the session checks' provider; nil with spErr
	spErr    error
}

func newOwnerlessRuntimesCheck(cfg *config.City, cityPath string, newStore func(string) (beads.Store, error), sp runtime.Provider, spErr error) *ownerlessRuntimesCheck {
	return &ownerlessRuntimesCheck{cfg: cfg, cityPath: cityPath, newStore: newStore, sp: sp, spErr: spErr}
}

func (c *ownerlessRuntimesCheck) Name() string { return "ownerless-runtimes" }

// CanFix returns false: an ownerless runtime the tick leaves (alive,
// attached, unprovable) is the operator's to inspect and stop.
func (c *ownerlessRuntimesCheck) CanFix() bool { return false }

func (c *ownerlessRuntimesCheck) WarmupEligible() bool { return false }

func (c *ownerlessRuntimesCheck) Fix(_ *doctor.CheckContext) error { return nil }

func (c *ownerlessRuntimesCheck) Run(_ *doctor.CheckContext) *doctor.CheckResult {
	store, err := c.newStore(c.cityPath)
	if err != nil {
		return warnCheck(c.Name(), fmt.Sprintf("skipping the ownerless-runtime scan: opening bead store: %v", err), "fix bead store access, then rerun gc doctor", nil)
	}
	if c.sp == nil {
		return warnCheck(c.Name(), fmt.Sprintf("skipping the ownerless-runtime scan: session provider: %v", c.spErr), "fix the session provider, then rerun gc doctor", nil)
	}
	findings, err := ownerlessRuntimeFindings(cliSessionStore(store, c.cfg, c.cityPath), c.sp, c.cityPath)
	if err != nil && len(findings) == 0 {
		return warnCheck(c.Name(), fmt.Sprintf("listing runtimes: %v", err), "rerun gc doctor once the session provider answers", nil)
	}
	if len(findings) == 0 {
		return okCheck(c.Name(), "every runtime has an owning session row")
	}
	return warnCheck(c.Name(), fmt.Sprintf("%d runtime(s) no session row owns", len(findings)),
		"the controller reaps a dead one of this city; inspect any other (gc session attach, or your provider's tools) and stop it if it is no longer wanted",
		findings)
}
