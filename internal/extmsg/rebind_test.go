package extmsg

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
)

func TestBindingServiceReplaceRebindsActiveBinding(t *testing.T) {
	freezeTestClock(t)
	store := beads.NewMemStore()
	fabric := NewServices(store)
	svc := fabric.Bindings
	ref := testConversationRef()

	first, err := svc.Bind(context.Background(), testControllerCaller(), BindInput{
		Conversation: ref,
		AgentName:    "myrig/frontdesk",
		Now:          testNow(),
	})
	if err != nil {
		t.Fatalf("Bind(frontdesk): %v", err)
	}

	// Plain bind still conflicts — handoff is explicitly opt-in.
	if _, err := svc.Bind(context.Background(), testControllerCaller(), BindInput{
		Conversation: ref,
		AgentName:    "myrig/specialist",
		Now:          testNow(),
	}); !errors.Is(err, ErrBindingConflict) {
		t.Fatalf("Bind(no replace) error = %v, want ErrBindingConflict", err)
	}

	second, err := svc.Bind(context.Background(), testControllerCaller(), BindInput{
		Conversation: ref,
		AgentName:    "myrig/specialist",
		Replace:      true,
		Now:          testNow().Add(time.Minute),
	})
	if err != nil {
		t.Fatalf("Bind(replace): %v", err)
	}
	if second.AgentName != "myrig/specialist" {
		t.Fatalf("rebound AgentName = %q, want myrig/specialist", second.AgentName)
	}
	if second.BindingGeneration != first.BindingGeneration+1 {
		t.Fatalf("BindingGeneration = %d, want %d", second.BindingGeneration, first.BindingGeneration+1)
	}

	active, err := svc.ResolveByConversation(context.Background(), ref)
	if err != nil {
		t.Fatalf("ResolveByConversation: %v", err)
	}
	if active == nil || active.ID != second.ID {
		t.Fatalf("active = %#v, want the rebound binding", active)
	}

	// Membership followed the handoff: the old agent's membership is gone,
	// the new agent's exists.
	members, err := fabric.Transcript.ListMemberships(context.Background(), testControllerCaller(), ref)
	if err != nil {
		t.Fatalf("ListMemberships: %v", err)
	}
	if len(members) != 1 || members[0].SessionID != "myrig/specialist" {
		t.Fatalf("memberships = %#v, want one keyed myrig/specialist", members)
	}
}

func TestBindingServiceHandoffKeepsBindingWhenDeliveryClearFails(t *testing.T) {
	freezeTestClock(t)
	store := beads.NewMemStore()
	delivery := &failingDeliveryContextService{err: errors.New("boom")}
	svc := newBindingService(store, delivery, nil, newBindingLockPool())
	ref := testConversationRef()

	// Displaced binding to a concrete session so the handoff attempts to clear
	// that session's delivery contexts.
	displaced, err := svc.Bind(context.Background(), testControllerCaller(), BindInput{
		Conversation: ref,
		SessionID:    "sess-a",
		Now:          testNow(),
	})
	if err != nil {
		t.Fatalf("Bind(displaced): %v", err)
	}

	// Handoff to a new session: clearing the displaced session's delivery fails.
	// The clear runs before the binding swap, so the displaced binding must stay
	// intact and the whole handoff is retryable — never a window where the
	// conversation is left unbound.
	if _, err := svc.Bind(context.Background(), testControllerCaller(), BindInput{
		Conversation: ref,
		SessionID:    "sess-b",
		Replace:      true,
		Now:          testNow().Add(time.Minute),
	}); err == nil {
		t.Fatal("Bind(handoff) error = nil, want delivery clear failure")
	}

	item, err := store.Get(displaced.ID)
	if err != nil {
		t.Fatalf("Get(displaced): %v", err)
	}
	if item.Status != "open" {
		t.Fatalf("displaced binding status after failed clear = %q, want open", item.Status)
	}
	active, err := svc.ResolveByConversation(context.Background(), ref)
	if err != nil {
		t.Fatalf("ResolveByConversation: %v", err)
	}
	if active == nil || active.ID != displaced.ID || active.SessionID != "sess-a" {
		t.Fatalf("active = %#v, want the displaced sess-a binding still bound", active)
	}
	// The replacement binding must not have been created.
	replacement, err := svc.ListBySession(context.Background(), "sess-b")
	if err != nil {
		t.Fatalf("ListBySession(sess-b): %v", err)
	}
	if len(replacement) != 0 {
		t.Fatalf("ListBySession(sess-b) = %#v, want no replacement binding created", replacement)
	}
}

