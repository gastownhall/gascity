package sqlite

// Conformance for the Graph logical witness. The properties a witness has to
// have are the ones tested here: equal logical content digests equally
// regardless of where or in what order it was written; every field the
// contract names moves the digest; every family is present with its count even
// when empty; a durable reopen reproduces it; and content that contradicts
// itself blocks instead of hashing.

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/coordclass"
	"github.com/gastownhall/gascity/internal/storebinding"
)

var witnessEpoch = time.Date(2026, 3, 4, 5, 6, 7, 8, time.UTC)

// witnessSeed is one logical bead, pinned end to end so two independently
// built stores are genuinely the same graph rather than merely similar.
type witnessSeed struct {
	id       string
	title    string
	beadType string
	labels   []string
	metadata map[string]string
	parent   string
}

func seedWitnessGraph(t *testing.T, front storebinding.GraphStore, seeds []witnessSeed, edges [][3]string) {
	t.Helper()
	for _, seed := range seeds {
		bead := beads.Bead{
			ID:        seed.id,
			Title:     seed.title,
			Type:      seed.beadType,
			Labels:    seed.labels,
			Metadata:  seed.metadata,
			ParentID:  seed.parent,
			CreatedAt: witnessEpoch,
			UpdatedAt: witnessEpoch,
		}
		if _, err := front.Create(bead); err != nil {
			t.Fatalf("seeding %s: %v", seed.id, err)
		}
	}
	for _, edge := range edges {
		if err := front.DepAdd(edge[0], edge[1], edge[2]); err != nil {
			t.Fatalf("seeding edge %v: %v", edge, err)
		}
	}
}

func canonicalWitnessSeeds() []witnessSeed {
	return []witnessSeed{
		{id: "gcg-10", title: "root", beadType: "molecule", labels: []string{"gc:root", "formula:build"}, metadata: map[string]string{"gc.kind": "workflow", "gc.step_ref": "root"}},
		{id: "gcg-11", title: "first step", beadType: "step", parent: "gcg-10", metadata: map[string]string{"gc.root_bead_id": "gcg-10"}},
		{id: "gcg-12", title: "second step", beadType: "step", parent: "gcg-10", labels: []string{"gc:step"}},
	}
}

func canonicalWitnessEdges() [][3]string {
	return [][3]string{
		{"gcg-11", "gcg-10", "parent-child"},
		{"gcg-12", "gcg-11", "blocks"},
	}
}

func witnessOf(t *testing.T, front storebinding.GraphStore, floors GraphAllocatorFloors) storebinding.SemanticWitness {
	t.Helper()
	witness, err := GraphSemanticWitness(context.Background(), front, floors)
	if err != nil {
		t.Fatalf("GraphSemanticWitness: %v", err)
	}
	if err := witness.Validate(); err != nil {
		t.Fatalf("witness failed its own validation: %v", err)
	}
	return witness
}

// TestGraphWitnessIsIndependentOfPhysicalPlacementAndOrder is the property the
// whole design rests on: the digest describes the graph, not the file. Two
// stores at different paths, written in different orders, must agree.
func TestGraphWitnessIsIndependentOfPhysicalPlacementAndOrder(t *testing.T) {
	forward := openGraphFrontDoor(t)
	seedWitnessGraph(t, forward, canonicalWitnessSeeds(), canonicalWitnessEdges())

	reversedSeeds := canonicalWitnessSeeds()
	// Reparented steps still need their parent to exist first only for the
	// edge, not the row, so reversing the row order is legal and must not move
	// the digest.
	reversedSeeds[0], reversedSeeds[2] = reversedSeeds[2], reversedSeeds[0]
	reverse := openGraphFrontDoor(t)
	seedWitnessGraph(t, reverse, reversedSeeds, canonicalWitnessEdges())

	floors := GraphAllocatorFloors{CrossClassMaximum: 900, PersistedFloor: 900}
	first := witnessOf(t, forward, floors)
	second := witnessOf(t, reverse, floors)
	if first.Digest != second.Digest {
		t.Fatalf("same graph digested differently:\n  %s\n  %s", first.Digest, second.Digest)
	}
	if first.Class != coordclass.ClassGraph || first.Algorithm != storebinding.SemanticWitnessAlgorithm {
		t.Fatalf("witness carries the wrong identity: %+v", first)
	}
}

