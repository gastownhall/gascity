package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

// legacyHeldBeadHookCity writes a city whose work query offers one in_progress
// bead held under legacyAssignee, plus a fake bd that enforces the two rules
// that make up the #5716 loop: a conditional reassign (--if-assignee) only
// lands while the stored assignee matches, and a close is rejected with
// "assignee mismatch" (exit 13) unless BEADS_ACTOR equals the stored assignee.
// It returns the file holding the stored assignee.
func legacyHeldBeadHookCity(t *testing.T, beadID, legacyAssignee string) (cityDir, ownerPath string) {
	t.Helper()
	return legacyHeldBeadHookCityWithRestamp(t, beadID, legacyAssignee, true)
}

// legacyHeldBeadHookCityWithRestamp is legacyHeldBeadHookCity with control over
// the conditional reassign: when restampWorks is false, every update carrying
// --if-assignee fails with a transient "database is locked" error and leaves the
// stored assignee untouched, while show keeps reading back the legacy owner.
func legacyHeldBeadHookCityWithRestamp(t *testing.T, beadID, legacyAssignee string, restampWorks bool) (cityDir, ownerPath string) {
	t.Helper()
	cityDir = t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityDir, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	cityToml := fmt.Sprintf(`[workspace]
name = "test-city"

[[agent]]
name = "builder"
max_active_sessions = 3
work_query = "printf '[{\"id\":\"%s\",\"status\":\"in_progress\",\"assignee\":\"%s\",\"metadata\":{\"gc.routed_to\":\"builder\"}}]'"
`, beadID, legacyAssignee)
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(cityToml), 0o644); err != nil {
		t.Fatal(err)
	}

	fakeBin := t.TempDir()
	stateDir := t.TempDir()
	ownerPath = filepath.Join(stateDir, "owner")
	statusPath := filepath.Join(stateDir, "status")
	if err := os.WriteFile(ownerPath, []byte(legacyAssignee), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(statusPath, []byte("in_progress"), 0o644); err != nil {
		t.Fatal(err)
	}
	restampFails := "0"
	if !restampWorks {
		restampFails = "1"
	}
	script := fmt.Sprintf(`#!/bin/sh
owner=$(cat %[1]q)
status=$(cat %[2]q)
case "$1" in
show)
  printf '[{"id":"%[3]s","status":"%%s","assignee":"%%s"}]' "$status" "$owner"
  exit 0 ;;
close)
  if [ "$BEADS_ACTOR" != "$owner" ]; then
    echo "Error closing %[3]s: assignee mismatch" >&2
    exit 13
  fi
  printf 'closed' > %[2]q
  printf '[{"id":"%[3]s","status":"closed"}]'
  exit 0 ;;
update)
  ifassignee=""; to=""; prev=""
  for arg in "$@"; do
    case "$prev" in
      --if-assignee) ifassignee="$arg" ;;
      --assignee) to="$arg" ;;
    esac
    prev="$arg"
  done
  if [ -n "$ifassignee" ]; then
    if [ "%[4]s" = "1" ]; then
      echo "Error updating %[3]s: database is locked" >&2
      exit 1
    fi
    if [ "$ifassignee" != "$owner" ]; then
      echo "Error updating %[3]s: assignee mismatch" >&2
      exit 13
    fi
    printf '%%s' "$to" > %[1]q
  fi
  printf '[]'
  exit 0 ;;
esac
printf '[]'
`, ownerPath, statusPath, beadID, restampFails)
	if err := os.WriteFile(filepath.Join(fakeBin, "bd"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("GC_CITY", cityDir)
	return cityDir, ownerPath
}

// The upgrade shape of #5716. A bead claimed before the upgrade under the pool
// session_name (claude-<beadID> / the slot label) is still in_progress when the
// SAME session bead respawns. The respawned env now says BEADS_ACTOR=<beadID>.
// Adoption must move the stored assignee to <beadID> (with a CAS naming the old
// spelling) before handing the bead over, or every later close is rejected and
// the worker loops.
func TestCmdHookClaimRestampsLegacySpellingOnAdoption(t *testing.T) {
	clearGCEnv(t)
	disableManagedDoltRecoveryForTest(t)
	const (
		beadID    = "ga-held1"
		sessionID = "gcg-session-557fc1017792caa9a01355325b212416"
	)
	legacy := "claude-" + sessionID
	cityDir, ownerPath := legacyHeldBeadHookCity(t, beadID, legacy)

	// The respawned unaliased pool worker's runtime env after #6324.
	t.Setenv("GC_TEMPLATE", "builder")
	t.Setenv("GC_ALIAS", "")
	t.Setenv("GC_AGENT", sessionID)
	t.Setenv("BEADS_ACTOR", sessionID)
	t.Setenv("GC_SESSION_NAME", legacy)
	t.Setenv("GC_SESSION_ID", sessionID)

	var stdout, stderr bytes.Buffer
	code := cmdHookWithOptions(nil, hookCommandOptions{Claim: true, JSON: true}, &stdout, &stderr)
	var result hookClaimJSONResult
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatalf("stdout is not JSON (code %d): %v\nraw: %s\nstderr: %s", code, err, stdout.String(), stderr.String())
	}
	if result.Action != "work" || result.BeadID != beadID || result.Reason != "existing_assignment" {
		t.Fatalf("result = %+v (code %d), want adoption of %q; stderr: %s", result, code, beadID, stderr.String())
	}
	if result.Assignee != sessionID {
		t.Fatalf("adopted assignee = %q, want the session bead id %q", result.Assignee, sessionID)
	}
	owner, err := os.ReadFile(ownerPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(string(owner)); got != sessionID {
		t.Fatalf("stored assignee after adoption = %q, want %q (legacy spelling %q must be re-stamped)", got, sessionID, legacy)
	}

	// And the worker's own close, actored by its runtime BEADS_ACTOR, now lands.
	if err := hookClaimBdStoreContext(context.Background(), cityDir, nil, sessionID).Close(beadID); err != nil {
		t.Fatalf("bd close as the respawned worker: %v", err)
	}
}

// The refusal shape of the same upgrade: the legacy spelling is canonical (the
// readback succeeds), but the work store fails the re-stamp. bd would still
// fence this worker's close on the legacy spelling, so adopting the bead would
// hand over the #5716 loop. It must not be adopted, the stored spelling must be
// left alone, and stderr must name the manual recovery.
func TestCmdHookClaimRefusesAdoptionWhenWorkStoreRestampFails(t *testing.T) {
	clearGCEnv(t)
	disableManagedDoltRecoveryForTest(t)
	const (
		beadID    = "ga-held1"
		sessionID = "gcg-session-557fc1017792caa9a01355325b212416"
	)
	legacy := "claude-" + sessionID
	_, ownerPath := legacyHeldBeadHookCityWithRestamp(t, beadID, legacy, false)

	t.Setenv("GC_TEMPLATE", "builder")
	t.Setenv("GC_ALIAS", "")
	t.Setenv("GC_AGENT", sessionID)
	t.Setenv("BEADS_ACTOR", sessionID)
	t.Setenv("GC_SESSION_NAME", legacy)
	t.Setenv("GC_SESSION_ID", sessionID)

	var stdout, stderr bytes.Buffer
	code := cmdHookWithOptions(nil, hookCommandOptions{Claim: true, JSON: true}, &stdout, &stderr)
	var result hookClaimJSONResult
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatalf("stdout is not JSON (code %d): %v\nraw: %s\nstderr: %s", code, err, stdout.String(), stderr.String())
	}
	if result.Action == "work" && result.BeadID == beadID {
		t.Fatalf("result = %+v (code %d), want %q NOT adopted after a failed work-store re-stamp; stderr: %s", result, code, beadID, stderr.String())
	}
	owner, err := os.ReadFile(ownerPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(string(owner)); got != legacy {
		t.Fatalf("stored assignee after refused adoption = %q, want the legacy spelling %q left in place", got, legacy)
	}
	if !strings.Contains(stderr.String(), "not adopting "+beadID) {
		t.Fatalf("stderr does not report the refusal; stderr: %s", stderr.String())
	}
	recovery := fmt.Sprintf("bd update %s --if-assignee %q --if-status in_progress --assignee %q", beadID, legacy, sessionID)
	if !strings.Contains(stderr.String(), recovery) {
		t.Fatalf("stderr does not name the manual recovery %q; stderr: %s", recovery, stderr.String())
	}
}

// restampHookAdoption's decision table, driven through the ops seam.
func TestRestampHookAdoption(t *testing.T) {
	const (
		sessionID = "gcg-session-1"
		legacy    = "claude-gcg-session-1"
	)
	bead := beads.Bead{ID: "ga-1", Status: "in_progress", Assignee: legacy}
	for _, tc := range []struct {
		name            string
		actor           string
		canonical       string
		verdict         hookAdoptionVerdict
		moved           bool
		err             error
		wantCalls       int
		wantAdopt       bool
		wantAssignee    string
		wantRecovery    bool
		wantStderrHas   string
		wantStderrLacks string
	}{
		{name: "legacy spelling is re-stamped", actor: sessionID, canonical: legacy, moved: true, wantCalls: 1, wantAdopt: true, wantAssignee: sessionID},
		{name: "lost CAS is not adopted", actor: sessionID, canonical: legacy, moved: false, wantCalls: 1, wantAdopt: false},
		// A work-store bead whose re-stamp failed is NOT adopted: bd fences its
		// close on the spelling that could not be moved, so adopting it hands
		// the worker the #5716 loop instead of a recoverable refusal.
		{name: "failed CAS on a work-store bead is not adopted", actor: sessionID, canonical: legacy, err: errors.New("boom"), wantCalls: 1, wantAdopt: false, wantRecovery: true},
		{name: "unsupported CAS on a work-store bead is not adopted", actor: sessionID, canonical: legacy, err: beads.ErrConditionalTransferUnsupported, wantCalls: 1, wantAdopt: false, wantRecovery: true},
		// The same unsupported error from the class route means the opposite:
		// nothing fences a graph-resident close, so the bead is adopted and no
		// recovery command is prescribed (none exists for a gcg- id).
		{name: "graph-resident route adopts as-is", actor: sessionID, canonical: legacy, err: errRestampGraphResident, wantCalls: 1, wantAdopt: true, wantAssignee: legacy, wantStderrHas: "graph-resident", wantStderrLacks: "--if-assignee"},
		{name: "already the claim identity", actor: sessionID, canonical: sessionID, wantCalls: 0, wantAdopt: true, wantAssignee: legacy},
		{name: "actor differs from claim identity (manual session)", actor: legacy, canonical: legacy, wantCalls: 0, wantAdopt: true, wantAssignee: legacy},
		{name: "no actor in env", actor: "", canonical: legacy, wantCalls: 0, wantAdopt: true, wantAssignee: legacy},
		{name: "unverified readback uses the query row", actor: sessionID, canonical: "", verdict: hookAdoptionUnverified, moved: true, wantCalls: 1, wantAdopt: true, wantAssignee: sessionID},
		// An unreadable readback makes `current` the work query's own row, so a
		// failed re-stamp proves nothing and stays fail-open, loudly.
		{name: "failed CAS on an unverified readback adopts as-is", actor: sessionID, canonical: "", verdict: hookAdoptionUnverified, err: errors.New("boom"), wantCalls: 1, wantAdopt: true, wantAssignee: legacy, wantRecovery: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			ops := hookClaimOps{RestampAdopted: func(_ context.Context, _ string, _ []string, beadID, from, to string) (bool, error) {
				calls++
				if beadID != bead.ID || from != legacy || to != sessionID {
					t.Fatalf("RestampAdopted(%q, %q, %q), want (%q, %q, %q)", beadID, from, to, bead.ID, legacy, sessionID)
				}
				return tc.moved, tc.err
			}}
			opts := hookClaimOptions{Assignee: sessionID, RuntimeActor: tc.actor}
			var stderr bytes.Buffer
			got, adopt := restampHookAdoption(bead, tc.canonical, tc.verdict, opts, ops, "/city", &stderr)
			if calls != tc.wantCalls {
				t.Fatalf("RestampAdopted calls = %d, want %d", calls, tc.wantCalls)
			}
			if adopt != tc.wantAdopt {
				t.Fatalf("adopt = %v, want %v; stderr: %s", adopt, tc.wantAdopt, stderr.String())
			}
			if adopt && got.Assignee != tc.wantAssignee {
				t.Fatalf("assignee = %q, want %q", got.Assignee, tc.wantAssignee)
			}
			if tc.wantRecovery {
				want := fmt.Sprintf("bd update %s --if-assignee %q --if-status in_progress --assignee %q", bead.ID, legacy, sessionID)
				if !strings.Contains(stderr.String(), want) {
					t.Fatalf("warning does not name the manual recovery %q; stderr: %s", want, stderr.String())
				}
			}
			if tc.wantStderrHas != "" && !strings.Contains(stderr.String(), tc.wantStderrHas) {
				t.Fatalf("stderr does not mention %q; stderr: %s", tc.wantStderrHas, stderr.String())
			}
			if tc.wantStderrLacks != "" && strings.Contains(stderr.String(), tc.wantStderrLacks) {
				t.Fatalf("stderr prescribes %q for a bead that needs no recovery; stderr: %s", tc.wantStderrLacks, stderr.String())
			}
		})
	}
}
