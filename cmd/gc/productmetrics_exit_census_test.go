package main

import (
	"fmt"
	"go/ast"
	"go/format"
	"go/parser"
	"go/token"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// The command metrics lifecycle must retain control until run returns. These
// are the only production calls that may bypass that funnel. Built-in panic is
// intentionally outside this census: the lifecycle has a separately tested
// panic path, while process-termination APIs cannot be recovered by the
// invocation wrapper.
var allowedGCExitBypassSites = map[string]func(gcExitBypassSite) error{
	"cmd_supervisor.go:supervisorHardExit:os.Exit": func(site gcExitBypassSite) error {
		if got := expressionShape(site.call.Args); got != "code" {
			return fmt.Errorf("exit argument = %q, want %q", got, "code")
		}
		literal, ok := site.root.(*ast.FuncLit)
		if !ok || literal.Body == nil || !hasNamedParameter(literal.Type, "code") {
			return fmt.Errorf("owner is not the reviewed function literal with a code parameter")
		}
		expression, ok := site.parent.(*ast.ExprStmt)
		if !ok || expression.X != site.call || !hasExactExitAncestors(site, expression, literal.Body, literal) {
			return fmt.Errorf("os.Exit is not a direct statement of the outer supervisorHardExit function literal")
		}
		if len(literal.Body.List) < 2 || literal.Body.List[len(literal.Body.List)-1] != expression {
			return fmt.Errorf("os.Exit is not the final outer supervisorHardExit statement")
		}
		if !isSupervisorHardExitBreadcrumb(literal.Body.List[len(literal.Body.List)-2]) {
			return fmt.Errorf("os.Exit is not immediately preceded by the exact repeated-shutdown breadcrumb")
		}
		return nil
	},
	"dolt_scope_watchdog.go:init:os.Exit": func(site gcExitBypassSite) error {
		return validatePrivateWatchdogExit(site, "managedDoltScopeWatchdogArg", "runManagedDoltScopeWatchdog(os.Args[2:], os.Stdout, os.Stderr)")
	},
	"dolt_start_managed.go:init:os.Exit": func(site gcExitBypassSite) error {
		return validatePrivateWatchdogExit(site, "managedDoltTestWatchdogArg", "runManagedDoltTestWatchdog(os.Args[2:], os.Stdout, os.Stderr)")
	},
	"main.go:main:os.Exit": func(site gcExitBypassSite) error {
		function, ok := site.root.(*ast.FuncDecl)
		if !ok || function.Name.Name != "main" || function.Body == nil {
			return fmt.Errorf("main exit owner is not a function declaration")
		}
		if got := expressionShape(site.call.Args); got != "mainExitCode(os.Args[1:], os.Stdout, os.Stderr)" {
			return fmt.Errorf("exit argument = %q, want the central process-entry funnel", got)
		}
		expression, ok := site.parent.(*ast.ExprStmt)
		if !ok || expression.X != site.call || !hasExactExitAncestors(site, expression, function.Body, function) {
			return fmt.Errorf("main os.Exit is not a direct function-body statement")
		}
		if len(function.Body.List) != 1 || function.Body.List[0] != expression {
			return fmt.Errorf("main body is not exactly the central os.Exit statement")
		}
		return nil
	},
	"cmd_supervisor.go:supervisorSignalLoop:supervisorHardExit": func(site gcExitBypassSite) error {
		function, ok := site.root.(*ast.FuncDecl)
		if !ok || function.Name.Name != "supervisorSignalLoop" {
			return fmt.Errorf("owner is not supervisorSignalLoop")
		}
		if got := expressionShape(site.call.Args); got != "stderr, supervisorHardExitCodeRepeatedShutdown" {
			return fmt.Errorf("hard-exit arguments = %q, want the reviewed repeated-shutdown call", got)
		}
		expression, ok := site.parent.(*ast.ExprStmt)
		if !ok || expression.X != site.call || len(site.ancestors) != 10 {
			return fmt.Errorf("hard exit is not a direct expression statement in the shutdown guard")
		}
		body, ok := site.ancestors[1].(*ast.BlockStmt)
		guard, guardOK := site.ancestors[2].(*ast.IfStmt)
		if !ok || !guardOK || guard.Body != body || !isReviewedSupervisorShutdownCondition(guard.Cond) {
			return fmt.Errorf("hard exit is not in the exact requestShutdown true body")
		}
		if len(body.List) != 2 || body.List[0] != expression {
			return fmt.Errorf("hard exit moved within the requestShutdown true body")
		}
		returned, ok := body.List[1].(*ast.ReturnStmt)
		if !ok || len(returned.Results) != 0 {
			return fmt.Errorf("hard exit is not followed by a direct empty return")
		}
		clause, clauseOK := site.ancestors[3].(*ast.CommClause)
		selectBody, selectBodyOK := site.ancestors[4].(*ast.BlockStmt)
		selection, selectionOK := site.ancestors[5].(*ast.SelectStmt)
		loopBody, loopBodyOK := site.ancestors[6].(*ast.BlockStmt)
		loop, loopOK := site.ancestors[7].(*ast.ForStmt)
		functionBody, functionBodyOK := site.ancestors[8].(*ast.BlockStmt)
		owner, ownerOK := site.ancestors[9].(*ast.FuncDecl)
		if !clauseOK || !selectBodyOK || !selectionOK || !loopBodyOK || !loopOK || !functionBodyOK || !ownerOK || owner != function {
			return fmt.Errorf("hard exit does not have the direct signal-clause/select/for/function ancestry")
		}
		if !isSupervisorSignalClause(clause) || len(clause.Body) == 0 || clause.Body[len(clause.Body)-1] != guard {
			return fmt.Errorf("shutdown guard is not the final direct statement of the signal clause")
		}
		if selection.Body != selectBody || loop.Body != loopBody || function.Body != functionBody || loop.Init != nil || loop.Cond != nil || loop.Post != nil {
			return fmt.Errorf("hard exit select/for/function ancestry is not the reviewed direct chain")
		}
		if len(loopBody.List) != 1 || loopBody.List[0] != selection || len(functionBody.List) != 1 || functionBody.List[0] != loop {
			return fmt.Errorf("select and for are not the sole direct statements of their reviewed owners")
		}
		return nil
	},
}

// startSummaryLine reads a data field named Fatal; it does not invoke a
// process-terminating method. Pin that sole name collision so a new escaped
// (*log.Logger).Fatal method value still fails the source census.
var allowedNonCallFatalReferences = map[string]string{
	"start_output.go:startSummaryLine:log.Logger.Fatal": "s.Fatal",
}

type sessionProviderFactoryShape struct {
	parameters string
	results    string
}

var canonicalSessionProviderFactories = map[string]sessionProviderFactoryShape{
	"newSessionProvider": {
		results: "runtime.Provider,error",
	},
	"newSessionProviderForCity": {
		parameters: "*config.City,string",
		results:    "runtime.Provider,error",
	},
	"newSessionProviderFromContext": {
		parameters: "sessionProviderContext,*sessionBeadSnapshot",
		results:    "runtime.Provider,error",
	},
	"newStatusSessionProviderForCity": {
		parameters: "*config.City,string",
		results:    "runtime.Provider,error",
	},
	"newStatusSessionProviderForCityWithSnapshot": {
		parameters: "*config.City,string,*sessionBeadSnapshot",
		results:    "runtime.Provider,error",
	},
}

var retiredSessionProviderFactoryNames = map[string]bool{
	"newSessionProviderWithError":                          true,
	"newSessionProviderForCityWithError":                   true,
	"newSessionProviderFromContextWithError":               true,
	"newStatusSessionProviderForCityWithError":             true,
	"newStatusSessionProviderForCityWithSnapshotWithError": true,
	"sessionProviderOrExit":                                true,
}

type gcExitBypassSite struct {
	file   string
	owner  string
	symbol string
	call   *ast.CallExpr
	root   ast.Node
	parent ast.Node
	// ancestors are ordered nearest-first, beginning with parent.
	ancestors []ast.Node
}

func (s gcExitBypassSite) key() string {
	return s.file + ":" + s.owner + ":" + s.symbol
}

func TestProductMetricsExitBypassCensus(t *testing.T) {
	dir, err := providerFactorySourceDir()
	if err != nil {
		t.Fatal(err)
	}
	sites, violations, err := scanGCExitBypasses(dir)
	if err != nil {
		t.Fatal(err)
	}

	seen := make(map[string]int, len(allowedGCExitBypassSites))
	for _, site := range sites {
		key := site.key()
		validate, ok := allowedGCExitBypassSites[key]
		if !ok {
			violations = append(violations, key+" is not an allowed process exit")
			continue
		}
		seen[key]++
		if err := validate(site); err != nil {
			violations = append(violations, key+": "+err.Error())
		}
	}
	for key := range allowedGCExitBypassSites {
		if seen[key] != 1 {
			violations = append(violations, fmt.Sprintf("%s count = %d, want exactly 1", key, seen[key]))
		}
	}
	slices.Sort(violations)
	if len(violations) != 0 {
		t.Fatalf("production exit-bypass census failed:\n%s", strings.Join(violations, "\n"))
	}
}

func TestSessionProviderFactoriesUseCanonicalErrorAPI(t *testing.T) {
	dir, err := providerFactorySourceDir()
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, dir+"/providers.go", nil, 0)
	if err != nil {
		t.Fatalf("parse providers.go: %v", err)
	}

	violations := sessionProviderFactoryAPIViolations(file)
	retired, err := retiredSessionProviderDeclarationViolations(dir)
	if err != nil {
		t.Fatal(err)
	}
	violations = append(violations, retired...)
	slices.Sort(violations)
	if len(violations) != 0 {
		t.Fatalf("session provider API is not canonical:\n%s", strings.Join(violations, "\n"))
	}
}