// TestGraphWitnessCarriesEveryFamilyWithExplicitZeroCounts is the
// completeness mechanism: an empty class still emits every family, so an
// exporter that skipped one is a structural difference, not a coincidence.
func TestGraphWitnessCarriesEveryFamilyWithExplicitZeroCounts(t *testing.T) {
	empty := witnessOf(t, openGraphFrontDoor(t), GraphAllocatorFloors{})

	counts := map[string]int{}
	for _, family := range empty.Families {
		counts[family.Name] = family.Count
	}
	for _, name := range GraphWitnessFamilies() {
		if _, present := counts[name]; !present {
			t.Fatalf("witness of an empty graph omits family %q", name)
		}
	}
	if len(counts) != len(GraphWitnessFamilies()) {
		t.Fatalf("witness families = %+v, want exactly %v", empty.Families, GraphWitnessFamilies())
	}
	for _, name := range []string{"bead", "dependency", "live_root_closure"} {
		if counts[name] != 0 {
			t.Fatalf("empty graph reported %d %s records", counts[name], name)
		}
	}
	if counts["allocator"] != 2 {
		t.Fatalf("allocator family count = %d, want the two floor records", counts["allocator"])
	}

	populated := openGraphFrontDoor(t)
	seedWitnessGraph(t, populated, canonicalWitnessSeeds(), canonicalWitnessEdges())
	full := witnessOf(t, populated, GraphAllocatorFloors{})
	if full.Digest == empty.Digest {
		t.Fatal("a populated graph digested the same as an empty one")
	}
}

// TestGraphWitnessMovesForEveryContractedField walks the field enumeration:
// each mutation is a different graph and must produce a different digest. A
// field that does NOT move the digest is a field a migration could silently
// drop.
func TestGraphWitnessMovesForEveryContractedField(t *testing.T) {
	floors := GraphAllocatorFloors{CrossClassMaximum: 900, PersistedFloor: 900}
	baseline := func(t *testing.T) (storebinding.GraphStore, string) {
		t.Helper()
		front := openGraphFrontDoor(t)
		seedWitnessGraph(t, front, canonicalWitnessSeeds(), canonicalWitnessEdges())
		return front, witnessOf(t, front, floors).Digest
	}

	for name, mutate := range map[string]func(t *testing.T, front storebinding.GraphStore){
		"title": func(t *testing.T, front storebinding.GraphStore) {
			title := "renamed"
			if err := front.Update("gcg-10", beads.UpdateOpts{Title: &title}); err != nil {
				t.Fatalf("Update: %v", err)
			}
		},
		"status": func(t *testing.T, front storebinding.GraphStore) {
			if err := front.Close("gcg-12"); err != nil {
				t.Fatalf("Close: %v", err)
			}
		},
		"assignee and claim fence": func(t *testing.T, front storebinding.GraphStore) {
			if _, ok, err := front.Claim("gcg-12", "worker"); err != nil || !ok {
				t.Fatalf("Claim = (%v, %v), want success", ok, err)
			}
		},
		"label": func(t *testing.T, front storebinding.GraphStore) {
			if err := front.Update("gcg-10", beads.UpdateOpts{Labels: []string{"gc:extra"}}); err != nil {
				t.Fatalf("Update(label): %v", err)
			}
		},
		"metadata": func(t *testing.T, front storebinding.GraphStore) {
			if err := front.SetMetadata("gcg-11", "gc.phase", "done"); err != nil {
				t.Fatalf("SetMetadata: %v", err)
			}
		},
		"parent": func(t *testing.T, front storebinding.GraphStore) {
			parent := "gcg-12"
			if err := front.Update("gcg-11", beads.UpdateOpts{ParentID: &parent}); err != nil {
				t.Fatalf("Update(parent): %v", err)
			}
		},
		"dependency edge": func(t *testing.T, front storebinding.GraphStore) {
			if err := front.DepRemove("gcg-12", "gcg-11"); err != nil {
				t.Fatalf("DepRemove: %v", err)
			}
		},
		"dependency type": func(t *testing.T, front storebinding.GraphStore) {
			if err := front.DepRemove("gcg-12", "gcg-11"); err != nil {
				t.Fatalf("DepRemove: %v", err)
			}
			if err := front.DepAdd("gcg-12", "gcg-11", "tracks"); err != nil {
				t.Fatalf("DepAdd(tracks): %v", err)
			}
		},
		"membership": func(t *testing.T, front storebinding.GraphStore) {
			if _, err := front.Create(beads.Bead{ID: "gcg-13", Title: "extra", Type: "step", CreatedAt: witnessEpoch, UpdatedAt: witnessEpoch}); err != nil {
				t.Fatalf("Create: %v", err)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			front, before := baseline(t)
			mutate(t, front)
			after := witnessOf(t, front, floors).Digest
			if after == before {
				t.Fatalf("changing %s did not move the witness digest", name)
			}
		})
	}
}

