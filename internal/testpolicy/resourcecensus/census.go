// Package resourcecensus checks Gas City's declared test-resource debt against
// syntax-aware observations from tracked Go test files.
package resourcecensus

import (
	"bytes"
	"errors"
	"fmt"
	"go/ast"
	"go/build/constraint"
	"go/parser"
	"go/token"
	"go/types"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/BurntSushi/toml"
)

// Resource is a syntax-observable test resource.
type Resource string

const (
	// ResourceSubprocess counts direct os/exec command construction.
	ResourceSubprocess Resource = "subprocess"
	// ResourceFixedSleep counts direct time.Sleep calls.
	ResourceFixedSleep Resource = "fixed_sleep"
	// ResourceEnvironment counts recognized process-environment mutations.
	ResourceEnvironment Resource = "environment"
	// ResourceCWD counts recognized process working-directory mutations.
	ResourceCWD Resource = "cwd"
	// ResourceSlowProcessGate counts the cmd/gc slow-process helper and calls.
	ResourceSlowProcessGate Resource = "slow_process_gate"
	// ResourceHTTPTestServer counts loopback servers opened by net/http/httptest.
	ResourceHTTPTestServer Resource = "http_test_server"
	// ResourceNetListen counts direct listeners opened by net.Listen.
	ResourceNetListen Resource = "net_listen"
	// ResourceNetListenUnixgram counts direct Unix datagram listeners opened by net.ListenUnixgram.
	ResourceNetListenUnixgram Resource = "net_listen_unixgram"
	// ResourceNetListenConfig counts direct listeners opened through net.ListenConfig.Listen.
	ResourceNetListenConfig Resource = "net_listen_config"
	// ResourceSyscallListen counts direct calls that put sockets into listening state through syscall.Listen.
	ResourceSyscallListen Resource = "syscall_listen"
)

var knownResources = map[Resource]struct{}{
	ResourceSubprocess:        {},
	ResourceFixedSleep:        {},
	ResourceEnvironment:       {},
	ResourceCWD:               {},
	ResourceSlowProcessGate:   {},
	ResourceHTTPTestServer:    {},
	ResourceNetListen:         {},
	ResourceNetListenConfig:   {},
	ResourceNetListenUnixgram: {},
	ResourceSyscallListen:     {},
}

// Scope selects the source population counted by a ledger row.
type Scope string

const (
	// ScopeAll includes every tracked Go test file.
	ScopeAll Scope = "all"
	// ScopeUntagged excludes explicitly and implicitly constrained files.
	ScopeUntagged Scope = "untagged"
	// ScopeCmdGCUntagged selects untagged test files beneath cmd/gc.
	ScopeCmdGCUntagged Scope = "cmd/gc+untagged"
)

type baselineKey struct {
	scope    Scope
	resource Resource
}

// Ledger is the checked source-level test-resource inventory.
type Ledger struct {
	Version              int                    `toml:"version"`
	AuditBaseline        []Baseline             `toml:"audit_baseline"`
	Debt                 []Baseline             `toml:"debt"`
	Medium               []MediumOwner          `toml:"medium"`
	ReviewedHermeticBody []ReviewedHermeticBody `toml:"reviewed_hermetic_body"`
	SmallDebt            []Baseline             `toml:"small_debt"`
}

// Baseline pins one source-census signal and its migration ownership.
type Baseline struct {
	Scope           Scope    `toml:"scope"`
	Resource        Resource `toml:"resource"`
	BaselineCalls   int      `toml:"baseline_calls"`
	BaselineFiles   int      `toml:"baseline_files"`
	ReportedCalls   int      `toml:"reported_calls"`
	ReportedFiles   int      `toml:"reported_files"`
	OwnerBead       string   `toml:"owner_bead"`
	Invariant       string   `toml:"invariant"`
	ResourceOwner   string   `toml:"resource_owner"`
	MigrationTarget string   `toml:"migration_target"`
	Expires         string   `toml:"expires"`
}

