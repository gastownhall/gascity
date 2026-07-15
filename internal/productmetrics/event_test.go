package productmetrics

import (
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"
)

const (
	fixedEventJSON = `{"event_id":"8c4f4128-a6e8-4f66-bd1b-1fcf1298b124","installation_id":"3cf9fd4e-3337-4c29-a0ab-2858cd8a1f21","app":"gascity","release_version":"0.31.0","os":"linux","occurred_hour_utc":"2026-07-11T00:00:00Z","command_id":"help"}`
	fixedBatchJSON = `{"schema_version":1,"events":[` + fixedEventJSON + `]}`
)

func fixedEvent() Event {
	return Event{
		EventID:         "8c4f4128-a6e8-4f66-bd1b-1fcf1298b124",
		InstallationID:  "3cf9fd4e-3337-4c29-a0ab-2858cd8a1f21",
		App:             AppGasCity,
		ReleaseVersion:  "0.31.0",
		OS:              OSLinux,
		OccurredHourUTC: "2026-07-11T00:00:00Z",
		CommandID:       CommandHelp,
	}
}

func TestEncodeExactEventAndBatchBytes(t *testing.T) {
	eventBytes, err := EncodeEvent(fixedEvent())
	if err != nil {
		t.Fatalf("EncodeEvent: %v", err)
	}
	if got := string(eventBytes); got != fixedEventJSON {
		t.Fatalf("event bytes mismatch\n got: %s\nwant: %s", got, fixedEventJSON)
	}

	batchBytes, err := EncodeBatch(Batch{SchemaVersion: SchemaVersionV1, Events: []Event{fixedEvent()}})
	if err != nil {
		t.Fatalf("EncodeBatch: %v", err)
	}
	if got := string(batchBytes); got != fixedBatchJSON {
		t.Fatalf("batch bytes mismatch\n got: %s\nwant: %s", got, fixedBatchJSON)
	}

	second := fixedEvent()
	second.EventID = "123e4567-e89b-42d3-a456-426614174000"
	second.OS = OSDarwin
	second.OccurredHourUTC = "2026-07-11T01:00:00Z"
	second.CommandID = CommandVersion
	wantMulti := `{"schema_version":1,"events":[` + fixedEventJSON + `,{"event_id":"123e4567-e89b-42d3-a456-426614174000","installation_id":"3cf9fd4e-3337-4c29-a0ab-2858cd8a1f21","app":"gascity","release_version":"0.31.0","os":"darwin","occurred_hour_utc":"2026-07-11T01:00:00Z","command_id":"version"}]}`
	gotMulti, err := EncodeBatch(Batch{SchemaVersion: SchemaVersionV1, Events: []Event{fixedEvent(), second}})
	if err != nil {
		t.Fatalf("EncodeBatch(multi): %v", err)
	}
	if got := string(gotMulti); got != wantMulti {
		t.Fatalf("multi-event bytes mismatch\n got: %s\nwant: %s", got, wantMulti)
	}
}

func TestDecodeRoundTripsCanonicalBytes(t *testing.T) {
	event, err := DecodeEvent([]byte(fixedEventJSON))
	if err != nil {
		t.Fatalf("DecodeEvent: %v", err)
	}
	if event != fixedEvent() {
		t.Fatalf("DecodeEvent = %#v, want %#v", event, fixedEvent())
	}
	batch, err := DecodeBatch([]byte(fixedBatchJSON))
	if err != nil {
		t.Fatalf("DecodeBatch: %v", err)
	}
	got, err := EncodeBatch(batch)
	if err != nil {
		t.Fatalf("EncodeBatch(decoded): %v", err)
	}
	if string(got) != fixedBatchJSON {
		t.Fatalf("round-trip = %s, want %s", got, fixedBatchJSON)
	}

	var viaJSON Batch
	if err := json.Unmarshal([]byte(fixedBatchJSON), &viaJSON); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}
	if _, err := json.Marshal(viaJSON); err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
}

