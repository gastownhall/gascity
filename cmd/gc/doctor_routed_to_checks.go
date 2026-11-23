package main

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/gastownhall/gascity/internal/agentutil"
	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/doctor"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/suspensionstate"
)

type v2RoutedToNamespaceCheck struct {
	cfg      *config.City
	cityPath string
	newStore func(string) (beads.Store, error)
}

func newV2RoutedToNamespaceCheck(cfg *config.City, cityPath string, newStore func(string) (beads.Store, error)) *v2RoutedToNamespaceCheck {
	return &v2RoutedToNamespaceCheck{cfg: cfg, cityPath: cityPath, newStore: newStore}
}

func (c *v2RoutedToNamespaceCheck) Name() string { return "v2-routed-to-namespace" }

func (c *v2RoutedToNamespaceCheck) CanFix() bool { return true }

// routedToDriftFinding is a single bead whose gc.routed_to value is a short
// (binding-unqualified) form of a route that is bound in this city's config,
// and so needs rewriting to the binding-qualified canonical form. canonicals
// holds every distinct qualified route that short form could mean; when more
// than one candidate exists there is no single unambiguous rewrite target, so
// Fix leaves it for manual resolution.
type routedToDriftFinding struct {
	label      string
	store      beads.Store
	beadID     string
	route      string
	canonicals []string
}

func (f routedToDriftFinding) describe() string {
	if len(f.canonicals) == 1 {
		return fmt.Sprintf("%s bead %s has gc.routed_to=%q; use %q", f.label, f.beadID, f.route, f.canonicals[0])
	}
	return fmt.Sprintf("%s bead %s has gc.routed_to=%q; use one of %s", f.label, f.beadID, f.route, strings.Join(f.canonicals, ", "))
}

func (c *v2RoutedToNamespaceCheck) Run(_ *doctor.CheckContext) *doctor.CheckResult {
	aliases := boundRoutedToAliases(c.cfg)
	if len(aliases) == 0 {
		return okCheck(c.Name(), "no binding-qualified route targets configured")
	}

	findings, skipped := c.collect(aliases)
	if len(findings) == 0 && len(skipped) == 0 {
		return okCheck(c.Name(), "no short-form gc.routed_to values targeting bound agents found")
	}
	details := make([]string, 0, len(findings)+len(skipped))
	for _, f := range findings {
		details = append(details, f.describe())
	}
	details = append(details, skipped...)
	sort.Strings(details)
	if len(findings) == 0 {
		return warnCheck(c.Name(),
			fmt.Sprintf("v2 routed_to namespace check skipped %d scope(s)", len(skipped)),
			"fix bead store access, then rerun gc doctor",
			details)
	}
	if len(skipped) > 0 {
		return warnCheck(c.Name(),
			fmt.Sprintf("%d short-form gc.routed_to value(s) target bound PackV2 agents; %d scope(s) skipped", len(findings), len(skipped)),
			"run gc doctor --fix to rewrite the unambiguous ones, fix skipped store access, then rerun gc doctor",
			details)
	}
	return warnCheck(c.Name(),
		fmt.Sprintf("%d short-form gc.routed_to value(s) target bound PackV2 agents", len(findings)),
		"run gc doctor --fix to rewrite gc.routed_to to the binding-qualified agent name, then rerun gc doctor",
		details)
}

// Fix rewrites every unambiguous finding's gc.routed_to to its canonical
// binding-qualified form. Findings with more than one candidate canonical
// (boundRoutedToAliases could not resolve a single rewrite target) are left
// untouched for manual resolution. Fix is partial-failure-tolerant: a
// SetMetadata error on one bead, or a store this check could not scan, does
// not stop it from attempting every other unambiguous finding — every error
// encountered is accumulated and returned together via errors.Join.
func (c *v2RoutedToNamespaceCheck) Fix(_ *doctor.CheckContext) error {
	aliases := boundRoutedToAliases(c.cfg)
	if len(aliases) == 0 {
		return nil
	}
	findings, skipped := c.collect(aliases)
	var errs []error
	for _, f := range findings {
		if len(f.canonicals) != 1 {
			continue
		}
		if err := f.store.SetMetadata(f.beadID, beadmeta.RoutedToMetadataKey, f.canonicals[0]); err != nil {
			errs = append(errs, fmt.Errorf("%s bead %s: rewrite gc.routed_to to %q: %w", f.label, f.beadID, f.canonicals[0], err))
		}
	}
	if len(skipped) > 0 {
		errs = append(errs, fmt.Errorf("v2-routed-to-namespace skipped %d scope(s): %s", len(skipped), strings.Join(skipped, "; ")))
	}
	return errors.Join(errs...)
}

