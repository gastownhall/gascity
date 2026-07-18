package resourcecensus

import (
	"errors"
	"fmt"
	"go/ast"
	"go/token"
	"go/types"
	"sort"
	"strings"
)

// ReviewedHermeticBody records a manually reviewed test body whose effective
// runnable size remains explicit.
type ReviewedHermeticBody struct {
	PackageDir    string `toml:"package_dir"`
	PackageName   string `toml:"package_name"`
	Owner         string `toml:"owner"`
	EffectiveSize string `toml:"effective_size"`
	MediumReason  string `toml:"medium_reason"`
}

// hermeticSourceIndex retains parsed Go files for packages eligible for a
// reviewed body. The ordinary census counts resources only in test source, but
// a reviewed body must also inspect same-package production helpers.
type hermeticSourceIndex struct {
	fileSet             *token.FileSet
	files               []parsedFile
	packageDeclarations map[packageKey]map[string]struct{}
}

type hermeticFile struct {
	source         parsedFile
	index          int
	bindings       bindingInfo
	testingObjects map[types.Object]bool
	resolved       bool
	resolveErr     error
}

type hermeticFunction struct {
	file        *hermeticFile
	declaration *ast.FuncDecl
}

type hermeticAnalyzer struct {
	fileSet             *token.FileSet
	importer            *emptyPackageImporter
	packageDeclarations map[packageKey]map[string]struct{}
	functions           map[packageKey]map[string][]*hermeticFunction
	slowHelpers         map[packageKey]types.Object
}

type hermeticResourceUse struct {
	resource Resource
	position token.Pos
}

type hermeticFunctionAnalysis struct {
	resources []hermeticResourceUse
	callees   []*hermeticFunction
}

type hermeticQueueEntry struct {
	function *hermeticFunction
	chain    []string
}

type retainedRealOwner struct {
	reviewed runnableKey
	retained runnableKey
}

var retainedRealOwners = []retainedRealOwner{
	{
		reviewed: runnableKey{
			packageDir:  "cmd/gc",
			packageName: "main",
			owner:       "TestDoSessionWait_RegistersReadyWaitForRigDependency",
		},
		retained: runnableKey{
			packageDir:  "cmd/gc",
			packageName: "main",
			owner:       "TestCmdSessionWait_AllowsRigDependencyBeads",
		},
	},
	{
		reviewed: runnableKey{
			packageDir:  "cmd/gc",
			packageName: "main",
			owner:       "TestDoSessionWake_PokesManagedControllerAfterStateChange",
		},
		retained: runnableKey{
			packageDir:  "cmd/gc",
			packageName: "main",
			owner:       "TestCmdSessionWake_PokesManagedControllerAndRequestsSuspendedStart",
		},
	},
	{
		reviewed: runnableKey{
			packageDir:  "cmd/gc",
			packageName: "main",
			owner:       "TestPrepareWaitWakeState_ResolvesRigDependencyBeads",
		},
		retained: runnableKey{
			packageDir:  "cmd/gc",
			packageName: "main",
			owner:       "TestCmdSessionWait_AllowsRigDependencyBeads",
		},
	},
	{
		reviewed: runnableKey{
			packageDir:  "cmd/gc",
			packageName: "main",
			owner:       "TestDoMailInbox_RendersMessagesFromReader",
		},
		retained: runnableKey{
			packageDir:  "cmd/gc",
			packageName: "main",
			owner:       "TestCmdMailInbox_ManagedExecLifecycleProviderReadsInbox",
		},
	},
}

func retainedRealOwnerFor(key runnableKey) (runnableKey, bool) {
	for _, pair := range retainedRealOwners {
		if pair.reviewed == key {
			return pair.retained, true
		}
	}
	return runnableKey{}, false
}

