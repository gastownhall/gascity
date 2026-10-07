package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/storeref"
)

// initWorkRecordGateRepo makes dir a git repository holding one commit on main.
// A row that claims a commit is reachable on one checkout and not on another
// has to own two real repositories; a stubbed oracle would only prove the stub.
func initWorkRecordGateRepo(t *testing.T, dir, content string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("creating the checkout at %s: %v", dir, err)
	}
	runGit(t, dir, "init", "--initial-branch=main")
	runGit(t, dir, "config", "user.name", "Gas City Test")
	runGit(t, dir, "config", "user.email", "gc-test@test.local")
	if err := os.WriteFile(filepath.Join(dir, "artifact.txt"), []byte(content+"\n"), 0o644); err != nil {
		t.Fatalf("writing the artifact in %s: %v", dir, err)
	}
	runGit(t, dir, "add", "artifact.txt")
	runGit(t, dir, "commit", "-m", "test: "+content)
}

// appendRigCheckout registers a rig with a checkout in an already-written
// city.toml. The class binding the fixture resolved is city-scope and keyed off
// [storage] alone, so a rig is orthogonal to it — but the close gate reads the
// rig table to learn which checkout a rig-owned bead answers to.
func appendRigCheckout(t *testing.T, cityPath, name, path string) {
	t.Helper()
	tomlPath := filepath.Join(cityPath, "city.toml")
	body, err := os.ReadFile(tomlPath)
	if err != nil {
		t.Fatalf("reading %s: %v", tomlPath, err)
	}
	body = append(body, []byte(fmt.Sprintf("\n[[rigs]]\nname = %s\npath = %s\n",
		strconv.Quote(name), strconv.Quote(path)))...)
	if err := os.WriteFile(tomlPath, body, 0o644); err != nil {
		t.Fatalf("writing %s: %v", tomlPath, err)
	}
}

// seedClassResidentWorkRecord plants a work-shaped bead carrying a work record
// into the class binding only, the way `gc storage migrate` leaves one behind.
func seedClassResidentWorkRecord(t *testing.T, classStore beads.Store, id string, metadata map[string]string) beads.Bead {
	t.Helper()
	created, err := migrationSeed(classStore, beads.Bead{
		ID:       id,
		Title:    "a work step the binding holds",
		Type:     "task",
		Metadata: beads.StringMap(metadata),
	})
	if err != nil {
		t.Fatalf("seeding %s in the class binding: %v", id, err)
	}
	if bdIDIsClassReserved(created.ID) {
		t.Fatalf("the fixture id %q carries a reserved class prefix; it cannot exercise the residence probe", created.ID)
	}
	return created
}

// TestBdCloseClassResidentAsksTheOwningRigsCheckout is the CLI door's half of
// the residency rule. The class door hands its gate the CITY checkout for every
// bead it serves, because a binding is a store rather than a checkout — but a
// bead whose gc.root_store_ref names a rig was worked in the RIG's checkout, and
// its commit lives there. Asking the city repository reports a landed commit as
// unreachable, which under enforcement refuses a close that satisfied the
// contract.
//
// Both checkouts are real repositories and only the rig's holds the commit, so
// "asked the wrong repository" is distinguishable from "asked no repository".
func TestBdCloseClassResidentAsksTheOwningRigsCheckout(t *testing.T) {
	cityPath, classStore := foreignProviderCity(t)
	rigRepo := t.TempDir()
	initWorkRecordGateRepo(t, rigRepo, "rig work")
	commit := strings.TrimSpace(runGit(t, rigRepo, "rev-parse", "HEAD"))
	initWorkRecordGateRepo(t, cityPath, "city work")
	appendRigCheckout(t, cityPath, "r1", rigRepo)
	// Set enforcement AFTER foreignProviderCity: clearGCEnv wipes live GC_* keys.
	t.Setenv(workRecordEnforceEnvVar, "1")

	owner := string(storeref.RigRef("r1"))
	landed := seedClassResidentWorkRecord(t, classStore, "gc-rigowned1", map[string]string{
		beadmeta.RootStoreRefMetadataKey: owner,
		beadmeta.WorkOutcomeMetadataKey:  beadmeta.WorkOutcomeShipped,
		beadmeta.WorkCommitMetadataKey:   commit,
		beadmeta.WorkBranchMetadataKey:   "main",
	})

	var stdout, stderr bytes.Buffer
	code, handled := maybeRouteBdByID(cityPath, "", []string{"close", landed.ID}, &stdout, &stderr)
	if !handled {
		t.Fatalf("a class-resident close fell through to the bd subprocess: %q", stderr.String())
	}
	if code != 0 {
		t.Fatalf("close of a rig-owned bead whose commit is on the rig checkout exited %d: %s", code, stderr.String())
	}
	if strings.Contains(stderr.String(), "work-record gate") {
		t.Errorf("a compliant close tripped the gate: %q", stderr.String())
	}
	after, err := classStore.Get(landed.ID)
	if err != nil {
		t.Fatalf("re-reading %s: %v", landed.ID, err)
	}
	if after.Status != "closed" {
		t.Errorf("the compliant close did not retire the class row; status=%q", after.Status)
	}
}

