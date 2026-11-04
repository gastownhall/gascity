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
// §12.2: on a city whose exec session runtime is v2-ready and not pinned to
// legacy, it reads the runtime's inventory once and judges each running box
// under an open session row as v2 does (compareIdentity on the batched
// identity, else on the box's observed identity, as the inventory lane
// reads it): a box v2 holds, Ownerless or Unknown, instead of adopting it
// is named. A city with any should stop those sessions before it runs v2 (an
// unset session_reconciler runs v2 on the next start).
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
			sp, err := newSessionProviderForCity(cfg, cityPath)
			if err != nil {
				return nil, fmt.Errorf("building the session provider: %w", err)
			}
			return sp, nil
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
	if mode, _, known := c.cfg.Daemon.SessionReconcilerMode(); known && mode == config.SessionReconcilerLegacy {
		return okCheck(c.Name(), "session_reconciler pins legacy: v2 does not run on this city")
	}
	if !execReadiness(script, c.cfg.EffectiveCityName(), c.cityPath).ready() {
		return okCheck(c.Name(), fmt.Sprintf("exec session runtime %q is not v2-ready: v2 does not run on it", name))
	}
	sp, err := c.provider()
	if err != nil {
		return warnCheck(c.Name(), fmt.Sprintf("exec session runtime %q: not checked: %v", name, err), "", nil)
	}
	leaf, entries, err := execInventoryLeaf(sp)
	if err != nil {
		return warnCheck(c.Name(), fmt.Sprintf("exec session runtime %q: not checked: %v", name, err), "", nil)
	}
	rows, unread := c.openRows()
	var unknown, ownerless []string
	for _, row := range rows {
		sessName := strings.TrimSpace(row.SessionName)
		e, listed := entries[sessName]
		if !listed {
			continue
		}
		id := identityOfEnv(e.Identity)
		if !e.IdentityKnown {
			id = readIdentityEnv(leaf, sessName) // the observed identity, as the lane reads it
		}
		switch compareIdentity(row, id) {
		case identityOwnerless:
			ownerless = append(ownerless, fmt.Sprintf("%s (row %s)", row.SessionName, row.ID))
		case identityUnknown:
			unknown = append(unknown, fmt.Sprintf("%s (row %s)", row.SessionName, row.ID))
		}
	}
	sort.Strings(unknown)
	sort.Strings(ownerless)
	details := append([]string(nil), unread...)
	for _, n := range unknown {
		details = append(details, "identity unknown: "+n)
	}
	for _, n := range ownerless {
		details = append(details, "Ownerless: "+n)
	}
	if len(unknown)+len(ownerless) == 0 {
		msg := fmt.Sprintf("v2 would hold no running box under an open session row (%d row(s))", len(rows))
		if len(unread) > 0 {
			return warnCheck(c.Name(), msg+"; some session legs were not read", "", details)
		}
		r := okCheck(c.Name(), msg)
		r.Details = details
		return r
	}
	return warnCheck(c.Name(),
		fmt.Sprintf("%d running box(es) under open session rows v2 would hold: %d whose identity is unknown, %d Ownerless", len(unknown)+len(ownerless), len(unknown), len(ownerless)),
		"stop those sessions before the city runs v2 (an unset session_reconciler runs v2 on the next start), or make the script report their identity (docs/reference/exec-session-provider.md, observe.v1 item 4)",
		details)
}

// execInventoryLeaf is sp's exec leaf with an inventory, and its batched
// inventory: the city's provider may be a composite (an ACP leg beside it).
func execInventoryLeaf(sp runtime.Provider) (runtime.Provider, map[string]runtime.InventoryEntry, error) {
	for _, leaf := range inventoryLeaves(sp, "") {
		if _, ok := leaf.provider.(runtime.V2ReadinessReporter); !ok {
			continue
		}
		inv, ok := runtime.InventoryFor(leaf.provider)
		if !ok {
			return nil, nil, fmt.Errorf("the exec leaf serves no inventory")
		}
		entries, err := inv.RuntimeInventory(context.Background())
		if err != nil {
			return nil, nil, fmt.Errorf("inventory failed: %w", err)
		}
		return leaf.provider, entries, nil
	}
	return nil, nil, fmt.Errorf("the session provider has no exec leaf")
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
