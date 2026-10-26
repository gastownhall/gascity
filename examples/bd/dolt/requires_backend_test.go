package dolt_test

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/orders"
)

// Every order the dolt pack ships manages a Dolt server (health, sync,
// compaction, backup, doctor, phantom and stale databases), so every one must
// declare requires_backend = ["dolt"]: gc then never dispatches it for a scope
// on another backend and reports it there as inapplicable rather than failed.
// A new order, or a dropped declaration, fails here.
func TestDoltPackOrdersDeclareRequiresBackend(t *testing.T) {
	dir := filepath.Join(repoRoot(t), "orders")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read orders dir: %v", err)
	}
	want := []string{
		"dolt-health", "dolt-remotes-patrol", "mol-dog-backup", "mol-dog-compactor",
		"mol-dog-doctor", "mol-dog-phantom-db", "mol-dog-stale-db",
	}
	var seen []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".toml") {
			continue
		}
		name := strings.TrimSuffix(e.Name(), ".toml")
		seen = append(seen, name)
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		o, err := orders.Parse(data)
		if err != nil {
			t.Fatalf("parse %s: %v", e.Name(), err)
		}
		o.Name = name
		if err := orders.Validate(o); err != nil {
			t.Errorf("%s: %v", e.Name(), err)
		}
		if !slices.Equal(o.RequiresBackend, []string{"dolt"}) {
			t.Errorf("%s requires_backend = %q, want [dolt]", e.Name(), o.RequiresBackend)
		}
	}
	slices.Sort(seen)
	if !slices.Equal(seen, want) {
		t.Fatalf("dolt pack orders = %q, want %q", seen, want)
	}
}
