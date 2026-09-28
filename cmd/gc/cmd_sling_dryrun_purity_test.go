package main

import (
	"bytes"
	"io/fs"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

// These tests guard the gc-r9fx dry-run purity contract: `gc sling --dry-run`
// must be observationally pure — no directories, bead metadata, convoy,
// formula, or routing mutation — on both the single-bead path and the
// container/epic error path.

// snapshotDirTree returns the sorted set of directories under root.
func snapshotDirTree(t *testing.T, root string) []string {
	t.Helper()
	var dirs []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			rel, relErr := filepath.Rel(root, path)
			if relErr != nil {
				return relErr
			}
			dirs = append(dirs, rel)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking %q: %v", root, err)
	}
	sort.Strings(dirs)
	return dirs
}

// listAllBeads returns every bead in the store, keyed by ID.
func listAllBeads(t *testing.T, store beads.Store) map[string]beads.Bead {
	t.Helper()
	list, err := store.List(beads.ListQuery{AllowScan: true})
	if err != nil {
		t.Fatalf("store.List: %v", err)
	}
	out := make(map[string]beads.Bead, len(list))
	for _, b := range list {
		out[b.ID] = b
	}
	return out
}

func assertSlingDryRunPurity(t *testing.T, cityDir string, dirsBefore []string, beadsBefore map[string]beads.Bead, store beads.Store) {
	t.Helper()
	if dirsAfter := snapshotDirTree(t, cityDir); !reflect.DeepEqual(dirsBefore, dirsAfter) {
		t.Errorf("dry-run mutated the directory tree:\nbefore: %v\nafter:  %v", dirsBefore, dirsAfter)
	}
	beadsAfter := listAllBeads(t, store)
	if len(beadsAfter) != len(beadsBefore) {
		t.Errorf("dry-run changed bead count: before %d, after %d", len(beadsBefore), len(beadsAfter))
	}
	for id, before := range beadsBefore {
		after, ok := beadsAfter[id]
		if !ok {
			t.Errorf("dry-run deleted bead %s", id)
			continue
		}
		if !reflect.DeepEqual(before, after) {
			t.Errorf("dry-run mutated bead %s:\nbefore: %+v\nafter:  %+v", id, before, after)
		}
	}
}

func TestCmdSlingDryRunSingleBeadIsObservationallyPure(t *testing.T) {
	cityDir := setupCmdSlingBeadExistsFixture(t)
	rigStore, err := openStoreAtForCity(filepath.Join(cityDir, "frontend"), cityDir)
	if err != nil {
		t.Fatalf("openStoreAtForCity: %v", err)
	}
	// The file store assigns IDs itself (an explicit ID on Create is ignored),
	// so sling the ID the store actually returned.
	seeded, err := rigStore.Create(beads.Bead{Type: "task", Status: "open", Title: "pure work"})
	if err != nil {
		t.Fatalf("Create(task): %v", err)
	}

	dirsBefore := snapshotDirTree(t, cityDir)
	beadsBefore := listAllBeads(t, rigStore)

	var stdout, stderr bytes.Buffer
	code := cmdSling(
		[]string{"frontend/worker", seeded.ID},
		false, false, false,
		"", nil, "",
		true, false, false, "",
		true, false, true,
		"", "",
		&stdout, &stderr,
	)
	if code != 0 {
		t.Fatalf("dry-run exit = %d, want 0; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "No side effects executed (--dry-run).") {
		t.Errorf("stdout %q missing dry-run footer", stdout.String())
	}
	assertSlingDryRunPurity(t, cityDir, dirsBefore, beadsBefore, rigStore)
}

func TestCmdSlingDryRunEpicErrorPathIsObservationallyPure(t *testing.T) {
	// The gc-r9fx incident: slinging an epic with --dry-run failed with
	// "is an epic" but still provisioned workspace state before the error.
	// The error must stay, and the failure path must mutate nothing.
	cityDir := setupCmdSlingBeadExistsFixture(t)
	rigStore, err := openStoreAtForCity(filepath.Join(cityDir, "frontend"), cityDir)
	if err != nil {
		t.Fatalf("openStoreAtForCity: %v", err)
	}
	// The file store assigns IDs itself (an explicit ID on Create is ignored),
	// so sling the ID the store actually returned.
	epic, err := rigStore.Create(beads.Bead{Type: "epic", Status: "open", Title: "big epic"})
	if err != nil {
		t.Fatalf("Create(epic): %v", err)
	}
	if _, err := rigStore.Create(beads.Bead{Type: "task", Status: "open", Title: "epic child"}); err != nil {
		t.Fatalf("Create(task child): %v", err)
	}

	dirsBefore := snapshotDirTree(t, cityDir)
	beadsBefore := listAllBeads(t, rigStore)

	var stdout, stderr bytes.Buffer
	code := cmdSling(
		[]string{"frontend/worker", epic.ID},
		false, false, false,
		"", nil, "",
		true, false, false, "",
		true, false, true,
		"", "",
		&stdout, &stderr,
	)
	if code == 0 {
		t.Fatalf("dry-run sling of an epic returned 0, want nonzero; stdout=%s", stdout.String())
	}
	if !strings.Contains(stderr.String(), "is an epic") {
		t.Errorf("stderr %q missing epic rejection", stderr.String())
	}
	assertSlingDryRunPurity(t, cityDir, dirsBefore, beadsBefore, rigStore)
}
