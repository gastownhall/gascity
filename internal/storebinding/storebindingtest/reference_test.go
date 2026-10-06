package storebindingtest

// The harness against its own reference.
//
// This is the suite's self-check: the canonical Beads adapters over an
// in-memory store are a real conforming implementation, so every assertion
// that is not capability-guarded has to pass against them. A suite that
// cannot pass its own reference is not a contract, and a suite that passes
// everything (see brokenfakes_test.go) is not one either.

import (
	"errors"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/storebinding"
)

func TestReferenceGraphStoreConforms(t *testing.T) {
	RunGraphStoreTests(Wrap(t), GraphSuite{
		NewStore:   ReferenceGraphStore,
		Capability: ReferenceCapability,
	})
}

func TestReferenceSessionsStoreConforms(t *testing.T) {
	RunSessionsStoreTests(Wrap(t), SessionsSuite{
		NewStore:   ReferenceSessionsStore,
		Capability: ReferenceCapability,
	})
}

func TestReferenceOrdersStoreConforms(t *testing.T) {
	RunOrdersStoreTests(Wrap(t), OrdersSuite{
		NewStore:   ReferenceOrdersStore,
		Capability: ReferenceCapability,
	})
}

func TestReferenceNudgeFrontDoorsConform(t *testing.T) {
	RunNudgeFrontDoorTests(Wrap(t), NudgesSuite{
		NewFrontDoors: ReferenceNudgeFrontDoors,
		Capability:    ReferenceCapability,
	})
}

func TestReferenceMessagingFrontDoorsConform(t *testing.T) {
	RunMessagingFrontDoorTests(Wrap(t), MessagingSuite{
		NewFrontDoors: ReferenceMessagingFrontDoors,
		Capability:    ReferenceCapability,
	})
}

// TestReferenceMailProviderConforms runs the exhaustive shared mail.Provider
// table through the Messaging front-door bridge, so the bridge itself is
// exercised rather than merely exported.
func TestReferenceMailProviderConforms(t *testing.T) {
	RunMailProviderTests(t, MessagingSuite{
		NewFrontDoors: ReferenceMessagingFrontDoors,
		Capability:    ReferenceCapability,
	})
}

func TestReferenceWorkTopologyConforms(t *testing.T) {
	RunWorkTopologyTests(Wrap(t), WorkTopologySuite{
		NewTopology:            ReferenceWorkTopology,
		WantPhysicalWorkspaces: 2,
	})
}

// TestUnifiedWorkTopologyComposesOncePerHandle is the unified shape: three
// semantic scopes, two of them sharing one opened database. The topology must
// still report one physical workspace per handle, or the binding migrates the
// shared file twice.
func TestUnifiedWorkTopologyComposesOncePerHandle(t *testing.T) {
	RunWorkTopologyTests(Wrap(t), WorkTopologySuite{
		NewTopology:            unifiedWorkTopology,
		WantPhysicalWorkspaces: 2,
	})
}

func unifiedWorkTopology(tb TB) storebinding.WorkTopology {
	tb.Helper()
	shared := beads.NewMemStore()
	topology, err := storebinding.NewWorkTopology(
		ScopedWorkspace(storebinding.HQScope(), "gc", "hq"),
		[]storebinding.Workspace{
			SharedWorkspace(storebinding.RigScope("alpha"), "ga", "shared", shared),
			SharedWorkspace(storebinding.RigScope("beta"), "gb", "shared", shared),
		},
	)
	if err != nil {
		tb.Fatalf("composing the unified Work topology: %v", err)
	}
	return topology
}

// TestReferenceCloseOwnership runs the close-ownership suite against a handle
// whose close is guarded by sync.Once, the shape every class component uses.
func TestReferenceCloseOwnership(t *testing.T) {
	RunCloseOwnershipTests(Wrap(t), CloseOwnershipSuite{
		NewHandle: func(TB) func() error { return onceCloser() },
	})
}

func onceCloser() func() error {
	closed := false
	return func() error {
		if closed {
			return nil
		}
		closed = true
		return nil
	}
}

// TestRecorderReportsFailuresWithoutFailingTheTest pins the mechanism the
// broken-fake proofs depend on: a Recorder collects a Fatalf, abandons only
// that subtest, and leaves the driving test passing.
func TestRecorderReportsFailuresWithoutFailingTheTest(t *testing.T) {
	recorder := NewRecorder(t.TempDir())
	reached := false
	recorder.Run("first", func(r Runner) {
		r.Fatalf("boom")
		reached = true //nolint:govet // unreachable is the point: Fatalf must abandon the subtest
	})
	recorder.Run("second", func(Runner) {})
	if reached {
		t.Fatal("Fatalf did not abandon the subtest; every suite's Fatalf would keep running against a broken store")
	}
	if got := recorder.FailedAssertions(); len(got) != 1 || got[0] != "first" {
		t.Fatalf("FailedAssertions = %v, want exactly [first]", got)
	}
	if messages := recorder.Messages("first"); len(messages) != 1 || messages[0] != "boom" {
		t.Fatalf("Messages(first) = %v, want [boom]", messages)
	}
	if !recorder.Failed() {
		t.Fatal("a recorder whose subtest failed does not report failed")
	}
}

// TestRecorderSurvivesAPanickingSuite proves a store that panics is recorded
// as a failure rather than taking the whole run down.
func TestRecorderSurvivesAPanickingSuite(t *testing.T) {
	recorder := NewRecorder(t.TempDir())
	recorder.Run("panicky", func(Runner) { panic(errors.New("store exploded")) })
	if got := recorder.FailedAssertions(); len(got) != 1 || got[0] != "panicky" {
		t.Fatalf("FailedAssertions = %v, want exactly [panicky]", got)
	}
}