var bootstrapPolicy = Ledger{
	Version: 2,
	AuditBaseline: []Baseline{
		{
			Scope:           ScopeAll,
			Resource:        ResourceSubprocess,
			BaselineCalls:   529,
			BaselineFiles:   159,
			ReportedCalls:   495,
			ReportedFiles:   135,
			OwnerBead:       "ga-80po0c.2",
			Invariant:       "tracked test source totals remain visible as audit evidence",
			ResourceOwner:   "ga-80po0c.2 owns this point-in-time source census",
			MigrationTarget: "P0.4a",
			Expires:         "2026-10-01",
		},
		{
			Scope:           ScopeAll,
			Resource:        ResourceFixedSleep,
			BaselineCalls:   439,
			BaselineFiles:   157,
			ReportedCalls:   447,
			ReportedFiles:   157,
			OwnerBead:       "ga-80po0c.2",
			Invariant:       "tracked test source totals remain visible as audit evidence",
			ResourceOwner:   "ga-80po0c.2 owns this point-in-time source census",
			MigrationTarget: "P0.4a",
			Expires:         "2026-10-01",
		},
	},
	Debt: []Baseline{
		{
			Scope:           ScopeUntagged,
			Resource:        ResourceSubprocess,
			BaselineCalls:   400,
			BaselineFiles:   111,
			ReportedCalls:   380,
			ReportedFiles:   98,
			OwnerBead:       "ga-80po0c.2",
			Invariant:       "untagged subprocess call/file totals cannot grow; reductions must lower this baseline",
			ResourceOwner:   "each process-owning test removes or replaces its source call site",
			MigrationTarget: "D1/D2/D5/D6/E6",
			Expires:         "2026-10-01",
		},
		{
			Scope:           ScopeUntagged,
			Resource:        ResourceFixedSleep,
			BaselineCalls:   285,
			BaselineFiles:   112,
			ReportedCalls:   295,
			ReportedFiles:   114,
			OwnerBead:       "ga-80po0c.2",
			Invariant:       "untagged fixed-sleep call/file totals cannot grow; reductions must lower this baseline",
			ResourceOwner:   "each owning test replaces elapsed wall time with its lifecycle signal",
			MigrationTarget: "W1-W5",
			Expires:         "2026-10-01",
		},
		{
			Scope:           ScopeCmdGCUntagged,
			Resource:        ResourceEnvironment,
			BaselineCalls:   4331,
			BaselineFiles:   204,
			ReportedCalls:   3960,
			ReportedFiles:   184,
			OwnerBead:       "ga-80po0c.2.3",
			Invariant:       "untagged cmd/gc environment call/file totals cannot grow; reductions must lower this baseline",
			ResourceOwner:   "cmd/gc callers restore or eliminate every recognized process-environment mutation",
			MigrationTarget: "D5/D6/E6",
			Expires:         "2026-10-01",
		},
		{
			Scope:           ScopeCmdGCUntagged,
			Resource:        ResourceCWD,
			BaselineCalls:   285,
			BaselineFiles:   43,
			ReportedCalls:   98,
			ReportedFiles:   13,
			OwnerBead:       "ga-80po0c.2.3",
			Invariant:       "untagged cmd/gc cwd call/file totals cannot grow; reductions must lower this baseline",
			ResourceOwner:   "cmd/gc callers restore or eliminate every recognized cwd mutation",
			MigrationTarget: "D5/D6",
			Expires:         "2026-10-01",
		},
		{
			Scope:           ScopeCmdGCUntagged,
			Resource:        ResourceSlowProcessGate,
			BaselineCalls:   71,
			BaselineFiles:   24,
			ReportedCalls:   78,
			ReportedFiles:   27,
			OwnerBead:       "ga-80po0c.2.3",
			Invariant:       "untagged cmd/gc slow-process marker totals cannot grow; reductions must lower this baseline",
			ResourceOwner:   "the helper definition and every marked caller retain an explicit process-suite migration owner",
			MigrationTarget: "D5/D6/E6",
			Expires:         "2026-10-01",
		},
		{
			Scope:           ScopeUntagged,
			Resource:        ResourceHTTPTestServer,
			BaselineCalls:   318,
			BaselineFiles:   67,
			ReportedCalls:   255,
			ReportedFiles:   56,
			OwnerBead:       "ga-80po0c.2.2",
			Invariant:       "untagged HTTP test server call/file totals cannot grow; reductions must lower this baseline",
			ResourceOwner:   "each owning test closes its loopback server and removes duplicate server-backed coverage",
			MigrationTarget: "P0.4c",
			Expires:         "2026-10-01",
		},
		{
			Scope:           ScopeUntagged,
			Resource:        ResourceNetListen,
			BaselineCalls:   92,
			BaselineFiles:   34,
			ReportedCalls:   92,
			ReportedFiles:   34,
			OwnerBead:       "ga-80po0c.2.2",
			Invariant:       "untagged net.Listen call/file totals cannot grow; reductions must lower this baseline",
			ResourceOwner:   "each owning test closes its listener and removes duplicate listener-backed coverage",
			MigrationTarget: "P0.4c",
			Expires:         "2026-10-01",
		},
		{
			Scope:           ScopeUntagged,
			Resource:        ResourceNetListenConfig,
			BaselineCalls:   1,
			BaselineFiles:   1,
			ReportedCalls:   1,
			ReportedFiles:   1,
			OwnerBead:       "ga-80po0c.2.2",
			Invariant:       "untagged net.ListenConfig.Listen call/file totals cannot grow; reductions must lower this baseline",
			ResourceOwner:   "each owning test closes its configured listener and removes duplicate listener-backed coverage",
			MigrationTarget: "P0.4c",
			Expires:         "2026-10-01",
		},
		{
			Scope:           ScopeUntagged,
			Resource:        ResourceNetListenUnixgram,
			BaselineCalls:   3,
			BaselineFiles:   2,
			ReportedCalls:   3,
			ReportedFiles:   2,
			OwnerBead:       "ga-80po0c.2.2",
			Invariant:       "untagged net.ListenUnixgram call/file totals cannot grow; reductions must lower this baseline",
			ResourceOwner:   "each owning test closes its Unix datagram listener and removes duplicate listener-backed coverage",
			MigrationTarget: "P0.4c",
			Expires:         "2026-10-01",
		},
		{
			Scope:           ScopeUntagged,
			Resource:        ResourceSyscallListen,
			BaselineCalls:   1,
			BaselineFiles:   1,
			ReportedCalls:   1,
			ReportedFiles:   1,
			OwnerBead:       "ga-80po0c.2.2",
			Invariant:       "untagged syscall.Listen call/file totals cannot grow; reductions must lower this baseline",
			ResourceOwner:   "each owning test closes its listening file descriptor and removes duplicate listener-backed coverage",
			MigrationTarget: "P0.4c",
			Expires:         "2026-10-01",
		},
	},
	Medium: []MediumOwner{
		{
			PackageDir:      "internal/api",
			PackageName:     "api",
			Owner:           "TestEveryEmittedErrorCodeIsRegistered",
			Resources:       []Resource{ResourceSubprocess},
			OwnerBead:       "ga-80po0c.2.1",
			Invariant:       "internal/api tracked-source error URN guard is a checked Medium owner",
			ResourceOwner:   "only the git ls-files call lexically inside TestEveryEmittedErrorCodeIsRegistered leaves Small debt",
			MigrationTarget: "P0.4b",
			Expires:         "2026-10-01",
		},
		{
			PackageDir:      "cmd/gc",
			PackageName:     "main",
			Owner:           "TestMain",
			Resources:       []Resource{ResourceEnvironment},
			OwnerBead:       "ga-80po0c.2.1",
			Invariant:       "cmd/gc TestMain is the checked package-level Medium owner",
			ResourceOwner:   "only environment calls lexically inside TestMain leave Small debt",
			MigrationTarget: "P0.4b",
			Expires:         "2026-10-01",
		},
		{
			PackageDir:      "scripts",
			PackageName:     "scripts_test",
			Owner:           "TestDockerSessionProtocol",
			Resources:       []Resource{ResourceSubprocess},
			OwnerBead:       "ga-80po0c.23.1",
			Invariant:       "Docker session adapter protocol proof is a checked Medium owner",
			ResourceOwner:   "the one adapter subprocess is confined to TestDockerSessionProtocol and Docker itself is a strict PATH-injected fake",
			MigrationTarget: "W6",
			Expires:         "2026-10-01",
		},
		{
			PackageDir:      "scripts",
			PackageName:     "scripts_test",
			Owner:           "TestProviderOverridesAndSuiteContractsCrossMakeIsolation",
			Resources:       []Resource{ResourceSubprocess},
			OwnerBead:       "ga-80po0c.2.1",
			Invariant:       "Make/provider and suite-contract proof is a checked Medium owner",
			ResourceOwner:   "the six isolated Make invocations are confined to TestProviderOverridesAndSuiteContractsCrossMakeIsolation",
			MigrationTarget: "P0.1",
			Expires:         "2026-10-01",
		},
	},
	ReviewedHermeticBody: []ReviewedHermeticBody{
		{
			PackageDir:    "cmd/gc",
			PackageName:   "main",
			Owner:         "TestDoSessionWait_RegistersReadyWaitForRigDependency",
			EffectiveSize: "medium",
			MediumReason:  "package TestMain mutates process state",
		},
		{
			PackageDir:    "cmd/gc",
			PackageName:   "main",
			Owner:         "TestDoSessionWake_PokesManagedControllerAfterStateChange",
			EffectiveSize: "medium",
			MediumReason:  "package TestMain mutates process state",
		},
		{
			PackageDir:    "cmd/gc",
			PackageName:   "main",
			Owner:         "TestPrepareWaitWakeState_ResolvesRigDependencyBeads",
			EffectiveSize: "medium",
			MediumReason:  "package TestMain mutates process state",
		},
		{
			PackageDir:    "cmd/gc",
			PackageName:   "main",
			Owner:         "TestDoMailInbox_RendersMessagesFromReader",
			EffectiveSize: "medium",
			MediumReason:  "package TestMain mutates process state",
		},
	},
	SmallDebt: []Baseline{
		{
			Scope:           ScopeUntagged,
			Resource:        ResourceSubprocess,
			BaselineCalls:   397,
			BaselineFiles:   109,
			ReportedCalls:   394,
			ReportedFiles:   105,
			OwnerBead:       "ga-80po0c.2.1",
			Invariant:       "untagged Small subprocess call/file totals cannot grow; reductions must lower this baseline",
			ResourceOwner:   "non-Medium lexical owners remove or replace each process call site",
			MigrationTarget: "D1/D2/D5/D6/E6",
			Expires:         "2026-10-01",
		},
		{
			Scope:           ScopeUntagged,
			Resource:        ResourceFixedSleep,
			BaselineCalls:   285,
			BaselineFiles:   112,
			ReportedCalls:   287,
			ReportedFiles:   113,
			OwnerBead:       "ga-80po0c.2.1",
			Invariant:       "untagged Small fixed-sleep call/file totals cannot grow; reductions must lower this baseline",
			ResourceOwner:   "non-Medium lexical owners replace elapsed wall time with lifecycle signals",
			MigrationTarget: "W1-W5",
			Expires:         "2026-10-01",
		},
		{
			Scope:           ScopeCmdGCUntagged,
			Resource:        ResourceEnvironment,
			BaselineCalls:   4325,
			BaselineFiles:   204,
			ReportedCalls:   4348,
			ReportedFiles:   200,
			OwnerBead:       "ga-80po0c.2.1",
			Invariant:       "untagged Small cmd/gc environment call/file totals cannot grow; reductions must lower this baseline",
			ResourceOwner:   "non-Medium lexical owners restore or eliminate every process-environment mutation",
			MigrationTarget: "D5/D6/E6",
			Expires:         "2026-10-01",
		},
		{
			Scope:           ScopeCmdGCUntagged,
			Resource:        ResourceCWD,
			BaselineCalls:   285,
			BaselineFiles:   43,
			ReportedCalls:   284,
			ReportedFiles:   43,
			OwnerBead:       "ga-80po0c.2.1",
			Invariant:       "untagged Small cmd/gc cwd call/file totals cannot grow; reductions must lower this baseline",
			ResourceOwner:   "non-Medium lexical owners restore or eliminate every cwd mutation",
			MigrationTarget: "D5/D6",
			Expires:         "2026-10-01",
		},
		{
			Scope:           ScopeCmdGCUntagged,
			Resource:        ResourceSlowProcessGate,
			BaselineCalls:   71,
			BaselineFiles:   24,
			ReportedCalls:   75,
			ReportedFiles:   25,
			OwnerBead:       "ga-80po0c.2.1",
			Invariant:       "untagged Small cmd/gc slow-process marker totals cannot grow; reductions must lower this baseline",
			ResourceOwner:   "each non-Medium marked caller retains an explicit process-suite migration owner",
			MigrationTarget: "D5/D6/E6",
			Expires:         "2026-10-01",
		},
		{
			Scope:           ScopeUntagged,
			Resource:        ResourceHTTPTestServer,
			BaselineCalls:   318,
			BaselineFiles:   67,
			ReportedCalls:   300,
			ReportedFiles:   66,
			OwnerBead:       "ga-80po0c.2.2",
			Invariant:       "untagged Small HTTP test server call/file totals cannot grow; reductions must lower this baseline",
			ResourceOwner:   "non-Medium lexical owners move server-backed tests to exact Medium ownership or replace the listener",
			MigrationTarget: "P0.4c",
			Expires:         "2026-10-01",
		},
		{
			Scope:           ScopeUntagged,
			Resource:        ResourceNetListen,
			BaselineCalls:   92,
			BaselineFiles:   34,
			ReportedCalls:   92,
			ReportedFiles:   34,
			OwnerBead:       "ga-80po0c.2.2",
			Invariant:       "untagged Small net.Listen call/file totals cannot grow; reductions must lower this baseline",
			ResourceOwner:   "non-Medium lexical owners move listener-backed tests to exact Medium ownership or replace the listener",
			MigrationTarget: "P0.4c",
			Expires:         "2026-10-01",
		},
		{
			Scope:           ScopeUntagged,
			Resource:        ResourceNetListenConfig,
			BaselineCalls:   1,
			BaselineFiles:   1,
			ReportedCalls:   1,
			ReportedFiles:   1,
			OwnerBead:       "ga-80po0c.2.2",
			Invariant:       "untagged Small net.ListenConfig.Listen call/file totals cannot grow; reductions must lower this baseline",
			ResourceOwner:   "non-Medium lexical owners move ListenConfig-backed tests to exact Medium ownership or replace the listener",
			MigrationTarget: "P0.4c",
			Expires:         "2026-10-01",
		},
		{
			Scope:           ScopeUntagged,
			Resource:        ResourceNetListenUnixgram,
			BaselineCalls:   3,
			BaselineFiles:   2,
			ReportedCalls:   3,
			ReportedFiles:   2,
			OwnerBead:       "ga-80po0c.2.2",
			Invariant:       "untagged Small net.ListenUnixgram call/file totals cannot grow; reductions must lower this baseline",
			ResourceOwner:   "non-Medium lexical owners move Unix datagram listener-backed tests to exact Medium ownership or replace the listener",
			MigrationTarget: "P0.4c",
			Expires:         "2026-10-01",
		},
		{
			Scope:           ScopeUntagged,
			Resource:        ResourceSyscallListen,
			BaselineCalls:   1,
			BaselineFiles:   1,
			ReportedCalls:   1,
			ReportedFiles:   1,
			OwnerBead:       "ga-80po0c.2.2",
			Invariant:       "untagged Small syscall.Listen call/file totals cannot grow; reductions must lower this baseline",
			ResourceOwner:   "non-Medium lexical owners move syscall-backed listener tests to exact Medium ownership or replace the listener",
			MigrationTarget: "P0.4c",
			Expires:         "2026-10-01",
		},
	},
}

