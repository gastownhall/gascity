package main

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// A real CLI open readies the builtin cache and regenerates the city's
// canonical provider shim. Install the tripwire AFTER that preparation so
// managed retry cannot escape the fixture through the freshly generated shim.
// This only runs in an explicitly marked command child of the integration
// test; normal gc and every other testscript retain their normal setup.
func configureObservationCLIForTests() {
	city := os.Getenv("GC_TEST_OBSERVATION_CITY")
	if city == "" {
		return
	}
	if err := EnsureBuiltinRuntimeAssets(city, io.Discard); err != nil {
		panic(fmt.Errorf("prepare observation command fixture: %w", err))
	}
	data, err := os.ReadFile(filepath.Join(city, "bin", "gc-beads-bd.sh"))
	if err != nil {
		panic(err)
	}
	if err := os.WriteFile(gcBeadsBdScriptPath(city), data, 0o700); err != nil {
		panic(err)
	}
	// os.Executable resolves the gc symlink to gc.test; a managed hook child
	// must keep the CLI basename, as reexecGCTestBinaryForTests requires.
	hookRunExecutable = func() (string, error) { return os.Args[0], nil }
}

// These are observation doors, rather than commands that also contain writes
// (hook --claim, doctor --fix, and nudge delivery). Callback initializers are
// roots too: hookCurrentSessionFrontDoor must not hide a recovering opener.
var observationRecoveryRoots = []string{
	"cmdCityStatusLocalFallback", "openCityStatusStore",
	"buildDoctorChecks", "doctorLiveSessionSinks",
	"hookQueryEnv", "hookWorkQueryStores", "hookSessionDrainPending",
	"doMailCheckFallback", "cmdMailInboxWithJSON", "doMailPeekFallback",
	"hookCurrentSessionFrontDoor", "wispStepInjectionContent",
	"resolveNudgeTarget",
}

var observationForbiddenRecovery = []string{
	"bdRuntimeEnvWithError", "bdRuntimeEnvForRigWithError",
	"bdStoreForCity", "bdStoreForCityWithConfig", "bdCommandRunnerForCity",
	"bdCommandRunnerWithManagedRetry", "bdCommandRunnerWithManagedRetryErr",
	"recoverManagedBDCommand", "healthBeadsProviderContext",
	"recoverBeadsProviderContext", "startBeadsLifecycle",
	"nativeDoltOpenEnvForScope", "nativeDoltOpenEnvForScopeContext",
	"nativeDoltOneShotOpenEnvForScope", "nativeDoltOneShotOpenEnvForScopeContext",
	"openCityStoreAt", "openCityStoreResultAt", "openStoreAtForCity",
	"managedRecoveryEnabled",
}

func TestObservationDoctorOmissionCountMatchesRegisteredChecks(t *testing.T) {
	path := filepath.Join(moduleRoot(t), "cmd", "gc", "cmd_doctor.go")
	file, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var registrations int
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "buildDoctorChecks" {
			continue
		}
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if ok {
				if id, ok := call.Fun.(*ast.Ident); ok && id.Name == "registerCityStoreCheck" {
					registrations++
				}
			}
			return true
		})
	}
	if registrations == 0 {
		t.Fatal("no store-check registrations found; derive the omission set from the replacement registry")
	}
	if registrations != doctorCityStoreCheckCount {
		t.Fatalf("doctor says it omits %d city store checks but registers %d; omission diagnostics must count every registered check", doctorCityStoreCheckCount, registrations)
	}
}

// The common env builder implements both policies. A literal false at its
// call site is the existing verify-only boundary; a true or unknown value is
// forbidden. Do not make a whole observation opener opaque to the walk: its
// other references (especially retry wrappers and native callbacks) matter.
var observationPolicyEnvBuilders = []string{
	"bdRuntimeEnvWithErrorRecovery", "bdRuntimeEnvWithErrorRecoveryContext",
	"bdRuntimeEnvForRigWithErrorRecovery", "bdRuntimeEnvForRigWithErrorRecoveryContext",
}

type observationSourceGraph map[string][]string

