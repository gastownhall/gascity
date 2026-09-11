package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/session/sessiontest"
)

// A claim must name the OCCUPANT of a pool slot, not the slot.
//
// clearPoolTemplateRuntimeIdentity gives an unaliased pool spawn a blank
// GC_ALIAS and stamps GC_AGENT with the slot-derived runtime session name. That
// name is a chair: every session that ever sits in the slot reuses it. On
// maintainer-city one such label ("gascity--gc__implementation-reviewer-1-pool")
// was the session_name of 24 distinct session beads, and the worst offender of
// 66. A claim recorded under it answers "is the holder alive?" about the chair,
// so a dead occupant's in_progress bead looks held by whoever sits there next
// and is never released, resumed, or replaced.
//
// The environment already carries the occupant: GC_SESSION_ID is the session
// bead id, and every reader already treats it as the first identity
// (sessionBeadAssigneeIdentities, currentSessionAssigneeIdentities,
// ComputeAwakeSet all lead with bead.ID). Only the WRITER was on the chair.

// The precedence contract on its own, in the five environment shapes a claim
// actually runs in. The rows are ordered by how much identity the environment
// carries, so the fallback chain reads top to bottom.
func TestHookClaimAssigneeIdentityPrecedence(t *testing.T) {
	const (
		slot      = "gascity--gc__implementation-reviewer-1-pool"
		sessionID = "gcg-session-557fc1017792caa9a01355325b212416"
	)
	tests := []struct {
		name                                                   string
		alias, sessionID, agentForQuery, resolved, sessionName string
		want                                                   string
	}{{
		// clearPoolTemplateRuntimeIdentity's output: blank alias, GC_AGENT and
		// GC_SESSION_NAME both on the reused slot label.
		name:  "unaliased pool worker takes its session bead id",
		alias: "", sessionID: sessionID, agentForQuery: slot,
		resolved: "gascity/gc__implementation-reviewer", sessionName: slot,
		want: sessionID,
	}, {
		// A namepool member or named holder. Its alias is a configured identity
		// an aliasless later invocation must still reach, so it outranks the id.
		name:  "aliased session keeps its alias",
		alias: "gascity/gc__mayor", sessionID: sessionID, agentForQuery: "gascity/gc__mayor",
		resolved: "gascity/gc__mayor", sessionName: "gascity--gc__mayor",
		want: "gascity/gc__mayor",
	}, {
		// `gc hook <agent>` outside a session: sessionTemplateContext is false, so
		// cmdHookWithOptions blanks GC_SESSION_ID and puts the resolved name on
		// the alias. The fallback chain is the only thing left and is unchanged.
		name:  "explicit target outside a session falls back to the resolved name",
		alias: "test-city/builder", sessionID: "", agentForQuery: "test-city/builder",
		resolved: "test-city/builder", sessionName: "test-city--builder",
		want: "test-city/builder",
	}, {
		// An unaliased worker whose environment carries no session bead id at
		// all — a shell that lost GC_SESSION_ID. It must still claim under
		// SOMETHING rather than fail the empty-assignee guard in tryHookClaim.
		name:  "unaliased worker with no session id keeps the agent form",
		alias: "", sessionID: "", agentForQuery: slot,
		resolved: "gascity/gc__implementation-reviewer", sessionName: slot,
		want: slot,
	}, {
		name:  "nothing but a session name still claims",
		alias: "", sessionID: "", agentForQuery: "", resolved: "", sessionName: slot,
		want: slot,
	}}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := hookClaimAssigneeIdentity(tc.alias, tc.sessionID, tc.agentForQuery, tc.resolved, tc.sessionName)
			if got != tc.want {
				t.Fatalf("hookClaimAssigneeIdentity = %q, want %q", got, tc.want)
			}
		})
	}
}