func validateReviewedHermeticBodies(rows []ReviewedHermeticBody, census Census) error {
	problems := validateReviewedHermeticBodyDefinitions(rows)
	if len(rows) == 0 {
		return problemsError(problems)
	}

	analyzer, err := newHermeticAnalyzer(census.hermeticSource, rows)
	if err != nil {
		problems = append(problems, fmt.Sprintf("building reviewed hermetic body source index: %v", err))
		return problemsError(problems)
	}
	for _, row := range rows {
		if !validHermeticIdentity(row) {
			continue
		}
		problems = append(problems, analyzer.validate(row)...)
	}
	return problemsError(problems)
}

func validateReviewedHermeticRowsAgainstPolicy(policyRows, ledgerRows []ReviewedHermeticBody) []string {
	problems := validateReviewedHermeticBodyDefinitions(policyRows)
	problems = append(problems, validateReviewedHermeticBodyDefinitions(ledgerRows)...)

	policyByKey := make(map[runnableKey]ReviewedHermeticBody, len(policyRows))
	for _, row := range policyRows {
		policyByKey[reviewedHermeticBodyKey(row)] = row
	}
	seen := make(map[runnableKey]struct{}, len(ledgerRows))
	for _, row := range ledgerRows {
		key := reviewedHermeticBodyKey(row)
		seen[key] = struct{}{}
		want, exists := policyByKey[key]
		if !exists {
			problems = append(problems, fmt.Sprintf("unexpected reviewed hermetic body: package_dir=%s package_name=%s owner=%s", row.PackageDir, row.PackageName, row.Owner))
			continue
		}
		prefix := reviewedHermeticBodyPrefix(row)
		if row.EffectiveSize != want.EffectiveSize {
			problems = append(problems, fmt.Sprintf("%s: effective_size = %q, bootstrap policy requires %q", prefix, row.EffectiveSize, want.EffectiveSize))
		}
		if row.MediumReason != want.MediumReason {
			problems = append(problems, fmt.Sprintf("%s: medium_reason = %q, bootstrap policy requires %q", prefix, row.MediumReason, want.MediumReason))
		}
	}
	for key := range policyByKey {
		if _, exists := seen[key]; exists {
			continue
		}
		problems = append(problems, fmt.Sprintf("missing required reviewed hermetic body: package_dir=%s package_name=%s owner=%s", key.packageDir, key.packageName, key.owner))
	}
	sort.Strings(problems)
	return problems
}

func validateReviewedHermeticBodyDefinitions(rows []ReviewedHermeticBody) []string {
	seen := make(map[runnableKey]struct{}, len(rows))
	var problems []string
	for _, row := range rows {
		key := reviewedHermeticBodyKey(row)
		prefix := reviewedHermeticBodyPrefix(row)
		if _, duplicate := seen[key]; duplicate {
			problems = append(problems, fmt.Sprintf("duplicate reviewed hermetic body: package_dir=%s package_name=%s owner=%s", row.PackageDir, row.PackageName, row.Owner))
		}
		seen[key] = struct{}{}
		if strings.TrimSpace(row.PackageDir) == "" {
			problems = append(problems, prefix+": package_dir is required")
		}
		if strings.TrimSpace(row.PackageName) == "" {
			problems = append(problems, prefix+": package_name is required")
		}
		if strings.TrimSpace(row.Owner) == "" {
			problems = append(problems, prefix+": owner is required")
		}
		if strings.ContainsAny(row.PackageDir, "*?[") || strings.ContainsAny(row.PackageName, "*?[") || strings.ContainsAny(row.Owner, "*?[") {
			problems = append(problems, prefix+": wildcard identities are not allowed")
		}
		if row.EffectiveSize != "medium" {
			problems = append(problems, fmt.Sprintf("%s: effective_size = %q, want %q", prefix, row.EffectiveSize, "medium"))
		}
		if strings.TrimSpace(row.MediumReason) == "" {
			problems = append(problems, prefix+": medium_reason is required")
		}
	}
	return problems
}

