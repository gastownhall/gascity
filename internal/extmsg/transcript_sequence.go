package extmsg

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"

	"github.com/gastownhall/gascity/internal/beads"
)

// Transcript sequence allocation.
//
// Every transcript entry is created at the pinned ID of its slot,
// <state-ID>-<sequence>-entry, through the store's PinnedIDCreator, so the
// store itself refuses a second entry at one sequence whichever process or
// store generation writes it. next_sequence is the published head: it
// advances by metadata compare-and-set from its exact stored value, one
// occupied slot at a time, so it never regresses and never skips a hole.
// An entry is readable once the head is past its sequence; Append returns
// success only after that.
//
// A store that cannot do both (transcriptAppendCapabilities) appends
// unguarded instead (appendUnguardedLocked), and the service logs that once.
// So does one append whose pinned create the store refuses before writing
// anything (pinnedCreateRefused).

const (
	// transcriptSlotAttempts bounds how many slots one Append tries while
	// other writers keep taking the head.
	transcriptSlotAttempts = 16
	// transcriptPublishAttempts bounds the failed (lost or errored) compare-
	// and-sets one publication tolerates; each successful advance resets it.
	transcriptPublishAttempts = 8
	// transcriptAppendTokenKey is the unprefixed entry metadata key holding
	// the per-Append random token that recognizes an ambiguous create of
	// an entry with no provider message ID. Unprefixed keys never reach the
	// decoded record's Metadata.
	transcriptAppendTokenKey = "append_token"
)

// errTranscriptUnresolved reports an append whose outcome bounded recovery
// could not settle: contention outlasted the retry budget, or a store fault
// persisted. A retry of the same message is safe.
var errTranscriptUnresolved = errors.New("transcript append unresolved; retry")

// beadLister is the list read a lookup runs over: a store, or its live reader.
type beadLister interface {
	List(query beads.ListQuery) ([]beads.Bead, error)
}

// transcriptEntryID is the pinned bead ID of the entry at sequence.
func transcriptEntryID(stateID string, sequence int64) string {
	return stateID + "-" + strconv.FormatInt(sequence, 10) + "-entry"
}

// newTranscriptAppendToken returns a fresh random token for one Append.
func newTranscriptAppendToken() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("generate transcript append token: %w", err)
	}
	return hex.EncodeToString(raw[:]), nil
}

// transcriptAppendCapabilities resolves the two store capabilities atomic
// sequence allocation needs, and runs the store's conditional-write
// capability probe, the check ops recording runs before it fences a write: the
// head's compare-and-set is a revision-fenced write, which a bd without
// --if-revision, the DoltLite read store and a read-only SQLite store refuse,
// and a bd store fences its pinned create the same way. It writes nothing; a
// bd store memoizes the probe's subprocesses. An error says why the store
// cannot allocate atomically.
func transcriptAppendCapabilities(store beads.MailStore) (beads.PinnedIDCreator, beads.MetadataCASWriter, error) {
	creator, ok := beads.As[beads.PinnedIDCreator](store)
	if !ok {
		return nil, nil, fmt.Errorf("the messaging store has no pinned-id create: %w", beads.ErrPinnedCreateUnsupported)
	}
	cas, ok := beads.As[beads.MetadataCASWriter](store)
	if !ok {
		return nil, nil, fmt.Errorf("the messaging store has no metadata compare-and-set: %w", beads.ErrConditionalWriteUnsupported)
	}
	if capable, reason := beads.ProbeConditionalWrites(store); !capable {
		return nil, nil, fmt.Errorf("the messaging store cannot fence writes (%s): %w", reason, beads.ErrConditionalWriteUnsupported)
	}
	return creator, cas, nil
}

// ProbeTranscriptSequencing reports whether store can allocate transcript
// sequences atomically: nil when it has both capabilities and passes its
// conditional-write probe, else why every append there runs unguarded and can
// give two entries one sequence. A nil answer does not cover a pinned create
// the store refuses when an append runs it (pinnedCreateRefused); that append
// runs unguarded too. It writes nothing.
func ProbeTranscriptSequencing(store beads.MailStore) error {
	_, _, err := transcriptAppendCapabilities(store)
	return err
}

// pinnedCreateRefused reports whether err is a pinned create the store
// refused before writing anything, which the PinnedIDCreator contract
// promises for both refusals: a store that cannot prove its pinned create
// insert-only (beads.ErrPinnedCreateUnsupported: a bd whose version cannot be
// read, or a wrapper over an engine without the capability), and a
// namespace-fenced store refusing an ID outside its namespaces
// (beads.ErrPinnedIDOutsideNamespace). The fence refuses every slot ID of a
// transcript state that gc storage migrate carried into a class binding under
// its work-store ID; the binding's minted IDs are inside the fence.
func pinnedCreateRefused(err error) bool {
	return errors.Is(err, beads.ErrPinnedCreateUnsupported) || errors.Is(err, beads.ErrPinnedIDOutsideNamespace)
}

