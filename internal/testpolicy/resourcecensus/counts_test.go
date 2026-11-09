package resourcecensus

import (
	"fmt"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Literal copies of the contract strings, so a change to the production
// constants is a visible test change: the maintainer-pr-review autofix and
// operators match these texts.
const (
	wantCountsHeader = "# GENERATED FILE - DO NOT EDIT BY HAND.\n" +
		"# Per-file counts for the P0.4 source resource census (owner: ga-cp3hwi).\n" +
		"# Regenerate: " + wantRegenerateCommand + "\n"
	wantRegenerateCommand = `GC_TEST_REPO_ROOT="$PWD" bazel run //internal/testpolicy/resourcecensus:resourcecensus_test -- -test.run='^TestRepositoryLedgerMatchesCensusAndDocumentation$' -update`
	wantRegenerateLine    = "regenerate with: " + wantRegenerateCommand + " (then review the diff of test/test-resource-counts.toml: each changed line is a ceiling change)"
	wantCountsPath        = "test/test-resource-counts.toml"
	wantDocSection        = "Checked source-level resource ratchets"
	wantDocSubsection     = "Updating the resource census"
)

// removedLedgerKeys are the per-row count keys policy version 3 moved into
// the counts file. This is the only test file that names them.
var removedLedgerKeys = []string{"baseline_" + "calls", "baseline_" + "files"}

const countsFixtureTwoRecords = wantCountsHeader + `version = 1

[[count]]
path = "cmd/gc-write-mint/e2e_test.go"
resource = "subprocess"
calls = 1
tagged = true

[[count]]
path = "cmd/gc/beads_provider_lifecycle_test.go"
resource = "slow_process_gate"
calls = 12
medium = 1
`

const countsFixtureEmpty = wantCountsHeader + "version = 1\n"

// A path may carry several resources: records sort by path, then resource.
const countsFixtureSameFile = wantCountsHeader + `version = 1

[[count]]
path = "sample/a_test.go"
resource = "fixed_sleep"
calls = 2

[[count]]
path = "sample/a_test.go"
resource = "subprocess"
calls = 3
medium = 3
`

var canonicalCountsFixtures = []struct {
	name string
	data string
}{
	{"empty", countsFixtureEmpty},
	{"two records", countsFixtureTwoRecords},
	{"one file two resources", countsFixtureSameFile},
}

func TestRenderCountsMatchesTheDesignExample(t *testing.T) {
	t.Parallel()

	got := RenderCounts([]CountRecord{
		{Path: "cmd/gc-write-mint/e2e_test.go", Resource: ResourceSubprocess, Calls: 1, Tagged: true},
		{Path: "cmd/gc/beads_provider_lifecycle_test.go", Resource: ResourceSlowProcessGate, Calls: 12, Medium: 1},
	})
	if got != countsFixtureTwoRecords {
		t.Fatalf("RenderCounts =\n%s\nwant\n%s", got, countsFixtureTwoRecords)
	}
}

func TestCountsRoundTripIsCanonical(t *testing.T) {
	t.Parallel()

	for _, fixture := range canonicalCountsFixtures {
		t.Run(fixture.name, func(t *testing.T) {
			t.Parallel()
			records, err := ParseCounts([]byte(fixture.data))
			if err != nil {
				t.Fatalf("ParseCounts: %v", err)
			}
			if got := RenderCounts(records); got != fixture.data {
				t.Fatalf("RenderCounts(ParseCounts(f)) != f:\n--- got ---\n%s\n--- want ---\n%s", got, fixture.data)
			}
		})
	}
}

func TestParseCountsReadsEveryField(t *testing.T) {
	t.Parallel()

	records, err := ParseCounts([]byte(countsFixtureTwoRecords))
	if err != nil {
		t.Fatalf("ParseCounts: %v", err)
	}
	want := []CountRecord{
		{Path: "cmd/gc-write-mint/e2e_test.go", Resource: ResourceSubprocess, Calls: 1, Tagged: true},
		{Path: "cmd/gc/beads_provider_lifecycle_test.go", Resource: ResourceSlowProcessGate, Calls: 12, Medium: 1},
	}
	if fmt.Sprint(records) != fmt.Sprint(want) {
		t.Fatalf("ParseCounts = %+v, want %+v", records, want)
	}
}

// malformedCounts is shared by the parse-level and validator-level tests.
var malformedCounts = []struct {
	name string
	data string
	want string
}{
	{
		name: "unknown key",
		data: strings.Replace(countsFixtureTwoRecords, "calls = 1\n", "calls = 1\nextra = 1\n", 1),
		want: "unknown key",
	},
	{
		name: "missing version",
		data: strings.Replace(countsFixtureTwoRecords, "version = 1\n", "", 1),
		want: "version",
	},
	{
		name: "wrong version",
		data: strings.Replace(countsFixtureTwoRecords, "version = 1\n", "version = 2\n", 1),
		want: "version",
	},
	{
		name: "duplicate record",
		data: countsFixtureTwoRecords + "\n" + strings.SplitN(countsFixtureTwoRecords, "\n\n", 3)[2],
		want: "duplicate",
	},
	{
		name: "unsorted records",
		data: func() string {
			parts := strings.SplitN(countsFixtureTwoRecords, "\n\n", 3)
			return parts[0] + "\n\n" + parts[2] + "\n" + parts[1] + "\n"
		}(),
		want: "not sorted",
	},
	{
		name: "calls zero",
		data: strings.Replace(countsFixtureTwoRecords, "calls = 12", "calls = 0", 1),
		want: "calls",
	},
	{
		name: "medium above calls",
		data: strings.Replace(countsFixtureTwoRecords, "medium = 1", "medium = 13", 1),
		want: "medium",
	},
	{
		name: "negative medium",
		data: strings.Replace(countsFixtureTwoRecords, "medium = 1", "medium = -1", 1),
		want: "medium",
	},
	{
		name: "tagged false",
		data: strings.Replace(countsFixtureTwoRecords, "tagged = true", "tagged = false", 1),
		want: "tagged",
	},
	{
		name: "unknown resource",
		data: strings.Replace(countsFixtureTwoRecords, `"subprocess"`, `"teleporter"`, 1),
		want: "resource",
	},
	{
		name: "quote in path",
		data: strings.Replace(countsFixtureTwoRecords, `cmd/gc-write-mint/e2e_test.go`, `cmd/gc-write-mint/e2e\"_test.go`, 1),
		want: "path",
	},
	{
		name: "trailing space",
		data: strings.Replace(countsFixtureTwoRecords, "calls = 1\n", "calls = 1 \n", 1),
		want: "non-canonical",
	},
	{
		name: "no spaces around equals",
		data: strings.Replace(countsFixtureTwoRecords, "calls = 1\n", "calls=1\n", 1),
		want: "non-canonical",
	},
	{
		name: "extra blank line between records",
		data: strings.Replace(countsFixtureTwoRecords, "\n\n[[count]]\npath = \"cmd/gc/", "\n\n\n[[count]]\npath = \"cmd/gc/", 1),
		want: "non-canonical",
	},
	{
		name: "trailing blank line",
		data: countsFixtureTwoRecords + "\n",
		want: "non-canonical",
	},
	{
		name: "missing final newline",
		data: strings.TrimSuffix(countsFixtureTwoRecords, "\n"),
		want: "non-canonical",
	},
	{
		name: "crlf line endings",
		data: strings.ReplaceAll(countsFixtureTwoRecords, "\n", "\r\n"),
		want: "non-canonical",
	},
	{
		name: "missing header",
		data: strings.TrimPrefix(countsFixtureTwoRecords, wantCountsHeader),
		want: "non-canonical",
	},
	{
		name: "edited header",
		data: strings.Replace(countsFixtureTwoRecords, "DO NOT EDIT BY HAND", "feel free to edit", 1),
		want: "non-canonical",
	},
	{
		name: "empty file",
		data: "",
		want: "version",
	},
}

func TestParseCountsRejectsMalformedFiles(t *testing.T) {
	t.Parallel()

	for _, tt := range malformedCounts {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if tt.data == countsFixtureTwoRecords {
				t.Fatal("malformed fixture was not mutated")
			}
			_, err := ParseCounts([]byte(tt.data))
			requireErrorContains(t, err, tt.want)
		})
	}
}