func validHermeticIdentity(row ReviewedHermeticBody) bool {
	return strings.TrimSpace(row.PackageDir) != "" &&
		strings.TrimSpace(row.PackageName) != "" &&
		strings.TrimSpace(row.Owner) != "" &&
		!strings.ContainsAny(row.PackageDir, "*?[") &&
		!strings.ContainsAny(row.PackageName, "*?[") &&
		!strings.ContainsAny(row.Owner, "*?[")
}

func reviewedHermeticBodyKey(row ReviewedHermeticBody) runnableKey {
	return runnableKey{packageDir: row.PackageDir, packageName: row.PackageName, owner: row.Owner}
}

func reviewedHermeticBodyPrefix(row ReviewedHermeticBody) string {
	return fmt.Sprintf("reviewed hermetic body package_dir=%s package_name=%s owner=%s", row.PackageDir, row.PackageName, row.Owner)
}

func problemsError(problems []string) error {
	if len(problems) == 0 {
		return nil
	}
	sort.Strings(problems)
	return errors.New(strings.Join(problems, "\n"))
}

func newHermeticAnalyzer(sourceIndex *hermeticSourceIndex, rows []ReviewedHermeticBody) (*hermeticAnalyzer, error) {
	if sourceIndex == nil || sourceIndex.fileSet == nil {
		return nil, errors.New("source file set is unavailable")
	}
	selectedPackages := make(map[packageKey]struct{}, len(rows))
	for _, row := range rows {
		selectedPackages[packageKey{directory: row.PackageDir, packageName: row.PackageName}] = struct{}{}
	}
	files := make([]parsedFile, 0)
	for _, source := range sourceIndex.files {
		if _, selected := selectedPackages[source.groupKey()]; selected {
			files = append(files, source)
		}
	}
	sort.SliceStable(files, func(i, j int) bool {
		if files[i].name != files[j].name {
			return files[i].name < files[j].name
		}
		if files[i].packageName != files[j].packageName {
			return files[i].packageName < files[j].packageName
		}
		return files[i].file.Pos() < files[j].file.Pos()
	})

	declarations := sourceIndex.packageDeclarations
	if declarations == nil {
		declarations = make(map[packageKey]map[string]struct{})
		for _, source := range files {
			key := source.groupKey()
			if declarations[key] == nil {
				declarations[key] = make(map[string]struct{})
			}
			recordPackageDeclarations(source.file, declarations[key])
		}
	}

	analyzer := &hermeticAnalyzer{
		fileSet:             sourceIndex.fileSet,
		importer:            newEmptyPackageImporter(),
		packageDeclarations: declarations,
		functions:           make(map[packageKey]map[string][]*hermeticFunction),
		slowHelpers:         make(map[packageKey]types.Object),
	}
	indexedFiles := make([]hermeticFile, len(files))
	for index, source := range files {
		indexedFiles[index] = hermeticFile{source: source, index: index}
	}
	for index := range indexedFiles {
		file := &indexedFiles[index]
		key := file.source.groupKey()
		if analyzer.functions[key] == nil {
			analyzer.functions[key] = make(map[string][]*hermeticFunction)
		}
		for _, declaration := range file.source.file.Decls {
			function, ok := declaration.(*ast.FuncDecl)
			if !ok || function.Recv != nil {
				continue
			}
			analyzer.functions[key][function.Name.Name] = append(analyzer.functions[key][function.Name.Name], &hermeticFunction{
				file:        file,
				declaration: function,
			})
		}
	}
	for key, functions := range analyzer.functions {
		for _, function := range functions["skipSlowCmdGCTest"] {
			if err := analyzer.resolveFile(function.file); err != nil {
				return nil, err
			}
			matched, err := isSlowHelperDeclaration(function.declaration, function.file.bindings)
			if err != nil {
				return nil, fmt.Errorf("scanning slow-process helper in %s: %w", function.file.source.name, err)
			}
			if !matched {
				continue
			}
			if _, exists := analyzer.slowHelpers[key]; exists {
				return nil, fmt.Errorf("scanning slow-process helper in %s: package %s has multiple canonical declarations", function.file.source.name, function.file.source.packageName)
			}
			object := function.file.bindings.defs[function.declaration.Name]
			if object == nil {
				return nil, fmt.Errorf("scanning slow-process helper in %s: declaration has no lexical binding", function.file.source.name)
			}
			analyzer.slowHelpers[key] = object
		}
	}
	return analyzer, nil
}

