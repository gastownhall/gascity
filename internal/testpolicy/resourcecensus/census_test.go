package resourcecensus

import (
	"go/ast"
	"go/token"
	"go/types"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"testing/fstest"
	"time"
)

func TestScanUsesImportIdentityAndParsedBuildConstraints(t *testing.T) {
	t.Parallel()

	files := fstest.MapFS{
		"sample/plain_test.go": &fstest.MapFile{Data: []byte(`package sample

import (
	shell "os/exec"
	clock "time"
)

type localExec struct{}
func (localExec) Command(string) {}
func (localExec) CommandContext(any, string) {}
type localClock struct{}
func (localClock) Sleep(int) {}

func TestResources() {
	shell.Command("one")
	shell.CommandContext(nil, "two")
	clock.Sleep(1)
	{
		shell := localExec{}
		shell.Command("not os/exec")
		shell.CommandContext(nil, "not os/exec")
		clock := localClock{}
		clock.Sleep(1)
	}
}
`)},
		"sample/tagged_test.go": &fstest.MapFile{Data: []byte(`//go:build integration && linux

package sample

import (
	"os/exec"
	"time"
)

func TestTagged() {
	exec.Command("tagged")
	time.Sleep(1)
}
`)},
		"sample/legacy_tagged_test.go": &fstest.MapFile{Data: []byte(`// +build darwin

package sample

import (
	"os/exec"
	"time"
)

func TestLegacyTagged() {
	exec.Command("legacy tagged")
	time.Sleep(1)
}
`)},
		"sample/false_positives_test.go": &fstest.MapFile{Data: []byte(`package sample

type localExec struct{}
func (localExec) Command(string) {}

func TestLocalNamesAreNotStdlibCalls() {
	exec := localExec{}
	exec.Command("not os/exec")
	_ = "time.Sleep(1); exec.Command(comment only)"
	// exec.Command("comment only")
}
`)},
	}

	got, err := ScanFS(files)
	if err != nil {
		t.Fatalf("ScanFS: %v", err)
	}

	assertCount(t, got, ScopeAll, ResourceSubprocess, 4, 3)
	assertCount(t, got, ScopeUntagged, ResourceSubprocess, 2, 1)
	assertCount(t, got, ScopeAll, ResourceFixedSleep, 3, 3)
	assertCount(t, got, ScopeUntagged, ResourceFixedSleep, 1, 1)
}

func TestScanTreatsImplicitPlatformFilenameConstraintsAsTagged(t *testing.T) {
	t.Parallel()

	files := fstest.MapFS{}
	for _, name := range []string{
		"sample/sample_linux_test.go",
		"sample/sample_amd64_test.go",
		"sample/sample_windows_arm64_test.go",
		"sample/linux_feature_test.go",
		"sample/sample_linux_extra_test.go",
		"sample/ordinary_test.go",
	} {
		files[name] = &fstest.MapFile{Data: []byte("package sample\nimport \"time\"\nfunc TestResource() { time.Sleep(1) }\n")}
	}

	got, err := ScanFS(files)
	if err != nil {
		t.Fatalf("ScanFS: %v", err)
	}
	assertCount(t, got, ScopeAll, ResourceFixedSleep, 6, 6)
	assertCount(t, got, ScopeUntagged, ResourceFixedSleep, 3, 3)
}

