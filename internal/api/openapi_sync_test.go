package api_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/api"
)

// TestOpenAPISpecInSync enforces that the committed openapi.json file
// matches the spec the supervisor actually serves. If this test fails,
// regenerate the spec via:
//
//	go run ./cmd/genspec
//
// The supervisor is the single Huma API; a GET /openapi.json against it
// yields the authoritative contract for every HTTP endpoint the control
// plane exposes.
func TestOpenAPISpecInSync(t *testing.T) {
	sm := api.NewSupervisorMux(emptyTestResolver{}, nil, false, "", time.Time{})
	req := httptest.NewRequest(http.MethodGet, "/openapi.json", nil)
	rec := httptest.NewRecorder()
	sm.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /openapi.json returned %d: %s", rec.Code, rec.Body.String())
	}

	var live any
	if err := json.Unmarshal(rec.Body.Bytes(), &live); err != nil {
		t.Fatalf("parse live spec: %v", err)
	}
	var liveBuf bytes.Buffer
	enc := json.NewEncoder(&liveBuf)
	enc.SetIndent("", "  ")
	if err := enc.Encode(live); err != nil {
		t.Fatalf("encode live spec: %v", err)
	}

	// Every tracked copy of the spec must match the live server. The internal
	// copy (internal/api/openapi.json) feeds the Go client generator. The
	// docs copies (docs/schema/openapi.{json,txt}) are what Mintlify publishes
	// for external consumers. All three must agree or external readers see a
	// different contract than the code enforces.
	tracked := []string{
		"openapi.json",
		filepath.Join("..", "..", "docs", "schema", "openapi.json"),
		filepath.Join("..", "..", "docs", "schema", "openapi.txt"),
	}
	for _, specPath := range tracked {
		onDisk, err := os.ReadFile(specPath)
		if err != nil {
			t.Fatalf("read %s: %v (run `go run ./cmd/genspec` to create it)", specPath, err)
		}
		if !bytes.Equal(onDisk, liveBuf.Bytes()) {
			t.Errorf("%s is out of sync with the live server spec.\n"+
				"Run `go run ./cmd/genspec` to regenerate.\n"+
				"Live spec size: %d bytes, on-disk size: %d bytes",
				specPath, liveBuf.Len(), len(onDisk))
		}
	}
}

func TestEventsSchemaPublished(t *testing.T) {
	root := filepath.Join("..", "..")
	jsonPath := filepath.Join(root, "docs", "schema", "events.json")
	txtPath := filepath.Join(root, "docs", "schema", "events.txt")

	jsonData, err := os.ReadFile(jsonPath)
	if err != nil {
		t.Fatalf("read %s: %v (run `go run ./cmd/genspec` to create it)", jsonPath, err)
	}
	txtData, err := os.ReadFile(txtPath)
	if err != nil {
		t.Fatalf("read %s: %v (run `go run ./cmd/genspec` to create it)", txtPath, err)
	}
	if !bytes.Equal(jsonData, txtData) {
		t.Fatalf("%s and %s differ; run `go run ./cmd/genspec`", jsonPath, txtPath)
	}

	type schemaRef struct {
		Ref string `json:"$ref"`
	}
	var eventsDoc struct {
		AnyOf []schemaRef          `json:"anyOf"`
		Defs  map[string]schemaRef `json:"$defs"`
	}
	if err := json.Unmarshal(jsonData, &eventsDoc); err != nil {
		t.Fatalf("parse %s: %v", jsonPath, err)
	}

	wantRefs := []string{
		"openapi.json#/components/schemas/TypedEventStreamEnvelope",
		"openapi.json#/components/schemas/TypedTaggedEventStreamEnvelope",
		"openapi.json#/components/schemas/EventStreamEnvelope",
		"openapi.json#/components/schemas/TaggedEventStreamEnvelope",
	}
	gotRefs := make(map[string]bool, len(eventsDoc.AnyOf)+len(eventsDoc.Defs))
	for _, ref := range eventsDoc.AnyOf {
		gotRefs[ref.Ref] = true
	}
	for _, ref := range eventsDoc.Defs {
		gotRefs[ref.Ref] = true
	}
	for _, want := range wantRefs {
		if !gotRefs[want] {
			t.Errorf("%s is missing ref %q", jsonPath, want)
		}
	}

	openAPIData, err := os.ReadFile(filepath.Join(root, "docs", "schema", "openapi.json"))
	if err != nil {
		t.Fatalf("read openapi.json: %v", err)
	}
	var openAPI struct {
		Components struct {
			Schemas map[string]any `json:"schemas"`
		} `json:"components"`
	}
	if err := json.Unmarshal(openAPIData, &openAPI); err != nil {
		t.Fatalf("parse openapi.json: %v", err)
	}
	for _, component := range []string{"TypedEventStreamEnvelope", "TypedTaggedEventStreamEnvelope", "EventStreamEnvelope", "TaggedEventStreamEnvelope"} {
		if _, ok := openAPI.Components.Schemas[component]; !ok {
			t.Errorf("events schema references missing OpenAPI component %q", component)
		}
	}
}

