package doctor

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestIssuePrefixCheck_NoBeadsDir(t *testing.T) {
	dir := t.TempDir()
	c := NewIssuePrefixCheck(dir, "city")
	r := c.Run(&CheckContext{CityPath: dir})
	if r.Status != StatusOK {
		t.Fatalf("status = %v, want StatusOK when .beads is missing", r.Status)
	}
	if !strings.Contains(r.Message, "skipping") {
		t.Fatalf("message = %q, want skipping note", r.Message)
	}
}

func TestIssuePrefixCheck_MissingCanonicalPrefix(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".beads", "config.yaml"), []byte("dolt.auto-start: false\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	c := NewIssuePrefixCheck(dir, "city")
	r := c.Run(&CheckContext{CityPath: dir})
	if r.Status != StatusWarning {
		t.Fatalf("status = %v, want StatusWarning when canonical prefix is absent", r.Status)
	}
}

func TestIssuePrefixCheck_Name(t *testing.T) {
	c := NewIssuePrefixCheck("/tmp/city", "city")
	if got := c.Name(); got != "issue-prefix:city" {
		t.Fatalf("Name() = %q, want issue-prefix:city", got)
	}
	if c.CanFix() {
		t.Fatal("CanFix() = true, want false (bd rejects config set issue_prefix)")
	}
}
