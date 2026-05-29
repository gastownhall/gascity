package orders

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/gastownhall/gascity/internal/fsys"
)

// discoverRoot discovers orders for one logical root. Wave 2 requires flat
// order files and treats selected flat order files as load-bearing config:
// unreadable files and older PackV1 subdirectory paths are hard errors.
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
	legacyFindings, err := findLegacySubdirectoryOrders(fs, root.Dir, "rename to orders/%s.toml")
	if err != nil {
		return nil, err
	}

	legacyDir := legacyOrdersDir(root.FormulaLayer)
	if legacyDir != "" && filepath.Clean(legacyDir) != filepath.Clean(root.Dir) {
		findings, err := findLegacySubdirectoryOrders(fs, legacyDir, "move to orders/%s.toml")
		if err != nil {
			return nil, err
		}
		legacyFindings = append(legacyFindings, findings...)
	}
	if len(legacyFindings) > 0 {
		return nil, legacyOrderLayoutError{findings: legacyFindings}
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
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("reading order root %s: %w", dir, err)
	}

	selected := make(map[string]string)
	var names []string
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		fileName := entry.Name()
		name, ok := TrimFlatOrderFilename(fileName)
		if !ok {
			continue
		}
		if _, exists := selected[name]; !exists {
			names = append(names, name)
			selected[name] = fileName
			continue
		}
		if fileName == name+CanonicalFlatOrderSuffix {
			selected[name] = fileName
		}
	}

	for _, name := range names {
		fileName := selected[name]
		source := filepath.Join(dir, fileName)
		data, err := fs.ReadFile(source)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return fmt.Errorf("reading order %s: %w", source, err)
		}
		if err := add(name, source, data); err != nil {
			return err
		}
	}
	return nil
}

type legacyOrderLayoutFinding struct {
	source string
	hint   string
}

type legacyOrderLayoutError struct {
	findings []legacyOrderLayoutFinding
}

func (e legacyOrderLayoutError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "unsupported PackV1 order paths (%d); migrate all legacy order directories to flat orders/<name>.toml files before loading this city. This cutover applies to all pack schemas.", len(e.findings))
	for _, finding := range e.findings {
		fmt.Fprintf(&b, "\n- unsupported PackV1 order path %s; %s", finding.source, finding.hint)
	}
	return b.String()
}

func findLegacySubdirectoryOrders(fs fsys.FS, dir, hintFmt string) ([]legacyOrderLayoutFinding, error) {
	entries, err := fs.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("reading order root %s: %w", dir, err)
	}
	var findings []legacyOrderLayoutFinding
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		name := entry.Name()
		source := filepath.Join(dir, name, orderFileName)
		if _, err := fs.ReadFile(source); err == nil {
			findings = append(findings, legacyOrderLayoutFinding{
				source: source,
				hint:   fmt.Sprintf(hintFmt, name),
			})
		} else if !errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("reading legacy order path %s: %w", source, err)
		}
	}
	return findings, nil
}

func legacyOrdersDir(formulaLayer string) string {
	if formulaLayer == "" {
		return ""
	}
	return filepath.Join(formulaLayer, orderDir)
}
