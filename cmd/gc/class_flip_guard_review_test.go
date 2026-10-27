//go:build !windows

package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/bdflags"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

// guardCase is one argv through bdClassFlipGuard on a split city.
type guardCase struct {
	name   string
	argv   []string
	before beads.Bead
	read   bdReadResult
	refuse bool
	want   string // a fragment the refusal must carry, when refused
	// enterprise judges against the bd 1.3.0 Enterprise inventory instead
	// of bd 1.3.2.
	enterprise bool
}

// bdEnterpriseInventory is the pinned Enterprise build's inventory id.
const bdEnterpriseInventory = "bd-1.3.0-enterprise-77612097cec0"

func runGuardCases(t *testing.T, cases []guardCase) {
	t.Helper()
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			deps := splitTestDeps(nil, func(string) (beads.Bead, bdReadResult) { return c.before, c.read })
			if c.enterprise {
				deps.inventory = func() (*bdflags.Inventory, error) { return bdflags.InventoryByName(bdEnterpriseInventory), nil }
			}
			g := bdClassFlipGuard(splitGuardTestCity(), t.TempDir(), c.argv, deps)
			if c.refuse != (g.refusal != "") {
				t.Fatalf("gc bd %s: refused=%v, want %v; refusal=%q", strings.Join(c.argv, " "), g.refusal != "", c.refuse, g.refusal)
			}
			if c.refuse && !strings.Contains(g.refusal, c.want) {
				t.Errorf("refusal does not carry %q: %q", c.want, g.refusal)
			}
			if c.refuse && !strings.Contains(g.refusal, "Nothing was written") {
				t.Errorf("refusal does not say nothing was written: %q", g.refusal)
			}
			// The same argv on a single-store city is never refused.
			if single := bdClassFlipGuard(&config.City{}, t.TempDir(), c.argv, deps); single.refusal != "" {
				t.Errorf("single-store city refused: %q", single.refusal)
			}
		})
	}
}

var guardTask = beads.Bead{ID: "demo-1", Type: "task"}

// TestGcBdSplitRegressionsFromReview are the seven bypasses the #8333
// re-review reproduced against real bd 1.3.2 and 1.3.0 Enterprise, each now
// refused by the allowlist.
func TestGcBdSplitRegressionsFromReview(t *testing.T) {
	runGuardCases(t, []guardCase{
		// 1. A short id bd resolves to a different (infra-shaped or
		// marker-masking) bead: ErrIDCollision is a refusal, never a not-found.
		{"1 short-id collision", []string{"update", "demo-ab", "--unset-metadata", "gc.synthetic"}, beads.Bead{}, bdReadCollision, true, "resolves \"demo-ab\" to a different bead", false},
		{"1 short-id collision, marker add", []string{"update", "demo-ab", "--add-label", "triage,wisp"}, beads.Bead{}, bdReadCollision, true, "different bead", false},
		// 2. A type change after a failed read: a synthetic convoy carrying
		// gc.root_bead_id is work only while it stays a convoy.
		{"2 type change after a failed read", []string{"update", "demo-2", "--type", "task"}, beads.Bead{}, bdReadFailed, true, "could not read", false},
		{"2 type change on an absent bead", []string{"update", "demo-2", "--type", "task"}, beads.Bead{}, bdReadNotFound, true, "no bead", false},
		// 3. An alias bd runs as a refused verb.
		{"3 mol fart", []string{"mol", "fart", "proto-a", "proto-b"}, beads.Bead{}, bdReadNotFound, true, "alias of mol bond", false},
		{"3 protomolecule pour", []string{"protomolecule", "pour", "f"}, beads.Bead{}, bdReadNotFound, true, "alias of mol", false},
		// 4. Flag values that spell --dry-run, and bools spelled =true: argv is
		// parsed as pflag parses it, and the verbs are refused regardless.
		{"4 --assignee --dry-run", []string{"mol", "pour", "f", "--assignee", "--dry-run"}, beads.Bead{}, bdReadNotFound, true, "gc sling", false},
		{"4 --persist=true", []string{"cook", "f", "--persist=true"}, beads.Bead{}, bdReadNotFound, true, "--persist", false},
		{"4 --prefix --dry-run", []string{"cook", "f", "--prefix", "--dry-run", "--persist"}, beads.Bead{}, bdReadNotFound, true, "--persist", false},
		// 5. --var reaching an expansion template's labels: every step is only
		// instantiated without --root-only, which is refused.
		{"5 --var into an expansion template", []string{"mol", "wisp", "expanding-formula", "--var", "label=gc:session"}, beads.Bead{}, bdReadNotFound, true, "without --root-only", false},
		{"5 --root-only=false", []string{"mol", "wisp", "expanding-formula", "--root-only=false"}, beads.Bead{}, bdReadNotFound, true, "without --root-only", false},
		// 6. A bulk restore, with a bool spelled =true.
		{"6 backup restore --force=true", []string{"backup", "restore", "/b", "--force=true"}, beads.Bead{}, bdReadNotFound, true, "journal-corruption-recovery.md", false},
		// 7. A legacy proto's children: pour is refused, and a root-only wisp
		// of an infra-shaped proto is judged on the proto it clones.
		{"7 pour a proto", []string{"mol", "pour", "demo-proto"}, beads.Bead{ID: "demo-proto", Type: "molecule"}, bdReadFound, true, "gc sling", false},
		{"7 root-only wisp of an infra proto", []string{"mol", "wisp", "demo-proto", "--root-only"}, beads.Bead{ID: "demo-proto", Type: "molecule", Labels: []string{"gc:wisp"}}, bdReadFound, true, "graph-class", false},
		{"7 root-only wisp of a colliding proto id", []string{"mol", "wisp", "demo-pro", "--root-only"}, beads.Bead{}, bdReadCollision, true, "different bead", false},
	})
}

