package orders

import (
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"

	"github.com/gastownhall/gascity/internal/fsys"
)

// discoverRoot discovers orders for one logical root. Wave 2 requires the
// canonical flat orders/<name>.toml layout and hard-errors when older PackV1
// order paths are still present.
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
	if err := rejectLegacySubdirectoryOrders(fs, root.Dir, "rename to orders/%s.toml"); err != nil {
		return nil, err
	}

	legacyDir := legacyOrdersDir(root.FormulaLayer)
	if legacyDir != "" && filepath.Clean(legacyDir) != filepath.Clean(root.Dir) {
		if err := rejectLegacySubdirectoryOrders(fs, legacyDir, "move to orders/%s.toml"); err != nil {
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
	msg := fmt.Sprintf(format, args...)
	if opts.DeprecatedPathWarningDedup != nil && !opts.VerboseDeprecatedPathWarnings && !opts.DeprecatedPathWarningDedup.First(msg) {
		return
	}
	if opts.DeprecatedPathWarningWriter != nil {
		fmt.Fprintln(opts.DeprecatedPathWarningWriter, msg) //nolint:errcheck // best-effort warning emission
		return
	}
	log.Print(msg)
}

func discoverFlatFiles(fs fsys.FS, dir string, found map[string]Order, add func(name, source string, data []byte) error, opts ScanOptions) error {
	entries, err := fs.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("reading order root %s: %w", dir, err)
	}
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
		source := filepath.Join(dir, fileName)
		if legacy {
			return fmt.Errorf("unsupported PackV1 order path %s; rename to orders/%s.toml", source, name)
		}
		if _, exists := found[name]; exists {
			continue
		}
		data, err := fs.ReadFile(source)
		if err != nil {
			if !errors.Is(err, os.ErrNotExist) {
				warnUnreadablePath(opts, "warning: unreadable order path %s: %v", source, err)
			}
			continue
		}
		if err := add(name, source, data); err != nil {
			return err
		}
	}
	return nil
}

func rejectLegacySubdirectoryOrders(fs fsys.FS, dir, hintFmt string) error {
	entries, err := fs.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("reading order root %s: %w", dir, err)
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		name := entry.Name()
		source := filepath.Join(dir, name, orderFileName)
		if _, err := fs.ReadFile(source); err == nil {
			return fmt.Errorf("unsupported PackV1 order path %s; %s", source, fmt.Sprintf(hintFmt, name))
		} else if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("reading legacy order path %s: %w", source, err)
		}
	}
	return nil
}

func warnUnreadablePath(_ ScanOptions, format string, args ...any) {
	log.Printf(format, args...)
}

func legacyOrdersDir(formulaLayer string) string {
	if formulaLayer == "" {
		return ""
	}
	return filepath.Join(formulaLayer, orderDir)
}
