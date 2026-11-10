package censusgrowth

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/testpolicy/resourcecensus"
)

var update = flag.Bool("update", false, "rewrite the golden report")

// rec is shorthand for one counts-file record.
func rec(path string, resource resourcecensus.Resource, calls, medium int, tagged bool) resourcecensus.CountRecord {
	return resourcecensus.CountRecord{Path: path, Resource: resource, Calls: calls, Medium: medium, Tagged: tagged}
}

// counts renders records exactly as the generator does, so ParseCounts accepts
// them. The records are sorted by the caller.
func counts(records ...resourcecensus.CountRecord) []byte {
	return []byte(resourcecensus.RenderCounts(records))
}

// row renders one counted policy row. Only the fields the report reads are
// meaningful; the rest are fixed so a test that changes one is the only diff.
func row(section, scope, resource, expires string) string {
	return fmt.Sprintf(`[[%s]]
scope = %q
resource = %q
reported_calls = 1
reported_files = 1
owner_bead = "ga-owner"
invariant = "inv"
resource_owner = "owner"
migration_target = "P0"
expires = %q
`, section, scope, resource, expires)
}

func policy(rows ...string) []byte {
	return []byte("version = 3\n\n" + strings.Join(rows, "\n"))
}

// defaultPolicy governs subprocess in the all and untagged scopes and tracks
// Small subprocess debt, which is every figure the report derives.
func defaultPolicy() []byte {
	return policy(
		row("audit_baseline", "all", "subprocess", "2026-12-31"),
		row("debt", "untagged", "subprocess", "2026-12-31"),
		row("small_debt", "all", "subprocess", "2026-12-31"),
	)
}

func baseRecords() []resourcecensus.CountRecord {
	return []resourcecensus.CountRecord{
		rec("a/a_test.go", resourcecensus.ResourceSubprocess, 5, 0, false),
		rec("b/b_test.go", resourcecensus.ResourceSubprocess, 3, 1, false),
		rec("c/c_test.go", resourcecensus.ResourceSubprocess, 2, 0, true),
	}
}

func mustCompare(t *testing.T, in Input) Report {
	t.Helper()
	report, err := Compare(in)
	if err != nil {
		t.Fatalf("Compare: %v", err)
	}
	return report
}

func figures(t *testing.T, report Report, scope resourcecensus.Scope, resource resourcecensus.Resource) RowDelta {
	t.Helper()
	for _, delta := range report.Rows {
		if delta.Scope == scope && delta.Resource == resource {
			return delta
		}
	}
	t.Fatalf("no row delta for %s/%s in %+v", scope, resource, report.Rows)
	return RowDelta{}
}

