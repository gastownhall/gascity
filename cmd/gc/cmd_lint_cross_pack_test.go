package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A city's packs compose ONE fragment namespace at runtime; lint . must
// resolve a {{template}} reference defined by a SIBLING pack rather than
// false-erroring "not defined" (measured 2026-08-24: ~35 false errors
// across CE agents referencing another pack's delivery-canon fragment).
func TestLintDotResolvesFragmentsAcrossSiblingPacks(t *testing.T) {
	root := t.TempDir()
	frag := filepath.Join(root, "frag-pack", "template-fragments")
	if err := os.MkdirAll(frag, 0o755); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(root, "frag-pack", "pack.toml"), "[pack]\nname = \"frag-pack\"\nversion = \"0.1.0\"\nschema = 2\n")
	mustWrite(t, filepath.Join(frag, "shared-bit.template.md"), "{{define \"shared-bit\"}}hello{{end}}\n")

	agents := filepath.Join(root, "user-pack", "agents", "worker")
	if err := os.MkdirAll(agents, 0o755); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(root, "user-pack", "pack.toml"),
		"[pack]\nname = \"user-pack\"\nversion = \"0.1.0\"\nschema = 2\n\n[[agent]]\nname = \"worker\"\nprompt_template = \"agents/worker/prompt.template.md\"\n")
	mustWrite(t, filepath.Join(agents, "prompt.template.md"), "before\n{{template \"shared-bit\" .}}\nafter\n")

	cwd, _ := os.Getwd()
	if err := os.Chdir(root); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(cwd) })

	report := lintTarget(".")
	for _, p := range report.Packs {
		for _, d := range p.Diagnostics {
			if strings.Contains(d.Message, "not defined") {
				t.Fatalf("cross-pack fragment false error survived: %+v", d)
			}
		}
	}
	if !report.Passed {
		t.Fatalf("expected lint to pass, got %+v", report)
	}
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
