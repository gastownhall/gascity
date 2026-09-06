package agent

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestIdentitySeparatorContractDocExists guards the acceptance criteria for
// docs/reference/specs/identity-separator-contract-v1.md: the doc must exist
// and must document the separator alphabet, the two-axis encoding table, the
// minting-vs-comparing split between gascity and beads, the non-collapsing
// requirement for a canonicalizer, and a citation back to this package's
// replacers as the source of truth.
func TestIdentitySeparatorContractDocExists(t *testing.T) {
	repoRoot := repoRootFromCaller(t)

	docPath := filepath.Join(repoRoot, "docs", "reference", "specs", "identity-separator-contract-v1.md")
	docBytes, err := os.ReadFile(docPath)
	if err != nil {
		t.Fatalf("identity-separator-contract-v1.md must exist: %v", err)
	}
	doc := string(docBytes)

	requiredDocSubstrings := []string{
		"`/`",
		"`--`",
		"`.`",
		"`__`",
		"never part of a name",
		"gascity",
		"beads",
		"MUST NOT collapse",
		"internal/agent/session_name.go",
		"sessionNameQualifiedReplacer",
		"sessionNameQualifiedReverseReplacer",
		"best-effort",
		"rig--builder--1",
	}
	for _, want := range requiredDocSubstrings {
		if !strings.Contains(doc, want) {
			t.Errorf("identity-separator-contract-v1.md missing required content: %q", want)
		}
	}
}

// TestSessionNameCommentReferencesIdentitySeparatorContract guards the
// acceptance criterion that this file's comment points readers at the
// written contract instead of leaving the encoding rule undocumented here.
func TestSessionNameCommentReferencesIdentitySeparatorContract(t *testing.T) {
	repoRoot := repoRootFromCaller(t)

	srcPath := filepath.Join(repoRoot, "internal", "agent", "session_name.go")
	srcBytes, err := os.ReadFile(srcPath)
	if err != nil {
		t.Fatalf("reading session_name.go: %v", err)
	}

	const wantRef = "docs/reference/specs/identity-separator-contract-v1.md"
	if !strings.Contains(string(srcBytes), wantRef) {
		t.Errorf("session_name.go comment must reference %s", wantRef)
	}
}

func repoRootFromCaller(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	return filepath.Join(filepath.Dir(thisFile), "..", "..")
}
