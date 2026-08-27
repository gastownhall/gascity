//go:build integration

package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/nudgequeue"
)

const (
	nudgeDueTargetSelectionShadowJourneyTarget         = "shadow-nudge-target-4c91"
	nudgeDueTargetSelectionShadowJourneyReceiptPrefix  = "receipt:- [session] "
	nudgeDueTargetSelectionShadowJourneyWitnessTimeout = 20 * time.Second
	nudgeDueTargetSelectionShadowJourneyTraceQuiet     = time.Second
)

var nudgeDueTargetSelectionShadowJourneyAssignment = regexp.MustCompile(`(?m)^([ \t]*nudge_shadow[ \t]*=[ \t]*)"([^"]*)"([ \t]*(?:#.*)?)$`)

type nudgeDueTargetSelectionShadowJourneySession struct {
	ID          string `json:"id"`
	Template    string `json:"template"`
	SessionName string `json:"session_name"`
	Closed      bool   `json:"closed"`
}

type nudgeDueTargetSelectionShadowJourneySessionList struct {
	Sessions []nudgeDueTargetSelectionShadowJourneySession `json:"sessions"`
}

type nudgeDueTargetSelectionShadowJourneyQueued struct {
	SchemaVersion string `json:"schema_version"`
	OK            bool   `json:"ok"`
	Target        string `json:"target"`
	SessionID     string `json:"session_id"`
	SessionName   string `json:"session_name"`
	Delivery      string `json:"delivery"`
	Queued        bool   `json:"queued"`
	Outcome       string `json:"outcome"`
}

type nudgeDueTargetSelectionShadowJourneyStatus struct {
	Counts struct {
		Pending  int `json:"pending"`
		InFlight int `json:"in_flight"`
		Dead     int `json:"dead"`
	} `json:"counts"`
}

type nudgeDueTargetSelectionShadowJourneyBead struct {
	ID       string            `json:"id"`
	Metadata map[string]string `json:"metadata"`
}

type nudgeDueTargetSelectionShadowJourneyTraceShow struct {
	Records []json.RawMessage `json:"records"`
}

type nudgeDueTargetSelectionShadowJourneyTraceRecord struct {
	TraceSchemaVersion   int             `json:"trace_schema_version"`
	Seq                  uint64          `json:"seq"`
	TraceID              string          `json:"trace_id"`
	TickID               string          `json:"tick_id"`
	RecordID             string          `json:"record_id"`
	RecordType           string          `json:"record_type"`
	TraceMode            string          `json:"trace_mode"`
	TraceSource          string          `json:"trace_source"`
	SiteCode             string          `json:"site_code"`
	Timestamp            time.Time       `json:"ts"`
	CityPath             string          `json:"city_path"`
	ConfigRevision       string          `json:"config_revision"`
	Template             string          `json:"template"`
	SessionBeadID        string          `json:"session_bead_id"`
	SessionName          string          `json:"session_name"`
	Alias                string          `json:"alias"`
	SessionKey           string          `json:"session_key"`
	ControllerInstanceID string          `json:"controller_instance_id"`
	ControllerPID        int             `json:"controller_pid"`
	ControllerStartedAt  *time.Time      `json:"controller_started_at"`
	TickTrigger          string          `json:"tick_trigger"`
	TriggerDetail        string          `json:"trigger_detail"`
	GCCommit             string          `json:"gc_commit"`
	ReasonCode           string          `json:"reason_code"`
	OutcomeCode          string          `json:"outcome_code"`
	FieldsJSON           json.RawMessage `json:"fields"`

	Fields nudgeDueTargetSelectionShadowJourneyTraceFields
	Raw    json.RawMessage
}

type nudgeDueTargetSelectionShadowJourneyTraceFields struct {
	Scope               string `json:"scope"`
	QueueItemCount      int    `json:"queue_item_count"`
	CandidateCount      int    `json:"candidate_count"`
	CandidateDigest     string `json:"candidate_digest"`
	LegacyCount         int    `json:"legacy_count"`
	LegacyDigest        string `json:"legacy_digest"`
	ComparisonOutcome   string `json:"comparison_outcome"`
	QueueDurationMS     int64  `json:"queue_duration_ms"`
	CandidateDurationMS int64  `json:"candidate_duration_ms"`
	LegacyDurationMS    int64  `json:"legacy_duration_ms"`
	TotalDurationMS     int64  `json:"total_duration_ms"`
	LegacyEffectOwner   bool   `json:"legacy_effect_owner"`
	ShadowEffectApplied bool   `json:"shadow_effect_applied"`
}

var nudgeDueTargetSelectionShadowJourneyTraceFieldKeys = [...]string{
	"scope",
	"queue_item_count",
	"candidate_count",
	"candidate_digest",
	"legacy_count",
	"legacy_digest",
	"comparison_outcome",
	"queue_duration_ms",
	"candidate_duration_ms",
	"legacy_duration_ms",
	"total_duration_ms",
	"legacy_effect_owner",
	"shadow_effect_applied",
}

