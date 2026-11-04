package doctor

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/formula"
)

func TestConfigValidationCapturesExpansionReads(t *testing.T) {
	wasEnabled := formula.IsFormulaV2Enabled()
	formula.SetFormulaV2Enabled(true)
	t.Cleanup(func() { formula.SetFormulaV2Enabled(wasEnabled) })
	dir := t.TempDir()
	t.Setenv("GC_FORMULA_REF", "working-tree")
	inputs := map[string]string{
		"plan": `formula = "plan"
[requires]
formula_compiler = ">=2.0.0"
[[steps]]
id = "work"
title = "Work"
[compose]
[[compose.expand]]
target = "work"
with = "expansion"
`,
		"expansion": `formula = "expansion"
type = "expansion"
[requires]
formula_compiler = ">=2.0.0"
[[template]]
id = "{target}.check"
title = "Check"
`,
	}
	for name, content := range inputs {
		if err := os.WriteFile(filepath.Join(dir, name+".formula.toml"), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	cfg := &config.City{Rigs: []config.Rig{{Name: "alpha"}}, FormulaLayers: config.FormulaLayers{City: []string{dir}}}
	input := BuildConfigValidationInput(context.Background(), cfg, []string{"plan"})
	got := input.Formulas["alpha"]["plan"]
	if got.Error != "" {
		t.Fatal(got.Error)
	}
	if got.Vars == nil || len(*got.Vars) != 0 {
		t.Fatalf("empty declarations must be an array: %+v", got)
	}
	if got.Provenance.Status != "complete" || len(got.Provenance.Sources) != 2 {
		t.Fatalf("expansion contributor missing: %+v", got.Provenance)
	}
	// Zero-rig cities and empty request lists still have non-null collections.
	empty, err := json.Marshal(BuildConfigValidationInput(context.Background(), &config.City{}, nil))
	if err != nil {
		t.Fatal(err)
	}
	var decoded struct {
		Config struct {
			Agents []json.RawMessage
			Rigs   []json.RawMessage
		}
		Formulas map[string]json.RawMessage
	}
	if err := json.Unmarshal(empty, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Config.Agents == nil || decoded.Config.Rigs == nil || decoded.Formulas == nil {
		t.Fatalf("null collections: %s", empty)
	}
}

// changingValidationSource simulates source bytes changing after the first read.
type changingValidationSource struct {
	formula.Source
	reads int
}

func (s *changingValidationSource) ReadFile(_ string) ([]byte, error) {
	s.reads++
	if s.reads > 1 {
		return nil, fmt.Errorf("unexpected second source read")
	}
	return []byte("first input"), nil
}

func TestConfigValidationHashesActualRead(t *testing.T) {
	underlying := &changingValidationSource{}
	source := &configValidationSource{Source: underlying}
	data, err := source.ReadFile(filepath.Join(t.TempDir(), "input.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "first input" || underlying.reads != 1 || len(source.sources) != 1 || source.sources[0].ContentSHA256 != "3d565a8c19015cbccfe05d782ebaee121220bd5189feb45cd102282a96ee4cbd" {
		t.Fatalf("capture=%+v reads=%d", source.sources, underlying.reads)
	}
}

func TestConfigValidationNormalizesAbsentOverrideMap(t *testing.T) {
	cfg := &config.City{Rigs: []config.Rig{
		{Name: "alpha"},
		{Name: "beta", FormulaVars: map[string]string{"command": ""}},
	}}
	input := BuildConfigValidationInput(context.Background(), cfg, nil)
	if input.Config.Rigs[0].FormulaVars == nil {
		t.Fatal("validation input must encode absent formula overrides as {}, not null")
	}
	if cfg.Rigs[0].FormulaVars != nil {
		t.Fatal("normalizing input mutated the invocation config")
	}
	if value, ok := input.Config.Rigs[1].FormulaVars["command"]; !ok || value != "" {
		t.Fatal("explicit empty override was lost")
	}
}
