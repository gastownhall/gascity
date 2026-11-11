package main

import (
	"fmt"
	"sort"
	"strings"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/coordclass"
	"github.com/gastownhall/gascity/internal/doctor"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/suspensionstate"
)

// unreachableRoutedWorkCheck reports open, unassigned, routed beads whose type
// the controller's demand probe excludes. gc hook lists such a bead for its
// route target, but the reconciler counts zero demand for it, so a pool with
// no other work never spawns a session. It is report-only: closing or
// re-materializing the bead is a disposition decision, not a mechanical fix.
//
// It reads the city store, every non-suspended rig store, and, on a split-storage
// city, the relocated graph binding, where workflow beads live and no scope store
// reaches. A gate is exempt: it is a wait condition parked on its owner, so a
// gate routed to a pool is not reported.
type unreachableRoutedWorkCheck struct {
	cfg      *config.City
	cityPath string
	list     routedBeadLister
}

// routedBeadLister reads the beads of the scope store at path. The check takes
// beads rather than a store so it never holds a raw beads.Store (storeclass).
type routedBeadLister func(path string, q beads.ListQuery) ([]beads.Bead, error)

func newUnreachableRoutedWorkCheck(cfg *config.City, cityPath string, list routedBeadLister) *unreachableRoutedWorkCheck {
	return &unreachableRoutedWorkCheck{cfg: cfg, cityPath: cityPath, list: list}
}

func (c *unreachableRoutedWorkCheck) Name() string { return "unreachable-routed-work" }

func (c *unreachableRoutedWorkCheck) Run(ctx *doctor.CheckContext) *doctor.CheckResult {
	findings, skipped := c.collect(ctx)
	if len(findings) == 0 && len(skipped) == 0 {
		return okCheck(c.Name(), "no routed work hidden from the controller's demand probe")
	}
	details := append(append([]string(nil), findings...), skipped...)
	sort.Strings(details)
	if len(findings) == 0 {
		return warnCheck(c.Name(),
			fmt.Sprintf("unreachable routed work check skipped %d scope(s)", len(skipped)),
			"fix bead store access, then rerun gc doctor",
			details)
	}
	message := fmt.Sprintf("%d routed bead(s) can never wake their route target", len(findings))
	if len(skipped) > 0 {
		message += fmt.Sprintf("; %d scope(s) skipped", len(skipped))
	}
	return warnCheck(c.Name(), message,
		"close each stale bead, or re-create the work as an actionable bead such as a vapor/root-only wisp; gc doctor --fix does not change them",
		details)
}

// collect scans the city store and every non-suspended, path-bearing rig store
// for open, unassigned beads routed to a target whose type Ready() excludes.
func (c *unreachableRoutedWorkCheck) collect(ctx *doctor.CheckContext) (findings, skipped []string) {
	scopes := []struct{ label, path string }{{"city", c.cityPath}}
	if c.cfg != nil {
		suspState, _ := loadSuspensionState(fsys.OSFS{}, c.cityPath)
		for _, rig := range c.cfg.Rigs {
			if suspensionstate.EffectiveRigSuspended(suspState, rig.Name, rig.EffectiveSuspendedOnStart()) || strings.TrimSpace(rig.Path) == "" {
				continue
			}
			scopes = append(scopes, struct{ label, path string }{"rig " + rig.Name, rig.Path})
		}
	}
	scanned := map[string]bool{}
	for _, sc := range scopes {
		if c.list == nil || strings.TrimSpace(sc.path) == "" {
			continue
		}
		if ctx.Canceled() {
			skipped = append(skipped, sc.label+" skipped: check abandoned at its timeout")
			continue
		}
		key := normalizePathForCompare(sc.path)
		if scanned[key] {
			continue
		}
		scanned[key] = true
		items, err := c.list(sc.path, beads.ListQuery{AllowScan: true})
		if err != nil {
			skipped = append(skipped, fmt.Sprintf("%s skipped: reading beads: %v", sc.label, err))
			continue
		}
		findings = append(findings, unreachableRoutedFindings(sc.label, items)...)
	}
	// On a split-storage city the workflow-class beads live in the relocated graph
	// binding, which none of the scope stores above reaches.
	if graph, relocated := cliStorageRoutes(c.cityPath).storeFor(coordclass.ClassGraph); relocated && graph != nil { // residency:allow — a doctor scan of every routed workflow bead wherever it lives; the binding is the one scope the work-scope walk above cannot reach
		const label = "graph binding"
		switch items, err := graph.List(beads.ListQuery{AllowScan: true, TierMode: beads.TierBoth}); {
		case err != nil:
			skipped = append(skipped, fmt.Sprintf("%s skipped: reading beads: %v", label, err))
		default:
			findings = append(findings, unreachableRoutedFindings(label, items)...)
		}
	}
	return findings, skipped
}

// unreachableRoutedFindings returns one finding per open, unassigned bead in
// items that is routed to a target and whose type the demand probe excludes.
func unreachableRoutedFindings(label string, items []beads.Bead) []string {
	var findings []string
	for _, b := range items {
		route := strings.TrimSpace(b.Metadata[beadmeta.RoutedToMetadataKey])
		// A gate is a deliberate wait condition parked on its owner, never
		// work a pool is expected to pick up.
		if b.Status != "open" || strings.TrimSpace(b.Assignee) != "" || route == "" ||
			b.Type == "gate" || !beads.IsReadyExcludedType(b.Type) {
			continue
		}
		findings = append(findings, fmt.Sprintf(
			"%s bead %s (type %s) is routed to %q: the controller's demand probe cannot see type %s, so %q will never be woken by it even though gc hook displays it",
			label, b.ID, b.Type, route, b.Type, route))
	}
	return findings
}

func (c *unreachableRoutedWorkCheck) CanFix() bool                     { return false }
func (c *unreachableRoutedWorkCheck) Fix(_ *doctor.CheckContext) error { return nil }
func (c *unreachableRoutedWorkCheck) WarmupEligible() bool             { return false }
