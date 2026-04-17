package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/gastownhall/gascity/internal/api"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/spf13/cobra"
)

const agentAddPromptScaffold = `You are the {{ .AgentName }} agent.

Describe what this agent should do here.
`

// loadCityConfig loads the city configuration with full pack expansion.
// Most CLI commands need this instead of config.Load so that agents defined
// via packs are visible. The only exceptions are quick pre-fetch checks
// in cmd_config.go and cmd_start.go that intentionally use config.Load to
// discover remote packs before fetching them.
func loadCityConfig(cityPath string) (*config.City, error) {
	extras := builtinPackIncludes(cityPath)
	cfg, _, err := config.LoadWithIncludes(fsys.OSFS{}, filepath.Join(cityPath, "city.toml"), extras...)
	if err != nil {
		return nil, err
	}
	applyFeatureFlags(cfg)
	return cfg, nil
}

// loadCityConfigFS is the testable variant of loadCityConfig that accepts a
// filesystem implementation. Used by functions that take an fsys.FS parameter
// for unit testing.
func loadCityConfigFS(fs fsys.FS, tomlPath string) (*config.City, error) {
	cfg, _, err := config.LoadWithIncludes(fs, tomlPath)
	if err != nil {
		return nil, err
	}
	applyFeatureFlags(cfg)
	return cfg, nil
}

// loadCityConfigForEditFS loads the raw city config WITHOUT pack/include
// expansion. Use for commands that modify city.toml and write it back —
// preserves include directives, pack references, and patches.
func loadCityConfigForEditFS(fs fsys.FS, tomlPath string) (*config.City, error) {
	return config.Load(fs, tomlPath)
}

// resolveAgentIdentity resolves an agent input string to a config.Agent using
// 3-step resolution:
//  1. Literal: try the input as-is (e.g., "mayor" or "hello-world/polecat").
//  2. Contextual: if input has no "/" and currentRigDir is set, try
//     "{currentRigDir}/{input}" to resolve rig-scoped agents from context.
//  3. Unambiguous bare name: scan all agents by Name (ignoring Dir).
//     Succeeds only when exactly one configured agent matches. Pool
//     members are synthesized when the input uses {name}-{N}.
func resolveAgentIdentity(cfg *config.City, input, currentRigDir string) (config.Agent, bool) {
	// Step 1: contextual rig match (bare name + rig context).
	// When the user is inside a rig directory and types a bare name like
	// "claude", prefer the rig-scoped agent (hello-world/claude) over the
	// city-scoped one. This matches the tutorial UX: cd into rig, sling to
	// the agent by bare name.
	if !strings.Contains(input, "/") && currentRigDir != "" {
		if a, ok := findAgentByQualified(cfg, currentRigDir+"/"+input); ok {
			return a, true
		}
	}
	// Step 2: literal match (qualified or city-scoped).
	if a, ok := findAgentByQualified(cfg, input); ok {
		return a, true
	}
	// Step 2b: qualified pool instance — "rig/polecat-2" matches pool "rig/polecat".
	if strings.Contains(input, "/") {
		if a, ok := resolvePoolInstance(cfg, input); ok {
			return a, true
		}
	}
	// Step 3: unambiguous bare name — scan all agents by Name (ignoring Dir).
	// Succeeds only when exactly one agent matches. Handles pool instances too.
	if !strings.Contains(input, "/") {
		var matches []config.Agent
		for _, a := range cfg.Agents {
			if a.Name == input {
				matches = append(matches, a)
				continue
			}
			// Pool instance: "polecat-2" matches pool "polecat" with Max >= 2 (or unlimited).
			if a, ok := matchPoolInstance(a, input); ok {
				matches = append(matches, a)
			}
		}
		if len(matches) == 1 {
			return matches[0], true
		}
	}
	return config.Agent{}, false
}

