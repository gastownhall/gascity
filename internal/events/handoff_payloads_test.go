package events

import (
	"reflect"
	"slices"
	"testing"
)

func TestHandoffLifecycleEventsAreKnownAndTyped(t *testing.T) {
	tests := []struct {
		eventType  string
		jsonFields []string
	}{
		{eventType: "session.handoff_staged", jsonFields: []string{"session_key", "message_id"}},
		{eventType: "session.handoff_restart_accepted", jsonFields: []string{"session_key", "message_id"}},
		{eventType: "session.handoff_successor_started", jsonFields: []string{"session_key", "message_id"}},
		{eventType: "session.handoff_released", jsonFields: []string{"session_key", "message_id"}},
		{eventType: "session.handoff_failed", jsonFields: []string{"session_key", "reason"}},
	}

	registered := RegisteredPayloadTypes()
	for _, test := range tests {
		t.Run(test.eventType, func(t *testing.T) {
			if !slices.Contains(KnownEventTypes, test.eventType) {
				t.Fatalf("%q is missing from KnownEventTypes", test.eventType)
			}
			payload, ok := registered[test.eventType]
			if !ok {
				t.Fatalf("%q has no registered typed payload", test.eventType)
			}
			if _, noPayload := payload.(NoPayload); noPayload {
				t.Fatalf("%q uses NoPayload, want lifecycle correlation fields", test.eventType)
			}
			payloadType := reflect.TypeOf(payload)
			if payloadType.Kind() == reflect.Pointer {
				payloadType = payloadType.Elem()
			}
			for _, field := range test.jsonFields {
				if !redPayloadHasJSONField(payloadType, field) {
					t.Fatalf("payload %v for %q has no %q JSON field", payloadType, test.eventType, field)
				}
			}
		})
	}
}

func redPayloadHasJSONField(payloadType reflect.Type, want string) bool {
	for index := 0; index < payloadType.NumField(); index++ {
		field := payloadType.Field(index)
		if field.Tag.Get("json") == want || field.Tag.Get("json") == want+",omitempty" {
			return true
		}
	}
	return false
}
