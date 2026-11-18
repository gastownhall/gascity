package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/dispatch"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runtime"
)

// unlaunchableCheckPath is the gc.check_path every fixture below stamps: a
// pack-relative check the pack declares but never shipped.
const unlaunchableCheckPath = ".gc/scripts/checks/verify.sh"

// unlaunchableCheckFixture is a compiled [steps.check] control (kind=ralph,
// exec check) whose script is missing, blocking one downstream step, driven
// through the real control dispatcher. Each sweep is a fresh dispatcher call
// with no carried state, so a sequence of sweeps models a sequence of
// dispatcher passes, restarts included.
type unlaunchableCheckFixture struct {
	cityDir    string
	scriptPath string
	cfg        *config.City
	store      beads.Store
	root       beads.Bead
	control    beads.Bead
	dependent  beads.Bead
}

func newUnlaunchableCheckFixture(t *testing.T) unlaunchableCheckFixture {
	t.Helper()
	clearGCEnv(t)
	fakeProvider := runtime.NewFake()
	oldProvider := dispatchControlSessionProvider
	dispatchControlSessionProvider = func() (runtime.Provider, error) { return fakeProvider, nil }
	t.Cleanup(func() { dispatchControlSessionProvider = oldProvider })

	cityDir := t.TempDir()
	// The checks directory exists and only the script is missing, the shape
	// of a pack that declares a check it never shipped. Pre-creating it also
	// keeps the refusal text stable across sweeps: the dispatcher writes its
	// own .gc state, and a missing ancestor directory would otherwise change
	// which path component the not-exist error names.
	scriptPath := filepath.Join(cityDir, unlaunchableCheckPath)
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
			beadmeta.CheckPathMetadataKey: unlaunchableCheckPath,
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
	return unlaunchableCheckFixture{
		cityDir:    cityDir,
		scriptPath: scriptPath,
		cfg:        &config.City{Workspace: config.Workspace{Name: "test-city"}},
		store:      store,
		root:       root,
		control:    control,
		dependent:  dependent,
	}
}

// sweep runs one control-dispatcher pass over the fixture's control bead and
// returns what it wrote to stderr.
func (f unlaunchableCheckFixture) sweep(t *testing.T, label string) (string, error) {
	t.Helper()
	var stderr bytes.Buffer
	err := runControlDispatcherWithStoreAndConfig(f.cityDir, f.cityDir, f.store, f.control.ID, f.cfg, io.Discard, &stderr)
	t.Logf("%s stderr: %s", label, strings.TrimSpace(stderr.String()))
	return stderr.String(), err
}

func (f unlaunchableCheckFixture) dependentReady(t *testing.T) bool {
	t.Helper()
	ready, err := f.store.Ready()
	if err != nil {
		t.Fatalf("Ready: %v", err)
	}
	for _, b := range ready {
		if b.ID == f.dependent.ID {
			return true
		}
	}
	return false
}

