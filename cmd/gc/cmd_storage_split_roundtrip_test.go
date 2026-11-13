package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

func readFileBytes(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// TestGuidedSplitAndCityRestoreRoundTrip is S-3 end to end: a single-store
// city is split with the one guided command, which names both ways back;
// the city restore of the split's own pinned snapshot puts the city back
// byte for byte (work store, rig store, city.toml) and moves the binding and
// the split's journal aside; and the same command splits the restored city
// again.
func TestGuidedSplitAndCityRestoreRoundTrip(t *testing.T) {
	c := newGuidedSplitCity(t)
	workFile := filepath.Join(c.request.CityPath, ".gc", "beads.json")
	rigFile := filepath.Join(c.rigPath, ".gc", "beads.json")
	tomlBefore, workBefore, rigBefore := c.readTOML(), readFileBytes(t, workFile), readFileBytes(t, rigFile)

	code, stdout, stderr := c.split(splitOptions{})
	if code != 0 {
		t.Fatalf("the split = %d:\n%s\n%s", code, stdout, stderr)
	}
	j := c.journal()
	for _, want := range []string{
		"the ready set is the same before and after the split",
		storageReverseInstruction(reverseToWork, nil),
		snapshotCityRestoreInstruction(j.Snapshot, j.SnapshotTo),
		"GC_REAPER_STALE_ISSUE_BURST_MAX",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("the split's report does not say %q:\n%s", want, stdout)
		}
	}
	if !strings.Contains(c.readTOML(), "[storage") || bytes.Equal(readFileBytes(t, workFile), workBefore) {
		t.Fatal("the split did not split the city, so this proves nothing")
	}

	split := c.reload()
	var rout, rerr bytes.Buffer
	if code := doStorageSnapshotRestoreCity(context.Background(), split, j.SnapshotTo, j.Snapshot, "gc storage snapshot restore", &rout, &rerr); code != 0 {
		t.Fatalf("the city restore = %d:\n%s\n%s", code, rout.String(), rerr.String())
	}
	if got := c.readTOML(); got != tomlBefore {
		t.Errorf("city.toml after the restore:\n%s\nwant:\n%s", got, tomlBefore)
	}
	if !bytes.Equal(readFileBytes(t, workFile), workBefore) {
		t.Error("the restored work store differs from the one before the split")
	}
	if !bytes.Equal(readFileBytes(t, rigFile), rigBefore) {
		t.Error("the restored rig store differs from the one before the split")
	}
	for _, gone := range []string{j.Root, storageSplitJournalPath(c.request.CityPath), minGCVersionPath(c.request.CityPath)} {
		if _, err := os.Stat(gone); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s survived the city restore (%v)", gone, err)
		}
	}

	if code, stdout, stderr := c.split(splitOptions{}); code != 0 {
		t.Fatalf("the split of the restored city = %d:\n%s\n%s", code, stdout, stderr)
	}
}

// TestGuidedSplitRepairsAMalformedLiteral: a work row's "discovered-from:<id>"
// blocks literal, which no split store resolves, is settled inside the split:
// <id> moved to the binding, so the literal is removed and the typed edge
// recorded on the row, and the row is ready in the split city. Every live
// split ran that repair by hand afterwards (gc doctor --fix).
func TestGuidedSplitRepairsAMalformedLiteral(t *testing.T) {
	c := newGuidedSplitCity(t)
	work := c.work()
	session := mustCreateInfraBead(t, work, beads.Bead{Title: "the session the bug was found in", Type: "session", Labels: []string{"gc:session"}})
	bug := mustCreateInfraBead(t, work, beads.Bead{Title: "a bug", Type: "bug"})
	if err := work.DepAdd(bug.ID, "discovered-from:"+session.ID, "blocks"); err != nil {
		t.Fatal(err)
	}
	code, stdout, stderr := c.split(splitOptions{})
	if code != 0 {
		t.Fatalf("the split = %d:\n%s\n%s", code, stdout, stderr)
	}

	got, err := c.work().Get(bug.ID)
	if err != nil {
		t.Fatal(err)
	}
	deps, err := c.work().DepList(bug.ID, "down")
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range deps {
		if strings.Contains(d.DependsOnID, ":") {
			t.Errorf("the literal survived the split: %+v", d)
		}
	}
	if !strings.Contains(got.Metadata[beadmeta.MigratedCrossEdgesMetadataKey], `"discovered-from"`) || !strings.Contains(got.Metadata[beadmeta.MigratedCrossEdgesMetadataKey], session.ID) {
		t.Errorf("%s does not record the typed edge onto %s: %q", bug.ID, session.ID, got.Metadata[beadmeta.MigratedCrossEdgesMetadataKey])
	}
}

// TestGuidedSplitRefusesAReadinessChange: a bead ready before the split and
// not after it, for no reason the split records, fails the split before it
// is recorded complete, naming the bead; the city is split, and the closing
// report's rollbacks stay open.
func TestGuidedSplitRefusesAReadinessChange(t *testing.T) {
	c := newGuidedSplitCity(t)
	reads := 0
	prev := splitReadyRead
	splitReadyRead = func(cityPath string, cfg *config.City) (splitReadySnapshot, error) {
		reads++
		got, err := prev(cityPath, cfg)
		if reads == 2 {
			got.IDs = slices.DeleteFunc(slices.Clone(got.IDs), func(string) bool { return true })
		}
		return got, err
	}
	t.Cleanup(func() { splitReadyRead = prev })
	code, stdout, stderr := c.split(splitOptions{})
	if code == 0 || !strings.Contains(stderr, "changed which beads are ready") || !strings.Contains(stderr, "ready before the split, not after") {
		t.Fatalf("the split = %d:\n%s\n%s", code, stdout, stderr)
	}
	if j := c.journal(); j.Phase == splitPhaseComplete || j.ReadyBefore == nil || len(j.ReadyBefore.IDs) == 0 {
		t.Fatalf("journal phase %s, ready before %+v", j.Phase, j.ReadyBefore)
	}
}

// TestSplitReadinessDifference pins what the comparison explains away: a
// row waiting on an external reference, a gate; anything else, either way,
// is a difference.
func TestSplitReadinessDifference(t *testing.T) {
	before := splitReadySnapshot{IDs: []string{"a", "b", "ext"}, Explained: []string{"ext"}}
	after := splitReadySnapshot{IDs: []string{"a", "c", "gate"}, Explained: []string{"gate"}}
	got := splitReadinessDifference(before, after)
	want := []string{"b (ready before the split, not after)", "c (ready after the split, not before)"}
	if !slices.Equal(got, want) {
		t.Fatalf("difference = %v, want %v", got, want)
	}
	if got := splitReadinessDifference(before, before); len(got) != 0 {
		t.Fatalf("a set compared with itself differs: %v", got)
	}
}
