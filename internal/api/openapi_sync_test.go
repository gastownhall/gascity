package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// TestOpenAPISpecInSync enforces that the committed openapi.json file matches
// what the running server actually serves. If this test fails, regenerate
// the spec file via:
//
//	go run ./cmd/genspec > internal/api/openapi.json
//
// This is how we make the spec a first-class artifact of the repo — any
// change to Huma types, routes, or handlers forces a spec update in the
// same PR so downstream client generators stay in sync.
func TestOpenAPISpecInSync(t *testing.T) {
	state := newFakeState(t)
	srv := New(state)

	req := httptest.NewRequest(http.MethodGet, "/openapi.json", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /openapi.json returned %d", rec.Code)
	}

	// Normalize the live spec to the same pretty-printed form as the file.
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

	specPath := filepath.Join("openapi.json")
	onDisk, err := os.ReadFile(specPath)
	if err != nil {
		t.Fatalf("read %s: %v (run `go run ./cmd/genspec > internal/api/openapi.json` to create it)", specPath, err)
	}

	if !bytes.Equal(onDisk, liveBuf.Bytes()) {
		t.Fatalf("openapi.json is out of sync with the live server spec.\n"+
			"Run `go run ./cmd/genspec > internal/api/openapi.json` to regenerate.\n"+
			"Live spec size: %d bytes, on-disk size: %d bytes",
			liveBuf.Len(), len(onDisk))
	}
}
