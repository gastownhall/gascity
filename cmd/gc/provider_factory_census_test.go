package main

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

var legacySessionProviderFactoryReferences = map[string]int{}

var legacySessionProviderAliasCalls = map[string]int{}

var legacySessionProviderAliasBindings = map[string]int{}

var legacySessionProviderExitHelperUses = map[string]int{}

var legacySessionProviderFactories = map[string]bool{
	"newSessionProviderWithError":                          true,
	"newSessionProviderForCityWithError":                   true,
	"newSessionProviderFromContextWithError":               true,
	"newStatusSessionProviderForCityWithError":             true,
	"newStatusSessionProviderForCityWithSnapshotWithError": true,
}

var canonicalProviderResultSources = map[string]bool{
	"newSessionProvider":                          true,
	"newSessionProviderForCity":                   true,
	"newSessionProviderFromContext":               true,
	"newStatusSessionProviderForCity":             true,
	"newStatusSessionProviderForCityWithSnapshot": true,
	"withSessionProviderConstructionContext":      true,
}

// These three mutable seams are intentional test seams. Any additional alias
// expands the construction surface and must fail review rather than silently
// inheriting permission to forward a provider-construction error.
var canonicalProviderAliasBindings = map[string]int{
	"cmd_convoy_dispatch.go:<package>:dispatchControlSessionProvider=newSessionProvider": 1,
	"cmd_rig.go:<package>:rigListSessionProvider=newSessionProvider":                     1,
	"cmd_stop.go:<package>:sessionProviderForStopCity=newSessionProviderForCity":         1,
}