// observationGraph follows references as well as calls. This covers helpers
// handed to store factories and package callback aliases, not just direct
// f() expressions. Foreign methods/interface dispatch are covered by the
// executable tripwires, not guessed by a method's unqualified name.
func observationGraph(files []*ast.File) observationSourceGraph {
	nodes := map[string]ast.Node{}
	for _, file := range files {
		for _, decl := range file.Decls {
			switch d := decl.(type) {
			case *ast.FuncDecl:
				if d.Recv == nil {
					nodes[d.Name.Name] = d.Body
				}
			case *ast.GenDecl:
				if d.Tok != token.VAR {
					continue
				}
				for _, spec := range d.Specs {
					v := spec.(*ast.ValueSpec)
					for i, name := range v.Names {
						if i < len(v.Values) {
							nodes[name.Name] = v.Values[i]
						}
					}
				}
			}
		}
	}
	graph := observationSourceGraph{}
	for name, body := range nodes {
		graph[name] = nil
		ast.Inspect(body, func(n ast.Node) bool {
			if call, ok := n.(*ast.CallExpr); ok {
				if id, ok := call.Fun.(*ast.Ident); ok && slices.Contains(observationPolicyEnvBuilders, id.Name) {
					var verifyOnly bool
					if len(call.Args) > 0 {
						last, ok := call.Args[len(call.Args)-1].(*ast.Ident)
						verifyOnly = ok && last.Name == "false"
					}
					if !verifyOnly {
						graph[name] = append(graph[name], "managedRecoveryEnabled")
					}
					return false
				}
			}
			// pkg.f is not the local f even when their names happen to match.
			if selector, ok := n.(*ast.SelectorExpr); ok {
				ast.Inspect(selector.X, func(inner ast.Node) bool {
					if id, ok := inner.(*ast.Ident); ok && nodes[id.Name] != nil {
						graph[name] = append(graph[name], id.Name)
					}
					return true
				})
				return false
			}
			if id, ok := n.(*ast.Ident); ok && nodes[id.Name] != nil && id.Name != name {
				graph[name] = append(graph[name], id.Name)
			}
			return true
		})
		slices.Sort(graph[name])
		graph[name] = slices.Compact(graph[name])
	}
	return graph
}

func (g observationSourceGraph) recoveryPath(root string, forbidden []string) []string {
	seen := map[string]bool{}
	var visit func(string, []string) []string
	visit = func(name string, path []string) []string {
		if seen[name] {
			return nil
		}
		seen[name] = true
		path = append(slices.Clone(path), name)
		if slices.Contains(forbidden, name) {
			return path
		}
		for _, next := range g[name] {
			if found := visit(next, path); found != nil {
				return found
			}
		}
		return nil
	}
	return visit(root, nil)
}

func TestObservationCommandsCannotReachManagedRecovery(t *testing.T) {
	root := moduleRoot(t)
	paths, err := filepath.Glob(filepath.Join(root, "cmd", "gc", "*.go"))
	if err != nil {
		t.Fatal(err)
	}
	var files []*ast.File
	for _, path := range paths {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, file)
	}
	graph := observationGraph(files)
	for _, root := range observationRecoveryRoots {
		t.Run(root, func(t *testing.T) {
			if _, exists := graph[root]; !exists {
				t.Fatalf("observation root %s is absent; update the registry when moving its call site", root)
			}
			if path := graph.recoveryPath(root, observationForbiddenRecovery); path != nil {
				t.Fatalf("observation acquires a recovering store through %s", strings.Join(path, " -> "))
			}
		})
	}
}

// Controlled mutation of a parsed fixture proves an indirect recovering
// callback is caught. The safe graph and a cycle are controls for the walker.
func TestObservationRecoveryGuardDetectsIndirectMutation(t *testing.T) {
	const source = `package main
var observation = safe
func safe() { helper() }
func helper() { safe() }
func recoverManagedBDCommand() {}
`
	for _, mutation := range []struct {
		name, source string
		wantPath     bool
	}{
		{"safe-cycle", source, false},
		{"helper-recovery", strings.Replace(source, "helper() { safe() }", "helper() { recoverManagedBDCommand() }", 1), true},
		{"callback-recovery", strings.Replace(source, "observation = safe", "observation = recoverManagedBDCommand", 1), true},
		{"verify-only-env", strings.Replace(source, "helper() { safe() }", "helper() { bdRuntimeEnvWithErrorRecoveryContext(nil, city, false) }", 1), false},
		{"recovering-env", strings.Replace(source, "helper() { safe() }", "helper() { bdRuntimeEnvWithErrorRecoveryContext(nil, city, true) }", 1), true},
	} {
		t.Run(mutation.name, func(t *testing.T) {
			file, err := parser.ParseFile(token.NewFileSet(), "mutation.go", mutation.source, 0)
			if err != nil {
				t.Fatal(err)
			}
			path := observationGraph([]*ast.File{file}).recoveryPath("observation", []string{"recoverManagedBDCommand", "managedRecoveryEnabled"})
			if (path != nil) != mutation.wantPath {
				t.Fatalf("recovery path = %v, want detected=%t", path, mutation.wantPath)
			}
		})
	}
}