func TestAsyncAcceptedRequestIDDescriptionsNameTypedResultEvents(t *testing.T) {
	sm := api.NewSupervisorMux(emptyTestResolver{}, nil, false, "", time.Time{})
	req := httptest.NewRequest(http.MethodGet, "/openapi.json", nil)
	rec := httptest.NewRecorder()
	sm.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /openapi.json returned %d: %s", rec.Code, rec.Body.String())
	}

	var openAPI struct {
		Components struct {
			Schemas map[string]struct {
				Properties map[string]struct {
					Description string `json:"description"`
				} `json:"properties"`
			} `json:"schemas"`
		} `json:"components"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &openAPI); err != nil {
		t.Fatalf("parse openapi: %v", err)
	}

	assertDescription := func(schema, want string) {
		t.Helper()
		got := openAPI.Components.Schemas[schema].Properties["request_id"].Description
		if !bytes.Contains([]byte(got), []byte(want)) {
			t.Fatalf("%s request_id description = %q, want to mention %q", schema, got, want)
		}
	}
	assertDescription("AsyncAcceptedBody", "request.result.session.create")
	assertDescription("AsyncAcceptedBody", "request.result.session.message")
	assertDescription("AsyncAcceptedBody", "request.result.session.submit")
	assertDescription("AsyncAcceptedResponse", "request.result.city.create")
	assertDescription("AsyncAcceptedResponse", "request.result.city.unregister")
}

func TestOrderResponseSchemaKeepsMigrationFieldsOptional(t *testing.T) {
	sm := api.NewSupervisorMux(emptyTestResolver{}, nil, false, "", time.Time{})
	req := httptest.NewRequest(http.MethodGet, "/openapi.json", nil)
	rec := httptest.NewRecorder()
	sm.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /openapi.json returned %d: %s", rec.Code, rec.Body.String())
	}

	var spec map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &spec); err != nil {
		t.Fatalf("parse live spec: %v", err)
	}

	components, ok := spec["components"].(map[string]any)
	if !ok {
		t.Fatal("openapi components missing")
	}
	schemas, ok := components["schemas"].(map[string]any)
	if !ok {
		t.Fatal("openapi schemas missing")
	}
	schema, ok := schemas["OrderResponse"].(map[string]any)
	if !ok {
		t.Fatal("OrderResponse schema missing")
	}

	if required, ok := schema["required"].([]any); ok {
		for _, item := range required {
			field, _ := item.(string)
			if field == "trigger" || field == "gate" {
				t.Fatalf("OrderResponse.%s should stay optional during migration; required=%v", field, required)
			}
		}
	}

	properties, ok := schema["properties"].(map[string]any)
	if !ok {
		t.Fatal("OrderResponse properties missing")
	}
	gate, ok := properties["gate"].(map[string]any)
	if !ok {
		t.Fatal("OrderResponse.gate property missing")
	}
	if deprecated, _ := gate["deprecated"].(bool); !deprecated {
		t.Fatalf("OrderResponse.gate should be deprecated; property=%v", gate)
	}
	if _, ok := properties["trigger"].(map[string]any); !ok {
		t.Fatal("OrderResponse.trigger property missing")
	}
}

// emptyTestResolver is a CityResolver with no cities. Huma schema
// generation is reflection-based and never calls resolver methods.
type emptyTestResolver struct{}

func (emptyTestResolver) ListCities() []api.CityInfo   { return nil }
func (emptyTestResolver) CityState(_ string) api.State { return nil }
