package config

import (
	"strings"
	"testing"
)

func TestParseGitHubRunMonitorsKeepsWorkflowConfigurationIsolated(t *testing.T) {
	cfg, err := Parse([]byte(`
[workspace]
name = "test"

[[rigs]]
name = "gascity"
path = "/gascity"

[[github.run_monitor]]
name = "mac-regression-red-streak"
owner = "gastownhall"
repo = "gascity"
workflow_file = "mac-regression.yml"
aggregate_job = "Mac regression summary"
activated_after = "2026-07-05T00:00:00Z"
threshold = 4
rig = "gascity"
route = "gascity/mac-triage"
priority = "P2"
notify = ["ops"]

[[github.run_monitor]]
name = "nightly-red-streak"
owner = "gastownhall"
repo = "gascity"
workflow_file = "nightly.yml"
event = "schedule"
aggregate_job = "Nightly summary"
activated_after = "2026-07-30T00:00:00Z"
rig = "gascity"
route = "gascity/nightly-triage"
`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(cfg.GitHub.RunMonitors) != 2 {
		t.Fatalf("len(GitHub.RunMonitors) = %d, want 2", len(cfg.GitHub.RunMonitors))
	}

	mac := cfg.GitHub.RunMonitors[0]
	if mac.Name != "mac-regression-red-streak" ||
		mac.WorkflowFile != "mac-regression.yml" ||
		mac.AggregateJob != "Mac regression summary" ||
		mac.Route != "gascity/mac-triage" {
		t.Fatalf("Mac Regression monitor = %#v, fields leaked or were not parsed", mac)
	}
	if mac.ThresholdOrDefault() != 4 || mac.PriorityOrDefault() != "P2" {
		t.Fatalf("Mac Regression defaults = threshold %d priority %q, want 4/P2",
			mac.ThresholdOrDefault(), mac.PriorityOrDefault())
	}
	if len(mac.Notify) != 1 || mac.Notify[0] != "ops" {
		t.Fatalf("Mac Regression notify = %v, want [ops]", mac.Notify)
	}

	nightly := cfg.GitHub.RunMonitors[1]
	if nightly.Name != "nightly-red-streak" ||
		nightly.WorkflowFile != "nightly.yml" ||
		nightly.AggregateJob != "Nightly summary" ||
		nightly.Route != "gascity/nightly-triage" {
		t.Fatalf("Nightly monitor = %#v, fields leaked or were not parsed", nightly)
	}
	if nightly.EventOrDefault() != "schedule" {
		t.Fatalf("Nightly EventOrDefault() = %q, want schedule", nightly.EventOrDefault())
	}
	if nightly.ThresholdOrDefault() != 3 {
		t.Fatalf("Nightly ThresholdOrDefault() = %d, want 3", nightly.ThresholdOrDefault())
	}
	if nightly.PriorityOrDefault() != "P1" {
		t.Fatalf("Nightly PriorityOrDefault() = %q, want P1", nightly.PriorityOrDefault())
	}
	if len(nightly.Notify) != 0 {
		t.Fatalf("Nightly notify = %v, want empty; Mac recipients must not leak", nightly.Notify)
	}
}

func TestValidateGitHubRunMonitorsRejectsInvalidEnrollment(t *testing.T) {
	valid := GitHubRunMonitor{
		Name:           "nightly-red-streak",
		Owner:          "gastownhall",
		Repo:           "gascity",
		WorkflowFile:   "nightly.yml",
		AggregateJob:   "Nightly summary",
		ActivatedAfter: "2026-07-30T00:00:00Z",
		Rig:            "gascity",
		Route:          "gascity/nightly-triage",
	}
	for _, tc := range []struct {
		name   string
		mutate func(*GitHubRunMonitor)
		want   string
	}{
		{name: "name", mutate: func(m *GitHubRunMonitor) { m.Name = "" }, want: "name is required"},
		{name: "owner", mutate: func(m *GitHubRunMonitor) { m.Owner = "" }, want: "owner is required"},
		{name: "repo", mutate: func(m *GitHubRunMonitor) { m.Repo = "" }, want: "repo is required"},
		{name: "workflow file", mutate: func(m *GitHubRunMonitor) { m.WorkflowFile = "" }, want: "workflow_file is required"},
		{name: "aggregate job", mutate: func(m *GitHubRunMonitor) { m.AggregateJob = "" }, want: "aggregate_job is required"},
		{name: "activation boundary", mutate: func(m *GitHubRunMonitor) { m.ActivatedAfter = "" }, want: "activated_after is required"},
		{name: "malformed activation boundary", mutate: func(m *GitHubRunMonitor) { m.ActivatedAfter = "July 30" }, want: "RFC3339"},
		{name: "threshold", mutate: func(m *GitHubRunMonitor) { m.Threshold = -1 }, want: "threshold must be at least 1"},
		{name: "rig", mutate: func(m *GitHubRunMonitor) { m.Rig = "" }, want: "rig is required"},
		{name: "unknown rig", mutate: func(m *GitHubRunMonitor) { m.Rig = "missing" }, want: "is not declared"},
		{name: "route", mutate: func(m *GitHubRunMonitor) { m.Route = "" }, want: "route is required"},
		{name: "blank notify", mutate: func(m *GitHubRunMonitor) { m.Notify = []string{" "} }, want: "notify contains an empty recipient"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			monitor := valid
			tc.mutate(&monitor)
			cfg := &City{
				Rigs:   []Rig{{Name: "gascity"}},
				GitHub: GitHubConfig{RunMonitors: []GitHubRunMonitor{monitor}},
			}
			err := ValidateGitHubRunMonitors(cfg)
			if err == nil {
				t.Fatal("ValidateGitHubRunMonitors() = nil, want error")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("ValidateGitHubRunMonitors() = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestValidateGitHubRunMonitorsRejectsDuplicateIdentity(t *testing.T) {
	base := GitHubRunMonitor{
		Name:           "mac-regression-red-streak",
		Owner:          "gastownhall",
		Repo:           "gascity",
		WorkflowFile:   "mac-regression.yml",
		AggregateJob:   "Mac regression summary",
		ActivatedAfter: "2026-07-05T00:00:00Z",
		Rig:            "gascity",
		Route:          "gascity/mac-triage",
	}
	for _, tc := range []struct {
		name   string
		second GitHubRunMonitor
		want   string
	}{
		{
			name: "monitor name",
			second: func() GitHubRunMonitor {
				m := base
				m.WorkflowFile = "nightly.yml"
				m.AggregateJob = "Nightly summary"
				return m
			}(),
			want: "duplicate name",
		},
		{
			name: "repository workflow",
			second: func() GitHubRunMonitor {
				m := base
				m.Name = "same-workflow-again"
				m.Owner = "GASTOWNHALL"
				m.Repo = "GasCity"
				m.WorkflowFile = "MAC-REGRESSION.YML"
				return m
			}(),
			want: "duplicate repo/workflow",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &City{
				Rigs: []Rig{{Name: "gascity"}},
				GitHub: GitHubConfig{
					RunMonitors: []GitHubRunMonitor{base, tc.second},
				},
			}
			err := ValidateGitHubRunMonitors(cfg)
			if err == nil {
				t.Fatal("ValidateGitHubRunMonitors() = nil, want duplicate error")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("ValidateGitHubRunMonitors() = %v, want %q", err, tc.want)
			}
		})
	}
}