func TestSessionProviderFactoryAPICensusRejectsCompatibilityShapes(t *testing.T) {
	dir := t.TempDir()
	writeExitCensusFixture(t, dir, "providers.go", `package main
func newSessionProvider() runtime.Provider { panic("fixture") }
func newSessionProviderForCity(*config.City, string) (runtime.Provider, error) { panic("fixture") }
func newSessionProviderFromContext(sessionProviderContext, *sessionBeadSnapshot) (runtime.Provider, error) { panic("fixture") }
func newStatusSessionProviderForCity(*config.City, string) (runtime.Provider, error) { panic("fixture") }
func newStatusSessionProviderForCityWithSnapshot(*config.City, string, *sessionBeadSnapshot) (runtime.Provider, error) { panic("fixture") }
`)
	writeExitCensusFixture(t, dir, "other.go", `package main
func sessionProviderOrExit() {}
`)
	file, err := parser.ParseFile(token.NewFileSet(), dir+"/providers.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	violations := sessionProviderFactoryAPIViolations(file)
	retired, err := retiredSessionProviderDeclarationViolations(dir)
	if err != nil {
		t.Fatal(err)
	}
	violations = append(violations, retired...)
	wants := []string{
		`newSessionProvider results = "runtime.Provider", want "runtime.Provider,error"`,
		"other.go:sessionProviderOrExit compatibility declaration still exists",
	}
	for _, want := range wants {
		if !slices.Contains(violations, want) {
			t.Fatalf("violations = %q, want %q", violations, want)
		}
	}
}

func TestRetiredSessionProviderDeclarationCensusRejectsPackageVariables(t *testing.T) {
	tests := map[string]struct {
		name   string
		source string
	}{
		"no initializer": {
			name: "newSessionProviderWithError",
			source: `package main
var newSessionProviderWithError func()
`,
		},
		"function literal": {
			name: "sessionProviderOrExit",
			source: `package main
var sessionProviderOrExit = func() {}
`,
		},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			writeExitCensusFixture(t, dir, "fixture.go", test.source)
			violations, err := retiredSessionProviderDeclarationViolations(dir)
			if err != nil {
				t.Fatal(err)
			}
			want := "fixture.go:" + test.name + " compatibility package variable still exists"
			if !slices.Contains(violations, want) {
				t.Fatalf("violations = %q, want %q", violations, want)
			}
		})
	}
}

