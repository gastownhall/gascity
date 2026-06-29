package configedit

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/fsys"
)

func newFormulaEditor(t *testing.T) (*Editor, string) {
	t.Helper()
	dir := t.TempDir()
	tomlPath := filepath.Join(dir, "city.toml")
	if err := os.WriteFile(tomlPath, []byte("[workspace]\nname = \"t\"\n"), 0o644); err != nil {
		t.Fatalf("seed city.toml: %v", err)
	}
	return NewEditor(fsys.OSFS{}, tomlPath), dir
}

func TestEditor_FormulaUpsertSourceDelete(t *testing.T) {
	e, dir := newFormulaEditor(t)
	content := []byte("formula = \"hello\"\n")

	if err := e.UpsertFormula("hello", content); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	onDisk := filepath.Join(dir, "formulas", "hello.toml")
	got, err := os.ReadFile(onDisk)
	if err != nil {
		t.Fatalf("read written file: %v", err)
	}
	if string(got) != string(content) {
		t.Fatalf("written content=%q want %q", got, content)
	}

	src, ok, err := e.FormulaSource("hello")
	if err != nil || !ok || string(src) != string(content) {
		t.Fatalf("FormulaSource = (%q, %v, %v)", src, ok, err)
	}

	// Upsert replaces.
	repl := []byte("formula = \"hello\"\ndescription = \"v2\"\n")
	if err := e.UpsertFormula("hello", repl); err != nil {
		t.Fatalf("re-upsert: %v", err)
	}
	if src, _, _ := e.FormulaSource("hello"); string(src) != string(repl) {
		t.Fatalf("replace failed: %q", src)
	}

	if err := e.DeleteFormula("hello"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, ok, _ := e.FormulaSource("hello"); ok {
		t.Fatal("formula still present after delete")
	}
	if err := e.DeleteFormula("hello"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("delete missing: %v, want ErrNotFound", err)
	}
}

func TestEditor_FormulaIgnoresPoisonedFormulasDir(t *testing.T) {
	dir := t.TempDir()
	tomlPath := filepath.Join(dir, "city.toml")
	evil := filepath.Join(t.TempDir(), "evil")
	// A malicious/legacy [formulas].dir pointing outside the city tree must be
	// ignored: writes are pinned to <cityRoot>/formulas.
	body := "[workspace]\nname = \"t\"\n[formulas]\ndir = \"" + evil + "\"\n"
	if err := os.WriteFile(tomlPath, []byte(body), 0o644); err != nil {
		t.Fatalf("seed city.toml: %v", err)
	}
	e := NewEditor(fsys.OSFS{}, tomlPath)

	if err := e.UpsertFormula("x", []byte("formula=\"x\"")); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "formulas", "x.toml")); err != nil {
		t.Fatalf("not written to the pinned formulas dir: %v", err)
	}
	if _, err := os.Stat(filepath.Join(evil, "x.toml")); !os.IsNotExist(err) {
		t.Fatalf("write escaped to the poisoned dir %s (stat err=%v)", evil, err)
	}
}

func TestEditor_FormulaSymlinkedDirRejected(t *testing.T) {
	dir := t.TempDir()
	tomlPath := filepath.Join(dir, "city.toml")
	if err := os.WriteFile(tomlPath, []byte("[workspace]\nname = \"t\"\n"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := os.Symlink(t.TempDir(), filepath.Join(dir, "formulas")); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	e := NewEditor(fsys.OSFS{}, tomlPath)
	if err := e.UpsertFormula("x", []byte("formula=\"x\"")); !errors.Is(err, ErrValidation) {
		t.Fatalf("symlinked formulas dir: %v, want ErrValidation", err)
	}
	if _, _, err := e.FormulaSource("x"); !errors.Is(err, ErrValidation) {
		t.Fatalf("FormulaSource via symlinked dir: %v, want ErrValidation", err)
	}
}

func TestEditor_FormulaSourceMissingIsNotFound(t *testing.T) {
	e, _ := newFormulaEditor(t)
	if _, ok, err := e.FormulaSource("nope"); ok || err != nil {
		t.Fatalf("missing source: ok=%v err=%v, want (false,nil)", ok, err)
	}
}

func TestEditor_FormulaNameTraversalRejected(t *testing.T) {
	e, _ := newFormulaEditor(t)
	for _, bad := range []string{"../evil", "a/b", "..", "", ".hidden", "a.b", "a/../b", "/abs", "-lead", "trail-"} {
		if err := e.UpsertFormula(bad, []byte("formula=\"x\"")); !errors.Is(err, ErrValidation) {
			t.Errorf("UpsertFormula(%q) = %v, want ErrValidation", bad, err)
		}
		if err := e.DeleteFormula(bad); !errors.Is(err, ErrValidation) {
			t.Errorf("DeleteFormula(%q) = %v, want ErrValidation", bad, err)
		}
	}
	for _, ok := range []string{"hello", "pr-review", "a", "Formula_1", "x-y-z"} {
		if err := e.UpsertFormula(ok, []byte("formula=\"x\"")); err != nil {
			t.Errorf("UpsertFormula(%q) = %v, want nil", ok, err)
		}
	}
}