// Every production construction call is pinned together with its result
// disposition. bind-error means the second result is a named local; the
// forwarding shapes below are the only reviewed multi-result pass-throughs.
var canonicalProviderCalls = map[string]int{
	"cmd_citystatus.go:cmdCityStatus:newStatusSessionProviderForCityWithSnapshot:bind-error":                                                   1,
	"cmd_convoy_dispatch.go:runControlDispatcherWithStoreAndConfig:dispatchControlSessionProvider:bind-error":                                  2,
	"cmd_doctor.go:buildDoctorChecks:newSessionProvider:bind-error":                                                                            1,
	"cmd_handoff.go:cmdHandoff:newSessionProvider:bind-error":                                                                                  1,
	"cmd_handoff.go:cmdHandoffRemote:newSessionProvider:bind-error":                                                                            1,
	"cmd_nudge.go:cmdNudgePoll:newSessionProvider:bind-error":                                                                                  1,
	"cmd_nudge.go:deliverSessionNudge:newSessionProvider:bind-error":                                                                           1,
	"cmd_nudge.go:sendMailNotify:newSessionProvider:bind-error":                                                                                1,
	"cmd_restart.go:cmdRigRestart:newSessionProvider:bind-error":                                                                               1,
	"cmd_rig.go:doRigList:rigListSessionProvider:bind-error":                                                                                   1,
	"cmd_runtime_drain.go:cmdRuntimeDrain:newSessionProvider:bind-error":                                                                       1,
	"cmd_runtime_drain.go:cmdRuntimeDrainAck:newSessionProvider:bind-error":                                                                    2,
	"cmd_runtime_drain.go:cmdRuntimeDrainCheck:newSessionProvider:bind-error":                                                                  2,
	"cmd_runtime_drain.go:cmdRuntimeRequestRestart:newSessionProvider:bind-error":                                                              1,
	"cmd_runtime_drain.go:cmdRuntimeUndrain:newSessionProvider:bind-error":                                                                     1,
	"cmd_session.go:cmdSessionAttach:newSessionProvider:bind-error":                                                                            1,
	"cmd_session.go:cmdSessionClose:newSessionProvider:bind-error":                                                                             1,
	"cmd_session.go:cmdSessionKill:newSessionProvider:bind-error":                                                                              1,
	"cmd_session.go:cmdSessionNew:newSessionProvider:bind-error":                                                                               1,
	"cmd_session.go:cmdSessionPrune:newSessionProvider:bind-error":                                                                             1,
	"cmd_session.go:cmdSessionRename:newSessionProvider:bind-error":                                                                            1,
	"cmd_session.go:cmdSessionSubmit:newSessionProvider:bind-error":                                                                            1,
	"cmd_session.go:cmdSessionSuspend:newSessionProvider:bind-error":                                                                           1,
	"cmd_session.go:doSessionListFallback:newSessionProviderFromContext:forward-to-withSessionProviderConstructionContext":                     1,
	"cmd_session.go:doSessionListFallback:withSessionProviderConstructionContext:bind-error":                                                   1,
	"cmd_session.go:doSessionPeekFallback:newSessionProvider:bind-error":                                                                       1,
	"cmd_session_reset.go:cmdSessionReset:newSessionProvider:bind-error":                                                                       1,
	"cmd_sling.go:cmdSlingWithJSON:newSessionProvider:bind-error":                                                                              1,
	"cmd_start.go:doStartStandalone:newSessionProvider:bind-error":                                                                             1,
	"cmd_status.go:cmdRigStatus:newStatusSessionProviderForCityWithSnapshot:bind-error":                                                        1,
	"cmd_stop.go:cmdStopBody:sessionProviderForStopCity:bind-error":                                                                            1,
	"cmd_supervisor.go:reconcileCities:newSessionProviderFromContext:bind-error":                                                               1,
	"completion.go:loadSessionsForCompletion:newSessionProviderFromContext:bind-error":                                                         1,
	"providers.go:newSessionProvider:newSessionProviderFromContext:forward-to-withSessionProviderConstructionContext":                          1,
	"providers.go:newSessionProvider:withSessionProviderConstructionContext:forward-return":                                                    1,
	"providers.go:newSessionProviderForCity:newSessionProviderFromContext:forward-to-withSessionProviderConstructionContext":                   1,
	"providers.go:newSessionProviderForCity:withSessionProviderConstructionContext:forward-return":                                             1,
	"providers.go:newStatusSessionProviderForCity:newStatusSessionProviderForCityWithSnapshot:forward-return":                                  1,
	"providers.go:newStatusSessionProviderForCityWithSnapshot:newSessionProviderFromContext:forward-to-withSessionProviderConstructionContext": 1,
	"providers.go:newStatusSessionProviderForCityWithSnapshot:withSessionProviderConstructionContext:bind-error":                               1,
	"session_logs_resolve.go:resolveStoredSessionLogSource:newSessionProvider:bind-error":                                                      1,
	"session_template_start.go:materializeSessionForAgentConfig:newSessionProvider:bind-error":                                                 1,
	"session_template_start.go:materializeSessionForTemplateWithOptions:newSessionProvider:bind-error":                                         1,
}

// Multi-result forwarding is intentionally narrower than ordinary result
// binding: only the canonical context wrapper and the one status constructor
// delegation may relay a construction error without inspecting it locally.
var reviewedCanonicalProviderForwards = map[string]bool{
	"cmd_session.go:doSessionListFallback:newSessionProviderFromContext->withSessionProviderConstructionContext":                     true,
	"providers.go:newSessionProvider:newSessionProviderFromContext->withSessionProviderConstructionContext":                          true,
	"providers.go:newSessionProviderForCity:newSessionProviderFromContext->withSessionProviderConstructionContext":                   true,
	"providers.go:newStatusSessionProviderForCityWithSnapshot:newSessionProviderFromContext->withSessionProviderConstructionContext": true,
	"providers.go:newSessionProvider:withSessionProviderConstructionContext->return":                                                 true,
	"providers.go:newSessionProviderForCity:withSessionProviderConstructionContext->return":                                          true,
	"providers.go:newStatusSessionProviderForCity:newStatusSessionProviderForCityWithSnapshot->return":                               true,
}

type providerFactoryCensus struct {
	references             map[string]int
	aliasBindings          map[string]int
	aliasCalls             map[string]int
	aliases                map[string]bool
	exitHelperUses         map[string]int
	directCalls            int
	canonicalAliasBindings map[string]int
	canonicalCalls         map[string]int
	violations             []string
}