func sessionProviderFactoryAPIViolations(file *ast.File) []string {
	declarations := map[string]*ast.FuncDecl{}
	for _, declaration := range file.Decls {
		function, ok := declaration.(*ast.FuncDecl)
		if !ok {
			continue
		}
		declarations[function.Name.Name] = function
	}

	var violations []string
	for name, shape := range canonicalSessionProviderFactories {
		function := declarations[name]
		if function == nil {
			violations = append(violations, name+" is missing")
			continue
		}
		if got := fieldShapes(function.Type.Params); got != shape.parameters {
			violations = append(violations, fmt.Sprintf("%s parameters = %q, want %q", name, got, shape.parameters))
		}
		if got := fieldShapes(function.Type.Results); got != shape.results {
			violations = append(violations, fmt.Sprintf("%s results = %q, want %q", name, got, shape.results))
		}
	}
	slices.Sort(violations)
	return violations
}

func retiredSessionProviderDeclarationViolations(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("read gc source directory %q: %w", dir, err)
	}
	fset := token.NewFileSet()
	var violations []string
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, dir+"/"+name, nil, 0)
		if err != nil {
			return nil, fmt.Errorf("parse gc source %q: %w", name, err)
		}
		for _, declaration := range file.Decls {
			switch typed := declaration.(type) {
			case *ast.FuncDecl:
				if retiredSessionProviderFactoryNames[typed.Name.Name] {
					violations = append(violations, name+":"+typed.Name.Name+" compatibility declaration still exists")
				}
			case *ast.GenDecl:
				for _, spec := range typed.Specs {
					values, ok := spec.(*ast.ValueSpec)
					if !ok {
						continue
					}
					for _, valueName := range values.Names {
						if retiredSessionProviderFactoryNames[valueName.Name] {
							violations = append(violations, name+":"+valueName.Name+" compatibility package variable still exists")
						}
					}
				}
			}
		}
	}
	slices.Sort(violations)
	return violations, nil
}

func TestExitBypassCensusRejectsAliasedAndEscapedReferences(t *testing.T) {
	tests := map[string]string{
		"aliased os import": `package main
import system "os"
func evade() { system.Exit(1) }
`,
		"escaped os exit": `package main
import "os"
var terminate = os.Exit
`,
		"log fatal": `package main
import "log"
func evade() { log.Fatalf("bad") }
`,
		"log logger fatal": `package main
import "log"
var logger = log.Default()
func evade() { logger.Fatal("bad") }
`,
		"escaped log logger fatal": `package main
import "log"
var logger = log.Default()
var terminate = logger.Fatal
`,
		"runtime goexit": `package main
import goruntime "runtime"
func evade() { goruntime.Goexit() }
`,
	}
	for name, source := range tests {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			writeExitCensusFixture(t, dir, "fixture.go", source)
			sites, violations, err := scanGCExitBypasses(dir)
			if err != nil {
				t.Fatal(err)
			}
			if !exitCensusRejectsReference(sites, violations) {
				t.Fatal("exit-bypass census accepted forbidden source")
			}
		})
	}
}

