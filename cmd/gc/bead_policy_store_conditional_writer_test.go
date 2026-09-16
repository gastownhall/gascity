// Capability-forwarding acceptance for the write-fence half of the remote worker
// family (cr-gdeav.5.10.3). The store the API answers worker verbs with is not
// the leaf backend: cmd/gc/api_state.go builds it as
// wrapStoreWithBeadPolicies(beads.NewCachingStore(unwrappedBase, ...), cfg), so
// the route's handle is a *beadPolicyStore OVER a *beads.CachingStore. These
// tests drive that exact shape, because the defect was the pair's: the policy
// layer embeds the beads.Store INTERFACE, which declares none of the optional
// capabilities, and beads.ConditionalWriterFor deliberately does not follow
// ConditionalWritesResolveTarget (internal/beads/beads.go:359-366) — so the
// worker claim and heartbeat routes, which probe the fence before their first
// write (internal/api/handler_worker.go workerLeaseWriter), answered typed 501
// for a city whose backing fences every write it makes.
package main

import (
	"errors"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

// workerVerbStoreShape builds the store the city actually hands the worker
// routes: leaf ← CachingStore ← beadPolicyStore, the same order
// wrapWithCachingStore produces (api_state.go re-wraps the cache it built over
// the unwrapped base).
func workerVerbStoreShape(t *testing.T, base beads.Store) beads.Store {
	t.Helper()
	shape := wrapStoreWithBeadPolicies(beads.NewCachingStoreForTest(base, nil), &config.City{})
	if _, _, ok := unwrapBeadPolicyStore(shape); !ok {
		t.Fatalf("test premise: %T is not the policy-over-cache shape the API serves", shape)
	}
	return shape
}

// TestPolicyWrappedStoreReachesConditionalWriterForWorkerVerbs is the red leg:
// before the forwarding existed, beads.ConditionalWriterFor returned false here
// and both worker verbs refused before writing anything.
func TestPolicyWrappedStoreReachesConditionalWriterForWorkerVerbs(t *testing.T) {
	backing := beads.NewMemStore()
	seeded, err := backing.Create(beads.Bead{Title: "remote worker target", Status: "open"})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	shape := workerVerbStoreShape(t, backing)

	// This is the call workerLeaseWriter makes, on the store the routes resolve.
	writer, ok := beads.ConditionalWriterFor(shape)
	if !ok {
		t.Fatal("ConditionalWriterFor found no writer through the policy/cache shape: the policy wrapper hid the backing's revision fence, so /worker/claim and /worker/heartbeat answer 501")
	}

	// The claim half of this shape is deliberately NOT asserted here. It needs
	// an assignee-shaped claim on the leaf and a forward through CachingStore,
	// neither of which exists at this commit — beads.ClaimFor reports
	// ErrClaimUnsupported through this shape, correctly. That is a separate
	// capability from the write fence this change forwards, and asserting it
	// here would couple a write-fence test to an unrelated store capability.
	// The claim dispatch itself is covered in internal/beads by
	// TestClaimForDispatchesBothClaimShapes.

	// The lease stamp the claim route would make goes through this writer and
	// must arrive FENCED at the backing: the two keys at the revision the route
	// just read (internal/api/handler_worker.go stampWorkerLease).
	fresh, err := shape.Get(seeded.ID)
	if err != nil {
		t.Fatalf("shape Get: %v", err)
	}
	stamp := beads.UpdateOpts{Metadata: map[string]string{
		beadmeta.LeaseOwnerMetadataKey: "worker-local-3-pool",
		beadmeta.ClaimedAtMetadataKey:  "2026-09-08T10:00:00Z",
	}}
	if err := writer.UpdateIfMatch(seeded.ID, fresh.Revision, stamp); err != nil {
		t.Fatalf("lease stamp through the shape: %v", err)
	}
	atBacking, err := backing.Get(seeded.ID)
	if err != nil {
		t.Fatalf("backing Get: %v", err)
	}
	if atBacking.Metadata[beadmeta.LeaseOwnerMetadataKey] != "worker-local-3-pool" || atBacking.Metadata[beadmeta.ClaimedAtMetadataKey] == "" {
		t.Fatalf("fenced stamp never reached the backing: %v", atBacking.Metadata)
	}

	// The fence is real at the backing, not merely present: a stale revision
	// loses and writes nothing. A stamp that always "succeeds" would be the
	// unconditional write this family was bounced for.
	if err := writer.UpdateIfMatch(seeded.ID, atBacking.Revision-1, beads.UpdateOpts{Metadata: map[string]string{
		beadmeta.LeaseOwnerMetadataKey: "a-displaced-incarnation",
	}}); !beads.IsPreconditionFailed(err) {
		t.Fatalf("stale-revision stamp err = %v, want a precondition failure at the backing", err)
	}
	if again, err := backing.Get(seeded.ID); err != nil || again.Metadata[beadmeta.LeaseOwnerMetadataKey] != "worker-local-3-pool" {
		t.Fatalf("the lost stamp still reached the backing: %q err %v", again.Metadata[beadmeta.LeaseOwnerMetadataKey], err)
	}

	// The narrow metadata CAS the release path uses resolves on the same shape.
	if _, ok := beads.MetadataCASWriterFor(shape); !ok {
		t.Fatal("MetadataCASWriterFor found no CAS through the policy/cache shape")
	}
}

// TestPolicyStoreGraphShapeForwardsConditionalWriter pins the promoted half: a
// split city serves its class stores as *beadPolicyGraphStore, which reaches the
// handle only through its embedded *beadPolicyStore.
func TestPolicyStoreGraphShapeForwardsConditionalWriter(t *testing.T) {
	backing := beads.NewMemStore()
	seeded, err := backing.Create(beads.Bead{Title: "graph class target"})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	graph := &beadPolicyGraphStore{
		beadPolicyStore: &beadPolicyStore{Store: beads.NewCachingStoreForTest(backing, nil), cfg: &config.City{}},
	}
	if _, ok := beads.ConditionalWriterFor(graph); !ok {
		t.Fatal("beadPolicyGraphStore does not forward the conditional-write capability")
	}
	writer, ok := beads.ConditionalWriterFor(graph)
	if !ok {
		t.Fatal("unreachable")
	}
	before, err := backing.Get(seeded.ID)
	if err != nil {
		t.Fatalf("backing Get: %v", err)
	}
	if err := writer.UpdateIfMatch(seeded.ID, before.Revision, beads.UpdateOpts{Metadata: map[string]string{
		beadmeta.LeaseOwnerMetadataKey: "worker-local-3-pool",
	}}); err != nil {
		t.Fatalf("fenced write through the graph shape: %v", err)
	}
	if after, err := backing.Get(seeded.ID); err != nil || after.Metadata[beadmeta.LeaseOwnerMetadataKey] != "worker-local-3-pool" {
		t.Fatalf("graph shape did not reach the backing: %v err %v", after.Metadata, err)
	}
}

// TestPolicyStoreReportsAbsentConditionalWriter keeps the forwarding honest: the
// handle forwards a capability and never invents one. A backing that cannot
// fence must still report "no writer" so workerLeaseWriter answers its typed 501
// and writes nothing, and a cache standing in front of such a backing must
// refuse the fenced write rather than fall back to an unconditional update.
func TestPolicyStoreReportsAbsentConditionalWriter(t *testing.T) {
	t.Run("policy store over a backing with no conditional writer", func(t *testing.T) {
		base := beads.NewMemStore()
		created, err := base.Create(beads.Bead{Title: "unfenceable", Status: "open"})
		if err != nil {
			t.Fatalf("seed: %v", err)
		}
		shape := wrapStoreWithBeadPolicies(conditionalWriterHidden{Store: base}, &config.City{})
		if _, ok := beads.ConditionalWriterFor(shape); ok {
			t.Fatal("the policy wrapper reported a conditional writer its backing does not have: an absent capability would be served by a stamp that cannot be fenced")
		}
		// Nothing the routes do next can fence, so nothing was written.
		if after, err := base.Get(created.ID); err != nil || after.Metadata[beadmeta.LeaseOwnerMetadataKey] != "" {
			t.Fatalf("a store with no capability left a lease behind: %v err %v", after.Metadata, err)
		}
	})

	t.Run("cache over such a backing refuses the fenced write", func(t *testing.T) {
		base := beads.NewMemStore()
		created, err := base.Create(beads.Bead{Title: "unfenceable behind the cache", Status: "open"})
		if err != nil {
			t.Fatalf("seed: %v", err)
		}
		shape := workerVerbStoreShape(t, conditionalWriterHidden{Store: base})
		// The cache advertises the capability by forwarding, so the pre-write
		// probe passes and the absent capability surfaces from the call. It must
		// be the named error — the route maps that to the same typed 501
		// (internal/api/handler_worker.go stampWorkerLease), never an
		// unconditional write.
		writer, ok := beads.ConditionalWriterFor(shape)
		if !ok {
			t.Fatal("test premise: the forwarding cache should advertise the capability for the probe to reach the call")
		}
		fresh, err := shape.Get(created.ID)
		if err != nil {
			t.Fatalf("shape Get: %v", err)
		}
		err = writer.UpdateIfMatch(created.ID, fresh.Revision, beads.UpdateOpts{Metadata: map[string]string{
			beadmeta.LeaseOwnerMetadataKey: "worker-local-3-pool",
		}})
		if !errors.Is(err, beads.ErrConditionalWriteUnsupported) {
			t.Fatalf("fenced write over an incapable backing = %v, want ErrConditionalWriteUnsupported", err)
		}
		if after, err := base.Get(created.ID); err != nil || after.Metadata[beadmeta.LeaseOwnerMetadataKey] != "" {
			t.Fatalf("the refused stamp still wrote to the backing: %v err %v", after.Metadata, err)
		}
	})
}

// conditionalWriterHidden embeds the beads.Store INTERFACE, so the leaf's
// conditional-write methods are not promoted: a backing that genuinely cannot
// fence a write, while still reading, writing, and claiming normally.
type conditionalWriterHidden struct {
	beads.Store
}
