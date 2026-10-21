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

	"github.com/gastownhall/gascity/internal/testpolicy/waiverclock"
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
	// ResourceListenerHelper counts calls to the explicit catalog of helpers
	// whose implementation owns a network listener.
	ResourceListenerHelper Resource = "listener_helper"
	// ResourceNetListen counts direct stream listeners opened by package-level
	// net constructors.
	ResourceNetListen Resource = "net_listen"
	// ResourceNetListenPacket counts direct packet listeners opened by
	// package-level net constructors.
	ResourceNetListenPacket Resource = "net_listen_packet"
	// ResourceNetListenConfig counts direct listeners opened through
	// net.ListenConfig methods.
	ResourceNetListenConfig Resource = "net_listen_config"
	// ResourceSyscallListen counts direct calls that put sockets into listening state through syscall.Listen.
	ResourceSyscallListen Resource = "syscall_listen"
	// ResourceTmux counts typed tmux test helpers, production constructors, and literal tmux process calls.
	ResourceTmux Resource = "tmux"
)

var knownResources = map[Resource]struct{}{
	ResourceSubprocess:      {},
	ResourceFixedSleep:      {},
	ResourceEnvironment:     {},
	ResourceCWD:             {},
	ResourceSlowProcessGate: {},
	ResourceHTTPTestServer:  {},
	ResourceListenerHelper:  {},
	ResourceNetListen:       {},
	ResourceNetListenConfig: {},
	ResourceNetListenPacket: {},
	ResourceSyscallListen:   {},
	ResourceTmux:            {},
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
	ReportedCalls   int      `toml:"reported_calls"`
	ReportedFiles   int      `toml:"reported_files"`
	OwnerBead       string   `toml:"owner_bead"`
	Invariant       string   `toml:"invariant"`
	ResourceOwner   string   `toml:"resource_owner"`
	MigrationTarget string   `toml:"migration_target"`
	Expires         string   `toml:"expires"`
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

// TrackedGoFiles lists every git-tracked *.go file under root, repository-
// relative with forward slashes. Listing tracked files rather than walking the
// filesystem means an untracked nested git worktree checked out under root —
// the common gitignored worktrees/<bead> pool-slot pattern — contributes
// nothing: its files live in that worktree's own index, never this one's.
func TrackedGoFiles(root string) ([]string, error) {
	cmd := exec.Command("git", "-C", root, "ls-files", "-z", "--", "*.go")
	out, err := cmd.Output()
	if err != nil {
		// Bazel runfiles trees carry no .git; walk the declared source tree
		// so census consumers stay effective under `bazel test`.
		return walkedTrackedGoFiles(root), nil
	}
	parts := strings.Split(string(out), "\x00")
	files := make([]string, 0, len(parts))
	for _, name := range parts {
		if name != "" {
			files = append(files, filepath.ToSlash(name))
		}
	}
	return files, nil
}

// ScanRepository scans the repository's tracked Go test files. Tracked sibling
// Go source supplies package-level declaration context but is never counted.
func ScanRepository(root string) (Census, error) {
	files, err := TrackedGoFiles(root)
	if err != nil {
		return Census{}, err
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
	packageFunctions           map[string]struct{}
	unresolvedImportQualifiers map[string]struct{}
}

type packageKey struct {
	directory   string
	packageName string
}

type listenerHelperPackageIdentity struct {
	importPath string
	key        packageKey
	names      []string
}

var listenerHelperPackageIdentities = []listenerHelperPackageIdentity{
	{
		key: packageKey{directory: "cmd/gc", packageName: "main"},
		names: []string{
			"managedDoltPortAvailableForHost",
			"registryBrowserLogin",
			"runController",
			"runSupervisor",
			"startControllerSocket",
			"startNudgeWakeListener",
		},
	},
	{
		importPath: "github.com/gastownhall/gascity/internal/runtime/runtimecapability",
		key:        packageKey{directory: "internal/runtime/runtimecapability", packageName: "runtimecapability"},
		names:      []string{"Run"},
	},
	{
		importPath: "github.com/gastownhall/gascity/test/acceptance/helpers",
		key:        packageKey{directory: "test/acceptance/helpers", packageName: "acceptancehelpers"},
		names:      []string{"WriteSupervisorConfig"},
	},
	{
		key:   packageKey{directory: "test/dashport", packageName: "dashport_test"},
		names: []string{"newHarness"},
	},
}

var targetedDotImportPaths = map[string]struct{}{
	"github.com/gastownhall/gascity/internal/runtime/runtimecapability": {},
	"github.com/gastownhall/gascity/internal/runtime/tmux":              {},
	"github.com/gastownhall/gascity/test/acceptance/helpers":            {},
	"github.com/gastownhall/gascity/test/tmuxtest":                      {},
	"net":               {},
	"net/http/httptest": {},
	"os":                {},
	"os/exec":           {},
	"syscall":           {},
	"testing":           {},
	"time":              {},
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
	packageName := path.Base(importPath)
	for _, identity := range listenerHelperPackageIdentities {
		if importPath == identity.importPath {
			packageName = identity.key.packageName
			break
		}
	}
	imported := types.NewPackage(importPath, packageName)
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
	packageFunctions := make(map[packageKey]map[string]struct{})
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
		listenerHelperNames := listenerHelperPackageNames(key)
		if len(listenerHelperNames) > 0 {
			functions := packageFunctions[key]
			if functions == nil {
				functions = make(map[string]struct{})
				packageFunctions[key] = functions
			}
			recordPackageFunctionDeclarations(file, functions, listenerHelperNames)
		}
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
		candidates := resourceCandidateCalls(file, key)
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
		bindings.packageFunctions = packageFunctions[source.groupKey()]
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
			packageFunctions:    packageFunctions,
		},
	}
	for _, source := range sources {
		if _, err := testingParameterObjects(source.file, source.bindings); err != nil {
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
			resources, err := matchedResourcesForCall(candidate.call, source.groupKey(), source.bindings, slowHelpers[source.groupKey()])
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
			if _, targeted := targetedDotImportPaths[importPath]; targeted {
				return fmt.Errorf("targeted dot import %q cannot be counted safely", importPath)
			}
		}
	}
	return nil
}