func decodeNudgeDueTargetSelectionShadowJourneyTraceFields(
	raw json.RawMessage,
) (nudgeDueTargetSelectionShadowJourneyTraceFields, error) {
	var encoded map[string]json.RawMessage
	if err := json.Unmarshal(raw, &encoded); err != nil {
		return nudgeDueTargetSelectionShadowJourneyTraceFields{}, fmt.Errorf("decode field object: %w", err)
	}

	expected := make(map[string]struct{}, len(nudgeDueTargetSelectionShadowJourneyTraceFieldKeys))
	var missing []string
	for _, key := range nudgeDueTargetSelectionShadowJourneyTraceFieldKeys {
		expected[key] = struct{}{}
		if _, ok := encoded[key]; !ok {
			missing = append(missing, key)
		}
	}
	var unknown []string
	for key := range encoded {
		if _, ok := expected[key]; !ok {
			unknown = append(unknown, key)
		}
	}
	sort.Strings(missing)
	sort.Strings(unknown)
	if len(missing) != 0 || len(unknown) != 0 {
		return nudgeDueTargetSelectionShadowJourneyTraceFields{},
			fmt.Errorf("field keys do not match exact schema: missing=%v unknown=%v", missing, unknown)
	}
	for _, key := range nudgeDueTargetSelectionShadowJourneyTraceFieldKeys {
		if bytes.Equal(bytes.TrimSpace(encoded[key]), []byte("null")) {
			return nudgeDueTargetSelectionShadowJourneyTraceFields{}, fmt.Errorf("field %q is null", key)
		}
	}

	var fields nudgeDueTargetSelectionShadowJourneyTraceFields
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nudgeDueTargetSelectionShadowJourneyTraceFields{}, fmt.Errorf("decode typed fields: %w", err)
	}
	return fields, nil
}

func TestNudgeDueTargetSelectionShadowJourneyDecodeTraceFieldsRequiresExactSchema(t *testing.T) {
	valid := func() map[string]any {
		return map[string]any{
			"scope":                 "queued_exact_due_target_selection",
			"queue_item_count":      1,
			"candidate_count":       1,
			"candidate_digest":      strings.Repeat("a", 64),
			"legacy_count":          1,
			"legacy_digest":         strings.Repeat("a", 64),
			"comparison_outcome":    "matched",
			"queue_duration_ms":     1,
			"candidate_duration_ms": 2,
			"legacy_duration_ms":    3,
			"total_duration_ms":     6,
			"legacy_effect_owner":   true,
			"shadow_effect_applied": false,
		}
	}
	encode := func(t *testing.T, fields map[string]any) json.RawMessage {
		t.Helper()
		raw, err := json.Marshal(fields)
		if err != nil {
			t.Fatalf("encode trace fields fixture: %v", err)
		}
		return raw
	}

	got, err := decodeNudgeDueTargetSelectionShadowJourneyTraceFields(encode(t, valid()))
	if err != nil {
		t.Fatalf("decode exact trace fields: %v", err)
	}
	if got.Scope != "queued_exact_due_target_selection" ||
		got.QueueItemCount != 1 ||
		got.CandidateDigest != strings.Repeat("a", 64) ||
		!got.LegacyEffectOwner ||
		got.ShadowEffectApplied {
		t.Fatalf("decoded exact trace fields = %+v", got)
	}

	t.Run("missing", func(t *testing.T) {
		fields := valid()
		delete(fields, "scope")
		if _, err := decodeNudgeDueTargetSelectionShadowJourneyTraceFields(encode(t, fields)); err == nil {
			t.Fatal("decode trace fields without scope succeeded, want error")
		}
	})

	t.Run("replacement", func(t *testing.T) {
		fields := valid()
		delete(fields, "candidate_digest")
		fields["candidate_hash"] = strings.Repeat("a", 64)
		if _, err := decodeNudgeDueTargetSelectionShadowJourneyTraceFields(encode(t, fields)); err == nil {
			t.Fatal("decode trace fields with replacement key succeeded, want error")
		}
	})

	t.Run("unknown_extra", func(t *testing.T) {
		fields := valid()
		fields["unexpected"] = true
		if _, err := decodeNudgeDueTargetSelectionShadowJourneyTraceFields(encode(t, fields)); err == nil {
			t.Fatal("decode trace fields with unknown extra key succeeded, want error")
		}
	})

	for _, key := range []string{
		"scope",
		"queue_item_count",
		"candidate_count",
		"candidate_digest",
		"legacy_count",
		"legacy_digest",
		"comparison_outcome",
		"queue_duration_ms",
		"candidate_duration_ms",
		"legacy_duration_ms",
		"total_duration_ms",
		"legacy_effect_owner",
		"shadow_effect_applied",
	} {
		t.Run("null_"+key, func(t *testing.T) {
			fields := valid()
			fields[key] = nil
			if _, err := decodeNudgeDueTargetSelectionShadowJourneyTraceFields(encode(t, fields)); err == nil {
				t.Fatalf("decode trace fields with null %q succeeded, want error", key)
			}
		})
	}
}

