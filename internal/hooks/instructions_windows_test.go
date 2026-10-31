//go:build windows

package hooks

import (
	"bytes"
	"path"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/bootstrap/packs/core"
	"github.com/gastownhall/gascity/internal/fsys"
)

func bareGCInstructionCommands(data []byte) []string {
	text := string(data)
	var out []string
	codeStart := -1
	for i := 0; i < len(text); {
		if text[i] != '`' {
			i++
			continue
		}
		j := i
		for j < len(text) && text[j] == '`' {
			j++
		}
		if j-i != 1 {
			i = j
			continue
		}
		if codeStart < 0 {
			codeStart = i + 1
		} else {
			code := text[codeStart:i]
			if code == "gc" || strings.HasPrefix(code, "gc ") {
				out = append(out, code)
			}
			codeStart = -1
		}
		i = j
	}
	return out
}

func requireGCExeInstructions(t *testing.T, data []byte, want string) {
	t.Helper()
	if bare := bareGCInstructionCommands(data); len(bare) > 0 {
		t.Fatalf("instructions still contain bare gc inline commands: %q\n%s", bare, data)
	}
	if !bytes.Contains(data, []byte(want)) {
		t.Fatalf("instructions missing %s:\n%s", want, data)
	}
}

func TestInstallAdaptsInstructionMarkdownOnWindows(t *testing.T) {
	tests := []struct {
		name     string
		provider string
		dst      []string
		want     string
	}{
		{
			name:     "copilot",
			provider: "copilot",
			dst:      []string{".github", "copilot-instructions.md"},
			want:     "`gc.exe bd update <id> --claim`",
		},
		{
			name:     "kiro",
			provider: "kiro",
			dst:      []string{"AGENTS.md"},
			want:     "`gc.exe nudge drain --inject`",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fs := fsys.NewFake()
			if err := Install(fs, "/city", "/work", []string{tt.provider}); err != nil {
				t.Fatalf("Install: %v", err)
			}
			requireGCExeInstructions(t, fs.Files[filepath.Join(append([]string{"/work"}, tt.dst...)...)], tt.want)
		})
	}
}

func TestInstallUpgradesLegacyInstructionMarkdownOnWindows(t *testing.T) {
	tests := []struct {
		name     string
		provider string
		rel      string
		dst      []string
		want     string
	}{
		{
			name:     "copilot",
			provider: "copilot",
			rel:      path.Join(".github", "copilot-instructions.md"),
			dst:      []string{".github", "copilot-instructions.md"},
			want:     "`gc.exe bd update <id> --claim`",
		},
		{
			name:     "kiro",
			provider: "kiro",
			rel:      "AGENTS.md",
			dst:      []string{"AGENTS.md"},
			want:     "`gc.exe nudge drain --inject`",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			legacy, err := core.PackFS.ReadFile(path.Join("overlay", "per-provider", tt.provider, tt.rel))
			if err != nil {
				t.Fatalf("read legacy instructions: %v", err)
			}
			fs := fsys.NewFake()
			dst := filepath.Join(append([]string{"/work"}, tt.dst...)...)
			fs.Files[dst] = legacy

			if err := Install(fs, "/city", "/work", []string{tt.provider}); err != nil {
				t.Fatalf("Install: %v", err)
			}
			requireGCExeInstructions(t, fs.Files[dst], tt.want)
			if got := fs.Files[dst+".bak"]; !bytes.Equal(got, legacy) {
				t.Fatalf("legacy backup = %q, want original legacy instructions", got)
			}
		})
	}
}

func TestInstallPreservesUserAuthoredInstructionMarkdownOnWindows(t *testing.T) {
	tests := []struct {
		name     string
		provider string
		dst      []string
	}{
		{name: "copilot", provider: "copilot", dst: []string{".github", "copilot-instructions.md"}},
		{name: "kiro", provider: "kiro", dst: []string{"AGENTS.md"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			custom := []byte("Custom workspace instructions with `gc local`.\n")
			fs := fsys.NewFake()
			dst := filepath.Join(append([]string{"/work"}, tt.dst...)...)
			fs.Files[dst] = custom

			if err := Install(fs, "/city", "/work", []string{tt.provider}); err != nil {
				t.Fatalf("Install: %v", err)
			}
			if got := fs.Files[dst]; !bytes.Equal(got, custom) {
				t.Fatalf("user-authored instructions were overwritten:\n%s", got)
			}
			if _, ok := fs.Files[dst+".bak"]; ok {
				t.Fatal("user-authored instructions were backed up as if they were managed")
			}
		})
	}
}
