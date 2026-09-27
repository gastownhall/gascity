// Package factory loads and validates the project-level software-factory
// policy consumed by factory formulas and host-side automation.
//
// The file is intentionally policy, not an integration registry: providers and
// tools named in it must already be available to the host. Missing or unclear
// policy fails closed so a factory run cannot infer permission to publish,
// approve, merge, deploy, or recover work.
package factory

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// ConfigRelativePath and ConfigVersion identify the project policy file and schema.
const (
	ConfigRelativePath = ".agent-factory/config.yaml"
	ConfigVersion      = 1
)

// ErrNotConfigured indicates that a project has no factory policy file.
var ErrNotConfigured = errors.New("software factory is not configured")

// Config is the validated project-level software-factory policy.
type Config struct {
	Version      int               `yaml:"version" json:"version"`
	Timezone     string            `yaml:"timezone,omitempty" json:"timezone,omitempty"`
	Repositories []Repository      `yaml:"repositories" json:"repositories"`
	Sources      []Source          `yaml:"sources,omitempty" json:"sources,omitempty"`
	Workflows    Workflows         `yaml:"workflows,omitempty" json:"workflows,omitempty"`
	SkillPrompts map[string]string `yaml:"skill_prompts,omitempty" json:"skill_prompts,omitempty"`
}

// Repository names a source repository and its worktree policy.
type Repository struct {
	ID       string         `yaml:"id" json:"id"`
	Provider string         `yaml:"provider" json:"provider"`
	Remote   string         `yaml:"remote" json:"remote"`
	Worktree WorktreePolicy `yaml:"worktree" json:"worktree"`
}

// WorktreePolicy controls how candidate worktrees are selected.
type WorktreePolicy struct {
	Mode string `yaml:"mode" json:"mode"`
	Base string `yaml:"base" json:"base"`
}

// Source describes an input that a factory workflow may collect.
type Source struct {
	ID          string         `yaml:"id" json:"id"`
	Provider    string         `yaml:"provider" json:"provider"`
	Type        string         `yaml:"type" json:"type"`
	Scope       string         `yaml:"scope" json:"scope"`
	Integration string         `yaml:"integration,omitempty" json:"integration,omitempty"`
	ReadTool    string         `yaml:"read_tool,omitempty" json:"read_tool,omitempty"`
	Arguments   map[string]any `yaml:"arguments,omitempty" json:"arguments,omitempty"`
	Pagination  string         `yaml:"pagination,omitempty" json:"pagination,omitempty"`
}

// Workflows groups the configured factory phases and delivery policies.
type Workflows struct {
	Collect       CollectWorkflow       `yaml:"collect,omitempty" json:"collect,omitempty"`
	Lookback      LookbackWorkflow      `yaml:"lookback,omitempty" json:"lookback,omitempty"`
	HumanDigest   HumanDigestWorkflow   `yaml:"human-digest,omitempty" json:"human-digest,omitempty"`
	PullRequests  PullRequestsWorkflow  `yaml:"pull-requests,omitempty" json:"pull-requests,omitempty"`
	PRBabysitting PRBabysittingWorkflow `yaml:"pr-babysitting,omitempty" json:"pr-babysitting,omitempty"`
	Ship          ShipWorkflow          `yaml:"ship,omitempty" json:"ship,omitempty"`
	ShipWatchdog  WatchdogWorkflow      `yaml:"ship-watchdog,omitempty" json:"ship-watchdog,omitempty"`
	Recovery      RecoveryWorkflow      `yaml:"recovery,omitempty" json:"recovery,omitempty"`
}

// CollectWorkflow bounds intake and its optional actions.
type CollectWorkflow struct {
	Enabled   bool         `yaml:"enabled" json:"enabled"`
	Schedule  string       `yaml:"schedule,omitempty" json:"schedule,omitempty"`
	Sources   []string     `yaml:"sources,omitempty" json:"sources,omitempty"`
	Window    string       `yaml:"window,omitempty" json:"window,omitempty"`
	Implement ActionPolicy `yaml:"implement,omitempty" json:"implement,omitempty"`
	Reply     ActionPolicy `yaml:"reply,omitempty" json:"reply,omitempty"`
	Close     ActionPolicy `yaml:"close,omitempty" json:"close,omitempty"`
}

// LookbackWorkflow defines the bounded comparison window for past signals.
type LookbackWorkflow struct {
	Enabled     bool         `yaml:"enabled" json:"enabled"`
	Schedule    string       `yaml:"schedule,omitempty" json:"schedule,omitempty"`
	Window      string       `yaml:"window,omitempty" json:"window,omitempty"`
	CompareWith string       `yaml:"compare_with,omitempty" json:"compare_with,omitempty"`
	Sources     []string     `yaml:"sources,omitempty" json:"sources,omitempty"`
	Implement   ActionPolicy `yaml:"implement,omitempty" json:"implement,omitempty"`
}