func TestParseCountsNamesTheConflictMarkerRecovery(t *testing.T) {
	t.Parallel()

	for _, marker := range []string{"<<<<<<< HEAD", "=======", ">>>>>>> feature"} {
		data := strings.Replace(countsFixtureTwoRecords, "\n[[count]]\npath = \"cmd/gc/", "\n"+marker+"\n[[count]]\npath = \"cmd/gc/", 1)
		_, err := ParseCounts([]byte(data))
		requireErrorContains(t, err, "conflict markers")
		requireErrorContains(t, err, "take main's copy of "+wantCountsPath)
		requireErrorContains(t, err, wantRegenerateCommand)
		requireErrorContains(t, err, "never hand-merge")
	}
}

// ---- generation -----------------------------------------------------------

func occurrence(path string, resource Resource, tagged bool) Occurrence {
	return Occurrence{Path: path, PackageDir: filepath.Dir(path), PackageName: "sample", Resource: resource, Tagged: tagged}
}

func runnableOccurrence(path, owner string, resource Resource) Occurrence {
	return Occurrence{Path: path, PackageDir: filepath.Dir(path), PackageName: "sample", Owner: owner, Runnable: true, Resource: resource}
}

func TestGenerateCountsRecordsOnlyGovernedOccurrences(t *testing.T) {
	t.Parallel()

	census := Census{Occurrences: []Occurrence{
		occurrence("sample/a_test.go", ResourceSubprocess, false),
		occurrence("sample/a_test.go", ResourceSubprocess, false),
		occurrence("sample/a_test.go", ResourceFixedSleep, false),
		// Tagged: governed by the scope=all audit rows only.
		occurrence("sample/tagged_test.go", ResourceSubprocess, true),
		// Governed by no counted row: cwd outside cmd/gc, and tagged environment.
		occurrence("sample/a_test.go", ResourceCWD, false),
		occurrence("sample/tagged_test.go", ResourceEnvironment, true),
		// cmd/gc untagged cwd is governed.
		occurrence("cmd/gc/x_test.go", ResourceCWD, false),
	}}
	ledger := validLedger()

	records, err := GenerateCounts(ledger, census)
	if err != nil {
		t.Fatalf("GenerateCounts: %v", err)
	}
	want := []CountRecord{
		{Path: "cmd/gc/x_test.go", Resource: ResourceCWD, Calls: 1},
		{Path: "sample/a_test.go", Resource: ResourceFixedSleep, Calls: 1},
		{Path: "sample/a_test.go", Resource: ResourceSubprocess, Calls: 2},
		{Path: "sample/tagged_test.go", Resource: ResourceSubprocess, Calls: 1, Tagged: true},
	}
	if fmt.Sprint(records) != fmt.Sprint(want) {
		t.Fatalf("GenerateCounts =\n%+v\nwant\n%+v", records, want)
	}
}