func (c providerFactoryCensus) invocationCount() int {
	invocations := c.directCalls
	for _, count := range c.aliasCalls {
		invocations += count
	}
	return invocations
}

func TestLegacySessionProviderFactoryCallerCensus(t *testing.T) {
	dir, err := providerFactorySourceDir()
	if err != nil {
		t.Fatal(err)
	}
	census, err := scanLegacySessionProviderFactoryCallers(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(census.violations) != 0 {
		t.Fatalf("legacy provider factory census found unclassified uses:\n%s", strings.Join(census.violations, "\n"))
	}
	if !maps.Equal(census.references, legacySessionProviderFactoryReferences) {
		t.Fatalf("legacy provider factory reference census changed\n got:\n%s\nwant:\n%s", formatProviderFactoryCensus(census.references), formatProviderFactoryCensus(legacySessionProviderFactoryReferences))
	}
	if !maps.Equal(census.aliasBindings, legacySessionProviderAliasBindings) {
		t.Fatalf("legacy provider factory alias-binding census changed\n got:\n%s\nwant:\n%s", formatProviderFactoryCensus(census.aliasBindings), formatProviderFactoryCensus(legacySessionProviderAliasBindings))
	}
	if !maps.Equal(census.aliasCalls, legacySessionProviderAliasCalls) {
		t.Fatalf("legacy provider factory alias-call census changed\n got:\n%s\nwant:\n%s", formatProviderFactoryCensus(census.aliasCalls), formatProviderFactoryCensus(legacySessionProviderAliasCalls))
	}
	if !maps.Equal(census.exitHelperUses, legacySessionProviderExitHelperUses) {
		t.Fatalf("retired sessionProviderOrExit census changed\n got:\n%s\nwant:\n%s", formatProviderFactoryCensus(census.exitHelperUses), formatProviderFactoryCensus(legacySessionProviderExitHelperUses))
	}
	if census.directCalls != 0 {
		t.Fatalf("direct legacy provider factory invocation count = %d, want 0", census.directCalls)
	}
	if invocations := census.invocationCount(); invocations != 0 {
		t.Fatalf("legacy provider factory invocation count = %d, want 0", invocations)
	}
}

func TestCanonicalSessionProviderFactoryCallerCensus(t *testing.T) {
	dir, err := providerFactorySourceDir()
	if err != nil {
		t.Fatal(err)
	}
	census, err := scanLegacySessionProviderFactoryCallers(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(census.violations) != 0 {
		t.Fatalf("canonical provider factory census found unclassified uses:\n%s", strings.Join(census.violations, "\n"))
	}
	if !maps.Equal(census.canonicalAliasBindings, canonicalProviderAliasBindings) {
		t.Fatalf("canonical provider alias/seam census changed\n got:\n%s\nwant:\n%s", formatProviderFactoryCensus(census.canonicalAliasBindings), formatProviderFactoryCensus(canonicalProviderAliasBindings))
	}
	if !maps.Equal(census.canonicalCalls, canonicalProviderCalls) {
		t.Fatalf("canonical provider call census changed\n got:\n%s\nwant:\n%s", formatProviderFactoryCensus(census.canonicalCalls), formatProviderFactoryCensus(canonicalProviderCalls))
	}
}

func TestProviderFactoryCensusExcludesDeclarationsButScansProvidersGo(t *testing.T) {
	census := scanProviderFactoryFixture(t, "providers.go", `package main

func newSessionProviderWithError() {}
func newSessionProviderForCityWithError() {}
func newSessionProviderFromContextWithError() {}
func newStatusSessionProviderForCityWithError() {}
func newStatusSessionProviderForCityWithSnapshotWithError() {}
func sessionProviderOrExit() {}
`)
	if len(census.references) != 0 || len(census.aliasBindings) != 0 || len(census.aliasCalls) != 0 || len(census.exitHelperUses) != 0 || census.directCalls != 0 || len(census.violations) != 0 {
		t.Fatalf("declarations were counted as uses: %#v", census)
	}
}

func TestProviderFactoryCensusTracksSecondOrderPackageAliasesToFixedPoint(t *testing.T) {
	census := scanProviderFactoryFixture(t, "fixture.go", `package main

var first = newSessionProviderWithError
var second = first

func invoke() { second() }
`)
	for _, alias := range []string{"first", "second"} {
		if !census.aliases[alias] {
			t.Fatalf("alias %q was not tracked: %#v", alias, census.aliases)
		}
	}
	if got := census.aliasBindings["fixture.go:<package>:second=first"]; got != 1 {
		t.Fatalf("second-order alias binding count = %d, want 1; census=%s", got, formatProviderFactoryCensus(census.aliasBindings))
	}
	if got := census.aliasCalls["fixture.go:invoke:second"]; got != 1 {
		t.Fatalf("second-order alias invocation count = %d, want 1; census=%s", got, formatProviderFactoryCensus(census.aliasCalls))
	}
	if got := census.invocationCount(); got != 1 {
		t.Fatalf("fixture invocation count = %d, want 1", got)
	}
}

func TestProviderFactoryCensusRejectsCallbackEscape(t *testing.T) {
	census := scanProviderFactoryFixture(t, "fixture.go", `package main

func acceptProviderFactory(any) {}
func evade() { acceptProviderFactory(newSessionProviderWithError) }
`)
	want := "fixture.go:evade:newSessionProviderWithError is a non-call provider factory use"
	if !slices.Contains(census.violations, want) {
		t.Fatalf("callback escape violations = %q, want %q", census.violations, want)
	}
}

func TestProviderFactoryCensusRejectsRetiredExitHelper(t *testing.T) {
	census := scanProviderFactoryFixture(t, "providers.go", `package main

func evade() { sessionProviderOrExit(nil, nil) }
`)
	want := "providers.go:evade:sessionProviderOrExit is a retired exit-helper use"
	if !slices.Contains(census.violations, want) {
		t.Fatalf("direct exit-helper violations = %q, want %q", census.violations, want)
	}
}

func TestProviderFactoryCensusRejectsDirectBlankError(t *testing.T) {
	census := scanProviderFactoryFixture(t, "fixture.go", `package main

func evade() {
	provider, _ := newSessionProvider()
	_ = provider
}
`)
	assertProviderFactoryViolation(t, census, "fixture.go:evade:newSessionProvider assigns construction error to _")
}

func TestProviderFactoryCensusRejectsAliasBlankError(t *testing.T) {
	census := scanProviderFactoryFixture(t, "fixture.go", `package main

var factory = newSessionProvider

func evade() {
	provider, _ := factory()
	_ = provider
}
`)
	assertProviderFactoryViolation(t, census, "fixture.go:evade:factory assigns construction error to _")
}

func TestProviderFactoryCensusRejectsCanonicalCallbackEscape(t *testing.T) {
	census := scanProviderFactoryFixture(t, "fixture.go", `package main

func acceptProviderFactory(any) {}
func evade() { acceptProviderFactory(newSessionProvider) }
`)
	assertProviderFactoryViolation(t, census, "fixture.go:evade:newSessionProvider is a non-call canonical provider factory use")
}

func TestProviderFactoryCensusRejectsDiscardedCanonicalCall(t *testing.T) {
	census := scanProviderFactoryFixture(t, "fixture.go", `package main

func evade() { newSessionProvider() }
`)
	assertProviderFactoryViolation(t, census, "fixture.go:evade:newSessionProvider discards provider construction results")
}

func TestProviderFactoryCensusRejectsUnreviewedMultiReturnWrapper(t *testing.T) {
	census := scanProviderFactoryFixture(t, "fixture.go", `package main

func unreviewed() (runtime.Provider, error) { return newSessionProvider() }
`)
	assertProviderFactoryViolation(t, census, "fixture.go:unreviewed:newSessionProvider forwards multiple results outside a reviewed provider wrapper")
}

func TestProviderFactoryCensusRejectsUncheckedBoundErrors(t *testing.T) {
	tests := map[string]string{
		"blank use": `package main
func evade() {
	provider, err := newSessionProvider()
	_ = err
	use(provider)
}
`,
		"overwrite before check": `package main
func evade() {
	provider, err := newSessionProvider()
	err = nil
	if err != nil { return }
	use(provider)
}
`,
		"provider use before check": `package main
func evade() {
	provider, err := newSessionProvider()
	use(provider)
	if err != nil { return }
}
`,
	}
	for name, source := range tests {
		t.Run(name, func(t *testing.T) {
			census := scanProviderFactoryFixture(t, "fixture.go", source)
			assertProviderFactoryViolation(t, census, "fixture.go:evade:newSessionProvider does not immediately guard construction error")
		})
	}
}

func TestProviderFactoryCensusAllowsImmediateErrorCheckAndReturn(t *testing.T) {
	census := scanProviderFactoryFixture(t, "fixture.go", `package main
func allowed() error {
	provider, err := newSessionProvider()
	if err != nil { return err }
	use(provider)
	return nil
}
`)
	if len(census.violations) != 0 {
		t.Fatalf("immediate error check violations = %q", census.violations)
	}
}

func assertProviderFactoryViolation(t *testing.T, census providerFactoryCensus, want string) {
	t.Helper()
	if !slices.Contains(census.violations, want) {
		t.Fatalf("provider factory violations = %q, want %q", census.violations, want)
	}
}

func scanProviderFactoryFixture(t *testing.T, name, source string) providerFactoryCensus {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(source), 0o600); err != nil {
		t.Fatalf("WriteFile(%q): %v", name, err)
	}
	census, err := scanLegacySessionProviderFactoryCallers(dir)
	if err != nil {
		t.Fatal(err)
	}
	return census
}

func providerFactorySourceDir() (string, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("get provider factory census working directory: %w", err)
	}
	for _, candidate := range []string{cwd, filepath.Join(cwd, "cmd", "gc")} {
		info, statErr := os.Stat(filepath.Join(candidate, "providers.go"))
		if statErr == nil && !info.IsDir() {
			return filepath.Clean(candidate), nil
		}
		if statErr != nil && !os.IsNotExist(statErr) {
			return "", fmt.Errorf("inspect provider factory source directory %q: %w", candidate, statErr)
		}
	}
	return "", fmt.Errorf("locate cmd/gc provider sources from working directory %q", cwd)
}

