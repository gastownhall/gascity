//go:build windows

package hooks

import (
	"bytes"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

func TestPreserveManagedFileUsesWindowsPathSeparators(t *testing.T) {
	rel := filepath.Join(".opencode", "plugins", "gascity.js")
	current := installedOpenCodePlugin(t)
	if len(current) == 0 {
		t.Fatal("installed OpenCode plugin was empty")
	}
	if !strings.Contains(string(current), "Gas City hooks for OpenCode.") {
		t.Fatalf("installed OpenCode plugin missing marker:\n%s", current)
	}
	if !PreserveManagedFile(rel, current) {
		t.Fatalf("current managed plugin at native Windows rel path %q was not preserved", rel)
	}
	stale := bytes.Replace(current,
		[]byte(fmt.Sprintf("GC_OPENCODE_HOOK_VERSION = %d", managedOpenCodeHookVersion)),
		[]byte("GC_OPENCODE_HOOK_VERSION = 0"), 1)
	if bytes.Equal(stale, current) {
		t.Fatal("test setup did not make OpenCode plugin stale")
	}
	if PreserveManagedFile(rel, stale) {
		t.Fatalf("stale managed plugin at native Windows rel path %q was preserved", rel)
	}
}