func TestGenerateCountsSortsBytewiseByPathThenResource(t *testing.T) {
	t.Parallel()

	census := Census{Occurrences: []Occurrence{
		occurrence("cmd/gc/a_test.go", ResourceSubprocess, false),
		occurrence("cmd/gc-write-mint/e2e_test.go", ResourceSubprocess, true),
		occurrence("cmd/gc/a_test.go", ResourceFixedSleep, false),
		occurrence("B/upper_test.go", ResourceSubprocess, false),
	}}
	records, err := GenerateCounts(validLedger(), census)
	if err != nil {
		t.Fatalf("GenerateCounts: %v", err)
	}
	var got []string
	for _, record := range records {
		got = append(got, record.Path+" "+string(record.Resource))
	}
	want := []string{
		"B/upper_test.go subprocess",
		"cmd/gc-write-mint/e2e_test.go subprocess", // '-' (0x2d) sorts before '/' (0x2f)
		"cmd/gc/a_test.go fixed_sleep",
		"cmd/gc/a_test.go subprocess",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("order =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

func TestGenerateCountsMediumFollowsTheSmallCountRule(t *testing.T) {
	t.Parallel()

	census := Census{
		Runnables: []RunnableOwner{
			{PackageDir: "sample", PackageName: "sample", Owner: "TestMedium"},
			{PackageDir: "sample", PackageName: "sample", Owner: "TestSmall"},
		},
		Occurrences: []Occurrence{
			runnableOccurrence("sample/a_test.go", "TestMedium", ResourceSubprocess),
			runnableOccurrence("sample/a_test.go", "TestMedium", ResourceSubprocess),
			runnableOccurrence("sample/a_test.go", "TestSmall", ResourceSubprocess),
			// Helper (non-runnable) occurrences are never medium-owned.
			{Path: "sample/a_test.go", PackageDir: "sample", PackageName: "sample", Owner: "TestMedium", Resource: ResourceSubprocess},
			// Medium declares subprocess only: other resources stay small.
			runnableOccurrence("sample/a_test.go", "TestMedium", ResourceFixedSleep),
		},
	}
	ledger := validLedger()
	ledger.Medium = []MediumOwner{validMediumOwner("sample", "sample", "TestMedium", ResourceSubprocess)}

	records, err := GenerateCounts(ledger, census)
	if err != nil {
		t.Fatalf("GenerateCounts: %v", err)
	}
	want := []CountRecord{
		{Path: "sample/a_test.go", Resource: ResourceFixedSleep, Calls: 1},
		{Path: "sample/a_test.go", Resource: ResourceSubprocess, Calls: 4, Medium: 2},
	}
	if fmt.Sprint(records) != fmt.Sprint(want) {
		t.Fatalf("GenerateCounts =\n%+v\nwant\n%+v", records, want)
	}
}

func TestGenerateCountsRejectsPathsTheFormatCannotRender(t *testing.T) {
	t.Parallel()

	for name, path := range map[string]string{
		"double quote": `sample/a"b_test.go`,
		"backslash":    `sample/a\b_test.go`,
		"newline":      "sample/a\nb_test.go",
		"tab":          "sample/a\tb_test.go",
		"delete":       "sample/a\x7fb_test.go",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			census := Census{Occurrences: []Occurrence{occurrence(path, ResourceSubprocess, false)}}
			_, err := GenerateCounts(validLedger(), census)
			requireErrorContains(t, err, "path")
		})
	}
}

// ---- derivation -----------------------------------------------------------

func TestDerivedCountsEqualTheScanForEveryCountedRow(t *testing.T) {
	t.Parallel()

	census := Census{
		Runnables: []RunnableOwner{{PackageDir: "cmd/gc", PackageName: "gc", Owner: "TestMedium"}},
		Occurrences: []Occurrence{
			runnableOccurrence("cmd/gc/a_test.go", "TestMedium", ResourceSubprocess),
			runnableOccurrence("cmd/gc/a_test.go", "TestSmall", ResourceSubprocess),
			occurrence("cmd/gc/a_test.go", ResourceSubprocess, false),
			occurrence("cmd/gc/b_test.go", ResourceSubprocess, false),
			occurrence("cmd/gc/b_test.go", ResourceEnvironment, false),
			occurrence("cmd/gc/c_test.go", ResourceFixedSleep, false),
			occurrence("other/d_test.go", ResourceSubprocess, true),
			occurrence("other/e_test.go", ResourceSubprocess, false),
		},
	}
	census.Occurrences[0].PackageDir, census.Occurrences[0].PackageName = "cmd/gc", "gc"
	census.Occurrences[1].PackageDir, census.Occurrences[1].PackageName = "cmd/gc", "gc"
	ledger := validLedger()
	ledger.Medium = []MediumOwner{validMediumOwner("cmd/gc", "gc", "TestMedium", ResourceSubprocess)}

	records, err := GenerateCounts(ledger, census)
	if err != nil {
		t.Fatalf("GenerateCounts: %v", err)
	}
	assertDerivedRowsMatchScan(t, ledger, census, records)
}

func TestDerivedSmallCountsDropMediumFromCallsAndFiles(t *testing.T) {
	t.Parallel()

	records := []CountRecord{
		{Path: "sample/a_test.go", Resource: ResourceSubprocess, Calls: 3, Medium: 3}, // all medium: not a Small file
		{Path: "sample/b_test.go", Resource: ResourceSubprocess, Calls: 4, Medium: 1},
		{Path: "sample/c_test.go", Resource: ResourceSubprocess, Calls: 2},
		{Path: "sample/t_test.go", Resource: ResourceSubprocess, Calls: 9, Tagged: true},
	}
	if got, want := DerivedCount(records, ScopeUntagged, ResourceSubprocess), (Count{Calls: 9, Files: 3}); got != want {
		t.Fatalf("DerivedCount(untagged) = %+v, want %+v", got, want)
	}
	if got, want := DerivedCount(records, ScopeAll, ResourceSubprocess), (Count{Calls: 18, Files: 4}); got != want {
		t.Fatalf("DerivedCount(all) = %+v, want %+v", got, want)
	}
	if got, want := DerivedSmallCount(records, ScopeUntagged, ResourceSubprocess), (Count{Calls: 5, Files: 2}); got != want {
		t.Fatalf("DerivedSmallCount(untagged) = %+v, want %+v", got, want)
	}
}

// assertDerivedRowsMatchScan is the permanent form of the reproduction the
// bead demanded before the stored fields were deleted: the per-file records
// reproduce Census.Count for every audit and debt row, and Census.SmallCount
// for every small_debt row.
func assertDerivedRowsMatchScan(t *testing.T, ledger Ledger, census Census, records []CountRecord) {
	t.Helper()
	rows := 0
	for _, row := range append(append([]Baseline(nil), ledger.AuditBaseline...), ledger.Debt...) {
		rows++
		if got, want := DerivedCount(records, row.Scope, row.Resource), census.Count(row.Scope, row.Resource); got != want {
			t.Errorf("scope=%s resource=%s: derived %+v, scan %+v", row.Scope, row.Resource, got, want)
		}
	}
	for _, row := range ledger.SmallDebt {
		rows++
		if got, want := DerivedSmallCount(records, row.Scope, row.Resource), census.SmallCount(row.Scope, row.Resource, ledger.Medium); got != want {
			t.Errorf("small scope=%s resource=%s: derived %+v, scan %+v", row.Scope, row.Resource, got, want)
		}
	}
	if rows == 0 {
		t.Fatal("ledger has no counted rows to compare")
	}
}

// ---- validator ------------------------------------------------------------

func countsPair(t *testing.T, stored, scanned Census, ledger Ledger) (string, Census) {
	t.Helper()
	records, err := GenerateCounts(ledger, stored)
	if err != nil {
		t.Fatalf("GenerateCounts(stored): %v", err)
	}
	return RenderCounts(records), scanned
}

func TestValidateCountsAcceptsAMatchingFile(t *testing.T) {
	t.Parallel()

	census := Census{Occurrences: []Occurrence{
		occurrence("sample/a_test.go", ResourceSubprocess, false),
		occurrence("sample/tagged_test.go", ResourceSubprocess, true),
	}}
	ledger := validLedger()
	data, _ := countsPair(t, census, census, ledger)
	if err := ValidateCounts(ledger, census, []byte(data)); err != nil {
		t.Fatalf("ValidateCounts: %v", err)
	}
}

func TestValidateCountsReportsGrowthAndStalenessPerRecord(t *testing.T) {
	t.Parallel()

	stored := Census{Occurrences: []Occurrence{
		occurrence("cmd/gc/x_test.go", ResourceSubprocess, false),
		occurrence("cmd/gc/x_test.go", ResourceSubprocess, false),
		occurrence("cmd/gc/x_test.go", ResourceSubprocess, false),
		occurrence("cmd/gc/x_test.go", ResourceSubprocess, false),
		occurrence("cmd/gc/y_test.go", ResourceSubprocess, false),
		occurrence("cmd/gc/y_test.go", ResourceSubprocess, false),
		occurrence("cmd/gc/y_test.go", ResourceSubprocess, false),
		occurrence("cmd/gc/y_test.go", ResourceSubprocess, false),
	}}
	ledger := validLedger()

	grown := Census{}
	grown.Occurrences = append(grown.Occurrences, stored.Occurrences...)
	grown.Occurrences = append(grown.Occurrences, occurrence("cmd/gc/x_test.go", ResourceSubprocess, false)) // x: 4 -> 5

	t.Run("grew", func(t *testing.T) {
		t.Parallel()
		data, scanned := countsPair(t, stored, grown, ledger)
		err := ValidateCounts(ledger, scanned, []byte(data))
		requireErrorContains(t, err, "source resource census grew: path=cmd/gc/x_test.go resource=subprocess calls=5 (baseline 4)")
		requireErrorContains(t, err, "Small resource census grew: path=cmd/gc/x_test.go resource=subprocess small=5 (baseline 4)")
		if strings.Contains(err.Error(), "path=cmd/gc/y_test.go") {
			t.Fatalf("an unchanged record was reported: %v", err)
		}
		requireFailureEnding(t, err)
	})

	t.Run("stale", func(t *testing.T) {
		t.Parallel()
		shrunk := Census{Occurrences: stored.Occurrences[1:]} // x: 4 -> 3
		data, scanned := countsPair(t, stored, shrunk, ledger)
		err := ValidateCounts(ledger, scanned, []byte(data))
		requireErrorContains(t, err, "source resource census baseline is stale: path=cmd/gc/x_test.go resource=subprocess calls=3 (baseline 4)")
		requireErrorContains(t, err, "Small resource census baseline is stale: path=cmd/gc/x_test.go resource=subprocess small=3 (baseline 4)")
		requireFailureEnding(t, err)
	})
}

func TestValidateCountsReportsMissingAndExtraRecords(t *testing.T) {
	t.Parallel()

	with := Census{Occurrences: []Occurrence{
		occurrence("sample/a_test.go", ResourceSubprocess, false),
		occurrence("sample/new_test.go", ResourceSubprocess, false),
		occurrence("sample/new_test.go", ResourceSubprocess, false),
	}}
	without := Census{Occurrences: []Occurrence{occurrence("sample/a_test.go", ResourceSubprocess, false)}}
	ledger := validLedger()

	t.Run("record missing from the file is growth from zero", func(t *testing.T) {
		t.Parallel()
		data, scanned := countsPair(t, without, with, ledger)
		err := ValidateCounts(ledger, scanned, []byte(data))
		requireErrorContains(t, err, "source resource census grew: path=sample/new_test.go resource=subprocess calls=2 (baseline 0)")
		requireFailureEnding(t, err)
	})

	t.Run("record the scan no longer has is stale at zero", func(t *testing.T) {
		t.Parallel()
		data, scanned := countsPair(t, with, without, ledger)
		err := ValidateCounts(ledger, scanned, []byte(data))
		requireErrorContains(t, err, "source resource census baseline is stale: path=sample/new_test.go resource=subprocess calls=0 (baseline 2)")
		requireFailureEnding(t, err)
	})
}

func TestValidateCountsReportsATaggedMismatch(t *testing.T) {
	t.Parallel()

	stored := Census{Occurrences: []Occurrence{occurrence("sample/a_test.go", ResourceSubprocess, true)}}
	scanned := Census{Occurrences: []Occurrence{occurrence("sample/a_test.go", ResourceSubprocess, false)}}
	data, scanned := countsPair(t, stored, scanned, validLedger())
	err := ValidateCounts(validLedger(), scanned, []byte(data))
	requireErrorContains(t, err, "tagged mismatch: path=sample/a_test.go resource=subprocess tagged=false (baseline true)")
	requireFailureEnding(t, err)
}

func TestValidateCountsCatchesAnOccurrenceMovingOutOfAMediumTest(t *testing.T) {
	t.Parallel()

	ledger := validLedger()
	ledger.Medium = []MediumOwner{validMediumOwner("sample", "sample", "TestMedium", ResourceSubprocess)}
	stored := Census{Occurrences: []Occurrence{
		runnableOccurrence("sample/x_test.go", "TestMedium", ResourceSubprocess),
		runnableOccurrence("sample/x_test.go", "TestSmall", ResourceSubprocess),
	}}
	// Same file, same total (2 calls): one occurrence moved from the
	// Medium-declared test to an undeclared one.
	scanned := Census{Occurrences: []Occurrence{
		runnableOccurrence("sample/x_test.go", "TestSmall", ResourceSubprocess),
		runnableOccurrence("sample/x_test.go", "TestOther", ResourceSubprocess),
	}}
	data, scanned := countsPair(t, stored, scanned, ledger)

	err := ValidateCounts(ledger, scanned, []byte(data))
	requireErrorContains(t, err, "Small resource census grew: path=sample/x_test.go resource=subprocess small=2 (baseline 1)")
	if strings.Contains(err.Error(), "source resource census grew") || strings.Contains(err.Error(), "source resource census baseline is stale") {
		t.Fatalf("the unchanged raw total was reported: %v", err)
	}
	requireFailureEnding(t, err)
}

func TestValidateCountsPrintsAggregateLinesBeforeRecordLines(t *testing.T) {
	t.Parallel()

	stored := Census{Occurrences: []Occurrence{
		occurrence("sample/a_test.go", ResourceSubprocess, false),
		occurrence("sample/b_test.go", ResourceSubprocess, false),
	}}
	scanned := Census{Occurrences: append(append([]Occurrence(nil), stored.Occurrences...),
		occurrence("sample/a_test.go", ResourceSubprocess, false))}
	data, scanned := countsPair(t, stored, scanned, validLedger())

	err := ValidateCounts(validLedger(), scanned, []byte(data))
	msg := err.Error()
	aggregate := "source resource census grew: scope=all resource=subprocess calls=3 (baseline 2), files=2 (baseline 2)"
	record := "source resource census grew: path=sample/a_test.go resource=subprocess calls=2 (baseline 1)"
	regenerate := "\n" + wantRegenerateLine
	small := "Small resource census grew: scope=untagged resource=subprocess calls=3 (baseline 2), files=2 (baseline 2)"
	for _, want := range []string{aggregate, small, record} {
		requireErrorContains(t, err, want)
	}
	if strings.Index(msg, aggregate) >= strings.Index(msg, record) || strings.Index(msg, small) >= strings.Index(msg, record) {
		t.Fatalf("aggregate lines must precede per-record lines:\n%s", msg)
	}
	if strings.Index(msg, record) > strings.Index(msg, regenerate) {
		t.Fatalf("per-record lines must precede the regenerate line:\n%s", msg)
	}
	// Aggregate lines are derived from the stored records, not from a count
	// the file never held.
	staleAggregate := "source resource census baseline is stale: scope=all resource=subprocess"
	if strings.Contains(msg, staleAggregate) {
		t.Fatalf("unexpected stale aggregate line:\n%s", msg)
	}
}

func TestValidateCountsCapsRecordLinesAtTwenty(t *testing.T) {
	t.Parallel()

	// Tagged files are governed by the scope=all audit rows only, so each
	// grown file yields exactly one per-record line.
	scanned := Census{}
	for i := 0; i < 25; i++ {
		scanned.Occurrences = append(scanned.Occurrences, occurrence(fmt.Sprintf("sample/f%02d_test.go", i), ResourceSubprocess, true))
	}
	data, scanned := countsPair(t, Census{}, scanned, validLedger())
	err := ValidateCounts(validLedger(), scanned, []byte(data))
	if err == nil {
		t.Fatal("ValidateCounts accepted 25 missing records")
	}
	lines := 0
	for _, line := range strings.Split(err.Error(), "\n") {
		if strings.HasPrefix(line, "source resource census grew: path=") {
			lines++
		}
	}
	if lines != 20 {
		t.Fatalf("per-record lines = %d, want 20:\n%v", lines, err)
	}
	requireErrorContains(t, err, "... and 5 more")
	requireErrorContains(t, err, "source resource census grew: scope=all resource=subprocess calls=25 (baseline 0), files=25 (baseline 0)")
	requireFailureEnding(t, err)
}

func TestValidateCountsReportsNonCanonicalFormLast(t *testing.T) {
	t.Parallel()

	census := Census{Occurrences: []Occurrence{occurrence("sample/a_test.go", ResourceSubprocess, false)}}
	ledger := validLedger()
	data, _ := countsPair(t, census, census, ledger)

	t.Run("canonical-only problem", func(t *testing.T) {
		t.Parallel()
		err := ValidateCounts(ledger, census, []byte(strings.Replace(data, "calls = 1\n", "calls = 1 \n", 1)))
		requireErrorContains(t, err, "non-canonical")
		requireFailureEnding(t, err)
	})

	t.Run("counts are compared before the bytes are judged", func(t *testing.T) {
		t.Parallel()
		grown := Census{Occurrences: append(append([]Occurrence(nil), census.Occurrences...),
			occurrence("sample/a_test.go", ResourceSubprocess, false))}
		err := ValidateCounts(ledger, grown, []byte(strings.Replace(data, "calls = 1\n", "calls = 1 \n", 1)))
		requireErrorContains(t, err, "source resource census grew: path=sample/a_test.go")
		requireErrorContains(t, err, "non-canonical")
		msg := err.Error()
		if strings.Index(msg, "source resource census grew") > strings.Index(msg, "non-canonical") {
			t.Fatalf("the canonical-bytes check must come last:\n%s", msg)
		}
	})
}

func TestValidateCountsParseFailuresCarryTheRegenerateLineAndProtocol(t *testing.T) {
	t.Parallel()

	census := Census{Occurrences: []Occurrence{occurrence("cmd/gc/x_test.go", ResourceSubprocess, false)}}
	for _, tt := range malformedCounts {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := ValidateCounts(validLedger(), census, []byte(tt.data))
			requireErrorContains(t, err, tt.want)
			requireFailureEnding(t, err)
		})
	}

	t.Run("conflict markers", func(t *testing.T) {
		t.Parallel()
		data := strings.Replace(countsFixtureTwoRecords, "\n[[count]]\npath = \"cmd/gc/", "\n<<<<<<< HEAD\n[[count]]\npath = \"cmd/gc/", 1)
		err := ValidateCounts(validLedger(), census, []byte(data))
		requireErrorContains(t, err, "take main's copy of "+wantCountsPath)
		requireFailureEnding(t, err)
	})
}

