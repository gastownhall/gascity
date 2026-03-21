package main

import (
	"testing"

	"github.com/gastownhall/gascity/internal/fsys"
)

func TestReadInstructionsFile(t *testing.T) {
	tests := []struct {
		name             string
		workDir          string
		cityPath         string
		instructionsFile string
		files            map[string][]byte
		want             string
	}{
		{
			name:             "happy path: file in workDir",
			workDir:          "/repo",
			cityPath:         "/city",
			instructionsFile: "CLAUDE.md",
			files: map[string][]byte{
				"/repo/CLAUDE.md": []byte("# Instructions\nDo good work.\n"),
			},
			want: "# Instructions\nDo good work.\n",
		},
		{
			name:             "fallback: file only in cityPath",
			workDir:          "/repo",
			cityPath:         "/city",
			instructionsFile: "CLAUDE.md",
			files: map[string][]byte{
				"/city/CLAUDE.md": []byte("# City Instructions\n"),
			},
			want: "# City Instructions\n",
		},
		{
			name:             "missing everywhere: returns empty string",
			workDir:          "/repo",
			cityPath:         "/city",
			instructionsFile: "CLAUDE.md",
			files:            map[string][]byte{},
			want:             "",
		},
		{
			name:             "empty instructionsFile defaults to AGENTS.md",
			workDir:          "/repo",
			cityPath:         "/city",
			instructionsFile: "",
			files: map[string][]byte{
				"/repo/AGENTS.md": []byte("# Agent Instructions\n"),
			},
			want: "# Agent Instructions\n",
		},
		{
			name:             "different provider filename: CLAUDE.md vs AGENTS.md",
			workDir:          "/repo",
			cityPath:         "/city",
			instructionsFile: "CLAUDE.md",
			files: map[string][]byte{
				"/repo/CLAUDE.md":  []byte("claude content"),
				"/repo/AGENTS.md": []byte("agents content"),
			},
			want: "claude content",
		},
		{
			name:             "workDir equals cityPath: no redundant read",
			workDir:          "/city",
			cityPath:         "/city",
			instructionsFile: "CLAUDE.md",
			files: map[string][]byte{
				"/city/CLAUDE.md": []byte("same-dir content"),
			},
			want: "same-dir content",
		},
		{
			name:             "workDir equals cityPath and file missing: empty string",
			workDir:          "/city",
			cityPath:         "/city",
			instructionsFile: "CLAUDE.md",
			files:            map[string][]byte{},
			want:             "",
		},
		{
			name:             "workDir has file, cityPath also has file: workDir wins",
			workDir:          "/repo",
			cityPath:         "/city",
			instructionsFile: "AGENTS.md",
			files: map[string][]byte{
				"/repo/AGENTS.md": []byte("repo version"),
				"/city/AGENTS.md": []byte("city version"),
			},
			want: "repo version",
		},
		{
			name:             "empty default falls back to cityPath AGENTS.md",
			workDir:          "/repo",
			cityPath:         "/city",
			instructionsFile: "",
			files: map[string][]byte{
				"/city/AGENTS.md": []byte("city agents fallback"),
			},
			want: "city agents fallback",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := fsys.NewFake()
			for path, content := range tt.files {
				f.Files[path] = content
			}

			got := readInstructionsFile(f, tt.workDir, tt.cityPath, tt.instructionsFile)
			if got != tt.want {
				t.Errorf("readInstructionsFile() = %q, want %q", got, tt.want)
			}
		})
	}
}