func TestScanTreatsGoSyslistPastPresentAndFutureSuffixesAsTagged(t *testing.T) {
	t.Parallel()

	names := []string{
		"sample/sample_hurd_test.go",
		"sample/sample_nacl_test.go",
		"sample/sample_zos_test.go",
		"sample/sample_amd64p32_test.go",
		"sample/sample_armbe_test.go",
		"sample/sample_arm64be_test.go",
		"sample/sample_mips64p32_test.go",
		"sample/sample_mips64p32le_test.go",
		"sample/sample_ppc_test.go",
		"sample/sample_riscv_test.go",
		"sample/sample_s390_test.go",
		"sample/sample_sparc_test.go",
		"sample/sample_sparc64_test.go",
		"sample/sample_linux_test.go",
	}
	files := fstest.MapFS{}
	for _, name := range names {
		files[name] = &fstest.MapFile{Data: []byte("package sample\nimport \"time\"\nfunc TestResource() { time.Sleep(1) }\n")}
	}

	got, err := ScanFS(files)
	if err != nil {
		t.Fatalf("ScanFS: %v", err)
	}
	assertCount(t, got, ScopeAll, ResourceFixedSleep, len(names), len(names))
	assertCount(t, got, ScopeUntagged, ResourceFixedSleep, 0, 0)
}

func TestScanUsesFilenamePrefixBeforeFirstDotForPlatformConstraint(t *testing.T) {
	t.Parallel()

	files := fstest.MapFS{
		"sample/sample_linux.v2_test.go": &fstest.MapFile{Data: []byte("package sample\nimport \"time\"\nfunc TestResource() { time.Sleep(1) }\n")},
		"sample/sample.v2_linux_test.go": &fstest.MapFile{Data: []byte("package sample\nimport \"time\"\nfunc TestResource() { time.Sleep(1) }\n")},
	}

	got, err := ScanFS(files)
	if err != nil {
		t.Fatalf("ScanFS: %v", err)
	}
	assertCount(t, got, ScopeAll, ResourceFixedSleep, 2, 2)
	assertCount(t, got, ScopeUntagged, ResourceFixedSleep, 1, 1)
}

func TestScanUnwrapsParenthesizedCallsWithoutLosingLexicalIdentity(t *testing.T) {
	t.Parallel()

	files := fstest.MapFS{
		"sample/resources_test.go": &fstest.MapFile{Data: []byte(`package sample
import (
	shell "os/exec"
	clock "time"
)
type localExec struct{}
func (localExec) Command(string) {}
func (localExec) CommandContext(any, string) {}
type localClock struct{}
func (localClock) Sleep(int) {}
func TestResources() {
	((shell).Command)("one")
	(((shell)).CommandContext)(nil, "two")
	((clock).Sleep)(1)
	{
		shell := localExec{}
		((shell).Command)("shadow")
		(((shell)).CommandContext)(nil, "shadow")
		clock := localClock{}
		((clock).Sleep)(1)
	}
}
`)},
	}

	got, err := ScanFS(files)
	if err != nil {
		t.Fatalf("ScanFS: %v", err)
	}
	assertCount(t, got, ScopeUntagged, ResourceSubprocess, 2, 1)
	assertCount(t, got, ScopeUntagged, ResourceFixedSleep, 1, 1)
}

func TestScanFailsClosedWhenCandidateQualifierBindingIsMissing(t *testing.T) {
	t.Parallel()

	_, err := ScanFS(fstest.MapFS{
		"sample/unresolved_test.go": &fstest.MapFile{Data: []byte(`package sample
func TestResource() { missing.Command("worker") }
`)},
	})
	requireErrorContains(t, err, `resource candidate qualifier "missing" has no lexical binding`)
}

func TestImportedCallFailsClosedWhenPackageBindingIsUnusable(t *testing.T) {
	t.Parallel()

	qualifier := ast.NewIdent("exec")
	call := &ast.CallExpr{Fun: &ast.SelectorExpr{X: qualifier, Sel: ast.NewIdent("Command")}}
	owner := types.NewPackage("resourcecensus.local/test", "sample")
	bindings := bindingInfo{uses: map[*ast.Ident]types.Object{
		qualifier: types.NewPkgName(token.NoPos, owner, qualifier.Name, nil),
	}}

	matched, err := isImportedCall(call, bindings, "os/exec", "Command", "CommandContext")
	if matched {
		t.Fatal("isImportedCall unexpectedly matched an unusable package binding")
	}
	requireErrorContains(t, err, `resource candidate qualifier "exec" has unusable package binding for "os/exec"`)
}