// requireFailureEnding asserts the message contract every failure shares: the
// regenerate line, then exactly one rebase-protocol paragraph, nothing after.
func requireFailureEnding(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("expected a failure, got nil")
	}
	msg := err.Error()
	tail := "\n" + wantRegenerateLine + "\n\n" + rebaseProtocol
	if !strings.HasSuffix(msg, tail) {
		t.Fatalf("failure does not end with the regenerate line and the protocol paragraph:\n%s", msg)
	}
	if got := strings.Count(msg, rebaseProtocol); got != 1 {
		t.Fatalf("protocol paragraph printed %d times, want 1:\n%s", got, msg)
	}
	if got := strings.Count(msg, "regenerate with:"); got != 1 {
		t.Fatalf("regenerate line printed %d times, want 1:\n%s", got, msg)
	}
}

func TestStructuralFailuresCarryTheRegenerateLineAndProtocol(t *testing.T) {
	t.Parallel()

	policy := validLedger()
	drifted := cloneLedger(policy)
	drifted.Debt[0].OwnerBead = "ga-other"
	err := validateAgainstPolicy(policy, drifted, Census{})
	requireErrorContains(t, err, `owner_bead = "ga-other"`)
	requireFailureEnding(t, err)

	wrongVersion := cloneLedger(policy)
	wrongVersion.Version = 2
	requireFailureEnding(t, validateAgainstPolicy(policy, wrongVersion, Census{}))
}