func TestNudgeDueTargetSelectionShadowColdEnableExactBinaryJourney(t *testing.T) {
	if usingSubprocess() {
		t.Skip("queued nudge shadow journey requires an isolated named tmux server")
	}

	cityDir := setupReconcilerCityWithDaemon(t, `[[agent]]
name = "`+nudgeDueTargetSelectionShadowJourneyTarget+`"
provider = "codex"
ready_prompt_prefix = "READY> "
start_command = "bash -c 'trap : WINCH; while :; do printf \"READY> \"; IFS= read -r line || true; printf \"receipt:%s\\n\" \"$line\"; done'"
`, `patrol_interval = "1m"
nudge_dispatcher = "supervisor"
session_reconciler = "off"
nudge_shadow = "off"
`, "")
	env := commandEnvForDir(cityDir, false)
	if out, err := runGCWithEnv(env, "", "supervisor", "stop", "--wait"); err != nil {
		t.Fatalf("stop bootstrap supervisor: %v\n%s", err, out)
	}
	env = replaceEnv(env, "GC_SESSION_RECONCILER_TRACE", "1")
	env = replaceEnv(env, "GC_SUPERVISOR_PRESERVE_SESSIONS_ON_SIGNAL", "1")
	registerCityCommandEnv(cityDir, env)
	gcHome := parseEnvList(env)["GC_HOME"]
	startIsolatedSupervisor(t, env, gcHome)
	waitForControllerReady(t, cityDir, 15*time.Second)
	sessionReconcilerColdDisableWaitForMode(t, cityDir, "off", "legacy")
	nudgeDueTargetSelectionShadowJourneyWaitForWakeSocket(t, cityDir)
	waitForExpectedTmuxSessions(t, cityDir, []string{nudgeDueTargetSelectionShadowJourneyTarget})
	waitForAgentRunning(t, cityDir, nudgeDueTargetSelectionShadowJourneyTarget, 30*time.Second)

	session := nudgeDueTargetSelectionShadowJourneyFindSession(t, cityDir)
	initialIdentity := sessionWaitDependencyShadowJourneyTmuxIdentity(t, cityDir, session.SessionName)
	initialTrace := nudgeDueTargetSelectionShadowJourneyTrace(t, cityDir)
	initialCursor := nudgeDueTargetSelectionShadowJourneyLastSeq(t, initialTrace)

	const initialOffToken = "off-before-canary-7f31c9"
	nudgeDueTargetSelectionShadowJourneyQueue(t, cityDir, session, initialOffToken)
	nudgeDueTargetSelectionShadowJourneyWaitForDelivery(t, cityDir, session, initialOffToken)
	nudgeDueTargetSelectionShadowJourneyWaitForTraceStability(t, cityDir, initialCursor, false)

	beforeEnableTrace := nudgeDueTargetSelectionShadowJourneyTrace(t, cityDir)
	beforeEnableCursor := nudgeDueTargetSelectionShadowJourneyLastSeq(t, beforeEnableTrace)
	nudgeDueTargetSelectionShadowJourneyStopPreserving(t, env)
	// Observation point: the pre-heal lifecycle state is staged here, after the
	// predecessor is gone and before the successor exists, so no tick can move
	// it between the write and the snapshot. Reading the naturally-created
	// "active" row earlier was a race against the first reconcile tick, and the
	// city has nothing left that keeps ticks out of that window.
	beforeEnableBead := nudgeDueTargetSelectionShadowJourneyStageLifecycleState(
		t, cityDir, session.ID, "active",
	)
	nudgeDueTargetSelectionShadowJourneyInstallMode(t, cityDir, "off", "required")
	nudgeDueTargetSelectionShadowJourneyAssertSessionUnchanged(
		t, cityDir, session, initialIdentity, beforeEnableBead, initialOffToken, false,
	)

	startIsolatedSupervisor(t, env, gcHome)
	waitForControllerReady(t, cityDir, 15*time.Second)
	sessionReconcilerColdDisableWaitForMode(t, cityDir, "off", "legacy")
	probeCursor := nudgeDueTargetSelectionShadowJourneyLastSeq(
		t,
		nudgeDueTargetSelectionShadowJourneyTrace(t, cityDir),
	)
	nudgeDueTargetSelectionShadowJourneyWaitForWakeSocket(t, cityDir)
	transitionTrace := nudgeDueTargetSelectionShadowJourneyWaitForQueueCount(
		t, cityDir, probeCursor, 0, false,
	)
	// The heal is the successor's, so wait for it on the successor's clock
	// instead of assuming it landed before the nudge shadow cycle above. A row
	// that never leaves "active" fails here; one that leaves it for anything
	// other than "awake" fails in the assertion below.
	nudgeDueTargetSelectionShadowJourneyWaitForLifecycleState(t, cityDir, session.ID, "awake")
	nudgeDueTargetSelectionShadowJourneyAssertSessionUnchanged(
		t, cityDir, session, initialIdentity, beforeEnableBead, initialOffToken, true,
	)
	for _, record := range nudgeDueTargetSelectionShadowJourneyRecordsAfter(t, transitionTrace, beforeEnableCursor) {
		if record.Fields.QueueItemCount != 0 ||
			record.Fields.CandidateCount != 0 ||
			record.Fields.LegacyCount != 0 ||
			!record.Fields.LegacyEffectOwner ||
			record.Fields.ShadowEffectApplied {
			t.Fatalf("cold-enable transition shadow record applied or selected an effect: %+v", record)
		}
	}

	requiredCursor := nudgeDueTargetSelectionShadowJourneyLastSeq(t, transitionTrace)
	const requiredToken = "required-canary-8d42ea"
	nudgeDueTargetSelectionShadowJourneyQueue(t, cityDir, session, requiredToken)
	nudgeDueTargetSelectionShadowJourneyWaitForDelivery(t, cityDir, session, requiredToken)
	requiredTrace := nudgeDueTargetSelectionShadowJourneyWaitForTraceStability(t, cityDir, requiredCursor, true)
	requiredRecords := nudgeDueTargetSelectionShadowJourneyQueueCountRecords(t, requiredTrace, requiredCursor, 1)
	if len(requiredRecords) != 1 {
		t.Fatalf("one-item nudge shadow records = %d, want exactly 1: %+v", len(requiredRecords), requiredRecords)
	}
	nudgeDueTargetSelectionShadowJourneyAssertRequiredRecord(
		t, requiredTrace, requiredRecords[0], requiredCursor, session, requiredToken,
	)
	if got := nudgeDueTargetSelectionShadowJourneyReceiptCount(t, cityDir, session.SessionName, requiredToken); got != 1 {
		t.Fatalf("required-mode visible legacy receipts = %d, want exactly 1", got)
	}
	nudgeDueTargetSelectionShadowJourneyAssertQueueEmpty(t, cityDir, session.ID)
	if got := sessionWaitDependencyShadowJourneyTmuxIdentity(t, cityDir, session.SessionName); got != initialIdentity {
		t.Fatalf("tmux identity changed across cold enable and delivery: before=%q after=%q", initialIdentity, got)
	}

	beforeDisableBead := nudgeDueTargetSelectionShadowJourneyReadBead(t, cityDir, session.ID)
	nudgeDueTargetSelectionShadowJourneyStopPreserving(t, env)
	nudgeDueTargetSelectionShadowJourneyInstallMode(t, cityDir, "required", "off")
	nudgeDueTargetSelectionShadowJourneyAssertSessionUnchanged(
		t, cityDir, session, initialIdentity, beforeDisableBead, requiredToken, false,
	)

	startIsolatedSupervisor(t, env, gcHome)
	waitForControllerReady(t, cityDir, 15*time.Second)
	sessionReconcilerColdDisableWaitForMode(t, cityDir, "off", "legacy")
	nudgeDueTargetSelectionShadowJourneyWaitForWakeSocket(t, cityDir)
	nudgeDueTargetSelectionShadowJourneyAssertSessionUnchanged(
		t, cityDir, session, initialIdentity, beforeDisableBead, requiredToken, false,
	)
	offCursor := nudgeDueTargetSelectionShadowJourneyLastSeq(
		t,
		nudgeDueTargetSelectionShadowJourneyTrace(t, cityDir),
	)

	const finalOffToken = "off-after-canary-9e53bd"
	nudgeDueTargetSelectionShadowJourneyQueue(t, cityDir, session, finalOffToken)
	nudgeDueTargetSelectionShadowJourneyWaitForDelivery(t, cityDir, session, finalOffToken)
	nudgeDueTargetSelectionShadowJourneyWaitForTraceStability(t, cityDir, offCursor, false)
	nudgeDueTargetSelectionShadowJourneyWaitForTraceStability(t, cityDir, requiredCursor, true)
	for _, token := range []string{initialOffToken, requiredToken, finalOffToken} {
		if got := nudgeDueTargetSelectionShadowJourneyReceiptCount(t, cityDir, session.SessionName, token); got != 1 {
			t.Fatalf("visible legacy receipts for %q = %d, want exactly 1", token, got)
		}
	}
	nudgeDueTargetSelectionShadowJourneyAssertQueueEmpty(t, cityDir, session.ID)
	if got := sessionWaitDependencyShadowJourneyTmuxIdentity(t, cityDir, session.SessionName); got != initialIdentity {
		t.Fatalf("tmux identity changed across full off/required/off journey: before=%q after=%q", initialIdentity, got)
	}
}

