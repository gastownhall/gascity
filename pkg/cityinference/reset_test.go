package cityinference

import (
	"context"
	"testing"
)

// A declared reset is honoured, written and attributable. Refusing it forever
// and never recording why was an outage, not a safety property.
func TestDeclaredResetIsHonouredAndRecorded(t *testing.T) {
	ctx := context.Background()
	api := NewFakeAPI(tenantAlpha, newWorkspaces())
	store := NewMemoryStore()
	p, err := NewProducer(api, testMapper(), store)
	if err != nil {
		t.Fatalf("new producer: %v", err)
	}
	inv := happyInvocation(t)
	if _, err := p.Push(ctx, []CityInvocation{inv}); err != nil {
		t.Fatalf("push: %v", err)
	}

	source := testSource()
	from := source.Epoch
	source.Epoch = from + 1
	source.Reset = &ResetDeclaration{
		FromEpoch: from, ToEpoch: source.Epoch, Reason: "source rebuilt", DeclaredBy: "ops@city",
	}
	after, err := NewProducer(api, Mapper{Source: source}, store)
	if err != nil {
		t.Fatalf("new producer: %v", err)
	}
	res, err := after.Push(ctx, []CityInvocation{inv})
	if err != nil {
		t.Fatalf("declared reset was refused: %v", err)
	}
	if res.Accepted != 1 {
		t.Fatalf("accepted %d after a declared reset, want the restarted frontier to have produced", res.Accepted)
	}

	st, ok, err := store.Load(ctx, CheckpointKey(source))
	if err != nil || !ok {
		t.Fatalf("checkpoint after reset: %v (found=%t)", err, ok)
	}
	if st.Epoch != source.Epoch {
		t.Fatalf("checkpoint epoch = %d, want the honoured reset written as %d", st.Epoch, source.Epoch)
	}
	if st.LastReset == nil || st.LastReset.DeclaredBy != "ops@city" || st.LastReset.FromEpoch != from {
		t.Fatalf("the honoured reset was not recorded: %+v", st.LastReset)
	}

	// And it settles: the next push under the same declared epoch skips rather
	// than re-uploading or refusing.
	again, err := after.Push(ctx, []CityInvocation{inv})
	if err != nil {
		t.Fatalf("push after reset: %v", err)
	}
	if again.Skipped != 1 {
		t.Fatalf("skipped %d, want the post-reset frontier to hold", again.Skipped)
	}
}
