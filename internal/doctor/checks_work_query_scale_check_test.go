package doctor

import (
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
)

func TestScaleCheckWorkQueryCorrespondenceCheckOKCases(t *testing.T) {
	tests := []struct {
		name string
		cfg  *config.City
	}{
		{
			name: "nil config",
			cfg:  nil,
		},
		{
			name: "neither overridden",
			cfg: &config.City{
				Agents: []config.Agent{{Name: "agent-a"}},
			},
		},
		{
			name: "both overridden",
			cfg: &config.City{
				Agents: []config.Agent{{
					Name:       "agent-a",
					WorkQuery:  "bd ready --custom",
					ScaleCheck: "bd ready --custom --count",
				}},
			},
		},
		{
			name: "no agents configured",
			cfg:  &config.City{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := NewScaleCheckWorkQueryCorrespondenceCheck(tt.cfg).Run(&CheckContext{})
			if r.Status != StatusOK {
				t.Fatalf("Status = %v, want StatusOK; details=%v", r.Status, r.Details)
			}
			if r.Severity != SeverityBlocking {
				t.Fatalf("Severity = %v, want default SeverityBlocking for OK result", r.Severity)
			}
		})
	}
}

func TestScaleCheckWorkQueryCorrespondenceCheckWarnsForWorkQueryOnly(t *testing.T) {
	cfg := &config.City{
		Agents: []config.Agent{{Name: "agent-a", WorkQuery: "bd ready --custom"}},
	}

	r := NewScaleCheckWorkQueryCorrespondenceCheck(cfg).Run(&CheckContext{})
	if r.Status != StatusWarning {
		t.Fatalf("Status = %v, want StatusWarning; details=%v", r.Status, r.Details)
	}
	if r.Severity != SeverityAdvisory {
		t.Fatalf("Severity = %v, want SeverityAdvisory (must not block CI or gate dispatch)", r.Severity)
	}
	if len(r.Details) != 1 {
		t.Fatalf("Details = %d, want 1: %v", len(r.Details), r.Details)
	}
	detail := r.Details[0]
	for _, want := range []string{
		`agent "agent-a"`,
		"work_query",
		"scale_check",
		`engdocs/architecture/dispatch.md`,
		"Invariant 11",
		"remove the raw work_query override",
		"route_label",
		"ga-atvk13",
		"rather than adding a matching scale_check",
	} {
		if !strings.Contains(detail, want) {
			t.Fatalf("Details[0] = %q, want to contain %q", detail, want)
		}
	}
}

func TestScaleCheckWorkQueryCorrespondenceCheckWarnsForScaleCheckOnly(t *testing.T) {
	cfg := &config.City{
		Agents: []config.Agent{{Name: "agent-a", ScaleCheck: "bd ready --custom --count"}},
	}

	r := NewScaleCheckWorkQueryCorrespondenceCheck(cfg).Run(&CheckContext{})
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
		"scale_check",
		"work_query",
		`engdocs/architecture/dispatch.md`,
		"Invariant 11",
	} {
		if !strings.Contains(detail, want) {
			t.Fatalf("Details[0] = %q, want to contain %q", detail, want)
		}
	}
	// The route_label remedy is only a safe direction for the work_query-only
	// case (ga-f57vc7): narrowing scale_check to match a raw work_query
	// override silently strands routed-but-unlabelled beads instead of
	// noisily churning them. No equivalent safe remedy is established for
	// the scale_check-only mirror case, so this message must stay generic
	// rather than asymmetrically implying one.
	if strings.Contains(detail, "route_label") {
		t.Fatalf("Details[0] = %q, must not carry the work_query-only remedy wording for the scale_check-only case", detail)
	}
}

func TestScaleCheckWorkQueryCorrespondenceCheckReportsOnlyOneSidedAgents(t *testing.T) {
	cfg := &config.City{
		Agents: []config.Agent{
			{Name: "agent-work-only", WorkQuery: "bd ready --custom"},
			{Name: "agent-scale-only", ScaleCheck: "bd ready --custom --count"},
			{Name: "agent-both", WorkQuery: "bd ready --a", ScaleCheck: "bd ready --a --count"},
			{Name: "agent-neither"},
		},
	}

	r := NewScaleCheckWorkQueryCorrespondenceCheck(cfg).Run(&CheckContext{})
	if r.Status != StatusWarning {
		t.Fatalf("Status = %v, want StatusWarning; details=%v", r.Status, r.Details)
	}
	if r.Severity != SeverityAdvisory {
		t.Fatalf("Severity = %v, want SeverityAdvisory", r.Severity)
	}
	if len(r.Details) != 2 {
		t.Fatalf("Details = %d, want 2 (only the one-sided agents): %v", len(r.Details), r.Details)
	}
	if !strings.Contains(r.Details[0], `agent "agent-work-only"`) {
		t.Fatalf("Details[0] = %q, want agent-work-only", r.Details[0])
	}
	if !strings.Contains(r.Details[1], `agent "agent-scale-only"`) {
		t.Fatalf("Details[1] = %q, want agent-scale-only", r.Details[1])
	}
	if strings.Contains(r.Message, "4") {
		t.Fatalf("Message = %q, must not conflate total agent count with flagged agent count", r.Message)
	}
}

func TestScaleCheckWorkQueryCorrespondenceCheckCanFix(t *testing.T) {
	c := NewScaleCheckWorkQueryCorrespondenceCheck(&config.City{})
	if c.CanFix() {
		t.Fatal("CanFix() = true, want false: the correct scale_check/work_query value is context-dependent, not derivable")
	}
	if err := c.Fix(&CheckContext{}); err != nil {
		t.Fatalf("Fix() = %v, want nil (no-op)", err)
	}
}
