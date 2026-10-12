package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
)

// The reader limit must not let structural roots hide executable work. Model a
// reader that honors --limit, unlike the fixed-response query fixtures.
func TestWorkflowRootAdmissionFiltersBeforeLimit(t *testing.T) {
	a := Agent{Name: "worker"}
	for _, mode := range []string{"child-owned", "ready-sibling", "root-only"} {
		t.Run(mode, func(t *testing.T) {
			script := `#!/bin/sh
set -eu
case "$*" in
  *"gc.routed_to=worker"*) ;;
  *) printf '[]'; exit 0 ;;
esac
limit=0
for arg in "$@"; do
  case "$arg" in --limit=*) limit=${arg#--limit=} ;; esac
done
jq -nc --arg mode "$MODE" --argjson limit "$limit" '
  [range(0; 25) | {id:("root-" + tostring),status:"open",metadata:{"gc.kind":"workflow","gc.formula_contract":"graph.v2","gc.workflow_expanded":"true","gc.routed_to":"worker"}}]
  + (if $mode == "ready-sibling" then [{id:"step",status:"open",metadata:{"gc.routed_to":"worker"}}]
     elif $mode == "root-only" then [{id:"launch",status:"open",metadata:{"gc.kind":"workflow","gc.routed_to":"worker"}}]
     else [] end)
  | if $limit > 0 then .[:$limit] else . end'
`
			env := map[string]string{"MODE": mode}
			out := runEffectiveWorkQuery(t, a, env, script)
			ids := workQueryOutputIDOrder(t, out)
			want := ""
			switch mode {
			case "ready-sibling":
				want = "step"
			case "root-only":
				want = "launch"
			}
			if strings.Join(ids, ",") != want {
				t.Fatalf("work IDs = %v, want %q", ids, want)
			}
			count := strings.TrimSpace(runShellWithFakeBd(t, a.EffectivePoolDemandQuery(), env, script))
			wantCount := "1"
			if mode == "child-owned" {
				wantCount = "0"
			}
			if count != wantCount {
				t.Fatalf("demand = %q, want %s", count, wantCount)
			}
		})
	}
}

func TestWorkflowRootAdmissionRefillsPartiallyExcludedFullWindow(t *testing.T) {
	a := Agent{Name: "worker"}
	for _, tc := range []struct {
		name      string
		tier      string
		roots     string
		work      string
		wantCount int
		wantReads string
	}{
		{"ordinary", "canonical", "0", "1", 1, "20"},
		{"mixed-window", "canonical", "2", "1", 1, "20"},
		{"partially-excluded-full-window", "canonical", "19", "2", 2, "20,0"},
		{"excluded-full-window", "canonical", "25", "30", 20, "20,0"},
		{"legacy-excluded-full-window", "legacy", "25", "30", 1, "20,0"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			log := filepath.Join(t.TempDir(), "reads")
			script := `#!/bin/sh
set -eu
case "$TIER:$*" in
  canonical:*"gc.routed_to=worker"*|legacy:*"gc.run_target=worker"*) ;;
  *) printf '[]'; exit 0 ;;
esac
limit=0
for arg in "$@"; do
  case "$arg" in --limit=*) limit=${arg#--limit=} ;; esac
done
printf '%s\n' "$limit" >> "$READ_LOG"
jq -nc --arg tier "$TIER" --argjson limit "$limit" --argjson roots "$ROOTS" --argjson work "$WORK" '
  [range(0; $roots) | {id:("root-" + tostring),metadata:{"gc.kind":"workflow","gc.workflow_expanded":"true"}}]
  + [range(0; $work) | {id:("work-" + tostring),metadata:{"gc.kind":(if $tier == "legacy" then "workflow" else "task" end)}}]
  | if $limit > 0 then .[:$limit] else . end'
`
			out := runEffectiveWorkQuery(t, a, map[string]string{
				"READ_LOG": log,
				"ROOTS":    tc.roots,
				"TIER":     tc.tier,
				"WORK":     tc.work,
			}, script)
			ids := workQueryOutputIDOrder(t, out)
			if len(ids) != tc.wantCount {
				t.Fatalf("work IDs = %v, want %d rows", ids, tc.wantCount)
			}
			for i, id := range ids {
				if want := "work-" + strconv.Itoa(i); id != want {
					t.Fatalf("work IDs = %v, want ordered prefix ending at work-%d", ids, tc.wantCount-1)
				}
			}
			reads, err := os.ReadFile(log)
			if err != nil {
				t.Fatal(err)
			}
			if got := strings.Join(strings.Fields(string(reads)), ","); got != tc.wantReads {
				t.Fatalf("read limits = %s, want %s", got, tc.wantReads)
			}
		})
	}
}

