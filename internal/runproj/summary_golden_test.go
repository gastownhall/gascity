package runproj

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

// TestBuildRunSummaryGolden pins the Go port of buildRunSummary to the
// TypeScript oracle: it loads the shared bead fixture, builds the summary, and
// asserts the canonical JSON matches runsummary_golden.json byte-for-byte. The
// golden was generated with JSON.stringify(value, null, 2) plus a trailing
// newline, so the canonicalization here mirrors that exactly (HTML escaping off,
// two-space indent, trailing newline).
func TestBuildRunSummaryGolden(t *testing.T) {
	fixturePath := filepath.Join("testdata", "beads_fixture.json")
	goldenPath := filepath.Join("testdata", "runsummary_golden.json")

	raw, err := os.ReadFile(fixturePath)
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	var beadList []beads.Bead
	if err := json.Unmarshal(raw, &beadList); err != nil {
		t.Fatalf("unmarshal fixture: %v", err)
	}

	summary := BuildRunSummary(beadList)
	got, err := canonicalJSON(summary)
	if err != nil {
		t.Fatalf("marshal summary: %v", err)
	}

	want, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}

	if !bytes.Equal(got, want) {
		t.Errorf("run summary does not match golden:\n%s", unifiedDiff(string(want), string(got)))
	}
}

// canonicalJSON marshals v the way the TS golden generator did: JSON.stringify
// with two-space indent, HTML escaping disabled (JSON.stringify does not escape
// <, >, & or U+2028/U+2029 the way Go's default encoder does), and a single
// trailing newline.
func canonicalJSON(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	// json.Encoder.Encode already appends a single trailing newline, matching the
	// generator's `JSON.stringify(...) + "\n"`.
	return buf.Bytes(), nil
}

// unifiedDiff renders a minimal line-oriented diff of want vs got so a golden
// mismatch points straight at the divergent lines.
func unifiedDiff(want, got string) string {
	wantLines := splitLines(want)
	gotLines := splitLines(got)
	var b bytes.Buffer
	n := len(wantLines)
	if len(gotLines) > n {
		n = len(gotLines)
	}
	for i := 0; i < n; i++ {
		var w, g string
		if i < len(wantLines) {
			w = wantLines[i]
		}
		if i < len(gotLines) {
			g = gotLines[i]
		}
		if w == g {
			continue
		}
		if i < len(wantLines) {
			b.WriteString("- " + w + "\n")
		}
		if i < len(gotLines) {
			b.WriteString("+ " + g + "\n")
		}
	}
	if b.Len() == 0 {
		return "(no line-level differences; check trailing bytes)"
	}
	return b.String()
}

func splitLines(s string) []string {
	return strings.Split(s, "\n")
}
