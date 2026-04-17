package api_test

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/specmerge"
)

// TestOpenAPISpecInSync enforces that the committed openapi.json file
// matches the merged per-city + supervisor spec that the control plane
// actually serves. If this test fails, regenerate the spec via:
//
//	go run ./cmd/genspec > internal/api/openapi.json
//
// This is how the spec becomes a first-class artifact of the repo — any
// change to Huma types, routes, or handlers forces a spec update in the
// same PR so downstream client generators stay in sync.
func TestOpenAPISpecInSync(t *testing.T) {
	live, err := specmerge.Merged("/openapi.json")
	if err != nil {
		t.Fatalf("merged spec: %v", err)
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
		t.Fatalf("openapi.json is out of sync with the merged live spec.\n"+
			"Run `go run ./cmd/genspec > internal/api/openapi.json` to regenerate.\n"+
			"Live spec size: %d bytes, on-disk size: %d bytes",
			liveBuf.Len(), len(onDisk))
	}
}
