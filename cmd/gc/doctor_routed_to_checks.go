package main

import (
	"fmt"
	"sort"
	"strings"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/doctor"
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

func (c *v2RoutedToNamespaceCheck) CanFix() bool { return false }

func (c *v2RoutedToNamespaceCheck) Fix(_ *doctor.CheckContext) error { return nil }

func (c *v2RoutedToNamespaceCheck) Run(_ *doctor.CheckContext) *doctor.CheckResult {
	aliases := boundRoutedToAliases(c.cfg)
	if len(aliases) == 0 {
		return okCheck(c.Name(), "no binding-qualified route targets configured")
	}

	var findings []string
	var skipped []string
	c.scanScope(&findings, &skipped, aliases, "city", c.cityPath)
	if c.cfg != nil {
		for _, rig := range c.cfg.Rigs {
			if rig.Suspended || strings.TrimSpace(rig.Path) == "" {
				continue
			}
			c.scanScope(&findings, &skipped, aliases, "rig "+rig.Name, rig.Path)
		}
	}

	if len(findings) == 0 && len(skipped) == 0 {
		return okCheck(c.Name(), "no short-form gc.routed_to values targeting bound agents found")
	}
	details := append([]string{}, findings...)
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
			"rewrite gc.routed_to to the binding-qualified agent name, fix skipped store access, then rerun gc doctor",
			details)
	}
	return warnCheck(c.Name(),
		fmt.Sprintf("%d short-form gc.routed_to value(s) target bound PackV2 agents", len(findings)),
		"rewrite gc.routed_to to the binding-qualified agent name, then rerun gc doctor",
		details)
}

func (c *v2RoutedToNamespaceCheck) scanScope(findings, skipped *[]string, aliases map[string][]string, label, path string) {
	if c.newStore == nil || strings.TrimSpace(path) == "" {
		return
	}
	store, err := c.newStore(path)
	if err != nil {
		*skipped = append(*skipped, fmt.Sprintf("%s skipped: opening bead store: %v", label, err))
		return
	}
	seen := make(map[string]bool)
	routes := make([]string, 0, len(aliases))
	for route := range aliases {
		routes = append(routes, route)
	}
	sort.Strings(routes)
	for _, route := range routes {
		items, err := store.List(beads.ListQuery{
			Metadata: map[string]string{"gc.routed_to": route},
		})
		if err != nil {
			*skipped = append(*skipped, fmt.Sprintf("%s skipped: listing beads: %v", label, err))
			return
		}
		for _, bead := range items {
			if seen[bead.ID] {
				continue
			}
			seen[bead.ID] = true
			c.scanRoutedToBead(findings, aliases, label, bead)
		}
	}
}

func (c *v2RoutedToNamespaceCheck) scanRoutedToBead(findings *[]string, aliases map[string][]string, label string, bead beads.Bead) {
	route := strings.TrimSpace(bead.Metadata["gc.routed_to"])
	if route == "" {
		return
	}
	canonicals, ok := aliases[route]
	if !ok {
		return
	}
	switch len(canonicals) {
	case 1:
		*findings = append(*findings, fmt.Sprintf("%s bead %s has gc.routed_to=%q; use %q", label, bead.ID, route, canonicals[0]))
	default:
		*findings = append(*findings, fmt.Sprintf("%s bead %s has gc.routed_to=%q; use one of %s", label, bead.ID, route, strings.Join(canonicals, ", ")))
	}
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
		addAlias(unboundRouteIdentity(agent), agent.QualifiedName())
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
