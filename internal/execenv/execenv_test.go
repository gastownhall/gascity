package execenv

import (
	"strings"
	"testing"
)

func TestFilterInheritedStripsSensitiveEnv(t *testing.T) {
	got := FilterInherited([]string{
		"PATH=/bin",
		"GITHUB_TOKEN=ghs_secret",
		"OPENAI_API_KEY=sk-secret",
		"GC_INSTANCE_TOKEN=fence",
		"HOME=/tmp/home",
	})
	joined := strings.Join(got, "\n")
	for _, secret := range []string{"GITHUB_TOKEN", "OPENAI_API_KEY", "GC_INSTANCE_TOKEN", "ghs_secret", "sk-secret", "fence"} {
		if strings.Contains(joined, secret) {
			t.Fatalf("FilterInherited leaked %q in %q", secret, joined)
		}
	}
	if !strings.Contains(joined, "PATH=/bin") || !strings.Contains(joined, "HOME=/tmp/home") {
		t.Fatalf("FilterInherited dropped non-sensitive env: %q", joined)
	}
}

func TestMergeMapPreservesExplicitSensitiveOverrides(t *testing.T) {
	got := MergeMap([]string{
		"PATH=/bin",
		"GC_DOLT_PASSWORD=stale",
		"GITHUB_TOKEN=ambient",
	}, map[string]string{
		"GC_DOLT_PASSWORD": "required",
		"BEADS_DIR":        "/city/.beads",
	})
	joined := strings.Join(got, "\n")
	if strings.Contains(joined, "GITHUB_TOKEN") || strings.Contains(joined, "ambient") || strings.Contains(joined, "stale") {
		t.Fatalf("MergeMap leaked inherited secret: %q", joined)
	}
	if !strings.Contains(joined, "GC_DOLT_PASSWORD=required") {
		t.Fatalf("MergeMap did not preserve explicit secret override: %q", joined)
	}
}

func TestRedactTextRedactsEnvValuesAndAssignments(t *testing.T) {
	got := RedactText(
		"token=literal-secret GITHUB_TOKEN=ghs_secret output ghs_secret --password hunter2",
		[]string{"GITHUB_TOKEN=ghs_secret"},
	)
	for _, secret := range []string{"literal-secret", "ghs_secret", "hunter2"} {
		if strings.Contains(got, secret) {
			t.Fatalf("RedactText leaked %q in %q", secret, got)
		}
	}
	if strings.Count(got, Redacted) < 3 {
		t.Fatalf("RedactText redactions = %q, want at least three", got)
	}
}
