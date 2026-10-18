package opjournal

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
)

// A respond op answers one interaction occurrence (DES-O1 §10.2): one
// raising of a runtime request, recorded as an interaction.raised fact. A
// runtime may raise the same request id again (tmux ids are content hashes,
// so two identical prompts in one generation share one), so the occurrence
// is <request id>#<n>, where n numbers the raisings of that request id in
// one generation from 1. A lane respond op is keyed by the occurrence's
// raised fact (RespondKey), never by the request id, so a second identical
// prompt gets its own answer instead of a replay of the first.

const (
	// occurrenceSep separates the request id from the raising number. The
	// number is parsed from the last one, so a request id may contain it.
	occurrenceSep = "#"
	// MaxRaisings caps the raisings of one request id in one generation.
	MaxRaisings = 1 << 20
	// PendingUnknown is Probe.Pending for a pending interaction whose request
	// id is not known. A failed look is a Probe error, not PendingUnknown.
	PendingUnknown = "?"
	// ResolvedCleared is the reason of an interaction.resolved the lane
	// records when it sees a raising gone with no answer of its own: another
	// prompt, or none, is pending.
	ResolvedCleared = "cleared"
	// respondKeyPrefix starts a lane respond's client key.
	respondKeyPrefix = "respond:"
	// maxOccurrenceSuffix is the longest "#<n>" a raising number makes.
	maxOccurrenceSuffix = len(occurrenceSep) + 7 // MaxRaisings has 7 digits
)

// RespondPayload is a respond op's payload: the answer to one interaction
// occurrence. An op stores it as EncodeRespondPayload's canonical JSON, which
// Propose requires, so equal answers have one request digest and a different
// second answer to one occurrence is key reuse.
type RespondPayload struct {
	// Action is the runtime's answer verb, such as approve or deny.
	Action string `json:"action"`
	// Text is free text for an answer that carries some.
	Text string `json:"text,omitempty"`
	// Metadata is provider-specific detail the runtime reads.
	Metadata map[string]string `json:"metadata,omitempty"`
}

func (p RespondPayload) validate() error {
	if err := validateText("respond action", p.Action, maxTokenLen); err != nil {
		return err
	}
	for k := range p.Metadata {
		if err := validateText("respond metadata key", k, maxTokenLen); err != nil {
			return err
		}
	}
	return nil
}

// EncodeRespondPayload returns p's canonical JSON, the payload of a respond
// op.
func EncodeRespondPayload(p RespondPayload) (string, error) {
	if err := p.validate(); err != nil {
		return "", err
	}
	b, err := json.Marshal(p)
	if err != nil {
		return "", invalidf("encoding respond payload: %v", err)
	}
	return string(b), nil
}