// TestBdCloseClassResidentBlocksACommitNoCheckoutHolds is the control on the row
// above: resolving the owning rig's checkout is not a license to stop asking.
// A shipped close whose commit exists in neither repository is still refused.
func TestBdCloseClassResidentBlocksACommitNoCheckoutHolds(t *testing.T) {
	cityPath, classStore := foreignProviderCity(t)
	rigRepo := t.TempDir()
	initWorkRecordGateRepo(t, rigRepo, "rig work")
	initWorkRecordGateRepo(t, cityPath, "city work")
	appendRigCheckout(t, cityPath, "r1", rigRepo)
	t.Setenv(workRecordEnforceEnvVar, "1")

	nowhere := seedClassResidentWorkRecord(t, classStore, "gc-rignowhere1", map[string]string{
		beadmeta.RootStoreRefMetadataKey: string(storeref.RigRef("r1")),
		beadmeta.WorkOutcomeMetadataKey:  beadmeta.WorkOutcomeShipped,
		beadmeta.WorkCommitMetadataKey:   "0000000000000000000000000000000000000000",
		beadmeta.WorkBranchMetadataKey:   "main",
	})

	var stdout, stderr bytes.Buffer
	code, handled := maybeRouteBdByID(cityPath, "", []string{"close", nowhere.ID}, &stdout, &stderr)
	if !handled {
		t.Fatalf("a class-resident close fell through to the bd subprocess: %q", stderr.String())
	}
	if code != 1 {
		t.Fatalf("close of a commit no checkout holds exited %d, want 1 (blocked): %s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "not reachable") {
		t.Errorf("the block does not name the unreachable commit: %q", stderr.String())
	}
	after, err := classStore.Get(nowhere.ID)
	if err != nil {
		t.Fatalf("re-reading %s: %v", nowhere.ID, err)
	}
	if after.Status == "closed" {
		t.Errorf("the blocked close wrote to the class binding anyway; status=%q", after.Status)
	}
}

// TestEvaluateWorkRecordCloseGateDegradesForAnUnknownOwner converges the CLI
// door on the HTTP door's honest degrade. When the bead names an owner this
// plane can point at no checkout for, there is no repository to ask: the
// reachability clause degrades to a warning and says so, rather than failing
// closed on a question the door cannot pose. The control is the clause that
// never degrades — a bead with no outcome at all is still refused, because "the
// commit could not be checked" is not a reason to accept a close that recorded
// nothing.
func TestEvaluateWorkRecordCloseGateDegradesForAnUnknownOwner(t *testing.T) {
	dirs := workRecordRepoDirs{
		cityPath: "/city",
		legacy:   "/city",
		rigs:     func() []config.Rig { return nil },
	}
	shipped := map[string]beads.Bead{
		"wr-ghost-owned": {ID: "wr-ghost-owned", Type: "task", Status: "in_progress", Metadata: beads.StringMap{
			beadmeta.RootStoreRefMetadataKey: string(storeref.RigRef("ghost")),
			beadmeta.WorkOutcomeMetadataKey:  beadmeta.WorkOutcomeShipped,
			beadmeta.WorkCommitMetadataKey:   "0000000000000000000000000000000000000000",
			beadmeta.WorkBranchMetadataKey:   "main",
		}},
	}
	var stderr strings.Builder
	if block := evaluateWorkRecordCloseGate([]string{"close", "wr-ghost-owned"}, panicOnGetStore{}, shipped, dirs, true, &stderr); block {
		t.Fatalf("an unanswerable reachability clause blocked the close: %s", stderr.String())
	}
	if out := stderr.String(); !strings.Contains(out, "reachability unverified") {
		t.Fatalf("gate output %q does not report the degraded clause", out)
	}

	recordless := map[string]beads.Bead{
		"wr-ghost-recordless": {ID: "wr-ghost-recordless", Type: "task", Status: "in_progress", Metadata: beads.StringMap{
			beadmeta.RootStoreRefMetadataKey: string(storeref.RigRef("ghost")),
		}},
	}
	var refusal strings.Builder
	if block := evaluateWorkRecordCloseGate([]string{"close", "wr-ghost-recordless"}, panicOnGetStore{}, recordless, dirs, true, &refusal); !block {
		t.Fatalf("an outcome-less close was accepted because the repository was unknown: %s", refusal.String())
	}
	if out := refusal.String(); !strings.Contains(out, "missing "+beadmeta.WorkOutcomeMetadataKey) {
		t.Fatalf("gate output %q does not name the missing outcome", out)
	}
}
