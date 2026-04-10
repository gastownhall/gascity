package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Phase 0 spec coverage from engdocs/design/session-model-unification.md:
// - Runtime Environment
// - session-context execution / gc hook

func TestPhase0Hook_UsesGCTemplateForConfigLookupInSessionContext(t *testing.T) {
	cityDir := t.TempDir()
	workDir := t.TempDir()
	fakeBin := t.TempDir()

	if err := os.MkdirAll(filepath.Join(cityDir, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	cityToml := `[workspace]
name = "test-city"

[[agent]]
name = "reviewer"
start_command = "true"
`
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(cityToml), 0o644); err != nil {
		t.Fatal(err)
	}

	fakeBD := filepath.Join(fakeBin, "bd")
	script := "#!/bin/sh\nprintf 'pwd=%s\\nargs=%s\\n' \"$PWD\" \"$*\"\n"
	if err := os.WriteFile(fakeBD, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	origPath := os.Getenv("PATH")
	t.Setenv("PATH", fakeBin+string(os.PathListSeparator)+origPath)
	t.Setenv("GC_CITY", cityDir)
	t.Setenv("GC_ALIAS", "mayor")
	t.Setenv("GC_AGENT", "mayor")
	t.Setenv("GC_TEMPLATE", "reviewer")
	t.Setenv("GC_SESSION_ORIGIN", "named")

	origWD, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(origWD) })
	if err := os.Chdir(workDir); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	code := cmdHook(nil, false, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("cmdHook() = %d, want 0; stderr=%s", code, stderr.String())
	}
	out := stdout.String()
	if !strings.Contains(out, "gc.routed_to=reviewer") {
		t.Fatalf("stdout = %q, want reviewer work_query routed via GC_TEMPLATE", out)
	}
	if strings.Contains(out, "gc.routed_to=mayor") {
		t.Fatalf("stdout = %q, should not resolve public alias as backing config", out)
	}
	if !strings.Contains(out, fmt.Sprintf("pwd=%s", cityDir)) {
		t.Fatalf("stdout = %q, want hook to run from city root", out)
	}
}
