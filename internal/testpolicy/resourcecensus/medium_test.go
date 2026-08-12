package resourcecensus

import (
	"path"
	"reflect"
	"strings"
	"testing"
	"testing/fstest"
)

func TestScanAttributesResourcesToExactRunnableOwners(t *testing.T) {
	t.Parallel()

	census, err := ScanFS(fstest.MapFS{
		"sample/resources_test.go": &fstest.MapFile{Data: []byte(`package sample
import (
	otherpkg "example.com/testdouble"
	operating "os"
	shell "os/exec"
	testpkg "testing"
	clock "time"
)

func TestMain(m *testpkg.M) { operating.Setenv("MAIN", "1") }
func TestOwned(t *testpkg.T) {
	operating.Setenv("OWNED", "1")
	t.Run("nested", func(t *testpkg.T) { operating.Chdir("nested") })
	helper()
}
func BenchmarkOwned(b *testpkg.B) { operating.Unsetenv("BENCH") }
func FuzzOwned(f *testpkg.F) { operating.Clearenv() }

func helper() {
	clock.Sleep(1)
	shell.Command("worker")
}

type localSuite struct{}
type localT struct{}
func (localSuite) TestMethod(t *testpkg.T) { operating.Setenv("METHOD", "1") }
func Testlowercase(t *testpkg.T) { operating.Setenv("LOWER", "1") }
func TestWrongSignature() { operating.Setenv("WRONG", "1") }
func TestWrongPackage(t *otherpkg.T) { operating.Setenv("WRONG_PACKAGE", "1") }
func TestValueParameter(t testpkg.T) { operating.Setenv("VALUE", "1") }
func TestLocalParameter(t *localT) { operating.Setenv("LOCAL", "1") }
func TestWrongTestingType(t *testpkg.M) { operating.Setenv("WRONG_TESTING_TYPE", "1") }
func TestExtraParameter(t *testpkg.T, extra int) { operating.Setenv("EXTRA", "1") }
func TestResult(t *testpkg.T) bool {
	operating.Setenv("RESULT", "1")
	return false
}
func TestGeneric[T any](t *testpkg.T) { operating.Setenv("GENERIC", "1") }
`)},
		"sample/production.go": &fstest.MapFile{Data: []byte(`package sample
import (
	"os"
	"testing"
)
func TestProduction(t *testing.T) { os.Setenv("PRODUCTION", "1") }
`)},
		"sample/tagged_test.go": &fstest.MapFile{Data: []byte(`//go:build integration

package sample
import (
	"os"
	"testing"
)
func TestTagged(t *testing.T) { os.Setenv("TAGGED", "1") }
`)},
	})
	if err != nil {
		t.Fatalf("ScanFS: %v", err)
	}

	wantRunnables := []RunnableOwner{
		{PackageDir: "sample", PackageName: "sample", Owner: "BenchmarkOwned"},
		{PackageDir: "sample", PackageName: "sample", Owner: "FuzzOwned"},
		{PackageDir: "sample", PackageName: "sample", Owner: "TestMain"},
		{PackageDir: "sample", PackageName: "sample", Owner: "TestOwned"},
		{PackageDir: "sample", PackageName: "sample", Owner: "TestTagged"},
	}
	if !reflect.DeepEqual(census.Runnables, wantRunnables) {
		t.Fatalf("Runnables = %+v, want %+v", census.Runnables, wantRunnables)
	}

	assertOccurrenceOwner(t, census, "sample/resources_test.go", ResourceEnvironment, "TestMain", true, false)
	assertOccurrenceOwner(t, census, "sample/resources_test.go", ResourceEnvironment, "TestOwned", true, false)
	assertOccurrenceOwner(t, census, "sample/resources_test.go", ResourceCWD, "TestOwned", true, false)
	assertOccurrenceOwner(t, census, "sample/resources_test.go", ResourceEnvironment, "BenchmarkOwned", true, false)
	assertOccurrenceOwner(t, census, "sample/resources_test.go", ResourceEnvironment, "FuzzOwned", true, false)
	assertOccurrenceOwner(t, census, "sample/resources_test.go", ResourceFixedSleep, "helper", false, false)
	assertOccurrenceOwner(t, census, "sample/resources_test.go", ResourceSubprocess, "helper", false, false)
	assertOccurrenceOwner(t, census, "sample/resources_test.go", ResourceEnvironment, "TestMethod", false, false)
	assertOccurrenceOwner(t, census, "sample/resources_test.go", ResourceEnvironment, "Testlowercase", false, false)
	assertOccurrenceOwner(t, census, "sample/resources_test.go", ResourceEnvironment, "TestWrongSignature", false, false)
	assertOccurrenceOwner(t, census, "sample/resources_test.go", ResourceEnvironment, "TestWrongPackage", false, false)
	assertOccurrenceOwner(t, census, "sample/resources_test.go", ResourceEnvironment, "TestValueParameter", false, false)
	assertOccurrenceOwner(t, census, "sample/resources_test.go", ResourceEnvironment, "TestLocalParameter", false, false)
	assertOccurrenceOwner(t, census, "sample/resources_test.go", ResourceEnvironment, "TestWrongTestingType", false, false)
	assertOccurrenceOwner(t, census, "sample/resources_test.go", ResourceEnvironment, "TestExtraParameter", false, false)
	assertOccurrenceOwner(t, census, "sample/resources_test.go", ResourceEnvironment, "TestResult", false, false)
	assertOccurrenceOwner(t, census, "sample/resources_test.go", ResourceEnvironment, "TestGeneric", false, false)
	assertOccurrenceOwner(t, census, "sample/tagged_test.go", ResourceEnvironment, "TestTagged", true, true)
	for _, occurrence := range census.Occurrences {
		if occurrence.Owner == "TestProduction" {
			t.Fatalf("production source contributed a resource occurrence: %+v", occurrence)
		}
	}
}

