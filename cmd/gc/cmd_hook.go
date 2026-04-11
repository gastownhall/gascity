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
)

func newHookCmd(stdout, stderr io.Writer) *cobra.Command {
	var inject bool
	cmd := &cobra.Command{
		Use:   "hook [agent]",
		Short: "Check for available work (use --inject for Stop hook output)",
		Long: `Checks for available work using the agent's work_query config.

Without --inject: prints raw output, exits 0 if work exists, 1 if empty.
With --inject: wraps output in <system-reminder> for hook injection, always exits 0.

The agent is determined from $GC_AGENT or a positional argument.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			if cmdHook(args, inject, stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&inject, "inject", false, "output <system-reminder> block for hook injection")
	return cmd
}

// cmdHook is the CLI entry point for gc hook. Resolves the agent from
// $GC_AGENT or a positional argument, loads the city config, and runs
// the agent's work query.
func cmdHook(args []string, inject bool, stdout, stderr io.Writer) int {
	originalAlias := os.Getenv("GC_ALIAS")
	originalAgent := os.Getenv("GC_AGENT")
	originalSessionName := os.Getenv("GC_SESSION_NAME")
	originalSessionID := os.Getenv("GC_SESSION_ID")
	originalSessionOrigin := os.Getenv("GC_SESSION_ORIGIN")
	originalTemplate := os.Getenv("GC_TEMPLATE")

	agentName := originalAlias
	if agentName == "" {
		agentName = originalAgent
	}
	sessionTemplateContext := false
	if len(args) == 0 {
		template := strings.TrimSpace(originalTemplate)
		hasSessionContext := strings.TrimSpace(originalSessionName) != "" ||
			strings.TrimSpace(originalSessionID) != ""
		if template != "" && hasSessionContext {
			agentName = template
			sessionTemplateContext = true
		}
	}
	namedTemplateContext := false
	if len(args) == 0 && strings.TrimSpace(os.Getenv("GC_SESSION_ORIGIN")) == "named" {
		if template := strings.TrimSpace(os.Getenv("GC_TEMPLATE")); template != "" {
			agentName = template
			namedTemplateContext = true
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
	cfg, err := loadCityConfig(cityPath)
	if err != nil {
		if inject {
			return 0
		}
		fmt.Fprintf(stderr, "gc hook: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
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

	// Many built-in/default work queries key off resolved session identity.
	// When hook is invoked as `gc hook <agent>`, export the fully resolved
	// agent/session names so the query sees the same identity that resolution
	// used instead of the caller's raw input string.
	resolvedAgentName := a.QualifiedName()
	resolvedSessionName := cliSessionName(cityPath, cfg.Workspace.Name, resolvedAgentName, cfg.Workspace.SessionTemplate)
	restoreAgent := os.Getenv("GC_AGENT")
	restoreSession := os.Getenv("GC_SESSION_NAME")
	_ = os.Setenv("GC_AGENT", resolvedAgentName)
	defer func() {
		if restoreAgent == "" {
			_ = os.Unsetenv("GC_AGENT")
		} else {
			_ = os.Setenv("GC_AGENT", restoreAgent)
		}
	}()
	_ = os.Setenv("GC_SESSION_NAME", resolvedSessionName)
	defer func() {
		if restoreSession == "" {
			_ = os.Unsetenv("GC_SESSION_NAME")
		} else {
			_ = os.Setenv("GC_SESSION_NAME", restoreSession)
		}
	}()

	workQuery := a.EffectiveWorkQuery()
	workDir := agentCommandDir(cityPath, &a, cfg.Rigs)
	workEnv := controllerWorkQueryEnv(cityPath, cfg, &a)
	if workEnv == nil {
		workEnv = map[string]string{}
	}

	// Build the work query subprocess environment. Rig-backed agents get
	// rig-scoped BEADS_DIR / GC_RIG_ROOT / Dolt coordinates so the query
	// reads the rig store rather than whatever BEADS_DIR the parent
	// process happens to inherit (issue #514). Many built-in work queries
	// also key off session identity. Explicit hook targets get resolved
	// names; named-session context preserves the runtime-supplied owner
	// env while selecting the backing config through GC_TEMPLATE.
	agentForQuery := resolvedAgentName
	sessionForQuery := resolvedSessionName
	if sessionTemplateContext {
		agentForQuery = originalAlias
		if agentForQuery == "" {
			agentForQuery = originalAgent
		}
		if agentForQuery == "" {
			agentForQuery = originalSessionName
		}
		sessionForQuery = originalSessionName
	}
	workEnv["GC_AGENT"] = agentForQuery
	workEnv["GC_SESSION_NAME"] = sessionForQuery
	if sessionTemplateContext {
		workEnv["GC_ALIAS"] = originalAlias
		workEnv["GC_SESSION_ID"] = originalSessionID
		workEnv["GC_SESSION_ORIGIN"] = originalSessionOrigin
		workEnv["GC_TEMPLATE"] = originalTemplate
	}
	runner := func(command, dir string) (string, error) {
		return shellWorkQueryWithEnv(command, dir, workEnv)
	}
	return doHook(workQuery, workDir, inject, runner, stdout, stderr)
}

// WorkQueryRunner runs a work query command and returns its stdout.
// dir sets the command's working directory.
type WorkQueryRunner func(command, dir string) (string, error)

func shellWorkQueryWithEnv(command, dir string, env map[string]string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "sh", "-c", command)
	cmd.WaitDelay = 2 * time.Second
	if dir != "" {
		cmd.Dir = dir
	}
	if len(env) > 0 {
		cmd.Env = mergeRuntimeEnv(os.Environ(), env)
	}
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
		return nil
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
			fmt.Fprintf(stdout, "<system-reminder>\nYou have pending work. Pick up the next item:\n\n<work-items>\n%s\n</work-items>\n\nClaim it and start working. Run 'gc hook' to see the full queue.\n</system-reminder>\n", normalized) //nolint:errcheck // best-effort stdout
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