// collect scans every in-scope bead store for beads whose gc.routed_to names
// a short form present in aliases. Callers must only call this with a
// non-empty aliases map (both Run and Fix short-circuit before calling it
// otherwise).
//
// Each store is listed once and matched in memory, so the number of store
// reads does not grow with the number of routes; a store path shared by two
// scopes is listed once and matched for each of them.
func (c *v2RoutedToNamespaceCheck) collect(aliases map[string][]string) (findings []routedToDriftFinding, skipped []string) {
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
	type listing struct {
		store beads.Store
		items []beads.Bead
		err   error
	}
	listed := map[string]listing{}
	for _, sc := range scopes {
		if c.newStore == nil || strings.TrimSpace(sc.path) == "" {
			continue
		}
		key := normalizePathForCompare(sc.path)
		got, ok := listed[key]
		if !ok {
			store, err := c.newStore(sc.path)
			if err != nil {
				skipped = append(skipped, fmt.Sprintf("%s skipped: opening bead store: %v", sc.label, err))
				continue
			}
			items, err := listRoutedToCandidates(store)
			got = listing{store: store, items: items, err: err}
			listed[key] = got
		}
		if got.err != nil {
			skipped = append(skipped, fmt.Sprintf("%s skipped: listing beads: %v", sc.label, got.err))
			continue
		}
		findings = append(findings, routedToDriftFindings(got.store, got.items, aliases, sc.label)...)
	}
	return findings, skipped
}

// listRoutedToCandidates reads every non-closed bead in store, both tiers:
// one listing, where a targeted read per route cost two bd forks per route
// per store (the filtered issues list plus an unfilterable wisp query) and
// grew with the city's bound routes.
func listRoutedToCandidates(store beads.Store) ([]beads.Bead, error) {
	return store.List(beads.ListQuery{AllowScan: true})
}

// routedToDriftFindings returns every distinct bead in items whose
// gc.routed_to is exactly a short form in aliases — the same exact match the
// store's metadata filter applies.
func routedToDriftFindings(store beads.Store, items []beads.Bead, aliases map[string][]string, label string) []routedToDriftFinding {
	var findings []routedToDriftFinding
	seen := make(map[string]bool)
	for _, bead := range items {
		route := bead.Metadata[beadmeta.RoutedToMetadataKey]
		canonicals, ok := aliases[route]
		if !ok || seen[bead.ID] {
			continue
		}
		seen[bead.ID] = true
		findings = append(findings, routedToDriftFinding{
			label:      label,
			store:      store,
			beadID:     bead.ID,
			route:      route,
			canonicals: canonicals,
		})
	}
	return findings
}

func boundRoutedToAliases(cfg *config.City) map[string][]string {
	aliases := map[string][]string{}
	if cfg == nil {
		return aliases
	}
	unbound := unboundRoutedToIdentities(cfg)
	addAlias := func(short, canonical string) {
		short = strings.TrimSpace(short)
		canonical = strings.TrimSpace(canonical)
		if short == "" || canonical == "" || short == canonical || unbound[short] {
			return
		}
		aliases[short] = appendUniqueString(aliases[short], canonical)
	}
	for i := range cfg.Agents {
		agent := cfg.Agents[i]
		if strings.TrimSpace(agent.BindingName) == "" {
			continue
		}
		addAlias(unboundRouteIdentity(agent), agentutil.RoutedToIdentity(&agent))
	}
	for i := range cfg.NamedSessions {
		session := cfg.NamedSessions[i]
		if strings.TrimSpace(session.BindingName) == "" {
			continue
		}
		addAlias(unboundNamedSessionRouteIdentity(session), session.QualifiedName())
	}
	for key := range aliases {
		sort.Strings(aliases[key])
	}
	return aliases
}

func unboundRouteIdentity(agent config.Agent) string {
	name := strings.TrimSpace(agent.Name)
	if name == "" {
		return ""
	}
	dir := strings.TrimSpace(agent.Dir)
	if dir == "" {
		return name
	}
	return dir + "/" + name
}

func unboundRoutedToIdentities(cfg *config.City) map[string]bool {
	identities := map[string]bool{}
	for i := range cfg.Agents {
		agent := cfg.Agents[i]
		if strings.TrimSpace(agent.BindingName) != "" {
			continue
		}
		if identity := unboundRouteIdentity(agent); identity != "" {
			identities[identity] = true
		}
	}
	for i := range cfg.NamedSessions {
		session := cfg.NamedSessions[i]
		if strings.TrimSpace(session.BindingName) != "" {
			continue
		}
		if identity := unboundNamedSessionRouteIdentity(session); identity != "" {
			identities[identity] = true
		}
	}
	return identities
}

func unboundNamedSessionRouteIdentity(session config.NamedSession) string {
	name := strings.TrimSpace(session.Name)
	if name == "" {
		name = strings.TrimSpace(session.Template)
	}
	if name == "" {
		return ""
	}
	dir := strings.TrimSpace(session.Dir)
	if dir == "" {
		return name
	}
	return dir + "/" + name
}

func appendUniqueString(values []string, value string) []string {
	for _, existing := range values {
		if existing == value {
			return values
		}
	}
	return append(values, value)
}
