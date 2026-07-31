package cityartifact_test

import (
	"context"
	"errors"
	"testing"

	"github.com/gastownhall/gascity/pkg/cityartifact"
)

func producerOn(t *testing.T, api cityartifact.API, src cityartifact.Source, store cityartifact.Store) *cityartifact.Producer {
	t.Helper()
	p, err := cityartifact.NewProducer(api, cityartifact.Mapper{Source: src}, store)
	if err != nil {
		t.Fatalf("NewProducer: %v", err)
	}
	return p
}

// AC2 happy path: an identical second push reuses accepted state — no second
// artifact, no second part.
func TestIdenticalPushIsIdempotent(t *testing.T) {
	ctx := context.Background()
	f := authorizedFake()
	store := cityartifact.NewMemoryStore()
	p := producerOn(t, f.Client("src_city_1", "gascity"), citySource(), store)

	first, err := p.Push(ctx, sampleArtifact())
	if err != nil {
		t.Fatalf("first Push: %v", err)
	}
	second, err := p.Push(ctx, sampleArtifact())
	if err != nil {
		t.Fatalf("second Push: %v", err)
	}
	if second.ArtifactID != first.ArtifactID {
		t.Fatalf("second push minted %q, first was %q", second.ArtifactID, first.ArtifactID)
	}
	if second.Uploaded != 0 || second.Skipped != 2 {
		t.Errorf("second push uploaded %d skipped %d, want 0/2", second.Uploaded, second.Skipped)
	}
	if got := f.PartCount(first.ArtifactID); got != 2 {
		t.Errorf("server stored %d parts, want 2", got)
	}
	if f.ArtifactCount() != 1 {
		t.Errorf("tenant holds %d artifacts, want 1", f.ArtifactCount())
	}
}

// AC2: a lost response. The checkpoint never landed, so the producer replays
// every request — and the derived keys make the server return the accepted state
// instead of creating a second artifact or a third part.
func TestLostCheckpointReplaysRatherThanDuplicates(t *testing.T) {
	ctx := context.Background()
	f := authorizedFake()
	first := producerOn(t, f.Client("src_city_1", "gascity"), citySource(), cityartifact.NewMemoryStore())
	res, err := first.Push(ctx, sampleArtifact())
	if err != nil {
		t.Fatalf("first Push: %v", err)
	}

	// Fresh store: the producer believes it has sent nothing at all.
	amnesiac := producerOn(t, f.Client("src_city_1", "gascity"), citySource(), cityartifact.NewMemoryStore())
	again, err := amnesiac.Push(ctx, sampleArtifact())
	if err != nil {
		t.Fatalf("replay Push: %v", err)
	}
	if again.ArtifactID != res.ArtifactID {
		t.Fatalf("replay minted artifact %q, original was %q", again.ArtifactID, res.ArtifactID)
	}
	if f.ArtifactCount() != 1 {
		t.Errorf("replay created %d artifacts, want 1", f.ArtifactCount())
	}
	if got := f.PartCount(res.ArtifactID); got != 2 {
		t.Errorf("replay stored %d parts, want 2", got)
	}
}

// AC2 happy path: a restart mid-upload resumes at the next part and re-sends
// none of the acknowledged ones.
func TestRestartResumesWithoutDuplicateParts(t *testing.T) {
	ctx := context.Background()
	f := authorizedFake()
	store := cityartifact.NewMemoryStore()
	p := producerOn(t, f.Client("src_city_1", "gascity"), citySource(), store)

	a := sampleArtifact()
	a.Parts = append(a.Parts, cityartifact.CityPart{Sequence: 3, Bytes: []byte("third part\n")})

	// Part 1 lands, part 2 fails.
	f.FailNext(cityartifact.OpUploadArtifactContent, nil)
	f.FailNext(cityartifact.OpUploadArtifactContent, errors.New("connection reset by peer"))
	res, err := p.Push(ctx, a)
	if !errors.Is(err, cityartifact.ErrUpstream) {
		t.Fatalf("err = %v, want ErrUpstream", err)
	}
	if res.Uploaded != 1 {
		t.Fatalf("uploaded %d before the fault, want 1", res.Uploaded)
	}
	if got := f.PartCount(res.ArtifactID); got != 1 {
		t.Fatalf("server stored %d parts, want 1", got)
	}
	if res.Finalized {
		t.Fatal("finalized despite a failed part")
	}

	// A new process, same durable checkpoint.
	resumed := producerOn(t, f.Client("src_city_1", "gascity"), citySource(), store)
	after, err := resumed.Push(ctx, a)
	if err != nil {
		t.Fatalf("resumed Push: %v", err)
	}
	if after.ArtifactID != res.ArtifactID {
		t.Errorf("resume minted a second artifact %q", after.ArtifactID)
	}
	if after.Uploaded != 2 || after.Skipped != 1 {
		t.Errorf("resume uploaded %d skipped %d, want 2/1", after.Uploaded, after.Skipped)
	}
	if got := f.PartCount(res.ArtifactID); got != 3 {
		t.Errorf("server stored %d parts, want 3", got)
	}
	if !after.Finalized {
		t.Error("resume did not finalize a complete manifest")
	}
	if f.ArtifactCount() != 1 {
		t.Errorf("tenant holds %d artifacts, want 1", f.ArtifactCount())
	}
}

