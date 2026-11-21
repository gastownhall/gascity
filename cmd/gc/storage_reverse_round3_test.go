package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/dispatch"
)

// killAbortAt runs --abort-reverse with a kill at point, which must stop it.
func killAbortAt(t *testing.T, city reverseEngineCity, point string) {
	t.Helper()
	prev := infraEngineCrashPoint
	infraEngineCrashPoint = func(p string) error {
		if p == point {
			return errors.New("killed at " + point)
		}
		return nil
	}
	code, _, stderr := city.abort(t)
	infraEngineCrashPoint = prev
	if code == 0 || !strings.Contains(stderr, "killed at "+point) {
		t.Fatalf("the kill at %s did not stop the abort: %d: %s", point, code, stderr)
	}
}

// TestReverseNeverResumesAnAbort (N4): an abort killed after it removed rows
// leaves the journal marked aborting, so --to-work refuses to resume it
// (it would archive the binding over a work store missing its rows) and
// names the abort; the abort run again finishes; and the R4/R5 re-list
// catches a store that lost rows by any other route.
func TestReverseNeverResumesAnAbort(t *testing.T) {
	city := newReverseEngineCity(t, 300, "")
	closeBindingGates(t, city)
	before := workStoreFingerprint(t, city.source)
	killReverseAt(t, city, "reverse phase "+reversePhaseName(reverseR3))
	killAbortAt(t, city, "abort-reverse rows")
	code, _, stderr := city.reverse(t, reverseOptions{})
	if code == 0 || !strings.Contains(stderr, "being abandoned") || !strings.Contains(stderr, storageAbortReverseInstruction()) {
		t.Fatalf("--to-work over an aborting journal = %d: %s", code, stderr)
	}
	if !fileExists(city.target.Root) {
		t.Fatal("the binding was archived over an abort")
	}
	if _, err := storageBootGate(city.cityPath, city.request.Cfg, "gc start", nil, os.Stderr); err == nil || !strings.Contains(err.Error(), "an abort of a reverse migration") {
		t.Fatalf("boot over an aborting journal = %v, want the abort named", err)
	}
	if code, stdout, stderr := city.abort(t); code != 0 {
		t.Fatalf("finishing the abort = %d: %s%s", code, stdout, stderr)
	}
	if got := workStoreFingerprint(t, city.source); got != before {
		t.Fatal("the finished abort did not leave the work store as it was")
	}

	// Rows lost by another route are found before R4 and copied again.
	other := newReverseEngineCity(t, 300, "")
	closeBindingGates(t, other)
	killReverseAt(t, other, "reverse phase "+reversePhaseName(reverseR3))
	under, _, _ := unwrapBeadPolicyStore(other.source)
	var lost string
	for id, b := range other.before {
		if isInfra(b) && (lost == "" || id < lost) {
			lost = id
		}
	}
	if err := under.Delete(lost); err != nil {
		t.Fatal(err)
	}
	code, _, stderr = other.reverse(t, reverseOptions{})
	if code == 0 || !strings.Contains(stderr, "no longer hold 1 row(s)") || !strings.Contains(stderr, lost) {
		t.Fatalf("a resume over a store that lost %s = %d: %s", lost, code, stderr)
	}
	if code, stdout, stderr := other.reverse(t, reverseOptions{}); code != 0 {
		t.Fatalf("the next run = %d: %s%s", code, stdout, stderr)
	}
	other.assertReversedNotLossy(t)
	other.assertRevertedCityServesSingleStore(t)

	// And before the archive: a row lost after R4 keeps the binding, and the
	// next run copies it again.
	late := newReverseEngineCity(t, 300, "")
	closeBindingGates(t, late)
	killReverseAt(t, late, "reverse phase "+reversePhaseName(reverseR4))
	lateUnder, _, _ := unwrapBeadPolicyStore(late.source)
	if err := lateUnder.Delete(lost); err != nil {
		t.Fatal(err)
	}
	code, _, stderr = late.reverse(t, reverseOptions{})
	if code == 0 || !strings.Contains(stderr, "checked before the archive (R5)") || !fileExists(late.target.Root) {
		t.Fatalf("a resume after R4 over a store that lost %s = %d: %s", lost, code, stderr)
	}
	if code, stdout, stderr := late.reverse(t, reverseOptions{}); code != 0 {
		t.Fatalf("the next run after R4 = %d: %s%s", code, stdout, stderr)
	}
	late.assertReversedNotLossy(t)
	late.assertRevertedCityServesSingleStore(t)
}