// resolvePoolInstance handles qualified pool instance names like "rig/polecat-2"
// by matching against each pool agent's QualifiedName() + instance suffix.
func resolvePoolInstance(cfg *config.City, input string) (config.Agent, bool) {
	for _, a := range cfg.Agents {
		sp := scaleParamsFor(&a)
		if !a.SupportsInstanceExpansion() {
			continue
		}
		prefix := a.QualifiedName() + "-"
		if !strings.HasPrefix(input, prefix) {
			continue
		}
		suffix := input[len(prefix):]
		n, err := strconv.Atoi(suffix)
		if err != nil || n < 1 {
			continue
		}
		isUnlimited := sp.Max < 0
		if !isUnlimited && n > sp.Max {
			continue
		}
		instance := deepCopyAgent(&a, a.Name+"-"+suffix, a.Dir)
		return instance, true
	}
	return config.Agent{}, false
}

// matchPoolInstance checks if input matches a multi-session agent's instance
// pattern (e.g., "polecat-2" matches agent "polecat"). Returns the synthesized instance.
func matchPoolInstance(a config.Agent, input string) (config.Agent, bool) {
	sp := scaleParamsFor(&a)
	if !a.SupportsInstanceExpansion() {
		return config.Agent{}, false
	}
	prefix := a.Name + "-"
	if !strings.HasPrefix(input, prefix) {
		return config.Agent{}, false
	}
	suffix := input[len(prefix):]
	n, err := strconv.Atoi(suffix)
	if err != nil || n < 1 {
		return config.Agent{}, false
	}
	isUnlimited := sp.Max < 0
	if !isUnlimited && n > sp.Max {
		return config.Agent{}, false
	}
	instance := deepCopyAgent(&a, input, a.Dir)
	return instance, true
}

// findAgentByQualified looks up an agent by its exact qualified identity
// (dir+name or dir/binding.name) from config.
func findAgentByQualified(cfg *config.City, identity string) (config.Agent, bool) {
	for _, a := range cfg.Agents {
		if config.AgentMatchesIdentity(&a, identity) {
			return a, true
		}
	}
	return config.Agent{}, false
}

// currentRigContext returns the rig name that provides context for bare agent
// name resolution. Checks GC_DIR env var first, then cwd.
func currentRigContext(cfg *config.City) string {
	if gcDir := os.Getenv("GC_DIR"); gcDir != "" {
		if name, _, found := findEnclosingRig(gcDir, cfg.Rigs); found {
			return name
		}
	}
	if cwd, err := os.Getwd(); err == nil {
		if name, _, found := findEnclosingRig(cwd, cfg.Rigs); found {
			return name
		}
	}
	return ""
}

func newAgentCmd(stdout, stderr io.Writer) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "agent",
		Short: "Manage agent configuration",
		Long: `Manage agent configuration in city.toml.

Runtime operations (attach, list, peek, nudge, kill, start, stop, destroy)
have moved to "gc session" and "gc runtime".`,
		Args: cobra.ArbitraryArgs,
		RunE: func(_ *cobra.Command, args []string) error {
			if len(args) == 0 {
				fmt.Fprintln(stderr, "gc agent: missing subcommand (add, suspend, resume)") //nolint:errcheck // best-effort stderr
			} else {
				fmt.Fprintf(stderr, "gc agent: unknown subcommand %q\n", args[0]) //nolint:errcheck // best-effort stderr
			}
			return errExit
		},
	}
	cmd.AddCommand(
		newAgentAddCmd(stdout, stderr),
		newAgentResumeCmd(stdout, stderr),
		newAgentSuspendCmd(stdout, stderr),
	)
	return cmd
}

