package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/events"
)

// TestEventStreamSchemaInSpec verifies that the events/stream endpoint
// has its event schemas (eventStreamEnvelope, HeartbeatEvent) documented
// in the OpenAPI spec — the whole point of Fix 1.
func TestEventStreamSchemaInSpec(t *testing.T) {
	spec := readCommittedOpenAPISpec(t)

	// Find the /v0/events/stream operation (supervisor-scope).
	paths, _ := spec["paths"].(map[string]any)
	streamPath, ok := paths["/v0/events/stream"].(map[string]any)
	if !ok {
		t.Fatal("/v0/events/stream not in spec")
	}
	get, ok := streamPath["get"].(map[string]any)
	if !ok {
		t.Fatal("/v0/events/stream GET not in spec")
	}

	// Check the 200 response has text/event-stream content with a schema.
	responses, _ := get["responses"].(map[string]any)
	ok200, _ := responses["200"].(map[string]any)
	content, _ := ok200["content"].(map[string]any)
	eventStream, ok := content["text/event-stream"].(map[string]any)
	if !ok {
		t.Fatal("200 response missing text/event-stream content")
	}
	if _, ok := eventStream["schema"]; !ok {
		t.Fatal("text/event-stream response missing schema")
	}

	// Serialize the spec and check event type schemas are referenced.
	bs, _ := json.Marshal(spec)
	specStr := string(bs)
	for _, want := range []string{"Event event", "Event heartbeat"} {
		if !strings.Contains(specStr, want) {
			t.Errorf("OpenAPI spec missing %q title", want)
		}
	}
}

// TestSSEEndpointsHaveSchemasInSpec verifies that every SSE endpoint has
// its event schemas documented in the OpenAPI spec. This enforces the
// "spec drives everything" principle: if a new SSE endpoint is added
// without registerSSE (skipping spec documentation), this test fails.
func TestSSEEndpointsHaveSchemasInSpec(t *testing.T) {
	spec := readCommittedOpenAPISpec(t)
	paths, _ := spec["paths"].(map[string]any)

	// All 3 SSE endpoints (+2 agent output variants = 4 streams total).
	sseEndpoints := []string{
		"/v0/events/stream",
		"/v0/city/{cityName}/session/{id}/stream",
		"/v0/city/{cityName}/agent/{base}/output/stream",
		"/v0/city/{cityName}/agent/{dir}/{base}/output/stream",
	}

	for _, path := range sseEndpoints {
		t.Run(path, func(t *testing.T) {
			p, ok := paths[path].(map[string]any)
			if !ok {
				t.Fatalf("path %s not in spec", path)
			}
			get, ok := p["get"].(map[string]any)
			if !ok {
				t.Fatalf("GET %s not in spec", path)
			}
			responses, _ := get["responses"].(map[string]any)
			ok200, _ := responses["200"].(map[string]any)
			content, _ := ok200["content"].(map[string]any)
			es, ok := content["text/event-stream"].(map[string]any)
			if !ok {
				t.Fatalf("%s 200 response has no text/event-stream content (missing schema!)", path)
			}
			if _, ok := es["schema"]; !ok {
				t.Fatalf("%s text/event-stream has no schema", path)
			}
		})
	}
}

func TestEventStreamsUseTypedEnvelopeUnions(t *testing.T) {
	tests := []struct {
		path      string
		eventName string
		wantRef   string
	}{
		{
			path:      "/v0/events/stream",
			eventName: "tagged_event",
			wantRef:   "#/components/schemas/TypedTaggedEventStreamEnvelope",
		},
		{
			path:      "/v0/city/{cityName}/events/stream",
			eventName: "event",
			wantRef:   "#/components/schemas/TypedEventStreamEnvelope",
		},
	}

	for _, source := range eventStreamSpecCases(t) {
		t.Run(source.name, func(t *testing.T) {
			for _, tt := range tests {
				t.Run(tt.path, func(t *testing.T) {
					gotRef := sseEventDataRef(t, source.spec, tt.path, tt.eventName)
					if gotRef != tt.wantRef {
						t.Fatalf("%s %s data ref = %q, want %q", tt.path, tt.eventName, gotRef, tt.wantRef)
					}
				})
			}

			schemas := componentSchemas(t, source.spec)
			for _, name := range []string{
				"EventStreamEnvelope",
				"TaggedEventStreamEnvelope",
				"TypedEventStreamEnvelope",
				"TypedTaggedEventStreamEnvelope",
			} {
				if _, ok := schemas[name]; !ok {
					t.Fatalf("components.schemas missing %s", name)
				}
			}
		})
	}
}