// TestReverseToRigsNeverGoesBackBelowR3AfterItsClearBegan (N3): once the
// binding clear has begun, a rig that lost a row is refused, the snapshot
// named, and nothing is deleted from either store; the abort refuses
// whenever the clear has begun; and a resume from a half-cleared binding
// finishes with every row in the rig.
func TestReverseToRigsNeverGoesBackBelowR3AfterItsClearBegan(t *testing.T) {
	// cleared stops the reverse right after its binding clear: the
	// binding no longer holds the rig's rows, and the rig holds the only copy.
	halfCleared := func(t *testing.T) *reverseRigFixture {
		t.Helper()
		r := newReverseRigFixture(t)
		prev := infraEngineCrashPoint
		infraEngineCrashPoint = func(p string) error {
			if p == "reverse R4 cleared" {
				return errors.New("killed")
			}
			return nil
		}
		code, _, _ := r.reverse(t, reverseOptions{Mode: reverseToRigs, Rigs: []string{"packs"}})
		infraEngineCrashPoint = prev
		if code == 0 {
			t.Fatal("the kill did not stop the reverse")
		}
		if journal, _, _ := readStorageReverseJournal(r.request.CityPath); !journal.ClearStarted {
			t.Fatal("precondition: the clear began")
		}
		return r
	}
	t.Run("abort refuses", func(t *testing.T) {
		r := halfCleared(t)
		rigBefore := workStoreFingerprint(t, r.rig("packs"))
		var stdout, stderr bytes.Buffer
		if code := doStorageAbortReverse(context.Background(), r.request, &stdout, &stderr); code == 0 || !strings.Contains(stderr.String(), "can only be finished") {
			t.Fatalf("--abort-reverse after the clear began = %d: %s", code, stderr.String())
		}
		if workStoreFingerprint(t, r.rig("packs")) != rigBefore {
			t.Fatal("the refused abort changed the rig")
		}
	})
	t.Run("abort refuses before the clear's first delete", func(t *testing.T) {
		// A kill between recording the clear and its first delete: the
		// binding holds every row, and the journal says the clear began.
		r := newReverseRigFixture(t)
		killed := errors.New("killed")
		prev := infraEngineCrashPoint
		infraEngineCrashPoint = func(p string) error {
			if p == "reverse phase "+reversePhaseName(reverseR3) {
				return killed
			}
			return nil
		}
		code, _, _ := r.reverse(t, reverseOptions{Mode: reverseToRigs, Rigs: []string{"packs"}})
		infraEngineCrashPoint = prev
		if code == 0 {
			t.Fatal("the kill did not stop the reverse")
		}
		journal, _, _ := readStorageReverseJournal(r.request.CityPath)
		journal.ClearStarted = true
		if err := writeStorageReverseJournal(r.request.CityPath, journal); err != nil {
			t.Fatal(err)
		}
		rigBefore := workStoreFingerprint(t, r.rig("packs"))
		var stdout, stderr bytes.Buffer
		if code := doStorageAbortReverse(context.Background(), r.request, &stdout, &stderr); code == 0 || !strings.Contains(stderr.String(), "can only be finished") {
			t.Fatalf("--abort-reverse once the clear was recorded = %d: %s", code, stderr.String())
		}
		if workStoreFingerprint(t, r.rig("packs")) != rigBefore {
			t.Fatal("the refused abort changed the rig")
		}
	})
	t.Run("abort refuses at R3 once a row left the binding by another route", func(t *testing.T) {
		// No clear was recorded, but the binding no longer holds a row the
		// rig got back: the rig's copy may be the only one.
		r := newReverseRigFixture(t)
		prev := infraEngineCrashPoint
		infraEngineCrashPoint = func(p string) error {
			if p == "reverse phase "+reversePhaseName(reverseR3) {
				return errors.New("killed")
			}
			return nil
		}
		code, _, _ := r.reverse(t, reverseOptions{Mode: reverseToRigs, Rigs: []string{"packs"}})
		infraEngineCrashPoint = prev
		if code == 0 {
			t.Fatal("the kill did not stop the reverse")
		}
		binding, err := openInfraDestination(r.target)
		if err != nil {
			t.Fatal(err)
		}
		if err := binding.Delete("gp-step2"); err != nil {
			t.Fatal(err)
		}
		_ = closeBeadStoreHandle(binding)
		rigBefore := workStoreFingerprint(t, r.rig("packs"))
		var stdout, stderr bytes.Buffer
		if code := doStorageAbortReverse(context.Background(), r.request, &stdout, &stderr); code == 0 || !strings.Contains(stderr.String(), "can only be finished") {
			t.Fatalf("--abort-reverse after a row left the binding = %d: %s", code, stderr.String())
		}
		if workStoreFingerprint(t, r.rig("packs")) != rigBefore {
			t.Fatal("the refused abort changed the rig")
		}
	})
	// lostAfterRecordedClear stops --to-rigs at its R3 advance, records the
	// clear as begun with no row yet deleted from the binding, then deletes
	// a returned row from the rig as bd does, with the edges onto it.
	lostAfterRecordedClear := func(t *testing.T) *reverseRigFixture {
		t.Helper()
		r := newReverseRigFixture(t)
		prev := infraEngineCrashPoint
		infraEngineCrashPoint = func(p string) error {
			if p == "reverse phase "+reversePhaseName(reverseR3) {
				return errors.New("killed")
			}
			return nil
		}
		code, _, _ := r.reverse(t, reverseOptions{Mode: reverseToRigs, Rigs: []string{"packs"}})
		infraEngineCrashPoint = prev
		if code == 0 {
			t.Fatal("the kill did not stop the reverse")
		}
		journal, _, _ := readStorageReverseJournal(r.request.CityPath)
		journal.ClearStarted = true
		if err := writeStorageReverseJournal(r.request.CityPath, journal); err != nil {
			t.Fatal(err)
		}
		rig := r.rig("packs")
		for _, from := range []string{"gp-step2", "gp-w2"} {
			if err := rig.DepRemove(from, "gp-step1"); err != nil {
				t.Fatal(err)
			}
		}
		if err := rig.Delete("gp-step1"); err != nil {
			t.Fatal(err)
		}
		return r
	}
	finished := func(t *testing.T, r *reverseRigFixture) {
		t.Helper()
		r.assertRigAsSeeded(t)
		for _, b := range rigTestList(t, r.binding()) {
			if b.Metadata[infraMigrationStampKey] == rigImportStamp("packs") {
				t.Fatalf("the binding still holds %s from rig packs", b.ID)
			}
		}
	}
	t.Run("a row the rig lost that the binding still holds is copied back", func(t *testing.T) {
		r := lostAfterRecordedClear(t)
		code, stdout, stderr := r.reverse(t, reverseOptions{Mode: reverseToRigs, Rigs: []string{"packs"}})
		if code != 0 || !strings.Contains(stdout, "1 row(s) a rig lost after the binding clear began were copied back") {
			t.Fatalf("the resume over a rig that lost a row the binding holds = %d: %s%s", code, stdout, stderr)
		}
		finished(t, r)
	})
	t.Run("a faulty re-copy is caught, and the next run finishes it", func(t *testing.T) {
		r := lostAfterRecordedClear(t)
		prev := reverseTargetOpener
		reverseTargetOpener = func(cityPath string, cfg *config.City, dest string) (*reverseTarget, error) {
			target, err := openReverseTarget(cityPath, cfg, dest)
			if err == nil {
				target.addEdges = func([]beads.DepWithPayload) error { return nil }
			}
			return target, err
		}
		code, _, stderr := r.reverse(t, reverseOptions{Mode: reverseToRigs, Rigs: []string{"packs"}})
		reverseTargetOpener = prev
		if code == 0 || !strings.Contains(stderr, "verifying") {
			t.Fatalf("a re-copy that dropped its edges = %d: %s", code, stderr)
		}
		if code, stdout, stderr := r.reverse(t, reverseOptions{Mode: reverseToRigs, Rigs: []string{"packs"}}); code != 0 {
			t.Fatalf("the next run = %d: %s%s", code, stdout, stderr)
		}
		finished(t, r)
	})
	t.Run("a re-copy killed after its rows is finished by the next run", func(t *testing.T) {
		r := lostAfterRecordedClear(t)
		prev := infraEngineCrashPoint
		infraEngineCrashPoint = func(p string) error {
			if p == "reverse rig:packs re-copy rows" {
				return errors.New("killed")
			}
			return nil
		}
		code, _, _ := r.reverse(t, reverseOptions{Mode: reverseToRigs, Rigs: []string{"packs"}})
		infraEngineCrashPoint = prev
		if code == 0 {
			t.Fatal("the kill did not stop the re-copy")
		}
		if journal, _, _ := readStorageReverseJournal(r.request.CityPath); strings.Join(journal.Recopying, ",") != "gp-step1" {
			t.Fatalf("the journal records re-copying %v, want gp-step1", journal.Recopying)
		}
		if code, stdout, stderr := r.reverse(t, reverseOptions{Mode: reverseToRigs, Rigs: []string{"packs"}}); code != 0 {
			t.Fatalf("the next run = %d: %s%s", code, stdout, stderr)
		}
		finished(t, r)
	})
	// partlyCleared stops --to-rigs right after it records its clear, then
	// removes gp-step2 from the binding as the clear's first deletes would,
	// and deletes gp-step1 from the rig as bd does, with the edges onto it
	// (gp-step2 -> gp-step1 among them); gone, when set, is deleted from the
	// rig too.
	partlyCleared := func(t *testing.T, gone ...string) *reverseRigFixture {
		t.Helper()
		r := newReverseRigFixture(t)
		prev := infraEngineCrashPoint
		infraEngineCrashPoint = func(p string) error {
			if p == "reverse R4 clear recorded" {
				return errors.New("killed")
			}
			return nil
		}
		code, _, _ := r.reverse(t, reverseOptions{Mode: reverseToRigs, Rigs: []string{"packs"}})
		infraEngineCrashPoint = prev
		if code == 0 {
			t.Fatal("the kill did not stop the reverse")
		}
		if journal, _, _ := readStorageReverseJournal(r.request.CityPath); !journal.ClearStarted || len(journal.ClearEdges) == 0 {
			t.Fatalf("precondition: the clear began with its edges recorded: %+v", journal)
		}
		binding, err := openInfraDestination(r.target)
		if err != nil {
			t.Fatal(err)
		}
		if err := binding.Delete("gp-step2"); err != nil {
			t.Fatal(err)
		}
		_ = closeBeadStoreHandle(binding)
		rig := r.rig("packs")
		for _, from := range []string{"gp-step2", "gp-w2"} {
			if err := rig.DepRemove(from, "gp-step1"); err != nil {
				t.Fatal(err)
			}
		}
		for _, id := range append([]string{"gp-step1"}, gone...) {
			if err := rig.Delete(id); err != nil {
				t.Fatal(err)
			}
		}
		return r
	}
	t.Run("a re-copy restores the edge from a row the clear already removed", func(t *testing.T) {
		r := partlyCleared(t)
		code, stdout, stderr := r.reverse(t, reverseOptions{Mode: reverseToRigs, Rigs: []string{"packs"}})
		if code != 0 {
			t.Fatalf("the resume = %d: %s%s", code, stdout, stderr)
		}
		if !hasEdge(t, r.rig("packs"), "gp-step2", "gp-step1") {
			t.Fatal("the edge from gp-step2, which the clear had removed from the binding, was not restored onto gp-step1")
		}
		finished(t, r)
	})
	t.Run("a recorded edge whose other end neither store holds is reported dropped", func(t *testing.T) {
		r := partlyCleared(t, "gp-w2")
		code, stdout, stderr := r.reverse(t, reverseOptions{Mode: reverseToRigs, Rigs: []string{"packs"}})
		if code != 0 || !strings.Contains(stdout, "not restored, their other end in neither store: gp-w2 -[tracks]-> gp-step1") {
			t.Fatalf("the resume with gp-w2 gone = %d: %s%s", code, stdout, stderr)
		}
		if !hasEdge(t, r.rig("packs"), "gp-step2", "gp-step1") {
			t.Fatal("the edge from gp-step2 was not restored onto gp-step1")
		}
	})
	t.Run("a row a re-copy wrote wrong is written again by the next run", func(t *testing.T) {
		r := lostAfterRecordedClear(t)
		prev := reverseTargetOpener
		reverseTargetOpener = func(cityPath string, cfg *config.City, dest string) (*reverseTarget, error) {
			target, err := openReverseTarget(cityPath, cfg, dest)
			if err == nil && target.create != nil {
				create := target.create
				target.create = func(rows []beads.Bead) error {
					wrong := append([]beads.Bead(nil), rows...)
					for i := range wrong {
						wrong[i].Title += " (garbled)"
					}
					return create(wrong)
				}
			}
			return target, err
		}
		code, _, stderr := r.reverse(t, reverseOptions{Mode: reverseToRigs, Rigs: []string{"packs"}})
		reverseTargetOpener = prev
		if code == 0 || !strings.Contains(stderr, "verifying") || !strings.Contains(stderr, "gp-step1 differs") {
			t.Fatalf("a re-copy that garbled its row = %d: %s", code, stderr)
		}
		if code, stdout, stderr := r.reverse(t, reverseOptions{Mode: reverseToRigs, Rigs: []string{"packs"}}); code != 0 {
			t.Fatalf("the honest rerun = %d: %s%s", code, stdout, stderr)
		}
		finished(t, r)
	})
	t.Run("a re-copied row records a cross-store edge the clear's first pass removed", func(t *testing.T) {
		r := newReverseRigFixture(t)
		binding, err := openInfraDestination(r.target)
		if err != nil {
			t.Fatal(err)
		}
		gcg, err := binding.Create(beads.Bead{Title: "city graph row", Type: "task"})
		if err != nil {
			t.Fatal(err)
		}
		if err := binding.Close(gcg.ID); err != nil {
			t.Fatal(err)
		}
		if err := binding.DepAdd(gcg.ID, "gp-step1", "tracks"); err != nil {
			t.Fatal(err)
		}
		_ = closeBeadStoreHandle(binding)
		prev := infraEngineCrashPoint
		infraEngineCrashPoint = func(p string) error {
			if p == "reverse R4 clear recorded" {
				return errors.New("killed")
			}
			return nil
		}
		code, _, _ := r.reverse(t, reverseOptions{Mode: reverseToRigs, Rigs: []string{"packs"}})
		infraEngineCrashPoint = prev
		if code == 0 {
			t.Fatal("the kill did not stop the reverse")
		}
		// The clear's first pass: the edge joining a row it removes to one
		// the binding keeps goes first.
		if binding, err = openInfraDestination(r.target); err != nil {
			t.Fatal(err)
		}
		if err := binding.DepRemove(gcg.ID, "gp-step1"); err != nil {
			t.Fatal(err)
		}
		_ = closeBeadStoreHandle(binding)
		rig := r.rig("packs")
		for _, from := range []string{"gp-step2", "gp-w2"} {
			if err := rig.DepRemove(from, "gp-step1"); err != nil {
				t.Fatal(err)
			}
		}
		if err := rig.Delete("gp-step1"); err != nil {
			t.Fatal(err)
		}
		if code, stdout, stderr := r.reverse(t, reverseOptions{Mode: reverseToRigs, Rigs: []string{"packs"}}); code != 0 {
			t.Fatalf("the resume = %d: %s%s", code, stdout, stderr)
		}
		got, err := r.rig("packs").Get("gp-step1")
		if err != nil {
			t.Fatal(err)
		}
		if raw := got.Metadata[beadmeta.MigratedCrossEdgesMetadataKey]; !strings.Contains(raw, `"other":"`+gcg.ID+`"`) || !strings.Contains(raw, `"action":"dropped"`) {
			t.Fatalf("the re-copied gp-step1 records %s, want %s -[tracks]-> gp-step1 recorded dropped", raw, gcg.ID)
		}
		finished(t, r)
	})
	t.Run("an edge only the binding recorded is restored", func(t *testing.T) {
		// gp-step2 -> gp-step1 leaves the rig before the clear, so only the
		// binding's record holds it; gp-step1 then leaves the binding (the
		// clear's first deletes) and gp-step2 the rig.
		r := newReverseRigFixture(t)
		prev := infraEngineCrashPoint
		infraEngineCrashPoint = func(p string) error {
			if p == "reverse phase "+reversePhaseName(reverseR3) {
				return errors.New("killed")
			}
			return nil
		}
		code, _, _ := r.reverse(t, reverseOptions{Mode: reverseToRigs, Rigs: []string{"packs"}})
		if code == 0 {
			t.Fatal("the kill at R3 did not stop the reverse")
		}
		rig := r.rig("packs")
		if err := rig.DepRemove("gp-step2", "gp-step1"); err != nil {
			t.Fatal(err)
		}
		infraEngineCrashPoint = func(p string) error {
			if p == "reverse R4 clear recorded" {
				return errors.New("killed")
			}
			return nil
		}
		code, _, _ = r.reverse(t, reverseOptions{Mode: reverseToRigs, Rigs: []string{"packs"}})
		infraEngineCrashPoint = prev
		if code == 0 {
			t.Fatal("the kill at the clear did not stop the reverse")
		}
		binding, err := openInfraDestination(r.target)
		if err != nil {
			t.Fatal(err)
		}
		if err := binding.Delete("gp-step1"); err != nil {
			t.Fatal(err)
		}
		_ = closeBeadStoreHandle(binding)
		if err := rig.Delete("gp-step2"); err != nil {
			t.Fatal(err)
		}
		if code, stdout, stderr := r.reverse(t, reverseOptions{Mode: reverseToRigs, Rigs: []string{"packs"}}); code != 0 {
			t.Fatalf("the resume = %d: %s%s", code, stdout, stderr)
		}
		if !hasEdge(t, r.rig("packs"), "gp-step2", "gp-step1") {
			t.Fatal("gp-step2 -> gp-step1, recorded only from the binding, was not restored")
		}
		finished(t, r)
	})
	t.Run("a rig that lost a row refuses, deleting nothing", func(t *testing.T) {
		r := halfCleared(t)
		if err := r.rig("packs").Delete("gp-step2"); err != nil {
			t.Fatal(err)
		}
		journal, _, _ := readStorageReverseJournal(r.request.CityPath)
		rigBefore, bindingBefore := workStoreFingerprint(t, r.rig("packs")), workStoreFingerprint(t, r.binding())
		code, _, stderr := r.reverse(t, reverseOptions{Mode: reverseToRigs, Rigs: []string{"packs"}})
		if code == 0 || !strings.Contains(stderr, "gp-step2") || !strings.Contains(stderr, beadmeta.InfraReversedFromMetadataKey+"="+journal.Stamp) {
			t.Fatalf("a resume over a rig that lost a row after the clear began = %d: %s", code, stderr)
		}
		if journal, _, _ := readStorageReverseJournal(r.request.CityPath); journal.Phase < reverseR3 {
			t.Fatalf("the journal went back to phase %d after the clear began", journal.Phase)
		}
		if workStoreFingerprint(t, r.rig("packs")) != rigBefore || workStoreFingerprint(t, r.binding()) != bindingBefore {
			t.Fatal("the refused resume deleted rows")
		}
	})
	t.Run("resume finishes", func(t *testing.T) {
		r := halfCleared(t)
		if code, stdout, stderr := r.reverse(t, reverseOptions{Mode: reverseToRigs, Rigs: []string{"packs"}}); code != 0 {
			t.Fatalf("the resume = %d: %s%s", code, stdout, stderr)
		}
		r.assertRigAsSeeded(t)
	})
	t.Run("the prepare deletes no stamped row the binding no longer holds", func(t *testing.T) {
		r := halfCleared(t)
		journal, _, _ := readStorageReverseJournal(r.request.CityPath)
		journal.Phase, journal.ClearStarted = reverseR0, false // an old build's reset
		if err := writeStorageReverseJournal(r.request.CityPath, journal); err != nil {
			t.Fatal(err)
		}
		rigBefore := workStoreFingerprint(t, r.rig("packs"))
		code, _, stderr := r.reverse(t, reverseOptions{Mode: reverseToRigs, Rigs: []string{"packs"}})
		if code == 0 || !strings.Contains(stderr, "may be the only copy") {
			t.Fatalf("a re-copy over rows the binding no longer holds = %d: %s", code, stderr)
		}
		if workStoreFingerprint(t, r.rig("packs")) != rigBefore {
			t.Fatal("the prepare deleted the only copy")
		}
	})
}