// Occurrence is one syntax-owned resource use.
type Occurrence struct {
	Path        string
	PackageDir  string
	PackageName string
	Owner       string
	Runnable    bool
	Tagged      bool
	Resource    Resource
}

// Census is a deterministic collection of resource occurrences.
type Census struct {
	Occurrences    []Occurrence
	Runnables      []RunnableOwner
	hermeticSource *hermeticSourceIndex
}

// Count is the call-site and unique-file count for a scope/resource pair.
type Count struct {
	Calls int
	Files int
}

// Count returns the observed count for scope and resource.
func (c Census) Count(scope Scope, resource Resource) Count {
	files := map[string]struct{}{}
	count := Count{}
	for _, occurrence := range c.Occurrences {
		if occurrence.Resource != resource || !scopeContains(scope, occurrence) {
			continue
		}
		count.Calls++
		files[occurrence.Path] = struct{}{}
	}
	count.Files = len(files)
	return count
}

func scopeContains(scope Scope, occurrence Occurrence) bool {
	switch scope {
	case ScopeAll:
		return true
	case ScopeUntagged:
		return !occurrence.Tagged
	case ScopeCmdGCUntagged:
		return !occurrence.Tagged && strings.HasPrefix(occurrence.Path, "cmd/gc/")
	default:
		return false
	}
}

// ScanRepository scans the repository's tracked Go test files. Tracked sibling
// Go source supplies package-level declaration context but is never counted.
func ScanRepository(root string) (Census, error) {
	cmd := exec.Command("git", "-C", root, "ls-files", "-z", "--", "*.go")
	out, err := cmd.Output()
	if err != nil {
		return Census{}, fmt.Errorf("listing tracked Go source: %w", err)
	}
	parts := strings.Split(string(out), "\x00")
	files := make([]string, 0, len(parts))
	for _, name := range parts {
		if name != "" {
			files = append(files, filepath.ToSlash(name))
		}
	}
	return scanFiles(os.DirFS(root), files, reviewedHermeticPackages(bootstrapPolicy.ReviewedHermeticBody))
}

// ScanFS scans every *_test.go file in sourceFS. Sibling Go source supplies
// package-level declaration context but is never counted. ScanFS is intended
// for hermetic policy fixtures; repository checks use ScanRepository so
// untracked files do not perturb the checked baseline.
func ScanFS(sourceFS fs.FS) (Census, error) {
	var files []string
	err := fs.WalkDir(sourceFS, ".", func(name string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !entry.IsDir() && strings.HasSuffix(name, ".go") {
			files = append(files, filepath.ToSlash(name))
		}
		return nil
	})
	if err != nil {
		return Census{}, fmt.Errorf("walking test source: %w", err)
	}
	return scanFiles(sourceFS, files, nil)
}

func reviewedHermeticPackages(rows []ReviewedHermeticBody) map[packageKey]struct{} {
	packages := make(map[packageKey]struct{}, len(rows))
	for _, row := range rows {
		packages[packageKey{directory: row.PackageDir, packageName: row.PackageName}] = struct{}{}
	}
	return packages
}

type parsedFile struct {
	name        string
	directory   string
	packageName string
	tagged      bool
	file        *ast.File
	calls       []resourceCall
	bindings    bindingInfo
}

type bindingInfo struct {
	defs                       map[*ast.Ident]types.Object
	uses                       map[*ast.Ident]types.Object
	expressionTypes            map[ast.Expr]types.TypeAndValue
	packageDeclarations        map[string]struct{}
	unresolvedImportQualifiers map[string]struct{}
}

type packageKey struct {
	directory   string
	packageName string
}

type resourceCall struct {
	call     *ast.CallExpr
	owner    string
	runnable bool
}

type emptyPackageImporter struct {
	packages map[string]*types.Package
}

func newEmptyPackageImporter() *emptyPackageImporter {
	return &emptyPackageImporter{packages: make(map[string]*types.Package)}
}

func (importer *emptyPackageImporter) Import(importPath string) (*types.Package, error) {
	if imported, ok := importer.packages[importPath]; ok {
		return imported, nil
	}
	imported := types.NewPackage(importPath, path.Base(importPath))
	if importPath == "net" {
		// Seed only the receiver type the census needs so go/types can carry
		// ListenConfig identity through pointers and aliases without loading
		// host toolchain export data.
		name := types.NewTypeName(token.NoPos, imported, "ListenConfig", nil)
		types.NewNamed(name, types.NewStruct(nil, nil), nil)
		imported.Scope().Insert(name)
	}
	imported.MarkComplete()
	importer.packages[importPath] = imported
	return imported, nil
}

// These sets mirror internal/syslist.KnownOS and KnownArch in the repository's
// pinned Go toolchain. Go owns them as the past, present, and future names used
// for filename matching. Scanning remains hermetic, so a toolchain update must
// review these code-owned copies.
var knownGOOS = map[string]struct{}{
	"aix": {}, "android": {}, "darwin": {}, "dragonfly": {},
	"freebsd": {}, "hurd": {}, "illumos": {}, "ios": {}, "js": {}, "linux": {},
	"nacl":   {},
	"netbsd": {}, "openbsd": {}, "plan9": {}, "solaris": {},
	"wasip1": {}, "windows": {}, "zos": {},
}

