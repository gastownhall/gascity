package runtime

import (
	"reflect"
	"sort"
	"testing"
)

// TestSessionEventCarriesNoProviderVocabulary guards the property the whole
// event contract rests on: a generic consumer must never have to know one
// provider's words. Every field here is either a semantic value this package
// defines (Kind), a gc-side identifier (Session), a timestamp, or Ref, which is
// documented as opaque and diagnostic.
//
// This is a field-set assertion rather than a comment because the failure mode
// is additive and looks harmless at the call site: relaying a provider's status
// string is one struct field and one assignment, and nothing else in the suite
// notices. If you are here because this test failed, the question to answer is
// not "how do I add my field to the list" but "can a consumer act on this
// without knowing which provider produced it". If it cannot, it belongs behind
// a semantic Kind, translated at the provider's own boundary. If it genuinely
// can, add it here with that reasoning in the commit message.
func TestSessionEventCarriesNoProviderVocabulary(t *testing.T) {
	want := []string{"Kind", "Ref", "Session", "Time"}

	typ := reflect.TypeOf(SessionEvent{})
	got := make([]string, 0, typ.NumField())
	for i := range typ.NumField() {
		got = append(got, typ.Field(i).Name)
	}
	sort.Strings(got)

	if !reflect.DeepEqual(got, want) {
		t.Errorf("SessionEvent fields = %v, want %v; see this test's doc comment before changing the list", got, want)
	}
}