func (a *hermeticAnalyzer) resolveFile(file *hermeticFile) error {
	if file.resolved {
		return file.resolveErr
	}
	file.resolved = true
	if err := validateImports(file.source.file); err != nil {
		file.resolveErr = fmt.Errorf("scanning imports in %s: %w", file.source.name, err)
		return file.resolveErr
	}
	bindings := resolveBindings(a.fileSet, file.source.file, a.importer, fmt.Sprintf("resourcecensus.hermetic/file%d", file.index))
	bindings.packageDeclarations = a.packageDeclarations[file.source.groupKey()]
	bindings.unresolvedImportQualifiers = unresolvedDefaultImportQualifiers(file.source.file)
	testingObjects, err := testingParameterObjects(file.source.file, bindings)
	if err != nil {
		file.resolveErr = fmt.Errorf("scanning testing parameters in %s: %w", file.source.name, err)
		return file.resolveErr
	}
	file.bindings = bindings
	file.testingObjects = testingObjects
	return nil
}

func (a *hermeticAnalyzer) validate(row ReviewedHermeticBody) []string {
	rowKey := reviewedHermeticBodyKey(row)
	root, rootProblem := a.exactUntaggedTest(rowKey)
	prefix := reviewedHermeticBodyPrefix(row)
	if rootProblem != "" {
		return []string{prefix + ": " + rootProblem}
	}

	var problems []string
	if retained, exists := retainedRealOwnerFor(rowKey); exists {
		if _, problem := a.exactUntaggedTest(retained); problem != "" {
			problems = append(problems, fmt.Sprintf(
				"%s: retained real composition owner package_dir=%s package_name=%s owner=%s: %s",
				prefix, retained.packageDir, retained.packageName, retained.owner, problem,
			))
		}
	}

	key := packageKey{directory: row.PackageDir, packageName: row.PackageName}
	uses, err := a.reachableResources(key, root)
	if err != nil {
		problems = append(problems, fmt.Sprintf("%s: scanning reachable helpers: %v", prefix, err))
		return problems
	}
	resources := make([]Resource, 0, len(uses))
	for resource := range uses {
		resources = append(resources, resource)
	}
	sort.Slice(resources, func(i, j int) bool { return resources[i] < resources[j] })
	for _, resource := range resources {
		use := uses[resource]
		position := a.fileSet.Position(use.position)
		problems = append(problems, fmt.Sprintf("%s: %s is reachable through %s (%s:%d)", prefix, resource, strings.Join(use.chain, " -> "), position.Filename, position.Line))
	}
	return problems
}

func (a *hermeticAnalyzer) exactUntaggedTest(key runnableKey) (*hermeticFunction, string) {
	packageKey := packageKey{directory: key.packageDir, packageName: key.packageName}
	declarations := a.functions[packageKey][key.owner]
	var roots []*hermeticFunction
	for _, function := range declarations {
		if !strings.HasSuffix(function.file.source.name, "_test.go") || !goTestName(function.declaration.Name.Name, "Test") || function.declaration.Name.Name == "TestMain" {
			continue
		}
		if isRunnableOwner(function.declaration, testingImportAliases(function.file.source.file)) {
			roots = append(roots, function)
		}
	}
	switch {
	case len(roots) == 0:
		return nil, "runnable owner does not exist"
	case len(declarations) != 1 || len(roots) != 1:
		return nil, "runnable owner is not unique"
	case roots[0].file.source.tagged:
		return nil, "runnable owner must be untagged"
	}
	return roots[0], ""
}

type hermeticReachableUse struct {
	chain    []string
	position token.Pos
}

