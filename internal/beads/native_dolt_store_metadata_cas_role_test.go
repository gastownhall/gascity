package beads

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
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

// A KEY HOLDING A NON-STRING VALUE matches the expectation the store's own read
// projection shows for it. metadataMapFromNative renders a JSON string as its
// text and anything else as its re-encoded JSON, so a read-then-CAS loop hands
// back "null" for a stored null and "1.5" for a stored 1.50 — the base store
// compared against exactly that rendering. Each row reads its expectation
// through metadataMapFromNative rather than restating it, so the CAS and the
// read cannot drift apart; the spelled-out rendering is only a sanity check on
// the row itself.
func TestCASMatchesAStoredNonStringValueByItsReadRendering(t *testing.T) {
	for _, tc := range []struct {
		name     string
		stored   string
		rendered string
	}{
		{name: "integer", stored: `3`, rendered: `3`},
		{name: "null", stored: `null`, rendered: `null`},
		{name: "non-canonical number", stored: `1.50`, rendered: `1.5`},
		{name: "exponent number", stored: `1e3`, rendered: `1000`},
		// Unsorted AND carrying a non-canonical number, so the rendering differs
		// from the canonical form a real backend reports in Current as well as
		// from the stored bytes.
		{name: "unsorted object", stored: `{"b":1.50,"a":2}`, rendered: `{"a":2,"b":1.5}`},
		{name: "object with whitespace", stored: `{"a": 1}`, rendered: `{"a":1}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			read, err := metadataMapFromNative(json.RawMessage(`{"k":` + tc.stored + `}`))
			if err != nil {
				t.Fatalf("read projection of %s: %v", tc.stored, err)
			}
			if read["k"] != tc.rendered {
				t.Fatalf("the read projection renders %s as %q, want %q", tc.stored, read["k"], tc.rendered)
			}
			spy := newCASRoleSpy()
			spy.setPresent(tc.stored)
			store := newNativeDoltStoreForTest(spy)

			ok, err := store.CompareAndSetMetadataKey("gc-1", "k", read["k"], "next")
			if err != nil || !ok {
				t.Fatalf("CAS(%q) over stored %s = (%v, %v), want (true, nil)", read["k"], tc.stored, ok, err)
			}
			if len(spy.requests) != 2 {
				t.Fatalf("CAS made %d role calls, want 2 (the string arm, then the stored bytes)", len(spy.requests))
			}
			if got := spy.requests[1].Expected; got == nil || string(*got) != tc.stored {
				t.Errorf("retry expected = %v, want the stored bytes %s verbatim", got, tc.stored)
			}
			if string(spy.value) != `"next"` {
				t.Errorf("stored value = %s, want the swapped-in string", spy.value)
			}
		})
	}
}

// And the other side: an expectation the read projection does NOT show for the
// stored value is a lost race, answered in one call with no retry. A stored 1.50
// reads as "1.5", so "1.50" is a spelling no reader of this store was handed;
// and a JSON string is never retried, not even against its own quoted form.
func TestCASRefusesAnExpectationTheReadRenderingDoesNotShow(t *testing.T) {
	for _, tc := range []struct {
		name, stored, expected string
	}{
		{name: "the stored literal of a non-canonical number", stored: `1.50`, expected: `1.50`},
		{name: "a different value over null", stored: `null`, expected: `0`},
		{name: "the quoted form of a string", stored: `"x"`, expected: `"x"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			spy := newCASRoleSpy()
			spy.setPresent(tc.stored)
			store := newNativeDoltStoreForTest(spy)

			ok, err := store.CompareAndSetMetadataKey("gc-1", "k", tc.expected, "thief")
			if err != nil {
				t.Fatalf("a lost race must not be an error: %v", err)
			}
			if ok {
				t.Fatalf("CAS(%q) claimed stored %s", tc.expected, tc.stored)
			}
			if len(spy.requests) != 1 {
				t.Errorf("CAS made %d role calls, want 1 — the stored-bytes retry is only for a rendering match", len(spy.requests))
			}
			if string(spy.value) != tc.stored {
				t.Errorf("the value moved: %s", spy.value)
			}
		})
	}
}

// A stored null is not the empty string — the read projection shows it as
// "null" — so an empty expectation refuses it on the absent arm alone.
func TestCASWithEmptyExpectedDoesNotTakeAStoredNullForEmpty(t *testing.T) {
	spy := newCASRoleSpy()
	spy.setPresent(`null`)
	store := newNativeDoltStoreForTest(spy)

	ok, err := store.CompareAndSetMetadataKey("gc-1", "lease", "", "holder")
	if err != nil || ok {
		t.Fatalf("CAS(\"\") over a stored null = (%v, %v), want (false, nil)", ok, err)
	}
	if len(spy.requests) != 1 {
		t.Errorf("CAS made %d role calls, want 1 — the present-empty arm is conditioned on Current being the empty string", len(spy.requests))
	}
}

// A refusal whose Current is not well-formed JSON is a broken answer, not a
// lost race: both arms decode Current to decide whether to retry, and a decode
// failure read as "no match" would report a backend fault as an ordinary miss.
func TestCASReportsAMalformedRefusalAsAnError(t *testing.T) {
	for _, tc := range []struct{ name, expected string }{
		{name: "non-empty expectation", expected: "holder"},
		{name: "empty expectation", expected: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			spy := newCASRoleSpy()
			spy.setPresent(`{"unterminated":`)
			store := newNativeDoltStoreForTest(spy)

			ok, err := store.CompareAndSetMetadataKey("gc-1", "lease", tc.expected, "next")
			if ok || err == nil {
				t.Fatalf("CAS over a malformed refusal = (%v, %v), want (false, error)", ok, err)
			}
			if !strings.Contains(err.Error(), "gc-1") {
				t.Errorf("error %q does not name the bead", err)
			}
			if len(spy.requests) != 1 {
				t.Errorf("CAS made %d role calls, want 1 — nothing is retried against a value it could not read", len(spy.requests))
			}
		})
	}
}
