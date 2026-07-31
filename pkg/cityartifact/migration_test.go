package cityartifact_test

import (
	"context"
	"errors"
	"testing"

	"github.com/gastownhall/gascity/pkg/cityartifact"
)

// The checkpoint key gained a domain segment. A producer that was mid-upload
// when that shipped must resume its frontier from the old key rather than
// restart from zero and re-upload every part.
func TestLegacyCheckpointIsAdopted(t *testing.T) {
	ctx := context.Background()
	f := authorizedFake()
	store := cityartifact.NewMemoryStore()
	p := producerOn(t, f.Client("src_city_1", "gascity"), citySource(), store)
	first, err := p.Push(ctx, sampleArtifact())
	if err != nil {
		t.Fatalf("Push: %v", err)
	}

	legacy := cityartifact.NewMemoryStore()
	st, ok, err := store.Load(ctx, cityartifact.CheckpointKey(citySource(), "city-artifact-1"))
	if err != nil || !ok {
		t.Fatalf("load: %v (found=%t)", err, ok)
	}
	if err := legacy.Save(ctx, cityartifact.LegacyCheckpointKey(citySource(), "city-artifact-1"), st); err != nil {
		t.Fatalf("seed legacy: %v", err)
	}

	resumed := producerOn(t, f.Client("src_city_1", "gascity"), citySource(), legacy)
	res, err := resumed.Push(ctx, sampleArtifact())
	if err != nil {
		t.Fatalf("resumed Push: %v", err)
	}
	if res.ArtifactID != first.ArtifactID {
		t.Fatalf("resumed artifact %q, want the legacy frontier's %q", res.ArtifactID, first.ArtifactID)
	}
	if f.ArtifactCount() != 1 {
		t.Fatalf("the legacy checkpoint was not adopted: server holds %d artifacts", f.ArtifactCount())
	}
	migrated, ok, err := legacy.Load(ctx, cityartifact.CheckpointKey(citySource(), "city-artifact-1"))
	if err != nil || !ok {
		t.Fatalf("the adopted checkpoint was not rewritten under the domained key: %v (found=%t)", err, ok)
	}
	if migrated.ArtifactID != first.ArtifactID {
		t.Fatalf("migrated checkpoint holds artifact %q, want %q", migrated.ArtifactID, first.ArtifactID)
	}
}

// The legacy key is the ambiguous one, so it may hold a neighbouring adapter's
// document. Adopting one would import a foreign epoch and wedge this producer
// on a drift it never caused.
func TestForeignLegacyCheckpointIsNotAdopted(t *testing.T) {
	ctx := context.Background()
	f := authorizedFake()
	store := cityartifact.NewMemoryStore()

	foreign := cityartifact.State{Epoch: 5, SourceID: citySource().SourceID}
	if err := store.Save(ctx, cityartifact.LegacyCheckpointKey(citySource(), "city-artifact-1"), foreign); err != nil {
		t.Fatalf("seed foreign: %v", err)
	}

	p := producerOn(t, f.Client("src_city_1", "gascity"), citySource(), store)
	res, err := p.Push(ctx, sampleArtifact())
	if err != nil {
		if errors.Is(err, cityartifact.ErrIdentityDrift) {
			t.Fatalf("a neighbour's legacy checkpoint was inherited as drift: %v", err)
		}
		t.Fatalf("Push: %v", err)
	}
	if res.ArtifactID == "" {
		t.Fatal("Push produced no artifact")
	}
}

// Rollback is a switch on the adapter, not an arrangement with the scheduler.
func TestDisabledProducerIssuesNothing(t *testing.T) {
	ctx := context.Background()
	f := authorizedFake()
	store := cityartifact.NewMemoryStore()
	p := producerOn(t, f.Client("src_city_1", "gascity"), citySource(), store)
	p.Disabled = true
	if _, err := p.Push(ctx, sampleArtifact()); !errors.Is(err, cityartifact.ErrDisabled) {
		t.Fatalf("err = %v, want ErrDisabled", err)
	}
	if f.ArtifactCount() != 0 {
		t.Fatalf("a disabled producer issued %d writes", f.ArtifactCount())
	}
	if _, ok, err := store.Load(ctx, cityartifact.CheckpointKey(citySource(), "city-artifact-1")); ok || err != nil {
		t.Fatalf("a disabled producer touched its checkpoint: found=%t err=%v", ok, err)
	}
}