func TestSmallCountExcludesOnlyDeclaredExactOwnerResources(t *testing.T) {
	t.Parallel()

	census, err := ScanFS(fstest.MapFS{
		"cmd/gc/resources_test.go": &fstest.MapFile{Data: []byte(`package main
import (
	"os"
	"os/exec"
	"testing"
	"time"
)
func TestMain(m *testing.M) { os.Setenv("MAIN", "1") }
func TestOwned(t *testing.T) {
	os.Setenv("OWNED", "1")
	t.Run("nested", func(t *testing.T) { os.Chdir("nested") })
	helper()
}
func TestOther(t *testing.T) { os.Setenv("OTHER", "1") }
func helper() {
	time.Sleep(1)
	exec.Command("worker")
}
`)},
		"cmd/gc/tagged_test.go": &fstest.MapFile{Data: []byte(`//go:build integration

package main
import (
	"os"
	"testing"
)
func TestTagged(t *testing.T) { os.Setenv("TAGGED", "1") }
`)},
	})
	if err != nil {
		t.Fatalf("ScanFS: %v", err)
	}
	medium := []MediumOwner{
		validMediumOwner("cmd/gc", "main", "TestMain", ResourceEnvironment),
		validMediumOwner("cmd/gc", "main", "TestOwned", ResourceCWD, ResourceSubprocess),
	}

	assertSmallCount(t, census, medium, ScopeCmdGCUntagged, ResourceEnvironment, 2, 1)
	assertSmallCount(t, census, medium, ScopeCmdGCUntagged, ResourceCWD, 0, 0)
	assertSmallCount(t, census, medium, ScopeCmdGCUntagged, ResourceFixedSleep, 1, 1)
	assertSmallCount(t, census, medium, ScopeCmdGCUntagged, ResourceSubprocess, 1, 1)
}

