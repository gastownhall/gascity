package storebinding

import (
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/coordclass"
	"github.com/gastownhall/gascity/internal/extmsg"
	"github.com/gastownhall/gascity/internal/mail"
)

// TestClassContractOperationsMatchTheContracts proves the operation registry is
// the contract rather than a copy of it. A hand-written list would drift; this
// one is resolved from the interface types, and the assertion below is what
// keeps the resolution honest if somebody replaces it with a literal map.
func TestClassContractOperationsMatchTheContracts(t *testing.T) {
	contracts := map[coordclass.Class][]reflect.Type{
		coordclass.ClassGraph:    {reflect.TypeOf((*GraphStore)(nil)).Elem()},
		coordclass.ClassSessions: {reflect.TypeOf((*SessionsStore)(nil)).Elem()},
		coordclass.ClassOrders:   {reflect.TypeOf((*OrdersStore)(nil)).Elem()},
		coordclass.ClassNudges: {
			reflect.TypeOf((*NudgeQueue)(nil)).Elem(),
			reflect.TypeOf((*NudgeShadows)(nil)).Elem(),
		},
		coordclass.ClassMessaging: {
			reflect.TypeOf((*mail.Provider)(nil)).Elem(),
			reflect.TypeOf((*extmsg.BindingService)(nil)).Elem(),
			reflect.TypeOf((*extmsg.DeliveryContextService)(nil)).Elem(),
			reflect.TypeOf((*extmsg.GroupService)(nil)).Elem(),
			reflect.TypeOf((*extmsg.TranscriptService)(nil)).Elem(),
		},
	}
	for class, types := range contracts {
		want := map[string]bool{}
		for _, contract := range types {
			for index := 0; index < contract.NumMethod(); index++ {
				want[contract.Method(index).Name] = true
			}
		}
		if len(want) == 0 {
			t.Fatalf("class %s has no contract methods; the registry has no subject", class)
		}
		got := ClassContractOperations(class)
		if len(got) != len(want) {
			t.Errorf("class %s registers %d operations, want %d", class, len(got), len(want))
		}
		for _, name := range got {
			if !want[name] {
				t.Errorf("class %s registers operation %q, which no contract declares", class, name)
			}
		}
		for name := range want {
			if !ClassContractHasOperation(class, name) {
				t.Errorf("class %s does not register contract operation %q", class, name)
			}
		}
	}
	if ClassContractHasOperation(coordclass.ClassGraph, "DropDatabase") {
		t.Error("the registry accepts an operation the Graph contract does not have")
	}
}

// TestClassEventValidateRejectsOffContractEvents pins the fail-closed half: an
// event that does not name a real contract operation is not an event.
func TestClassEventValidateRejectsOffContractEvents(t *testing.T) {
	valid := ClassEvent{
		Class:     coordclass.ClassGraph,
		Binding:   "work",
		Kind:      ClassEventWrite,
		Operation: "Create",
		Outcome:   ClassOutcomeOK,
	}
	if err := valid.Validate(); err != nil {
		t.Fatalf("a well-formed event was rejected: %v", err)
	}
	for name, mutate := range map[string]func(ClassEvent) ClassEvent{
		"off-contract operation": func(e ClassEvent) ClassEvent { e.Operation = "Vacuum"; return e },
		"wrong class":            func(e ClassEvent) ClassEvent { e.Class = coordclass.ClassNudges; return e },
		"unknown kind":           func(e ClassEvent) ClassEvent { e.Kind = 0; return e },
		"unknown outcome":        func(e ClassEvent) ClassEvent { e.Outcome = 0; return e },
		"unnamed binding":        func(e ClassEvent) ClassEvent { e.Binding = ""; return e },
	} {
		if err := mutate(valid).Validate(); !errors.Is(err, ErrInvalidClassEvent) {
			t.Errorf("%s: Validate = %v, want ErrInvalidClassEvent", name, err)
		}
	}
}

// TestClassEmitterReportsClaimsWithThreeOutcomes pins the distinction a
// dashboard depends on: a lost compare-and-swap is a conflict, not a failure.
func TestClassEmitterReportsClaimsWithThreeOutcomes(t *testing.T) {
	var seen []ClassEvent
	emitter := classEmitter{
		class:    coordclass.ClassGraph,
		binding:  "work",
		observer: ClassObserverFunc(func(event ClassEvent) { seen = append(seen, event) }),
	}
	emitter.observeClaim("Claim", true, nil)
	emitter.observeClaim("Claim", false, nil)
	emitter.observeClaim("Claim", false, errors.New("store is down"))
	emitter.observeClaim("Claim", true, errors.New("store is down"))

	want := []ClassOutcome{ClassOutcomeOK, ClassOutcomeConflict, ClassOutcomeFailed, ClassOutcomeFailed}
	if len(seen) != len(want) {
		t.Fatalf("the emitter produced %d events, want %d", len(seen), len(want))
	}
	for index, outcome := range want {
		if seen[index].Outcome != outcome {
			t.Errorf("event %d outcome = %s, want %s", index, seen[index].Outcome, outcome)
		}
		if err := seen[index].Validate(); err != nil {
			t.Errorf("event %d is invalid: %v", index, err)
		}
	}
}

