package config

import (
	"fmt"
	"strings"
	"time"
)

const (
	defaultGitHubRunMonitorThreshold = 3
	defaultGitHubRunMonitorPriority  = "P1"
	defaultGitHubRunMonitorEvent     = "schedule"
)

// GitHubRunMonitor declares a scheduled GitHub Actions workflow to watch for
// consecutive red (failing) runs of a designated aggregate job.
type GitHubRunMonitor struct {
	// Name is the stable monitor identity used by patches and diagnostics.
	Name string `toml:"name" jsonschema:"required"`
	// Owner is the GitHub repository owner or organization.
	Owner string `toml:"owner" jsonschema:"required"`
	// Repo is the GitHub repository name.
	Repo string `toml:"repo" jsonschema:"required"`
	// WorkflowFile is the workflow file name (e.g. "nightly.yml") whose runs
	// are polled.
	WorkflowFile string `toml:"workflow_file" jsonschema:"required"`
	// AggregateJob is the job name within the workflow whose own conclusion
	// is treated as the authoritative pass/fail verdict for the run.
	AggregateJob string `toml:"aggregate_job" jsonschema:"required"`
	// ActivatedAfter is an RFC3339 timestamp; runs started at or before this
	// boundary are ignored so pre-enrollment history cannot trip the streak.
	ActivatedAfter string `toml:"activated_after" jsonschema:"required"`
	// Threshold is the number of consecutive red aggregate runs required
	// before an episode bead is created. Zero/unset defaults to 3.
	Threshold int `toml:"threshold,omitempty"`
	// Rig is the Gas City rig that owns episode beads for this monitor.
	Rig string `toml:"rig" jsonschema:"required"`
	// Route is the operator-supplied route target for episode beads.
	Route string `toml:"route" jsonschema:"required"`
	// Priority is the episode bead priority. Empty defaults to "P1".
	Priority string `toml:"priority,omitempty"`
	// Event is the workflow trigger event to filter runs by (e.g.
	// "schedule"). Empty defaults to "schedule".
	Event string `toml:"event,omitempty"`
	// Notify lists session or mail recipients for episode notifications.
	Notify []string `toml:"notify,omitempty"`
}

// ThresholdOrDefault returns the configured consecutive-red threshold, or
// the default when unset.
func (m GitHubRunMonitor) ThresholdOrDefault() int {
	if m.Threshold > 0 {
		return m.Threshold
	}
	return defaultGitHubRunMonitorThreshold
}

// PriorityOrDefault returns the configured episode priority, or the default
// when unset.
func (m GitHubRunMonitor) PriorityOrDefault() string {
	if priority := strings.TrimSpace(m.Priority); priority != "" {
		return priority
	}
	return defaultGitHubRunMonitorPriority
}

// EventOrDefault returns the configured trigger event to filter runs by, or
// the default when unset.
func (m GitHubRunMonitor) EventOrDefault() string {
	if event := strings.TrimSpace(m.Event); event != "" {
		return event
	}
	return defaultGitHubRunMonitorEvent
}

// ValidateGitHubRunMonitors checks GitHub scheduled-run red-streak monitor
// declarations.
func ValidateGitHubRunMonitors(cfg *City) error {
	if cfg == nil || len(cfg.GitHub.RunMonitors) == 0 {
		return nil
	}

	rigs := make(map[string]bool, len(cfg.Rigs))
	for _, rig := range cfg.Rigs {
		rigs[rig.Name] = true
	}

	seenNames := make(map[string]int, len(cfg.GitHub.RunMonitors))
	seenRepoWorkflow := make(map[string]string, len(cfg.GitHub.RunMonitors))
	for i, monitor := range cfg.GitHub.RunMonitors {
		ctx := fmt.Sprintf("github.run_monitor[%d]", i)
		name := strings.TrimSpace(monitor.Name)
		if name == "" {
			return fmt.Errorf("%s: name is required", ctx)
		}
		if prev, ok := seenNames[name]; ok {
			return fmt.Errorf("%s %q: duplicate name also used by github.run_monitor[%d]", ctx, name, prev)
		}
		seenNames[name] = i

		owner := strings.TrimSpace(monitor.Owner)
		if owner == "" {
			return fmt.Errorf("%s %q: owner is required", ctx, name)
		}
		repo := strings.TrimSpace(monitor.Repo)
		if repo == "" {
			return fmt.Errorf("%s %q: repo is required", ctx, name)
		}
		workflowFile := strings.TrimSpace(monitor.WorkflowFile)
		if workflowFile == "" {
			return fmt.Errorf("%s %q: workflow_file is required", ctx, name)
		}
		if strings.TrimSpace(monitor.AggregateJob) == "" {
			return fmt.Errorf("%s %q: aggregate_job is required", ctx, name)
		}
		activatedAfter := strings.TrimSpace(monitor.ActivatedAfter)
		if activatedAfter == "" {
			return fmt.Errorf("%s %q: activated_after is required", ctx, name)
		}
		if _, err := time.Parse(time.RFC3339, activatedAfter); err != nil {
			return fmt.Errorf("%s %q: activated_after must be RFC3339: %w", ctx, name, err)
		}
		if monitor.Threshold < 0 {
			return fmt.Errorf("%s %q: threshold must be at least 1", ctx, name)
		}
		rig := strings.TrimSpace(monitor.Rig)
		if rig == "" {
			return fmt.Errorf("%s %q: rig is required", ctx, name)
		}
		if !rigs[rig] {
			return fmt.Errorf("%s %q: rig %q is not declared", ctx, name, rig)
		}
		if strings.TrimSpace(monitor.Route) == "" {
			return fmt.Errorf("%s %q: route is required", ctx, name)
		}
		for _, recipient := range monitor.Notify {
			if strings.TrimSpace(recipient) == "" {
				return fmt.Errorf("%s %q: notify contains an empty recipient", ctx, name)
			}
		}

		repoWorkflowKey := strings.ToLower(owner) + "/" + strings.ToLower(repo) + "@" + strings.ToLower(workflowFile)
		if prev, ok := seenRepoWorkflow[repoWorkflowKey]; ok {
			return fmt.Errorf("%s %q: duplicate repo/workflow %s also monitored by %q", ctx, name, repoWorkflowKey, prev)
		}
		seenRepoWorkflow[repoWorkflowKey] = name
	}
	return nil
}
