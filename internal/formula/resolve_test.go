package formula

import (
	"maps"
	"os"
	"path/filepath"
	"testing"
)

func writeLayerFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		t.Fatal(err)
	}
	return abs
}

func TestResolve_MissingEverywhere(t *testing.T) {
	dir := t.TempDir()
	writeLayerFile(t, dir, "mol-a.toml", "")

	if _, ok := Resolve([]string{dir}, "mol-missing"); ok {
		t.Errorf("Resolve(mol-missing): expected not found")
	}
}

func TestResolve_LastWinsAcrossLayers(t *testing.T) {
	dir := t.TempDir()
	layer1 := filepath.Join(dir, "layer1")
	layer2 := filepath.Join(dir, "layer2")
	writeLayerFile(t, layer1, "mol-a.toml", "lower")
	want := writeLayerFile(t, layer2, "mol-a.toml", "higher")

	got, ok := Resolve([]string{layer1, layer2}, "mol-a")
	if !ok {
		t.Fatalf("Resolve: not found")
	}
	if got != want {
		t.Errorf("Resolve = %q, want layer2 path %q", got, want)
	}
}

// TestResolve_ThreeLayersMiddleWins guards reverse-iteration off-by-one.
// Real callers (ComputeFormulaLayers) build 3+ layers; two-layer tests
// can pass with a `>` boundary as easily as `>=`.
func TestResolve_ThreeLayersMiddleWins(t *testing.T) {
	dir := t.TempDir()
	low := filepath.Join(dir, "low")
	mid := filepath.Join(dir, "mid")
	high := filepath.Join(dir, "high")
	writeLayerFile(t, low, "mol-a.toml", "low")
	want := writeLayerFile(t, mid, "mol-a.toml", "mid")
	if err := os.MkdirAll(high, 0o755); err != nil {
		t.Fatal(err)
	}

	got, ok := Resolve([]string{low, mid, high}, "mol-a")
	if !ok {
		t.Fatalf("Resolve: not found")
	}
	if got != want {
		t.Errorf("Resolve = %q, want mid layer path %q (highest layer with the formula)", got, want)
	}
}

func TestResolve_CanonicalBeatsLegacyWithinLayer(t *testing.T) {
	dir := t.TempDir()
	want := writeLayerFile(t, dir, "mol-a.toml", "canonical")
	writeLayerFile(t, dir, "mol-a.formula.toml", "legacy")

	got, ok := Resolve([]string{dir}, "mol-a")
	if !ok {
		t.Fatalf("Resolve: not found")
	}
	if got != want {
		t.Errorf("Resolve = %q, want canonical %q", got, want)
	}
}

func TestResolve_JSONFallback(t *testing.T) {
	dir := t.TempDir()
	want := writeLayerFile(t, dir, "mol-a.formula.json", "")

	got, ok := Resolve([]string{dir}, "mol-a")
	if !ok {
		t.Fatalf("Resolve: not found")
	}
	if got != want {
		t.Errorf("Resolve = %q, want JSON path %q", got, want)
	}
}

func TestResolve_HigherLayerLegacyWinsOverLowerCanonical(t *testing.T) {
	dir := t.TempDir()
	layer1 := filepath.Join(dir, "layer1")
	layer2 := filepath.Join(dir, "layer2")
	writeLayerFile(t, layer1, "mol-a.toml", "lower canonical")
	want := writeLayerFile(t, layer2, "mol-a.formula.toml", "higher legacy")

	got, ok := Resolve([]string{layer1, layer2}, "mol-a")
	if !ok {
		t.Fatalf("Resolve: not found")
	}
	if got != want {
		t.Errorf("Resolve = %q, want %q (cross-layer priority outranks within-layer extension preference)", got, want)
	}
}

func TestResolveAll_SingleLayer(t *testing.T) {
	dir := t.TempDir()
	a := writeLayerFile(t, dir, "mol-a.toml", "")
	b := writeLayerFile(t, dir, "mol-b.formula.toml", "")

	got := ResolveAll([]string{dir})
	want := map[string]string{"mol-a": a, "mol-b": b}
	if !maps.Equal(got, want) {
		t.Errorf("ResolveAll = %v, want %v", got, want)
	}
}

func TestResolveAll_LayerShadowing(t *testing.T) {
	dir := t.TempDir()
	layer1 := filepath.Join(dir, "layer1")
	layer2 := filepath.Join(dir, "layer2")
	writeLayerFile(t, layer1, "mol-a.toml", "lower")
	bOnlyLow := writeLayerFile(t, layer1, "mol-b.toml", "lower-only")
	aHigh := writeLayerFile(t, layer2, "mol-a.toml", "higher")
	cOnlyHigh := writeLayerFile(t, layer2, "mol-c.toml", "higher-only")

	got := ResolveAll([]string{layer1, layer2})
	want := map[string]string{"mol-a": aHigh, "mol-b": bOnlyLow, "mol-c": cOnlyHigh}
	if !maps.Equal(got, want) {
		t.Errorf("ResolveAll = %v, want %v", got, want)
	}
}

