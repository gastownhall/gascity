package main

import (
	"strings"
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
// The v2 start effect (prepareRow, launchWorkDir) holds the same reader over
// its read-only city work legs (legsTriggerResolver), and reads the trigger's
// opt_* pins through it too.
//
// Both topologies; the single-store leg is the legacy control.

// drainItemFixture seeds a drain source work bead (in source) carrying a
// work dir, a drain item root and its step in the graph store, and a pool
// session in the sessions store whose trigger is the step.
func drainItemFixture(t *testing.T, e splitEnv, source beads.Store) (startCandidate, string) {
	t.Helper()
	sourceDir := t.TempDir()
	src, err := source.Create(beads.Bead{Title: "drain source", Type: "task", Metadata: map[string]string{
		beadmeta.WorkDirMetadataKey: sourceDir,
	}})
	if err != nil {
		t.Fatal(err)
	}
	graph := e.graphStore()
	root, err := graph.Create(beads.Bead{Title: "drain item root", Type: "task", Metadata: map[string]string{
		beadmeta.KindMetadataKey:          beadmeta.KindWorkflow,
		beadmeta.DrainMemberIDMetadataKey: src.ID,
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
	return newTriggerSessionCandidate(t, e.sessionsStore(), step.ID, nil, ""), sourceDir
}

// legacyTriggerResolver is the legacy start's residency trigger reader
// (CityRuntime.newWarmClaimTriggerResolver) over e's topology.
func legacyTriggerResolver(e splitEnv) warmClaimTriggerResolver {
	topo := residencyTopologyForCity(e.cityPath, e.cfg, e.work, e.rigStores)
	return func(id string) (beads.Bead, error) { return byIDBeadForTopology(topo, id) }
}

// The legacy reconciler's start.
func TestPreparedStartResolvesTheDrainSourceWorkDirAcrossStores(t *testing.T) {
	forEachTopology(t, func(t *testing.T, e splitEnv) {
		candidate, sourceDir := drainItemFixture(t, e, e.work)
		prepared, _, err := buildPreparedStartWithWorkDirResolver(candidate, e.cityPath, e.cfg, e.sessionsStore(), nil, dispatchOptionSources{trigger: legacyTriggerResolver(e)})
		if err != nil {
			t.Fatalf("buildPreparedStartWithWorkDirResolver: %v", err)
		}
		if prepared.cfg.WorkDir != sourceDir {
			t.Fatalf("prepared work dir = %q, want the drain source's %q", prepared.cfg.WorkDir, sourceDir)
		}
	})
}

// The v2 reconciler's start effect (prepareRow) and its transcript probe's
// work dir (launchWorkDir): they read through the effect's read-only stores,
// the sessions store and the city work legs, with no trigger resolver of
// their own.
func TestV2PrepareRowResolvesTheDrainSourceWorkDirAcrossStores(t *testing.T) {
	forEachTopology(t, func(t *testing.T, e splitEnv) {
		candidate, sourceDir := drainItemFixture(t, e, e.work)
		r := effectReads{legs: testWorkLegs(e.cityPath, e.cfg, e.work, nil).readOnly(), city: e.sessionsStore(), cityPath: e.cityPath}
		w := &World{CityPath: e.cityPath, Env: &reconcileEnv{Cfg: e.cfg}}
		prepared, err := prepareRow(r, w, candidate.info, candidate.tp, sessTranscriptUnknown)
		if err != nil {
			t.Fatalf("prepareRow: %v", err)
		}
		if prepared.cfg.WorkDir != sourceDir {
			t.Fatalf("v2 prepared work dir = %q, want the drain source's %q", prepared.cfg.WorkDir, sourceDir)
		}
		if got := launchWorkDir(r, w, candidate.info, candidate.tp); got != sourceDir {
			t.Fatalf("v2 launch work dir = %q, want the drain source's %q", got, sourceDir)
		}
	})
}

// A drain source in a rig store. The legacy start read it through the session
// store, which on a single-store city is the city store, so a rig-resident
// source was missed there too; the residency reader takes the rig leg. This is
// an intentional behavior change on single-store cities with rigs.
func TestPreparedStartResolvesARigDrainSourceWorkDir(t *testing.T) {
	forEachTopologyWithRig(t, func(t *testing.T, e splitEnv) {
		candidate, sourceDir := drainItemFixture(t, e, e.rig)
		prepared, _, err := buildPreparedStartWithWorkDirResolver(candidate, e.cityPath, e.cfg, e.sessionsStore(), nil, dispatchOptionSources{trigger: legacyTriggerResolver(e)})
		if err != nil {
			t.Fatalf("buildPreparedStartWithWorkDirResolver: %v", err)
		}
		if prepared.cfg.WorkDir != sourceDir {
			t.Fatalf("legacy prepared work dir = %q, want the rig drain source's %q", prepared.cfg.WorkDir, sourceDir)
		}
		r := effectReads{legs: testWorkLegs(e.cityPath, e.cfg, e.work, e.rigStores).readOnly(), city: e.sessionsStore(), cityPath: e.cityPath}
		w := &World{CityPath: e.cityPath, Env: &reconcileEnv{Cfg: e.cfg}}
		v2, err := prepareRow(r, w, candidate.info, candidate.tp, sessTranscriptUnknown)
		if err != nil {
			t.Fatalf("prepareRow: %v", err)
		}
		if v2.cfg.WorkDir != sourceDir {
			t.Fatalf("v2 prepared work dir = %q, want the rig drain source's %q", v2.cfg.WorkDir, sourceDir)
		}
	})
}

// The v2 start effect launches a pool session with the opt_* pins of its
// trigger in a rig store: prepareRow reads the trigger through the city work
// legs (legsTriggerResolver). The sessions store it read before holds no rig
// work on either topology, so this is a behavior change on single-store cities
// with rigs too.
func TestV2PrepareRowAppliesARigTriggersOptionPins(t *testing.T) {
	forEachTopologyWithRig(t, func(t *testing.T, e splitEnv) {
		trigger := newTriggerOptionBead(t, e.rig, "", map[string]string{"model": "sonnet"})
		candidate := newTriggerSessionCandidate(t, e.sessionsStore(), trigger.ID, nil, "")
		r := effectReads{legs: testWorkLegs(e.cityPath, e.cfg, e.work, e.rigStores).readOnly(), city: e.sessionsStore(), cityPath: e.cityPath}
		w := &World{CityPath: e.cityPath, Env: &reconcileEnv{Cfg: e.cfg}}
		prepared, err := prepareRow(r, w, candidate.info, candidate.tp, sessTranscriptUnknown)
		if err != nil {
			t.Fatalf("prepareRow: %v", err)
		}
		if !strings.Contains(prepared.cfg.Command, "--model claude-sonnet-4-6") {
			t.Fatalf("v2 prepared command = %q, want the rig trigger %s's opt_model pin --model claude-sonnet-4-6", prepared.cfg.Command, trigger.ID)
		}
	})
}
