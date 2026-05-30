package doctor

import (
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
)

func TestNamedAlwaysMinConflictCheckOKCases(t *testing.T) {
	tests := []struct {
		name string
		cfg  *config.City
	}{
		{
			name: "no named sessions",
			cfg: &config.City{
				Agents: []config.Agent{{Name: "agent-a", MinActiveSessions: intPtr(1)}},
			},
		},
		{
			name: "on demand named session with pool minimum",
			cfg: &config.City{
				NamedSessions: []config.NamedSession{{Name: "primary", Template: "agent-a", Mode: "on_demand"}},
				Agents:        []config.Agent{{Name: "agent-a", MinActiveSessions: intPtr(1)}},
			},
		},
		{
			name: "always named session without pool minimum",
			cfg: &config.City{
				NamedSessions: []config.NamedSession{{Name: "primary", Template: "agent-a", Mode: "always"}},
				Agents:        []config.Agent{{Name: "agent-a", MinActiveSessions: intPtr(0)}},
			},
		},
		{
			name: "suspended agent is ignored",
			cfg: &config.City{
				NamedSessions: []config.NamedSession{{Name: "primary", Template: "agent-a", Mode: "always"}},
				Agents: []config.Agent{{
					Name:              "agent-a",
					MinActiveSessions: intPtr(1),
					Suspended:         true,
				}},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := NewNamedAlwaysMinConflictCheck(tt.cfg).Run(&CheckContext{})
			if r.Status != StatusOK {
				t.Fatalf("Status = %v, want StatusOK; details=%v", r.Status, r.Details)
			}
			if r.Severity != SeverityBlocking {
				t.Fatalf("Severity = %v, want default SeverityBlocking for OK result", r.Severity)
			}
		})
	}
}

func TestNamedAlwaysMinConflictCheckWarnsWithAdvisorySeverity(t *testing.T) {
	cfg := &config.City{
		NamedSessions: []config.NamedSession{{Name: "primary", Template: "agent-a", Mode: "always"}},
		Agents:        []config.Agent{{Name: "agent-a", MinActiveSessions: intPtr(1)}},
	}

	r := NewNamedAlwaysMinConflictCheck(cfg).Run(&CheckContext{})
	if r.Status != StatusWarning {
		t.Fatalf("Status = %v, want StatusWarning; details=%v", r.Status, r.Details)
	}
	if r.Severity != SeverityAdvisory {
		t.Fatalf("Severity = %v, want SeverityAdvisory", r.Severity)
	}
	if len(r.Details) != 1 {
		t.Fatalf("Details = %d, want 1: %v", len(r.Details), r.Details)
	}
	detail := r.Details[0]
	for _, want := range []string{
		`agent "agent-a"`,
		"min_active_sessions=1",
		`named_session "primary"`,
		"set min_active_sessions=0 to avoid a duplicate pool session",
	} {
		if !strings.Contains(detail, want) {
			t.Fatalf("Details[0] = %q, want to contain %q", detail, want)
		}
	}
}

func TestNamedAlwaysMinConflictCheckWarnsForMinTwo(t *testing.T) {
	cfg := &config.City{
		NamedSessions: []config.NamedSession{{Name: "primary", Template: "agent-a", Mode: "always"}},
		Agents:        []config.Agent{{Name: "agent-a", MinActiveSessions: intPtr(2)}},
	}

	r := NewNamedAlwaysMinConflictCheck(cfg).Run(&CheckContext{})
	if r.Status != StatusWarning {
		t.Fatalf("Status = %v, want StatusWarning; details=%v", r.Status, r.Details)
	}
	if len(r.Details) != 1 || !strings.Contains(r.Details[0], "min_active_sessions=2") {
		t.Fatalf("Details = %v, want min_active_sessions=2 warning", r.Details)
	}
}

func TestNamedAlwaysMinConflictCheckReportsTwoAgents(t *testing.T) {
	cfg := &config.City{
		NamedSessions: []config.NamedSession{
			{Name: "primary-a", Template: "agent-a", Mode: "always"},
			{Name: "primary-b", Template: "agent-b", Mode: "always"},
		},
		Agents: []config.Agent{
			{Name: "agent-a", MinActiveSessions: intPtr(1)},
			{Name: "agent-b", MinActiveSessions: intPtr(2)},
		},
	}

	r := NewNamedAlwaysMinConflictCheck(cfg).Run(&CheckContext{})
	if r.Status != StatusWarning {
		t.Fatalf("Status = %v, want StatusWarning; details=%v", r.Status, r.Details)
	}
	if r.Severity != SeverityAdvisory {
		t.Fatalf("Severity = %v, want SeverityAdvisory", r.Severity)
	}
	if len(r.Details) != 2 {
		t.Fatalf("Details = %d, want 2: %v", len(r.Details), r.Details)
	}
	for _, want := range []string{`agent "agent-a"`, `named_session "primary-a"`} {
		if !strings.Contains(r.Details[0], want) {
			t.Fatalf("Details[0] = %q, want to contain %q", r.Details[0], want)
		}
	}
	for _, want := range []string{`agent "agent-b"`, `named_session "primary-b"`} {
		if !strings.Contains(r.Details[1], want) {
			t.Fatalf("Details[1] = %q, want to contain %q", r.Details[1], want)
		}
	}
}

func intPtr(v int) *int {
	return &v
}