func TestExitBypassCensusRejectsSyscallAndUnixExitVariants(t *testing.T) {
	tests := map[string]string{
		"direct syscall": `package main
import "syscall"
func evade() { syscall.Exit(1) }
`,
		"aliased syscall": `package main
import system "syscall"
func evade() { system.Exit(1) }
`,
		"dot-imported syscall": `package main
import . "syscall"
func evade() { Exit(1) }
`,
		"escaped syscall": `package main
import "syscall"
var terminate = syscall.Exit
`,
		"direct unix": `package main
import "golang.org/x/sys/unix"
func evade() { unix.Exit(1) }
`,
		"aliased unix": `package main
import system "golang.org/x/sys/unix"
func evade() { system.Exit(1) }
`,
		"dot-imported unix": `package main
import . "golang.org/x/sys/unix"
func evade() { Exit(1) }
`,
		"escaped unix": `package main
import "golang.org/x/sys/unix"
var terminate = unix.Exit
`,
	}
	for name, source := range tests {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			writeExitCensusFixture(t, dir, "fixture.go", source)
			sites, violations, err := scanGCExitBypasses(dir)
			if err != nil {
				t.Fatal(err)
			}
			if !exitCensusRejectsReference(sites, violations) {
				t.Fatal("exit-bypass census accepted forbidden source")
			}
		})
	}
}

func TestExitBypassCensusRejectsSupervisorHardExitOutsideReviewedCall(t *testing.T) {
	tests := map[string]struct {
		file   string
		source string
	}{
		"extra direct owner": {file: "fixture.go", source: `package main
func evade() { supervisorHardExit(nil, 1) }
`},
		"moved owner": {file: "cmd_supervisor.go", source: `package main
func renamedSupervisorSignalLoop() { supervisorHardExit(nil, 130) }
`},
		"alias": {file: "fixture.go", source: `package main
var terminate = supervisorHardExit
func evade() { terminate(nil, 1) }
`},
		"callback": {file: "fixture.go", source: `package main
func accept(any) {}
func evade() { accept(supervisorHardExit) }
`},
		"bare reference": {file: "fixture.go", source: `package main
var retained = supervisorHardExit
`},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			writeExitCensusFixture(t, dir, test.file, test.source)
			sites, violations, err := scanGCExitBypasses(dir)
			if err != nil {
				t.Fatal(err)
			}
			if !exitCensusRejectsReference(sites, violations) {
				t.Fatal("exit-bypass census accepted supervisorHardExit reference")
			}
		})
	}
}

func TestExitBypassCensusRequiresReviewedSupervisorControlFlow(t *testing.T) {
	tests := map[string]string{
		"unconditional same owner": `package main
func supervisorSignalLoop() {
	supervisorHardExit(stderr, supervisorHardExitCodeRepeatedShutdown)
	return
}
`,
		"moved within true body": `package main
func supervisorSignalLoop() {
	if requestShutdown(mode, shutdownTrigger{Source: "signal", Signal: sig.String()}) {
		beforeHardExit()
		supervisorHardExit(stderr, supervisorHardExitCodeRepeatedShutdown)
		return
	}
}
`,
		"missing direct return": `package main
func supervisorSignalLoop() {
	if requestShutdown(mode, shutdownTrigger{Source: "signal", Signal: sig.String()}) {
		supervisorHardExit(stderr, supervisorHardExitCodeRepeatedShutdown)
	}
}
`,
	}
	for name, source := range tests {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			writeExitCensusFixture(t, dir, "cmd_supervisor.go", source)
			sites, violations, err := scanGCExitBypasses(dir)
			if err != nil {
				t.Fatal(err)
			}
			if !exitCensusRejectsReference(sites, violations) {
				t.Fatal("exit-bypass census accepted moved supervisorHardExit control flow")
			}
		})
	}
}

func TestExitBypassCensusRequiresReviewedSupervisorAncestry(t *testing.T) {
	tests := map[string]string{
		"nested closure": `package main
func supervisorSignalLoop() {
	for {
		select {
		case sig := <-sigCh:
			func() {
				if requestShutdown(mode, shutdownTrigger{Source: "signal", Signal: sig.String()}) {
					supervisorHardExit(stderr, supervisorHardExitCodeRepeatedShutdown)
					return
				}
			}()
		}
	}
}
`,
		"nested goroutine": `package main
func supervisorSignalLoop() {
	for {
		select {
		case sig := <-sigCh:
			go func() {
				if requestShutdown(mode, shutdownTrigger{Source: "signal", Signal: sig.String()}) {
					supervisorHardExit(stderr, supervisorHardExitCodeRepeatedShutdown)
					return
				}
			}()
		}
	}
}
`,
		"wrong select case": `package main
func supervisorSignalLoop() {
	for {
		select {
		case <-done:
			if requestShutdown(mode, shutdownTrigger{Source: "signal", Signal: sig.String()}) {
				supervisorHardExit(stderr, supervisorHardExitCodeRepeatedShutdown)
				return
			}
		}
	}
}
`,
		"select outside for": `package main
func supervisorSignalLoop() {
	select {
	case sig := <-sigCh:
		if requestShutdown(mode, shutdownTrigger{Source: "signal", Signal: sig.String()}) {
			supervisorHardExit(stderr, supervisorHardExitCodeRepeatedShutdown)
			return
		}
	}
}
`,
	}
	for name, source := range tests {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			writeExitCensusFixture(t, dir, "cmd_supervisor.go", source)
			sites, violations, err := scanGCExitBypasses(dir)
			if err != nil {
				t.Fatal(err)
			}
			if !exitCensusRejectsReference(sites, violations) {
				t.Fatal("exit-bypass census accepted moved supervisor ancestry")
			}
		})
	}
}

