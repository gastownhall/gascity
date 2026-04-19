package main

import (
	"bytes"
	"os"
	"testing"
)

func TestDoConfigShowMissingRemoteImportSuggestsInstall(t *testing.T) {
	clearGCEnv(t)
	dir := t.TempDir()
	t.Chdir(dir)
	if err := os.MkdirAll(".gc", 0o755); err != nil {
		t.Fatalf("MkdirAll(.gc): %v", err)
	}
	writeCityToml(t, dir, "[workspace]\nname = \"demo\"\n")
	writePackToml(t, dir, `[pack]
name = "demo"
schema = 1

[imports.tools]
source = "https://example.com/tools.git"
version = "^1.4"
`)

	var stdout, stderr bytes.Buffer
	code := doConfigShow(false, false, &stdout, &stderr)
	if code == 0 {
		t.Fatal("expected failure for missing remote import")
	}
	if got := stderr.String(); !bytes.Contains([]byte(got), []byte(`run "gc import install"`)) {
		t.Fatalf("stderr = %q, want install remediation", got)
	}
}