func TestValidateMediumOwnersRequiresExactLiveCompleteRows(t *testing.T) {
	t.Parallel()

	census := Census{
		Runnables: []RunnableOwner{
			{PackageDir: "sample", PackageName: "sample", Owner: "TestMain"},
			{PackageDir: "sample", PackageName: "sample", Owner: "TestOwned"},
		},
		Occurrences: []Occurrence{
			{Path: "sample/main_test.go", PackageDir: "sample", PackageName: "sample", Owner: "TestMain", Runnable: true, Resource: ResourceEnvironment},
			{Path: "sample/owned_test.go", PackageDir: "sample", PackageName: "sample", Owner: "TestOwned", Runnable: true, Resource: ResourceEnvironment},
			{Path: "sample/owned_test.go", PackageDir: "sample", PackageName: "sample", Owner: "TestOwned", Runnable: true, Resource: ResourceCWD},
			{Path: "sample/helper_test.go", PackageDir: "sample", PackageName: "sample", Owner: "helper", Resource: ResourceSubprocess},
		},
	}
	validMain := validMediumOwner("sample", "sample", "TestMain", ResourceEnvironment)
	validOwned := validMediumOwner("sample", "sample", "TestOwned", ResourceCWD, ResourceEnvironment, ResourceSubprocess)
	blankPackageName := validMain
	blankPackageName.PackageName = " "
	blankMetadata := validMain
	blankMetadata.OwnerBead = ""
	expired := validMain
	expired.Expires = "2026-07-12"

	tests := []struct {
		name string
		rows []MediumOwner
		want string
	}{
		{name: "valid", rows: []MediumOwner{validMain, validOwned}},
		{name: "missing package", rows: []MediumOwner{validMediumOwner("missing", "missing", "TestMain", ResourceEnvironment)}, want: `medium owner package_dir=missing package_name=missing owner=TestMain: runnable owner does not exist`},
		{name: "missing owner", rows: []MediumOwner{validMediumOwner("sample", "sample", "TestMissing", ResourceEnvironment)}, want: `medium owner package_dir=sample package_name=sample owner=TestMissing: runnable owner does not exist`},
		{name: "nested subtest is not an owner", rows: []MediumOwner{validMediumOwner("sample", "sample", "TestOwned/nested", ResourceEnvironment)}, want: `medium owner package_dir=sample package_name=sample owner=TestOwned/nested: runnable owner does not exist`},
		{name: "duplicate row", rows: []MediumOwner{validMain, validMain}, want: `duplicate medium owner: package_dir=sample package_name=sample owner=TestMain`},
		{name: "empty resources", rows: []MediumOwner{validMediumOwner("sample", "sample", "TestMain")}, want: `medium owner package_dir=sample package_name=sample owner=TestMain: resources must not be empty`},
		{name: "duplicate resource", rows: []MediumOwner{validMediumOwner("sample", "sample", "TestMain", ResourceEnvironment, ResourceEnvironment)}, want: `medium owner package_dir=sample package_name=sample owner=TestMain: duplicate resource "environment"`},
		{name: "unknown resource", rows: []MediumOwner{validMediumOwner("sample", "sample", "TestMain", Resource("quantum_vm"))}, want: `medium owner package_dir=sample package_name=sample owner=TestMain: unknown resource "quantum_vm"`},
		{name: "blank package clause", rows: []MediumOwner{blankPackageName}, want: `package_name is required`},
		{name: "blank metadata", rows: []MediumOwner{blankMetadata}, want: `owner_bead is required`},
		{name: "expired", rows: []MediumOwner{expired}, want: `expired 2026-07-12`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := validateMediumOwners(tt.rows, census, fixedNow())
			if tt.want == "" {
				if err != nil {
					t.Fatalf("validateMediumOwners: %v", err)
				}
				return
			}
			requireErrorContains(t, err, tt.want)
		})
	}
}

