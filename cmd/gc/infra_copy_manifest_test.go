package main

import (
	"bytes"
	"errors"
	"os"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
)

// The copy manifest keyed by (id, created_at) (prod-evidence item 2).
//
// bd reuses short wisp ids. On maintainer-city mc-wisp-jkl, an auto-handoff mail
// created 2026-08-15, carries an id the 2026-08-10 manifest already lists. Matched
// by id alone it read as "retained / removed since cutover": the containment
// check called a never-copied bead safe, and the clear would have deleted it.

// reuseCarriedID removes the carried row id from the work store, then mints an
// unrelated bead under the same id — what bd does once a short wisp id is free.
func reuseCarriedID(t *testing.T, source beads.Store, id string) beads.Bead {
	t.Helper()
	if err := source.Delete(id); err != nil {
		t.Fatalf("deleting the carried row %s from the work store: %v", id, err)
	}
	reused, err := source.Create(beads.Bead{ID: id, Title: "auto-handoff mail (a later bead, same short id)", Type: "message"})
	if err != nil {
		t.Fatalf("re-minting %s: %v", id, err)
	}
	if reused.ID != id {
		t.Fatalf("the work store minted %s, want the reused id %s", reused.ID, id)
	}
	return reused
}

// carriedMessageID returns the id of the delivered-mail row the cutover carried.
func carriedMessageID(t *testing.T, source beads.Store) string {
	t.Helper()
	rows, err := readInfraSnapshot(source)
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range rows {
		if b.Type == "message" {
			return b.ID
		}
	}
	t.Fatal("the fixture carried no message")
	return ""
}

// backdateManifestEntry rewrites the manifest's created_at for id to an hour
// earlier: the original bead under that id was created long before the reused
// one, as on maintainer-city (08-10 manifest, 08-15 reuse).
func backdateManifestEntry(t *testing.T, target infraBindingTarget, id string) {
	t.Helper()
	manifest, recorded, err := readInfraCopyManifestRecord(target)
	if err != nil || !recorded {
		t.Fatalf("reading the manifest: recorded=%v err=%v", recorded, err)
	}
	at, ok := manifest.Created[id]
	if !ok {
		t.Fatalf("the manifest recorded no created_at for %s; the migration must key every entry by (id, created_at)", id)
	}
	manifest.Created[id] = at.Add(-time.Hour)
	ids := make([]string, 0, len(manifest.IDs))
	for i := range manifest.IDs {
		ids = append(ids, i)
	}
	sort.Strings(ids)
	if err := writeInfraCopyManifestRecord(target, ids, manifest.Created); err != nil {
		t.Fatal(err)
	}
}

func TestMigrationManifestKeysEveryEntryByCreatedAt(t *testing.T) {
	_, _, source, target := convergedRecoveryCity(t)
	manifest, recorded, err := readInfraCopyManifestRecord(target)
	if err != nil || !recorded {
		t.Fatalf("reading the manifest: recorded=%v err=%v", recorded, err)
	}
	rows, err := readInfraSnapshot(source)
	if err != nil {
		t.Fatal(err)
	}
	if manifest.Len() == 0 {
		t.Fatal("the fixture carried nothing")
	}
	for _, b := range rows {
		at, ok := manifest.Created[b.ID]
		if !ok {
			t.Errorf("manifest entry %s has no created_at", b.ID)
			continue
		}
		if !sameCreatedSecond(at, b.CreatedAt) {
			t.Errorf("manifest entry %s created_at = %s, want the row's %s", b.ID, at, b.CreatedAt)
		}
	}
	if manifest.WrittenAt.IsZero() {
		t.Error("the manifest records no write time")
	}
}

func TestContainmentCallsAReusedIDStrandedWhileTheBindingHoldsTheOriginal(t *testing.T) {
	cityPath, _, source, target := convergedRecoveryCity(t)
	id := carriedMessageID(t, source)
	backdateManifestEntry(t, target, id)
	reuseCarriedID(t, source, id)
	if _, err := openMigratedDestination(t, target).Get(id); err != nil {
		t.Fatalf("the fixture's binding lost the original %s: %v", id, err)
	}

	if got := strandedIDs(t, cityPath, target); !slices.Contains(got, id) {
		t.Errorf("stranded = %v; the reused %s was never copied (the binding holds the ORIGINAL bead under that id) and must be stranded, not retained", got, id)
	}
}

func TestContainmentCallsAReusedIDStrandedAfterTheBindingCollectedTheOriginal(t *testing.T) {
	cityPath, _, source, target := convergedRecoveryCity(t)
	id := carriedMessageID(t, source)
	backdateManifestEntry(t, target, id)
	if err := openMigratedDestination(t, target).Delete(id); err != nil {
		t.Fatalf("collecting the original %s from the binding: %v", id, err)
	}
	reuseCarriedID(t, source, id)

	manifest, _, err := readInfraCopyManifestRecord(target)
	if err != nil {
		t.Fatal(err)
	}
	gap, err := classifyInfraContainmentGap(cityPath, target, manifest)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(gap.Stranded, id) {
		t.Errorf("stranded = %v; the reused %s must be stranded, never 'removed since cutover'", gap.Stranded, id)
	}
	if slices.Contains(gap.Retained, id) {
		t.Errorf("the reused %s was classed retained; the clear would delete the only copy", id)
	}
	if gap.RemovedSinceCutover != 0 {
		t.Errorf("removed since cutover = %d, want 0: the bead the binding collected is not the one the work store holds", gap.RemovedSinceCutover)
	}
}