// unaliasedPoolWorkerHookCity writes a city whose single agent yields one
// unassigned, route-matched bead, plus a fake bd that records the BEADS_ACTOR
// each subprocess ran under — the channel the claim's assignee travels on
// (hookClaimEnvMap). It returns the bd log path.
func unaliasedPoolWorkerHookCity(t *testing.T, beadID string) string {
	t.Helper()
	cityDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityDir, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	cityToml := fmt.Sprintf(`[workspace]
name = "test-city"

[[agent]]
name = "builder"
max_active_sessions = 3
work_query = "printf '[{\"id\":\"%s\",\"status\":\"open\",\"assignee\":\"\",\"metadata\":{\"gc.routed_to\":\"builder\"}}]'"
`, beadID)
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(cityToml), 0o644); err != nil {
		t.Fatal(err)
	}

	fakeBin := t.TempDir()
	stateDir := t.TempDir()
	logPath := filepath.Join(stateDir, "bd.log")
	ownerPath := filepath.Join(stateDir, "owner")
	// `bd update <id> --claim --json` is the claim mutation (BdStore.Claim); it
	// takes its actor implicitly from BEADS_ACTOR, so echoing that back as the
	// claimed assignee is what a real bd does and what lets the claim be
	// accepted as ours (hookClaimThroughStore). The owner file makes the
	// subsequent canonical readback agree with the mutation.
	script := fmt.Sprintf(`#!/bin/sh
printf '%%s\t%%s\n' "$BEADS_ACTOR" "$*" >> %q
for arg in "$@"; do
  if [ "$arg" = "--claim" ]; then
    printf '%%s' "$BEADS_ACTOR" > %q
    printf '{"id":"%s","status":"in_progress","assignee":"%%s"}' "$BEADS_ACTOR"
    exit 0
  fi
done
if [ "$1" = "show" ] && [ -f %q ]; then
  printf '[{"id":"%s","status":"in_progress","assignee":"%%s"}]' "$(cat %q)"
  exit 0
fi
printf '[]'
`, logPath, ownerPath, beadID, ownerPath, beadID, ownerPath)
	if err := os.WriteFile(filepath.Join(fakeBin, "bd"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("GC_CITY", cityDir)
	return logPath
}

// The production shape. An unaliased pool worker must claim under its session
// bead id, and the slot label must not appear as an actor on any bd subprocess.
func TestCmdHookClaimUnaliasedPoolWorkerClaimsUnderItsSessionBeadID(t *testing.T) {
	clearGCEnv(t)
	disableManagedDoltRecoveryForTest(t)
	const (
		beadID    = "ga-fresh1"
		slotLabel = "test-city--builder-1-pool"
		sessionID = "gcg-session-557fc1017792caa9a01355325b212416"
	)
	logPath := unaliasedPoolWorkerHookCity(t, beadID)

	// clearPoolTemplateRuntimeIdentity's exact output: no alias, an explicitly
	// BLANK GC_ALIAS, and GC_AGENT on the slot-derived runtime session name.
	t.Setenv("GC_TEMPLATE", "builder")
	t.Setenv("GC_ALIAS", "")
	t.Setenv("GC_AGENT", slotLabel)
	t.Setenv("GC_SESSION_NAME", slotLabel)
	t.Setenv("GC_SESSION_ID", sessionID)

	var stdout, stderr bytes.Buffer
	code := cmdHookWithOptions(nil, hookCommandOptions{Claim: true, JSON: true}, &stdout, &stderr)

	var result hookClaimJSONResult
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatalf("stdout is not JSON (code %d): %v\nraw: %s\nstderr: %s", code, err, stdout.String(), stderr.String())
	}
	if result.Action != "work" || result.BeadID != beadID {
		t.Fatalf("result = %+v (code %d), want a fresh claim of %q; stderr: %s", result, code, beadID, stderr.String())
	}
	if result.Assignee != sessionID {
		t.Fatalf("claim assignee = %q, want the session bead id %q — a slot label is a chair reused by every occupant, so a claim recorded under it names no live worker", result.Assignee, sessionID)
	}

	logData, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("ReadFile(%s): %v", logPath, err)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(logData)), "\n") {
		actor, args, _ := strings.Cut(line, "\t")
		if actor == slotLabel {
			t.Fatalf("bd ran under the slot label as BEADS_ACTOR (args: %s); the whole log:\n%s", args, logData)
		}
	}
}