var knownGOARCH = map[string]struct{}{
	"386": {}, "amd64": {}, "amd64p32": {},
	"arm": {}, "armbe": {}, "arm64": {}, "arm64be": {},
	"loong64": {},
	"mips":    {}, "mipsle": {}, "mips64": {}, "mips64le": {},
	"mips64p32": {}, "mips64p32le": {},
	"ppc": {}, "ppc64": {}, "ppc64le": {},
	"riscv": {}, "riscv64": {},
	"s390": {}, "s390x": {},
	"sparc": {}, "sparc64": {},
	"wasm": {},
}

func scanFiles(sourceFS fs.FS, names []string, hermeticPackages map[packageKey]struct{}) (Census, error) {
	sort.Strings(names)
	fileSet := token.NewFileSet()
	importer := newEmptyPackageImporter()
	var sources []parsedFile
	var hermeticSources []parsedFile
	var runnables []RunnableOwner
	packageDeclarations := make(map[packageKey]map[string]struct{})
	for _, name := range names {
		data, err := fs.ReadFile(sourceFS, name)
		if err != nil {
			return Census{}, fmt.Errorf("reading %s: %w", name, err)
		}
		file, err := parser.ParseFile(fileSet, name, data, parser.ParseComments|parser.SkipObjectResolution)
		if err != nil {
			return Census{}, fmt.Errorf("parsing %s: %w", name, err)
		}
		normalized := filepath.ToSlash(name)
		key := packageKey{directory: path.Dir(normalized), packageName: file.Name.Name}
		declarations := packageDeclarations[key]
		if declarations == nil {
			declarations = make(map[string]struct{})
			packageDeclarations[key] = declarations
		}
		recordPackageDeclarations(file, declarations)
		source := parsedFile{
			name:        normalized,
			directory:   key.directory,
			packageName: key.packageName,
			file:        file,
		}
		_, retainHermeticSource := hermeticPackages[key]
		retainHermeticSource = hermeticPackages == nil || retainHermeticSource
		if !strings.HasSuffix(name, "_test.go") {
			if retainHermeticSource {
				hermeticSources = append(hermeticSources, source)
			}
			continue
		}
		tagged, err := parsedBuildConstraint(data)
		if err != nil {
			return Census{}, fmt.Errorf("parsing build constraint in %s: %w", name, err)
		}
		if err := validateImports(file); err != nil {
			return Census{}, fmt.Errorf("scanning imports in %s: %w", name, err)
		}
		runnables = append(runnables, runnableOwners(file, key.directory, key.packageName)...)
		candidates := resourceCandidateCalls(file)
		source.tagged = tagged || hasImplicitPlatformConstraint(name)
		source.calls = candidates
		if retainHermeticSource {
			hermeticSources = append(hermeticSources, source)
		}
		scanned := len(candidates) > 0 || hasSlowHelperDeclarationCandidate(file)
		if !scanned {
			continue
		}
		sources = append(sources, source)
	}

	for index := range sources {
		source := &sources[index]
		bindings := resolveBindings(fileSet, source.file, importer, fmt.Sprintf("resourcecensus.local/file%d", index))
		bindings.packageDeclarations = packageDeclarations[source.groupKey()]
		bindings.unresolvedImportQualifiers = unresolvedDefaultImportQualifiers(source.file)
		source.bindings = bindings
	}

	slowHelpers := make(map[packageKey]types.Object)
	for _, source := range sources {
		for _, declaration := range source.file.Decls {
			function, ok := declaration.(*ast.FuncDecl)
			if !ok {
				continue
			}
			matched, err := isSlowHelperDeclaration(function, source.bindings)
			if err != nil {
				return Census{}, fmt.Errorf("scanning slow-process helper in %s: %w", source.name, err)
			}
			if !matched {
				continue
			}
			key := source.groupKey()
			if _, exists := slowHelpers[key]; exists {
				return Census{}, fmt.Errorf("scanning slow-process helper in %s: package %s has multiple canonical declarations", source.name, source.packageName)
			}
			object := source.bindings.defs[function.Name]
			if object == nil {
				return Census{}, fmt.Errorf("scanning slow-process helper in %s: declaration has no lexical binding", source.name)
			}
			slowHelpers[key] = object
		}
	}

	census := Census{
		Runnables: uniqueSortedRunnables(runnables),
		hermeticSource: &hermeticSourceIndex{
			fileSet:             fileSet,
			files:               hermeticSources,
			packageDeclarations: packageDeclarations,
		},
	}
	for _, source := range sources {
		testingObjects, err := testingParameterObjects(source.file, source.bindings)
		if err != nil {
			return Census{}, fmt.Errorf("scanning testing parameters in %s: %w", source.name, err)
		}
		for _, declaration := range source.file.Decls {
			function, ok := declaration.(*ast.FuncDecl)
			if !ok {
				continue
			}
			matched, err := isSlowHelperDeclaration(function, source.bindings)
			if err != nil {
				return Census{}, fmt.Errorf("scanning slow-process helper in %s: %w", source.name, err)
			}
			if matched {
				census.add(source, function.Name.Name, false, ResourceSlowProcessGate)
			}
		}

		for _, candidate := range source.calls {
			resources, err := matchedResourcesForCall(candidate.call, source.bindings, testingObjects, slowHelpers[source.groupKey()])
			if err != nil {
				return Census{}, fmt.Errorf("scanning resource calls in %s: %w", source.name, err)
			}
			for _, resource := range resources {
				census.add(source, candidate.owner, candidate.runnable, resource)
			}
		}
	}

	sort.Slice(census.Occurrences, func(i, j int) bool {
		left, right := census.Occurrences[i], census.Occurrences[j]
		if left.Path != right.Path {
			return left.Path < right.Path
		}
		if left.Owner != right.Owner {
			return left.Owner < right.Owner
		}
		return left.Resource < right.Resource
	})
	return census, nil
}

func (p parsedFile) groupKey() packageKey {
	return packageKey{directory: p.directory, packageName: p.packageName}
}

func (c *Census) add(source parsedFile, owner string, runnable bool, resource Resource) {
	c.Occurrences = append(c.Occurrences, Occurrence{
		Path:        source.name,
		PackageDir:  source.directory,
		PackageName: source.packageName,
		Owner:       owner,
		Runnable:    runnable,
		Tagged:      source.tagged,
		Resource:    resource,
	})
}

func parsedBuildConstraint(content []byte) (bool, error) {
	// Match go/build: one UTF-8 BOM is permitted only at the start of a Go
	// source file and is removed before the leading build header is parsed.
	content = bytes.TrimPrefix(content, []byte{0xef, 0xbb, 0xbf})
	header, goBuild, err := leadingBuildHeader(content)
	if err != nil {
		return false, err
	}
	if goBuild != nil {
		if _, err := constraint.Parse(string(goBuild)); err != nil {
			return false, err
		}
		return true, nil
	}
	for len(header) > 0 {
		line := header
		if index := bytes.IndexByte(line, '\n'); index >= 0 {
			line, header = line[:index], header[index+1:]
		} else {
			header = nil
		}
		text := string(bytes.TrimSpace(line))
		if !constraint.IsPlusBuild(text) {
			continue
		}
		// go/build ignores malformed legacy constraints.
		if _, err := constraint.Parse(text); err == nil {
			return true, nil
		}
	}
	return false, nil
}

// leadingBuildHeader mirrors the placement rules in go/build.parseFileHeader:
// modern constraints may appear before the package clause, while legacy
// constraints must precede the last separating blank in the leading // block.
func leadingBuildHeader(content []byte) (header, goBuild []byte, err error) {
	end := 0
	rest := content
	ended := false
	inBlock := false

Lines:
	for len(rest) > 0 {
		line := rest
		if index := bytes.IndexByte(line, '\n'); index >= 0 {
			line, rest = line[:index], rest[index+1:]
		} else {
			rest = nil
		}
		line = bytes.TrimSpace(line)
		if len(line) == 0 && !ended {
			end = len(content) - len(rest)
			continue
		}
		if !bytes.HasPrefix(line, []byte("//")) {
			ended = true
		}
		if !inBlock && constraint.IsGoBuild(string(line)) {
			if goBuild != nil {
				return nil, nil, errors.New("multiple //go:build comments")
			}
			goBuild = line
		}

		for len(line) > 0 {
			if inBlock {
				if index := bytes.Index(line, []byte("*/")); index >= 0 {
					inBlock = false
					line = bytes.TrimSpace(line[index+2:])
					continue
				}
				continue Lines
			}
			switch {
			case bytes.HasPrefix(line, []byte("//")):
				continue Lines
			case bytes.HasPrefix(line, []byte("/*")):
				inBlock = true
				line = bytes.TrimSpace(line[2:])
			default:
				break Lines
			}
		}
	}
	return content[:end], goBuild, nil
}