// TestReverseRecordsNoPreservedEdgeOnAResume (N1): a first plan that finds
// no edge onto a planned row records that, so a resume never takes the
// reverse's own restored edges for ones the store held: abandoning after a
// resume leaves the work store exactly as it was (M61).
func TestReverseRecordsNoPreservedEdgeOnAResume(t *testing.T) {
	city := newReverseEngineCity(t, 300, "")
	closeBindingGates(t, city)
	before := workStoreFingerprint(t, city.source)
	killReverseAt(t, city, "reverse city verify")
	journal, _, _ := readStorageReverseJournal(city.cityPath)
	if !journal.PreservedRecorded || len(journal.Preserved) != 0 {
		t.Fatalf("the first plan recorded preserved=%v (%d), want an empty answer recorded", journal.PreservedRecorded, len(journal.Preserved))
	}
	killReverseAt(t, city, "reverse phase "+reversePhaseName(reverseR2))
	journal, _, _ = readStorageReverseJournal(city.cityPath)
	if len(journal.Preserved) != 0 {
		t.Fatalf("the resume recorded %d preserved edge(s): its own restored edges", len(journal.Preserved))
	}
	if code, stdout, stderr := city.abort(t); code != 0 {
		t.Fatalf("--abort-reverse = %d: %s%s", code, stdout, stderr)
	}
	if got := workStoreFingerprint(t, city.source); got != before {
		t.Fatal("abandoning after a resume did not leave the work store as it was")
	}
}

