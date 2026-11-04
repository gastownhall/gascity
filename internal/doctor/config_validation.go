package doctor

import (
	"context"
	"crypto/sha256"
	"fmt"
	"path/filepath"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/formula"
)

// ConfigValidationInput is point-in-time input for an opted-in pack diagnostic.
// It is supplied only by explicit config validation, never reused or cached, and
// is not an atomic filesystem snapshot or a guarantee about future execution.
type ConfigValidationInput struct {
	SchemaVersion string                                        `json:"schema_version"`
	Config        *config.City                                  `json:"config"`
	Formulas      map[string]map[string]ConfigValidationFormula `json:"formulas"`
}

// ConfigValidationFormula contains either compiled declarations and provenance,
// or an actionable discovery/compile error. The pack decides applicability.
type ConfigValidationFormula struct {
	Name       string                      `json:"name,omitempty"`
	Vars       *[]ConfigValidationVar      `json:"vars,omitempty"`
	Provenance *ConfigValidationProvenance `json:"provenance,omitempty"`
	Error      string                      `json:"error,omitempty"`
}

// ConfigValidationVar preserves declaration membership and absent/empty defaults.
type ConfigValidationVar struct {
	Name    string  `json:"name"`
	Default *string `json:"default,omitempty"`
}

// ConfigValidationProvenance describes evidence captured during compilation.
// Complete means every contributing read was captured. Unavailable must include
// a reason and must never be represented as proof of a source pin.
type ConfigValidationProvenance struct {
	Status  string                   `json:"status"`
	Sources []ConfigValidationSource `json:"sources"`
	Reason  string                   `json:"reason,omitempty"`
}

// ConfigValidationSource identifies bytes actually read at a selected path.
type ConfigValidationSource struct {
	Path          string `json:"path"`
	ContentSHA256 string `json:"content_sha256"`
}

// BuildConfigValidationInput compiles pack-requested names for every configured
// rig using this invocation's resolved formula layers and rig overrides. It does
// not reload configuration, select roles, or interpret formula policy.
func BuildConfigValidationInput(ctx context.Context, cfg *config.City, names []string) ConfigValidationInput {
	resolved := *cfg
	if resolved.Agents == nil {
		resolved.Agents = []config.Agent{}
	}
	if resolved.Rigs == nil {
		resolved.Rigs = []config.Rig{}
	}
	input := ConfigValidationInput{SchemaVersion: "1", Config: &resolved, Formulas: make(map[string]map[string]ConfigValidationFormula, len(cfg.Rigs))}
	for _, rig := range cfg.Rigs {
		entries := make(map[string]ConfigValidationFormula, len(names))
		input.Formulas[rig.Name] = entries
		for _, name := range names {
			source := &configValidationSource{Source: formula.SourceFromEnv(), sources: []ConfigValidationSource{}}
			recipe, err := formula.CompileWithoutRuntimeVarValidationFromSource(ctx, name, cfg.FormulaLayers.SearchPaths(rig.Name), rig.FormulaVars, source)
			if err != nil {
				entries[name] = ConfigValidationFormula{Error: fmt.Sprintf("rig %q formula %q: %v", rig.Name, name, err)}
				continue
			}
			vars := make([]ConfigValidationVar, 0, len(recipe.Vars))
			for _, vname := range recipe.VariableNames() {
				v := ConfigValidationVar{Name: vname}
				if def := recipe.Vars[vname]; def != nil {
					v.Default = def.Default
				}
				vars = append(vars, v)
			}
			provenance := &ConfigValidationProvenance{Status: "complete", Sources: source.sources}
			if source.unavailable != "" || len(source.sources) == 0 {
				provenance.Status = "unavailable"
				provenance.Reason = source.unavailable
				if provenance.Reason == "" {
					provenance.Reason = "compiler supplied no source reads"
				}
			}
			entries[name] = ConfigValidationFormula{Name: name, Vars: &vars, Provenance: provenance}
		}
	}
	return input
}

type configValidationSource struct {
	formula.Source
	sources     []ConfigValidationSource
	unavailable string
}

func (s *configValidationSource) ReadFile(path string) ([]byte, error) {
	data, err := s.Source.ReadFile(path)
	if err != nil {
		return nil, err
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		s.unavailable = fmt.Sprintf("resolving contributing source %q: %v", path, err)
		return data, nil
	}
	row := ConfigValidationSource{Path: absolute, ContentSHA256: fmt.Sprintf("%x", sha256.Sum256(data))}
	for _, prior := range s.sources {
		if prior == row {
			return data, nil
		}
	}
	s.sources = append(s.sources, row)
	return data, nil
}