func TestTypedEventEnvelopeUnionsCoverKnownEventTypes(t *testing.T) {
	for _, source := range eventStreamSpecCases(t) {
		t.Run(source.name, func(t *testing.T) {
			for _, tc := range []struct {
				schemaName string
				cityField  bool
			}{
				{schemaName: "TypedEventStreamEnvelope"},
				{schemaName: "TypedTaggedEventStreamEnvelope", cityField: true},
			} {
				t.Run(tc.schemaName, func(t *testing.T) {
					assertTypedEventEnvelopeUnion(t, source.spec, tc.schemaName, tc.cityField)
				})
			}
		})
	}
}

func eventStreamSpecCases(t *testing.T) []struct {
	name string
	spec map[string]any
} {
	t.Helper()
	return []struct {
		name string
		spec map[string]any
	}{
		{name: "committed", spec: readCommittedOpenAPISpec(t)},
		{name: "live-supervisor", spec: readLiveSupervisorOpenAPISpec(t)},
	}
}

func readLiveSupervisorOpenAPISpec(t *testing.T) map[string]any {
	t.Helper()

	sm := newTestSupervisorMux(t, map[string]*fakeState{})
	req := httptest.NewRequest(http.MethodGet, "/openapi.json", nil)
	rec := httptest.NewRecorder()
	sm.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /openapi.json status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var spec map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &spec); err != nil {
		t.Fatalf("decode live openapi.json: %v", err)
	}
	return spec
}

func sseEventDataRef(t *testing.T, spec map[string]any, path, eventName string) string {
	t.Helper()

	paths, ok := spec["paths"].(map[string]any)
	if !ok {
		t.Fatal("OpenAPI paths missing")
	}
	pathItem, ok := paths[path].(map[string]any)
	if !ok {
		t.Fatalf("path %s missing", path)
	}
	get, ok := pathItem["get"].(map[string]any)
	if !ok {
		t.Fatalf("GET %s missing", path)
	}
	responses, ok := get["responses"].(map[string]any)
	if !ok {
		t.Fatalf("GET %s responses missing", path)
	}
	ok200, ok := responses["200"].(map[string]any)
	if !ok {
		t.Fatalf("GET %s 200 response missing", path)
	}
	content, ok := ok200["content"].(map[string]any)
	if !ok {
		t.Fatalf("GET %s 200 content missing", path)
	}
	stream, ok := content["text/event-stream"].(map[string]any)
	if !ok {
		t.Fatalf("GET %s text/event-stream content missing", path)
	}
	schema, ok := stream["schema"].(map[string]any)
	if !ok {
		t.Fatalf("GET %s stream schema missing", path)
	}
	items, ok := schema["items"].(map[string]any)
	if !ok {
		t.Fatalf("GET %s stream items schema missing", path)
	}
	oneOf, ok := items["oneOf"].([]any)
	if !ok {
		t.Fatalf("GET %s stream oneOf missing", path)
	}
	for _, branch := range oneOf {
		branchSchema, ok := branch.(map[string]any)
		if !ok {
			continue
		}
		properties, ok := branchSchema["properties"].(map[string]any)
		if !ok {
			continue
		}
		eventProperty, ok := properties["event"].(map[string]any)
		if !ok {
			continue
		}
		if got, _ := eventProperty["const"].(string); got != eventName {
			continue
		}
		dataProperty, ok := properties["data"].(map[string]any)
		if !ok {
			t.Fatalf("GET %s event %s data property missing", path, eventName)
		}
		ref, _ := dataProperty["$ref"].(string)
		return ref
	}
	t.Fatalf("GET %s SSE event %s not found", path, eventName)
	return ""
}

