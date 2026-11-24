package orders

import (
	"errors"
	"path/filepath"

	"github.com/gastownhall/gascity/internal/fsys"
)

// orderDir is the subdirectory name within formula layers that contains orders.
const orderDir = "orders"

// orderFileName is the expected filename inside each order subdirectory.
const orderFileName = "order.toml"

// ScanRoot describes one order discovery root and, optionally, the
// formula layer it belongs to for PACK_DIR semantics.
type ScanRoot struct {
	Dir          string
	FormulaLayer string
}

// Scan discovers orders across formula layers. Wave 2 requires top-level flat
// order files; older PackV1 directory layouts now hard-error. Higher-priority
// layers (later in the slice) override lower ones by order name. Orders in the
// skip list are excluded. Disabled orders are returned so overrides can enable
// them; call FilterEnabled to get the active set.
func Scan(fs fsys.FS, formulaLayers []string, skip []string) ([]Order, error) {
	roots := make([]ScanRoot, 0, len(formulaLayers))
	for _, layer := range formulaLayers {
		roots = append(roots, ScanRoot{
			Dir:          filepath.Join(filepath.Dir(layer), orderDir),
			FormulaLayer: layer,
		})
	}
	return ScanRoots(fs, roots, skip)
}

// ScanRoots discovers orders across explicit order roots. Higher-priority
// roots (later in the slice) override lower ones by order name. Disabled
// orders are returned (FilterEnabled decides the active set); orders in the
// skip list are excluded.
func ScanRoots(fs fsys.FS, roots []ScanRoot, skip []string) ([]Order, error) {
	skipSet := make(map[string]bool, len(skip))
	for _, s := range skip {
		skipSet[s] = true
	}

	// Scan layers lowest → highest priority. Later entries override earlier ones.
	found := make(map[string]Order) // name → order
	var order []string              // preserve discovery order
	var legacyFindings []legacyOrderLayoutFinding

	for _, root := range roots {
		discovered, err := discoverRoot(fs, root)
		if err != nil {
			var legacyErr legacyOrderLayoutError
			if errors.As(err, &legacyErr) {
				legacyFindings = append(legacyFindings, legacyErr.findings...)
				continue
			}
			return nil, err
		}
		for _, a := range discovered {
			name := a.Name
			if _, exists := found[name]; !exists {
				order = append(order, name)
			}
			found[name] = a // higher-priority layer overwrites
		}
	}
	if len(legacyFindings) > 0 {
		return nil, legacyOrderLayoutError{findings: legacyFindings}
	}

	// Collect results, excluding skipped orders. Disabled orders stay in the
	// result so overrides can enable them; FilterEnabled decides the active set.
	var result []Order
	for _, name := range order {
		a := found[name]
		if skipSet[name] || hasSkippedAlias(a, skipSet) {
			continue
		}
		result = append(result, a)
	}
	return result, nil
}

func hasSkippedAlias(a Order, skipSet map[string]bool) bool {
	for _, alias := range a.skipAliases {
		if skipSet[alias] {
			return true
		}
	}
	return false
}
