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
// layers (later in the slice) override lower ones by order name. Disabled
// orders and those in the skip list are excluded.
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
// roots (later in the slice) override lower ones by order name.
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

	// Collect results, excluding disabled and skipped orders.
	var result []Order
	for _, name := range order {
		a := found[name]
		if !a.IsEnabled() {
			continue
		}
		if skipSet[name] {
			continue
		}
		result = append(result, a)
	}
	return result, nil
}