func TestSmallCountUsesDirectoryPackageClauseAndOwnerIdentity(t *testing.T) {
	t.Parallel()

	census := Census{Occurrences: []Occurrence{
		{Path: "owned/a_test.go", PackageDir: "owned", PackageName: "sample", Owner: "TestOwned", Runnable: true, Resource: ResourceSubprocess},
		{Path: "owned/b_test.go", PackageDir: "owned", PackageName: "sample_test", Owner: "TestOwned", Runnable: true, Resource: ResourceSubprocess},
		{Path: "elsewhere/a_test.go", PackageDir: "elsewhere", PackageName: "sample", Owner: "TestOwned", Runnable: true, Resource: ResourceSubprocess},
	}}
	medium := []MediumOwner{validMediumOwner("owned", "sample", "TestOwned", ResourceSubprocess)}

	assertSmallCount(t, census, medium, ScopeUntagged, ResourceSubprocess, 2, 2)
}

func TestParseLedgerAcceptsMediumAndSmallDebtRows(t *testing.T) {
	t.Parallel()

	ledger, err := ParseLedger([]byte(`version = 2

[[medium]]
package_dir = "sample"
package_name = "sample"
owner = "TestOwned"
resources = ["subprocess"]
owner_bead = "ga-test"
invariant = "exact owner"
resource_owner = "lexical declaration"
migration_target = "P0.4b"
expires = "2026-10-01"

[[small_debt]]
scope = "untagged"
resource = "subprocess"
baseline_calls = 1
baseline_files = 1
reported_calls = 1
reported_files = 1
owner_bead = "ga-test"
invariant = "small debt cannot grow"
resource_owner = "owning test cleanup"
migration_target = "D1"
expires = "2026-10-01"
`))
	if err != nil {
		t.Fatalf("ParseLedger: %v", err)
	}
	if len(ledger.Medium) != 1 || ledger.Medium[0].Owner != "TestOwned" {
		t.Fatalf("Medium = %+v", ledger.Medium)
	}
	if len(ledger.SmallDebt) != 1 || ledger.SmallDebt[0].BaselineCalls != 1 {
		t.Fatalf("SmallDebt = %+v", ledger.SmallDebt)
	}
}

func TestValidateUsesRawAuditAndExactMediumFilteredSmallDebt(t *testing.T) {
	t.Parallel()

	census := Census{
		Runnables: []RunnableOwner{{PackageDir: "sample", PackageName: "sample", Owner: "TestOwned"}},
		Occurrences: []Occurrence{
			{Path: "sample/a_test.go", PackageDir: "sample", PackageName: "sample", Owner: "TestOwned", Runnable: true, Resource: ResourceSubprocess},
			{Path: "sample/b_test.go", PackageDir: "sample", PackageName: "sample", Owner: "helper", Resource: ResourceSubprocess},
		},
	}
	medium := validMediumOwner("sample", "sample", "TestOwned", ResourceSubprocess)
	policy := Ledger{
		Version:       2,
		AuditBaseline: []Baseline{validAudit(ScopeAll, ResourceSubprocess, 2, 2)},
		Debt:          []Baseline{validDebt(ScopeUntagged, ResourceSubprocess, 2, 2)},
		Medium:        []MediumOwner{medium},
		SmallDebt:     []Baseline{validDebt(ScopeUntagged, ResourceSubprocess, 1, 1)},
	}
	if err := validateAgainstPolicy(policy, cloneLedger(policy), census, fixedNow()); err != nil {
		t.Fatalf("validateAgainstPolicy: %v", err)
	}

	t.Run("audit remains raw", func(t *testing.T) {
		grown := cloneLedger(policy)
		grown.AuditBaseline[0].BaselineCalls = 1
		grown.AuditBaseline[0].BaselineFiles = 1
		err := validateAgainstPolicy(grown, cloneLedger(grown), census, fixedNow())
		requireErrorContains(t, err, "source resource census grew: scope=all resource=subprocess calls=2 (baseline 1), files=2 (baseline 1)")
	})

	t.Run("small debt uses exact filter", func(t *testing.T) {
		grown := cloneLedger(policy)
		grown.SmallDebt[0].BaselineCalls = 0
		grown.SmallDebt[0].BaselineFiles = 0
		err := validateAgainstPolicy(grown, cloneLedger(grown), census, fixedNow())
		requireErrorContains(t, err, "Small resource census grew: scope=untagged resource=subprocess calls=1 (baseline 0), files=1 (baseline 0)")
	})

	t.Run("small debt reductions lower the baseline", func(t *testing.T) {
		stale := cloneLedger(policy)
		stale.SmallDebt[0].BaselineCalls = 2
		stale.SmallDebt[0].BaselineFiles = 2
		err := validateAgainstPolicy(stale, cloneLedger(stale), census, fixedNow())
		requireErrorContains(t, err, "Small resource census baseline is stale: scope=untagged resource=subprocess calls=1 (baseline 2), files=1 (baseline 2); lower the checked baseline to bank the improvement")
	})
}

