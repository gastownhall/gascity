package main

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/gastownhall/gascity/internal/citylayout"
)

// MaterializeSystemFormulas writes embedded system formula and order files
// to the city's formulas/ and orders/ directories respectively. Files are
// always overwritten to stay in sync with the gc binary version. Returns
// the formulas directory path, or "" if there are no embedded system files.
// Does not remove stale files because these directories are shared with
// user-created content. Idempotent: safe to call on every gc start.
func MaterializeSystemFormulas(embedded fs.FS, subdir, cityPath string) (string, error) {
	files := collectFormulaFiles(embedded, subdir)
	if len(files) == 0 {
		return "", nil
	}

	formulasDir := filepath.Join(cityPath, citylayout.FormulasRoot)
	ordersDir := filepath.Join(cityPath, citylayout.OrdersRoot)

	for _, relPath := range files {
		data, err := fs.ReadFile(embedded, filepath.Join(subdir, relPath))
		if err != nil {
			return "", fmt.Errorf("reading embedded %s: %w", relPath, err)
		}

		// Route orders/ to the city orders/ root, formulas to formulas/.
		var dst string
		if isOrderFile(relPath) {
			// relPath is "orders/<name>.order.toml"; strip the leading "orders/"
			// since ordersDir already points to cityPath/orders/.
			dst = filepath.Join(ordersDir, strings.TrimPrefix(relPath, "orders/"))
		} else {
			dst = filepath.Join(formulasDir, relPath)
		}

		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return "", fmt.Errorf("creating dir for %s: %w", relPath, err)
		}
		if err := os.WriteFile(dst, data, 0o644); err != nil {
			return "", fmt.Errorf("writing %s: %w", relPath, err)
		}
	}

	return formulasDir, nil
}

// ListEmbeddedSystemFormulas returns the relative paths of all formula
// and order files in the embedded FS. Used by doctor check for presence detection.
func ListEmbeddedSystemFormulas(embedded fs.FS, subdir string) []string {
	return collectFormulaFiles(embedded, subdir)
}

// collectFormulaFiles walks the embedded FS under subdir and returns
// relative paths of *.formula.toml files and orders/*.order.toml files.
func collectFormulaFiles(embedded fs.FS, subdir string) []string {
	var files []string
	_ = fs.WalkDir(embedded, subdir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		rel := strings.TrimPrefix(path, subdir+"/")
		if rel == path {
			// path == subdir (root entry, not a file under subdir)
			return nil
		}
		if isFormulaFile(rel) {
			files = append(files, rel)
		}
		return nil
	})
	return files
}

// isFormulaFile returns true if the relative path is a formula or order file.
func isFormulaFile(rel string) bool {
	return strings.HasSuffix(rel, ".formula.toml") || isOrderFile(rel)
}

// isOrderFile returns true if the relative path is an order file (orders/<name>.order.toml).
func isOrderFile(rel string) bool {
	return strings.HasPrefix(rel, "orders/") && strings.HasSuffix(rel, ".order.toml")
}
