package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/githubmonitor"
	"github.com/spf13/cobra"
)

type githubPRLister interface {
	ListOpenPullRequests(context.Context, string, string) ([]githubmonitor.PullRequest, error)
}

var (
	newGitHubPRBackfillClient = func(token string) githubPRLister {
		return githubmonitor.NewGraphQLClient(token)
	}
	resolveGitHubTokenForBackfill = resolveGitHubToken
	openGitHubPRRepairStore       = func(cityPath, scopeRoot string) (beads.Store, error) {
		return openStoreAtForCity(scopeRoot, cityPath)
	}
)

type githubPRBackfillOptions struct {
	monitorName    string
	jsonOutput     bool
	includeClean   bool
	timeout        time.Duration
	actionableOnly bool
	createRepairs  bool
}

type githubPRBackfillResult struct {
	SchemaVersion   string                 `json:"schema_version"`
	CityPath        string                 `json:"city_path"`
	MonitorCount    int                    `json:"monitor_count"`
	ResultCount     int                    `json:"result_count"`
	ActionableCount int                    `json:"actionable_count"`
	Results         []githubmonitor.Result `json:"results"`
	RepairBeads     []githubPRRepairBead   `json:"repair_beads,omitempty"`
	ExistingRepairs int                    `json:"existing_repairs,omitempty"`
	CreatedRepairs  int                    `json:"created_repairs,omitempty"`
}

type githubPRRepairBead struct {
	ID      string `json:"id"`
	PR      int    `json:"pr"`
	URL     string `json:"url,omitempty"`
	Created bool   `json:"created"`
	Route   string `json:"route,omitempty"`
}

func newGitHubCmd(stdout, stderr io.Writer) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "github",
		Short: "GitHub integration commands",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return cmd.Help()
		},
	}
	cmd.AddCommand(newGitHubPRCmd(stdout, stderr))
	return cmd
}

func newGitHubPRCmd(stdout, stderr io.Writer) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "pr",
		Short: "GitHub pull-request monitor commands",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return cmd.Help()
		},
	}
	cmd.AddCommand(newGitHubPRBackfillCmd(stdout, stderr))
	return cmd
}

