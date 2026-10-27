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
	"github.com/gastownhall/gascity/internal/coordclass"
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
	if err := rewriteInfraCopyManifest(target, manifest, ids, manifest.Created); err != nil {
		t.Fatal(err)
	}
}

// backdateHeldOriginal re-creates the binding's row under id an hour earlier,
// matching backdateManifestEntry: the bead the copy carried was created long
// before the work store minted its id again, and its created_at is what tells
// the two apart (infraHeldRowIsThisBead). Created moments apart in a fixture,
// they would share a second and read as one bead.
func backdateHeldOriginal(t *testing.T, target infraBindingTarget, id string) {
	t.Helper()
	dest, err := openInfraDestination(target)
	if err != nil {
		t.Fatalf("opening the binding: %v", err)
	}
	defer func() {
		if err := closeBeadStoreHandle(dest); err != nil {
			t.Errorf("closing the binding: %v", err)
		}
	}()
	original, err := dest.Get(id)
	if err != nil {
		t.Fatalf("reading the binding's original %s: %v", id, err)
	}
	if err := dest.Delete(id); err != nil {
		t.Fatalf("removing the binding's original %s: %v", id, err)
	}
	original.CreatedAt = original.CreatedAt.Add(-time.Hour)
	original.UpdatedAt = original.UpdatedAt.Add(-time.Hour)
	if _, err := dest.(beads.ForeignIDCreator).CreateWithForeignID(infraMigrationRow(original)); err != nil {
		t.Fatalf("re-creating the binding's original %s an hour earlier: %v", id, err)
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
	backdateHeldOriginal(t, target, id)
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
// (persisting it in the annotated format) while recovering some OTHER row.
// The legacy entries must keep the legacy bound, or a reused id reads as
// carried.
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
// file's mtime — bounds the bare ids an older build appends to it later.
func TestAnnotatedManifestRecordsItsWriteTime(t *testing.T) {
	target := infraBindingTarget{Root: t.TempDir()}
	before := time.Now()
	if err := writeInfraCopyManifestRecord(target, []string{"mc-1"}, map[string]time.Time{"mc-1": before.Add(-time.Hour)}, coordclass.CurrentGeneration); err != nil {
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
	if !m.LegacyBound.Equal(m.WrittenAt) {
		t.Errorf("legacy bound = %s, want the write time %s", m.LegacyBound, m.WrittenAt)
	}
	m.IDs["mc-9"] = true
	if got := m.Match(beads.Bead{ID: "mc-9", CreatedAt: after.Add(time.Hour)}); got != infraManifestPresumedReused {
		t.Errorf("Match = %d for a bare id whose row was created an hour after the write, want presumed reused (%d)", got, infraManifestPresumedReused)
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

// heldReusedIDCity is the shape no command could clear: the copy carried the
// original bead under id, the binding still holds it, and the work store has
// since minted id again for an unrelated bead. It returns the binding's row
// and the work-store row under id.
func heldReusedIDCity(t *testing.T) (cityPath string, cfg *config.City, source beads.Store, target infraBindingTarget, original, reused beads.Bead) {
	t.Helper()
	cityPath, cfg, source, target = convergedRecoveryCity(t)
	id := carriedMessageID(t, source)
	backdateManifestEntry(t, target, id)
	backdateHeldOriginal(t, target, id)
	reused = reuseCarriedID(t, source, id)
	original, err := openMigratedDestination(t, target).Get(id)
	if err != nil {
		t.Fatalf("the fixture's binding lost the original %s: %v", id, err)
	}
	return cityPath, cfg, source, target, original, reused
}

// fixtureSessionID returns the id of the session bead the cutover carried.
func fixtureSessionID(t *testing.T, source beads.Store) string {
	t.Helper()
	rows, err := readInfraSnapshot(source)
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range rows {
		if b.Type == "session" {
			return b.ID
		}
	}
	t.Fatal("the fixture carried no session")
	return ""
}

// recoverChoiceRecords returns the rows the per-id choices backed up.
func recoverChoiceRecords(t *testing.T, target infraBindingTarget) []infraRecoverChoiceRecord {
	t.Helper()
	records, err := readInfraRecoverChoices(target)
	if err != nil {
		t.Fatalf("reading the recover-choices backup: %v", err)
	}
	return records
}

// A reused id whose original the binding still holds cannot be carried under
// that id. Without a choice, recovery names it with both rows and both
// answers, folds neither bead in under the other's identity, and the city
// stays refused, with a refusal that says the binding holds a DIFFERENT bead
// under the id rather than that the id is absent from it.
func TestRecoverStrandedNamesAReusedIDTheBindingStillHolds(t *testing.T) {
	cityPath, cfg, source, target, original, reused := heldReusedIDCity(t)
	id := reused.ID

	var stdout, stderr bytes.Buffer
	if code := runStrandedRecovery(t, cityPath, cfg, &stdout, &stderr); code == 0 {
		t.Errorf("recovery exited 0 with %s unresolved; stdout: %s", id, stdout.String())
	}
	for _, want := range []string{"NOT RECORDED", id, "--" + strandedReplaceBindingRowFlag, "--" + strandedDropWorkRowFlag, reused.Title, original.Title} {
		if !strings.Contains(stdout.String(), want) {
			t.Errorf("recovery output does not name %q; stdout: %s stderr: %s", want, stdout.String(), stderr.String())
		}
	}
	if got, err := openMigratedDestination(t, target).Get(id); err != nil || got.Title != original.Title {
		t.Errorf("binding %s = %q (err %v), want the original %q left in place", id, got.Title, err, original.Title)
	}
	if got := strandedIDs(t, cityPath, target); !slices.Contains(got, id) {
		t.Errorf("stranded = %v; the reused %s must stay stranded until the operator chooses", got, id)
	}

	var log bytes.Buffer
	report := migrateInfraClasses(t, cityPath, cfg, &log)
	if report.Outcome != infraMigrationStranded || !slices.Equal(report.HeldDifferent, []string{id}) {
		t.Fatalf("re-check = %s held-different %v, want stranded naming %s; log: %s", report.Outcome, report.HeldDifferent, id, log.String())
	}
	advice := infraMigrationOperatorAdvice(report, "gc start")
	for _, refusal := range []string{log.String(), advice} {
		for _, want := range []string{"DIFFERENT bead", "--" + strandedReplaceBindingRowFlag, "--" + strandedDropWorkRowFlag} {
			if !strings.Contains(refusal, want) {
				t.Errorf("the refusal does not say %q: %s", want, refusal)
			}
		}
		if strings.Contains(refusal, "NOT in the converged binding") {
			t.Errorf("the refusal claims the id is absent from the binding, which holds a different bead under it: %s", refusal)
		}
	}
	if _, err := source.Get(id); err != nil {
		t.Errorf("the work-store row %s went missing without a choice: %v", id, err)
	}
}

// --replace-binding-row: the binding's row is backed up and removed, with its
// edges, and the work-store row takes the id. The city then clears.
func TestRecoverReplacesTheBindingRowOnRequest(t *testing.T) {
	cityPath, cfg, source, target, original, reused := heldReusedIDCity(t)
	id := reused.ID
	sessionID := fixtureSessionID(t, source)
	dest := openMigratedDestination(t, target)
	if err := dest.DepAdd(id, sessionID, "related"); err != nil {
		t.Fatalf("giving the binding's original an edge: %v", err)
	}
	_ = closeBeadStoreHandle(dest)
	if err := source.DepAdd(id, sessionID, "blocks"); err != nil {
		t.Fatalf("giving the reused work row an edge: %v", err)
	}

	code, stdout, stderr := runStrandedRecoveryWith(t, cityPath, cfg, strandedRecoveryOptions{ReplaceBindingRow: map[string]bool{id: true}})
	if code != 0 {
		t.Fatalf("recover --%s %s exited %d: %s %s", strandedReplaceBindingRowFlag, id, code, stdout, stderr)
	}
	binding := openMigratedDestination(t, target)
	got, err := binding.Get(id)
	if err != nil || got.Title != reused.Title {
		t.Fatalf("binding %s = %q (err %v), want the work-store row %q", id, got.Title, err, reused.Title)
	}
	deps, err := binding.DepList(id, "down")
	if err != nil {
		t.Fatal(err)
	}
	if len(deps) != 1 || deps[0].DependsOnID != sessionID || deps[0].Type != "blocks" {
		t.Errorf("binding %s edges = %+v, want exactly the work-store row's blocks edge to %s (the original's related edge goes with it)", id, deps, sessionID)
	}
	manifest, _, err := readInfraCopyManifestRecord(target)
	if err != nil {
		t.Fatal(err)
	}
	if !sameCreatedSecond(manifest.Created[id], reused.CreatedAt) {
		t.Errorf("manifest created_at for %s = %s, want the replacing row's %s", id, manifest.Created[id], reused.CreatedAt)
	}
	records := recoverChoiceRecords(t, target)
	if len(records) != 1 || records[0].Store != infraRecoverChoiceStoreBinding || records[0].Choice != strandedReplaceBindingRowFlag ||
		records[0].Entry.Bead.Title != original.Title || len(records[0].Entry.Deps) != 1 {
		t.Errorf("backup = %+v, want the binding's original %q with its one edge", records, original.Title)
	}
	if got := strandedIDs(t, cityPath, target); len(got) != 0 {
		t.Errorf("still stranded after the replace: %v", got)
	}
	assertClearProceeds(t, cityPath, target)
	if _, err := source.Get(id); err == nil {
		t.Errorf("the clear left the carried work-store copy %s", id)
	}
	if code, so, se := runStrandedRecoveryWith(t, cityPath, cfg, strandedRecoveryOptions{ReplaceBindingRow: map[string]bool{id: true}}); code != 0 {
		t.Errorf("re-running the same replace = %d, want a no-op: %s %s", code, so, se)
	}
}

// --drop-work-row: the work-store row is backed up and removed, with its
// edges; the binding's row and the manifest are untouched. The city then
// clears, and a re-run with the same choice is a no-op.
func TestRecoverDropsTheWorkRowOnRequest(t *testing.T) {
	cityPath, cfg, source, target, original, reused := heldReusedIDCity(t)
	id := reused.ID
	sessionID := fixtureSessionID(t, source)
	if err := source.DepAdd(id, sessionID, "blocks"); err != nil {
		t.Fatalf("giving the reused work row an edge: %v", err)
	}
	before, _, err := readInfraCopyManifestRecord(target)
	if err != nil {
		t.Fatal(err)
	}

	code, stdout, stderr := runStrandedRecoveryWith(t, cityPath, cfg, strandedRecoveryOptions{DropWorkRow: map[string]bool{id: true}})
	if code != 0 {
		t.Fatalf("recover --%s %s exited %d: %s %s", strandedDropWorkRowFlag, id, code, stdout, stderr)
	}
	if _, err := source.Get(id); !errors.Is(err, beads.ErrNotFound) {
		t.Errorf("the work-store row %s is still there (err %v)", id, err)
	}
	binding := openMigratedDestination(t, target)
	if got, err := binding.Get(id); err != nil || got.Title != original.Title {
		t.Errorf("binding %s = %q (err %v), want the original %q kept", id, got.Title, err, original.Title)
	}
	if deps, err := binding.DepList(id, "down"); err != nil || len(deps) != 0 {
		t.Errorf("binding %s edges = %+v (err %v); the dropped row's edge was written onto the bead that keeps the id", id, deps, err)
	}
	after, _, err := readInfraCopyManifestRecord(target)
	if err != nil {
		t.Fatal(err)
	}
	if !after.Created[id].Equal(before.Created[id]) {
		t.Errorf("manifest created_at for %s moved from %s to %s; the binding's original still holds the id", id, before.Created[id], after.Created[id])
	}
	records := recoverChoiceRecords(t, target)
	if len(records) != 1 || records[0].Store != infraRecoverChoiceStoreWork || records[0].Choice != strandedDropWorkRowFlag ||
		records[0].Entry.Bead.Title != reused.Title || len(records[0].Entry.Deps) != 1 {
		t.Errorf("backup = %+v, want the work-store row %q with its one edge", records, reused.Title)
	}
	if got := strandedIDs(t, cityPath, target); len(got) != 0 {
		t.Errorf("still stranded after the drop: %v", got)
	}
	assertClearProceeds(t, cityPath, target)
	code, stdout, _ = runStrandedRecoveryWith(t, cityPath, cfg, strandedRecoveryOptions{DropWorkRow: map[string]bool{id: true}})
	if code != 0 || !strings.Contains(stdout, "already resolved") {
		t.Errorf("re-running the same drop = %d, want a no-op with a note: %s", code, stdout)
	}
}

// The edges of a work-store row the binding holds a different bead for are
// not written onto that bead while the choice is pending, and --dry-run shows
// both rows and the answers while writing nothing.
func TestRecoverWritesNothingOntoADifferentBeadUnderAReusedID(t *testing.T) {
	cityPath, cfg, source, target, original, reused := heldReusedIDCity(t)
	id := reused.ID
	sessionID := fixtureSessionID(t, source)
	if err := source.DepAdd(id, sessionID, "blocks"); err != nil {
		t.Fatal(err)
	}

	code, stdout, stderr := runStrandedRecoveryWith(t, cityPath, cfg, strandedRecoveryOptions{DryRun: true, ReplaceBindingRow: map[string]bool{id: true}})
	if code != 0 || !strings.Contains(stdout, "would replace the binding's row under "+id) {
		t.Errorf("dry-run replace = %d: %s %s", code, stdout, stderr)
	}
	code, stdout, _ = runStrandedRecoveryWith(t, cityPath, cfg, strandedRecoveryOptions{DryRun: true})
	if code == 0 {
		t.Errorf("a dry run with %s awaiting a choice exited 0: %s", id, stdout)
	}
	for _, want := range []string{"awaiting a choice", "work:", "binding:", reused.Title, original.Title} {
		if !strings.Contains(stdout, want) {
			t.Errorf("the dry run does not show %q: %s", want, stdout)
		}
	}
	if _, err := os.Stat(target.RecoverChoicesBackupPath()); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a dry run wrote the recover-choices backup (stat err %v)", err)
	}

	runStrandedRecoveryWith(t, cityPath, cfg, strandedRecoveryOptions{})
	binding := openMigratedDestination(t, target)
	if deps, err := binding.DepList(id, "down"); err != nil || len(deps) != 0 {
		t.Errorf("binding %s edges = %+v (err %v); the work-store row's edge was written onto the binding's different bead", id, deps, err)
	}
	if got, err := binding.Get(id); err != nil || got.Title != original.Title {
		t.Errorf("binding %s = %q (err %v), want the original untouched", id, got.Title, err)
	}
}

// A held-reused choice names only a row it answers for, and not both answers.
// Each refusal writes nothing.
func TestRecoverRefusesAHeldChoiceForARowThatNeedsNone(t *testing.T) {
	cityPath, cfg, source, target, _, reused := heldReusedIDCity(t)
	id := reused.ID
	before, _ := os.ReadFile(target.ManifestPath())
	sessionID := fixtureSessionID(t, source)
	cases := []struct {
		name string
		opts strandedRecoveryOptions
		want string
	}{
		{"unknown id", strandedRecoveryOptions{DropWorkRow: map[string]bool{"gc-nope": true}}, "gc-nope"},
		{"drop a row the binding carried", strandedRecoveryOptions{DropWorkRow: map[string]bool{sessionID: true}}, sessionID},
		{"legacy flag on a held row", strandedRecoveryOptions{Move: map[string]bool{id: true}}, id},
		{"both answers for one row", strandedRecoveryOptions{ReplaceBindingRow: map[string]bool{id: true}, DropWorkRow: map[string]bool{id: true}}, "choose one"},
	}
	for _, tc := range cases {
		code, stdout, stderr := runStrandedRecoveryWith(t, cityPath, cfg, tc.opts)
		if code == 0 || !strings.Contains(stderr, tc.want) {
			t.Errorf("%s: recover = %d, want a refusal naming %q: %s %s", tc.name, code, tc.want, stdout, stderr)
		}
	}
	if after, _ := os.ReadFile(target.ManifestPath()); !bytes.Equal(before, after) {
		t.Error("a refused recover rewrote the manifest")
	}
	if _, err := os.Stat(target.RecoverChoicesBackupPath()); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a refused recover wrote the recover-choices backup (stat err %v)", err)
	}
	if _, err := source.Get(id); err != nil {
		t.Errorf("a refused recover removed the work-store row %s: %v", id, err)
	}
}

// The classification table, independent of any store.
func TestInfraContainmentClassifyTable(t *testing.T) {
	written := time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC)
	before := written.Add(-24 * time.Hour)
	after := written.Add(5 * 24 * time.Hour)
	keyed := infraCopyManifest{
		IDs:         map[string]bool{"mc-wisp-jkl": true, "mc-1": true, "mc-legacy": true, "mc-none": true},
		Created:     map[string]time.Time{"mc-wisp-jkl": before, "mc-1": before, "mc-none": {}},
		WrittenAt:   written,
		LegacyBound: written,
	}
	unboundedManifest := infraCopyManifest{IDs: map[string]bool{"mc-legacy": true}, WrittenAt: written}
	// "by occupant": the manifest does not decide, and the binding's row under
	// the id does (judgeByOccupant) — every held row it does not record as
	// carried, whatever the row's age.
	cases := []struct {
		name       string
		manifest   infraCopyManifest
		bead       beads.Bead
		held       bool
		want       string
		removed    int
		unrecorded bool
	}{
		{"carried, held", keyed, beads.Bead{ID: "mc-1", CreatedAt: before}, true, "retained", 0, false},
		{"carried, collected", keyed, beads.Bead{ID: "mc-1", CreatedAt: before}, false, "retained", 1, false},
		{"reused id, original held", keyed, beads.Bead{ID: "mc-wisp-jkl", CreatedAt: after}, true, "by occupant", 0, false},
		{"reused id, original collected", keyed, beads.Bead{ID: "mc-wisp-jkl", CreatedAt: after}, false, "stranded", 0, false},
		{"legacy entry, older row", keyed, beads.Bead{ID: "mc-legacy", CreatedAt: before}, false, "retained", 1, false},
		{"legacy entry, younger row", keyed, beads.Bead{ID: "mc-legacy", CreatedAt: after}, true, "by occupant", 0, false},
		// Presumed reused: stranded, and never deleted on the bare id.
		{"legacy entry, younger row, collected", keyed, beads.Bead{ID: "mc-legacy", CreatedAt: after}, false, "stranded", 0, false},
		// The bound cannot place a row with no created_at.
		{"legacy entry, no created_at, held", keyed, beads.Bead{ID: "mc-legacy"}, true, "by occupant", 0, false},
		{"legacy entry, no created_at, collected", keyed, beads.Bead{ID: "mc-legacy"}, false, "retained", 1, true},
		{"recorded with no created_at, a row with none", keyed, beads.Bead{ID: "mc-none"}, false, "retained", 1, false},
		{"recorded with no created_at, a row with one", keyed, beads.Bead{ID: "mc-none", CreatedAt: after}, false, "stranded", 0, false},
		{"unlisted, younger, held", keyed, beads.Bead{ID: "mc-9", CreatedAt: after}, true, "by occupant", 0, false},
		{"unlisted, older, held", keyed, beads.Bead{ID: "mc-9", CreatedAt: before}, true, "by occupant", 0, false},
		{"unlisted, no created_at, held", keyed, beads.Bead{ID: "mc-9"}, true, "by occupant", 0, false},
		{"unlisted, older, not held", keyed, beads.Bead{ID: "mc-9", CreatedAt: before}, false, "stranded", 0, false},
		{"unlisted, no created_at, not held", keyed, beads.Bead{ID: "mc-9"}, false, "stranded", 0, false},
		{"unbounded legacy entry, held", unboundedManifest, beads.Bead{ID: "mc-legacy", CreatedAt: after}, true, "by occupant", 0, false},
		// The id-only reading, named unrecorded: the clear refuses it.
		{"unbounded legacy entry, collected", unboundedManifest, beads.Bead{ID: "mc-legacy", CreatedAt: after}, false, "retained", 1, true},
	}
	b := beads.Bead{ID: "mc-legacy", CreatedAt: after}
	if got := unboundedManifest.Match(b); got != infraManifestLegacyUnbounded {
		t.Fatalf("Match = %d, want unbounded", got)
	}
	if unboundedManifest.Carried(b) {
		t.Error("an unbounded legacy entry reads as carried for a deletion")
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var gap infraContainmentGap
			byOccupant := gap.classify(tc.bead, tc.held, tc.manifest)
			got := "none"
			switch {
			case byOccupant && len(gap.Stranded)+len(gap.Retained)+len(gap.Unrecorded) == 0:
				got = "by occupant"
			case byOccupant:
			case len(gap.Stranded) == 1 && len(gap.Retained) == 0:
				got = "stranded"
			case len(gap.Retained) == 1 && len(gap.Stranded) == 0:
				got = "retained"
			}
			if got != tc.want || gap.RemovedSinceCutover != tc.removed {
				t.Errorf("classify = %s (removed %d), want %s (removed %d)", got, gap.RemovedSinceCutover, tc.want, tc.removed)
			}
			if unrecorded := slices.Contains(gap.Unrecorded, tc.bead.ID); unrecorded != tc.unrecorded {
				t.Errorf("unrecorded = %v, want %v", unrecorded, tc.unrecorded)
			}
		})
	}
}