func TestScanUsesExactPackageBindingsAndSkipsUnrelatedFiles(t *testing.T) {
	t.Parallel()

	files := fstest.MapFS{
		"sample/other_package_test.go": &fstest.MapFile{Data: []byte(`package sample
import exec "example.test/not-os-exec"
func TestResource() { exec.Command("not a subprocess") }
`)},
		"sample/no_candidate_test.go": &fstest.MapFile{Data: []byte(`package sample
func TestIncomplete() { _ = unresolvedSiblingDeclaration }
`)},
	}

	got, err := ScanFS(files)
	if err != nil {
		t.Fatalf("ScanFS: %v", err)
	}
	assertCount(t, got, ScopeAll, ResourceSubprocess, 0, 0)
	assertCount(t, got, ScopeAll, ResourceFixedSleep, 0, 0)
}

func TestScanPreservesBindingsAfterIncompleteTypeErrors(t *testing.T) {
	t.Parallel()

	files := fstest.MapFS{
		"sample/incomplete_darwin_test.go": &fstest.MapFile{Data: []byte(`//go:build darwin

package sample

import (
	shell "os/exec"
	clock "time"
)

var _ unresolvedSiblingType

type localExec struct{}
func (localExec) Command(string) {}
type localClock struct{}
func (localClock) Sleep(int) {}

func TestResources() {
	unresolvedSiblingCall()
	shell.Command("worker")
	clock.Sleep(1)
	{
		shell := localExec{}
		shell.Command("not os/exec")
		clock := localClock{}
		clock.Sleep(1)
	}
}
`)},
	}

	got, err := ScanFS(files)
	if err != nil {
		t.Fatalf("ScanFS: %v", err)
	}
	assertCount(t, got, ScopeAll, ResourceSubprocess, 1, 1)
	assertCount(t, got, ScopeUntagged, ResourceSubprocess, 0, 0)
	assertCount(t, got, ScopeAll, ResourceFixedSleep, 1, 1)
	assertCount(t, got, ScopeUntagged, ResourceFixedSleep, 0, 0)
}

func TestScanRejectsTargetedDotImports(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		path       string
		importPath string
		source     string
	}{
		{
			name:       "os exec",
			path:       "sample/dot_exec_test.go",
			importPath: "os/exec",
			source: `package sample
import . "os/exec"
func TestResource() { Command("worker") }
`,
		},
		{
			name:       "time",
			path:       "sample/dot_time_test.go",
			importPath: "time",
			source: `package sample
import . "time"
func TestResource() { Sleep(1) }
`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ScanFS(fstest.MapFS{
				tt.path: &fstest.MapFile{Data: []byte(tt.source)},
			})
			requireErrorContains(t, err, tt.path)
			requireErrorContains(t, err, tt.importPath)
		})
	}
}

func TestScanAllowsBlankImportsOfTargetedPackages(t *testing.T) {
	t.Parallel()

	files := fstest.MapFS{
		"sample/blank_import_test.go": &fstest.MapFile{Data: []byte(`package sample
import (
	_ "os/exec"
	_ "time"
)
func TestResource() {}
`)},
	}
	got, err := ScanFS(files)
	if err != nil {
		t.Fatalf("ScanFS: %v", err)
	}
	assertCount(t, got, ScopeAll, ResourceSubprocess, 0, 0)
	assertCount(t, got, ScopeAll, ResourceFixedSleep, 0, 0)
}

