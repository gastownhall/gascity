package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
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
	if err := writeInfraCopyManifestRecord(target, ids, manifest.Created, manifest.LegacyBound); err != nil {
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

// writeLegacyManifest rewrites the city's manifest in the format every build
// before this one wrote: one bare id per line, no annotations.
func writeLegacyManifest(t *testing.T, target infraBindingTarget) []string {
	t.Helper()
	ids := manifestIDs(t, target)
	if err := os.WriteFile(target.ManifestPath(), []byte(strings.Join(ids, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return ids
}

// waitPastMarkerBound sleeps until a bead created now is younger than the
// legacy bound the convergence marker records (the end of its second).
func waitPastMarkerBound(t *testing.T, target infraBindingTarget) time.Time {
	t.Helper()
	bound, ok := readInfraMarkerBound(target)
	if !ok {
		t.Fatal("the fixture's convergence marker records no time")
	}
	if d := time.Until(bound.Add(infraManifestClockSlack)); d > 0 {
		time.Sleep(d + 50*time.Millisecond)
	}
	return bound
}

// A manifest written by an older build has no created_at. It keeps reading
// compatibly: an id it lists still matches the row it carried. But a row
// created after the manifest was written is never that row, and "when it was
// written" is the time the convergence marker RECORDS, never the file's mtime.
func TestLegacyManifestStillMatchesByIDButNotForARowYoungerThanIt(t *testing.T) {
	prevSlack := infraManifestClockSlack
	infraManifestClockSlack = 0
	t.Cleanup(func() { infraManifestClockSlack = prevSlack })

	cityPath, _, source, target := convergedRecoveryCity(t)
	ids := writeLegacyManifest(t, target)
	bound := waitPastMarkerBound(t, target)
	legacy, recorded, err := readInfraCopyManifestRecord(target)
	if err != nil || !recorded {
		t.Fatalf("reading the legacy manifest: recorded=%v err=%v", recorded, err)
	}
	if legacy.Len() != len(ids) || len(legacy.Created) != 0 {
		t.Fatalf("legacy read = %d ids / %d created, want %d / 0", legacy.Len(), len(legacy.Created), len(ids))
	}
	if !legacy.LegacyBound.Equal(bound) || !legacy.WrittenAt.Equal(bound) {
		t.Errorf("legacy bound = %s / written %s, want the marker's recorded time %s", legacy.LegacyBound, legacy.WrittenAt, bound)
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

// Review probe B: copying a legacy manifest back from its .bak, an rsync
// without -t, or a touch moves its mtime past an id reuse. The bound must not
// move with it, or the clear deletes a reused row that was never copied.
func TestLegacyManifestMtimeBumpDoesNotLetTheClearDeleteAReusedRow(t *testing.T) {
	prevSlack := infraManifestClockSlack
	infraManifestClockSlack = 0
	t.Cleanup(func() { infraManifestClockSlack = prevSlack })

	cityPath, _, source, target := convergedRecoveryCity(t)
	writeLegacyManifest(t, target)
	waitPastMarkerBound(t, target)
	id := carriedMessageID(t, source)
	if err := openMigratedDestination(t, target).Delete(id); err != nil {
		t.Fatal(err)
	}
	reuseCarriedID(t, source, id)
	if got := strandedIDs(t, cityPath, target); !slices.Contains(got, id) {
		t.Fatalf("precondition: the reused %s is not stranded (%v)", id, got)
	}
	future := time.Now().Add(time.Hour)
	if err := os.Chtimes(target.ManifestPath(), future, future); err != nil {
		t.Fatal(err)
	}
	if got := strandedIDs(t, cityPath, target); !slices.Contains(got, id) {
		t.Errorf("after an mtime bump the reused %s is no longer stranded (%v)", id, got)
	}
	var announce bytes.Buffer
	if _, err := clearRetainedInfraCopies(cityPath, target, &announce); err == nil {
		t.Error("the clear succeeded with a reused, never-copied row in the work store")
	}
	if _, err := source.Get(id); err != nil {
		t.Errorf("the clear deleted the reused, never-copied %s: %v", id, err)
	}
}

// Review probe A: this build's recover-stranded rewrites a legacy manifest
// (moving #gc:written to now) while recovering some OTHER row. The legacy
// entries must keep the legacy bound, or a reused id reads as carried.
func TestRecoverRewritingALegacyManifestKeepsItsLegacyBound(t *testing.T) {
	prevSlack := infraManifestClockSlack
	infraManifestClockSlack = 0
	t.Cleanup(func() { infraManifestClockSlack = prevSlack })

	cityPath, cfg, source, target := convergedRecoveryCity(t)
	writeLegacyManifest(t, target)
	bound := waitPastMarkerBound(t, target)
	id := carriedMessageID(t, source)
	reuseCarriedID(t, source, id)
	other := mustCreateInfraBead(t, source, beads.Bead{Title: "a later stranded mail", Type: "message"})
	if got := strandedIDs(t, cityPath, target); !slices.Contains(got, id) || !slices.Contains(got, other.ID) {
		t.Fatalf("precondition: stranded = %v, want %s and %s", got, id, other.ID)
	}

	var stdout, stderr bytes.Buffer
	runStrandedRecovery(t, cityPath, cfg, &stdout, &stderr)
	after := strandedIDs(t, cityPath, target)
	if !slices.Contains(after, id) {
		t.Errorf("after recover-stranded rewrote the legacy manifest, the reused %s is no longer stranded (%v); stdout: %s", id, after, stdout.String())
	}
	if slices.Contains(after, other.ID) {
		t.Errorf("the recovered %s is still stranded", other.ID)
	}
	manifest, _, err := readInfraCopyManifestRecord(target)
	if err != nil {
		t.Fatal(err)
	}
	if !manifest.LegacyBound.Equal(bound) {
		t.Errorf("the rewrite persisted legacy bound %s, want the original %s", manifest.LegacyBound, bound)
	}
	contents, err := os.ReadFile(target.ManifestPath())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(contents), infraManifestLegacyWrittenPrefix) {
		t.Errorf("the rewritten manifest does not persist the legacy bound:\n%s", contents)
	}
	// And a second rewrite carries it unchanged.
	future := time.Now().Add(time.Hour)
	_ = os.Chtimes(target.ManifestPath(), future, future)
	runStrandedRecovery(t, cityPath, cfg, &stdout, &stderr)
	if again, _, _ := readInfraCopyManifestRecord(target); !again.LegacyBound.Equal(bound) {
		t.Errorf("a second rewrite moved the legacy bound to %s, want %s", again.LegacyBound, bound)
	}
	if got := strandedIDs(t, cityPath, target); !slices.Contains(got, id) {
		t.Errorf("after a second rewrite the reused %s is no longer stranded (%v)", id, got)
	}
}

// With no trustworthy bound — a legacy manifest whose marker carries no time —
// the clear refuses legacy entries rather than guess, read-only verdicts keep
// the id-only reading, and recover-stranded records the rows the binding still
// holds once it proves them equal, after which the clear proceeds.
func TestClearRefusesUnboundedLegacyEntriesUntilRecoverRecordsThem(t *testing.T) {
	cityPath, cfg, _, target := convergedRecoveryCity(t)
	writeLegacyManifest(t, target)
	if err := os.WriteFile(target.MarkerPath(), []byte("infrastructure classes migrated\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	manifest, _, err := readInfraCopyManifestRecord(target)
	if err != nil {
		t.Fatal(err)
	}
	if !manifest.LegacyBound.IsZero() || !manifest.WrittenAt.IsZero() {
		t.Fatalf("a marker with no time produced bound %s / written %s; the mtime must not stand in", manifest.LegacyBound, manifest.WrittenAt)
	}
	if got := strandedIDs(t, cityPath, target); len(got) != 0 {
		t.Errorf("unbounded legacy entries were called stranded: %v", got)
	}
	var announce bytes.Buffer
	_, err = clearRetainedInfraCopies(cityPath, target, &announce)
	var unbounded *infraUnboundedLegacyRows
	if !errors.As(err, &unbounded) {
		t.Fatalf("clear = %v, want the unbounded-legacy refusal", err)
	}

	var stdout, stderr bytes.Buffer
	if code := runStrandedRecovery(t, cityPath, cfg, &stdout, &stderr); code != 0 {
		t.Fatalf("recover exited %d: %s %s", code, stdout.String(), stderr.String())
	}
	upgraded, _, err := readInfraCopyManifestRecord(target)
	if err != nil {
		t.Fatal(err)
	}
	if len(upgraded.Created) != upgraded.Len() {
		t.Errorf("recover recorded created_at for %d of %d entries", len(upgraded.Created), upgraded.Len())
	}
	if _, err := clearRetainedInfraCopies(cityPath, target, &announce); err != nil {
		t.Errorf("the clear still refuses after recover recorded the entries: %v", err)
	}
}

// M11: created_at is compared at the work store's second granularity. A
// manifest that recorded nanoseconds still matches a row read back truncated
// (or rounded within the second); a different second is a different bead.
func TestManifestCreatedAtMatchesToTheSecond(t *testing.T) {
	recorded := time.Date(2026, 8, 9, 18, 2, 11, 987654321, time.UTC)
	m := infraCopyManifest{IDs: map[string]bool{"mc-1": true}, Created: map[string]time.Time{"mc-1": recorded}}
	for _, tc := range []struct {
		name string
		at   time.Time
		want infraManifestMatch
	}{
		{"truncated to the second", recorded.Truncate(time.Second), infraManifestCarried},
		{"same second, other nanos", recorded.Truncate(time.Second).Add(5 * time.Millisecond), infraManifestCarried},
		{"same instant, other zone", recorded.In(time.FixedZone("x", 3600)), infraManifestCarried},
		{"next second", recorded.Truncate(time.Second).Add(time.Second), infraManifestReusedID},
		{"previous second", recorded.Truncate(time.Second).Add(-time.Nanosecond), infraManifestReusedID},
	} {
		if got := m.Match(beads.Bead{ID: "mc-1", CreatedAt: tc.at}); got != tc.want {
			t.Errorf("%s: Match = %d, want %d", tc.name, got, tc.want)
		}
	}
}

// M17: an annotated manifest records its own write time, and that — not the
// file's mtime — judges the rows it does not list.
func TestAnnotatedManifestRecordsItsWriteTime(t *testing.T) {
	target := infraBindingTarget{Root: t.TempDir()}
	before := time.Now()
	if err := writeInfraCopyManifestRecord(target, []string{"mc-1"}, map[string]time.Time{"mc-1": before.Add(-time.Hour)}, time.Time{}); err != nil {
		t.Fatal(err)
	}
	after := time.Now()
	future := after.Add(48 * time.Hour)
	if err := os.Chtimes(target.ManifestPath(), future, future); err != nil {
		t.Fatal(err)
	}
	m, _, err := readInfraCopyManifestRecord(target)
	if err != nil {
		t.Fatal(err)
	}
	if m.WrittenAt.Before(before.Add(-time.Second)) || m.WrittenAt.After(after.Add(time.Second)) {
		t.Fatalf("written = %s, want the write time (between %s and %s), never the mtime %s", m.WrittenAt, before, after, future)
	}
	young := beads.Bead{ID: "mc-9", CreatedAt: after.Add(time.Hour)}
	if !m.createdAfterWrite(young) {
		t.Error("an unlisted row created an hour after the write is not younger than the manifest")
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
		IDs:         map[string]bool{"mc-wisp-jkl": true, "mc-1": true, "mc-legacy": true},
		Created:     map[string]time.Time{"mc-wisp-jkl": before, "mc-1": before},
		WrittenAt:   written,
		LegacyBound: written,
	}
	unboundedManifest := infraCopyManifest{IDs: map[string]bool{"mc-legacy": true}, WrittenAt: written}
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
	for _, tc := range []struct {
		name string
		held bool
	}{{"unbounded legacy entry, held", true}, {"unbounded legacy entry, collected", false}} {
		t.Run(tc.name, func(t *testing.T) {
			b := beads.Bead{ID: "mc-legacy", CreatedAt: after}
			if got := unboundedManifest.Match(b); got != infraManifestLegacyUnbounded {
				t.Fatalf("Match = %d, want unbounded", got)
			}
			if unboundedManifest.Carried(b) {
				t.Error("an unbounded legacy entry reads as carried for a deletion")
			}
			var gap infraContainmentGap
			gap.classify(b, tc.held, unboundedManifest)
			if len(gap.Retained) != 1 || len(gap.Stranded) != 0 {
				t.Errorf("classify = %+v, want the id-only reading (retained)", gap)
			}
		})
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
	if err := writeInfraCopyManifestRecord(target, ids, created, time.Time{}); err != nil {
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

// unboundedCollectedCity is the re-review's P5/Q2 shape: a legacy manifest
// whose marker records no time (so its entries are unbounded), and a binding
// that has closed and collected the bead behind one of them. It returns the
// id of that bead, which is still in the work store as the frozen,
// cutover-time row.
func unboundedCollectedCity(t *testing.T, closeWorkRow bool) (cityPath string, cfg *config.City, source beads.Store, target infraBindingTarget, id string) {
	t.Helper()
	cityPath, cfg, source, target = convergedRecoveryCity(t)
	writeLegacyManifest(t, target)
	if err := os.WriteFile(target.MarkerPath(), []byte("infrastructure classes migrated\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	rows, err := readInfraSnapshot(source)
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range rows {
		if b.Type == "session" {
			id = b.ID
		}
	}
	if id == "" {
		t.Fatal("the fixture carried no session")
	}
	if frozen, _ := source.Get(id); frozen.Status == "closed" {
		t.Fatal("the fixture's frozen session is already closed")
	}
	if closeWorkRow {
		if err := source.Close(id); err != nil {
			t.Fatal(err)
		}
	}
	dest := openMigratedDestination(t, target)
	if err := dest.Close(id); err != nil {
		t.Fatal(err)
	}
	if err := dest.Delete(id); err != nil {
		t.Fatal(err)
	}
	var announce bytes.Buffer
	var unbounded *infraUnboundedLegacyRows
	if _, err := clearRetainedInfraCopies(cityPath, target, &announce); !errors.As(err, &unbounded) {
		t.Fatalf("precondition: clear = %v, want the unbounded refusal", err)
	}
	return cityPath, cfg, source, target, id
}

func runStrandedRecoveryWith(t *testing.T, cityPath string, cfg *config.City, opts strandedRecoveryOptions) (int, string, string) {
	t.Helper()
	stubInfraControllerPing(t, 0)
	var stdout, stderr bytes.Buffer
	request := storageOperatorRequest{CityPath: cityPath, Cfg: cfg, FleetStopped: true}
	code := doStorageRecoverStranded(context.Background(), request, opts, &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

func assertClearProceeds(t *testing.T, cityPath string, target infraBindingTarget) {
	t.Helper()
	var announce bytes.Buffer
	if _, err := clearRetainedInfraCopies(cityPath, target, &announce); err != nil {
		t.Errorf("the clear still refuses (no exit from the loop): %v", err)
	}
}

// A CLOSED frozen row is terminal state: recover moves and records it, and the
// clear then proceeds (re-review P5 and mutant N32).
func TestRecoverMovesAClosedUnboundedEntryTheBindingCollected(t *testing.T) {
	cityPath, cfg, _, target, id := unboundedCollectedCity(t, true)
	if code, stdout, stderr := runStrandedRecoveryWith(t, cityPath, cfg, strandedRecoveryOptions{}); code != 0 {
		t.Fatalf("recover exited %d: %s %s", code, stdout, stderr)
	}
	got, err := openMigratedDestination(t, target).Get(id)
	if err != nil {
		t.Fatalf("recover did not move the closed unbounded entry %s: %v", id, err)
	}
	if got.Status != "closed" {
		t.Errorf("moved %s arrived %q, want closed", id, got.Status)
	}
	assertClearProceeds(t, cityPath, target)
}

// Re-review round 3, must-fix 1 (probe Q2): an OPEN frozen row whose binding
// bead was closed and collected is NOT moved — that would resurrect it OPEN
// (#5987). Recover refuses, names it and the two choices, and writes nothing.
func TestRecoverRefusesAnOpenUnboundedEntryTheBindingCollected(t *testing.T) {
	cityPath, cfg, _, target, id := unboundedCollectedCity(t, false)
	before, err := os.ReadFile(target.ManifestPath())
	if err != nil {
		t.Fatal(err)
	}
	code, stdout, stderr := runStrandedRecoveryWith(t, cityPath, cfg, strandedRecoveryOptions{})
	if code == 0 {
		t.Fatalf("recover moved or accepted an open frozen row; stdout: %s", stdout)
	}
	for _, want := range []string{id, "--" + strandedMoveFlag, "--" + strandedRecordCarriedFlag, "status=open", "type=session", "created=", "updated=", "title="} {
		if !strings.Contains(stderr, want) {
			t.Errorf("the refusal does not name %q: %s", want, stderr)
		}
	}
	if _, err := openMigratedDestination(t, target).Get(id); err == nil {
		t.Errorf("the refused recover resurrected %s in the binding", id)
	}
	if after, _ := os.ReadFile(target.ManifestPath()); !bytes.Equal(before, after) {
		t.Error("the refused recover rewrote the manifest")
	}
}

// The operator's --move choice: the row is copied in as it is.
func TestRecoverMovesAnOpenUnboundedEntryOnRequest(t *testing.T) {
	cityPath, cfg, _, target, id := unboundedCollectedCity(t, false)
	if code, stdout, stderr := runStrandedRecoveryWith(t, cityPath, cfg, strandedRecoveryOptions{Move: map[string]bool{id: true}}); code != 0 {
		t.Fatalf("recover --move %s exited %d: %s %s", id, code, stdout, stderr)
	}
	if _, err := openMigratedDestination(t, target).Get(id); err != nil {
		t.Errorf("recover --move did not copy %s: %v", id, err)
	}
	assertClearProceeds(t, cityPath, target)
}

// The operator's --record-carried choice: the row is NOT copied, it is
// recorded as the bead the copy carried, and the clear deletes it.
func TestRecoverRecordsAnOpenUnboundedEntryAsCarriedOnRequest(t *testing.T) {
	cityPath, cfg, source, target, id := unboundedCollectedCity(t, false)
	if code, stdout, stderr := runStrandedRecoveryWith(t, cityPath, cfg, strandedRecoveryOptions{RecordCarried: map[string]bool{id: true}}); code != 0 {
		t.Fatalf("recover --record-carried %s exited %d: %s %s", id, code, stdout, stderr)
	}
	if _, err := openMigratedDestination(t, target).Get(id); err == nil {
		t.Errorf("recover --record-carried copied %s into the binding", id)
	}
	manifest, _, err := readInfraCopyManifestRecord(target)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := manifest.Created[id]; !ok {
		t.Errorf("recover --record-carried did not record %s with its created_at", id)
	}
	if got := strandedIDs(t, cityPath, target); len(got) != 0 {
		t.Errorf("stranded after --record-carried: %v", got)
	}
	assertClearProceeds(t, cityPath, target)
	if _, err := source.Get(id); err == nil {
		t.Errorf("the clear left the frozen row %s in the work store", id)
	}
}

// A choice may only name a row it answers for.
func TestRecoverRefusesAChoiceForARowThatNeedsNone(t *testing.T) {
	cityPath, cfg, _, target, id := unboundedCollectedCity(t, false)
	before, _ := os.ReadFile(target.ManifestPath())
	code, _, stderr := runStrandedRecoveryWith(t, cityPath, cfg, strandedRecoveryOptions{Move: map[string]bool{id: true, "gc-nope": true}})
	if code == 0 || !strings.Contains(stderr, "gc-nope") {
		t.Errorf("recover accepted a choice for a row that needs none (code %d): %s", code, stderr)
	}
	if after, _ := os.ReadFile(target.ManifestPath()); !bytes.Equal(before, after) {
		t.Error("the refused recover rewrote the manifest")
	}
	code, _, stderr = runStrandedRecoveryWith(t, cityPath, cfg, strandedRecoveryOptions{Move: map[string]bool{id: true}, RecordCarried: map[string]bool{id: true}})
	if code == 0 || !strings.Contains(stderr, "choose one") {
		t.Errorf("recover accepted both choices for %s (code %d): %s", id, code, stderr)
	}
}

// Every annotated write bounds the bare ids: with no legacy entries the
// bound is the write time, so a bare id an OLDER build's recover-stranded
// appends later is judged (reused when younger, carried when older) instead
// of unbounded.
func TestAnnotatedWriteAlwaysBoundsLaterBareIDs(t *testing.T) {
	target := infraBindingTarget{Root: t.TempDir()}
	created := time.Now().Add(-time.Hour)
	before := time.Now()
	if err := writeInfraCopyManifestRecord(target, []string{"mc-1"}, map[string]time.Time{"mc-1": created}, time.Time{}); err != nil {
		t.Fatal(err)
	}
	contents, err := os.ReadFile(target.ManifestPath())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(contents), infraManifestLegacyWrittenPrefix) {
		t.Fatalf("an all-annotated write carries no legacy bound:\n%s", contents)
	}
	// What an older build's superset rewrite does: keep every line, append a
	// bare id.
	if err := os.WriteFile(target.ManifestPath(), append(contents, []byte("mc-old\n")...), 0o644); err != nil {
		t.Fatal(err)
	}
	m, _, err := readInfraCopyManifestRecord(target)
	if err != nil {
		t.Fatal(err)
	}
	if m.LegacyBound.Before(before.Add(-time.Second)) {
		t.Errorf("legacy bound = %s, want the write time (~%s)", m.LegacyBound, before)
	}
	if got := m.Match(beads.Bead{ID: "mc-old", CreatedAt: before.Add(-time.Minute)}); got != infraManifestCarried {
		t.Errorf("an older bare id = %d, want carried", got)
	}
	if got := m.Match(beads.Bead{ID: "mc-old", CreatedAt: time.Now().Add(time.Hour)}); got != infraManifestReusedID {
		t.Errorf("a younger bare id = %d, want reused", got)
	}
}

// Mutant N33: the clock slack applies to the unlisted-row rule — a row created
// within the slack after the write is not younger than the manifest.
func TestCreatedAfterWriteHonorsTheClockSlack(t *testing.T) {
	written := time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC)
	m := infraCopyManifest{WrittenAt: written}
	if m.createdAfterWrite(beads.Bead{ID: "x", CreatedAt: written.Add(infraManifestClockSlack / 2)}) {
		t.Error("a row inside the clock slack reads as younger than the manifest")
	}
	if !m.createdAfterWrite(beads.Bead{ID: "x", CreatedAt: written.Add(2 * infraManifestClockSlack)}) {
		t.Error("a row past the clock slack does not read as younger")
	}
}

// Mutant N35: infraRetainedIDs (the list a refusal names) takes the read-only
// reading, so an unbounded legacy entry is listed as retained.
func TestInfraRetainedIDsListsUnboundedLegacyEntries(t *testing.T) {
	cityPath, _, source, target := convergedRecoveryCity(t)
	writeLegacyManifest(t, target)
	if err := os.WriteFile(target.MarkerPath(), []byte("infrastructure classes migrated\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ids, err := infraRetainedIDs(cityPath, target)
	if err != nil {
		t.Fatal(err)
	}
	if id := carriedMessageID(t, source); !slices.Contains(ids, id) {
		t.Errorf("retained ids %v omit the unbounded legacy entry %s", ids, id)
	}
}

// Mutant A20: the "next: clear" line names the copies the binding still holds
// apart from those it has since collected.
func TestRecoverNextLineCountsHeldAndCollectedApart(t *testing.T) {
	cityPath, cfg, source, target := convergedRecoveryCity(t)
	id := carriedMessageID(t, source)
	if err := openMigratedDestination(t, target).Delete(id); err != nil {
		t.Fatal(err)
	}
	mustCreateInfraBead(t, source, beads.Bead{Title: "a later stranded mail", Type: "message"})
	code, stdout, stderr := runStrandedRecoveryWith(t, cityPath, cfg, strandedRecoveryOptions{})
	if code != 0 {
		t.Fatalf("recover exited %d: %s %s", code, stdout, stderr)
	}
	rows, err := readInfraSnapshot(source)
	if err != nil {
		t.Fatal(err)
	}
	want := fmt.Sprintf("still holds %d retained cop(ies) (%d of beads the binding holds, 1 the binding has since collected)", len(rows), len(rows)-1)
	if !strings.Contains(stdout, want) {
		t.Errorf("stdout does not say %q:\n%s", want, stdout)
	}
}

// Mutant R1: a frozen row that is in_progress or blocked is not closed, and is
// refused like an open one.
func TestRecoverRefusesInProgressAndBlockedUnboundedEntries(t *testing.T) {
	for _, status := range []string{"in_progress", "blocked"} {
		t.Run(status, func(t *testing.T) {
			cityPath, cfg, source, target, id := unboundedCollectedCity(t, false)
			st := status
			if err := source.Update(id, beads.UpdateOpts{Status: &st}); err != nil {
				t.Fatalf("setting %s to %s: %v", id, status, err)
			}
			code, stdout, stderr := runStrandedRecoveryWith(t, cityPath, cfg, strandedRecoveryOptions{})
			if code == 0 {
				t.Fatalf("recover moved a %s frozen row; stdout: %s", status, stdout)
			}
			if !strings.Contains(stderr, "status="+status) {
				t.Errorf("the refusal does not show status=%s: %s", status, stderr)
			}
			if _, err := openMigratedDestination(t, target).Get(id); err == nil {
				t.Errorf("the refused recover copied the %s row %s", status, id)
			}
		})
	}
}

// Mutants R6/R7: a run whose ONLY action is --record-carried (the binding
// holds no infrastructure row, every legacy row is open) records them and
// counts them removed since cutover; the clear then proceeds.
func TestRecoverWhoseOnlyActionIsRecordCarried(t *testing.T) {
	cityPath, cfg, source, target := convergedRecoveryCity(t)
	writeLegacyManifest(t, target)
	if err := os.WriteFile(target.MarkerPath(), []byte("infrastructure classes migrated\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	rows, err := readInfraSnapshot(source)
	if err != nil {
		t.Fatal(err)
	}
	dest := openMigratedDestination(t, target)
	choice := map[string]bool{}
	for _, b := range rows {
		if b.Status == "closed" {
			t.Fatalf("fixture row %s is closed; every legacy row must be open", b.ID)
		}
		if err := dest.Delete(b.ID); err != nil {
			t.Fatal(err)
		}
		choice[b.ID] = true
	}
	_ = closeBeadStoreHandle(dest)
	code, stdout, stderr := runStrandedRecoveryWith(t, cityPath, cfg, strandedRecoveryOptions{RecordCarried: choice})
	if code != 0 {
		t.Fatalf("recover --record-carried exited %d: %s %s", code, stdout, stderr)
	}
	if want := fmt.Sprintf("removed since cutover: %d\n", len(rows)); !strings.Contains(stdout, want) {
		t.Errorf("stdout does not count the recorded rows (%q):\n%s", want, stdout)
	}
	manifest, _, err := readInfraCopyManifestRecord(target)
	if err != nil {
		t.Fatal(err)
	}
	for id := range choice {
		if _, ok := manifest.Created[id]; !ok {
			t.Errorf("%s was not recorded", id)
		}
	}
	assertClearProceeds(t, cityPath, target)
}

// --dry-run and --dump show the rows awaiting a choice without writing, and a
// dry run names what each choice would do.
func TestRecoverDryRunAndDumpShowUnboundedRows(t *testing.T) {
	cityPath, cfg, _, target, id := unboundedCollectedCity(t, false)
	before, _ := os.ReadFile(target.ManifestPath())
	code, stdout, _ := runStrandedRecoveryWith(t, cityPath, cfg, strandedRecoveryOptions{DryRun: true})
	if code == 0 || !strings.Contains(stdout, "awaiting a choice") || !strings.Contains(stdout, id) {
		t.Errorf("dry run without a choice = %d:\n%s", code, stdout)
	}
	_, stdout, _ = runStrandedRecoveryWith(t, cityPath, cfg, strandedRecoveryOptions{DryRun: true, RecordCarried: map[string]bool{id: true}})
	if !strings.Contains(stdout, "would record as carried: "+id+" (the clear then deletes its work-store row; the backup keeps it)") {
		t.Errorf("dry run --record-carried does not say what it would do:\n%s", stdout)
	}
	_, stdout, _ = runStrandedRecoveryWith(t, cityPath, cfg, strandedRecoveryOptions{DryRun: true, Move: map[string]bool{id: true}})
	if !strings.Contains(stdout, "would move: "+id) {
		t.Errorf("dry run --move does not say what it would do:\n%s", stdout)
	}
	dump := filepath.Join(t.TempDir(), "dump.json")
	code, _, stderr := runStrandedRecoveryWith(t, cityPath, cfg, strandedRecoveryOptions{DumpPath: dump})
	if code == 0 {
		t.Error("a dump with rows awaiting a choice went on to write")
	}
	if data, err := os.ReadFile(dump); err != nil || !strings.Contains(string(data), id) {
		t.Errorf("the dump does not hold %s (err %v); stderr: %s", id, err, stderr)
	}
	if after, _ := os.ReadFile(target.ManifestPath()); !bytes.Equal(before, after) {
		t.Error("a dry run or dump rewrote the manifest")
	}
	if _, err := openMigratedDestination(t, target).Get(id); err == nil {
		t.Error("a dry run or dump copied the row")
	}
}

// Re-running with the same choice after it took effect is a no-op with a
// note, for both choices, and after a copy that failed before the manifest was
// extended (the re-run is a resume).
func TestRecoverRerunWithTheSameChoiceIsANoOp(t *testing.T) {
	for name, opts := range map[string]func(string) strandedRecoveryOptions{
		"record-carried": func(id string) strandedRecoveryOptions {
			return strandedRecoveryOptions{RecordCarried: map[string]bool{id: true}}
		},
		"move": func(id string) strandedRecoveryOptions {
			return strandedRecoveryOptions{Move: map[string]bool{id: true}}
		},
	} {
		t.Run(name, func(t *testing.T) {
			cityPath, cfg, _, _, id := unboundedCollectedCity(t, false)
			if code, so, se := runStrandedRecoveryWith(t, cityPath, cfg, opts(id)); code != 0 {
				t.Fatalf("first run = %d: %s %s", code, so, se)
			}
			code, stdout, stderr := runStrandedRecoveryWith(t, cityPath, cfg, opts(id))
			if code != 0 || !strings.Contains(stdout, "already resolved") || !strings.Contains(stdout, id) {
				t.Errorf("re-run with the same flags = %d: %s %s", code, stdout, stderr)
			}
		})
	}
	t.Run("move after a failed copy", func(t *testing.T) {
		cityPath, cfg, source, target, id := unboundedCollectedCity(t, false)
		b, err := source.Get(id)
		if err != nil {
			t.Fatal(err)
		}
		dest := openMigratedDestination(t, target)
		if _, err := dest.(beads.ForeignIDCreator).CreateWithForeignID(infraMigrationRow(b)); err != nil {
			t.Fatal(err)
		}
		_ = closeBeadStoreHandle(dest)
		code, stdout, stderr := runStrandedRecoveryWith(t, cityPath, cfg, strandedRecoveryOptions{Move: map[string]bool{id: true}})
		if code != 0 || !strings.Contains(stdout, "already resolved") {
			t.Errorf("re-run with --move after a failed copy = %d: %s %s", code, stdout, stderr)
		}
		if got := strandedIDs(t, cityPath, target); len(got) != 0 {
			t.Errorf("still stranded after the resume: %v", got)
		}
		assertClearProceeds(t, cityPath, target)
	})
}
