package storebindingtest

// The proof that the suites are load-bearing.
//
// Each case wraps the conforming reference in exactly one defect, drives the
// matching suite with a recording runner, and demands that the suite fail
// EXACTLY the named assertions. Demanding the exact set matters in both
// directions: a defect that fails nothing means the assertion is decorative,
// and a defect that fails everything means the suite cannot tell one fault
// from another and its failures will not locate anything.
//
// The transaction and claim defects are proven in the SQLite consumer instead
// (internal/storebinding/sqlite), against a base that genuinely has those
// capabilities. Proving them here would be circular: the in-memory reference
// has neither, so the fake and the honest store would fail identically.

import (
	"reflect"
	"sort"
	"testing"

	"github.com/gastownhall/gascity/internal/storebinding"
)

func TestGraphSuiteRejectsBrokenGraphStores(t *testing.T) {
	cases := []struct {
		name       string
		defect     GraphDefect
		capability storebinding.ClassCapability
		want       []string
	}{
		{
			name:       "the wrapper alone conforms",
			defect:     GraphDefectNone,
			capability: ReferenceCapability,
		},
		{
			name:       "a store that declares capabilities it does not have",
			defect:     GraphDefectNone,
			capability: storebinding.ClassCapability{Available: true, Transactions: true, Claims: true},
			want:       []string{"ClaimIsCompareAndSwap", "TransactionRollsBackEntirely"},
		},
		{
			name:       "a class handed out while declared unavailable",
			defect:     GraphDefectNone,
			capability: storebinding.ClassCapability{},
			want:       []string{"ClassIsDeclaredAvailable"},
		},
		{
			name:       "a conditional write that ignores the revision",
			defect:     GraphDefectStaleWriteAccepted,
			capability: ReferenceCapability,
			want:       []string{"UpdateIfMatchRejectsStaleRevision"},
		},
		{
			name:       "an absent bead reported as a store fault",
			defect:     GraphDefectNotFoundMisclassified,
			capability: ReferenceCapability,
			want:       []string{"DeleteRemovesTheBead", "GetUnknownIsNotFound"},
		},
		{
			name:       "readiness that lost the dependency projection",
			defect:     GraphDefectReadinessIgnoresDependencies,
			capability: ReferenceCapability,
			want:       []string{"DependenciesGateReadiness"},
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			recorder := NewRecorder(t.TempDir())
			RunGraphStoreTests(recorder, GraphSuite{
				NewStore: func(tb TB) storebinding.GraphStore {
					return NewBrokenGraphStore(ReferenceGraphStore(tb), testCase.defect)
				},
				Capability: testCase.capability,
			})
			assertRejected(t, recorder, testCase.want)
		})
	}
}

// TestSessionsSuiteRejectsAnUnrolledBackTransaction is the load-bearing proof
// for the Sessions transaction assertions. The reference is Beads over
// beads.MemStore, whose Tx is a straight pass-through with no rollback — the
// exact "no-op transaction" shape a contract that grew a Tx has to be able to
// reject. Declaring Transactions over it is therefore not a synthetic fake at
// all: it is an honest store paired with a dishonest capability declaration,
// which is what the suite exists to catch.
func TestSessionsSuiteRejectsAnUnrolledBackTransaction(t *testing.T) {
	cases := []struct {
		name       string
		capability storebinding.ClassCapability
		want       []string
	}{
		{
			name:       "the honest reference conforms",
			capability: ReferenceCapability,
		},
		{
			name:       "a store that declares a transaction it does not roll back",
			capability: storebinding.ClassCapability{Available: true, Transactions: true},
			want:       []string{"TransactionRollsBackEntirely"},
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			recorder := NewRecorder(t.TempDir())
			RunSessionsStoreTests(recorder, SessionsSuite{
				NewStore:   ReferenceSessionsStore,
				Capability: testCase.capability,
			})
			assertRejected(t, recorder, testCase.want)
		})
	}
}

func TestOrdersSuiteRejectsBrokenOrdersStores(t *testing.T) {
	cases := []struct {
		name   string
		defect OrdersDefect
		want   []string
	}{
		{name: "the wrapper alone conforms", defect: OrdersDefectNone},
		{
			name:   "recency reversed",
			defect: OrdersDefectRecentRunsOldestFirst,
			want:   []string{"RecentRunsAreNewestFirst"},
		},
		{
			name:   "a closed run that still holds the single flight",
			defect: OrdersDefectClosedRunStaysOpen,
			want:   []string{"CloseRunEndsTheSingleFlight"},
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			recorder := NewRecorder(t.TempDir())
			RunOrdersStoreTests(recorder, OrdersSuite{
				NewStore: func(tb TB) storebinding.OrdersStore {
					return NewBrokenOrdersStore(ReferenceOrdersStore(tb), testCase.defect)
				},
				Capability: ReferenceCapability,
			})
			assertRejected(t, recorder, testCase.want)
		})
	}
}

