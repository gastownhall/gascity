package worker

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/sessionlog"
)

// TestMessageRecordsNormalizedProviderFamilyLabel pins that the recorded
// gc.agent.tokens.* provider label is always the normalized invocation-usage
// family, never the raw provider string, even when the session carries no
// recognized Profile. The family gate already normalizes the provider through
// invocationUsageFamily; the metric label and pricing key must use that same
// normalized value so a custom/empty-Profile session cannot emit a metric
// series (e.g. "claude-eco") that is inconsistent with the family that gated
// the record and that the family-keyed pricing registry would then miss.
func TestMessageRecordsNormalizedProviderFamilyLabel(t *testing.T) {
	reader := setupInvocationMetricsReader(t)

	searchBase := t.TempDir()
	workDir := t.TempDir()
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	manager := sessionpkg.NewManager(store, sp)

	// No Profile is set, so the label/pricing derivation cannot lean on
	// profileFamily and must normalize the claude-family alias provider itself.
	handle, err := NewSessionHandle(SessionHandleConfig{
		Manager:     manager,
		SearchPaths: []string{searchBase},
		Session: SessionSpec{
			Template: "probe",
			Title:    "Probe",
			Command:  "claude",
			WorkDir:  workDir,
			Provider: "claude-eco",
			Metadata: map[string]string{"agent_name": "myrig/polecat-1"},
		},
	})
	if err != nil {
		t.Fatalf("NewSessionHandle: %v", err)
	}
	if err := handle.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	info, err := manager.Get(handle.sessionID)
	if err != nil {
		t.Fatalf("Get(%q): %v", handle.sessionID, err)
	}
	slugDir := filepath.Join(searchBase, sessionlog.ProjectSlug(workDir))
	if err := os.MkdirAll(slugDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(%q): %v", slugDir, err)
	}
	transcriptPath := filepath.Join(slugDir, info.SessionKey+".jsonl")
	writeWorkerTestJSONL(t, transcriptPath, []map[string]any{
		usageEntry("u1", "claude-opus-4-7", 100, 50, 2000, 800),
	})

	if _, err := handle.Message(context.Background(), MessageRequest{Text: "hello"}); err != nil {
		t.Fatalf("Message: %v", err)
	}

	out := collectInvocationMetrics(t, reader)
	got, attrSets := invocationInt64Total(out, "gc.agent.tokens.input")
	if got != 100 {
		t.Fatalf("gc.agent.tokens.input = %d, want 100 (claude-family alias must still record)", got)
	}
	if len(attrSets) != 1 {
		t.Fatalf("gc.agent.tokens.input: %d datapoints, want 1", len(attrSets))
	}
	if provider := attrSets[0]["provider"]; provider != "claude" {
		t.Errorf("provider label = %q, want claude (normalized family, not the raw provider string)", provider)
	}
}
