package main

import (
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
)

// revisionOrderingComparison matches an identifier ending in Revision compared
// against a numeric literal with an ORDERING operator — the shape every fence
// site had drifted into.
var revisionOrderingComparison = regexp.MustCompile(`[A-Za-z_.]*[Rr]evision\s*(<=|>=|<|>)\s*-?[0-9]`)

// revisionArithmetic matches an identifier ending in Revision combined with a
// numeric literal — `preRebindPersisted.Revision + 1` and friends. `revision++`
// on an in-process generation counter does NOT match: the operator must be
// followed by a number, so a bare increment is out of scope by construction.
var revisionArithmetic = regexp.MustCompile(`[A-Za-z_.]*[Rr]evision[A-Za-z0-9_]*\s*[-+*/]\s*-?[0-9]`)

// revisionVersusRevision matches two Revision-named identifiers on either side
// of an ordering operator. Paired with storeRevisionField it fires only when one
// side is a STORE token — the exact `.Revision` field on a bead or persisted
// response. cmd/gc's own generation counters (poolMembershipIndex.revision,
// lease.MembershipRevision) are minted in-process, monotonic by construction,
// and legitimately ordered; neither spells `.Revision`.
var revisionVersusRevision = regexp.MustCompile(`[A-Za-z_.]*[Rr]evision[A-Za-z0-9_]*\s*(<=|>=|<|>)\s*[A-Za-z_.]*[Rr]evision[A-Za-z0-9_]*`)

// storeRevisionField matches the store-token spelling `x.Revision`.
var storeRevisionField = regexp.MustCompile(`\.Revision\b`)

// codeBeforeLineComment drops a trailing `//` comment so the census reads code
// and not prose. Without it every needle fires on the doc comment that explains
// the rule, and the only way to document a forbidden shape would be to avoid
// naming it. Quotes and backticks are tracked so a `//` inside a string literal
// is not mistaken for a comment; `/*…*/` is not handled because the shapes here
// are single-expression and no in-scope file uses block comments for code.
func codeBeforeLineComment(line string) string {
	var quote rune
	for i, r := range line {
		switch {
		case quote != 0:
			if r == quote && (i == 0 || line[i-1] != '\\') {
				quote = 0
			}
		case r == '"' || r == '\'' || r == '`':
			quote = r
		case r == '/' && strings.HasPrefix(line[i:], "//"):
			return line[:i]
		}
	}
	return line
}

// TestRevisionConsumersNeverOrderARevision is the standing guard for
// ga-f7v2ft.140 and .141. The revision contract on beads.ConditionalWriter says
// a revision is an opaque token callers may test only for EQUALITY, with zero as
// the "unavailable" sentinel; ordering is undefined. Two independent fence sites
// nevertheless drifted to `> 0` / `<= 0`, and because bd hands out SIGNED
// revisions each one silently misclassified the negative half of every city's
// rows — one failing closed (the advisory status heal, the trigger rebind, the
// recovery lease) and one failing open (the pre-wake incarnation commit, whose
// CAS simply never ran). Both defects were invisible to every existing test
// because the native Mem/File stores mint small positive counters.
//
// Revision CONSUMERS are scanned; the stores that MINT revisions
// (internal/beads) legitimately order their own tokens. Use beads.RevisionKnown
// for the known/unavailable question and plain equality for everything else.
//
// ga-f7v2ft.144 widened it to the other two shapes the same contract forbids.
// The routed-work pool reuse path predicted its own post-write revision as
// `preRebindPersisted.Revision + 1` and refused any re-read that disagreed —
// which on bd is every re-read, because bd mints signed row_lock tokens, not
// counters. Predicting a token is arithmetic; the guard around the prediction
// (`expected <= previous`) ordered two revisions against each other. The
// replacement is the shape the recovery paths already use: commit, re-read, and
// carry whatever token the store minted, proving exactness from the committed
// IMAGE rather than from the token's value.
//
// Three needles, one rule: a store revision may be tested for equality with
// another store revision, and for known/unavailable via beads.RevisionKnown.
// Nothing else.
func TestRevisionConsumersNeverOrderARevision(t *testing.T) {
	_, currentFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	cmdGC := filepath.Dir(currentFile)
	repoRoot := filepath.Dir(filepath.Dir(cmdGC))
	for _, dir := range []string{cmdGC, filepath.Join(repoRoot, "internal", "session")} {
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatalf("ReadDir(%q): %v", dir, err)
		}
		for _, entry := range entries {
			name := entry.Name()
			if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
				continue
			}
			path := filepath.Join(dir, name)
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("ReadFile(%q): %v", path, err)
			}
			for i, raw := range strings.Split(string(data), "\n") {
				line := codeBeforeLineComment(raw)
				if match := revisionOrderingComparison.FindString(line); match != "" {
					t.Errorf("%s:%d orders a revision (%q): revisions are opaque and signed — test beads.RevisionKnown for known/unavailable and equality for everything else",
						path, i+1, match)
				}
				if match := revisionArithmetic.FindString(line); match != "" {
					t.Errorf("%s:%d does arithmetic on a revision (%q): a revision is an opaque token, not a counter — re-read the row and carry the revision the store minted instead of deriving one",
						path, i+1, match)
				}
				if match := revisionVersusRevision.FindString(line); match != "" && storeRevisionField.MatchString(match) {
					t.Errorf("%s:%d orders one revision against another (%q): ordering across revisions is undefined — compare them for equality",
						path, i+1, match)
				}
			}
		}
	}
}