func newAgentAddCmd(stdout, stderr io.Writer) *cobra.Command {
	var name, promptTemplate, dir string
	var suspended bool
	cmd := &cobra.Command{
		Use:   "add --name <name>",
		Short: "Add an agent scaffold",
		Long: `Add a new agent scaffold under agents/<name>/.

Creates agents/<name>/prompt.template.md and, when needed,
agents/<name>/agent.toml. These files live in the city directory and do
not append [[agent]] blocks to city.toml.

Use --prompt-template to copy prompt content from an existing file into
the canonical prompt.template.md location. Use --dir to record a rig or
working-directory prefix in agent.toml. Use --suspended to scaffold the
agent in a suspended state.`,
		Example: `  gc agent add --name mayor
  gc agent add --name polecat --dir my-project
  gc agent add --name worker --prompt-template ./worker.md --suspended`,
		Args: cobra.ArbitraryArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			if cmdAgentAdd(name, promptTemplate, dir, suspended, stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&name, "name", "", "Name of the agent")
	cmd.Flags().StringVar(&promptTemplate, "prompt-template", "", "Path to prompt template file (relative to city root)")
	cmd.Flags().StringVar(&dir, "dir", "", "Working directory for the agent (relative to city root)")
	cmd.Flags().BoolVar(&suspended, "suspended", false, "Register the agent in suspended state")
	return cmd
}

// cmdAgentAdd is the CLI entry point for adding an agent. It locates
// the city root and delegates to doAgentAdd.
func cmdAgentAdd(name, promptTemplate, dir string, suspended bool, stdout, stderr io.Writer) int {
	if name == "" {
		fmt.Fprintln(stderr, "gc agent add: missing --name flag") //nolint:errcheck // best-effort stderr
		return 1
	}
	cityPath, err := resolveCity()
	if err != nil {
		fmt.Fprintf(stderr, "gc agent add: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	return doAgentAdd(fsys.OSFS{}, cityPath, name, promptTemplate, dir, suspended, stdout, stderr)
}

// doAgentAdd is the pure logic for "gc agent add". It loads city.toml,
// checks for duplicates, and writes a v2 agent scaffold under agents/<name>/.
// Accepts an injected FS for testability.
func doAgentAdd(fs fsys.FS, cityPath, name, promptTemplate, dir string, suspended bool, stdout, stderr io.Writer) int {
	tomlPath := filepath.Join(cityPath, "city.toml")
	packPath := filepath.Join(cityPath, "pack.toml")
	if _, err := fs.Stat(packPath); err != nil {
		fmt.Fprintln(stderr, "gc agent add: this command requires a city directory with pack.toml; run \"gc doctor\" or \"gc doctor --fix\" to migrate this city first") //nolint:errcheck // best-effort stderr
		return 1
	}

	cfg, err := loadCityConfigFS(fs, tomlPath)
	if err != nil {
		fmt.Fprintf(stderr, "gc agent add: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}

	inputDir, inputName := config.ParseQualifiedName(name)
	// If input contained a dir component, use it (overrides --dir flag).
	if inputDir != "" {
		dir = inputDir
		name = inputName
	}
	for _, a := range cfg.Agents {
		if a.Name == name {
			fmt.Fprintf(stderr, "gc agent add: agent %q already exists\n", name) //nolint:errcheck // best-effort stderr
			return 1
		}
	}

	agentDir := filepath.Join(cityPath, "agents", name)
	if err := fs.MkdirAll(agentDir, 0o755); err != nil {
		fmt.Fprintf(stderr, "gc agent add: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}

	var promptData []byte
	if promptTemplate != "" {
		src := promptTemplate
		if !filepath.IsAbs(src) {
			src = filepath.Join(cityPath, src)
		}
		var err error
		promptData, err = fs.ReadFile(src)
		if err != nil {
			fmt.Fprintf(stderr, "gc agent add: reading prompt template %q: %v\n", promptTemplate, err) //nolint:errcheck // best-effort stderr
			return 1
		}
	} else {
		promptData = []byte(agentAddPromptScaffold)
	}

	promptPath := filepath.Join(agentDir, "prompt.template.md")
	if err := fs.WriteFile(promptPath, promptData, 0o644); err != nil {
		fmt.Fprintf(stderr, "gc agent add: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}

	if dir != "" || suspended {
		var b strings.Builder
		if dir != "" {
			fmt.Fprintf(&b, "dir = %q\n", dir) //nolint:errcheck // best-effort strings.Builder
		}
		if suspended {
			b.WriteString("suspended = true\n")
		}
		if err := fs.WriteFile(filepath.Join(agentDir, "agent.toml"), []byte(b.String()), 0o644); err != nil {
			fmt.Fprintf(stderr, "gc agent add: %v\n", err) //nolint:errcheck // best-effort stderr
			return 1
		}
	}

	fmt.Fprintf(stdout, "Scaffolded agent '%s'\n", name) //nolint:errcheck // best-effort stdout
	return 0
}

func newAgentSuspendCmd(stdout, stderr io.Writer) *cobra.Command {
	return &cobra.Command{
		Use:   "suspend <name>",
		Short: "Suspend an agent (reconciler will skip it)",
		Long: `Suspend an agent by setting suspended=true in city.toml.

Suspended agents are skipped by the reconciler — their sessions are not
started or restarted. Existing sessions continue running but won't be
replaced if they exit. Use "gc agent resume" to restore.`,
		Args: cobra.ArbitraryArgs,
		RunE: func(_ *cobra.Command, args []string) error {
			if cmdAgentSuspend(args, stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
}

// cmdAgentSuspend is the CLI entry point for suspending an agent.
func cmdAgentSuspend(args []string, stdout, stderr io.Writer) int {
	if len(args) < 1 {
		fmt.Fprintln(stderr, "gc agent suspend: missing agent name") //nolint:errcheck // best-effort stderr
		return 1
	}
	cityPath, err := resolveCity()
	if err != nil {
		fmt.Fprintf(stderr, "gc agent suspend: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	if c := apiClient(cityPath); c != nil {
		qname := resolveAgentForAPI(cityPath, args[0])
		err := c.SuspendAgent(qname)
		if err == nil {
			fmt.Fprintf(stdout, "Suspended agent '%s'\n", args[0]) //nolint:errcheck // best-effort stdout
			return 0
		}
		if !api.ShouldFallback(err) {
			fmt.Fprintf(stderr, "gc agent suspend: %v\n", err) //nolint:errcheck // best-effort stderr
			return 1
		}
		// Connection error — fall through to direct mutation.
	}
	return doAgentSuspend(fsys.OSFS{}, cityPath, args[0], stdout, stderr)
}

// doAgentSuspend sets suspended=true on the named agent in city.toml.
// Uses raw config (no pack expansion) to preserve includes/patches on write-back.
// If the agent isn't found in raw config but exists in expanded config, it's
// pack-derived and the user gets a helpful error directing them to [[patches]].
// Accepts an injected FS for testability.
func doAgentSuspend(fs fsys.FS, cityPath, name string, stdout, stderr io.Writer) int {
	tomlPath := filepath.Join(cityPath, "city.toml")

	// Phase 1: load raw config (no expansion) for safe write-back.
	cfg, err := loadCityConfigForEditFS(fs, tomlPath)
	if err != nil {
		fmt.Fprintf(stderr, "gc agent suspend: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}

	// Try to find agent in raw config.
	resolved, ok := resolveAgentIdentity(cfg, name, currentRigContext(cfg))
	if ok {
		// Found in raw config — toggle and write back.
		resolvedQN := resolved.QualifiedName()
		for i := range cfg.Agents {
			if cfg.Agents[i].QualifiedName() == resolvedQN {
				cfg.Agents[i].Suspended = true
				break
			}
		}
		content, err := cfg.Marshal()
		if err != nil {
			fmt.Fprintf(stderr, "gc agent suspend: %v\n", err) //nolint:errcheck // best-effort stderr
			return 1
		}
		if err := fs.WriteFile(tomlPath, content, 0o644); err != nil {
			fmt.Fprintf(stderr, "gc agent suspend: %v\n", err) //nolint:errcheck // best-effort stderr
			return 1
		}
		fmt.Fprintf(stdout, "Suspended agent '%s'\n", name) //nolint:errcheck // best-effort stdout
		return 0
	}

	// Phase 2: not in raw config — check expanded config for pack-derived agents.
	expanded, err := loadCityConfigFS(fs, tomlPath)
	if err != nil {
		// Fall through to generic not-found using raw cfg.
		fmt.Fprintln(stderr, agentNotFoundMsg("gc agent suspend", name, cfg)) //nolint:errcheck // best-effort stderr
		return 1
	}
	if _, ok := resolveAgentIdentity(expanded, name, currentRigContext(expanded)); ok {
		fmt.Fprintf(stderr, "gc agent suspend: agent %q is defined by a pack — use [[patches]] to override\n", name) //nolint:errcheck // best-effort stderr
		return 1
	}

	// Not found anywhere.
	fmt.Fprintln(stderr, agentNotFoundMsg("gc agent suspend", name, expanded)) //nolint:errcheck // best-effort stderr
	return 1
}

func newAgentResumeCmd(stdout, stderr io.Writer) *cobra.Command {
	return &cobra.Command{
		Use:   "resume <name>",
		Short: "Resume a suspended agent",
		Long: `Resume a suspended agent by clearing suspended in city.toml.

The reconciler will start the agent on its next tick. Supports bare
names (resolved via rig context) and qualified names (e.g. "myrig/worker").`,
		Args: cobra.ArbitraryArgs,
		RunE: func(_ *cobra.Command, args []string) error {
			if cmdAgentResume(args, stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
}

// cmdAgentResume is the CLI entry point for resuming a suspended agent.
func cmdAgentResume(args []string, stdout, stderr io.Writer) int {
	if len(args) < 1 {
		fmt.Fprintln(stderr, "gc agent resume: missing agent name") //nolint:errcheck // best-effort stderr
		return 1
	}
	cityPath, err := resolveCity()
	if err != nil {
		fmt.Fprintf(stderr, "gc agent resume: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	if c := apiClient(cityPath); c != nil {
		qname := resolveAgentForAPI(cityPath, args[0])
		err := c.ResumeAgent(qname)
		if err == nil {
			fmt.Fprintf(stdout, "Resumed agent '%s'\n", args[0]) //nolint:errcheck // best-effort stdout
			return 0
		}
		if !api.ShouldFallback(err) {
			fmt.Fprintf(stderr, "gc agent resume: %v\n", err) //nolint:errcheck // best-effort stderr
			return 1
		}
		// Connection error — fall through to direct mutation.
	}
	return doAgentResume(fsys.OSFS{}, cityPath, args[0], stdout, stderr)
}

// doAgentResume clears suspended on the named agent in city.toml.
// Uses raw config (no pack expansion) to preserve includes/patches on write-back.
// If the agent isn't found in raw config but exists in expanded config, it's
// pack-derived and the user gets a helpful error directing them to [[patches]].
// Accepts an injected FS for testability.
func doAgentResume(fs fsys.FS, cityPath, name string, stdout, stderr io.Writer) int {
	tomlPath := filepath.Join(cityPath, "city.toml")

	// Phase 1: load raw config (no expansion) for safe write-back.
	cfg, err := loadCityConfigForEditFS(fs, tomlPath)
	if err != nil {
		fmt.Fprintf(stderr, "gc agent resume: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}

	// Try to find agent in raw config.
	resolved, ok := resolveAgentIdentity(cfg, name, currentRigContext(cfg))
	if ok {
		// Found in raw config — toggle and write back.
		resolvedQN := resolved.QualifiedName()
		for i := range cfg.Agents {
			if cfg.Agents[i].QualifiedName() == resolvedQN {
				cfg.Agents[i].Suspended = false
				break
			}
		}
		content, err := cfg.Marshal()
		if err != nil {
			fmt.Fprintf(stderr, "gc agent resume: %v\n", err) //nolint:errcheck // best-effort stderr
			return 1
		}
		if err := fs.WriteFile(tomlPath, content, 0o644); err != nil {
			fmt.Fprintf(stderr, "gc agent resume: %v\n", err) //nolint:errcheck // best-effort stderr
			return 1
		}
		fmt.Fprintf(stdout, "Resumed agent '%s'\n", name) //nolint:errcheck // best-effort stdout
		return 0
	}

	// Phase 2: not in raw config — check expanded config for pack-derived agents.
	expanded, err := loadCityConfigFS(fs, tomlPath)
	if err != nil {
		// Fall through to generic not-found using raw cfg.
		fmt.Fprintln(stderr, agentNotFoundMsg("gc agent resume", name, cfg)) //nolint:errcheck // best-effort stderr
		return 1
	}
	if _, ok := resolveAgentIdentity(expanded, name, currentRigContext(expanded)); ok {
		fmt.Fprintf(stderr, "gc agent resume: agent %q is defined by a pack — use [[patches]] to override\n", name) //nolint:errcheck // best-effort stderr
		return 1
	}

	// Not found anywhere.
	fmt.Fprintln(stderr, agentNotFoundMsg("gc agent resume", name, expanded)) //nolint:errcheck // best-effort stderr
	return 1
}
