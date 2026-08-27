package session

import (
	"errors"
	"maps"
	"reflect"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/rollout/gate"
)

func TestRebindTriggerIfMatchCommitsCompleteProvenanceUnderOneRevisionFence(t *testing.T) {
	front, store, id := conditionalTriggerBindingStore(t)
	pre, persisted, err := front.GetPersistedResponse(id)
	if err != nil {
		t.Fatalf("read preimage: %v", err)
	}
	binding := TriggerBinding{
		WorkID:         "ga-next",
		StoreRef:       "city:test-city",
		BrainParentSID: "sid-next",
		Pack:           "review-pack",
		Workspace:      "workspace-b",
		WorkDir:        "/city/worker-root/review-pack/workspace-b",
	}

	got, committed, err := front.RebindTriggerIfMatch(pre, persisted, binding)
	if err != nil {
		t.Fatalf("rebind trigger: %v", err)
	}
	after, err := store.Get(id)
	if err != nil {
		t.Fatalf("read rebound row: %v", err)
	}
	// Equality and inequality only. `persisted.Revision+1` held on the native
	// counter stores and on nothing else; the contract promises a FRESH opaque
	// token, not the next one (ga-f7v2ft.144).
	if !beads.RevisionKnown(after.Revision) || after.Revision == persisted.Revision {
		t.Fatalf("rebound revision = %d, want a fresh token off the fenced %d", after.Revision, persisted.Revision)
	}
	if !maps.Equal(after.Metadata, committed.Apply(persisted.Metadata)) {
		t.Fatalf("returned patch does not name the durable image\n durable=%#v\n  patch=%#v", after.Metadata, committed)
	}
	want := map[string]string{
		beadmeta.TriggerBeadIDMetadataKey:       binding.WorkID,
		beadmeta.TriggerBeadStoreRefMetadataKey: binding.StoreRef,
		beadmeta.BrainParentSIDMetadataKey:      binding.BrainParentSID,
		beadmeta.PackMetadataKey:                binding.Pack,
		beadmeta.PackWorkspaceMetadataKey:       binding.Workspace,
		beadmeta.WorkDirMetadataKey:             binding.WorkDir,
		beadmeta.LegacyWorkDirMetadataKey:       binding.WorkDir,
	}
	for key, value := range want {
		if after.Metadata[key] != value {
			t.Errorf("rebound metadata[%q] = %q, want %q", key, after.Metadata[key], value)
		}
	}
	if !binding.Matches(got) {
		t.Fatalf("returned Info does not match binding: %+v", got)
	}
}

func TestRebindTriggerIfMatchFailsClosedOnStaleRevision(t *testing.T) {
	front, store, id := conditionalTriggerBindingStore(t)
	pre, persisted, err := front.GetPersistedResponse(id)
	if err != nil {
		t.Fatalf("read preimage: %v", err)
	}
	if err := store.SetMetadata(id, "unrelated", "newer"); err != nil {
		t.Fatalf("advance row revision: %v", err)
	}

	got, _, err := front.RebindTriggerIfMatch(pre, persisted, TriggerBinding{
		WorkID:   "ga-next",
		StoreRef: "city:test-city",
		WorkDir:  "/city/worker",
	})
	if !beads.IsPreconditionFailed(err) {
		t.Fatalf("stale rebind error = %v, want precondition failure", err)
	}
	if !reflect.DeepEqual(got, pre) {
		t.Fatalf("stale rebind returned changed Info\n got=%+v\nwant=%+v", got, pre)
	}
	after, err := store.Get(id)
	if err != nil {
		t.Fatalf("read row after stale rebind: %v", err)
	}
	if after.Metadata[beadmeta.TriggerBeadIDMetadataKey] != "ga-old" {
		t.Fatalf("stale rebind changed durable trigger to %q", after.Metadata[beadmeta.TriggerBeadIDMetadataKey])
	}
}