// TestClassEmitterWithoutAnObserverIsSilent proves emission is optional and
// never a nil dereference on the hot path.
func TestClassEmitterWithoutAnObserverIsSilent(*testing.T) {
	emitter := classEmitter{class: coordclass.ClassGraph, binding: "work"}
	emitter.observe(ClassEventWrite, "Create", nil)
	emitter.observeClaim("Claim", true, nil)
	emitter.emit(ClassEventCache, "Get", ClassOutcomeHit)
}

// wrapperClasses maps each production wrapper to the class its events report.
var wrapperClasses = map[string]coordclass.Class{
	"wrappedGraphStore":             coordclass.ClassGraph,
	"wrappedSessionsStore":          coordclass.ClassSessions,
	"wrappedOrdersStore":            coordclass.ClassOrders,
	"wrappedNudgeQueue":             coordclass.ClassNudges,
	"wrappedNudgeShadows":           coordclass.ClassNudges,
	"wrappedMailProvider":           coordclass.ClassMessaging,
	"wrappedBindingService":         coordclass.ClassMessaging,
	"wrappedDeliveryContextService": coordclass.ClassMessaging,
	"wrappedGroupService":           coordclass.ClassMessaging,
	"wrappedTranscriptService":      coordclass.ClassMessaging,
}

// emissionOperationArgument names, per emission helper, which argument carries
// the operation name.
var emissionOperationArgument = map[string]int{
	"emit":         1,
	"observe":      1,
	"observeClaim": 0,
	"write":        0,
}

// TestEveryWrapperEmitsItsOwnContractOperation is the structural half of event
// parity.
//
// Parity across providers holds because no provider names an operation: only
// these wrappers do, and each names exactly the contract method it is
// overriding. This census resolves every emission site in the package's syntax
// tree and checks both halves of that claim — the emitted name is a real
// operation of the wrapper's class, and it is the name of the method emitting
// it. A copy-pasted override that kept the wrong operation name is the exact
// defect it catches, and it is the one a runtime parity test cannot see because
// both providers would report the same wrong name.
func TestEveryWrapperEmitsItsOwnContractOperation(t *testing.T) {
	checked := 0
	for receiver, class := range wrapperClasses {
		methods := packageReceiverFuncDecls(t, receiver)
		if len(methods) == 0 {
			t.Errorf("the census found no methods on wrapper %s; it is blind to that class", receiver)
			continue
		}
		for name, function := range methods {
			for _, emitted := range emittedOperations(function) {
				checked++
				if emitted != name {
					t.Errorf("%s.%s emits operation %q; a wrapper must name the contract method it overrides, or two providers report the same call under different names", receiver, name, emitted)
				}
				if !ClassContractHasOperation(class, emitted) {
					t.Errorf("%s.%s emits operation %q, which is not on the %s contract", receiver, name, emitted, class)
				}
			}
		}
	}
	if checked == 0 {
		t.Fatal("the census checked no emission site; every wrapper emission would be unpoliced")
	}
}

// emittedOperations returns the operation-name literals one method passes to
// the emission helpers.
func emittedOperations(function *ast.FuncDecl) []string {
	var operations []string
	ast.Inspect(function.Body, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		selector, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		index, emits := emissionOperationArgument[selector.Sel.Name]
		if !emits || index >= len(call.Args) {
			return true
		}
		literal, ok := call.Args[index].(*ast.BasicLit)
		if !ok || literal.Kind != token.STRING {
			return true
		}
		value, err := strconv.Unquote(literal.Value)
		if err != nil {
			return true
		}
		operations = append(operations, value)
		return true
	})
	return operations
}

// packageReceiverFuncDecls returns the methods declared on one receiver type in
// this package's non-test sources, resolved by receiver TYPE rather than by
// text, so a wrapper cannot escape a census by moving files.
func packageReceiverFuncDecls(t *testing.T, receiverType string) map[string]*ast.FuncDecl {
	t.Helper()
	methods := map[string]*ast.FuncDecl{}
	fileSet := token.NewFileSet()
	// parser.ParseDir is deprecated because it ignores build tags. That is
	// acceptable here and nowhere near worth pulling go/packages in for: this
	// census walks the package's own directory to find method declarations, and
	// internal/storebinding carries no build-tagged files.
	//nolint:staticcheck // SA1019: build tags are irrelevant to this directory walk.
	packages, err := parser.ParseDir(fileSet, ".", func(info fs.FileInfo) bool {
		return !strings.HasSuffix(info.Name(), "_test.go")
	}, 0)
	if err != nil {
		t.Fatalf("parsing the storage package: %v", err)
	}
	for _, parsed := range packages {
		for _, file := range parsed.Files {
			for _, declaration := range file.Decls {
				function, ok := declaration.(*ast.FuncDecl)
				if !ok || function.Recv == nil || len(function.Recv.List) != 1 || function.Body == nil {
					continue
				}
				if receiverTypeName(function.Recv.List[0].Type) != receiverType {
					continue
				}
				methods[function.Name.Name] = function
			}
		}
	}
	return methods
}
