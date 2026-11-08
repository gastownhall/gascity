package events

import (
	"go/ast"
	"go/parser"
	"go/token"
	"slices"
	"sort"
	"strconv"
	"testing"
)

// TestEveryKnownEventTypeHasAnAuthorship is the authorship sibling of
// TestEveryKnownEventTypeHasRegisteredPayload: a new type fails CI until its
// author class is decided.
func TestEveryKnownEventTypeHasAnAuthorship(t *testing.T) {
	var missing []string
	for _, eventType := range KnownEventTypes {
		if _, ok := RegisteredAuthorship(eventType); !ok {
			missing = append(missing, eventType)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		t.Fatalf("event types with no registered authorship (add them to builtinAuthorship in authorship.go):\n  %v", missing)
	}
}

// TestEveryDeclaredEventTypeHasAnAuthorship covers what KnownEventTypes
// leaves out on purpose (ProviderHealthGateAlert and ReconcilerAlert have no
// typed payload yet): every string constant declared in events.go is an event
// type, and each needs an authorship whether or not it is listed.
func TestEveryDeclaredEventTypeHasAnAuthorship(t *testing.T) {
	declared := declaredEventTypes(t)
	if len(declared) < len(KnownEventTypes) {
		t.Fatalf("parsed %d constants from events.go, fewer than the %d KnownEventTypes; the scan is broken", len(declared), len(KnownEventTypes))
	}
	var missing []string
	for _, eventType := range declared {
		if _, ok := RegisteredAuthorship(eventType); !ok {
			missing = append(missing, eventType)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		t.Fatalf("event-type constants in events.go with no registered authorship:\n  %v", missing)
	}
	// The table holds nothing events.go does not declare.
	for eventType := range builtinAuthorship {
		if !slices.Contains(declared, eventType) {
			t.Errorf("builtinAuthorship lists %q, which events.go does not declare", eventType)
		}
	}
}

// declaredEventTypes returns the value of every string constant in events.go.
func declaredEventTypes(t *testing.T) []string {
	t.Helper()
	file, err := parser.ParseFile(token.NewFileSet(), "events.go", nil, 0)
	if err != nil {
		t.Fatalf("parsing events.go: %v", err)
	}
	var out []string
	for _, decl := range file.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.CONST {
			continue
		}
		for _, spec := range gen.Specs {
			for _, value := range spec.(*ast.ValueSpec).Values {
				lit, ok := value.(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					continue
				}
				s, err := strconv.Unquote(lit.Value)
				if err != nil {
					t.Fatalf("unquoting %s: %v", lit.Value, err)
				}
				out = append(out, s)
			}
		}
	}
	return out
}

// The decisions the design records (DES-BX D13; CD §5) stay as recorded: a
// reclassification is a reviewed edit of this list, not a drive-by.
func TestAuthorshipKeepsTheRecordedDecisions(t *testing.T) {
	want := map[Authorship][]string{
		AuthorLedger: {BeadCreated, BeadUpdated, BeadClosed, BeadDeleted},
		AuthorController: {
			ExecutionStepCompleted, ExecutionStepStalled,
			RequestResultCityCreate, RequestResultCityUnregister, RequestResultSessionCreate,
			RequestResultSessionMessage, RequestResultSessionSubmit, RequestResultRigCreate,
			BeadDeadAssigneeReopened, BeadWorktreeReaped, BeadWorktreeReapSkipped,
			ControllerStarted, ControllerStopped,
			SupervisorStarted, SupervisorShutdownRequested, SupervisorRequest, SupervisorFSPressureSkippedTick,
			CitySuspended, CityResumed, CityCreated, CityUnregisterRequested,
			StorageBindingConverged, StorageBindingGenesis, StorageBindingUnconverged,
			StorageBindingUncheckable, StorageBindingNotConfigured,
			SessionWoke, SessionCrashed, SessionQuarantined, SessionIdleKilled, SessionMaxAgeKilled,
		},
		AuthorWorker: {
			BeadClaimRejected, BeadClaimReleased, HookClaimReclaimedStale,
			ExecutionClaimWindowExpired, ExecutionStepStarted,
			ExecutionWorkAssociated, ExecutionRunAnchored, ExecutionStepDefined,
			MailSent, MailRead, MailArchived, MailMarkedRead, MailMarkedUnread, MailReplied, MailDeleted,
			SessionDraining, SessionUndrained, SessionStopped,
		},
	}
	for class, types := range want {
		for _, eventType := range types {
			if got := AuthorshipOf(eventType); got != class {
				t.Errorf("AuthorshipOf(%q) = %s, want %s", eventType, got, class)
			}
		}
	}
}

func TestUnregisteredTypesAreWorkerAuthored(t *testing.T) {
	for _, eventType := range []string{"custom.box-signal", "pack.deploy_done", "controller.custom_from_a_pack"} {
		if _, ok := RegisteredAuthorship(eventType); ok {
			t.Fatalf("%q is registered", eventType)
		}
		if got := AuthorshipOf(eventType); got != AuthorWorker {
			t.Errorf("AuthorshipOf(%q) = %s, want worker", eventType, got)
		}
	}
}

func TestRegisterAuthorship(t *testing.T) {
	const eventType = "authorship_test.registered"
	RegisterAuthorship(eventType, AuthorController)
	RegisterAuthorship(eventType, AuthorController) // the same class again is a no-op
	if got, ok := RegisteredAuthorship(eventType); !ok || got != AuthorController {
		t.Fatalf("RegisteredAuthorship = %s, %v", got, ok)
	}
	for name, register := range map[string]func(){
		"conflicting class": func() { RegisterAuthorship(eventType, AuthorWorker) },
		"empty type":        func() { RegisterAuthorship("", AuthorWorker) },
		"invalid class":     func() { RegisterAuthorship("authorship_test.invalid", Authorship("box")) },
		"empty class":       func() { RegisterAuthorship("authorship_test.empty", "") },
	} {
		t.Run(name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Fatal("RegisterAuthorship did not panic")
				}
			}()
			register()
		})
	}
	if got := AuthorshipOf(eventType); got != AuthorController {
		t.Fatalf("a refused re-registration changed the class to %s", got)
	}
}