func TestWorkflowRootAdmissionRefillHandlesWindowLargerThanExecArgument(t *testing.T) {
	a := Agent{Name: "worker"}
	log := filepath.Join(t.TempDir(), "reads")
	script := `#!/bin/sh
set -eu
case "$*" in
  *"gc.routed_to=worker"*) ;;
  *) printf '[]'; exit 0 ;;
esac
limit=0
for arg in "$@"; do
  case "$arg" in --limit=*) limit=${arg#--limit=} ;; esac
done
printf '%s\n' "$limit" >> "$READ_LOG"
jq -nc --arg padding "$PADDING" --argjson limit "$limit" '
  [range(0; 20) | {id:("root-" + tostring),description:$padding,metadata:{"gc.kind":"workflow","gc.workflow_expanded":"true"}}]
  + [{id:"work",metadata:{"gc.kind":"task"}}]
  | if $limit > 0 then .[:$limit] else . end'
`
	out := runEffectiveWorkQuery(t, a, map[string]string{
		"PADDING":  strings.Repeat("x", 7_000),
		"READ_LOG": log,
	}, script)
	if got := strings.Join(workQueryOutputIDOrder(t, out), ","); got != "work" {
		t.Fatalf("work IDs = %s, want work", got)
	}
	reads, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(strings.Fields(string(reads)), ","); got != "20,0" {
		t.Fatalf("read limits = %s, want 20,0", got)
	}
}

func TestWorkflowRootAdmissionMetadataRepresentation(t *testing.T) {
	query := `printf '%s' "$ROWS" | jq -c '` + poolDemandAdmissionJQ() + `'`
	rows := `[{"id":"boolean","metadata":{"gc.kind":"workflow","gc.workflow_expanded":true}},
{"id":"whitespace","metadata":{"gc.kind":" workflow ","gc.workflow_expanded":" true "}},
{"id":"task","metadata":{"gc.kind":"task","gc.workflow_expanded":"true"}},
{"id":"root-only","metadata":{"gc.kind":"workflow"}}]`
	out := runShellWithFakeBd(t, query, map[string]string{"ROWS": rows}, "#!/bin/sh\nexit 99\n")
	if got := strings.Join(workQueryOutputIDOrder(t, out), ","); got != "task,root-only" {
		t.Fatalf("admitted IDs = %s, want task,root-only", got)
	}
}

type admissionParityRow struct {
	ID       string            `json:"id"`
	Metadata map[string]string `json:"metadata"`
}

func TestPoolDemandGeneratedJQAgreesWithGoAdmission(t *testing.T) {
	rows := []admissionParityRow{
		{ID: "task", Metadata: map[string]string{beadmeta.KindMetadataKey: "task"}},
		{ID: "scope", Metadata: map[string]string{beadmeta.KindMetadataKey: beadmeta.KindScope}},
		{ID: "spec", Metadata: map[string]string{beadmeta.KindMetadataKey: beadmeta.KindSpec}},
		{ID: "expanded-root", Metadata: map[string]string{
			beadmeta.KindMetadataKey:             beadmeta.KindWorkflow,
			beadmeta.WorkflowExpandedMetadataKey: "true",
		}},
		{ID: "root-only-native", Metadata: map[string]string{
			beadmeta.KindMetadataKey:                   beadmeta.KindWorkflow,
			beadmeta.NativeStepDependenciesMetadataKey: "[]",
		}},
		{ID: "graph-root-with-children", Metadata: map[string]string{
			beadmeta.KindMetadataKey:                   beadmeta.KindWorkflow,
			beadmeta.NativeStepDependenciesMetadataKey: `["step"]`,
		}},
		{ID: "legacy-run-target-root", Metadata: map[string]string{
			beadmeta.KindMetadataKey:            beadmeta.KindWorkflow,
			beadmeta.FormulaContractMetadataKey: beadmeta.FormulaContractGraphV2,
		}},
		{ID: "routed-unstamped-graph-root", Metadata: map[string]string{
			beadmeta.KindMetadataKey:            beadmeta.KindWorkflow,
			beadmeta.FormulaContractMetadataKey: beadmeta.FormulaContractGraphV2,
			beadmeta.RoutedToMetadataKey:        "worker",
		}},
	}

	assertGeneratedJQMatchesGoAdmission(t, rows)
}

func assertGeneratedJQMatchesGoAdmission(t *testing.T, rows []admissionParityRow) {
	t.Helper()
	encoded, err := json.Marshal(rows)
	if err != nil {
		t.Fatalf("marshal admission corpus: %v", err)
	}
	query := `printf '%s' "$ROWS" | jq -c '` + poolDemandAdmissionJQ() + `'`
	out := runShellWithFakeBd(t, query, map[string]string{"ROWS": string(encoded)}, "#!/bin/sh\nexit 99\n")
	got := workQueryOutputIDOrder(t, out)

	rules := PoolDemandServeRulesForQuery()
	want := make([]string, 0, len(rows))
	for _, row := range rows {
		if rules.AllowsMetadata(row.Metadata) {
			want = append(want, row.ID)
		}
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("generated jq admitted IDs %v, Go admission allowed %v", got, want)
	}
}