// TestReverseToRigsResumesAfterItsRenames (N2): killed between R4's renames
// and the journal's R4, the resume does not ask for the import marker it
// renamed: it finishes, and the rig reads as awaiting import.
func TestReverseToRigsResumesAfterItsRenames(t *testing.T) {
	r := newReverseRigFixture(t)
	prev := infraEngineCrashPoint
	infraEngineCrashPoint = func(p string) error {
		if p == "reverse R4 renamed" {
			return errors.New("killed")
		}
		return nil
	}
	code, _, _ := r.reverse(t, reverseOptions{Mode: reverseToRigs, Rigs: []string{"packs"}})
	infraEngineCrashPoint = prev
	if code == 0 {
		t.Fatal("the kill did not stop the reverse")
	}
	if code, stdout, stderr := r.reverse(t, reverseOptions{Mode: reverseToRigs, Rigs: []string{"packs"}}); code != 0 {
		t.Fatalf("the resume after the renames = %d: %s%s", code, stdout, stderr)
	}
	if rows := censusRigs(r.request.CityPath, r.cfg, r.target); rows[0].State != rigCensusAwaiting {
		t.Fatalf("the rig after the resume = %+v, want awaiting import", rows[0])
	}
	r.assertRigAsSeeded(t)
}

// TestReverseRefusesAPayloadEdgeItCouldNotPutBack: an edge the work store
// held onto a planned row, carrying a payload, in a store that cannot write
// one is refused at plan time; and the edge pass refuses, rather than
// removing, an edge whose payload it cannot write.
func TestReverseRefusesAPayloadEdgeItCouldNotPutBack(t *testing.T) {
	city := newReverseEngineCity(t, 300, "")
	closeBindingGates(t, city)
	w, d := withOldClearEdge(t, city)
	under, _, _ := unwrapBeadPolicyStore(city.source)
	writer := under.(beads.DepMetadataWriter)
	if err := writer.DepAddWithMetadata(w, d, "waits-for", `{"gate":"all-children"}`); err != nil {
		t.Fatal(err)
	}
	prev := reverseTargetOpener
	reverseTargetOpener = func(cityPath string, cfg *config.City, dest string) (*reverseTarget, error) {
		target, err := openReverseTarget(cityPath, cfg, dest)
		if err == nil {
			target.carriesPayload = false
		}
		return target, err
	}
	t.Cleanup(func() { reverseTargetOpener = prev })
	code, _, stderr := city.reverse(t, reverseOptions{})
	if code == 0 || !strings.Contains(stderr, w+" -[waits-for]-> "+d) || !strings.Contains(stderr, "cannot write a payload") {
		t.Fatalf("--to-work over a payload edge the store could not put back = %d: %s", code, stderr)
	}

	target, err := openReverseTarget(city.cityPath, city.request.Cfg, reverseCityDest)
	if err != nil {
		t.Fatal(err)
	}
	defer target.close()
	target.carriesPayload = false
	_, err = reverseCopyInto(target, func() (*reverseTarget, error) { return target, nil }, nil,
		[]beads.DepWithPayload{{Dep: beads.Dep{IssueID: w, DependsOnID: d, Type: "waits-for"}, Payload: `{"gate":"any-children"}`}}, map[string]bool{}, "infra/x", infraEngineClock{})
	if err == nil || !strings.Contains(err.Error(), "refused rather than removed") {
		t.Fatalf("the edge pass = %v", err)
	}
	if !hasEdge(t, city.source, w, d) {
		t.Fatal("the edge pass removed an edge it could not write back")
	}

	// The abort's pre-check: a preserved payload edge the store cannot put
	// back refuses the abort before any row is deleted.
	reverseTargetOpener = prev
	late := newReverseEngineCity(t, 300, "")
	closeBindingGates(t, late)
	lw, ld := withOldClearEdge(t, late)
	lateUnder, _, _ := unwrapBeadPolicyStore(late.source)
	if err := lateUnder.(beads.DepMetadataWriter).DepAddWithMetadata(lw, ld, "waits-for", `{"gate":"all-children"}`); err != nil {
		t.Fatal(err)
	}
	killReverseAt(t, late, "reverse phase "+reversePhaseName(reverseR2))
	before := workStoreFingerprint(t, late.source)
	reverseTargetOpener = func(cityPath string, cfg *config.City, dest string) (*reverseTarget, error) {
		target, err := openReverseTarget(cityPath, cfg, dest)
		if err == nil {
			target.carriesPayload = false
		}
		return target, err
	}
	code, _, stderr = late.abort(t)
	if code == 0 || !strings.Contains(stderr, lw+" -[waits-for]-> "+ld) || !strings.Contains(stderr, "Nothing was removed") {
		t.Fatalf("--abort-reverse over a payload edge the store could not put back = %d: %s", code, stderr)
	}
	if workStoreFingerprint(t, late.source) != before {
		t.Fatal("the refused abort changed the work store")
	}
}