func TestExitBypassCensusRejectsNestedSupervisorExitDefinition(t *testing.T) {
	tests := map[string]string{
		"nested closure": `package main
import "os"
var supervisorHardExit = func(stderr io.Writer, code int) {
	func() { os.Exit(code) }()
}
`,
		"nested goroutine": `package main
import "os"
var supervisorHardExit = func(stderr io.Writer, code int) {
	go func() { os.Exit(code) }()
}
`,
	}
	for name, source := range tests {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			writeExitCensusFixture(t, dir, "cmd_supervisor.go", source)
			sites, violations, err := scanGCExitBypasses(dir)
			if err != nil {
				t.Fatal(err)
			}
			if !exitCensusRejectsReference(sites, violations) {
				t.Fatal("exit-bypass census accepted nested os.Exit definition")
			}
		})
	}
}

func TestExitBypassCensusRequiresSupervisorHardExitBreadcrumb(t *testing.T) {
	tests := map[string]string{
		"removed": `package main
import "os"
var supervisorHardExit = func(stderr io.Writer, code int) {
	os.Exit(code)
}
`,
		"changed": `package main
import (
	"fmt"
	"os"
)
var supervisorHardExit = func(stderr io.Writer, code int) {
	fmt.Fprintln(stderr, "gc supervisor: exiting immediately")
	os.Exit(code)
}
`,
		"moved after exit": `package main
import (
	"fmt"
	"os"
)
var supervisorHardExit = func(stderr io.Writer, code int) {
	os.Exit(code)
	fmt.Fprintln(stderr, "gc supervisor: repeated shutdown request received; exiting immediately")
}
`,
	}
	for name, source := range tests {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			writeExitCensusFixture(t, dir, "cmd_supervisor.go", source)
			sites, violations, err := scanGCExitBypasses(dir)
			if err != nil {
				t.Fatal(err)
			}
			if !exitCensusRejectsReference(sites, violations) {
				t.Fatal("exit-bypass census accepted a changed supervisor hard-exit breadcrumb")
			}
		})
	}
}

func TestExitBypassCensusPinsMainAndWatchdogAncestry(t *testing.T) {
	tests := map[string]struct {
		file   string
		source string
	}{
		"main nested closure": {
			file: "main.go",
			source: `package main
import "os"
func main() {
	func() { os.Exit(mainExitCode(os.Args[1:], os.Stdout, os.Stderr)) }()
}
`,
		},
		"main nested goroutine": {
			file: "main.go",
			source: `package main
import "os"
func main() {
	go func() { os.Exit(mainExitCode(os.Args[1:], os.Stdout, os.Stderr)) }()
}
`,
		},
		"main extra statement": {
			file: "main.go",
			source: `package main
import "os"
func main() {
	beforeExit()
	os.Exit(mainExitCode(os.Args[1:], os.Stdout, os.Stderr))
}
`,
		},
		"scope watchdog nested closure": {
			file: "dolt_scope_watchdog.go",
			source: `package main
import "os"
func init() {
	if len(os.Args) < 2 || os.Args[1] != managedDoltScopeWatchdogArg { return }
	func() { os.Exit(runManagedDoltScopeWatchdog(os.Args[2:], os.Stdout, os.Stderr)) }()
}
`,
		},
		"scope watchdog nested goroutine": {
			file: "dolt_scope_watchdog.go",
			source: `package main
import "os"
func init() {
	if len(os.Args) < 2 || os.Args[1] != managedDoltScopeWatchdogArg { return }
	go func() { os.Exit(runManagedDoltScopeWatchdog(os.Args[2:], os.Stdout, os.Stderr)) }()
}
`,
		},
		"test watchdog nested closure": {
			file: "dolt_start_managed.go",
			source: `package main
import "os"
func init() {
	if len(os.Args) < 2 || os.Args[1] != managedDoltTestWatchdogArg { return }
	func() { os.Exit(runManagedDoltTestWatchdog(os.Args[2:], os.Stdout, os.Stderr)) }()
}
`,
		},
		"test watchdog nested goroutine": {
			file: "dolt_start_managed.go",
			source: `package main
import "os"
func init() {
	if len(os.Args) < 2 || os.Args[1] != managedDoltTestWatchdogArg { return }
	go func() { os.Exit(runManagedDoltTestWatchdog(os.Args[2:], os.Stdout, os.Stderr)) }()
}
`,
		},
		"scope watchdog extra statement": {
			file: "dolt_scope_watchdog.go",
			source: `package main
import "os"
func init() {
	if len(os.Args) < 2 || os.Args[1] != managedDoltScopeWatchdogArg { return }
	beforeExit()
	os.Exit(runManagedDoltScopeWatchdog(os.Args[2:], os.Stdout, os.Stderr))
}
`,
		},
		"test watchdog extra statement": {
			file: "dolt_start_managed.go",
			source: `package main
import "os"
func init() {
	if len(os.Args) < 2 || os.Args[1] != managedDoltTestWatchdogArg { return }
	beforeExit()
	os.Exit(runManagedDoltTestWatchdog(os.Args[2:], os.Stdout, os.Stderr))
}
`,
		},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			writeExitCensusFixture(t, dir, test.file, test.source)
			sites, violations, err := scanGCExitBypasses(dir)
			if err != nil {
				t.Fatal(err)
			}
			if !exitCensusRejectsReference(sites, violations) {
				t.Fatal("exit-bypass census accepted moved exit ancestry")
			}
		})
	}
}

