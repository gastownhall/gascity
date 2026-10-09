package main

import (
	"io"
	"slices"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

// These tests pin that every control-ready scan observes the ledger as it is
// at the moment of the scan (ga-vnycm2.9). The serve loop re-scans only after
// it has written (processed a control bead) or after it has waited for another
// writer (idle re-poll, event wake, follow-loop sweep), so a scan answered from
// an earlier snapshot is wrong on both arms: it re-offers the control bead the
// loop just closed, and it hides the control bead a worker's close just made
// ready.

const freshnessControlTarget = "gascity/control-dispatcher"

func freshnessControlAgent() config.Agent {
	return config.Agent{Name: config.ControlDispatcherAgentName, Dir: "gascity"}
}

func createRoutedControl(t *testing.T, store beads.Store, kind string) beads.Bead {
	t.Helper()
	b, err := store.Create(beads.Bead{
		Type: "task",
		Metadata: map[string]string{
			beadmeta.RoutedToMetadataKey: freshnessControlTarget,
			beadmeta.KindMetadataKey:     kind,
		},
	})
	if err != nil {
		t.Fatalf("create %s control bead: %v", kind, err)
	}
	return b
}

func scanControlReadyIDs(t *testing.T, cityDir string) []string {
	t.Helper()
	queue, handled, err := tryControlReadyScan(workflowServeControlReadyQuery(freshnessControlAgent()), cityDir, nil)
	if err != nil {
		t.Fatalf("control-ready scan: %v", err)
	}
	if !handled {
		t.Fatal("control-ready scan: query not recognized as a control-ready query")
	}
	ids := make([]string, 0, len(queue))
	for _, item := range queue {
		ids = append(ids, item.ID)
	}
	return ids
}

// TestControlReadyScanObservesWorkerCloseImmediately is hop-latency cause (1):
// a worker closes the step a retry control is blocked on, and the very next
// scan must offer that control instead of answering from a snapshot taken
// before the close. No bd is on PATH: the scan answers from the leg's own
// Ready read, with no shell fallback for a control whose blocker is closed
// (the read the primed snapshot used to decline and repeat live).
func TestControlReadyScanObservesWorkerCloseImmediately(t *testing.T) {
	cityDir, store := setUpControlReadyFileStoreCity(t)

	step, err := store.Create(beads.Bead{Type: "task", Assignee: "worker"})
	if err != nil {
		t.Fatalf("create step: %v", err)
	}
	retry := createRoutedControl(t, store, beadmeta.KindRetry)
	if err := store.DepAdd(retry.ID, step.ID, "blocks"); err != nil {
		t.Fatalf("block retry on step: %v", err)
	}
	noBDOnPathForTest(t)

	if got := scanControlReadyIDs(t, cityDir); len(got) != 0 {
		t.Fatalf("queue before the step closed = %v, want empty (retry is blocked)", got)
	}

	if err := store.Close(step.ID); err != nil {
		t.Fatalf("close step: %v", err)
	}

	if got, want := scanControlReadyIDs(t, cityDir), []string{retry.ID}; !slices.Equal(got, want) {
		t.Fatalf("queue after the step closed = %v, want %v: the scan answered from a snapshot taken before the worker's close", got, want)
	}
}

// TestControlReadyScanDropsControlClosedSincePreviousScan is the producer side
// of hop-latency cause (2): once the dispatcher closes a control bead, the next
// scan must not offer it again.
func TestControlReadyScanDropsControlClosedSincePreviousScan(t *testing.T) {
	cityDir, store := setUpControlReadyFileStoreCity(t)
	noBDOnPathForTest(t)

	control := createRoutedControl(t, store, beadmeta.KindScopeCheck)
	if got, want := scanControlReadyIDs(t, cityDir), []string{control.ID}; !slices.Equal(got, want) {
		t.Fatalf("first scan = %v, want %v", got, want)
	}
	if err := store.Close(control.ID); err != nil {
		t.Fatalf("close control: %v", err)
	}
	if got := scanControlReadyIDs(t, cityDir); len(got) != 0 {
		t.Fatalf("scan after the control closed = %v, want empty: a closed control bead was re-offered", got)
	}
}

// TestDrainWorkflowServeWorkProcessesEachControlOnceAndSeesItsSuccessors runs
// the real drain loop over the real control-ready scan, the way a graph hop
// does: dispatching a control closes it and mints the next control. The drain
// must process every control exactly once and pick the minted successor up in
// the same pass -- no re-processing of a closed control while a stale snapshot
// ages out, and no successor hidden until a later sweep.
func TestDrainWorkflowServeWorkProcessesEachControlOnceAndSeesItsSuccessors(t *testing.T) {
	cityDir, store := setUpControlReadyFileStoreCity(t)
	noBDOnPathForTest(t)

	retry := createRoutedControl(t, store, beadmeta.KindRetry)
	finalize := createRoutedControl(t, store, beadmeta.KindWorkflowFinalize)

	prevControl := controlDispatcherServe
	prevInterval := workflowServeIdlePollInterval
	prevAttempts := workflowServeIdlePollAttempts
	workflowServeIdlePollInterval = 0
	workflowServeIdlePollAttempts = 0
	t.Cleanup(func() {
		controlDispatcherServe = prevControl
		workflowServeIdlePollInterval = prevInterval
		workflowServeIdlePollAttempts = prevAttempts
	})

	var processed []string
	var scopeCheck beads.Bead
	controlDispatcherServe = func(_, _ string, beadID string, _ io.Writer, _ io.Writer) error {
		processed = append(processed, beadID)
		if len(processed) > 10 {
			t.Fatalf("drain re-processed control beads %v: a closed control bead keeps being re-offered", processed)
		}
		b, err := store.Get(beadID)
		if err != nil {
			return err
		}
		if b.Status != "open" {
			// ProcessControl's bead_not_open skip: a nil return the drain
			// counts as progress.
			return nil
		}
		if beadID == retry.ID {
			scopeCheck = createRoutedControl(t, store, beadmeta.KindScopeCheck)
		}
		return store.Close(beadID)
	}

	result, err := drainWorkflowServeWork(freshnessControlAgent(), cityDir, cityDir, workflowServeControlReadyQuery(freshnessControlAgent()), nil, io.Discard)
	if err != nil {
		t.Fatalf("drainWorkflowServeWork: %v", err)
	}
	if !result.processedAny {
		t.Fatal("drain reported no progress")
	}
	if want := []string{retry.ID, finalize.ID, scopeCheck.ID}; !slices.Equal(processed, want) {
		t.Fatalf("processed = %v, want %v (each control exactly once, the minted successor in the same pass)", processed, want)
	}
}