func (a *hermeticAnalyzer) reachableResources(key packageKey, root *hermeticFunction) (map[Resource]hermeticReachableUse, error) {
	queue := []hermeticQueueEntry{{function: root, chain: []string{root.declaration.Name.Name}}}
	visited := make(map[*ast.FuncDecl]struct{})
	uses := make(map[Resource]hermeticReachableUse)
	for len(queue) > 0 {
		entry := queue[0]
		queue = queue[1:]
		if _, seen := visited[entry.function.declaration]; seen {
			continue
		}
		visited[entry.function.declaration] = struct{}{}
		analysis, err := a.analyzeFunction(key, entry.function)
		if err != nil {
			return nil, err
		}
		for _, use := range analysis.resources {
			if _, reported := uses[use.resource]; reported {
				continue
			}
			uses[use.resource] = hermeticReachableUse{chain: append([]string(nil), entry.chain...), position: use.position}
		}
		for _, callee := range analysis.callees {
			if _, seen := visited[callee.declaration]; seen {
				continue
			}
			chain := append(append([]string(nil), entry.chain...), callee.declaration.Name.Name)
			queue = append(queue, hermeticQueueEntry{function: callee, chain: chain})
		}
	}
	return uses, nil
}

func (a *hermeticAnalyzer) analyzeFunction(key packageKey, function *hermeticFunction) (hermeticFunctionAnalysis, error) {
	if function.declaration.Body == nil {
		return hermeticFunctionAnalysis{}, nil
	}
	if err := a.resolveFile(function.file); err != nil {
		return hermeticFunctionAnalysis{}, err
	}
	resources := make(map[Resource]token.Pos)
	callees := make(map[*ast.FuncDecl]*hermeticFunction)
	var inspectErr error
	var parents []ast.Node
	ast.Inspect(function.declaration.Body, func(node ast.Node) bool {
		if node == nil {
			parents = parents[:len(parents)-1]
			return true
		}
		var parent ast.Node
		if len(parents) > 0 {
			parent = parents[len(parents)-1]
		}
		parents = append(parents, node)
		if inspectErr != nil {
			return false
		}
		if call, ok := node.(*ast.CallExpr); ok {
			matched, err := matchedResourcesForCall(call, function.file.bindings, function.file.testingObjects, a.slowHelpers[key])
			if err != nil {
				inspectErr = fmt.Errorf("%s: %w", function.file.source.name, err)
				return false
			}
			for _, resource := range matched {
				if _, exists := resources[resource]; !exists {
					resources[resource] = call.Pos()
				}
			}
		}
		identifier, ok := node.(*ast.Ident)
		if !ok || !isHermeticFunctionReference(identifier, parent, function.file.bindings, a.functions[key]) {
			return true
		}
		for _, callee := range a.functions[key][identifier.Name] {
			callees[callee.declaration] = callee
		}
		return true
	})
	if inspectErr != nil {
		return hermeticFunctionAnalysis{}, inspectErr
	}

	result := hermeticFunctionAnalysis{
		resources: make([]hermeticResourceUse, 0, len(resources)),
		callees:   make([]*hermeticFunction, 0, len(callees)),
	}
	for resource, position := range resources {
		result.resources = append(result.resources, hermeticResourceUse{resource: resource, position: position})
	}
	sort.Slice(result.resources, func(i, j int) bool {
		if result.resources[i].position != result.resources[j].position {
			return result.resources[i].position < result.resources[j].position
		}
		return result.resources[i].resource < result.resources[j].resource
	})
	for _, callee := range callees {
		result.callees = append(result.callees, callee)
	}
	sort.Slice(result.callees, func(i, j int) bool {
		left, right := result.callees[i], result.callees[j]
		if left.declaration.Name.Name != right.declaration.Name.Name {
			return left.declaration.Name.Name < right.declaration.Name.Name
		}
		if left.file.source.name != right.file.source.name {
			return left.file.source.name < right.file.source.name
		}
		return left.declaration.Pos() < right.declaration.Pos()
	})
	return result, nil
}

