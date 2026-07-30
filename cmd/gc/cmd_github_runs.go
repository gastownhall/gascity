package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
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

// githubRunEvaluationResult pairs a monitor with its observed evaluation (or
// the error observing it produced).
type githubRunEvaluationResult struct {
	monitor config.GitHubRunMonitor
	eval    redstreak.Evaluation
	err     error
}

func doGitHubRunsEvaluate(opts githubRunsEvaluateOptions, stdout, stderr io.Writer) int {
	cityPath, err := resolveCity()
	if err != nil {
		fmt.Fprintf(stderr, "gc github runs evaluate: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	cfg, prov, err := loadConfigCommandCityConfig(cityPath)
	if err != nil {
		fmt.Fprintf(stderr, "gc github runs evaluate: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	for _, warning := range prov.Warnings {
		fmt.Fprintf(stderr, "gc github runs evaluate: warning: %s\n", warning) //nolint:errcheck // best-effort stderr
	}

	monitors, err := selectGitHubRunMonitors(cfg, opts.monitorName)
	if err != nil {
		fmt.Fprintf(stderr, "gc github runs evaluate: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}

	ctx := context.Background()
	token, err := resolveGitHubToken(ctx)
	if err != nil {
		fmt.Fprintf(stderr, "gc github runs evaluate: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	client := newGitHubRunsEvaluateClient(token)

	// Phase 1: evaluate every selected monitor unconditionally. One
	// monitor's observation failure must never suppress evaluation of the
	// others -- the mutation decision below needs every monitor's outcome
	// before any bead is written.
	results := make([]githubRunEvaluationResult, 0, len(monitors))
	for _, monitor := range monitors {
		eval, evalErr := client.Evaluate(ctx, monitor)
		results = append(results, githubRunEvaluationResult{monitor: monitor, eval: eval, err: evalErr})
	}

	// Phase 2: classify errors. A hard error (anything but a data-contract
	// error) aborts mutation for the entire batch -- an observation we
	// cannot trust for one monitor must not let us write confidently for
	// the rest. A data-contract error is a soft error: the aggregate job
	// couldn't be unambiguously resolved for one run, but the walked streak
	// up to that point is still trustworthy and gets recorded as red.
	hasAnyError := false
	hasHardError := false
	for _, r := range results {
		if r.err == nil {
			continue
		}
		hasAnyError = true
		fmt.Fprintf(stderr, "gc github runs evaluate: monitor %q: %v\n", r.monitor.Name, r.err) //nolint:errcheck // best-effort stderr
		var dataErr *redstreak.DataContractError
		if !errors.As(r.err, &dataErr) {
			hasHardError = true
		}
	}

	exitCode := 0
	if hasAnyError {
		exitCode = 1
	}
	if hasHardError || opts.dryRun {
		return exitCode
	}

	for _, r := range results {
		if err := applyRedStreakEvaluation(cityPath, cfg, r.monitor, r.eval); err != nil {
			fmt.Fprintf(stderr, "gc github runs evaluate: monitor %q: %v\n", r.monitor.Name, err) //nolint:errcheck // best-effort stderr
			return 1
		}
		fmt.Fprintf(stdout, "%s: %d consecutive failure(s), %d consecutive success(es) (run %d)\n", //nolint:errcheck // best-effort stdout
			r.monitor.Name, r.eval.ConsecutiveFailures, r.eval.ConsecutiveSuccesses, r.eval.LatestRunID)
	}

	return exitCode
}

// selectGitHubRunMonitors returns the configured run monitors to evaluate.
// An empty name selects all of them; otherwise exactly the named monitor.
func selectGitHubRunMonitors(cfg *config.City, name string) ([]config.GitHubRunMonitor, error) {
	if cfg == nil || len(cfg.GitHub.RunMonitors) == 0 {
		return nil, errors.New("no github.run_monitor entries are configured")
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return append([]config.GitHubRunMonitor(nil), cfg.GitHub.RunMonitors...), nil
	}
	for _, monitor := range cfg.GitHub.RunMonitors {
		if strings.TrimSpace(monitor.Name) == name {
			return []config.GitHubRunMonitor{monitor}, nil
		}
	}
	return nil, fmt.Errorf("github.run_monitor %q not found", name)
}

// applyRedStreakEvaluation opens the monitor's rig store, decides whether its
// evaluation warrants creating or updating an episode bead, and applies that
// mutation. It is a no-op when the evaluation is neither an active red streak
// nor a recovery worth recording.
func applyRedStreakEvaluation(cityPath string, cfg *config.City, monitor config.GitHubRunMonitor, eval redstreak.Evaluation) error {
	rig, ok := rigByName(cfg, strings.TrimSpace(monitor.Rig))
	if !ok {
		return fmt.Errorf("rig %q not found", monitor.Rig)
	}
	scopeRoot := resolveStoreScopeRoot(cityPath, rig.Path)
	store, err := openGitHubRunEvaluateStore(cityPath, scopeRoot)
	if err != nil {
		return err
	}

	existing, err := store.ListByMetadata(map[string]string{"redstreak.monitor": monitor.Name}, 0)
	if err != nil {
		return fmt.Errorf("checking existing red-streak episodes: %w", err)
	}
	open := firstOpenRedStreakEpisode(existing)

	switch redStreakAction(open, monitor.ThresholdOrDefault(), eval) {
	case "create":
		if _, err := createRedStreakEpisode(store, monitor, eval); err != nil {
			return fmt.Errorf("creating red-streak episode: %w", err)
		}
	case "update":
		if err := updateRedStreakEpisode(store, *open, monitor, eval); err != nil {
			return fmt.Errorf("updating red-streak episode %s: %w", open.ID, err)
		}
	}
	return nil
}

// redStreakAction decides what, if anything, to do with a monitor's episode
// bead given its current evaluation. ConsecutiveFailures and
// ConsecutiveSuccesses are mutually exclusive by construction of
// redstreak.EvaluateHistory, so isRed and isGreen never both hold.
//
//   - No open episode: only a qualifying red streak starts one ("create").
//     A green-only evaluation with no active incident has nothing to
//     recover from, so it is not recorded.
//   - An open episode already reflects the evaluation's latest run: no-op.
//     This is what makes repeated evaluation of the same run idempotent.
//   - Otherwise: the open episode advances to the new run ("update"), which
//     covers both a deepening red streak and a recovery being recorded
//     against the still-open incident. Recovery never auto-closes it.
func redStreakAction(existing *beads.Bead, threshold int, eval redstreak.Evaluation) string {
	isRed := eval.ConsecutiveFailures >= threshold
	isGreen := eval.ConsecutiveSuccesses > 0
	if !isRed && !isGreen {
		return "none"
	}
	if existing == nil {
		if isRed {
			return "create"
		}
		return "none"
	}
	if existing.Metadata["redstreak.run_id"] == strconv.FormatInt(eval.LatestRunID, 10) {
		return "none"
	}
	return "update"
}

// firstOpenRedStreakEpisode returns the first non-closed bead, or nil. A
// closed episode is invisible here so a fresh qualifying evaluation always
// starts a new episode rather than reopening or duplicating the old one.
func firstOpenRedStreakEpisode(candidates []beads.Bead) *beads.Bead {
	for i := range candidates {
		if candidates[i].Status != "closed" {
			return &candidates[i]
		}
	}
	return nil
}

// redStreakMetadata builds the metadata written on both create and update.
// redstreak.recovery_seen_at is included only when this evaluation observed
// at least one consecutive success, so a red-only mutation leaves any prior
// recovery timestamp untouched (metadata writes merge key-by-key rather than
// replacing the map).
func redStreakMetadata(monitor config.GitHubRunMonitor, eval redstreak.Evaluation) map[string]string {
	metadata := map[string]string{
		"redstreak.monitor":              monitor.Name,
		"redstreak.workflow_file":        monitor.WorkflowFile,
		"redstreak.run_id":               strconv.FormatInt(eval.LatestRunID, 10),
		"redstreak.consecutive_count":    strconv.Itoa(eval.ConsecutiveFailures),
		"redstreak.aggregate_conclusion": eval.AggregateConclusion,
		"redstreak.recovery_ready":       strconv.FormatBool(eval.ConsecutiveSuccesses >= monitor.ThresholdOrDefault()),
		"redstreak.data_contract_error":  strconv.FormatBool(eval.DataContractError),
		"redstreak.first_run_id":         strconv.FormatInt(eval.FirstRunID, 10),
		"redstreak.first_run_url":        eval.FirstRunURL,
	}
	if eval.ConsecutiveSuccesses > 0 {
		metadata["redstreak.recovery_seen_at"] = eval.EvaluatedAt.Format(time.RFC3339)
	}
	return metadata
}

// createRedStreakEpisode opens a new episode bead for a monitor's red streak.
// gc.routed_to is set only here, at creation: subsequent updates never
// resend it, so it survives untouched across the episode's lifetime.
func createRedStreakEpisode(store beads.Store, monitor config.GitHubRunMonitor, eval redstreak.Evaluation) (beads.Bead, error) {
	metadata := redStreakMetadata(monitor, eval)
	metadata[beadmeta.RoutedToMetadataKey] = monitor.Route
	priority := redStreakPriorityValue(monitor.PriorityOrDefault())
	return store.Create(beads.Bead{
		Title:       redStreakEpisodeTitle(monitor),
		Type:        "task",
		Priority:    &priority,
		Description: redStreakEpisodeDescription(monitor, eval),
		Labels:      []string{"ci-nightly-red-streak"},
		Metadata:    metadata,
	})
}

// updateRedStreakEpisode advances an existing episode bead to a monitor's
// latest evaluation.
func updateRedStreakEpisode(store beads.Store, existing beads.Bead, monitor config.GitHubRunMonitor, eval redstreak.Evaluation) error {
	metadata := redStreakMetadata(monitor, eval)
	desc := redStreakEpisodeDescription(monitor, eval)
	return store.Update(existing.ID, beads.UpdateOpts{
		Metadata:    metadata,
		Description: &desc,
	})
}

// redStreakPriorityValue converts a "P<n>" priority label to bd's priority
// int. An unparseable label defaults to P2.
func redStreakPriorityValue(priority string) int {
	trimmed := strings.TrimPrefix(strings.ToUpper(strings.TrimSpace(priority)), "P")
	value, err := strconv.Atoi(trimmed)
	if err != nil {
		return 2
	}
	return value
}

func redStreakEpisodeTitle(monitor config.GitHubRunMonitor) string {
	return fmt.Sprintf("Red streak: %s (%s/%s %s)", monitor.Name, monitor.Owner, monitor.Repo, monitor.WorkflowFile)
}

func redStreakEpisodeDescription(monitor config.GitHubRunMonitor, eval redstreak.Evaluation) string {
	var b strings.Builder
	fmt.Fprintf(&b, "GitHub Actions red-streak monitor %q observed %d consecutive failing run(s) of %q.\n\n",
		monitor.Name, eval.ConsecutiveFailures, monitor.AggregateJob)
	fmt.Fprintf(&b, "Repository: %s/%s\n", monitor.Owner, monitor.Repo)
	fmt.Fprintf(&b, "Workflow: %s\n", monitor.WorkflowFile)
	if eval.LatestRunURL != "" {
		fmt.Fprintf(&b, "Latest run: %s\n", eval.LatestRunURL)
	}
	if eval.FirstRunID != 0 && eval.FirstRunURL != "" {
		fmt.Fprintf(&b, "First run: %s\n", eval.FirstRunURL)
	}
	if eval.AggregateConclusion != "" {
		fmt.Fprintf(&b, "Aggregate conclusion: %s\n", eval.AggregateConclusion)
	}
	if eval.DataContractError {
		b.WriteString("\nThe aggregate job could not be unambiguously resolved in at least one observed run.\n")
	}
	fmt.Fprintf(&b, "\nRoute: %s\n", monitor.Route)
	return b.String()
}