// TestReverseVerifyCatchesADroppedRestoredEdge (M13): a fault that drops each
// restored edge whose source is not a planned row (the work beads' records)
// fails the verify, and an honest rerun repairs it.
func TestReverseVerifyCatchesADroppedRestoredEdge(t *testing.T) {
	city := newReverseEngineCity(t, 300, "")
	closeBindingGates(t, city)
	prev := reverseTargetOpener
	reverseTargetOpener = func(cityPath string, cfg *config.City, dest string) (*reverseTarget, error) {
		target, err := openReverseTarget(cityPath, cfg, dest)
		if err != nil {
			return nil, err
		}
		add := target.addEdges
		target.addEdges = func(edges []beads.DepWithPayload) error {
			var kept []beads.DepWithPayload
			for _, e := range edges {
				if !strings.HasPrefix(e.IssueID, "mc-w") {
					kept = append(kept, e)
				}
			}
			return add(kept)
		}
		return target, nil
	}
	code, _, stderr := city.reverse(t, reverseOptions{})
	reverseTargetOpener = prev
	if code == 0 || !strings.Contains(stderr, "dependency of mc-w") || !strings.Contains(stderr, "missing") {
		t.Fatalf("a copy that dropped the work beads' restored edges = %d: %s", code, stderr)
	}
	if code, stdout, stderr := city.reverse(t, reverseOptions{}); code != 0 {
		t.Fatalf("the honest rerun = %d: %s%s", code, stdout, stderr)
	}
	city.assertReversedNotLossy(t)
}

// TestReverseRecordWithoutItsJournalHolds (M15): killed after R0, with the
// journal gone, boot and --from-work refuse, naming infra.reverting.
func TestReverseRecordWithoutItsJournalHolds(t *testing.T) {
	city := newReverseEngineCity(t, 60, "")
	closeBindingGates(t, city)
	killReverseAt(t, city, "reverse phase "+reversePhaseName(reverseR0))
	if err := os.Remove(storageReverseJournalPath(city.cityPath)); err != nil {
		t.Fatal(err)
	}
	if _, err := storageBootGate(city.cityPath, city.request.Cfg, "gc start", nil, os.Stderr); err == nil || !strings.Contains(err.Error(), infraRevertingRecordName) {
		t.Fatalf("boot = %v, want infra.reverting named", err)
	}
	var stdout, stderr bytes.Buffer
	if code := doStorageMigrate(context.Background(), city.request, &stdout, &stderr); code == 0 || !strings.Contains(stderr.String(), infraRevertingRecordName) {
		t.Fatalf("--from-work = %d: %s", code, stderr.String())
	}
}

// TestAbortReverseUnpinsTheSnapshot (M17): the abort keeps the snapshot and
// removes its pin.
func TestAbortReverseUnpinsTheSnapshot(t *testing.T) {
	city := newReverseEngineCity(t, 60, "")
	closeBindingGates(t, city)
	killReverseAt(t, city, "reverse city verify")
	journal, _, _ := readStorageReverseJournal(city.cityPath)
	if code, stdout, stderr := city.abort(t); code != 0 {
		t.Fatalf("--abort-reverse = %d: %s%s", code, stdout, stderr)
	}
	dir := filepath.Join(journal.SnapshotTo, journal.Snapshot)
	if !fileExists(dir) || fileExists(filepath.Join(dir, storageSnapshotPinName)) {
		t.Fatalf("after the abort the snapshot exists=%v pinned=%v, want kept and unpinned", fileExists(dir), fileExists(filepath.Join(dir, storageSnapshotPinName)))
	}
}