func TestScanMatchesGoLeadingBuildHeaderPlacement(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		source     string
		wantTagged bool
		wantError  string
	}{
		{
			name: "go build separated",
			source: `//go:build integration

package sample
import "time"
func TestResource() { time.Sleep(1) }
`,
			wantTagged: true,
		},
		{
			name:       "go build after UTF-8 BOM",
			source:     "\ufeff//go:build integration\n\npackage sample\nimport \"time\"\nfunc TestResource() { time.Sleep(1) }\n",
			wantTagged: true,
		},
		{
			name: "go build adjacent to package",
			source: `//go:build integration
package sample
import "time"
func TestResource() { time.Sleep(1) }
`,
			wantTagged: true,
		},
		{
			name: "legacy build separated",
			source: `// +build integration

package sample
import "time"
func TestResource() { time.Sleep(1) }
`,
			wantTagged: true,
		},
		{
			name: "legacy build adjacent to package",
			source: `// +build integration
package sample
import "time"
func TestResource() { time.Sleep(1) }
`,
		},
		{
			name: "legacy build in package doc",
			source: `// Package sample owns fixtures.
// +build integration
package sample
import "time"
func TestResource() { time.Sleep(1) }
`,
		},
		{
			name: "directives after package",
			source: `package sample
//go:build integration
// +build integration
import "time"
func TestResource() { time.Sleep(1) }
`,
		},
		{
			name: "directive like comments",
			source: `//go:buildintegration
// +buildintegration

package sample
import "time"
func TestResource() { time.Sleep(1) }
`,
		},
		{
			name: "go build text inside block comment",
			source: `/*
//go:build integration
*/

package sample
import "time"
func TestResource() { time.Sleep(1) }
`,
		},
		{
			name: "go build after leading block comment",
			source: `/* copyright */
//go:build integration
package sample
import "time"
func TestResource() { time.Sleep(1) }
`,
			wantTagged: true,
		},
		{
			name: "go build after block comment on same line",
			source: `/**///go:build integration
package sample
import "time"
func TestResource() { time.Sleep(1) }
`,
		},
		{
			name: "legacy build after leading block comment",
			source: `/* copyright */
// +build integration

package sample
import "time"
func TestResource() { time.Sleep(1) }
`,
		},
		{
			name: "malformed go build",
			source: `//go:build (integration

package sample
import "time"
func TestResource() { time.Sleep(1) }
`,
			wantError: "parsing build constraint",
		},
		{
			name: "malformed legacy build",
			source: `// +build (integration

package sample
import "time"
func TestResource() { time.Sleep(1) }
`,
			wantTagged: true,
		},
		{
			name: "multiple go build lines",
			source: `//go:build integration
//go:build linux

package sample
import "time"
func TestResource() { time.Sleep(1) }
`,
			wantError: "multiple //go:build comments",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := "sample/header_test.go"
			got, err := ScanFS(fstest.MapFS{
				path: &fstest.MapFile{Data: []byte(tt.source)},
			})
			if tt.wantError != "" {
				requireErrorContains(t, err, tt.wantError)
				return
			}
			if err != nil {
				t.Fatalf("ScanFS: %v", err)
			}
			assertCount(t, got, ScopeAll, ResourceFixedSleep, 1, 1)
			wantUntagged := 1
			if tt.wantTagged {
				wantUntagged = 0
			}
			assertCount(t, got, ScopeUntagged, ResourceFixedSleep, wantUntagged, wantUntagged)
		})
	}
}

func TestScanRejectsMalformedBuildConstraint(t *testing.T) {
	t.Parallel()

	files := fstest.MapFS{
		"sample/sample_test.go": &fstest.MapFile{Data: []byte("//go:build (linux\n\npackage sample\n")},
	}
	_, err := ScanFS(files)
	requireErrorContains(t, err, "parsing build constraint")
}

func TestValidateAcceptsExactSourceRatchets(t *testing.T) {
	t.Parallel()

	census := Census{Occurrences: []Occurrence{
		{Path: "sample/a_test.go", Resource: ResourceSubprocess},
		{Path: "sample/a_test.go", Resource: ResourceSubprocess},
		{Path: "sample/b_test.go", Resource: ResourceSubprocess},
	}}
	policy := validLedger(census)
	ledger := cloneLedger(policy)

	if err := validateAgainstPolicy(policy, ledger, census, fixedNow()); err != nil {
		t.Fatalf("Validate: %v", err)
	}
}