// TestRebindTriggerIfMatchAcceptsASignedRevision is the ga-f7v2ft.141 red for
// this fence. RebindTriggerIfMatch gated its CAS on `persisted.Revision <= 0`
// and never falls back to an unconditional write, so on a bd row carrying a
// NEGATIVE revision — roughly half of every city's — the rebind refused every
// time with "not an open revisioned row" and the trigger provenance cluster was
// never replaced. The revision contract permits equality only; the sign says
// nothing about whether the row is revisioned.
func TestRebindTriggerIfMatchAcceptsASignedRevision(t *testing.T) {
	const negativeRevision = int64(-444891346261809656) // observed live, v59 journey
	front, store, id := signedRevisionTriggerBindingStore(t, negativeRevision)
	pre, persisted, err := front.GetPersistedResponse(id)
	if err != nil {
		t.Fatalf("read preimage: %v", err)
	}
	if persisted.Revision != negativeRevision {
		t.Fatalf("test premise: persisted revision = %d, want the seeded signed revision %d", persisted.Revision, negativeRevision)
	}
	binding := TriggerBinding{
		WorkID:   "ga-next",
		StoreRef: "city:test-city",
		WorkDir:  "/city/worker",
	}

	got, _, err := front.RebindTriggerIfMatch(pre, persisted, binding)
	if err != nil {
		t.Fatalf("rebind on revision %d: %v: the fence is gated on the revision's SIGN, so a real revision reads as unrevisioned", negativeRevision, err)
	}
	if !binding.Matches(got) {
		t.Fatalf("returned Info does not match binding: %+v", got)
	}
	after, err := store.Get(id)
	if err != nil {
		t.Fatalf("read rebound row: %v", err)
	}
	if after.Metadata[beadmeta.TriggerBeadIDMetadataKey] != binding.WorkID {
		t.Fatalf("rebound trigger = %q, want %q", after.Metadata[beadmeta.TriggerBeadIDMetadataKey], binding.WorkID)
	}
}

// TestRebindTriggerIfMatchFailsClosedOnAStaleSignedRevision is the other arm:
// accepting a negative revision must not weaken the fence. A concurrent writer
// that moved the same signed-revision row still wins.
func TestRebindTriggerIfMatchFailsClosedOnAStaleSignedRevision(t *testing.T) {
	front, store, id := signedRevisionTriggerBindingStore(t, -1700993557661895454)
	pre, persisted, err := front.GetPersistedResponse(id)
	if err != nil {
		t.Fatalf("read preimage: %v", err)
	}
	if err := store.SetMetadata(id, "unrelated", "newer"); err != nil {
		t.Fatalf("advance row revision: %v", err)
	}

	got, _, err := front.RebindTriggerIfMatch(pre, persisted, TriggerBinding{
		WorkID:   "ga-next",
		StoreRef: "city:test-city",
		WorkDir:  "/city/worker",
	})
	if !beads.IsPreconditionFailed(err) {
		t.Fatalf("stale signed-revision rebind error = %v, want precondition failure", err)
	}
	if !reflect.DeepEqual(got, pre) {
		t.Fatalf("stale rebind returned changed Info\n got=%+v\nwant=%+v", got, pre)
	}
	after, err := store.Get(id)
	if err != nil {
		t.Fatalf("read row after stale rebind: %v", err)
	}
	if after.Metadata[beadmeta.TriggerBeadIDMetadataKey] != "ga-old" {
		t.Fatalf("stale rebind changed durable trigger to %q", after.Metadata[beadmeta.TriggerBeadIDMetadataKey])
	}
}

// TestRebindTriggerIfMatchRefusesAnUnrevisionedRow keeps the zero sentinel a
// refusal: zero means the store did not supply a revision, so there is nothing
// to fence on and this call must never degrade to an unconditional write.
func TestRebindTriggerIfMatchRefusesAnUnrevisionedRow(t *testing.T) {
	front, store, id := signedRevisionTriggerBindingStore(t, 0)
	pre, persisted, err := front.GetPersistedResponse(id)
	if err != nil {
		t.Fatalf("read preimage: %v", err)
	}

	got, _, err := front.RebindTriggerIfMatch(pre, persisted, TriggerBinding{
		WorkID:   "ga-next",
		StoreRef: "city:test-city",
		WorkDir:  "/city/worker",
	})
	if err == nil || !strings.Contains(err.Error(), "not an open revisioned row") {
		t.Fatalf("unrevisioned rebind error = %v, want the revisioned-row refusal", err)
	}
	if !reflect.DeepEqual(got, pre) {
		t.Fatalf("refused rebind returned changed Info\n got=%+v\nwant=%+v", got, pre)
	}
	after, err := store.Get(id)
	if err != nil {
		t.Fatalf("read row after refused rebind: %v", err)
	}
	if after.Metadata[beadmeta.TriggerBeadIDMetadataKey] != "ga-old" {
		t.Fatalf("refused rebind changed durable trigger to %q", after.Metadata[beadmeta.TriggerBeadIDMetadataKey])
	}
}

func TestRebindTriggerIfMatchRefusesStoreWithoutResolvedConditionalWrites(t *testing.T) {
	store := beads.NewMemStore()
	created, err := store.Create(triggerBindingSessionBead())
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	front := NewStore(beads.SessionStore{Store: store})
	pre, persisted, err := front.GetPersistedResponse(created.ID)
	if err != nil {
		t.Fatalf("read preimage: %v", err)
	}

	got, _, err := front.RebindTriggerIfMatch(pre, persisted, TriggerBinding{
		WorkID:   "ga-next",
		StoreRef: "city:test-city",
		WorkDir:  "/city/worker",
	})
	if !errors.Is(err, beads.ErrConditionalWriteUnsupported) {
		t.Fatalf("unresolved conditional-write error = %v, want ErrConditionalWriteUnsupported", err)
	}
	if !reflect.DeepEqual(got, pre) {
		t.Fatalf("refused rebind returned changed Info\n got=%+v\nwant=%+v", got, pre)
	}
}