// TestGcBdSplitAllowsThePatrolShapes keeps the workflows that depend on the
// formula verbs working: the gastown patrols' root-only wisp and burn.
func TestGcBdSplitAllowsThePatrolShapes(t *testing.T) {
	runGuardCases(t, []guardCase{
		{"patrol wisp", []string{"mol", "wisp", "mol-refinery-patrol", "--root-only", "--var", "target_branch=main", "--var", "rig_name=r", "--var", "binding_prefix=gt", "--json"}, beads.Bead{}, bdReadNotFound, false, "", false},
		{"patrol wisp, root-only=true", []string{"mol", "wisp", "mol-witness-patrol", "--root-only=true", "--var=binding_prefix="}, beads.Bead{}, bdReadNotFound, false, "", false},
		{"wisp create", []string{"mol", "wisp", "create", "mol-boot-patrol", "--root-only"}, beads.Bead{}, bdReadNotFound, false, "", false},
		{"root-only wisp of a work proto", []string{"mol", "wisp", "demo-proto", "--root-only"}, beads.Bead{ID: "demo-proto", Type: "molecule", Labels: []string{"template"}}, bdReadFound, false, "", false},
		{"burn", []string{"mol", "burn", "tt-wisp-1", "--force"}, beads.Bead{ID: "tt-wisp-1", Type: "molecule"}, bdReadFound, false, "", false},
		{"burn of an id the store does not hold", []string{"mol", "burn", "tt-wisp-1", "--force"}, beads.Bead{}, bdReadNotFound, true, "no bead", false},
		{"deacon wisp gc", []string{"mol", "wisp", "gc", "--age", "24h"}, beads.Bead{}, bdReadNotFound, false, "", false},
		{"mol current", []string{"mol", "current", "--json"}, beads.Bead{}, bdReadNotFound, false, "", false},
		{"cook prints", []string{"cook", "f", "--prefix", "--dry-run"}, beads.Bead{}, bdReadNotFound, false, "", false},
		{"wisp --dry-run is not vetted", []string{"mol", "wisp", "f", "--root-only", "--dry-run"}, beads.Bead{}, bdReadNotFound, true, "--dry-run is not allowed", false},
		// A bool flag between verb and subverb: cobra and pflag both read
		// `mol wisp create f`, so it is judged as that.
		{"bool flag before a subverb", []string{"mol", "wisp", "--root-only", "create", "f"}, beads.Bead{}, bdReadNotFound, false, "", false},
	})
}