func TestStrictDecodeRejectsUnknownDuplicateAndTrailingJSON(t *testing.T) {
	eventUnknown := strings.TrimSuffix(fixedEventJSON, "}") + `,"extra":true}`
	eventDuplicate := strings.Replace(fixedEventJSON, `"event_id":`, `"event_id":"8c4f4128-a6e8-4f66-bd1b-1fcf1298b124","event_id":`, 1)
	escapedDuplicate := strings.Replace(fixedEventJSON, `"event_id":`, `"\u0065vent_id":"8c4f4128-a6e8-4f66-bd1b-1fcf1298b124","event_id":`, 1)
	batchUnknown := strings.TrimSuffix(fixedBatchJSON, "}") + `,"extra":true}`
	batchDuplicate := strings.Replace(fixedBatchJSON, `"schema_version":1`, `"schema_version":1,"schema_version":1`, 1)
	nestedUnknown := `{"schema_version":1,"events":[` + eventUnknown + `]}`
	nestedDuplicate := `{"schema_version":1,"events":[` + eventDuplicate + `]}`

	for name, raw := range map[string]string{
		"event unknown":           eventUnknown,
		"event duplicate":         eventDuplicate,
		"event escaped duplicate": escapedDuplicate,
		"event trailing":          fixedEventJSON + ` {}`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := DecodeEvent([]byte(raw)); err == nil {
				t.Fatal("DecodeEvent unexpectedly accepted invalid JSON")
			}
		})
	}
	for name, raw := range map[string]string{
		"batch unknown":    batchUnknown,
		"batch duplicate":  batchDuplicate,
		"nested unknown":   nestedUnknown,
		"nested duplicate": nestedDuplicate,
		"batch trailing":   fixedBatchJSON + ` []`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := DecodeBatch([]byte(raw)); err == nil {
				t.Fatal("DecodeBatch unexpectedly accepted invalid JSON")
			}
		})
	}
}

func TestStrictDecodeRejectsCaseFoldedFieldAliasesInEitherOrder(t *testing.T) {
	eventIDPair := `"event_id":"8c4f4128-a6e8-4f66-bd1b-1fcf1298b124"`
	upperEventIDPair := `"EVENT_ID":"8c4f4128-a6e8-4f66-bd1b-1fcf1298b124"`
	upperEventID := strings.Replace(fixedEventJSON, eventIDPair, upperEventIDPair, 1)
	canonicalThenAlias := strings.Replace(fixedEventJSON, eventIDPair, eventIDPair+","+upperEventIDPair, 1)
	aliasThenCanonical := strings.Replace(fixedEventJSON, eventIDPair, upperEventIDPair+","+eventIDPair, 1)

	schemaPair := `"schema_version":1`
	upperSchemaPair := `"SCHEMA_VERSION":1`
	upperSchema := strings.Replace(fixedBatchJSON, schemaPair, upperSchemaPair, 1)
	canonicalThenSchemaAlias := strings.Replace(fixedBatchJSON, schemaPair, schemaPair+","+upperSchemaPair, 1)
	schemaAliasThenCanonical := strings.Replace(fixedBatchJSON, schemaPair, upperSchemaPair+","+schemaPair, 1)
	nestedUpperEventID := `{"schema_version":1,"events":[` + upperEventID + `]}`
	nestedCanonicalThenAlias := `{"schema_version":1,"events":[` + canonicalThenAlias + `]}`
	nestedAliasThenCanonical := `{"schema_version":1,"events":[` + aliasThenCanonical + `]}`

	for name, raw := range map[string]string{
		"uppercase event field":        upperEventID,
		"canonical then event alias":   canonicalThenAlias,
		"event alias then canonical":   aliasThenCanonical,
		"uppercase batch field":        upperSchema,
		"canonical then batch alias":   canonicalThenSchemaAlias,
		"batch alias then canonical":   schemaAliasThenCanonical,
		"uppercase nested event field": nestedUpperEventID,
		"nested canonical then alias":  nestedCanonicalThenAlias,
		"nested alias then canonical":  nestedAliasThenCanonical,
	} {
		t.Run(name, func(t *testing.T) {
			var err error
			if strings.Contains(name, "batch") || strings.Contains(name, "nested") {
				_, err = DecodeBatch([]byte(raw))
			} else {
				_, err = DecodeEvent([]byte(raw))
			}
			if err == nil {
				t.Fatal("decoder accepted a case-folded field alias")
			}
		})
	}
}

