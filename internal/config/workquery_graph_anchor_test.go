package config

import (
	"encoding/json"
	"os/exec"
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

// TestEffectiveWorkQueryWorkflowLatchesDoNotShadowLaterTiers pins the shell
// half of the structural-latch rule. Graph routing stamps gc.routed_to on
// every graph.v2 root, so a ready expanded root (and a scope or spec latch)
// can sit in the routed tier of a worker that will never claim it. A routed
// tier that answered with only latches would exit before the later tiers ran:
// the hook drops the latches and drains while the controller still counts the
// shadowed row, and the replacement seat drains the same way.
func TestEffectiveWorkQueryWorkflowLatchesDoNotShadowLaterTiers(t *testing.T) {
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
      *"--metadata-field gc.run_target=hello-world/worker"*)
        printf '[{"id":"legacy-root","status":"open","assignee":"","metadata":{"gc.kind":"workflow","gc.formula_contract":"graph.v2","gc.run_target":"hello-world/worker"}}]'
        ;;
      *"--metadata-field gc.routed_to=hello-world/worker"*)
        printf '[{"id":"expanded-root","status":"open","assignee":"","metadata":{"gc.kind":"workflow","gc.formula_contract":"graph.v2","gc.workflow_expanded":"true","gc.routed_to":"hello-world/worker"}},{"id":"scope-latch","status":"open","assignee":"","metadata":{"gc.kind":"scope","gc.routed_to":"hello-world/worker"}}]'
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
	if len(ids) != 1 || ids[0] != "legacy-root" {
		t.Fatalf("EffectiveWorkQuery() candidates = %v, want only the legacy root-only workflow behind the routed latches", ids)
	}
}

// TestEffectiveWorkQueryLegacyEphemeralSkipsExpandedRoutedWorkflowRoot covers
// the wisp tier, which selects routed rows in jq rather than through bd ready
// and serves only its first row: an older expanded wisp root must not take
// that row from the wisp step routed to the same worker.
func TestEffectiveWorkQueryLegacyEphemeralSkipsExpandedRoutedWorkflowRoot(t *testing.T) {
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("jq not available; legacy ephemeral tier exercises a jq pipeline")
	}
	a := Agent{Name: "worker", Dir: "foundations"}
	ids := workQueryOutputIDs(t, runEffectiveWorkQuery(t, a, nil, `#!/bin/sh
set -eu
case "$1" in
  list|ready)
    printf '[]'
    ;;
  query)
    case "$*" in
      *"ephemeral=true AND status=open"*)
        printf '[{"id":"expanded-wisp-root","issue_type":"task","status":"open","ephemeral":true,"created_at":"2026-05-01T00:00:00Z","metadata":{"gc.kind":"workflow","gc.formula_contract":"graph.v2","gc.workflow_expanded":"true","gc.routed_to":"foundations/worker"}},{"id":"wisp-step","issue_type":"task","status":"open","ephemeral":true,"created_at":"2026-05-01T00:00:01Z","metadata":{"gc.kind":"task","gc.routed_to":"foundations/worker"}}]'
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
`))
	if ids["expanded-wisp-root"] || !ids["wisp-step"] {
		t.Fatalf("EffectiveWorkQuery() ids = %v, want the wisp step and never the expanded wisp root", ids)
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
