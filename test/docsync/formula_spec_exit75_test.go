package docsync

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// formulaSpecV2Doc is the formulas v2 specification whose section 3.1 defines
// the check-script exit-code contract.
const formulaSpecV2Doc = "docs/reference/specs/formula-spec-v2.md"

func readFormulaSpecV2(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(repoRoot(), formulaSpecV2Doc))
	if err != nil {
		t.Fatalf("reading %s: %v", formulaSpecV2Doc, err)
	}
	return string(data)
}

// TestFormulaSpecStderrFallbackRowIsMarkedInternal guards the section 3.1
// exit-code table: the stderr-fallback row must say, in the row itself, that
// it carries no compatibility guarantee. A gate-script author who reads only
// the table must not take it for as durable as the exit-75 row.
func TestFormulaSpecStderrFallbackRowIsMarkedInternal(t *testing.T) {
	const rowPrefix = "| any other nonzero carrying a typed infrastructure string on stderr"
	for _, line := range strings.Split(readFormulaSpecV2(t), "\n") {
		if !strings.HasPrefix(line, rowPrefix) {
			continue
		}
		lower := strings.ToLower(line)
		for _, want := range []string{"internal fallback", "not a compatibility guarantee"} {
			if !strings.Contains(lower, want) {
				t.Errorf("%s: stderr-fallback exit-code row does not contain %q: %s", formulaSpecV2Doc, want, line)
			}
		}
		return
	}
	t.Fatalf("%s: exit-code table has no row starting %q", formulaSpecV2Doc, rowPrefix)
}

// TestFormulaSpecExit75IsPartOfCompilerContract guards the Deprecated-surfaces
// section: it must say that changing or retiring exit 75's meaning is a
// breaking formula_compiler contract change, handled like every other entry
// there (a new contract version plus a table entry), not a silent edit.
func TestFormulaSpecExit75IsPartOfCompilerContract(t *testing.T) {
	doc := readFormulaSpecV2(t)
	const heading = "### Deprecated surfaces"
	_, section, found := strings.Cut(doc, heading)
	if !found {
		t.Fatalf("%s: no %q section", formulaSpecV2Doc, heading)
	}
	lower := strings.ToLower(section)
	for _, want := range []string{"exit 75", "breaking", "contract version"} {
		if !strings.Contains(lower, want) {
			t.Errorf("%s: %q section does not contain %q; exit 75's meaning must be folded into the formula_compiler contract-change rule", formulaSpecV2Doc, heading, want)
		}
	}
}
