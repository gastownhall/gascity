package main

import (
	"errors"
	"fmt"
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
// gc.routed_to metadata does not name the same actor. A hold label names its
// next actor by bare value ("mayor"); gc.routed_to is the sole persisted
// routing key (ga-eld2x) and names actors by their configured route
// identity, which in a city that imports the actor through a pack binding is
// binding-qualified ("gastown.mayor"). The comparison therefore goes through
// boundRoutedToAliases — the same short-form→canonical map the
// v2-routed-to-namespace check rewrites from — so a correctly routed bead
// never reads as drifted, while the bare label suffix stays accepted where
// it is itself a live route identity (rewriting short forms is the
// namespace check's jurisdiction, not this one's).
// --fix backfills gc.routed_to from the label value, preferring the
// binding-qualified identity when the bare value resolves to exactly one;
// backfilling the bare suffix in a bound city would stamp an unroutable
// identity over the very route this check exists to protect.
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
// metadata have drifted apart. routes holds the binding-qualified route
// identities want is a bound short form of, if any (several when the short
// form is ambiguous).
type holdRouteTarget struct {
	label  string
	store  beads.Store
	beadID string
	want   string
	got    string
	routes []string
}

// holdRouteMatches reports whether got names the same actor a hold:<value>
// label names: the bare value itself — exact, which also covers unbound
// agents, whose route identity IS the bare name — or any binding-qualified
// identity the value is a bound short form of.
func holdRouteMatches(got, want string, routes []string) bool {
	if got == want {
		return true
	}
	for _, route := range routes {
		if got == route {
			return true
		}
	}
	return false
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
			routes := aliases[want]
			if holdRouteMatches(got, want, routes) {
				continue
			}
			targets = append(targets, holdRouteTarget{label: sc.label, store: store, beadID: b.ID, want: want, got: got, routes: routes})
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
		details = append(details, fmt.Sprintf("%s bead %s has hold:%s but gc.routed_to=%q", tgt.label, tgt.beadID, tgt.want, tgt.got))
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
		"run gc doctor --fix to backfill gc.routed_to from the hold:<value> label",
		details)
}

// Fix backfills each drifted bead's gc.routed_to from its hold label value,
// preferring the single binding-qualified identity when the bare value is a
// bound short form. When the value is bound to several identities the
// intended route is genuinely ambiguous: those beads are reported rather
// than guessed at, mirroring v2-routed-to-namespace's multi-canonical
// policy. Fix is partial-failure-tolerant like that check: a SetMetadata
// error on one bead, or a store this check could not scan, does not stop it
// from attempting every other repair — every error encountered is
// accumulated and returned together via errors.Join.
func (c *holdLabelRoutedToCheck) Fix(_ *doctor.CheckContext) error {
	targets, skipped := c.collect()
	var errs []error
	for _, tgt := range targets {
		value := tgt.want
		switch {
		case len(tgt.routes) == 1:
			value = tgt.routes[0]
		case len(tgt.routes) > 1:
			errs = append(errs, fmt.Errorf("%s bead %s: hold:%s is bound to several route identities (%s); set gc.routed_to to the intended one", tgt.label, tgt.beadID, tgt.want, strings.Join(tgt.routes, ", ")))
			continue
		}
		if err := tgt.store.SetMetadata(tgt.beadID, beadmeta.RoutedToMetadataKey, value); err != nil {
			errs = append(errs, fmt.Errorf("%s bead %s: backfill gc.routed_to: %w", tgt.label, tgt.beadID, err))
		}
	}
	if len(skipped) > 0 {
		errs = append(errs, fmt.Errorf("hold-label-routed-to skipped %d scope(s): %s", len(skipped), strings.Join(skipped, "; ")))
	}
	return errors.Join(errs...)
}
