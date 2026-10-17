package main

import (
	"fmt"
	"maps"
	"slices"
	"sort"
	"strings"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/doctor"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/suspensionstate"
)

// poolIdleRoutedWorkCheck detects a pool template that has gc.routed_to work
// sitting open and unclaimed while a live instance of that same pool is idle
// (holds no current trigger bead) and so could pick the work up right now.
//
// An idle instance alone is not a finding: a pool's min-floor idle workers
// legitimately hold no bead while waiting for routed work to arrive. This
// check only fires when both conditions hold together — idle capacity AND
// unclaimed work already routed to it.
type poolIdleRoutedWorkCheck struct {
	cfg      *config.City
	cityPath string
	newStore func(string) (beads.Store, error)
}

func newPoolIdleRoutedWorkCheck(cfg *config.City, cityPath string, newStore func(string) (beads.Store, error)) *poolIdleRoutedWorkCheck {
	return &poolIdleRoutedWorkCheck{cfg: cfg, cityPath: cityPath, newStore: newStore}
}

func (c *poolIdleRoutedWorkCheck) Name() string { return "pool-idle-routed-work" }

// CanFix returns false: deciding whether to nudge, investigate, or leave an
// idle instance alone belongs to a human or an order, not this check.
func (c *poolIdleRoutedWorkCheck) CanFix() bool { return false }

// Fix is a no-op. Detection only: it never nudges or reassigns on the
// operator's behalf.
func (c *poolIdleRoutedWorkCheck) Fix(_ *doctor.CheckContext) error { return nil }

// poolIdleRoutedWorkFinding is one pool template, in one store scope, that
// has unclaimed gc.routed_to work sitting beside at least one idle instance.
type poolIdleRoutedWorkFinding struct {
	scope         string
	template      string
	beadIDs       []string
	idleInstances []string
}

func (f poolIdleRoutedWorkFinding) describe() string {
	return fmt.Sprintf("%s pool %s has %d unclaimed routed bead(s) (%s) while %d instance(s) sit idle (%s)",
		f.scope, f.template, len(f.beadIDs), strings.Join(f.beadIDs, ", "), len(f.idleInstances), strings.Join(f.idleInstances, ", "))
}

func (c *poolIdleRoutedWorkCheck) Run(ctx *doctor.CheckContext) *doctor.CheckResult {
	if c.cfg == nil {
		return okCheck(c.Name(), "no config available")
	}
	findings, skipped := c.collect(ctx)
	if len(findings) == 0 && len(skipped) == 0 {
		return okCheck(c.Name(), "no pool has unclaimed routed work sitting beside an idle instance")
	}
	details := make([]string, 0, len(findings)+len(skipped))
	for _, f := range findings {
		details = append(details, f.describe())
	}
	details = append(details, skipped...)
	sort.Strings(details)
	if len(findings) == 0 {
		return warnCheck(c.Name(),
			fmt.Sprintf("pool idle-routed-work check skipped %d scope(s)", len(skipped)),
			"fix bead store access, then rerun gc doctor",
			details)
	}
	msg := fmt.Sprintf("%d pool(s) have unclaimed routed work while an instance sits idle", len(findings))
	if len(skipped) > 0 {
		msg = fmt.Sprintf("%s; %d scope(s) skipped", msg, len(skipped))
	}
	return warnCheck(c.Name(),
		msg,
		"nudge the idle instance (gc session nudge <name>) or investigate why it has not claimed the routed work",
		details)
}

// poolIdleRoutedWorkScope is one bead store the check reads: the city, or one
// non-suspended, path-bearing rig (rig is empty for the city).
type poolIdleRoutedWorkScope struct {
	label string
	rig   string
	store beads.Store
}

// collect scans every in-scope bead store (the city plus every non-suspended,
// path-bearing rig) for pool templates that have both an idle live instance
// and unclaimed gc.routed_to work. It mirrors v2RoutedToNamespaceCheck's scope
// iteration so routed-work sanity checks agree on what "in scope" means.
//
// Cost is bounded by stores, not stores x templates: sessions are enumerated
// once per store and grouped by template in memory, and a routed-work lookup
// is issued only for a template with an idle instance, only in a store that
// can hold work routed to it — the city store (HQ beads may be routed to any
// pool) and the template's own rig store. It stops issuing store calls once
// the doctor runner abandons the check.
func (c *poolIdleRoutedWorkCheck) collect(ctx *doctor.CheckContext) (findings []poolIdleRoutedWorkFinding, skipped []string) {
	if c.newStore == nil {
		return nil, nil
	}
	scopes, skipped := c.openScopes(ctx)

	pools := c.poolTemplateRigs()
	idle := map[string]map[string]bool{}
	readable := scopes[:0]
	for _, sc := range scopes {
		if ctx.Canceled() {
			return findings, append(skipped, sc.label+" skipped: check abandoned at its timeout")
		}
		if err := c.collectIdleInstances(sc.store, pools, idle); err != nil {
			skipped = append(skipped, fmt.Sprintf("%s skipped: %v", sc.label, err))
			continue
		}
		readable = append(readable, sc)
	}

	templates := make([]string, 0, len(idle))
	for template := range idle {
		templates = append(templates, template)
	}
	sort.Strings(templates)
	for _, sc := range readable {
		for _, template := range templates {
			if sc.rig != "" && pools[template] != sc.rig {
				continue
			}
			if ctx.Canceled() {
				return findings, append(skipped, sc.label+" skipped: check abandoned at its timeout")
			}
			beadIDs, err := unclaimedRoutedWork(sc.store, template)
			if err != nil {
				skipped = append(skipped, fmt.Sprintf("%s skipped: listing routed work for %s: %v", sc.label, template, err))
				break
			}
			if len(beadIDs) == 0 {
				continue
			}
			findings = append(findings, poolIdleRoutedWorkFinding{
				scope:         sc.label,
				template:      template,
				beadIDs:       beadIDs,
				idleInstances: slices.Sorted(maps.Keys(idle[template])),
			})
		}
	}
	return findings, skipped
}

