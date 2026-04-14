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
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"

	"github.com/gastownhall/gascity/internal/api/specgen"
)

func main() {
	registry := specgen.BuildRegistry()

	root := repoRoot()
	asyncAPIPath := filepath.Join(root, "contracts", "supervisor-ws", "asyncapi.yaml")
	openAPIPath := filepath.Join(root, "contracts", "http", "openapi.yaml")

	asyncAPIContent := specgen.GenerateAsyncAPI(registry)
	if err := os.WriteFile(asyncAPIPath, []byte(asyncAPIContent), 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "write %s: %v\n", asyncAPIPath, err)
		os.Exit(1)
	}
	fmt.Printf("wrote %s (%d actions)\n", asyncAPIPath, len(registry.ActionNames()))

	openAPIContent := specgen.GenerateOpenAPI()
	if err := os.WriteFile(openAPIPath, []byte(openAPIContent), 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "write %s: %v\n", openAPIPath, err)
		os.Exit(1)
	}
	fmt.Printf("wrote %s\n", openAPIPath)
}

func repoRoot() string {
	_, filename, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(filename), "..", "..")
}
