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
	seenRefused := map[string]bool{}
	rows := readShippedInvocations(t)
	n := len(rows)
	for _, row := range rows {
		key := strings.Join(row.Argv, " ")
		for _, inv := range bdflags.Inventories() {
			refusal := shippedInvocationSplitRefusal(t, inv, row.Argv)
			refused := refusal != ""
			if _, known := bdSplitKnownRefusedShipped[key]; known {
				seenRefused[key] = true
				if !refused {
					t.Errorf("%s: %q (%s) is listed as knowingly refused but was allowed", inv.Name, key, row.Src)
				}
				continue
			}
			if refused {
				t.Errorf("%s: shipped invocation %q (%s) is refused on a split city: %s", inv.Name, key, row.Src, refusal)
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

// shippedInvocationRow is one pinned gc bd invocation and the source line it
// was scanned from.
type shippedInvocationRow struct {
	Argv []string `json:"argv"`
	Src  string   `json:"src"`
}

// readShippedInvocations reads testdata/gc_bd_shipped_invocations.jsonl.
func readShippedInvocations(t *testing.T) []shippedInvocationRow {
	t.Helper()
	f, err := os.Open("testdata/gc_bd_shipped_invocations.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close() //nolint:errcheck // test read
	var rows []shippedInvocationRow
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var row shippedInvocationRow
		if err := json.Unmarshal(sc.Bytes(), &row); err != nil {
			t.Fatal(err)
		}
		rows = append(rows, row)
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	return rows
}

// shippedInvocationSplitRefusal runs argv through the split-city guard on the
// pinned bd inventory and returns its refusal, or "" when it is allowed.
func shippedInvocationSplitRefusal(t *testing.T, inv *bdflags.Inventory, argv []string) string {
	t.Helper()
	deps := splitTestDeps(strings.NewReader(""), func(id string) (beads.Bead, bdReadResult) {
		return beads.Bead{ID: id, Type: "task"}, bdReadFound
	})
	deps.inventory = func() (*bdflags.Inventory, error) { return inv, nil }
	g := bdClassFlipGuard(splitGuardTestCity(), t.TempDir(), argv, deps)
	// A file the invocation names (--metadata @file, delete --from-file,
	// batch -f) does not exist in this test's scope: that refusal is the
	// test's, not the allowlist's.
	if strings.Contains(g.refusal, "no such file or directory") {
		return ""
	}
	return g.refusal
}

// TestGcBdShippedSpawnStormListIsUnassignedAndAllowed pins the core
// spawn-storm-detect sweep: it lists unassigned beads with --no-assignee (an
// empty --assignee filters nothing), and the split-city guard allows that
// argv on both pinned bd builds. The sweep fails open when the list call is
// refused, so a refusal would silently disable the detector.
func TestGcBdShippedSpawnStormListIsUnassignedAndAllowed(t *testing.T) {
	const src = "gascity:internal/bootstrap/packs/core/assets/scripts/spawn-storm-detect.sh:"
	want := []string{"list", "--status=open", "--no-assignee", "--json", "--limit=0"}
	var got [][]string
	for _, row := range readShippedInvocations(t) {
		if strings.HasPrefix(row.Src, src) {
			got = append(got, row.Argv)
		}
	}
	if len(got) != 1 || strings.Join(got[0], " ") != strings.Join(want, " ") {
		t.Fatalf("spawn-storm-detect.sh census rows = %q, want exactly [%q]", got, want)
	}
	for _, inv := range bdflags.Inventories() {
		if refusal := shippedInvocationSplitRefusal(t, inv, want); refusal != "" {
			t.Errorf("%s: %q is refused on a split city: %s", inv.Name, strings.Join(want, " "), refusal)
		}
	}
}
