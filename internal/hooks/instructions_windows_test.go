//go:build windows

package hooks

import (
	"bytes"
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/fsys"
)

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
