package main

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/doctor"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
)

// execRuntimeIdentityCheck is the migration aid of PAR-EXEC §5 item 8 and
// §12.2: on a city with an exec session runtime whose config resolves to v2
// as the controller latches it (resolveReconcilerMode: the runtime v2-ready,
// and session_reconciler a v2 the latch admits, or unset with no default
// fallback), it reads the runtime's inventory once and judges each running
// box under an open session row as v2 does (compareIdentity on the batched
// identity, else on the box's observed identity, as the inventory lane
// reads it): a box v2 holds, Ownerless or Unknown, instead of adopting it
// is named. A city with any should stop those sessions before it runs v2.
type execRuntimeIdentityCheck struct {
	cfg      *config.City
	cityPath string
	openLegs func() []v2SessionLeg
	provider func() (runtime.Provider, error)
}

// newExecRuntimeIdentityCheck reads the session provider doctor already
// built for its session checks (sp, or spErr for why there is none).
func newExecRuntimeIdentityCheck(cfg *config.City, cityPath string, sp runtime.Provider, spErr error) *execRuntimeIdentityCheck {
	return &execRuntimeIdentityCheck{
		cfg: cfg, cityPath: cityPath,
		openLegs: func() []v2SessionLeg { return openV2SessionLegs(cityPath, cfg) },
		provider: func() (runtime.Provider, error) {
			if sp == nil {
				return nil, spErr
			}
			return sp, nil
		},
	}
}

// execIdentityObserveBudget bounds the boxes the check observes one by one,
// those whose batched identity is null: each observe can take seconds, so
// once the budget is spent the rest are reported not checked. A variable so
// a test can spend it.
var execIdentityObserveBudget = 20 * time.Second

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
	if _, ok := execRuntimeScript(c.cfg, name); !ok {
		return okCheck(c.Name(), "the session runtime is not an exec runtime")
	}
	// The latch's own resolution, which reads the runtime's admission
	// handshake: a legacy pin, a default fallback and a refused v2 run
	// legacy or nothing, so v2 holds no box.
	switch res, err := resolveReconcilerMode(c.cfg, c.cityPath, reconcilerModeLookupEnv); {
	case err != nil:
		return okCheck(c.Name(), "v2 does not run on this city: "+err.Error())
	case res.Mode != reconcilerV2:
		return okCheck(c.Name(), "v2 does not run on this city: session reconciler: "+res.String())
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
	var unknown, ownerless, notChecked []string
	start := time.Now()
	for _, row := range rows {
		sessName := strings.TrimSpace(row.SessionName)
		e, listed := entries[sessName]
		if !listed {
			continue
		}
		id := identityOfEnv(e.Identity)
		if !e.IdentityKnown {
			if time.Since(start) >= execIdentityObserveBudget {
				notChecked = append(notChecked, fmt.Sprintf("%s (row %s)", row.SessionName, row.ID))
				continue
			}
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
	sort.Strings(notChecked)
	for _, n := range notChecked {
		details = append(details, "not checked (observe budget spent): "+n)
	}
	if len(unknown)+len(ownerless) == 0 {
		msg := fmt.Sprintf("v2 would hold no running box under an open session row (%d row(s))", len(rows))
		if len(notChecked) > 0 {
			msg = fmt.Sprintf("v2 would hold none of the boxes checked; %d box(es) not checked: the observe budget (%s) was spent", len(notChecked), execIdentityObserveBudget)
		}
		if len(unread) > 0 {
			msg += "; some session legs were not read"
		}
		if len(unread)+len(notChecked) > 0 {
			return warnCheck(c.Name(), msg, "", details)
		}
		r := okCheck(c.Name(), msg)
		r.Details = details
		return r
	}
	return warnCheck(c.Name(),
		fmt.Sprintf("%d running box(es) under open session rows v2 would hold: %d whose identity is unknown, %d Ownerless%s", len(unknown)+len(ownerless), len(unknown), len(ownerless), notCheckedNote(len(notChecked))),
		"stop those sessions before the city runs v2 (its config resolves to v2), or make the script report their identity (docs/reference/exec-session-provider.md, observe.v1 item 4)",
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

// notCheckedNote is the check message's note on boxes left unchecked.
func notCheckedNote(n int) string {
	if n == 0 {
		return ""
	}
	return fmt.Sprintf("; %d more not checked (the observe budget was spent)", n)
}