func TestValidateRejectsMediumPolicyDriftBeforeLiveCensus(t *testing.T) {
	t.Parallel()

	census := Census{
		Runnables:   []RunnableOwner{{PackageDir: "sample", PackageName: "sample", Owner: "TestOwned"}},
		Occurrences: []Occurrence{{Path: "sample/a_test.go", PackageDir: "sample", PackageName: "sample", Owner: "TestOwned", Runnable: true, Resource: ResourceSubprocess}},
	}
	policy := Ledger{
		Version:   2,
		Medium:    []MediumOwner{validMediumOwner("sample", "sample", "TestOwned", ResourceSubprocess)},
		SmallDebt: []Baseline{validDebt(ScopeUntagged, ResourceSubprocess, 0, 0)},
	}
	tests := []struct {
		name   string
		mutate func(*MediumOwner)
		want   string
	}{
		{
			name: "resources",
			mutate: func(row *MediumOwner) {
				row.Resources = []Resource{ResourceCWD}
			},
			want: `resources = [cwd], bootstrap policy requires [subprocess]`,
		},
		{
			name: "owner bead",
			mutate: func(row *MediumOwner) {
				row.OwnerBead = "ga-other"
			},
			want: `owner_bead = "ga-other", bootstrap policy requires "ga-test"`,
		},
		{
			name: "invariant",
			mutate: func(row *MediumOwner) {
				row.Invariant = "different invariant"
			},
			want: `invariant = "different invariant", bootstrap policy requires "exact runnable owner"`,
		},
		{
			name: "resource owner",
			mutate: func(row *MediumOwner) {
				row.ResourceOwner = "different owner"
			},
			want: `resource_owner = "different owner", bootstrap policy requires "lexical declaration"`,
		},
		{
			name: "migration target",
			mutate: func(row *MediumOwner) {
				row.MigrationTarget = "P9"
			},
			want: `migration_target = "P9", bootstrap policy requires "P0.4b"`,
		},
		{
			name: "expiry",
			mutate: func(row *MediumOwner) {
				row.Expires = "2026-11-01"
			},
			want: `expires = "2026-11-01", bootstrap policy requires "2026-10-01"`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ledger := cloneLedger(policy)
			tt.mutate(&ledger.Medium[0])
			err := validateAgainstPolicy(policy, ledger, census, fixedNow())
			requireErrorContains(t, err, tt.want)
			if strings.Contains(err.Error(), "resource census") {
				t.Fatalf("live census was compared before Medium policy drift was rejected: %v", err)
			}
		})
	}
}

