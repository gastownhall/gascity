package beads

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Admission order is expressed in several dialects (a Go comparator, a bd
// --sort flag, a jq sort_by key). They only stay in lockstep if every one of
// them derives from AdmissionPolicy. This guard fails when a consumer package
// spells a dialect literally instead of asking the policy for it — the drift
// that makes one admission path disagree with the rest.
//
// The policy's own package is the sanctioned home for these spellings.
func TestConsumersDoNotHardcodeAdmissionOrder(t *testing.T) {
	forbidden := map[string]string{
		`--sort hybrid`:      "use AdmissionPolicy.BdSortFlag()",
		`--sort oldest`:      "use AdmissionPolicy.BdSortFlag()",
		`sort_by((.priority`: "use AdmissionPolicy.JQSortKey()",
		`beads.ReadyLess(`:   "use beads.LessFunc(policy)",
		`beads.FIFOLess(`:    "use beads.LessFunc(policy)",
	}
	// Scanned because they own the admission paths this bead unified.
	scanned := []string{
		filepath.Join("..", "..", "cmd", "gc"),
		filepath.Join("..", "..", "internal", "config"),
	}

	for _, dir := range scanned {
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatalf("reading %s: %v", dir, err)
		}
		for _, e := range entries {
			name := e.Name()
			if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
				continue
			}
			path := filepath.Join(dir, name)
			src, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("reading %s: %v", path, err)
			}
			for _, line := range strings.Split(string(src), "\n") {
				if strings.Contains(line, "nolint:admissionorder") {
					continue
				}
				for literal, fix := range forbidden {
					if strings.Contains(line, literal) {
						t.Errorf("%s hardcodes admission order %q — %s\n  %s",
							path, literal, fix, strings.TrimSpace(line))
					}
				}
			}
		}
	}
}
