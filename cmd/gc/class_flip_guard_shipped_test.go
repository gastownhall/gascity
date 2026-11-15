package main

import (
	"bufio"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/bdflags"
	"github.com/gastownhall/gascity/internal/beads"
)

// bdSplitKnownRefusedShipped are the shipped gc bd invocations a split city
// refuses on purpose, each with the alternative a reader of that source
// should use. Every other shipped invocation must be allowed.
var bdSplitKnownRefusedShipped = map[string]string{
	"backup restore": "the split-city restore path in journal-corruption-recovery.md",
	"backup restore /path/to/city/.dolt-backup/X --force": "the split-city restore path in journal-corruption-recovery.md",
	"init --db X":        "the corruption runbook's re-init: on a split city it follows the split-city procedure in docs/runbooks/split-storage-classes.md",
	"mol pour":           "prose in the gastown mayor prompt; gc sling / gc formula cook",
	"create --root-bead": "an invalid flag in gascity-packs build-basic/decompose.md: bd rejects it on every city",
	"list --root X":      "an invalid flag in gascity-packs superpowers confirm-spec-approval.md: bd rejects it on every city",
	"list --type=bug --status=open --search X": "an invalid flag in gastown mol-refinery-patrol.toml: bd rejects --search on every city",
}

// TestGcBdShippedInvocationsAreAllowedOnSplit runs every gc bd invocation
// scanned from the shipped packs, prompts, formulas and scripts (gascity's
// own, gascity-packs, the workflows repo — its Python argv builders read by
// AST — and the maintainer city's own orders, prompts and rig packs) through
// the split-city guard on both
// pinned bd builds. Each must be allowed, or be knowingly refused with a
// documented alternative. The list is pinned in
// testdata/gc_bd_shipped_invocations.jsonl; regenerate it with
// testdata/scan_gc_bd_invocations.py (see its header) when the sources change.
func TestGcBdShippedInvocationsAreAllowedOnSplit(t *testing.T) {
	f, err := os.Open("testdata/gc_bd_shipped_invocations.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close() //nolint:errcheck // test read
	seenRefused := map[string]bool{}
	n := 0
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var row struct {
			Argv []string `json:"argv"`
			Src  string   `json:"src"`
		}
		if err := json.Unmarshal(sc.Bytes(), &row); err != nil {
			t.Fatal(err)
		}
		n++
		key := strings.Join(row.Argv, " ")
		for _, inv := range bdflags.Inventories() {
			deps := splitTestDeps(strings.NewReader(""), func(id string) (beads.Bead, bdReadResult) {
				return beads.Bead{ID: id, Type: "task"}, bdReadFound
			})
			pinned := inv
			deps.inventory = func() (*bdflags.Inventory, error) { return pinned, nil }
			g := bdClassFlipGuard(splitGuardTestCity(), t.TempDir(), row.Argv, deps)
			// A file the invocation names (--metadata @file, delete --from-file,
			// batch -f) does not exist in this test's scope: that refusal is the
			// test's, not the allowlist's.
			refused := g.refusal != "" && !strings.Contains(g.refusal, "no such file or directory")
			if _, known := bdSplitKnownRefusedShipped[key]; known {
				seenRefused[key] = true
				if !refused {
					t.Errorf("%s: %q (%s) is listed as knowingly refused but was allowed", inv.Name, key, row.Src)
				}
				continue
			}
			if refused {
				t.Errorf("%s: shipped invocation %q (%s) is refused on a split city: %s", inv.Name, key, row.Src, g.refusal)
			}
		}
	}
	if n < 300 {
		t.Fatalf("only %d pinned invocations; the scan looks truncated", n)
	}
	for key := range bdSplitKnownRefusedShipped {
		if !seenRefused[key] {
			t.Errorf("knowingly-refused %q no longer appears in the pinned scan; drop it", key)
		}
	}
}
