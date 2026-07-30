package config

import "errors"

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
	return 0
}

// PriorityOrDefault returns the configured episode priority, or the default
// when unset.
func (m GitHubRunMonitor) PriorityOrDefault() string {
	return ""
}

// EventOrDefault returns the configured trigger event to filter runs by, or
// the default when unset.
func (m GitHubRunMonitor) EventOrDefault() string {
	return ""
}

// ValidateGitHubRunMonitors checks GitHub scheduled-run red-streak monitor
// declarations.
func ValidateGitHubRunMonitors(cfg *City) error {
	_ = cfg
	return errors.New("not implemented")
}