func TestCompareClassification(t *testing.T) {
	base := baseRecords()
	cases := []struct {
		name        string
		head        []resourcecensus.CountRecord
		wantGrowth  bool
		wantAll     [2]resourcecensus.Count // base then head aggregate, all/subprocess
		wantSmall   [2]resourcecensus.Count // base then head Small, all/subprocess
		wantChanges int
	}{
		{
			name:        "no change",
			head:        base,
			wantGrowth:  false,
			wantAll:     [2]resourcecensus.Count{{Calls: 10, Files: 3}, {Calls: 10, Files: 3}},
			wantSmall:   [2]resourcecensus.Count{{Calls: 9, Files: 3}, {Calls: 9, Files: 3}},
			wantChanges: 0,
		},
		{
			name: "growth of one call",
			head: []resourcecensus.CountRecord{
				rec("a/a_test.go", resourcecensus.ResourceSubprocess, 6, 0, false),
				base[1], base[2],
			},
			wantGrowth:  true,
			wantAll:     [2]resourcecensus.Count{{Calls: 10, Files: 3}, {Calls: 11, Files: 3}},
			wantSmall:   [2]resourcecensus.Count{{Calls: 9, Files: 3}, {Calls: 10, Files: 3}},
			wantChanges: 1,
		},
		{
			name: "improvement",
			head: []resourcecensus.CountRecord{
				rec("a/a_test.go", resourcecensus.ResourceSubprocess, 4, 0, false),
				base[1], base[2],
			},
			wantGrowth:  false,
			wantAll:     [2]resourcecensus.Count{{Calls: 10, Files: 3}, {Calls: 9, Files: 3}},
			wantSmall:   [2]resourcecensus.Count{{Calls: 9, Files: 3}, {Calls: 8, Files: 3}},
			wantChanges: 1,
		},
		{
			name: "mixed: the all scope improves while the untagged scope grows",
			head: []resourcecensus.CountRecord{
				rec("a/a_test.go", resourcecensus.ResourceSubprocess, 6, 0, false),
				base[1], // the tagged record c is gone
			},
			wantGrowth:  true,
			wantAll:     [2]resourcecensus.Count{{Calls: 10, Files: 3}, {Calls: 9, Files: 2}},
			wantSmall:   [2]resourcecensus.Count{{Calls: 9, Files: 3}, {Calls: 8, Files: 2}},
			wantChanges: 2,
		},
		{
			name: "Small-only growth: a Medium declaration shrinks, calls stay",
			head: []resourcecensus.CountRecord{
				base[0],
				rec("b/b_test.go", resourcecensus.ResourceSubprocess, 3, 0, false),
				base[2],
			},
			wantGrowth:  true,
			wantAll:     [2]resourcecensus.Count{{Calls: 10, Files: 3}, {Calls: 10, Files: 3}},
			wantSmall:   [2]resourcecensus.Count{{Calls: 9, Files: 3}, {Calls: 10, Files: 3}},
			wantChanges: 1,
		},
		{
			name: "Small-only improvement: a Medium declaration grows, calls stay",
			head: []resourcecensus.CountRecord{
				base[0],
				rec("b/b_test.go", resourcecensus.ResourceSubprocess, 3, 3, false),
				base[2],
			},
			wantGrowth:  false,
			wantAll:     [2]resourcecensus.Count{{Calls: 10, Files: 3}, {Calls: 10, Files: 3}},
			wantSmall:   [2]resourcecensus.Count{{Calls: 9, Files: 3}, {Calls: 7, Files: 2}},
			wantChanges: 1,
		},
		{
			name: "new record",
			head: append(append([]resourcecensus.CountRecord{}, base...),
				rec("d/d_test.go", resourcecensus.ResourceSubprocess, 1, 0, false)),
			wantGrowth:  true,
			wantAll:     [2]resourcecensus.Count{{Calls: 10, Files: 3}, {Calls: 11, Files: 4}},
			wantSmall:   [2]resourcecensus.Count{{Calls: 9, Files: 3}, {Calls: 10, Files: 4}},
			wantChanges: 1,
		},
		{
			name:        "removed record",
			head:        []resourcecensus.CountRecord{base[0], base[2]},
			wantGrowth:  false,
			wantAll:     [2]resourcecensus.Count{{Calls: 10, Files: 3}, {Calls: 7, Files: 2}},
			wantSmall:   [2]resourcecensus.Count{{Calls: 9, Files: 3}, {Calls: 7, Files: 2}},
			wantChanges: 1,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			report := mustCompare(t, Input{
				BaseCounts: counts(base...), HeadCounts: counts(tc.head...),
				BasePolicy: defaultPolicy(), HeadPolicy: defaultPolicy(),
			})
			delta := figures(t, report, resourcecensus.ScopeAll, resourcecensus.ResourceSubprocess)
			if delta.Base != tc.wantAll[0] || delta.Head != tc.wantAll[1] {
				t.Errorf("all/subprocess aggregate = %+v -> %+v, want %+v -> %+v", delta.Base, delta.Head, tc.wantAll[0], tc.wantAll[1])
			}
			if delta.BaseSmall != tc.wantSmall[0] || delta.HeadSmall != tc.wantSmall[1] {
				t.Errorf("all/subprocess Small = %+v -> %+v, want %+v -> %+v", delta.BaseSmall, delta.HeadSmall, tc.wantSmall[0], tc.wantSmall[1])
			}
			if got := report.Growth(); got != tc.wantGrowth {
				t.Errorf("Growth() = %v, want %v", got, tc.wantGrowth)
			}
			if got := len(report.Files); got != tc.wantChanges {
				t.Errorf("len(Files) = %d, want %d: %+v", got, tc.wantChanges, report.Files)
			}
			if report.PolicyRowChanged() {
				t.Errorf("PolicyRowChanged() = true with identical policy files: %+v", report.PolicyChanges)
			}
		})
	}
}

