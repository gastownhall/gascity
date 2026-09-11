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

	// Check basic structure: frontmatter title, never a body H1 (Mintlify
	// renders the title; a body H1 would duplicate it).
	if !strings.Contains(out, `title: "CLI Reference"`) {
		t.Error("missing CLI Reference frontmatter title")
	}
	if strings.Contains(out, "# CLI Reference") {
		t.Error("body H1 duplicates the frontmatter title")
	}
	if !strings.Contains(out, "Auto-generated") {
		t.Error("missing auto-generated note")
	}
}

func TestGenDocImportAddDocumentsSourceLanes(t *testing.T) {
	var buf bytes.Buffer
	root := newRootCmd(&buf, &buf)

	var md bytes.Buffer
	if err := docgen.RenderCLIMarkdown(&md, root); err != nil {
		t.Fatalf("RenderCLIMarkdown: %v", err)
	}

	section, ok := cliDocSection(md.String(), "gc import add")
	if !ok {
		t.Fatal("missing gc import add section")
	}
	for _, want := range []string{
		"local paths outside git worktrees",
		"remote git repositories",
		"remote GitHub repository subpaths",
		"Registry catalog handles are lookup shortcuts",
		"source and optional version",
		"local binding name",
		"display/advisory metadata",
	} {
		if !strings.Contains(section, want) {
			t.Fatalf("gc import add docs missing %q:\n%s", want, section)
		}
	}
}

func TestGenDocImportAddExamplesAvoidRejectedSourceRefs(t *testing.T) {
	var buf bytes.Buffer
	root := newRootCmd(&buf, &buf)

	var md bytes.Buffer
	if err := docgen.RenderCLIMarkdown(&md, root); err != nil {
		t.Fatalf("RenderCLIMarkdown: %v", err)
	}

	section, ok := cliDocSection(md.String(), "gc import add")
	if !ok {
		t.Fatal("missing gc import add section")
	}
	for _, line := range strings.Split(section, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "gc import add ") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 4 {
			t.Fatalf("gc import add example missing source: %q", line)
		}
		source := fields[3]
		if isRemoteImportSource(source) && hasRepositoryRefInSource(source) {
			t.Fatalf("gc import add example uses rejected source ref %q in:\n%s", source, section)
		}
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
		t.Errorf("docs/reference/cli.md is stale — missing sections for %d commands. Run: go run ./cmd/gc gen-doc\nMissing: %v", len(missing), missing)
	}

	var live bytes.Buffer
	if err := docgen.RenderCLIMarkdown(&live, root); err != nil {
		t.Fatalf("RenderCLIMarkdown: %v", err)
	}
	var staleSections []string
	for _, command := range []string{
		"gc mail inbox",
		"gc mail read",
		"gc mail peek",
		"gc mail thread",
		"gc mail count",
		"gc trace status",
		"gc trace show",
	} {
		committedSection, ok := cliDocSection(doc, command)
		if !ok {
			continue
		}
		liveSection, ok := cliDocSection(live.String(), command)
		if !ok || committedSection != liveSection {
			staleSections = append(staleSections, command)
		}
	}
	if len(staleSections) > 0 {
		t.Errorf("docs/reference/cli.md has stale command sections. Run: go run ./cmd/gc gen-doc\nStale: %v", staleSections)
	}
}

func cliDocSection(doc, command string) (string, bool) {
	heading := "## " + command + "\n"
	start := strings.Index(doc, heading)
	if start < 0 {
		return "", false
	}
	rest := doc[start+len(heading):]
	next := strings.Index(rest, "\n## ")
	if next < 0 {
		return doc[start:], true
	}
	return doc[start : start+len(heading)+next+1], true
}