func exitCensusRejectsReference(sites []gcExitBypassSite, violations []string) bool {
	if len(violations) != 0 {
		return true
	}
	for _, site := range sites {
		validate, allowed := allowedGCExitBypassSites[site.key()]
		if !allowed || validate(site) != nil {
			return true
		}
	}
	return false
}

func TestExitBypassCensusRequiresPrivateWatchdogGuard(t *testing.T) {
	dir := t.TempDir()
	writeExitCensusFixture(t, dir, "dolt_scope_watchdog.go", `package main
import "os"
func init() {
	os.Exit(runManagedDoltScopeWatchdog(os.Args[2:], os.Stdout, os.Stderr))
}
`)
	sites, violations, err := scanGCExitBypasses(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(violations) != 0 || len(sites) != 1 {
		t.Fatalf("fixture scan = sites %#v, violations %q", sites, violations)
	}
	if err := allowedGCExitBypassSites[sites[0].key()](sites[0]); err == nil {
		t.Fatal("watchdog exit without its private sentinel guard was allowed")
	}
}

func scanGCExitBypasses(dir string) ([]gcExitBypassSite, []string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, nil, fmt.Errorf("read gc source directory %q: %w", dir, err)
	}

	var sites []gcExitBypassSite
	var violations []string
	fset := token.NewFileSet()
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, parseErr := parser.ParseFile(fset, dir+"/"+name, nil, 0)
		if parseErr != nil {
			return nil, nil, fmt.Errorf("parse gc source %q: %w", name, parseErr)
		}
		imports, dotImports, importViolations := exitSensitiveImports(file)
		violations = append(violations, importViolations...)
		for _, declaration := range file.Decls {
			owner, roots := exitCensusDeclarationRoots(declaration)
			for _, root := range roots {
				rootSites, rootViolations := scanExitCensusRoot(name, owner, root, imports, dotImports)
				sites = append(sites, rootSites...)
				violations = append(violations, rootViolations...)
			}
		}
	}
	return sites, violations, nil
}

func exitSensitiveImports(file *ast.File) (map[string]string, map[string]bool, []string) {
	aliases := map[string]string{}
	dotImports := map[string]bool{}
	var violations []string
	for _, spec := range file.Imports {
		path, err := strconv.Unquote(spec.Path.Value)
		if err != nil || (path != "os" && path != "log" && path != "runtime" && path != "syscall" && path != "golang.org/x/sys/unix") {
			continue
		}
		alias := path
		if slash := strings.LastIndexByte(alias, '/'); slash >= 0 {
			alias = alias[slash+1:]
		}
		if spec.Name != nil {
			alias = spec.Name.Name
		}
		switch alias {
		case "_":
			continue
		case ".":
			dotImports[path] = true
			violations = append(violations, fmt.Sprintf("dot import of %q weakens the exit-bypass census", path))
		default:
			aliases[alias] = path
		}
	}
	return aliases, dotImports, violations
}

func exitCensusDeclarationRoots(declaration ast.Decl) (string, []ast.Node) {
	switch typed := declaration.(type) {
	case *ast.FuncDecl:
		if typed.Body == nil {
			return typed.Name.Name, nil
		}
		return typed.Name.Name, []ast.Node{typed}
	case *ast.GenDecl:
		var roots []ast.Node
		for _, spec := range typed.Specs {
			values, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			for index, value := range values.Values {
				owner := "<package>"
				if index < len(values.Names) {
					owner = values.Names[index].Name
				}
				roots = append(roots, &ownedExitCensusRoot{owner: owner, node: value})
			}
		}
		return "", roots
	default:
		return "", nil
	}
}

type ownedExitCensusRoot struct {
	owner string
	node  ast.Node
}

func (r *ownedExitCensusRoot) Pos() token.Pos { return r.node.Pos() }
func (r *ownedExitCensusRoot) End() token.Pos { return r.node.End() }

