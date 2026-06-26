package config

import (
	"fmt"
	"regexp"
	"strings"
	"time"
)

var validGitHubWebhookSecretEnv = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

const defaultGitHubMergeQueuePolicy = "observe"

// defaultGitHubRepairWorkflow is the formula attached to repair beads when a
// monitor does not configure an explicit repair_workflow. It carries the
// standard polecat branch/test/push/refinery steps.
const defaultGitHubRepairWorkflow = "mol-polecat-work"

// GitHubConfig groups GitHub-facing repository monitor declarations.
type GitHubConfig struct {
	// PRMonitors declares GitHub pull-request readiness monitors.
	PRMonitors []GitHubPRMonitor `toml:"pr_monitor,omitempty"`
}

// GitHubPRMonitor declares how one repository/base-branch set is monitored
// and where durable repair work should be routed when readiness fails.
type GitHubPRMonitor struct {
	// Name is the stable monitor identity used by patches and diagnostics.
	Name string `toml:"name" jsonschema:"required"`
	// Owner is the GitHub repository owner or organization.
	Owner string `toml:"owner" jsonschema:"required"`
	// Repo is the GitHub repository name.
	Repo string `toml:"repo" jsonschema:"required"`
	// BaseBranches lists the base branches this monitor owns.
	BaseBranches []string `toml:"base_branches" jsonschema:"required"`
	// Rig is the Gas City rig that owns repair work for this repository.
	Rig string `toml:"rig" jsonschema:"required"`
	// Notify lists session or mail recipients for readiness notifications.
	Notify []string `toml:"notify,omitempty"`
	// RepairRoute is the operator-supplied route target for repair work.
	RepairRoute string `toml:"repair_route" jsonschema:"required"`
	// RepairWorkflow is the formula attached to repair beads created for this
	// monitor. Empty defaults to the standard polecat repair workflow so routed
	// repair work carries the branch/test/push/refinery steps instead of
	// sitting as a raw routed task.
	RepairWorkflow string `toml:"repair_workflow,omitempty"`
	// WebhookSecretEnv is the environment variable containing the webhook
	// HMAC secret. The secret value itself must not be stored in city.toml.
	WebhookSecretEnv string `toml:"webhook_secret_env,omitempty"`
	// WebhookSecretKey is an optional stable key for identifying the webhook
	// secret during rotation. When omitted, WebhookSecretEnv is the key.
	WebhookSecretKey string `toml:"webhook_secret_key,omitempty"`
	// PollInterval optionally enables bounded polling/backfill cadence.
	PollInterval string `toml:"poll_interval,omitempty"`
	// MergeQueuePolicy controls merge-queue signal handling. Empty defaults
	// to "observe"; valid values are "ignore", "observe", and "repair".
	MergeQueuePolicy string `toml:"merge_queue,omitempty" jsonschema:"enum=ignore,enum=observe,enum=repair"`
}

// MergeQueuePolicyOrDefault returns the normalized merge-queue policy.
func (m GitHubPRMonitor) MergeQueuePolicyOrDefault() string {
	policy := strings.TrimSpace(strings.ToLower(m.MergeQueuePolicy))
	if policy == "" {
		return defaultGitHubMergeQueuePolicy
	}
	return policy
}

// RepairWorkflowOrDefault returns the configured repair workflow formula name,
// or the default polecat repair workflow when unset.
func (m GitHubPRMonitor) RepairWorkflowOrDefault() string {
	if wf := strings.TrimSpace(m.RepairWorkflow); wf != "" {
		return wf
	}
	return defaultGitHubRepairWorkflow
}

// WebhookSecretKeyOrDefault returns the configured secret key or the env var
// name when no explicit key is configured.
func (m GitHubPRMonitor) WebhookSecretKeyOrDefault() string {
	if key := strings.TrimSpace(m.WebhookSecretKey); key != "" {
		return key
	}
	return strings.TrimSpace(m.WebhookSecretEnv)
}

