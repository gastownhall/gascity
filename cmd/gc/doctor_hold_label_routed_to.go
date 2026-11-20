package main

import (
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/doctor"
)

// holdLabelExternalValue is a hold:<value> value that never implies a routing
// gap: it names a human/out-of-system dependency, not an agent.
const holdLabelExternalValue = "external"

// holdLabelClearedValue is the other: `bd set-state <id> hold=cleared` releases
// a hold and leaves hold:cleared behind, so it means "no hold", not an agent.
const holdLabelClearedValue = "cleared"

// holdLabelRoutedToCheck detects beads carrying a hold:<value> label whose
// gc.routed_to metadata is missing or does not match <value>. gc.routed_to is
// the sole persisted routing key (ga-eld2x); a hold:<value> label with no
// matching gc.routed_to has silently drifted from its intended route. When
// <value> is the short form of a binding-qualified route
// (boundRoutedToAliases), that canonical route also matches, since it is the
// form v2-routed-to-namespace requires.
// --fix backfills gc.routed_to with that canonical route, or with the label
// value verbatim when <value> has no binding-qualified alias. A value bound
// under several bindings has no single repair, so --fix leaves it for manual
// resolution, matching v2-routed-to-namespace.
type holdLabelRoutedToCheck struct {
	cfg      *config.City
	cityPath string
	newStore func(string) (beads.Store, error)
}

func newHoldLabelRoutedToCheck(cfg *config.City, cityPath string, newStore func(string) (beads.Store, error)) *holdLabelRoutedToCheck {
	return &holdLabelRoutedToCheck{cfg: cfg, cityPath: cityPath, newStore: newStore}
}

func (c *holdLabelRoutedToCheck) Name() string { return "hold-label-routed-to" }

func (c *holdLabelRoutedToCheck) CanFix() bool { return true }

func (c *holdLabelRoutedToCheck) WarmupEligible() bool { return false }

// holdLabelValue returns the hold value carried by labels, if any
// hold:<value> label is present and <value> is not "external" or "cleared".
func holdLabelValue(labels []string) (string, bool) {
	for _, l := range labels {
		val, ok := strings.CutPrefix(l, "hold:")
		if !ok {
			continue
		}
		val = strings.TrimSpace(val)
		if val == "" || val == holdLabelExternalValue || val == holdLabelClearedValue {
			continue
		}
		return val, true
	}
	return "", false
}

// holdRouteTarget is a single bead whose hold:<value> label and gc.routed_to
// metadata have drifted apart. want is the hold value; canonicals holds every
// binding-qualified route it is a short form of (boundRoutedToAliases), empty
// when the value has no alias.
type holdRouteTarget struct {
	label      string
	store      beads.Store
	beadID     string
	want       string
	got        string
	canonicals []string
}

// route returns the gc.routed_to value that repairs t: the hold value itself
// when it has no canonical route, or its unique canonical route. It reports
// false when the hold value has several canonical routes, so no single
// repair exists.
func (t holdRouteTarget) route() (string, bool) {
	switch len(t.canonicals) {
	case 0:
		return t.want, true
	case 1:
		return t.canonicals[0], true
	default:
		return "", false
	}
}

func (t holdRouteTarget) describe() string {
	prefix := fmt.Sprintf("%s bead %s has hold:%s but gc.routed_to=%q", t.label, t.beadID, t.want, t.got)
	if route, ok := t.route(); ok {
		return fmt.Sprintf("%s; use %q", prefix, route)
	}
	return fmt.Sprintf("%s; use one of %s", prefix, strings.Join(t.canonicals, ", "))
}

func (c *holdLabelRoutedToCheck) collect() (targets []holdRouteTarget, skipped []string) {
	aliases := boundRoutedToAliases(c.cfg)
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
		// hold:<value> carries a dynamic value suffix, so no targeted
		// label/metadata query is possible; AllowScan is required for a
		// broad filter (internal/beads/query.go). Status is left unset so the
		// scan matches every non-closed bead (open, in_progress, blocked,
		// deferred, ...), not just "open" — an exact Status match would
		// silently hide hold:<value> drift on any other status (ga-fm2vgd.2).
		items, err := store.List(beads.ListQuery{AllowScan: true})
		if err != nil {
			skipped = append(skipped, fmt.Sprintf("%s skipped: listing beads: %v", sc.label, err))
			continue
		}
		for _, b := range items {
			want, ok := holdLabelValue(b.Labels)
			if !ok {
				continue
			}
			got := strings.TrimSpace(b.Metadata[beadmeta.RoutedToMetadataKey])
			canonicals := aliases[want]
			if got == want || slices.Contains(canonicals, got) {
				continue
			}
			targets = append(targets, holdRouteTarget{label: sc.label, store: store, beadID: b.ID, want: want, got: got, canonicals: canonicals})
		}
	}
	return targets, skipped
}

func (c *holdLabelRoutedToCheck) Run(_ *doctor.CheckContext) *doctor.CheckResult {
	targets, skipped := c.collect()
	if len(targets) == 0 && len(skipped) == 0 {
		return okCheck(c.Name(), "no hold:<value> labels are missing a matching gc.routed_to")
	}
	details := make([]string, 0, len(targets)+len(skipped))
	for _, tgt := range targets {
		details = append(details, tgt.describe())
	}
	details = append(details, skipped...)
	sort.Strings(details)
	if len(targets) == 0 {
		return warnCheck(c.Name(),
			fmt.Sprintf("hold-label-routed-to check skipped %d scope(s)", len(skipped)),
			"fix bead store access, then rerun gc doctor",
			details)
	}
	return warnCheck(c.Name(),
		fmt.Sprintf("%d bead(s) carry a hold:<value> label without matching gc.routed_to", len(targets)),
		"run gc doctor --fix to backfill gc.routed_to from the hold:<value> label, binding-qualified when the value names a bound agent; set gc.routed_to by hand where several routes are listed",
		details)
}

// Fix writes the repair route of every target that has one. Targets whose
// hold value names several bound routes are left untouched; doctor's
// verifying re-run keeps reporting them.
func (c *holdLabelRoutedToCheck) Fix(_ *doctor.CheckContext) error {
	targets, skipped := c.collect()
	for _, tgt := range targets {
		route, ok := tgt.route()
		if !ok {
			continue
		}
		if err := tgt.store.SetMetadata(tgt.beadID, beadmeta.RoutedToMetadataKey, route); err != nil {
			return fmt.Errorf("%s bead %s: backfill gc.routed_to: %w", tgt.label, tgt.beadID, err)
		}
	}
	if len(skipped) > 0 {
		return fmt.Errorf("hold-label-routed-to skipped %d scope(s): %s", len(skipped), strings.Join(skipped, "; "))
	}
	return nil
}
