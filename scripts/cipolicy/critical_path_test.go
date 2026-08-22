package cipolicy

import (
	"strings"
	"testing"
)

func TestCriticalPathEvidenceJobWiring(t *testing.T) {
	docs := loadPolicyDocuments(t)
	if err := validateCriticalPathEvidenceJob(docs.ci); err != nil {
		t.Fatal(err)
	}
}

func TestCriticalPathEvidenceJobMutationsFailPolicy(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*testing.T, policyDocuments)
	}{
		{
			name: "not required by ci-required",
			mutate: func(t *testing.T, docs policyDocuments) {
				needs := job(t, docs.ci, "ci-required")["needs"].([]any)
				filtered := make([]any, 0, len(needs))
				for _, n := range needs {
					if n != "critical-path-evidence" {
						filtered = append(filtered, n)
					}
				}
				job(t, docs.ci, "ci-required")["needs"] = filtered
			},
		},
		{
			name: "conditional instead of unconditional",
			mutate: func(t *testing.T, docs policyDocuments) {
				job(t, docs.ci, "critical-path-evidence")["if"] = "success()"
			},
		},
		{
			name: "missing a mapped job dependency",
			mutate: func(t *testing.T, docs policyDocuments) {
				needs := job(t, docs.ci, "critical-path-evidence")["needs"].([]any)
				job(t, docs.ci, "critical-path-evidence")["needs"] = needs[:len(needs)-1]
			},
		},
		{
			name: "extra dependency loosens the evidence set",
			mutate: func(t *testing.T, docs policyDocuments) {
				needs := job(t, docs.ci, "critical-path-evidence")["needs"].([]any)
				job(t, docs.ci, "critical-path-evidence")["needs"] = append(needs, "mcp-mail")
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			docs := loadPolicyDocuments(t)
			tt.mutate(t, docs)
			if err := validateCriticalPathEvidenceJob(docs.ci); err == nil {
				t.Fatal("mutation unexpectedly passed critical-path-evidence policy")
			}
		})
	}
}

// TestChangesOutputsPreserveSharedUnionForCriticalPathCategories is a
// regression test: the shared-path union in the `changes` job is exercised
// by the critical-path-evidence gate this bead adds, so a future edit that
// quietly drops the union (or adds it to the deliberately-bare
// openclaw_bridge output) must fail policy even though validateChangesJob
// only checks the filters: input block, not this outputs: block.
func TestChangesOutputsPreserveSharedUnionForCriticalPathCategories(t *testing.T) {
	docs := loadPolicyDocuments(t)
	outputs, ok := job(t, docs.ci, "changes")["outputs"].(map[string]any)
	if !ok {
		t.Fatal("changes job outputs are not a mapping")
	}

	unioned := []string{"cmd_gc_process", "integration", "worker", "worker_phase2", "packs", "docker", "k8s"}
	for _, category := range unioned {
		want := "${{ steps.filter.outputs." + category + " == 'true' || steps.filter.outputs.shared == 'true' }}"
		got, _ := outputs[category].(string)
		if got != want {
			t.Fatalf("changes output %q = %q, want %q (shared-path union)", category, got, want)
		}
	}

	const wantOpenclaw = "${{ steps.filter.outputs.openclaw_bridge }}"
	if got, _ := outputs["openclaw_bridge"].(string); got != wantOpenclaw {
		t.Fatalf(
			"changes output \"openclaw_bridge\" = %q, want %q (deliberately exempt from the shared union)",
			got, wantOpenclaw,
		)
	}
}

func TestCriticalPathEvidenceErrorNamesTheBrokenJob(t *testing.T) {
	docs := loadPolicyDocuments(t)
	needs := job(t, docs.ci, "critical-path-evidence")["needs"].([]any)
	job(t, docs.ci, "critical-path-evidence")["needs"] = needs[:len(needs)-1]

	err := validateCriticalPathEvidenceJob(docs.ci)
	if err == nil || !strings.Contains(err.Error(), "critical-path-evidence") {
		t.Fatalf("error = %v, want critical-path-evidence context", err)
	}
}