func hasImplicitPlatformConstraint(name string) bool {
	base := path.Base(filepath.ToSlash(name))
	stem, _, _ := strings.Cut(base, ".")
	stem = strings.TrimSuffix(stem, "_test")
	parts := strings.Split(stem, "_")
	if len(parts) < 2 {
		return false
	}
	last := parts[len(parts)-1]
	if _, ok := knownGOOS[last]; ok {
		return true
	}
	_, ok := knownGOARCH[last]
	return ok
}

func validateImports(file *ast.File) error {
	for _, spec := range file.Imports {
		importPath, err := strconv.Unquote(spec.Path.Value)
		if err != nil {
			return fmt.Errorf("decoding import path %s: %w", spec.Path.Value, err)
		}
		if spec.Name != nil && spec.Name.Name == "_" {
			continue
		}
		if spec.Name != nil && spec.Name.Name == "." {
			if importPath == "net" || importPath == "os/exec" || importPath == "time" || importPath == "os" || importPath == "syscall" || importPath == "testing" || importPath == "net/http/httptest" {
				return fmt.Errorf("targeted dot import %q cannot be counted safely", importPath)
			}
		}
	}
	return nil
}

func resourceCandidateCalls(file *ast.File) []resourceCall {
	aliases := testingImportAliases(file)
	var calls []resourceCall
	for _, declaration := range file.Decls {
		function, ok := declaration.(*ast.FuncDecl)
		if ok {
			calls = appendResourceCandidateCalls(calls, function.Body, function.Name.Name, isRunnableOwner(function, aliases))
			continue
		}
		calls = appendResourceCandidateCalls(calls, declaration, "", false)
	}
	return calls
}

func appendResourceCandidateCalls(calls []resourceCall, node ast.Node, owner string, runnable bool) []resourceCall {
	ast.Inspect(node, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		switch function := unparen(call.Fun).(type) {
		case *ast.SelectorExpr:
			switch function.Sel.Name {
			case "Command", "CommandContext", "Sleep", "Setenv", "Unsetenv", "Clearenv", "Chdir", "Listen", "ListenUnixgram", "NewServer", "NewTLSServer", "NewUnstartedServer":
				calls = append(calls, resourceCall{call: call, owner: owner, runnable: runnable})
			}
		case *ast.Ident:
			if function.Name == "skipSlowCmdGCTest" {
				calls = append(calls, resourceCall{call: call, owner: owner, runnable: runnable})
			}
		}
		return true
	})
	return calls
}

func runnableOwners(file *ast.File, packageDir, packageName string) []RunnableOwner {
	aliases := testingImportAliases(file)
	var owners []RunnableOwner
	for _, declaration := range file.Decls {
		function, ok := declaration.(*ast.FuncDecl)
		if !ok || !isRunnableOwner(function, aliases) {
			continue
		}
		owners = append(owners, RunnableOwner{PackageDir: packageDir, PackageName: packageName, Owner: function.Name.Name})
	}
	return owners
}

func testingImportAliases(file *ast.File) map[string]struct{} {
	aliases := make(map[string]struct{})
	for _, spec := range file.Imports {
		importPath, err := strconv.Unquote(spec.Path.Value)
		if err != nil || importPath != "testing" {
			continue
		}
		if spec.Name == nil {
			aliases["testing"] = struct{}{}
			continue
		}
		if spec.Name.Name != "." && spec.Name.Name != "_" {
			aliases[spec.Name.Name] = struct{}{}
		}
	}
	return aliases
}

func isRunnableOwner(function *ast.FuncDecl, testingAliases map[string]struct{}) bool {
	if function.Recv != nil || function.Type.TypeParams != nil || function.Type.Params == nil || functionParameterCount(function.Type.Params) != 1 || functionParameterCount(function.Type.Results) != 0 {
		return false
	}
	wantType := ""
	switch {
	case function.Name.Name == "TestMain":
		wantType = "M"
	case goTestName(function.Name.Name, "Test"):
		wantType = "T"
	case goTestName(function.Name.Name, "Benchmark"):
		wantType = "B"
	case goTestName(function.Name.Name, "Fuzz"):
		wantType = "F"
	default:
		return false
	}
	field := function.Type.Params.List[0]
	pointer, ok := unparen(field.Type).(*ast.StarExpr)
	if !ok {
		return false
	}
	selector, ok := unparen(pointer.X).(*ast.SelectorExpr)
	if !ok || selector.Sel.Name != wantType {
		return false
	}
	qualifier, ok := unparen(selector.X).(*ast.Ident)
	if !ok {
		return false
	}
	_, ok = testingAliases[qualifier.Name]
	return ok
}

func goTestName(name, prefix string) bool {
	if !strings.HasPrefix(name, prefix) {
		return false
	}
	if len(name) == len(prefix) {
		return true
	}
	next, _ := utf8.DecodeRuneInString(name[len(prefix):])
	return !unicode.IsLower(next)
}

func uniqueSortedRunnables(runnables []RunnableOwner) []RunnableOwner {
	sort.Slice(runnables, func(i, j int) bool {
		left, right := runnables[i], runnables[j]
		if left.PackageDir != right.PackageDir {
			return left.PackageDir < right.PackageDir
		}
		if left.PackageName != right.PackageName {
			return left.PackageName < right.PackageName
		}
		return left.Owner < right.Owner
	})
	result := runnables[:0]
	for _, runnable := range runnables {
		if len(result) == 0 || result[len(result)-1] != runnable {
			result = append(result, runnable)
		}
	}
	return result
}

func resolveBindings(fileSet *token.FileSet, file *ast.File, importer types.Importer, packagePath string) bindingInfo {
	info := bindingInfo{
		defs:            make(map[*ast.Ident]types.Object),
		uses:            make(map[*ast.Ident]types.Object),
		expressionTypes: make(map[ast.Expr]types.TypeAndValue),
	}
	receivers := netListenReceiverExpressions(file)
	var checkedExpressionTypes map[ast.Expr]types.TypeAndValue
	if len(receivers) > 0 {
		checkedExpressionTypes = make(map[ast.Expr]types.TypeAndValue)
	}
	config := types.Config{
		Importer:                 importer,
		DisableUnusedImportCheck: true,
		IgnoreFuncBodies:         false,
		Error:                    func(error) {},
	}
	_, _ = config.Check(packagePath, fileSet, []*ast.File{file}, &types.Info{
		Defs:  info.defs,
		Uses:  info.uses,
		Types: checkedExpressionTypes,
	})
	for _, receiver := range receivers {
		if typeAndValue, ok := checkedExpressionTypes[receiver]; ok {
			info.expressionTypes[receiver] = typeAndValue
		}
	}
	return info
}

func netListenReceiverExpressions(file *ast.File) []ast.Expr {
	hasNetImport := false
	for _, spec := range file.Imports {
		importPath, err := strconv.Unquote(spec.Path.Value)
		if err == nil && importPath == "net" && (spec.Name == nil || spec.Name.Name != "_") {
			hasNetImport = true
			break
		}
	}
	if !hasNetImport {
		return nil
	}

	var receivers []ast.Expr
	ast.Inspect(file, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		selector, ok := unparen(call.Fun).(*ast.SelectorExpr)
		if ok && selector.Sel.Name == "Listen" {
			receivers = append(receivers, unparen(selector.X))
		}
		return true
	})
	return receivers
}

