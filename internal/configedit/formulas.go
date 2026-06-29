package configedit

import (
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/gastownhall/gascity/internal/citylayout"
	"github.com/gastownhall/gascity/internal/fsys"
)

// formulaNameRE constrains a city-local formula name to a flat, traversal-safe
// slug so it maps to exactly <formulas-dir>/<name>.toml — no separators, dots,
// or leading/trailing punctuation that could escape the formulas directory.
var formulaNameRE = regexp.MustCompile(`^[a-zA-Z0-9](?:[a-zA-Z0-9_-]{0,62}[a-zA-Z0-9])?$`)

// ValidFormulaName reports whether name is a legal city-local formula name: a
// flat, traversal-safe slug that maps to exactly <formulas-dir>/<name>.toml. It
// returns an ErrValidation-wrapped error describing the violation, or nil when
// the name is acceptable. The formula write plane shares this with the editor so
// the validate endpoint predicts the same accept/reject decision the upsert and
// delete paths enforce, instead of accepting names that the later PUT rejects.
func ValidFormulaName(name string) error {
	if !formulaNameRE.MatchString(name) {
		return fmt.Errorf("%w: invalid formula name %q", ErrValidation, name)
	}
	return nil
}

// UpsertFormula creates or replaces the city-local formula source at
// <cityRoot>/formulas/<name>.toml. The write is atomic (temp + rename).
// Callers MUST validate the formula content (parse + Validate + name match)
// before calling; the Editor only persists the bytes safely.
func (e *Editor) UpsertFormula(name string, content []byte) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	dir, target, err := e.formulaTarget(name)
	if err != nil {
		return err
	}
	if err := e.rejectSymlinkedPath(dir, target); err != nil {
		return err
	}
	if err := e.fs.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("creating formulas dir: %w", err)
	}
	return fsys.WriteFileAtomic(e.fs, target, content, 0o644)
}

// DeleteFormula removes a city-local formula source. It returns ErrNotFound when
// no such city-local formula exists (pack-supplied formulas are not deletable).
func (e *Editor) DeleteFormula(name string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	dir, target, err := e.formulaTarget(name)
	if err != nil {
		return err
	}
	if err := e.rejectSymlinkedPath(dir, target); err != nil {
		return err
	}
	if _, statErr := e.fs.Stat(target); statErr != nil {
		if errors.Is(statErr, fs.ErrNotExist) {
			return fmt.Errorf("%w: formula %q", ErrNotFound, name)
		}
		return statErr
	}
	return e.fs.Remove(target)
}

// FormulaSource returns the raw TOML of a city-local formula, or ok=false when
// no editable city-local source exists for the name.
func (e *Editor) FormulaSource(name string) ([]byte, bool, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	dir, target, err := e.formulaTarget(name)
	if err != nil {
		return nil, false, err
	}
	if err := e.rejectSymlinkedPath(dir, target); err != nil {
		return nil, false, err
	}
	data, err := e.fs.ReadFile(target)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, false, nil
		}
		return nil, false, err
	}
	return data, true, nil
}

// formulaTarget resolves the city-local formulas dir and the safe per-name path.
// It deliberately pins the dir to the well-known <cityRoot>/formulas and does NOT
// honor a custom [formulas].dir from city.toml: that key is rejected on the
// compose path (validateCityAuthoringSurface), so trusting it here — reading the
// raw on-disk file, which may have drifted from the validated snapshot — would
// turn every formula op into an arbitrary-path primitive. Both the dir and the
// per-name target are re-checked for containment as defense in depth.
func (e *Editor) formulaTarget(name string) (dir, target string, err error) {
	if err := ValidFormulaName(name); err != nil {
		return "", "", err
	}
	cityRoot := filepath.Dir(e.tomlPath)
	dir = citylayout.ResolveFormulasDir(cityRoot, "")
	if !withinDir(cityRoot, dir) {
		return "", "", fmt.Errorf("%w: formulas dir escapes the city tree", ErrValidation)
	}
	target = filepath.Join(dir, name+".toml")
	if !withinDir(dir, target) {
		return "", "", fmt.Errorf("%w: formula name %q escapes the formulas dir", ErrValidation, name)
	}
	return dir, target, nil
}

// rejectSymlinkedPath fails if the formulas dir or the target file is a symlink,
// so a planted link can't redirect a write/read/delete out of the city tree.
func (e *Editor) rejectSymlinkedPath(dir, target string) error {
	if fi, err := e.fs.Lstat(dir); err == nil && fi.Mode()&fs.ModeSymlink != 0 {
		return fmt.Errorf("%w: formulas dir is a symlink", ErrValidation)
	}
	if fi, err := e.fs.Lstat(target); err == nil && fi.Mode()&fs.ModeSymlink != 0 {
		return fmt.Errorf("%w: formula target is a symlink", ErrValidation)
	}
	return nil
}

// withinDir reports whether child is dir itself or a path nested under it.
func withinDir(dir, child string) bool {
	rel, err := filepath.Rel(dir, child)
	if err != nil || rel == ".." || filepath.IsAbs(rel) ||
		strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return false
	}
	return true
}