func TestUnmarshalJSONNilReceiversReturnErrors(t *testing.T) {
	assertErrorWithoutPanic := func(name string, call func() error) {
		t.Helper()
		t.Run(name, func(t *testing.T) {
			defer func() {
				if recovered := recover(); recovered != nil {
					t.Errorf("UnmarshalJSON panicked: %v", recovered)
				}
			}()
			if err := call(); err == nil {
				t.Error("UnmarshalJSON returned nil error for nil receiver")
			}
		})
	}
	var commandID *CommandID
	var event *Event
	var batch *Batch
	assertErrorWithoutPanic("CommandID", func() error { return commandID.UnmarshalJSON([]byte(`"help"`)) })
	assertErrorWithoutPanic("Event", func() error { return event.UnmarshalJSON([]byte(fixedEventJSON)) })
	assertErrorWithoutPanic("Batch", func() error { return batch.UnmarshalJSON([]byte(fixedBatchJSON)) })
}

func TestUnmarshalJSONFailuresLeaveReceiversUnchanged(t *testing.T) {
	eventSeed := fixedEvent()
	eventSeed.CommandID = CommandVersion
	for name, raw := range map[string]string{
		"unknown field": strings.TrimSuffix(fixedEventJSON, "}") + `,"extra":true}`,
		"duplicate key": strings.Replace(fixedEventJSON, `"app":"gascity"`, `"app":"gascity","app":"gascity"`, 1),
		"case alias":    strings.Replace(fixedEventJSON, `"event_id"`, `"EVENT_ID"`, 1),
		"invalid value": strings.Replace(fixedEventJSON, `"command_id":"help"`, `"command_id":"not-a-member"`, 1),
		"malformed":     `{`,
		"null":          `null`,
	} {
		t.Run("event "+name, func(t *testing.T) {
			got := eventSeed
			if err := got.UnmarshalJSON([]byte(raw)); err == nil {
				t.Fatal("UnmarshalJSON unexpectedly succeeded")
			}
			if got != eventSeed {
				t.Fatalf("receiver changed to %#v, want %#v", got, eventSeed)
			}
		})
	}

	batchSeed := Batch{SchemaVersion: SchemaVersionV1, Events: []Event{eventSeed}}
	for name, raw := range map[string]string{
		"unknown field": strings.TrimSuffix(fixedBatchJSON, "}") + `,"extra":true}`,
		"duplicate key": strings.Replace(fixedBatchJSON, `"events":`, `"events":[],"events":`, 1),
		"case alias":    strings.Replace(fixedBatchJSON, `"schema_version"`, `"SCHEMA_VERSION"`, 1),
		"invalid value": strings.Replace(fixedBatchJSON, `"schema_version":1`, `"schema_version":2`, 1),
		"malformed":     `[`,
		"null":          `null`,
	} {
		t.Run("batch "+name, func(t *testing.T) {
			got := Batch{SchemaVersion: batchSeed.SchemaVersion, Events: append([]Event(nil), batchSeed.Events...)}
			if err := got.UnmarshalJSON([]byte(raw)); err == nil {
				t.Fatal("UnmarshalJSON unexpectedly succeeded")
			}
			if !reflect.DeepEqual(got, batchSeed) {
				t.Fatalf("receiver changed to %#v, want %#v", got, batchSeed)
			}
		})
	}

	for name, raw := range map[string]string{
		"unknown":    `"not-a-member"`,
		"wrong type": `1`,
		"malformed":  `"`,
		"null":       `null`,
	} {
		t.Run("command ID "+name, func(t *testing.T) {
			got := CommandVersion
			if err := got.UnmarshalJSON([]byte(raw)); err == nil {
				t.Fatal("UnmarshalJSON unexpectedly succeeded")
			}
			if got != CommandVersion {
				t.Fatalf("receiver changed to %v, want version", got)
			}
		})
	}
}