// announcements returns the decoded control.check_unlaunchable payloads the
// dispatcher recorded, in order.
func (f unlaunchableCheckFixture) announcements(t *testing.T) []events.ControlCheckUnlaunchablePayload {
	t.Helper()
	path := filepath.Join(f.cityDir, ".gc", "events.jsonl")
	recorded, err := events.ReadAll(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var out []events.ControlCheckUnlaunchablePayload
	for _, e := range recorded {
		if e.Type != events.ControlCheckUnlaunchable {
			continue
		}
		if e.Subject != f.control.ID {
			t.Fatalf("%s subject = %q, want the control bead %q", e.Type, e.Subject, f.control.ID)
		}
		var payload events.ControlCheckUnlaunchablePayload
		if err := json.Unmarshal(e.Payload, &payload); err != nil {
			t.Fatalf("decode %s payload: %v", e.Type, err)
		}
		out = append(out, payload)
	}
	return out
}

// TestControlDispatchUnlaunchableCheckStaysOpenEscalatesAndHeals drives a
// [steps.check] control whose check script is missing through the real
// dispatcher disposition (gastownhall/gascity#4239). Before this, the
// resolution error classified TierNone and quarantine closed the step failed,
// which released the step's dependents. It must instead stay open on the
// drift-pending lane, keep its dependents blocked, escalate once, stay quiet
// on verbatim repeats, and pass once the script ships.
func TestControlDispatchUnlaunchableCheckStaysOpenEscalatesAndHeals(t *testing.T) {
	f := newUnlaunchableCheckFixture(t)
	// Escalate on the first refusal so the test does not wait out a budget.
	t.Setenv("GC_CONTROL_SEMANTIC_RETRY_BUDGET", "0s")

	// Sweep 1: held open, escalated once, dependent still blocked.
	stderr1, err := f.sweep(t, "sweep 1")
	if !errors.Is(err, dispatch.ErrControlPending) {
		t.Fatalf("sweep 1 error = %v, want ErrControlPending", err)
	}
	if !strings.Contains(stderr1, "control dispatch: pending stalled bead="+f.control.ID) {
		t.Fatalf("sweep 1 stderr = %q, want the one-shot pending stall escalation", stderr1)
	}
	if strings.Contains(stderr1, "quarantined bead=") {
		t.Fatalf("sweep 1 stderr = %q, want NO quarantine of an unlaunchable check", stderr1)
	}
	held, err := f.store.Get(f.control.ID)
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
	if f.dependentReady(t) {
		t.Fatal("dependent is ready while its check cannot be launched; the step failed open")
	}
	if r, err := f.store.Get(f.root.ID); err != nil || r.Status != "open" {
		t.Fatalf("root status = %q (err %v), want open", r.Status, err)
	}

	// Sweep 2: still pending, quiet, no second escalation.
	stderr2, err := f.sweep(t, "sweep 2")
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
	if err := os.WriteFile(f.scriptPath, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write script: %v", err)
	}
	if _, err := f.sweep(t, "sweep 3"); err != nil {
		t.Fatalf("sweep 3 error = %v, want nil after the script ships", err)
	}
	healed, err := f.store.Get(f.control.ID)
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
	if !f.dependentReady(t) {
		t.Fatal("dependent not ready after the check passed")
	}
}

// TestControlDispatchUnlaunchableCheckIsAnnouncedOncePerReason pins the
// operator-visible half of the hold (ga-x5iwx8.2). Holding the step open is
// right, but until the stall horizon elapses (15 minutes by default) the hold
// was silent: the dispatcher re-swept every few seconds and nothing reached
// the event bus, so an operator watching events could not tell a check that
// will never launch from one that is about to pass. The first refusal must be
// announced once, carrying the bead, the stamped check path and the reason; a
// verbatim repeat must not be re-announced (that would be an alarm per sweep);
// a CHANGED reason must be, because it is a new thing for the operator to fix.
func TestControlDispatchUnlaunchableCheckIsAnnouncedOncePerReason(t *testing.T) {
	f := newUnlaunchableCheckFixture(t)
	// Keep the stall horizon far away so everything below comes from the
	// first-refusal announcement, not the control.stalled escalation.
	t.Setenv("GC_CONTROL_SEMANTIC_RETRY_BUDGET", "1h")

	for i := 1; i <= 3; i++ {
		if _, err := f.sweep(t, fmt.Sprintf("missing sweep %d", i)); !errors.Is(err, dispatch.ErrControlPending) {
			t.Fatalf("missing sweep %d error = %v, want ErrControlPending (the step stays held open)", i, err)
		}
	}
	got := f.announcements(t)
	if len(got) != 1 {
		t.Fatalf("%s emitted %d times across 3 sweeps of one missing script, want exactly 1", events.ControlCheckUnlaunchable, len(got))
	}
	first := got[0]
	if first.BeadID != f.control.ID {
		t.Fatalf("payload.bead_id = %q, want %q", first.BeadID, f.control.ID)
	}
	if first.CheckPath != unlaunchableCheckPath {
		t.Fatalf("payload.check_path = %q, want the stamped gc.check_path %q", first.CheckPath, unlaunchableCheckPath)
	}
	if first.Kind != "ralph" || first.RootBeadID != f.root.ID || first.StorePath != f.cityDir {
		t.Fatalf("payload kind=%q root_bead_id=%q store_path=%q, want ralph, %q, %q",
			first.Kind, first.RootBeadID, first.StorePath, f.root.ID, f.cityDir)
	}
	if !strings.Contains(first.Error, "resolving check path") || !strings.Contains(first.Error, "no such file or directory") {
		t.Fatalf("payload.error = %q, want the missing-script resolution failure", first.Error)
	}
	if held := mustGetBead(t, f.store, f.control.ID); held.Status != "open" {
		t.Fatalf("control status = %q, want open: announcing must not change the hold", held.Status)
	}
	if f.dependentReady(t) {
		t.Fatal("dependent is ready while its check cannot be launched; the step failed open")
	}

	// The script ships without its execute bit: a different reason, so a new
	// announcement, and again only one across repeated sweeps.
	if err := os.WriteFile(f.scriptPath, []byte("#!/bin/sh\nexit 0\n"), 0o644); err != nil {
		t.Fatalf("write script: %v", err)
	}
	for i := 1; i <= 2; i++ {
		if _, err := f.sweep(t, fmt.Sprintf("not-executable sweep %d", i)); !errors.Is(err, dispatch.ErrControlPending) {
			t.Fatalf("not-executable sweep %d error = %v, want ErrControlPending", i, err)
		}
	}
	got = f.announcements(t)
	if len(got) != 2 {
		t.Fatalf("%s emitted %d times after the reason changed, want 2 (one per distinct reason)", events.ControlCheckUnlaunchable, len(got))
	}
	if got[1].CheckPath != unlaunchableCheckPath || !strings.Contains(got[1].Error, "not executable") {
		t.Fatalf("second announcement check_path=%q error=%q, want the not-executable refusal for %q",
			got[1].CheckPath, got[1].Error, unlaunchableCheckPath)
	}

	// chmod heals it: the step passes and nothing further is announced.
	if err := os.Chmod(f.scriptPath, 0o755); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	if _, err := f.sweep(t, "healed sweep"); err != nil {
		t.Fatalf("healed sweep error = %v, want nil once the script can launch", err)
	}
	if got := f.announcements(t); len(got) != 2 {
		t.Fatalf("%s emitted %d times after the heal, want still 2", events.ControlCheckUnlaunchable, len(got))
	}

	recorded, err := events.ReadAll(filepath.Join(f.cityDir, ".gc", "events.jsonl"))
	if err != nil {
		t.Fatalf("read events: %v", err)
	}
	if n := countEventsOfType(recorded, events.ControlStalled); n != 0 {
		t.Fatalf("control.stalled emitted %d times inside the stall budget, want 0", n)
	}
}