func resourceCandidateCalls(file *ast.File, key packageKey) []resourceCall {
	aliases := testingImportAliases(file)
	listenerHelperSelectors := listenerHelperSelectorCandidates(file)
	samePackageHelperNames := listenerHelperPackageNames(key)
	var calls []resourceCall
	for _, declaration := range file.Decls {
		function, ok := declaration.(*ast.FuncDecl)
		if ok {
			calls = appendResourceCandidateCalls(calls, function.Body, function.Name.Name, isRunnableOwner(function, aliases), listenerHelperSelectors, samePackageHelperNames)
			continue
		}
		calls = appendResourceCandidateCalls(calls, declaration, "", false, listenerHelperSelectors, samePackageHelperNames)
	}
	return calls
}

func appendResourceCandidateCalls(calls []resourceCall, node ast.Node, owner string, runnable bool, listenerHelperSelectors map[string]struct{}, listenerHelperPackageNames []string) []resourceCall {
	ast.Inspect(node, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		switch function := unparen(call.Fun).(type) {
		case *ast.SelectorExpr:
			switch function.Sel.Name {
			case "Command", "CommandContext", "ConfigureProcessEnv", "KillAllTestSessions", "LookPath", "NewGuard", "NewGuardWithSocket", "NewProvider", "NewProviderWithConfig", "NewSeamBackedWithConfig", "NewServer", "NewTLSServer", "NewTmux", "NewTmuxWithConfig", "NewUnstartedServer", "RequireTmux", "Sleep", "Setenv", "Unsetenv", "Clearenv", "Chdir", "Listen", "ListenIP", "ListenMulticastUDP", "ListenPacket", "ListenTCP", "ListenUDP", "ListenUnix", "ListenUnixgram":
				calls = append(calls, resourceCall{call: call, owner: owner, runnable: runnable})
			}
			if _, candidate := listenerHelperSelectors[function.Sel.Name]; candidate {
				calls = append(calls, resourceCall{call: call, owner: owner, runnable: runnable})
			}
		case *ast.Ident:
			if function.Name == "skipSlowCmdGCTest" || containsString(listenerHelperPackageNames, function.Name) {
				calls = append(calls, resourceCall{call: call, owner: owner, runnable: runnable})
			}
		}
		return true
	})
	return calls
}

