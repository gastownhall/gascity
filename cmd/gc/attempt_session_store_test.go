package main

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/formula"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/session"
)

// TestControlDispatcherRetryRoutesAttemptThroughTheCitySessionStore is AC-4
// at the control-dispatch root: a rig-scoped retry whose next attempt targets
// an on-demand named session binds the canonical session bead held only in the
// city's sessions store, while the attempt and its control stay in the rig
// workflow store (#8436).
func TestControlDispatcherRetryRoutesAttemptThroughTheCitySessionStore(t *testing.T) {
	cityPath := t.TempDir()
	rigPath := filepath.Join(cityPath, "frontend")
	if err := os.MkdirAll(rigPath, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte(`
[workspace]
name = "test-city"

[daemon]
formula_v2 = true

[[rigs]]
name = "frontend"
path = "frontend"

[[agent]]
name = "control-dispatcher"
max_active_sessions = 1

[[agent]]
name = "worker"
max_active_sessions = 1

[[named_session]]
template = "worker"
mode = "on_demand"
`), 0o644); err != nil {
		t.Fatalf("write city.toml: %v", err)
	}
	cfg, _, err := config.LoadWithIncludes(fsys.OSFS{}, filepath.Join(cityPath, "city.toml"))
	if err != nil {
		t.Fatalf("load city: %v", err)
	}
	resolveRigPaths(cityPath, cfg.Rigs)
	spec, ok := session.FindNamedSessionSpec(cfg, cfg.EffectiveCityName(), "worker")
	if !ok {
		t.Fatal("fixture: no worker named session spec")
	}
	sessions := beads.NewMemStoreFrom(5000, nil, nil)
	named, err := sessions.Create(beads.Bead{
		Title:  "worker",
		Type:   session.BeadType,
		Labels: []string{session.LabelSession},
		Metadata: map[string]string{
			"session_name":              spec.SessionName,
			"template":                  spec.Agent.QualifiedName(),
			"configured_named_session":  "true",
			"configured_named_identity": spec.Identity,
			"configured_named_mode":     "on_demand",
			"state":                     "asleep",
			"continuity_eligible":       "true",
		},
	})
	if err != nil {
		t.Fatalf("create named session: %v", err)
	}
	prevOpen := slingOpenCityStore
	slingOpenCityStore = func(path string) (beads.Store, error) {
		if path != cityPath {
			t.Fatalf("slingOpenCityStore(%q), want %q", path, cityPath)
		}
		return sessions, nil
	}
	t.Cleanup(func() { slingOpenCityStore = prevOpen })

	workflow := beads.NewMemStore()
	stepSpec, err := json.Marshal(&formula.Step{
		ID:       "run",
		Title:    "Run",
		Type:     "task",
		Retry:    &formula.RetrySpec{MaxAttempts: 3},
		Children: []*formula.Step{{ID: "summarize", Title: "Summarize", Type: "task", Metadata: map[string]string{"gc.run_target": "worker"}}},
	})
	if err != nil {
		t.Fatalf("marshal step spec: %v", err)
	}
	root := mustCreateBead(t, workflow, beads.Bead{Title: "workflow", Metadata: map[string]string{"gc.kind": "workflow"}})
	control := mustCreateBead(t, workflow, beads.Bead{Title: "run retry", Metadata: map[string]string{
		"gc.kind":             "retry",
		"gc.root_bead_id":     root.ID,
		"gc.step_ref":         "mol-test.run",
		"gc.step_id":          "run",
		"gc.max_attempts":     "3",
		"gc.source_step_spec": string(stepSpec),
		"gc.control_epoch":    "1",
	}})
	attempt := mustCreateBead(t, workflow, beads.Bead{Title: "attempt 1", Metadata: map[string]string{
		"gc.root_bead_id":   root.ID,
		"gc.step_ref":       "mol-test.run.attempt.1",
		"gc.attempt":        "1",
		"gc.outcome":        "fail",
		"gc.failure_class":  "transient",
		"gc.failure_reason": "timeout",
	}})
	if err := workflow.Close(attempt.ID); err != nil {
		t.Fatalf("close attempt 1: %v", err)
	}
	if err := workflow.DepAdd(control.ID, attempt.ID, "blocks"); err != nil {
		t.Fatalf("add retry dependency: %v", err)
	}

	var stderr bytes.Buffer
	if err := runControlDispatcherWithStoreAndConfig(cityPath, rigPath, workflow, control.ID, cfg, io.Discard, &stderr); err != nil {
		t.Fatalf("runControlDispatcherWithStoreAndConfig: %v (stderr: %s)", err, stderr.String())
	}
	all, err := workflow.List(beads.ListQuery{AllowScan: true, TierMode: beads.TierBoth})
	if err != nil {
		t.Fatal(err)
	}
	var work beads.Bead
	for _, b := range all {
		if b.Metadata["gc.step_ref"] == "mol-test.run.attempt.2.summarize" {
			work = b
		}
	}
	if work.ID == "" {
		t.Fatalf("attempt 2 work step not created in the rig workflow store (stderr: %s)", stderr.String())
	}
	if work.Assignee != named.ID {
		t.Errorf("attempt 2 work step assignee = %q (gc.routed_to %q), want the named session's id %q from the city sessions store", work.Assignee, work.Metadata["gc.routed_to"], named.ID)
	}
	if n := countSessionBeads(t, workflow); n != 0 {
		t.Errorf("the rig workflow store holds %d session bead(s), want none", n)
	}
	held, err := sessions.List(beads.ListQuery{AllowScan: true, TierMode: beads.TierBoth})
	if err != nil {
		t.Fatal(err)
	}
	if len(held) != 1 {
		t.Errorf("the city sessions store holds %d bead(s), want only the named session", len(held))
	}
}

func mustCreateBead(t *testing.T, store beads.Store, b beads.Bead) beads.Bead {
	t.Helper()
	created, err := store.Create(b)
	if err != nil {
		t.Fatalf("create %q: %v", b.Title, err)
	}
	return created
}
