package scripts_test

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The merge-integrity guard's CI-visible half (ga-f7v2ft.175).
//
// scripts/check-merge-integrity.sh is the guard itself: it needs three git
// trees and a merge base, so its functional falsification lives in its own
// `--self-test` (sixteen cases on real temp git repos) and in the historical
// run at 5ec88b1535, where check 1 reports exactly the ga-f7v2ft.167
// resurrection and check 3 reports the ga-f7v2ft.184 island.
//
// THIS FILE DELIBERATELY SPAWNS NOTHING. The test-resource census
// (internal/testpolicy/resourcecensus) ratchets every os/exec call site in a
// tracked test file against a checked baseline in all three of its scopes, so a
// new `exec.Command` here is a policy change to census.go, test-resources.toml
// and the TESTING.md ledger — not a thing to slip in beside a guard. What is
// left is what can be checked without a subprocess, and it is the half that
// actually rots: the allowlist's contract and the wiring that makes anyone run
// the guard at all.
//
// A guard nobody invokes and an allowlist that silently swallows a malformed
// line are the two ways this protection dies quietly. Both fail here.

const (
	mergeIntegrityScript    = "check-merge-integrity.sh"
	mergeIntegrityAllowlist = "merge-integrity-allow.txt"
	mergeIntegrityExtractor = "lib/go-top-level-symbols.awk"
	mergeIntegrityTarget    = "check-merge-integrity:"
	mergeIntegrityDoc       = "TESTING.md"
)

type mergeIntegrityEntry struct {
	Kind   string
	Name   string
	Reason string
	Line   int
}

// parseMergeIntegrityAllowlist mirrors read_allowlist() in the shell guard:
// tab-separated kind/name/reason, blank lines and # comments ignored, and any
// other shape is a violation rather than a skipped line.
func parseMergeIntegrityAllowlist(t *testing.T, path string) []mergeIntegrityEntry {
	t.Helper()
	file, err := os.Open(path) //nolint:gosec // fixed repo-relative path
	if err != nil {
		t.Fatalf("opening %s: %v", path, err)
	}
	defer file.Close() //nolint:errcheck // read-only

	var entries []mergeIntegrityEntry
	scanner := bufio.NewScanner(file)
	for line := 1; scanner.Scan(); line++ {
		text := scanner.Text()
		if strings.TrimSpace(text) == "" || strings.HasPrefix(strings.TrimSpace(text), "#") {
			continue
		}
		parts := strings.SplitN(text, "\t", 3)
		if len(parts) < 3 {
			t.Errorf("%s line %d is malformed: want <kind>\\t<name>\\t<reason>, got %q\n"+
				"The shell guard fails closed on this line; so does every reader of it.", path, line, text)
			continue
		}
		entries = append(entries, mergeIntegrityEntry{
			Kind:   strings.TrimSpace(parts[0]),
			Name:   strings.TrimSpace(parts[1]),
			Reason: strings.TrimSpace(parts[2]),
			Line:   line,
		})
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	return entries
}

// TestMergeIntegrityAllowlistEntriesCarryAReason enforces the contract that
// makes the allowlist a record rather than a mute button. Every entry names a
// known kind, an identifier, and a reason — and no name appears twice, because
// two entries for one symbol means one of the reasons is already wrong.
func TestMergeIntegrityAllowlistEntriesCarryAReason(t *testing.T) {
	root := repoRoot(t)
	path := filepath.Join(root, "scripts", mergeIntegrityAllowlist)
	entries := parseMergeIntegrityAllowlist(t, path)

	seen := map[string]int{}
	for _, entry := range entries {
		if entry.Kind != "symbol" && entry.Kind != "test" && entry.Kind != "restored" {
			t.Errorf("%s line %d: unknown kind %q, want symbol, test or restored", path, entry.Line, entry.Kind)
		}
		if entry.Name == "" {
			t.Errorf("%s line %d: entry has no name", path, entry.Line)
		}
		if entry.Reason == "" {
			t.Errorf("%s line %d: entry %q has no reason.\n"+
				"An allowlist without reasons is a mute button; the guard exists so a merge cannot go quiet.",
				path, entry.Line, entry.Name)
		}
		key := entry.Kind + "/" + entry.Name
		if prior, dup := seen[key]; dup {
			t.Errorf("%s line %d: %s is already allowed on line %d; one of the two reasons is stale",
				path, entry.Line, key, prior)
		}
		seen[key] = entry.Line
	}
}

// TestMergeIntegrityGuardStaysWired pins the path from "someone runs make" to
// "the guard proves its own bite and then audits the tree". Each link has died
// on its own in this repository's history: a script with no target, a target
// that stopped running --self-test, a workflow doc that stopped naming the
// command.
func TestMergeIntegrityGuardStaysWired(t *testing.T) {
	root := repoRoot(t)

	for _, rel := range []string{
		filepath.Join("scripts", mergeIntegrityScript),
		filepath.Join("scripts", mergeIntegrityAllowlist),
		filepath.Join("scripts", mergeIntegrityExtractor),
	} {
		if _, err := os.Stat(filepath.Join(root, rel)); err != nil {
			t.Fatalf("%s is missing: %v", rel, err)
		}
	}

	script := readRepoFile(t, root, filepath.Join("scripts", mergeIntegrityScript))
	if !strings.Contains(script, mergeIntegrityExtractor) {
		t.Errorf("%s no longer references %s; the symbol census has no extractor", mergeIntegrityScript, mergeIntegrityExtractor)
	}
	for _, needle := range []string{"DELETED-SYMBOL RESURRECTION", "VANISHED TESTS", "RESTORED LANE DELETION"} {
		if !strings.Contains(script, needle) {
			t.Errorf("%s no longer reports %q; one of the three checks has been dropped", mergeIntegrityScript, needle)
		}
	}

	makefile := readRepoFile(t, root, "Makefile")
	target, ok := makefileRecipe(makefile, mergeIntegrityTarget)
	if !ok {
		t.Fatalf("Makefile has no %s target; nothing invokes the guard", mergeIntegrityTarget)
	}
	if !strings.Contains(target, "--self-test") {
		t.Errorf("the %s recipe no longer runs --self-test first.\n"+
			"A guard that does not prove its bite before it passes is a guard nobody can trust when it passes.",
			mergeIntegrityTarget)
	}
	if strings.Count(target, mergeIntegrityScript) < 2 {
		t.Errorf("the %s recipe runs the self-test but not the audit (or the other way round)", mergeIntegrityTarget)
	}

	doc := readRepoFile(t, root, mergeIntegrityDoc)
	if !strings.Contains(doc, "make check-merge-integrity") {
		t.Errorf("%s no longer names `make check-merge-integrity`.\n"+
			"The merge workflow section is how the next origin/main sync learns to run it.", mergeIntegrityDoc)
	}
}

func readRepoFile(t *testing.T, root, rel string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root, rel)) //nolint:gosec // fixed repo-relative path
	if err != nil {
		t.Fatalf("reading %s: %v", rel, err)
	}
	return string(data)
}

// makefileRecipe returns the recipe lines that follow a target, which is where
// the make-level wiring lives.
func makefileRecipe(makefile, target string) (string, bool) {
	lines := strings.Split(makefile, "\n")
	for i, line := range lines {
		if !strings.HasPrefix(line, target) {
			continue
		}
		var recipe []string
		for _, next := range lines[i+1:] {
			if !strings.HasPrefix(next, "\t") {
				break
			}
			recipe = append(recipe, next)
		}
		return strings.Join(recipe, "\n"), true
	}
	return "", false
}
