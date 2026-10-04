package beads

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	beadslib "github.com/steveyegge/beads"
	"github.com/steveyegge/beads/issueops"
)

// casRoleSpy is a MetadataCAS double holding ONE key, with the role's own
// distinction between an absent key and a key present holding the empty
// string — the distinction gc's string-shaped front door does not have and
// must therefore normalize across.
type casRoleSpy struct {
	beadslib.Storage
	present  bool
	value    json.RawMessage
	requests []issueops.CompareAndSetKeyRequest
	err      error
}

func (s *casRoleSpy) MetadataCAS() (issueops.MetadataCAS, error) { return s, nil }

func (s *casRoleSpy) CompareAndSetKey(_ context.Context, req issueops.CompareAndSetKeyRequest) (issueops.CompareAndSetKeyResult, error) {
	s.requests = append(s.requests, req)
	if s.err != nil {
		return issueops.CompareAndSetKeyResult{}, s.err
	}
	matched := false
	switch {
	case req.Expected == nil:
		matched = !s.present
	case s.present:
		matched = string(*req.Expected) == string(s.value)
	}
	if !matched {
		return issueops.CompareAndSetKeyResult{Swapped: false, Current: s.current()}, nil
	}
	if req.Value == nil {
		s.present = false
		s.value = nil
	} else {
		s.present = true
		s.value = append(json.RawMessage(nil), *req.Value...)
	}
	return issueops.CompareAndSetKeyResult{Swapped: true, Current: s.current()}, nil
}

func (s *casRoleSpy) current() *json.RawMessage {
	if !s.present {
		return nil
	}
	value := append(json.RawMessage(nil), s.value...)
	return &value
}

func newCASRoleSpy() *casRoleSpy { return &casRoleSpy{} }

func (s *casRoleSpy) setPresent(raw string) {
	s.present = true
	s.value = json.RawMessage(raw)
}

// A non-empty expectation is ONE call carrying the JSON-encoded value.
func TestCASSendsTheExpectedValueAsJSON(t *testing.T) {
	spy := newCASRoleSpy()
	spy.setPresent(`"holder-a"`)
	store := newNativeDoltStoreForTest(spy)

	ok, err := store.CompareAndSetMetadataKey("gc-1", "lease", "holder-a", "holder-b")
	if err != nil || !ok {
		t.Fatalf("CAS = (%v, %v), want (true, nil)", ok, err)
	}
	if len(spy.requests) != 1 {
		t.Fatalf("CAS made %d role calls, want 1", len(spy.requests))
	}
	req := spy.requests[0]
	if req.Actor != "native-test" || req.IssueID != "gc-1" || req.Key != "lease" {
		t.Errorf("request = %+v, want the store actor and the caller's id/key", req)
	}
	if req.Expected == nil || string(*req.Expected) != `"holder-a"` {
		t.Errorf("expected = %v, want the JSON-encoded string", req.Expected)
	}
	if req.Value == nil || string(*req.Value) != `"holder-b"` {
		t.Errorf("value = %v, want the JSON-encoded string", req.Value)
	}
}

// THE ACQUIRE'S TWO ARMS. gc's front door takes "" to mean "absent OR present
// holding the empty string", because parsing an absent key out of the stored
// map yields "" and callers cannot tell the two apart. The role CAN, so a
// single-arm request would only ever claim one of them — and which one it
// missed would depend on whether the row predates the clear-as-deletion this
// same commit introduces.
func TestCASWithEmptyExpectedClaimsAnAbsentKeyOnTheFirstArm(t *testing.T) {
	spy := newCASRoleSpy()
	store := newNativeDoltStoreForTest(spy)

	ok, err := store.CompareAndSetMetadataKey("gc-1", "lease", "", "holder")
	if err != nil || !ok {
		t.Fatalf("CAS over an absent key = (%v, %v), want (true, nil)", ok, err)
	}
	if len(spy.requests) != 1 {
		t.Fatalf("the absent arm took %d calls, want 1 — the empty arm must not be dialed when the first one wins", len(spy.requests))
	}
	if spy.requests[0].Expected != nil {
		t.Errorf("first arm expected = %v, want nil (absent)", spy.requests[0].Expected)
	}
}

