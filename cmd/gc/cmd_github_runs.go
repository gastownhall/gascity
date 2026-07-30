package main

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/redstreak"
	"github.com/spf13/cobra"
)

// githubRunEvaluator observes a monitor's current GitHub Actions run/job
// history and evaluates its red-streak state. redstreak.Client satisfies it.
type githubRunEvaluator interface {
	Evaluate(ctx context.Context, monitor config.GitHubRunMonitor) (redstreak.Evaluation, error)
}

var (
	// newGitHubRunsEvaluateClient constructs the GitHub run evaluator used by
	// `gc github runs evaluate`. Package var so tests can stub it without
	// making real GitHub API calls.
	newGitHubRunsEvaluateClient = func(token string) githubRunEvaluator {
		return redstreak.NewClient(token)
	}
	// openGitHubRunEvaluateStore opens the beads store backing red-streak
	// episode beads. Package var for the same testability reason.
	openGitHubRunEvaluateStore = func(cityPath, scopeRoot string) (beads.Store, error) {
		return openStoreAtForCity(scopeRoot, cityPath)
	}
)

type githubRunsEvaluateOptions struct {
	monitorName string
	dryRun      bool
}

func newGitHubRunsCmd(stdout, stderr io.Writer) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "runs",
		Short: "GitHub Actions scheduled-run red-streak monitor commands",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return cmd.Help()
		},
	}
	cmd.AddCommand(newGitHubRunsEvaluateCmd(stdout, stderr))
	return cmd
}

func newGitHubRunsEvaluateCmd(stdout, stderr io.Writer) *cobra.Command {
	opts := githubRunsEvaluateOptions{}
	cmd := &cobra.Command{
		Use:   "evaluate",
		Short: "Evaluate configured GitHub Actions red-streak monitors",
		Long: `Evaluate configured GitHub Actions scheduled-run red-streak monitors.

The command reads [[github.run_monitor]] entries from the resolved city
configuration, polls each monitor's workflow run history from GitHub, and
maintains a durable episode bead per monitor once its aggregate job reaches
the configured consecutive-red threshold. Recovery is recorded but never
auto-closes an episode; a human must review and close it. By default all
configured monitors are evaluated; pass --monitor to evaluate just one.`,
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			if doGitHubRunsEvaluate(opts, stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&opts.monitorName, "monitor", "", "evaluate only the named monitor (default: all configured monitors)")
	cmd.Flags().BoolVar(&opts.dryRun, "dry-run", false, "evaluate and report without mutating any episode bead")
	return cmd
}

func doGitHubRunsEvaluate(_ githubRunsEvaluateOptions, _, stderr io.Writer) int {
	fmt.Fprintf(stderr, "gc github runs evaluate: %v\n", errors.New("not implemented")) //nolint:errcheck // best-effort stderr
	return 1
}
