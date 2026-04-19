package api

import (
	"sort"
	"testing"

	"github.com/gastownhall/gascity/internal/events"
)

// TestEveryKnownEventTypeHasRegisteredPayload enforces Principle 7's
// strict unregistered-type policy: every constant in
// events.KnownEventTypes MUST have a registered payload by the time
// this package's init() functions finish. A new event-type constant
// without a registered payload fails CI and prevents the
// /v0/events/stream wire schema from having a shape "we don't know."
func TestEveryKnownEventTypeHasRegisteredPayload(t *testing.T) {
	var missing []string
	for _, eventType := range events.KnownEventTypes {
		if _, ok := events.LookupPayload(eventType); !ok {
			missing = append(missing, eventType)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		t.Fatalf("unregistered event types (add to internal/api/event_payloads.go or the appropriate domain package init):\n  %v",
			missing)
	}
}
