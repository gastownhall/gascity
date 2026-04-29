package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/gastownhall/gascity/internal/config"
)

func newHookCmd(stdout, stderr io.Writer) *cobra.Command {
	var inject bool
	var hookFormat string
	cmd := &cobra.Command{
		Use:   "hook [agent]",
		Short: "Check for available work (use --inject for Stop hook output)",
		Long: `Checks for available work using the agent's work_query config.

Without --inject: prints raw output, exits 0 if work exists, 1 if empty.
With --inject: wraps output in <system-reminder> for hook injection, always exits 0.

		The agent is determined from $GC_AGENT or a positional argument.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			if cmdHookWithFormat(args, inject, hookFormat, stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&inject, "inject", false, "output <system-reminder> block for hook injection")
	cmd.Flags().StringVar(&hookFormat, "hook-format", "", "format hook output for a provider")
	return cmd
}

// cmdHook is the CLI entry point for gc hook. Resolves the agent from
// $GC_AGENT or a positional argument, loads the city config, and runs
// the agent's work query.
func cmdHook(args []string, stdout, stderr io.Writer) int {
	return cmdHookWithFormat(args, false, "", stdout, stderr)
}

func cmdHookWithFormat(args []string, inject bool, hookFormat string, stdout, stderr io.Writer) int {
	agentName := os.Getenv("GC_ALIAS")
	if agentName == "" {
		agentName = os.Getenv("GC_AGENT")
	}
	sessionTemplateContext := false
	if len(args) == 0 {
		template := strings.TrimSpace(os.Getenv("GC_TEMPLATE"))
		hasSessionContext := strings.TrimSpace(os.Getenv("GC_SESSION_NAME")) != "" ||
			strings.TrimSpace(os.Getenv("GC_SESSION_ID")) != ""
		if template != "" && hasSessionContext {
			agentName = template
			sessionTemplateContext = true
		}
	}
	if len(args) > 0 {
		agentName = args[0]
	}
	if agentName == "" {
		if inject {
			return 0 // --inject always exits 0
		}
		fmt.Fprintln(stderr, "gc hook: agent not specified (set $GC_AGENT or pass as argument)") //nolint:errcheck // best-effort stderr
		return 1
	}

	cityPath, err := resolveCity()
	if err != nil {
		if inject {
			return 0
		}
		fmt.Fprintf(stderr, "gc hook: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	cfg, err := loadCityConfig(cityPath, stderr)
	if err != nil {
		if inject {
			return 0
		}
		fmt.Fprintf(stderr, "gc hook: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	// Normalize relative rig paths to absolute so downstream rig-matching
	// (agentCommandDir, bdRuntimeEnvForRig) compares apples to apples.
	// Other CLI entry points (cmd_sling, cmd_start, cmd_rig, cmd_supervisor)
	// do the same immediately after loadCityConfig.
	resolveRigPaths(cityPath, cfg.Rigs)

	if citySuspended(cfg) {
		if inject {
			return 0
		}
		fmt.Fprintln(stderr, "gc hook: city is suspended") //nolint:errcheck // best-effort stderr
		return 1
	}

	a, ok := resolveAgentIdentity(cfg, agentName, currentRigContext(cfg))
	if !ok {
		if inject {
			return 0
		}
		fmt.Fprintf(stderr, "gc hook: agent %q not found in config\n", agentName) //nolint:errcheck // best-effort stderr
		return 1
	}

	if isAgentEffectivelySuspended(cfg, &a) {
		if inject {
			return 0
		}
		fmt.Fprintf(stderr, "gc hook: agent %q is suspended\n", agentName) //nolint:errcheck // best-effort stderr
		return 1
	}

	cityName := loadedCityName(cfg, cityPath)
	workQuery := a.EffectiveWorkQuery()
	// Expand {{.Rig}}/{{.AgentBase}} in user-supplied work_query so agent-side
	// hook invocation sees the same rig substitution as the controller-side
	// probes in build_desired_state.go / session_reconcile.go. #793.
	workQuery = expandAgentCommandTemplate(cityPath, cityName, &a, cfg.Rigs, "work_query", workQuery, stderr)
	workDir := agentCommandDir(cityPath, &a, cfg.Rigs)

	// Build the work query subprocess environment. Rig-backed agents get
	// rig-scoped BEADS_DIR / GC_RIG_ROOT / Dolt coordinates so the query
	// reads the rig store rather than whatever BEADS_DIR the parent
	// process happens to inherit (issue #514). Many built-in work queries
	// also key off session identity. Explicit hook targets get resolved
	// names; named-session context preserves the runtime-supplied owner
	// env while selecting the backing config through GC_TEMPLATE.
	resolvedAgentName := a.QualifiedName()
	resolvedSessionName := cliSessionName(cityPath, cityName, resolvedAgentName, cfg.Workspace.SessionTemplate)
	agentForQuery := resolvedAgentName
	sessionForQuery := resolvedSessionName
	if sessionTemplateContext {
		agentForQuery = os.Getenv("GC_ALIAS")
		if agentForQuery == "" {
			agentForQuery = os.Getenv("GC_SESSION_NAME")
		}
		if agentForQuery == "" {
			agentForQuery = os.Getenv("GC_AGENT")
		}
		sessionForQuery = os.Getenv("GC_SESSION_NAME")
	}
	overrides := hookQueryEnv(cityPath, cfg, &a)
	overrides["GC_AGENT"] = agentForQuery
	overrides["GC_SESSION_NAME"] = sessionForQuery
	if sessionTemplateContext {
		overrides["GC_ALIAS"] = os.Getenv("GC_ALIAS")
		overrides["GC_SESSION_ID"] = os.Getenv("GC_SESSION_ID")
		overrides["GC_SESSION_ORIGIN"] = os.Getenv("GC_SESSION_ORIGIN")
		overrides["GC_TEMPLATE"] = os.Getenv("GC_TEMPLATE")
	}
	queryEnv := mergeRuntimeEnv(os.Environ(), overrides)
	runner := func(command, dir string) (string, error) {
		return shellWorkQueryWithEnv(command, dir, queryEnv)
	}
	return doHookWithFormat(workQuery, workDir, inject, hookFormat, runner, stdout, stderr)
}

// hookQueryEnv returns the full work-query environment for a hook subprocess.
// It includes scope metadata (store root/scope/prefix) plus any rig-scoped
// runtime overrides so hook queries observe the same routing contract as the
// controller probes.
func hookQueryEnv(cityPath string, cfg *config.City, a *config.Agent) map[string]string {
	env := controllerWorkQueryEnv(cityPath, cfg, a)
	if env == nil {
		env = map[string]string{}
	}
	return env
}

// WorkQueryRunner runs a work query command and returns its stdout.
// dir sets the command's working directory.
type WorkQueryRunner func(command, dir string) (string, error)

// shellWorkQueryWithEnv runs a work query command via sh -c and returns
// stdout. If env is non-nil it is used as the subprocess environment
// (including any rig-scoped BEADS_DIR / GC_RIG_ROOT overrides); otherwise
// the child inherits the parent process environment. Times out after 30
// seconds.
func shellWorkQueryWithEnv(command, dir string, env []string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "sh", "-c", command)
	cmd.WaitDelay = 2 * time.Second
	if dir != "" {
		cmd.Dir = dir
	}
	cmd.Env = workQueryEnvForDir(env, dir)
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("running work query %q: %w", command, err)
	}
	return string(out), nil
}

// workQueryEnvForDir ensures the subprocess environment does not carry a
// stale inherited PWD when exec.Cmd.Dir points somewhere else. Some shells
// (notably macOS /bin/sh) preserve the inherited PWD instead of recomputing
// it from the real working directory, which breaks hook work_query commands
// that inspect $PWD.
func workQueryEnvForDir(env []string, dir string) []string {
	if env == nil {
		env = mergeRuntimeEnv(os.Environ(), nil)
	}
	if dir == "" {
		return env
	}
	out := removeEnvKey(append([]string(nil), env...), "PWD")
	return append(out, "PWD="+dir)
}

// doHook is the pure logic for gc hook. Runs the work query and outputs
// results based on mode. Without inject: prints raw output, returns 0 if
// work, 1 if empty. With inject: wraps in <system-reminder>, always returns 0.
func doHook(workQuery, dir string, inject bool, runner WorkQueryRunner, stdout, stderr io.Writer) int {
	return doHookWithFormat(workQuery, dir, inject, "", runner, stdout, stderr)
}

func doHookWithFormat(workQuery, dir string, inject bool, hookFormat string, runner WorkQueryRunner, stdout, stderr io.Writer) int {
	output, err := runner(workQuery, dir)
	if err != nil {
		if inject {
			return 0 // --inject always exits 0
		}
		fmt.Fprintf(stderr, "gc hook: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}

	trimmed := strings.TrimSpace(output)
	normalized := normalizeWorkQueryOutput(trimmed)
	hasWork := workQueryHasReadyWork(normalized)

	if inject {
		if hasWork {
			content := formatHookInjectReminder(normalized)
			_ = writeProviderHookContextForEvent(stdout, hookFormat, "Stop", content)
		}
		return 0 // --inject always exits 0
	}

	// Non-inject mode: print raw output. Return 0 only when work exists.
	if !hasWork {
		if normalized != "" {
			fmt.Fprint(stdout, normalized) //nolint:errcheck // best-effort stdout
		}
		return 1
	}
	fmt.Fprint(stdout, normalized) //nolint:errcheck // best-effort stdout
	return 0
}

func formatHookInjectReminder(normalizedWork string) string {
	return fmt.Sprintf(`<system-reminder>
You have pending work. Pick up the next item:

<work-items>
%s
</work-items>

Use the bead id from the work item:
- If the item is not assigned to you yet, run `+"`bd update <id> --claim`"+`.
- Do the requested work.
- When done, run `+"`bd close <id>`"+`.
Run `+"`gc hook`"+` to see the full queue.
</system-reminder>
`, normalizedWork)
}

func workQueryHasReadyWork(output string) bool {
	if output == "" {
		return false
	}
	// Newer bd versions print a human-readable no-work line to stdout instead
	// of staying silent. Treat that as "no work" for hooks and WakeWork.
	if strings.Contains(output, "No ready work found") {
		return false
	}
	var decoded any
	if err := json.Unmarshal([]byte(output), &decoded); err == nil {
		switch v := decoded.(type) {
		case []any:
			return len(v) > 0
		case map[string]any:
			return len(v) > 0
		case nil:
			return false
		}
	}
	return true
}

func normalizeWorkQueryOutput(output string) string {
	if output == "" {
		return output
	}
	var decoded any
	if err := json.Unmarshal([]byte(output), &decoded); err != nil {
		return output
	}
	if _, ok := decoded.(map[string]any); !ok {
		return output
	}
	normalized, err := json.Marshal([]any{decoded})
	if err != nil {
		return output
	}
	return string(normalized)
}