func TestBindingServiceHandoffRollsBackMembershipWhenReplacementFails(t *testing.T) {
	freezeTestClock(t)
	// A transactional store: the swap rolls back atomically on failure.
	store := &atomicTxStore{MemStore: beads.NewMemStore()}
	ref := testConversationRef()

	// Set up the displaced binding with a real transcript so its membership exists.
	realSvc := NewServices(store).Bindings
	displaced, err := realSvc.Bind(context.Background(), testControllerCaller(), BindInput{
		Conversation: ref,
		AgentName:    "myrig/frontdesk",
		Now:          testNow(),
	})
	if err != nil {
		t.Fatalf("Bind(displaced): %v", err)
	}

	// Hand off through a service whose transcript fails the replacement's
	// membership ensure. On a transactional store the swap (close displaced +
	// create replacement + ensure replacement membership) must roll back as a
	// unit: the displaced binding stays the active one, no replacement bead is
	// left behind, and the displaced membership dropped before the swap is
	// re-ensured.
	flaky := &flakyTranscriptService{failEnsureCount: 1, err: errors.New("boom")}
	flakySvc := newBindingService(store, nil, flaky, newBindingLockPool())
	if _, err := flakySvc.Bind(context.Background(), testControllerCaller(), BindInput{
		Conversation: ref,
		AgentName:    "myrig/specialist",
		Replace:      true,
		Now:          testNow().Add(time.Minute),
	}); err == nil {
		t.Fatal("Bind(handoff) error = nil, want replacement membership failure")
	}

	if flaky.removeCalls != 1 {
		t.Fatalf("removeCalls = %d, want 1 (displaced membership dropped once)", flaky.removeCalls)
	}
	if flaky.ensureCalls != 2 {
		t.Fatalf("ensureCalls = %d, want 2 (replacement ensure failed, displaced membership re-ensured)", flaky.ensureCalls)
	}

	// The displaced binding is still the one and only active binding — not a
	// leaked replacement that selectActiveBinding would reject as an invariant
	// violation.
	bs := flakySvc.(*bindingService)
	remaining, err := bs.listBindingsForConversation(ref)
	if err != nil {
		t.Fatalf("listBindingsForConversation: %v", err)
	}
	var actives []SessionBindingRecord
	for _, b := range remaining {
		if b.Status == BindingActive {
			actives = append(actives, b)
		}
		if b.AgentName == "myrig/specialist" {
			t.Fatalf("found leaked replacement binding %#v, want the swap rolled back", b)
		}
	}
	if len(actives) != 1 {
		t.Fatalf("active bindings = %d (%#v), want exactly 1 (the displaced)", len(actives), remaining)
	}
	if actives[0].ID != displaced.ID || actives[0].AgentName != "myrig/frontdesk" {
		t.Fatalf("active = %#v, want the displaced frontdesk binding %s", actives[0], displaced.ID)
	}

	active, err := flakySvc.ResolveByConversation(context.Background(), ref)
	if err != nil {
		t.Fatalf("ResolveByConversation: %v", err)
	}
	if active == nil || active.ID != displaced.ID {
		t.Fatalf("active = %#v, want the displaced binding %s still bound", active, displaced.ID)
	}
}