func recordPackageDeclarations(file *ast.File, declarations map[string]struct{}) {
	for _, declaration := range file.Decls {
		switch declaration := declaration.(type) {
		case *ast.FuncDecl:
			if declaration.Recv == nil {
				declarations[declaration.Name.Name] = struct{}{}
			}
		case *ast.GenDecl:
			for _, spec := range declaration.Specs {
				switch spec := spec.(type) {
				case *ast.TypeSpec:
					declarations[spec.Name.Name] = struct{}{}
				case *ast.ValueSpec:
					for _, name := range spec.Names {
						declarations[name.Name] = struct{}{}
					}
				}
			}
		}
	}
}

// unresolvedDefaultImportQualifiers returns common versioned-import package
// names that the hermetic path.Base importer cannot derive.
func unresolvedDefaultImportQualifiers(file *ast.File) map[string]struct{} {
	qualifiers := make(map[string]struct{})
	for _, spec := range file.Imports {
		if spec.Name != nil {
			continue
		}
		importPath, err := strconv.Unquote(spec.Path.Value)
		if err != nil {
			continue
		}
		base := path.Base(importPath)
		if isVersionSegment(base) {
			qualifier := path.Base(path.Dir(importPath))
			if token.IsIdentifier(qualifier) {
				qualifiers[qualifier] = struct{}{}
			}
			continue
		}
		if index := strings.LastIndex(base, ".v"); index > 0 && isVersionSegment(base[index+1:]) {
			qualifier := base[:index]
			if token.IsIdentifier(qualifier) {
				qualifiers[qualifier] = struct{}{}
			}
		}
	}
	return qualifiers
}

func isVersionSegment(value string) bool {
	if len(value) < 2 || value[0] != 'v' {
		return false
	}
	for _, character := range value[1:] {
		if character < '0' || character > '9' {
			return false
		}
	}
	return true
}

func hasSlowHelperDeclarationCandidate(file *ast.File) bool {
	for _, declaration := range file.Decls {
		function, ok := declaration.(*ast.FuncDecl)
		if ok && function.Name.Name == "skipSlowCmdGCTest" {
			return true
		}
	}
	return false
}

func testingParameterObjects(file *ast.File, bindings bindingInfo) (map[types.Object]bool, error) {
	objects := make(map[types.Object]bool)
	var inspectErr error
	ast.Inspect(file, func(node ast.Node) bool {
		if inspectErr != nil {
			return false
		}
		var function *ast.FuncType
		switch node := node.(type) {
		case *ast.FuncDecl:
			function = node.Type
		case *ast.FuncLit:
			function = node.Type
		default:
			return true
		}
		if function.Params == nil {
			return true
		}
		for _, field := range function.Params.List {
			matched, err := isTestingParameterType(field.Type, bindings)
			if err != nil {
				inspectErr = err
				return false
			}
			if !matched {
				continue
			}
			for _, name := range field.Names {
				object := bindings.defs[name]
				if object == nil {
					inspectErr = fmt.Errorf("testing parameter %q has no lexical binding", name.Name)
					return false
				}
				objects[object] = true
			}
		}
		return true
	})
	return objects, inspectErr
}

func isNetListenConfigType(expression ast.Expr, bindings bindingInfo) (bool, error) {
	if expression == nil {
		return false, nil
	}
	expression = unparen(expression)
	if pointer, ok := expression.(*ast.StarExpr); ok {
		expression = pointer.X
	}
	return isImportedType(expression, bindings, "net", "ListenConfig")
}

func isNetListenConfigValue(expression ast.Expr, bindings bindingInfo) (bool, error) {
	expression = unparen(expression)
	if address, ok := expression.(*ast.UnaryExpr); ok && address.Op == token.AND {
		expression = unparen(address.X)
	}
	composite, ok := expression.(*ast.CompositeLit)
	if !ok {
		return false, nil
	}
	return isNetListenConfigType(composite.Type, bindings)
}

func isNetListenConfigCall(call *ast.CallExpr, bindings bindingInfo) (bool, error) {
	selector, ok := unparen(call.Fun).(*ast.SelectorExpr)
	if !ok || selector.Sel.Name != "Listen" {
		return false, nil
	}
	receiver := unparen(selector.X)
	if typeAndValue, ok := bindings.expressionTypes[receiver]; ok && typeAndValue.Type != nil {
		return isNetListenConfigObjectType(typeAndValue.Type), nil
	}
	direct, err := isNetListenConfigValue(receiver, bindings)
	if err != nil || direct {
		return direct, err
	}
	identifier, ok := receiver.(*ast.Ident)
	if !ok {
		return false, nil
	}
	object := bindings.uses[identifier]
	if object == nil {
		if _, declared := bindings.packageDeclarations[identifier.Name]; declared {
			return false, nil
		}
		if _, imported := bindings.unresolvedImportQualifiers[identifier.Name]; imported {
			return false, nil
		}
		return false, fmt.Errorf("net.ListenConfig receiver %q has no lexical binding", identifier.Name)
	}
	return isNetListenConfigObjectType(object.Type()), nil
}

func isNetListenConfigObjectType(objectType types.Type) bool {
	objectType = types.Unalias(objectType)
	if pointer, ok := objectType.(*types.Pointer); ok {
		objectType = types.Unalias(pointer.Elem())
	}
	named, ok := objectType.(*types.Named)
	if !ok || named.Obj().Pkg() == nil {
		return false
	}
	return named.Obj().Name() == "ListenConfig" && named.Obj().Pkg().Path() == "net"
}

func isTestingParameterType(expression ast.Expr, bindings bindingInfo) (bool, error) {
	expression = unparen(expression)
	if pointer, ok := expression.(*ast.StarExpr); ok {
		return isImportedType(pointer.X, bindings, "testing", "T")
	}
	return isImportedType(expression, bindings, "testing", "TB")
}

func isImportedType(expression ast.Expr, bindings bindingInfo, importPath, typeName string) (bool, error) {
	selector, ok := unparen(expression).(*ast.SelectorExpr)
	if !ok || selector.Sel.Name != typeName {
		return false, nil
	}
	identifier, ok := unparen(selector.X).(*ast.Ident)
	if !ok {
		return false, nil
	}
	return isImportedQualifier(identifier, bindings, importPath)
}

func isTestingCall(call *ast.CallExpr, bindings bindingInfo, testingObjects map[types.Object]bool, method string) (bool, error) {
	selector, ok := unparen(call.Fun).(*ast.SelectorExpr)
	if !ok || selector.Sel.Name != method {
		return false, nil
	}
	identifier, ok := unparen(selector.X).(*ast.Ident)
	if !ok {
		return false, nil
	}
	object := bindings.uses[identifier]
	if object == nil {
		if _, declared := bindings.packageDeclarations[identifier.Name]; declared {
			return false, nil
		}
		if _, imported := bindings.unresolvedImportQualifiers[identifier.Name]; imported {
			return false, nil
		}
		return false, fmt.Errorf("testing resource receiver %q has no lexical binding", identifier.Name)
	}
	return testingObjects[object], nil
}

func isSlowHelperDeclaration(function *ast.FuncDecl, bindings bindingInfo) (bool, error) {
	if function.Recv != nil || function.Name.Name != "skipSlowCmdGCTest" || function.Type.Params == nil {
		return false, nil
	}
	if functionParameterCount(function.Type.Results) != 0 || functionParameterCount(function.Type.Params) != 2 || len(function.Type.Params.List) != 2 {
		return false, nil
	}
	firstType := unparen(function.Type.Params.List[0].Type)
	pointer, ok := firstType.(*ast.StarExpr)
	if !ok {
		return false, nil
	}
	first, err := isImportedType(pointer.X, bindings, "testing", "T")
	if err != nil || !first {
		return false, err
	}
	second, ok := unparen(function.Type.Params.List[1].Type).(*ast.Ident)
	if !ok || bindings.uses[second] != types.Universe.Lookup("string") {
		return false, nil
	}
	return true, nil
}

func functionParameterCount(fields *ast.FieldList) int {
	if fields == nil {
		return 0
	}
	count := 0
	for _, field := range fields.List {
		if len(field.Names) == 0 {
			count++
		} else {
			count += len(field.Names)
		}
	}
	return count
}

func isSlowHelperCall(call *ast.CallExpr, bindings bindingInfo, ownership types.Object) bool {
	if ownership == nil || len(call.Args) != 2 {
		return false
	}
	identifier, ok := unparen(call.Fun).(*ast.Ident)
	if !ok || identifier.Name != "skipSlowCmdGCTest" {
		return false
	}
	object := bindings.uses[identifier]
	return object == nil || object == ownership
}

