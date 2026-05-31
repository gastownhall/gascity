package config

import (
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/fsys"
)

func TestParseGitHubPRMonitors(t *testing.T) {
	cfg, err := Parse([]byte(`
[workspace]
name = "test"

[[github.pr_monitor]]
name = "sample-main"
owner = "sample-org"
repo = "sample-repo"
base_branches = ["main", "release"]
rig = "sample"
notify = ["addr-a", "addr-b"]
repair_route = "route-a"
webhook_secret_env = "SAMPLE_GITHUB_WEBHOOK_SECRET"
webhook_secret_key = "sample-main"
poll_interval = "2m"
merge_queue = "repair"
`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(cfg.GitHub.PRMonitors) != 1 {
		t.Fatalf("len(GitHub.PRMonitors) = %d, want 1", len(cfg.GitHub.PRMonitors))
	}
	got := cfg.GitHub.PRMonitors[0]
	if got.Name != "sample-main" {
		t.Errorf("Name = %q, want sample-main", got.Name)
	}
	if got.Owner != "sample-org" || got.Repo != "sample-repo" {
		t.Errorf("repo = %s/%s, want sample-org/sample-repo", got.Owner, got.Repo)
	}
	if len(got.BaseBranches) != 2 || got.BaseBranches[0] != "main" || got.BaseBranches[1] != "release" {
		t.Errorf("BaseBranches = %v, want [main release]", got.BaseBranches)
	}
	if got.Rig != "sample" {
		t.Errorf("Rig = %q, want sample", got.Rig)
	}
	if len(got.Notify) != 2 || got.Notify[0] != "addr-a" || got.Notify[1] != "addr-b" {
		t.Errorf("Notify = %v, want [addr-a addr-b]", got.Notify)
	}
	if got.RepairRoute != "route-a" {
		t.Errorf("RepairRoute = %q, want route-a", got.RepairRoute)
	}
	if got.WebhookSecretEnv != "SAMPLE_GITHUB_WEBHOOK_SECRET" {
		t.Errorf("WebhookSecretEnv = %q, want SAMPLE_GITHUB_WEBHOOK_SECRET", got.WebhookSecretEnv)
	}
	if got.WebhookSecretKey != "sample-main" {
		t.Errorf("WebhookSecretKey = %q, want sample-main", got.WebhookSecretKey)
	}
	if got.PollInterval != "2m" {
		t.Errorf("PollInterval = %q, want 2m", got.PollInterval)
	}
	if got.MergeQueuePolicy != "repair" {
		t.Errorf("MergeQueuePolicy = %q, want repair", got.MergeQueuePolicy)
	}
}

func TestValidateGitHubPRMonitorsRejectsMissingRepoAndRoute(t *testing.T) {
	for _, tc := range []struct {
		name    string
		monitor GitHubPRMonitor
		want    string
	}{
		{
			name: "missing owner",
			monitor: GitHubPRMonitor{
				Name:         "bad",
				Repo:         "sample-repo",
				BaseBranches: []string{"main"},
				Rig:          "sample",
				RepairRoute:  "route-a",
			},
			want: "owner is required",
		},
		{
			name: "missing repo",
			monitor: GitHubPRMonitor{
				Name:         "bad",
				Owner:        "sample-org",
				BaseBranches: []string{"main"},
				Rig:          "sample",
				RepairRoute:  "route-a",
			},
			want: "repo is required",
		},
		{
			name: "missing route",
			monitor: GitHubPRMonitor{
				Name:         "bad",
				Owner:        "sample-org",
				Repo:         "sample-repo",
				BaseBranches: []string{"main"},
				Rig:          "sample",
			},
			want: "repair_route is required",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &City{
				Rigs: []Rig{{Name: "sample"}},
				GitHub: GitHubConfig{
					PRMonitors: []GitHubPRMonitor{tc.monitor},
				},
			}
			err := ValidateGitHubPRMonitors(cfg)
			if err == nil {
				t.Fatal("ValidateGitHubPRMonitors() = nil, want error")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("ValidateGitHubPRMonitors() = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestValidateGitHubPRMonitorsRejectsDuplicateRepoBase(t *testing.T) {
	cfg := &City{
		Rigs: []Rig{{Name: "sample"}},
		GitHub: GitHubConfig{
			PRMonitors: []GitHubPRMonitor{
				{
					Name:         "main",
					Owner:        "sample-org",
					Repo:         "sample-repo",
					BaseBranches: []string{"main", "release"},
					Rig:          "sample",
					RepairRoute:  "route-a",
				},
				{
					Name:         "main-again",
					Owner:        "SAMPLE-ORG",
					Repo:         "Sample-Repo",
					BaseBranches: []string{"main"},
					Rig:          "sample",
					RepairRoute:  "route-a",
				},
			},
		},
	}
	err := ValidateGitHubPRMonitors(cfg)
	if err == nil {
		t.Fatal("ValidateGitHubPRMonitors() = nil, want duplicate error")
	}
	if !strings.Contains(err.Error(), "duplicate repo/base") || !strings.Contains(err.Error(), "sample-org/sample-repo") {
		t.Fatalf("ValidateGitHubPRMonitors() = %v, want duplicate repo/base for sample-org/sample-repo", err)
	}
}

func TestValidateGitHubPRMonitorsAcceptsEnvBackedWebhookSecret(t *testing.T) {
	cfg := &City{
		Rigs: []Rig{{Name: "sample"}},
		GitHub: GitHubConfig{
			PRMonitors: []GitHubPRMonitor{
				{
					Name:             "main",
					Owner:            "sample-org",
					Repo:             "sample-repo",
					BaseBranches:     []string{"main"},
					Rig:              "sample",
					RepairRoute:      "route-a",
					WebhookSecretEnv: "SAMPLE_GITHUB_WEBHOOK_SECRET",
				},
			},
		},
	}
	if err := ValidateGitHubPRMonitors(cfg); err != nil {
		t.Fatalf("ValidateGitHubPRMonitors: %v", err)
	}

	cfg.GitHub.PRMonitors[0].WebhookSecretEnv = "bad-name"
	err := ValidateGitHubPRMonitors(cfg)
	if err == nil {
		t.Fatal("ValidateGitHubPRMonitors() = nil, want invalid env error")
	}
	if !strings.Contains(err.Error(), "webhook_secret_env") {
		t.Fatalf("ValidateGitHubPRMonitors() = %v, want webhook_secret_env error", err)
	}
}

func TestLoadWithIncludesMergesAndPatchesGitHubPRMonitors(t *testing.T) {
	fs := fsys.NewFake()
	fs.Files["/city/city.toml"] = []byte(`
include = ["github.toml"]

[workspace]
name = "test"

[[rigs]]
name = "sample"
path = "/sample"

[[github.pr_monitor]]
name = "release"
owner = "sample-org"
repo = "sample-repo"
base_branches = ["release"]
rig = "sample"
repair_route = "route-a"
merge_queue = "observe"

[[patches.github_pr_monitor]]
name = "main"
poll_interval = "5m"
notify = ["ops"]
merge_queue = "repair"
`)
	fs.Files["/city/github.toml"] = []byte(`
[[github.pr_monitor]]
name = "main"
owner = "sample-org"
repo = "sample-repo"
base_branches = ["main"]
rig = "sample"
repair_route = "route-a"
poll_interval = "1m"
merge_queue = "observe"
`)

	cfg, _, err := LoadWithIncludes(fs, "/city/city.toml")
	if err != nil {
		t.Fatalf("LoadWithIncludes: %v", err)
	}
	if len(cfg.GitHub.PRMonitors) != 2 {
		t.Fatalf("len(GitHub.PRMonitors) = %d, want 2", len(cfg.GitHub.PRMonitors))
	}
	var mainMonitor *GitHubPRMonitor
	for i := range cfg.GitHub.PRMonitors {
		if cfg.GitHub.PRMonitors[i].Name == "main" {
			mainMonitor = &cfg.GitHub.PRMonitors[i]
			break
		}
	}
	if mainMonitor == nil {
		t.Fatalf("main monitor missing: %#v", cfg.GitHub.PRMonitors)
	}
	if mainMonitor.PollInterval != "5m" {
		t.Errorf("patched PollInterval = %q, want 5m", mainMonitor.PollInterval)
	}
	if mainMonitor.MergeQueuePolicy != "repair" {
		t.Errorf("patched MergeQueuePolicy = %q, want repair", mainMonitor.MergeQueuePolicy)
	}
	if len(mainMonitor.Notify) != 1 || mainMonitor.Notify[0] != "ops" {
		t.Errorf("patched Notify = %v, want [ops]", mainMonitor.Notify)
	}
}