// TestGcBdSplitM7LabelOnlyInfraStep is mutant M7: a formula whose only infra
// marker is a step LABEL. Every path that would mint that step is refused; the
// root-only wisp mints only the root, which carries no step labels.
func TestGcBdSplitM7LabelOnlyInfraStep(t *testing.T) {
	runGuardCases(t, []guardCase{
		{"pour", []string{"mol", "pour", "label-only"}, beads.Bead{}, bdReadNotFound, true, "gc sling", false},
		{"wisp", []string{"mol", "wisp", "label-only"}, beads.Bead{}, bdReadNotFound, true, "without --root-only", false},
		{"bond", []string{"mol", "bond", "label-only", "demo-1"}, beads.Bead{}, bdReadNotFound, true, "gc sling", false},
		{"cook --persist", []string{"cook", "label-only", "--persist"}, beads.Bead{}, bdReadNotFound, true, "--persist", false},
		{"root-only wisp", []string{"mol", "wisp", "label-only", "--root-only"}, beads.Bead{}, bdReadNotFound, false, "", false},
	})
}

// TestGcBdSplitM11LabelAndMetadataOpOrder is mutant M11: bd applies label
// edits as replace, then add, then remove (issueops.ApplyLabelPatch), and
// metadata set before unset, so the guard must judge them in that order.
func TestGcBdSplitM11LabelAndMetadataOpOrder(t *testing.T) {
	wisp := beads.Bead{ID: "demo-1", Type: "task"}
	runGuardCases(t, []guardCase{
		{"add then remove the same label: removed", []string{"update", "demo-1", "--remove-label", "wisp", "--add-label", "wisp"}, wisp, bdReadFound, false, "", false},
		{"replace then add: added", []string{"update", "demo-1", "--add-label", "gc:session", "--set-labels", "triage"}, wisp, bdReadFound, true, "sessions-class", false},
		{"set then unset the same key: unset", []string{"update", "demo-1", "--unset-metadata", "gc.kind", "--set-metadata", "gc.kind=wisp"}, wisp, bdReadFound, false, "", false},
	})
}

// TestGcBdClassGuardParsesLabelsAsBdDoes: bd parses update's label flags and
// create's --labels as ONE CSV record, so a quoted label is the bare label.
// The label VERBS split on plain commas, so there a quoted label really is a
// different label.
func TestGcBdClassGuardParsesLabelsAsBdDoes(t *testing.T) {
	runGuardCases(t, []guardCase{
		{"quoted wisp", []string{"update", "demo-1", "--add-label", `"wisp"`}, guardTask, bdReadFound, true, "graph-class", false},
		{"quoted second label", []string{"update", "demo-1", "--add-label", `triage,"gc:session"`}, guardTask, bdReadFound, true, "sessions-class", false},
		{"quoted set-labels", []string{"update", "demo-1", "--set-labels", `"gc:nudge"`}, guardTask, bdReadFound, true, "nudges-class", false},
		{"enterprise -l", []string{"update", "demo-1", "-l", "gc:session"}, guardTask, bdReadFound, true, "sessions-class", true},
		{"-l is not a flag of bd 1.3.2 update", []string{"update", "demo-1", "-l", "gc:session"}, guardTask, bdReadFound, true, "bd would reject", false},
		{"combined shorthand", []string{"update", "demo-1", "-qtmessage"}, guardTask, bdReadFound, true, "messaging-class", false},
		{"unparseable label list", []string{"update", "demo-1", "--add-label", `a"b`}, guardTask, bdReadFound, true, "bd would reject", false},
		{"quoted ordinary label", []string{"update", "demo-1", "--add-label", `"triage"`}, guardTask, bdReadFound, false, "", false},
		{"label verb keeps the quotes", []string{"label", "add", "demo-1", `"wisp"`}, guardTask, bdReadFound, false, "", false},
		{"create quoted label", []string{"create", "--title", "x", "--labels", `"gc:session"`}, beads.Bead{}, bdReadNotFound, true, "sessions-class", false},
		{"q quoted label", []string{"q", "x", "-l", `triage,"wisp"`}, beads.Bead{}, bdReadNotFound, true, "graph-class", false},
		{"new alias", []string{"new", "--title", "x", "--type", "message"}, beads.Bead{}, bdReadNotFound, true, "messaging-class", false},
		{"create work bead", []string{"create", "--title", "x", "--labels", `"triage"`}, beads.Bead{}, bdReadNotFound, false, "", false},
	})
}