func TestResolveAll_CanonicalBeatsLegacyWithinLayer(t *testing.T) {
	// ResolveAll re-implements within-layer canonical-beats-legacy
	// independently of Resolve (it iterates entries from os.ReadDir, not
	// the by-name Stat path). Keep this even though Resolve has the same
	// invariant covered.
	dir := t.TempDir()
	canonical := writeLayerFile(t, dir, "mol-a.toml", "canonical")
	writeLayerFile(t, dir, "mol-a.formula.toml", "legacy")

	got := ResolveAll([]string{dir})
	if got["mol-a"] != canonical {
		t.Errorf("ResolveAll[mol-a] = %q, want canonical %q", got["mol-a"], canonical)
	}
}

func TestResolveAll_ExcludesJSON(t *testing.T) {
	// JSON formulas are loader-only fallback — they must not appear in the
	// symlink staging map even when they are the only file present for a
	// formula name.
	dir := t.TempDir()
	writeLayerFile(t, dir, "mol-json-only.formula.json", "")
	writeLayerFile(t, dir, "mol-toml.toml", "")

	got := ResolveAll([]string{dir})
	if _, ok := got["mol-json-only"]; ok {
		t.Errorf("ResolveAll picked up JSON-only formula: %v", got)
	}
	if _, ok := got["mol-toml"]; !ok {
		t.Errorf("ResolveAll missing TOML formula: %v", got)
	}
}

func TestResolveAll_EmptyAndMissing(t *testing.T) {
	if got := ResolveAll(nil); len(got) != 0 {
		t.Errorf("ResolveAll(nil) = %v, want empty", got)
	}
	if got := ResolveAll([]string{"/nonexistent"}); len(got) != 0 {
		t.Errorf("ResolveAll(missing) = %v, want empty", got)
	}
}

// TestResolveAndResolveAll_AgreeOnPath guards the whole point of this
// package: two consumers must not see a different winning path for the
// same name. Any divergence reintroduces the original bug class.
func TestResolveAndResolveAll_AgreeOnPath(t *testing.T) {
	dir := t.TempDir()
	low := filepath.Join(dir, "low")
	mid := filepath.Join(dir, "mid")
	high := filepath.Join(dir, "high")
	writeLayerFile(t, low, "shadowed.toml", "low")
	writeLayerFile(t, low, "low-only.toml", "low-only")
	writeLayerFile(t, mid, "shadowed.formula.toml", "mid")
	writeLayerFile(t, high, "shadowed.toml", "high")
	writeLayerFile(t, high, "high-only.formula.toml", "high-only")

	layers := []string{low, mid, high}
	all := ResolveAll(layers)
	for name, allPath := range all {
		resolvePath, ok := Resolve(layers, name)
		if !ok {
			t.Errorf("Resolve(%q) returned not-found, but ResolveAll has %q", name, allPath)
			continue
		}
		if resolvePath != allPath {
			t.Errorf("divergence on %q: Resolve=%q ResolveAll=%q", name, resolvePath, allPath)
		}
	}
}

// TestLoadByName_LastWinsAcrossLayers is the parser-level regression test
// for the bug Resolve fixes: parser.loadFormula previously iterated layers
// first-wins, inverting the lowest→highest priority contract that the
// symlink staging path (cmd/gc/formula_resolve.go) and the producer
// (config.ComputeFormulaLayers) both honor.
func TestLoadByName_LastWinsAcrossLayers(t *testing.T) {
	dir := t.TempDir()
	layer1 := filepath.Join(dir, "layer1")
	layer2 := filepath.Join(dir, "layer2")

	const lower = `{"formula":"mol-x","version":2,"type":"workflow","steps":[{"id":"s","title":"lower"}]}`
	const higher = `{"formula":"mol-x","version":2,"type":"workflow","steps":[{"id":"s","title":"higher"}]}`

	writeLayerFile(t, layer1, "mol-x.formula.json", lower)
	writeLayerFile(t, layer2, "mol-x.formula.json", higher)

	p := NewParser(layer1, layer2)
	got, err := p.LoadByName("mol-x")
	if err != nil {
		t.Fatalf("LoadByName(mol-x): %v", err)
	}
	if len(got.Steps) != 1 || got.Steps[0].Title != "higher" {
		t.Errorf("LoadByName picked wrong layer: title=%q, want %q", got.Steps[0].Title, "higher")
	}
}