// TestBindingServiceHandoffReportsDisplacedMembershipRestoreFailure asserts that
// when the swap rolls back AND re-ensuring the displaced binding's membership
// also fails, the restore failure is surfaced on the returned error rather than
// being silently swallowed.
func TestBindingServiceHandoffReportsDisplacedMembershipRestoreFailure(t *testing.T) {
	freezeTestClock(t)
	store := &atomicTxStore{MemStore: beads.NewMemStore()}
	ref := testConversationRef()

	realSvc := NewServices(store).Bindings
	displaced, err := realSvc.Bind(context.Background(), testControllerCaller(), BindInput{
		Conversation: ref,
		AgentName:    "myrig/frontdesk",
		Now:          testNow(),
	})
	if err != nil {
		t.Fatalf("Bind(displaced): %v", err)
	}

	// failEnsureCount: 2 fails both the replacement ensure (driving the rollback)
	// and the subsequent displaced-membership restore.
	flaky := &flakyTranscriptService{failEnsureCount: 2, err: errors.New("boom")}
	flakySvc := newBindingService(store, nil, flaky, newBindingLockPool())
	_, err = flakySvc.Bind(context.Background(), testControllerCaller(), BindInput{
		Conversation: ref,
		AgentName:    "myrig/specialist",
		Replace:      true,
		Now:          testNow().Add(time.Minute),
	})
	if err == nil {
		t.Fatal("Bind(handoff) error = nil, want a reported failure")
	}
	if !errors.Is(err, ErrTranscriptSyncFailed) {
		t.Fatalf("error = %v, want it to wrap ErrTranscriptSyncFailed", err)
	}
	if !strings.Contains(err.Error(), "restore displaced transcript membership after failed handoff") {
		t.Fatalf("error = %v, want it to report the displaced membership restore failure", err)
	}
	if flaky.ensureCalls != 2 {
		t.Fatalf("ensureCalls = %d, want 2 (replacement ensure + displaced restore both attempted)", flaky.ensureCalls)
	}

	// The displaced binding still remains the sole active binding.
	bs := flakySvc.(*bindingService)
	remaining, err := bs.listBindingsForConversation(ref)
	if err != nil {
		t.Fatalf("listBindingsForConversation: %v", err)
	}
	var actives []SessionBindingRecord
	for _, b := range remaining {
		if b.Status == BindingActive {
			actives = append(actives, b)
		}
	}
	if len(actives) != 1 || actives[0].ID != displaced.ID {
		t.Fatalf("active bindings = %#v, want only the displaced %s", actives, displaced.ID)
	}
}

