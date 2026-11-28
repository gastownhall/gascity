package docsync

import (
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// Formula examples in docs/ teach check script paths. The form that resolves
// is "../assets/scripts/checks/<name>" (a formula-layer asset); a path under
// ".gc/" names runtime state that no layer ever ships, so a check declared
// that way can never launch (#4239). These helpers scan fenced TOML blocks
// for check script paths so tests can keep every example on the resolvable
// form.

// tomlBlock is one fenced ```toml block of a markdown file. startLine is the
// 1-based file line of the block's first content line (the line after the
// opening fence).
type tomlBlock struct {
	lines     []string
	startLine int
}

// docCheckPath is a check script path declared inside a fenced TOML block,
// with the 1-based file line that carries it.
type docCheckPath struct {
	path string
	line int
}

var (
	// fenceLineRE matches an opening or closing code fence (``` or longer).
	fenceLineRE = regexp.MustCompile("^`{3,}")
	// tomlTableRE captures the dotted name of a [table] or [[array]] header.
	tomlTableRE = regexp.MustCompile(`^\[+\s*([^\]\s]+)\s*\]+`)
	// tomlPathKeyRE captures a `path = "…"` assignment.
	tomlPathKeyRE = regexp.MustCompile(`^path\s*=\s*"([^"]*)"`)
	// tomlCheckDotPathRE captures a `check.path = "…"` dotted assignment.
	tomlCheckDotPathRE = regexp.MustCompile(`^check\.path\s*=\s*"([^"]*)"`)
	// tomlCheckInlineRE captures a `check = { … }` inline table.
	tomlCheckInlineRE = regexp.MustCompile(`^check\s*=\s*\{(.*)\}\s*$`)
	// inlinePathRE extracts a `path = "…"` pair inside an inline table.
	inlinePathRE = regexp.MustCompile(`\bpath\s*=\s*"([^"]*)"`)
)

// fencedTOMLBlocks returns the ```toml fenced blocks of a markdown document.
// Any other fence (```sh, ```text, …) is skipped, so TOML-looking text inside
// it is ignored. A toml fence left unterminated at end of file is an error:
// everything after it would silently escape the scan.
func fencedTOMLBlocks(content string) ([]tomlBlock, error) {
	var blocks []tomlBlock
	inFence := false
	isTOML := false
	var current tomlBlock
	fenceLine := 0
	for i, line := range strings.Split(content, "\n") {
		trimmed := strings.TrimSpace(line)
		if !fenceLineRE.MatchString(trimmed) {
			if inFence {
				current.lines = append(current.lines, line)
			}
			continue
		}
		if !inFence {
			inFence = true
			info := strings.TrimSpace(strings.TrimLeft(trimmed, "`"))
			fields := strings.Fields(info)
			isTOML = len(fields) > 0 && fields[0] == "toml"
			if isTOML {
				current = tomlBlock{startLine: i + 2}
				fenceLine = i + 1
			}
			continue
		}
		// Closing fence. An info string on a closing fence is not valid
		// markdown, but treat any fence line as closing — matching how the
		// rest of this package scans fenced blocks.
		if isTOML {
			blocks = append(blocks, current)
			current = tomlBlock{}
		}
		inFence = false
		isTOML = false
	}
	if inFence && isTOML {
		return nil, fmt.Errorf("unterminated ```toml fence opening at line %d", fenceLine)
	}
	return blocks, nil
}

// checkPathsInTOML returns every check script path a block declares:
//
//   - `path = "…"` in a table whose dotted name ends in `.check.check`
//     ([steps.check.check] / [template.check.check]);
//   - `check = { … path = "…" … }` in a table ending in `.check`;
//   - `check.path = "…"` in a table ending in `.check`.
//
// Line numbers are file lines (block.startLine + offset within the block).
func checkPathsInTOML(block tomlBlock) []docCheckPath {
	var out []docCheckPath
	table := ""
	for i, line := range block.lines {
		trimmed := strings.TrimSpace(line)
		if m := tomlTableRE.FindStringSubmatch(trimmed); m != nil {
			table = m[1]
			continue
		}
		fileLine := block.startLine + i
		switch {
		case strings.HasSuffix(table, ".check.check"):
			if m := tomlPathKeyRE.FindStringSubmatch(trimmed); m != nil {
				out = append(out, docCheckPath{path: m[1], line: fileLine})
			}
		case strings.HasSuffix(table, ".check"):
			if m := tomlCheckDotPathRE.FindStringSubmatch(trimmed); m != nil {
				out = append(out, docCheckPath{path: m[1], line: fileLine})
				continue
			}
			if m := tomlCheckInlineRE.FindStringSubmatch(trimmed); m != nil {
				if pm := inlinePathRE.FindStringSubmatch(m[1]); pm != nil {
					out = append(out, docCheckPath{path: pm[1], line: fileLine})
				}
			}
		}
	}
	return out
}

// isRuntimeStateCheckPath reports whether a check script path names the .gc/
// runtime state directory: Gas City never fills .gc/ from pack assets, so a
// check pointed there has no backing file on any layer (#4239).
func isRuntimeStateCheckPath(p string) bool {
	return strings.HasPrefix(path.Clean(p), ".gc/")
}

// scanDocCheckPaths returns every check script path a markdown document's
// ```toml fences declare, with file lines. An unterminated toml fence is an
// error.
func scanDocCheckPaths(content string) ([]docCheckPath, error) {
	blocks, err := fencedTOMLBlocks(content)
	if err != nil {
		return nil, err
	}
	var out []docCheckPath
	for _, block := range blocks {
		out = append(out, checkPathsInTOML(block)...)
	}
	return out, nil
}

// TestScanDocCheckPaths pins the scanner: every authored shape of a check
// path declaration is found with its line, non-check tables and non-TOML
// context are ignored, and an unterminated toml fence is an error.
func TestScanDocCheckPaths(t *testing.T) {
	cases := []struct {
		name    string
		doc     string
		want    []docCheckPath
		wantErr bool
	}{
		{
			name: "steps check table",
			doc: "Intro prose.\n\n```toml\n" +
				"[[steps]]\nid = \"verify\"\n\n" +
				"[steps.check.check]\nmode = \"exec\"\npath = \".gc/scripts/checks/x.sh\"\n```\n",
			want: []docCheckPath{{path: ".gc/scripts/checks/x.sh", line: 9}},
		},
		{
			name: "template check table",
			doc: "```toml\n" +
				"[template.check.check]\nmode = \"exec\"\npath = \".gc/scripts/checks/x.sh\"\n```\n",
			want: []docCheckPath{{path: ".gc/scripts/checks/x.sh", line: 4}},
		},
		{
			name: "inline check table",
			doc: "```toml\n" +
				"[steps.check]\ncheck = { mode = \"exec\", path = \".gc/x.sh\" }\n```\n",
			want: []docCheckPath{{path: ".gc/x.sh", line: 3}},
		},
		{
			name: "dotted check key",
			doc: "```toml\n" +
				"[steps.check]\ncheck.path = \".gc/x.sh\"\n```\n",
			want: []docCheckPath{{path: ".gc/x.sh", line: 3}},
		},
		{
			name: "dot-slash gc path cleans to runtime state",
			doc: "```toml\n" +
				"[steps.check.check]\npath = \"./.gc/scripts/x.sh\"\n```\n",
			want: []docCheckPath{{path: "./.gc/scripts/x.sh", line: 3}},
		},
		{
			name: "asset form is scanned but not runtime state",
			doc: "```toml\n" +
				"[steps.check.check]\npath = \"../assets/scripts/checks/x.sh\"\n```\n",
			want: []docCheckPath{{path: "../assets/scripts/checks/x.sh", line: 3}},
		},
		{
			name: "workdir-relative and absolute paths",
			doc: "```toml\n" +
				"[steps.check.check]\npath = \"scripts/verify.sh\"\n\n" +
				"[steps.check.check]\npath = \"/opt/checks/x.sh\"\n```\n",
			want: []docCheckPath{{path: "scripts/verify.sh", line: 3}, {path: "/opt/checks/x.sh", line: 6}},
		},
		{
			name: "path under non-check table ignored",
			doc: "```toml\n" +
				"[agent]\npath = \".gc/x\"\n```\n",
		},
		{
			name: "shell fence ignored",
			doc: "```sh\n" +
				"path = \".gc/scripts/x.sh\"\n```\n",
		},
		{
			name: "prose ignored",
			doc:  "A check may declare `path = \".gc/scripts/x.sh\"` in prose.\n",
		},
		{
			name:    "unterminated toml fence errors",
			doc:     "```toml\n[steps.check.check]\npath = \".gc/x.sh\"\n",
			wantErr: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := scanDocCheckPaths(tc.doc)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("scanDocCheckPaths(%q) = %+v, want error", tc.doc, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("scanDocCheckPaths(%q): %v", tc.doc, err)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("scanDocCheckPaths(%q) = %+v, want %+v", tc.doc, got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("match %d = %+v, want %+v", i, got[i], tc.want[i])
				}
			}
		})
	}
}

