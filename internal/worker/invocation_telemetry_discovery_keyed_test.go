package worker

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/beadstest"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/sessionlog"
	"github.com/gastownhall/gascity/internal/usage"
)

func keyedDiscoveryTime() time.Time {
	return time.Date(2026, 6, 15, 10, 0, 0, 0, time.UTC)
}

// codexWorkerSessionMetaWithThread is separate from the shared constant-id
// helper: keyed identity fixtures must agree with their own filename suffix.
func codexWorkerSessionMetaWithThread(cwd, thread string) map[string]any {
	line := codexWorkerSessionMeta(cwd)
	line["payload"].(map[string]any)["id"] = thread
	return line
}

func keyedDiscoveryMeta(key string) map[string]string {
	at := keyedDiscoveryTime()
	return map[string]string{
		"provider": "codex", "session_key": key, "work_dir": "/work/standing",
		"awake_started_at": at.Format(time.RFC3339), "last_woke_at": at.Format(time.RFC3339),
		"slept_at": at.Add(time.Minute).Format(time.RFC3339), "session_name": "test-session",
	}
}

func newKeyedDiscoveryFactory(t *testing.T, root string) (*Factory, *keyedDiscoverySink) {
	t.Helper()
	sink := &keyedDiscoverySink{}
	factory, err := NewFactory(FactoryConfig{
		CityPath: t.TempDir(), Store: beadstest.NewRecordingStore(beads.NewMemStore()), Provider: runtime.NewFake(),
		SearchPaths: []string{root}, UsageSink: sink,
	})
	if err != nil {
		t.Fatal(err)
	}
	return factory, sink
}

type keyedDiscoverySink struct{ facts []usage.Fact }

func (s *keyedDiscoverySink) Record(_ context.Context, f usage.Fact) error {
	s.facts = append(s.facts, f)
	return nil
}

func TestCodexKeyedDiscoveryOutcomeMapping(t *testing.T) {
	for _, tc := range []struct {
		outcome          sessionlog.CodexKeyedOutcome
		path             string
		clean, permanent bool
	}{
		{sessionlog.CodexKeyedFound, "selected-rollout", true, false},
		{sessionlog.CodexKeyedAbsent, "", true, false},
		{sessionlog.CodexKeyedInvalid, "", true, false},
		{sessionlog.CodexKeyedUncertain, "", false, false},
		{sessionlog.CodexKeyedAmbiguous, "", true, true},
		{sessionlog.CodexKeyedIdentityMismatch, "", true, true},
	} {
		t.Run(string(tc.outcome), func(t *testing.T) {
			path, clean, permanent := codexKeyedDiscovery(sessionlog.CodexKeyedLookup{Path: tc.path, Outcome: tc.outcome})
			if path != tc.path || clean != tc.clean || permanent != tc.permanent {
				t.Fatalf("mapping = (%q,%v,%v), want (%q,%v,%v)", path, clean, permanent, tc.path, tc.clean, tc.permanent)
			}
		})
	}
}

// KB-8 includes both key shapes for each provider. The public tests below also
// execute both real settle sites, so implementing an unused helper is not enough.
func TestCodexKeyedSweepMissFinalityTable(t *testing.T) {
	for _, family := range []string{"codex", "claude", "opencode"} {
		for _, keyless := range []bool{false, true} {
			for _, permanent := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/keyless=%v/permanent=%v", family, keyless, permanent), func(t *testing.T) {
					want := permanent && (family == "codex" || (family == "claude" && keyless))
					if got := sweepMissIsFinal(family, keyless, permanent); got != want {
						t.Fatalf("final = %v, want %v", got, want)
					}
				})
			}
		}
	}
}

func writeKeyedDiscoveryScenario(t *testing.T, root, scenario string) (map[string]string, bool) {
	t.Helper()
	const key = "synthetic-keyed-thread"
	meta := keyedDiscoveryMeta(key)
	at := keyedDiscoveryTime()
	path := codexWorkerRolloutPathWithID(t, root, at, key)
	switch scenario {
	case "ambiguous":
		writeWorkerTestJSONL(t, path, []map[string]any{codexWorkerSessionMetaWithThread(meta["work_dir"], key)})
		writeWorkerTestJSONL(t, codexWorkerRolloutPathWithID(t, root, at.AddDate(0, 0, -1), key), []map[string]any{codexWorkerSessionMetaWithThread(meta["work_dir"], key)})
		return meta, true
	case "identity_mismatch":
		writeWorkerTestJSONL(t, path, []map[string]any{codexWorkerSessionMetaWithThread(meta["work_dir"], "different-thread")})
		return meta, true
	case "absent":
		return meta, false
	case "uncertain":
		if err := os.WriteFile(path, []byte(`{"type":"session_meta","payload":`), 0o600); err != nil {
			t.Fatal(err)
		}
		return meta, false
	case "invalid":
		meta["work_dir"] = ""
		return meta, false
	default:
		t.Fatalf("unknown discovery fixture %q", scenario)
		return nil, false
	}
}