func TestClearRefusesToDeleteARowThatReusesAManifestID(t *testing.T) {
	cityPath, _, source, target := convergedRecoveryCity(t)
	id := carriedMessageID(t, source)
	backdateManifestEntry(t, target, id)
	reused := reuseCarriedID(t, source, id)

	var announce bytes.Buffer
	_, err := clearRetainedInfraCopies(cityPath, target, &announce)
	var unproven *infraUnprovenSourceRows
	if !errors.As(err, &unproven) {
		t.Fatalf("clear = %v, want the unproven-rows refusal naming %s", err, id)
	}
	if !slices.Contains(unproven.IDs, id) {
		t.Errorf("the refusal names %v, want %s", unproven.IDs, id)
	}
	got, err := source.Get(id)
	if err != nil {
		t.Fatalf("the clear deleted the reused %s, the only copy: %v", id, err)
	}
	if got.Title != reused.Title {
		t.Errorf("work store %s title = %q, want the reused bead's %q", id, got.Title, reused.Title)
	}
}

// A manifest written by an older build has no created_at. It keeps reading
// compatibly: an id it lists still matches the row it carried. But a row
// created after the manifest was written is never that row.
func TestLegacyManifestStillMatchesByIDButNotForARowYoungerThanIt(t *testing.T) {
	prevSlack := infraManifestClockSlack
	infraManifestClockSlack = 0
	t.Cleanup(func() { infraManifestClockSlack = prevSlack })

	cityPath, _, source, target := convergedRecoveryCity(t)
	ids := manifestIDs(t, target)
	if err := os.WriteFile(target.ManifestPath(), []byte(strings.Join(ids, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	written := time.Now()
	if err := os.Chtimes(target.ManifestPath(), written, written); err != nil {
		t.Fatal(err)
	}
	legacy, recorded, err := readInfraCopyManifestRecord(target)
	if err != nil || !recorded {
		t.Fatalf("reading the legacy manifest: recorded=%v err=%v", recorded, err)
	}
	if legacy.Len() != len(ids) || len(legacy.Created) != 0 {
		t.Fatalf("legacy read = %d ids / %d created, want %d / 0", legacy.Len(), len(legacy.Created), len(ids))
	}
	if !legacy.WrittenAt.Equal(written) {
		t.Errorf("legacy written time = %s, want the file's modification time %s", legacy.WrittenAt, written)
	}
	if got := strandedIDs(t, cityPath, target); len(got) != 0 {
		t.Fatalf("a legacy manifest strands %v; its carried rows must still match by id", got)
	}

	id := carriedMessageID(t, source)
	reuseCarriedID(t, source, id)
	if got := strandedIDs(t, cityPath, target); !slices.Equal(got, []string{id}) {
		t.Errorf("stranded = %v, want exactly the reused %s (created after the legacy manifest was written)", got, id)
	}
}

func TestRecoverStrandedMovesARowThatReusedACollectedID(t *testing.T) {
	cityPath, cfg, source, target := convergedRecoveryCity(t)
	id := carriedMessageID(t, source)
	backdateManifestEntry(t, target, id)
	if err := openMigratedDestination(t, target).Delete(id); err != nil {
		t.Fatal(err)
	}
	reused := reuseCarriedID(t, source, id)
	if got := strandedIDs(t, cityPath, target); !slices.Contains(got, id) {
		t.Fatalf("precondition: %s not stranded (%v)", id, got)
	}

	var stdout, stderr bytes.Buffer
	if code := runStrandedRecovery(t, cityPath, cfg, &stdout, &stderr); code != 0 {
		t.Fatalf("recovery exited %d; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	got, err := openMigratedDestination(t, target).Get(id)
	if err != nil {
		t.Fatalf("recovery did not move the reused %s: %v", id, err)
	}
	if got.Title != reused.Title {
		t.Errorf("binding %s title = %q, want the reused bead's %q", id, got.Title, reused.Title)
	}
	manifest, _, err := readInfraCopyManifestRecord(target)
	if err != nil {
		t.Fatal(err)
	}
	if !sameCreatedSecond(manifest.Created[id], reused.CreatedAt) {
		t.Errorf("manifest created_at for %s = %s, want the recovered bead's %s", id, manifest.Created[id], reused.CreatedAt)
	}
	if got := strandedIDs(t, cityPath, target); len(got) != 0 {
		t.Errorf("still stranded after recovery: %v", got)
	}
}

// A reused id whose original the binding still holds cannot be carried under
// that id: recovery names it and the city stays refused, never folding the old
// bead in under the new one's identity.
func TestRecoverStrandedNamesAReusedIDTheBindingStillHolds(t *testing.T) {
	cityPath, cfg, source, target := convergedRecoveryCity(t)
	id := carriedMessageID(t, source)
	backdateManifestEntry(t, target, id)
	reuseCarriedID(t, source, id)

	var stdout, stderr bytes.Buffer
	runStrandedRecovery(t, cityPath, cfg, &stdout, &stderr)
	if !strings.Contains(stdout.String(), "NOT RECORDED") || !strings.Contains(stdout.String(), id) {
		t.Errorf("recovery did not name %s as unprovable; stdout: %s stderr: %s", id, stdout.String(), stderr.String())
	}
	if got := strandedIDs(t, cityPath, target); !slices.Contains(got, id) {
		t.Errorf("stranded = %v; the reused %s must stay stranded", got, id)
	}
}

// The classification table, independent of any store.
func TestInfraContainmentClassifyTable(t *testing.T) {
	written := time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC)
	before := written.Add(-24 * time.Hour)
	after := written.Add(5 * 24 * time.Hour)
	keyed := infraCopyManifest{
		IDs:       map[string]bool{"mc-wisp-jkl": true, "mc-1": true, "mc-legacy": true},
		Created:   map[string]time.Time{"mc-wisp-jkl": before, "mc-1": before},
		WrittenAt: written,
	}
	cases := []struct {
		name    string
		bead    beads.Bead
		held    bool
		want    string
		removed int
	}{
		{"carried, held", beads.Bead{ID: "mc-1", CreatedAt: before}, true, "retained", 0},
		{"carried, collected", beads.Bead{ID: "mc-1", CreatedAt: before}, false, "retained", 1},
		{"reused id, original held", beads.Bead{ID: "mc-wisp-jkl", CreatedAt: after}, true, "stranded", 0},
		{"reused id, original collected", beads.Bead{ID: "mc-wisp-jkl", CreatedAt: after}, false, "stranded", 0},
		{"legacy entry, older row", beads.Bead{ID: "mc-legacy", CreatedAt: before}, false, "retained", 1},
		{"legacy entry, younger row", beads.Bead{ID: "mc-legacy", CreatedAt: after}, true, "stranded", 0},
		{"unlisted, younger, held", beads.Bead{ID: "mc-9", CreatedAt: after}, true, "stranded", 0},
		{"unlisted, older, held", beads.Bead{ID: "mc-9", CreatedAt: before}, true, "retained", 0},
		{"unlisted, older, not held", beads.Bead{ID: "mc-9", CreatedAt: before}, false, "stranded", 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var gap infraContainmentGap
			gap.classify(tc.bead, tc.held, keyed)
			got := "none"
			switch {
			case len(gap.Stranded) == 1 && len(gap.Retained) == 0:
				got = "stranded"
			case len(gap.Retained) == 1 && len(gap.Stranded) == 0:
				got = "retained"
			}
			if got != tc.want || gap.RemovedSinceCutover != tc.removed {
				t.Errorf("classify = %s (removed %d), want %s (removed %d)", got, gap.RemovedSinceCutover, tc.want, tc.removed)
			}
		})
	}
}

// An older build reads every non-empty line as an id. The annotated format must
// leave its view intact: every real id present, and nothing it could mistake
// for a real id.
func TestAnnotatedManifestStaysReadableByTheLegacyParser(t *testing.T) {
	target := infraBindingTarget{Root: t.TempDir()}
	ids := []string{"mc-1", "mc-wisp-jkl"}
	created := map[string]time.Time{"mc-1": time.Date(2026, 8, 1, 9, 30, 0, 0, time.UTC)}
	if err := writeInfraCopyManifestRecord(target, ids, created); err != nil {
		t.Fatal(err)
	}
	contents, err := os.ReadFile(target.ManifestPath())
	if err != nil {
		t.Fatal(err)
	}
	legacyView := map[string]bool{}
	for _, line := range strings.Split(string(contents), "\n") {
		if id := strings.TrimSpace(line); id != "" {
			legacyView[id] = true
		}
	}
	for _, id := range ids {
		if !legacyView[id] {
			t.Errorf("the legacy parser does not see %s", id)
		}
	}
	for id := range legacyView {
		if !slices.Contains(ids, id) && !strings.HasPrefix(id, "#") {
			t.Errorf("the legacy parser sees %q, which could collide with a real id", id)
		}
	}
	got, recorded, err := readInfraCopyManifestRecord(target)
	if err != nil || !recorded {
		t.Fatalf("read back: recorded=%v err=%v", recorded, err)
	}
	if got.Len() != 2 || !got.Created["mc-1"].Equal(created["mc-1"]) || got.WrittenAt.IsZero() {
		t.Errorf("read back %+v", got)
	}
	if _, ok := got.Created["mc-wisp-jkl"]; ok {
		t.Error("an id written without created_at read back with one")
	}
}