// HasTranscriptState reports whether store holds a transcript state, open or
// closed: whether external messaging has recorded a transcript entry or a
// membership there.
func HasTranscriptState(store beads.MailStore) (bool, error) {
	items, err := store.List(beads.ListQuery{Label: labelTranscriptStateBase, IncludeClosed: true, Limit: 1})
	if err != nil {
		return false, fmt.Errorf("list transcript states: %w", err)
	}
	return len(items) > 0, nil
}

// appendUnguardedLocked appends over a store transcriptAppendCapabilities
// refused: the entry is created at the state's head under a minted ID, then
// the head is written past it unconditionally. Nothing at the store keeps a
// second entry off that sequence, so an ambiguous create, a failed head write
// or a writer holding another lock pool can give two entries one sequence;
// what it buys is that a store that cannot fence writes still records every
// transcript entry. An entry already stored under providerMessageID at or past
// the head, left there by an append whose head write failed, is published the
// same way before it is returned, so a retry of that append answers only once
// the entry is readable. The caller holds the conversation's binding lock.
func (s *transcriptService) appendUnguardedLocked(ref ConversationRef, state ConversationTranscriptStateRecord, providerMessageID string, entryAt func(int64) beads.Bead) (ConversationTranscriptRecord, error) {
	if providerMessageID != "" {
		existing, err := findTranscriptByProviderMessage(beads.LiveFor(s.store), ref, providerMessageID)
		if err != nil {
			return ConversationTranscriptRecord{}, err
		}
		if existing != nil {
			if existing.Sequence >= transcriptHead(state) {
				if err := s.advanceUnguardedHead(state, existing.Sequence); err != nil {
					return ConversationTranscriptRecord{}, fmt.Errorf("publish stored transcript entry %s: %w", existing.ID, err)
				}
			}
			return *existing, nil
		}
	}
	sequence := transcriptHead(state)
	created, err := s.store.Create(entryAt(sequence))
	if err != nil {
		return ConversationTranscriptRecord{}, fmt.Errorf("create transcript entry: %w", err)
	}
	if err := s.advanceUnguardedHead(state, sequence); err != nil {
		return ConversationTranscriptRecord{}, err
	}
	return decodeTranscriptBead(created)
}

// advanceUnguardedHead writes the head of state past sequence, unconditionally,
// with the retention floor of a legacy state that has none. Callers pass a
// sequence at or past the head they read under the binding lock, so the write
// never lowers a head written through that lock; a writer in another lock pool
// can still race it.
func (s *transcriptService) advanceUnguardedHead(state ConversationTranscriptStateRecord, sequence int64) error {
	updates := map[string]string{"next_sequence": strconv.FormatInt(sequence+1, 10)}
	if state.EarliestAvailableSequence <= 0 {
		updates["earliest_available_sequence"] = "1"
	}
	if err := s.store.SetMetadataBatch(state.ID, updates); err != nil {
		return fmt.Errorf("update transcript state: %w", err)
	}
	return nil
}

// transcriptAllocator appends one entry to one conversation's transcript.
type transcriptAllocator struct {
	svc        *transcriptService
	creator    beads.PinnedIDCreator
	cas        beads.MetadataCASWriter
	ref        ConversationRef
	stateID    string
	providerID string
	token      string
	// entryAt builds the entry bead for sequence, without its ID.
	entryAt func(sequence int64) beads.Bead
}