// A row the binding holds that the manifest does not record as carried is
// filed by the binding's row under its id: this very bead is a retained copy
// recover-stranded must record before the clear removes it, and another bead
// is a strand only a per-id choice resolves.
func TestInfraContainmentJudgeByOccupant(t *testing.T) {
	created := time.Date(2026, 8, 15, 9, 0, 0, 0, time.UTC)
	row := beads.Bead{ID: "mc-9", Title: "mail", Type: "message", Status: "open", CreatedAt: created}
	closedSince := row
	closedSince.Status = "closed"
	// The binding's copy of a row that carries no created_at: the copy's
	// provenance stamp, the binding's insert clock, and served since.
	timeless := beads.Bead{ID: "mc-9", Title: "mail", Type: "message", Status: "open"}
	copiedSince := beads.Bead{
		ID: "mc-9", Title: "mail", Type: "message", Status: "closed", CreatedAt: created.Add(72 * time.Hour),
		Metadata: beads.StringMap{infraMigrationStampKey: config.StorageWorkBinding},
	}
	for _, tc := range []struct {
		name     string
		row      beads.Bead
		occupant beads.Bead
		same     bool
	}{
		{"the same bead, changed since", row, closedSince, true},
		{"the same second, truncated", row, beads.Bead{ID: "mc-9", Title: "mail", Type: "message", CreatedAt: created.Add(400 * time.Millisecond)}, true},
		{"another bead under the id", row, beads.Bead{ID: "mc-9", Title: "mail", Type: "message", CreatedAt: created.Add(-time.Hour)}, false},
		{"no created_at, a copy served since", timeless, copiedSince, true},
		{"no created_at, equal fields no copy wrote", timeless, beads.Bead{ID: "mc-9", Title: "mail", Type: "message", Status: "open", CreatedAt: created}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var gap infraContainmentGap
			gap.judgeByOccupant(tc.row, tc.occupant)
			if tc.same {
				if !slices.Equal(gap.Retained, []string{"mc-9"}) || !slices.Equal(gap.Unrecorded, []string{"mc-9"}) || len(gap.Stranded)+len(gap.HeldDifferent) != 0 {
					t.Errorf("gap = %+v, want a retained, unrecorded copy", gap)
				}
				return
			}
			if !slices.Equal(gap.Stranded, []string{"mc-9"}) || !slices.Equal(gap.HeldDifferent, []string{"mc-9"}) || len(gap.Retained)+len(gap.Unrecorded) != 0 {
				t.Errorf("gap = %+v, want a strand held for a different bead", gap)
			}
		})
	}
}