// AC2: the checkpoint advances only over contiguous acknowledgements. A failed
// part leaves the frontier where it was, so the next attempt sends that part
// again rather than the one after it.
func TestCheckpointAdvancesOnlyOnAcknowledgement(t *testing.T) {
	ctx := context.Background()
	f := authorizedFake()
	store := cityartifact.NewMemoryStore()
	p := producerOn(t, f.Client("src_city_1", "gascity"), citySource(), store)

	f.FailNext(cityartifact.OpUploadArtifactContent, errors.New("upstream unavailable"))
	if _, err := p.Push(ctx, sampleArtifact()); !errors.Is(err, cityartifact.ErrUpstream) {
		t.Fatalf("err = %v, want ErrUpstream", err)
	}
	key := cityartifact.CheckpointKey(citySource(), "city-artifact-1")
	st, ok, err := store.Load(ctx, key)
	if err != nil || !ok {
		t.Fatalf("Load = %v, %v", ok, err)
	}
	if st.Frontier != 0 {
		t.Fatalf("frontier advanced to %d over an unacknowledged part", st.Frontier)
	}
	if st.ArtifactID == "" {
		t.Fatal("the acknowledged create was not checkpointed")
	}

	res, err := p.Push(ctx, sampleArtifact())
	if err != nil {
		t.Fatalf("retry Push: %v", err)
	}
	if res.Uploaded != 2 || res.Skipped != 0 {
		t.Errorf("retry uploaded %d skipped %d, want 2/0", res.Uploaded, res.Skipped)
	}
}

// AC2 edge: a changed hash under an acknowledged part stops the adapter and
// sends nothing.
func TestChangedManifestStopsTheAdapter(t *testing.T) {
	ctx := context.Background()
	f := authorizedFake()
	store := cityartifact.NewMemoryStore()
	p := producerOn(t, f.Client("src_city_1", "gascity"), citySource(), store)

	if _, err := p.Push(ctx, sampleArtifact()); err != nil {
		t.Fatalf("Push: %v", err)
	}
	before := len(f.Calls())

	rewritten := sampleArtifact()
	rewritten.Parts[0].Bytes = []byte("rewritten first part\n")
	res, err := p.Push(ctx, rewritten)
	if !errors.Is(err, cityartifact.ErrChangedManifest) {
		t.Fatalf("err = %v, want ErrChangedManifest", err)
	}
	for _, call := range f.Calls()[before:] {
		if call == cityartifact.OpUploadArtifactContent {
			t.Error("dispatched an upload for a rewritten part")
		}
	}
	if got := f.PartCount(res.ArtifactID); got != 2 {
		t.Errorf("server holds %d parts after a rewrite refusal, want the original 2", got)
	}
	// The checkpoint is intact: the original manifest still replays cleanly.
	if _, err := p.Push(ctx, sampleArtifact()); err != nil {
		t.Errorf("the original manifest no longer replays: %v", err)
	}
}

// AC2 edge: the artifact's own definition changing after creation is the same
// refusal — re-creating would fork the artifact.
func TestChangedDefinitionStopsTheAdapter(t *testing.T) {
	ctx := context.Background()
	f := authorizedFake()
	f.Authorize("proj-beta")
	store := cityartifact.NewMemoryStore()
	p := producerOn(t, f.Client("src_city_1", "gascity"), citySource(), store)

	if _, err := p.Push(ctx, sampleArtifact()); err != nil {
		t.Fatalf("Push: %v", err)
	}
	moved := sampleArtifact()
	moved.ProjectID = "proj-beta"
	if _, err := p.Push(ctx, moved); !errors.Is(err, cityartifact.ErrChangedManifest) {
		t.Fatalf("err = %v, want ErrChangedManifest", err)
	}
	if f.ArtifactCount() != 1 {
		t.Errorf("a changed definition forked into %d artifacts", f.ArtifactCount())
	}
}

