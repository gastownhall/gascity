package main

import (
	"os"
	"path/filepath"
	"sort"
	"testing"
	"testing/fstest"
)

// --- MaterializeSystemFormulas ---

func TestMaterializeEmpty(t *testing.T) {
	cityPath := t.TempDir()
	fs := fstest.MapFS{}

	dir, err := MaterializeSystemFormulas(fs, ".", cityPath)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if dir != "" {
		t.Errorf("expected empty dir, got %q", dir)
	}
	// formulas/ should not exist.
	formulasDir := filepath.Join(cityPath, "formulas")
	if _, err := os.Stat(formulasDir); !os.IsNotExist(err) {
		t.Errorf("formulas dir should not exist for empty FS")
	}
}

func TestMaterializeWritesFiles(t *testing.T) {
	cityPath := t.TempDir()

	fs := fstest.MapFS{
		"sysformulas/hello.formula.toml": &fstest.MapFile{Data: []byte("[formula]\nname = \"hello\"\n")},
	}

	dir, err := MaterializeSystemFormulas(fs, "sysformulas", cityPath)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	expected := filepath.Join(cityPath, "formulas")
	if dir != expected {
		t.Errorf("dir = %q, want %q", dir, expected)
	}

	data, err := os.ReadFile(filepath.Join(dir, "hello.formula.toml"))
	if err != nil {
		t.Fatalf("reading materialized file: %v", err)
	}
	if string(data) != "[formula]\nname = \"hello\"\n" {
		t.Errorf("content = %q", string(data))
	}
}

func TestMaterializeOverwrites(t *testing.T) {
	cityPath := t.TempDir()
	formulasDir := filepath.Join(cityPath, "formulas")
	if err := os.MkdirAll(formulasDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(formulasDir, "hello.formula.toml"), []byte("old content"), 0o644); err != nil {
		t.Fatal(err)
	}

	fs := fstest.MapFS{
		"sf/hello.formula.toml": &fstest.MapFile{Data: []byte("new content")},
	}

	dir, err := MaterializeSystemFormulas(fs, "sf", cityPath)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(dir, "hello.formula.toml"))
	if err != nil {
		t.Fatalf("reading file: %v", err)
	}
	if string(data) != "new content" {
		t.Errorf("content = %q, want %q", string(data), "new content")
	}
}

func TestMaterializeDoesNotRemoveUserFiles(t *testing.T) {
	cityPath := t.TempDir()
	formulasDir := filepath.Join(cityPath, "formulas")
	if err := os.MkdirAll(formulasDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Pre-existing user formula not in the embedded FS — should be left alone.
	if err := os.WriteFile(filepath.Join(formulasDir, "user.formula.toml"), []byte("user"), 0o644); err != nil {
		t.Fatal(err)
	}

	fs := fstest.MapFS{
		"sf/fresh.formula.toml": &fstest.MapFile{Data: []byte("fresh")},
	}

	_, err := MaterializeSystemFormulas(fs, "sf", cityPath)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// user.formula.toml should still exist (not removed).
	if _, err := os.Stat(filepath.Join(formulasDir, "user.formula.toml")); err != nil {
		t.Error("user formula file was removed")
	}
	// fresh.formula.toml should exist.
	if _, err := os.Stat(filepath.Join(formulasDir, "fresh.formula.toml")); err != nil {
		t.Error("fresh formula file missing")
	}
}

func TestMaterializeIdempotent(t *testing.T) {
	cityPath := t.TempDir()

	fs := fstest.MapFS{
		"sf/a.formula.toml": &fstest.MapFile{Data: []byte("aaa")},
	}

	dir1, err := MaterializeSystemFormulas(fs, "sf", cityPath)
	if err != nil {
		t.Fatalf("first call: %v", err)
	}
	dir2, err := MaterializeSystemFormulas(fs, "sf", cityPath)
	if err != nil {
		t.Fatalf("second call: %v", err)
	}
	if dir1 != dir2 {
		t.Errorf("dir changed: %q vs %q", dir1, dir2)
	}
	data, err := os.ReadFile(filepath.Join(dir2, "a.formula.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "aaa" {
		t.Errorf("content after second call = %q", string(data))
	}
}

func TestMaterializeWithOrders(t *testing.T) {
	cityPath := t.TempDir()

	fs := fstest.MapFS{
		"sf/basic.formula.toml":    &fstest.MapFile{Data: []byte("basic")},
		"sf/orders/foo/order.toml": &fstest.MapFile{Data: []byte("foo order")},
		"sf/orders/bar/order.toml": &fstest.MapFile{Data: []byte("bar order")},
	}

	dir, err := MaterializeSystemFormulas(fs, "sf", cityPath)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Check basic formula goes to formulas/.
	data, err := os.ReadFile(filepath.Join(dir, "basic.formula.toml"))
	if err != nil {
		t.Fatalf("reading basic: %v", err)
	}
	if string(data) != "basic" {
		t.Errorf("basic content = %q", string(data))
	}

	// Check order files go to orders/ (peer of formulas/).
	ordersDir := filepath.Join(cityPath, "orders")
	data, err = os.ReadFile(filepath.Join(ordersDir, "foo", "order.toml"))
	if err != nil {
		t.Fatalf("reading foo order: %v", err)
	}
	if string(data) != "foo order" {
		t.Errorf("foo order content = %q", string(data))
	}

	data, err = os.ReadFile(filepath.Join(ordersDir, "bar", "order.toml"))
	if err != nil {
		t.Fatalf("reading bar order: %v", err)
	}
	if string(data) != "bar order" {
		t.Errorf("bar order content = %q", string(data))
	}
}

// --- ListEmbeddedSystemFormulas ---

func TestListEmbeddedEmpty(t *testing.T) {
	fs := fstest.MapFS{}
	got := ListEmbeddedSystemFormulas(fs, ".")
	if len(got) != 0 {
		t.Errorf("expected empty slice, got %v", got)
	}
}

func TestListEmbeddedWithFiles(t *testing.T) {
	fs := fstest.MapFS{
		"sf/a.formula.toml":      &fstest.MapFile{Data: []byte("a")},
		"sf/b.formula.toml":      &fstest.MapFile{Data: []byte("b")},
		"sf/orders/p/order.toml": &fstest.MapFile{Data: []byte("p")},
		"sf/readme.txt":          &fstest.MapFile{Data: []byte("skip")},
	}

	got := ListEmbeddedSystemFormulas(fs, "sf")
	sort.Strings(got)
	want := []string{"a.formula.toml", "b.formula.toml", "orders/p/order.toml"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("got[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}
