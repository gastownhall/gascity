package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/formula"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/workspacesvc"
	"github.com/spf13/cobra"
)

func newConfigCmd(stdout, stderr io.Writer) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "config",
		Short: "Inspect and validate city configuration",
		Long: `Inspect, validate, and debug the resolved city configuration.

The config system supports multi-file composition with includes,
packs, patches, and overrides. Use "show" to dump the resolved
config and "explain" to see where each value originated.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return cmd.Help()
		},
	}
	cmd.AddCommand(newConfigShowCmd(stdout, stderr))
	cmd.AddCommand(newConfigExplainCmd(stdout, stderr))
	return cmd
}

func newConfigShowCmd(stdout, stderr io.Writer) *cobra.Command {
	var validate bool
	var showProvenance bool
	cmd := &cobra.Command{
		Use:   "show",
		Short: "Dump the resolved city configuration as TOML",
		Long: `Dump the fully resolved city configuration as TOML.

Loads city.toml with all includes, packs, patches, and overrides,
then outputs the merged result. Use --validate to check for errors
without printing. Use --provenance to see which file contributed each
config element. Use -f to layer additional config files.`,
		Example: `  gc config show
  gc config show --validate
  gc config show --provenance
  gc config show -f overlay.toml`,
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			if doConfigShow(validate, showProvenance, stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&validate, "validate", false, "validate config and exit (0 = valid, 1 = errors)")
	cmd.Flags().BoolVar(&showProvenance, "provenance", false, "show where each config element originated")
	cmd.Flags().StringArrayVarP(&extraConfigFiles, "file", "f", nil,
		"additional config files to layer (can be repeated)")
	return cmd
}

// doConfigShow loads city.toml (with includes) and dumps the resolved
// config, validates it, or shows provenance.
func doConfigShow(validate, showProvenance bool, stdout, stderr io.Writer) int {
	cityPath, err := resolveCity()
	if err != nil {
		fmt.Fprintf(stderr, "gc config show: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}

	// Auto-fetch remote packs before full config load.
	if quickCfg, qErr := config.Load(fsys.OSFS{}, filepath.Join(cityPath, "city.toml")); qErr == nil && len(quickCfg.Packs) > 0 {
		if fErr := config.FetchPacks(quickCfg.Packs, cityPath); fErr != nil {
			fmt.Fprintf(stderr, "gc config show: fetching packs: %v\n", fErr) //nolint:errcheck // best-effort stderr
			return 1
		}
	}

	cfg, prov, err := config.LoadWithIncludes(fsys.OSFS{}, filepath.Join(cityPath, "city.toml"), extraConfigFiles...)
	if err != nil {
		fmt.Fprintf(stderr, "gc config show: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}

	// Composition warnings.
	for _, w := range prov.Warnings {
		fmt.Fprintf(stderr, "gc config show: warning: %s\n", w) //nolint:errcheck // best-effort stderr
	}

	// Run validation.
	var validationErrors []string
	if err := config.ValidateAgents(cfg.Agents); err != nil {
		validationErrors = append(validationErrors, err.Error())
	}
	if err := config.ValidateRigs(cfg.Rigs, config.EffectiveHQPrefix(cfg)); err != nil {
		validationErrors = append(validationErrors, err.Error())
	}
	if err := config.ValidateServices(cfg.Services); err != nil {
		validationErrors = append(validationErrors, err.Error())
	} else if err := workspacesvc.ValidateRuntimeSupport(cfg.Services); err != nil {
		validationErrors = append(validationErrors, err.Error())
	}
	validationErrors = append(validationErrors, validateLegacyFormulaConfigRoutes(cfg)...)
	validationWarnings := singletonSessionMigrationWarnings(cfg)

	if validate {
		for _, w := range validationWarnings {
			fmt.Fprintf(stderr, "gc config show: warning: %s\n", w) //nolint:errcheck // best-effort stderr
		}
		if len(validationErrors) > 0 {
			for _, e := range validationErrors {
				fmt.Fprintf(stderr, "gc config show: %s\n", e) //nolint:errcheck // best-effort stderr
			}
			return 1
		}
		fmt.Fprintln(stdout, "Config valid.") //nolint:errcheck // best-effort stdout
		return 0
	}

	// Print validation warnings even in show mode.
	for _, w := range validationWarnings {
		fmt.Fprintf(stderr, "gc config show: warning: %s\n", w) //nolint:errcheck // best-effort stderr
	}
	for _, e := range validationErrors {
		fmt.Fprintf(stderr, "gc config show: warning: %s\n", e) //nolint:errcheck // best-effort stderr
	}

	if showProvenance {
		printProvenance(prov, stdout)
		return 0
	}

	data, err := cfg.Marshal()
	if err != nil {
		fmt.Fprintf(stderr, "gc config show: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	fmt.Fprint(stdout, string(data)) //nolint:errcheck // best-effort stdout
	return 0
}

func newConfigExplainCmd(stdout, stderr io.Writer) *cobra.Command {
	var rigFilter string
	var agentFilter string
	cmd := &cobra.Command{
		Use:   "explain",
		Short: "Show resolved agent config with provenance annotations",
		Long: `Show the resolved configuration for each agent with provenance.

