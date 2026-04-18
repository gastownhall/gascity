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

	if err := os.MkdirAll(filepath.Join(cityDir, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	cityToml := `[workspace]
name = "test-city"

[[agent]]
name = "reviewer"
start_command = "true"
work_query = "printf 'pwd=%s|agent=%s|template=%s|session=%s|origin=%s' \"$PWD\" \"$GC_AGENT\" \"$GC_TEMPLATE\" \"$GC_SESSION_NAME\" \"$GC_SESSION_ORIGIN\""
`
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(cityToml), 0o644); err != nil {
		t.Fatal(err)
	}

	t.Setenv("GC_CITY", cityDir)
	t.Setenv("GC_ALIAS", "mayor")
	t.Setenv("GC_AGENT", "mayor")
	t.Setenv("GC_SESSION_NAME", "test-city--mayor")
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
	if !strings.Contains(out, "template=reviewer") {
		t.Fatalf("stdout = %q, want reviewer work_query selected via GC_TEMPLATE", out)
	}
	if !strings.Contains(out, "agent=mayor") {
		t.Fatalf("stdout = %q, want GC_AGENT to remain the public named handle", out)
	}
	if !strings.Contains(out, "session=test-city--mayor") {
		t.Fatalf("stdout = %q, want GC_SESSION_NAME to remain the named session handle", out)
	}
	if !strings.Contains(out, "origin=named") {
		t.Fatalf("stdout = %q, want GC_SESSION_ORIGIN to remain named", out)
	}
	if !strings.Contains(out, fmt.Sprintf("pwd=%s", cityDir)) {
		t.Fatalf("stdout = %q, want hook to run from city root", out)
	}
}

func TestPhase0Hook_AliaslessOrdinarySessionUsesGCTemplateForConfigLookup(t *testing.T) {
	cityDir := t.TempDir()
	workDir := t.TempDir()

	if err := os.MkdirAll(filepath.Join(cityDir, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	cityToml := `[workspace]
name = "test-city"

[[agent]]
name = "reviewer"
start_command = "true"
work_query = "printf 'agent=%s|template=%s|session=%s|origin=%s' \"$GC_AGENT\" \"$GC_TEMPLATE\" \"$GC_SESSION_NAME\" \"$GC_SESSION_ORIGIN\""
`
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(cityToml), 0o644); err != nil {
		t.Fatal(err)
	}

	t.Setenv("GC_CITY", cityDir)
	t.Setenv("GC_ALIAS", "")
	t.Setenv("GC_AGENT", "s-gc-ordinary")
	t.Setenv("GC_SESSION_NAME", "s-gc-ordinary")
	t.Setenv("GC_TEMPLATE", "reviewer")
	t.Setenv("GC_SESSION_ORIGIN", "ephemeral")

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
	for _, want := range []string{
		"agent=s-gc-ordinary",
		"template=reviewer",
		"session=s-gc-ordinary",
		"origin=ephemeral",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("stdout = %q, want %q", out, want)
		}
	}
}

func TestPhase0Hook_NamedSessionContextPreservesExactOwnerEnv(t *testing.T) {
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
	script := "#!/bin/sh\nprintf 'id=%s\\nname=%s\\nalias=%s\\nagent=%s\\norigin=%s\\ntemplate=%s\\nargs=%s\\n' \"$GC_SESSION_ID\" \"$GC_SESSION_NAME\" \"$GC_ALIAS\" \"$GC_AGENT\" \"$GC_SESSION_ORIGIN\" \"$GC_TEMPLATE\" \"$*\"\n"
	if err := os.WriteFile(fakeBD, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	origPath := os.Getenv("PATH")
	t.Setenv("PATH", fakeBin+string(os.PathListSeparator)+origPath)
	t.Setenv("GC_CITY", cityDir)
	t.Setenv("GC_SESSION_ID", "mc-session-123")
	t.Setenv("GC_SESSION_NAME", "test-city--mayor")
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
	for _, want := range []string{
		"id=mc-session-123",
		"name=test-city--mayor",
		"alias=mayor",
		"agent=mayor",
		"origin=named",
		"template=reviewer",
		"--assignee=mc-session-123",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("stdout = %q, want %q", out, want)
		}
	}
}