func TestCompareFileChangeKinds(t *testing.T) {
	base := baseRecords()
	head := []resourcecensus.CountRecord{
		rec("a/a_test.go", resourcecensus.ResourceSubprocess, 6, 0, false), // changed
		// b removed
		base[2],
		rec("d/d_test.go", resourcecensus.ResourceSubprocess, 1, 0, false), // added
	}
	report := mustCompare(t, Input{
		BaseCounts: counts(base...), HeadCounts: counts(head...),
		BasePolicy: defaultPolicy(), HeadPolicy: defaultPolicy(),
	})
	want := []struct {
		path string
		kind ChangeKind
	}{
		{"a/a_test.go", Changed},
		{"b/b_test.go", Removed},
		{"d/d_test.go", Added},
	}
	if len(report.Files) != len(want) {
		t.Fatalf("Files = %+v, want %d entries", report.Files, len(want))
	}
	for i, w := range want {
		if report.Files[i].Path != w.path || report.Files[i].Kind != w.kind {
			t.Errorf("Files[%d] = %s %s, want %s %s", i, report.Files[i].Path, report.Files[i].Kind, w.path, w.kind)
		}
	}
	changed := report.Files[0]
	if changed.Base == nil || changed.Head == nil || changed.Base.Calls != 5 || changed.Head.Calls != 6 {
		t.Errorf("changed entry must carry both records: %+v", changed)
	}
	if report.Files[1].Head != nil || report.Files[2].Base != nil {
		t.Errorf("a removed entry has no head record and an added one no base record: %+v", report.Files)
	}
}

func TestCompareTaggedFlip(t *testing.T) {
	base := []resourcecensus.CountRecord{rec("a/a_test.go", resourcecensus.ResourceSubprocess, 4, 0, true)}
	untagged := []resourcecensus.CountRecord{rec("a/a_test.go", resourcecensus.ResourceSubprocess, 4, 0, false)}

	t.Run("tagged to untagged grows the untagged scope", func(t *testing.T) {
		report := mustCompare(t, Input{
			BaseCounts: counts(base...), HeadCounts: counts(untagged...),
			BasePolicy: defaultPolicy(), HeadPolicy: defaultPolicy(),
		})
		delta := figures(t, report, resourcecensus.ScopeUntagged, resourcecensus.ResourceSubprocess)
		if (delta.Base != resourcecensus.Count{}) || delta.Head != (resourcecensus.Count{Calls: 4, Files: 1}) {
			t.Errorf("untagged aggregate = %+v -> %+v, want {0 0} -> {4 1}", delta.Base, delta.Head)
		}
		if !report.Growth() {
			t.Error("Growth() = false for a file that left the tagged lane")
		}
		if len(report.Files) != 1 || report.Files[0].Kind != Changed {
			t.Errorf("a tagged flip is one changed record: %+v", report.Files)
		}
	})
	t.Run("untagged to tagged is an improvement", func(t *testing.T) {
		report := mustCompare(t, Input{
			BaseCounts: counts(untagged...), HeadCounts: counts(base...),
			BasePolicy: defaultPolicy(), HeadPolicy: defaultPolicy(),
		})
		if report.Growth() {
			t.Error("Growth() = true for a file that joined the tagged lane")
		}
		if len(report.Files) != 1 || report.Files[0].Kind != Changed {
			t.Errorf("a tagged flip is one changed record: %+v", report.Files)
		}
	})
}