// TestGraphWitnessCarriesOpaqueEdgeMetadata pins the payload into the digest.
// Two graphs with identical beads and identical edges, differing only in what
// an edge carried, are different graphs — a migration that moved the edges but
// dropped their payloads must not hash equal.
func TestGraphWitnessCarriesOpaqueEdgeMetadata(t *testing.T) {
	withPayload := func(t *testing.T, payload string) string {
		t.Helper()
		front := openGraphFrontDoor(t)
		edge := beads.GraphApplyEdge{FromKey: "step", ToKey: "root", Type: "blocks", Metadata: payload}
		if _, err := front.ApplyGraphPlan(context.Background(), &beads.GraphApplyPlan{
			Nodes: []beads.GraphApplyNode{
				{Key: "root", Title: "root", Type: "molecule"},
				{Key: "step", Title: "step", Type: "step"},
			},
			Edges: []beads.GraphApplyEdge{edge},
		}); err != nil {
			t.Fatalf("ApplyGraphPlan(%q): %v", payload, err)
		}
		return witnessOf(t, front, GraphAllocatorFloors{}).Digest
	}

	carried := withPayload(t, `{"recipe":"build"}`)
	different := withPayload(t, `{"recipe":"release"}`)
	dropped := withPayload(t, "")
	if carried == different {
		t.Fatal("two different edge payloads digested identically")
	}
	if carried == dropped {
		t.Fatal("an edge that lost its payload digested the same as one that kept it")
	}
}

// TestGraphWitnessSeparatesStorageTiers isolates the tier from membership:
// two graphs with the same beads, differing only in which tier one bead lives
// on, are different graphs. A wisp that arrives as a durable row survives GC
// it should not have survived, so the tier is semantic, not physical.
func TestGraphWitnessSeparatesStorageTiers(t *testing.T) {
	tiered := func(t *testing.T, storage beads.StorageClass) string {
		t.Helper()
		front := openGraphFrontDoor(t)
		seedWitnessGraph(t, front, canonicalWitnessSeeds(), canonicalWitnessEdges())
		if _, err := front.CreateWithStorage(beads.Bead{
			ID: "gcg-14", Title: "tiered", Type: "step",
			CreatedAt: witnessEpoch, UpdatedAt: witnessEpoch,
		}, storage); err != nil {
			t.Fatalf("CreateWithStorage(%q): %v", storage, err)
		}
		return witnessOf(t, front, GraphAllocatorFloors{}).Digest
	}

	if durable, ephemeral := tiered(t, beads.StorageDefault), tiered(t, beads.StorageEphemeral); durable == ephemeral {
		t.Fatal("the same bead on the durable and ephemeral tiers digested identically")
	}
}

