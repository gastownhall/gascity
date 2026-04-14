// Command specgen generates AsyncAPI and OpenAPI specs from the Go type registry.
//
// Usage:
//
//	go run ./cmd/specgen
//
// This writes:
//   - contracts/supervisor-ws/asyncapi.yaml (WebSocket API spec)
//   - contracts/http/openapi.yaml (HTTP API spec)
//
// The specs are embedded at build time and served at:
//   - GET /v0/asyncapi.yaml
//   - GET /v0/openapi.yaml
//
// The AsyncAPI spec is generated using swaggest/go-asyncapi (spec-2.4.0)
// and the OpenAPI spec uses swaggest/openapi-go (OpenAPI 3.1.0). Both
// libraries produce spec-compliant output with proper JSON Schema from
// Go struct reflection.
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"

	gcapi "github.com/gastownhall/gascity/internal/api"
	"github.com/gastownhall/gascity/internal/api/specgen"
)

func main() {
	// Import all dispatch_*.go registrations by referencing the api package.
	registry := gcapi.ActionTableRegistry()

	root := repoRoot()
	asyncAPIPath := filepath.Join(root, "contracts", "supervisor-ws", "asyncapi.yaml")
	openAPIPath := filepath.Join(root, "contracts", "http", "openapi.yaml")

	asyncAPIYAML, err := specgen.GenerateAsyncAPI(registry, gcapi.EnvelopeTypes())
	if err != nil {
		fmt.Fprintf(os.Stderr, "generate asyncapi: %v\n", err)
		os.Exit(1)
	}
	if err := os.WriteFile(asyncAPIPath, asyncAPIYAML, 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "write %s: %v\n", asyncAPIPath, err)
		os.Exit(1)
	}
	fmt.Printf("wrote %s (%d actions, %d bytes)\n", asyncAPIPath, len(registry.ActionNames()), len(asyncAPIYAML))

	openAPIYAML, err := specgen.GenerateOpenAPI()
	if err != nil {
		fmt.Fprintf(os.Stderr, "generate openapi: %v\n", err)
		os.Exit(1)
	}
	if err := os.WriteFile(openAPIPath, openAPIYAML, 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "write %s: %v\n", openAPIPath, err)
		os.Exit(1)
	}
	fmt.Printf("wrote %s (%d bytes)\n", openAPIPath, len(openAPIYAML))
}

func repoRoot() string {
	_, filename, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(filename), "..", "..")
}
