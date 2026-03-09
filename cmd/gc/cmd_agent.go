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

// loadCityConfig loads the city configuration with full pack expansion.
// Most CLI commands need this instead of config.Load so that agents defined
// via packs are visible. The only exceptions are quick pre-fetch checks
// in cmd_config.go and cmd_start.go that intentionally use config.Load to
// discover remote packs before fetching them.
func loadCityConfig(cityPath string) (*config.City, error) {
	cfg, _, err := config.LoadWithIncludes(fsys.OSFS{}, filepath.Join(cityPath, "city.toml"))
	if err != nil {
		return nil, err
	}
	injectBuiltinPacks(cfg, cityPath)
	return cfg, nil
}

// loadCityConfigFS is the testable variant of loadCityConfig that accepts a
// filesystem implementation. Used by functions that take an fsys.FS parameter
// for unit testing.
func loadCityConfigFS(fs fsys.FS, tomlPath string) (*config.City, error) {
	cfg, _, err := config.LoadWithIncludes(fs, tomlPath)
	return cfg, err
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
	// Step 1: literal match.
	if a, ok := findAgentByQualified(cfg, input); ok {
		return a, true
	}
	// Step 2: contextual (bare name + rig context).
	if !strings.Contains(input, "/") && currentRigDir != "" {
		if a, ok := findAgentByQualified(cfg, currentRigDir+"/"+input); ok {
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
			if a.Pool != nil && a.Pool.IsMultiInstance() {
				prefix := a.Name + "-"
				if strings.HasPrefix(input, prefix) {
					suffix := input[len(prefix):]
					if n, err := strconv.Atoi(suffix); err == nil && n >= 1 && (a.Pool.IsUnlimited() || n <= a.Pool.Max) {
						instance := a
						instance.Name = input
						instance.Pool = nil
						matches = append(matches, instance)
					}
				}
			}
		}
		if len(matches) == 1 {
			return matches[0], true
		}
	}
	return config.Agent{}, false
}

// findAgentByQualified looks up an agent by its qualified identity (dir+name).
// For pool agents with Max > 1, matches {name}-{N} patterns within the same dir.
func findAgentByQualified(cfg *config.City, identity string) (config.Agent, bool) {
	dir, name := config.ParseQualifiedName(identity)
	for _, a := range cfg.Agents {
		if a.Dir == dir && a.Name == name {
			return a, true
		}
		// Pool: match {name}-{N} within same dir.
		if a.Dir == dir && a.Pool != nil && a.Pool.IsMultiInstance() {
			prefix := a.Name + "-"
			if strings.HasPrefix(name, prefix) {
				suffix := name[len(prefix):]
				if n, err := strconv.Atoi(suffix); err == nil && n >= 1 && (a.Pool.IsUnlimited() || n <= a.Pool.Max) {
					instance := a
					instance.Name = name
					instance.Pool = nil // instances are not pools
					return instance, true
				}
			}
		}
	}
	return config.Agent{}, false
}

// currentRigContext returns the rig name that provides context for bare agent
// name resolution. Checks GC_DIR env var first, then cwd.
func currentRigContext(cfg *config.City) string {
	if gcDir := os.Getenv("GC_DIR"); gcDir != "" {
		for _, r := range cfg.Rigs {
			if filepath.Clean(gcDir) == filepath.Clean(r.Path) {
				return r.Name
			}
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
		// Deprecation shims — print removal message and exit.
		newAgentAttachCmd(stdout, stderr),
		newAgentDestroyCmd(stdout, stderr),
		newAgentKillCmd(stdout, stderr),
		newAgentListCmd(stdout, stderr),
		newAgentNudgeCmd(stdout, stderr),
		newAgentPeekCmd(stdout, stderr),
		newAgentStartCmd(stdout, stderr),
		newAgentStatusCmd(stdout, stderr),
		newAgentStopCmd(stdout, stderr),
		// Deprecation shims — runtime/session drain commands.
		newAgentDrainCmd(stdout, stderr),
		newAgentUndrainCmd(stdout, stderr),
		newAgentDrainCheckCmd(stdout, stderr),
		newAgentDrainAckCmd(stdout, stderr),
		newAgentRequestRestartCmd(stdout, stderr),
		newAgentLogsCmd(stdout, stderr),
	)
	return cmd
}

func newAgentAddCmd(stdout, stderr io.Writer) *cobra.Command {
	var name, promptTemplate, dir string
	var suspended bool
	cmd := &cobra.Command{
		Use:   "add --name <name>",
		Short: "Add an agent to the workspace",
		Long: `Add a new agent to the workspace configuration.

Appends an [[agents]] block to city.toml. The agent will be started
on the next "gc start" or controller reconcile tick. Use --dir to
scope the agent to a rig's working directory.`,
		Example: `  gc agent add --name mayor
  gc agent add --name polecat --dir my-project
  gc agent add --name worker --prompt-template prompts/worker.md --suspended`,
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

func newAgentListCmd(_, stderr io.Writer) *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "Deprecated: use \"gc session list\"",
		Args:  cobra.ArbitraryArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			fmt.Fprintln(stderr, "gc agent list: removed, use \"gc session list\" instead") //nolint:errcheck // best-effort stderr
			return errExit
		},
	}
}