func TestCodexKeyedBothSettleSites(t *testing.T) {
	for _, scenario := range []string{"ambiguous", "identity_mismatch", "absent", "uncertain", "invalid"} {
		t.Run("K13_"+scenario, func(t *testing.T) {
			root := t.TempDir()
			meta, wantSettled := writeKeyedDiscoveryScenario(t, root, scenario)
			factory, sink := newKeyedDiscoveryFactory(t, root)
			before := maps.Clone(meta)
			path, settled := factory.DiscoverSweepTranscript("gc-keyed", meta, keyedDiscoveryTime().Add(time.Minute))
			if path != "" || settled != wantSettled {
				t.Errorf("Discover = (%q,%v), want empty/%v", path, settled, wantSettled)
			}
			emitted, settled, err := factory.SweepSessionModelUsage(context.Background(), "gc-keyed", meta, keyedDiscoveryTime().Add(time.Minute))
			if err != nil || emitted != 0 || settled != wantSettled {
				t.Errorf("Sweep = (%d,%v,%v), want 0/%v/nil", emitted, settled, err, wantSettled)
			}
			if len(sink.facts) != 0 || !maps.Equal(before, meta) {
				t.Fatal("unavailable lookup recorded facts or changed interval metadata")
			}
		})
	}
}

// K12 and Z3: the five counts are the mechanism fixture from one city/window,
// not a fleet loss-rate claim. Expected keys are computed independently of the
// implementation's ModelIdempotencyKey helper, and compared as a set.
func TestCodexKeyedDriftRecovers222Facts(t *testing.T) {
	root := t.TempDir()
	factory, sink := newKeyedDiscoveryFactory(t, root)
	wantKeys := make(map[string]bool)
	total := 0
	for row, count := range []int{17, 17, 27, 50, 111} {
		key, sessionID := fmt.Sprintf("synthetic-thread-%d", row), fmt.Sprintf("gc-drift-%d", row)
		meta := keyedDiscoveryMeta(key)
		meta["work_dir"] = fmt.Sprintf("/work/standing-%d", row)
		meta["slept_at"] = keyedDiscoveryTime().Add(3 * time.Minute).Format(time.RFC3339)
		lines := []map[string]any{codexWorkerSessionMetaWithThread(fmt.Sprintf("/work/rollout-%d", row), key), codexWorkerTurnContext()}
		for invocation := 1; invocation <= count; invocation++ {
			lines = append(lines, codexWorkerTokenCount(keyedDiscoveryTime().Add(time.Duration(invocation)*time.Second).Format(time.RFC3339Nano), invocation*100, 70, 0, 30))
			hash := sha256.Sum256([]byte(sessionID + ":total:" + fmt.Sprint(invocation*100)))
			wantKeys[hex.EncodeToString(hash[:])] = true
		}
		writeWorkerTestJSONL(t, codexWorkerRolloutPathWithID(t, root, keyedDiscoveryTime(), key), lines)
		emitted, settled, err := factory.SweepSessionModelUsage(context.Background(), sessionID, meta, keyedDiscoveryTime().Add(3*time.Minute))
		if err != nil || !settled || emitted != count {
			t.Errorf("row %d: Sweep = (%d,%v,%v), want %d/true/nil", row, emitted, settled, err, count)
		}
		total += emitted
	}
	gotKeys := make(map[string]bool)
	for _, fact := range sink.facts {
		gotKeys[fact.IdempotencyKey] = true
		if fact.Provider != "codex" || fact.Kind != usage.KindModel || fact.SessionID == "" || fact.RunID != fact.SessionID || fact.StepID != "" {
			t.Errorf("fact attribution changed: %+v", fact)
		}
	}
	if total != 222 || len(sink.facts) != 222 || !maps.Equal(gotKeys, wantKeys) {
		t.Fatalf("K12: emitted=%d facts=%d distinct keys=%d, want 222 with the frozen session-scoped keys", total, len(sink.facts), len(gotKeys))
	}
}