// AC2 edge: a gap stops the adapter before anything is sent.
func TestGapStopsTheAdapter(t *testing.T) {
	ctx := context.Background()
	f := authorizedFake()
	p := producerOn(t, f.Client("src_city_1", "gascity"), citySource(), cityartifact.NewMemoryStore())

	gapped := sampleArtifact()
	gapped.Parts = []cityartifact.CityPart{
		{Sequence: 2, Bytes: []byte("second\n")},
		{Sequence: 3, Bytes: []byte("third\n")},
	}
	res, err := p.Push(ctx, gapped)
	if !errors.Is(err, cityartifact.ErrGap) {
		t.Fatalf("err = %v, want ErrGap", err)
	}
	if res.Uploaded != 0 {
		t.Errorf("uploaded %d parts across a gap", res.Uploaded)
	}
	if f.PartCount(res.ArtifactID) != 0 {
		t.Errorf("server stored parts across a gap")
	}
	if res.Finalized {
		t.Error("finalized across a gap")
	}
}

// AC2: a duplicate part sequence in the manifest is refused before any call.
func TestDuplicatePartSequenceRefused(t *testing.T) {
	ctx := context.Background()
	f := authorizedFake()
	p := producerOn(t, f.Client("src_city_1", "gascity"), citySource(), cityartifact.NewMemoryStore())

	dup := sampleArtifact()
	dup.Parts = append(dup.Parts, cityartifact.CityPart{Sequence: 1, Bytes: []byte("again\n")})
	if _, err := p.Push(ctx, dup); !errors.Is(err, cityartifact.ErrInvalidArtifact) {
		t.Fatalf("err = %v, want ErrInvalidArtifact", err)
	}
	if len(f.Calls()) != 0 {
		t.Errorf("dispatched %v before refusing a duplicate sequence", f.Calls())
	}
}

// AC2: credential rotation changes nothing. The key preimage holds the enrolled
// source, not the credential, so a rotated client replays instead of duplicating.
func TestCredentialRotationDoesNotDuplicate(t *testing.T) {
	ctx := context.Background()
	f := authorizedFake()
	store := cityartifact.NewMemoryStore()

	before := producerOn(t, f.Client("src_city_1", "gascity"), citySource(), store)
	res, err := before.Push(ctx, sampleArtifact())
	if err != nil {
		t.Fatalf("Push: %v", err)
	}

	// New credential, same enrolled source, same checkpoint.
	rotated := producerOn(t, f.Client("src_city_1", "gascity"), citySource(), store)
	after, err := rotated.Push(ctx, sampleArtifact())
	if err != nil {
		t.Fatalf("rotated Push: %v", err)
	}
	if after.ArtifactID != res.ArtifactID || after.Uploaded != 0 {
		t.Errorf("rotation produced %+v, want a replay of %q", after, res.ArtifactID)
	}
	if f.ArtifactCount() != 1 || f.PartCount(res.ArtifactID) != 2 {
		t.Errorf("rotation duplicated: %d artifacts, %d parts", f.ArtifactCount(), f.PartCount(res.ArtifactID))
	}
}

// AC2: post-finalize input is refused. Finalize is one-way and this adapter does
// not ask the server to reopen it.
func TestPostFinalizeInputRefused(t *testing.T) {
	ctx := context.Background()
	f := authorizedFake()
	store := cityartifact.NewMemoryStore()
	p := producerOn(t, f.Client("src_city_1", "gascity"), citySource(), store)

	res, err := p.Push(ctx, sampleArtifact())
	if err != nil {
		t.Fatalf("Push: %v", err)
	}
	grown := sampleArtifact()
	grown.Parts = append(grown.Parts, cityartifact.CityPart{Sequence: 3, Bytes: []byte("late\n")})
	if _, err := p.Push(ctx, grown); !errors.Is(err, cityartifact.ErrFinalized) {
		t.Fatalf("err = %v, want ErrFinalized", err)
	}
	if got := f.PartCount(res.ArtifactID); got != 2 {
		t.Errorf("server stored %d parts after finalize, want 2", got)
	}
}

