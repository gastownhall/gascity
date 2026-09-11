package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/fsys"
)

func TestDoImportAddRejectsUserinfoAndRedacts(t *testing.T) {
	city := t.TempDir()
	if err := os.WriteFile(filepath.Join(city, "city.toml"), []byte("[workspace]\nname = \"demo\"\n"), 0o644); err != nil {
		t.Fatalf("write city.toml: %v", err)
	}
	var stdout, stderr strings.Builder
	rc := doImportAdd(fsys.OSFS{}, city, "https://user:ghp_secrettoken@github.com/gascity/repo", "", "", &stdout, &stderr)
	if rc == 0 {
		t.Fatalf("expected userinfo source to be rejected")
	}
	if strings.Contains(stderr.String(), "ghp_secrettoken") {
		t.Fatalf("stderr leaked the token: %q", stderr.String())
	}
	if !strings.Contains(stderr.String(), "***@") {
		t.Fatalf("stderr should carry a redacted source, got %q", stderr.String())
	}
}