// DecodeRespondPayload reads a respond op's payload. Anything but
// EncodeRespondPayload's canonical encoding of a valid answer is refused.
func DecodeRespondPayload(s string) (RespondPayload, error) {
	dec := json.NewDecoder(strings.NewReader(s))
	dec.DisallowUnknownFields()
	var p RespondPayload
	if err := dec.Decode(&p); err != nil {
		return RespondPayload{}, invalidf("respond payload is not a JSON answer: %v", err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return RespondPayload{}, invalidf("respond payload has trailing data")
	}
	canonical, err := EncodeRespondPayload(p)
	if err != nil {
		return RespondPayload{}, err
	}
	if canonical != s {
		return RespondPayload{}, invalidf("respond payload is not in canonical form (want %s)", canonical)
	}
	return p, nil
}

// InteractionOccurrence names the nth raising of requestID in one generation:
// <request id>#<n>.
func InteractionOccurrence(requestID string, n int) (string, error) {
	if n < 1 || n > MaxRaisings {
		return "", invalidf("interaction raising %d is outside 1 through %d", n, MaxRaisings)
	}
	if err := validateText("interaction request id", requestID, maxClientKeyLen-maxOccurrenceSuffix); err != nil {
		return "", err
	}
	return requestID + occurrenceSep + strconv.Itoa(n), nil
}

// ParseInteractionOccurrence splits an occurrence into its request id and
// raising number.
func ParseInteractionOccurrence(occ string) (requestID string, n int, err error) {
	i := strings.LastIndex(occ, occurrenceSep)
	if i < 0 {
		return "", 0, invalidf("interaction %q names no raising (<request id>#<n>)", occ)
	}
	requestID, num := occ[:i], occ[i+len(occurrenceSep):]
	n, err = strconv.Atoi(num)
	if err != nil || strconv.Itoa(n) != num {
		return "", 0, invalidf("interaction %q: raising %q is not a number", occ, num)
	}
	if _, err := InteractionOccurrence(requestID, n); err != nil {
		return "", 0, fmt.Errorf("interaction %q: %w", occ, err)
	}
	return requestID, n, nil
}

// interactionFact is the identity of an interaction.* fact of occurrence occ.
func interactionFact(kind FactKind, conv string, gen int64, occ string) Fact {
	return Fact{Kind: kind, Conversation: conv, Generation: gen, Interaction: occ}
}

// RespondKey is the client key of a lane respond op: respond:<id of the
// interaction.raised fact it answers>. raised is the fact as recorded, its ID
// set. One principal's answers to one raising are one op, and the next
// raising of the same request id has another key.
func RespondKey(raised Fact) (string, error) {
	if raised.Kind != FactInteractionRaised {
		return "", invalidf("respond key: %s is not an interaction.raised fact", raised.Kind)
	}
	if _, _, err := ParseInteractionOccurrence(raised.Interaction); err != nil {
		return "", fmt.Errorf("respond key: %w", err)
	}
	if err := raised.validateIdentity(); err != nil {
		return "", fmt.Errorf("respond key: %w", err)
	}
	prefix, ok := strings.CutSuffix(raised.ID, factIDInfix+factIDSuffix(raised))
	if !ok || validatePrefix(prefix) != nil {
		return "", invalidf("respond key: fact id %q is not the id of its occurrence", raised.ID)
	}
	return composedKey("respond key", respondKeyPrefix+raised.ID)
}

// validateRespond checks what Propose requires of a respond op beyond its
// shape: a RespondPayload and, for a lane op, the occurrence it answers and
// the key that occurrence gives it. A legacy op keeps its client key
// (DES-O0API §3.2).
func validateRespond(prefix string, op Op) error {
	if _, err := DecodeRespondPayload(op.Payload); err != nil {
		return err
	}
	if op.Executor != ExecutorLane {
		return nil
	}
	if _, _, err := ParseInteractionOccurrence(op.Scope.Interaction); err != nil {
		return err
	}
	id, err := FactID(prefix, interactionFact(FactInteractionRaised, op.Conversation, op.Scope.Generation, op.Scope.Interaction))
	if err != nil {
		return err
	}
	if want := respondKeyPrefix + id; op.ClientKey != want {
		return invalidf("a lane respond is keyed by the occurrence it answers: client key %q, want %q", op.ClientKey, want)
	}
	return nil
}

// InteractionRaising is one raising of a runtime request: its
// interaction.raised fact, and whether an interaction.resolved records its
// end.
type InteractionRaising struct {
	Raised   Fact
	Resolved bool
}

// LatestRaising returns the latest raising of requestID in generation gen of
// conv; found is false when it was never raised there. Raisings are numbered
// densely (RecordRaising), so a galloping search over their pinned fact ids
// finds the latest in a logarithmic number of reads.
func (s *Store) LatestRaising(conv string, gen int64, requestID string) (r InteractionRaising, found bool, err error) {
	raised := func(n int) (Fact, bool, error) {
		occ, err := InteractionOccurrence(requestID, n)
		if err != nil {
			return Fact{}, false, err
		}
		return s.readFact(interactionFact(FactInteractionRaised, conv, gen, occ))
	}
	last, ok, err := raised(1)
	if err != nil || !ok {
		return InteractionRaising{}, false, err
	}
	lo, hi := 1, 2
	for hi <= MaxRaisings {
		f, ok, err := raised(hi)
		if err != nil {
			return InteractionRaising{}, false, err
		}
		if !ok {
			break
		}
		lo, last = hi, f
		hi *= 2
	}
	hi = min(hi, MaxRaisings+1)
	for hi-lo > 1 {
		mid := lo + (hi-lo)/2
		f, ok, err := raised(mid)
		if err != nil {
			return InteractionRaising{}, false, err
		}
		if ok {
			lo, last = mid, f
		} else {
			hi = mid
		}
	}
	_, resolved, err := s.readFact(interactionFact(FactInteractionResolved, conv, gen, last.Interaction))
	if err != nil {
		return InteractionRaising{}, false, err
	}
	return InteractionRaising{Raised: last, Resolved: resolved}, true, nil
}

// RecordRaising records that the runtime shows requestID now in generation
// gen of conv: the latest raising when it is not resolved yet, else a new
// interaction.raised for the next occurrence. created reports a new fact. Two
// observers of one raising write one fact: both number it alike, and the
// write is create-if-absent.
func (s *Store) RecordRaising(conv string, gen int64, requestID string, basis FactBasis, at time.Time) (r InteractionRaising, created bool, err error) {
	latest, found, err := s.LatestRaising(conv, gen, requestID)
	if err != nil {
		return InteractionRaising{}, false, fmt.Errorf("reading the raisings of %q in conversation %s: %w", requestID, conv, err)
	}
	if found && !latest.Resolved {
		return latest, false, nil
	}
	n := 1
	if found {
		if _, n, err = ParseInteractionOccurrence(latest.Raised.Interaction); err != nil {
			return InteractionRaising{}, false, err
		}
		n++
	}
	occ, err := InteractionOccurrence(requestID, n)
	if err != nil {
		return InteractionRaising{}, false, err
	}
	f := interactionFact(FactInteractionRaised, conv, gen, occ)
	f.Basis, f.At = basis, utc(at)
	if created, err = s.RecordFact(f); err != nil {
		return InteractionRaising{}, false, fmt.Errorf("recording raising %s of conversation %s: %w", occ, conv, err)
	}
	if f.ID, err = FactID(s.prefix, f); err != nil {
		return InteractionRaising{}, false, err
	}
	return InteractionRaising{Raised: f}, created, nil
}

// raisingOf reads occurrence occ of generation gen of conv: whether it was
// raised, and whether it is resolved.
func (s *Store) raisingOf(conv string, gen int64, occ string) (raised, resolved bool, err error) {
	if _, raised, err = s.readFact(interactionFact(FactInteractionRaised, conv, gen, occ)); err != nil || !raised {
		return false, false, err
	}
	_, resolved, err = s.readFact(interactionFact(FactInteractionResolved, conv, gen, occ))
	return raised, resolved, err
}

// readFact reads the fact of f's occurrence at its pinned id; found is false
// when none is recorded.
func (s *Store) readFact(f Fact) (Fact, bool, error) {
	id, err := FactID(s.prefix, f)
	if err != nil {
		return Fact{}, false, err
	}
	b, err := s.store.Get(id)
	if errors.Is(err, beads.ErrNotFound) {
		return Fact{}, false, nil
	}
	if err != nil {
		return Fact{}, false, fmt.Errorf("reading fact %s: %w", id, err)
	}
	got, err := DecodeFact(b)
	if err != nil {
		return Fact{}, false, err
	}
	if got.Kind != f.Kind || got.Conversation != f.Conversation || got.Generation != f.Generation || got.occurrence() != f.occurrence() {
		return Fact{}, false, corruptf(id, "stored fact records another occurrence (hash collision)")
	}
	return got, true, nil
}
