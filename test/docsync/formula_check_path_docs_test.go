package docsync

import (
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/formula"
)

// tomlFence is one fenced Markdown block whose info string is toml.
type tomlFence struct {
	// firstLine is the 1-based file line of the block's first content line.
	firstLine int
	lines     []string
}

// docCheckPath is one check script path declared in a fenced TOML example.
type docCheckPath struct {
	line  int
	table string
	path  string
}

func (c docCheckPath) String() string {
	return fmt.Sprintf("%d: [%s] %s", c.line, c.table, c.path)
}

// fencedTOMLBlocks returns every fenced block of a Markdown page whose info
// string is toml. A fence closes on a line of at least as many of the same
// fence character with no info string; a fence left open at the end of the
// page is an error, because everything after it would be misread.
func fencedTOMLBlocks(content string) ([]tomlFence, error) {
	var blocks []tomlFence
	var open *tomlFence
	var marker string
	openLine := 0
	isTOML := false
	for i, raw := range strings.Split(content, "\n") {
		trimmed := strings.TrimSpace(raw)
		if marker == "" {
			fence, info, ok := fenceOpener(trimmed)
			if !ok {
				continue
			}
			marker, openLine = fence, i+1
			isTOML = strings.EqualFold(info, "toml")
			if isTOML {
				open = &tomlFence{firstLine: i + 2}
			}
			continue
		}
		if strings.HasPrefix(trimmed, marker) && strings.Trim(trimmed, marker[:1]) == "" {
			if isTOML {
				blocks = append(blocks, *open)
				open = nil
			}
			marker = ""
			continue
		}
		if isTOML {
			open.lines = append(open.lines, raw)
		}
	}
	if marker != "" {
		return nil, fmt.Errorf("fence opened at line %d is never closed", openLine)
	}
	return blocks, nil
}

// fenceOpener reports whether a trimmed line opens a fence, returning the run
// of fence characters and the first word of the info string.
func fenceOpener(trimmed string) (fence, info string, ok bool) {
	for _, ch := range []string{"`", "~"} {
		n := len(trimmed) - len(strings.TrimLeft(trimmed, ch))
		if n < 3 {
			continue
		}
		fields := strings.Fields(trimmed[n:])
		if len(fields) > 0 {
			info = fields[0]
		}
		return trimmed[:n], info, true
	}
	return "", "", false
}

var (
	tomlTableHeaderRE = regexp.MustCompile(`^\[\[?\s*([^\[\]]+?)\s*\]\]?\s*(#.*)?$`)
	tomlKeyValueRE    = regexp.MustCompile(`^([A-Za-z0-9_.\-" ]+?)\s*=\s*(.+)$`)
	tomlStringRE      = regexp.MustCompile(`^(?:"([^"]*)"|'([^']*)')`)
	tomlInlinePathRE  = regexp.MustCompile(`(?:^|[{,\s])path\s*=\s*(?:"([^"]*)"|'([^']*)')`)
)

// checkPathsInTOML returns every check script path a fenced TOML example
// declares, with its file line. A check script path is the value whose full
// dotted key ends in check.check.path, written as `path = "…"` under a
// [….check.check] table, `check.path = "…"` under a [….check] table, or an
// inline `check = { … path = "…" … }` under a [….check] table.
func checkPathsInTOML(block tomlFence) []docCheckPath {
	var found []docCheckPath
	table := ""
	for i, raw := range block.lines {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if m := tomlTableHeaderRE.FindStringSubmatch(line); m != nil {
			table = normalizeTOMLKey(m[1])
			continue
		}
		m := tomlKeyValueRE.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		key := normalizeTOMLKey(m[1])
		if table != "" {
			key = table + "." + key
		}
		value := strings.TrimSpace(m[2])
		switch {
		case isCheckScriptKey(key+".path") && strings.HasPrefix(value, "{"):
			if pm := tomlInlinePathRE.FindStringSubmatch(value); pm != nil {
				found = append(found, docCheckPath{line: block.firstLine + i, table: table, path: pm[1] + pm[2]})
			}
		case isCheckScriptKey(key):
			if sm := tomlStringRE.FindStringSubmatch(value); sm != nil {
				found = append(found, docCheckPath{line: block.firstLine + i, table: table, path: sm[1] + sm[2]})
			}
		}
	}
	return found
}

