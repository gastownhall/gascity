package doctor

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
)

// --- DurationRangeCheck ---

func TestDurationRangeCheck_AllReasonable(t *testing.T) {
	cfg := &config.City{
		Session: config.SessionConfig{
			SetupTimeout:       "10s",
			NudgeReadyTimeout:  "10s",
			NudgeRetryInterval: "500ms",
			NudgeLockTimeout:   "30s",
			StartupTimeout:     "60s",
		},
		Daemon: config.DaemonConfig{
			PatrolInterval:    "30s",
			RestartWindow:     "1h",
			ShutdownTimeout:   "5s",
			DriftDrainTimeout: "2m",
		},
	}
	c := NewDurationRangeCheck(cfg)
	r := c.Run(&CheckContext{})
	if r.Status != StatusOK {
		t.Errorf("status = %d, want OK; msg = %s; details = %v", r.Status, r.Message, r.Details)
	}
}

func TestDurationRangeCheck_TooSmall(t *testing.T) {
	cfg := &config.City{
		Session: config.SessionConfig{
			StartupTimeout: "1ns",
		},
	}
	c := NewDurationRangeCheck(cfg)
	r := c.Run(&CheckContext{})
	if r.Status != StatusWarning {
		t.Errorf("status = %d, want Warning", r.Status)
	}
	if len(r.Details) == 0 {
		t.Error("expected details about too-small duration")
	}
	found := false
	for _, d := range r.Details {
		if strings.Contains(d, "startup_timeout") && strings.Contains(d, "below minimum") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected startup_timeout below-minimum warning in details: %v", r.Details)
	}
}

func TestDurationRangeCheck_TooLarge(t *testing.T) {
	cfg := &config.City{
		Daemon: config.DaemonConfig{
			PatrolInterval: "720h", // 30 days — exceeds 24h max
		},
	}
	c := NewDurationRangeCheck(cfg)
	r := c.Run(&CheckContext{})
	if r.Status != StatusWarning {
		t.Errorf("status = %d, want Warning", r.Status)
	}
	found := false
	for _, d := range r.Details {
		if strings.Contains(d, "patrol_interval") && strings.Contains(d, "exceeds maximum") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected patrol_interval exceeds-maximum warning in details: %v", r.Details)
	}
}

func TestDurationRangeCheck_EmptySkipped(t *testing.T) {
	cfg := &config.City{} // All empty — nothing to check.
	c := NewDurationRangeCheck(cfg)
	r := c.Run(&CheckContext{})
	if r.Status != StatusOK {
		t.Errorf("status = %d, want OK; msg = %s", r.Status, r.Message)
	}
}

func TestDurationRangeCheck_UnparseableSkipped(t *testing.T) {
	// Unparseable durations are handled by ValidateDurations; this check
	// should skip them rather than erroring.
	cfg := &config.City{
		Session: config.SessionConfig{
			StartupTimeout: "5mins", // invalid
		},
	}
	c := NewDurationRangeCheck(cfg)
	r := c.Run(&CheckContext{})
	if r.Status != StatusOK {
		t.Errorf("status = %d, want OK (unparseable skipped); msg = %s", r.Status, r.Message)
	}
}

func TestDurationRangeCheck_AgentIdleTimeout(t *testing.T) {
	cfg := &config.City{
		Agents: []config.Agent{
			{Name: "worker", IdleTimeout: "1ms"},
		},
	}
	c := NewDurationRangeCheck(cfg)
	r := c.Run(&CheckContext{})
	if r.Status != StatusWarning {
		t.Errorf("status = %d, want Warning; msg = %s", r.Status, r.Message)
	}
	found := false
	for _, d := range r.Details {
		if strings.Contains(d, "idle_timeout") && strings.Contains(d, "below minimum") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected idle_timeout warning in details: %v", r.Details)
	}
}

func TestDurationRangeCheck_AgentPoolDrainTimeout(t *testing.T) {
	cfg := &config.City{
		Agents: []config.Agent{
			{
				Name:         "pool-agent",
				DrainTimeout: "10ns",
			},
		},
	}
	c := NewDurationRangeCheck(cfg)
	r := c.Run(&CheckContext{})
	if r.Status != StatusWarning {
		t.Errorf("status = %d, want Warning; msg = %s", r.Status, r.Message)
	}
}

func TestDurationRangeCheck_MultipleIssues(t *testing.T) {
	cfg := &config.City{
		Session: config.SessionConfig{
			StartupTimeout: "1ns",   // too small
			SetupTimeout:   "9999h", // too large
		},
	}
	c := NewDurationRangeCheck(cfg)
	r := c.Run(&CheckContext{})
	if r.Status != StatusWarning {
		t.Errorf("status = %d, want Warning", r.Status)
	}
	if len(r.Details) < 2 {
		t.Errorf("expected at least 2 issues, got %d: %v", len(r.Details), r.Details)
	}
}

