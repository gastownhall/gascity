package config

import (
	"encoding/json"
	"testing"
)

func TestEffectiveWorkQueryPrefersReadyStepOverAssignedGraphWorkflowRoot(t *testing.T) {
	a := Agent{Name: "worker", Dir: "hello-world"}
	out := runEffectiveWorkQuery(t, a, map[string]string{
		"GC_SESSION_NAME":   "worker-session",
		"GC_SESSION_ORIGIN": "ephemeral",
	}, graphWorkflowAnchorFakeBD(true))

	ids := workQueryOutputIDs(t, out)
	if !ids["ready-step"] {
		t.Fatalf("EffectiveWorkQuery() = %q, want routed ready step", out)
	}
	if ids["workflow-root"] {
		t.Fatalf("EffectiveWorkQuery() = %q, must not serve workflow anchor ahead of ready step", out)
	}
	if ids["foreign-workflow-root"] {
		t.Fatalf("EffectiveWorkQuery() = %q, anchored session must not claim another workflow root", out)
	}
	if ids["foreign-ready-step"] {
		t.Fatalf("EffectiveWorkQuery() = %q, anchored session must not hop to another workflow", out)
	}
}

func TestEffectiveWorkQueryKeepsAssignedGraphWorkflowRootAsFallback(t *testing.T) {
	a := Agent{Name: "worker", Dir: "hello-world"}
	out := runEffectiveWorkQuery(t, a, map[string]string{
		"GC_SESSION_NAME":   "worker-session",
		"GC_SESSION_ORIGIN": "ephemeral",
	}, graphWorkflowAnchorFakeBD(false))

	if !workQueryOutputIDs(t, out)["workflow-root"] {
		t.Fatalf("EffectiveWorkQuery() = %q, want workflow anchor fallback", out)
	}
}

func TestEffectiveWorkQueryKeepsOpenGraphWorkflowRootForInitialLaunch(t *testing.T) {
	a := Agent{Name: "worker", Dir: "hello-world"}
	out := runEffectiveWorkQuery(t, a, map[string]string{
		"GC_SESSION_ORIGIN": "ephemeral",
	}, `#!/bin/sh
set -eu
case "$1" in
  list|query)
    printf '[]'
    ;;
  ready)
    case "$*" in
      *"--metadata-field gc.routed_to=hello-world/worker"*)
        printf '[{"id":"launch-root","status":"open","assignee":"","metadata":{"gc.kind":"workflow","gc.formula_contract":"graph.v2","gc.routed_to":"hello-world/worker"}}]'
        ;;
      *)
        printf '[]'
        ;;
    esac
    ;;
  *)
    printf '[]'
    ;;
esac
`)

	if !workQueryOutputIDs(t, out)["launch-root"] {
		t.Fatalf("EffectiveWorkQuery() = %q, want open workflow root for initial launch", out)
	}
}

func TestEffectiveWorkQueryPrefersExecutableDemandOverOpenGraphWorkflowRoot(t *testing.T) {
	a := Agent{Name: "worker", Dir: "hello-world"}
	out := runEffectiveWorkQuery(t, a, map[string]string{
		"GC_SESSION_ORIGIN": "ephemeral",
	}, `#!/bin/sh
set -eu
case "$1" in
  list|query)
    printf '[]'
    ;;
  ready)
    case "$*" in
      *"--metadata-field gc.routed_to=hello-world/worker"*)
        printf '[{"id":"launch-root","status":"open","assignee":"","metadata":{"gc.kind":"workflow","gc.formula_contract":"graph.v2","gc.routed_to":"hello-world/worker"}},{"id":"ready-step","status":"open","assignee":"","metadata":{"gc.kind":"task","gc.root_bead_id":"launch-root","gc.routed_to":"hello-world/worker"}}]'
        ;;
      *)
        printf '[]'
        ;;
    esac
    ;;
  *)
    printf '[]'
    ;;
esac
`)

	ids := workQueryOutputIDOrder(t, out)
	if len(ids) != 2 || ids[0] != "ready-step" || ids[1] != "launch-root" {
		t.Fatalf("EffectiveWorkQuery() candidate order = %v, want executable step before workflow-root fallback", ids)
	}
}

