//go:build acceptance_a

package acceptance_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/fsys"
	storesqlite "github.com/gastownhall/gascity/internal/storebinding/sqlite"
	helpers "github.com/gastownhall/gascity/test/acceptance/helpers"
)

// A sling into relocated graph storage carries the report author exclusion
// onto the workflow root and every step, including synthesized controls.
func TestSlingOnGraphInheritsClaimantExclusions(t *testing.T) {
	env := testEnv.Clone()
	c := helpers.NewCity(t, env)
	c.InitNoStart("claude")
	c.WriteConfig(`[workspace]
name = "review-exclusions"

[daemon]
formula_v2 = true

[storage.classes]
work = "work"
graph = "infra"
sessions = "infra"
messaging = "infra"
orders = "infra"
nudges = "infra"

[storage.bindings.infra]
provider = "sqlite-beads"
path = ".gc/infra"
`)
	c.WriteV2AgentDir("review", `start_command = "true"`,
		`work_query = "gc ready --json --metadata-field gc.routed_to=review"`)
	formulaDir := filepath.Join(c.Dir, "formulas")
	if err := os.MkdirAll(formulaDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(formulaDir, "review-proof.formula.toml"), []byte(`formula = "review-proof"
version = 2
contract = "graph.v2"

[[steps]]
id = "inspect"
title = "Inspect report"

[[steps]]
id = "verdict"
title = "Write verdict"
needs = ["inspect"]
`), 0o644); err != nil {
		t.Fatal(err)
	}
	work, err := beads.OpenFileStore(fsys.OSFS{}, filepath.Join(c.Dir, ".gc", "beads.json"))
	if err != nil {
		t.Fatal(err)
	}
	source, err := work.Create(beads.Bead{Title: "report", Type: "task", Metadata: map[string]string{beadmeta.ExcludeClaimantsMetadataKey: "review.author"}})
	if err != nil {
		t.Fatal(err)
	}
	run := func(args ...string) string {
		t.Helper()
		out, stderr, err := helpers.RunGCStreams(env, c.Dir, args...)
		if err != nil {
			t.Fatalf("gc %v: %v\n%s\n%s", args, err, out, stderr)
		}
		return out
	}
	var launch struct {
		WorkflowID string `json:"workflow_id"`
	}
	if err := json.Unmarshal([]byte(run("sling", "review", source.ID, "--on", "review-proof", "--json")), &launch); err != nil {
		t.Fatal(err)
	}
	if launch.WorkflowID == "" {
		t.Fatal("sling returned no workflow")
	}
	graphPath, err := storesqlite.GraphPath(filepath.Join(c.Dir, ".gc", "infra"))
	if err != nil {
		t.Fatal(err)
	}
	graph, err := beads.OpenSQLiteStore(filepath.Dir(graphPath), beads.WithSQLiteStoreReadOnly())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closer, ok := graph.(interface{ CloseStore() error }); ok {
			if err := closer.CloseStore(); err != nil {
				t.Error(err)
			}
		}
	})
	members, err := graph.ListByMetadata(map[string]string{beadmeta.RootBeadIDMetadataKey: launch.WorkflowID}, 0, beads.WithBothTiers)
	if err != nil {
		t.Fatal(err)
	}
	if len(members) != 3 {
		t.Fatalf("graph has %d steps, want inspect, verdict, finalizer", len(members))
	}
	root, err := graph.Get(launch.WorkflowID)
	if err != nil {
		t.Fatal(err)
	}
	if !beadmeta.ExcludesClaimant(root.Metadata, "review.author") {
		t.Fatalf("workflow root missing author exclusion: %v", root.Metadata)
	}
	for _, step := range members {
		if !beadmeta.ExcludesClaimant(step.Metadata, "review.author") {
			t.Fatalf("step %s missing author exclusion: %v", step.ID, step.Metadata)
		}
	}
}
