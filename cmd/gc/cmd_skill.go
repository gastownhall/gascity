package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/materialize"
	"github.com/spf13/cobra"
)

func newSkillCmd(stdout, stderr io.Writer) *cobra.Command {
	var cmd *cobra.Command
	cmd = &cobra.Command{
		Use:   "skill",
		Short: "List visible skills",
		Long: `List skills visible to the current city.

Output includes:
  - City pack skills (skills/<name>/SKILL.md under the city root)
  - Imported pack shared skills (binding-qualified, e.g. ops.code-review)
  - Compatibility bootstrap skills, when legacy implicit imports still exist
  - With --agent/--session: that agent's agents/<name>/skills/ catalog

The listing is a diagnostic view of what's *available*. It does not
collapse precedence, filter to agents whose provider has a vendor
sink, or predict exactly which entries the materializer will pick on
name collision. For the materialized set, inspect the
<scope-root>/.<vendor>/skills/ sink after "gc start" or run
"gc doctor" to surface collisions.`,
		Args: cobra.ArbitraryArgs,
		RunE: func(_ *cobra.Command, args []string) error {
			if len(args) == 0 {
				return cmd.Help()
			}
			fmt.Fprintf(stderr, "gc skill: unknown subcommand %q\n", args[0]) //nolint:errcheck // best-effort stderr
			return errExit
		},
	}
	cmd.AddCommand(newSkillListCmd(stdout, stderr))
	return cmd
}

func newSkillListCmd(stdout, stderr io.Writer) *cobra.Command {
	var agentName string
	var sessionID string
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List visible skills",
		Long:  "List the current shared and agent-local visible skills, optionally scoped to an agent or session.",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			if strings.TrimSpace(agentName) != "" && strings.TrimSpace(sessionID) != "" {
				fmt.Fprintln(stderr, "gc skill list: --agent and --session are mutually exclusive") //nolint:errcheck // best-effort stderr
				return errExit
			}
			cityPath, err := resolveCity()
			if err != nil {
				fmt.Fprintf(stderr, "gc skill list: %v\n", err) //nolint:errcheck // best-effort stderr
				return errExit
			}
			cfg, err := loadCityConfig(cityPath, stderr)
			if err != nil {
				fmt.Fprintf(stderr, "gc skill list: %v\n", err) //nolint:errcheck // best-effort stderr
				return errExit
			}

			var store beads.Store
			if strings.TrimSpace(sessionID) != "" {
				store, err = openCityStoreAt(cityPath)
				if err != nil {
					fmt.Fprintf(stderr, "gc skill list: %v\n", err) //nolint:errcheck // best-effort stderr
					return errExit
				}
			}

			entries, err := listVisibleSkillEntries(cityPath, cfg, store, agentName, sessionID)
			if err != nil {
				fmt.Fprintf(stderr, "gc skill list: %v\n", err) //nolint:errcheck // best-effort stderr
				return errExit
			}
			writeVisibilityEntries(stdout, entries)
			return nil
		},
	}
	cmd.Flags().StringVar(&agentName, "agent", "", "show the effective skill view for this agent")
	cmd.Flags().StringVar(&sessionID, "session", "", "show the effective skill view for this session")
	return cmd
}

func listVisibleSkillEntries(cityPath string, cfg *config.City, store beads.Store, agentName, sessionID string) ([]visibilityEntry, error) {
	entries := discoverSkillEntries(cityPath, "city")
	// Legacy implicit-import compatibility packs may still contribute
	// shared skills on upgraded installs. Keep surfacing them here while
	// the compatibility path exists; normal launch-time system packs come
	// from .gc/system/packs and are not part of this listing.
	entries = append(entries, discoverBootstrapSkillEntries()...)
	if strings.TrimSpace(agentName) == "" && strings.TrimSpace(sessionID) == "" {
		entries = append(entries, discoverImportedSkillEntries(sharedSkillCatalogInputs(cfg, currentRigContext(cfg)))...)
		sortVisibilityEntries(entries)
		return entries, nil
	}
	agent, err := resolveVisibilityAgent(cityPath, cfg, store, agentName, sessionID)
	if err != nil {
		return nil, err
	}
	// Every agent sees the entire shared catalog plus its own agent-local
	// skills. No attachment filtering.
	entries = append(entries, discoverImportedSkillEntries(sharedSkillCatalogInputs(cfg, agentRigScopeName(agent, cfg.Rigs)))...)
	entries = append(entries, discoverAgentSkillEntries(agentAssetRoot(cityPath, agent), agent.Name, "agent")...)
	sortVisibilityEntries(entries)
	return entries, nil
}

