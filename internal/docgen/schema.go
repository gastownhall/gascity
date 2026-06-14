// Package docgen generates JSON Schema and markdown documentation from
// Gas City's Go config structs.
package docgen

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

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

// addGoCommentsFiltered calls r.AddGoComments for each visible (non-hidden)
// top-level directory under root, skipping any directory whose name begins
// with ".". CWD must already be set to root before calling.
//
// This avoids the TOCTOU failure where .gc/*/pr-checkout/ dirs are deleted by
// mpr cleanup while filepath.Walk is in progress: r.AddGoComments("module",
// ".") walks the entire tree including .gc/; if a directory disappears
// mid-scan the walk surfaces an I/O error that propagates up and fails schema
// generation. By enumerating only visible top-level dirs, we never enter .gc/.
func addGoCommentsFiltered(r *jsonschema.Reflector, module, root string) error {
	entries, err := os.ReadDir(root)
	if err != nil {
		return fmt.Errorf("reading %s: %w", root, err)
	}
	for _, entry := range entries {
		if !entry.IsDir() || strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		if err := r.AddGoComments(module, entry.Name()); err != nil {
			return fmt.Errorf("extracting Go comments from %s: %w", entry.Name(), err)
		}
	}
	return nil
}

// newReflector creates a jsonschema.Reflector configured for TOML field
// names with Go doc comments extracted from the source tree.
//
// AddGoComments requires CWD to be set to the module root so that
// filepath.Walk produces paths like "internal/config" which map to the
// correct import paths. Hidden directories (names beginning with ".") are
// excluded via addGoCommentsFiltered.
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
	if err := addGoCommentsFiltered(r, "github.com/gastownhall/gascity", "."); err != nil {
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
		"Use [imports.*] for PackV2 composition; legacy includes and [[agent]] fields remain visible for migration compatibility. " +
		"Legacy [packs.*] entries are still accepted by the runtime for migration/fetch compatibility but are intentionally omitted from this public schema.\n\n" +
		"> **PackV2 format source of truth:** The public PackV2 format and loader semantics are specified in [Gas City Pack Specification (2.0)](/specs/pack-spec)."
	removeRequiredField(s, "DaemonConfig", "formula_v2")
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
		"a pack's metadata, providers, services, commands, and import surface. " +
		"PackV2 agent authoring uses agents/<name>/agent.toml; inline [[agent]] " +
		"tables remain schema-visible for migration compatibility. Cities and rigs " +
		"compose packs via [imports.*]."
	return s, nil
}

func removeRequiredField(s *jsonschema.Schema, definitionName, fieldName string) {
	if s == nil || s.Definitions == nil {
		return
	}
	def := s.Definitions[definitionName]
	if def == nil || len(def.Required) == 0 {
		return
	}
	required := def.Required[:0]
	for _, name := range def.Required {
		if name != fieldName {
			required = append(required, name)
		}
	}
	def.Required = required
}
