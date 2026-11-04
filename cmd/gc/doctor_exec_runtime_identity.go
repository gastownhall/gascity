package main

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/doctor"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
)

// execRuntimeIdentityCheck is the migration aid of PAR-EXEC §5 item 8 and
// §12.2: on a city whose exec session runtime is v2-ready, it reads the
// runtime's inventory once and counts the running boxes under open session
// rows that carry no identity (identity null) or no session id (Ownerless).
// v2 holds such a box, Unknown or Ownerless, instead of adopting it, so a
// city with any should stop those sessions before it runs v2 (an unset
// session_reconciler runs v2 on the next start). A city on another runtime,
// or on an exec runtime that is not v2-ready, is not affected.
type execRuntimeIdentityCheck struct {
	cfg      *config.City
	cityPath string
	openLegs func() []v2SessionLeg
	provider func() (runtime.Provider, error)
}

func newExecRuntimeIdentityCheck(cfg *config.City, cityPath string) *execRuntimeIdentityCheck {
	return &execRuntimeIdentityCheck{
		cfg: cfg, cityPath: cityPath,
		openLegs: func() []v2SessionLeg { return openV2SessionLegs(cityPath, cfg) },
		provider: func() (runtime.Provider, error) {
			reg, err := runtimeRegistryForCity(cfg)
			if err != nil {
				return nil, err
			}
			return reg.New(v2SessionRuntimeRawName(cfg, reconcilerModeLookupEnv), cfg.Session, loadedCityName(cfg, cityPath), cityPath)
		},
	}
}

// Name implements doctor.Check.
func (*execRuntimeIdentityCheck) Name() string { return "exec-runtime-identity" }

// CanFix implements doctor.Check.
func (*execRuntimeIdentityCheck) CanFix() bool { return false }

// WarmupEligible implements doctor.Check: it forks the runtime's inventory.
func (*execRuntimeIdentityCheck) WarmupEligible() bool { return false }

// Fix implements doctor.Check.
func (*execRuntimeIdentityCheck) Fix(_ *doctor.CheckContext) error { return nil }

// Run implements doctor.Check.
func (c *execRuntimeIdentityCheck) Run(_ *doctor.CheckContext) *doctor.CheckResult {
	name := v2SessionRuntimeName(c.cfg, reconcilerModeLookupEnv)
	script, ok := execRuntimeScript(c.cfg, name)
	if !ok {
		return okCheck(c.Name(), "the session runtime is not an exec runtime")
	}
	if !execReadiness(script, c.cfg.EffectiveCityName(), c.cityPath).ready() {
		return okCheck(c.Name(), fmt.Sprintf("exec session runtime %q is not v2-ready: v2 does not run on it", name))
	}
	sp, err := c.provider()
	if err != nil {
		return warnCheck(c.Name(), fmt.Sprintf("exec session runtime %q: not checked: %v", name, err), "", nil)
	}
	inv, ok := runtime.InventoryFor(sp)
	if !ok {
		return warnCheck(c.Name(), fmt.Sprintf("exec session runtime %q serves no inventory: not checked", name), "", nil)
	}
	entries, err := inv.RuntimeInventory(context.Background())
	if err != nil {
		return warnCheck(c.Name(), fmt.Sprintf("exec session runtime %q: inventory failed: %v", name, err), "", nil)
	}
	rows, unread := c.openRows()
	var null, ownerless []string
	for _, row := range rows {
		e, listed := entries[strings.TrimSpace(row.SessionName)]
		switch {
		case !listed:
		case !e.IdentityKnown:
			null = append(null, fmt.Sprintf("%s (row %s)", row.SessionName, row.ID))
		case strings.TrimSpace(e.Identity["GC_SESSION_ID"]) == "":
			ownerless = append(ownerless, fmt.Sprintf("%s (row %s)", row.SessionName, row.ID))
		}
	}
	sort.Strings(null)
	sort.Strings(ownerless)
	details := append([]string(nil), unread...)
	for _, n := range null {
		details = append(details, "identity null: "+n)
	}
	for _, n := range ownerless {
		details = append(details, "no session id (Ownerless): "+n)
	}
	if len(null)+len(ownerless) == 0 {
		msg := fmt.Sprintf("every running box under an open session row exposes its identity (%d row(s))", len(rows))
		if len(unread) > 0 {
			return warnCheck(c.Name(), msg+"; some session legs were not read", "", details)
		}
		r := okCheck(c.Name(), msg)
		r.Details = details
		return r
	}
	return warnCheck(c.Name(),
		fmt.Sprintf("%d running box(es) under open session rows v2 would hold: %d with identity null, %d with no session id", len(null)+len(ownerless), len(null), len(ownerless)),
		"stop those sessions before the city runs v2 (an unset session_reconciler runs v2 on the next start), or make the script report their identity (docs/reference/exec-session-provider.md, observe.v1 item 4)",
		details)
}

// openRows are the city's open session rows across its legs, folded by bead
// ID, and a line for each leg not read.
func (c *execRuntimeIdentityCheck) openRows() ([]session.Info, []string) {
	var rows []session.Info
	var unread []string
	seen := map[string]bool{}
	for _, leg := range c.openLegs() {
		if leg.store == nil {
			unread = append(unread, fmt.Sprintf("leg %s: not read: %s", leg.ref, leg.notChecked))
			continue
		}
		infos, err := sessionFrontDoor(leg.store).ListAll(session.ListAllOptions{Live: true})
		_ = closeBeadStoreHandle(leg.store)
		if err != nil {
			unread = append(unread, fmt.Sprintf("leg %s: not read: %v", leg.ref, err))
		}
		for _, info := range infos {
			id := strings.TrimSpace(info.ID)
			if info.Closed || seen[id] || strings.TrimSpace(info.SessionName) == "" {
				continue
			}
			seen[id] = id != ""
			rows = append(rows, info)
		}
	}
	return rows, unread
}