// K14: a resumed rollout may predate the wake by months, but must still lie in
// the supplied creation-to-wake day range. Mismatch uses the same cwd so the old
// cwd-only finder cannot accidentally satisfy the identity refusal.
func TestCodexKeyedPromptOpIdentityAndWindow(t *testing.T) {
	for _, scenario := range []string{"drift", "mismatch", "outside_created_to_anchor"} {
		t.Run("K14_"+scenario, func(t *testing.T) {
			root := t.TempDir()
			const key = "synthetic-resumed-thread"
			anchor := keyedDiscoveryTime()
			created := anchor.AddDate(0, -3, 0)
			rolloutAt := created.Add(time.Hour)
			thread, cwd, want := key, "/work/drifted", ""
			if scenario == "mismatch" {
				thread, cwd = "another-thread", "/work/standing"
			}
			if scenario == "outside_created_to_anchor" {
				rolloutAt = created.AddDate(0, 0, -10)
			}
			path := codexWorkerRolloutPathWithID(t, root, rolloutAt, key)
			writeWorkerTestJSONL(t, path, []map[string]any{codexWorkerSessionMetaWithThread(cwd, thread)})
			if scenario == "drift" {
				want = path
			}
			h := &SessionHandle{adapter: SessionLogAdapter{SearchPaths: []string{root}}}
			meta := keyedDiscoveryMeta(key)
			if got := discoverCodexInvocationTranscript(h, "gc-prompt", created, meta); got != want {
				t.Fatalf("prompt discovery = %q, want %q", got, want)
			}
		})
	}
}

func captureKeyedDiscoveryLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	var output bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&output, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(previous) })
	return &output
}

// D1-D3: execute the real arms rather than calling a proposed record helper.
// Assert values and exact keys, so free text and sensitive identifiers cannot
// creep into an otherwise correctly named record.
func TestCodexKeyedDiscoveryDiagnostics(t *testing.T) {
	for _, seam := range []string{"prompt_op", "sweep"} {
		for _, outcome := range []string{"found", "found_drift", "ambiguous", "identity_mismatch", "absent", "uncertain", "invalid"} {
			t.Run(seam+"/"+outcome, func(t *testing.T) {
				root := t.TempDir()
				meta := keyedDiscoveryMeta("synthetic-keyed-thread")
				final := outcome == "ambiguous" || outcome == "identity_mismatch"
				if strings.HasPrefix(outcome, "found") {
					cwd := meta["work_dir"]
					if outcome == "found_drift" {
						cwd = "/work/drifted"
					}
					writeWorkerTestJSONL(t, codexWorkerRolloutPathWithID(t, root, keyedDiscoveryTime(), meta["session_key"]), []map[string]any{codexWorkerSessionMetaWithThread(cwd, meta["session_key"])})
				} else {
					meta, _ = writeKeyedDiscoveryScenario(t, root, outcome)
				}
				factory, _ := newKeyedDiscoveryFactory(t, root)
				output := captureKeyedDiscoveryLogs(t)
				if seam == "prompt_op" {
					discoverCodexInvocationTranscript(&SessionHandle{adapter: factory.Adapter()}, "gc-keyed", keyedDiscoveryTime(), meta)
				} else {
					factory.discoverSweepTranscript("codex", "gc-keyed", meta, keyedDiscoveryTime().Add(time.Minute))
				}
				wantOutcome := outcome
				if outcome == "found_drift" {
					wantOutcome = "found"
				}
				level := "DEBUG"
				if seam == "sweep" && final {
					level = "WARN"
				}
				assertKeyedDiscoveryRecord(t, output.String(), map[string]any{
					"session_id": "gc-keyed", "seam": seam, "mechanism": "keyed", "outcome": wantOutcome,
					"final": final, "cwd_differs": outcome == "found_drift", "level": level, "msg": "codex rollout discovery",
				})
			})
		}
	}
}

func assertKeyedDiscoveryRecord(t *testing.T, output string, want map[string]any) {
	t.Helper()
	var records []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(output), "\n") {
		if line == "" {
			continue
		}
		var record map[string]any
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatalf("invalid structured log %q: %v", line, err)
		}
		if record["msg"] == "codex rollout discovery" {
			records = append(records, record)
		}
	}
	if len(records) != 1 {
		t.Fatalf("discovery records = %d, want exactly 1; logs: %s", len(records), output)
	}
	got := records[0]
	delete(got, "time")
	if !maps.Equal(got, want) {
		t.Errorf("discovery attributes = %v, want %v", got, want)
	}
	rendered, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"synthetic-keyed-thread", "different-thread", "/", "reason", "thread_id", "path", "cwd\""} {
		if strings.Contains(string(rendered), forbidden) {
			t.Errorf("discovery log contains forbidden value/name %q: %s", forbidden, rendered)
		}
	}
}