func TestNudgesSuiteRejectsBrokenQueues(t *testing.T) {
	cases := []struct {
		name   string
		defect NudgesDefect
		want   []string
	}{
		{name: "the wrapper alone conforms", defect: NudgesDefectNone},
		{
			name:   "a claim that ignores the session fence",
			defect: NudgesDefectClaimIgnoresFence,
			want:   []string{"ClaimDueHonorsTheSessionFence"},
		},
		{
			name:   "a failure report that includes the retried items",
			defect: NudgesDefectRecordFailureReportsEveryItem,
			want:   []string{"RecordFailureReportsOnlyDeadLetters"},
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			recorder := NewRecorder(t.TempDir())
			RunNudgeFrontDoorTests(recorder, NudgesSuite{
				NewFrontDoors: func(tb TB) storebinding.NudgeFrontDoors {
					return NewBrokenNudgeFrontDoors(ReferenceNudgeFrontDoors(tb), testCase.defect)
				},
				Capability: ReferenceCapability,
			})
			assertRejected(t, recorder, testCase.want)
		})
	}
}

func TestMessagingSuiteRejectsBrokenFrontDoors(t *testing.T) {
	cases := []struct {
		name   string
		defect MessagingDefect
		want   []string
	}{
		{name: "the wrapper alone conforms", defect: MessagingDefectNone},
		{
			name:   "an incomplete front-door set",
			defect: MessagingDefectMissingTranscripts,
			want:   []string{"FrontDoorSetArrivesComplete"},
		},
		{
			name:   "an inbox that leaks another agent's mail",
			defect: MessagingDefectInboxIgnoresRecipient,
			want:   []string{"InboxIsolatesRecipients"},
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			recorder := NewRecorder(t.TempDir())
			RunMessagingFrontDoorTests(recorder, MessagingSuite{
				NewFrontDoors: func(tb TB) storebinding.MessagingFrontDoors {
					return NewBrokenMessagingFrontDoors(ReferenceMessagingFrontDoors(tb), testCase.defect)
				},
				Capability: ReferenceCapability,
			})
			assertRejected(t, recorder, testCase.want)
		})
	}
}

func TestCloseOwnershipSuiteRejectsADoubleCloseFailure(t *testing.T) {
	recorder := NewRecorder(t.TempDir())
	RunCloseOwnershipTests(recorder, CloseOwnershipSuite{
		NewHandle: func(TB) func() error { return NewBrokenCloser(CloserDefectSecondCloseFails) },
	})
	assertRejected(t, recorder, []string{"CloseIsIdempotent"})
}

func TestWorkTopologySuiteRejectsAMiscountedComposition(t *testing.T) {
	// The unified topology opens ONE database for two rig scopes. A binding
	// that believes it has three physical workspaces would migrate the shared
	// file twice, so the suite has to reject the wrong count.
	recorder := NewRecorder(t.TempDir())
	RunWorkTopologyTests(recorder, WorkTopologySuite{
		NewTopology:            unifiedWorkTopology,
		WantPhysicalWorkspaces: 3,
	})
	assertRejected(t, recorder, []string{"PhysicalWorkspacesComposeOncePerHandle"})
}

// assertRejected demands the recorded failures are exactly want.
func assertRejected(t *testing.T, recorder *Recorder, want []string) {
	t.Helper()
	got := recorder.FailedAssertions()
	expected := append([]string(nil), want...)
	sort.Strings(expected)
	if len(expected) == 0 {
		if len(got) != 0 {
			t.Fatalf("a conforming store failed %v: %s", got, firstMessages(recorder, got))
		}
		return
	}
	if !reflect.DeepEqual(got, expected) {
		t.Fatalf("the suite failed %v, want exactly %v; recorded: %s", got, expected, firstMessages(recorder, got))
	}
}

func firstMessages(recorder *Recorder, assertions []string) string {
	out := ""
	for _, assertion := range assertions {
		messages := recorder.Messages(assertion)
		if len(messages) == 0 {
			continue
		}
		out += "\n  " + assertion + ": " + messages[0]
	}
	if out == "" {
		return "(no messages)"
	}
	return out
}
