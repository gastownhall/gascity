package cityneutral

import (
	"context"
	"errors"
	"testing"
)

// The checkpoint key gained a domain segment. A producer that was mid-run when
// that shipped must resume its frontier from the old key rather than restart
// from zero and re-upload the run.
func TestLegacyCheckpointIsAdopted(t *testing.T) {
	ctx := context.Background()
	f := NewFake("svc-city@tenant", "gc-city-01", "city")
	store := NewMemoryStore()
	p, err := NewProducer(f, Mapper{Source: citySource(), AllowRawContent: true}, store)
	if err != nil {
		t.Fatalf("NewProducer: %v", err)
	}
	first := pushOK(t, p, chainWith(recordsUpTo(2), false))

	// Rebuild the pre-migration world: the same frontier, under the old key.
	legacy := NewMemoryStore()
	st, ok, err := store.Load(ctx, CheckpointKey(citySource(), "city-run-7"))
	if err != nil || !ok {
		t.Fatalf("load: %v (found=%t)", err, ok)
	}
	if err := legacy.Save(ctx, LegacyCheckpointKey(citySource(), "city-run-7"), st); err != nil {
		t.Fatalf("seed legacy: %v", err)
	}

	resumed, err := NewProducer(f, Mapper{Source: citySource(), AllowRawContent: true}, legacy)
	if err != nil {
		t.Fatalf("NewProducer: %v", err)
	}
	res := pushOK(t, resumed, chainWith(recordsUpTo(2), false))
	if res.Accepted != 0 || res.Skipped != 2 {
		t.Fatalf("resumed accepted %d skipped %d, want the legacy frontier to have been adopted", res.Accepted, res.Skipped)
	}
	if res.RunTeamID != first.RunTeamID {
		t.Fatalf("resumed run %q, want %q", res.RunTeamID, first.RunTeamID)
	}
	migrated, ok, err := legacy.Load(ctx, CheckpointKey(citySource(), "city-run-7"))
	if err != nil || !ok {
		t.Fatalf("the adopted checkpoint was not rewritten under the domained key: %v (found=%t)", err, ok)
	}
	if migrated.RunTeamID != first.RunTeamID {
		t.Fatalf("migrated checkpoint holds run %q, want %q", migrated.RunTeamID, first.RunTeamID)
	}
}

// The legacy key is the ambiguous one, so it may hold a neighbouring adapter's
// document. Adopting one would import a foreign epoch and wedge this producer
// on a drift it never caused.
func TestForeignLegacyCheckpointIsNotAdopted(t *testing.T) {
	ctx := context.Background()
	f := NewFake("svc-city@tenant", "gc-city-01", "city")
	store := NewMemoryStore()

	// What a cityartifact checkpoint at epoch 5 decodes to here: our own source,
	// a live epoch, and none of this domain's fields.
	foreign := State{Epoch: 5, SourceID: citySource().SourceID}
	if err := store.Save(ctx, LegacyCheckpointKey(citySource(), "city-run-7"), foreign); err != nil {
		t.Fatalf("seed foreign: %v", err)
	}

	p, err := NewProducer(f, Mapper{Source: citySource(), AllowRawContent: true}, store)
	if err != nil {
		t.Fatalf("NewProducer: %v", err)
	}
	res, err := p.Push(ctx, chainWith(recordsUpTo(2), false))
	if err != nil {
		if errors.Is(err, ErrIdentityDrift) {
			t.Fatalf("a neighbour's legacy checkpoint was inherited as drift: %v", err)
		}
		t.Fatalf("Push: %v", err)
	}
	if res.Accepted != 2 {
		t.Fatalf("accepted %d, want 2", res.Accepted)
	}
}

// Rollback is a switch on the adapter, not an arrangement with the scheduler.
func TestDisabledProducerIssuesNothing(t *testing.T) {
	f := NewFake("svc-city@tenant", "gc-city-01", "city")
	store := NewMemoryStore()
	p, err := NewProducer(f, Mapper{Source: citySource(), AllowRawContent: true}, store)
	if err != nil {
		t.Fatalf("NewProducer: %v", err)
	}
	p.Disabled = true
	if _, err := p.Push(context.Background(), chainWith(recordsUpTo(2), false)); !errors.Is(err, ErrDisabled) {
		t.Fatalf("err = %v, want ErrDisabled", err)
	}
	if _, ok, err := store.Load(context.Background(), CheckpointKey(citySource(), "city-run-7")); ok || err != nil {
		t.Fatalf("a disabled producer touched its checkpoint: found=%t err=%v", ok, err)
	}
}
