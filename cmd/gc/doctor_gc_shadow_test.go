package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/doctor"
)

// writeGCShadowFile writes content at path, creating parent directories.
func writeGCShadowFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func snapshotPath(home, name string) string {
	return filepath.Join(home, ".claude", "shell-snapshots", name)
}

func TestGCShadowCheck_AliasInSnapshotWarns(t *testing.T) {
	home := t.TempDir()
	writeGCShadowFile(t, snapshotPath(home, "snapshot-zsh-1.sh"),
		"# oh-my-zsh git plugin\nalias gc='git commit --verbose'\n")

	c := &gcShadowCheck{homeDir: home}
	r := c.Run(nil)
	if r.Status != doctor.StatusWarning {
		t.Fatalf("status = %v, want StatusWarning", r.Status)
	}
	if r.Severity != doctor.SeverityAdvisory {
		t.Fatalf("severity = %v, want SeverityAdvisory (observability, never gates)", r.Severity)
	}
	if r.FixHint == "" {
		t.Fatalf("a warning should carry a FixHint (unalias + fresh session)")
	}
	if !strings.Contains(strings.Join(r.Details, "\n"), "snapshot-zsh-1.sh") {
		t.Fatalf("details should name the offending snapshot file, got %v", r.Details)
	}
}

func TestGCShadowCheck_AliasDoubleDashForm(t *testing.T) {
	home := t.TempDir()
	writeGCShadowFile(t, snapshotPath(home, "snapshot-zsh-2.sh"),
		"alias -- gc='git commit --verbose'\n")

	r := (&gcShadowCheck{homeDir: home}).Run(nil)
	if r.Status != doctor.StatusWarning {
		t.Fatalf("status = %v, want StatusWarning for `alias -- gc=`", r.Status)
	}
}

func TestGCShadowCheck_FunctionForms(t *testing.T) {
	for name, content := range map[string]string{
		"posix":   "gc () {\n  git commit \"$@\"\n}\n",
		"keyword": "function gc {\n  git commit \"$@\"\n}\n",
	} {
		t.Run(name, func(t *testing.T) {
			home := t.TempDir()
			writeGCShadowFile(t, snapshotPath(home, "snapshot-bash-1.sh"), content)
			r := (&gcShadowCheck{homeDir: home}).Run(nil)
			if r.Status != doctor.StatusWarning {
				t.Fatalf("status = %v, want StatusWarning for gc function form %q", r.Status, content)
			}
		})
	}
}

func TestGCShadowCheck_RCFileWarns(t *testing.T) {
	home := t.TempDir()
	writeGCShadowFile(t, filepath.Join(home, ".zshrc"), "alias gc='git commit --verbose'\n")

	r := (&gcShadowCheck{homeDir: home}).Run(nil)
	if r.Status != doctor.StatusWarning {
		t.Fatalf("status = %v, want StatusWarning for alias in ~/.zshrc", r.Status)
	}
	if !strings.Contains(strings.Join(r.Details, "\n"), ".zshrc") {
		t.Fatalf("details should name .zshrc, got %v", r.Details)
	}
}

func TestGCShadowCheck_NegativesStayOK(t *testing.T) {
	home := t.TempDir()
	writeGCShadowFile(t, snapshotPath(home, "snapshot-zsh-3.sh"), strings.Join([]string{
		"# alias gc='git commit --verbose'",  // commented out
		"alias gcb='git checkout -b'",        // different name, gc prefix
		"alias mygc='true'",                  // suffix
		"echo alias gc is fine inside words", // not a definition
		"gcloud () { true; }",                // different function
	}, "\n")+"\n")
	writeGCShadowFile(t, filepath.Join(home, ".bashrc"), "# alias gc=nope\nalias gca='git commit -a'\n")

	r := (&gcShadowCheck{homeDir: home}).Run(nil)
	if r.Status != doctor.StatusOK {
		t.Fatalf("status = %v, want StatusOK; details: %v", r.Status, r.Details)
	}
}

func TestGCShadowCheck_NoSnapshotDirOK(t *testing.T) {
	r := (&gcShadowCheck{homeDir: t.TempDir()}).Run(nil)
	if r.Status != doctor.StatusOK {
		t.Fatalf("status = %v, want StatusOK when nothing exists", r.Status)
	}
}

func TestGCShadowCheck_EmptyHomeSkips(t *testing.T) {
	r := (&gcShadowCheck{homeDir: ""}).Run(nil)
	if r.Status != doctor.StatusOK {
		t.Fatalf("status = %v, want StatusOK when home dir is unknown", r.Status)
	}
}
