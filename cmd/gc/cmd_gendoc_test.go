package main

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/docgen"
	"github.com/spf13/cobra"
)

func TestGenDocProducesMarkdown(t *testing.T) {
	var buf bytes.Buffer
	root := newRootCmd(&buf, &buf)

	// Render to buffer using the renderer directly (avoids needing repo root
	// for the go.mod check in the RunE handler).
	var md bytes.Buffer
	if err := docgen.RenderCLIMarkdown(&md, root); err != nil {
		t.Fatalf("RenderCLIMarkdown: %v", err)
	}

	out := md.String()
	if out == "" {
		t.Fatal("empty markdown output")
	}

	// Check known visible commands exist.
	for _, cmd := range []string{"gc init", "gc start", "gc stop", "gc agent", "gc rig add", "gc mail"} {
		if !strings.Contains(out, "## "+cmd) {
			t.Errorf("missing command %q in CLI reference", cmd)
		}
	}

	// Check hidden commands are absent.
	if strings.Contains(out, "## gc gen-doc") {
		t.Error("hidden command gen-doc should not appear")
	}

	// Check basic structure.
	if !strings.Contains(out, "# CLI Reference") {
		t.Error("missing CLI Reference header")
	}
	if !strings.Contains(out, "Auto-generated") {
		t.Error("missing auto-generated note")
	}
}

// TestCLIDocsFreshness verifies every non-hidden command in the live cobra
// tree has a section in docs/reference/cli.md. Catches "added or renamed a
// command without running go run ./cmd/genschema". Avoids strict byte-equal
// comparison because cobra lazily registers `completion`/`help` only on
// Execute, which the in-test render path does not trigger.
func TestCLIDocsFreshness(t *testing.T) {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	repoRoot := filepath.Join(filepath.Dir(thisFile), "..", "..")

	committedPath := filepath.Join(repoRoot, "docs", "reference", "cli.md")
	committed, err := os.ReadFile(committedPath)
	if err != nil {
		t.Fatalf("reading %s: %v\nRun: go run ./cmd/genschema", committedPath, err)
	}
	doc := string(committed)

	var buf bytes.Buffer
	root := newRootCmd(&buf, &buf)

	var missing []string
	var walk func(cmd *cobra.Command)
	walk = func(cmd *cobra.Command) {
		if cmd.Hidden || cmd.Annotations["gc.docgen.skip"] == "true" {
			return
		}
		heading := "## " + cmd.CommandPath() + "\n"
		if !strings.Contains(doc, heading) {
			missing = append(missing, cmd.CommandPath())
		}
		for _, c := range cmd.Commands() {
			walk(c)
		}
	}
	walk(root)

	if len(missing) > 0 {
		t.Errorf("docs/reference/cli.md is stale — missing sections for %d commands. Run: go run ./cmd/genschema\nMissing: %v", len(missing), missing)
	}
}