func listenerHelperSelectorCandidates(file *ast.File) map[string]struct{} {
	candidates := make(map[string]struct{})
	for _, spec := range file.Imports {
		if spec.Name != nil && spec.Name.Name == "_" {
			continue
		}
		importPath, err := strconv.Unquote(spec.Path.Value)
		if err != nil {
			continue
		}
		for _, identity := range listenerHelperPackageIdentities {
			if importPath == identity.importPath {
				for _, name := range identity.names {
					candidates[name] = struct{}{}
				}
			}
		}
	}
	return candidates
}

func listenerHelperPackageNames(key packageKey) []string {
	for _, identity := range listenerHelperPackageIdentities {
		if key == identity.key {
			return identity.names
		}
	}
	return nil
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
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
		if ok && (selector.Sel.Name == "Listen" || selector.Sel.Name == "ListenPacket") {
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

func recordPackageFunctionDeclarations(file *ast.File, functions map[string]struct{}, catalogNames []string) {
	for _, declaration := range file.Decls {
		function, ok := declaration.(*ast.FuncDecl)
		if ok && function.Recv == nil && containsString(catalogNames, function.Name.Name) {
			functions[function.Name.Name] = struct{}{}
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

// testingParameterObjects is retained for its fail-closed error: both call
// sites discard the returned set and keep the call only so an unresolvable
// `*testing.T`/`testing.TB` parameter aborts the scan. Do not delete it as an
// unused value.
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
	if !ok || (selector.Sel.Name != "Listen" && selector.Sel.Name != "ListenPacket") {
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

// checkTestingReceiverBinding fails closed when a Setenv/Chdir call's receiver
// identifier cannot be resolved lexically, so an ambiguous call is never
// silently miscounted (or silently ignored) by matchedResourcesForCall.
func checkTestingReceiverBinding(call *ast.CallExpr, bindings bindingInfo, method string) error {
	selector, ok := unparen(call.Fun).(*ast.SelectorExpr)
	if !ok || selector.Sel.Name != method {
		return nil
	}
	identifier, ok := unparen(selector.X).(*ast.Ident)
	if !ok {
		return nil
	}
	if object := bindings.uses[identifier]; object != nil {
		return nil
	}
	if _, declared := bindings.packageDeclarations[identifier.Name]; declared {
		return nil
	}
	if _, imported := bindings.unresolvedImportQualifiers[identifier.Name]; imported {
		return nil
	}
	return fmt.Errorf("testing resource receiver %q has no lexical binding", identifier.Name)
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

func isListenerHelperPackageCall(call *ast.CallExpr, key packageKey, bindings bindingInfo) bool {
	identifier, ok := unparen(call.Fun).(*ast.Ident)
	if !ok {
		return false
	}
	for _, identity := range listenerHelperPackageIdentities {
		if key != identity.key {
			continue
		}
		for _, helperName := range identity.names {
			if identifier.Name != helperName {
				continue
			}
			if _, declared := bindings.packageFunctions[helperName]; !declared {
				return false
			}
			object := bindings.uses[identifier]
			if object == nil {
				return true
			}
			function, ok := object.(*types.Func)
			return ok && function.Pkg() != nil && function.Pkg().Name() == key.packageName && function.Parent() == function.Pkg().Scope()
		}
	}
	return false
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
		if removed := removedLedgerFields(fields); len(removed) > 0 {
			return Ledger{}, countsFailure(fmt.Sprintf("ledger field %s was removed in policy version 3: per-row counts now live in %s", strings.Join(removed, ", "), CountsPath))
		}
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

// Validate checks schema ownership and exact census baselines. It never reads
// or takes a clock: whether a row's date is acceptable today is answered by
// handing PolicyExpiries to internal/testpolicy/waiverclock from the one
// never-cached date check (internal/testpolicy/waiverexpiry). Keeping the two
// apart is what lets this check's result be cached without going stale on the
// calendar.
func Validate(ledger Ledger, census Census) error {
	return validateAgainstPolicy(bootstrapPolicy, ledger, census)
}

// PolicyExpiries returns one dated expiry per bootstrap policy row, for the
// waiver clock to judge against today. Validate forces every row of the checked
// ledger to equal its policy row, expires included, so these are the checked
// ledger's dates.
func PolicyExpiries() []waiverclock.Expiry {
	return collectExpiries(bootstrapPolicy)
}

// validateAgainstPolicy checks the ledger's structure: its rows equal the
// bootstrap policy, the Medium and reviewed hermetic rows exist in the scan,
// and the rows are sorted. The counts live in the counts file and are judged by
// ValidateCounts.
func validateAgainstPolicy(policy, ledger Ledger, census Census) error {
	fail := func(problems ...string) error {
		if len(problems) == 0 {
			return nil
		}
		sort.Strings(problems)
		return countsFailure(problems...)
	}

	if problems := validateManifestAgainstPolicy(policy, ledger); len(problems) > 0 {
		return fail(problems...)
	}
	if err := validateMediumOwners(ledger.Medium, census); err != nil {
		return fail(err.Error())
	}
	if err := validateReviewedHermeticBodies(ledger.ReviewedHermeticBody, census); err != nil {
		return fail(err.Error())
	}
	return nil
}

func validateManifestAgainstPolicy(policy, ledger Ledger) []string {
	var problems []string
	if policy.Version != 3 {
		problems = append(problems, fmt.Sprintf("bootstrap policy version = %d, want 3", policy.Version))
	}
	if ledger.Version != policy.Version {
		problems = append(problems, fmt.Sprintf("ledger version = %d, bootstrap policy requires %d", ledger.Version, policy.Version))
	}
	problems = append(problems, validateRowsAgainstPolicy("audit", policy.AuditBaseline, ledger.AuditBaseline)...)
	problems = append(problems, validateRowsAgainstPolicy("debt", policy.Debt, ledger.Debt)...)
	problems = append(problems, validateMediumRowsAgainstPolicy(policy.Medium, ledger.Medium)...)
	problems = append(problems, validateMediumRowOrder(ledger.Medium)...)
	problems = append(problems, validateReviewedHermeticRowOrder(ledger.ReviewedHermeticBody)...)
	problems = append(problems, validateReviewedHermeticRowsAgainstPolicy(policy.ReviewedHermeticBody, ledger.ReviewedHermeticBody)...)
	problems = append(problems, validateRowsAgainstPolicy("small debt", policy.SmallDebt, ledger.SmallDebt)...)
	return problems
}

func validateRowsAgainstPolicy(kind string, policyRows, ledgerRows []Baseline) []string {
	var problems []string
	policyByKey := map[baselineKey]Baseline{}
	for _, row := range policyRows {
		key := baselineKey{row.Scope, row.Resource}
		prefix := fmt.Sprintf("bootstrap %s baseline scope=%s resource=%s", kind, row.Scope, row.Resource)
		if _, exists := policyByKey[key]; exists {
			problems = append(problems, fmt.Sprintf("duplicate bootstrap %s baseline: scope=%s resource=%s", kind, row.Scope, row.Resource))
		}
		policyByKey[key] = row
		problems = append(problems, validateBaselineDefinition(prefix, row)...)
	}

	seen := map[baselineKey]bool{}
	for _, row := range ledgerRows {
		key := baselineKey{row.Scope, row.Resource}
		prefix := fmt.Sprintf("%s baseline scope=%s resource=%s", kind, row.Scope, row.Resource)
		if seen[key] {
			problems = append(problems, fmt.Sprintf("duplicate %s baseline: scope=%s resource=%s", kind, row.Scope, row.Resource))
		}
		seen[key] = true
		problems = append(problems, validateBaselineDefinition(prefix, row)...)
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

func validateBaselineDefinition(prefix string, row Baseline) []string {
	var problems []string
	if !knownScope(row.Scope) {
		problems = append(problems, fmt.Sprintf("%s: unknown scope %q", prefix, row.Scope))
	}
	if _, ok := knownResources[row.Resource]; !ok {
		problems = append(problems, fmt.Sprintf("%s: unknown resource %q", prefix, row.Resource))
	}
	if row.ReportedCalls < 0 || row.ReportedFiles < 0 {
		problems = append(problems, prefix+": historical census must be non-negative")
	}
	problems = append(problems, validateOwnership(prefix, row)...)
	return problems
}

func knownScope(scope Scope) bool {
	return scope == ScopeAll || scope == ScopeUntagged || scope == ScopeCmdGCUntagged
}

func validateOwnership(prefix string, row Baseline) []string {
	return validateOwnershipFields(prefix, row.OwnerBead, row.Invariant, row.ResourceOwner, row.MigrationTarget, row.Expires)
}

// validateOwnershipFields checks that a row declares who owns it and when it is
// meant to be gone. It checks that the date is well formed but deliberately does
// not check whether it has passed: that verdict depends on today, which this
// cached check must never read. collectExpiries hands each row to the waiver
// clock once instead. See internal/testpolicy/waiverexpiry.
func validateOwnershipFields(prefix, owner, invariant, resourceOwner, migration, expiryText string) []string {
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
	if _, err := time.Parse("2006-01-02", expiryText); err != nil {
		problems = append(problems, fmt.Sprintf("%s: expiry %q must use YYYY-MM-DD", prefix, expiryText))
	}
	return problems
}

// collectExpiries gathers every dated row in the ledger exactly once, so a
// passing date produces one finding per row rather than one per place the row is
// reached. A malformed date is skipped: validateOwnershipFields already reports
// it, and faulting it twice turns one authoring mistake into two findings.
func collectExpiries(ledger Ledger) []waiverclock.Expiry {
	var expiries []waiverclock.Expiry
	add := func(prefix, owner, expiryText string) {
		expires, err := time.Parse("2006-01-02", expiryText)
		if err != nil || strings.TrimSpace(owner) == "" {
			return
		}
		expiries = append(expiries, waiverclock.Expiry{Label: prefix, Owner: owner, Expires: expires})
	}
	addBaselines := func(kind string, rows []Baseline) {
		for _, row := range rows {
			add(fmt.Sprintf("%s baseline scope=%s resource=%s", kind, row.Scope, row.Resource), row.OwnerBead, row.Expires)
		}
	}
	addBaselines("audit", ledger.AuditBaseline)
	addBaselines("debt", ledger.Debt)
	addBaselines("small debt", ledger.SmallDebt)
	for _, row := range ledger.Medium {
		add(fmt.Sprintf("medium owner package_dir=%s package_name=%s owner=%s", row.PackageDir, row.PackageName, row.Owner), row.OwnerBead, row.Expires)
	}
	return expiries
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
	return string(row.Resource)
}

const (
	markdownBegin = "<!-- BEGIN CHECKED TEST RESOURCE LEDGER -->"
	markdownEnd   = "<!-- END CHECKED TEST RESOURCE LEDGER -->"
)

// CheckedMarkdownBlock returns the single generated inventory block.
func CheckedMarkdownBlock(document string) (string, error) {
	start, end, err := markdownBlockSpan(document)
	if err != nil {
		return "", err
	}
	return document[start:end], nil
}

// ReplaceMarkdownBlock returns document with its single checked test resource
// ledger block replaced by replacement. Content outside the marker pair is
// preserved byte-for-byte. Pass RenderMarkdown's output as replacement to
// regenerate the block from a Ledger.
func ReplaceMarkdownBlock(document, replacement string) (string, error) {
	start, end, err := markdownBlockSpan(document)
	if err != nil {
		return "", err
	}
	return document[:start] + replacement + document[end:], nil
}

func markdownBlockSpan(document string) (start, end int, err error) {
	if strings.Count(document, markdownBegin) != 1 || strings.Count(document, markdownEnd) != 1 {
		return 0, 0, errors.New("TESTING.md must contain exactly one checked test resource ledger marker pair")
	}
	start = strings.Index(document, markdownBegin)
	end = strings.Index(document, markdownEnd)
	if end < start {
		return 0, 0, errors.New("TESTING.md resource ledger end marker precedes begin marker")
	}
	end += len(markdownEnd)
	return start, end, nil
}

// walkedTrackedGoFiles enumerates non-test .go files across the module when
// the git index is unavailable (bazel runfiles trees). Best effort: hidden
// directories and walk errors are skipped rather than fatal.
func walkedTrackedGoFiles(root string) []string {
	var files []string
	tops := []string{"internal", "cmd", "pkg", "examples", "test", "scripts"}
	for _, top := range tops {
		_ = filepath.WalkDir(filepath.Join(root, top), func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if d.IsDir() {
				if name := d.Name(); name != "." && strings.HasPrefix(name, ".") {
					return filepath.SkipDir
				}
				return nil
			}
			rel, rerr := filepath.Rel(root, path)
			if rerr != nil {
				return nil
			}
			if strings.HasSuffix(path, ".go") {
				files = append(files, filepath.ToSlash(rel))
			}
			return nil
		})
	}
	return files
}