func isImportedCall(call *ast.CallExpr, bindings bindingInfo, importPath string, names ...string) (bool, error) {
	selector, ok := unparen(call.Fun).(*ast.SelectorExpr)
	if !ok {
		return false, nil
	}
	identifier, ok := unparen(selector.X).(*ast.Ident)
	if !ok {
		return false, nil
	}
	matchedName := false
	for _, name := range names {
		if selector.Sel.Name == name {
			matchedName = true
			break
		}
	}
	if !matchedName {
		return false, nil
	}
	return isImportedQualifier(identifier, bindings, importPath)
}

func isImportedQualifier(identifier *ast.Ident, bindings bindingInfo, importPath string) (bool, error) {
	binding, ok := bindings.uses[identifier]
	if !ok || binding == nil {
		if _, declared := bindings.packageDeclarations[identifier.Name]; declared {
			return false, nil
		}
		if _, imported := bindings.unresolvedImportQualifiers[identifier.Name]; imported {
			return false, nil
		}
		return false, fmt.Errorf("resource candidate qualifier %q has no lexical binding", identifier.Name)
	}
	packageName, ok := binding.(*types.PkgName)
	if !ok {
		return false, nil
	}
	imported := packageName.Imported()
	if imported == nil {
		return false, fmt.Errorf("resource candidate qualifier %q has unusable package binding for %q", identifier.Name, importPath)
	}
	return imported.Path() == importPath, nil
}

func unparen(expression ast.Expr) ast.Expr {
	for {
		parenthesized, ok := expression.(*ast.ParenExpr)
		if !ok {
			return expression
		}
		expression = parenthesized.X
	}
}

// ParseLedger decodes a ledger and rejects undeclared fields.
func ParseLedger(data []byte) (Ledger, error) {
	var ledger Ledger
	metadata, err := toml.Decode(string(data), &ledger)
	if err != nil {
		return Ledger{}, fmt.Errorf("decode resource ledger: %w", err)
	}
	if undecoded := metadata.Undecoded(); len(undecoded) > 0 {
		fields := make([]string, 0, len(undecoded))
		for _, key := range undecoded {
			fields = append(fields, key.String())
		}
		sort.Strings(fields)
		return Ledger{}, fmt.Errorf("unknown ledger field: %s", strings.Join(fields, ", "))
	}
	return ledger, nil
}

// LoadLedger loads a checked resource ledger from disk.
func LoadLedger(name string) (Ledger, error) {
	data, err := os.ReadFile(name)
	if err != nil {
		return Ledger{}, err
	}
	return ParseLedger(data)
}

// Validate checks schema ownership, expiration, and exact census baselines.
func Validate(ledger Ledger, census Census, now time.Time) error {
	return validateAgainstPolicy(bootstrapPolicy, ledger, census, now)
}

func validateAgainstPolicy(policy, ledger Ledger, census Census, now time.Time) error {
	if problems := validateManifestAgainstPolicy(policy, ledger, now); len(problems) > 0 {
		sort.Strings(problems)
		return errors.New(strings.Join(problems, "\n"))
	}
	if err := validateMediumOwners(ledger.Medium, census, now); err != nil {
		return err
	}
	if err := validateReviewedHermeticBodies(ledger.ReviewedHermeticBody, census); err != nil {
		return err
	}

	var problems []string
	for _, baseline := range ledger.AuditBaseline {
		prefix := fmt.Sprintf("audit baseline scope=%s resource=%s", baseline.Scope, baseline.Resource)
		problems = append(problems, validateBaseline(prefix, baseline, census)...)
	}
	for _, debt := range ledger.Debt {
		prefix := fmt.Sprintf("debt baseline scope=%s resource=%s", debt.Scope, debt.Resource)
		problems = append(problems, validateBaseline(prefix, debt, census)...)
	}
	for _, debt := range ledger.SmallDebt {
		problems = append(problems, validateSmallBaseline(debt, census, ledger.Medium)...)
	}
	if len(problems) == 0 {
		return nil
	}
	sort.Strings(problems)
	return errors.New(strings.Join(problems, "\n"))
}

func validateManifestAgainstPolicy(policy, ledger Ledger, now time.Time) []string {
	var problems []string
	if policy.Version != 2 {
		problems = append(problems, fmt.Sprintf("bootstrap policy version = %d, want 2", policy.Version))
	}
	if ledger.Version != policy.Version {
		problems = append(problems, fmt.Sprintf("ledger version = %d, bootstrap policy requires %d", ledger.Version, policy.Version))
	}
	problems = append(problems, validateRowsAgainstPolicy("audit", policy.AuditBaseline, ledger.AuditBaseline, now)...)
	problems = append(problems, validateRowsAgainstPolicy("debt", policy.Debt, ledger.Debt, now)...)
	problems = append(problems, validateMediumRowsAgainstPolicy(policy.Medium, ledger.Medium, now)...)
	problems = append(problems, validateReviewedHermeticRowsAgainstPolicy(policy.ReviewedHermeticBody, ledger.ReviewedHermeticBody)...)
	problems = append(problems, validateRowsAgainstPolicy("small debt", policy.SmallDebt, ledger.SmallDebt, now)...)
	return problems
}

func validateRowsAgainstPolicy(kind string, policyRows, ledgerRows []Baseline, now time.Time) []string {
	var problems []string
	policyByKey := map[baselineKey]Baseline{}
	for _, row := range policyRows {
		key := baselineKey{row.Scope, row.Resource}
		prefix := fmt.Sprintf("bootstrap %s baseline scope=%s resource=%s", kind, row.Scope, row.Resource)
		if _, exists := policyByKey[key]; exists {
			problems = append(problems, fmt.Sprintf("duplicate bootstrap %s baseline: scope=%s resource=%s", kind, row.Scope, row.Resource))
		}
		policyByKey[key] = row
		problems = append(problems, validateBaselineDefinition(prefix, row, now)...)
	}

	seen := map[baselineKey]bool{}
	for _, row := range ledgerRows {
		key := baselineKey{row.Scope, row.Resource}
		prefix := fmt.Sprintf("%s baseline scope=%s resource=%s", kind, row.Scope, row.Resource)
		if seen[key] {
			problems = append(problems, fmt.Sprintf("duplicate %s baseline: scope=%s resource=%s", kind, row.Scope, row.Resource))
		}
		seen[key] = true
		problems = append(problems, validateBaselineDefinition(prefix, row, now)...)
		want, exists := policyByKey[key]
		if !exists {
			problems = append(problems, fmt.Sprintf("unexpected %s baseline: scope=%s resource=%s", kind, row.Scope, row.Resource))
			continue
		}
		problems = append(problems, comparePolicyFields(prefix, row, want)...)
	}
	for key := range policyByKey {
		if !seen[key] {
			problems = append(problems, fmt.Sprintf("missing required %s baseline: scope=%s resource=%s", kind, key.scope, key.resource))
		}
	}
	return problems
}

func comparePolicyFields(prefix string, got, want Baseline) []string {
	var problems []string
	for _, field := range []struct {
		name      string
		got, want int
	}{
		{"baseline_calls", got.BaselineCalls, want.BaselineCalls},
		{"baseline_files", got.BaselineFiles, want.BaselineFiles},
		{"reported_calls", got.ReportedCalls, want.ReportedCalls},
		{"reported_files", got.ReportedFiles, want.ReportedFiles},
	} {
		if field.got != field.want {
			problems = append(problems, fmt.Sprintf("%s: %s = %d, bootstrap policy requires %d", prefix, field.name, field.got, field.want))
		}
	}
	for _, field := range []struct {
		name      string
		got, want string
	}{
		{"owner_bead", got.OwnerBead, want.OwnerBead},
		{"invariant", got.Invariant, want.Invariant},
		{"resource_owner", got.ResourceOwner, want.ResourceOwner},
		{"migration_target", got.MigrationTarget, want.MigrationTarget},
		{"expires", got.Expires, want.Expires},
	} {
		if field.got != field.want {
			problems = append(problems, fmt.Sprintf("%s: %s = %q, bootstrap policy requires %q", prefix, field.name, field.got, field.want))
		}
	}
	return problems
}