func TestEffectiveWorkQueryKeepsOrdinaryInProgressRecoveryPriority(t *testing.T) {
	a := Agent{Name: "worker", Dir: "hello-world"}
	out := runEffectiveWorkQuery(t, a, map[string]string{
		"GC_SESSION_NAME":   "worker-session",
		"GC_SESSION_ORIGIN": "ephemeral",
	}, `#!/bin/sh
set -eu
case "$1" in
  list)
    printf '[{"id":"in-progress-step","status":"in_progress","assignee":"worker-session","metadata":{"gc.kind":"task","gc.routed_to":"hello-world/worker"}}]'
    ;;
  show)
    printf '[{"id":"in-progress-step","dependencies":[]}]'
    ;;
  ready)
    printf '[{"id":"unrelated-ready-step","status":"open","assignee":"","metadata":{"gc.routed_to":"hello-world/worker"}}]'
    ;;
  query)
    printf '[]'
    ;;
  *)
    printf '[]'
    ;;
esac
`)

	ids := workQueryOutputIDs(t, out)
	if !ids["in-progress-step"] {
		t.Fatalf("EffectiveWorkQuery() = %q, want ordinary in-progress recovery", out)
	}
	if ids["unrelated-ready-step"] {
		t.Fatalf("EffectiveWorkQuery() = %q, ordinary recovery must retain priority", out)
	}
}

func TestEffectiveWorkQueryPrefersOwnedInProgressStepWhenGraphRootSortsFirst(t *testing.T) {
	a := Agent{Name: "worker", Dir: "hello-world"}
	out := runEffectiveWorkQuery(t, a, map[string]string{
		"GC_SESSION_NAME":   "worker-session",
		"GC_SESSION_ORIGIN": "ephemeral",
	}, `#!/bin/sh
set -eu
case "$1" in
  list)
    printf '[{"id":"workflow-root","status":"in_progress","assignee":"worker-session","metadata":{"gc.kind":"workflow","gc.formula_contract":"graph.v2","gc.routed_to":"hello-world/worker"}},{"id":"in-progress-step","status":"in_progress","assignee":"worker-session","metadata":{"gc.kind":"task","gc.root_bead_id":"workflow-root","gc.routed_to":"hello-world/worker"}}]'
    ;;
  show)
    printf '[{"id":"in-progress-step","dependencies":[]}]'
    ;;
  ready)
    printf '[{"id":"unrelated-ready-step","status":"open","assignee":"","metadata":{"gc.routed_to":"hello-world/worker"}}]'
    ;;
  query)
    printf '[]'
    ;;
  *)
    printf '[]'
    ;;
esac
`)

	ids := workQueryOutputIDs(t, out)
	if !ids["in-progress-step"] {
		t.Fatalf("EffectiveWorkQuery() = %q, want owned in-progress step", out)
	}
	if ids["workflow-root"] || ids["unrelated-ready-step"] {
		t.Fatalf("EffectiveWorkQuery() = %q, owned in-progress step must retain priority", out)
	}
}

func graphWorkflowAnchorFakeBD(withReadyStep bool) string {
	ready := `printf '[]'`
	if withReadyStep {
		ready = `case "$*" in
      *"--metadata-field gc.root_bead_id=workflow-root"*)
        printf '[{"id":"ready-step","status":"open","assignee":"","metadata":{"gc.kind":"task","gc.root_bead_id":"workflow-root","gc.routed_to":"hello-world/worker"}}]'
        ;;
      *"--metadata-field gc.routed_to=hello-world/worker"*)
        printf '[{"id":"foreign-workflow-root","status":"open","assignee":"","metadata":{"gc.kind":"workflow","gc.formula_contract":"graph.v2","gc.routed_to":"hello-world/worker"}},{"id":"foreign-ready-step","status":"open","assignee":"","metadata":{"gc.kind":"task","gc.root_bead_id":"foreign-workflow-root","gc.routed_to":"hello-world/worker"}}]'
        ;;
      *)
        printf '[]'
        ;;
    esac`
	}
	return `#!/bin/sh
set -eu
case "$1" in
  list)
    printf '[{"id":"workflow-root","status":"in_progress","assignee":"worker-session","metadata":{"gc.kind":"workflow","gc.formula_contract":"graph.v2","gc.routed_to":"hello-world/worker"}}]'
    ;;
  show)
    printf '[{"id":"workflow-root","dependencies":[]}]'
    ;;
  ready)
    ` + ready + `
    ;;
  query)
    printf '[]'
    ;;
  *)
    printf '[]'
    ;;
esac
`
}

func workQueryOutputIDs(t *testing.T, output string) map[string]bool {
	t.Helper()
	var rows []struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal([]byte(output), &rows); err != nil {
		t.Fatalf("work query output is not a JSON array: %v\noutput: %s", err, output)
	}
	ids := make(map[string]bool, len(rows))
	for _, row := range rows {
		ids[row.ID] = true
	}
	return ids
}

func workQueryOutputIDOrder(t *testing.T, output string) []string {
	t.Helper()
	var rows []struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal([]byte(output), &rows); err != nil {
		t.Fatalf("work query output is not a JSON array: %v\noutput: %s", err, output)
	}
	ids := make([]string, 0, len(rows))
	for _, row := range rows {
		ids = append(ids, row.ID)
	}
	return ids
}
