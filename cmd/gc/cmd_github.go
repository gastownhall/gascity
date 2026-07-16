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

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/githubmonitor"
	"github.com/gastownhall/gascity/internal/molecule"
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
	// attachGitHubPRRepairWorkflow instantiates the configured repair workflow
	// on a freshly created repair bead. It is a package var so tests can stub
	// the molecule instantiation without on-disk formulas.
	attachGitHubPRRepairWorkflow = defaultAttachGitHubPRRepairWorkflow
	// nudgeGitHubPRRepairWorker notifies an already-assigned repair worker that
	// the PR's failures changed, instead of duplicating work. Package var for
	// the same testability reason.
	nudgeGitHubPRRepairWorker = defaultNudgeGitHubPRRepairWorker
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
	SchemaVersion     string                 `json:"schema_version"`
	CityPath          string                 `json:"city_path"`
	MonitorCount      int                    `json:"monitor_count"`
	ResultCount       int                    `json:"result_count"`
	ActionableCount   int                    `json:"actionable_count"`
	Results           []githubmonitor.Result `json:"results"`
	RepairBeads       []githubPRRepairBead   `json:"repair_beads,omitempty"`
	ExistingRepairs   int                    `json:"existing_repairs,omitempty"`
	CreatedRepairs    int                    `json:"created_repairs,omitempty"`
	UpdatedRepairs    int                    `json:"updated_repairs,omitempty"`
	DispatchedRepairs int                    `json:"dispatched_repairs,omitempty"`
}

type githubPRRepairBead struct {
	ID         string `json:"id"`
	PR         int    `json:"pr"`
	URL        string `json:"url,omitempty"`
	Created    bool   `json:"created"`
	Updated    bool   `json:"updated,omitempty"`
	Dispatched bool   `json:"dispatched,omitempty"`
	Route      string `json:"route,omitempty"`
	Workflow   string `json:"workflow,omitempty"`
}