func nudgeDueTargetSelectionShadowJourneyFindSession(t *testing.T, cityDir string) nudgeDueTargetSelectionShadowJourneySession {
	t.Helper()
	out, err := gc(cityDir, "session", "list", "--state", "all", "--template", nudgeDueTargetSelectionShadowJourneyTarget, "--json")
	if err != nil {
		t.Fatalf("list nudge target session: %v\n%s", err, out)
	}
	var result nudgeDueTargetSelectionShadowJourneySessionList
	if err := json.Unmarshal([]byte(strings.TrimSpace(extractJSONPayload(out))), &result); err != nil {
		t.Fatalf("decode nudge target session list: %v\n%s", err, out)
	}
	for _, session := range result.Sessions {
		if session.Template == nudgeDueTargetSelectionShadowJourneyTarget &&
			session.ID != "" &&
			session.SessionName != "" &&
			!session.Closed {
			return session
		}
	}
	t.Fatalf("live nudge target session absent: %+v", result)
	return nudgeDueTargetSelectionShadowJourneySession{}
}

func nudgeDueTargetSelectionShadowJourneyQueue(
	t *testing.T,
	cityDir string,
	session nudgeDueTargetSelectionShadowJourneySession,
	message string,
) {
	t.Helper()
	out, err := gc(cityDir, "session", "nudge", session.ID, message, "--delivery=queue", "--json")
	if err != nil {
		t.Fatalf("queue nudge %q: %v\n%s", message, err, out)
	}
	var queued nudgeDueTargetSelectionShadowJourneyQueued
	if err := json.Unmarshal([]byte(strings.TrimSpace(extractJSONPayload(out))), &queued); err != nil {
		t.Fatalf("decode queued nudge %q: %v\n%s", message, err, out)
	}
	if queued.SchemaVersion != "1" ||
		!queued.OK ||
		queued.Target != nudgeDueTargetSelectionShadowJourneyTarget ||
		queued.SessionID != session.ID ||
		queued.SessionName != session.SessionName ||
		queued.Delivery != "queue" ||
		!queued.Queued ||
		queued.Outcome != "queued" {
		t.Fatalf("queued nudge %q = %+v, want exact-session queued result", message, queued)
	}
}

func nudgeDueTargetSelectionShadowJourneyWaitForDelivery(
	t *testing.T,
	cityDir string,
	session nudgeDueTargetSelectionShadowJourneySession,
	token string,
) {
	t.Helper()
	var lastStatus nudgeDueTargetSelectionShadowJourneyStatus
	var lastStatusErr error
	var receiptCount int
	nudgeDueTargetSelectionShadowJourneyPoll(
		t,
		nudgeDueTargetSelectionShadowJourneyWitnessTimeout,
		"one visible drained nudge "+token,
		func() (bool, string) {
			receiptCount = nudgeDueTargetSelectionShadowJourneyReceiptCount(t, cityDir, session.SessionName, token)
			if receiptCount > 1 {
				t.Fatalf("visible legacy receipts for %q = %d, want at most 1", token, receiptCount)
			}
			lastStatus, lastStatusErr = nudgeDueTargetSelectionShadowJourneyReadStatus(cityDir, session.ID)
			if lastStatusErr == nil &&
				receiptCount == 1 &&
				lastStatus.Counts.Pending == 0 &&
				lastStatus.Counts.InFlight == 0 &&
				lastStatus.Counts.Dead == 0 {
				return true, ""
			}
			return false, fmt.Sprintf("receipts=%d status=%+v status_error=%v", receiptCount, lastStatus, lastStatusErr)
		},
	)
}