Displays every resolved field with an annotation showing which config
file provided the value. Use --rig and --agent to filter the output.
Useful for debugging config composition and understanding override
resolution.`,
		Example: `  gc config explain
  gc config explain --agent mayor
  gc config explain --rig my-project
  gc config explain -f overlay.toml --agent polecat`,
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			if doConfigExplain(rigFilter, agentFilter, stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&rigFilter, "rig", "", "filter to agents in this rig")
	cmd.Flags().StringVar(&agentFilter, "agent", "", "filter to a specific agent name")
	cmd.Flags().StringArrayVarP(&extraConfigFiles, "file", "f", nil,
		"additional config files to layer (can be repeated)")
	return cmd
}

func singletonSessionMigrationWarnings(cfg *config.City) []string {
	if cfg == nil {
		return nil
	}
	cityName := cfg.EffectiveCityName()
	namedByTemplate := make(map[string]bool, len(cfg.NamedSessions))
	for i := range cfg.NamedSessions {
		spec, ok := findNamedSessionSpec(cfg, cityName, cfg.NamedSessions[i].QualifiedName())
		if !ok {
			continue
		}
		namedByTemplate[namedSessionBackingTemplate(spec)] = true
	}
	var warnings []string
	for i := range cfg.Agents {
		agentCfg := &cfg.Agents[i]
		if m := agentCfg.EffectiveMaxActiveSessions(); m == nil || *m != 1 {
			continue
		}
		if namedByTemplate[agentCfg.QualifiedName()] {
			continue
		}
		warnings = append(warnings,
			fmt.Sprintf("agent %q: max_active_sessions=1 now limits ephemeral capacity but does not create a persistent singleton; declare [[named_session]] for a canonical session identity", agentCfg.QualifiedName()))
	}
	sort.Strings(warnings)
	return warnings
}

func validateLegacyFormulaConfigRoutes(cfg *config.City) []string {
	if cfg == nil {
		return nil
	}
	paths := formulaValidationPaths(cfg)
	if len(paths) == 0 {
		return nil
	}
	parser := formula.NewParser(paths...)
	formulaNames := discoverFormulaNames(paths)
	agentTargets, namedTargets := formulaValidationTargets(cfg)
	var errs []string
	for _, name := range formulaNames {
		loaded, err := parser.LoadByName(name)
		if err != nil {
			errs = append(errs, fmt.Sprintf("formula %q: %v", name, err))
			continue
		}
		resolved, err := parser.Resolve(loaded)
		if err != nil {
			errs = append(errs, fmt.Sprintf("formula %q: %v", name, err))
			continue
		}
		if resolved.Version < 2 || resolved.Type != formula.TypeWorkflow {
			continue
		}
		collectLegacyGraphAssigneeErrors(name, resolved.Steps, agentTargets, namedTargets, &errs)
	}
	sort.Strings(errs)
	return errs
}

func formulaValidationPaths(cfg *config.City) []string {
	seen := make(map[string]struct{})
	var paths []string
	add := func(candidates []string) {
		for _, p := range candidates {
			if strings.TrimSpace(p) == "" {
				continue
			}
			if _, ok := seen[p]; ok {
				continue
			}
			seen[p] = struct{}{}
			paths = append(paths, p)
		}
	}
	add(cfg.FormulaLayers.City)
	for _, rigPaths := range cfg.FormulaLayers.Rigs {
		add(rigPaths)
	}
	return paths
}

func discoverFormulaNames(paths []string) []string {
	seen := make(map[string]struct{})
	var names []string
	for _, dir := range paths {
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, entry := range entries {
			if entry.IsDir() {
				continue
			}
			name, ok := formula.TrimTOMLFilename(entry.Name())
			if !ok {
				continue
			}
			if _, dup := seen[name]; dup {
				continue
			}
			seen[name] = struct{}{}
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names
}

func formulaValidationTargets(cfg *config.City) (map[string]struct{}, map[string]struct{}) {
	agentTargets := make(map[string]struct{}, len(cfg.Agents)*2)
	for i := range cfg.Agents {
		agentTargets[cfg.Agents[i].Name] = struct{}{}
		agentTargets[cfg.Agents[i].QualifiedName()] = struct{}{}
	}
	namedTargets := make(map[string]struct{}, len(cfg.NamedSessions)*2)
	for i := range cfg.NamedSessions {
		namedTargets[cfg.NamedSessions[i].IdentityName()] = struct{}{}
		namedTargets[cfg.NamedSessions[i].QualifiedName()] = struct{}{}
	}
	return agentTargets, namedTargets
}

func collectLegacyGraphAssigneeErrors(
	formulaName string,
	steps []*formula.Step,
	agentTargets map[string]struct{},
	namedTargets map[string]struct{},
	errs *[]string,
) {
	for _, step := range steps {
		if step == nil {
			continue
		}
		target := strings.TrimSpace(step.Assignee)
		if target != "" && !strings.Contains(target, "{") {
			if _, ok := namedTargets[target]; !ok {
				if _, ok := agentTargets[target]; ok {
					*errs = append(*errs,
						fmt.Sprintf("formula %q step %q: assignee=%q now requires a concrete session target; use metadata.gc.run_target for config routing", formulaName, step.ID, target))
				}
			}
		}
		collectLegacyGraphAssigneeErrors(formulaName, step.Children, agentTargets, namedTargets, errs)
	}
}

// doConfigExplain shows the resolved config for agents with provenance
// annotations showing where each value originated.
func doConfigExplain(rigFilter, agentFilter string, stdout, stderr io.Writer) int {
	cityPath, err := resolveCity()
	if err != nil {
		fmt.Fprintf(stderr, "gc config explain: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}

	// Auto-fetch remote packs before full config load.
	if quickCfg, qErr := config.Load(fsys.OSFS{}, filepath.Join(cityPath, "city.toml")); qErr == nil && len(quickCfg.Packs) > 0 {
		if fErr := config.FetchPacks(quickCfg.Packs, cityPath); fErr != nil {
			fmt.Fprintf(stderr, "gc config explain: fetching packs: %v\n", fErr) //nolint:errcheck // best-effort stderr
			return 1
		}
	}

	cfg, prov, err := config.LoadWithIncludes(fsys.OSFS{}, filepath.Join(cityPath, "city.toml"), extraConfigFiles...)
	if err != nil {
		fmt.Fprintf(stderr, "gc config explain: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}

	// Filter agents.
	var agents []config.Agent
	for _, a := range cfg.Agents {
		if rigFilter != "" && a.Dir != rigFilter {
			continue
		}
		if agentFilter != "" && a.Name != agentFilter {
			continue
		}
		agents = append(agents, a)
	}

	if len(agents) == 0 {
		if rigFilter != "" || agentFilter != "" {
			fmt.Fprintf(stderr, "gc config explain: no agents match filters (rig=%q agent=%q)\n", rigFilter, agentFilter) //nolint:errcheck // best-effort stderr
		} else {
			fmt.Fprintf(stderr, "gc config explain: no agents configured\n") //nolint:errcheck // best-effort stderr
		}
		return 1
	}

	for i, a := range agents {
		if i > 0 {
			fmt.Fprintln(stdout) //nolint:errcheck // best-effort
		}
		explainAgent(stdout, &a, prov)
	}
	return 0
}

// explainAgent prints the resolved config for a single agent with
// provenance annotations.
func explainAgent(w io.Writer, a *config.Agent, prov *config.Provenance) {
	qn := a.QualifiedName()
	source := prov.Agents[qn]
	if source == "" {
		source = prov.Root
	}

	fmt.Fprintf(w, "Agent: %s\n", qn)        //nolint:errcheck // best-effort
	fmt.Fprintf(w, "  source: %s\n", source) //nolint:errcheck // best-effort

	// Core fields.
	explainField(w, "name", a.Name, source)
	if a.Dir != "" {
		explainField(w, "dir", a.Dir, source)
	}
	if a.WorkDir != "" {
		explainField(w, "work_dir", a.WorkDir, source)
	}
	if a.Suspended {
		explainField(w, "suspended", "true", source)
	}
	if len(a.PreStart) > 0 {
		explainField(w, "pre_start", fmt.Sprintf("[%d commands]", len(a.PreStart)), source)
	}
	if a.PromptTemplate != "" {
		explainField(w, "prompt_template", a.PromptTemplate, source)
	}
	if a.Session != "" {
		explainField(w, "session", a.Session, source)
	}
	if a.Provider != "" {
		explainField(w, "provider", a.Provider, source)
	}
	if a.StartCommand != "" {
		explainField(w, "start_command", a.StartCommand, source)
	}
	if a.Nudge != "" {
		explainField(w, "nudge", a.Nudge, source)
	}
	if a.PromptMode != "" {
		explainField(w, "prompt_mode", a.PromptMode, source)
	}
	if a.PromptFlag != "" {
		explainField(w, "prompt_flag", a.PromptFlag, source)
	}

	// Env.
	if len(a.Env) > 0 {
		for k, v := range a.Env {
			explainField(w, "env."+k, v, source)
		}
	}

	// Scaling.
	if a.MinActiveSessions != nil || a.MaxActiveSessions != nil || a.ScaleCheck != "" || a.DrainTimeout != "" {
		sp := scaleParamsFor(a)
		explainField(w, "min_active_sessions", fmt.Sprintf("%d", sp.Min), source)
		explainField(w, "max_active_sessions", fmt.Sprintf("%d", sp.Max), source)
		if sp.Check != "" {
			explainField(w, "scale_check", sp.Check, source)
		}
		if a.DrainTimeout != "" {
			explainField(w, "drain_timeout", a.DrainTimeout, source)
		}
	}
}

// explainField prints a single field with its provenance source.
func explainField(w io.Writer, key, value, source string) {
	// Truncate long values.
	display := value
	if len(display) > 60 {
		display = display[:57] + "..."
	}
	// Quote strings that contain spaces.
	if strings.ContainsAny(display, " \t") {
		display = `"` + display + `"`
	}
	line := fmt.Sprintf("  %-30s = %-30s", key, display)
	if source != "" {
		line += "  # " + filepath.Base(source)
	}
	fmt.Fprintln(w, line) //nolint:errcheck // best-effort
}

// printProvenance writes a human-readable provenance summary.
func printProvenance(prov *config.Provenance, w io.Writer) {
	fmt.Fprintf(w, "Sources (%d files):\n", len(prov.Sources)) //nolint:errcheck // best-effort
	for i, s := range prov.Sources {
		label := "  "
		if i == 0 {
			label = "* "
		}
		fmt.Fprintf(w, "  %s%s\n", label, s) //nolint:errcheck // best-effort
	}
	if len(prov.Agents) > 0 {
		fmt.Fprintln(w, "\nAgents:") //nolint:errcheck // best-effort
		for name, src := range prov.Agents {
			fmt.Fprintf(w, "  %-30s ← %s\n", name, src) //nolint:errcheck // best-effort
		}
	}
	if len(prov.Rigs) > 0 {
		fmt.Fprintln(w, "\nRigs:") //nolint:errcheck // best-effort
		for name, src := range prov.Rigs {
			fmt.Fprintf(w, "  %-30s ← %s\n", name, src) //nolint:errcheck // best-effort
		}
	}
	if len(prov.Workspace) > 0 {
		fmt.Fprintln(w, "\nWorkspace:") //nolint:errcheck // best-effort
		for field, src := range prov.Workspace {
			fmt.Fprintf(w, "  %-30s ← %s\n", field, src) //nolint:errcheck // best-effort
		}
	}
	if len(prov.Warnings) > 0 {
		fmt.Fprintln(w, "\nWarnings:") //nolint:errcheck // best-effort
		for _, w2 := range prov.Warnings {
			fmt.Fprintf(w, "  %s\n", w2) //nolint:errcheck // best-effort
		}
	}
}