// An older build reads every non-empty line as an id. The annotated format must
// leave its view intact: every real id present, and nothing it could mistake
// for a real id.
func TestAnnotatedManifestStaysReadableByTheLegacyParser(t *testing.T) {
	target := infraBindingTarget{Root: t.TempDir()}
	ids := []string{"mc-1", "mc-2", "mc-wisp-jkl"}
	created := map[string]time.Time{"mc-1": time.Date(2026, 8, 1, 9, 30, 0, 0, time.UTC), "mc-2": {}}
	if err := writeInfraCopyManifestRecord(target, ids, created, coordclass.CurrentGeneration); err != nil {
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
	if got.Len() != 3 || !got.Created["mc-1"].Equal(created["mc-1"]) || got.WrittenAt.IsZero() {
		t.Errorf("read back %+v", got)
	}
	if at, ok := got.Created["mc-2"]; !ok || !at.IsZero() {
		t.Errorf("an id recorded with no created_at read back as %s (recorded %v)", at, ok)
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
// appends later is judged (presumed reused when younger, carried when older)
// instead of unbounded.
func TestAnnotatedWriteAlwaysBoundsLaterBareIDs(t *testing.T) {
	target := infraBindingTarget{Root: t.TempDir()}
	created := time.Now().Add(-time.Hour)
	before := time.Now()
	if err := writeInfraCopyManifestRecord(target, []string{"mc-1"}, map[string]time.Time{"mc-1": created}, coordclass.CurrentGeneration); err != nil {
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
	if got := m.Match(beads.Bead{ID: "mc-old", CreatedAt: time.Now().Add(time.Hour)}); got != infraManifestPresumedReused {
		t.Errorf("a younger bare id = %d, want presumed reused", got)
	}
}

// Mutant N33: the clock slack applies to the legacy bound: a row created
// within the slack after it still reads as the bead its bare id names.
func TestLegacyBoundHonorsTheClockSlack(t *testing.T) {
	bound := time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC)
	m := infraCopyManifest{IDs: map[string]bool{"x": true}, LegacyBound: bound}
	if got := m.Match(beads.Bead{ID: "x", CreatedAt: bound.Add(infraManifestClockSlack / 2)}); got != infraManifestCarried {
		t.Errorf("a row inside the clock slack = %d, want carried (%d)", got, infraManifestCarried)
	}
	if got := m.Match(beads.Bead{ID: "x", CreatedAt: bound.Add(2 * infraManifestClockSlack)}); got != infraManifestPresumedReused {
		t.Errorf("a row past the clock slack = %d, want presumed reused (%d)", got, infraManifestPresumedReused)
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

// The classifier generation (infra_copy_manifest.go). An earlier build's
// classifier called a startup-health episode and an own-session wait work, so
// its copy neither carried nor listed them, and they are still in the work
// store of every city that build migrated.

// reclassifiedShapes are the shapes coordclass.GenerationOwnedSessions moved
// off work.
func reclassifiedShapes() []beads.Bead {
	return []beads.Bead{
		{Title: "Startup health: worker-1", Type: "startup-health-episode", Metadata: map[string]string{"startup_health_session_name": "worker-1"}},
		{Title: "durable wait", Type: "gate", Labels: []string{"session:s-1"}, Metadata: map[string]string{"session_id": "s-1"}},
	}
}

func TestManifestRecordsItsClassifierGeneration(t *testing.T) {
	target := infraBindingTarget{Root: t.TempDir()}
	read := func() (infraCopyManifest, error) {
		manifest, _, err := readInfraCopyManifestRecord(target)
		return manifest, err
	}
	if err := writeInfraCopyManifest(target, nil); err != nil {
		t.Fatal(err)
	}
	if got, err := read(); err != nil || got.Generation != coordclass.CurrentGeneration {
		t.Errorf("a genesis manifest reads generation %d (err %v), want the current %d", got.Generation, err, coordclass.CurrentGeneration)
	}
	if err := writeInfraCopyManifestRecord(target, []string{"mc-1"}, nil, coordclass.GenerationLegacy); err != nil {
		t.Fatal(err)
	}
	if got, err := read(); err != nil || got.Generation != coordclass.GenerationLegacy {
		t.Errorf("a rewrite of a legacy-generation manifest reads %d (err %v), want it carried through as %d", got.Generation, err, coordclass.GenerationLegacy)
	}
	if err := os.WriteFile(target.ManifestPath(), []byte("mc-1\n#gc:written 2026-10-26T12:00:00Z\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got, err := read(); err != nil || got.Generation != coordclass.GenerationLegacy {
		t.Errorf("a manifest that records no generation reads %d (err %v), want %d: its build predates generations", got.Generation, err, coordclass.GenerationLegacy)
	}
	for _, bad := range []string{"two", "0", "-1"} {
		if err := os.WriteFile(target.ManifestPath(), []byte("mc-1\n"+infraManifestGenerationPrefix+bad+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := read(); err == nil {
			t.Errorf("a generation line %q read without an error", bad)
		}
	}
}

// Upgrading a city an earlier build migrated must not strand the rows that
// build's copy left as work: boot would refuse, and every seat's drain fence
// would fail closed, over rows no writer stranded. Recover-stranded must not
// carry them either (they are frozen pre-cutover session state), and the
// clear must neither refuse over them nor delete them. A real strand beside
// them makes recover-stranded rewrite the manifest, which must keep the
// generation it read.
func TestAnEarlierCopysReclassifiedRowsAreLeftWhereItLeftThem(t *testing.T) {
	for _, plant := range reclassifiedShapes() {
		t.Run(plant.Type, func(t *testing.T) {
			cityPath, cfg, source, target := convergedRecoveryCity(t)
			writeLegacyManifest(t, target)
			planted := mustCreateInfraBead(t, source, plant)
			strand := mustCreateInfraBead(t, source, beads.Bead{Title: "auto-handoff", Type: "message"})

			if got := strandedIDs(t, cityPath, target); slices.Contains(got, planted.ID) || !slices.Contains(got, strand.ID) {
				t.Errorf("stranded = %v, want %s and not %s: the earlier copy's classifier left %s as work", got, strand.ID, planted.ID, planted.ID)
			}
			if code, stdout, stderr := runStrandedRecoveryWith(t, cityPath, cfg, strandedRecoveryOptions{}); code != 0 {
				t.Fatalf("recover-stranded exited %d: %s %s", code, stdout, stderr)
			}
			if _, err := openMigratedDestination(t, target).Get(strand.ID); err != nil {
				t.Errorf("recover-stranded did not move the real strand %s: %v", strand.ID, err)
			}
			if _, err := openMigratedDestination(t, target).Get(planted.ID); err == nil {
				t.Errorf("recover-stranded carried %s, frozen pre-cutover session state, into the binding", planted.ID)
			}
			manifest, _, err := readInfraCopyManifestRecord(target)
			if err != nil {
				t.Fatal(err)
			}
			if manifest.Generation != coordclass.GenerationLegacy {
				t.Errorf("recover-stranded rewrote the manifest at generation %d, want the %d it read: the next check would strand %s", manifest.Generation, coordclass.GenerationLegacy, planted.ID)
			}
			assertClearProceeds(t, cityPath, target)
			if _, err := source.Get(planted.ID); err != nil {
				t.Errorf("the clear removed %s, a row the copy never carried: %v", planted.ID, err)
			}
		})
	}
}

// The control: on a city this build migrated, the same shape written to the
// work store after the copy IS a strand. The generation exempts only what an
// earlier copy's classifier excluded.
func TestACurrentCopyStrandsAReclassifiedShapeWrittenAfterIt(t *testing.T) {
	for _, plant := range reclassifiedShapes() {
		t.Run(plant.Type, func(t *testing.T) {
			cityPath, _, source, target := convergedRecoveryCity(t)
			planted := mustCreateInfraBead(t, source, plant)
			if got := strandedIDs(t, cityPath, target); !slices.Contains(got, planted.ID) {
				t.Errorf("stranded = %v, want %s: this build's copy carried every %s, so one written after it is a strand", got, planted.ID, plant.Type)
			}
		})
	}
}

// A re-copy from the clear's backup covers the rows the backup holds, which
// the replaced manifest's generation selected. Recording the current
// generation would strand the episode the clear left in the work store.
func TestRecopyFromTheBackupKeepsTheReplacedManifestsGeneration(t *testing.T) {
	cityPath, _, source, target := convergedRecoveryCity(t)
	writeLegacyManifest(t, target)
	planted := mustCreateInfraBead(t, source, reclassifiedShapes()[0])
	assertClearProceeds(t, cityPath, target)
	if err := os.RemoveAll(target.Dir); err != nil {
		t.Fatal(err)
	}

	var log bytes.Buffer
	if got := runInfraClassMigrationFrom(cityPath, target, infraMigrationFromBackup, "gc storage migrate", &log); got.Outcome != infraMigrationConverged {
		t.Fatalf("backup re-copy = %s, want converged: %s", got.Outcome, log.String())
	}
	manifest, _, err := readInfraCopyManifestRecord(target)
	if err != nil {
		t.Fatal(err)
	}
	if manifest.Generation != coordclass.GenerationLegacy {
		t.Errorf("the re-copied manifest records generation %d, want the replaced manifest's %d", manifest.Generation, coordclass.GenerationLegacy)
	}
	if got := strandedIDs(t, cityPath, target); slices.Contains(got, planted.ID) {
		t.Errorf("stranded = %v; the re-copy turned the row the clear left, %s, into a strand", got, planted.ID)
	}
}

// appendBareManifestID appends id to the manifest as one bare line: what an
// older build's recover-stranded superset rewrite adds for a row it proved.
func appendBareManifestID(t *testing.T, target infraBindingTarget, id string) {
	t.Helper()
	contents, err := os.ReadFile(target.ManifestPath())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target.ManifestPath(), append(contents, []byte(id+"\n")...), 0o644); err != nil {
		t.Fatal(err)
	}
}

// Review probe (iteration 2): an older build's recover-stranded carried a
// post-cutover mail into the binding verbatim and appended its bare id to the
// legacy manifest, and the binding has since closed the mail. The work-store
// row is the same bead — same id, created in the same second — so it is not a
// reused id: containment retains it rather than offering the per-id choices,
// recover-stranded records it with its created_at, the clear removes the
// frozen work-store copy, and the binding's closed row is left as it is.
//
// Red-before, on field equality: recover-stranded exits 1 with
// "NOT RECORDED: gc-4 (status "open" != "closed")" and calls it a different bead.
func TestRecoverRecordsABeadTheBindingChangedSinceAnOlderRecoverCarriedIt(t *testing.T) {
	prevSlack := infraManifestClockSlack
	infraManifestClockSlack = 0
	t.Cleanup(func() { infraManifestClockSlack = prevSlack })

	cityPath, cfg, source, target := convergedRecoveryCity(t)
	writeLegacyManifest(t, target)
	waitPastMarkerBound(t, target)
	mail := mustCreateInfraBead(t, source, beads.Bead{Title: "a mail written after the cutover", Type: "message"})
	dest, err := openInfraDestination(target)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := dest.(beads.ForeignIDCreator).CreateWithForeignID(infraMigrationRow(mail)); err != nil {
		t.Fatalf("carrying %s the way an older recover-stranded did: %v", mail.ID, err)
	}
	appendBareManifestID(t, target, mail.ID)
	if err := dest.Close(mail.ID); err != nil {
		t.Fatalf("closing the binding's copy of %s: %v", mail.ID, err)
	}
	if err := closeBeadStoreHandle(dest); err != nil {
		t.Fatal(err)
	}

	manifest, _, err := readInfraCopyManifestRecord(target)
	if err != nil {
		t.Fatal(err)
	}
	if got := manifest.Match(mail); got != infraManifestPresumedReused {
		t.Fatalf("precondition: Match = %d, want the bare id younger than the legacy bound (presumed reused)", got)
	}
	gap, err := classifyInfraContainmentGap(cityPath, target, manifest)
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(gap.Stranded, mail.ID) || slices.Contains(gap.HeldDifferent, mail.ID) || !slices.Contains(gap.Retained, mail.ID) {
		t.Errorf("containment = stranded %v held-different %v retained %v; the binding holds %s itself, so it is a retained copy", gap.Stranded, gap.HeldDifferent, gap.Retained, mail.ID)
	}

	code, stdout, stderr := runStrandedRecoveryWith(t, cityPath, cfg, strandedRecoveryOptions{})
	if code != 0 {
		t.Fatalf("recover-stranded exited %d: %s %s", code, stdout, stderr)
	}
	recorded, _, err := readInfraCopyManifestRecord(target)
	if err != nil {
		t.Fatal(err)
	}
	if !sameCreatedSecond(recorded.Created[mail.ID], mail.CreatedAt) {
		t.Errorf("manifest created_at for %s = %s, want the bead's %s", mail.ID, recorded.Created[mail.ID], mail.CreatedAt)
	}
	assertClearProceeds(t, cityPath, target)
	if _, err := source.Get(mail.ID); !errors.Is(err, beads.ErrNotFound) {
		t.Errorf("the clear left the frozen work-store copy of %s (err %v)", mail.ID, err)
	}
	if got, err := openMigratedDestination(t, target).Get(mail.ID); err != nil || got.Status != "closed" {
		t.Errorf("binding %s = status %q (err %v), want the binding's own closed row untouched", mail.ID, got.Status, err)
	}
}

// Review probe (iteration 2, codex): a recover-stranded run that leaves a row
// awaiting a per-id choice still rewrites the manifest when it recovers another
// row. The rewrite carries #gc:written unchanged, and the row it left, under an
// id the binding holds for a different bead, stays a strand that names the
// choice in boot and status.
func TestRecoverRewriteKeepsAnUnresolvedHeldRowStranded(t *testing.T) {
	cityPath, cfg, source, target := convergedRecoveryCity(t)
	before, _, err := readInfraCopyManifestRecord(target)
	if err != nil {
		t.Fatal(err)
	}
	waiting := mustCreateInfraBead(t, source, beads.Bead{Title: "a mail written after the cutover", Type: "message"})
	dest, err := openInfraDestination(target)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := dest.(beads.ForeignIDCreator).CreateWithForeignID(beads.Bead{
		ID: waiting.ID, Title: "an older bead the binding holds under the id", Type: "message", CreatedAt: waiting.CreatedAt.Add(-time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	if err := closeBeadStoreHandle(dest); err != nil {
		t.Fatal(err)
	}
	other := mustCreateInfraBead(t, source, beads.Bead{Title: "another stranded mail", Type: "message"})

	code, stdout, stderr := runStrandedRecoveryWith(t, cityPath, cfg, strandedRecoveryOptions{})
	if code == 0 {
		t.Errorf("recover-stranded exited 0 with %s awaiting a choice: %s", waiting.ID, stdout)
	}
	after, _, err := readInfraCopyManifestRecord(target)
	if err != nil {
		t.Fatal(err)
	}
	if !after.IDs[other.ID] {
		t.Fatalf("the run did not record %s, so it never rewrote the manifest and this proves nothing: %s %s", other.ID, stdout, stderr)
	}
	if !after.WrittenAt.Equal(before.WrittenAt) {
		t.Errorf("the rewrite moved #gc:written from %s to %s", before.WrittenAt, after.WrittenAt)
	}
	var log bytes.Buffer
	report := migrateInfraClasses(t, cityPath, cfg, &log)
	if report.Outcome != infraMigrationStranded || !slices.Equal(report.Stranded, []string{waiting.ID}) || !slices.Equal(report.HeldDifferent, []string{waiting.ID}) {
		t.Errorf("re-check = %s stranded %v held-different %v, want %s stranded and named held for a different bead; log: %s", report.Outcome, report.Stranded, report.HeldDifferent, waiting.ID, log.String())
	}
	if !strings.Contains(infraMigrationOperatorAdvice(report, "gc start"), "--"+strandedDropWorkRowFlag) {
		t.Errorf("the refusal no longer names the per-id choices: %s", infraMigrationOperatorAdvice(report, "gc start"))
	}
}

// Review probe (iteration 2, codex): an entry the manifest keys by created_at
// never matches a row that carries none, so the clear does not delete it on
// the id alone. recover-stranded, which proves the row is the bead the binding
// holds, records it as a bead with no created_at, and the clear then proceeds.
func TestAnnotatedEntryNeverCarriesARowWithNoCreatedAt(t *testing.T) {
	written := time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC)
	m := infraCopyManifest{
		IDs:         map[string]bool{"mc-1": true},
		Created:     map[string]time.Time{"mc-1": written.Add(-time.Hour)},
		WrittenAt:   written,
		LegacyBound: written,
	}
	row := beads.Bead{ID: "mc-1"}
	if got := m.Match(row); got != infraManifestLegacyUnbounded {
		t.Errorf("Match = %d, want unbounded: nothing ties a row with no created_at to the entry", got)
	}
	if m.Carried(row) || !m.ReadsCarried(row) {
		t.Errorf("Carried = %v, ReadsCarried = %v; want a reading no deletion acts on and the id-only read-only reading", m.Carried(row), m.ReadsCarried(row))
	}

	cityPath, cfg, source, target := convergedRecoveryCity(t)
	id := carriedMessageID(t, source)
	rows, err := source.List(beads.ListQuery{IncludeClosed: true, TierMode: beads.TierBoth, AllowScan: true})
	if err != nil {
		t.Fatal(err)
	}
	for i := range rows {
		if rows[i].ID == id {
			rows[i].CreatedAt = time.Time{}
		}
	}
	timeless := beads.NewMemStoreFrom(len(rows), rows, nil)
	prev := openInfraMigrationSource
	openInfraMigrationSource = func(string) (beads.Store, error) { return timeless, nil }
	t.Cleanup(func() { openInfraMigrationSource = prev })

	var announce bytes.Buffer
	if _, err := clearRetainedInfraCopies(cityPath, target, &announce); err == nil {
		t.Errorf("the clear deleted %s, whose row carries no created_at for the manifest's entry to match", id)
	}
	if _, err := timeless.Get(id); err != nil {
		t.Fatalf("the clear removed %s on its id alone: %v", id, err)
	}
	if code, stdout, stderr := runStrandedRecoveryWith(t, cityPath, cfg, strandedRecoveryOptions{}); code != 0 {
		t.Fatalf("recover-stranded exited %d: %s %s", code, stdout, stderr)
	}
	recorded, _, err := readInfraCopyManifestRecord(target)
	if err != nil {
		t.Fatal(err)
	}
	if at, ok := recorded.Created[id]; !ok || !at.IsZero() {
		t.Errorf("recover-stranded recorded %s as %s (recorded %v), want a bead with no created_at rather than a created_at its row carries none of", id, at, ok)
	}
	assertClearProceeds(t, cityPath, target)
	if _, err := timeless.Get(id); !errors.Is(err, beads.ErrNotFound) {
		t.Errorf("the clear left %s after recover-stranded proved it (err %v)", id, err)
	}
}

// A copy records a row that carries no created_at as such (#gc:no-created-at),
// so the clear removes it like any other row the copy proved, as a manifest of
// bare ids always allowed, and the copy leaves no legacy entry: its write time
// bounds the bare ids an older build appends later.
func TestACopyRecordsTheRowsThatCarryNoCreatedAt(t *testing.T) {
	cityPath := t.TempDir()
	source := beads.NewMemStoreFrom(1, []beads.Bead{{ID: "gc-1", Title: "a legacy session", Type: "session", Status: "open", Labels: []string{"gc:session"}}}, nil)
	prev := openInfraMigrationSource
	openInfraMigrationSource = func(string) (beads.Store, error) { return source, nil }
	t.Cleanup(func() { openInfraMigrationSource = prev })
	cfg := infraSplitConfig(filepath.Join(cityPath, ".gc", "store"))

	var log bytes.Buffer
	if got := migrateInfraClasses(t, cityPath, cfg, &log); got.Outcome != infraMigrationConverged {
		t.Fatalf("migrate = %s, want converged: %s", got.Outcome, log.String())
	}
	manifest, _, err := readInfraCopyManifestRecord(mustResolveInfraTarget(t, cityPath, cfg))
	if err != nil {
		t.Fatal(err)
	}
	if at, ok := manifest.Created["gc-1"]; !ok || !at.IsZero() {
		t.Errorf("the copy recorded gc-1 as %s (recorded %v), want a bead with no created_at", at, ok)
	}
	if manifest.LegacyBound.IsZero() {
		t.Error("the copy left the bare ids an older build appends later unbounded")
	}
	if _, err := source.Get("gc-1"); !errors.Is(err, beads.ErrNotFound) {
		t.Errorf("the clear left gc-1 in the work store (err %v)", err)
	}
}

// holdADifferentBeadUnder has the binding hold another bead under id, created
// two hours before at: the original a reused id leaves behind.
func holdADifferentBeadUnder(t *testing.T, target infraBindingTarget, id string, at time.Time) {
	t.Helper()
	dest, err := openInfraDestination(target)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := dest.(beads.ForeignIDCreator).CreateWithForeignID(beads.Bead{
		ID: id, Title: "an older bead the binding holds under the id", Type: "message", CreatedAt: at.Add(-2 * time.Hour),
	}); err != nil {
		t.Fatalf("holding a different bead under %s: %v", id, err)
	}
	if err := closeBeadStoreHandle(dest); err != nil {
		t.Fatal(err)
	}
}

// Review probe (iteration 3, codex): a row the binding holds for a DIFFERENT
// bead is a strand that names the per-id choices, whatever its age. Judged by
// the manifest's write time, a row older than the manifest, or one with no
// created_at, read as a retained copy: boot named the clear, which refuses the
// row, and never the choice recover-stranded asks for it.
func TestContainmentNamesAHeldDifferentRowOfAnyAge(t *testing.T) {
	for _, timeless := range []bool{false, true} {
		t.Run(map[bool]string{false: "older than the manifest", true: "no created_at"}[timeless], func(t *testing.T) {
			cityPath, cfg, source, target := convergedRecoveryCity(t)
			row := mustCreateInfraBead(t, source, beads.Bead{Title: "a mail written after the cutover", Type: "message"})
			holdADifferentBeadUnder(t, target, row.ID, row.CreatedAt)
			manifest, _, err := readInfraCopyManifestRecord(target)
			if err != nil {
				t.Fatal(err)
			}
			manifest.WrittenAt = row.CreatedAt.Add(time.Hour)
			if err := rewriteInfraCopyManifest(target, manifest, manifestIDs(t, target), manifest.Created); err != nil {
				t.Fatal(err)
			}
			if timeless {
				rows, err := source.List(beads.ListQuery{IncludeClosed: true, TierMode: beads.TierBoth, AllowScan: true})
				if err != nil {
					t.Fatal(err)
				}
				for i := range rows {
					if rows[i].ID == row.ID {
						rows[i].CreatedAt = time.Time{}
					}
				}
				store := beads.NewMemStoreFrom(len(rows), rows, nil)
				prev := openInfraMigrationSource
				openInfraMigrationSource = func(string) (beads.Store, error) { return store, nil }
				t.Cleanup(func() { openInfraMigrationSource = prev })
			}
			manifest, _, err = readInfraCopyManifestRecord(target)
			if err != nil {
				t.Fatal(err)
			}
			if row.CreatedAt.After(manifest.WrittenAt) {
				t.Fatalf("precondition: %s (created %s) is younger than the manifest (written %s)", row.ID, row.CreatedAt, manifest.WrittenAt)
			}

			gap, err := classifyInfraContainmentGap(cityPath, target, manifest)
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Contains(gap.Stranded, row.ID) || !slices.Contains(gap.HeldDifferent, row.ID) || slices.Contains(gap.Retained, row.ID) {
				t.Errorf("containment = stranded %v held-different %v retained %v; the binding holds a different bead under %s", gap.Stranded, gap.HeldDifferent, gap.Retained, row.ID)
			}
			var log bytes.Buffer
			report := migrateInfraClasses(t, cityPath, cfg, &log)
			if report.Outcome != infraMigrationStranded || !slices.Equal(report.HeldDifferent, []string{row.ID}) {
				t.Errorf("boot = %s held-different %v, want %s stranded and held for a different bead; log: %s", report.Outcome, report.HeldDifferent, row.ID, log.String())
			}
			advice := infraMigrationOperatorAdvice(report, "gc start")
			for _, flag := range []string{strandedReplaceBindingRowFlag, strandedDropWorkRowFlag} {
				if !strings.Contains(advice, "--"+flag) {
					t.Errorf("the refusal does not name --%s: %s", flag, advice)
				}
			}
			if code, stdout, stderr := runStrandedRecoveryWith(t, cityPath, cfg, strandedRecoveryOptions{DropWorkRow: map[string]bool{row.ID: true}}); code != 0 {
				t.Fatalf("recover-stranded --%s %s exited %d: %s %s", strandedDropWorkRowFlag, row.ID, code, stdout, stderr)
			}
			if got := strandedIDs(t, cityPath, target); len(got) != 0 {
				t.Errorf("stranded after the advised choice = %v, want none", got)
			}
		})
	}
}

// Review probe (iteration 3, claude): the residue of an interrupted
// recover-stranded, a row the binding holds for that very bead that the
// manifest does not record, is a retained copy the clear refuses until
// recover-stranded records it. Boot, `gc storage status` and the migration
// name recover-stranded before the clear, and the advised commands, run in
// that order, converge the city. They named the clear alone, which refused the
// row as one nothing proves the binding holds.
func TestUnrecordedResidueIsNamedForRecoverStrandedBeforeTheClear(t *testing.T) {
	cityPath, cfg, source, target := convergedRecoveryCity(t)
	residue := mustCreateInfraBead(t, source, beads.Bead{Title: "a mail written after the cutover", Type: "message"})
	dest, err := openInfraDestination(target)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := dest.(beads.ForeignIDCreator).CreateWithForeignID(infraMigrationRow(residue)); err != nil {
		t.Fatalf("copying %s the way an interrupted recover-stranded left it: %v", residue.ID, err)
	}
	if err := closeBeadStoreHandle(dest); err != nil {
		t.Fatal(err)
	}
	manifest, _, err := readInfraCopyManifestRecord(target)
	if err != nil {
		t.Fatal(err)
	}
	gap, err := classifyInfraContainmentGap(cityPath, target, manifest)
	if err != nil {
		t.Fatal(err)
	}
	if len(gap.Stranded) != 0 || !slices.Contains(gap.Retained, residue.ID) || !slices.Equal(gap.Unrecorded, []string{residue.ID}) {
		t.Fatalf("containment = stranded %v retained %v unrecorded %v, want %s a retained copy the manifest does not record", gap.Stranded, gap.Retained, gap.Unrecorded, residue.ID)
	}
	recoverFirst := func(surface, text string) {
		t.Helper()
		recoverAt, clearAt := strings.Index(text, storageRecoveryInstruction()), strings.Index(text, storageClearInstruction())
		if recoverAt < 0 || clearAt < 0 || recoverAt > clearAt {
			t.Errorf("%s does not name `%s` before `%s`:\n%s", surface, storageRecoveryInstruction(), storageClearInstruction(), text)
		}
		if !strings.Contains(text, residue.ID) {
			t.Errorf("%s does not name %s:\n%s", surface, residue.ID, text)
		}
	}

	var log bytes.Buffer
	boot := confirmInfraConvergence(cityPath, target, "gc start", &log)
	boot.Target = target
	if boot.Outcome != infraMigrationRetained {
		t.Fatalf("boot = %s, want retained-copies; log: %s", boot.Outcome, log.String())
	}
	recoverFirst("the boot refusal", infraMigrationOperatorAdvice(boot, "gc start"))

	stubInfraControllerPing(t, 0)
	var stdout, stderr bytes.Buffer
	if code := doStorageStatus(storageOperatorRequest{CityPath: cityPath, Cfg: cfg}, &stdout, &stderr); code == 0 {
		t.Fatalf("status exited 0 with a retained copy in the work store: %s", stdout.String())
	}
	recoverFirst("`gc storage status`", stdout.String())

	log.Reset()
	migrated := migrateInfraClasses(t, cityPath, cfg, &log)
	if migrated.Outcome != infraMigrationRetained || !slices.Equal(migrated.Unrecorded, []string{residue.ID}) {
		t.Errorf("migrate = %s unrecorded %v, want retained-copies naming %s; log: %s", migrated.Outcome, migrated.Unrecorded, residue.ID, log.String())
	}
	recoverFirst("the migration's refusal", infraMigrationOperatorAdvice(migrated, "gc storage migrate"))
	if _, err := source.Get(residue.ID); err != nil {
		t.Fatalf("the refused migration removed %s from the work store: %v", residue.ID, err)
	}

	if code, out, errOut := runStrandedRecoveryWith(t, cityPath, cfg, strandedRecoveryOptions{}); code != 0 {
		t.Fatalf("recover-stranded exited %d: %s %s", code, out, errOut)
	}
	log.Reset()
	if got := migrateInfraClasses(t, cityPath, cfg, &log); got.Outcome != infraMigrationConverged {
		t.Fatalf("migrate after recover-stranded = %s, want converged; log: %s", got.Outcome, log.String())
	}
	if _, err := source.Get(residue.ID); !errors.Is(err, beads.ErrNotFound) {
		t.Errorf("the clear left the frozen work-store copy of %s (err %v)", residue.ID, err)
	}
	stdout.Reset()
	stderr.Reset()
	if code := doStorageStatus(storageOperatorRequest{CityPath: cityPath, Cfg: cfg}, &stdout, &stderr); code != 0 {
		t.Errorf("status exited %d after the advised commands: %s %s", code, stdout.String(), stderr.String())
	}
}

// Review probe (iteration 3, codex): a read of the binding's row under a held
// id that fails is an error, not "the same bead". recover-stranded folded it
// into the same bead, so a row held for a different bead lost its per-id
// choice on that run; the containment check refused. Both now refuse, and both
// read a row the binding no longer holds as not held.
func TestAFailedReadOfAHeldRowIsAnErrorNotTheSameBead(t *testing.T) {
	cityPath, cfg, source, target := convergedRecoveryCity(t)
	row := mustCreateInfraBead(t, source, beads.Bead{Title: "a mail written after the cutover", Type: "message"})
	holdADifferentBeadUnder(t, target, row.ID, row.CreatedAt)
	before, err := os.ReadFile(target.ManifestPath())
	if err != nil {
		t.Fatal(err)
	}
	refused := errors.New("binding read refused")
	prev := getInfraHeldRow
	readErr := refused
	getInfraHeldRow = func(destination beads.Store, id string) (beads.Bead, error) {
		if id == row.ID {
			return beads.Bead{}, readErr
		}
		return prev(destination, id)
	}
	t.Cleanup(func() { getInfraHeldRow = prev })

	code, stdout, stderr := runStrandedRecoveryWith(t, cityPath, cfg, strandedRecoveryOptions{})
	if code == 0 {
		t.Fatalf("recover-stranded exited 0 over a failed read of the binding's row under %s: %s", row.ID, stdout)
	}
	if !strings.Contains(stderr, refused.Error()) || !strings.Contains(stderr, row.ID) {
		t.Errorf("stderr does not name the failed read of %s: %s", row.ID, stderr)
	}
	if after, _ := os.ReadFile(target.ManifestPath()); !bytes.Equal(before, after) {
		t.Error("recover-stranded rewrote the manifest after a failed read")
	}
	manifest, _, err := readInfraCopyManifestRecord(target)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := classifyInfraContainmentGap(cityPath, target, manifest); !errors.Is(err, refused) {
		t.Errorf("containment = %v, want the failed read", err)
	}

	readErr = fmt.Errorf("getting bead %q: %w", row.ID, beads.ErrNotFound)
	gap, err := classifyInfraContainmentGap(cityPath, target, manifest)
	if err != nil {
		t.Fatalf("containment refused over a row the binding no longer holds: %v", err)
	}
	if !slices.Equal(gap.Stranded, []string{row.ID}) || len(gap.HeldDifferent) != 0 {
		t.Errorf("containment = stranded %v held-different %v, want %s stranded as a row the binding does not hold", gap.Stranded, gap.HeldDifferent, row.ID)
	}
}

// timelessWorkStore stands a copy of source in for the work store, with the
// rows under ids carrying no created_at, as a row written before created_at
// existed does. Every store's create stamps one, so no fixture can mint such a
// row.
func timelessWorkStore(t *testing.T, source beads.Store, ids ...string) beads.Store {
	t.Helper()
	rows, err := source.List(beads.ListQuery{IncludeClosed: true, TierMode: beads.TierBoth, AllowScan: true})
	if err != nil {
		t.Fatal(err)
	}
	timeless := setOf(ids)
	for i := range rows {
		if timeless[rows[i].ID] {
			rows[i].CreatedAt = time.Time{}
		}
	}
	store := beads.NewMemStoreFrom(len(rows), rows, nil)
	prev := openInfraMigrationSource
	openInfraMigrationSource = func(string) (beads.Store, error) { return store, nil }
	t.Cleanup(func() { openInfraMigrationSource = prev })
	return store
}

// holdACopyServedSince has the binding hold, under row's id, the row a copy
// out of the work store writes (infraMigrationRow: the provenance stamp, and
// the binding's insert clock for a created_at row carries none of), in place
// of any row it held there, and then closes it, as a binding that has served
// the bead since does.
func holdACopyServedSince(t *testing.T, target infraBindingTarget, row beads.Bead) {
	t.Helper()
	dest, err := openInfraDestination(target)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := closeBeadStoreHandle(dest); err != nil {
			t.Errorf("closing the binding: %v", err)
		}
	}()
	if err := dest.Delete(row.ID); err != nil && !errors.Is(err, beads.ErrNotFound) {
		t.Fatalf("removing the binding's row under %s: %v", row.ID, err)
	}
	if _, err := dest.(beads.ForeignIDCreator).CreateWithForeignID(infraMigrationRow(row)); err != nil {
		t.Fatalf("copying %s into the binding: %v", row.ID, err)
	}
	if err := dest.Close(row.ID); err != nil {
		t.Fatalf("closing the binding's copy of %s: %v", row.ID, err)
	}
	held, err := dest.Get(row.ID)
	if err != nil {
		t.Fatal(err)
	}
	if held.CreatedAt.IsZero() || held.Metadata[infraMigrationStampKey] == "" {
		t.Fatalf("precondition: the binding's copy of %s = created %s, stamp %q; want the insert clock and the copy's stamp", row.ID, held.CreatedAt, held.Metadata[infraMigrationStampKey])
	}
}

// Review probe (iteration 4, codex): a work-store row with no created_at under
// an id whose binding row a copy wrote. The binding's insert stamped its own
// clock where the row carried none, and the binding has closed the bead since,
// so the two rows share no identity key and differ in their fields. The copy's
// provenance stamp is the proof: no bead is minted with no created_at, so the
// copy under its id was made from this very row. Containment retains it rather
// than naming it held for a different bead (whose --replace-binding-row would
// delete the live row), recover-stranded records it as a bead with no
// created_at, which reads as carried whatever the legacy bound, and the clear
// then converges the city.
//
// Red before: the residue and the unbounded entry were named held for a
// different bead, and the bounded entry read as carried on its bare id alone,
// with no look at the binding's row.
func TestARowWithNoCreatedAtIsTheBeadItsCopyStamped(t *testing.T) {
	for _, shape := range []string{"unrecorded residue", "unbounded legacy entry", "bounded legacy entry"} {
		t.Run(shape, func(t *testing.T) {
			cityPath, cfg, source, target := convergedRecoveryCity(t)
			var id string
			switch shape {
			case "unrecorded residue":
				id = mustCreateInfraBead(t, source, beads.Bead{Title: "a mail written after the cutover", Type: "message"}).ID
			case "unbounded legacy entry":
				id = carriedMessageID(t, source)
				writeLegacyManifest(t, target)
				if err := os.WriteFile(target.MarkerPath(), []byte("infrastructure classes migrated\n"), 0o644); err != nil {
					t.Fatal(err)
				}
			case "bounded legacy entry":
				id = carriedMessageID(t, source)
				writeLegacyManifest(t, target)
			}
			work := timelessWorkStore(t, source, id)
			row, err := work.Get(id)
			if err != nil {
				t.Fatal(err)
			}
			holdACopyServedSince(t, target, row)

			manifest, _, err := readInfraCopyManifestRecord(target)
			if err != nil {
				t.Fatal(err)
			}
			gap, err := classifyInfraContainmentGap(cityPath, target, manifest)
			if err != nil {
				t.Fatal(err)
			}
			if slices.Contains(gap.Stranded, id) || slices.Contains(gap.HeldDifferent, id) || !slices.Contains(gap.Retained, id) || !slices.Contains(gap.Unrecorded, id) {
				t.Fatalf("containment = stranded %v held-different %v retained %v unrecorded %v; the binding holds the copy of %s, so it is a retained copy recover-stranded records", gap.Stranded, gap.HeldDifferent, gap.Retained, gap.Unrecorded, id)
			}

			code, stdout, stderr := runStrandedRecoveryWith(t, cityPath, cfg, strandedRecoveryOptions{})
			if code != 0 {
				t.Fatalf("recover-stranded exited %d: %s %s", code, stdout, stderr)
			}
			if strings.Contains(stdout+stderr, "--"+strandedReplaceBindingRowFlag) {
				t.Errorf("recover-stranded offered --%s for the binding's copy of %s: %s %s", strandedReplaceBindingRowFlag, id, stdout, stderr)
			}
			recorded, _, err := readInfraCopyManifestRecord(target)
			if err != nil {
				t.Fatal(err)
			}
			if at, ok := recorded.Created[id]; !ok || !at.IsZero() {
				t.Errorf("manifest entry for %s = %s (recorded %v), want it recorded as a bead with no created_at", id, at, ok)
			}
			if got := recorded.Match(row); got != infraManifestCarried {
				t.Errorf("Match(%s) = %d after recover-stranded, want carried (%d)", id, got, infraManifestCarried)
			}

			var log bytes.Buffer
			if got := migrateInfraClasses(t, cityPath, cfg, &log); got.Outcome != infraMigrationConverged {
				t.Fatalf("migrate after recover-stranded = %s, want converged; log: %s", got.Outcome, log.String())
			}
			if _, err := work.Get(id); !errors.Is(err, beads.ErrNotFound) {
				t.Errorf("the clear left the work-store row %s (err %v)", id, err)
			}
			if got, err := openMigratedDestination(t, target).Get(id); err != nil || got.Status != "closed" {
				t.Errorf("binding %s = status %q (err %v), want the binding's own closed row untouched", id, got.Status, err)
			}
			var statusOut, statusErr bytes.Buffer
			if code := doStorageStatus(storageOperatorRequest{CityPath: cityPath, Cfg: cfg}, &statusOut, &statusErr); code != 0 {
				t.Errorf("status exited %d: %s %s", code, statusOut.String(), statusErr.String())
			}
		})
	}
}

// Review probe (iteration 4, codex): a bare legacy id does not carry a row with
// no created_at on its own, because the legacy bound cannot place such a row.
// Read as carried, containment never looked at the binding's row under the id,
// and the clear deleted the work-store row even where the binding holds a bead
// no copy wrote, so the only copy of the row was lost. The binding's row now
// decides: one no copy wrote is a different bead, named with the per-id
// choices, and the clear refuses.
func TestABareIDDoesNotCarryARowWithNoCreatedAtOnItsOwn(t *testing.T) {
	cityPath, cfg, source, target := convergedRecoveryCity(t)
	id := carriedMessageID(t, source)
	writeLegacyManifest(t, target)
	work := timelessWorkStore(t, source, id)
	row, err := work.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	if err := openMigratedDestination(t, target).Delete(id); err != nil {
		t.Fatal(err)
	}
	holdADifferentBeadUnder(t, target, id, time.Now())

	manifest, _, err := readInfraCopyManifestRecord(target)
	if err != nil {
		t.Fatal(err)
	}
	if got := manifest.Match(row); got == infraManifestCarried {
		t.Errorf("Match(%s) = carried on a bare id, for a row with no created_at the bound cannot place", id)
	}
	gap, err := classifyInfraContainmentGap(cityPath, target, manifest)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(gap.Stranded, id) || !slices.Contains(gap.HeldDifferent, id) || slices.Contains(gap.Retained, id) {
		t.Errorf("containment = stranded %v held-different %v retained %v; the binding holds a bead no copy wrote under %s", gap.Stranded, gap.HeldDifferent, gap.Retained, id)
	}
	var announce bytes.Buffer
	if _, err := clearRetainedInfraCopies(cityPath, target, &announce); err == nil {
		t.Errorf("the clear proceeded over %s, a row nothing proves the binding holds", id)
	}
	if _, err := work.Get(id); err != nil {
		t.Fatalf("the clear deleted %s, the only copy: %v", id, err)
	}
	code, stdout, stderr := runStrandedRecoveryWith(t, cityPath, cfg, strandedRecoveryOptions{})
	if code == 0 || !strings.Contains(stdout, "--"+strandedReplaceBindingRowFlag) || !strings.Contains(stdout, "--"+strandedDropWorkRowFlag) {
		t.Errorf("recover-stranded = %d, want a refusal naming the per-id choices for %s: %s %s", code, id, stdout, stderr)
	}
}

// Review probe (iteration 4, claude): an older build's recover-stranded carried
// a post-cutover row into the binding verbatim and appended its bare id to the
// legacy manifest; the binding then closed the bead and its GC collected it.
// The work store still holds the frozen copy, open, because that build's
// recover only adds and no clear followed. The bare id is younger than the
// legacy bound, so the row reads as reused, but only presumptively: the entry
// may name this very bead. Containment and the clear keep it stranded and never
// delete it, and recover-stranded does not move it back OPEN (#5987) without
// the operator's per-id answer. A closed one it moves.
//
// Red before: recover-stranded moved the frozen open copy back into the
// binding with no question, and the dry run counted it an ordinary strand.
func TestRecoverAsksBeforeMovingAPresumedReusedRowTheBindingCollected(t *testing.T) {
	for _, closed := range []bool{false, true} {
		t.Run(map[bool]string{false: "open", true: "closed"}[closed], func(t *testing.T) {
			prevSlack := infraManifestClockSlack
			infraManifestClockSlack = 0
			t.Cleanup(func() { infraManifestClockSlack = prevSlack })

			cityPath, cfg, source, target := convergedRecoveryCity(t)
			writeLegacyManifest(t, target)
			waitPastMarkerBound(t, target)
			late := mustCreateInfraBead(t, source, beads.Bead{Title: "a mail an older recover-stranded carried", Type: "message"})
			dest, err := openInfraDestination(target)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := dest.(beads.ForeignIDCreator).CreateWithForeignID(infraMigrationRow(late)); err != nil {
				t.Fatalf("carrying %s the way an older recover-stranded did: %v", late.ID, err)
			}
			appendBareManifestID(t, target, late.ID)
			if err := dest.Close(late.ID); err != nil {
				t.Fatal(err)
			}
			if err := dest.Delete(late.ID); err != nil {
				t.Fatalf("collecting %s from the binding: %v", late.ID, err)
			}
			if err := closeBeadStoreHandle(dest); err != nil {
				t.Fatal(err)
			}
			if closed {
				if err := source.Close(late.ID); err != nil {
					t.Fatal(err)
				}
			}

			manifest, _, err := readInfraCopyManifestRecord(target)
			if err != nil {
				t.Fatal(err)
			}
			if got := manifest.Match(late); got != infraManifestPresumedReused {
				t.Fatalf("precondition: Match(%s) = %d, want presumed reused (%d): a bare id younger than the legacy bound", late.ID, got, infraManifestPresumedReused)
			}
			if got := strandedIDs(t, cityPath, target); !slices.Contains(got, late.ID) {
				t.Errorf("stranded = %v, want %s: nothing proves the binding received it", got, late.ID)
			}
			var announce bytes.Buffer
			if _, err := clearRetainedInfraCopies(cityPath, target, &announce); err == nil {
				t.Errorf("the clear proceeded over %s, which it may never delete on its bare id", late.ID)
			}
			if _, err := source.Get(late.ID); err != nil {
				t.Fatalf("the clear deleted %s: %v", late.ID, err)
			}

			if closed {
				if code, stdout, stderr := runStrandedRecoveryWith(t, cityPath, cfg, strandedRecoveryOptions{}); code != 0 {
					t.Fatalf("recover-stranded exited %d for a closed row: %s %s", code, stdout, stderr)
				}
				if got, err := openMigratedDestination(t, target).Get(late.ID); err != nil || got.Status != "closed" {
					t.Errorf("binding %s = status %q (err %v), want the closed row moved in as it is", late.ID, got.Status, err)
				}
				assertClearProceeds(t, cityPath, target)
				return
			}

			before, err := os.ReadFile(target.ManifestPath())
			if err != nil {
				t.Fatal(err)
			}
			code, stdout, stderr := runStrandedRecoveryWith(t, cityPath, cfg, strandedRecoveryOptions{})
			if code == 0 {
				t.Fatalf("recover-stranded exited 0 over the open %s: %s", late.ID, stdout)
			}
			for _, want := range []string{late.ID, "--" + strandedMoveFlag, "--" + strandedRecordCarriedFlag, "status=open"} {
				if !strings.Contains(stderr, want) {
					t.Errorf("the refusal does not name %q: %s", want, stderr)
				}
			}
			if _, err := openMigratedDestination(t, target).Get(late.ID); err == nil {
				t.Errorf("recover-stranded moved the frozen open copy of %s back into the binding", late.ID)
			}
			if after, _ := os.ReadFile(target.ManifestPath()); !bytes.Equal(before, after) {
				t.Error("the refused recover-stranded rewrote the manifest")
			}

			dump := filepath.Join(t.TempDir(), "dump.json")
			code, stdout, _ = runStrandedRecoveryWith(t, cityPath, cfg, strandedRecoveryOptions{DryRun: true, DumpPath: dump})
			if code == 0 || !strings.Contains(stdout, "awaiting a choice") || !strings.Contains(stdout, late.ID) {
				t.Errorf("the dry run = %d, want %s shown awaiting a choice:\n%s", code, late.ID, stdout)
			}
			if data, err := os.ReadFile(dump); err != nil || !strings.Contains(string(data), late.ID) {
				t.Errorf("the dump does not hold %s (err %v)", late.ID, err)
			}

			code, stdout, stderr = runStrandedRecoveryWith(t, cityPath, cfg, strandedRecoveryOptions{RecordCarried: map[string]bool{late.ID: true}})
			if code != 0 {
				t.Fatalf("recover-stranded --%s %s exited %d: %s %s", strandedRecordCarriedFlag, late.ID, code, stdout, stderr)
			}
			if _, err := openMigratedDestination(t, target).Get(late.ID); err == nil {
				t.Errorf("--%s copied %s into the binding", strandedRecordCarriedFlag, late.ID)
			}
			if got := strandedIDs(t, cityPath, target); len(got) != 0 {
				t.Errorf("stranded after --%s: %v", strandedRecordCarriedFlag, got)
			}
			assertClearProceeds(t, cityPath, target)
			if _, err := source.Get(late.ID); !errors.Is(err, beads.ErrNotFound) {
				t.Errorf("the clear left the frozen copy of %s (err %v)", late.ID, err)
			}
		})
	}
}

// How the manifest reads a row against an entry that records no created_at
// (#gc:no-created-at), and a bare id against a row it cannot place.
func TestManifestMatchesARowWithNoCreatedAt(t *testing.T) {
	bound := time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC)
	created := bound.Add(-time.Hour)
	m := infraCopyManifest{
		IDs:         map[string]bool{"mc-none": true, "mc-keyed": true, "mc-bare": true},
		Created:     map[string]time.Time{"mc-none": {}, "mc-keyed": created},
		WrittenAt:   bound,
		LegacyBound: bound,
	}
	cases := []struct {
		name string
		b    beads.Bead
		want infraManifestMatch
	}{
		{"recorded with none, a row with none", beads.Bead{ID: "mc-none"}, infraManifestCarried},
		{"recorded with none, a row with one", beads.Bead{ID: "mc-none", CreatedAt: created}, infraManifestReusedID},
		{"keyed, a row with none", beads.Bead{ID: "mc-keyed"}, infraManifestLegacyUnbounded},
		{"bare id, a row with none", beads.Bead{ID: "mc-bare"}, infraManifestLegacyUnbounded},
		{"bare id, an older row", beads.Bead{ID: "mc-bare", CreatedAt: created}, infraManifestCarried},
		{"bare id, a younger row", beads.Bead{ID: "mc-bare", CreatedAt: bound.Add(time.Hour)}, infraManifestPresumedReused},
	}
	for _, tc := range cases {
		if got := m.Match(tc.b); got != tc.want {
			t.Errorf("%s: Match = %d, want %d", tc.name, got, tc.want)
		}
	}
	m.LegacyBound = time.Time{}
	if got := m.Match(beads.Bead{ID: "mc-none"}); got != infraManifestCarried {
		t.Errorf("an entry recorded with no created_at, under a manifest with no legacy bound = %d, want carried (%d)", got, infraManifestCarried)
	}
}

// A #gc:no-created-at line names exactly one id, and a #gc:created line never
// records a zero time: no created_at is what #gc:no-created-at says, and only
// it.
func TestManifestRefusesAMalformedCreatedAtRecord(t *testing.T) {
	target := infraBindingTarget{Root: t.TempDir()}
	for _, bad := range []string{
		infraManifestNoCreatedPrefix + "mc-1 mc-2",
		infraManifestCreatedPrefix + "mc-1 0001-01-01T00:00:00Z",
	} {
		if err := os.WriteFile(target.ManifestPath(), []byte("mc-1\n"+bad+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, _, err := readInfraCopyManifestRecord(target); err == nil {
			t.Errorf("a manifest line %q read without an error", bad)
		}
	}
}