func newAgentAttachCmd(_, stderr io.Writer) *cobra.Command {
	return &cobra.Command{
		Use:   "attach",
		Short: "Deprecated: use \"gc session attach\"",
		Args:  cobra.ArbitraryArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			fmt.Fprintln(stderr, "gc agent attach: removed, use \"gc session attach\" instead") //nolint:errcheck // best-effort stderr
			return errExit
		},
	}
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
// checks for duplicates, appends the new agent, and writes back.
// Accepts an injected FS for testability.
func doAgentAdd(fs fsys.FS, cityPath, name, promptTemplate, dir string, suspended bool, stdout, stderr io.Writer) int {
	tomlPath := filepath.Join(cityPath, "city.toml")
	cfg, err := loadCityConfigForEditFS(fs, tomlPath)
	if err != nil {
		fmt.Fprintf(stderr, "gc agent add: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}

	inputDir, inputName := config.ParseQualifiedName(name)
	for _, a := range cfg.Agents {
		if a.Dir == inputDir && a.Name == inputName {
			fmt.Fprintf(stderr, "gc agent add: agent %q already exists\n", name) //nolint:errcheck // best-effort stderr
			return 1
		}
	}
	// If input contained a dir component, use it (overrides --dir flag).
	if inputDir != "" {
		dir = inputDir
		name = inputName
	}

	newAgent := config.Agent{
		Name:           name,
		Dir:            dir,
		PromptTemplate: promptTemplate,
		Suspended:      suspended,
	}
	cfg.Agents = append(cfg.Agents, newAgent)
	content, err := cfg.Marshal()
	if err != nil {
		fmt.Fprintf(stderr, "gc agent add: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	if err := fs.WriteFile(tomlPath, content, 0o644); err != nil {
		fmt.Fprintf(stderr, "gc agent add: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}

	fmt.Fprintf(stdout, "Added agent '%s'\n", name) //nolint:errcheck // best-effort stdout
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
		for i := range cfg.Agents {
			if cfg.Agents[i].Dir == resolved.Dir && cfg.Agents[i].Name == resolved.Name {
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
		for i := range cfg.Agents {
			if cfg.Agents[i].Dir == resolved.Dir && cfg.Agents[i].Name == resolved.Name {
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

func newAgentNudgeCmd(_, stderr io.Writer) *cobra.Command {
	return &cobra.Command{
		Use:   "nudge",
		Short: "Deprecated: use \"gc session nudge\"",
		Args:  cobra.ArbitraryArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			fmt.Fprintln(stderr, "gc agent nudge: removed, use \"gc session nudge\" instead") //nolint:errcheck // best-effort stderr
			return errExit
		},
	}
}

func newAgentPeekCmd(_, stderr io.Writer) *cobra.Command {
	return &cobra.Command{
		Use:   "peek",
		Short: "Deprecated: use \"gc session peek\"",
		Args:  cobra.ArbitraryArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			fmt.Fprintln(stderr, "gc agent peek: removed, use \"gc session peek\" instead") //nolint:errcheck // best-effort stderr
			return errExit
		},
	}
}

func newAgentKillCmd(_, stderr io.Writer) *cobra.Command {
	return &cobra.Command{
		Use:   "kill",
		Short: "Deprecated: use \"gc session kill\"",
		Args:  cobra.ArbitraryArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			fmt.Fprintln(stderr, "gc agent kill: removed, use \"gc session kill\" instead") //nolint:errcheck // best-effort stderr
			return errExit
		},
	}
}

func newAgentDrainCmd(_, stderr io.Writer) *cobra.Command {
	return &cobra.Command{
		Use:   "drain",
		Short: "Deprecated: use \"gc runtime drain\"",
		Args:  cobra.ArbitraryArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			fmt.Fprintln(stderr, "gc agent drain: removed, use \"gc runtime drain\" instead") //nolint:errcheck // best-effort stderr
			return errExit
		},
	}
}

func newAgentUndrainCmd(_, stderr io.Writer) *cobra.Command {
	return &cobra.Command{
		Use:   "undrain",
		Short: "Deprecated: use \"gc runtime undrain\"",
		Args:  cobra.ArbitraryArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			fmt.Fprintln(stderr, "gc agent undrain: removed, use \"gc runtime undrain\" instead") //nolint:errcheck // best-effort stderr
			return errExit
		},
	}
}

func newAgentDrainCheckCmd(_, stderr io.Writer) *cobra.Command {
	return &cobra.Command{
		Use:   "drain-check",
		Short: "Deprecated: use \"gc runtime drain-check\"",
		Args:  cobra.ArbitraryArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			fmt.Fprintln(stderr, "gc agent drain-check: removed, use \"gc runtime drain-check\" instead") //nolint:errcheck // best-effort stderr
			return errExit
		},
	}
}

func newAgentDrainAckCmd(_, stderr io.Writer) *cobra.Command {
	return &cobra.Command{
		Use:   "drain-ack",
		Short: "Deprecated: use \"gc runtime drain-ack\"",
		Args:  cobra.ArbitraryArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			fmt.Fprintln(stderr, "gc agent drain-ack: removed, use \"gc runtime drain-ack\" instead") //nolint:errcheck // best-effort stderr
			return errExit
		},
	}
}

func newAgentRequestRestartCmd(_, stderr io.Writer) *cobra.Command {
	return &cobra.Command{
		Use:   "request-restart",
		Short: "Deprecated: use \"gc runtime request-restart\"",
		Args:  cobra.ArbitraryArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			fmt.Fprintln(stderr, "gc agent request-restart: removed, use \"gc runtime request-restart\" instead") //nolint:errcheck // best-effort stderr
			return errExit
		},
	}
}

func newAgentLogsCmd(_, stderr io.Writer) *cobra.Command {
	return &cobra.Command{
		Use:   "logs",
		Short: "Deprecated: use \"gc session logs\"",
		Args:  cobra.ArbitraryArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			fmt.Fprintln(stderr, "gc agent logs: removed, use \"gc session logs\" instead") //nolint:errcheck // best-effort stderr
			return errExit
		},
	}
}