// --- EventLogSizeCheck ---

func TestEventLogSizeCheck_SmallFile(t *testing.T) {
	dir := t.TempDir()
	gcDir := filepath.Join(dir, ".gc")
	if err := os.MkdirAll(gcDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(gcDir, "events.jsonl"), []byte("small\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	c := NewEventLogSizeCheck()
	r := c.Run(&CheckContext{CityPath: dir})
	if r.Status != StatusOK {
		t.Errorf("status = %d, want OK; msg = %s", r.Status, r.Message)
	}
}

func TestEventLogSizeCheck_LargeFile(t *testing.T) {
	dir := t.TempDir()
	gcDir := filepath.Join(dir, ".gc")
	if err := os.MkdirAll(gcDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// Use a small threshold for testing.
	c := &EventLogSizeCheck{MaxSize: 100}
	path := filepath.Join(gcDir, "events.jsonl")
	data := make([]byte, 200) // exceeds 100-byte threshold
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}

	r := c.Run(&CheckContext{CityPath: dir})
	if r.Status != StatusWarning {
		t.Errorf("status = %d, want Warning; msg = %s", r.Status, r.Message)
	}
	if r.FixHint == "" {
		t.Error("expected fix hint for large event log")
	}
}

func TestEventLogSizeCheck_MissingFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}

	c := NewEventLogSizeCheck()
	r := c.Run(&CheckContext{CityPath: dir})
	if r.Status != StatusOK {
		t.Errorf("status = %d, want OK (missing file = nothing to check); msg = %s", r.Status, r.Message)
	}
}

func TestEventLogSizeCheck_ExactlyAtThreshold(t *testing.T) {
	dir := t.TempDir()
	gcDir := filepath.Join(dir, ".gc")
	if err := os.MkdirAll(gcDir, 0o755); err != nil {
		t.Fatal(err)
	}

	c := &EventLogSizeCheck{MaxSize: 100}
	data := make([]byte, 100) // exactly at threshold
	if err := os.WriteFile(filepath.Join(gcDir, "events.jsonl"), data, 0o644); err != nil {
		t.Fatal(err)
	}

	r := c.Run(&CheckContext{CityPath: dir})
	if r.Status != StatusOK {
		t.Errorf("status = %d, want OK (at threshold, not over); msg = %s", r.Status, r.Message)
	}
}

// --- ConfigSemanticsCheck ---

func TestConfigSemanticsCheck_Clean(t *testing.T) {
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test"},
		Agents:    []config.Agent{{Name: "worker"}},
	}
	c := NewConfigSemanticsCheck(cfg, "city.toml")
	r := c.Run(&CheckContext{})
	if r.Status != StatusOK {
		t.Errorf("status = %d, want OK; msg = %s; details = %v", r.Status, r.Message, r.Details)
	}
}

func TestConfigSemanticsCheck_BadProviderRef(t *testing.T) {
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test"},
		Agents: []config.Agent{
			{Name: "worker", Provider: "nonexistent"},
		},
	}
	c := NewConfigSemanticsCheck(cfg, "city.toml")
	r := c.Run(&CheckContext{})
	if r.Status != StatusWarning {
		t.Errorf("status = %d, want Warning; msg = %s", r.Status, r.Message)
	}
	if len(r.Details) == 0 {
		t.Error("expected warning details about bad provider")
	}
}

func TestConfigSemanticsCheck_MultipleWarnings(t *testing.T) {
	cfg := &config.City{
		Workspace: config.Workspace{
			Name:     "test",
			Provider: "bogus",
		},
		Agents: []config.Agent{
			{Name: "a1", Provider: "missing1"},
			{Name: "a2", Provider: "missing2"},
		},
	}
	c := NewConfigSemanticsCheck(cfg, "city.toml")
	r := c.Run(&CheckContext{})
	if r.Status != StatusWarning {
		t.Errorf("status = %d, want Warning; msg = %s", r.Status, r.Message)
	}
	if len(r.Details) < 2 {
		t.Errorf("expected multiple warnings, got %d: %v", len(r.Details), r.Details)
	}
}

// --- humanSize ---

func TestHumanSize(t *testing.T) {
	tests := []struct {
		bytes int64
		want  string
	}{
		{0, "0 B"},
		{512, "512 B"},
		{1024, "1.0 KB"},
		{1536, "1.5 KB"},
		{1048576, "1.0 MB"},
		{1073741824, "1.0 GB"},
	}
	for _, tt := range tests {
		got := humanSize(tt.bytes)
		if got != tt.want {
			t.Errorf("humanSize(%d) = %q, want %q", tt.bytes, got, tt.want)
		}
	}
}
