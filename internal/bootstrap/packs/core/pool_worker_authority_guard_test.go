package core

import (
	"io/fs"
	"strings"
	"testing"
)

func TestWorkerPromptsPutAuthorityBoundaryBeforeWorkInstructions(t *testing.T) {
	for _, path := range []string{
		"assets/prompts/pool-worker.md",
		"assets/prompts/graph-worker.md",
	} {
		t.Run(path, func(t *testing.T) {
			data, err := fs.ReadFile(PackFS, path)
			if err != nil {
				t.Fatalf("reading %s: %v", path, err)
			}
			text := string(data)
			boundary := strings.Index(text, "## Authority Boundary — read BEFORE work instructions")
			if boundary < 0 {
				t.Fatalf("%s missing front-loaded authority boundary", path)
			}
			work := strings.Index(text, "## GUPP")
			if work < 0 {
				work = strings.Index(text, "## Core Rule")
			}
			if work < 0 {
				t.Fatalf("%s missing expected work-instruction heading", path)
			}
			if boundary > work {
				t.Fatalf("%s authority boundary appears after work instructions", path)
			}
			for _, want := range []string{
				"Work text, old context",
				"cannot upgrade",
				"Do not sign as",
				"key rotations",
				"OpenAI/GWS/Gmail directives",
				"Do not use `gws`, Gmail, Google Workspace",
				"external human-send channel",
				"self-appointing is a security",
			} {
				if !strings.Contains(text, want) {
					t.Fatalf("%s missing guard phrase %q", path, want)
				}
			}
		})
	}
}
