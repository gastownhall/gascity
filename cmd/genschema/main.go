// Command genschema generates JSON Schema and markdown reference docs
// from Gas City's Go config structs. Run from the repository root:
//
//	go run ./cmd/genschema
//
// Output:
//
//	docs/schema/city-schema.json
//	docs/schema/city-schema.txt
//	docs/reference/config.md
//	docs/reference/cli.md
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/gastownhall/gascity/internal/docgen"
	"github.com/invopop/jsonschema"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "genschema: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	// Validate we're at repo root.
	if _, err := os.Stat("go.mod"); err != nil {
		return fmt.Errorf("must run from repository root (go.mod not found)")
	}

	// Ensure output directories exist.
	for _, dir := range []string{"docs/schema", "docs/reference"} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("creating %s: %w", dir, err)
		}
	}

	// Generate schemas.
	citySchema, err := docgen.GenerateCitySchema()
	if err != nil {
		return fmt.Errorf("generating city schema: %w", err)
	}
	// Write JSON schema.
	if err := writeSchema("docs/schema/city-schema.json", citySchema); err != nil {
		return err
	}
	if err := writeSchema("docs/schema/city-schema.txt", citySchema); err != nil {
		return err
	}

	// Write markdown reference doc.
	if err := docgen.WriteMarkdown("docs/reference/config.md", citySchema); err != nil {
		return fmt.Errorf("writing config.md: %w", err)
	}

	// Generate CLI reference via "gc gen-doc" (has access to real command tree).
	genDoc := exec.Command("go", "run", "./cmd/gc", "gen-doc")
	genDoc.Stdout = os.Stdout
	genDoc.Stderr = os.Stderr
	if err := genDoc.Run(); err != nil {
		return fmt.Errorf("generating CLI docs: %w", err)
	}

	files := []string{
		"docs/schema/city-schema.json",
		"docs/schema/city-schema.txt",
		"docs/reference/config.md",
		"docs/reference/cli.md",
	}
	fmt.Println("Generated:")
	for _, f := range files {
		fmt.Printf("  %s\n", f)
	}
	return nil
}

// writeSchema writes a JSON Schema to a file using atomic write (temp + rename).
func writeSchema(path string, s *jsonschema.Schema) error {
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return fmt.Errorf("marshaling %s: %w", path, err)
	}
	data = append(data, '\n')

	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".genschema-*")
	if err != nil {
		return fmt.Errorf("creating temp file for %s: %w", path, err)
	}
	tmpName := tmp.Name()

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return fmt.Errorf("writing %s: %w", path, err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("closing %s: %w", path, err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("renaming %s: %w", path, err)
	}
	return nil
}
