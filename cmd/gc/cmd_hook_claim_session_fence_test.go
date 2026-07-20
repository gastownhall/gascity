package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/session"
)

// writeFenceTestCity writes a minimal single-worker city and returns its dir.
func writeFenceTestCity(t *testing.T) string {
	t.Helper()
	cityDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityDir, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(`[workspace]
name = "test-city"

[[agent]]
name = "worker"
`), 0o644); err != nil {
		t.Fatal(err)
	}
	return cityDir
}

// newFenceSessionBead creates a session bead in the city store with the given
// runtime state and instance token, returning its id.
func newFenceSessionBead(t *testing.T, cityDir string, state session.State, instanceToken string) string {
	t.Helper()
	store, err := openCityStoreAt(cityDir)
	if err != nil {
		t.Fatalf("openCityStoreAt: %v", err)
	}
	bead, err := store.Create(beads.Bead{
		Title:  "worker-1",
		Type:   session.BeadType,
		Labels: []string{"gc:session", "agent:worker-1"},
		Metadata: map[string]string{
			"session_name":   "worker-1",
			"template":       "worker",
			"state":          string(state),
			"instance_token": instanceToken,
		},
	})
	if err != nil {
		t.Fatalf("create session bead: %v", err)
	}
	return bead.ID
}

