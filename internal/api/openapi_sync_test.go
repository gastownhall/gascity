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
	sm := api.NewSupervisorMux(emptyTestResolver{}, nil, false, "", "", time.Time{})
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
	// docs/reference/schema/openapi.json is the published docs artifact. The
	// .txt copy is a compatibility mirror kept in sync with the served JSON
	// contract.
	tracked := []string{
		"openapi.json",
		filepath.Join("..", "..", "docs", "reference", "schema", "openapi.json"),
		filepath.Join("..", "..", "docs", "reference", "schema", "openapi.txt"),
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
	jsonPath := filepath.Join(root, "docs", "reference", "schema", "events.json")
	txtPath := filepath.Join(root, "docs", "reference", "schema", "events.txt")

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

	openAPIData, err := os.ReadFile(filepath.Join(root, "docs", "reference", "schema", "openapi.json"))
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
	sm := api.NewSupervisorMux(emptyTestResolver{}, nil, false, "", "", time.Time{})
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

	assertCursorDescription := func(schema, want string) {
		t.Helper()
		got := openAPI.Components.Schemas[schema].Properties["event_cursor"].Description
		if !bytes.Contains([]byte(got), []byte(want)) {
			t.Fatalf("%s event_cursor description = %q, want to mention %q", schema, got, want)
		}
	}
	assertCursorDescription("AsyncAcceptedBody", "after_seq")
	assertCursorDescription("AsyncAcceptedResponse", "after_cursor")
	assertCursorDescription("AsyncAcceptedBody", "no event provider")
	assertCursorDescription("AsyncAcceptedResponse", "no event provider")

	got := openAPI.Components.Schemas["SessionCreateSucceededPayload"].Properties["session"].Description
	if !bytes.Contains([]byte(got), []byte("lifecycle commands")) {
		t.Fatalf("SessionCreateSucceededPayload session description = %q, want to mention lifecycle commands", got)
	}
}

func TestOrderResponseSchemaKeepsMigrationFieldsOptional(t *testing.T) {
	sm := api.NewSupervisorMux(emptyTestResolver{}, nil, false, "", "", time.Time{})
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

// TestOpenAPIBeadCloseReasonContract pins the served contract for why a bead
// was closed (gastownhall/gascity#2663): the Bead schema carries an optional
// close_reason string, and POST /bead/{id}/close takes an optional body whose
// reason is a string.
func TestOpenAPIBeadCloseReasonContract(t *testing.T) {
	sm := api.NewSupervisorMux(emptyTestResolver{}, nil, false, "", "", time.Time{})
	rec := httptest.NewRecorder()
	sm.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/openapi.json", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /openapi.json returned %d: %s", rec.Code, rec.Body.String())
	}
	var spec struct {
		Paths      map[string]map[string]json.RawMessage `json:"paths"`
		Components struct {
			Schemas map[string]struct {
				Properties map[string]struct {
					Type any `json:"type"`
				} `json:"properties"`
				Required []string `json:"required"`
			} `json:"schemas"`
		} `json:"components"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &spec); err != nil {
		t.Fatalf("parse spec: %v", err)
	}

	bead := spec.Components.Schemas["Bead"]
	prop, ok := bead.Properties["close_reason"]
	if !ok {
		t.Fatal("Bead schema has no close_reason property")
	}
	if prop.Type != "string" {
		t.Fatalf("Bead.close_reason type = %v, want string", prop.Type)
	}
	for _, name := range bead.Required {
		if name == "close_reason" {
			t.Fatal("Bead.close_reason is required, want optional")
		}
	}

	var op struct {
		RequestBody *struct {
			Required bool `json:"required"`
			Content  map[string]struct {
				Schema struct {
					Ref string `json:"$ref"`
				} `json:"schema"`
			} `json:"content"`
		} `json:"requestBody"`
	}
	raw, ok := spec.Paths["/v0/city/{cityName}/bead/{id}/close"]["post"]
	if !ok {
		t.Fatal("spec has no POST /v0/city/{cityName}/bead/{id}/close")
	}
	if err := json.Unmarshal(raw, &op); err != nil {
		t.Fatalf("parse close operation: %v", err)
	}
	if op.RequestBody == nil {
		t.Fatal("POST /bead/{id}/close has no request body")
	}
	if op.RequestBody.Required {
		t.Fatal("POST /bead/{id}/close request body is required, want optional")
	}
	ref := op.RequestBody.Content["application/json"].Schema.Ref
	const prefix = "#/components/schemas/"
	if len(ref) <= len(prefix) {
		t.Fatalf("close request body schema ref = %q", ref)
	}
	body, ok := spec.Components.Schemas[ref[len(prefix):]]
	if !ok {
		t.Fatalf("close request body schema %q not found", ref)
	}
	if reason, ok := body.Properties["reason"]; !ok || reason.Type != "string" {
		t.Fatalf("close request body properties = %+v, want a string reason", body.Properties)
	}
}