func TestAuthorshipValid(t *testing.T) {
	for _, a := range []Authorship{AuthorWorker, AuthorController, AuthorLedger} {
		if !a.Valid() {
			t.Errorf("%q is not Valid", a)
		}
	}
	for _, a := range []Authorship{"", "box", "Worker"} {
		if a.Valid() {
			t.Errorf("%q is Valid", a)
		}
	}
}

// The execution topology facts are Worker by the BX-4a decision: gc sling,
// gc formula cook and the control-dispatcher session record them through
// executionevent.EmitCurrent at worker sites, and a box that dropped them would
// break the runs view. They only annotate, so a forged copy is low-harm. A
// logged execution.step_completed is read as journal-confirmed, so a forged
// copy would mint a completion: it stays Controller.
func TestExecutionTopologyIsWorkerAndCompletionIsController(t *testing.T) {
	for _, eventType := range []string{ExecutionWorkAssociated, ExecutionRunAnchored, ExecutionStepDefined} {
		if got := AuthorshipOf(eventType); got != AuthorWorker {
			t.Errorf("AuthorshipOf(%q) = %s, want worker", eventType, got)
		}
	}
	if got := AuthorshipOf(ExecutionStepCompleted); got != AuthorController {
		t.Errorf("AuthorshipOf(%q) = %s, want controller", ExecutionStepCompleted, got)
	}
}

// Rule 3: a type an agent-facing command records is Worker, or a worker
// site's recorder would drop it at the source. gc session close records
// session.stranded_claims through the city recorder.
func TestTypesAgentCommandsRecordAreWorkerAuthored(t *testing.T) {
	for _, eventType := range []string{SessionStrandedClaims, SessionStopped, SessionDraining, ControlStalled, ControlCheckUnlaunchable, OrderFailed} {
		if got := AuthorshipOf(eventType); got != AuthorWorker {
			t.Errorf("AuthorshipOf(%q) = %s, want worker", eventType, got)
		}
	}
}