// append runs the slot loop: dedup, adopt or help an occupied head, else
// pin-create at the head and recover an ambiguous create by reading the
// slot back.
func (a *transcriptAllocator) append(ctx context.Context) (ConversationTranscriptRecord, error) {
	for range transcriptSlotAttempts {
		if err := checkContext(ctx); err != nil {
			return ConversationTranscriptRecord{}, err
		}
		_, state, err := a.svc.readTranscriptStateLive(a.stateID)
		if err != nil {
			return ConversationTranscriptRecord{}, err
		}
		if a.providerID != "" {
			existing, err := findTranscriptByProviderMessage(beads.LiveFor(a.svc.store), a.ref, a.providerID)
			if err != nil {
				return ConversationTranscriptRecord{}, err
			}
			if existing != nil {
				if err := a.publishThrough(ctx, existing.Sequence); err != nil {
					return ConversationTranscriptRecord{}, err
				}
				return *existing, nil
			}
		}
		head := transcriptHead(state)
		occupant, err := a.svc.transcriptSlotOccupant(a.ref, head)
		if err != nil {
			return ConversationTranscriptRecord{}, err
		}
		if occupant != nil {
			if err := a.publishThrough(ctx, head); err != nil {
				return ConversationTranscriptRecord{}, err
			}
			if a.owns(*occupant) {
				return occupant.record, nil
			}
			continue
		}
		bead := a.entryAt(head)
		bead.ID = transcriptEntryID(a.stateID, head)
		created, createErr := a.creator.CreatePinned(bead)
		if createErr == nil {
			record, err := decodeTranscriptOccupant(created, a.ref, head)
			if err != nil {
				return ConversationTranscriptRecord{}, err
			}
			if err := a.publishThrough(ctx, head); err != nil {
				return ConversationTranscriptRecord{}, err
			}
			return record, nil
		}
		if pinnedCreateRefused(createErr) {
			return ConversationTranscriptRecord{}, fmt.Errorf("create transcript entry %s: %w", bead.ID, createErr)
		}
		slot, err := a.readSlot(bead.ID, head, createErr)
		if err != nil {
			return ConversationTranscriptRecord{}, err
		}
		if err := a.publishThrough(ctx, head); err != nil {
			return ConversationTranscriptRecord{}, err
		}
		if a.owns(slot) {
			a.svc.warnf("extmsg: transcript entry %s was stored by a create that reported %v; recovered it from its slot", bead.ID, createErr)
			return slot.record, nil
		}
	}
	return ConversationTranscriptRecord{}, fmt.Errorf("append transcript entry to %s: head taken on %d attempts: %w", a.stateID, transcriptSlotAttempts, errTranscriptUnresolved)
}

// readSlot reads the pinned slot back after a failed create. Confirmed
// absence keeps the create error: the write may still commit later, and its
// pinned ID then protects the slot. A failed read or a malformed occupant
// keeps both errors.
func (a *transcriptAllocator) readSlot(id string, sequence int64, createErr error) (transcriptOccupant, error) {
	b, err := beads.LiveFor(a.svc.store).Get(id)
	switch {
	case errors.Is(err, beads.ErrNotFound):
		return transcriptOccupant{}, fmt.Errorf("create transcript entry %s: %w: %w", id, errTranscriptUnresolved, createErr)
	case err != nil:
		return transcriptOccupant{}, fmt.Errorf("create transcript entry %s: %w: %w (read back: %w)", id, errTranscriptUnresolved, createErr, err)
	}
	record, err := decodeTranscriptOccupant(b, a.ref, sequence)
	if err != nil {
		return transcriptOccupant{}, fmt.Errorf("create transcript entry %s: %w (read back: %w)", id, createErr, err)
	}
	return transcriptOccupant{record: record, token: b.Metadata[transcriptAppendTokenKey]}, nil
}

// owns reports whether occupant is this Append's entry: its token, or the
// same nonempty provider message ID.
func (a *transcriptAllocator) owns(occupant transcriptOccupant) bool {
	if occupant.token != "" && occupant.token == a.token {
		return true
	}
	return a.providerID != "" && occupant.record.ProviderMessageID == a.providerID
}

// publishThrough advances next_sequence until it is past target. Each
// advance needs an occupant at the head it publishes, so a lagging head is
// repaired slot by slot and a hole stops publication with a retryable error.
func (a *transcriptAllocator) publishThrough(ctx context.Context, target int64) error {
	failures := 0
	var lastErr error
	for failures < transcriptPublishAttempts {
		if err := checkContext(ctx); err != nil {
			return err
		}
		stateBead, state, err := a.svc.readTranscriptStateLive(a.stateID)
		if err != nil {
			return err
		}
		head := transcriptHead(state)
		if head > target {
			return a.ensureEarliestAvailable(state)
		}
		if head < target {
			occupant, err := a.svc.transcriptSlotOccupant(a.ref, head)
			if err != nil {
				return err
			}
			if occupant == nil {
				return fmt.Errorf("publish transcript %s through %d: no entry at sequence %d: %w", a.stateID, target, head, errTranscriptUnresolved)
			}
		}
		swapped, err := a.cas.CompareAndSetMetadataKey(a.stateID, "next_sequence", stateBead.Metadata["next_sequence"], strconv.FormatInt(head+1, 10))
		switch {
		case err != nil:
			lastErr = err
			failures++
		case swapped:
			failures = 0
		default:
			failures++
		}
	}
	if lastErr == nil {
		lastErr = errors.New("compare-and-set lost on every attempt")
	}
	a.svc.warnf("extmsg: transcript %s: gave up publishing through sequence %d after %d failed compare-and-sets: %v", a.stateID, target, transcriptPublishAttempts, lastErr)
	return fmt.Errorf("publish transcript %s through %d: %w: %w", a.stateID, target, errTranscriptUnresolved, lastErr)
}

