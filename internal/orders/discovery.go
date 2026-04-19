package orders

import (
	"fmt"
	"log"
	"path/filepath"

	"github.com/gastownhall/gascity/internal/fsys"
)

// discoverRoot discovers orders for one logical root. It prefers the canonical
// flat .toml file format, then falls back to the deprecated infixed flat form,
// then the deprecated subdirectory format, then the deprecated formulas/orders
// legacy path.
func discoverRoot(fs fsys.FS, root ScanRoot) ([]Order, error) {
	return discoverRootWithOptions(fs, root, ScanOptions{})
}

func discoverRootWithOptions(fs fsys.FS, root ScanRoot, opts ScanOptions) ([]Order, error) {
	found := make(map[string]Order)
	var names []string

	add := func(name, source string, data []byte) error {
		a, err := Parse(data)
		if err != nil {
			return fmt.Errorf("order %q in %s: %w", name, source, err)
		}
		a.Name = name
		a.Source = source
		a.FormulaLayer = root.FormulaLayer
		if _, exists := found[name]; !exists {
			names = append(names, name)
		}
		found[name] = a
		return nil
	}

	if err := discoverFlatFiles(fs, root.Dir, found, add, opts); err != nil {
		return nil, err
	}
	if err := discoverSubdirectoryOrders(fs, root.Dir, found, func(name, source string, data []byte) error {
		warnDeprecatedPath(opts, "warning: deprecated order path %s; rename to orders/%s.toml", source, name)
		return add(name, source, data)
	}); err != nil {
		return nil, err
	}

	legacyDir := legacyOrdersDir(root.FormulaLayer)
	if legacyDir != "" && filepath.Clean(legacyDir) != filepath.Clean(root.Dir) {
		if err := discoverSubdirectoryOrders(fs, legacyDir, found, func(name, source string, data []byte) error {
			warnDeprecatedPath(opts, "warning: deprecated order path %s; move to orders/%s.toml", source, name)
			return add(name, source, data)
		}); err != nil {
			return nil, err
		}
	}

	result := make([]Order, 0, len(names))
	for _, name := range names {
		result = append(result, found[name])
	}
	return result, nil
}

func warnDeprecatedPath(opts ScanOptions, format string, args ...any) {
	if opts.SuppressDeprecatedPathWarnings {
		return
	}
	log.Printf(format, args...)
}

func discoverFlatFiles(fs fsys.FS, dir string, found map[string]Order, add func(name, source string, data []byte) error, opts ScanOptions) error {
	entries, err := fs.ReadDir(dir)
	if err != nil {
		return nil
	}
	// Two-pass scan: canonical .toml files win over legacy .order.toml files
	// regardless of ReadDir ordering. A legacy file is only consumed if no
	// canonical file (in this call OR an earlier call via `found`) supplies
	// the same name.
	scan := func(wantLegacy bool) error {
		for _, entry := range entries {
			if entry.IsDir() {
				continue
			}
			fileName := entry.Name()
			name, ok := TrimFlatOrderFilename(fileName)
			if !ok {
				continue
			}
			legacy := fileName == name+LegacyFlatOrderSuffix
			if legacy != wantLegacy {
				continue
			}
			if _, exists := found[name]; exists {
				continue
			}
			source := filepath.Join(dir, fileName)
			data, err := fs.ReadFile(source)
			if err != nil {
				continue
			}
			if legacy {
				warnDeprecatedPath(opts, "warning: deprecated order path %s; rename to orders/%s.toml", source, name)
			}
			if err := add(name, source, data); err != nil {
				return err
			}
		}
		return nil
	}
	if err := scan(false); err != nil {
		return err
	}
	return scan(true)
}

func discoverSubdirectoryOrders(fs fsys.FS, dir string, found map[string]Order, add func(name, source string, data []byte) error) error {
	entries, err := fs.ReadDir(dir)
	if err != nil {
		return nil
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		name := entry.Name()
		if _, exists := found[name]; exists {
			continue
		}
		source := filepath.Join(dir, name, orderFileName)
		data, err := fs.ReadFile(source)
		if err != nil {
			continue
		}
		if err := add(name, source, data); err != nil {
			return err
		}
	}
	return nil
}

func legacyOrdersDir(formulaLayer string) string {
	if formulaLayer == "" {
		return ""
	}
	return filepath.Join(formulaLayer, orderDir)
}