// TestReverseKeepsACityTOMLEditedMidReverse (M18, M19): city.toml changed
// after R4's edit makes the resume refuse and the abort refuse, and neither
// touches the bytes. The abort refuses before its first delete, so it removes
// no row and does not mark the reverse as being abandoned; an abort killed
// after its deletes refuses the same way, without claiming it removed nothing.
// Either way the remedy it prints finishes it: once [storage] is back by hand
// as the split had it, the operator's own change kept, the abort keeps that
// file, the work store is as it was, and the city reads as split again.
func TestReverseKeepsACityTOMLEditedMidReverse(t *testing.T) {
	const edit = "# the operator's edit\n"
	for _, tc := range []struct {
		name string
		// abort runs the abort over the edited file, not the resume; killed
		// first kills an abort after its deletes, before the edit.
		abort, killed bool
	}{
		{"resume", false, false},
		{"abort", true, false},
		{"abort killed after its deletes", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			city := newReverseEngineCity(t, 60, "")
			closeBindingGates(t, city)
			before := workStoreFingerprint(t, city.source)
			killReverseAt(t, city, "reverse R4 config")
			if tc.killed {
				killAbortAt(t, city, "abort-reverse rows")
			}
			journal, _, err := readStorageReverseJournal(city.cityPath)
			if err != nil || journal.CityTOMLBefore == nil {
				t.Fatalf("the journal after R4's edit = %+v, %v; want city.toml's earlier bytes in it", journal, err)
			}
			path := filepath.Join(city.cityPath, "city.toml")
			f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := f.WriteString(edit); err != nil {
				t.Fatal(err)
			}
			_ = f.Close()
			edited, _ := os.ReadFile(path)
			rows := workStoreFingerprint(t, city.source)
			var code int
			var stderr, want string
			if tc.abort {
				code, _, stderr = city.abort(t)
				want = "changed since the reverse edited it"
			} else {
				code, _, stderr = city.reverse(t, reverseOptions{})
				want = "changed while the reverse was under way"
			}
			if code == 0 || !strings.Contains(stderr, want) {
				t.Fatalf("%s over an edited city.toml = %d: %s, want %q", tc.name, code, stderr, want)
			}
			if got, _ := os.ReadFile(path); !bytes.Equal(got, edited) {
				t.Fatalf("%s changed the operator's city.toml", tc.name)
			}
			if !tc.abort {
				// The way out it names: the abort, which keeps the change.
				for _, want := range []string{storageAbortReverseInstruction(), "keeps your change once the file declares the split's [storage]", "city_toml_before"} {
					if !strings.Contains(stderr, want) {
						t.Fatalf("the resume's refusal does not say %q: %s", want, stderr)
					}
				}
				return
			}
			if got := workStoreFingerprint(t, city.source); got != rows {
				t.Fatal("the refused abort removed rows")
			}
			if after, _, _ := readStorageReverseJournal(city.cityPath); after.Aborting != tc.killed {
				t.Fatalf("after the refused abort the journal says aborting=%v, want %v", after.Aborting, tc.killed)
			}
			if strings.Contains(stderr, "Nothing was removed") == tc.killed {
				t.Fatalf("the refusal of an abort killed=%v says whether it removed rows wrongly: %s", tc.killed, stderr)
			}
			for _, want := range []string{"put [storage] back as the split had it", "city_toml_before"} {
				if !strings.Contains(stderr, want) {
					t.Fatalf("the refusal does not say %q: %s", want, stderr)
				}
			}

			// A [storage] that serves another root is not the split put back.
			elsewhere := strings.Replace(*journal.CityTOMLBefore, `path = ".gc/store"`, `path = ".gc/elsewhere"`, 1)
			if elsewhere == *journal.CityTOMLBefore {
				t.Fatal("the fixture's city.toml names no binding path .gc/store")
			}
			if err := os.WriteFile(path, []byte(elsewhere+edit), 0o644); err != nil {
				t.Fatal(err)
			}
			if code, _, stderr := city.abort(t); code == 0 || !strings.Contains(stderr, "composes no [storage] that serves the binding") {
				t.Fatalf("the abort over a [storage] serving another root = %d: %s", code, stderr)
			}
			if got := workStoreFingerprint(t, city.source); got != rows {
				t.Fatal("the abort over a [storage] serving another root removed rows")
			}

			// The remedy it prints: [storage] back by hand, the edit kept.
			restored := *journal.CityTOMLBefore + edit
			if err := os.WriteFile(path, []byte(restored), 0o644); err != nil {
				t.Fatal(err)
			}
			if code, stdout, stderr := city.abort(t); code != 0 {
				t.Fatalf("the abort over a city.toml with [storage] back by hand = %d: %s%s", code, stdout, stderr)
			}
			if got, _ := os.ReadFile(path); string(got) != restored {
				t.Fatalf("the abort did not keep the city.toml put back by hand:\n%s", got)
			}
			if got := workStoreFingerprint(t, city.source); got != before {
				t.Fatal("the abort did not leave the work store as it was")
			}
			for _, p := range []string{bornSplitServedNotePath(city.cityPath), infraClearedNotePath(city.cityPath), city.target.MarkerPath()} {
				if !fileExists(p) {
					t.Fatalf("%s is gone after the abort", p)
				}
			}
			for _, p := range []string{storageReverseJournalPath(city.cityPath), filepath.Join(city.target.Root, infraRevertingRecordName), filepath.Join(city.target.Root, infraReversedManifestName)} {
				if fileExists(p) {
					t.Fatalf("%s is left behind by the abort", p)
				}
			}
			cfg, err := loadCityConfigWithoutBuiltinPackRefresh(city.cityPath, io.Discard)
			if err != nil || cfg.Storage == nil {
				t.Fatalf("the city after the abort = %+v, %v; want it to declare [storage]", cfg, err)
			}
			if _, err := storageBootGate(city.cityPath, cfg, "gc start", nil, io.Discard); err != nil {
				t.Fatalf("boot after the abort = %v, want the split served again", err)
			}
		})
	}
}

// TestReverseRefusesAnotherReverseInProgress (M20).
func TestReverseRefusesAnotherReverseInProgress(t *testing.T) {
	city := newReverseEngineCity(t, 60, "")
	closeBindingGates(t, city)
	killReverseAt(t, city, "reverse city verify")
	if code, _, stderr := city.reverse(t, reverseOptions{Mode: reverseToRigs, Rigs: []string{"packs"}}); code == 0 || !strings.Contains(stderr, "another reverse is in progress") {
		t.Fatalf("--to-rigs over an open --to-work = %d: %s", code, stderr)
	}
	r := newReverseRigFixture(t)
	prev := infraEngineCrashPoint
	infraEngineCrashPoint = func(p string) error {
		if p == "reverse rig:packs verify" {
			return errors.New("killed")
		}
		return nil
	}
	_, _, _ = r.reverse(t, reverseOptions{Mode: reverseToRigs, Rigs: []string{"packs"}})
	infraEngineCrashPoint = prev
	if code, _, stderr := r.reverse(t, reverseOptions{Mode: reverseToRigs, Rigs: []string{"other"}}); code == 0 || !strings.Contains(stderr, "another reverse is in progress") {
		t.Fatalf("--to-rigs --rig other over a packs journal = %d: %s", code, stderr)
	}
}

// TestReverseRefusesAnUnconvergedBinding (M21, M22).
func TestReverseRefusesAnUnconvergedBinding(t *testing.T) {
	city := newReverseEngineCity(t, 60, "")
	closeBindingGates(t, city)
	if err := os.Remove(city.target.MarkerPath()); err != nil {
		t.Fatal(err)
	}
	if code, _, stderr := city.reverse(t, reverseOptions{}); code == 0 || !strings.Contains(stderr, "has not converged") {
		t.Fatalf("--to-work without a marker = %d: %s", code, stderr)
	}
	if fileExists(storageReverseJournalPath(city.cityPath)) {
		t.Fatal("the refusal wrote a journal")
	}
	other := newReverseEngineCity(t, 60, "")
	closeBindingGates(t, other)
	if err := writeInfraRecopyRecord(other.target, infraMigrationFromWork); err != nil {
		t.Fatal(err)
	}
	if code, _, stderr := other.reverse(t, reverseOptions{}); code == 0 || !strings.Contains(stderr, infraRecopyRecordName) {
		t.Fatalf("--to-work over an unfinished re-copy = %d: %s", code, stderr)
	}
}