// installFenceWorkQueryProbe puts a fake `bd` on PATH that records each
// invocation by touching the returned marker path and prints an empty
// work-query result, so a test can assert whether the claim fence reached the
// work query. Call it AFTER creating any session beads so bead setup uses the
// real store, not the probe.
func installFenceWorkQueryProbe(t *testing.T) string {
	t.Helper()
	fakeBin := t.TempDir()
	queryMarker := filepath.Join(t.TempDir(), "query-ran")
	fakeBD := filepath.Join(fakeBin, "bd")
	if err := os.WriteFile(fakeBD, []byte("#!/bin/sh\ntouch \"$QUERY_MARKER\"\nprintf '[]'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("QUERY_MARKER", queryMarker)
	return queryMarker
}

// setFenceClaimEnv points cmdHookWithOptions at the given session identity.
func setFenceClaimEnv(t *testing.T, cityDir, sessionID, instanceToken string) {
	t.Helper()
	t.Setenv("GC_CITY", cityDir)
	t.Setenv("GC_TEMPLATE", "worker")
	t.Setenv("GC_ALIAS", "worker-1")
	t.Setenv("GC_SESSION_ID", sessionID)
	t.Setenv("GC_SESSION_NAME", "worker-1")
	t.Setenv("GC_SESSION_ORIGIN", "ephemeral")
	t.Setenv("GC_INSTANCE_TOKEN", instanceToken)
}

// TestHookCommandClaimStaleSessionDrainsBeforeWorkQuery proves a definitively
// stale session (here failed-create) is refused before the work query AND that
// the refusal now honors the gc hook --claim result contract: a --json caller
// gets a structured terminal drain record (action=drain, reason=stale_session)
// instead of empty stdout, so a startup wrapper can distinguish a definitive
// stale-session refusal from a transient command failure and stop retrying.
func TestHookCommandClaimStaleSessionDrainsBeforeWorkQuery(t *testing.T) {
	clearGCEnv(t)
	disableManagedDoltRecoveryForTest(t)
	t.Setenv("GC_BEADS", "file")
	cityDir := writeFenceTestCity(t)
	sessionID := newFenceSessionBead(t, cityDir, session.StateFailedCreate, "failed-token")
	queryMarker := installFenceWorkQueryProbe(t)
	setFenceClaimEnv(t, cityDir, sessionID, "failed-token")

	var stdout, stderr bytes.Buffer
	code := cmdHookWithOptions(nil, hookCommandOptions{Claim: true, JSON: true}, &stdout, &stderr)

	// Without --drain-ack the refusal is still terminal (exit 1) but now carries a
	// schema-backed drain record instead of empty stdout.
	if code != 1 {
		t.Fatalf("code = %d, want 1; stdout=%q stderr=%s", code, stdout.String(), stderr.String())
	}
	var result hookClaimJSONResult
	if err := json.Unmarshal(bytes.TrimSpace(stdout.Bytes()), &result); err != nil {
		t.Fatalf("stdout is not a JSON drain result: %v\n%s", err, stdout.String())
	}
	if result.Action != "drain" || result.Reason != hookClaimReasonStaleSession {
		t.Fatalf("result = %+v, want action=drain reason=stale_session", result)
	}
	if result.DrainAcknowledged {
		t.Fatalf("result.DrainAcknowledged = true without --drain-ack")
	}
	if !strings.Contains(stderr.String(), "refusing stale session") ||
		!strings.Contains(stderr.String(), "failed-create") {
		t.Fatalf("stderr = %q, want failed-session refusal naming the state", stderr.String())
	}
	if _, err := os.Stat(queryMarker); !os.IsNotExist(err) {
		t.Fatalf("work query ran for stale session; stat error = %v", err)
	}
}

// TestHookCommandClaimEligibleStatesReachWorkQuery proves the fence lets the
// states a live worker legitimately claims in — active/awake plus the in-startup
// states creating/start-pending the deferred-start path passes through before
// its async active commit lands — through to the work query, rather than
// refusing a healthy first claim as stale.
func TestHookCommandClaimEligibleStatesReachWorkQuery(t *testing.T) {
	for _, state := range []session.State{
		session.StateActive,
		session.StateAwake,
		session.StateCreating,
		session.StateStartPending,
	} {
		t.Run(string(state), func(t *testing.T) {
			clearGCEnv(t)
			disableManagedDoltRecoveryForTest(t)
			t.Setenv("GC_BEADS", "file")
			cityDir := writeFenceTestCity(t)
			sessionID := newFenceSessionBead(t, cityDir, state, "current-token")
			queryMarker := installFenceWorkQueryProbe(t)
			setFenceClaimEnv(t, cityDir, sessionID, "current-token")

			var stdout, stderr bytes.Buffer
			code := cmdHookWithOptions(nil, hookCommandOptions{Claim: true, JSON: true}, &stdout, &stderr)

			// The probe bd returns no work, so the claim drains with no_work; the
			// point is that the fence let an eligible session THROUGH to the work
			// query (marker created) instead of refusing it as stale.
			if _, err := os.Stat(queryMarker); err != nil {
				t.Fatalf("work query did not run for eligible %s session: %v; stderr=%s", state, err, stderr.String())
			}
			if strings.Contains(stderr.String(), "refusing stale session") {
				t.Fatalf("eligible %s session was refused as stale: %s", state, stderr.String())
			}
			if code != 1 {
				t.Fatalf("code = %d, want 1 (JSON no-work drain without --drain-ack); stderr=%s", code, stderr.String())
			}
			var result hookClaimJSONResult
			if err := json.Unmarshal(bytes.TrimSpace(stdout.Bytes()), &result); err != nil {
				t.Fatalf("stdout is not a JSON result: %v\n%s", err, stdout.String())
			}
			if result.Action != "drain" || result.Reason != hookClaimReasonNoWork {
				t.Fatalf("result = %+v, want action=drain reason=no_work (probe returns no work)", result)
			}
		})
	}
}

// TestHookCommandClaimEmptyLegacyStateReachesWorkQuery proves a pre-metadata legacy
// session bead — one persisted with an empty state during upgrade — reaches the work
// query instead of being refused as stale. With Closed=false and a matching instance
// token the runtime is the live current incarnation, and the session lifecycle
// canonicalizes empty state to active, so draining it here would starve a healthy
// upgraded legacy worker of its routed work.
func TestHookCommandClaimEmptyLegacyStateReachesWorkQuery(t *testing.T) {
	clearGCEnv(t)
	disableManagedDoltRecoveryForTest(t)
	t.Setenv("GC_BEADS", "file")
	cityDir := writeFenceTestCity(t)
	sessionID := newFenceSessionBead(t, cityDir, session.StateNone, "current-token")
	queryMarker := installFenceWorkQueryProbe(t)
	setFenceClaimEnv(t, cityDir, sessionID, "current-token")

	var stdout, stderr bytes.Buffer
	code := cmdHookWithOptions(nil, hookCommandOptions{Claim: true, JSON: true}, &stdout, &stderr)

	if _, err := os.Stat(queryMarker); err != nil {
		t.Fatalf("work query did not run for empty-legacy-state session: %v; stderr=%s", err, stderr.String())
	}
	if strings.Contains(stderr.String(), "refusing stale session") {
		t.Fatalf("empty-legacy-state session was refused as stale: %s", stderr.String())
	}
	if code != 1 {
		t.Fatalf("code = %d, want 1 (JSON no-work drain without --drain-ack); stderr=%s", code, stderr.String())
	}
	var result hookClaimJSONResult
	if err := json.Unmarshal(bytes.TrimSpace(stdout.Bytes()), &result); err != nil {
		t.Fatalf("stdout is not a JSON result: %v\n%s", err, stdout.String())
	}
	if result.Action != "drain" || result.Reason != hookClaimReasonNoWork {
		t.Fatalf("result = %+v, want action=drain reason=no_work (probe returns no work)", result)
	}
}

// TestHookCommandClaimTokenlessRuntimeSkipsFence proves the fence's empty-token
// guard keeps a token-less (legacy/unmanaged) runtime out of the identity check:
// with GC_INSTANCE_TOKEN unset the fence is skipped entirely and the work query
// runs, even when the session bead is in a state the fence would otherwise refuse
// as stale. This pins the deliberate compatibility escape hatch so a future
// refactor cannot silently start fencing — and refusing — healthy legacy workers.
func TestHookCommandClaimTokenlessRuntimeSkipsFence(t *testing.T) {
	clearGCEnv(t)
	disableManagedDoltRecoveryForTest(t)
	t.Setenv("GC_BEADS", "file")
	cityDir := writeFenceTestCity(t)
	// A failed-create bead would drain stale if the fence ran; the point is that a
	// token-less runtime never reaches that classification.
	sessionID := newFenceSessionBead(t, cityDir, session.StateFailedCreate, "legacy-token")
	queryMarker := installFenceWorkQueryProbe(t)
	setFenceClaimEnv(t, cityDir, sessionID, "")

	var stdout, stderr bytes.Buffer
	code := cmdHookWithOptions(nil, hookCommandOptions{Claim: true, JSON: true}, &stdout, &stderr)

	if _, err := os.Stat(queryMarker); err != nil {
		t.Fatalf("work query did not run for token-less runtime (fence should be skipped): %v; stderr=%s", err, stderr.String())
	}
	if strings.Contains(stderr.String(), "refusing stale session") {
		t.Fatalf("token-less runtime was refused by the fence: %s", stderr.String())
	}
	if code != 1 {
		t.Fatalf("code = %d, want 1 (JSON no-work drain without --drain-ack); stderr=%s", code, stderr.String())
	}
	var result hookClaimJSONResult
	if err := json.Unmarshal(bytes.TrimSpace(stdout.Bytes()), &result); err != nil {
		t.Fatalf("stdout is not a JSON result: %v\n%s", err, stdout.String())
	}
	if result.Action != "drain" || result.Reason != hookClaimReasonNoWork {
		t.Fatalf("result = %+v, want action=drain reason=no_work (probe returns no work)", result)
	}
}

// TestHookCommandClaimAbsentSessionBeadDrainsStale proves a runtime whose session
// bead is confirmed absent — GC_SESSION_ID names no bead in the store — is refused
// as stale before the work query, not failed open into the claim path. A vanished
// session bead is a definitive identity failure: the incarnation can no longer
// prove it is the current one, so it must drain (action=drain,
// reason=stale_session) and stop rather than adopt routed work ahead of the
// reconciler terminating it.
func TestHookCommandClaimAbsentSessionBeadDrainsStale(t *testing.T) {
	clearGCEnv(t)
	disableManagedDoltRecoveryForTest(t)
	t.Setenv("GC_BEADS", "file")
	cityDir := writeFenceTestCity(t)
	queryMarker := installFenceWorkQueryProbe(t)
	setFenceClaimEnv(t, cityDir, "worker-1-vanished", "any-token")

	var stdout, stderr bytes.Buffer
	code := cmdHookWithOptions(nil, hookCommandOptions{Claim: true, JSON: true}, &stdout, &stderr)

	if code != 1 {
		t.Fatalf("code = %d, want 1; stdout=%q stderr=%s", code, stdout.String(), stderr.String())
	}
	var result hookClaimJSONResult
	if err := json.Unmarshal(bytes.TrimSpace(stdout.Bytes()), &result); err != nil {
		t.Fatalf("stdout is not a JSON drain result: %v\n%s", err, stdout.String())
	}
	if result.Action != "drain" || result.Reason != hookClaimReasonStaleSession {
		t.Fatalf("result = %+v, want action=drain reason=stale_session", result)
	}
	if !strings.Contains(stderr.String(), "refusing stale session") ||
		!strings.Contains(stderr.String(), "not found") {
		t.Fatalf("stderr = %q, want stale refusal naming the missing bead", stderr.String())
	}
	if _, err := os.Stat(queryMarker); !os.IsNotExist(err) {
		t.Fatalf("work query ran for a session with no bead; stat error = %v", err)
	}
}

// TestHookCommandClaimFailsOpenOnSessionStoreError proves a GENUINE session-store
// fault — here a corrupt/unreadable store file, so the fence's store open itself
// fails — is NOT mislabeled as a stale session: the fence fails open and lets the
// normal claim path run, which surfaces and escalates its own store errors. This
// is the counterpart to the absent-bead case above: a confirmed-missing bead
// drains stale, but an infrastructure fault must never refuse a possibly-healthy
// worker.
func TestHookCommandClaimFailsOpenOnSessionStoreError(t *testing.T) {
	clearGCEnv(t)
	disableManagedDoltRecoveryForTest(t)
	t.Setenv("GC_BEADS", "file")
	cityDir := writeFenceTestCity(t)
	queryMarker := installFenceWorkQueryProbe(t)
	// Corrupt the file store so openCityStoreAt fails to parse it: a genuine
	// store-open fault, distinct from an absent bead (a confirmed identity
	// failure that drains stale).
	if err := os.WriteFile(filepath.Join(cityDir, ".gc", "beads.json"), []byte("{ this is not valid json"), 0o644); err != nil {
		t.Fatal(err)
	}
	setFenceClaimEnv(t, cityDir, "worker-1", "any-token")

	var stdout, stderr bytes.Buffer
	code := cmdHookWithOptions(nil, hookCommandOptions{Claim: true, JSON: true}, &stdout, &stderr)

	if _, err := os.Stat(queryMarker); err != nil {
		t.Fatalf("fail-open did not reach the work query: %v; stderr=%s", err, stderr.String())
	}
	if strings.Contains(stderr.String(), "refusing stale session") {
		t.Fatalf("store fault was mislabeled as a stale session: %s", stderr.String())
	}
	if !strings.Contains(stderr.String(), "session fence unavailable") {
		t.Fatalf("stderr = %q, want fence-unavailable diagnostic", stderr.String())
	}
	if code != 1 {
		t.Fatalf("code = %d, want 1 (JSON no-work drain without --drain-ack)", code)
	}
}

// TestClassifyHookClaimSessionLookupError exercises the error taxonomy that
// decides whether a failed session lookup is a definitive identity failure
// (stale, drain) or a transient store fault (unavailable, fail open). The two
// confirmed-identity errors mirror the documented session.Store.Get contract: a
// confirmed-absent id wraps beads.ErrNotFound, a present-but-non-session id is
// session.ErrSessionNotFound.
func TestClassifyHookClaimSessionLookupError(t *testing.T) {
	cases := []struct {
		name    string
		err     error
		want    hookClaimSessionVerdict
		wantMsg string
	}{
		{
			name:    "confirmed absent bead is stale",
			err:     fmt.Errorf("loading session %q: %w", "s", beads.ErrNotFound),
			want:    hookClaimSessionStale,
			wantMsg: "not found",
		},
		{
			name:    "present but non-session bead is stale",
			err:     fmt.Errorf("%w: %s", session.ErrSessionNotFound, "s"),
			want:    hookClaimSessionStale,
			wantMsg: "non-session",
		},
		{
			name:    "genuine store read fault fails open",
			err:     fmt.Errorf("loading session %q: %w", "s", errors.New("dial tcp: connection refused")),
			want:    hookClaimSessionStoreUnavailable,
			wantMsg: "loading session bead",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			verdict, msg := classifyHookClaimSessionLookupError(tc.err)
			if verdict != tc.want {
				t.Fatalf("verdict = %d, want %d (msg=%q)", verdict, tc.want, msg)
			}
			if !strings.Contains(msg, tc.wantMsg) {
				t.Fatalf("msg = %q, want substring %q", msg, tc.wantMsg)
			}
		})
	}
}

// TestHookClaimSessionEligibility exercises the pure eligibility decision over a
// session Info snapshot for every branch of the fence.
func TestHookClaimSessionEligibility(t *testing.T) {
	const token = "current-token"
	cases := []struct {
		name    string
		info    session.Info
		token   string
		want    hookClaimSessionVerdict
		wantMsg string
	}{
		{
			name:    "closed",
			info:    session.Info{ID: "s", Closed: true, InstanceToken: token},
			token:   token,
			want:    hookClaimSessionStale,
			wantMsg: "closed",
		},
		{
			name:    "superseded token",
			info:    session.Info{ID: "s", MetadataState: string(session.StateActive), InstanceToken: "replacement-token"},
			token:   "stale-runtime-token",
			want:    hookClaimSessionStale,
			wantMsg: "token",
		},
		{
			name:    "empty stored token",
			info:    session.Info{ID: "s", MetadataState: string(session.StateActive), InstanceToken: ""},
			token:   token,
			want:    hookClaimSessionStale,
			wantMsg: "token",
		},
		{
			name:    "failed-create",
			info:    session.Info{ID: "s", MetadataState: string(session.StateFailedCreate), InstanceToken: token},
			token:   token,
			want:    hookClaimSessionStale,
			wantMsg: "failed-create",
		},
		{
			name:    "drained",
			info:    session.Info{ID: "s", MetadataState: string(session.StateDrained), InstanceToken: token},
			token:   token,
			want:    hookClaimSessionStale,
			wantMsg: "drained",
		},
		{
			name:  "active",
			info:  session.Info{ID: "s", MetadataState: string(session.StateActive), InstanceToken: token},
			token: token,
			want:  hookClaimSessionEligible,
		},
		{
			name:  "awake",
			info:  session.Info{ID: "s", MetadataState: string(session.StateAwake), InstanceToken: token},
			token: token,
			want:  hookClaimSessionEligible,
		},
		{
			name:  "creating",
			info:  session.Info{ID: "s", MetadataState: string(session.StateCreating), InstanceToken: token},
			token: token,
			want:  hookClaimSessionEligible,
		},
		{
			name:  "start-pending",
			info:  session.Info{ID: "s", MetadataState: string(session.StateStartPending), InstanceToken: token},
			token: token,
			want:  hookClaimSessionEligible,
		},
		{
			// A pre-metadata legacy bead mid-upgrade carries an empty MetadataState
			// (session.StateNone). With Closed=false and a matching instance token it
			// is the live current incarnation, and the session lifecycle canonicalizes
			// empty state to active, so the fence must admit it rather than drain a
			// healthy upgraded legacy worker before it claims its routed work.
			name:  "empty legacy state admitted as active",
			info:  session.Info{ID: "s", MetadataState: string(session.StateNone), InstanceToken: token},
			token: token,
			want:  hookClaimSessionEligible,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			verdict, reason := hookClaimSessionEligibility(tc.info, tc.token)
			if verdict != tc.want {
				t.Fatalf("verdict = %d, want %d (reason=%q)", verdict, tc.want, reason)
			}
			if tc.want == hookClaimSessionEligible && reason != "" {
				t.Fatalf("eligible verdict carried reason %q, want empty", reason)
			}
			if tc.wantMsg != "" && !strings.Contains(reason, tc.wantMsg) {
				t.Fatalf("reason = %q, want substring %q", reason, tc.wantMsg)
			}
		})
	}
}

