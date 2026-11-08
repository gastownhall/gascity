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
		workQueryOnlyRemedyMarker,
		"review the work_query override",
		"do not narrow scale_check",
	} {
		if !strings.Contains(detail, want) {
			t.Fatalf("Details[0] = %q, want to contain %q", detail, want)
		}
	}
	assertNoUnavailableAdvice(t, detail)
}

// workQueryOnlyRemedyMarker is the phrase that identifies the work_query-only
// remedy: aligning a custom work_query with the default routed-work probe
// using keys that exist today.
const workQueryOnlyRemedyMarker = "default routed-work probe"

// assertNoUnavailableAdvice fails when a warning recommends config that the
// current tree cannot honor. route_label / route_label_any do not exist in
// config.Agent yet, so naming them (or the ga-atvk13 migration that adds
// them) sends the operator to a key the loader rejects. The message must also
// not imply that a matching scale_check holds a cold named seat at zero: the
// reconciler still probes routed work (FR-S0.1).
func assertNoUnavailableAdvice(t *testing.T, detail string) {
	t.Helper()
	for _, banned := range []string{"route_label", "route_label_any", "ga-atvk13", "at zero"} {
		if strings.Contains(detail, banned) {
			t.Fatalf("Details[0] = %q, must not contain %q: that advice is unusable on this tree", detail, banned)
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
	// The review/align remedy is only established for the work_query-only
	// direction (ga-f57vc7): narrowing scale_check to match a raw work_query
	// override silently strands routed-but-unlabelled beads instead of
	// noisily churning them. No equivalent remedy is established for the
	// scale_check-only mirror case, so this message must stay generic rather
	// than asymmetrically implying one.
	for _, banned := range []string{workQueryOnlyRemedyMarker, "review the work_query override"} {
		if strings.Contains(detail, banned) {
			t.Fatalf("Details[0] = %q, must not carry the work_query-only remedy wording %q for the scale_check-only case", detail, banned)
		}
	}
	assertNoUnavailableAdvice(t, detail)
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
