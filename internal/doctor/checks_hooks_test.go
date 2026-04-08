package doctor

import (
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/hooks"
)

func TestHooksCheck_RunOK(t *testing.T) {
	fs := fsys.NewFake()
	check := NewHooksCheck("/city", []string{"/work"}, fs, nil)
	result := check.Run(&CheckContext{CityPath: "/city"})
	if result.Status != StatusOK {
		t.Errorf("Run() status = %d, want StatusOK", result.Status)
	}
}

func TestHooksCheck_RunDetectsStale(t *testing.T) {
	fs := fsys.NewFake()
	// Use a stale Claude hooks file (city-level) since its stale format
	// is easy to construct: replace gc handoff with gc prime in current embed.
	stale := staleClaudeHooksForTest(t)
	fs.Files["/city/hooks/claude.json"] = stale
	check := NewHooksCheck("/city", []string{"/work"}, fs, nil)
	result := check.Run(&CheckContext{CityPath: "/city"})
	if result.Status != StatusWarning {
		t.Errorf("Run() status = %d, want StatusWarning", result.Status)
	}
	if len(result.Details) != 1 {
		t.Errorf("Run() details = %v, want 1 entry", result.Details)
	}
}

func TestHooksCheck_FixCallsCallback(t *testing.T) {
	fs := fsys.NewFake()
	stale := staleClaudeHooksForTest(t)
	fs.Files["/city/hooks/claude.json"] = stale

	var fixCalled bool
	var fixStale map[string][]string
	fixFn := func(stale map[string][]string) error {
		fixCalled = true
		fixStale = stale
		return nil
	}

	check := NewHooksCheck("/city", []string{"/work"}, fs, fixFn)
	if err := check.Fix(&CheckContext{CityPath: "/city"}); err != nil {
		t.Fatalf("Fix() error = %v", err)
	}
	if !fixCalled {
		t.Error("Fix() did not call fixFn")
	}
	if providers, ok := fixStale["/city"]; !ok || len(providers) != 1 || providers[0] != "claude" {
		t.Errorf("Fix() stale = %v, want {/city: [claude]}", fixStale)
	}
}

func TestHooksCheck_CityAndWorkDirSeparate(t *testing.T) {
	fs := fsys.NewFake()
	// Stale Claude at city level.
	stale := staleClaudeHooksForTest(t)
	fs.Files["/city/hooks/claude.json"] = stale
	// Two rigs — Claude should only appear once (under cityDir), not per rig.
	check := NewHooksCheck("/city", []string{"/work1", "/work2"}, fs, nil)
	result := check.Run(&CheckContext{CityPath: "/city"})
	if result.Status != StatusWarning {
		t.Errorf("Run() status = %d, want StatusWarning", result.Status)
	}
	// Should be exactly 1 detail entry (cityDir), not 2.
	if len(result.Details) != 1 {
		t.Errorf("Run() details = %v, want 1 entry (city-level only)", result.Details)
	}
}

func TestHooksCheck_CanFix(t *testing.T) {
	check := NewHooksCheck("/city", nil, fsys.NewFake(), nil)
	if check.CanFix() {
		t.Error("CanFix() = true with nil FixFn, want false")
	}
	check.FixFn = func(map[string][]string) error { return nil }
	if !check.CanFix() {
		t.Error("CanFix() = false with FixFn set, want true")
	}
}

// staleClaudeHooksForTest constructs a stale Claude hooks file by applying
// the known stale transformation to the current embedded Claude config.
func staleClaudeHooksForTest(t *testing.T) []byte {
	t.Helper()
	// Use hooks.StaleCityHooks indirectly: we need to construct data that
	// claudeFileNeedsUpgrade returns true for. The stale version replaces
	// "gc handoff" with "gc prime --hook" in the PreCompact command.
	providers := hooks.SupportedProviders()
	found := false
	for _, p := range providers {
		if p == "claude" {
			found = true
		}
	}
	if !found {
		t.Skip("claude not in supported providers")
	}
	// Read current Claude embed via Install to a fake FS, then apply the
	// known stale transformation.
	fs := fsys.NewFake()
	if err := hooks.Install(fs, "/tmp-city", "/tmp-work", []string{"claude"}); err != nil {
		t.Fatalf("Install claude: %v", err)
	}
	current := fs.Files["/tmp-city/hooks/claude.json"]
	stale := strings.Replace(string(current), `gc handoff "context cycle"`, `gc prime --hook`, 1)
	return []byte(stale)
}