func nudgeDueTargetSelectionShadowJourneyReceiptCount(t *testing.T, cityDir, sessionName, message string) int {
	t.Helper()
	out, err := runCommand("", commandEnvForDir(cityDir, false), integrationGCCommandTimeout,
		"tmux", "-L", filepath.Base(cityDir), "capture-pane", "-p", "-t", "="+sessionName+":", "-S", "-")
	if err != nil {
		t.Fatalf("capture nudge target pane %s: %v\n%s", sessionName, err, out)
	}
	return strings.Count(out, nudgeDueTargetSelectionShadowJourneyReceiptPrefix+message)
}

func nudgeDueTargetSelectionShadowJourneyReadStatus(
	cityDir string,
	sessionID string,
) (nudgeDueTargetSelectionShadowJourneyStatus, error) {
	out, err := gc(cityDir, "nudge", "status", sessionID, "--json")
	if err != nil {
		return nudgeDueTargetSelectionShadowJourneyStatus{}, fmt.Errorf("gc nudge status: %w: %s", err, out)
	}
	var status nudgeDueTargetSelectionShadowJourneyStatus
	if err := json.Unmarshal([]byte(strings.TrimSpace(extractJSONPayload(out))), &status); err != nil {
		return nudgeDueTargetSelectionShadowJourneyStatus{}, fmt.Errorf("decode gc nudge status: %w: %s", err, out)
	}
	return status, nil
}