func TestCompareMoves(t *testing.T) {
	base := baseRecords()
	t.Run("a decrease and an equal increase elsewhere is a move", func(t *testing.T) {
		head := []resourcecensus.CountRecord{
			rec("a/a_test.go", resourcecensus.ResourceSubprocess, 4, 0, false),
			rec("b/b_test.go", resourcecensus.ResourceSubprocess, 4, 1, false),
			base[2],
		}
		report := mustCompare(t, Input{
			BaseCounts: counts(base...), HeadCounts: counts(head...),
			BasePolicy: defaultPolicy(), HeadPolicy: defaultPolicy(),
		})
		if report.Growth() {
			t.Errorf("a pure move must not flag GROWTH: %+v", report.Rows)
		}
		if len(report.Moves) != 1 {
			t.Fatalf("Moves = %+v, want one", report.Moves)
		}
		move := report.Moves[0]
		if move.Resource != resourcecensus.ResourceSubprocess || move.Calls != 1 {
			t.Errorf("move = %+v, want 1 subprocess call", move)
		}
		if len(move.From) != 1 || move.From[0] != "a/a_test.go" || len(move.To) != 1 || move.To[0] != "b/b_test.go" {
			t.Errorf("move From/To = %v / %v, want a -> b", move.From, move.To)
		}
		if !strings.Contains(report.Markdown(), "moved") {
			t.Errorf("the report must mark the move as moved:\n%s", report.Markdown())
		}
	})
	t.Run("different resources do not net against each other", func(t *testing.T) {
		withSleep := append(append([]resourcecensus.CountRecord{}, base...),
			rec("a/a_test.go", resourcecensus.ResourceFixedSleep, 1, 0, false))
		head := []resourcecensus.CountRecord{
			rec("a/a_test.go", resourcecensus.ResourceFixedSleep, 1, 0, false),
			rec("a/a_test.go", resourcecensus.ResourceSubprocess, 4, 0, false),
			rec("b/b_test.go", resourcecensus.ResourceFixedSleep, 1, 0, false),
			base[1], base[2],
		}
		sortRecords(withSleep)
		sortRecords(head)
		report := mustCompare(t, Input{
			BaseCounts: counts(withSleep...), HeadCounts: counts(head...),
			BasePolicy: defaultPolicy(), HeadPolicy: defaultPolicy(),
		})
		if len(report.Moves) != 0 {
			t.Errorf("a subprocess decrease and a fixed_sleep increase are not a move: %+v", report.Moves)
		}
	})
	t.Run("a net increase is not labeled moved", func(t *testing.T) {
		head := []resourcecensus.CountRecord{
			rec("a/a_test.go", resourcecensus.ResourceSubprocess, 4, 0, false),
			rec("b/b_test.go", resourcecensus.ResourceSubprocess, 5, 1, false),
			base[2],
		}
		report := mustCompare(t, Input{
			BaseCounts: counts(base...), HeadCounts: counts(head...),
			BasePolicy: defaultPolicy(), HeadPolicy: defaultPolicy(),
		})
		if len(report.Moves) != 0 || !report.Growth() {
			t.Errorf("Moves = %+v, Growth = %v; want no move and GROWTH", report.Moves, report.Growth())
		}
	})
}

func sortRecords(records []resourcecensus.CountRecord) {
	for i := 1; i < len(records); i++ {
		for j := i; j > 0 && (records[j].Path < records[j-1].Path ||
			(records[j].Path == records[j-1].Path && records[j].Resource < records[j-1].Resource)); j-- {
			records[j], records[j-1] = records[j-1], records[j]
		}
	}
}

func TestComparePolicyRows(t *testing.T) {
	records := counts(baseRecords()...)
	cases := []struct {
		name     string
		head     []byte
		wantKind ChangeKind
		wantRows int
	}{
		{
			name: "expires change",
			head: policy(
				row("audit_baseline", "all", "subprocess", "2027-06-30"),
				row("debt", "untagged", "subprocess", "2026-12-31"),
				row("small_debt", "all", "subprocess", "2026-12-31"),
			),
			wantKind: Changed, wantRows: 1,
		},
		{
			name: "owner_bead change",
			head: policy(
				row("audit_baseline", "all", "subprocess", "2026-12-31"),
				strings.Replace(row("debt", "untagged", "subprocess", "2026-12-31"), "ga-owner", "ga-other", 1),
				row("small_debt", "all", "subprocess", "2026-12-31"),
			),
			wantKind: Changed, wantRows: 1,
		},
		{
			name: "added policy row",
			head: policy(
				row("audit_baseline", "all", "subprocess", "2026-12-31"),
				row("debt", "untagged", "subprocess", "2026-12-31"),
				row("debt", "untagged", "fixed_sleep", "2026-12-31"),
				row("small_debt", "all", "subprocess", "2026-12-31"),
			),
			wantKind: Added, wantRows: 1,
		},
		{
			name: "removed policy row",
			head: policy(
				row("audit_baseline", "all", "subprocess", "2026-12-31"),
				row("small_debt", "all", "subprocess", "2026-12-31"),
			),
			wantKind: Removed, wantRows: 1,
		},
		{
			name: "added medium row",
			head: append(defaultPolicy(), []byte(`
[[medium]]
package_dir = "x"
package_name = "x"
owner = "TestMain"
resources = ["subprocess"]
owner_bead = "ga-owner"
invariant = "inv"
resource_owner = "owner"
migration_target = "P0"
expires = "2026-12-31"
`)...),
			wantKind: Added, wantRows: 1,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			report := mustCompare(t, Input{
				BaseCounts: records, HeadCounts: records,
				BasePolicy: defaultPolicy(), HeadPolicy: tc.head,
			})
			if !report.PolicyRowChanged() {
				t.Fatal("PolicyRowChanged() = false")
			}
			if report.Growth() {
				t.Errorf("a policy-only change must not flag GROWTH: %+v", report.Rows)
			}
			if len(report.PolicyChanges) != tc.wantRows {
				t.Fatalf("PolicyChanges = %+v, want %d", report.PolicyChanges, tc.wantRows)
			}
			if report.PolicyChanges[0].Kind != tc.wantKind {
				t.Errorf("PolicyChanges[0].Kind = %s, want %s", report.PolicyChanges[0].Kind, tc.wantKind)
			}
			if !strings.Contains(report.Markdown(), "POLICY-ROW") {
				t.Errorf("the report must carry a POLICY-ROW flag:\n%s", report.Markdown())
			}
		})
	}

	t.Run("the changed field is named", func(t *testing.T) {
		report := mustCompare(t, Input{
			BaseCounts: records, HeadCounts: records,
			BasePolicy: defaultPolicy(), HeadPolicy: cases[0].head,
		})
		if got := report.PolicyChanges[0].Fields; len(got) != 1 || got[0] != "expires" {
			t.Errorf("Fields = %v, want [expires]", got)
		}
	})
}