func validateBaselineDefinition(prefix string, row Baseline, now time.Time) []string {
	var problems []string
	if !knownScope(row.Scope) {
		problems = append(problems, fmt.Sprintf("%s: unknown scope %q", prefix, row.Scope))
	}
	if _, ok := knownResources[row.Resource]; !ok {
		problems = append(problems, fmt.Sprintf("%s: unknown resource %q", prefix, row.Resource))
	}
	if row.BaselineCalls < 0 || row.BaselineFiles < 0 {
		problems = append(problems, prefix+": baselines must be non-negative")
	}
	if row.ReportedCalls < 0 || row.ReportedFiles < 0 {
		problems = append(problems, prefix+": historical census must be non-negative")
	}
	problems = append(problems, validateOwnership(prefix, row, now)...)
	return problems
}

func validateBaseline(prefix string, row Baseline, census Census) []string {
	if row.BaselineCalls < 0 || row.BaselineFiles < 0 {
		return []string{prefix + ": baselines must be non-negative"}
	}
	actual := census.Count(row.Scope, row.Resource)
	switch {
	case actual.Calls > row.BaselineCalls || actual.Files > row.BaselineFiles:
		return []string{fmt.Sprintf("source resource census grew: scope=%s resource=%s calls=%d (baseline %d), files=%d (baseline %d)", row.Scope, row.Resource, actual.Calls, row.BaselineCalls, actual.Files, row.BaselineFiles)}
	case actual.Calls < row.BaselineCalls || actual.Files < row.BaselineFiles:
		return []string{fmt.Sprintf("source resource census baseline is stale: scope=%s resource=%s calls=%d (baseline %d), files=%d (baseline %d); lower the checked baseline to bank the improvement", row.Scope, row.Resource, actual.Calls, row.BaselineCalls, actual.Files, row.BaselineFiles)}
	default:
		return nil
	}
}

func knownScope(scope Scope) bool {
	return scope == ScopeAll || scope == ScopeUntagged || scope == ScopeCmdGCUntagged
}

func validateOwnership(prefix string, row Baseline, now time.Time) []string {
	return validateOwnershipFields(prefix, row.OwnerBead, row.Invariant, row.ResourceOwner, row.MigrationTarget, row.Expires, now)
}

func validateOwnershipFields(prefix, owner, invariant, resourceOwner, migration, expiryText string, now time.Time) []string {
	var problems []string
	for name, value := range map[string]string{
		"owner_bead":       owner,
		"invariant":        invariant,
		"resource_owner":   resourceOwner,
		"migration_target": migration,
	} {
		if strings.TrimSpace(value) == "" {
			problems = append(problems, fmt.Sprintf("%s: %s is required", prefix, name))
		}
	}
	expiry, err := time.Parse("2006-01-02", expiryText)
	if err != nil {
		problems = append(problems, fmt.Sprintf("%s: expiry %q must use YYYY-MM-DD", prefix, expiryText))
	} else if expiry.Before(day(now)) {
		problems = append(problems, fmt.Sprintf("%s: expired %s", prefix, expiryText))
	}
	return problems
}

func day(value time.Time) time.Time {
	value = value.UTC()
	return time.Date(value.Year(), value.Month(), value.Day(), 0, 0, 0, 0, time.UTC)
}

// RenderMarkdown renders the exact checked TESTING.md inventory block.
func RenderMarkdown(ledger Ledger) string {
	type row struct {
		kind      string
		scope     string
		baseline  string
		owner     string
		invariant string
		migration string
		expiry    string
	}
	var rows []row
	appendRows := func(kind string, baselines []Baseline) {
		for _, baseline := range baselines {
			rows = append(rows, row{
				kind:      kind,
				scope:     renderedSourceScope(baseline.Scope),
				baseline:  renderedBaseline(baseline),
				owner:     baseline.OwnerBead,
				invariant: baseline.Invariant + "; " + baseline.ResourceOwner,
				migration: baseline.MigrationTarget,
				expiry:    baseline.Expires,
			})
		}
	}
	appendRows("Audit baseline", ledger.AuditBaseline)
	for _, medium := range ledger.Medium {
		resources := make([]string, 0, len(medium.Resources))
		for _, resource := range medium.Resources {
			resources = append(resources, string(resource))
		}
		rows = append(rows, row{
			kind:      "Medium owner",
			scope:     fmt.Sprintf("`%s` package `%s`", medium.PackageDir, medium.PackageName),
			baseline:  medium.Owner + ": " + strings.Join(resources, ", "),
			owner:     medium.OwnerBead,
			invariant: medium.Invariant + "; " + medium.ResourceOwner,
			migration: medium.MigrationTarget,
			expiry:    medium.Expires,
		})
	}
	appendRows("Small debt ratchet", ledger.SmallDebt)
	appendRows("Source debt ratchet", ledger.Debt)
	sort.Slice(rows, func(i, j int) bool {
		left := rows[i].kind + "\x00" + rows[i].scope + "\x00" + rows[i].baseline
		right := rows[j].kind + "\x00" + rows[j].scope + "\x00" + rows[j].baseline
		return left < right
	})

	var output strings.Builder
	output.WriteString(markdownBegin)
	output.WriteString("\n| Ledger kind | Source scope | Resource baseline | Tracking owner | Invariant / resource owner | Migration | Expiry |\n")
	output.WriteString("| --- | --- | --- | --- | --- | --- | --- |\n")
	for _, row := range rows {
		fmt.Fprintf(&output, "| %s | %s | %s | %s | %s | %s | %s |\n",
			row.kind, row.scope, row.baseline, row.owner, row.invariant, row.migration, row.expiry)
	}
	if len(ledger.ReviewedHermeticBody) > 0 {
		reviewed := append([]ReviewedHermeticBody(nil), ledger.ReviewedHermeticBody...)
		sort.Slice(reviewed, func(i, j int) bool {
			left := reviewed[i].PackageDir + "\x00" + reviewed[i].PackageName + "\x00" + reviewed[i].Owner
			right := reviewed[j].PackageDir + "\x00" + reviewed[j].PackageName + "\x00" + reviewed[j].Owner
			return left < right
		})
		output.WriteString("\n| Reviewed hermetic body | Effective runnable size | Medium reason | Retained real composition owner |\n")
		output.WriteString("| --- | --- | --- | --- |\n")
		for _, body := range reviewed {
			retained := "—"
			if owner, exists := retainedRealOwnerFor(reviewedHermeticBodyKey(body)); exists {
				retained = fmt.Sprintf("`%s` package `%s` — %s", owner.packageDir, owner.packageName, owner.owner)
			}
			fmt.Fprintf(&output, "| `%s` package `%s` — %s | %s | %s | %s |\n",
				body.PackageDir, body.PackageName, body.Owner, body.EffectiveSize, body.MediumReason, retained)
		}
	}
	output.WriteString(markdownEnd)
	return output.String()
}

func renderedSourceScope(scope Scope) string {
	switch scope {
	case ScopeAll:
		return "all tracked test source"
	case ScopeUntagged:
		return "all untagged test source"
	case ScopeCmdGCUntagged:
		return "`cmd/gc` untagged test source"
	default:
		return string(scope)
	}
}

func renderedBaseline(row Baseline) string {
	result := fmt.Sprintf("%s: %d calls / %d files", row.Resource, row.BaselineCalls, row.BaselineFiles)
	if row.ReportedCalls != 0 && (row.ReportedCalls != row.BaselineCalls || row.ReportedFiles != row.BaselineFiles) {
		result += fmt.Sprintf(" (historical regex census: %d / %d)", row.ReportedCalls, row.ReportedFiles)
	}
	return result
}

const (
	markdownBegin = "<!-- BEGIN CHECKED TEST RESOURCE LEDGER -->"
	markdownEnd   = "<!-- END CHECKED TEST RESOURCE LEDGER -->"
)

// CheckedMarkdownBlock returns the single generated inventory block.
func CheckedMarkdownBlock(document string) (string, error) {
	if strings.Count(document, markdownBegin) != 1 || strings.Count(document, markdownEnd) != 1 {
		return "", errors.New("TESTING.md must contain exactly one checked test resource ledger marker pair")
	}
	start := strings.Index(document, markdownBegin)
	end := strings.Index(document, markdownEnd)
	if end < start {
		return "", errors.New("TESTING.md resource ledger end marker precedes begin marker")
	}
	end += len(markdownEnd)
	return document[start:end], nil
}
