// Command genspec writes the live OpenAPI 3.1 spec to disk so downstream
// clients (CLI, dashboard, third-party consumers) can be generated from
// it. The spec is the merged per-city + supervisor spec (see
// internal/specmerge), which is the authoritative contract for every
// HTTP endpoint the control plane exposes.
//
// Usage:
//
//	go run ./cmd/genspec > internal/api/openapi.json
//
// This is the "spec drives everything" entry point: the committed spec
// is the contract; if it drifts from what the server actually serves,
// TestOpenAPISpecInSync fails.
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"

	"github.com/gastownhall/gascity/internal/specmerge"
)

func main() {
	spec, err := specmerge.Merged("/openapi.json")
	if err != nil {
		fmt.Fprintf(os.Stderr, "merged spec: %v\n", err)
		os.Exit(1)
	}

	var out bytes.Buffer
	enc := json.NewEncoder(&out)
	enc.SetIndent("", "  ")
	if err := enc.Encode(spec); err != nil {
		fmt.Fprintf(os.Stderr, "encode spec: %v\n", err)
		os.Exit(1)
	}
	if _, err := os.Stdout.Write(out.Bytes()); err != nil {
		fmt.Fprintf(os.Stderr, "write: %v\n", err)
		os.Exit(1)
	}
}
