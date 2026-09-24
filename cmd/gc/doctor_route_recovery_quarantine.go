package main

import (
	"fmt"
	"sort"
	"strings"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/doctor"
)

// routeRecoveryQuarantineCheck surfaces the beads the controller's route-recovery
// lane could not converge.
//
// The lane restores gc.routed_to from the route a bead already carries, so the
// pool autoscaler — which keys only on gc.routed_to — can see it as demand
// (ga-n2d.4). Two outcomes are not repairs and must not be silent:
//
//   - recheck-failed: the open scan reports the bead as a candidate and the
//     authoritative live re-read refuses to confirm it, pass after pass. That is
//     the pre-ga-eld2x relic shape, and the lane marks it rather than re-reading
//     it forever.
//   - restore-flap: the restore SUCCEEDS every pass because a sibling lane clears
//     gc.routed_to again before the next one. The lane bounds its re-stamps and
//     stops, because a faster treadmill is not a fix; the bug is in the clearing
//     lane and this is where an operator finds out.
//
// Quarantine is a label, never a skip: the lane keeps re-evaluating the bead at
// backstop cadence and clears the marker itself the moment a re-check passes.
// --fix lifts it explicitly, which re-arms the lane's per-bead counters.
type routeRecoveryQuarantineCheck struct {
	cfg      *config.City
	cityPath string
	newStore func(string) (beads.Store, error)
}

func newRouteRecoveryQuarantineCheck(cfg *config.City, cityPath string, newStore func(string) (beads.Store, error)) *routeRecoveryQuarantineCheck {
	return &routeRecoveryQuarantineCheck{cfg: cfg, cityPath: cityPath, newStore: newStore}
}

func (c *routeRecoveryQuarantineCheck) Name() string { return "route-recovery-quarantine" }

func (c *routeRecoveryQuarantineCheck) CanFix() bool { return true }

func (c *routeRecoveryQuarantineCheck) WarmupEligible() bool { return false }

// quarantinedRoute is one marked bead and where it lives.
type quarantinedRoute struct {
	label  string
	store  beads.Store
	beadID string
	reason string
}

func (c *routeRecoveryQuarantineCheck) collect() (found []quarantinedRoute, skipped []string) {
	scopes := []struct{ label, path string }{{"city", c.cityPath}}
	if c.cfg != nil {
		for _, rig := range c.cfg.Rigs {
			if rig.Suspended || strings.TrimSpace(rig.Path) == "" {
				continue
			}
			scopes = append(scopes, struct{ label, path string }{"rig " + rig.Name, rig.Path})
		}
	}
	for _, sc := range scopes {
		if c.newStore == nil || strings.TrimSpace(sc.path) == "" {
			continue
		}
		store, err := c.newStore(sc.path)
		if err != nil {
			skipped = append(skipped, fmt.Sprintf("%s skipped: opening bead store: %v", sc.label, err))
			continue
		}
		// A targeted metadata query, not a scan: the marker is the index.
		items, err := store.List(beads.ListQuery{
			Metadata:      map[string]string{beadmeta.RouteQuarantineMetadataKey: "true"},
			IncludeClosed: true,
		})
		if err != nil {
			skipped = append(skipped, fmt.Sprintf("%s skipped: listing beads: %v", sc.label, err))
			continue
		}
		for _, b := range items {
			if !isRouteRecoveryQuarantined(b) {
				continue
			}
			reason := strings.TrimSpace(b.Metadata[beadmeta.RouteQuarantineReasonMetadataKey])
			if reason == "" {
				reason = "unknown"
			}
			found = append(found, quarantinedRoute{label: sc.label, store: store, beadID: b.ID, reason: reason})
		}
	}
	return found, skipped
}

func (c *routeRecoveryQuarantineCheck) Run(_ *doctor.CheckContext) *doctor.CheckResult {
	found, skipped := c.collect()
	if len(found) == 0 && len(skipped) == 0 {
		return okCheck(c.Name(), "no beads are quarantined by route recovery")
	}
	details := make([]string, 0, len(found)+len(skipped))
	flapping := 0
	for _, q := range found {
		if q.reason == routeRecoveryQuarantineRestoreFlap {
			flapping++
		}
		details = append(details, fmt.Sprintf("%s bead %s quarantined: %s", q.label, q.beadID, q.reason))
	}
	details = append(details, skipped...)
	sort.Strings(details)
	if len(found) == 0 {
		return warnCheck(c.Name(),
			fmt.Sprintf("route-recovery quarantine review skipped %d scope(s)", len(skipped)),
			"fix bead store access, then rerun gc doctor",
			details)
	}
	remedy := "investigate each bead's route, then run gc doctor --fix to lift the quarantine and let the controller re-evaluate"
	if flapping > 0 {
		// A flap is a defect in whatever clears gc.routed_to, not in the bead.
		remedy = "a sibling lane is clearing gc.routed_to on " + fmt.Sprintf("%d", flapping) +
			" bead(s); find and fix that lane, then run gc doctor --fix to lift the quarantine"
	}
	return warnCheck(c.Name(),
		fmt.Sprintf("%d bead(s) quarantined by route recovery", len(found)),
		remedy,
		details)
}

func (c *routeRecoveryQuarantineCheck) Fix(_ *doctor.CheckContext) error {
	found, skipped := c.collect()
	for _, q := range found {
		if err := q.store.SetMetadataBatch(q.beadID, map[string]string{
			beadmeta.RouteQuarantineMetadataKey:       "",
			beadmeta.RouteQuarantineReasonMetadataKey: "",
		}); err != nil {
			return fmt.Errorf("%s bead %s: lifting route-recovery quarantine: %w", q.label, q.beadID, err)
		}
	}
	if len(skipped) > 0 {
		return fmt.Errorf("route-recovery-quarantine skipped %d scope(s): %s", len(skipped), strings.Join(skipped, "; "))
	}
	return nil
}