// TestBindingServiceHandoffConvergesOnNonAtomicStore asserts that on a
// non-transactional backend whose Tx cannot close the displaced binding and
// create the replacement atomically, a failed handoff never leaves two active
// bindings (the unrecoverable invariant violation) and a retry converges on the
// requested target.
func TestBindingServiceHandoffConvergesOnNonAtomicStore(t *testing.T) {
	freezeTestClock(t)
	// bdLikeStore stages Close inside Tx but persists Create immediately, like
	// BdStore: an in-Tx close+create swap would leave two active bindings here.
	store := &bdLikeStore{MemStore: beads.NewMemStore()}
	ref := testConversationRef()

	realSvc := NewServices(store).Bindings
	displaced, err := realSvc.Bind(context.Background(), testControllerCaller(), BindInput{
		Conversation: ref,
		AgentName:    "myrig/frontdesk",
		Now:          testNow(),
	})
	if err != nil {
		t.Fatalf("Bind(displaced): %v", err)
	}

	flaky := &flakyTranscriptService{failEnsureCount: 1, err: errors.New("boom")}
	flakySvc := newBindingService(store, nil, flaky, newBindingLockPool())
	if _, err := flakySvc.Bind(context.Background(), testControllerCaller(), BindInput{
		Conversation: ref,
		AgentName:    "myrig/specialist",
		Replace:      true,
		Now:          testNow().Add(time.Minute),
	}); err == nil {
		t.Fatal("Bind(handoff) error = nil, want replacement membership failure")
	}

	// The failed handoff must not have produced two active bindings: at most one
	// active binding may remain, so a later resolve/bind never hits the
	// multiple-active invariant violation.
	bs := flakySvc.(*bindingService)
	remaining, err := bs.listBindingsForConversation(ref)
	if err != nil {
		t.Fatalf("listBindingsForConversation: %v", err)
	}
	var actives []SessionBindingRecord
	for _, b := range remaining {
		if b.Status == BindingActive {
			actives = append(actives, b)
		}
	}
	if len(actives) > 1 {
		t.Fatalf("active bindings = %d (%#v), want at most 1 — the brick is two active bindings", len(actives), remaining)
	}
	for _, b := range remaining {
		if b.ID == displaced.ID && b.Status == BindingActive {
			t.Fatalf("displaced binding %s is still active, want it closed by the close-first swap", displaced.ID)
		}
	}

	// A retry of the same handoff target converges: it rebinds the (now active)
	// replacement and re-ensures its transcript membership.
	retry, err := flakySvc.Bind(context.Background(), testControllerCaller(), BindInput{
		Conversation: ref,
		AgentName:    "myrig/specialist",
		Replace:      true,
		Now:          testNow().Add(2 * time.Minute),
	})
	if err != nil {
		t.Fatalf("Bind(retry) error = %v, want convergence", err)
	}
	if retry.AgentName != "myrig/specialist" {
		t.Fatalf("retry AgentName = %q, want myrig/specialist", retry.AgentName)
	}
	if flaky.ensureCalls < 2 {
		t.Fatalf("ensureCalls = %d, want the retry to re-ensure the replacement membership", flaky.ensureCalls)
	}
}

func TestBindingServiceReplaceAcrossKindsAndIdempotence(t *testing.T) {
	freezeTestClock(t)
	store := beads.NewMemStore()
	svc := NewServices(store).Bindings
	ref := testConversationRef()

	if _, err := svc.Bind(context.Background(), testControllerCaller(), BindInput{
		Conversation: ref,
		SessionID:    "sess-a",
		Now:          testNow(),
	}); err != nil {
		t.Fatalf("Bind(session): %v", err)
	}

	// Session binding -> agent binding via replace.
	rebound, err := svc.Bind(context.Background(), testControllerCaller(), BindInput{
		Conversation: ref,
		AgentName:    "myrig/helper",
		Replace:      true,
		Now:          testNow().Add(time.Minute),
	})
	if err != nil {
		t.Fatalf("Bind(replace session->agent): %v", err)
	}
	if rebound.AgentName != "myrig/helper" || rebound.SessionID != "" {
		t.Fatalf("rebound = %#v, want agent binding", rebound)
	}

	// Replace to the same target is idempotent — keeps the record.
	same, err := svc.Bind(context.Background(), testControllerCaller(), BindInput{
		Conversation: ref,
		AgentName:    "myrig/helper",
		Replace:      true,
		Now:          testNow().Add(2 * time.Minute),
	})
	if err != nil {
		t.Fatalf("Bind(replace same target): %v", err)
	}
	if same.ID != rebound.ID || same.BindingGeneration != rebound.BindingGeneration {
		t.Fatalf("idempotent replace changed record: %#v vs %#v", same, rebound)
	}

	// Replace with no active binding behaves like a plain bind.
	if _, err := svc.Unbind(context.Background(), testControllerCaller(), UnbindInput{
		Conversation: &ref,
		Now:          testNow().Add(3 * time.Minute),
	}); err != nil {
		t.Fatalf("Unbind: %v", err)
	}
	fresh, err := svc.Bind(context.Background(), testControllerCaller(), BindInput{
		Conversation: ref,
		SessionID:    "sess-b",
		Replace:      true,
		Now:          testNow().Add(4 * time.Minute),
	})
	if err != nil {
		t.Fatalf("Bind(replace on unbound): %v", err)
	}
	if fresh.SessionID != "sess-b" {
		t.Fatalf("fresh = %#v, want sess-b session binding", fresh)
	}
}

