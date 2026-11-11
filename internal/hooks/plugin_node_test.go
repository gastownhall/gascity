package hooks

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
)

// TestProviderPluginNodeSuite executes opencode_plugin.test.mjs under node so
// the embedded OpenCode and MiMo Code plugins are exercised as the provider
// loads them, not just string-matched. The suite drives a fake gc through
// GC_BIN and pins where the plugins put each turn's clock, nudge and mail
// lines so the provider's prompt-cache prefix survives across turns: the
// cached prime stays attached to system[0] ahead of the provider's own text;
// OpenCode's plugin adds the per-turn lines to the user message, and MiMo
// Code's appends them after every stable system entry, including when
// chat.message runs first and OpenCode's system-list fold is reproduced.
func TestProviderPluginNodeSuite(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the fake gc used by the suite is a POSIX shell script")
	}
	nodeBin, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; cannot execute the provider plugins")
	}
	suite, err := filepath.Abs("opencode_plugin.test.mjs")
	if err != nil {
		t.Fatalf("resolving suite path: %v", err)
	}

	cmd := exec.Command(nodeBin, "--test", suite)
	// Hermetic env: the suite sets GC_BIN itself; nothing else from the
	// developer's shell may leak into the plugin's child processes.
	cmd.Env = []string{
		"HOME=" + t.TempDir(),
		"PATH=" + os.Getenv("PATH"),
		"TMPDIR=" + t.TempDir(),
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("node --test %s: %v\noutput:\n%s", suite, err, out)
	}
}
