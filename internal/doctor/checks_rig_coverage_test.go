package doctor

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
)

func TestRigPackCoverageCheck_NoPacks(t *testing.T) {
	cfg := &config.City{}
	c := NewRigPackCoverageCheck(cfg, t.TempDir())
	r := c.Run(&CheckContext{})
	if r.Status != StatusOK {
		t.Errorf("status = %d, want OK; msg = %s", r.Status, r.Message)
	}
}

func TestRigPackCoverageCheck_NoRigs(t *testing.T) {
	dir := t.TempDir()
	packDir := filepath.Join(dir, "packs", "workflow")
	writeTestPack(t, packDir, `
[pack]
name = "workflow"
schema = 2

[[named_session]]
template = "patrol"
scope = "rig"
mode = "always"
`)
	writeTestAgent(t, packDir, "patrol")

	cfg := &config.City{
		PackDirs: []string{packDir},
	}
	c := NewRigPackCoverageCheck(cfg, dir)
	r := c.Run(&CheckContext{})
	if r.Status != StatusWarning {
		t.Errorf("status = %d, want Warning; msg = %s", r.Status, r.Message)
	}
	if len(r.Details) == 0 {
		t.Error("expected details about orphaned rig-scoped named_sessions")
	}
}

func TestRigPackCoverageCheck_RigIncludesPack(t *testing.T) {
	dir := t.TempDir()
	packDir := filepath.Join(dir, "packs", "workflow")
	writeTestPack(t, packDir, `
[pack]
name = "workflow"
schema = 2

[[named_session]]
template = "patrol"
scope = "rig"
mode = "always"
`)
	writeTestAgent(t, packDir, "patrol")

	cfg := &config.City{
		PackDirs: []string{packDir},
		Rigs: []config.Rig{
			{Name: "myproject"},
		},
		RigPackDirs: map[string][]string{
			"myproject": {packDir},
		},
	}
	c := NewRigPackCoverageCheck(cfg, dir)
	r := c.Run(&CheckContext{})
	if r.Status != StatusOK {
		t.Errorf("status = %d, want OK; msg = %s; details = %v", r.Status, r.Message, r.Details)
	}
}

func TestRigPackCoverageCheck_SuspendedRigIgnored(t *testing.T) {
	dir := t.TempDir()
	packDir := filepath.Join(dir, "packs", "workflow")
	writeTestPack(t, packDir, `
[pack]
name = "workflow"
schema = 2

[[named_session]]
template = "patrol"
scope = "rig"
mode = "always"
`)
	writeTestAgent(t, packDir, "patrol")

	cfg := &config.City{
		PackDirs: []string{packDir},
		Rigs: []config.Rig{
			{Name: "myproject", Suspended: true},
		},
		RigPackDirs: map[string][]string{
			"myproject": {packDir},
		},
	}
	c := NewRigPackCoverageCheck(cfg, dir)
	r := c.Run(&CheckContext{})
	if r.Status != StatusWarning {
		t.Errorf("status = %d, want Warning (suspended rig should not count); msg = %s", r.Status, r.Message)
	}
}

func TestRigPackCoverageCheck_OnDemandNotWarned(t *testing.T) {
	dir := t.TempDir()
	packDir := filepath.Join(dir, "packs", "workflow")
	writeTestPack(t, packDir, `
[pack]
name = "workflow"
schema = 2

[[named_session]]
template = "helper"
scope = "rig"
mode = "on_demand"
`)
	writeTestAgent(t, packDir, "helper")

	cfg := &config.City{
		PackDirs: []string{packDir},
	}
	c := NewRigPackCoverageCheck(cfg, dir)
	r := c.Run(&CheckContext{})
	if r.Status != StatusOK {
		t.Errorf("status = %d, want OK (on_demand should not warn); msg = %s", r.Status, r.Message)
	}
}

func TestRigPackCoverageCheck_CityScopedIgnored(t *testing.T) {
	dir := t.TempDir()
	packDir := filepath.Join(dir, "packs", "workflow")
	writeTestPack(t, packDir, `
[pack]
name = "workflow"
schema = 2

[[named_session]]
template = "coordinator"
scope = "city"
mode = "always"
`)
	writeTestAgent(t, packDir, "coordinator")

	cfg := &config.City{
		PackDirs: []string{packDir},
	}
	c := NewRigPackCoverageCheck(cfg, dir)
	r := c.Run(&CheckContext{})
	if r.Status != StatusOK {
		t.Errorf("status = %d, want OK (city-scoped should not warn); msg = %s", r.Status, r.Message)
	}
}

func TestRigPackCoverageCheck_MultipleOrphanedSessions(t *testing.T) {
	dir := t.TempDir()
	packDir := filepath.Join(dir, "packs", "workflow")
	writeTestPack(t, packDir, `
[pack]
name = "workflow"
schema = 2

[[named_session]]
template = "patrol"
scope = "rig"
mode = "always"

[[named_session]]
template = "merger"
scope = "rig"
mode = "always"
`)
	writeTestAgent(t, packDir, "patrol")
	writeTestAgent(t, packDir, "merger")

	cfg := &config.City{
		PackDirs: []string{packDir},
		Rigs: []config.Rig{
			{Name: "myproject"},
		},
	}
	c := NewRigPackCoverageCheck(cfg, dir)
	r := c.Run(&CheckContext{})
	if r.Status != StatusWarning {
		t.Errorf("status = %d, want Warning; msg = %s", r.Status, r.Message)
	}
	found := 0
	for _, d := range r.Details {
		if strings.Contains(d, "patrol") || strings.Contains(d, "merger") {
			found++
		}
	}
	if found < 2 {
		t.Errorf("expected details for both patrol and merger, got %v", r.Details)
	}
}

