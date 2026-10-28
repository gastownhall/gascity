//go:build windows

package runtime

import (
	"os"
	"path/filepath"
	"testing"
)

func TestStageWorkDirAdaptsInstructionCopyEntryDestinationForms(t *testing.T) {
	tests := []struct {
		name    string
		relDst  string
		makeSrc func(t *testing.T, root string) string
		prepare func(t *testing.T, workDir string)
		wantRel string
	}{
		{
			name:   "exact file",
			relDst: filepath.Join(".github", "copilot-instructions.md"),
			makeSrc: func(t *testing.T, root string) string {
				return writeStageSourceFile(t, root, "source.md", "Run `gc prime`.\n")
			},
			wantRel: filepath.Join(".github", "copilot-instructions.md"),
		},
		{
			name:   "existing destination directory",
			relDst: ".github",
			makeSrc: func(t *testing.T, root string) string {
				return writeStageSourceFile(t, root, "copilot-instructions.md", "Run `gc prime`.\n")
			},
			prepare: func(t *testing.T, workDir string) {
				if err := os.MkdirAll(filepath.Join(workDir, ".github"), 0o755); err != nil {
					t.Fatalf("prepare .github: %v", err)
				}
			},
			wantRel: filepath.Join(".github", "copilot-instructions.md"),
		},
		{
			name:   "dot destination directory",
			relDst: ".",
			makeSrc: func(t *testing.T, root string) string {
				return writeStageSourceFile(t, root, "AGENTS.md", "Run `gc prime`.\n")
			},
			wantRel: "AGENTS.md",
		},
		{
			name:   "empty destination",
			relDst: "",
			makeSrc: func(t *testing.T, root string) string {
				return writeStageSourceFile(t, root, "AGENTS.md", "Run `gc prime`.\n")
			},
			wantRel: "AGENTS.md",
		},
		{
			name:   "github source directory",
			relDst: ".github",
			makeSrc: func(t *testing.T, root string) string {
				srcDir := filepath.Join(root, ".github")
				if err := os.MkdirAll(srcDir, 0o755); err != nil {
					t.Fatalf("mkdir source directory: %v", err)
				}
				writeStageSourceFile(t, srcDir, "copilot-instructions.md", "Run `gc prime`.\n")
				return srcDir
			},
			wantRel: filepath.Join(".github", "copilot-instructions.md"),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srcRoot := t.TempDir()
			workDir := t.TempDir()
			src := tt.makeSrc(t, srcRoot)
			if tt.prepare != nil {
				tt.prepare(t, workDir)
			}

			for round := 0; round < 2; round++ {
				if err := StageWorkDir(workDir, "", []CopyEntry{{Src: src, RelDst: tt.relDst}}); err != nil {
					t.Fatalf("StageWorkDir round %d: %v", round, err)
				}
				got, err := os.ReadFile(filepath.Join(workDir, tt.wantRel))
				if err != nil {
					t.Fatalf("ReadFile round %d: %v", round, err)
				}
				if string(got) != "Run `gc.exe prime`.\n" {
					t.Fatalf("round %d staged %q, want adapted instruction bytes", round, got)
				}
			}
		})
	}
}

func writeStageSourceFile(t *testing.T, root, rel, content string) string {
	t.Helper()
	path := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("MkdirAll source: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile source: %v", err)
	}
	return path
}
