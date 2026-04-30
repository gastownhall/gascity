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

	var details []string
	c.scanScope(&details, aliases, "city", c.cityPath)
	if c.cfg != nil {
		for _, rig := range c.cfg.Rigs {
			if rig.Suspended || strings.TrimSpace(rig.Path) == "" {
				continue
			}
			c.scanScope(&details, aliases, "rig "+rig.Name, rig.Path)
		}
	}

	if len(details) == 0 {
		return okCheck(c.Name(), "no short-form gc.routed_to values targeting bound agents found")
	}
	sort.Strings(details)
	return warnCheck(c.Name(),
		fmt.Sprintf("%d short-form gc.routed_to value(s) target bound PackV2 agents", len(details)),
		"rewrite gc.routed_to to the binding-qualified agent name, then rerun gc doctor",
		details)
}

func (c *v2RoutedToNamespaceCheck) scanScope(details *[]string, aliases map[string][]string, label, path string) {
	if c.newStore == nil || strings.TrimSpace(path) == "" {
		return
	}
	store, err := c.newStore(path)
	if err != nil {
		return
	}
	items, err := store.List(beads.ListQuery{AllowScan: true})
	if err != nil {
		return
	}
	for _, bead := range items {
		route := strings.TrimSpace(bead.Metadata["gc.routed_to"])
		if route == "" {
			continue
		}
		canonicals, ok := aliases[route]
		if !ok {
			continue
		}
		switch len(canonicals) {
		case 1:
			*details = append(*details, fmt.Sprintf("%s bead %s has gc.routed_to=%q; use %q", label, bead.ID, route, canonicals[0]))
		default:
			*details = append(*details, fmt.Sprintf("%s bead %s has gc.routed_to=%q; use one of %s", label, bead.ID, route, strings.Join(canonicals, ", ")))
		}
	}
}

func boundRoutedToAliases(cfg *config.City) map[string][]string {
	aliases := map[string][]string{}
	if cfg == nil {
		return aliases
	}
	for i := range cfg.Agents {
		agent := cfg.Agents[i]
		if strings.TrimSpace(agent.BindingName) == "" {
			continue
		}
		short := unboundRouteIdentity(agent)
		canonical := strings.TrimSpace(agent.QualifiedName())
		if short == "" || canonical == "" || short == canonical {
			continue
		}
		aliases[short] = appendUniqueString(aliases[short], canonical)
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

func appendUniqueString(values []string, value string) []string {
	for _, existing := range values {
		if existing == value {
			return values
		}
	}
	return append(values, value)
}