func TestCompareFlagsInMarkdown(t *testing.T) {
	base := baseRecords()
	t.Run("growth", func(t *testing.T) {
		head := []resourcecensus.CountRecord{
			rec("a/a_test.go", resourcecensus.ResourceSubprocess, 6, 0, false), base[1], base[2],
		}
		md := mustCompare(t, Input{
			BaseCounts: counts(base...), HeadCounts: counts(head...),
			BasePolicy: defaultPolicy(), HeadPolicy: defaultPolicy(),
		}).Markdown()
		if !strings.Contains(md, "GROWTH") {
			t.Errorf("missing GROWTH flag:\n%s", md)
		}
		if strings.Contains(md, "POLICY-ROW") {
			t.Errorf("unexpected POLICY-ROW flag:\n%s", md)
		}
	})
	t.Run("no change carries no flag", func(t *testing.T) {
		md := mustCompare(t, Input{
			BaseCounts: counts(base...), HeadCounts: counts(base...),
			BasePolicy: defaultPolicy(), HeadPolicy: defaultPolicy(),
		}).Markdown()
		if strings.Contains(md, "GROWTH") || strings.Contains(md, "POLICY-ROW") {
			t.Errorf("an unchanged ledger must carry no flag:\n%s", md)
		}
	})
	t.Run("the report opens with the sticky-comment marker", func(t *testing.T) {
		md := mustCompare(t, Input{
			BaseCounts: counts(base...), HeadCounts: counts(base...),
			BasePolicy: defaultPolicy(), HeadPolicy: defaultPolicy(),
		}).Markdown()
		if !strings.HasPrefix(md, Marker+"\n") {
			t.Errorf("Markdown must start with Marker %q:\n%s", Marker, md)
		}
	})
}

func TestCompareBaseWithoutCountsFile(t *testing.T) {
	report := mustCompare(t, Input{
		BaseCounts: nil, HeadCounts: counts(baseRecords()...),
		BasePolicy: nil, HeadPolicy: defaultPolicy(),
	})
	if !report.BasePredatesCounts {
		t.Error("BasePredatesCounts = false")
	}
	if report.Growth() || report.PolicyRowChanged() {
		t.Error("with no base there is no delta, so no flag")
	}
	if !strings.Contains(report.Markdown(), "base predates the counts file: no delta available") {
		t.Errorf("missing the no-delta line:\n%s", report.Markdown())
	}
}

func TestCompareHeadWithoutCountsFileIsAnError(t *testing.T) {
	for name, head := range map[string][]byte{"nil": nil, "empty": {}} {
		t.Run(name, func(t *testing.T) {
			_, err := Compare(Input{
				BaseCounts: counts(baseRecords()...), HeadCounts: head,
				BasePolicy: defaultPolicy(), HeadPolicy: defaultPolicy(),
			})
			if err == nil {
				t.Fatal("Compare succeeded without a head counts file")
			}
			if !strings.Contains(err.Error(), resourcecensus.CountsPath) {
				t.Errorf("error %q should name %s", err, resourcecensus.CountsPath)
			}
		})
	}
}