// HumanDigestWorkflow selects evidence for a human decision digest.
type HumanDigestWorkflow struct {
	Enabled      bool     `yaml:"enabled" json:"enabled"`
	Schedule     string   `yaml:"schedule,omitempty" json:"schedule,omitempty"`
	Window       string   `yaml:"window,omitempty" json:"window,omitempty"`
	Repositories []string `yaml:"repositories,omitempty" json:"repositories,omitempty"`
	Sources      []string `yaml:"sources,omitempty" json:"sources,omitempty"`
	Include      []string `yaml:"include,omitempty" json:"include,omitempty"`
	Granularity  string   `yaml:"granularity,omitempty" json:"granularity,omitempty"`
}

// PullRequestsWorkflow configures PR intake and approval policies.
type PullRequestsWorkflow struct {
	Enabled  bool              `yaml:"enabled" json:"enabled"`
	Schedule string            `yaml:"schedule,omitempty" json:"schedule,omitempty"`
	Filters  map[string]string `yaml:"filters,omitempty" json:"filters,omitempty"`
	Approve  ActionPolicy      `yaml:"approve,omitempty" json:"approve,omitempty"`
	Merge    ActionPolicy      `yaml:"merge,omitempty" json:"merge,omitempty"`
}

// PRBabysittingWorkflow configures a bounded PR follow-up run.
type PRBabysittingWorkflow struct {
	Enabled       bool         `yaml:"enabled" json:"enabled"`
	Schedule      string       `yaml:"schedule,omitempty" json:"schedule,omitempty"`
	Target        PRTarget     `yaml:"target,omitempty" json:"target,omitempty"`
	Authorization string       `yaml:"authorization,omitempty" json:"authorization,omitempty"`
	Fix           ActionPolicy `yaml:"fix,omitempty" json:"fix,omitempty"`
	Publish       ActionPolicy `yaml:"publish,omitempty" json:"publish,omitempty"`
	Reply         ActionPolicy `yaml:"reply,omitempty" json:"reply,omitempty"`
	Approve       ActionPolicy `yaml:"approve,omitempty" json:"approve,omitempty"`
	Merge         ActionPolicy `yaml:"merge,omitempty" json:"merge,omitempty"`
	Soak          string       `yaml:"soak,omitempty" json:"soak,omitempty"`
}

// PRTarget identifies the PR that a babysitting run may inspect.
type PRTarget struct {
	Host       string `yaml:"host,omitempty" json:"host,omitempty"`
	Repository string `yaml:"repository,omitempty" json:"repository,omitempty"`
	ID         string `yaml:"id,omitempty" json:"id,omitempty"`
}

// ShipWorkflow controls explicit publication, merge, deploy, and close actions.
type ShipWorkflow struct {
	Enabled  bool         `yaml:"enabled" json:"enabled"`
	Schedule string       `yaml:"schedule,omitempty" json:"schedule,omitempty"`
	Publish  ActionPolicy `yaml:"publish,omitempty" json:"publish,omitempty"`
	Merge    ActionPolicy `yaml:"merge,omitempty" json:"merge,omitempty"`
	Deploy   ActionPolicy `yaml:"deploy,omitempty" json:"deploy,omitempty"`
	Close    ActionPolicy `yaml:"close,omitempty" json:"close,omitempty"`
}

// WatchdogWorkflow configures post-delivery checks and notifications.
type WatchdogWorkflow struct {
	Enabled  bool         `yaml:"enabled" json:"enabled"`
	Schedule string       `yaml:"schedule,omitempty" json:"schedule,omitempty"`
	Notify   ActionPolicy `yaml:"notify,omitempty" json:"notify,omitempty"`
}

// RecoveryWorkflow configures scheduled recovery checks.
type RecoveryWorkflow struct {
	Enabled  bool   `yaml:"enabled" json:"enabled"`
	Schedule string `yaml:"schedule,omitempty" json:"schedule,omitempty"`
}

// ActionPolicy lists the conditions for one named action.
type ActionPolicy struct {
	Mode     string   `yaml:"mode,omitempty" json:"mode,omitempty"`
	Allow    []string `yaml:"allow,omitempty" json:"allow,omitempty"`
	Require  []string `yaml:"require,omitempty" json:"require,omitempty"`
	Stop     []string `yaml:"stop,omitempty" json:"stop,omitempty"`
	Tone     string   `yaml:"tone,omitempty" json:"tone,omitempty"`
	Guidance string   `yaml:"guidance,omitempty" json:"guidance,omitempty"`
}

// LoadProject loads the factory policy from a project root.
func LoadProject(projectRoot string) (Config, error) {
	return LoadFile(filepath.Join(projectRoot, ConfigRelativePath))
}

// LoadFile parses and validates a factory policy file.
func LoadFile(path string) (Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Config{}, fmt.Errorf("%w: %s", ErrNotConfigured, path)
		}
		return Config{}, fmt.Errorf("read %s: %w", path, err)
	}
	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return Config{}, fmt.Errorf("parse %s: %w", path, err)
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, fmt.Errorf("validate %s: %w", path, err)
	}
	return cfg, nil
}