func isHermeticFunctionReference(identifier *ast.Ident, parent ast.Node, bindings bindingInfo, functions map[string][]*hermeticFunction) bool {
	targets := functions[identifier.Name]
	if len(targets) == 0 || bindings.defs[identifier] != nil || nonValueIdentifier(identifier, parent) {
		return false
	}
	object := bindings.uses[identifier]
	if object == nil || object.Parent() == types.Universe {
		return true
	}
	for _, target := range targets {
		if target.file.bindings.defs[target.declaration.Name] == object {
			return true
		}
	}
	return false
}

func nonValueIdentifier(identifier *ast.Ident, parent ast.Node) bool {
	switch parent := parent.(type) {
	case *ast.SelectorExpr:
		return parent.Sel == identifier
	case *ast.KeyValueExpr:
		return parent.Key == identifier
	case *ast.BranchStmt:
		return parent.Label == identifier
	case *ast.LabeledStmt:
		return parent.Label == identifier
	case *ast.ImportSpec:
		return parent.Name == identifier
	case *ast.File:
		return parent.Name == identifier
	case *ast.FuncDecl:
		return parent.Name == identifier
	case *ast.TypeSpec:
		return parent.Name == identifier
	case *ast.ValueSpec:
		for _, name := range parent.Names {
			if name == identifier {
				return true
			}
		}
	case *ast.Field:
		for _, name := range parent.Names {
			if name == identifier {
				return true
			}
		}
	}
	return false
}

// matchedResourcesForCall is the single mapping from a syntax-owned call to
// the resource identities recognized by both the census and hermetic review.
func matchedResourcesForCall(call *ast.CallExpr, bindings bindingInfo, testingObjects map[types.Object]bool, slowHelperObject types.Object) ([]Resource, error) {
	var resources []Resource
	appendImported := func(resource Resource, importPath string, names ...string) error {
		matched, err := isImportedCall(call, bindings, importPath, names...)
		if err != nil {
			return err
		}
		if matched {
			resources = append(resources, resource)
		}
		return nil
	}
	if err := appendImported(ResourceNetListen, "net", "Listen"); err != nil {
		return nil, err
	}
	matched, err := isNetListenConfigCall(call, bindings)
	if err != nil {
		return nil, err
	}
	if matched {
		resources = append(resources, ResourceNetListenConfig)
	}
	if err := appendImported(ResourceNetListenUnixgram, "net", "ListenUnixgram"); err != nil {
		return nil, err
	}
	if err := appendImported(ResourceSyscallListen, "syscall", "Listen"); err != nil {
		return nil, err
	}
	if err := appendImported(ResourceHTTPTestServer, "net/http/httptest", "NewServer", "NewTLSServer", "NewUnstartedServer"); err != nil {
		return nil, err
	}
	if err := appendImported(ResourceSubprocess, "os/exec", "Command", "CommandContext"); err != nil {
		return nil, err
	}
	if err := appendImported(ResourceFixedSleep, "time", "Sleep"); err != nil {
		return nil, err
	}
	if err := appendImported(ResourceEnvironment, "os", "Setenv", "Unsetenv", "Clearenv"); err != nil {
		return nil, err
	}
	if err := appendImported(ResourceCWD, "os", "Chdir"); err != nil {
		return nil, err
	}
	matched, err = isTestingCall(call, bindings, testingObjects, "Setenv")
	if err != nil {
		return nil, err
	}
	if matched {
		resources = append(resources, ResourceEnvironment)
	}
	matched, err = isTestingCall(call, bindings, testingObjects, "Chdir")
	if err != nil {
		return nil, err
	}
	if matched {
		resources = append(resources, ResourceCWD)
	}
	if isSlowHelperCall(call, bindings, slowHelperObject) {
		resources = append(resources, ResourceSlowProcessGate)
	}
	return resources, nil
}