// normalizeTOMLKey strips quotes and the spaces TOML allows around the dots
// of a dotted key.
func normalizeTOMLKey(key string) string {
	parts := strings.Split(key, ".")
	for i, part := range parts {
		parts[i] = strings.Trim(strings.TrimSpace(part), `"'`)
	}
	return strings.Join(parts, ".")
}

func isCheckScriptKey(key string) bool {
	return key == "check.check.path" || strings.HasSuffix(key, ".check.check.path")
}

// isRuntimeStatePath reports whether a check path points into the city's
// .gc/ runtime state, which Gas City never fills from a pack's assets/.
func isRuntimeStatePath(p string) bool {
	cleaned := path.Clean(p)
	return cleaned == ".gc" || strings.HasPrefix(cleaned, ".gc/")
}

// runtimeStateCheckPaths returns the check script paths of a Markdown page's
// TOML examples that point into .gc/.
func runtimeStateCheckPaths(content string) ([]docCheckPath, error) {
	blocks, err := fencedTOMLBlocks(content)
	if err != nil {
		return nil, err
	}
	var bad []docCheckPath
	for _, block := range blocks {
		for _, c := range checkPathsInTOML(block) {
			if isRuntimeStatePath(c.path) {
				bad = append(bad, c)
			}
		}
	}
	return bad, nil
}

// TestDocsCheckPathsAvoidRuntimeStateDir keeps the published docs from
// teaching a check script path under .gc/. A pack ships its check scripts in
// assets/, and nothing copies them into .gc/, so a formula that copies such an
// example declares a gate whose script never exists (#4239). The working form
// is ../assets/scripts/checks/<name>, which resolves through the formula layers.
func TestDocsCheckPathsAvoidRuntimeStateDir(t *testing.T) {
	docsRoot := filepath.Join(repoRoot(), "docs")
	files, err := collectDocsMarkdownFiles(docsRoot)
	if err != nil {
		t.Fatalf("listing docs: %v", err)
	}
	var findings []string
	for _, rel := range files {
		data, err := os.ReadFile(filepath.Join(docsRoot, filepath.FromSlash(rel)))
		if err != nil {
			t.Fatalf("reading docs/%s: %v", rel, err)
		}
		bad, err := runtimeStateCheckPaths(string(data))
		if err != nil {
			t.Errorf("docs/%s: %v", rel, err)
			continue
		}
		for _, c := range bad {
			findings = append(findings, "docs/"+rel+":"+c.String())
		}
	}
	if len(findings) > 0 {
		t.Errorf("check script paths under .gc/ never resolve to a pack-shipped script; use ../assets/scripts/checks/<name>:\n  %s",
			strings.Join(findings, "\n  "))
	}
}

// TestUnderstandingFormulasLinksAssetCheckForm keeps the guide's explanation
// of the ../assets/ check form pointing at its normative definition.
func TestUnderstandingFormulasLinksAssetCheckForm(t *testing.T) {
	data, err := os.ReadFile(filepath.Join(repoRoot(), "docs", "guides", "understanding-formulas.md"))
	if err != nil {
		t.Fatalf("reading guide: %v", err)
	}
	content := string(data)
	for _, want := range []string{
		"../assets/scripts/checks/implementation-review-approved.sh",
		"../assets/scripts/checks/build-artifact-valid.sh",
		"/reference/specs/formula-spec-v2#31-check",
	} {
		if !strings.Contains(content, want) {
			t.Errorf("docs/guides/understanding-formulas.md does not mention %q", want)
		}
	}
}

