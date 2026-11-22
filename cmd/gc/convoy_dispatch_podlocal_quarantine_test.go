package main

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	sessionk8s "github.com/gastownhall/gascity/internal/runtime/k8s"
)

// podHostedDispatcherCity writes the city a pod-hosted control dispatcher
// starts in: a file-backed work store, a k8s session-provider selection, and
// the minimal control-dispatcher agent the dispatch harness expects.
func podHostedDispatcherCity(t *testing.T) string {
	t.Helper()
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
	return cityPath
}

// seedPooledRetryEval seeds the retry graph the recycling sweep works over:
// one failed transient run assigned to a pooled session, its retry-eval
// control bead ready to dispatch. It returns the eval bead.
func seedPooledRetryEval(t *testing.T, cityPath string) beads.Bead {
	t.Helper()
	store, err := openStoreAtForCity(cityPath, cityPath)
	if err != nil {
		t.Fatalf("openStoreAtForCity: %v", err)
	}
	root, err := store.Create(beads.Bead{
		Title: "workflow",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":             "workflow",
			"gc.formula_contract": "graph.v2",
		},
	})
	if err != nil {
		t.Fatalf("Create(root): %v", err)
	}
	logical, err := store.Create(beads.Bead{
		Title: "review",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":         "retry",
			"gc.root_bead_id": root.ID,
			"gc.step_ref":     "demo.review",
			"gc.max_attempts": "3",
			"gc.on_exhausted": "hard_fail",
		},
	})
	if err != nil {
		t.Fatalf("Create(logical): %v", err)
	}
	run1, err := store.Create(beads.Bead{
		Title:    "review attempt 1",
		Type:     "task",
		Assignee: "polecat-2",
		Labels:   []string{"pool:polecat"},
		Metadata: map[string]string{
			"gc.kind":            "retry-run",
			"gc.root_bead_id":    root.ID,
			"gc.step_ref":        "demo.review.run.1",
			"gc.logical_bead_id": logical.ID,
			"gc.attempt":         "1",
			"gc.max_attempts":    "3",
			"gc.on_exhausted":    "hard_fail",
			"gc.outcome":         "fail",
			"gc.failure_class":   "transient",
			"gc.failure_reason":  "rate_limited",
		},
	})
	if err != nil {
		t.Fatalf("Create(run1): %v", err)
	}
	if err := store.Close(run1.ID); err != nil {
		t.Fatalf("Close(run1): %v", err)
	}
	eval1, err := store.Create(beads.Bead{
		Title: "review eval 1",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":            "retry-eval",
			"gc.root_bead_id":    root.ID,
			"gc.step_ref":        "demo.review.eval.1",
			"gc.logical_bead_id": logical.ID,
			"gc.attempt":         "1",
			"gc.max_attempts":    "3",
			"gc.on_exhausted":    "hard_fail",
		},
	})
	if err != nil {
		t.Fatalf("Create(eval1): %v", err)
	}
	if err := store.DepAdd(logical.ID, eval1.ID, "blocks"); err != nil {
		t.Fatalf("DepAdd(logical->eval1): %v", err)
	}
	if err := store.DepAdd(eval1.ID, run1.ID, "blocks"); err != nil {
		t.Fatalf("DepAdd(eval1->run1): %v", err)
	}
	return eval1
}

// TestPodHostedDispatchQuarantinesRefusedPooledRecycle pins the pod-hosted
// recycle disposition end to end: a marked control dispatcher whose pooled
// retry attempts to recycle its subject builds the client-free pod-local
// provider through the production seams, reaches the pod-local stop refusal,
// and quarantines the control bead with that refusal — the recycle is never
// recorded as successful. On the base the same dispatch quarantined with a
// provider-construction failure instead, because a worker pod cannot build
// a Kubernetes client.
func TestPodHostedDispatchQuarantinesRefusedPooledRecycle(t *testing.T) {
	cityPath := podHostedDispatcherCity(t)
	t.Setenv("GC_CITY", cityPath)
	// The pod's own marker: this dispatcher's session provider selection
	// resolves to the client-free pod-local provider, not to a Kubernetes
	// client that fails in a credential-less pod.
	t.Setenv(sessionk8s.PodSessionEnv, "polecat-2")
	t.Setenv("GC_SESSION", "")
	// The pod carries no pod-creation settings to parse.
	for _, key := range []string{
		"GC_K8S_NODE_SELECTOR", "GC_K8S_TOLERATIONS", "GC_K8S_AFFINITY",
		"GC_K8S_PRIORITY_CLASS", "GC_K8S_DOLT_HOST", "GC_K8S_DOLT_PORT",
	} {
		t.Setenv(key, "")
	}

	eval1 := seedPooledRetryEval(t, cityPath)

	var stdout bytes.Buffer
	if err := runControlDispatcher(eval1.ID, &stdout, io.Discard); err != nil {
		t.Fatalf("runControlDispatcher(retry-eval): %v", err)
	}

	reloaded, err := openStoreAtForCity(cityPath, cityPath)
	if err != nil {
		t.Fatalf("openStoreAtForCity(reload): %v", err)
	}
	evalAfter, err := reloaded.Get(eval1.ID)
	if err != nil {
		t.Fatalf("Get(eval1): %v", err)
	}
	if evalAfter.Status != "closed" {
		t.Fatalf("eval1 status after the refused recycle = %q, want closed (quarantined)", evalAfter.Status)
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
	if !strings.Contains(reason, "pod-local runtime stop") {
		t.Fatalf("eval1 %s = %q, want the pod-local stop refusal quoted", beadmeta.ControlQuarantineReasonMetadataKey, reason)
	}
	if !strings.Contains(reason, "owned by the controller") {
		t.Fatalf("eval1 %s = %q, want the controller-owned sentinel present", beadmeta.ControlQuarantineReasonMetadataKey, reason)
	}
	if got := evalAfter.Metadata[beadmeta.RetrySessionRecycledMetadataKey]; got != "" {
		t.Fatalf("eval1 %s = %q, want none: a refused recycle is never recorded as recycled", beadmeta.RetrySessionRecycledMetadataKey, got)
	}
}