func TestCompareRejectsMalformedInput(t *testing.T) {
	good := counts(baseRecords()...)
	cases := map[string]Input{
		"head counts": {BaseCounts: good, HeadCounts: []byte("not toml ["), BasePolicy: defaultPolicy(), HeadPolicy: defaultPolicy()},
		"base counts": {BaseCounts: []byte("version = 99\n"), HeadCounts: good, BasePolicy: defaultPolicy(), HeadPolicy: defaultPolicy()},
		"head policy": {BaseCounts: good, HeadCounts: good, BasePolicy: defaultPolicy(), HeadPolicy: []byte("bogus = 1\n")},
		"base policy": {BaseCounts: good, HeadCounts: good, BasePolicy: []byte("bogus = 1\n"), HeadPolicy: defaultPolicy()},
	}
	for name, in := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := Compare(in); err == nil {
				t.Fatal("Compare accepted malformed input")
			}
		})
	}
}

func TestMarkdownCapsFileDetailAtFiftyLines(t *testing.T) {
	var head []resourcecensus.CountRecord
	for i := 0; i < 53; i++ {
		head = append(head, rec(fmt.Sprintf("pkg/f%02d_test.go", i), resourcecensus.ResourceSubprocess, 1, 0, false))
	}
	report := mustCompare(t, Input{
		BaseCounts: counts(), HeadCounts: counts(head...),
		BasePolicy: defaultPolicy(), HeadPolicy: defaultPolicy(),
	})
	if len(report.Files) != 53 {
		t.Fatalf("Files has %d entries, want all 53 (the cap is a rendering rule)", len(report.Files))
	}
	md := report.Markdown()
	if !strings.Contains(md, "pkg/f49_test.go") {
		t.Errorf("the 50th record must be listed:\n%s", md)
	}
	if strings.Contains(md, "pkg/f50_test.go") {
		t.Errorf("the 51st record must be folded into the tail:\n%s", md)
	}
	if !strings.Contains(md, "and 3 more") {
		t.Errorf("missing the %q tail:\n%s", "and 3 more", md)
	}
}

func TestMarkdownGolden(t *testing.T) {
	base := baseRecords()
	var head []resourcecensus.CountRecord
	head = append(head,
		rec("a/a_test.go", resourcecensus.ResourceSubprocess, 4, 0, false),
		rec("b/b_test.go", resourcecensus.ResourceSubprocess, 4, 1, false),
		base[2],
	)
	for i := 0; i < 52; i++ {
		head = append(head, rec(fmt.Sprintf("pkg/f%02d_test.go", i), resourcecensus.ResourceSubprocess, 1, 0, i%2 == 0))
	}
	sortRecords(head)
	headPolicy := policy(
		row("audit_baseline", "all", "subprocess", "2027-06-30"),
		row("debt", "untagged", "subprocess", "2026-12-31"),
		row("small_debt", "all", "subprocess", "2026-12-31"),
	)
	got := mustCompare(t, Input{
		BaseCounts: counts(base...), HeadCounts: counts(head...),
		BasePolicy: defaultPolicy(), HeadPolicy: headPolicy,
	}).Markdown()

	golden := filepath.Join("testdata", "report.golden.md")
	if *update {
		if err := os.WriteFile(golden, []byte(got), 0o644); err != nil {
			t.Fatalf("write golden: %v", err)
		}
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}
	if got != string(want) {
		t.Errorf("report differs from %s (regenerate with -update and review the diff)\n--- got ---\n%s\n--- want ---\n%s", golden, got, want)
	}
}

func TestGrowthUsesResourcecensusDerivations(t *testing.T) {
	// The report's figures must equal resourcecensus's own derivations, so the
	// Small rule has one implementation.
	base, head := baseRecords(), append(baseRecords(), rec("d/d_test.go", resourcecensus.ResourceSubprocess, 2, 1, false))
	report := mustCompare(t, Input{
		BaseCounts: counts(base...), HeadCounts: counts(head...),
		BasePolicy: defaultPolicy(), HeadPolicy: defaultPolicy(),
	})
	for _, delta := range report.Rows {
		if want := resourcecensus.DerivedCount(head, delta.Scope, delta.Resource); delta.Head != want {
			t.Errorf("%s/%s head = %+v, want DerivedCount %+v", delta.Scope, delta.Resource, delta.Head, want)
		}
		if want := resourcecensus.DerivedSmallCount(head, delta.Scope, delta.Resource); delta.HeadSmall != want {
			t.Errorf("%s/%s head Small = %+v, want DerivedSmallCount %+v", delta.Scope, delta.Resource, delta.HeadSmall, want)
		}
		if want := resourcecensus.DerivedSmallCount(base, delta.Scope, delta.Resource); delta.BaseSmall != want {
			t.Errorf("%s/%s base Small = %+v, want DerivedSmallCount %+v", delta.Scope, delta.Resource, delta.BaseSmall, want)
		}
	}
}