func TestScanDocCheckPaths(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    []string
	}{
		{
			name:    "steps check table",
			content: "intro\n```toml\n[steps.check.check]\nmode = \"exec\"\npath = \".gc/scripts/checks/x.sh\"\n```\n",
			want:    []string{"5: [steps.check.check] .gc/scripts/checks/x.sh"},
		},
		{
			name:    "template check table",
			content: "```toml\n[[template]]\nid = \"t\"\n\n[template.check.check]\npath = \".gc/scripts/checks/x.sh\"\n```\n",
			want:    []string{"6: [template.check.check] .gc/scripts/checks/x.sh"},
		},
		{
			name:    "inline check table",
			content: "```toml\n[steps.check]\ncheck = { mode = \"exec\", path = \".gc/x.sh\" }\n```\n",
			want:    []string{"3: [steps.check] .gc/x.sh"},
		},
		{
			name:    "dotted check key",
			content: "```toml\n[steps.check]\ncheck.path = \".gc/x.sh\"\n```\n",
			want:    []string{"3: [steps.check] .gc/x.sh"},
		},
		{
			name:    "dot-slash prefix is cleaned",
			content: "```toml\n[steps.check.check]\npath = \"./.gc/scripts/x.sh\"\n```\n",
			want:    []string{"3: [steps.check.check] ./.gc/scripts/x.sh"},
		},
		{
			name: "allowed paths",
			content: "```toml\n[steps.check.check]\npath = \"../assets/scripts/checks/x.sh\"\n```\n" +
				"```toml\n[steps.check.check]\npath = \"scripts/verify.sh\"\n```\n" +
				"```toml\n[steps.check.check]\npath = \"/opt/checks/x.sh\"\n```\n",
		},
		{
			name:    "non-check table",
			content: "```toml\n[agent]\npath = \".gc/x\"\n```\n",
		},
		{
			name:    "shell fence and prose",
			content: "Do not use .gc/scripts/x.sh.\n```shell\npath = \".gc/scripts/x.sh\"\n```\n",
		},
		{
			name:    "toml example nested in a longer fence",
			content: "````markdown\n```toml\n[steps.check.check]\npath = \".gc/x.sh\"\n```\n````\n",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			bad, err := runtimeStateCheckPaths(tt.content)
			if err != nil {
				t.Fatalf("runtimeStateCheckPaths: %v", err)
			}
			var got []string
			for _, c := range bad {
				got = append(got, c.String())
			}
			if strings.Join(got, "\n") != strings.Join(tt.want, "\n") {
				t.Errorf("findings = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestFencedTOMLBlocksRejectsUnclosedFence(t *testing.T) {
	_, err := fencedTOMLBlocks("text\n```toml\n[steps.check.check]\npath = \"x.sh\"\n")
	if err == nil || !strings.Contains(err.Error(), "line 2") {
		t.Fatalf("fencedTOMLBlocks error = %v, want an unclosed fence at line 2", err)
	}
}

// markdownSection returns the lines of a Markdown page from the heading line
// that starts with heading up to the next heading of the same or a higher
// level. Lines inside fenced blocks are never headings.
func markdownSection(t *testing.T, content, heading string) string {
	t.Helper()
	level := len(heading) - len(strings.TrimLeft(heading, "#"))
	lines := strings.Split(content, "\n")
	start := -1
	inFence := false
	for i, line := range lines {
		if strings.HasPrefix(strings.TrimSpace(line), "```") {
			inFence = !inFence
		}
		if inFence {
			continue
		}
		if start < 0 {
			if strings.HasPrefix(line, heading) {
				start = i
			}
			continue
		}
		hashes := len(line) - len(strings.TrimLeft(line, "#"))
		if hashes > 0 && hashes <= level && strings.HasPrefix(line[hashes:], " ") {
			return strings.Join(lines[start:i], "\n")
		}
	}
	if start < 0 {
		t.Fatalf("heading %q not found", heading)
	}
	return strings.Join(lines[start:], "\n")
}

// assetCheckExamples returns the TOML examples of a Markdown section that
// declare a check script in the ../assets/scripts/checks/ form, with that
// declaration.
func assetCheckExamples(t *testing.T, section string) ([]tomlFence, []docCheckPath) {
	t.Helper()
	blocks, err := fencedTOMLBlocks(section)
	if err != nil {
		t.Fatalf("scanning section: %v", err)
	}
	var examples []tomlFence
	var checks []docCheckPath
	for _, block := range blocks {
		for _, c := range checkPathsInTOML(block) {
			if strings.HasPrefix(c.path, "../assets/scripts/checks/") {
				examples = append(examples, block)
				checks = append(checks, c)
				break
			}
		}
	}
	return examples, checks
}

var tomlFormulaNameRE = regexp.MustCompile(`(?m)^formula\s*=\s*"([^"]+)"`)

// TestFormulaSpecAssetCheckPathExampleCompiles loads the formula-spec-v2
// section 3.1 example of a layer-shipped check script with the real formula
// parser, so the documented ../assets/ form is the one the compiler resolves.
func TestFormulaSpecAssetCheckPathExampleCompiles(t *testing.T) {
	data, err := os.ReadFile(filepath.Join(repoRoot(), "docs", "reference", "specs", "formula-spec-v2.md"))
	if err != nil {
		t.Fatalf("reading formula spec: %v", err)
	}
	section := markdownSection(t, string(data), "### 3.1. Check")
	examples, checks := assetCheckExamples(t, section)
	if len(examples) != 1 {
		t.Fatalf("formula-spec-v2 section 3.1 has %d TOML examples with a ../assets/scripts/checks/ check path, want 1", len(examples))
	}
	example := strings.Join(examples[0].lines, "\n")
	name := tomlFormulaNameRE.FindStringSubmatch(example)
	if name == nil {
		t.Fatalf("section 3.1 asset check example declares no formula name:\n%s", example)
	}
	scriptRel := strings.TrimPrefix(checks[0].path, "../assets/")

	writeLayer := func(t *testing.T, withScript bool) (formulasDir, script string) {
		t.Helper()
		packDir := filepath.Join(t.TempDir(), "pack")
		formulasDir = filepath.Join(packDir, "formulas")
		if err := os.MkdirAll(formulasDir, 0o755); err != nil {
			t.Fatalf("mkdir formulas: %v", err)
		}
		if err := os.WriteFile(filepath.Join(formulasDir, name[1]+".toml"), []byte(example), 0o644); err != nil {
			t.Fatalf("write formula: %v", err)
		}
		script = filepath.Join(packDir, "assets", filepath.FromSlash(scriptRel))
		if withScript {
			if err := os.MkdirAll(filepath.Dir(script), 0o755); err != nil {
				t.Fatalf("mkdir checks: %v", err)
			}
			if err := os.WriteFile(script, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
				t.Fatalf("write check script: %v", err)
			}
		}
		return formulasDir, script
	}

	t.Run("shipped script resolves to the layer asset", func(t *testing.T) {
		formulasDir, script := writeLayer(t, true)
		f, err := formula.NewParser(formulasDir).LoadByName(name[1])
		if err != nil {
			t.Fatalf("LoadByName(%q): %v", name[1], err)
		}
		if len(f.Steps) != 1 || f.Steps[0].Ralph == nil || f.Steps[0].Ralph.Check == nil {
			t.Fatalf("example compiled to %d steps without exactly one check step", len(f.Steps))
		}
		if got := f.Steps[0].Ralph.Check.Path; got != script {
			t.Errorf("check path = %q, want the layer's shipped script %q", got, script)
		}
	})

	t.Run("missing script fails to load", func(t *testing.T) {
		formulasDir, _ := writeLayer(t, false)
		_, err := formula.NewParser(formulasDir).LoadByName(name[1])
		if err == nil || !strings.Contains(err.Error(), "not found in any formula layer") {
			t.Fatalf("LoadByName(%q) error = %v, want not found in any formula layer", name[1], err)
		}
	})

	t.Run("key table names the assets form", func(t *testing.T) {
		var row string
		for _, line := range strings.Split(section, "\n") {
			if strings.HasPrefix(line, "| `check.path` |") {
				row = line
				break
			}
		}
		if !strings.Contains(row, "../assets/") {
			t.Errorf("section 3.1 check.path row = %q, want it to name the ../assets/ form", row)
		}
	})
}