func TestRebaseProtocolIsOneParagraphWithFourNamedSteps(t *testing.T) {
	t.Parallel()

	text := rebaseProtocol
	if strings.Contains(text, "\n\n") || strings.HasSuffix(text, "\n") {
		t.Fatalf("protocol must be one paragraph without a trailing newline: %q", text)
	}
	for step, wants := range map[string][]string{
		"(1)": {"discard", "bootstrapPolicy", "policy.go", "TESTING.md", "main's versions"},
		"(2)": {"[[medium]]", "[[reviewed_hermetic_body]]", "by hand"},
		"(3)": {wantRegenerateCommand},
		"(4)": {wantCountsPath},
	} {
		if !strings.Contains(text, step) {
			t.Fatalf("protocol does not carry step %s:\n%s", step, text)
		}
		for _, want := range wants {
			if !strings.Contains(text, want) {
				t.Errorf("protocol step %s must name %q:\n%s", step, want, text)
			}
		}
	}
	if !strings.Contains(text, wantCountsPath) || !strings.Contains(text, wantDocSection) {
		t.Errorf("protocol must point at the counts file and the TESTING.md section %q:\n%s", wantDocSection, text)
	}
	if !strings.Contains(text, "hand-merge") {
		t.Errorf("protocol must forbid hand-merging:\n%s", text)
	}
	// The autofix and operators match these on the log; the protocol must not
	// look like a census failure.
	forbidden := []string{
		"source resource census grew:",
		"source resource census baseline is stale:",
		"Small resource census grew:",
		"Small resource census baseline is stale:",
		"baseline is stale",
		"census grew",
		"scope=",
		"(baseline ",
	}
	forbidden = append(forbidden, removedLedgerKeys...)
	for _, bad := range forbidden {
		if strings.Contains(text, bad) {
			t.Errorf("protocol contains %q", bad)
		}
	}
	if regenerateLine != wantRegenerateLine {
		t.Errorf("regenerateLine = %q\nwant %q", regenerateLine, wantRegenerateLine)
	}
}