// ValidateGitHubPRMonitors checks GitHub PR readiness monitor declarations.
func ValidateGitHubPRMonitors(cfg *City) error {
	if cfg == nil || len(cfg.GitHub.PRMonitors) == 0 {
		return nil
	}

	rigs := make(map[string]bool, len(cfg.Rigs))
	for _, rig := range cfg.Rigs {
		rigs[rig.Name] = true
	}

	seenNames := make(map[string]int, len(cfg.GitHub.PRMonitors))
	seenRepoBase := make(map[string]string, len(cfg.GitHub.PRMonitors))
	for i, monitor := range cfg.GitHub.PRMonitors {
		ctx := fmt.Sprintf("github.pr_monitor[%d]", i)
		name := strings.TrimSpace(monitor.Name)
		if name == "" {
			return fmt.Errorf("%s: name is required", ctx)
		}
		if prev, ok := seenNames[name]; ok {
			return fmt.Errorf("%s %q: duplicate name also used by github.pr_monitor[%d]", ctx, name, prev)
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
		rig := strings.TrimSpace(monitor.Rig)
		if rig == "" {
			return fmt.Errorf("%s %q: rig is required", ctx, name)
		}
		if !rigs[rig] {
			return fmt.Errorf("%s %q: rig %q is not declared", ctx, name, rig)
		}
		if strings.TrimSpace(monitor.RepairRoute) == "" {
			return fmt.Errorf("%s %q: repair_route is required", ctx, name)
		}
		if len(monitor.BaseBranches) == 0 {
			return fmt.Errorf("%s %q: base_branches is required", ctx, name)
		}

		branchSeen := make(map[string]bool, len(monitor.BaseBranches))
		for _, base := range monitor.BaseBranches {
			base = strings.TrimSpace(base)
			if base == "" {
				return fmt.Errorf("%s %q: base_branches contains an empty branch", ctx, name)
			}
			baseKey := strings.ToLower(base)
			if branchSeen[baseKey] {
				return fmt.Errorf("%s %q: duplicate base branch %q", ctx, name, base)
			}
			branchSeen[baseKey] = true

			repoBaseKey := strings.ToLower(owner) + "/" + strings.ToLower(repo) + "@" + baseKey
			if prev, ok := seenRepoBase[repoBaseKey]; ok {
				return fmt.Errorf("%s %q: duplicate repo/base %s/%s@%s also monitored by %q",
					ctx, name, strings.ToLower(owner), strings.ToLower(repo), baseKey, prev)
			}
			seenRepoBase[repoBaseKey] = name
		}

		for _, recipient := range monitor.Notify {
			if strings.TrimSpace(recipient) == "" {
				return fmt.Errorf("%s %q: notify contains an empty recipient", ctx, name)
			}
		}
		if envName := strings.TrimSpace(monitor.WebhookSecretEnv); envName != "" && !validGitHubWebhookSecretEnv.MatchString(envName) {
			return fmt.Errorf("%s %q: webhook_secret_env must be an environment variable name, got %q", ctx, name, monitor.WebhookSecretEnv)
		}
		if monitor.WebhookSecretKey != "" && strings.TrimSpace(monitor.WebhookSecretKey) == "" {
			return fmt.Errorf("%s %q: webhook_secret_key must not be blank", ctx, name)
		}
		if err := validateGitHubPRMonitorPollInterval(ctx, name, monitor.PollInterval); err != nil {
			return err
		}
		switch monitor.MergeQueuePolicyOrDefault() {
		case "ignore", "observe", "repair":
		default:
			return fmt.Errorf("%s %q: merge_queue must be \"ignore\", \"observe\", or \"repair\", got %q",
				ctx, name, monitor.MergeQueuePolicy)
		}
	}
	return nil
}

func validateGitHubPRMonitorPollInterval(ctx, name, value string) error {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	dur, err := time.ParseDuration(value)
	if err != nil {
		return fmt.Errorf("%s %q: poll_interval is not a valid duration: %w", ctx, name, err)
	}
	if dur <= 0 {
		return fmt.Errorf("%s %q: poll_interval must be positive: got %q", ctx, name, value)
	}
	return nil
}