func newGitHubPRBackfillCmd(stdout, stderr io.Writer) *cobra.Command {
	opts := githubPRBackfillOptions{
		timeout:        45 * time.Second,
		actionableOnly: true,
	}
	cmd := &cobra.Command{
		Use:   "backfill [monitor-name]",
		Short: "Query configured GitHub PR readiness monitors",
		Long: `Query configured GitHub PR readiness monitors.

The command reads [[github.pr_monitor]] entries from the resolved city
configuration, queries open pull requests from GitHub, and reports PRs that
need repair: failed checks, merge conflicts, blocked mergeability, or branches
behind their base. By default clean and pending-only PRs are omitted; pass
--all to include every observed PR.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			if len(args) == 1 {
				opts.monitorName = args[0]
			}
			if opts.includeClean {
				opts.actionableOnly = false
			}
			if doGitHubPRBackfill(opts, stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&opts.jsonOutput, "json", false, "emit JSON")
	cmd.Flags().BoolVar(&opts.includeClean, "all", false, "include clean and pending-only PRs")
	cmd.Flags().BoolVar(&opts.createRepairs, "create-repair-beads", false, "create deduped repair beads for actionable PRs")
	cmd.Flags().DurationVar(&opts.timeout, "timeout", opts.timeout, "GitHub query timeout")
	return cmd
}

func doGitHubPRBackfill(opts githubPRBackfillOptions, stdout, stderr io.Writer) int {
	cityPath, err := resolveCity()
	if err != nil {
		fmt.Fprintf(stderr, "gc github pr backfill: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	cfg, prov, err := loadConfigCommandCityConfig(cityPath)
	if err != nil {
		fmt.Fprintf(stderr, "gc github pr backfill: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	if !opts.jsonOutput {
		for _, warning := range prov.Warnings {
			fmt.Fprintf(stderr, "gc github pr backfill: warning: %s\n", warning) //nolint:errcheck // best-effort stderr
		}
	}

	monitors, err := selectGitHubPRMonitors(cfg, opts.monitorName)
	if err != nil {
		fmt.Fprintf(stderr, "gc github pr backfill: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	ctx, cancel := context.WithTimeout(context.Background(), opts.timeout)
	defer cancel()

	token, err := resolveGitHubTokenForBackfill(ctx)
	if err != nil {
		fmt.Fprintf(stderr, "gc github pr backfill: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	client := newGitHubPRBackfillClient(token)

	result := githubPRBackfillResult{
		SchemaVersion: "1",
		CityPath:      cityPath,
		MonitorCount:  len(monitors),
	}
	for _, monitor := range monitors {
		prs, err := client.ListOpenPullRequests(ctx, strings.TrimSpace(monitor.Owner), strings.TrimSpace(monitor.Repo))
		if err != nil {
			fmt.Fprintf(stderr, "gc github pr backfill: monitor %q: %v\n", monitor.Name, err) //nolint:errcheck // best-effort stderr
			return 1
		}
		evaluated := githubmonitor.EvaluatePullRequests(monitor, prs)
		for _, prResult := range evaluated {
			if prResult.Actionable {
				result.ActionableCount++
				if opts.createRepairs {
					repair, created, err := ensureGitHubPRRepairBead(cityPath, cfg, monitor, prResult)
					if err != nil {
						fmt.Fprintf(stderr, "gc github pr backfill: repair bead for %s/%s#%d: %v\n", prResult.Owner, prResult.Repo, prResult.Number, err) //nolint:errcheck // best-effort stderr
						return 1
					}
					result.RepairBeads = append(result.RepairBeads, githubPRRepairBead{
						ID:      repair.ID,
						PR:      prResult.Number,
						URL:     prResult.URL,
						Created: created,
						Route:   prResult.RepairRoute,
					})
					if created {
						result.CreatedRepairs++
					} else {
						result.ExistingRepairs++
					}
				}
			}
			if opts.actionableOnly && !prResult.Actionable {
				continue
			}
			result.Results = append(result.Results, prResult)
		}
	}
	result.ResultCount = len(result.Results)
	sortGitHubPRBackfillResults(result.Results)

	if opts.jsonOutput {
		if writeCLIJSONLineOrExit(stdout, stderr, "gc github pr backfill", result) != 0 {
			return 1
		}
		return 0
	}
	writeGitHubPRBackfillText(stdout, result)
	return 0
}

func ensureGitHubPRRepairBead(cityPath string, cfg *config.City, monitor config.GitHubPRMonitor, result githubmonitor.Result) (beads.Bead, bool, error) {
	if !result.Actionable {
		return beads.Bead{}, false, errors.New("result is not actionable")
	}
	rig, ok := rigByName(cfg, strings.TrimSpace(monitor.Rig))
	if !ok {
		return beads.Bead{}, false, fmt.Errorf("rig %q not found", monitor.Rig)
	}
	scopeRoot := resolveStoreScopeRoot(cityPath, rig.Path)
	store, err := openGitHubPRRepairStore(cityPath, scopeRoot)
	if err != nil {
		return beads.Bead{}, false, err
	}

	filters := githubPRRepairDedupeMetadata(result)
	existing, err := store.ListByMetadata(filters, 1)
	if err != nil {
		return beads.Bead{}, false, fmt.Errorf("checking existing repair beads: %w", err)
	}
	if len(existing) > 0 {
		return existing[0], false, nil
	}

	priority := 1
	created, err := store.Create(beads.Bead{
		Title:       githubPRRepairTitle(result),
		Type:        "task",
		Priority:    &priority,
		Description: githubPRRepairDescription(result),
		Labels:      []string{"github", "ci", "repair", "pr-monitor"},
		Metadata:    githubPRRepairMetadata(result),
	})
	if err != nil {
		return beads.Bead{}, false, fmt.Errorf("creating repair bead: %w", err)
	}
	return created, true, nil
}

func githubPRRepairDedupeMetadata(result githubmonitor.Result) map[string]string {
	return map[string]string{
		"source":              "github-pr-monitor",
		"github.owner":        result.Owner,
		"github.repo":         result.Repo,
		"github.pr":           strconv.Itoa(result.Number),
		"github.head_sha":     result.HeadSHA,
		"github.failure_kind": result.FailureKind,
	}
}

func githubPRRepairMetadata(result githubmonitor.Result) map[string]string {
	metadata := githubPRRepairDedupeMetadata(result)
	metadata["github.monitor"] = result.Monitor
	metadata["github.url"] = result.URL
	metadata["github.base"] = result.BaseRefName
	metadata["github.head"] = result.HeadRefName
	metadata["github.merge_state_status"] = result.MergeStateStatus
	metadata["github.state"] = result.State
	metadata["github.failed_checks"] = strings.Join(result.FailedChecks, "\n")
	metadata["github.pending_checks"] = strings.Join(result.PendingChecks, "\n")
	metadata["gc.routed_to"] = result.RepairRoute
	return metadata
}

func githubPRRepairTitle(result githubmonitor.Result) string {
	title := strings.TrimSpace(result.Title)
	if title == "" {
		title = result.URL
	}
	if title == "" {
		title = result.FailureKind
	}
	return fmt.Sprintf("Repair GitHub PR %s/%s#%d readiness: %s", result.Owner, result.Repo, result.Number, title)
}

func githubPRRepairDescription(result githubmonitor.Result) string {
	var b strings.Builder
	fmt.Fprintf(&b, "GitHub PR readiness monitor %q found actionable work.\n\n", result.Monitor)
	fmt.Fprintf(&b, "Repository: %s/%s\n", result.Owner, result.Repo)
	fmt.Fprintf(&b, "PR: #%d", result.Number)
	if result.URL != "" {
		fmt.Fprintf(&b, " %s", result.URL)
	}
	b.WriteString("\n")
	fmt.Fprintf(&b, "Base: %s\n", result.BaseRefName)
	if result.HeadRefName != "" {
		fmt.Fprintf(&b, "Head: %s\n", result.HeadRefName)
	}
	if result.HeadSHA != "" {
		fmt.Fprintf(&b, "Head SHA: %s\n", result.HeadSHA)
	}
	fmt.Fprintf(&b, "State: %s\n", result.State)
	if result.FailureKind != "" {
		fmt.Fprintf(&b, "Failure kind: %s\n", result.FailureKind)
	}
	if result.MergeStateStatus != "" {
		fmt.Fprintf(&b, "GitHub merge state: %s\n", result.MergeStateStatus)
	}
	if len(result.FailedChecks) > 0 {
		b.WriteString("\nFailed checks:\n")
		for _, check := range result.FailedChecks {
			fmt.Fprintf(&b, "- %s\n", check)
		}
	}
	if len(result.PendingChecks) > 0 {
		b.WriteString("\nPending checks:\n")
		for _, check := range result.PendingChecks {
			fmt.Fprintf(&b, "- %s\n", check)
		}
	}
	if result.RepairRoute != "" {
		fmt.Fprintf(&b, "\nRoute: %s\n", result.RepairRoute)
	}
	return b.String()
}

func selectGitHubPRMonitors(cfg *config.City, name string) ([]config.GitHubPRMonitor, error) {
	if cfg == nil || len(cfg.GitHub.PRMonitors) == 0 {
		return nil, errors.New("no github.pr_monitor entries are configured")
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return append([]config.GitHubPRMonitor(nil), cfg.GitHub.PRMonitors...), nil
	}
	for _, monitor := range cfg.GitHub.PRMonitors {
		if strings.TrimSpace(monitor.Name) == name {
			return []config.GitHubPRMonitor{monitor}, nil
		}
	}
	return nil, fmt.Errorf("github.pr_monitor %q not found", name)
}

func sortGitHubPRBackfillResults(results []githubmonitor.Result) {
	slices.SortFunc(results, func(a, b githubmonitor.Result) int {
		if c := strings.Compare(a.Monitor, b.Monitor); c != 0 {
			return c
		}
		if c := strings.Compare(a.Repo, b.Repo); c != 0 {
			return c
		}
		return a.Number - b.Number
	})
}

func writeGitHubPRBackfillText(stdout io.Writer, result githubPRBackfillResult) {
	if len(result.Results) == 0 {
		fmt.Fprintf(stdout, "No actionable GitHub PR readiness problems found across %d monitor(s).\n", result.MonitorCount) //nolint:errcheck
		return
	}
	for _, pr := range result.Results {
		fmt.Fprintf(stdout, "%s %s/%s#%d %s", pr.Monitor, pr.Owner, pr.Repo, pr.Number, pr.State) //nolint:errcheck
		if pr.FailureKind != "" {
			fmt.Fprintf(stdout, " %s", pr.FailureKind) //nolint:errcheck
		}
		if pr.MergeStateStatus != "" {
			fmt.Fprintf(stdout, " merge=%s", pr.MergeStateStatus) //nolint:errcheck
		}
		if len(pr.FailedChecks) > 0 {
			fmt.Fprintf(stdout, " failed=%s", strings.Join(pr.FailedChecks, ",")) //nolint:errcheck
		}
		if len(pr.PendingChecks) > 0 {
			fmt.Fprintf(stdout, " pending=%s", strings.Join(pr.PendingChecks, ",")) //nolint:errcheck
		}
		if pr.RepairRoute != "" {
			fmt.Fprintf(stdout, " route=%s", pr.RepairRoute) //nolint:errcheck
		}
		if pr.URL != "" {
			fmt.Fprintf(stdout, " url=%s", pr.URL) //nolint:errcheck
		}
		fmt.Fprintln(stdout) //nolint:errcheck
	}
}

func resolveGitHubToken(ctx context.Context) (string, error) {
	for _, key := range []string{"GITHUB_TOKEN", "GH_TOKEN"} {
		if token := strings.TrimSpace(os.Getenv(key)); token != "" {
			return token, nil
		}
	}
	cmd := exec.CommandContext(ctx, "gh", "auth", "token")
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("GitHub token not found in GITHUB_TOKEN/GH_TOKEN and `gh auth token` failed: %w", err)
	}
	token := strings.TrimSpace(string(out))
	if token == "" {
		return "", errors.New("GitHub token not found: `gh auth token` returned empty output")
	}
	return token, nil
}