// K13 compatibility at both sites, including Claude's permanent discovery
// refusal with a key: that refusal must NOT acquire Codex's new finality.
func TestCodexKeyedFinalityPreservesOtherFamilies(t *testing.T) {
	for _, family := range []string{"claude", "opencode"} {
		for _, keyless := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/keyless=%v", family, keyless), func(t *testing.T) {
				factory, sink := newKeyedDiscoveryFactory(t, t.TempDir())
				meta := keyedDiscoveryMeta("missing-thread")
				meta["provider"], meta["work_dir"] = family, ""
				if keyless {
					meta["session_key"] = ""
				}
				want := family == "claude" && keyless
				// A real session row without a workdir is a classified permanent
				// refusal; an absent row would be a transient read error instead.
				row, createErr := factory.store.Create(beads.Bead{Type: "session", Title: "compatibility", Metadata: maps.Clone(meta)})
				if createErr != nil {
					t.Fatal(createErr)
				}
				path, settled := factory.DiscoverSweepTranscript(row.ID, meta, keyedDiscoveryTime())
				if path != "" || settled != want {
					t.Errorf("Discover = (%q,%v), want empty/%v", path, settled, want)
				}
				emitted, settled, err := factory.SweepSessionModelUsage(context.Background(), row.ID, meta, keyedDiscoveryTime())
				if emitted != 0 || settled != want || err != nil || len(sink.facts) != 0 {
					t.Errorf("Sweep = (%d,%v,%v), want 0/%v/nil", emitted, settled, err, want)
				}
			})
		}
	}
}

// Z2: discovery is read-only even when the caller supplies an existing cursor.
func TestCodexKeyedDiscoveryLeavesStateUntouched(t *testing.T) {
	root := t.TempDir()
	factory, sink := newKeyedDiscoveryFactory(t, root)
	meta := keyedDiscoveryMeta("readonly-thread")
	meta["invocation_usage_cursor"] = "total:100"
	path := codexWorkerRolloutPathWithID(t, root, keyedDiscoveryTime(), meta["session_key"])
	writeWorkerTestJSONL(t, path, []map[string]any{codexWorkerSessionMetaWithThread("/work/drifted", meta["session_key"])})
	before := maps.Clone(meta)
	store := factory.store.(*beadstest.RecordingStore)
	store.Reset()
	got, settled := factory.DiscoverSweepTranscript("gc-readonly", meta, keyedDiscoveryTime().Add(time.Minute))
	if got != path || !settled {
		t.Errorf("Discover = (%q,%v), want found/true", got, settled)
	}
	prompt := discoverCodexInvocationTranscript(&SessionHandle{adapter: factory.Adapter()}, "gc-readonly", keyedDiscoveryTime(), meta)
	if prompt != path {
		t.Errorf("prompt = %q, want %q", prompt, path)
	}
	if !maps.Equal(before, meta) || len(store.Calls()) != 0 || len(sink.facts) != 0 {
		t.Fatalf("discovery mutated state: meta=%v writes=%v facts=%v", meta, store.Calls(), sink.facts)
	}
}

func TestCodexKeyedFinalityPreservesClaudeAmbiguity(t *testing.T) {
	for _, keyless := range []bool{false, true} {
		t.Run(fmt.Sprintf("keyless=%v", keyless), func(t *testing.T) {
			factory, sink := newKeyedDiscoveryFactory(t, t.TempDir())
			meta := keyedDiscoveryMeta("absent-claude-thread")
			meta["provider"] = "claude"
			meta["work_dir"] = t.TempDir()
			if keyless {
				meta["session_key"] = ""
			}
			row, err := factory.store.Create(beads.Bead{Type: "session", Labels: []string{sessionpkg.LabelSession}, Metadata: maps.Clone(meta)})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := factory.store.Create(beads.Bead{Type: "session", Labels: []string{sessionpkg.LabelSession}, Metadata: maps.Clone(meta)}); err != nil {
				t.Fatal(err)
			}
			_, classification, err := factory.manager.TranscriptPathClassified(row.ID, factory.searchPaths)
			if err != nil || classification != sessionpkg.TranscriptAmbiguous {
				t.Fatalf("fixture must be ambiguous: class=%v err=%v", classification, err)
			}
			path, settled := factory.DiscoverSweepTranscript(row.ID, meta, keyedDiscoveryTime())
			if path != "" || settled != keyless {
				t.Errorf("Discover = (%q,%v), want empty/%v", path, settled, keyless)
			}
			emitted, settled, err := factory.SweepSessionModelUsage(context.Background(), row.ID, meta, keyedDiscoveryTime())
			if emitted != 0 || settled != keyless || err != nil || len(sink.facts) != 0 {
				t.Errorf("Sweep = (%d,%v,%v), want 0/%v/nil", emitted, settled, err, keyless)
			}
		})
	}
}