// TestGcBdCreateGuardParsesLabelsAsBdDoes is the same on the pre-existing
// create guard, which runs before the by-id door.
func TestGcBdCreateGuardParsesLabelsAsBdDoes(t *testing.T) {
	cfg := splitGuardTestCity()
	for _, argv := range [][]string{
		{"create", "--title", "x", "--labels", `"gc:session"`},
		{"create", "--title", "x", "-l", `triage,"wisp"`},
		{"new", "--title", "x", "--label", `"gc:nudge"`},
		{"create", "--title", "x", "--labels", `a"b`},
	} {
		if msg, refused := bdRelocatedClassCreateRefusal(cfg, t.TempDir(), argv); !refused || msg == "" {
			t.Errorf("gc bd %s was not refused", strings.Join(argv, " "))
		}
	}
	if _, refused := bdRelocatedClassCreateRefusal(cfg, t.TempDir(), []string{"create", "--title", "x", "--labels", `"triage"`}); refused {
		t.Error("a quoted work label was refused")
	}
}

// TestGcBdClassGuardSQLRefusesHiddenWrites keeps sql to one plain read.
func TestGcBdClassGuardSQLRefusesHiddenWrites(t *testing.T) {
	runGuardCases(t, []guardCase{
		{"comment hides a separator", []string{"sql", "/* ; SELECT 1 */ UPDATE issues SET issue_type='message'"}, beads.Bead{}, bdReadFound, true, "comment", false},
		{"executable comment", []string{"sql", "/*!40000 UPDATE issues SET issue_type='message' */"}, beads.Bead{}, bdReadFound, true, "comment", false},
		{"line comment", []string{"sql", "SELECT 1 -- x\nUPDATE issues SET issue_type='message'"}, beads.Bead{}, bdReadFound, true, "comment", false},
		{"hash comment", []string{"sql", "SELECT 1 # x"}, beads.Bead{}, bdReadFound, true, "comment", false},
		{"two statements", []string{"sql", "select 1; update issues set issue_type='message'"}, beads.Bead{}, bdReadFound, true, "more than one statement", false},
		{"dolt procedure as a function", []string{"sql", "SELECT DOLT_MERGE('pre-split')"}, beads.Bead{}, bdReadFound, true, "Dolt procedure", false},
		{"backticked dolt procedure", []string{"sql", "SELECT `dolt_merge`('pre-split')"}, beads.Bead{}, bdReadFound, true, "backtick-quoted Dolt", false},
		{"backticked dolt table", []string{"sql", "select * from ` DOLT_branches`"}, beads.Bead{}, bdReadFound, true, "backtick-quoted Dolt", false},
		{"explain wraps a write", []string{"sql", "EXPLAIN UPDATE issues SET issue_type='message'"}, beads.Bead{}, bdReadFound, true, "not a plain SELECT", false},
		{"unknown flag", []string{"sql", "--weird", "select 1"}, beads.Bead{}, bdReadFound, true, "--weird", false},
		{"two queries", []string{"sql", "select 1", "select 2"}, beads.Bead{}, bdReadFound, true, "exactly one query", false},
		{"--csv read", []string{"sql", "--csv", "select id from issues"}, beads.Bead{}, bdReadFound, false, "", false},
		{"--json read, trailing semicolon", []string{"sql", "--json", "SELECT COUNT(*) FROM issues;"}, beads.Bead{}, bdReadFound, false, "", false},
		{"core pack derived-table read", []string{"sql", "--csv", "SELECT * FROM (\nWITH c AS (SELECT id FROM issues) SELECT COUNT(*) FROM c\n) gc_scope_read"}, beads.Bead{}, bdReadFound, false, "", false},
	})
}

// TestGcBdClassGuardModelsMetadataNull: bd 1.3.2 stores --set-metadata k=null
// as JSON null (reads back absent), the Enterprise build as the string "null".
func TestGcBdClassGuardModelsMetadataNull(t *testing.T) {
	convoy := beads.Bead{ID: "demo-2", Type: "convoy", Metadata: map[string]string{"gc.synthetic": "true", "gc.root_bead_id": "gcg-1"}}
	runGuardCases(t, []guardCase{
		{"null unmasks the graph marker", []string{"update", "demo-2", "--set-metadata", "gc.synthetic=null"}, convoy, bdReadFound, true, "graph-class", false},
		{"empty unmasks it too", []string{"update", "demo-2", "--set-metadata", "gc.synthetic="}, convoy, bdReadFound, true, "graph-class", false},
		{"type change unmasks it", []string{"update", "demo-2", "--type=task"}, convoy, bdReadFound, true, "graph-class", false},
		{"null of an absent marker", []string{"update", "demo-1", "--set-metadata", "gc.kind=null"}, guardTask, bdReadFound, false, "", false},
	})
}

