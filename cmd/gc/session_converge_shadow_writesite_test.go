package main

import (
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
)

// convergeComparedKeyWriteSiteInventory is the PERMANENT writer inventory the
// S19 Stage 3 double-write safety review demanded (3b/3c). Every non-test cmd/gc
// file that writes a compared metadata key MUST appear here, and every file here
// must be wired into the in-process recorder — either by calling
// recordLegacyCompareWrites directly, or (for pure map-builders with no bead ID)
// by carrying the `convergecompare:recorded-by-caller` marker documenting the
// caller that records on its behalf.
//
// Adding a new writer of a compared key without registering it here fails
// TestConvergeCompareKeyWriteSitesWired. This is the enforcement described in the
// plan: "no non-test cmd/gc code may write a compared metadata key except via the
// recording wrapper."
var convergeComparedKeyWriteSiteInventory = map[string]string{
	"session_identity.go":           "desiredSessionIdentity builds the canonical stamp (pure); recorded by callers (adoptionBarrier.create, syncSessionBeads.create)",
	"session_name_lookup.go":        "pool-create canonical stamp; recorded via recordLegacyCompareWrites(poolSessionCreate)",
	"session_reconcile.go":          "healStatePatchWithRollback builds priming clears; recorded via recordLegacyCompareWrites(healStateWithRollback) at the ApplyPatch site",
	"session_beads.go":              "syncSessionBeads reclaim priming clears + create canonical stamp + named-session retire canonical clears; recorded via recordLegacyCompareWrites",
	"session_lifecycle_parallel.go": "clearStaleResumeKeyMetadata priming clears; recorded via recordLegacyCompareWrites(clearStaleResumeKeyMetadata)",
	"session_converge_shadow.go":    "the recorder + owned-key oracle itself (applyDerivedToOwnedKeys writes a local prediction map, not a store)",
}

// comparedKeyConstantNames are the metadata-key CONSTANT identifiers whose map
// assignment counts as a compared-key write, regardless of package qualifier.
var comparedKeyConstantNames = []string{
	"CanonicalInstanceNameMetadata",
	"CanonicalPoolSlotMetadata",
	"PrimedAtMetadataKey",
	"PrimingAttemptedAtMetadataKey",
	"PromptHashMetadataKey",
}

// comparedKeyStringLiterals are the raw string values of the compared keys, in
// case a site writes the literal rather than the constant.
var comparedKeyStringLiterals = []string{
	`"canonical_instance_name"`,
	`"canonical_pool_slot"`,
	`"primed_at"`,
	`"priming_attempted_at"`,
	`"prompt_hash"`,
}

// TestConvergeCompareKeyWriteSitesWired is the write-site-completeness guard, in
// the TestGCNonTestFilesStayOnWorkerBoundary style. It asserts:
//
//  1. every non-test cmd/gc file that writes a compared key is registered in the
//     inventory (a new, unregistered writer fails the build); and
//  2. every registered writer is wired into the recorder (calls
//     recordLegacyCompareWrites, or carries the recorded-by-caller marker); and
//  3. the inventory carries no stale entries (a file that no longer writes any
//     compared key must be removed).
func TestConvergeCompareKeyWriteSitesWired(t *testing.T) {
	_, currentFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	dir := filepath.Dir(currentFile)
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir(%q): %v", dir, err)
	}

	// A compared-key write is an assignment of the form `[<key-ref>] =` (not `==`).
	// Build one matcher per key reference.
	var writeMatchers []*regexp.Regexp
	for _, name := range comparedKeyConstantNames {
		// Index assignment: [session.PrimedAtMetadataKey] =  /  [PrimedAtMetadataKey] =
		writeMatchers = append(writeMatchers, regexp.MustCompile(`\[[A-Za-z0-9_]*\.?`+regexp.QuoteMeta(name)+`\]\s*=[^=]`))
		// Map-literal key: sessionpkg.PrimedAtMetadataKey:  (a write via composite literal)
		writeMatchers = append(writeMatchers, regexp.MustCompile(`[A-Za-z0-9_]+\.`+regexp.QuoteMeta(name)+`\s*:`))
	}
	for _, lit := range comparedKeyStringLiterals {
		// Only the index-assignment literal form ["primed_at"] = is matched; a
		// string-literal-as-map-key colon matcher would false-match doc comments
		// like `// primedAt mirrors "primed_at": ...`. cmd/gc writes these keys via
		// the exported constants, not string literals, so this stays a safety net.
		writeMatchers = append(writeMatchers, regexp.MustCompile(`\[`+regexp.QuoteMeta(lit)+`\]\s*=[^=]`))
	}

	writersFound := map[string]bool{}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("ReadFile(%q): %v", name, err)
		}
		content := string(data)
		writes := false
		for _, m := range writeMatchers {
			if m.MatchString(content) {
				writes = true
				break
			}
		}
		if !writes {
			continue
		}
		writersFound[name] = true

		if _, registered := convergeComparedKeyWriteSiteInventory[name]; !registered {
			t.Errorf("%s writes a compared metadata key but is NOT registered in convergeComparedKeyWriteSiteInventory — register it and wire it into recordLegacyCompareWrites", name)
			continue
		}
		if !strings.Contains(content, "recordLegacyCompareWrites") &&
			!strings.Contains(content, "convergecompare:recorded-by-caller") {
			t.Errorf("%s is a registered compared-key writer but is not wired into the recorder (missing recordLegacyCompareWrites call or convergecompare:recorded-by-caller marker)", name)
		}
	}

	// Stale-entry guard: every inventory entry must still be a live writer.
	for name := range convergeComparedKeyWriteSiteInventory {
		if !writersFound[name] {
			t.Errorf("inventory entry %q no longer writes a compared metadata key — remove the stale entry", name)
		}
	}
}
