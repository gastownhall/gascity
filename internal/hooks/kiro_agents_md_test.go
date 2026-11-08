package hooks

import (
	"bytes"
	"sort"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/bootstrap/packs/core"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/overlay"
)

func currentKiroAgentsMD(t *testing.T) []byte {
	t.Helper()
	data, err := core.PackFS.ReadFile("overlay/per-provider/kiro/AGENTS.md")
	if err != nil {
		t.Fatalf("read embedded kiro AGENTS.md: %v", err)
	}
	return data
}

// The current core fallback must never itself be a released body, or the
// upgrade would rewrite it forever and could never tell old from new.
func TestCurrentKiroAgentsMDIsNotAReleasedBody(t *testing.T) {
	current := currentKiroAgentsMD(t)
	if overlay.IsReleasedKiroAgentsMD(current) {
		t.Fatal("core kiro AGENTS.md is listed as a released body to replace")
	}
	if !strings.Contains(string(current), "gc hook --claim") {
		t.Fatal("core kiro AGENTS.md no longer names gc hook --claim; re-check which released bodies the upgrade must replace")
	}
}

func TestInstallKiroUpgradesReleasedAgentsMD(t *testing.T) {
	current := currentKiroAgentsMD(t)
	for version, body := range overlay.ReleasedKiroAgentsMDBodies() {
		t.Run(version, func(t *testing.T) {
			fs := fsys.NewFake()
			fs.Files["/work/AGENTS.md"] = append([]byte(nil), body...)

			if err := Install(fs, "/city", "/work", []string{"kiro"}); err != nil {
				t.Fatalf("Install: %v", err)
			}
			if got := fs.Files["/work/AGENTS.md"]; !bytes.Equal(got, current) {
				t.Fatalf("AGENTS.md was not upgraded from the released %s body:\n%s", version, got)
			}
			assertNoAgentsMDBackup(t, fs)

			// A second install is a no-op.
			fs.Calls = nil
			if err := Install(fs, "/city", "/work", []string{"kiro"}); err != nil {
				t.Fatalf("second Install: %v", err)
			}
			if got := fs.Files["/work/AGENTS.md"]; !bytes.Equal(got, current) {
				t.Fatalf("second Install changed AGENTS.md:\n%s", got)
			}
			for _, call := range fs.Calls {
				if call.Method == "WriteFile" && call.Path == "/work/AGENTS.md" {
					t.Fatalf("second Install rewrote the current AGENTS.md: %+v", fs.Calls)
				}
			}
			assertNoAgentsMDBackup(t, fs)
		})
	}
}

func TestInstallKiroKeepsCustomAgentsMD(t *testing.T) {
	released := overlay.ReleasedKiroAgentsMDBodies()["v1.4.0"]
	if released == nil {
		t.Fatal("missing released v1.4.0 kiro AGENTS.md body")
	}
	for name, body := range map[string][]byte{
		"repository instructions": []byte("# Project agents\n\nRun make test before committing.\n"),
		"released body no EOL":    bytes.TrimSuffix(released, []byte("\n")),
		"released body CRLF":      bytes.ReplaceAll(released, []byte("\n"), []byte("\r\n")),
		"released body edited":    bytes.Replace(released, []byte("You are an agent"), []byte("You are a reviewer agent"), 1),
		"empty":                   {},
	} {
		t.Run(name, func(t *testing.T) {
			fs := fsys.NewFake()
			fs.Files["/work/AGENTS.md"] = append([]byte(nil), body...)
			if err := Install(fs, "/city", "/work", []string{"kiro"}); err != nil {
				t.Fatalf("Install: %v", err)
			}
			if got := fs.Files["/work/AGENTS.md"]; !bytes.Equal(got, body) {
				t.Fatalf("AGENTS.md = %q, want the existing file kept", got)
			}
			assertNoAgentsMDBackup(t, fs)
		})
	}
}

func TestInstallKiroWritesAgentsMDWhenMissing(t *testing.T) {
	fs := fsys.NewFake()
	if err := Install(fs, "/city", "/work", []string{"kiro"}); err != nil {
		t.Fatalf("Install: %v", err)
	}
	if got := fs.Files["/work/AGENTS.md"]; !bytes.Equal(got, currentKiroAgentsMD(t)) {
		t.Fatalf("AGENTS.md = %q, want the core fallback", got)
	}
}

// Kiro's AGENTS.md stays out of the staging preserve map: listing a path there
// moved the core fingerprint for copilot (and drained fresh sessions), and
// overlay staging already applies the released-body rule itself.
func TestPreserveManagedFileDoesNotCoverKiroAgentsMD(t *testing.T) {
	if _, ok := managedOverlayHookPaths[overlay.KiroAgentsMDPath]; ok {
		t.Fatal("managedOverlayHookPaths lists the kiro AGENTS.md")
	}
	if PreserveManagedFile(overlay.KiroAgentsMDPath, currentKiroAgentsMD(t)) {
		t.Fatal("PreserveManagedFile keeps the kiro AGENTS.md")
	}
}

func assertNoAgentsMDBackup(t *testing.T, fs *fsys.Fake) {
	t.Helper()
	var backups []string
	for p := range fs.Files {
		if strings.HasPrefix(p, "/work/AGENTS.md.bak") {
			backups = append(backups, p)
		}
	}
	sort.Strings(backups)
	if len(backups) > 0 {
		t.Fatalf("unexpected AGENTS.md backups: %v", backups)
	}
}