func TestDecodeRejectsInvalidValuesAndBatchBounds(t *testing.T) {
	twentyFiveEvents := make([]string, MaxBatchEvents)
	for i := range twentyFiveEvents {
		twentyFiveEvents[i] = fixedEventJSON
	}
	maxBatchJSON := `{"schema_version":1,"events":[` + strings.Join(twentyFiveEvents, ",") + `]}`
	if _, err := DecodeBatch([]byte(maxBatchJSON)); err != nil {
		t.Fatalf("DecodeBatch(maximum batch): %v", err)
	}

	twentySixEvents := append(append([]string(nil), twentyFiveEvents...), fixedEventJSON)
	invalidUUID := strings.Replace(fixedEventJSON, "8c4f4128-a6e8-4f66-bd1b-1fcf1298b124", "8C4F4128-A6E8-4F66-BD1B-1FCF1298B124", 1)
	unknownCommand := strings.Replace(fixedEventJSON, `"command_id":"help"`, `"command_id":"definitely-not-a-command"`, 1)
	for name, raw := range map[string]string{
		"null event":      `null`,
		"missing fields":  `{}`,
		"invalid UUID":    invalidUUID,
		"unknown command": unknownCommand,
		"unknown schema":  strings.Replace(fixedBatchJSON, `"schema_version":1`, `"schema_version":2`, 1),
		"empty batch":     `{"schema_version":1,"events":[]}`,
		"oversized batch": `{"schema_version":1,"events":[` + strings.Join(twentySixEvents, ",") + `]}`,
	} {
		t.Run(name, func(t *testing.T) {
			var err error
			if strings.Contains(name, "batch") || name == "unknown schema" {
				_, err = DecodeBatch([]byte(raw))
			} else {
				_, err = DecodeEvent([]byte(raw))
			}
			if err == nil {
				t.Fatal("decoder unexpectedly accepted an invalid contract value")
			}
		})
	}
}

func TestValidationRejectsValuesOutsideClosedContract(t *testing.T) {
	tests := map[string]func(*Event){
		"event UUID noncanonical":         func(e *Event) { e.EventID = strings.ToUpper(e.EventID) },
		"event UUID wrong version":        func(e *Event) { e.EventID = "8c4f4128-a6e8-3f66-bd1b-1fcf1298b124" },
		"installation UUID wrong variant": func(e *Event) { e.InstallationID = "3cf9fd4e-3337-4c29-70ab-2858cd8a1f21" },
		"wrong app":                       func(e *Event) { e.App = "beads" },
		"development release":             func(e *Event) { e.ReleaseVersion = "development" },
		"noncanonical semver":             func(e *Event) { e.ReleaseVersion = "v0.31.0" },
		"unsupported OS":                  func(e *Event) { e.OS = OperatingSystem("windows") },
		"non-hour timestamp":              func(e *Event) { e.OccurredHourUTC = "2026-07-11T00:01:00Z" },
		"non-UTC timestamp":               func(e *Event) { e.OccurredHourUTC = "2026-07-11T00:00:00+01:00" },
		"unknown command":                 func(e *Event) { e.CommandID = CommandID(65535) },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			event := fixedEvent()
			mutate(&event)
			if _, err := EncodeEvent(event); err == nil {
				t.Fatal("EncodeEvent unexpectedly accepted invalid event")
			}
		})
	}

	for name, batch := range map[string]Batch{
		"unknown schema":  {SchemaVersion: 2, Events: []Event{fixedEvent()}},
		"empty events":    {SchemaVersion: SchemaVersionV1},
		"too many events": {SchemaVersion: SchemaVersionV1, Events: make([]Event, MaxBatchEvents+1)},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := EncodeBatch(batch); err == nil {
				t.Fatal("EncodeBatch unexpectedly accepted invalid batch")
			}
		})
	}
}

func TestPermanentCommandSentinels(t *testing.T) {
	want := map[CommandID]string{
		CommandHelp:        "help",
		CommandVersion:     "version",
		CommandUnknown:     "unknown",
		CommandPackCommand: "pack-command",
	}
	for id, wire := range want {
		if got := id.String(); got != wire {
			t.Errorf("%v.String() = %q, want %q", id, got, wire)
		}
		if len(wire) > 64 {
			t.Errorf("sentinel %q exceeds 64 bytes", wire)
		}
		for i := range len(wire) {
			if wire[i] < 0x20 || wire[i] > 0x7e {
				t.Errorf("sentinel %q is not printable ASCII", wire)
			}
		}
	}
}