type parsedProviderFactoryFile struct {
	name string
	file *ast.File
}

type providerAliasBinding struct {
	left  string
	right string
}

func scanLegacySessionProviderFactoryCallers(dir string) (providerFactoryCensus, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return providerFactoryCensus{}, fmt.Errorf("read provider factory source directory %q: %w", dir, err)
	}

	var files []parsedProviderFactoryFile
	fset := token.NewFileSet()
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		parsed, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, 0)
		if err != nil {
			return providerFactoryCensus{}, fmt.Errorf("parse provider factory source %q: %w", name, err)
		}
		files = append(files, parsedProviderFactoryFile{name: name, file: parsed})
	}

	aliases := discoverProviderFactoryAliases(files, legacySessionProviderFactories)
	bindingUses, aliasBindings := providerFactoryAliasBindings(files, legacySessionProviderFactories, aliases)
	census := providerFactoryCensus{
		references:             map[string]int{},
		aliasBindings:          aliasBindings,
		aliasCalls:             map[string]int{},
		aliases:                aliases,
		exitHelperUses:         map[string]int{},
		canonicalAliasBindings: map[string]int{},
		canonicalCalls:         map[string]int{},
	}
	for _, parsed := range files {
		for _, declaration := range parsed.file.Decls {
			functionName, roots := providerDeclarationRoots(declaration)
			for _, root := range roots {
				scanProviderFactoryDeclaration(parsed.name, functionName, root, bindingUses, &census)
			}
		}
	}
	scanCanonicalProviderFactoryCallers(files, &census)
	slices.Sort(census.violations)
	return census, nil
}