// TestGraphWitnessMovesForAllocatorFloors is the "the ID space moved, not just
// the rows" mechanism. A destination that re-mints from zero must fail digest
// equality before it can ever collide.
func TestGraphWitnessMovesForAllocatorFloors(t *testing.T) {
	front := openGraphFrontDoor(t)
	seedWitnessGraph(t, front, canonicalWitnessSeeds(), canonicalWitnessEdges())

	base := witnessOf(t, front, GraphAllocatorFloors{CrossClassMaximum: 900, PersistedFloor: 900}).Digest
	crossClassMoved := witnessOf(t, front, GraphAllocatorFloors{CrossClassMaximum: 901, PersistedFloor: 900}).Digest
	persistedMoved := witnessOf(t, front, GraphAllocatorFloors{CrossClassMaximum: 900, PersistedFloor: 901}).Digest
	reMintedFromZero := witnessOf(t, front, GraphAllocatorFloors{}).Digest

	if crossClassMoved == base || persistedMoved == base || reMintedFromZero == base {
		t.Fatalf("allocator floors are not in the digest: base=%s crossClass=%s persisted=%s zero=%s", base, crossClassMoved, persistedMoved, reMintedFromZero)
	}
	if crossClassMoved == persistedMoved {
		t.Fatal("the two allocator floors are interchangeable in the digest; they are distinct semantic values")
	}
}

// TestGraphWitnessSurvivesAFreshReopen is the witness contract's fresh-reopen rule: the digest
// must be reproducible from durable bytes, not from a warm connection.
func TestGraphWitnessSurvivesAFreshReopen(t *testing.T) {
	root := t.TempDir()
	first, err := OpenGraph(graphSpec(t, root))
	if err != nil {
		t.Fatalf("OpenGraph: %v", err)
	}
	seedWitnessGraph(t, first.Graph(), canonicalWitnessSeeds(), canonicalWitnessEdges())
	floors := GraphAllocatorFloors{CrossClassMaximum: 42, PersistedFloor: 42}
	before := witnessOf(t, first.Graph(), floors).Digest
	if err := first.Close(); err != nil {
		t.Fatalf("closing the first component: %v", err)
	}

	second := openGraphComponent(t, root)
	after := witnessOf(t, second.Graph(), floors).Digest
	if after != before {
		t.Fatalf("witness did not survive a real close and reopen:\n  %s\n  %s", before, after)
	}
}

// TestGraphWitnessBlocksOnDamagedState pins the blocking taxonomy: content
// that contradicts itself is refused rather than witnessed. A confident digest
// over damaged state is worse than no digest at all.
func TestGraphWitnessBlocksOnDamagedState(t *testing.T) {
	t.Run("internal edge to an absent Graph bead", func(t *testing.T) {
		front := openGraphFrontDoor(t)
		seedWitnessGraph(t, front, canonicalWitnessSeeds(), canonicalWitnessEdges())
		if err := front.DepAdd("gcg-10", "gcg-9999", "blocks"); err != nil {
			t.Fatalf("DepAdd(dangling): %v", err)
		}
		if _, err := GraphSemanticWitness(context.Background(), front, GraphAllocatorFloors{}); !errors.Is(err, ErrGraphWitnessDamagedState) {
			t.Fatalf("witness over a dangling edge = %v, want ErrGraphWitnessDamagedState", err)
		}
	})

	// The inbound half of the same rule. A Graph-namespace source that is not
	// in the store is a bead this class was supposed to hold and lost, and no
	// other class can vouch for it.
	t.Run("absent Graph bead depends on a present one", func(t *testing.T) {
		front := openGraphFrontDoor(t)
		seedWitnessGraph(t, front, canonicalWitnessSeeds(), canonicalWitnessEdges())
		if err := front.DepAdd("gcg-8888", "gcg-10", "blocks"); err != nil {
			t.Fatalf("DepAdd(inbound dangling): %v", err)
		}
		if _, err := GraphSemanticWitness(context.Background(), front, GraphAllocatorFloors{}); !errors.Is(err, ErrGraphWitnessDamagedState) {
			t.Fatalf("witness over an absent Graph source = %v, want ErrGraphWitnessDamagedState", err)
		}
	})

	t.Run("duplicate label on one bead", func(t *testing.T) {
		front := openGraphFrontDoor(t)
		if _, err := front.Create(beads.Bead{
			ID: "gcg-20", Title: "damaged", Type: "task",
			Labels:    []string{"gc:dup", "gc:dup"},
			CreatedAt: witnessEpoch, UpdatedAt: witnessEpoch,
		}); err != nil {
			t.Fatalf("Create: %v", err)
		}
		if _, err := GraphSemanticWitness(context.Background(), front, GraphAllocatorFloors{}); !errors.Is(err, ErrGraphWitnessDamagedState) {
			t.Fatalf("witness over a duplicated label = %v, want ErrGraphWitnessDamagedState", err)
		}
	})
}