// TestGcBdClassGuardJudgesOnlyAResolvedBead: a class-touching write is judged
// against exactly the bead its id resolves to; absent, colliding or unreadable
// is a refusal. A write that touches no class field needs no read.
func TestGcBdClassGuardJudgesOnlyAResolvedBead(t *testing.T) {
	runGuardCases(t, []guardCase{
		{"absent bead", []string{"update", "demo-9", "--add-label", "wisp"}, beads.Bead{}, bdReadNotFound, true, "no bead", false},
		{"failed read", []string{"update", "demo-9", "--remove-label", "gc:session"}, beads.Bead{}, bdReadFailed, true, "could not read", false},
		{"collision", []string{"label", "add", "demo-9", "triage,gc:wait"}, beads.Bead{}, bdReadCollision, true, "different bead", false},
		{"set-state on a failed read", []string{"set-state", "demo-9", "patrol=muted"}, beads.Bead{}, bdReadFailed, true, "could not read", false},
		{"no id", []string{"update", "--add-label", "wisp"}, beads.Bead{}, bdReadFound, true, "name the bead explicitly", false},
		{"class-free write, no read needed", []string{"update", "demo-9", "--status", "in_progress", "--set-metadata", "gc.outcome=pass"}, beads.Bead{}, bdReadFailed, false, "", false},
	})
}

// TestGcBdClassGuardKnowsTheFleetsFlags: an ordinary update spelled with the
// Enterprise build's flags is not refused, and the store-redirecting globals
// are.
func TestGcBdClassGuardKnowsTheFleetsFlags(t *testing.T) {
	runGuardCases(t, []guardCase{
		{"--if-revision", []string{"update", "demo-1", "--if-revision", "7", "--add-label", "triage"}, guardTask, bdReadFound, false, "", true},
		{"--clear-notes", []string{"update", "demo-1", "--clear-notes", "--add-label", "triage"}, guardTask, bdReadFound, false, "", true},
		{"--json in front", []string{"--json", "update", "demo-1", "--add-label", "wisp"}, guardTask, bdReadFound, true, "graph-class", false},
		{"--profile redirects the store", []string{"--profile", "team", "update", "demo-1", "--add-label", "triage"}, guardTask, bdReadFound, true, "--profile", false},
		{"-C redirects the store", []string{"-C", "/elsewhere", "update", "demo-1", "--add-label", "triage"}, guardTask, bdReadFound, true, "--directory", false},
		{"--directory redirects the store", []string{"update", "demo-1", "--directory=/elsewhere"}, guardTask, bdReadFound, true, "--directory", false},
		{"create --repo", []string{"create", "--title", "x", "--repo", "/elsewhere"}, guardTask, bdReadFound, true, "--repo", false},
		{"gate create --repo", []string{"gate", "create", "--blocks", "demo-1", "--repo", "o/r"}, guardTask, bdReadFound, true, "--repo", false},
		{"--db redirects the store", []string{"update", "demo-1", "--db", "other", "--add-label", "triage"}, guardTask, bdReadFound, true, "--db", false},
		{"init --db is not vetted", []string{"init", "--db", "city", "--prefix", "tt"}, guardTask, bdReadFound, true, "--db", false},
		{"init --prefix", []string{"init", "--prefix", "tt", "--non-interactive"}, guardTask, bdReadFound, false, "", false},
		{"init --from-jsonl is not", []string{"init", "--from-jsonl"}, guardTask, bdReadFound, true, "--from-jsonl", false},
		{"unknown verb", []string{"frobnicate"}, guardTask, bdReadFound, true, "unknown command", false},
		{"serve", []string{"serve"}, guardTask, bdReadFound, true, "write API", false},
		{"dolt pull", []string{"dolt", "pull"}, guardTask, bdReadFound, true, "wholesale", false},
		{"vc merge", []string{"vc", "merge", "pre-split"}, guardTask, bdReadFound, true, "wholesale", false},
		{"federation sync", []string{"federation", "sync"}, guardTask, bdReadFound, true, "wholesale", false},
		{"sync", []string{"sync"}, guardTask, bdReadFound, true, "wholesale", false},
		{"backup status", []string{"backup", "status", "--json"}, guardTask, bdReadFound, false, "", false},
		{"backup sync", []string{"backup", "sync", "--json"}, guardTask, bdReadFound, false, "", false},
		{"dep rm alias", []string{"dep", "rm", "demo-1", "demo-2"}, guardTask, bdReadFound, false, "", false},
		{"bare --version", []string{"--version"}, guardTask, bdReadFound, false, "", false},
	})
}