func TestValidateRequiresExactMediumAndSmallDebtRowSets(t *testing.T) {
	t.Parallel()

	census := Census{
		Runnables:   []RunnableOwner{{PackageDir: "sample", PackageName: "sample", Owner: "TestOwned"}},
		Occurrences: []Occurrence{{Path: "sample/a_test.go", PackageDir: "sample", PackageName: "sample", Owner: "TestOwned", Runnable: true, Resource: ResourceSubprocess}},
	}
	policy := Ledger{
		Version:   2,
		Medium:    []MediumOwner{validMediumOwner("sample", "sample", "TestOwned", ResourceSubprocess)},
		SmallDebt: []Baseline{validDebt(ScopeUntagged, ResourceSubprocess, 0, 0)},
	}
	tests := []struct {
		name   string
		mutate func(*Ledger)
		want   string
	}{
		{
			name: "missing medium",
			mutate: func(ledger *Ledger) {
				ledger.Medium = nil
			},
			want: "missing required medium owner: package_dir=sample package_name=sample owner=TestOwned",
		},
		{
			name: "unexpected medium",
			mutate: func(ledger *Ledger) {
				ledger.Medium = append(ledger.Medium, validMediumOwner("sample", "sample", "TestOther", ResourceSubprocess))
			},
			want: "unexpected medium owner: package_dir=sample package_name=sample owner=TestOther",
		},
		{
			name: "duplicate medium",
			mutate: func(ledger *Ledger) {
				ledger.Medium = append(ledger.Medium, ledger.Medium[0])
			},
			want: "duplicate medium owner: package_dir=sample package_name=sample owner=TestOwned",
		},
		{
			name: "missing small debt",
			mutate: func(ledger *Ledger) {
				ledger.SmallDebt = nil
			},
			want: "missing required small debt baseline: scope=untagged resource=subprocess",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ledger := cloneLedger(policy)
			tt.mutate(&ledger)
			err := validateAgainstPolicy(policy, ledger, census, fixedNow())
			requireErrorContains(t, err, tt.want)
			if strings.Contains(err.Error(), "resource census") {
				t.Fatalf("live census was compared before row-set drift was rejected: %v", err)
			}
		})
	}
}

func assertOccurrenceOwner(t *testing.T, census Census, sourcePath string, resource Resource, owner string, runnable, tagged bool) {
	t.Helper()
	for _, occurrence := range census.Occurrences {
		if occurrence.Path == sourcePath && occurrence.Resource == resource && occurrence.Owner == owner && occurrence.Tagged == tagged {
			if occurrence.PackageDir != path.Dir(sourcePath) {
				t.Fatalf("occurrence package dir = %q, want %q: %+v", occurrence.PackageDir, path.Dir(sourcePath), occurrence)
			}
			if occurrence.Runnable != runnable {
				t.Fatalf("occurrence runnable = %t, want %t: %+v", occurrence.Runnable, runnable, occurrence)
			}
			return
		}
	}
	t.Fatalf("missing occurrence path=%s resource=%s owner=%s tagged=%t; got %+v", sourcePath, resource, owner, tagged, census.Occurrences)
}

func assertSmallCount(t *testing.T, census Census, medium []MediumOwner, scope Scope, resource Resource, wantCalls, wantFiles int) {
	t.Helper()
	got := census.SmallCount(scope, resource, medium)
	if got.Calls != wantCalls || got.Files != wantFiles {
		t.Fatalf("SmallCount(%s, %s) = %d calls / %d files, want %d / %d; occurrences=%+v",
			scope, resource, got.Calls, got.Files, wantCalls, wantFiles, census.Occurrences)
	}
}

func validMediumOwner(packageDir, packageName, owner string, resources ...Resource) MediumOwner {
	return MediumOwner{
		PackageDir:      packageDir,
		PackageName:     packageName,
		Owner:           owner,
		Resources:       resources,
		OwnerBead:       "ga-test",
		Invariant:       "exact runnable owner",
		ResourceOwner:   "lexical declaration",
		MigrationTarget: "P0.4b",
		Expires:         "2026-10-01",
	}
}