// ---- the old per-row keys -------------------------------------------------

func TestParseLedgerRejectsTheRemovedPerRowCounts(t *testing.T) {
	t.Parallel()

	for _, key := range removedLedgerKeys {
		t.Run(key, func(t *testing.T) {
			t.Parallel()
			_, err := ParseLedger([]byte("version = 3\n\n[[debt]]\nscope = \"untagged\"\nresource = \"subprocess\"\n" + key + " = 5\n"))
			requireErrorContains(t, err, key)
			requireErrorContains(t, err, wantCountsPath)
			requireErrorContains(t, err, "policy version 3")
			requireFailureEnding(t, err)
		})
	}

	t.Run("an unrelated unknown field keeps the plain error", func(t *testing.T) {
		t.Parallel()
		_, err := ParseLedger([]byte("version = 3\n\n[[debt]]\nscope = \"untagged\"\nbogus = 5\n"))
		requireErrorContains(t, err, "unknown ledger field: debt.bogus")
		if strings.Contains(err.Error(), rebaseProtocol) {
			t.Fatalf("protocol attached to an unrelated parse failure: %v", err)
		}
	})

	t.Run("a version 3 ledger without the keys parses", func(t *testing.T) {
		t.Parallel()
		if _, err := ParseLedger([]byte("version = 3\n\n[[debt]]\nscope = \"untagged\"\nresource = \"subprocess\"\n")); err != nil {
			t.Fatalf("ParseLedger: %v", err)
		}
	})
}

func TestPolicyIsVersionThree(t *testing.T) {
	t.Parallel()

	if bootstrapPolicy.Version != 3 {
		t.Fatalf("bootstrapPolicy.Version = %d, want 3", bootstrapPolicy.Version)
	}
}

// ---- row order ------------------------------------------------------------

func TestValidateRequiresSortedMediumRowsAndNamesTheFirstOutOfOrderRow(t *testing.T) {
	t.Parallel()

	census := Census{Runnables: []RunnableOwner{
		{PackageDir: "alpha", PackageName: "alpha", Owner: "TestA"},
		{PackageDir: "beta", PackageName: "beta", Owner: "TestB"},
		{PackageDir: "gamma", PackageName: "gamma", Owner: "TestC"},
	}}
	policy := validLedger()
	policy.Medium = []MediumOwner{
		validMediumOwner("alpha", "alpha", "TestA", ResourceSubprocess),
		validMediumOwner("beta", "beta", "TestB", ResourceSubprocess),
		validMediumOwner("gamma", "gamma", "TestC", ResourceSubprocess),
	}
	if err := validateAgainstPolicy(policy, cloneLedger(policy), census); err != nil {
		t.Fatalf("sorted medium rows rejected: %v", err)
	}

	ledger := cloneLedger(policy)
	ledger.Medium[0], ledger.Medium[1] = ledger.Medium[1], ledger.Medium[0] // beta, alpha, gamma
	err := validateAgainstPolicy(policy, ledger, census)
	requireErrorContains(t, err, "medium rows are not sorted by (package_dir, package_name, owner)")
	requireErrorContains(t, err, "package_dir=alpha package_name=alpha owner=TestA")
	if strings.Contains(err.Error(), "package_dir=gamma package_name=gamma owner=TestC: ") {
		t.Fatalf("a row after the first out-of-order row was named: %v", err)
	}
	requireFailureEnding(t, err)
}

func TestValidateRequiresSortedReviewedHermeticRows(t *testing.T) {
	t.Parallel()

	first, second := validReviewedHermeticBody("TestA"), validReviewedHermeticBody("TestB")
	first.PackageName, second.PackageName = "alpha", "beta"
	first.PackageDir, second.PackageDir = "alpha", "beta"
	problems := validateReviewedHermeticRowOrder([]ReviewedHermeticBody{second, first})
	if len(problems) != 1 || !strings.Contains(problems[0], "reviewed_hermetic_body rows are not sorted by (package_dir, package_name, owner)") ||
		!strings.Contains(problems[0], "package_dir=alpha package_name=alpha owner=TestA") {
		t.Fatalf("problems = %v", problems)
	}
	if problems := validateReviewedHermeticRowOrder([]ReviewedHermeticBody{first, second}); len(problems) != 0 {
		t.Fatalf("sorted rows rejected: %v", problems)
	}
}

// ---- -update --------------------------------------------------------------

const testingDocWithBlock = "# TESTING\n\npreamble\n\n" + markdownBegin + "\nstale\n" + markdownEnd + "\n\nepilogue\n"

func updateTree(t *testing.T, doc string) string {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "test"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "TESTING.md"), []byte(doc), 0o644); err != nil {
		t.Fatal(err)
	}
	return root
}

func updateCensus() Census {
	return Census{Occurrences: []Occurrence{
		occurrence("sample/a_test.go", ResourceSubprocess, false),
		occurrence("sample/a_test.go", ResourceSubprocess, false),
		occurrence("sample/t_test.go", ResourceSubprocess, true),
	}}
}