func assertTypedEventEnvelopeUnion(t *testing.T, spec map[string]any, schemaName string, cityField bool) {
	t.Helper()

	schemas := componentSchemas(t, spec)
	union, ok := schemas[schemaName]
	if !ok {
		t.Fatalf("components.schemas missing %s", schemaName)
	}
	oneOf, ok := union["oneOf"].([]any)
	if !ok {
		t.Fatalf("%s oneOf missing", schemaName)
	}
	discriminator := typedEventDiscriminatorMapping(t, union, schemaName)

	expectedPayloadRefs := expectedEventPayloadRefs(t)
	seen := map[string]int{}
	for _, branch := range oneOf {
		refObj, ok := branch.(map[string]any)
		if !ok {
			t.Fatalf("%s oneOf branch is not an object: %#v", schemaName, branch)
		}
		ref, _ := refObj["$ref"].(string)
		if ref == "" {
			t.Fatalf("%s oneOf branch missing $ref: %#v", schemaName, branch)
		}
		variant := schemaByRef(t, schemas, ref)
		properties, ok := variant["properties"].(map[string]any)
		if !ok {
			t.Fatalf("%s variant %s properties missing", schemaName, ref)
		}
		typeProperty, ok := properties["type"].(map[string]any)
		if !ok {
			t.Fatalf("%s variant %s type property missing", schemaName, ref)
		}
		if _, ok := typeProperty["not"].(map[string]any); ok {
			assertCustomEventEnvelopeVariant(t, schemaName, ref, cityField, properties, variant, typeProperty)
			seen[""]++
			continue
		}
		eventType := constOrSingleEnum(t, typeProperty)
		wantPayloadRef, ok := expectedPayloadRefs[eventType]
		if !ok {
			t.Fatalf("%s variant %s has unknown event type %q", schemaName, ref, eventType)
		}
		seen[eventType]++
		if gotRef := discriminator[eventType]; gotRef != ref {
			t.Fatalf("%s discriminator mapping for %s = %q, want %q", schemaName, eventType, gotRef, ref)
		}

		payloadProperty, ok := properties["payload"].(map[string]any)
		if !ok {
			t.Fatalf("%s variant %s payload property missing", schemaName, ref)
		}
		gotPayloadRef, _ := payloadProperty["$ref"].(string)
		if gotPayloadRef != wantPayloadRef {
			t.Fatalf("%s variant %s payload ref = %q, want %q", schemaName, eventType, gotPayloadRef, wantPayloadRef)
		}

		wantRequired := []string{"seq", "type", "ts", "actor", "payload"}
		wantProperties := []string{"seq", "type", "ts", "actor", "subject", "message", "workflow", "payload"}
		if cityField {
			wantRequired = append(wantRequired, "city")
			wantProperties = append(wantProperties, "city")
		}
		assertProperties(t, schemaName, eventType, properties, wantProperties)
		assertRequiredFields(t, schemaName, eventType, variant, wantRequired)
	}

	var missing, duplicate []string
	for _, eventType := range events.KnownEventTypes {
		switch seen[eventType] {
		case 0:
			missing = append(missing, eventType)
		case 1:
		default:
			duplicate = append(duplicate, eventType)
		}
	}
	sort.Strings(missing)
	sort.Strings(duplicate)
	if len(missing) > 0 || len(duplicate) > 0 {
		t.Fatalf("%s event coverage mismatch; missing=%v duplicate=%v", schemaName, missing, duplicate)
	}
	if len(discriminator) != len(events.KnownEventTypes) {
		t.Fatalf("%s discriminator mapping count = %d, want %d", schemaName, len(discriminator), len(events.KnownEventTypes))
	}
	if seen[""] != 1 {
		t.Fatalf("%s custom event branch count = %d, want 1", schemaName, seen[""])
	}
	for eventType := range discriminator {
		if seen[eventType] == 0 {
			t.Fatalf("%s discriminator maps unknown event type %q", schemaName, eventType)
		}
	}
}

func assertCustomEventEnvelopeVariant(
	t *testing.T,
	schemaName string,
	ref string,
	cityField bool,
	properties map[string]any,
	variant map[string]any,
	typeProperty map[string]any,
) {
	t.Helper()

	notSchema, ok := typeProperty["not"].(map[string]any)
	if !ok {
		t.Fatalf("%s custom variant %s missing type.not schema", schemaName, ref)
	}
	rawEnum, ok := notSchema["enum"].([]any)
	if !ok {
		t.Fatalf("%s custom variant %s type.not.enum missing", schemaName, ref)
	}
	blocked := make(map[string]bool, len(rawEnum))
	for _, raw := range rawEnum {
		eventType, ok := raw.(string)
		if !ok {
			t.Fatalf("%s custom variant %s type.not.enum contains non-string %#v", schemaName, ref, raw)
		}
		blocked[eventType] = true
	}
	for _, eventType := range events.KnownEventTypes {
		if !blocked[eventType] {
			t.Fatalf("%s custom variant %s does not exclude known event type %q", schemaName, ref, eventType)
		}
	}
	if _, ok := typeProperty["const"]; ok {
		t.Fatalf("%s custom variant %s type schema must not have const", schemaName, ref)
	}
	if _, ok := typeProperty["enum"]; ok {
		t.Fatalf("%s custom variant %s type schema must not have enum", schemaName, ref)
	}
	payloadProperty, ok := properties["payload"].(map[string]any)
	if !ok {
		t.Fatalf("%s custom variant %s payload property missing", schemaName, ref)
	}
	if len(payloadProperty) != 0 {
		t.Fatalf("%s custom variant %s payload schema = %#v, want unconstrained custom JSON", schemaName, ref, payloadProperty)
	}

	wantRequired := []string{"seq", "type", "ts", "actor", "payload"}
	wantProperties := []string{"seq", "type", "ts", "actor", "subject", "message", "workflow", "payload"}
	if cityField {
		wantRequired = append(wantRequired, "city")
		wantProperties = append(wantProperties, "city")
	}
	assertProperties(t, schemaName, "custom", properties, wantProperties)
	assertRequiredFields(t, schemaName, "custom", variant, wantRequired)
}