func scanExitCensusRoot(file, owner string, root ast.Node, imports map[string]string, dotImports map[string]bool) ([]gcExitBypassSite, []string) {
	if owned, ok := root.(*ownedExitCensusRoot); ok {
		owner = owned.owner
		root = owned.node
	}
	directCallees := map[ast.Expr]*ast.CallExpr{}
	ast.Inspect(root, func(node ast.Node) bool {
		if call, ok := node.(*ast.CallExpr); ok {
			directCallees[call.Fun] = call
		}
		return true
	})
	parents := exitCensusParentMap(root)

	var sites []gcExitBypassSite
	var violations []string
	ast.Inspect(root, func(node ast.Node) bool {
		symbol, expression, ok := exitSensitiveReference(node, imports, dotImports)
		if !ok {
			return true
		}
		call := directCallees[expression]
		key := file + ":" + owner + ":" + symbol
		if call == nil {
			if strings.HasPrefix(symbol, "log.Logger.") {
				if want, ok := allowedNonCallFatalReferences[key]; ok && expressionShape([]ast.Expr{expression}) == want {
					return true
				}
			}
			violations = append(violations, key+" is a non-call reference")
			return true
		}
		sites = append(sites, gcExitBypassSite{
			file: file, owner: owner, symbol: symbol, call: call, root: root,
			parent: parents[call], ancestors: exitCensusAncestors(call, parents),
		})
		return true
	})
	return sites, violations
}

func exitCensusParentMap(root ast.Node) map[ast.Node]ast.Node {
	parents := map[ast.Node]ast.Node{}
	var stack []ast.Node
	ast.Inspect(root, func(node ast.Node) bool {
		if node == nil {
			stack = stack[:len(stack)-1]
			return false
		}
		if len(stack) != 0 {
			parents[node] = stack[len(stack)-1]
		}
		stack = append(stack, node)
		return true
	})
	return parents
}

func exitCensusAncestors(node ast.Node, parents map[ast.Node]ast.Node) []ast.Node {
	var ancestors []ast.Node
	for parent := parents[node]; parent != nil; parent = parents[parent] {
		ancestors = append(ancestors, parent)
	}
	return ancestors
}

func hasExactExitAncestors(site gcExitBypassSite, want ...ast.Node) bool {
	if len(site.ancestors) != len(want) {
		return false
	}
	for index := range want {
		if site.ancestors[index] != want[index] {
			return false
		}
	}
	return true
}

const supervisorHardExitBreadcrumb = "gc supervisor: repeated shutdown request received; exiting immediately"

func isSupervisorHardExitBreadcrumb(statement ast.Stmt) bool {
	expression, ok := statement.(*ast.ExprStmt)
	if !ok {
		return false
	}
	call, ok := expression.X.(*ast.CallExpr)
	if !ok || call.Ellipsis.IsValid() || len(call.Args) != 2 {
		return false
	}
	selector, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || selector.Sel.Name != "Fprintln" {
		return false
	}
	qualifier, ok := selector.X.(*ast.Ident)
	if !ok || qualifier.Name != "fmt" || formatNode(call.Args[0]) != "stderr" {
		return false
	}
	message, ok := call.Args[1].(*ast.BasicLit)
	if !ok || message.Kind != token.STRING {
		return false
	}
	value, err := strconv.Unquote(message.Value)
	return err == nil && value == supervisorHardExitBreadcrumb
}

func isSupervisorSignalClause(clause *ast.CommClause) bool {
	if clause == nil {
		return false
	}
	assignment, ok := clause.Comm.(*ast.AssignStmt)
	if !ok || assignment.Tok != token.DEFINE || len(assignment.Lhs) != 1 || len(assignment.Rhs) != 1 {
		return false
	}
	signal, ok := assignment.Lhs[0].(*ast.Ident)
	if !ok || signal.Name != "sig" {
		return false
	}
	receive, ok := assignment.Rhs[0].(*ast.UnaryExpr)
	if !ok || receive.Op != token.ARROW {
		return false
	}
	channel, ok := receive.X.(*ast.Ident)
	return ok && channel.Name == "sigCh"
}

func isReviewedSupervisorShutdownCondition(expression ast.Expr) bool {
	call, ok := expression.(*ast.CallExpr)
	if !ok || len(call.Args) != 2 {
		return false
	}
	callee, ok := call.Fun.(*ast.Ident)
	if !ok || callee.Name != "requestShutdown" || formatNode(call.Args[0]) != "mode" {
		return false
	}
	trigger, ok := call.Args[1].(*ast.CompositeLit)
	if !ok || formatNode(trigger.Type) != "shutdownTrigger" || len(trigger.Elts) != 2 {
		return false
	}
	want := map[string]string{"Source": `"signal"`, "Signal": "sig.String()"}
	for _, element := range trigger.Elts {
		field, ok := element.(*ast.KeyValueExpr)
		if !ok {
			return false
		}
		name, ok := field.Key.(*ast.Ident)
		if !ok || want[name.Name] != formatNode(field.Value) {
			return false
		}
		delete(want, name.Name)
	}
	return len(want) == 0
}