func readFile(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestUpdateGeneratedWritesTheCountsFileAndTheDocumentationBlock(t *testing.T) {
	root := updateTree(t, testingDocWithBlock)
	policy := validLedger()
	census := updateCensus()

	if err := updateGenerated(policy, root, cloneLedger(policy), census); err != nil {
		t.Fatalf("updateGenerated: %v", err)
	}
	records, err := GenerateCounts(policy, census)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := readFile(t, filepath.Join(root, "test", "test-resource-counts.toml")), RenderCounts(records); got != want {
		t.Fatalf("counts file =\n%s\nwant\n%s", got, want)
	}
	wantDoc := "# TESTING\n\npreamble\n\n" + RenderMarkdown(policy) + "\n\nepilogue\n"
	if got := readFile(t, filepath.Join(root, "TESTING.md")); got != wantDoc {
		t.Fatalf("TESTING.md =\n%s\nwant\n%s", got, wantDoc)
	}
	if err := ValidateCounts(policy, census, []byte(readFile(t, filepath.Join(root, "test", "test-resource-counts.toml")))); err != nil {
		t.Fatalf("the generated file does not validate: %v", err)
	}
}

func TestUpdateGeneratedIsIdempotent(t *testing.T) {
	root := updateTree(t, testingDocWithBlock)
	policy := validLedger()
	census := updateCensus()
	counts := filepath.Join(root, "test", "test-resource-counts.toml")
	doc := filepath.Join(root, "TESTING.md")

	if err := updateGenerated(policy, root, cloneLedger(policy), census); err != nil {
		t.Fatal(err)
	}
	firstCounts, firstDoc := readFile(t, counts), readFile(t, doc)
	old := time.Unix(1_000_000_000, 0)
	for _, name := range []string{counts, doc} {
		if err := os.Chtimes(name, old, old); err != nil {
			t.Fatal(err)
		}
	}

	if err := updateGenerated(policy, root, cloneLedger(policy), census); err != nil {
		t.Fatal(err)
	}
	if readFile(t, counts) != firstCounts || readFile(t, doc) != firstDoc {
		t.Fatal("a second update changed the bytes")
	}
	for _, name := range []string{counts, doc} {
		info, err := os.Stat(name)
		if err != nil {
			t.Fatal(err)
		}
		if !info.ModTime().Equal(old) {
			t.Fatalf("%s was rewritten although its bytes did not differ (mtime %v)", name, info.ModTime())
		}
	}
	entries, err := os.ReadDir(filepath.Join(root, "test"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("temp files left behind: %v", entries)
	}
}

func TestUpdateGeneratedWritesAtomicallyThroughATempFileAndRename(t *testing.T) {
	root := updateTree(t, testingDocWithBlock)
	policy := validLedger()
	census := updateCensus()
	counts := filepath.Join(root, "test", "test-resource-counts.toml")
	if err := os.WriteFile(counts, []byte("previous\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	original := renameFile
	t.Cleanup(func() { renameFile = original })

	t.Run("renames a same-directory temp file over the target", func(t *testing.T) {
		var calls [][2]string
		renameFile = func(oldPath, newPath string) error {
			calls = append(calls, [2]string{oldPath, newPath})
			if _, err := os.Stat(oldPath); err != nil {
				t.Errorf("temp file %s missing at rename time: %v", oldPath, err)
			}
			if newPath == counts {
				if got := readFile(t, counts); got != "previous\n" {
					t.Errorf("target changed before the rename: %q", got)
				}
			}
			return original(oldPath, newPath)
		}
		if err := updateGenerated(policy, root, cloneLedger(policy), census); err != nil {
			t.Fatal(err)
		}
		var countsRenames [][2]string
		for _, call := range calls {
			if call[1] == counts {
				countsRenames = append(countsRenames, call)
			}
		}
		if len(countsRenames) != 1 {
			t.Fatalf("renames onto the counts file = %v, want exactly one", calls)
		}
		if filepath.Dir(countsRenames[0][0]) != filepath.Dir(counts) || countsRenames[0][0] == counts {
			t.Fatalf("temp file %s is not a distinct file in the target's directory", countsRenames[0][0])
		}
	})

	t.Run("a failed rename leaves the previous file and no temp file", func(t *testing.T) {
		if err := os.WriteFile(counts, []byte("previous\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		renameFile = func(string, string) error { return fmt.Errorf("injected rename failure") }
		err := updateGenerated(policy, root, cloneLedger(policy), census)
		requireErrorContains(t, err, "injected rename failure")
		if got := readFile(t, counts); got != "previous\n" {
			t.Fatalf("counts file = %q, want it untouched", got)
		}
		entries, readErr := os.ReadDir(filepath.Dir(counts))
		if readErr != nil {
			t.Fatal(readErr)
		}
		if len(entries) != 1 {
			t.Fatalf("temp files left behind: %v", entries)
		}
	})
}

func TestUpdateGeneratedRefusesOnStructuralFailureBeforeWritingAnything(t *testing.T) {
	root := updateTree(t, testingDocWithBlock)
	policy := validLedger()
	drifted := cloneLedger(policy)
	drifted.Debt[0].OwnerBead = "ga-other"

	err := updateGenerated(policy, root, drifted, updateCensus())
	requireErrorContains(t, err, `owner_bead = "ga-other"`)
	requireFailureEnding(t, err)
	if _, statErr := os.Stat(filepath.Join(root, "test", "test-resource-counts.toml")); !os.IsNotExist(statErr) {
		t.Fatalf("counts file written despite a structural failure: %v", statErr)
	}
	if got := readFile(t, filepath.Join(root, "TESTING.md")); got != testingDocWithBlock {
		t.Fatalf("TESTING.md rewritten despite a structural failure:\n%s", got)
	}
}

func TestUpdateGeneratedRefusesOnAScanProblemBeforeWritingAnything(t *testing.T) {
	root := updateTree(t, testingDocWithBlock)
	policy := validLedger()
	census := Census{Occurrences: []Occurrence{occurrence("sample/a\"b_test.go", ResourceSubprocess, false)}}

	err := updateGenerated(policy, root, cloneLedger(policy), census)
	requireErrorContains(t, err, "path")
	if _, statErr := os.Stat(filepath.Join(root, "test", "test-resource-counts.toml")); !os.IsNotExist(statErr) {
		t.Fatalf("counts file written despite a scan problem: %v", statErr)
	}
	if got := readFile(t, filepath.Join(root, "TESTING.md")); got != testingDocWithBlock {
		t.Fatalf("TESTING.md rewritten despite a scan problem:\n%s", got)
	}
}

func TestUpdateGeneratedWritesNothingWhenTheDocumentationBlockIsMissing(t *testing.T) {
	root := updateTree(t, "# TESTING\n\nno markers here\n")
	policy := validLedger()

	err := updateGenerated(policy, root, cloneLedger(policy), updateCensus())
	requireErrorContains(t, err, "marker pair")
	if _, statErr := os.Stat(filepath.Join(root, "test", "test-resource-counts.toml")); !os.IsNotExist(statErr) {
		t.Fatalf("counts file written although TESTING.md could not be updated: %v", statErr)
	}
}

func TestUpdateGeneratedReplacesAFileWithConflictMarkers(t *testing.T) {
	root := updateTree(t, testingDocWithBlock)
	policy := validLedger()
	census := updateCensus()
	counts := filepath.Join(root, "test", "test-resource-counts.toml")
	if err := os.WriteFile(counts, []byte("<<<<<<< HEAD\nversion = 1\n=======\n>>>>>>> branch\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := updateGenerated(policy, root, cloneLedger(policy), census); err != nil {
		t.Fatalf("updateGenerated must regenerate over a conflicted counts file: %v", err)
	}
	if err := ValidateCounts(policy, census, []byte(readFile(t, counts))); err != nil {
		t.Fatalf("regenerated file does not validate: %v", err)
	}
}

func TestUpdateGeneratedSaysWhenTheSourceTreeIsReadOnly(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("directory permissions do not bind root")
	}
	root := updateTree(t, testingDocWithBlock)
	dir := filepath.Join(root, "test")
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })
	policy := validLedger()

	err := updateGenerated(policy, root, cloneLedger(policy), updateCensus())
	requireErrorContains(t, err, "read-only")
	requireErrorContains(t, err, wantRegenerateCommand)
}

// ---- documentation and the real repository --------------------------------

func TestTestingMDCarriesTheSectionTheFailureMessagePointsAt(t *testing.T) {
	t.Parallel()

	doc := readFile(t, filepath.Join(repositoryRoot(t), "TESTING.md"))
	if !strings.Contains(rebaseProtocol, `section "`+wantDocSection+`"`) {
		t.Fatalf("the protocol must name the TESTING.md section %q", wantDocSection)
	}
	if !hasHeading(doc, "## ", wantDocSection) {
		t.Fatalf("TESTING.md has no %q heading, but the failure message points at it", "## "+wantDocSection)
	}
	section := sectionBody(doc, "## "+wantDocSection)
	if !hasHeading(section, "### ", wantDocSubsection) {
		t.Fatalf("section %q has no %q subsection", wantDocSection, "### "+wantDocSubsection)
	}
	subsection := sectionBody(section, "### "+wantDocSubsection)
	for _, want := range []string{
		wantRegenerateCommand,
		wantCountsPath,
		"hand-merge",
		"a leftover per-row count", // the compile-error sentence
		"-update",
		"banks", // -update banks growth with no friction; the diff is the review surface
		"[[medium]]",
		"[[reviewed_hermetic_body]]",
	} {
		if !strings.Contains(subsection, want) {
			t.Errorf("subsection %q does not mention %q", wantDocSubsection, want)
		}
	}
	for _, key := range removedLedgerKeys {
		if strings.Contains(doc, key) {
			t.Errorf("TESTING.md names the removed key %q; say \"a leftover per-row count\" instead", key)
		}
	}
}

func hasHeading(doc, prefix, title string) bool {
	for _, line := range strings.Split(doc, "\n") {
		if line == prefix+title {
			return true
		}
	}
	return false
}

// sectionBody returns the text from heading to the next heading of the same
// or a shallower level.
func sectionBody(doc, heading string) string {
	start := strings.Index(doc, "\n"+heading+"\n")
	if start < 0 {
		return ""
	}
	body := doc[start+1:]
	level := len(heading) - len(strings.TrimLeft(heading, "#"))
	lines := strings.Split(body, "\n")
	end := len(lines)
	for i, line := range lines[1:] {
		trimmed := strings.TrimLeft(line, "#")
		if hashes := len(line) - len(trimmed); hashes > 0 && hashes <= level && strings.HasPrefix(trimmed, " ") {
			end = i + 1
			break
		}
	}
	return strings.Join(lines[:end], "\n")
}

func TestRepositoryCountsFileIsCanonicalAndDerivesEveryCountedRow(t *testing.T) {
	root := repositoryRoot(t)
	ledger, err := LoadLedger(filepath.Join(root, "test", "test-resources.toml"))
	if err != nil {
		t.Fatalf("LoadLedger: %v", err)
	}
	census, err := ScanRepository(root)
	if err != nil {
		t.Fatalf("ScanRepository: %v", err)
	}
	data := readFile(t, filepath.Join(root, "test", "test-resource-counts.toml"))
	records, err := ParseCounts([]byte(data))
	if err != nil {
		t.Fatalf("ParseCounts(real file): %v", err)
	}
	if got := RenderCounts(records); got != data {
		t.Fatal("RenderCounts(ParseCounts(f)) != f for the real counts file")
	}
	generated, err := GenerateCounts(ledger, census)
	if err != nil {
		t.Fatalf("GenerateCounts: %v", err)
	}
	if got := RenderCounts(generated); got != data {
		t.Fatalf("the committed counts file is not what -update generates; run: %s", wantRegenerateCommand)
	}
	assertDerivedRowsMatchScan(t, ledger, census, records)
}

func TestCountsSourceIsHermetic(t *testing.T) {
	t.Parallel()

	dir := filepath.Join(repositoryRoot(t), "internal", "testpolicy", "resourcecensus")
	for _, name := range []string{"counts.go", "counts_test.go"} {
		file, err := parser.ParseFile(token.NewFileSet(), filepath.Join(dir, name), nil, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		for _, spec := range file.Imports {
			path, err := strconv.Unquote(spec.Path.Value)
			if err != nil {
				t.Fatal(err)
			}
			switch path {
			case "os/exec":
				t.Errorf("%s imports os/exec: the counts code and its tests start no process", name)
			case "time":
				// counts_test.go sets file mtimes with a fixed instant to
				// prove no rewrite; it never reads the clock (checked below).
				if name != "counts_test.go" {
					t.Errorf("%s imports time: the date check belongs to waiverclock alone", name)
				}
			}
		}
	}
	data, err := os.ReadFile(filepath.Join(dir, "counts.go"))
	if err != nil {
		t.Fatal(err)
	}
	for _, banned := range []string{"time." + "Now", "exec.Command", `"git"`} {
		if strings.Contains(string(data), banned) {
			t.Errorf("counts.go contains %s", banned)
		}
	}
	self, err := os.ReadFile(filepath.Join(dir, "counts_test.go"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(self), "time."+"Now") {
		t.Error("counts_test.go reads the clock")
	}
}