func TestInjectedImmutableCommandCatalogRoundTripsWithoutExpandingProduction(t *testing.T) {
	const injectedID CommandID = 1000
	event := fixedEvent()
	event.CommandID = injectedID
	if _, err := EncodeEvent(event); err == nil {
		t.Fatal("production encoder accepted a non-sentinel ID from an injected-only catalog")
	}

	generatedCount := 0
	generatedCommandIDCatalog(func(commandIDEntry) { generatedCount++ })
	if generatedCount != 190 {
		t.Fatalf("generated production catalog has %d entries, want 190", generatedCount)
	}

	injected := func(yield func(commandIDEntry)) {
		productionCommandIDCatalog(yield)
		yield(commandIDEntry{id: injectedID, wire: "injected-only"})
	}
	encoded, err := encodeEventWithCommandIDCatalog(event, injected)
	if err != nil {
		t.Fatalf("encodeEventWithCommandIDCatalog: %v", err)
	}
	if !strings.Contains(string(encoded), `"command_id":"injected-only"`) {
		t.Fatalf("injected encoding = %s, want injected-only wire ID", encoded)
	}
	decoded, err := decodeEventWithCommandIDCatalog(encoded, injected)
	if err != nil {
		t.Fatalf("decodeEventWithCommandIDCatalog: %v", err)
	}
	if decoded != event {
		t.Fatalf("injected round trip = %#v, want %#v", decoded, event)
	}
	if _, err := DecodeEvent(encoded); err == nil {
		t.Fatal("production decoder accepted a non-sentinel ID from an injected-only catalog")
	}
}

func TestExampleMatchesGoldenWithoutAmbientInputs(t *testing.T) {
	want, err := os.ReadFile("testdata/example-v1.json")
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{"", "/tmp/should-not-matter", "https://invalid.example"} {
		t.Setenv("HOME", value)
		t.Setenv("GC_PRODUCT_METRICS_ENDPOINT", value)
		got, err := EncodeBatch(ExampleBatch())
		if err != nil {
			t.Fatalf("EncodeBatch(ExampleBatch): %v", err)
		}
		if string(got) != strings.TrimSpace(string(want)) {
			t.Fatalf("example mismatch\n got: %s\nwant: %s", got, want)
		}
	}
	if got := ExampleBatch().Events[0].CommandID; got != CommandHelp {
		t.Fatalf("example command = %v, want help", got)
	}
}

func TestDTOsHaveExactClosedShape(t *testing.T) {
	assertFields := func(typ reflect.Type, want []string) {
		t.Helper()
		if typ.NumField() != len(want) {
			t.Fatalf("%s has %d fields, want %d", typ, typ.NumField(), len(want))
		}
		for i, name := range want {
			field := typ.Field(i)
			if field.Name != name {
				t.Errorf("%s field %d = %s, want %s", typ, i, field.Name, name)
			}
		}
	}
	assertFields(reflect.TypeOf(Event{}), []string{"EventID", "InstallationID", "App", "ReleaseVersion", "OS", "OccurredHourUTC", "CommandID"})
	assertFields(reflect.TypeOf(Batch{}), []string{"SchemaVersion", "Events"})

	for _, typ := range []reflect.Type{reflect.TypeOf(Event{}), reflect.TypeOf(Batch{})} {
		assertNoOpenDTOType(t, typ, map[reflect.Type]bool{})
	}
}

func assertNoOpenDTOType(t *testing.T, typ reflect.Type, seen map[reflect.Type]bool) {
	t.Helper()
	if seen[typ] {
		return
	}
	seen[typ] = true
	if typ == reflect.TypeOf(json.RawMessage{}) || typ == reflect.TypeOf(time.Duration(0)) || typ == reflect.TypeOf((*error)(nil)).Elem() {
		t.Fatalf("DTO contains forbidden type %s", typ)
	}
	switch typ.Kind() {
	case reflect.Map, reflect.Interface:
		t.Fatalf("DTO contains open type %s", typ)
	case reflect.Array, reflect.Pointer, reflect.Slice:
		assertNoOpenDTOType(t, typ.Elem(), seen)
	case reflect.Struct:
		for i := 0; i < typ.NumField(); i++ {
			assertNoOpenDTOType(t, typ.Field(i).Type, seen)
		}
	}
}