func TestCASWithEmptyExpectedFallsBackToThePresentEmptyArm(t *testing.T) {
	spy := newCASRoleSpy()
	spy.setPresent(`""`)
	store := newNativeDoltStoreForTest(spy)

	ok, err := store.CompareAndSetMetadataKey("gc-1", "lease", "", "holder")
	if err != nil || !ok {
		t.Fatalf("CAS over a present-empty key = (%v, %v), want (true, nil) — a legacy row written before clear-as-deletion still holds \"\"", ok, err)
	}
	if len(spy.requests) != 2 {
		t.Fatalf("the fallback took %d calls, want 2 (absent, then present-empty)", len(spy.requests))
	}
	if spy.requests[1].Expected == nil || string(*spy.requests[1].Expected) != `""` {
		t.Errorf("second arm expected = %v, want the JSON empty string", spy.requests[1].Expected)
	}
}

// The mutation guard on the two arms: a key holding a REAL value must not be
// claimable by an empty expectation, or every lease acquire steals a live one.
func TestCASWithEmptyExpectedRefusesANonEmptyValue(t *testing.T) {
	spy := newCASRoleSpy()
	spy.setPresent(`"live-holder"`)
	store := newNativeDoltStoreForTest(spy)

	ok, err := store.CompareAndSetMetadataKey("gc-1", "lease", "", "thief")
	if err != nil {
		t.Fatalf("a lost race must not be an error: %v", err)
	}
	if ok {
		t.Fatal("an empty expectation claimed a key holding a live value")
	}
	if string(spy.value) != `"live-holder"` {
		t.Errorf("the value moved: %s", spy.value)
	}
	if len(spy.requests) != 1 {
		t.Errorf("the present-empty arm was dialed against a non-empty value (%d calls); the fallback is conditioned on Current being \"\"", len(spy.requests))
	}
}

// CLEAR IS A DELETION. gc's release paths write "" to clear a key, and the two
// states read identically through gc's front door — so the role's deletion is
// the spelling that makes the NEXT acquire's first arm the one that wins,
// instead of leaving every workspace permanently on the fallback arm.
func TestCASClearingAKeyRemovesIt(t *testing.T) {
	spy := newCASRoleSpy()
	spy.setPresent(`"holder"`)
	store := newNativeDoltStoreForTest(spy)

	ok, err := store.CompareAndSetMetadataKey("gc-1", "lease", "holder", "")
	if err != nil || !ok {
		t.Fatalf("clear = (%v, %v), want (true, nil)", ok, err)
	}
	if spy.requests[0].Value != nil {
		t.Errorf("clear sent value %s, want nil (deletion)", *spy.requests[0].Value)
	}
	if spy.present {
		t.Error("the key survived the clear")
	}

	// And the acquire that follows wins on the ABSENT arm, in one call.
	spy.requests = nil
	if ok, err := store.CompareAndSetMetadataKey("gc-1", "lease", "", "next-holder"); err != nil || !ok {
		t.Fatalf("re-acquire after clear = (%v, %v), want (true, nil)", ok, err)
	}
	if len(spy.requests) != 1 {
		t.Errorf("re-acquire took %d calls, want 1", len(spy.requests))
	}
}

// A missing bead is an ERROR, not a lost race — the front door's own contract.
func TestCASReportsAMissingBeadAsAnError(t *testing.T) {
	spy := newCASRoleSpy()
	spy.err = issueops.ErrNotFound
	store := newNativeDoltStoreForTest(spy)

	ok, err := store.CompareAndSetMetadataKey("gc-missing", "lease", "", "holder")
	if ok {
		t.Fatal("CAS reported a swap against a missing bead")
	}
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("error = %v, want ErrNotFound", err)
	}
}
