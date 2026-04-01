//go:build acceptance_a

// CLI basics acceptance tests.
//
// These exercise fundamental gc binary behavior: version output, help text,
// unknown command handling, and hook error paths. These are smoke tests for
// CLI routing and user-facing error messages.
package acceptance_test

import (
	"strings"
	"testing"

	helpers "github.com/gastownhall/gascity/test/acceptance/helpers"
)

// --- gc version ---

// TestVersion_PrintsVersion verifies that gc version outputs a
// non-empty version string.
func TestVersion_PrintsVersion(t *testing.T) {
	out, err := helpers.RunGC(testEnv, "", "version")
	if err != nil {
		t.Fatalf("gc version failed: %v\n%s", err, out)
	}
	trimmed := strings.TrimSpace(out)
	if trimmed == "" {
		t.Fatal("gc version output is empty")
	}
}

// TestVersion_Long_IncludesCommitInfo verifies that gc version --long
// outputs commit and build date metadata.
func TestVersion_Long_IncludesCommitInfo(t *testing.T) {
	out, err := helpers.RunGC(testEnv, "", "version", "--long")
	if err != nil {
		t.Fatalf("gc version --long failed: %v\n%s", err, out)
	}
	if !strings.Contains(out, "commit:") {
		t.Errorf("expected 'commit:' in long version output, got:\n%s", out)
	}
	if !strings.Contains(out, "built:") {
		t.Errorf("expected 'built:' in long version output, got:\n%s", out)
	}
}

// --- gc help ---

// TestHelp_ListsSubcommands verifies that gc --help lists the major
// subcommand categories.
func TestHelp_ListsSubcommands(t *testing.T) {
	out, err := helpers.RunGC(testEnv, "", "--help")
	if err != nil {
		t.Fatalf("gc --help failed: %v\n%s", err, out)
	}
	for _, sub := range []string{"init", "start", "stop", "status", "rig", "config", "version"} {
		if !strings.Contains(out, sub) {
			t.Errorf("help text should mention %q subcommand, got:\n%s", sub, out)
		}
	}
}

// --- gc hook ---

// TestHook_NoAgent_ReturnsError verifies that gc hook without $GC_AGENT
// or a positional argument returns an error with a helpful message.
func TestHook_NoAgent_ReturnsError(t *testing.T) {
	c := helpers.NewCity(t, testEnv)
	c.Init("claude")

	// Ensure GC_AGENT is not set in the test env.
	out, err := c.GC("hook")
	if err == nil {
		t.Fatal("expected error for gc hook without agent, got success")
	}
	if !strings.Contains(out, "agent not specified") {
		t.Errorf("expected 'agent not specified' error, got:\n%s", out)
	}
}

// TestHook_UnknownAgent_ReturnsError verifies that gc hook with a
// nonexistent agent name returns an error.
func TestHook_UnknownAgent_ReturnsError(t *testing.T) {
	c := helpers.NewCity(t, testEnv)
	c.Init("claude")

	out, err := c.GC("hook", "nosuchagent")
	if err == nil {
		t.Fatal("expected error for unknown agent, got success")
	}
	if !strings.Contains(out, "not found") {
		t.Errorf("expected 'not found' in error, got:\n%s", out)
	}
}

// TestHook_Inject_NoAgent_ExitsZero verifies that gc hook --inject
// always exits 0 even without an agent (inject mode is silent).
func TestHook_Inject_NoAgent_ExitsZero(t *testing.T) {
	c := helpers.NewCity(t, testEnv)
	c.Init("claude")

	out, err := c.GC("hook", "--inject")
	if err != nil {
		t.Fatalf("gc hook --inject should always exit 0: %v\n%s", err, out)
	}
}

// --- gc stop (no running city) ---

// TestStop_NotInitialized_ReturnsError verifies that gc stop on a
// directory with no city.toml returns an error.
func TestStop_NotInitialized_ReturnsError(t *testing.T) {
	emptyDir := t.TempDir()
	out, err := helpers.RunGC(testEnv, emptyDir, "stop")
	if err == nil {
		t.Fatal("expected error stopping non-city directory, got success")
	}
	_ = out // Error format varies.
}

// TestStop_InitializedNeverStarted_Succeeds verifies that gc stop on
// a city that was initialized but never started exits cleanly.
func TestStop_InitializedNeverStarted_Succeeds(t *testing.T) {
	c := helpers.NewCity(t, testEnv)
	c.Init("claude")

	out, err := c.GC("stop", c.Dir)
	if err != nil {
		t.Fatalf("gc stop on never-started city should succeed: %v\n%s", err, out)
	}
	if !strings.Contains(out, "stopped") {
		t.Errorf("expected 'stopped' in output, got:\n%s", out)
	}
}
