package packman

import (
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"
)

// writeCacheClone creates a fake clone dir with a single file and the given mtime.
func writeCacheClone(t *testing.T, cacheRoot, name string, mtime time.Time) string {
	t.Helper()
	dir := filepath.Join(cacheRoot, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("MkdirAll(%q): %v", dir, err)
	}
	if err := os.WriteFile(filepath.Join(dir, "pack.toml"), []byte("[pack]\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := os.Chtimes(dir, mtime, mtime); err != nil {
		t.Fatalf("Chtimes: %v", err)
	}
	return dir
}

func names(entries []PruneEntry) []string {
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.Name)
	}
	sort.Strings(out)
	return out
}

func TestClassifyPruneEntries(t *testing.T) {
	now := time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC)
	old := now.Add(-30 * 24 * time.Hour)
	recent := now.Add(-1 * time.Hour)

	tests := []struct {
		name       string
		clones     map[string]time.Time // dir name -> mtime
		referenced map[string]bool
		keepDays   int
		wantKeep   []string
		wantPrune  []string
	}{
		{
			name:       "referenced kept even when old",
			clones:     map[string]time.Time{"ref": old, "orphan": old},
			referenced: map[string]bool{"ref": true},
			keepDays:   7,
			wantKeep:   []string{"ref"},
			wantPrune:  []string{"orphan"},
		},
		{
			name:       "recent unreferenced protected by keep-days",
			clones:     map[string]time.Time{"fresh": recent, "stale": old},
			referenced: map[string]bool{},
			keepDays:   7,
			wantKeep:   []string{"fresh"},
			wantPrune:  []string{"stale"},
		},
		{
			name:       "keep-days zero disables age guard",
			clones:     map[string]time.Time{"fresh": recent, "stale": old},
			referenced: map[string]bool{},
			keepDays:   0,
			wantKeep:   nil,
			wantPrune:  []string{"fresh", "stale"},
		},
		{
			name:       "all referenced",
			clones:     map[string]time.Time{"a": old, "b": recent},
			referenced: map[string]bool{"a": true, "b": true},
			keepDays:   7,
			wantKeep:   []string{"a", "b"},
			wantPrune:  nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cacheRoot := t.TempDir()
			for name, mtime := range tt.clones {
				writeCacheClone(t, cacheRoot, name, mtime)
			}
			keep, prune, err := classifyPruneEntries(cacheRoot, tt.referenced, tt.keepDays, now)
			if err != nil {
				t.Fatalf("classifyPruneEntries: %v", err)
			}
			if got := names(keep); !equalStrings(got, tt.wantKeep) {
				t.Errorf("kept = %v, want %v", got, tt.wantKeep)
			}
			if got := names(prune); !equalStrings(got, tt.wantPrune) {
				t.Errorf("pruned = %v, want %v", got, tt.wantPrune)
			}
		})
	}
}

func TestClassifyPruneEntriesMissingRoot(t *testing.T) {
	keep, prune, err := classifyPruneEntries(filepath.Join(t.TempDir(), "absent"), nil, 7, time.Now())
	if err != nil {
		t.Fatalf("classifyPruneEntries on missing root: %v", err)
	}
	if len(keep) != 0 || len(prune) != 0 {
		t.Fatalf("missing root should yield no entries, got keep=%d prune=%d", len(keep), len(prune))
	}
}

func TestPruneDryRunDeletesNothing(t *testing.T) {
	cacheRoot := t.TempDir()
	old := time.Now().Add(-30 * 24 * time.Hour)
	orphan := writeCacheClone(t, cacheRoot, "orphan", old)
	ref := writeCacheClone(t, cacheRoot, "ref", old)

	result, err := Prune(cacheRoot, map[string]bool{"ref": true}, 7, false)
	if err != nil {
		t.Fatalf("Prune dry-run: %v", err)
	}
	if result.Applied {
		t.Fatal("dry-run reported Applied=true")
	}
	if got := names(result.Pruned); !equalStrings(got, []string{"orphan"}) {
		t.Fatalf("pruned candidates = %v, want [orphan]", got)
	}
	if result.FreedBytes <= 0 {
		t.Fatalf("FreedBytes = %d, want > 0", result.FreedBytes)
	}
	// Nothing actually removed.
	for _, dir := range []string{orphan, ref} {
		if _, err := os.Stat(dir); err != nil {
			t.Fatalf("dry-run removed %q: %v", dir, err)
		}
	}
}

func TestPruneApplyRemovesOnlyUnreferencedAndOld(t *testing.T) {
	cacheRoot := t.TempDir()
	now := time.Now()
	old := now.Add(-30 * 24 * time.Hour)
	recent := now.Add(-1 * time.Hour)

	orphanOld := writeCacheClone(t, cacheRoot, "orphan-old", old)
	orphanFresh := writeCacheClone(t, cacheRoot, "orphan-fresh", recent)
	referenced := writeCacheClone(t, cacheRoot, "ref", old)

	result, err := Prune(cacheRoot, map[string]bool{"ref": true}, 7, true)
	if err != nil {
		t.Fatalf("Prune apply: %v", err)
	}
	if !result.Applied {
		t.Fatal("apply reported Applied=false")
	}
	if got := names(result.Pruned); !equalStrings(got, []string{"orphan-old"}) {
		t.Fatalf("pruned = %v, want [orphan-old]", got)
	}
	if _, err := os.Stat(orphanOld); !os.IsNotExist(err) {
		t.Fatalf("orphan-old should be deleted, stat err = %v", err)
	}
	for _, dir := range []string{orphanFresh, referenced} {
		if _, err := os.Stat(dir); err != nil {
			t.Fatalf("apply removed protected dir %q: %v", dir, err)
		}
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
