package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReadRuntimeIssuePrefixParsesJSONValue(t *testing.T) {
	binDir := t.TempDir()
	fakeBd := filepath.Join(binDir, "bd")
	script := `#!/bin/sh
if [ "$1" = "config" ] && [ "$2" = "get" ] && [ "$3" = "--json" ] && [ "$4" = "issue_prefix" ]; then
  printf '{"value":"gc"}\n'
  exit 0
fi
echo "unexpected: $*" >&2
exit 1
`
	if err := os.WriteFile(fakeBd, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	got, err := readRuntimeIssuePrefix(dir)
	if err != nil {
		t.Fatalf("readRuntimeIssuePrefix: %v", err)
	}
	if got != "gc" {
		t.Fatalf("readRuntimeIssuePrefix = %q, want gc", got)
	}
}

func TestVerifyRuntimeIssuePrefixRejectsMissing(t *testing.T) {
	binDir := t.TempDir()
	fakeBd := filepath.Join(binDir, "bd")
	script := `#!/bin/sh
printf '{"value":""}\n'
exit 0
`
	if err := os.WriteFile(fakeBd, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	err := verifyRuntimeIssuePrefix(dir, "gc")
	if err == nil {
		t.Fatal("verifyRuntimeIssuePrefix() error = nil, want missing prefix failure")
	}
	if !strings.Contains(err.Error(), "issue_prefix") {
		t.Fatalf("error = %v, want issue_prefix mention", err)
	}
}

func TestVerifyRuntimeIssuePrefixAcceptsMatch(t *testing.T) {
	binDir := t.TempDir()
	fakeBd := filepath.Join(binDir, "bd")
	script := `#!/bin/sh
printf '{"value":"gc"}\n'
exit 0
`
	if err := os.WriteFile(fakeBd, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := verifyRuntimeIssuePrefix(dir, "gc"); err != nil {
		t.Fatalf("verifyRuntimeIssuePrefix: %v", err)
	}
}

func TestShouldVerifyRuntimeIssuePrefixOnlyForManagedScript(t *testing.T) {
	cityPath := t.TempDir()
	realScript := gcBeadsBdScriptPath(cityPath)
	if err := os.MkdirAll(filepath.Dir(realScript), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(realScript, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	t.Setenv("GC_BEADS", "bd")
	if !shouldVerifyRuntimeIssuePrefix(cityPath) {
		t.Fatal("shouldVerifyRuntimeIssuePrefix() = false for GC_BEADS=bd, want true")
	}

	t.Setenv("GC_BEADS", "exec:"+filepath.Join(t.TempDir(), "gc-beads-bd"))
	if shouldVerifyRuntimeIssuePrefix(cityPath) {
		t.Fatal("shouldVerifyRuntimeIssuePrefix() = true for capture script, want false")
	}
}