func TestValidateRejectsDebtGrowthAndStaleHighBaselines(t *testing.T) {
	t.Parallel()

	census := Census{Occurrences: []Occurrence{
		{Path: "sample/a_test.go", Resource: ResourceSubprocess},
		{Path: "sample/b_test.go", Resource: ResourceSubprocess},
	}}

	t.Run("growth", func(t *testing.T) {
		policy := validLedger(census)
		row := findRow(t, policy.Debt, ScopeUntagged, ResourceSubprocess)
		row.BaselineCalls = 1
		row.BaselineFiles = 1
		ledger := cloneLedger(policy)
		err := validateAgainstPolicy(policy, ledger, census, fixedNow())
		requireErrorContains(t, err,
			"source resource census grew: scope=untagged resource=subprocess calls=2 (baseline 1), files=2 (baseline 1)")
	})

	t.Run("stale high", func(t *testing.T) {
		policy := validLedger(census)
		row := findRow(t, policy.Debt, ScopeUntagged, ResourceSubprocess)
		row.BaselineCalls = 3
		row.BaselineFiles = 3
		ledger := cloneLedger(policy)
		err := validateAgainstPolicy(policy, ledger, census, fixedNow())
		requireErrorContains(t, err,
			"source resource census baseline is stale: scope=untagged resource=subprocess calls=2 (baseline 3), files=2 (baseline 3); lower the checked baseline to bank the improvement")
	})
}

func TestValidateAllowsHistoricalNeedleToDifferFromASTCensus(t *testing.T) {
	t.Parallel()

	census := Census{Occurrences: []Occurrence{
		{Path: "sample/a_test.go", Resource: ResourceSubprocess},
		{Path: "sample/b_test.go", Resource: ResourceSubprocess},
	}}
	policy := validLedger(census)
	row := findRow(t, policy.Debt, ScopeUntagged, ResourceSubprocess)
	row.ReportedCalls = 1
	row.ReportedFiles = 1
	ledger := cloneLedger(policy)

	if err := validateAgainstPolicy(policy, ledger, census, fixedNow()); err != nil {
		t.Fatalf("Validate rejected historical source needle: %v", err)
	}
}