func nudgeDueTargetSelectionShadowJourneyAssertQueueEmpty(
	t *testing.T,
	cityDir string,
	sessionID string,
) {
	t.Helper()
	status, err := nudgeDueTargetSelectionShadowJourneyReadStatus(cityDir, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if status.Counts.Pending != 0 || status.Counts.InFlight != 0 || status.Counts.Dead != 0 {
		t.Fatalf("nudge queue is not drained cleanly: %+v", status)
	}
}

func nudgeDueTargetSelectionShadowJourneyReadBead(
	t *testing.T,
	cityDir string,
	sessionID string,
) nudgeDueTargetSelectionShadowJourneyBead {
	t.Helper()
	out, err := runCommand(
		cityDir,
		replaceEnv(commandEnvForDir(cityDir, false), "GC_BEADS", "file"),
		integrationBDCommandTimeout,
		bdBinary, "show", sessionID, "--json",
	)
	if err != nil {
		t.Fatalf("read durable session bead %s: %v\n%s", sessionID, err, out)
	}
	var bead nudgeDueTargetSelectionShadowJourneyBead
	if err := json.Unmarshal([]byte(strings.TrimSpace(extractJSONPayload(out))), &bead); err != nil {
		t.Fatalf("decode durable session bead %s: %v\n%s", sessionID, err, out)
	}
	if bead.ID != sessionID {
		t.Fatalf("durable session bead ID = %q, want %q", bead.ID, sessionID)
	}
	return bead
}

// nudgeDueTargetSelectionShadowJourneyStageLifecycleState writes the durable
// lifecycle state through the same file-bd shim the journey reads with, then
// returns the resulting bead. Call it only while no controller is running for
// the city: that is what makes the returned snapshot a stable pre-state rather
// than a value the next reconcile tick can move out from under the caller.
func nudgeDueTargetSelectionShadowJourneyStageLifecycleState(
	t *testing.T,
	cityDir string,
	sessionID string,
	state string,
) nudgeDueTargetSelectionShadowJourneyBead {
	t.Helper()
	out, err := runCommand(
		cityDir,
		replaceEnv(commandEnvForDir(cityDir, false), "GC_BEADS", "file"),
		integrationBDCommandTimeout,
		bdBinary, "update", sessionID, "--set-metadata", "state="+state,
	)
	if err != nil {
		t.Fatalf("stage durable lifecycle state %q on %s: %v\n%s", state, sessionID, err, out)
	}
	staged := nudgeDueTargetSelectionShadowJourneyReadBead(t, cityDir, sessionID)
	if got := staged.Metadata["state"]; got != state {
		t.Fatalf("staged durable lifecycle state = %q, want %q: %+v", got, state, staged.Metadata)
	}
	return staged
}

// nudgeDueTargetSelectionShadowJourneyWaitForLifecycleState blocks until the
// durable row reports the wanted lifecycle state, so a transition assertion
// runs on the writer's schedule rather than the caller's.
func nudgeDueTargetSelectionShadowJourneyWaitForLifecycleState(
	t *testing.T,
	cityDir string,
	sessionID string,
	state string,
) {
	t.Helper()
	nudgeDueTargetSelectionShadowJourneyPoll(
		t,
		nudgeDueTargetSelectionShadowJourneyWitnessTimeout,
		fmt.Sprintf("durable lifecycle state %q on session %s", state, sessionID),
		func() (bool, string) {
			got := nudgeDueTargetSelectionShadowJourneyReadBead(t, cityDir, sessionID).Metadata["state"]
			return got == state, "state=" + got
		},
	)
}

func nudgeDueTargetSelectionShadowJourneyTrace(
	t *testing.T,
	cityDir string,
) nudgeDueTargetSelectionShadowJourneyTraceShow {
	t.Helper()
	out, err := gc(cityDir, "trace", "show", "--json")
	if err != nil {
		t.Fatalf("read nudge shadow trace: %v\n%s", err, out)
	}
	var trace nudgeDueTargetSelectionShadowJourneyTraceShow
	if err := json.Unmarshal([]byte(strings.TrimSpace(extractJSONPayload(out))), &trace); err != nil {
		t.Fatalf("decode nudge shadow trace: %v\n%s", err, out)
	}
	return trace
}

func nudgeDueTargetSelectionShadowJourneyRecordsAfter(
	t *testing.T,
	trace nudgeDueTargetSelectionShadowJourneyTraceShow,
	afterSeq uint64,
) []nudgeDueTargetSelectionShadowJourneyTraceRecord {
	t.Helper()
	var records []nudgeDueTargetSelectionShadowJourneyTraceRecord
	for _, raw := range trace.Records {
		var record nudgeDueTargetSelectionShadowJourneyTraceRecord
		if err := json.Unmarshal(raw, &record); err != nil {
			t.Fatalf("decode trace record: %v\n%s", err, raw)
		}
		if record.Seq <= afterSeq || record.SiteCode != "nudge.due_target_selection.shadow" {
			continue
		}
		fields, err := decodeNudgeDueTargetSelectionShadowJourneyTraceFields(record.FieldsJSON)
		if err != nil {
			t.Fatalf("decode nudge shadow fields at seq %d: %v\n%s", record.Seq, err, record.FieldsJSON)
		}
		record.Fields = fields
		record.Raw = append(json.RawMessage(nil), raw...)
		records = append(records, record)
	}
	return records
}

func nudgeDueTargetSelectionShadowJourneyQueueCountRecords(
	t *testing.T,
	trace nudgeDueTargetSelectionShadowJourneyTraceShow,
	afterSeq uint64,
	queueItemCount int,
) []nudgeDueTargetSelectionShadowJourneyTraceRecord {
	t.Helper()
	var matches []nudgeDueTargetSelectionShadowJourneyTraceRecord
	for _, record := range nudgeDueTargetSelectionShadowJourneyRecordsAfter(t, trace, afterSeq) {
		if record.Fields.QueueItemCount == queueItemCount {
			matches = append(matches, record)
		}
	}
	return matches
}

func nudgeDueTargetSelectionShadowJourneyLastSeq(
	t *testing.T,
	trace nudgeDueTargetSelectionShadowJourneyTraceShow,
) uint64 {
	t.Helper()
	var last uint64
	for _, raw := range trace.Records {
		var record struct {
			Seq uint64 `json:"seq"`
		}
		if err := json.Unmarshal(raw, &record); err != nil {
			t.Fatalf("decode trace record sequence: %v\n%s", err, raw)
		}
		if record.Seq > last {
			last = record.Seq
		}
	}
	return last
}

func nudgeDueTargetSelectionShadowJourneyWaitForQueueCount(
	t *testing.T,
	cityDir string,
	afterSeq uint64,
	queueItemCount int,
	exactlyOne bool,
) nudgeDueTargetSelectionShadowJourneyTraceShow {
	t.Helper()
	var lastTrace nudgeDueTargetSelectionShadowJourneyTraceShow
	nudgeDueTargetSelectionShadowJourneyPoll(
		t,
		nudgeDueTargetSelectionShadowJourneyWitnessTimeout,
		fmt.Sprintf("queue_item_count=%d shadow record after seq %d", queueItemCount, afterSeq),
		func() (bool, string) {
			lastTrace = nudgeDueTargetSelectionShadowJourneyTrace(t, cityDir)
			matches := nudgeDueTargetSelectionShadowJourneyQueueCountRecords(t, lastTrace, afterSeq, queueItemCount)
			if exactlyOne && len(matches) > 1 {
				t.Fatalf("queue_item_count=%d shadow records after seq %d = %d, want exactly 1: %+v",
					queueItemCount, afterSeq, len(matches), matches)
			}
			if len(matches) > 0 {
				return true, ""
			}
			return false, fmt.Sprintf("records=%+v", nudgeDueTargetSelectionShadowJourneyRecordsAfter(t, lastTrace, afterSeq))
		},
	)
	return lastTrace
}

func nudgeDueTargetSelectionShadowJourneyWaitForTraceStability(
	t *testing.T,
	cityDir string,
	afterSeq uint64,
	wantOneItemRecord bool,
) nudgeDueTargetSelectionShadowJourneyTraceShow {
	t.Helper()
	var (
		lastTrace   nudgeDueTargetSelectionShadowJourneyTraceShow
		lastHead    uint64
		stableSince time.Time
	)
	expectation := "no nudge shadow record"
	if wantOneItemRecord {
		expectation = "exactly one one-item nudge shadow record"
	}
	nudgeDueTargetSelectionShadowJourneyPoll(
		t,
		nudgeDueTargetSelectionShadowJourneyWitnessTimeout,
		fmt.Sprintf("%s after seq %d and a %s quiet trace window",
			expectation, afterSeq, nudgeDueTargetSelectionShadowJourneyTraceQuiet),
		func() (bool, string) {
			lastTrace = nudgeDueTargetSelectionShadowJourneyTrace(t, cityDir)
			records := nudgeDueTargetSelectionShadowJourneyRecordsAfter(t, lastTrace, afterSeq)
			if wantOneItemRecord {
				if len(records) > 1 {
					t.Fatalf("nudge shadow records after seq %d = %d, want exactly one: %+v",
						afterSeq, len(records), records)
				}
				if len(records) == 1 && records[0].Fields.QueueItemCount != 1 {
					t.Fatalf("only nudge shadow record after seq %d has queue_item_count=%d, want 1: %+v",
						afterSeq, records[0].Fields.QueueItemCount, records[0])
				}
				if len(records) == 0 {
					stableSince = time.Time{}
					return false, "one-item record not flushed yet"
				}
			} else if len(records) != 0 {
				t.Fatalf("off mode emitted nudge shadow records after seq %d: %+v", afterSeq, records)
			}

			head := nudgeDueTargetSelectionShadowJourneyLastSeq(t, lastTrace)
			now := time.Now()
			if stableSince.IsZero() || head != lastHead {
				lastHead = head
				stableSince = now
				return false, fmt.Sprintf("trace head=%d; quiet for %s", head, 0*time.Second)
			}
			quietFor := now.Sub(stableSince)
			if quietFor >= nudgeDueTargetSelectionShadowJourneyTraceQuiet {
				return true, ""
			}
			return false, fmt.Sprintf("trace head=%d; quiet for %s", head, quietFor)
		},
	)
	return lastTrace
}

func nudgeDueTargetSelectionShadowJourneyAssertRequiredRecord(
	t *testing.T,
	trace nudgeDueTargetSelectionShadowJourneyTraceShow,
	record nudgeDueTargetSelectionShadowJourneyTraceRecord,
	afterSeq uint64,
	session nudgeDueTargetSelectionShadowJourneySession,
	message string,
) {
	t.Helper()
	fields := record.Fields
	if record.TraceSchemaVersion != 1 ||
		record.Seq <= afterSeq ||
		record.TraceID == "" ||
		record.TickID == "" ||
		record.RecordID == "" ||
		record.RecordType != "decision" ||
		record.TraceMode != "baseline" ||
		record.TraceSource != "always_on" ||
		record.Timestamp.IsZero() ||
		record.CityPath == "" ||
		record.ConfigRevision == "" ||
		record.ControllerInstanceID == "" ||
		record.ControllerPID <= 0 ||
		record.ControllerStartedAt == nil ||
		record.ControllerStartedAt.IsZero() ||
		record.TickTrigger != "" ||
		record.TriggerDetail != "" ||
		record.GCCommit == "" ||
		record.ReasonCode != "retained" ||
		record.OutcomeCode != "no_change" {
		t.Fatalf("one-item nudge shadow provenance = %+v, want committed always-on controller decision", record)
	}
	if record.Template != "" ||
		record.SessionBeadID != "" ||
		record.SessionName != "" ||
		record.Alias != "" ||
		record.SessionKey != "" {
		t.Fatalf("one-item nudge shadow envelope leaked target identity: %+v", record)
	}

	var cycleStarts []nudgeDueTargetSelectionShadowJourneyTraceRecord
	for _, raw := range trace.Records {
		var candidate nudgeDueTargetSelectionShadowJourneyTraceRecord
		if err := json.Unmarshal(raw, &candidate); err != nil {
			t.Fatalf("decode trace record while locating cycle start: %v\n%s", err, raw)
		}
		if candidate.TickID == record.TickID && candidate.RecordType == "cycle_start" {
			cycleStarts = append(cycleStarts, candidate)
		}
	}
	if len(cycleStarts) != 1 {
		t.Fatalf("cycle_start records for decision tick %q = %d, want exactly 1: %+v",
			record.TickID, len(cycleStarts), cycleStarts)
	}
	cycleStart := cycleStarts[0]
	if cycleStart.TraceSchemaVersion != 1 ||
		cycleStart.Seq == 0 ||
		cycleStart.Seq >= record.Seq ||
		cycleStart.TraceID != record.TraceID ||
		cycleStart.RecordID == "" ||
		cycleStart.TraceMode != "baseline" ||
		cycleStart.TraceSource != "always_on" ||
		cycleStart.Timestamp.IsZero() ||
		cycleStart.CityPath != record.CityPath ||
		cycleStart.ControllerInstanceID != record.ControllerInstanceID ||
		cycleStart.ControllerPID != record.ControllerPID ||
		cycleStart.ControllerStartedAt == nil ||
		cycleStart.ControllerStartedAt.IsZero() ||
		!cycleStart.ControllerStartedAt.Equal(*record.ControllerStartedAt) ||
		cycleStart.TickTrigger != "control" ||
		cycleStart.TriggerDetail != "queued_exact_due_target_selection" ||
		cycleStart.GCCommit != record.GCCommit {
		t.Fatalf("one-item nudge shadow cycle-start provenance = %+v, want earlier matching control trigger for decision %+v",
			cycleStart, record)
	}

	if fields.Scope != "queued_exact_due_target_selection" ||
		fields.QueueItemCount != 1 ||
		fields.CandidateCount != 1 ||
		fields.LegacyCount != 1 ||
		len(fields.CandidateDigest) != 64 ||
		fields.CandidateDigest != fields.LegacyDigest ||
		fields.ComparisonOutcome != "matched" ||
		!fields.LegacyEffectOwner ||
		fields.ShadowEffectApplied {
		t.Fatalf("one-item nudge shadow fields = %+v, want matched legacy-only 1:1 selection", fields)
	}
	if fields.QueueDurationMS < 0 ||
		fields.CandidateDurationMS < 0 ||
		fields.LegacyDurationMS < 0 ||
		fields.TotalDurationMS < fields.QueueDurationMS {
		t.Fatalf("one-item nudge shadow timings are invalid: %+v", fields)
	}
	for _, secret := range []string{
		nudgeDueTargetSelectionShadowJourneyTarget,
		session.ID,
		session.SessionName,
		message,
	} {
		if bytes.Contains(record.Raw, []byte(secret)) {
			t.Fatalf("one-item nudge shadow record leaked raw target/message %q: %s", secret, record.Raw)
		}
	}
}

func nudgeDueTargetSelectionShadowJourneyStopPreserving(t *testing.T, env []string) {
	t.Helper()
	pid := sessionReconcilerColdDisableSupervisorPID(t, env)
	if err := syscall.Kill(pid, syscall.SIGTERM); err != nil {
		t.Fatalf("SIGTERM test-owned preserve supervisor %d: %v", pid, err)
	}
	sessionReconcilerColdDisableWaitPIDGone(t, pid)
}

func nudgeDueTargetSelectionShadowJourneyInstallMode(t *testing.T, cityDir, from, to string) {
	t.Helper()
	configPath := filepath.Join(cityDir, "city.toml")
	info, err := os.Lstat(configPath)
	if err != nil {
		t.Fatalf("inspect city config: %v", err)
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		t.Fatalf("refusing non-regular city config: %s", info.Mode())
	}
	current, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read city config: %v", err)
	}
	matches := nudgeDueTargetSelectionShadowJourneyAssignment.FindAllSubmatch(current, -1)
	if len(matches) != 1 || string(matches[0][2]) != from {
		t.Fatalf("nudge_shadow assignments = %q, want exactly one %q assignment", matches, from)
	}
	next := nudgeDueTargetSelectionShadowJourneyAssignment.ReplaceAll(
		current,
		[]byte(fmt.Sprintf(`${1}"%s"${3}`, to)),
	)
	candidate := filepath.Join(cityDir, ".city.toml.nudge-shadow-next")
	if err := fsys.WriteFileAtomic(fsys.OSFS{}, candidate, next, info.Mode().Perm()); err != nil {
		t.Fatalf("write same-directory nudge-shadow candidate: %v", err)
	}
	t.Cleanup(func() { _ = os.Remove(candidate) })
	if out, err := gc(cityDir, "config", "show", "--validate", "--root-file", candidate); err != nil {
		t.Fatalf("validate nudge_shadow=%q candidate: %v\n%s", to, err, out)
	}
	if err := os.Rename(candidate, configPath); err != nil {
		t.Fatalf("atomically install nudge_shadow=%q candidate: %v", to, err)
	}
}