func witnessFamilyCount(t *testing.T, witness storebinding.SemanticWitness, family string) int {
	t.Helper()
	for _, counted := range witness.Families {
		if counted.Name == family {
			return counted.Count
		}
	}
	t.Fatalf("witness carries no %q family: %+v", family, witness.Families)
	return 0
}

// TestGraphWitnessAdmitsCrossClassAndExternalReferences is the witness contract: a
// reference the Graph class is not the authority for is not damage. Graph
// legitimately points at Work and Sessions beads and at declared-external
// targets, and a witness that refused those would refuse healthy migrations —
// or, worse, would produce digests that differ purely by which classes were
// resident, which would make the equality claim meaningless.
func TestGraphWitnessAdmitsCrossClassAndExternalReferences(t *testing.T) {
	floors := GraphAllocatorFloors{CrossClassMaximum: 900, PersistedFloor: 900}
	for name, edge := range map[string][3]string{
		"outbound required cross-class reference": {"gcg-10", "ga-4321", "tracks"},
		"inbound required cross-class reference":  {"ga-4321", "gcg-10", "tracks"},
		"declared-external reference":             {"gcg-10", "external:jira/PROJ-7", "relates-to"},
	} {
		t.Run(name, func(t *testing.T) {
			seeded := func(t *testing.T, withEdge bool) storebinding.SemanticWitness {
				t.Helper()
				front := openGraphFrontDoor(t)
				seedWitnessGraph(t, front, canonicalWitnessSeeds(), canonicalWitnessEdges())
				if withEdge {
					if err := front.DepAdd(edge[0], edge[1], edge[2]); err != nil {
						t.Fatalf("DepAdd%v: %v", edge, err)
					}
				}
				witness, err := GraphSemanticWitness(context.Background(), front, floors)
				if err != nil {
					t.Fatalf("witness over a %s: %v", name, err)
				}
				return witness
			}

			without := seeded(t, false)
			with := seeded(t, true)
			if with.Digest == without.Digest {
				t.Fatalf("a %s never reached the stream; the digest did not move", name)
			}
			if got, want := witnessFamilyCount(t, with, "dependency"), witnessFamilyCount(t, without, "dependency")+1; got != want {
				t.Fatalf("dependency family count = %d, want %d — the reference was dropped rather than typed", got, want)
			}
			// An admitted reference must still be deterministic: a second store
			// at a different path holding the same graph has to agree, or the
			// witness would only ever compare against itself.
			if again := seeded(t, true); again.Digest != with.Digest {
				t.Fatalf("a %s digested differently in a second store:\n  %s\n  %s", name, with.Digest, again.Digest)
			}
		})
	}
}