// TestDocsCheckPathsAvoidRuntimeStateDir fails when any docs/ TOML example
// declares a check script under .gc/ — runtime state no formula layer ships,
// so a formula copied from the example can never launch its check (#4239).
// Check scripts belong in a layer's assets/ and are declared as
// "../assets/scripts/checks/<name>" (formula-spec-v2 §3.1).
func TestDocsCheckPathsAvoidRuntimeStateDir(t *testing.T) {
	root := repoRoot()
	docsRoot := filepath.Join(root, "docs")
	files, err := collectDocsMarkdownFiles(docsRoot)
	if err != nil {
		t.Fatalf("collecting docs markdown: %v", err)
	}

	var findings []string
	for _, rel := range files {
		data, err := os.ReadFile(filepath.Join(docsRoot, filepath.FromSlash(rel)))
		if err != nil {
			t.Fatalf("reading docs/%s: %v", rel, err)
		}
		paths, err := scanDocCheckPaths(string(data))
		if err != nil {
			t.Errorf("docs/%s: %v", rel, err)
			continue
		}
		for _, cp := range paths {
			if isRuntimeStateCheckPath(cp.path) {
				findings = append(findings, fmt.Sprintf("docs/%s:%d: %s", rel, cp.line, cp.path))
			}
		}
	}

	if len(findings) > 0 {
		t.Errorf(`docs declare check scripts under .gc/ — runtime state no layer ships (#4239):

A check path under .gc/ never resolves: Gas City does not stage pack assets
into .gc/, so the check is held open as unlaunchable. Point examples at a
formula-layer asset instead: path = "../assets/scripts/checks/<name>" (see
formula-spec-v2 §3.1, "Where the check script lives").

Offending declarations:`)
		for _, f := range findings {
			t.Errorf("  %s", f)
		}
	}
}