func scanCanonicalProviderFactoryCallers(files []parsedProviderFactoryFile, census *providerFactoryCensus) {
	aliases := discoverProviderFactoryAliases(files, canonicalProviderResultSources)
	bindingUses, aliasBindings := providerFactoryAliasBindings(files, canonicalProviderResultSources, aliases)
	census.canonicalAliasBindings = aliasBindings

	for _, parsed := range files {
		for _, declaration := range parsed.file.Decls {
			functionName, roots := providerDeclarationRoots(declaration)
			for _, root := range roots {
				scanCanonicalProviderFactoryDeclaration(parsed.name, functionName, root, bindingUses, aliases, census)
			}
		}
	}
}

func scanCanonicalProviderFactoryDeclaration(
	fileName, functionName string,
	root ast.Node,
	bindingUses map[*ast.Ident]providerAliasBinding,
	aliases map[string]bool,
	census *providerFactoryCensus,
) {
	parents := providerFactoryParentMap(root)
	ast.Inspect(root, func(node ast.Node) bool {
		identifier, ok := node.(*ast.Ident)
		if !ok || (!canonicalProviderResultSources[identifier.Name] && !aliases[identifier.Name]) {
			return true
		}
		if _, isBinding := bindingUses[identifier]; isBinding {
			return true
		}

		key := fmt.Sprintf("%s:%s:%s", fileName, functionName, identifier.Name)
		parent, isCall := parents[identifier].(*ast.CallExpr)
		if !isCall || parent.Fun != identifier {
			census.violations = append(census.violations, key+" is a non-call canonical provider factory use")
			return true
		}

		shape, violation := canonicalProviderCallDisposition(fileName, functionName, identifier.Name, parent, parents)
		census.canonicalCalls[key+":"+shape]++
		if violation != "" {
			census.violations = append(census.violations, key+violation)
		}
		return true
	})
}