// githubPRRepairOutcome reports what ensureGitHubPRRepairBead did for one PR.
type githubPRRepairOutcome struct {
	bead       beads.Bead
	created    bool
	updated    bool
	dispatched bool
	// dispatchErr records a non-fatal workflow-attach failure. The repair bead
	// still exists and stays routed; the controller can scale a worker without
	// the molecule, so callers surface this as a warning rather than aborting.
	dispatchErr error
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
					outcome, err := ensureGitHubPRRepairBead(cityPath, cfg, monitor, prResult)
					if err != nil {
						fmt.Fprintf(stderr, "gc github pr backfill: repair bead for %s/%s#%d: %v\n", prResult.Owner, prResult.Repo, prResult.Number, err) //nolint:errcheck // best-effort stderr
						return 1
					}
					if outcome.dispatchErr != nil {
						fmt.Fprintf(stderr, "gc github pr backfill: warning: repair workflow for %s/%s#%d not attached: %v\n", prResult.Owner, prResult.Repo, prResult.Number, outcome.dispatchErr) //nolint:errcheck // best-effort stderr
					}
					result.RepairBeads = append(result.RepairBeads, githubPRRepairBead{
						ID:         outcome.bead.ID,
						PR:         prResult.Number,
						URL:        prResult.URL,
						Created:    outcome.created,
						Updated:    outcome.updated,
						Dispatched: outcome.dispatched,
						Route:      prResult.RepairRoute,
						Workflow:   monitor.RepairWorkflowOrDefault(),
					})
					switch {
					case outcome.created:
						result.CreatedRepairs++
					case outcome.updated:
						result.ExistingRepairs++
						result.UpdatedRepairs++
					}
					if outcome.dispatched {
						result.DispatchedRepairs++
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

func ensureGitHubPRRepairBead(cityPath string, cfg *config.City, monitor config.GitHubPRMonitor, result githubmonitor.Result) (githubPRRepairOutcome, error) {
	if !result.Actionable {
		return githubPRRepairOutcome{}, errors.New("result is not actionable")
	}
	rig, ok := rigByName(cfg, strings.TrimSpace(monitor.Rig))
	if !ok {
		return githubPRRepairOutcome{}, fmt.Errorf("rig %q not found", monitor.Rig)
	}
	scopeRoot := resolveStoreScopeRoot(cityPath, rig.Path)
	store, err := openGitHubPRRepairStore(cityPath, scopeRoot)
	if err != nil {
		return githubPRRepairOutcome{}, err
	}

	// Dedupe by owner/repo/pr/head_sha only. The failure kind (blocked,
	// checks_failed, ...) transitions as GitHub re-evaluates the same commit,
	// so including it in the key would spawn a fresh bead per transition
	// (ga-kfufjq6). A changed head SHA is genuinely new work and keys a new bead.
	filters := githubPRRepairDedupeMetadata(result)
	existing, err := store.ListByMetadata(filters, 0)
	if err != nil {
		return githubPRRepairOutcome{}, fmt.Errorf("checking existing repair beads: %w", err)
	}
	if open := firstOpenRepairBead(existing); open != nil {
		updated, err := refreshGitHubPRRepairBead(store, *open, result)
		if err != nil {
			return githubPRRepairOutcome{}, err
		}
		// A worker already on this PR/head is nudged with the refreshed
		// failures rather than handed a duplicate bead.
		if assignee := strings.TrimSpace(updated.Assignee); assignee != "" {
			nudgeGitHubPRRepairWorker(cityPath, assignee, updated, result)
		}
		return githubPRRepairOutcome{bead: updated, updated: true}, nil
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
		return githubPRRepairOutcome{}, fmt.Errorf("creating repair bead: %w", err)
	}
	outcome := githubPRRepairOutcome{bead: created, created: true}
	// Attach the configured repair workflow so the routed bead carries the
	// standard branch/test/push/refinery steps instead of sitting as a raw
	// routed task (ga-y5yhvnk). Attach failure is non-fatal: the bead is
	// created and routed, so the pool scaler can still pick it up.
	if err := attachGitHubPRRepairWorkflow(store, cfg, rig, monitor, created, result); err != nil {
		outcome.dispatchErr = err
		return outcome, nil
	}
	outcome.dispatched = true
	return outcome, nil
}

// firstOpenRepairBead returns the first non-closed bead, or nil. A closed
// repair bead must not suppress a fresh bead when the same PR/head regresses
// after the prior repair was completed.
func firstOpenRepairBead(candidates []beads.Bead) *beads.Bead {
	for i := range candidates {
		if candidates[i].Status != "closed" {
			return &candidates[i]
		}
	}
	return nil
}

// refreshGitHubPRRepairBead updates the volatile state (failure kind, checks,
// merge state, description, route) on an existing repair bead so one bead
// tracks the PR/head across GitHub re-evaluations.
func refreshGitHubPRRepairBead(store beads.Store, existing beads.Bead, result githubmonitor.Result) (beads.Bead, error) {
	metadata := githubPRRepairVolatileMetadata(result)
	metadata[beadmeta.RoutedToMetadataKey] = result.RepairRoute
	desc := githubPRRepairDescription(result)
	if err := store.Update(existing.ID, beads.UpdateOpts{
		Metadata:    metadata,
		Description: &desc,
	}); err != nil {
		return beads.Bead{}, fmt.Errorf("refreshing repair bead %s: %w", existing.ID, err)
	}
	existing.Description = desc
	if existing.Metadata == nil {
		existing.Metadata = make(map[string]string, len(metadata))
	}
	for k, v := range metadata {
		existing.Metadata[k] = v
	}
	return existing, nil
}

func githubPRRepairDedupeMetadata(result githubmonitor.Result) map[string]string {
	return map[string]string{
		"source":          "github-pr-monitor",
		"github.owner":    result.Owner,
		"github.repo":     result.Repo,
		"github.pr":       strconv.Itoa(result.Number),
		"github.head_sha": result.HeadSHA,
	}
}

// githubPRRepairVolatileMetadata holds the fields that change as GitHub
// re-evaluates the same PR/head; these are refreshed on every monitor pass.
func githubPRRepairVolatileMetadata(result githubmonitor.Result) map[string]string {
	return map[string]string{
		"github.failure_kind":       result.FailureKind,
		"github.monitor":            result.Monitor,
		"github.url":                result.URL,
		"github.base":               result.BaseRefName,
		"github.head":               result.HeadRefName,
		"github.merge_state_status": result.MergeStateStatus,
		"github.state":              result.State,
		"github.failed_checks":      strings.Join(result.FailedChecks, "\n"),
		"github.pending_checks":     strings.Join(result.PendingChecks, "\n"),
	}
}

func githubPRRepairMetadata(result githubmonitor.Result) map[string]string {
	metadata := githubPRRepairDedupeMetadata(result)
	for k, v := range githubPRRepairVolatileMetadata(result) {
		metadata[k] = v
	}
	metadata[beadmeta.RoutedToMetadataKey] = result.RepairRoute
	return metadata
}

// defaultAttachGitHubPRRepairWorkflow instantiates the monitor's repair
// workflow as a molecule attached to the repair bead, so routed repair work
// carries the standard polecat steps. The error is treated as non-fatal by the
// caller (the bead is already created and routed).
func defaultAttachGitHubPRRepairWorkflow(store beads.Store, cfg *config.City, rig config.Rig, monitor config.GitHubPRMonitor, bead beads.Bead, result githubmonitor.Result) error {
	workflow := monitor.RepairWorkflowOrDefault()
	if workflow == "" {
		return nil
	}
	searchPaths := cfg.FormulaLayers.SearchPaths(strings.TrimSpace(rig.Name))
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := molecule.CookOn(ctx, store, workflow, searchPaths, molecule.Options{
		ParentID:       bead.ID,
		IdempotencyKey: "github-pr-repair-workflow:" + bead.ID,
		Vars:           githubPRRepairWorkflowVars(bead, result),
	}); err != nil {
		return fmt.Errorf("instantiating repair workflow %q: %w", workflow, err)
	}
	return nil
}

func githubPRRepairWorkflowVars(bead beads.Bead, result githubmonitor.Result) map[string]string {
	return map[string]string{
		"bead_id": bead.ID,
		"bead":    bead.ID,
		"title":   bead.Title,
		"pr":      strconv.Itoa(result.Number),
		"repo":    result.Owner + "/" + result.Repo,
		"branch":  result.HeadRefName,
	}
}

// defaultNudgeGitHubPRRepairWorker best-effort notifies an assigned worker that
// a PR's failures changed. It shells out to `gc session nudge`; failures are
// ignored because the durable bead update is the source of truth.
func defaultNudgeGitHubPRRepairWorker(cityPath, assignee string, bead beads.Bead, result githubmonitor.Result) {
	msg := fmt.Sprintf("GitHub PR %s/%s#%d still needs repair (%s); refreshed failures on %s.",
		result.Owner, result.Repo, result.Number, result.FailureKind, bead.ID)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "gc", "--city", cityPath, "session", "nudge", assignee, msg)
	disableProductMetricsForChild(cmd)
	_ = cmd.Run() //nolint:errcheck // best-effort; the bead update is the durable record
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
	if len(result.RepairBeads) > 0 {
		fmt.Fprintf(stdout, "Repair beads: %d created, %d updated, %d dispatched.\n", //nolint:errcheck
			result.CreatedRepairs, result.UpdatedRepairs, result.DispatchedRepairs)
		for _, rb := range result.RepairBeads {
			action := "existing"
			switch {
			case rb.Created:
				action = "created"
			case rb.Updated:
				action = "updated"
			}
			fmt.Fprintf(stdout, "  %s #%d %s", rb.ID, rb.PR, action) //nolint:errcheck
			if rb.Dispatched {
				fmt.Fprintf(stdout, " dispatched=%s", rb.Workflow) //nolint:errcheck
			}
			if rb.Route != "" {
				fmt.Fprintf(stdout, " route=%s", rb.Route) //nolint:errcheck
			}
			fmt.Fprintln(stdout) //nolint:errcheck
		}
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