// TestWriteHookClaimDrainStaleSessionWithDrainAck proves the stale-session drain
// honors --drain-ack: it runs the drain-ack, marks the record acknowledged, and
// exits 0 so a startup wrapper treats the refusal as a completed drain.
func TestWriteHookClaimDrainStaleSessionWithDrainAck(t *testing.T) {
	acked := false
	fakeAck := func(io.Writer) error { acked = true; return nil }

	var stdout, stderr bytes.Buffer
	code := writeHookClaimDrain(hookClaimReasonStaleSession, true, true, fakeAck, &stdout, &stderr)

	if code != 0 {
		t.Fatalf("code = %d, want 0 for an acknowledged drain; stderr=%s", code, stderr.String())
	}
	if !acked {
		t.Fatalf("drain-ack function was not called")
	}
	var result hookClaimJSONResult
	if err := json.Unmarshal(bytes.TrimSpace(stdout.Bytes()), &result); err != nil {
		t.Fatalf("stdout is not JSON: %v\n%s", err, stdout.String())
	}
	if result.Action != "drain" || result.Reason != hookClaimReasonStaleSession || !result.DrainAcknowledged {
		t.Fatalf("result = %+v, want drain/stale_session/acknowledged", result)
	}
}

// TestWriteHookClaimDrainDoesNotAckWhenNotRequested proves the drain path never
// runs drain-ack unless --drain-ack was requested, and returns the historical
// exit 1 for an unacknowledged drain.
func TestWriteHookClaimDrainDoesNotAckWhenNotRequested(t *testing.T) {
	fakeAck := func(io.Writer) error {
		t.Fatalf("drain-ack must not run without --drain-ack")
		return nil
	}
	var stdout, stderr bytes.Buffer
	code := writeHookClaimDrain(hookClaimReasonStaleSession, true, false, fakeAck, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("code = %d, want 1 when drain is not acknowledged", code)
	}
	var result hookClaimJSONResult
	if err := json.Unmarshal(bytes.TrimSpace(stdout.Bytes()), &result); err != nil {
		t.Fatalf("stdout is not JSON: %v\n%s", err, stdout.String())
	}
	if result.DrainAcknowledged {
		t.Fatalf("result.DrainAcknowledged = true without --drain-ack")
	}
}
