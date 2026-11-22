package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/runtime/guard"
	sessionk8s "github.com/gastownhall/gascity/internal/runtime/k8s"
)

// TestPodHostedDispatcherQuarantinesARefusedPooledRecycle: a control
// dispatcher running inside a generated pod serves its session provider
// through the pod-local provider, which refuses to stop any session. A pooled
// transient retry's recycle of its subject's session reaches that refusal
// through the worker boundary; ProcessControl returns it, and the dispatcher's
// existing error path quarantines the retry-eval control with the refusal as
// its reason. The control never records a recycled session. Kills: a kill path
// that reads the refusal (or the pod's false liveness probe of another
// session) as a session already gone and reports the recycle done.
func TestPodHostedDispatcherQuarantinesARefusedPooledRecycle(t *testing.T) {
	cityPath := t.TempDir()
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte(`[workspace]
name = "test-city"

[beads]
provider = "file"
`), 0o644); err != nil {
		t.Fatalf("WriteFile(city.toml): %v", err)
	}
	writeCatalogFile(t, cityPath, "pack.toml", "[pack]\nname = \"test-city\"\nschema = 2\n")
	writeCatalogFile(t, cityPath, "agents/control-dispatcher/agent.toml", "start_command = \"echo hello\"\n")
	t.Setenv("GC_CITY", cityPath)

	store, err := openStoreAtForCity(cityPath, cityPath)
	if err != nil {
		t.Fatalf("openStoreAtForCity: %v", err)
	}
	create := func(b beads.Bead) beads.Bead {
		t.Helper()
		created, err := store.Create(b)
		if err != nil {
			t.Fatalf("Create(%s): %v", b.Title, err)
		}
		return created
	}
	root := create(beads.Bead{Title: "workflow", Type: "task", Metadata: map[string]string{
		"gc.kind": "workflow", "gc.formula_contract": "graph.v2",
	}})
	logical := create(beads.Bead{Title: "review", Type: "task", Metadata: map[string]string{
		"gc.kind": "retry", "gc.root_bead_id": root.ID, "gc.step_ref": "demo.review",
		"gc.max_attempts": "3", "gc.on_exhausted": "hard_fail",
	}})
	run1 := create(beads.Bead{Title: "review attempt 1", Type: "task", Assignee: "worker-2", Labels: []string{"pool:worker"}, Metadata: map[string]string{
		"gc.kind": "retry-run", "gc.root_bead_id": root.ID, "gc.step_ref": "demo.review.run.1",
		"gc.logical_bead_id": logical.ID, "gc.attempt": "1", "gc.max_attempts": "3", "gc.on_exhausted": "hard_fail",
		"gc.outcome": "fail", "gc.failure_class": "transient", "gc.failure_reason": "rate_limited",
	}})
	if err := store.Close(run1.ID); err != nil {
		t.Fatalf("Close(run1): %v", err)
	}
	eval1 := create(beads.Bead{Title: "review eval 1", Type: "task", Metadata: map[string]string{
		"gc.kind": "retry-eval", "gc.root_bead_id": root.ID, "gc.step_ref": "demo.review.eval.1",
		"gc.logical_bead_id": logical.ID, "gc.attempt": "1", "gc.max_attempts": "3", "gc.on_exhausted": "hard_fail",
	}})
	if err := store.DepAdd(logical.ID, eval1.ID, "blocks"); err != nil {
		t.Fatalf("DepAdd(logical->eval1): %v", err)
	}
	if err := store.DepAdd(eval1.ID, run1.ID, "blocks"); err != nil {
		t.Fatalf("DepAdd(eval1->run1): %v", err)
	}

	// The dispatcher's pod hosts its own session; the subject's session is
	// another, so the pod-local provider refuses its stop without running tmux.
	podLocal, err := sessionk8s.NewPodLocal("dispatcher-pod")
	if err != nil {
		t.Fatalf("NewPodLocal: %v", err)
	}
	oldProvider := dispatchControlSessionProvider
	dispatchControlSessionProvider = func() (runtime.Provider, error) { return podLocal, nil }
	t.Cleanup(func() { dispatchControlSessionProvider = oldProvider })

	var stdout, stderr bytes.Buffer
	if err := runControlDispatcher(eval1.ID, &stdout, &stderr); err != nil {
		t.Fatalf("runControlDispatcher(retry-eval) = %v; want the refusal quarantined, not retried; stderr=%s", err, stderr.String())
	}
	if !strings.Contains(stderr.String(), "control dispatch: quarantined bead="+eval1.ID) {
		t.Fatalf("stderr = %q; want the control quarantined", stderr.String())
	}

	reloaded, err := openStoreAtForCity(cityPath, cityPath)
	if err != nil {
		t.Fatalf("openStoreAtForCity(reload): %v", err)
	}
	evalAfter, err := reloaded.Get(eval1.ID)
	if err != nil {
		t.Fatalf("Get(eval1): %v", err)
	}
	if evalAfter.Metadata["gc.retry_session_recycled"] == "true" {
		t.Fatal("eval1 records a recycled session after the pod-local provider refused the stop")
	}
	if evalAfter.Status != "closed" || evalAfter.Metadata[beadmeta.ControlQuarantinedMetadataKey] != "true" {
		t.Fatalf("eval1 status %q quarantined %q; want closed and quarantined", evalAfter.Status, evalAfter.Metadata[beadmeta.ControlQuarantinedMetadataKey])
	}
	reason := evalAfter.Metadata[beadmeta.ControlQuarantineReasonMetadataKey]
	for _, want := range []string{runtime.ErrStopRefused.Error(), guard.ErrControllerOwnedRuntime.Error(), "worker-2"} {
		if !strings.Contains(reason, want) {
			t.Fatalf("quarantine reason %q; want it to carry the pod-local refusal (%q)", reason, want)
		}
	}
	if strings.Contains(stdout.String(), "action=") {
		t.Fatalf("stdout = %q; want no reported control action", stdout.String())
	}
}
