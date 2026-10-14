package main

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/dispatch"
	"github.com/gastownhall/gascity/internal/runtime"
)

// TestControlDispatchUnlaunchableCheckStaysOpenEscalatesAndHeals drives a
// [steps.check] control whose check script is missing through the real
// dispatcher disposition (gastownhall/gascity#4239). Before this, the
// resolution error classified TierNone and quarantine closed the step failed,
// which released the step's dependents. It must instead stay open on the
// drift-pending lane, keep its dependents blocked, escalate once, stay quiet
// on verbatim repeats, and pass once the script ships.
func TestControlDispatchUnlaunchableCheckStaysOpenEscalatesAndHeals(t *testing.T) {
	clearGCEnv(t)
	// Escalate on the first refusal so the test does not wait out a budget.
	t.Setenv("GC_CONTROL_SEMANTIC_RETRY_BUDGET", "0s")
	fakeProvider := runtime.NewFake()
	oldProvider := dispatchControlSessionProvider
	dispatchControlSessionProvider = func() (runtime.Provider, error) { return fakeProvider, nil }
	t.Cleanup(func() { dispatchControlSessionProvider = oldProvider })

	cfg := &config.City{Workspace: config.Workspace{Name: "test-city"}}
	cityDir := t.TempDir()
	const checkPath = ".gc/scripts/checks/verify.sh"
	// The checks directory exists and only the script is missing, the shape
	// of a pack that declares a check it never shipped. Pre-creating it also
	// keeps the refusal text stable across sweeps: the dispatcher writes its
	// own .gc state, and a missing ancestor directory would otherwise change
	// which path component the not-exist error names.
	scriptPath := filepath.Join(cityDir, checkPath)
	if err := os.MkdirAll(filepath.Dir(scriptPath), 0o755); err != nil {
		t.Fatalf("mkdir script dir: %v", err)
	}

	store := beads.NewMemStore()
	mustCreateBead := func(b beads.Bead) beads.Bead {
		t.Helper()
		created, err := store.Create(b)
		if err != nil {
			t.Fatalf("create %q: %v", b.Title, err)
		}
		return created
	}
	root := mustCreateBead(beads.Bead{
		Title: "workflow",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":             "workflow",
			"gc.formula_contract": "graph.v2",
		},
	})
	control := mustCreateBead(beads.Bead{
		Title: "verified step",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":                     "ralph",
			"gc.root_bead_id":             root.ID,
			"gc.step_ref":                 "mol-test.verified",
			"gc.step_id":                  "verified",
			"gc.max_attempts":             "1",
			"gc.control_epoch":            "1",
			beadmeta.CheckModeMetadataKey: beadmeta.CheckModeExec,
			beadmeta.CheckPathMetadataKey: checkPath,
		},
	})
	iteration := mustCreateBead(beads.Bead{
		Title: "verified step iteration 1",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":         "scope",
			"gc.root_bead_id": root.ID,
			"gc.step_ref":     "mol-test.verified.iteration.1",
			"gc.attempt":      "1",
		},
	})
	if err := store.Close(iteration.ID); err != nil {
		t.Fatalf("close iteration: %v", err)
	}
	if err := store.DepAdd(control.ID, iteration.ID, "blocks"); err != nil {
		t.Fatalf("dep control->iteration: %v", err)
	}
	dependent := mustCreateBead(beads.Bead{
		Title:    "downstream step",
		Type:     "task",
		Metadata: map[string]string{"gc.root_bead_id": root.ID},
	})
	if err := store.DepAdd(dependent.ID, control.ID, "blocks"); err != nil {
		t.Fatalf("dep dependent->control: %v", err)
	}

	dependentReady := func() bool {
		t.Helper()
		ready, err := store.Ready()
		if err != nil {
			t.Fatalf("Ready: %v", err)
		}
		for _, b := range ready {
			if b.ID == dependent.ID {
				return true
			}
		}
		return false
	}
	sweep := func(label string) (string, error) {
		t.Helper()
		var stderr bytes.Buffer
		err := runControlDispatcherWithStoreAndConfig(cityDir, cityDir, store, control.ID, cfg, io.Discard, &stderr)
		t.Logf("%s stderr: %s", label, strings.TrimSpace(stderr.String()))
		return stderr.String(), err
	}

	// Sweep 1: held open, escalated once, dependent still blocked.
	stderr1, err := sweep("sweep 1")
	if !errors.Is(err, dispatch.ErrControlPending) {
		t.Fatalf("sweep 1 error = %v, want ErrControlPending", err)
	}
	if !strings.Contains(stderr1, "control dispatch: pending stalled bead="+control.ID) {
		t.Fatalf("sweep 1 stderr = %q, want the one-shot pending stall escalation", stderr1)
	}
	if strings.Contains(stderr1, "quarantined bead=") {
		t.Fatalf("sweep 1 stderr = %q, want NO quarantine of an unlaunchable check", stderr1)
	}
	held, err := store.Get(control.ID)
	if err != nil {
		t.Fatalf("get control: %v", err)
	}
	if held.Status != "open" {
		t.Fatalf("control status = %q (outcome %q), want open", held.Status, held.Metadata[beadmeta.OutcomeMetadataKey])
	}
	if got := held.Metadata[beadmeta.ControlPendingReasonMetadataKey]; !strings.Contains(got, "resolving check path") {
		t.Fatalf("%s = %q, want the resolution failure recorded", beadmeta.ControlPendingReasonMetadataKey, got)
	}
	if got := held.Metadata[beadmeta.ControlPendingStalledMetadataKey]; got != "true" {
		t.Fatalf("%s = %q, want \"true\"", beadmeta.ControlPendingStalledMetadataKey, got)
	}
	if got := held.Metadata[beadmeta.ControlQuarantineReasonMetadataKey]; got != "" {
		t.Fatalf("%s = %q, want empty", beadmeta.ControlQuarantineReasonMetadataKey, got)
	}
	if dependentReady() {
		t.Fatal("dependent is ready while its check cannot be launched; the step failed open")
	}
	if r, err := store.Get(root.ID); err != nil || r.Status != "open" {
		t.Fatalf("root status = %q (err %v), want open", r.Status, err)
	}

	// Sweep 2: still pending, quiet, no second escalation.
	stderr2, err := sweep("sweep 2")
	if !errors.Is(err, dispatch.ErrControlPending) {
		t.Fatalf("sweep 2 error = %v, want ErrControlPending", err)
	}
	if !dispatch.IsQuietControllerRetry(err) {
		t.Fatal("sweep 2 verbatim repeat was not marked quiet")
	}
	if strings.Contains(stderr2, "pending stalled bead=") {
		t.Fatalf("sweep 2 stderr = %q, want the escalation latched", stderr2)
	}

	// Ship the script: sweep 3 passes the step and releases the dependent.
	if err := os.WriteFile(scriptPath, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write script: %v", err)
	}
	if _, err := sweep("sweep 3"); err != nil {
		t.Fatalf("sweep 3 error = %v, want nil after the script ships", err)
	}
	healed, err := store.Get(control.ID)
	if err != nil {
		t.Fatalf("get control: %v", err)
	}
	if healed.Status != "closed" || healed.Metadata[beadmeta.OutcomeMetadataKey] != beadmeta.OutcomePass {
		t.Fatalf("control status=%q outcome=%q, want closed pass", healed.Status, healed.Metadata[beadmeta.OutcomeMetadataKey])
	}
	for _, key := range []string{
		beadmeta.ControlPendingReasonMetadataKey,
		beadmeta.ControlPendingCountMetadataKey,
		beadmeta.ControlPendingFirstSeenMetadataKey,
		beadmeta.ControlPendingStalledMetadataKey,
	} {
		if v := healed.Metadata[key]; v != "" {
			t.Errorf("%s = %q after the heal, want cleared", key, v)
		}
	}
	if !dependentReady() {
		t.Fatal("dependent not ready after the check passed")
	}
}