// AC2: a declared reset restarts the checkpoint on the same key; an epoch that
// went backwards is drift, not a retry.
func TestEpochResetAndRegression(t *testing.T) {
	ctx := context.Background()
	f := authorizedFake()
	store := cityartifact.NewMemoryStore()
	p := producerOn(t, f.Client("src_city_1", "gascity"), citySource(), store)
	if _, err := p.Push(ctx, sampleArtifact()); err != nil {
		t.Fatalf("Push: %v", err)
	}

	behind := citySource()
	behind.Epoch = 0
	regressed := producerOn(t, f.Client("src_city_1", "gascity"), behind, store)
	if _, err := regressed.Push(ctx, sampleArtifact()); !errors.Is(err, cityartifact.ErrIdentityDrift) {
		t.Fatalf("err = %v, want ErrIdentityDrift for a backwards epoch", err)
	}

	// An epoch that merely went up is not a reset: a config typo and a real
	// reset are the same number, so the advance is refused without a
	// declaration naming what it resets from and who declared it.
	bumped := citySource()
	bumped.Epoch = 2
	undeclared := producerOn(t, f.Client("src_city_1", "gascity"), bumped, store)
	if _, err := undeclared.Push(ctx, sampleArtifact()); !errors.Is(err, cityartifact.ErrIdentityDrift) {
		t.Fatalf("undeclared epoch advance: err = %v, want ErrIdentityDrift", err)
	}

	ahead := citySource()
	ahead.Epoch = 2
	ahead.Reset = &cityartifact.ResetDeclaration{
		FromEpoch: 1, ToEpoch: 2, Reason: "source rebuilt", DeclaredBy: "ops@city",
	}
	reset := producerOn(t, f.Client("src_city_1", "gascity"), ahead, store)
	res, err := reset.Push(ctx, sampleArtifact())
	if err != nil {
		t.Fatalf("reset Push: %v", err)
	}
	key := cityartifact.CheckpointKey(ahead, "city-artifact-1")
	if key != cityartifact.CheckpointKey(citySource(), "city-artifact-1") {
		t.Fatalf("a reset landed on a different checkpoint key")
	}
	st, _, err := store.Load(ctx, key)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if st.Epoch != 2 {
		t.Errorf("checkpoint epoch = %d, want 2", st.Epoch)
	}
	if f.ArtifactCount() != 2 {
		t.Errorf("a declared reset produced %d artifacts, want a second generation", f.ArtifactCount())
	}
	if res.ArtifactID == "" {
		t.Error("reset produced no artifact")
	}
}

// AC2: a checkpoint that belongs to another enrolled source is drift, not state
// to inherit. This is the credential-swap case a shared checkpoint file makes
// reachable.
func TestCheckpointFromAnotherSourceIsDrift(t *testing.T) {
	ctx := context.Background()
	f := authorizedFake()
	store := cityartifact.NewMemoryStore()
	key := cityartifact.CheckpointKey(citySource(), "city-artifact-1")
	if err := store.Save(ctx, key, cityartifact.State{Epoch: 1, SourceID: "src_someone_else"}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	p := producerOn(t, f.Client("src_city_1", "gascity"), citySource(), store)
	if _, err := p.Push(ctx, sampleArtifact()); !errors.Is(err, cityartifact.ErrIdentityDrift) {
		t.Fatalf("err = %v, want ErrIdentityDrift", err)
	}
	if len(f.Calls()) != 0 {
		t.Errorf("dispatched %v against a foreign checkpoint", f.Calls())
	}
}

// AC2: checkpoints are namespaced by source, so the same City artifact ID under
// two enrolled sources is two artifacts and not a collision.
func TestCheckpointsAreNamespacedBySource(t *testing.T) {
	ctx := context.Background()
	f := authorizedFake()
	store := cityartifact.NewMemoryStore()
	mine := producerOn(t, f.Client("src_city_1", "gascity"), citySource(), store)
	if _, err := mine.Push(ctx, sampleArtifact()); err != nil {
		t.Fatalf("Push: %v", err)
	}
	other := cityartifact.Source{SourceID: "src_city_2", Kind: "gascity", Epoch: 1}
	stranger := producerOn(t, f.Client("src_city_2", "gascity"), other, store)
	// Same City artifact ID, different source: the key namespaces the source, so
	// this is a different checkpoint and must not inherit the first one.
	if _, err := stranger.Push(ctx, sampleArtifact()); err != nil {
		t.Fatalf("stranger Push: %v", err)
	}
	if f.ArtifactCount() != 2 {
		t.Errorf("two sources shared one artifact: count = %d", f.ArtifactCount())
	}
}