func providerFactoryParentMap(root ast.Node) map[ast.Node]ast.Node {
	parents := map[ast.Node]ast.Node{}
	var stack []ast.Node
	ast.Inspect(root, func(node ast.Node) bool {
		if node == nil {
			stack = stack[:len(stack)-1]
			return false
		}
		if len(stack) > 0 {
			parents[node] = stack[len(stack)-1]
		}
		stack = append(stack, node)
		return true
	})
	return parents
}

func canonicalProviderCallDisposition(fileName, functionName, callee string, call *ast.CallExpr, parents map[ast.Node]ast.Node) (string, string) {
	key := fmt.Sprintf("%s:%s:%s", fileName, functionName, callee)
	switch parent := parents[call].(type) {
	case *ast.AssignStmt:
		if len(parent.Rhs) != 1 || parent.Rhs[0] != call || len(parent.Lhs) != 2 {
			return "unreviewed-assignment", " uses an unreviewed provider result assignment"
		}
		errorName, ok := parent.Lhs[1].(*ast.Ident)
		if !ok {
			return "unreviewed-error-target", " does not bind construction error to a named local"
		}
		if errorName.Name == "_" {
			return "blank-error", " assigns construction error to _"
		}
		providerName, ok := parent.Lhs[0].(*ast.Ident)
		if !ok || providerName.Name == "_" {
			return "bind-error", " does not bind the provider to a named local"
		}
		if !hasImmediateProviderErrorGuard(parent, providerName.Name, errorName.Name, parents) {
			return "bind-error", " does not immediately guard construction error"
		}
		return "bind-error", ""
	case *ast.ValueSpec:
		if len(parent.Values) != 1 || parent.Values[0] != call || len(parent.Names) != 2 {
			return "unreviewed-declaration", " uses an unreviewed provider result declaration"
		}
		if parent.Names[1].Name == "_" {
			return "blank-error", " assigns construction error to _"
		}
		return "bind-error", " does not immediately guard construction error"
	case *ast.ExprStmt:
		return "discarded", " discards provider construction results"
	case *ast.ReturnStmt:
		forward := key + "->return"
		if len(parent.Results) == 1 && parent.Results[0] == call && reviewedCanonicalProviderForwards[forward] {
			return "forward-return", ""
		}
		return "unreviewed-forward", " forwards multiple results outside a reviewed provider wrapper"
	case *ast.CallExpr:
		outer, ok := parent.Fun.(*ast.Ident)
		forward := key
		if ok {
			forward += "->" + outer.Name
		}
		if ok && len(parent.Args) == 1 && parent.Args[0] == call && reviewedCanonicalProviderForwards[forward] {
			return "forward-to-" + outer.Name, ""
		}
		return "unreviewed-forward", " forwards multiple results outside a reviewed provider wrapper"
	default:
		return "unreviewed-context", " uses provider construction results in an unreviewed context"
	}
}