// TestReverseRefusesASharedStore (M34): another city on the work store
// refuses, naming it and the attestation; attested, a co-tenant holding its
// controller lock still refuses.
func TestReverseRefusesASharedStore(t *testing.T) {
	city := newReverseEngineCity(t, 60, "")
	closeBindingGates(t, city)
	killReverseAt(t, city, "reverse phase "+reversePhaseName(reverseR2))
	other := sharedStoreCities(t, city.request)
	code, _, stderr := city.reverse(t, reverseOptions{})
	if code == 0 || !strings.Contains(stderr, other) || !strings.Contains(stderr, "--force=shared-store:"+sharedToken) {
		t.Fatalf("--to-work over a shared store = %d: %s", code, stderr)
	}
	before := workStoreFingerprint(t, city.source)
	if code, _, stderr := city.abort(t); code == 0 || !strings.Contains(stderr, other) || !strings.Contains(stderr, "--force=shared-store:"+sharedToken) {
		t.Fatalf("--abort-reverse over a shared store = %d: %s", code, stderr)
	}
	if workStoreFingerprint(t, city.source) != before {
		t.Fatal("the refused abort changed the work store")
	}
	held, err := acquireControllerLock(other)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close() //nolint:errcheck // test cleanup
	city.request.Force = storageForce{SharedStores: []string{sharedToken}}
	if code, _, stderr := city.reverse(t, reverseOptions{}); code == 0 || !strings.Contains(stderr, "holds its controller lock") {
		t.Fatalf("--to-work attested beside a live co-tenant = %d: %s", code, stderr)
	}
}

// TestAbortReverseJudgesThePhaseUnderTheGuard (M35): a to-rigs reverse that
// reaches its R4 while the abort waits for the guard is refused, from the
// journal as it is under the guard, and the rig keeps every row.
func TestAbortReverseJudgesThePhaseUnderTheGuard(t *testing.T) {
	r := newReverseRigFixture(t)
	prev := infraEngineCrashPoint
	infraEngineCrashPoint = func(p string) error {
		if p == "reverse rig:packs verify" {
			return errors.New("killed")
		}
		return nil
	}
	_, _, _ = r.reverse(t, reverseOptions{Mode: reverseToRigs, Rigs: []string{"packs"}})
	rigBefore := workStoreFingerprint(t, r.rig("packs"))
	infraEngineCrashPoint = func(p string) error {
		if p != "abort-reverse guarded" {
			return nil
		}
		journal, _, _ := readStorageReverseJournal(r.request.CityPath)
		journal.Phase = reverseR4
		if err := writeStorageReverseJournal(r.request.CityPath, journal); err != nil {
			return err
		}
		binding, err := openInfraDestination(r.target)
		if err != nil {
			return err
		}
		defer closeBeadStoreHandle(binding) //nolint:errcheck // test cleanup
		for _, b := range rigTestList(t, binding) {
			if b.Metadata[infraMigrationStampKey] == rigImportStamp("packs") {
				if err := binding.Delete(b.ID); err != nil {
					return err
				}
			}
		}
		return nil
	}
	var stdout, stderr bytes.Buffer
	code := doStorageAbortReverse(context.Background(), r.request, &stdout, &stderr)
	infraEngineCrashPoint = prev
	if code == 0 || !strings.Contains(stderr.String(), "can only be finished") {
		t.Fatalf("--abort-reverse over a reverse that reached R4 under it = %d: %s", code, stderr.String())
	}
	if workStoreFingerprint(t, r.rig("packs")) != rigBefore {
		t.Fatal("the refused abort deleted the rig's rows")
	}
}

// TestReverseDryRunTakesTheRunsLanes: the dry run opens its stores inside a
// window that starts nothing and holds no lock, so they take the lanes the
// run's window would force.
func TestReverseDryRunTakesTheRunsLanes(t *testing.T) {
	city := newReverseEngineCity(t, 60, "")
	closeBindingGates(t, city)
	inWindow := false
	prev := reverseTargetOpener
	reverseTargetOpener = func(cityPath string, cfg *config.City, dest string) (*reverseTarget, error) {
		inWindow = inWindow || activeMigrationWindow(cityPath) != nil
		return openReverseTarget(cityPath, cfg, dest)
	}
	t.Cleanup(func() { reverseTargetOpener = prev })
	if code, stdout, stderr := city.reverse(t, reverseOptions{DryRun: true}); code != 0 {
		t.Fatalf("--dry-run = %d: %s%s", code, stdout, stderr)
	}
	if !inWindow {
		t.Fatal("the dry run opened its stores outside the lane window")
	}
	if activeMigrationWindow(city.cityPath) != nil {
		t.Fatal("the dry run left its lane window open")
	}
}

// TestReverseRefusesASharedRigStore: a rig store another registered city
// also uses refuses the reverse and its abort, naming the attestation, while
// the work store is the city's own.
func TestReverseRefusesASharedRigStore(t *testing.T) {
	r := newReverseRigFixture(t)
	rigRoot := rigStoreRoot(r.request.CityPath, r.request.Cfg.Rigs[0])
	other := otherCityWithRig(t, filepath.Join(t.TempDir(), "elsewhere"))
	r.otherCities = []string{other}
	shared := storeDoltIdentity{Database: "packs", Endpoint: "loopback:3399"}
	prev := storeDoltIdentityOf
	t.Cleanup(func() { storeDoltIdentityOf = prev })
	storeDoltIdentityOf = func(cityPath, scopeRoot string) storeDoltIdentity {
		if scopeRoot == rigRoot || cityPath == other {
			return shared
		}
		return storeDoltIdentity{}
	}
	before := workStoreFingerprint(t, r.rig("packs"))
	code, _, stderr := r.reverse(t, reverseOptions{Mode: reverseToRigs, Rigs: []string{"packs"}})
	if code == 0 || !strings.Contains(stderr, "--force=shared-store:packs@loopback:3399") {
		t.Fatalf("--to-rigs over a shared rig store = %d: %s", code, stderr)
	}
	if after := workStoreFingerprint(t, r.rig("packs")); after != before {
		t.Fatalf("the refused reverse changed the rig: %s", after)
	}

	storeDoltIdentityOf = prev
	prevCrash := infraEngineCrashPoint
	infraEngineCrashPoint = func(p string) error {
		if p == "reverse rig:packs verify" {
			return errors.New("killed")
		}
		return nil
	}
	_, _, _ = r.reverse(t, reverseOptions{Mode: reverseToRigs, Rigs: []string{"packs"}})
	infraEngineCrashPoint = prevCrash
	storeDoltIdentityOf = func(cityPath, scopeRoot string) storeDoltIdentity {
		if scopeRoot == rigRoot || cityPath == other {
			return shared
		}
		return storeDoltIdentity{}
	}
	var stdout, errOut bytes.Buffer
	if code := doStorageAbortReverse(context.Background(), r.request, &stdout, &errOut); code == 0 || !strings.Contains(errOut.String(), "--force=shared-store:packs@loopback:3399") {
		t.Fatalf("the abort over a shared rig store = %d: %s", code, errOut.String())
	}
}