func TestValidateRejectsBootstrapPolicyDriftBeforeLiveCensus(t *testing.T) {
	t.Parallel()

	policy := validLedger(Census{})
	policy.AuditBaseline[0].ReportedCalls = 11
	policy.AuditBaseline[0].ReportedFiles = 3
	policy.AuditBaseline[0].Invariant = "audit invariant"
	policy.AuditBaseline[0].ResourceOwner = "audit owner"
	policy.AuditBaseline[0].MigrationTarget = "P0.4a"
	policy.AuditBaseline[0].Expires = "2026-10-01"
	policy.Debt[0].ReportedCalls = 7

	tests := []struct {
		name   string
		mutate func(*Ledger)
		want   string
	}{
		{
			name: "zeroed history",
			mutate: func(ledger *Ledger) {
				ledger.AuditBaseline[0].ReportedCalls = 0
			},
			want: "reported_calls = 0, bootstrap policy requires 11",
		},
		{
			name: "rewritten history",
			mutate: func(ledger *Ledger) {
				ledger.Debt[0].ReportedCalls = 8
			},
			want: "reported_calls = 8, bootstrap policy requires 7",
		},
		{
			name: "owner drift",
			mutate: func(ledger *Ledger) {
				ledger.Debt[0].OwnerBead = "ga-other"
			},
			want: `owner_bead = "ga-other", bootstrap policy requires "P0.4"`,
		},
		{
			name: "invariant drift",
			mutate: func(ledger *Ledger) {
				ledger.Debt[0].Invariant = "rewritten"
			},
			want: `invariant = "rewritten", bootstrap policy requires "existing debt cannot grow"`,
		},
		{
			name: "resource owner drift",
			mutate: func(ledger *Ledger) {
				ledger.Debt[0].ResourceOwner = "rewritten"
			},
			want: `resource_owner = "rewritten", bootstrap policy requires "owning test cleanup"`,
		},
		{
			name: "migration drift",
			mutate: func(ledger *Ledger) {
				ledger.Debt[0].MigrationTarget = "elsewhere"
			},
			want: `migration_target = "elsewhere", bootstrap policy requires "D1/D2"`,
		},
		{
			name: "expiry drift",
			mutate: func(ledger *Ledger) {
				ledger.Debt[0].Expires = "2027-01-01"
			},
			want: `expires = "2027-01-01", bootstrap policy requires "2026-10-01"`,
		},
		{
			name: "simultaneous census and manifest growth",
			mutate: func(ledger *Ledger) {
				ledger.Debt[0].BaselineCalls = 1
				ledger.Debt[0].BaselineFiles = 1
			},
			want: "baseline_calls = 1, bootstrap policy requires 0",
		},
	}

	grownCensus := Census{Occurrences: []Occurrence{{Path: "sample/new_test.go", Resource: ResourceSubprocess}}}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ledger := cloneLedger(policy)
			tt.mutate(&ledger)
			err := validateAgainstPolicy(policy, ledger, grownCensus, fixedNow())
			requireErrorContains(t, err, tt.want)
			if strings.Contains(err.Error(), "source resource census") {
				t.Fatalf("live census was compared before bootstrap policy drift was rejected: %v", err)
			}
		})
	}
}

func TestValidateUsesCodeOwnedBootstrapPolicy(t *testing.T) {
	t.Parallel()

	ledger := cloneLedger(bootstrapPolicy)
	ledger.Debt[0].OwnerBead = "ga-rewritten"
	err := Validate(ledger, Census{}, fixedNow())
	requireErrorContains(t, err, `owner_bead = "ga-rewritten", bootstrap policy requires "ga-80po0c.2"`)
	if strings.Contains(err.Error(), "source resource census") {
		t.Fatalf("live census was compared before code-owned policy drift was rejected: %v", err)
	}
}

func TestValidateRequiresTheExactBootstrapRowSet(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		mutate func(*Ledger)
		want   string
	}{
		{
			name: "missing audit row",
			mutate: func(ledger *Ledger) {
				ledger.AuditBaseline = ledger.AuditBaseline[1:]
			},
			want: `missing required audit baseline: scope=all resource=subprocess`,
		},
		{
			name: "missing debt row",
			mutate: func(ledger *Ledger) {
				ledger.Debt = ledger.Debt[1:]
			},
			want: `missing required debt baseline: scope=untagged resource=subprocess`,
		},
		{
			name: "unexpected audit row",
			mutate: func(ledger *Ledger) {
				ledger.AuditBaseline = append(ledger.AuditBaseline, validAudit(ScopeUntagged, ResourceFixedSleep, 0, 0))
			},
			want: `unexpected audit baseline: scope=untagged resource=fixed_sleep`,
		},
		{
			name: "unexpected debt row",
			mutate: func(ledger *Ledger) {
				ledger.Debt = append(ledger.Debt, validDebt(ScopeAll, ResourceFixedSleep, 0, 0))
			},
			want: `unexpected debt baseline: scope=all resource=fixed_sleep`,
		},
		{
			name: "duplicate debt row",
			mutate: func(ledger *Ledger) {
				ledger.Debt = append(ledger.Debt, ledger.Debt[0])
			},
			want: `duplicate debt baseline: scope=untagged resource=subprocess`,
		},
		{
			name: "expired debt",
			mutate: func(ledger *Ledger) {
				ledger.Debt[0].Expires = "2026-07-12"
			},
			want: `debt baseline scope=untagged resource=subprocess: expired 2026-07-12`,
		},
		{
			name: "unknown resource",
			mutate: func(ledger *Ledger) {
				ledger.Debt[0].Resource = Resource("quantum_vm")
			},
			want: `debt baseline scope=untagged resource=quantum_vm: unknown resource "quantum_vm"`,
		},
		{
			name: "negative historical census",
			mutate: func(ledger *Ledger) {
				ledger.Debt[0].ReportedCalls = -1
			},
			want: `debt baseline scope=untagged resource=subprocess: historical census must be non-negative`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			policy := validLedger(Census{})
			ledger := cloneLedger(policy)
			tt.mutate(&ledger)
			err := validateAgainstPolicy(policy, ledger, Census{}, fixedNow())
			requireErrorContains(t, err, tt.want)
		})
	}
}

