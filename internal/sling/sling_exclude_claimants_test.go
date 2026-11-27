package sling

import (
	"context"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/beadstest"
	"github.com/gastownhall/gascity/internal/config"
	convoycore "github.com/gastownhall/gascity/internal/convoy"
	"github.com/gastownhall/gascity/internal/formula"
)

// An input's exclusions must already be on every graph bead when it is born,
// including synthesized controls. Checking Create catches post-pour stamping.
type exclusionCreateStore struct {
	beads.Store
	t    *testing.T
	seen int
}

func TestGraphInputExclusionsUnionAndUnresolvedMember(t *testing.T) {
	store := beads.NewMemStore()
	convoy, err := store.Create(beads.Bead{Type: "convoy", Metadata: map[string]string{beadmeta.ExcludeClaimantsMetadataKey: "review.convoy"}})
	if err != nil {
		t.Fatal(err)
	}
	member, err := store.Create(beads.Bead{Type: "task", Status: "closed", Metadata: map[string]string{beadmeta.ExcludeClaimantsMetadataKey: "review.author"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := convoycore.TrackItemIn(convoycore.MemberClasses{Convoy: beadstest.Work(store)}, convoy.ID, member.ID); err != nil {
		t.Fatal(err)
	}
	recipe := &formula.Recipe{Steps: []formula.RecipeStep{{IsRoot: true, Metadata: map[string]string{
		beadmeta.InputConvoyIDMetadataKey:    convoy.ID,
		beadmeta.ExcludeClaimantsMetadataKey: "review.local,review.author",
	}}}}
	if err := inheritGraphInputExclusions(recipe, SlingDeps{Store: beadstest.Work(store)}); err != nil {
		t.Fatal(err)
	}
	for _, who := range []string{"review.local", "review.author", "review.convoy"} {
		if !beadmeta.ExcludesClaimant(recipe.Steps[0].Metadata, who) {
			t.Errorf("missing %s: %v", who, recipe.Steps[0].Metadata)
		}
	}
	if err := store.DepAdd(convoy.ID, "missing-report", "tracks"); err != nil {
		t.Fatal(err)
	}
	if err := inheritGraphInputExclusions(recipe, SlingDeps{Store: beadstest.Work(store)}); err == nil {
		t.Fatal("unresolved input member silently dropped its possible exclusions")
	}
}

func (s *exclusionCreateStore) Create(b beads.Bead) (beads.Bead, error) {
	s.seen++
	if !beadmeta.ExcludesClaimant(b.Metadata, "review.author") {
		s.t.Errorf("creating %s (%s) without input exclusion: %v", b.Title, b.Metadata[beadmeta.KindMetadataKey], b.Metadata)
	}
	return s.Store.Create(b)
}

func TestSlingOnGraphInheritsExcludedClaimantsBeforeCreation(t *testing.T) {
	deps := graphV2ConvoyFirstSlingTestConfig(t)
	source, err := deps.Store.Create(beads.Bead{Title: "report", Type: "task", Metadata: map[string]string{
		beadmeta.ExcludeClaimantsMetadataKey: "review.author",
	}})
	if err != nil {
		t.Fatal(err)
	}
	graph := &exclusionCreateStore{Store: beads.NewMemStore(), t: t}
	deps.GraphStore = beadstest.Graph(graph)
	s, err := New(deps)
	if err != nil {
		t.Fatal(err)
	}
	result, err := s.AttachFormula(context.Background(), "graph-work", source.ID, config.Agent{Name: "review", MaxActiveSessions: intPtr(1)}, FormulaOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if result.WorkflowID == "" || graph.seen < 3 {
		t.Fatalf("missing workflow/root/controls: result=%+v created=%d", result, graph.seen)
	}
}