// Control: a named holder / canonical slot keeps claiming under its alias.
//
// A non-empty GC_ALIAS means clearPoolTemplateRuntimeIdentity did NOT run, so
// the alias is a real occupant identity that read paths query through GC_AGENT
// and that a later `gc hook <agent>` from a fresh shell — which has no
// GC_SESSION_ID at all — must still resolve to. Moving the session id ahead of
// the alias would strand exactly that adoption, which is why the reorder stops
// short of it.
func TestCmdHookClaimAliasedSessionStillClaimsUnderItsAlias(t *testing.T) {
	clearGCEnv(t)
	disableManagedDoltRecoveryForTest(t)
	const (
		beadID    = "ga-fresh1"
		alias     = "builder-3"
		sessionID = "gcg-session-aaaabbbbccccdddd"
	)
	unaliasedPoolWorkerHookCity(t, beadID)

	t.Setenv("GC_TEMPLATE", "builder")
	t.Setenv("GC_ALIAS", alias)
	t.Setenv("GC_AGENT", alias)
	t.Setenv("GC_SESSION_NAME", "test-city--builder-3")
	t.Setenv("GC_SESSION_ID", sessionID)

	var stdout, stderr bytes.Buffer
	code := cmdHookWithOptions(nil, hookCommandOptions{Claim: true, JSON: true}, &stdout, &stderr)

	var result hookClaimJSONResult
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatalf("stdout is not JSON (code %d): %v\nraw: %s\nstderr: %s", code, err, stdout.String(), stderr.String())
	}
	if result.Action != "work" || result.BeadID != beadID {
		t.Fatalf("result = %+v (code %d), want a fresh claim of %q; stderr: %s", result, code, beadID, stderr.String())
	}
	if result.Assignee != alias {
		t.Fatalf("claim assignee = %q, want the alias %q unchanged", result.Assignee, alias)
	}
}

// TestPoolWorkerClaimIdentityDistinguishesOccupantsOfOneSlot is the guard the
// existing pool-slot pins could not be: an ABSOLUTE property rather than a
// relative one.
//
// The pins in pool_slot_unaliased_test.go compare one derived value against
// another derived value ("GC_AGENT == tp.SessionName") or against a literal
// fixture already written in the good shape. Both stayed green through #5372,
// which changed poolRuntimeSessionName from a bead-ID-bearing name to the
// slot-derived "<identity>-pool" step-aside. GC_AGENT still equalled the
// session name; the session name had simply stopped identifying an occupant.
//
// The invariant that does not depend on any naming scheme: a pool slot is a
// CHAIR, and every session that sits in it spawns with the same runtime name.
// Two distinct incarnations must therefore never claim work under the same
// string, or "is the holder still alive?" becomes a question about the chair
// and every reader answers yes while the occupant is gone.
//
// So: seed two successive occupants of ONE derived chair, run each through the
// real runtime-env projection, and require the claim identities to differ.
// Any assignee that is a pure function of (config, identity, template, slot)
// fails this by construction, whatever that function is.
func TestPoolWorkerClaimIdentityDistinguishesOccupantsOfOneSlot(t *testing.T) {
	cfg := transientSlotPoolConfig()

	// The chair, derived the way production derives it -- not a literal.
	chair := poolRuntimeSessionName(cfg, "rig/claude-1", "rig/claude", true)
	if strings.TrimSpace(chair) == "" {
		t.Fatal("poolRuntimeSessionName returned an empty runtime name")
	}

	seen := map[string]string{}
	for _, beadID := range []string{"gcg-session-first", "gcg-session-second"} {
		info := sessiontest.SeedBead(t, beads.Bead{
			ID:     beadID,
			Type:   sessionBeadType,
			Status: "open",
			Labels: []string{sessionBeadLabel, "agent:rig/claude-1"},
			Metadata: map[string]string{
				"template":             "rig/claude",
				"agent_name":           "rig/claude-1",
				"session_name":         chair,
				"pool_slot":            "1",
				poolManagedMetadataKey: boolMetadata(true),
			},
		})

		env := sessionpkg.RuntimeEnvWithSessionContext(info, 1, 1, "tok")
		if env["GC_SESSION_ID"] != beadID {
			t.Fatalf("%s: GC_SESSION_ID = %q, want the session bead id -- the claim reads the occupant from here",
				beadID, env["GC_SESSION_ID"])
		}

		assignee := hookClaimAssigneeIdentity(env["GC_ALIAS"], env["GC_SESSION_ID"], env["GC_AGENT"], "", env["GC_SESSION_NAME"])
		if assignee == chair {
			t.Fatalf("%s claimed under the chair %q: that string is shared by every occupant of pool slot 1, "+
				"so a liveness read about the holder answers about the slot instead", beadID, assignee)
		}
		if assignee != beadID {
			t.Fatalf("%s: claim assignee = %q, want the session bead id -- sessionBeadAssigneeIdentities leads with "+
				"bead.ID, so it is the one form every reader resolves back to a single incarnation", beadID, assignee)
		}
		if prior, dup := seen[assignee]; dup {
			t.Fatalf("occupants %s and %s both claim under %q: distinct incarnations of one chair must be distinguishable",
				prior, beadID, assignee)
		}
		seen[assignee] = beadID
	}
}