func nudgeDueTargetSelectionShadowJourneyWaitForWakeSocket(t *testing.T, cityDir string) {
	t.Helper()
	path := nudgequeue.WakeSocketPath(cityDir)
	var lastErr error
	nudgeDueTargetSelectionShadowJourneyPoll(
		t,
		nudgeDueTargetSelectionShadowJourneyWitnessTimeout,
		"test-city nudge wake socket "+path,
		func() (bool, string) {
			conn, err := net.DialTimeout("unix", path, 250*time.Millisecond)
			if err == nil {
				_, _ = conn.Write([]byte{1})
				_ = conn.Close()
				return true, ""
			}
			lastErr = err
			return false, "last error: " + lastErr.Error()
		},
	)
}

func nudgeDueTargetSelectionShadowJourneyPoll(
	t *testing.T,
	timeout time.Duration,
	description string,
	check func() (bool, string),
) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), timeout)
	defer cancel()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	var detail string
	for {
		if done, observed := check(); done {
			return
		} else {
			detail = observed
		}
		select {
		case <-ctx.Done():
			t.Fatalf("waiting for %s: %v; %s", description, ctx.Err(), detail)
		case <-ticker.C:
		}
	}
}

func nudgeDueTargetSelectionShadowJourneyAssertSessionUnchanged(
	t *testing.T,
	cityDir string,
	session nudgeDueTargetSelectionShadowJourneySession,
	wantIdentity string,
	wantBead nudgeDueTargetSelectionShadowJourneyBead,
	latestMessage string,
	allowActiveToAwake bool,
) {
	t.Helper()
	if got := sessionWaitDependencyShadowJourneyTmuxIdentity(t, cityDir, session.SessionName); got != wantIdentity {
		t.Fatalf("tmux identity changed across cold config transition: before=%q after=%q", wantIdentity, got)
	}
	gotBead := nudgeDueTargetSelectionShadowJourneyReadBead(t, cityDir, session.ID)
	if gotBead.ID != wantBead.ID {
		t.Fatalf("durable session bead ID changed across cold config transition: before=%q after=%q", wantBead.ID, gotBead.ID)
	}
	if allowActiveToAwake {
		if wantBead.Metadata["state"] != "active" || gotBead.Metadata["state"] != "awake" {
			t.Fatalf("preserved-successor state transition = %q -> %q, want active -> awake",
				wantBead.Metadata["state"], gotBead.Metadata["state"])
		}
		wantMetadata := cloneNudgeDueTargetSelectionShadowJourneyMetadata(wantBead.Metadata)
		gotMetadata := cloneNudgeDueTargetSelectionShadowJourneyMetadata(gotBead.Metadata)
		delete(wantMetadata, "state")
		delete(gotMetadata, "state")
		if !reflect.DeepEqual(gotMetadata, wantMetadata) {
			t.Fatalf("durable session metadata beyond active -> awake changed across preserved successor: before=%+v after=%+v",
				wantMetadata, gotMetadata)
		}
	} else if !reflect.DeepEqual(gotBead, wantBead) {
		t.Fatalf("durable session bead changed across cold config transition: before=%+v after=%+v", wantBead, gotBead)
	}
	if got := nudgeDueTargetSelectionShadowJourneyReceiptCount(t, cityDir, session.SessionName, latestMessage); got != 1 {
		t.Fatalf("cold config transition changed visible receipt count for %q: got=%d want=1", latestMessage, got)
	}
	nudgeDueTargetSelectionShadowJourneyAssertQueueEmpty(t, cityDir, session.ID)
}

func cloneNudgeDueTargetSelectionShadowJourneyMetadata(metadata map[string]string) map[string]string {
	cloned := make(map[string]string, len(metadata))
	for key, value := range metadata {
		cloned[key] = value
	}
	return cloned
}
