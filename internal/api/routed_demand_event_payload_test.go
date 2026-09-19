package api

import (
	"reflect"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/events"
)

const routedDemandStrandedWireType = "routed_demand.stranded"

func TestRoutedDemandStrandedEventIsKnownAndTyped(t *testing.T) {
	if !knownEventType(routedDemandStrandedWireType) {
		t.Fatalf("%s missing from events.KnownEventTypes", routedDemandStrandedWireType)
	}

	sample, ok := events.LookupPayload(routedDemandStrandedWireType)
	if !ok {
		t.Fatalf("%s has no registered typed payload", routedDemandStrandedWireType)
	}
	if _, noPayload := sample.(events.NoPayload); noPayload {
		t.Fatalf("%s registered events.NoPayload; want structured severity/template/bead_ids payload", routedDemandStrandedWireType)
	}

	fields := payloadJSONFields(t, sample)
	for _, field := range []string{"severity", "template", "bead_ids"} {
		if !fields[field] {
			t.Fatalf("%s payload fields = %v, want JSON field %q", routedDemandStrandedWireType, fields, field)
		}
	}
}

func knownEventType(eventType string) bool {
	for _, known := range events.KnownEventTypes {
		if known == eventType {
			return true
		}
	}
	return false
}

func payloadJSONFields(t *testing.T, sample events.Payload) map[string]bool {
	t.Helper()
	typ := reflect.TypeOf(sample)
	for typ.Kind() == reflect.Pointer {
		typ = typ.Elem()
	}
	if typ.Kind() != reflect.Struct {
		t.Fatalf("payload sample %T is %s, want struct", sample, typ.Kind())
	}
	fields := make(map[string]bool)
	for i := 0; i < typ.NumField(); i++ {
		field := typ.Field(i)
		if field.PkgPath != "" {
			continue
		}
		name := strings.Split(field.Tag.Get("json"), ",")[0]
		if name == "" {
			name = field.Name
		}
		if name != "-" {
			fields[name] = true
		}
	}
	return fields
}
