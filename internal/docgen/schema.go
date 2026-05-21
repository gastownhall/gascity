// Package docgen generates JSON Schema and markdown documentation from
// Gas City's Go config structs.
package docgen

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/invopop/jsonschema"
)

// ModuleRoot finds the repo root by walking up from the current directory
// looking for go.mod. Returns the absolute path.
func ModuleRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("getting working directory: %w", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("go.mod not found in any parent of %s", dir)
		}
		dir = parent
	}
}

// newReflector creates a jsonschema.Reflector configured for TOML field
// names with Go doc comments extracted from the source tree.
//
// AddGoComments requires the path parameter to be "." with the working
// directory set to the module root, so that filepath.Walk produces paths
// like "internal/config" which gopath.Join maps to the correct import path.
func newReflector() (*jsonschema.Reflector, error) {
	root, err := ModuleRoot()
	if err != nil {
		return nil, err
	}

	// Save and restore CWD — AddGoComments needs CWD at module root.
	orig, err := os.Getwd()
	if err != nil {
		return nil, fmt.Errorf("getting working directory: %w", err)
	}
	if err := os.Chdir(root); err != nil {
		return nil, fmt.Errorf("chdir to module root: %w", err)
	}
	defer func() { _ = os.Chdir(orig) }()

	r := &jsonschema.Reflector{
		FieldNameTag: "toml",
	}
	if err := r.AddGoComments("github.com/gastownhall/gascity", "."); err != nil {
		return nil, fmt.Errorf("extracting Go comments: %w", err)
	}
	return r, nil
}

// GenerateCitySchema produces a JSON Schema for the city.toml config format.
// It reflects the config.City struct using TOML field names and extracts
// doc comments as descriptions.
func GenerateCitySchema() (*jsonschema.Schema, error) {
	r, err := newReflector()
	if err != nil {
		return nil, err
	}
	s := r.Reflect(&config.City{})
	s.Title = "Gas City Configuration"
	s.Description = "Schema for city.toml — the PackV2 deployment file for a Gas City instance. " +
		"Pack definitions live in pack.toml and conventional pack directories such as agents/, formulas/, orders/, and commands/. " +
		"Use [imports.*] for PackV2 composition; legacy includes, [packs.*], and [[agent]] fields remain visible for migration compatibility."
	return s, nil
}

// GeneratePackSchema produces a JSON Schema for the pack.toml manifest
// format (PackV2). It reflects the config.PackConfig struct using TOML
// field names and extracts doc comments as descriptions.
func GeneratePackSchema() (*jsonschema.Schema, error) {
	r, err := newReflector()
	if err != nil {
		return nil, err
	}
	s := r.Reflect(&config.PackConfig{})
	s.Title = "Gas City Pack Manifest"
	s.Description = "Schema for pack.toml — the PackV2 manifest that declares " +
		"a pack's metadata, agents, providers, services, commands, and import surface. " +
		"Cities and rigs compose packs via [imports.*]."
	return s, nil
}