// openScopes opens the city store and every non-suspended, path-bearing rig
// store, reporting a store that fails to open as a skipped scope.
func (c *poolIdleRoutedWorkCheck) openScopes(ctx *doctor.CheckContext) (scopes []poolIdleRoutedWorkScope, skipped []string) {
	type scopePath struct{ label, rig, path string }
	paths := []scopePath{{label: "city", path: c.cityPath}}
	suspState, _ := loadSuspensionState(fsys.OSFS{}, c.cityPath)
	for _, rig := range c.cfg.Rigs {
		if suspensionstate.EffectiveRigSuspended(suspState, rig.Name, rig.EffectiveSuspendedOnStart()) || strings.TrimSpace(rig.Path) == "" {
			continue
		}
		paths = append(paths, scopePath{label: "rig " + rig.Name, rig: rig.Name, path: rig.Path})
	}
	for _, sp := range paths {
		if strings.TrimSpace(sp.path) == "" {
			continue
		}
		if ctx.Canceled() {
			skipped = append(skipped, sp.label+" skipped: check abandoned at its timeout")
			continue
		}
		store, err := c.newStore(sp.path)
		if err != nil {
			skipped = append(skipped, fmt.Sprintf("%s skipped: opening bead store: %v", sp.label, err))
			continue
		}
		scopes = append(scopes, poolIdleRoutedWorkScope{label: sp.label, rig: sp.rig, store: store})
	}
	return scopes, skipped
}

// poolTemplateRigs maps every non-suspended generic-ephemeral pool template to
// the rig it belongs to ("" for a city-scoped template).
func (c *poolIdleRoutedWorkCheck) poolTemplateRigs() map[string]string {
	pools := map[string]string{}
	for i := range c.cfg.Agents {
		agent := &c.cfg.Agents[i]
		if agent.Suspended || !agent.SupportsGenericEphemeralSessions() {
			continue
		}
		if template := agent.QualifiedName(); template != "" {
			pools[template] = configuredRigName(c.cityPath, agent, c.cfg.Rigs)
		}
	}
	return pools
}

// collectIdleInstances enumerates one store's sessions in a single list and
// records, per pool template, every live instance that holds no trigger bead.
func (c *poolIdleRoutedWorkCheck) collectIdleInstances(store beads.Store, pools map[string]string, idle map[string]map[string]bool) error {
	sessions, err := cliSessionFrontDoor(store, c.cfg, c.cityPath).List("", "")
	if err != nil {
		return fmt.Errorf("listing sessions: %w", err)
	}
	for _, info := range sessions {
		if _, ok := pools[info.Template]; !ok {
			continue
		}
		if !poolSessionIsLiveInfo(info) || strings.TrimSpace(info.TriggerBeadID) != "" {
			continue
		}
		name := strings.TrimSpace(info.SessionName)
		if name == "" {
			name = info.ID
		}
		if idle[info.Template] == nil {
			idle[info.Template] = map[string]bool{}
		}
		idle[info.Template][name] = true
	}
	return nil
}

// unclaimedRoutedWork returns the sorted IDs of open, unassigned beads in store
// routed to template — a targeted gc.routed_to metadata lookup, never a
// full-store scan.
func unclaimedRoutedWork(store beads.Store, template string) ([]string, error) {
	// Live so bd's raw --status=open filter drops blocked/deferred rows
	// before mapBdStatus collapses them into "open" and the check reports
	// work the instance is correct to leave alone (same tradeoff as
	// listOpenForControllerDemandLive). FederatedReadTier because a
	// relocated class leg answers at exactly the tier asked.
	items, err := beads.HandlesFor(store).Live.List(beads.ListQuery{
		Status:   "open",
		TierMode: beads.FederatedReadTier,
		Metadata: map[string]string{beadmeta.RoutedToMetadataKey: template},
	})
	if err != nil {
		return nil, err
	}
	var beadIDs []string
	for _, b := range items {
		if strings.TrimSpace(b.Assignee) != "" || b.Status != "open" {
			continue
		}
		beadIDs = append(beadIDs, b.ID)
	}
	sort.Strings(beadIDs)
	return beadIDs, nil
}