func TestRigPackCoverageCheck_PartialCoverage(t *testing.T) {
	dir := t.TempDir()
	packDir := filepath.Join(dir, "packs", "workflow")
	writeTestPack(t, packDir, `
[pack]
name = "workflow"
schema = 2

[[named_session]]
template = "patrol"
scope = "rig"
mode = "always"
`)
	writeTestAgent(t, packDir, "patrol")

	cfg := &config.City{
		PackDirs: []string{packDir},
		Rigs: []config.Rig{
			{Name: "covered"},
			{Name: "uncovered"},
		},
		RigPackDirs: map[string][]string{
			"covered": {packDir},
		},
	}
	c := NewRigPackCoverageCheck(cfg, dir)
	r := c.Run(&CheckContext{})
	if r.Status != StatusWarning {
		t.Errorf("status = %d, want Warning (uncovered rig exists); msg = %s", r.Status, r.Message)
	}
	foundUncovered := false
	for _, d := range r.Details {
		if strings.Contains(d, "uncovered") {
			foundUncovered = true
		}
	}
	if !foundUncovered {
		t.Errorf("expected detail about uncovered rig, got %v", r.Details)
	}
}

func TestRigPackCoverageCheck_FixHint(t *testing.T) {
	dir := t.TempDir()
	packDir := filepath.Join(dir, "packs", "workflow")
	writeTestPack(t, packDir, `
[pack]
name = "workflow"
schema = 2

[[named_session]]
template = "patrol"
scope = "rig"
mode = "always"
`)
	writeTestAgent(t, packDir, "patrol")

	cfg := &config.City{
		PackDirs: []string{packDir},
		Rigs: []config.Rig{
			{Name: "myproject"},
		},
	}
	c := NewRigPackCoverageCheck(cfg, dir)
	r := c.Run(&CheckContext{})
	if r.FixHint == "" {
		t.Error("expected a fix hint")
	}
	if !strings.Contains(r.FixHint, "defaults.rig.imports") {
		t.Errorf("FixHint = %q, want it to mention [defaults.rig.imports] to guide the operator to the actual config knob", r.FixHint)
	}
}

// TestRigPackCoverageCheck_PackWithoutRigSessions asserts that a pack
// which declares [pack] metadata but no rig-scoped always-mode sessions
// does NOT trigger a warning. The check is about orphaned rig-scoped
// always-mode sessions; a pack that doesn't declare any has nothing to
// orphan. Regression coverage for the "no-sessions" case the PR body
// claims is covered but TestRigPackCoverageCheck_NoPacks does not
// exercise (that one uses a zero-pack city).
func TestRigPackCoverageCheck_PackWithoutRigSessions(t *testing.T) {
	dir := t.TempDir()
	packDir := filepath.Join(dir, "packs", "city-only")
	writeTestPack(t, packDir, `
[pack]
name = "city-only"
schema = 2

[[named_session]]
template = "coordinator"
scope = "city"
mode = "always"
`)
	writeTestAgent(t, packDir, "coordinator")

	cfg := &config.City{
		PackDirs: []string{packDir},
		Rigs:     []config.Rig{{Name: "myproject"}},
	}
	c := NewRigPackCoverageCheck(cfg, dir)
	r := c.Run(&CheckContext{})
	if r.Status != StatusOK {
		t.Errorf("status = %d, want OK (no rig-scoped always sessions to orphan); details = %v", r.Status, r.Details)
	}
}

// TestRigPackCoverageCheck_MalformedPackToml asserts the [M1] behavior
// — a pack whose pack.toml exists but cannot be parsed surfaces a
// warning identifying the pack and the parse error, rather than
// silently skipping (which would defeat the diagnostic's purpose).
func TestRigPackCoverageCheck_MalformedPackToml(t *testing.T) {
	dir := t.TempDir()
	packDir := filepath.Join(dir, "packs", "broken")
	if err := os.MkdirAll(packDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(packDir, "pack.toml"), []byte("not valid = toml = at all"), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg := &config.City{
		PackDirs: []string{packDir},
		Rigs:     []config.Rig{{Name: "myproject"}},
	}
	c := NewRigPackCoverageCheck(cfg, dir)
	r := c.Run(&CheckContext{})
	if r.Status != StatusWarning {
		t.Fatalf("status = %d, want Warning (malformed pack.toml); details = %v", r.Status, r.Details)
	}
	var found bool
	for _, d := range r.Details {
		if strings.Contains(d, "broken") && strings.Contains(d, "pack.toml") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected a detail identifying packs/broken/pack.toml, got %v", r.Details)
	}
}

func writeTestPack(t *testing.T, packDir, content string) {
	t.Helper()
	if err := os.MkdirAll(packDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(packDir, "pack.toml"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func writeTestAgent(t *testing.T, packDir, name string) {
	t.Helper()
	agentDir := filepath.Join(packDir, "agents", name)
	if err := os.MkdirAll(agentDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(agentDir, "agent.toml"), []byte(`scope = "rig"`), 0o644); err != nil {
		t.Fatal(err)
	}
}
