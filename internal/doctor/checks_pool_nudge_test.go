package doctor

import (
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
)

func TestPoolAgentNudgeCheckWarnsForPoolAgentWithoutNudge(t *testing.T) {
	cfg := &config.City{
		Agents: []config.Agent{{
			Name:              "worker",
			MinActiveSessions: intPtr(1),
			MaxActiveSessions: intPtr(4),
		}},
	}

	r := NewPoolAgentNudgeCheck(cfg).Run(&CheckContext{})
	if r.Status != StatusWarning {
		t.Fatalf("Status = %v, want StatusWarning; details=%v", r.Status, r.Details)
	}
	if r.Severity != SeverityAdvisory {
		t.Fatalf("Severity = %v, want SeverityAdvisory", r.Severity)
	}
	if len(r.Details) != 1 {
		t.Fatalf("Details = %d, want 1: %v", len(r.Details), r.Details)
	}
	for _, want := range []string{
		"pool agent worker has no claim nudge",
		"drained by the execution backstop after 90s with no probe",
		"set `nudge` on the agent or its patch",
		"#6748",
	} {
		if !strings.Contains(r.Details[0], want) {
			t.Fatalf("Details[0] = %q, want to contain %q", r.Details[0], want)
		}
	}
	if r.FixHint == "" {
		t.Fatalf("FixHint empty, want a hint naming `nudge`")
	}
}

func TestPoolAgentNudgeCheckOKCases(t *testing.T) {
	tests := []struct {
		name string
		cfg  *config.City
	}{
		{
			name: "nil config",
			cfg:  nil,
		},
		{
			name: "pool agent with nudge",
			cfg: &config.City{
				Agents: []config.Agent{{
					Name:              "worker",
					Nudge:             "Run gc hook --claim --json now.",
					MinActiveSessions: intPtr(1),
					MaxActiveSessions: intPtr(4),
				}},
			},
		},
		{
			name: "default-capacity agent with nudge",
			cfg: &config.City{
				Agents: []config.Agent{{Name: "worker", Nudge: "Check your hook for work."}},
			},
		},
		{
			name: "named-session agent without nudge is not a pool",
			cfg: &config.City{
				NamedSessions: []config.NamedSession{{Name: "mayor", Template: "mayor", Mode: "always"}},
				Agents:        []config.Agent{{Name: "mayor", MaxActiveSessions: intPtr(1)}},
			},
		},
		{
			name: "zero-capacity agent without nudge opens no seat",
			cfg: &config.City{
				Agents: []config.Agent{{Name: "template-only", MaxActiveSessions: intPtr(0)}},
			},
		},
		{
			name: "suspended pool agent is ignored",
			cfg: &config.City{
				Agents: []config.Agent{{
					Name:              "worker",
					MinActiveSessions: intPtr(1),
					Suspended:         true,
				}},
			},
		},
		{
			name: "implicit provider agent is ignored",
			cfg: &config.City{
				Agents: []config.Agent{{Name: "claude", Provider: "claude", Implicit: true}},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := NewPoolAgentNudgeCheck(tt.cfg).Run(&CheckContext{})
			if r.Status != StatusOK {
				t.Fatalf("Status = %v, want StatusOK; details=%v", r.Status, r.Details)
			}
			if r.Severity != SeverityBlocking {
				t.Fatalf("Severity = %v, want default SeverityBlocking for OK result", r.Severity)
			}
		})
	}
}

func TestPoolAgentNudgeCheckReportsOnlyPoolAgentsWithoutNudge(t *testing.T) {
	cfg := &config.City{
		NamedSessions: []config.NamedSession{{Name: "mayor", Template: "mayor", Mode: "always"}},
		Agents: []config.Agent{
			{Name: "mayor", MaxActiveSessions: intPtr(1)},
			{Name: "reviewer", Dir: "rig-a", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(2)},
			{Name: "worker", Nudge: "Check your hook for work.", MaxActiveSessions: intPtr(5)},
			{Name: "singleton-pool", MaxActiveSessions: intPtr(1)},
		},
	}

	r := NewPoolAgentNudgeCheck(cfg).Run(&CheckContext{})
	if r.Status != StatusWarning {
		t.Fatalf("Status = %v, want StatusWarning; details=%v", r.Status, r.Details)
	}
	if len(r.Details) != 2 {
		t.Fatalf("Details = %d, want 2: %v", len(r.Details), r.Details)
	}
	if !strings.Contains(r.Details[0], "pool agent rig-a/reviewer has no claim nudge") {
		t.Fatalf("Details[0] = %q, want rig-qualified reviewer", r.Details[0])
	}
	if !strings.Contains(r.Details[1], "pool agent singleton-pool has no claim nudge") {
		t.Fatalf("Details[1] = %q, want singleton-pool", r.Details[1])
	}
	if !strings.Contains(r.Message, "2 pool agent(s)") {
		t.Fatalf("Message = %q, want count of 2", r.Message)
	}
}

func TestPoolAgentNudgeCheckWhitespaceNudgeCountsAsMissing(t *testing.T) {
	cfg := &config.City{
		Agents: []config.Agent{{Name: "worker", Nudge: "  \n", MaxActiveSessions: intPtr(3)}},
	}
	r := NewPoolAgentNudgeCheck(cfg).Run(&CheckContext{})
	if r.Status != StatusWarning {
		t.Fatalf("Status = %v, want StatusWarning for whitespace-only nudge", r.Status)
	}
}

func TestPoolAgentNudgeCheckWarmupEligible(t *testing.T) {
	if NewPoolAgentNudgeCheck(nil).WarmupEligible() {
		t.Fatalf("WarmupEligible must be false")
	}
	if NewPoolAgentNudgeCheck(nil).CanFix() {
		t.Fatalf("CanFix must be false")
	}
}
