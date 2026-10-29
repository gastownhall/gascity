package main

import (
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
)

// The prepared start's drain-source work dir across stores (S3c, site 3).
//
// A pool session started for a prepared drain item resolves its work dir from
// the drain source: the trigger (the item step, graph class) names its root
// (graph class), whose gc.drain_member_id names the source work bead, which
// carries the work dir. buildPreparedStartWithTranscript read all three
// through the sessions store it was handed. On a split city that is the
// binding: the source work bead is on the work ledger, so the read missed and
// the session launched in the template's directory instead of the source's.
// The start path already holds the residency-correct reader for its trigger
// (withTriggerBeadResolver); the work dir now reads through it.
//
// Both topologies; the single-store leg is the legacy control.

func TestPreparedStartResolvesTheDrainSourceWorkDirAcrossStores(t *testing.T) {
	forEachTopology(t, func(t *testing.T, e splitEnv) {
		sourceDir := t.TempDir()
		source, err := e.work.Create(beads.Bead{Title: "drain source", Type: "task", Metadata: map[string]string{
			beadmeta.WorkDirMetadataKey: sourceDir,
		}})
		if err != nil {
			t.Fatal(err)
		}
		graph := e.graphStore()
		root, err := graph.Create(beads.Bead{Title: "drain item root", Type: "task", Metadata: map[string]string{
			beadmeta.KindMetadataKey:          beadmeta.KindWorkflow,
			beadmeta.DrainMemberIDMetadataKey: source.ID,
		}})
		if err != nil {
			t.Fatal(err)
		}
		step, err := graph.Create(beads.Bead{Title: "drain item step", Type: "task", Metadata: map[string]string{
			beadmeta.RootBeadIDMetadataKey: root.ID,
		}})
		if err != nil {
			t.Fatal(err)
		}
		sessions := e.sessionsStore()
		candidate := newTriggerSessionCandidate(t, sessions, step.ID, nil, "")
		topo := residencyTopologyForCity(e.cityPath, e.cfg, e.work, nil)
		resolver := func(id string) (beads.Bead, error) { return byIDBeadForTopology(topo, id) }

		prepared, _, err := buildPreparedStartWithWorkDirResolver(candidate, e.cityPath, e.cfg, sessions, nil, dispatchOptionSources{trigger: resolver})
		if err != nil {
			t.Fatalf("buildPreparedStartWithWorkDirResolver: %v", err)
		}
		if prepared.cfg.WorkDir != sourceDir {
			t.Fatalf("prepared work dir = %q, want the drain source's %q", prepared.cfg.WorkDir, sourceDir)
		}
	})
}