// discoverBootstrapSkillEntries enumerates skills that come from any
// legacy implicit-import compatibility packs. It normally returns an
// empty slice on the gc import launch path because BootstrapPacks is
// empty, but older upgraded installs may still carry compatibility
// state.
//
// Each returned entry's Source field is the compatibility pack name.
// Path is the absolute filesystem path to the SKILL.md file because
// compatibility packs live under the user-global cache, not the city
// directory.
func discoverBootstrapSkillEntries() []visibilityEntry {
	// LoadCityCatalog("") skips the city-pack walk and returns only the
	// compatibility bootstrap entries plus any explicit imported
	// catalogs passed by the caller. Using it here keeps the listing in
	// sync with the materializer's shared-catalog discovery.
	cat, err := materialize.LoadCityCatalog("")
	if err != nil {
		return nil
	}
	out := make([]visibilityEntry, 0, len(cat.Entries))
	for _, e := range cat.Entries {
		out = append(out, visibilityEntry{
			Name:   e.Name,
			Source: e.Origin,
			Path:   filepath.ToSlash(filepath.Join(e.Source, "SKILL.md")),
		})
	}
	return out
}

type visibilityEntry struct {
	Name   string
	Source string
	Path   string
}

func resolveVisibilityAgent(cityPath string, cfg *config.City, store beads.Store, agentName, sessionID string) (*config.Agent, error) {
	switch {
	case strings.TrimSpace(agentName) != "":
		resolved, ok := resolveAgentIdentity(cfg, agentName, currentRigContext(cfg))
		if !ok {
			return nil, fmt.Errorf("unknown agent %q", agentName)
		}
		template := resolveAgentTemplate(resolved.QualifiedName(), cfg)
		agent := findAgentByTemplate(cfg, template)
		if agent == nil {
			return nil, fmt.Errorf("unknown agent %q", agentName)
		}
		return agent, nil
	case strings.TrimSpace(sessionID) != "":
		if store == nil {
			return nil, fmt.Errorf("session store unavailable")
		}
		id, err := resolveSessionIDAllowClosedWithConfig(cityPath, cfg, store, sessionID)
		if err != nil {
			return nil, err
		}
		bead, err := store.Get(id)
		if err != nil {
			return nil, fmt.Errorf("loading session %q: %w", sessionID, err)
		}
		template := normalizedSessionTemplate(bead, cfg)
		if template == "" {
			template = strings.TrimSpace(bead.Metadata["agent_name"])
		}
		template = resolveAgentTemplate(template, cfg)
		agent := findAgentByTemplate(cfg, template)
		if agent == nil {
			return nil, fmt.Errorf("session %q maps to unknown agent template %q", sessionID, template)
		}
		return agent, nil
	default:
		return nil, nil
	}
}

func agentAssetRoot(cityPath string, agent *config.Agent) string {
	if agent != nil && strings.TrimSpace(agent.SourceDir) != "" {
		return agent.SourceDir
	}
	return cityPath
}

func discoverSkillEntries(root, source string) []visibilityEntry {
	return discoverSkillDirEntries(filepath.Join(root, "skills"), "skills", source)
}

func discoverImportedSkillEntries(catalogs []config.DiscoveredSkillCatalog) []visibilityEntry {
	var out []visibilityEntry
	for _, catalog := range catalogs {
		source := strings.TrimSpace(catalog.BindingName)
		if source == "" {
			source = strings.TrimSpace(catalog.PackName)
		}
		entries, err := os.ReadDir(catalog.SourceDir)
		if err != nil {
			continue
		}
		for _, entry := range entries {
			if !entry.IsDir() {
				continue
			}
			name := entry.Name()
			if strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_") {
				continue
			}
			skillPath := filepath.Join(catalog.SourceDir, name, "SKILL.md")
			if info, err := os.Stat(skillPath); err != nil || info.IsDir() {
				continue
			}
			publicName := name
			if catalog.BindingName != "" {
				publicName = catalog.BindingName + "." + name
			}
			out = append(out, visibilityEntry{
				Name:   publicName,
				Source: source,
				Path:   filepath.ToSlash(skillPath),
			})
		}
	}
	sortVisibilityEntries(out)
	return out
}

func discoverAgentSkillEntries(root, agentName, source string) []visibilityEntry {
	return discoverSkillDirEntries(filepath.Join(root, "agents", agentName, "skills"), filepath.Join("agents", agentName, "skills"), source)
}

func discoverSkillDirEntries(dir, relBase, source string) []visibilityEntry {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []visibilityEntry
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		name := entry.Name()
		if strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_") {
			continue
		}
		skillPath := filepath.Join(dir, name, "SKILL.md")
		if info, err := os.Stat(skillPath); err != nil || info.IsDir() {
			continue
		}
		out = append(out, visibilityEntry{
			Name:   name,
			Source: source,
			Path:   filepath.ToSlash(filepath.Join(relBase, name, "SKILL.md")),
		})
	}
	sortVisibilityEntries(out)
	return out
}

func sortVisibilityEntries(entries []visibilityEntry) {
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].Source != entries[j].Source {
			return entries[i].Source < entries[j].Source
		}
		if entries[i].Name != entries[j].Name {
			return entries[i].Name < entries[j].Name
		}
		return entries[i].Path < entries[j].Path
	})
}

func writeVisibilityEntries(w io.Writer, entries []visibilityEntry) {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tFROM\tPATH") //nolint:errcheck // best-effort
	for _, entry := range entries {
		fmt.Fprintf(tw, "%s\t%s\t%s\n", entry.Name, entry.Source, entry.Path) //nolint:errcheck // best-effort
	}
	_ = tw.Flush()
}