// TestReverseJudgesTheJournalUnderTheGuard: a journal another run moved
// while this one waited for the lock and the guard (an abort that began, or
// a reverse that started) refuses the run, and it writes nothing.
func TestReverseJudgesTheJournalUnderTheGuard(t *testing.T) {
	moved := func(t *testing.T, city reverseEngineCity, move func(storageReverseJournal) storageReverseJournal) {
		t.Helper()
		before := workStoreFingerprint(t, city.source)
		var wrote storageReverseJournal
		prev := infraEngineCrashPoint
		infraEngineCrashPoint = func(p string) error {
			if p != reverseGuardedSeam {
				return nil
			}
			journal, _, _ := readStorageReverseJournal(city.cityPath)
			wrote = move(journal)
			return writeStorageReverseJournal(city.cityPath, wrote)
		}
		code, _, stderr := city.reverse(t, reverseOptions{})
		infraEngineCrashPoint = prev
		if code == 0 || !strings.Contains(stderr, "changed while this run waited") {
			t.Fatalf("--to-work over a journal moved under the guard = %d: %s", code, stderr)
		}
		if after, _, _ := readStorageReverseJournal(city.cityPath); !reflect.DeepEqual(after, wrote) {
			t.Fatalf("the refused run rewrote the journal: %+v, want %+v", after, wrote)
		}
		if workStoreFingerprint(t, city.source) != before {
			t.Fatal("the refused run changed the work store")
		}
	}
	t.Run("an abort began", func(t *testing.T) {
		city := newReverseEngineCity(t, 60, "")
		closeBindingGates(t, city)
		killReverseAt(t, city, "reverse phase "+reversePhaseName(reverseR2))
		moved(t, city, func(j storageReverseJournal) storageReverseJournal { j.Aborting = true; return j })
	})
	t.Run("a reverse started", func(t *testing.T) {
		city := newReverseEngineCity(t, 60, "")
		closeBindingGates(t, city)
		moved(t, city, func(storageReverseJournal) storageReverseJournal {
			return storageReverseJournal{Format: storageReverseJournalFormat, Mode: reverseToRigs, Rigs: []string{"packs"}, Phase: reverseR1, Run: "other", Stamp: "infra/other"}
		})
	})
}

// TestReverseToRigsClearRefusesAnOpenGateMintedAfterItBegan: the plan
// refuses an open cross-store gate of a row it returns, but a gate the
// runtime mints for that row once the clear has begun (a writer the fleet
// attestation missed) is met only by the clear. It refuses, naming the gate
// and the row, and removes nothing from the binding.
func TestReverseToRigsClearRefusesAnOpenGateMintedAfterItBegan(t *testing.T) {
	r := newReverseRigFixture(t)
	prev := infraEngineCrashPoint
	infraEngineCrashPoint = func(p string) error {
		if p == "reverse R4 clear recorded" {
			return errors.New("killed")
		}
		return nil
	}
	code, _, _ := r.reverse(t, reverseOptions{Mode: reverseToRigs, Rigs: []string{"packs"}})
	infraEngineCrashPoint = prev
	if code == 0 {
		t.Fatal("the kill did not stop the reverse")
	}
	binding, err := openInfraDestination(r.target)
	if err != nil {
		t.Fatal(err)
	}
	owner, err := binding.Get("gp-step1")
	if err != nil {
		t.Fatal(err)
	}
	gate, err := binding.Create(dispatch.NewCrossStoreGate(beads.GraphStore{Store: binding}, owner, "gp-w1"))
	if err != nil {
		t.Fatal(err)
	}
	_ = closeBeadStoreHandle(binding)
	before := workStoreFingerprint(t, r.binding())
	code, _, stderr := r.reverse(t, reverseOptions{Mode: reverseToRigs, Rigs: []string{"packs"}})
	if code == 0 || !strings.Contains(stderr, "the binding holds "+gate.ID+", an open cross-store gate for gp-step1") || !strings.Contains(stderr, "nothing was removed") {
		t.Fatalf("the clear over an open gate of a returned row = %d: %s", code, stderr)
	}
	if workStoreFingerprint(t, r.binding()) != before {
		t.Fatal("the refused clear removed rows from the binding")
	}
}

// TestReverseContainmentCheckReadsNoneOfItsOwnRows: once the copy is in the
// work store, a resume's (or a dry run's) containment check leaves out the
// rows this run wrote before it classifies, so it reads the binding for none
// of them. The binding-born rows here are each one the check would
// otherwise judge by the binding's row under its id: one read apiece.
func TestReverseContainmentCheckReadsNoneOfItsOwnRows(t *testing.T) {
	city := newReverseEngineCity(t, 60, "")
	closeBindingGates(t, city)
	for i := 0; i < 5; i++ {
		createInBinding(t, city, beads.Bead{Title: fmt.Sprintf("born in the binding %d", i), Type: "task"}, true)
	}
	killReverseAt(t, city, "reverse phase "+reversePhaseName(reverseR2))
	reads := 0
	prev := getInfraHeldRow
	getInfraHeldRow = func(destination beads.Store, id string) (beads.Bead, error) {
		reads++
		return prev(destination, id)
	}
	t.Cleanup(func() { getInfraHeldRow = prev })
	if code, stdout, stderr := city.reverse(t, reverseOptions{DryRun: true}); code != 0 {
		t.Fatalf("--dry-run after R2 = %d: %s%s", code, stdout, stderr)
	}
	if reads != 0 {
		t.Fatalf("the dry run after R2 read the binding %d time(s) for rows this run wrote, want none", reads)
	}
	if code, stdout, stderr := city.reverse(t, reverseOptions{}); code != 0 {
		t.Fatalf("the resume after R2 = %d: %s%s", code, stdout, stderr)
	}
	if reads != 0 {
		t.Fatalf("the resume after R2 read the binding %d time(s) for rows this run wrote, want none", reads)
	}
}

// TestReverseRetainedCopiesRemedyNamesWhatTheClearRefuses: the one refusal of
// the retained copies names, before the clear, what the clear would refuse:
// a stranded row the work store also holds (settle it first), and copies the
// copy manifest does not record (record them with recover-stranded first).
func TestReverseRetainedCopiesRemedyNamesWhatTheClearRefuses(t *testing.T) {
	t.Run("a stranded row too", func(t *testing.T) {
		city := newRetainedReverseEngineCity(t, 60)
		closeBindingGates(t, city)
		under, _, _ := unwrapBeadPolicyStore(city.source)
		stray := mustCreateInfraBead(t, under, beads.Bead{Title: "mail the binding never served", Type: "message", Assignee: "worker"})
		code, stdout, stderr := city.reverse(t, reverseOptions{DryRun: true})
		if code == 0 || !strings.Contains(stderr, "retained pre-migration cop(ies)") || !strings.Contains(stderr, "the clear refuses while the work store holds a stranded row, so settle those first") || !strings.Contains(stderr, stray.ID) {
			t.Fatalf("--to-work --dry-run over retained copies and a stranded row = %d: %s%s", code, stdout, stderr)
		}
	})
	t.Run("copies the manifest does not record", func(t *testing.T) {
		city := newRetainedReverseEngineCity(t, 60)
		closeBindingGates(t, city)
		m, recorded, err := readInfraCopyManifestRecord(city.target)
		if err != nil || !recorded {
			t.Fatalf("the copy manifest = %v, %v", recorded, err)
		}
		var dropped string
		for _, id := range sortedMapKeys(m.IDs) {
			if b, ok := city.before[id]; ok && isInfra(b) {
				dropped = id
				break
			}
		}
		if dropped == "" {
			t.Fatal("no retained copy to leave out of the manifest")
		}
		var ids []string
		for _, id := range sortedMapKeys(m.IDs) {
			if id != dropped {
				ids = append(ids, id)
			}
		}
		if err := writeInfraCopyManifestFileAt(city.target.Root, city.target.ManifestPath(), ids, m.Created, m.WrittenAt, m.LegacyBound, m.Generation, nil); err != nil {
			t.Fatal(err)
		}
		code, stdout, stderr := city.reverse(t, reverseOptions{DryRun: true})
		if code == 0 || !strings.Contains(stderr, "the copy manifest does not record 1 of them ("+dropped+")") || !strings.Contains(stderr, storageRecoveryInstruction()) {
			t.Fatalf("--to-work --dry-run over an unrecorded retained copy = %d: %s%s", code, stdout, stderr)
		}
	})
}