// ensureEarliestAvailable records the retention floor of a legacy state that
// has none, without overwriting one another writer set.
func (a *transcriptAllocator) ensureEarliestAvailable(state ConversationTranscriptStateRecord) error {
	if state.EarliestAvailableSequence > 0 {
		return nil
	}
	if _, err := a.cas.CompareAndSetMetadataKey(a.stateID, "earliest_available_sequence", "", "1"); err != nil {
		return fmt.Errorf("set transcript %s earliest sequence: %w", a.stateID, err)
	}
	return nil
}

// transcriptHead is the state's published head: the next sequence to fill.
// A state without one starts at 1.
func transcriptHead(state ConversationTranscriptStateRecord) int64 {
	if state.NextSequence < 1 {
		return 1
	}
	return state.NextSequence
}

// transcriptOccupant is an entry found at a slot.
type transcriptOccupant struct {
	record ConversationTranscriptRecord
	token  string
}

// transcriptSlotOccupant reads the live bucket of sequence, closed rows
// included, for the entry of ref at that sequence: a pinned entry, or a
// legacy entry under a minted ID. More than one is corruption.
func (s *transcriptService) transcriptSlotOccupant(ref ConversationRef, sequence int64) (*transcriptOccupant, error) {
	items, err := beads.LiveFor(s.store).List(beads.ListQuery{
		Label:         transcriptBucketLabel(ref, transcriptBucket(sequence)),
		IncludeClosed: true,
	})
	if err != nil {
		return nil, fmt.Errorf("list transcript bucket %d: %w", transcriptBucket(sequence), err)
	}
	var out *transcriptOccupant
	for _, item := range items {
		if !hasLabel(item, "gc:extmsg-transcript") {
			continue
		}
		record, err := decodeTranscriptBead(item)
		if err != nil {
			return nil, fmt.Errorf("decode transcript entry %s: %w", item.ID, err)
		}
		if !sameConversationRef(record.Conversation, ref) || record.Sequence != sequence {
			continue
		}
		if out != nil {
			return nil, fmt.Errorf("%w: transcript entries %s and %s share sequence %d", ErrInvariantViolation, out.record.ID, record.ID, sequence)
		}
		out = &transcriptOccupant{record: record, token: item.Metadata[transcriptAppendTokenKey]}
	}
	return out, nil
}

// readTranscriptStateLive reads the transcript state bead past any cache and
// returns it with its decoded record.
func (s *transcriptService) readTranscriptStateLive(stateID string) (beads.Bead, ConversationTranscriptStateRecord, error) {
	b, err := beads.LiveFor(s.store).Get(stateID)
	if err != nil {
		return beads.Bead{}, ConversationTranscriptStateRecord{}, fmt.Errorf("read transcript state %s: %w", stateID, err)
	}
	record, err := decodeTranscriptStateBead(b)
	if err != nil {
		return beads.Bead{}, ConversationTranscriptStateRecord{}, fmt.Errorf("decode transcript state %s: %w", stateID, err)
	}
	return b, record, nil
}

// decodeTranscriptOccupant decodes the bead at the pinned ID of sequence and
// refuses it unless it is that sequence's entry of ref.
func decodeTranscriptOccupant(b beads.Bead, ref ConversationRef, sequence int64) (ConversationTranscriptRecord, error) {
	if !hasLabel(b, "gc:extmsg-transcript") {
		return ConversationTranscriptRecord{}, fmt.Errorf("%w: pinned transcript entry ID %s holds a bead that is not a transcript entry", ErrInvariantViolation, b.ID)
	}
	record, err := decodeTranscriptBead(b)
	if err != nil {
		return ConversationTranscriptRecord{}, fmt.Errorf("%w: decode pinned transcript entry %s: %w", ErrInvariantViolation, b.ID, err)
	}
	if !sameConversationRef(record.Conversation, ref) || record.Sequence != sequence {
		return ConversationTranscriptRecord{}, fmt.Errorf("%w: pinned transcript entry %s holds %s sequence %d, want %s sequence %d",
			ErrInvariantViolation, b.ID, conversationLockKey(record.Conversation), record.Sequence, conversationLockKey(ref), sequence)
	}
	return record, nil
}