func exitSensitiveReference(node ast.Node, imports map[string]string, dotImports map[string]bool) (string, ast.Expr, bool) {
	if selector, ok := node.(*ast.SelectorExpr); ok {
		identifier, ok := selector.X.(*ast.Ident)
		if ok {
			path := imports[identifier.Name]
			if exitSensitiveSymbol(path, selector.Sel.Name) {
				return path + "." + selector.Sel.Name, selector, true
			}
		}
		// Fatal methods on *log.Logger terminate just like the package-level
		// helpers. Without type checking, conservatively reject this tiny method
		// vocabulary on any receiver in production command code.
		if isFatalName(selector.Sel.Name) {
			return "log.Logger." + selector.Sel.Name, selector, true
		}
		return "", nil, false
	}
	identifier, ok := node.(*ast.Ident)
	if !ok {
		return "", nil, false
	}
	if identifier.Name == "supervisorHardExit" {
		return "supervisorHardExit", identifier, true
	}
	for path := range dotImports {
		if exitSensitiveSymbol(path, identifier.Name) {
			return path + "." + identifier.Name, identifier, true
		}
	}
	return "", nil, false
}

func exitSensitiveSymbol(path, name string) bool {
	switch path {
	case "os":
		return name == "Exit"
	case "log":
		return isFatalName(name)
	case "runtime":
		return name == "Goexit"
	case "syscall", "golang.org/x/sys/unix":
		return name == "Exit"
	default:
		return false
	}
}

func isFatalName(name string) bool {
	return name == "Fatal" || name == "Fatalf" || name == "Fatalln"
}

func validatePrivateWatchdogExit(site gcExitBypassSite, sentinel, wantArgument string) error {
	function, ok := site.root.(*ast.FuncDecl)
	if !ok || function.Name.Name != "init" || function.Body == nil {
		return fmt.Errorf("watchdog exit owner is not init")
	}
	if got := expressionShape(site.call.Args); got != wantArgument {
		return fmt.Errorf("exit argument = %q, want %q", got, wantArgument)
	}
	expression, ok := site.parent.(*ast.ExprStmt)
	if !ok || expression.X != site.call || !hasExactExitAncestors(site, expression, function.Body, function) {
		return fmt.Errorf("watchdog os.Exit is not a direct init-body statement")
	}
	if len(function.Body.List) != 2 || function.Body.List[1] != expression {
		return fmt.Errorf("watchdog init body is not exactly the sentinel guard followed by os.Exit")
	}
	if len(function.Body.List) == 0 || !isPrivateWatchdogGuard(function.Body.List[0], sentinel) {
		return fmt.Errorf("first statement is not the exact %s argv sentinel guard", sentinel)
	}
	return nil
}

func isPrivateWatchdogGuard(statement ast.Stmt, sentinel string) bool {
	guard, ok := statement.(*ast.IfStmt)
	if !ok || guard.Else != nil || len(guard.Body.List) != 1 {
		return false
	}
	returned, ok := guard.Body.List[0].(*ast.ReturnStmt)
	if !ok || len(returned.Results) != 0 {
		return false
	}
	or, ok := guard.Cond.(*ast.BinaryExpr)
	if !ok || or.Op != token.LOR {
		return false
	}
	return isLenOSArgsLessThanTwo(or.X) && isOSArgsSentinelMismatch(or.Y, sentinel)
}

func isLenOSArgsLessThanTwo(expression ast.Expr) bool {
	binary, ok := expression.(*ast.BinaryExpr)
	if !ok || binary.Op != token.LSS || expressionShape([]ast.Expr{binary.Y}) != "2" {
		return false
	}
	call, ok := binary.X.(*ast.CallExpr)
	return ok && expressionShape([]ast.Expr{call.Fun}) == "len" && expressionShape(call.Args) == "os.Args"
}

func isOSArgsSentinelMismatch(expression ast.Expr, sentinel string) bool {
	binary, ok := expression.(*ast.BinaryExpr)
	return ok && binary.Op == token.NEQ && expressionShape([]ast.Expr{binary.X}) == "os.Args[1]" && expressionShape([]ast.Expr{binary.Y}) == sentinel
}

func expressionShape(expressions []ast.Expr) string {
	parts := make([]string, 0, len(expressions))
	for _, expression := range expressions {
		parts = append(parts, formatNode(expression))
	}
	return strings.Join(parts, ", ")
}

func fieldShapes(fields *ast.FieldList) string {
	if fields == nil {
		return ""
	}
	var shapes []string
	for _, field := range fields.List {
		count := len(field.Names)
		if count == 0 {
			count = 1
		}
		for range count {
			shapes = append(shapes, formatNode(field.Type))
		}
	}
	return strings.Join(shapes, ",")
}

func hasNamedParameter(function *ast.FuncType, name string) bool {
	if function == nil || function.Params == nil {
		return false
	}
	for _, field := range function.Params.List {
		for _, parameter := range field.Names {
			if parameter.Name == name {
				return true
			}
		}
	}
	return false
}

func formatNode(node any) string {
	var output strings.Builder
	if err := format.Node(&output, token.NewFileSet(), node); err != nil {
		return "<invalid>"
	}
	return output.String()
}

func writeExitCensusFixture(t *testing.T, dir, name, source string) {
	t.Helper()
	if err := os.WriteFile(dir+"/"+name, []byte(source), 0o600); err != nil {
		t.Fatalf("WriteFile(%q): %v", name, err)
	}
}
