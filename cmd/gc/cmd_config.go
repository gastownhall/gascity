package main

import (
	"encoding/json"
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

func jsonEncoder(w io.Writer) *json.Encoder {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc
}

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

	if err := ensureLegacyNamedPacksCached(cityPath); err != nil {
		fmt.Fprintf(stderr, "gc config show: fetching packs: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
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

	data, err := configForDisplay(cfg).Marshal()
	if err != nil {
		fmt.Fprintf(stderr, "gc config show: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	// Emit provider inheritance chain annotations as a comment block
	// preceding the marshaled TOML. `cfg.Marshal()` strips comments, so
	// we can't annotate per-block — instead we produce an up-front
	// summary that operators can diff / grep against.
	if annotations := renderProviderChainAnnotations(cfg); annotations != "" {
		fmt.Fprint(stdout, annotations) //nolint:errcheck // best-effort stdout
	}
	fmt.Fprint(stdout, string(data)) //nolint:errcheck // best-effort stdout
	return 0
}

func configForDisplay(cfg *config.City) *config.City {
	if cfg == nil {
		return nil
	}
	clone := *cfg
	clone.Workspace = cfg.Workspace
	if strings.TrimSpace(clone.Workspace.Name) == "" {
		clone.Workspace.Name = config.EffectiveCityName(cfg, "")
	}
	if strings.TrimSpace(clone.Workspace.Prefix) == "" {
		clone.Workspace.Prefix = strings.TrimSpace(cfg.ResolvedWorkspacePrefix)
	}
	return &clone
}

// renderProviderChainAnnotations produces a commented block summarizing
// each custom provider's resolved inheritance chain. Format:
//
//	# Provider inheritance chains (as resolved at config load):
//	#   codex-max       → codex → builtin:codex
//	#   my-standalone   → (no inheritance)
//	#   my-alias        → my-base (no built-in ancestor)
//
// Returns empty string if there are no custom providers OR if the
// resolved cache was not built (e.g., when chain resolution failed).
func renderProviderChainAnnotations(cfg *config.City) string {
	if cfg == nil || len(cfg.ResolvedProviders) == 0 {
		return ""
	}
	names := make([]string, 0, len(cfg.ResolvedProviders))
	for n := range cfg.ResolvedProviders {
		names = append(names, n)
	}
	sort.Strings(names)

	var b strings.Builder
	b.WriteString("# Provider inheritance chains (as resolved at config load):\n")
	for _, name := range names {
		r := cfg.ResolvedProviders[name]
		b.WriteString("#   ")
		b.WriteString(padRight(name, 20))
		b.WriteString(" ")
		if len(r.Chain) <= 1 {
			b.WriteString("(no inheritance)")
		} else {
			for i, hop := range r.Chain {
				if i > 0 {
					b.WriteString(" → ")
				}
				if hop.Kind == "builtin" {
					b.WriteString(config.BasePrefixBuiltin)
				}
				b.WriteString(hop.Name)
			}
			if r.BuiltinAncestor == "" && len(r.Chain) > 1 {
				b.WriteString(" (no built-in ancestor)")
			}
		}
		b.WriteString("\n")
	}
	b.WriteString("#\n")
	return b.String()
}

func padRight(s string, n int) string {
	if len(s) >= n {
		return s
	}
	return s + strings.Repeat(" ", n-len(s))
}

func newConfigExplainCmd(stdout, stderr io.Writer) *cobra.Command {
	var rigFilter string
	var agentFilter string
	var providerFilter string
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "explain",
		Short: "Show resolved config with provenance annotations",
		Long: `Show the resolved configuration with provenance.

For agents (default): displays every resolved field with an annotation
showing which config file provided the value. Use --rig and --agent to
filter.

For providers (--provider): displays the resolved ProviderSpec along
with per-field and per-map-key attribution — which chain layer
(builtin:X or providers.Y) contributed each value. Useful for
debugging base-chain inheritance.

Use --json to emit machine-readable output (providers only).`,
		Example: `  gc config explain
  gc config explain --agent mayor
  gc config explain --rig my-project
  gc config explain --provider codex-max
  gc config explain --provider codex-max --json
  gc config explain -f overlay.toml --agent polecat`,
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			if providerFilter != "" {
				if doConfigExplainProvider(providerFilter, asJSON, stdout, stderr) != 0 {
					return errExit
				}
				return nil
			}
			if asJSON {
				fmt.Fprintln(stderr, "gc config explain: --json is only supported with --provider") //nolint:errcheck
				return errExit
			}
			if doConfigExplain(rigFilter, agentFilter, stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&rigFilter, "rig", "", "filter to agents in this rig")
	cmd.Flags().StringVar(&agentFilter, "agent", "", "filter to a specific agent name")
	cmd.Flags().StringVar(&providerFilter, "provider", "", "explain a provider's resolved chain instead of agents")
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit JSON (requires --provider)")
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

	if err := ensureLegacyNamedPacksCached(cityPath); err != nil {
		fmt.Fprintf(stderr, "gc config explain: fetching packs: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
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

// doConfigExplainProvider explains a single provider's resolved chain.
// Emits human-readable output by default, or JSON when asJSON is true.
func doConfigExplainProvider(providerName string, asJSON bool, stdout, stderr io.Writer) int {
	cityPath, err := resolveCity()
	if err != nil {
		fmt.Fprintf(stderr, "gc config explain: %v\n", err) //nolint:errcheck
		return 1
	}

	if quickCfg, qErr := config.Load(fsys.OSFS{}, filepath.Join(cityPath, "city.toml")); qErr == nil && len(quickCfg.Packs) > 0 {
		if fErr := config.FetchPacks(quickCfg.Packs, cityPath); fErr != nil {
			fmt.Fprintf(stderr, "gc config explain: fetching packs: %v\n", fErr) //nolint:errcheck
			return 1
		}
	}

	cfg, _, err := config.LoadWithIncludes(fsys.OSFS{}, filepath.Join(cityPath, "city.toml"), extraConfigFiles...)
	if err != nil {
		fmt.Fprintf(stderr, "gc config explain: %v\n", err) //nolint:errcheck
		return 1
	}

	resolved, ok := config.ResolvedProviderCached(cfg, providerName)
	if !ok {
		if _, isBuiltin := config.BuiltinProviders()[providerName]; isBuiltin {
			fmt.Fprintf(stderr, "gc config explain: %q is a built-in provider; --provider only resolves custom entries\n", providerName) //nolint:errcheck
		} else {
			fmt.Fprintf(stderr, "gc config explain: no provider %q in resolved config\n", providerName) //nolint:errcheck
		}
		return 1
	}

	if asJSON {
		return renderProviderExplainJSON(resolved, providerName, stdout, stderr)
	}
	renderProviderExplainText(stdout, resolved, providerName)
	return 0
}

// renderProviderExplainText writes a human-readable explanation of a
// resolved provider chain.
func renderProviderExplainText(w io.Writer, r config.ResolvedProvider, name string) {
	fmt.Fprintf(w, "Provider: %s\n", name) //nolint:errcheck

	if len(r.Chain) > 0 {
		var hops []string
		for _, h := range r.Chain {
			if h.Kind == "builtin" {
				hops = append(hops, config.BasePrefixBuiltin+h.Name)
			} else {
				hops = append(hops, "providers."+h.Name)
			}
		}
		fmt.Fprintf(w, "  chain: %s\n", strings.Join(hops, " → ")) //nolint:errcheck
	}
	if r.BuiltinAncestor != "" {
		fmt.Fprintf(w, "  builtin_ancestor: %s\n", r.BuiltinAncestor) //nolint:errcheck
	}

	explainProviderField(w, "command", r.Command, r.Provenance.FieldLayer["command"])
	if len(r.Args) > 0 {
		explainProviderField(w, "args", fmt.Sprintf("[%d]", len(r.Args)), r.Provenance.FieldLayer["args"])
	}
	explainProviderField(w, "prompt_mode", r.PromptMode, r.Provenance.FieldLayer["prompt_mode"])
	explainProviderField(w, "prompt_flag", r.PromptFlag, r.Provenance.FieldLayer["prompt_flag"])
	if r.ReadyDelayMs != 0 {
		explainProviderField(w, "ready_delay_ms", fmt.Sprintf("%d", r.ReadyDelayMs), r.Provenance.FieldLayer["ready_delay_ms"])
	}
	explainProviderField(w, "ready_prompt_prefix", r.ReadyPromptPrefix, r.Provenance.FieldLayer["ready_prompt_prefix"])
	if len(r.ProcessNames) > 0 {
		explainProviderField(w, "process_names", strings.Join(r.ProcessNames, ","), r.Provenance.FieldLayer["process_names"])
	}
	explainProviderField(w, "resume_command", r.ResumeCommand, r.Provenance.FieldLayer["resume_command"])
	explainProviderField(w, "resume_flag", r.ResumeFlag, r.Provenance.FieldLayer["resume_flag"])
	explainProviderField(w, "resume_style", r.ResumeStyle, r.Provenance.FieldLayer["resume_style"])
	explainProviderField(w, "session_id_flag", r.SessionIDFlag, r.Provenance.FieldLayer["session_id_flag"])
	explainProviderField(w, "title_model", r.TitleModel, r.Provenance.FieldLayer["title_model"])

	explainResolvedBool(w, "supports_hooks", r.SupportsHooks, r.Provenance.FieldLayer["supports_hooks"])
	explainResolvedBool(w, "supports_acp", r.SupportsACP, r.Provenance.FieldLayer["supports_acp"])
	explainResolvedBool(w, "emits_permission_warning", r.EmitsPermissionWarning, r.Provenance.FieldLayer["emits_permission_warning"])

	explainProviderMap(w, "env", r.Env, r.Provenance.MapKeyLayer["env"])
	explainProviderMap(w, "permission_modes", r.PermissionModes, r.Provenance.MapKeyLayer["permission_modes"])
	explainProviderMap(w, "option_defaults", r.EffectiveDefaults, r.Provenance.MapKeyLayer["option_defaults"])
}

func explainProviderField(w io.Writer, key, value, layer string) {
	if value == "" {
		return
	}
	display := value
	if len(display) > 60 {
		display = display[:57] + "..."
	}
	if strings.ContainsAny(display, " \t") {
		display = `"` + display + `"`
	}
	line := fmt.Sprintf("  %-28s = %-30s", key, display)
	if layer != "" {
		line += "  # " + layer
	}
	fmt.Fprintln(w, line) //nolint:errcheck
}

// explainResolvedBool prints a resolved boolean only when some chain
// layer explicitly set it (layer != ""). The underlying ResolvedProvider
// exposes a plain bool; per-layer attribution lives in Provenance and
// carries the tri-state signal (absent layer = no explicit setter).
func explainResolvedBool(w io.Writer, key string, value bool, layer string) {
	if layer == "" {
		return
	}
	v := "false"
	if value {
		v = "true"
	}
	explainProviderField(w, key, v, layer)
}

func explainProviderMap(w io.Writer, field string, m map[string]string, perKey map[string]string) {
	if len(m) == 0 {
		return
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		line := fmt.Sprintf("  %-28s = %-30s", field+"."+k, m[k])
		if layer := perKey[k]; layer != "" {
			line += "  # " + layer
		}
		fmt.Fprintln(w, line) //nolint:errcheck
	}
}

// renderProviderExplainJSON emits a machine-readable view of a resolved
// provider including chain and per-field/per-key provenance.
func renderProviderExplainJSON(r config.ResolvedProvider, name string, stdout, stderr io.Writer) int {
	chain := make([]map[string]string, 0, len(r.Chain))
	for _, h := range r.Chain {
		chain = append(chain, map[string]string{"kind": h.Kind, "name": h.Name})
	}
	// Surface tri-state capability flags as null when no chain layer set
	// them (i.e. Provenance has no attribution for the field). The
	// ResolvedProvider stores bool, so we use provenance presence as the
	// explicit-vs-default signal.
	triStateFromProvenance := func(field string, value bool) any {
		if _, set := r.Provenance.FieldLayer[field]; !set {
			return nil
		}
		return value
	}
	payload := map[string]any{
		"name":             name,
		"builtin_ancestor": r.BuiltinAncestor,
		"chain":            chain,
		"resolved": map[string]any{
			"command":                  r.Command,
			"args":                     r.Args,
			"prompt_mode":              r.PromptMode,
			"prompt_flag":              r.PromptFlag,
			"ready_delay_ms":           r.ReadyDelayMs,
			"ready_prompt_prefix":      r.ReadyPromptPrefix,
			"process_names":            r.ProcessNames,
			"resume_command":           r.ResumeCommand,
			"resume_flag":              r.ResumeFlag,
			"resume_style":             r.ResumeStyle,
			"session_id_flag":          r.SessionIDFlag,
			"title_model":              r.TitleModel,
			"supports_hooks":           triStateFromProvenance("supports_hooks", r.SupportsHooks),
			"supports_acp":             triStateFromProvenance("supports_acp", r.SupportsACP),
			"emits_permission_warning": triStateFromProvenance("emits_permission_warning", r.EmitsPermissionWarning),
			"env":                      r.Env,
			"permission_modes":         r.PermissionModes,
			"option_defaults":          r.EffectiveDefaults,
		},
		"provenance": map[string]any{
			"field_layer":   r.Provenance.FieldLayer,
			"map_key_layer": r.Provenance.MapKeyLayer,
		},
	}
	enc := jsonEncoder(stdout)
	if err := enc.Encode(payload); err != nil {
		fmt.Fprintf(stderr, "gc config explain: json encode: %v\n", err) //nolint:errcheck
		return 1
	}
	return 0
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
