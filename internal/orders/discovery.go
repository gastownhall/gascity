package orders

import (
	"fmt"
	"log"
	"path/filepath"
	"strings"

	"github.com/gastownhall/gascity/internal/fsys"
)

const flatOrderSuffix = ".order.toml"

// discoverRoot discovers orders for one logical root. It prefers the V2 flat
// file format, then falls back to the deprecated subdirectory format, then the
// deprecated formulas/orders legacy path.
func discoverRoot(fs fsys.FS, root ScanRoot) ([]Order, error) {
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

	if err := discoverFlatFiles(fs, root.Dir, add); err != nil {
		return nil, err
	}
	if err := discoverSubdirectoryOrders(fs, root.Dir, found, func(name, source string, data []byte) error {
		log.Printf("warning: deprecated order path %s; rename to orders/%s.order.toml", source, name)
		return add(name, source, data)
	}); err != nil {
		return nil, err
	}

	legacyDir := legacyOrdersDir(root.FormulaLayer)
	if legacyDir != "" && filepath.Clean(legacyDir) != filepath.Clean(root.Dir) {
		if err := discoverSubdirectoryOrders(fs, legacyDir, found, func(name, source string, data []byte) error {
			log.Printf("warning: deprecated order path %s; move to orders/%s.order.toml", source, name)
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

func discoverFlatFiles(fs fsys.FS, dir string, add func(name, source string, data []byte) error) error {
	entries, err := fs.ReadDir(dir)
	if err != nil {
		return nil
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		fileName := entry.Name()
		if !strings.HasSuffix(fileName, flatOrderSuffix) {
			continue
		}
		name := strings.TrimSuffix(fileName, flatOrderSuffix)
		source := filepath.Join(dir, fileName)
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