// TestGraphWitnessBindsReferenceKindIntoTheDigest proves the kind is hashed
// rather than merely tolerated. The tuple and payload are held fixed, so the
// only thing that can move the digest is the kind itself — which is what makes
// "an internal edge became external" a detectable change of graph.
func TestGraphWitnessBindsReferenceKindIntoTheDigest(t *testing.T) {
	dep := beads.Dep{IssueID: "gcg-10", DependsOnID: "gcg-11", Type: "blocks"}
	digestOf := func(kind string) string {
		stream := &canonicalStream{}
		encodeWitnessDependency(stream, graphWitnessEdge{dep: dep, kind: kind}, "", false)
		return stream.digest()
	}

	digests := map[string]string{}
	for _, kind := range []string{graphRefKindInternal, graphRefKindCrossClass, graphRefKindExternal} {
		digest := digestOf(kind)
		if owner, collided := digests[digest]; collided {
			t.Fatalf("kinds %q and %q digest identically; the kind is not in the stream", owner, kind)
		}
		digests[digest] = kind
		if repeat := digestOf(kind); repeat != digest {
			t.Fatalf("kind %q digested unstably:\n  %s\n  %s", kind, digest, repeat)
		}
	}
}

// TestGraphEndpointKindReadsTheDeclarationNotTheStore pins the classification
// rule. It is a function of the ID alone — no store is consulted — so the same
// reference types identically on both sides of a migration whatever each side
// happens to hold. An ID that declares nothing recognizable is internal: an
// unreadable endpoint is not a license to skip resolution.
func TestGraphEndpointKindReadsTheDeclarationNotTheStore(t *testing.T) {
	for id, want := range map[string]string{
		"gcg-10":               graphRefKindInternal,
		"gcg-4.1":              graphRefKindInternal,
		"gcg":                  graphRefKindInternal,
		"":                     graphRefKindInternal,
		"12345":                graphRefKindInternal,
		"-orphaned":            graphRefKindInternal,
		"ga-4321":              graphRefKindCrossClass,
		"bd-7":                 graphRefKindCrossClass,
		"external:jira/PROJ-7": graphRefKindExternal,
		"EXTERNAL:vendor/9":    graphRefKindExternal,
	} {
		if got := graphEndpointKind(id); got != want {
			t.Errorf("graphEndpointKind(%q) = %q, want %q", id, got, want)
		}
	}

	// An edge is internal only when it is internal at both ends, and a
	// declared-external endpoint outranks a cross-class one because nothing is
	// expected to resolve it.
	for name, tc := range map[string]struct {
		dep  beads.Dep
		want string
	}{
		"both ends internal":      {beads.Dep{IssueID: "gcg-10", DependsOnID: "gcg-11"}, graphRefKindInternal},
		"foreign target":          {beads.Dep{IssueID: "gcg-10", DependsOnID: "ga-1"}, graphRefKindCrossClass},
		"foreign source":          {beads.Dep{IssueID: "ga-1", DependsOnID: "gcg-10"}, graphRefKindCrossClass},
		"external target":         {beads.Dep{IssueID: "gcg-10", DependsOnID: "external:x"}, graphRefKindExternal},
		"external beats foreign":  {beads.Dep{IssueID: "ga-1", DependsOnID: "external:x"}, graphRefKindExternal},
		"unrecognized stays home": {beads.Dep{IssueID: "gcg-10", DependsOnID: "99"}, graphRefKindInternal},
	} {
		if got := graphReferenceKind(tc.dep); got != tc.want {
			t.Errorf("graphReferenceKind(%s) = %q, want %q", name, got, tc.want)
		}
	}
}

