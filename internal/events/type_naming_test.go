package events

import (
	"regexp"
	"strings"
	"testing"
)

// eventTypeSegment is the shape every dot-separated segment of an event type
// has: lowercase alphanumerics, with underscores separating words.
var eventTypeSegment = regexp.MustCompile(`^[a-z0-9]+(_[a-z0-9]+)*$`)

// TestEveryKnownEventTypeFollowsTheNamingConvention keeps one spelling for
// multi-word segments across the whole taxonomy.
//
// An event type is a wire identifier: once a subscriber matches on it, it is in
// the OpenAPI discriminator maps and the generated TS types, and changing it is
// a breaking change. So the moment to be consistent is before the constant is
// added, and the thing that makes that automatic is a test rather than a
// reviewer noticing that a new type spells its second word differently from the
// thirty-odd that came before it.
//
// Underscore rather than hyphen is not a preference — it is what the existing
// taxonomy already does, unanimously. The trap this catches is a type named
// after an internal enum's String() value, where "not-configured" is the right
// spelling for a payload field and the wrong one for a type name.
func TestEveryKnownEventTypeFollowsTheNamingConvention(t *testing.T) {
	if len(KnownEventTypes) == 0 {
		t.Fatal("KnownEventTypes is empty, so this test proves nothing")
	}
	for _, eventType := range KnownEventTypes {
		segments := strings.Split(eventType, ".")
		if len(segments) < 2 {
			t.Errorf("event type %q has no namespace; every type is <namespace>.<name>", eventType)
			continue
		}
		for _, segment := range segments {
			if !eventTypeSegment.MatchString(segment) {
				t.Errorf("event type %q has segment %q, which is not lower_snake_case; every other multi-word segment in this package uses an underscore, and a wire identifier is not something to be inconsistent about", eventType, segment)
			}
		}
	}
}