func hasImmediateProviderErrorGuard(assign *ast.AssignStmt, providerName, errorName string, parents map[ast.Node]ast.Node) bool {
	statements, index, ok := providerStatementSequence(assign, parents)
	if !ok || index+1 >= len(statements) {
		return false
	}
	guard, ok := statements[index+1].(*ast.IfStmt)
	if !ok || guard.Init != nil || !isExactProviderErrorCondition(guard.Cond, errorName) || providerNameReferenced(guard.Body, providerName) {
		return false
	}
	if blockEndsWithDirectReturn(guard.Body) {
		return true
	}
	// buildDoctorChecks intentionally converts construction failure into one
	// registered error check and confines every provider use to the else branch.
	// A short declaration plus a final if/else in the same statement sequence
	// proves the possibly-nil provider cannot escape that branch.
	_, hasElseBlock := guard.Else.(*ast.BlockStmt)
	return hasElseBlock && assign.Tok == token.DEFINE && index+1 == len(statements)-1
}

func providerStatementSequence(statement ast.Stmt, parents map[ast.Node]ast.Node) ([]ast.Stmt, int, bool) {
	var statements []ast.Stmt
	switch parent := parents[statement].(type) {
	case *ast.BlockStmt:
		statements = parent.List
	case *ast.CaseClause:
		statements = parent.Body
	case *ast.CommClause:
		statements = parent.Body
	default:
		return nil, 0, false
	}
	for index, candidate := range statements {
		if candidate == statement {
			return statements, index, true
		}
	}
	return nil, 0, false
}

func isExactProviderErrorCondition(expression ast.Expr, errorName string) bool {
	condition, ok := expression.(*ast.BinaryExpr)
	if !ok || condition.Op != token.NEQ {
		return false
	}
	left, leftOK := condition.X.(*ast.Ident)
	right, rightOK := condition.Y.(*ast.Ident)
	return leftOK && rightOK && left.Name == errorName && right.Name == "nil"
}

func providerNameReferenced(root ast.Node, providerName string) bool {
	referenced := false
	ast.Inspect(root, func(node ast.Node) bool {
		identifier, ok := node.(*ast.Ident)
		if ok && identifier.Name == providerName {
			referenced = true
			return false
		}
		return !referenced
	})
	return referenced
}

func blockEndsWithDirectReturn(block *ast.BlockStmt) bool {
	if block == nil || len(block.List) == 0 {
		return false
	}
	_, ok := block.List[len(block.List)-1].(*ast.ReturnStmt)
	return ok
}

func discoverProviderFactoryAliases(files []parsedProviderFactoryFile, sources map[string]bool) map[string]bool {
	aliases := map[string]bool{}
	for changed := true; changed; {
		changed = false
		for _, parsed := range files {
			for _, declaration := range parsed.file.Decls {
				general, ok := declaration.(*ast.GenDecl)
				if !ok || general.Tok != token.VAR {
					continue
				}
				for _, spec := range general.Specs {
					values, ok := spec.(*ast.ValueSpec)
					if !ok {
						continue
					}
					for index, value := range values.Values {
						if index >= len(values.Names) {
							break
						}
						identifier, ok := value.(*ast.Ident)
						if !ok || (!sources[identifier.Name] && !aliases[identifier.Name]) {
							continue
						}
						alias := values.Names[index].Name
						if alias == "_" || sources[alias] || aliases[alias] {
							continue
						}
						aliases[alias] = true
						changed = true
					}
				}
			}
		}
	}
	return aliases
}