// atomicTxStore wraps a MemStore with an atomic Tx: writes made inside the
// callback are rolled back when the callback returns an error — created beads are
// deleted and closed beads are reopened — modeling a transactional backend such
// as NativeDoltStore. It covers exactly the write set of the handoff swap
// (close the displaced binding, create the replacement); it does not roll back
// Update/SetMetadataBatch, which that swap does not use.
type atomicTxStore struct {
	*beads.MemStore
}

func (s *atomicTxStore) AtomicTx() bool { return true }

func (s *atomicTxStore) Tx(_ string, fn func(beads.Tx) error) error {
	rec := &atomicRecordingTx{store: s.MemStore}
	if err := fn(rec); err != nil {
		rec.rollback()
		return err
	}
	return nil
}

type atomicRecordingTx struct {
	store   *beads.MemStore
	created []string
	reopen  []string
}

func (tx *atomicRecordingTx) Create(b beads.Bead) (beads.Bead, error) {
	created, err := tx.store.Create(b)
	if err == nil {
		tx.created = append(tx.created, created.ID)
	}
	return created, err
}

func (tx *atomicRecordingTx) Update(id string, opts beads.UpdateOpts) error {
	return tx.store.Update(id, opts)
}

func (tx *atomicRecordingTx) SetMetadataBatch(id string, kvs map[string]string) error {
	return tx.store.SetMetadataBatch(id, kvs)
}

func (tx *atomicRecordingTx) Close(id string) error {
	prior, getErr := tx.store.Get(id)
	if err := tx.store.Close(id); err != nil {
		return err
	}
	if getErr == nil && prior.Status != "closed" {
		tx.reopen = append(tx.reopen, id)
	}
	return nil
}

func (tx *atomicRecordingTx) rollback() {
	for _, id := range tx.created {
		_ = tx.store.Delete(id)
	}
	for _, id := range tx.reopen {
		_ = tx.store.Reopen(id)
	}
}

// bdLikeStore models an external store such as BdStore: Create persists
// immediately, while Close inside a Tx is staged and applied only if the
// callback succeeds, so a failed Tx discards the staged close but keeps the
// persisted create. A close-of-displaced + create-of-replacement done inside one
// Tx would therefore leave two active bindings on this backend; the handoff's
// close-first ordering must converge here instead.
type bdLikeStore struct {
	*beads.MemStore
}

func (s *bdLikeStore) AtomicTx() bool { return false }

func (s *bdLikeStore) Tx(_ string, fn func(beads.Tx) error) error {
	staged := &bdLikeTx{store: s.MemStore}
	if err := fn(staged); err != nil {
		return err // staged closes discarded; immediate creates persist
	}
	return staged.apply()
}

type bdLikeTx struct {
	store  *beads.MemStore
	closes []string
}

func (tx *bdLikeTx) Create(b beads.Bead) (beads.Bead, error) { return tx.store.Create(b) }

func (tx *bdLikeTx) Update(id string, opts beads.UpdateOpts) error {
	return tx.store.Update(id, opts)
}

func (tx *bdLikeTx) SetMetadataBatch(id string, kvs map[string]string) error {
	return tx.store.SetMetadataBatch(id, kvs)
}

func (tx *bdLikeTx) Close(id string) error {
	tx.closes = append(tx.closes, id)
	return nil
}

func (tx *bdLikeTx) apply() error {
	for _, id := range tx.closes {
		if err := tx.store.Close(id); err != nil {
			return err
		}
	}
	return nil
}
