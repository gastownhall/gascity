package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/session"
)

// startedE2EWorkQuery is collapsedIdentityWorkQuery guarded so it finds no
// work when GC_INSTANCE_TOKEN reaches the work query's env: the fix must read
// the fence from the process env, not leak the secret into the child.
var startedE2EWorkQuery = strings.Replace(collapsedIdentityWorkQuery, "sh -c '",
	`sh -c 'if [ -n "$GC_INSTANCE_TOKEN" ]; then printf "[]"; exit 0; fi; `, 1)

// startedE2ECity is the collapsed-olivia city with the conditional writes the
// started write's CAS needs, and an active seat row of generation 1 holding
// token, its process env set the way a session start publishes it.
func startedE2ECity(t *testing.T, token, epoch string) (cityDir, sessionID string) {
	t.Helper()
	clearGCEnv(t)
	disableManagedDoltRecoveryForTest(t)
	t.Setenv("GC_BEADS", "file")
	cityDir = writeCollapsedIdentityCity(t)
	tomlPath := filepath.Join(cityDir, "city.toml")
	raw, err := os.ReadFile(tomlPath)
	if err != nil {
		t.Fatal(err)
	}
	cityToml := strings.Replace(string(raw), collapsedIdentityWorkQuery, startedE2EWorkQuery, 1)
	if cityToml == string(raw) {
		t.Fatal("city.toml does not carry collapsedIdentityWorkQuery")
	}
	cityToml += "\n[beads]\nconditional_writes = \"require\"\n"
	if err := os.WriteFile(tomlPath, []byte(cityToml), 0o644); err != nil {
		t.Fatal(err)
	}
	sessionID = newOliviaSessionBead(t, cityDir, map[string]string{
		"session_name":   "olivia-pool",
		"template":       "olivia",
		"alias":          "olivia",
		"agent_name":     "olivia",
		"state":          string(session.StateActive),
		"instance_token": token,
		"generation":     "1",
	})
	setCollapsedPoolClaimEnv(t, cityDir, sessionID, token)
	t.Setenv("GC_RUNTIME_EPOCH", epoch)
	return cityDir, sessionID
}

// runStartedE2EClaim runs `gc hook --claim --json` and wants exit code want
// (a drain without --drain-ack exits 1).
func runStartedE2EClaim(t *testing.T, want int) (hookClaimJSONResult, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	if code := cmdHookWithOptions(nil, hookCommandOptions{Claim: true, JSON: true}, &stdout, &stderr); code != want {
		t.Fatalf("code = %d, want %d; stdout=%q stderr=%s", code, want, stdout.String(), stderr.String())
	}
	var result hookClaimJSONResult
	if err := json.Unmarshal(bytes.TrimSpace(stdout.Bytes()), &result); err != nil {
		t.Fatalf("stdout is not a JSON claim result: %v\n%s", err, stdout.String())
	}
	return result, stderr.String()
}

// TestHookClaimRecordsWorkStartedThroughProductionEnv runs `gc hook --claim`
// through cmdHookWithOptions, so the claim env is the one production builds:
// mergeRuntimeEnv over os.Environ(), whose inherited-secret filter drops
// GC_INSTANCE_TOKEN. The started write must still be fenced by the seat's
// token and generation and land on the row, with no unrecorded event (#8080).
func TestHookClaimRecordsWorkStartedThroughProductionEnv(t *testing.T) {
	cityDir, sessionID := startedE2ECity(t, "live-token", "1")

	result, stderr := runStartedE2EClaim(t, 0)
	if result.Action != "work" || result.BeadID != "mc-olivia-1" {
		t.Fatalf("result = %+v, want action=work bead=mc-olivia-1; stderr=%s", result, stderr)
	}
	raw, err := hookSessionWorkStarted(sessionID)
	if err != nil {
		t.Fatalf("reading work_started: %v", err)
	}
	cell, err := session.DecodeWorkStarted(raw)
	if err != nil {
		t.Fatalf("decoding work_started %q: %v", raw, err)
	}
	if len(cell.Claims) != 1 || cell.Claims[0].Bead != "mc-olivia-1" || cell.Claims[0].Gen != 1 {
		t.Fatalf("work_started = %q, want one generation-1 entry for mc-olivia-1; stderr=%s", raw, stderr)
	}
	if strings.Contains(stderr, "started on session") {
		t.Errorf("stderr reports a failed started write:\n%s", stderr)
	}
	evs, err := os.ReadFile(filepath.Join(cityDir, ".gc", "events.jsonl"))
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if strings.Contains(string(evs), events.WorkStartedUnrecorded) {
		t.Errorf("events carry %s:\n%s", events.WorkStartedUnrecorded, evs)
	}
}

// TestHookClaimStartedFenceReadsProcessEpoch: the generation fences the
// started write too. A seat whose process env names generation 2 against a
// generation-1 row holding its token passes the token-only pre-claim fence and
// is refused at the started write.
func TestHookClaimStartedFenceReadsProcessEpoch(t *testing.T) {
	_, sessionID := startedE2ECity(t, "live-token", "2")

	result, stderr := runStartedE2EClaim(t, 1)
	if result.Action != "drain" || result.Reason != hookClaimReasonStaleSession {
		t.Fatalf("result = %+v, want a stale_session drain; stderr=%s", result, stderr)
	}
	if raw, err := hookSessionWorkStarted(sessionID); err != nil || raw != "" {
		t.Fatalf("work_started = %q (err %v), want none for a stale incarnation", raw, err)
	}
}