// TestGraphWitnessEdgeMultiplicityCrossChecksWhatBothIndexesCanSee guards the
// half of the bidirectional check that admitting cross-class references could
// have quietly weakened. An edge with both endpoints in the store is visible
// from both sides, so the two views must agree exactly; an edge with one
// endpoint outside the class is visible from one side only, and that asymmetry
// is explained by non-residence rather than by a lost index row.
func TestGraphWitnessEdgeMultiplicityCrossChecksWhatBothIndexesCanSee(t *testing.T) {
	resident := map[string]struct{}{"gcg-10": {}, "gcg-11": {}}
	internal := beads.Dep{IssueID: "gcg-11", DependsOnID: "gcg-10", Type: "blocks"}
	outbound := beads.Dep{IssueID: "gcg-10", DependsOnID: "ga-1", Type: "tracks"}
	inbound := beads.Dep{IssueID: "ga-1", DependsOnID: "gcg-10", Type: "tracks"}

	for name, tc := range map[string]struct {
		dep              beads.Dep
		forward, reverse int
		want             int
		wantDamaged      bool
	}{
		"internal edge agrees":            {internal, 1, 1, 1, false},
		"internal duplicate agrees":       {internal, 2, 2, 2, false},
		"internal lost a reverse row":     {internal, 2, 1, 0, true},
		"internal lost a forward row":     {internal, 0, 1, 0, true},
		"outbound reference is one-sided": {outbound, 1, 0, 1, false},
		"inbound reference is one-sided":  {inbound, 0, 1, 1, false},
		"non-resident endpoint both ways": {outbound, 1, 1, 0, true},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := witnessEdgeMultiplicity(tc.dep, resident, tc.forward, tc.reverse)
			if tc.wantDamaged {
				if !errors.Is(err, ErrGraphWitnessDamagedState) {
					t.Fatalf("witnessEdgeMultiplicity = (%d, %v), want ErrGraphWitnessDamagedState", got, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("witnessEdgeMultiplicity: %v", err)
			}
			if got != tc.want {
				t.Fatalf("witnessEdgeMultiplicity = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestGraphWitnessRejectsInvalidInput(t *testing.T) {
	if _, err := GraphSemanticWitness(context.Background(), nil, GraphAllocatorFloors{}); err == nil {
		t.Fatal("GraphSemanticWitness(nil front door) succeeded")
	}
	front := openGraphFrontDoor(t)
	if _, err := GraphSemanticWitness(context.Background(), front, GraphAllocatorFloors{PersistedFloor: -1}); err == nil {
		t.Fatal("GraphSemanticWitness accepted a negative floor")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := GraphSemanticWitness(ctx, front, GraphAllocatorFloors{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("GraphSemanticWitness(canceled) = %v, want context.Canceled", err)
	}
}

// TestSemanticWitnessValidationRejectsMalformedValues keeps the shared witness
// type from accepting something no real export could have produced.
func TestSemanticWitnessValidationRejectsMalformedValues(t *testing.T) {
	valid := witnessOf(t, openGraphFrontDoor(t), GraphAllocatorFloors{})

	for name, mutate := range map[string]func(w *storebinding.SemanticWitness){
		"version":          func(w *storebinding.SemanticWitness) { w.Version = 2 },
		"class":            func(w *storebinding.SemanticWitness) { w.Class = coordclass.ClassWork },
		"algorithm":        func(w *storebinding.SemanticWitness) { w.Algorithm = "gascity.storage-semantic-witness.v0" },
		"contract":         func(w *storebinding.SemanticWitness) { w.Contract = "" },
		"digest":           func(w *storebinding.SemanticWitness) { w.Digest = "deadbeef" },
		"unnamed family":   func(w *storebinding.SemanticWitness) { w.Families[0].Name = "" },
		"negative count":   func(w *storebinding.SemanticWitness) { w.Families[0].Count = -1 },
		"duplicate family": func(w *storebinding.SemanticWitness) { w.Families[1].Name = w.Families[0].Name },
	} {
		t.Run(name, func(t *testing.T) {
			broken := valid
			broken.Families = append([]storebinding.WitnessFamilyCount(nil), valid.Families...)
			mutate(&broken)
			if err := broken.Validate(); err == nil {
				t.Fatalf("Validate accepted a witness with a broken %s", name)
			}
		})
	}
}
