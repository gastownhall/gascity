package main

import (
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/runtime/guard"
	sessionk8s "github.com/gastownhall/gascity/internal/runtime/k8s"
)

// TestPodHostedDispatcherQuarantinesARefusedPooledRecycle: a control
// dispatcher running inside a generated pod (its session marker set, its city
// selecting k8s) resolves its session provider through the production registry
// to the client-free pod-local provider, which refuses to stop any session. A
// pooled transient retry's recycle of its subject's session reaches that
// refusal through the worker boundary; ProcessControl returns it, and the
// dispatcher's existing error path quarantines the retry-eval control as a hard
// failure with the refusal as its reason. The control never records a recycled
// session. Kills: a kill path that reads the refusal (or the pod's false
// liveness probe of another session) as a session already gone and reports the
// recycle done, and a marked selection that still builds a Kubernetes client.
//
// The control beads are graph class, so the fixture seeds and reads them
// through the city's graph-class store, and the dispatch is the serve drain a
// dispatcher session runs (controlDispatcherServe), which reads the control
// from that store. On a city that relocates nothing that store is the city
// store; under the split-strict gate it is the class view that holds them.
func TestPodHostedDispatcherQuarantinesARefusedPooledRecycle(t *testing.T) {
	cityPath := t.TempDir()
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte(`[workspace]
name = "test-city"

[beads]
provider = "file"

[session]
provider = "k8s"
`), 0o644); err != nil {
		t.Fatalf("WriteFile(city.toml): %v", err)
	}
	writeCatalogFile(t, cityPath, "pack.toml", "[pack]\nname = \"test-city\"\nschema = 2\n")
	writeCatalogFile(t, cityPath, "agents/control-dispatcher/agent.toml", "start_command = \"echo hello\"\n")
	t.Setenv("GC_CITY", cityPath)
	// The dispatcher's pod hosts its own session; the subject's session is
	// another, so the pod-local provider refuses its stop without running tmux.
	t.Setenv(sessionk8s.PodSessionEnv, "dispatcher-pod")
	t.Setenv("GC_SESSION", "")
	// The pod carries no pod-creation settings to parse.
	for _, key := range []string{
		"GC_K8S_NODE_SELECTOR", "GC_K8S_TOLERATIONS", "GC_K8S_AFFINITY",
		"GC_K8S_PRIORITY_CLASS", "GC_K8S_DOLT_HOST", "GC_K8S_DOLT_PORT",
	} {
		t.Setenv(key, "")
	}

	store, err := openStoreAtForCity(cityPath, cityPath)
	if err != nil {
		t.Fatalf("openStoreAtForCity: %v", err)
	}
	graph := cliGraphStore(store, nil, cityPath)
	create := func(b beads.Bead) beads.Bead {
		t.Helper()
		created, err := graph.Create(b)
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
	if err := graph.Close(run1.ID); err != nil {
		t.Fatalf("Close(run1): %v", err)
	}
	eval1 := create(beads.Bead{Title: "review eval 1", Type: "task", Metadata: map[string]string{
		"gc.kind": "retry-eval", "gc.root_bead_id": root.ID, "gc.step_ref": "demo.review.eval.1",
		"gc.logical_bead_id": logical.ID, "gc.attempt": "1", "gc.max_attempts": "3", "gc.on_exhausted": "hard_fail",
	}})
	if err := graph.DepAdd(logical.ID, eval1.ID, "blocks"); err != nil {
		t.Fatalf("DepAdd(logical->eval1): %v", err)
	}
	if err := graph.DepAdd(eval1.ID, run1.ID, "blocks"); err != nil {
		t.Fatalf("DepAdd(eval1->run1): %v", err)
	}

	var stdout, stderr bytes.Buffer
	if err := runControlDispatcherInStore(cityPath, cityPath, eval1.ID, &stdout, &stderr, nil); err != nil {
		t.Fatalf("runControlDispatcherInStore(retry-eval) = %v; want the refusal quarantined, not retried; stderr=%s", err, stderr.String())
	}
	if !strings.Contains(stderr.String(), "control dispatch: quarantined bead="+eval1.ID) {
		t.Fatalf("stderr = %q; want the control quarantined", stderr.String())
	}

	reloaded, err := openStoreAtForCity(cityPath, cityPath)
	if err != nil {
		t.Fatalf("openStoreAtForCity(reload): %v", err)
	}
	evalAfter, err := cliGraphStore(reloaded, nil, cityPath).Get(eval1.ID)
	if err != nil {
		t.Fatalf("Get(eval1): %v", err)
	}
	if got := evalAfter.Metadata[beadmeta.RetrySessionRecycledMetadataKey]; got != "" {
		t.Fatalf("eval1 %s = %q, want none: a refused recycle is never recorded as recycled", beadmeta.RetrySessionRecycledMetadataKey, got)
	}
	if evalAfter.Status != "closed" || evalAfter.Metadata[beadmeta.ControlQuarantinedMetadataKey] != "true" {
		t.Fatalf("eval1 status %q quarantined %q; want closed and quarantined", evalAfter.Status, evalAfter.Metadata[beadmeta.ControlQuarantinedMetadataKey])
	}
	if !slices.Contains(evalAfter.Labels, "gc:control-quarantined") {
		t.Fatalf("eval1 labels = %v, want gc:control-quarantined", evalAfter.Labels)
	}
	if got := evalAfter.Metadata[beadmeta.OutcomeMetadataKey]; got != beadmeta.OutcomeFail {
		t.Fatalf("eval1 %s = %q, want %q", beadmeta.OutcomeMetadataKey, got, beadmeta.OutcomeFail)
	}
	if got := evalAfter.Metadata[beadmeta.FailureClassMetadataKey]; got != beadmeta.FailureClassHard {
		t.Fatalf("eval1 %s = %q, want %q: the stop refusal is not transient", beadmeta.FailureClassMetadataKey, got, beadmeta.FailureClassHard)
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