func TestParseLedgerRejectsUndeclaredFields(t *testing.T) {
	t.Parallel()

	_, err := ParseLedger([]byte("version = 1\nmystery = true\n"))
	requireErrorContains(t, err, "unknown ledger field: mystery")
}

func TestParseLedgerRejectsClassificationFields(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		data string
		want string
	}{
		{"medium rows", "version = 1\n[[medium]]\npackage = 'sample'\n", "unknown ledger field: medium"},
		{"small debt rows", "version = 1\n[[small_debt]]\nscope = 'untagged'\n", "unknown ledger field: small_debt"},
		{"size field", "version = 1\n[[debt]]\nscope = 'untagged'\nintended_size = 'small'\n", "unknown ledger field: debt.intended_size"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ParseLedger([]byte(tt.data))
			requireErrorContains(t, err, tt.want)
		})
	}
}

func TestRenderMarkdownIsDeterministic(t *testing.T) {
	t.Parallel()

	ledger := Ledger{
		Version: 1,
		AuditBaseline: []Baseline{
			validAudit(ScopeAll, ResourceFixedSleep, 4, 2),
		},
		Debt: []Baseline{
			validDebt(ScopeUntagged, ResourceSubprocess, 3, 2),
		},
	}
	got := RenderMarkdown(ledger)
	want := `<!-- BEGIN CHECKED TEST RESOURCE LEDGER -->
| Ledger kind | Source scope | Resource baseline | Tracking owner | Invariant / resource owner | Migration | Expiry |
| --- | --- | --- | --- | --- | --- | --- |
| Audit baseline | all tracked test source | fixed_sleep: 4 calls / 2 files | P0.4 | source census only; does not classify tests; audit owner | P0.4a | 2026-10-01 |
| Source debt ratchet | all untagged test source | subprocess: 3 calls / 2 files | P0.4 | existing debt cannot grow; owning test cleanup | D1/D2 | 2026-10-01 |
<!-- END CHECKED TEST RESOURCE LEDGER -->`
	if got != want {
		t.Fatalf("RenderMarkdown mismatch\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

func TestCheckedMarkdownBlockRequiresOneOrderedMarkerPair(t *testing.T) {
	t.Parallel()

	for _, document := range []string{
		"no markers",
		markdownEnd + "\n" + markdownBegin,
		markdownBegin + "\n" + markdownEnd + "\n" + markdownBegin,
	} {
		if _, err := CheckedMarkdownBlock(document); err == nil {
			t.Fatalf("CheckedMarkdownBlock(%q) unexpectedly succeeded", document)
		}
	}
}

func TestRepositoryLedgerMatchesCensusAndDocumentation(t *testing.T) {
	root := repositoryRoot(t)
	ledger, err := LoadLedger(filepath.Join(root, "test", "test-resources.toml"))
	if err != nil {
		t.Fatalf("LoadLedger: %v", err)
	}
	census, err := ScanRepository(root)
	if err != nil {
		t.Fatalf("ScanRepository: %v", err)
	}
	if err := Validate(ledger, census, time.Now().UTC()); err != nil {
		t.Fatalf("resource ledger drift:\n%v", err)
	}

	doc, err := fs.ReadFile(os.DirFS(root), "TESTING.md")
	if err != nil {
		t.Fatalf("read TESTING.md: %v", err)
	}
	got, err := CheckedMarkdownBlock(string(doc))
	if err != nil {
		t.Fatalf("checked TESTING.md block: %v\n--- wanted block ---\n%s", err, RenderMarkdown(ledger))
	}
	if want := RenderMarkdown(ledger); got != want {
		t.Fatalf("TESTING.md resource ledger block is stale\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

func assertCount(t *testing.T, census Census, scope Scope, resource Resource, wantCalls, wantFiles int) {
	t.Helper()
	got := census.Count(scope, resource)
	if got.Calls != wantCalls || got.Files != wantFiles {
		t.Fatalf("Count(%s, %s) = %d calls / %d files, want %d / %d; occurrences=%+v",
			scope, resource, got.Calls, got.Files, wantCalls, wantFiles, census.Occurrences)
	}
}

func validLedger(census Census) Ledger {
	allSubprocess := census.Count(ScopeAll, ResourceSubprocess)
	allSleep := census.Count(ScopeAll, ResourceFixedSleep)
	untaggedSubprocess := census.Count(ScopeUntagged, ResourceSubprocess)
	untaggedSleep := census.Count(ScopeUntagged, ResourceFixedSleep)
	return Ledger{
		Version: 1,
		AuditBaseline: []Baseline{
			validAudit(ScopeAll, ResourceSubprocess, allSubprocess.Calls, allSubprocess.Files),
			validAudit(ScopeAll, ResourceFixedSleep, allSleep.Calls, allSleep.Files),
		},
		Debt: []Baseline{
			validDebt(ScopeUntagged, ResourceSubprocess, untaggedSubprocess.Calls, untaggedSubprocess.Files),
			validDebt(ScopeUntagged, ResourceFixedSleep, untaggedSleep.Calls, untaggedSleep.Files),
		},
	}
}

func validAudit(scope Scope, resource Resource, calls, files int) Baseline {
	return Baseline{
		Scope:           scope,
		Resource:        resource,
		BaselineCalls:   calls,
		BaselineFiles:   files,
		OwnerBead:       "P0.4",
		Invariant:       "source census only; does not classify tests",
		ResourceOwner:   "audit owner",
		MigrationTarget: "P0.4a",
		Expires:         "2026-10-01",
	}
}

func validDebt(scope Scope, resource Resource, calls, files int) Baseline {
	return Baseline{
		Scope:           scope,
		Resource:        resource,
		BaselineCalls:   calls,
		BaselineFiles:   files,
		OwnerBead:       "P0.4",
		Invariant:       "existing debt cannot grow",
		ResourceOwner:   "owning test cleanup",
		MigrationTarget: "D1/D2",
		Expires:         "2026-10-01",
	}
}

func cloneLedger(source Ledger) Ledger {
	clone := source
	clone.AuditBaseline = append([]Baseline(nil), source.AuditBaseline...)
	clone.Debt = append([]Baseline(nil), source.Debt...)
	return clone
}

func findRow(t *testing.T, rows []Baseline, scope Scope, resource Resource) *Baseline {
	t.Helper()
	for i := range rows {
		if rows[i].Scope == scope && rows[i].Resource == resource {
			return &rows[i]
		}
	}
	t.Fatalf("row not found: scope=%s resource=%s", scope, resource)
	return nil
}

func fixedNow() time.Time {
	return time.Date(2026, time.July, 13, 0, 0, 0, 0, time.UTC)
}

func requireErrorContains(t *testing.T, err error, want string) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected error containing %q, got nil", want)
	}
	if !strings.Contains(err.Error(), want) {
		t.Fatalf("error = %q, want substring %q", err, want)
	}
}

func repositoryRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller did not report census_test.go")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", "..", ".."))
}