func providerFactoryAliasBindings(files []parsedProviderFactoryFile, sources, aliases map[string]bool) (map[*ast.Ident]providerAliasBinding, map[string]int) {
	bindingUses := map[*ast.Ident]providerAliasBinding{}
	bindings := map[string]int{}
	for _, parsed := range files {
		for _, declaration := range parsed.file.Decls {
			general, ok := declaration.(*ast.GenDecl)
			if !ok || general.Tok != token.VAR {
				continue
			}
			for _, spec := range general.Specs {
				values, ok := spec.(*ast.ValueSpec)
				if !ok {
					continue
				}
				for index, value := range values.Values {
					if index >= len(values.Names) {
						break
					}
					identifier, ok := value.(*ast.Ident)
					if !ok || (!sources[identifier.Name] && !aliases[identifier.Name]) {
						continue
					}
					binding := providerAliasBinding{left: values.Names[index].Name, right: identifier.Name}
					bindingUses[identifier] = binding
					bindings[fmt.Sprintf("%s:<package>:%s=%s", parsed.name, binding.left, binding.right)]++
				}
			}
		}
	}
	return bindingUses, bindings
}

func providerDeclarationRoots(declaration ast.Decl) (string, []ast.Node) {
	switch typed := declaration.(type) {
	case *ast.FuncDecl:
		if typed.Body == nil {
			return typed.Name.Name, nil
		}
		return typed.Name.Name, []ast.Node{typed.Body}
	case *ast.GenDecl:
		roots := make([]ast.Node, 0, len(typed.Specs))
		for _, spec := range typed.Specs {
			values, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			for _, value := range values.Values {
				roots = append(roots, value)
			}
		}
		return "<package>", roots
	default:
		return "<package>", nil
	}
}

func scanProviderFactoryDeclaration(fileName, functionName string, root ast.Node, bindingUses map[*ast.Ident]providerAliasBinding, census *providerFactoryCensus) {
	directCallUses := map[*ast.Ident]bool{}
	ast.Inspect(root, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		if identifier, ok := call.Fun.(*ast.Ident); ok {
			directCallUses[identifier] = true
		}
		return true
	})

	ast.Inspect(root, func(node ast.Node) bool {
		identifier, ok := node.(*ast.Ident)
		if !ok {
			return true
		}
		key := fmt.Sprintf("%s:%s:%s", fileName, functionName, identifier.Name)
		isDirectCall := directCallUses[identifier]
		_, isBinding := bindingUses[identifier]

		if legacySessionProviderFactories[identifier.Name] {
			census.references[key]++
			if isDirectCall {
				census.directCalls++
			} else if !isBinding {
				census.violations = append(census.violations, key+" is a non-call provider factory use")
			}
			return true
		}
		if census.aliases[identifier.Name] {
			if isDirectCall {
				census.aliasCalls[key]++
			} else if !isBinding {
				census.violations = append(census.violations, key+" is a non-call provider factory use")
			}
			return true
		}
		if identifier.Name == "sessionProviderOrExit" {
			census.exitHelperUses[key]++
			if !isDirectCall {
				census.violations = append(census.violations, key+" is a non-call exit-helper use")
			} else {
				census.violations = append(census.violations, key+" is a retired exit-helper use")
			}
		}
		return true
	})
}

func formatProviderFactoryCensus(census map[string]int) string {
	entries := make([]string, 0, len(census))
	for caller, count := range census {
		entries = append(entries, fmt.Sprintf("%s = %d", caller, count))
	}
	slices.Sort(entries)
	return strings.Join(entries, "\n")
}