// TestGcBdImportDryRunReportsAndSkipInfraNamesDependents covers two review
// nits: an import --dry-run writes nothing, so it is reported rather than
// refused; and a kept row whose dependency points at a left-out
// infrastructure id would write a cross-store edge, so it is refused.
func TestGcBdImportDryRunReportsAndSkipInfraNamesDependents(t *testing.T) {
	archive := `{"id":"demo-1","title":"w","issue_type":"task","dependencies":[{"issue_id":"demo-1","depends_on_id":"demo-3","type":"blocks"}]}` + "\n" +
		`{"id":"demo-2","title":"w2","issue_type":"task"}` + "\n" +
		`{"id":"demo-3","title":"s","issue_type":"session"}` + "\n"
	scope := t.TempDir()
	if err := os.WriteFile(filepath.Join(scope, "a.jsonl"), []byte(archive), 0o644); err != nil {
		t.Fatal(err)
	}
	read := func(string) (beads.Bead, bdReadResult) { return beads.Bead{}, bdReadNotFound }
	g := bdClassFlipGuard(splitGuardTestCity(), scope, []string{"import", "a.jsonl", "--dry-run"}, splitTestDeps(nil, read))
	if g.refusal != "" || !strings.Contains(g.notice, "dry run") || strings.Join(g.args, " ") != "import a.jsonl --dry-run" {
		t.Fatalf("dry run: refusal=%q notice=%q args=%v", g.refusal, g.notice, g.args)
	}
	// The kept row depends on a left-out session row: the edge would cross
	// stores, so even --skip-infra refuses it.
	g = bdClassFlipGuard(splitGuardTestCity(), scope, []string{"import", "a.jsonl", "--skip-infra"}, splitTestDeps(nil, read))
	if g.cleanup != nil {
		defer g.cleanup()
	}
	if g.refusal == "" || !strings.Contains(g.refusal, "across stores") || !strings.Contains(g.refusal, "demo-3") {
		t.Fatalf("skip-infra with a cross-store dependency: refusal=%q notice=%q", g.refusal, g.notice)
	}
}

// casGetCountingStore counts Gets and can fail them.
type casGetCountingStore struct {
	beads.Store
	gets   int
	getErr error
}

func (s *casGetCountingStore) Get(id string) (beads.Bead, error) {
	s.gets++
	if s.getErr != nil {
		return beads.Bead{}, s.getErr
	}
	return s.Store.Get(id)
}

// TestBeadsMetadataCASClassGuardReadsOnlyWhenItMatters covers the
// metadata-cas nit and finding 6 there: a key no Classify arm reads is never
// pre-read, and clearing a class key on a bead the store could not read is
// refused.
func TestBeadsMetadataCASClassGuardReadsOnlyWhenItMatters(t *testing.T) {
	holds := workStoreClassHolds(splitGuardTestCity())
	store := &casGetCountingStore{Store: beads.NewMemStore()}
	if refusal := beadsMetadataCASClassFlipRefusal(store, beadsMetadataCASRequest{beadID: "demo-1", key: "review_sha", next: "x"}, holds); refusal != "" || store.gets != 0 {
		t.Fatalf("ordinary key: refusal=%q gets=%d; want no refusal and no read", refusal, store.gets)
	}
	store.getErr = errors.New("bd unavailable")
	if refusal := beadsMetadataCASClassFlipRefusal(store, beadsMetadataCASRequest{beadID: "demo-1", key: "gc.synthetic", next: ""}, holds); !strings.Contains(refusal, "could not read") {
		t.Fatalf("clearing a class key on an unreadable bead: refusal=%q", refusal)
	}
	if refusal := beadsMetadataCASClassFlipRefusal(store, beadsMetadataCASRequest{beadID: "demo-1", key: "gc.root_bead_id", next: "gcg-1"}, holds); !strings.Contains(refusal, "graph-class") {
		t.Fatalf("setting a graph marker on an unreadable bead: refusal=%q", refusal)
	}
}