func typedEventDiscriminatorMapping(t *testing.T, union map[string]any, schemaName string) map[string]string {
	t.Helper()

	rawDiscriminator, ok := union["discriminator"].(map[string]any)
	if !ok {
		t.Fatalf("%s discriminator missing", schemaName)
	}
	if got, _ := rawDiscriminator["propertyName"].(string); got != "type" {
		t.Fatalf("%s discriminator.propertyName = %q, want type", schemaName, got)
	}
	rawMapping, ok := rawDiscriminator["mapping"].(map[string]any)
	if !ok {
		t.Fatalf("%s discriminator.mapping missing", schemaName)
	}
	mapping := make(map[string]string, len(rawMapping))
	for eventType, rawRef := range rawMapping {
		ref, ok := rawRef.(string)
		if !ok || ref == "" {
			t.Fatalf("%s discriminator mapping for %s is not a ref: %#v", schemaName, eventType, rawRef)
		}
		mapping[eventType] = ref
	}
	return mapping
}

func componentSchemas(t *testing.T, spec map[string]any) map[string]map[string]any {
	t.Helper()

	components, ok := spec["components"].(map[string]any)
	if !ok {
		t.Fatal("OpenAPI components missing")
	}
	rawSchemas, ok := components["schemas"].(map[string]any)
	if !ok {
		t.Fatal("OpenAPI components.schemas missing")
	}
	schemas := make(map[string]map[string]any, len(rawSchemas))
	for name, raw := range rawSchemas {
		schema, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		schemas[name] = schema
	}
	return schemas
}

func expectedEventPayloadRefs(t *testing.T) map[string]string {
	t.Helper()

	registered := events.RegisteredPayloadTypes()
	expected := make(map[string]string, len(events.KnownEventTypes))
	for _, eventType := range events.KnownEventTypes {
		sample, ok := registered[eventType]
		if !ok {
			t.Fatalf("%s payload not registered", eventType)
		}
		expected[eventType] = "#/components/schemas/" + reflect.TypeOf(sample).Name()
	}
	return expected
}

func schemaByRef(t *testing.T, schemas map[string]map[string]any, ref string) map[string]any {
	t.Helper()

	const prefix = "#/components/schemas/"
	if !strings.HasPrefix(ref, prefix) {
		t.Fatalf("schema ref %q does not have prefix %q", ref, prefix)
	}
	name := strings.TrimPrefix(ref, prefix)
	schema, ok := schemas[name]
	if !ok {
		t.Fatalf("schema ref %q target missing", ref)
	}
	return schema
}

func constOrSingleEnum(t *testing.T, schema map[string]any) string {
	t.Helper()

	if value, ok := schema["const"].(string); ok {
		return value
	}
	enum, ok := schema["enum"].([]any)
	if !ok || len(enum) != 1 {
		t.Fatalf("schema has neither string const nor single-value enum: %#v", schema)
	}
	value, ok := enum[0].(string)
	if !ok {
		t.Fatalf("schema single enum value is not a string: %#v", enum[0])
	}
	return value
}

func assertProperties(t *testing.T, schemaName, eventType string, properties map[string]any, fields []string) {
	t.Helper()

	for _, field := range fields {
		if _, ok := properties[field]; !ok {
			t.Fatalf("%s variant %s missing property %q", schemaName, eventType, field)
		}
	}
}

func assertRequiredFields(t *testing.T, schemaName, eventType string, schema map[string]any, fields []string) {
	t.Helper()

	rawRequired, ok := schema["required"].([]any)
	if !ok {
		t.Fatalf("%s variant %s required fields missing", schemaName, eventType)
	}
	required := map[string]bool{}
	for _, raw := range rawRequired {
		name, _ := raw.(string)
		required[name] = true
	}
	for _, field := range fields {
		if !required[field] {
			t.Fatalf("%s variant %s missing required field %q; required=%v", schemaName, eventType, field, rawRequired)
		}
	}
}
