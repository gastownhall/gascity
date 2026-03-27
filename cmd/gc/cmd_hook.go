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
	agentName := os.Getenv("GC_AGENT")
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

	workQuery := a.EffectiveWorkQuery()
	workDir := agentCommandDir(cityPath, &a, cfg.Rigs)
	return doHook(workQuery, workDir, inject, shellWorkQuery, stdout, stderr)
}

// WorkQueryRunner runs a work query command and returns its stdout.
// dir sets the command's working directory.
type WorkQueryRunner func(command, dir string) (string, error)

// shellWorkQuery runs a work query command via sh -c and returns stdout.
// dir sets the command's working directory. Times out after 30 seconds.
func shellWorkQuery(command, dir string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "sh", "-c", command)
	cmd.WaitDelay = 2 * time.Second
	if dir != "" {
		cmd.Dir = dir
	}
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("running work query %q: %w", command, err)
	}
	return string(out), nil
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
	hasWork := workQueryHasReadyWork(trimmed)

	if inject {
		if hasWork {
			fmt.Fprintf(stdout, "<system-reminder>\nYou have pending work. Pick up the next item:\n\n<work-items>\n%s\n</work-items>\n\nStart working on it. Run 'gc hook' to see the full queue.\n</system-reminder>\n", trimmed) //nolint:errcheck // best-effort stdout
		}
		return 0 // --inject always exits 0
	}

	// Non-inject mode: print raw output. Return 0 only when work exists.
	if !hasWork {
		if trimmed != "" {
			fmt.Fprint(stdout, output) //nolint:errcheck // best-effort stdout
		}
		return 1
	}
	fmt.Fprint(stdout, output) //nolint:errcheck // best-effort stdout
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