// Validate checks the required fields and supported policy values.
func (c Config) Validate() error {
	var problems []string
	if c.Version != ConfigVersion {
		problems = append(problems, fmt.Sprintf("version must be %d", ConfigVersion))
	}
	if len(c.Repositories) == 0 {
		problems = append(problems, "repositories must contain at least one entry")
	}
	repoIDs := make(map[string]struct{}, len(c.Repositories))
	for i, repo := range c.Repositories {
		prefix := fmt.Sprintf("repositories[%d]", i)
		if strings.TrimSpace(repo.ID) == "" {
			problems = append(problems, prefix+".id is required")
		} else if _, ok := repoIDs[repo.ID]; ok {
			problems = append(problems, "duplicate repository id "+repo.ID)
		} else {
			repoIDs[repo.ID] = struct{}{}
		}
		if strings.TrimSpace(repo.Provider) == "" {
			problems = append(problems, prefix+".provider is required")
		}
		if strings.TrimSpace(repo.Remote) == "" {
			problems = append(problems, prefix+".remote is required")
		}
		if strings.TrimSpace(repo.Worktree.Mode) == "" {
			problems = append(problems, prefix+".worktree.mode is required")
		} else if repo.Worktree.Mode != "fresh-per-run" && repo.Worktree.Mode != "existing" {
			problems = append(problems, prefix+".worktree.mode must be fresh-per-run or existing")
		}
		if repo.Worktree.Mode == "fresh-per-run" && strings.TrimSpace(repo.Worktree.Base) == "" {
			problems = append(problems, prefix+".worktree.base is required for fresh-per-run")
		}
	}
	sourceIDs := make(map[string]struct{}, len(c.Sources))
	for i, source := range c.Sources {
		prefix := fmt.Sprintf("sources[%d]", i)
		if strings.TrimSpace(source.ID) == "" {
			problems = append(problems, prefix+".id is required")
		} else if _, ok := sourceIDs[source.ID]; ok {
			problems = append(problems, "duplicate source id "+source.ID)
		} else {
			sourceIDs[source.ID] = struct{}{}
		}
		if strings.TrimSpace(source.Provider) == "" {
			problems = append(problems, prefix+".provider is required")
		}
		if strings.TrimSpace(source.Scope) == "" {
			problems = append(problems, prefix+".scope is required")
		}
	}
	if c.Workflows.Collect.Enabled && len(c.Workflows.Collect.Sources) == 0 {
		problems = append(problems, "workflows.collect.sources is required when collect is enabled")
	}
	if c.Workflows.Lookback.Enabled && len(c.Workflows.Lookback.Sources) == 0 {
		problems = append(problems, "workflows.lookback.sources is required when lookback is enabled")
	}
	for name, ids := range map[string][]string{
		"workflows.collect.sources":      c.Workflows.Collect.Sources,
		"workflows.lookback.sources":     c.Workflows.Lookback.Sources,
		"workflows.human-digest.sources": c.Workflows.HumanDigest.Sources,
	} {
		for _, id := range ids {
			if _, ok := sourceIDs[id]; !ok {
				problems = append(problems, name+" references unknown source "+id)
			}
		}
	}
	for name, policy := range map[string]ActionPolicy{
		"collect.implement":      c.Workflows.Collect.Implement,
		"collect.reply":          c.Workflows.Collect.Reply,
		"collect.close":          c.Workflows.Collect.Close,
		"lookback.implement":     c.Workflows.Lookback.Implement,
		"pull-requests.approve":  c.Workflows.PullRequests.Approve,
		"pull-requests.merge":    c.Workflows.PullRequests.Merge,
		"pr-babysitting.fix":     c.Workflows.PRBabysitting.Fix,
		"pr-babysitting.publish": c.Workflows.PRBabysitting.Publish,
		"pr-babysitting.reply":   c.Workflows.PRBabysitting.Reply,
		"pr-babysitting.approve": c.Workflows.PRBabysitting.Approve,
		"pr-babysitting.merge":   c.Workflows.PRBabysitting.Merge,
		"ship.publish":           c.Workflows.Ship.Publish,
		"ship.merge":             c.Workflows.Ship.Merge,
		"ship.deploy":            c.Workflows.Ship.Deploy,
		"ship.close":             c.Workflows.Ship.Close,
		"ship-watchdog.notify":   c.Workflows.ShipWatchdog.Notify,
	} {
		if mode := strings.TrimSpace(policy.Mode); mode != "" && !validMode(mode) {
			problems = append(problems, "workflows."+name+" has unsupported mode "+mode)
		}
	}
	if len(problems) > 0 {
		sort.Strings(problems)
		return errors.New(strings.Join(problems, "; "))
	}
	return nil
}

func validMode(mode string) bool {
	switch mode {
	case "never", "manual", "criteria", "after-fix", "after-merge":
		return true
	default:
		return false
	}
}