func TestRebindTriggerIfMatchExactReplayIsNoOp(t *testing.T) {
	store := beads.NewMemStore()
	bead := triggerBindingSessionBead()
	bead.Metadata[beadmeta.TriggerBeadIDMetadataKey] = "ga-next"
	bead.Metadata[beadmeta.TriggerBeadStoreRefMetadataKey] = "city:test-city"
	bead.Metadata[beadmeta.BrainParentSIDMetadataKey] = ""
	bead.Metadata[beadmeta.PackMetadataKey] = ""
	bead.Metadata[beadmeta.PackWorkspaceMetadataKey] = ""
	bead.Metadata[beadmeta.WorkDirMetadataKey] = "/city/worker"
	bead.Metadata[beadmeta.LegacyWorkDirMetadataKey] = "/city/worker"
	created, err := store.Create(bead)
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	front := NewStore(beads.SessionStore{Store: store})
	pre, persisted, err := front.GetPersistedResponse(created.ID)
	if err != nil {
		t.Fatalf("read preimage: %v", err)
	}

	got, committed, err := front.RebindTriggerIfMatch(pre, persisted, TriggerBinding{
		WorkID:   "ga-next",
		StoreRef: "city:test-city",
		WorkDir:  "/city/worker",
	})
	if err != nil {
		t.Fatalf("exact replay: %v", err)
	}
	if !reflect.DeepEqual(got, pre) {
		t.Fatalf("exact replay changed Info\n got=%+v\nwant=%+v", got, pre)
	}
	// The no-op replay commits nothing, so the patch is empty and the row keeps
	// its token. A caller that demanded the revision MOVE would refuse its own
	// idempotent replay; the committed patch is what tells the two apart.
	if len(committed) != 0 {
		t.Fatalf("exact replay committed patch = %#v, want empty", committed)
	}
	after, err := store.Get(created.ID)
	if err != nil {
		t.Fatalf("read replayed row: %v", err)
	}
	if after.Revision != persisted.Revision {
		t.Fatalf("exact replay revision = %d, want unchanged %d", after.Revision, persisted.Revision)
	}
}

func conditionalTriggerBindingStore(t *testing.T) (*Store, beads.Store, string) {
	t.Helper()
	opened, err := beads.OpenStoreAtForCity(t.Context(), beads.StoreOpenOptions{
		Provider:          "file",
		OpenFileStore:     func() (beads.Store, error) { return beads.NewMemStore(), nil },
		ConditionalWrites: gate.Auto,
	})
	if err != nil {
		t.Fatalf("open conditional-write store: %v", err)
	}
	created, err := opened.Store.Create(triggerBindingSessionBead())
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	return NewStore(beads.SessionStore{Store: opened.Store}), opened.Store, created.ID
}

// signedRevisionTriggerBindingStore is conditionalTriggerBindingStore for a row
// whose revision is a SIGNED bd token rather than the native stores' small
// positive counter. The row is minted through the ordinary Create path first so
// its shape is identical, then re-seeded verbatim under the given revision.
func signedRevisionTriggerBindingStore(t *testing.T, revision int64) (*Store, beads.Store, string) {
	t.Helper()
	scratch := beads.NewMemStore()
	created, err := scratch.Create(triggerBindingSessionBead())
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	created.Revision = revision
	opened, err := beads.OpenStoreAtForCity(t.Context(), beads.StoreOpenOptions{
		Provider:          "file",
		OpenFileStore:     func() (beads.Store, error) { return beads.NewMemStoreFrom(1, []beads.Bead{created}, nil), nil },
		ConditionalWrites: gate.Auto,
	})
	if err != nil {
		t.Fatalf("open conditional-write store: %v", err)
	}
	return NewStore(beads.SessionStore{Store: opened.Store}), opened.Store, created.ID
}

func triggerBindingSessionBead() beads.Bead {
	return beads.Bead{
		Title:  "worker",
		Type:   BeadType,
		Labels: []string{LabelSession},
		Metadata: map[string]string{
			"state":                                 string(StateActive),
			beadmeta.TriggerBeadIDMetadataKey:       "ga-old",
			beadmeta.TriggerBeadStoreRefMetadataKey: "city:test-city",
			beadmeta.BrainParentSIDMetadataKey:      "sid-old",
			beadmeta.PackMetadataKey:                "old-pack",
			beadmeta.PackWorkspaceMetadataKey:       "old-workspace",
			beadmeta.WorkDirMetadataKey:             "/city/old",
			beadmeta.LegacyWorkDirMetadataKey:       "/city/old",
		},
	}
}
