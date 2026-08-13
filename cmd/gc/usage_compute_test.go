package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/session/sessiontest"
	"github.com/gastownhall/gascity/internal/usage"
)

// TestComputeFactGetCandidate is the usage-lane Get-budget gate: emitDueComputeFacts only
// issues a per-session store Get when computeFactGetCandidate returns true, so this pins
// the pre-Get filter that keeps a steady fleet of parked, already-accounted sessions at
// zero Gets. A mutation that drops any filter clause (terminal-state, awake-interval
// present, or interval-not-already-emitted) flips a case and fails.
func TestComputeFactGetCandidate(t *testing.T) {
	info := func(state, awake, emitted string) session.Info {
		return sessiontest.SeedBead(t, beads.Bead{
			ID: "gc-x", Type: session.BeadType, Status: "open", Labels: []string{session.LabelSession},
			Metadata: map[string]string{"state": state, "awake_started_at": awake, "usage_compute_emitted_at": emitted},
		})
	}
	const t1 = "2026-01-02T00:30:00Z"
	cases := []struct {
		name string
		info session.Info
		want bool
	}{
		{"active-not-terminal", info("active", t1, ""), false},
		{"terminal-no-awake", info("asleep", "", ""), false},
		{"terminal-awake-not-emitted", info("asleep", t1, ""), true},
		{"terminal-awake-already-emitted", info("asleep", t1, t1), false},
		{"terminal-awake-emitted-stale-interval", info("asleep", t1, "2026-01-01T00:00:00Z"), true},
		{"drained-terminal", info("drained", t1, ""), true},
	}
	for _, tc := range cases {
		if got := computeFactGetCandidate(tc.info); got != tc.want {
			t.Errorf("%s: computeFactGetCandidate = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestLiveModelSweepCandidate(t *testing.T) {
	const awakeStart = "2026-01-02T00:30:00Z"
	for _, tc := range []struct {
		name  string
		state string
		awake string
		want  bool
	}{
		{name: "active", state: "active", awake: awakeStart, want: true},
		{name: "awake alias", state: "awake", awake: awakeStart, want: true},
		{name: "missing interval anchor", state: "active", want: false},
		{name: "terminal", state: "asleep", awake: awakeStart, want: false},
		{name: "transitional", state: "draining", awake: awakeStart, want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			info := session.Info{MetadataState: tc.state, AwakeStartedAt: tc.awake}
			if got := liveModelSweepCandidate(info); got != tc.want {
				t.Fatalf("liveModelSweepCandidate() = %v, want %v", got, tc.want)
			}
			if isLiveModelSweepState(tc.state) && isComputeTerminalState(tc.state) {
				t.Fatalf("state %q is both live and compute-terminal", tc.state)
			}
		})
	}
}

type captureSink struct{ facts []usage.Fact }

func (c *captureSink) Record(_ context.Context, f usage.Fact) error {
	c.facts = append(c.facts, f)
	return nil
}

type erroringSink struct{ calls int }

func (e *erroringSink) Record(context.Context, usage.Fact) error {
	e.calls++
	return errors.New("disk full")
}

func TestEmitComputeFactForBead(t *testing.T) {
	store := beads.NewMemStore()
	start := time.Date(2026, 6, 15, 10, 0, 0, 0, time.UTC)
	slept := start.Add(90 * time.Second)
	b, err := store.Create(beads.Bead{
		Title: "session",
		Metadata: map[string]string{
			"state":            "asleep",
			"session_name":     "s-x",
			"awake_started_at": start.Format(time.RFC3339),
			"slept_at":         slept.Format(time.RFC3339),
			"molecule_id":      "mol-7",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	sink := &captureSink{}
	now := slept.Add(5 * time.Second)

	if !emitComputeFactForBead(context.Background(), sink, store, b, "fake", "demo", now, nil, true) {
		t.Fatal("expected first emit to record a fact")
	}
	if len(sink.facts) != 1 {
		t.Fatalf("want 1 fact, got %d", len(sink.facts))
	}
	f := sink.facts[0]
	if f.Kind != usage.KindCompute {
		t.Fatalf("kind = %q", f.Kind)
	}
	if f.WallSeconds != 90 {
		t.Fatalf("wall = %v, want 90 (slept_at - awake_started_at)", f.WallSeconds)
	}
	if f.RunID != "mol-7" {
		t.Fatalf("runID = %q, want mol-7", f.RunID)
	}
	// SessionID is the session bead id (distinct from RunID mol-7 here), so a
	// session-keyed rollup joins compute facts symmetrically with model facts.
	if f.SessionID != b.ID {
		t.Fatalf("SessionID = %q, want the session bead id %q", f.SessionID, b.ID)
	}
	if f.Runtime != "fake" || f.City != "demo" || f.Worker != "s-x" {
		t.Fatalf("unexpected fact fields: %+v", f)
	}
	if f.IdempotencyKey == "" {
		t.Fatal("missing idempotency key")
	}

	// Marker should now suppress re-emit. Re-fetch the bead (marker persisted).
	refreshed, err := store.Get(b.ID)
	if err != nil {
		t.Fatal(err)
	}
	if emitComputeFactForBead(context.Background(), sink, store, refreshed, "fake", "demo", now, nil, true) {
		t.Fatal("second emit on same interval must no-op (marker set)")
	}
	if len(sink.facts) != 1 {
		t.Fatalf("no new fact expected, got %d", len(sink.facts))
	}
}

// TestEmitComputeFactForBeadMultiInterval proves the create -> sleep -> wake ->
// sleep path bills two distinct compute facts. A reused session bead gets a
// fresh awake_started_at epoch on the second wake, so the interval-1 emit marker
// (keyed on the first epoch) does not suppress interval 2.
func TestEmitComputeFactForBeadMultiInterval(t *testing.T) {
	store := beads.NewMemStore()
	t1 := time.Date(2026, 6, 15, 10, 0, 0, 0, time.UTC)
	s1 := t1.Add(60 * time.Second)
	b, err := store.Create(beads.Bead{
		Title: "session",
		Metadata: map[string]string{
			"state":            "asleep",
			"session_name":     "pool-1",
			"awake_started_at": t1.Format(time.RFC3339Nano),
			"slept_at":         s1.Format(time.RFC3339Nano),
			"molecule_id":      "run-A",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	sink := &captureSink{}

	if !emitComputeFactForBead(context.Background(), sink, store, b, "fake", "demo", s1.Add(time.Second), nil, true) {
		t.Fatal("interval 1 should emit")
	}

	// Second awake interval: the controller stamps a fresh epoch on wake.
	t2 := t1.Add(2 * time.Hour)
	s2 := t2.Add(30 * time.Second)
	if err := store.SetMetadata(b.ID, "awake_started_at", t2.Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	if err := store.SetMetadata(b.ID, "slept_at", s2.Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	refreshed, err := store.Get(b.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !emitComputeFactForBead(context.Background(), sink, store, refreshed, "fake", "demo", s2.Add(time.Second), nil, true) {
		t.Fatal("interval 2 should emit a second compute fact")
	}
	if len(sink.facts) != 2 {
		t.Fatalf("want 2 compute facts across two awake intervals, got %d", len(sink.facts))
	}
	if sink.facts[0].IdempotencyKey == sink.facts[1].IdempotencyKey {
		t.Fatal("two intervals must have distinct idempotency keys")
	}
	if sink.facts[0].WallSeconds != 60 || sink.facts[1].WallSeconds != 30 {
		t.Fatalf("interval wall seconds wrong: %v, %v", sink.facts[0].WallSeconds, sink.facts[1].WallSeconds)
	}
}

func TestEmitComputeFactForBeadSinkErrorIsLogged(t *testing.T) {
	store := beads.NewMemStore()
	start := time.Date(2026, 6, 15, 10, 0, 0, 0, time.UTC)
	b, err := store.Create(beads.Bead{Title: "s", Metadata: map[string]string{
		"state":            "asleep",
		"awake_started_at": start.Format(time.RFC3339Nano),
	}})
	if err != nil {
		t.Fatal(err)
	}
	var logged []string
	logf := func(format string, args ...any) { logged = append(logged, fmt.Sprintf(format, args...)) }
	sink := &erroringSink{}

	if emitComputeFactForBead(context.Background(), sink, store, b, "fake", "demo", start.Add(time.Minute), logf, true) {
		t.Fatal("a failing sink must not report success")
	}
	if len(logged) == 0 {
		t.Fatal("sink failure must be surfaced via logf, not dropped silently")
	}
	// Marker must stay unset so a later tick retries.
	refreshed, err := store.Get(b.ID)
	if err != nil {
		t.Fatal(err)
	}
	if refreshed.Metadata[usageComputeEmittedAtKey] != "" {
		t.Fatal("marker must not be set when the sink write failed (so the fact retries)")
	}
}

func TestEmitComputeFactForBeadNoOps(t *testing.T) {
	store := beads.NewMemStore()
	ctx := context.Background()
	now := time.Now().UTC()
	sink := &captureSink{}

	// No awake_started_at → nothing to bill.
	b1, _ := store.Create(beads.Bead{Title: "s1", Metadata: map[string]string{"state": "asleep"}})
	if emitComputeFactForBead(ctx, sink, store, b1, "fake", "demo", now, nil, true) {
		t.Fatal("no awake_started_at must no-op")
	}
	// Discard sink → no-op even with a valid interval.
	b2, _ := store.Create(beads.Bead{Title: "s2", Metadata: map[string]string{"state": "asleep", "awake_started_at": now.Format(time.RFC3339)}})
	if emitComputeFactForBead(ctx, usage.Discard, store, b2, "fake", "demo", now, nil, true) {
		t.Fatal("discard sink must no-op")
	}
	if len(sink.facts) != 0 {
		t.Fatalf("expected no facts, got %d", len(sink.facts))
	}
}

// TestEmitComputeFactForBeadHungSinkReturnsPromptly is the reconcile-path half
// of the hung-exec-sink regression: a compute fact whose sink write hangs must
// not stall the reconcile tick, and must leave the emit marker unset so the
// fact retries on a later tick.
func TestEmitComputeFactForBeadHungSinkReturnsPromptly(t *testing.T) {
	script := filepath.Join(t.TempDir(), "hang.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nsleep 30\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	store := beads.NewMemStore()
	start := time.Date(2026, 6, 15, 10, 0, 0, 0, time.UTC)
	b, err := store.Create(beads.Bead{Title: "s", Metadata: map[string]string{
		"state":            "asleep",
		"awake_started_at": start.Format(time.RFC3339Nano),
	}})
	if err != nil {
		t.Fatal(err)
	}
	sink := usage.NewExecSinkWithTimeout(script, 100*time.Millisecond)

	done := make(chan bool, 1)
	began := time.Now()
	go func() {
		done <- emitComputeFactForBead(context.Background(), sink, store, b, "fake", "demo", start.Add(time.Minute), nil, true)
	}()
	select {
	case ok := <-done:
		if elapsed := time.Since(began); elapsed > 5*time.Second {
			t.Fatalf("reconcile compute path blocked on a hung sink: took %s", elapsed)
		}
		if ok {
			t.Fatal("a timed-out sink write must report failure, not success")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("reconcile compute path did not return under a hung sink")
	}
	refreshed, err := store.Get(b.ID)
	if err != nil {
		t.Fatal(err)
	}
	if refreshed.Metadata[usageComputeEmittedAtKey] != "" {
		t.Fatal("marker must stay unset when the sink timed out (so the fact retries)")
	}
}

const codexSweepSessionKey = "019e3e8e-3591-7532-a1ef-8b9e882bea2f"

// writeCodexRolloutForSweep fabricates a codex rollout transcript
// (rollout-<localtime>-<sessionID>.jsonl) under root/YYYY/MM/DD reachable by the
// window-free keyed discovery: a session_meta line whose cwd is workDir, a
// turn_context supplying the model, and one event_msg token_count per element of
// tokenCounts ({total, lastInput, lastOutput}). The keyed sweep scenarios share
// codexSweepSessionKey; callers vary only the transcript contents and location.
func writeCodexRolloutForSweep(t *testing.T, root, workDir string, tokenCounts [][3]int) {
	t.Helper()
	dayDir := filepath.Join(root, "2026", "06", "15")
	if err := os.MkdirAll(dayDir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dayDir, "rollout-2026-06-15T10-00-00-"+codexSweepSessionKey+".jsonl")
	lines := []string{
		fmt.Sprintf(`{"timestamp":"2026-06-15T10:00:00.000Z","type":"session_meta","payload":{"id":%q,"cwd":%q}}`, codexSweepSessionKey, workDir),
		`{"timestamp":"2026-06-15T10:00:01.000Z","type":"turn_context","payload":{"model":"gpt-5-codex"}}`,
	}
	for i, tc := range tokenCounts {
		lines = append(lines, fmt.Sprintf(
			`{"timestamp":"2026-06-15T10:00:%02dZ","type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"total_tokens":%d},"last_token_usage":{"input_tokens":%d,"cached_input_tokens":0,"output_tokens":%d}}}}`,
			i+2, tc[0], tc[1], tc[2]))
	}
	body := ""
	for _, l := range lines {
		body += l + "\n"
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func kindCount(facts []usage.Fact, kind usage.Kind) int {
	n := 0
	for _, f := range facts {
		if f.Kind == kind {
			n++
		}
	}
	return n
}

// rawSinkModelFactCount counts the model facts APPENDED to the sink file,
// without usage.ReadFacts's IdempotencyKey dedup. Idempotency assertions must
// use this: ReadFacts collapses a replayed fact at read time, so it would
// silently pass even if a tick re-recorded work the cursor should have skipped.
func rawSinkModelFactCount(t *testing.T, path string) int {
	t.Helper()
	body, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return 0
	}
	if err != nil {
		t.Fatalf("reading usage sink %s: %v", path, err)
	}
	n := 0
	for _, line := range strings.Split(string(body), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var f usage.Fact
		if err := json.Unmarshal([]byte(line), &f); err != nil {
			t.Fatalf("malformed usage fact %q: %v", line, err)
		}
		if f.Kind == usage.KindModel {
			n++
		}
	}
	return n
}

// liveSweepStart anchors a live fixture just behind the wall clock. A live
// session has no slept_at, so its transcript-discovery window runs from
// awake_started_at all the way to now: a hardcoded fixture date drifts out of
// discovery's bounded day lookback as real time advances, and the fixture stops
// being discoverable — the test would start failing for everyone on a fixed
// future day. Deriving the anchor from time.Now keeps the window an hour wide
// forever.
func liveSweepStart() time.Time {
	return time.Now().UTC().Add(-time.Hour)
}

// liveCodexSessionMeta is an AWAKE codex session's metadata: a non-terminal state
// and NO slept_at, so it is a live-lane candidate whose discovery window runs to
// the wall clock. An empty sessionKey is omitted entirely, selecting the keyless
// (work_dir, wake-window) discovery path.
func liveCodexSessionMeta(start time.Time, workDir, sessionKey string) map[string]string {
	meta := map[string]string{
		"state":            "active",
		"session_name":     "codex-live-1",
		"awake_started_at": start.Format(time.RFC3339),
		"work_dir":         workDir,
		"provider":         "codex",
		"builtin_ancestor": "codex",
		"molecule_id":      "run-L",
	}
	if sessionKey != "" {
		meta["session_key"] = sessionKey
	}
	return meta
}

// liveSweepHarness is the shared wiring for the live model-usage sweep cases: a
// session bead in a memory store, a real usage sink, and codexRoot as the only
// transcript search path. The cases differ only in session metadata and in what
// is on disk, so everything else is built once here.
type liveSweepHarness struct {
	cr       *CityRuntime
	store    *beads.MemStore
	meta     map[string]string
	beadID   string
	sinkPath string
	info     session.Info
}

func newLiveSweepHarness(t *testing.T, codexRoot string, meta map[string]string) liveSweepHarness {
	t.Helper()
	cityPath := t.TempDir()
	sinkPath := filepath.Join(cityPath, ".gc", "usage.jsonl")
	store := beads.NewMemStore()
	cfg := &config.City{Daemon: config.DaemonConfig{ObservePaths: []string{codexRoot}}}
	cs := &controllerState{cityBeadStore: store, usageSink: usage.NewLocalSink(sinkPath), cityName: "demo", cityPath: cityPath}
	h := liveSweepHarness{
		cr:       &CityRuntime{cs: cs, cfg: cfg, sp: runtime.NewFake(), cityName: "demo", cityPath: cityPath, stderr: io.Discard},
		store:    store,
		meta:     meta,
		sinkPath: sinkPath,
	}
	h.info = h.addSession(t, meta)
	h.beadID = h.info.ID
	return h
}

// addSession creates another session bead in the harness store and returns the
// snapshot row the reconcile tick would hand emitDueComputeFacts for it.
func (h liveSweepHarness) addSession(t *testing.T, meta map[string]string) session.Info {
	t.Helper()
	b, err := h.store.Create(beads.Bead{
		Type:     session.BeadType,
		Status:   "open",
		Title:    meta["session_name"],
		Labels:   []string{session.LabelSession},
		Metadata: meta,
	})
	if err != nil {
		t.Fatal(err)
	}
	return session.Info{ID: b.ID, MetadataState: meta["state"], AwakeStartedAt: meta["awake_started_at"]}
}

// tick runs one STEADY-STATE reconcile-tick usage pass over the harness session.
// The boot pass is driven directly by the one case that covers it, which needs a
// two-session snapshot anyway.
func (h liveSweepHarness) tick() {
	h.cr.emitDueComputeFacts(context.Background(), []session.Info{h.info}, false)
}

// memo returns the live-sweep memo the tick holds for this session's current
// awake epoch and conversation.
func (h liveSweepHarness) memo() liveSweepMemo {
	return h.cr.liveSweepMemoFor(h.beadID, h.meta["awake_started_at"], h.meta["session_key"])
}

// expireSweepThrottle clears the session's liveModelSweepMinInterval floor,
// standing in for that interval elapsing between ticks. It preserves the rest of
// the memo (resolved path, settled-miss sentinel) so a case advances only the
// clock.
func (h liveSweepHarness) expireSweepThrottle() {
	memo := h.memo()
	memo.nextSweepAt = time.Time{}
	h.cr.storeLiveSweepMemo(h.beadID, memo)
}

func (h liveSweepHarness) cursor(t *testing.T) string {
	t.Helper()
	b, err := h.store.Get(h.beadID)
	if err != nil {
		t.Fatal(err)
	}
	return b.Metadata[session.MetadataKeyInvocationUsageCursor]
}

// TestEmitDueComputeFactsSweepsLiveSessionModelUsage is the undercount
// regression for "model calls today": model facts used to be minted only by the
// terminal end-of-interval sweep, so a session that stayed awake for hours
// contributed nothing to the day's totals until it finally closed. The reconcile
// tick must sweep an AWAKE session's transcript incrementally — billing each
// invocation as it lands, without minting a compute fact or closing the still-open
// interval — and the persisted invocation cursor must make a tick with no new
// transcript activity a no-op.
func TestEmitDueComputeFactsSweepsLiveSessionModelUsage(t *testing.T) {
	workDir := t.TempDir()
	codexRoot := t.TempDir()
	start := liveSweepStart()
	writeCodexRolloutForSweepAt(t, codexRoot, start, workDir, codexSweepSessionKey, [][3]int{
		{150, 100, 50},  // total=150, last input=100, output=50
		{450, 200, 100}, // total=450, last input=200, output=100
	})
	h := newLiveSweepHarness(t, codexRoot, liveCodexSessionMeta(start, workDir, codexSweepSessionKey))

	// Tick 1: nothing terminal has happened, yet both invocations already on the
	// transcript must bill now instead of waiting for retirement.
	h.tick()
	facts1, warnings, err := usage.ReadFacts(h.sinkPath)
	if err != nil {
		t.Fatalf("ReadFacts (tick 1): %v", err)
	}
	if len(warnings) != 0 {
		t.Fatalf("unexpected sink warnings: %v", warnings)
	}
	if got := kindCount(facts1, usage.KindModel); got != 2 {
		t.Fatalf("tick 1 model facts = %d, want 2 (a live session's invocations must bill before it closes); facts: %+v", got, facts1)
	}
	if got := kindCount(facts1, usage.KindCompute); got != 0 {
		t.Fatalf("tick 1 compute facts = %d, want 0: the awake interval has not ended", got)
	}
	for _, f := range facts1 {
		if f.RunID != "run-L" {
			t.Fatalf("fact RunID = %q, want run-L: %+v", f.RunID, f)
		}
		if f.Provider != "codex" {
			t.Fatalf("model fact Provider = %q, want codex", f.Provider)
		}
	}

	// The interval stays OPEN: stamping either accounting marker on a live sweep
	// would suppress the real end-of-interval compute fact and terminal sweep.
	afterTick1, err := h.store.Get(h.beadID)
	if err != nil {
		t.Fatal(err)
	}
	if got := afterTick1.Metadata[session.MetadataKeyInvocationUsageCursor]; got != "total:450" {
		t.Fatalf("invocation_usage_cursor = %q, want total:450 (advanced past the swept batch)", got)
	}
	if got := afterTick1.Metadata[usageComputeEmittedAtKey]; got != "" {
		t.Fatalf("live sweep closed the awake interval (usage_compute_emitted_at = %q), want unset", got)
	}
	if got := afterTick1.Metadata[usageModelSweptAtKey]; got != "" {
		t.Fatalf("live sweep stamped the terminal sweep marker (%q), want unset while the interval accumulates", got)
	}

	// Discovery is memoized for this awake epoch so a per-tick sweep does not
	// repeat the bounded rollout scan.
	if memo := h.memo(); memo.path == "" {
		t.Fatalf("tick 1 did not memoize the resolved transcript path: memo=%+v", memo)
	}

	// Tick 2: no transcript activity. The sweep-interval floor is cleared first so
	// this asserts the CURSOR, not the throttle (TestEmitDueComputeFactsThrottles-
	// LiveModelSweep owns the floor): a live session stays a candidate on every
	// tick, so only the persisted cursor prevents a double-count.
	h.expireSweepThrottle()
	h.tick()
	if got := rawSinkModelFactCount(t, h.sinkPath); got != 2 {
		t.Fatalf("tick 2 appended model facts to a total of %d, want 2: the cursor must make a no-activity tick a no-op", got)
	}

	// Tick 3: one new invocation lands. Only that delta bills, and the session is
	// still awake, so still no compute fact.
	writeCodexRolloutForSweepAt(t, codexRoot, start, workDir, codexSweepSessionKey, [][3]int{
		{150, 100, 50},
		{450, 200, 100},
		{750, 300, 150},
	})
	h.expireSweepThrottle()
	h.tick()
	facts3, _, err := usage.ReadFacts(h.sinkPath)
	if err != nil {
		t.Fatalf("ReadFacts (tick 3): %v", err)
	}
	if got := kindCount(facts3, usage.KindModel); got != 3 {
		t.Fatalf("tick 3 model facts = %d, want 3 (one appended invocation): %+v", got, facts3)
	}
	if got := rawSinkModelFactCount(t, h.sinkPath); got != 3 {
		t.Fatalf("tick 3 appended model facts to a total of %d, want 3 (only the delta)", got)
	}
	if got := kindCount(facts3, usage.KindCompute); got != 0 {
		t.Fatalf("tick 3 compute facts = %d, want 0 while the session is awake", got)
	}
	if got := h.cursor(t); got != "total:750" {
		t.Fatalf("invocation_usage_cursor = %q, want total:750", got)
	}
}

// TestEmitDueComputeFactsThrottlesLiveModelSweep pins the live lane's cost
// bound. Unlike the terminal lane — gated by a persisted per-interval marker, so
// it touches a session once — a live session is a candidate on EVERY tick, so
// without a floor the reconcile tick would run bounded transcript discovery and a
// transcript read for every awake session at whatever cadence the tick spins
// (pokes drive it sub-second). A session must be swept at most once per
// liveModelSweepMinInterval no matter how often the tick fires.
func TestEmitDueComputeFactsThrottlesLiveModelSweep(t *testing.T) {
	workDir := t.TempDir()
	codexRoot := t.TempDir()
	start := liveSweepStart()
	writeCodexRolloutForSweepAt(t, codexRoot, start, workDir, codexSweepSessionKey, [][3]int{
		{150, 100, 50},
		{450, 200, 100},
	})
	h := newLiveSweepHarness(t, codexRoot, liveCodexSessionMeta(start, workDir, codexSweepSessionKey))

	h.tick()
	if got := rawSinkModelFactCount(t, h.sinkPath); got != 2 {
		t.Fatalf("tick 1 model facts = %d, want 2", got)
	}
	if memo := h.memo(); !memo.nextSweepAt.After(time.Now().UTC()) {
		t.Fatalf("tick 1 left the sweep-interval floor unarmed: nextSweepAt=%v", memo.nextSweepAt)
	}

	// A third invocation lands and the tick fires again immediately. The session is
	// inside its floor, so it must not be re-swept — no discovery, no transcript
	// read, no fact — even though there is genuinely new usage waiting.
	writeCodexRolloutForSweepAt(t, codexRoot, start, workDir, codexSweepSessionKey, [][3]int{
		{150, 100, 50},
		{450, 200, 100},
		{750, 300, 150},
	})
	h.tick()
	if got := rawSinkModelFactCount(t, h.sinkPath); got != 2 {
		t.Fatalf("tick 2 model facts = %d, want 2: a tick inside liveModelSweepMinInterval must not re-sweep the session", got)
	}
	if got := h.cursor(t); got != "total:450" {
		t.Fatalf("invocation_usage_cursor = %q, want total:450 (unmoved by the throttled tick)", got)
	}

	// Once the floor elapses the same session is swept again and the delta bills:
	// the throttle delays a sweep, it never drops one.
	h.expireSweepThrottle()
	h.tick()
	if got := rawSinkModelFactCount(t, h.sinkPath); got != 3 {
		t.Fatalf("tick 3 model facts = %d, want 3 (the delta bills once the floor elapses)", got)
	}
}

// TestEmitDueComputeFactsSkipsLiveModelSweepOnBootPass pins the boot carve-out.
// The boot reconcile covers the WHOLE fleet at once on the synchronous readiness
// path, which is exactly where fleet-proportional per-session file discovery and
// reads must not land. The live lane therefore waits for the first steady-state
// tick — while the terminal lane, whose per-interval marker makes it self-limiting,
// keeps running on boot as before.
func TestEmitDueComputeFactsSkipsLiveModelSweepOnBootPass(t *testing.T) {
	workDir := t.TempDir()
	codexRoot := t.TempDir()
	start := liveSweepStart()
	writeCodexRolloutForSweepAt(t, codexRoot, start, workDir, codexSweepSessionKey, [][3]int{
		{150, 100, 50},
		{450, 200, 100},
	})
	h := newLiveSweepHarness(t, codexRoot, liveCodexSessionMeta(start, workDir, codexSweepSessionKey))

	// A retired session in the same snapshot: its interval ended, so the terminal
	// lane owes it a compute fact on the boot pass. Its own workdir has no rollout,
	// so it contributes no model facts either way.
	slept := start.Add(90 * time.Second)
	terminal := h.addSession(t, map[string]string{
		"state":            "asleep",
		"session_name":     "codex-retired-1",
		"awake_started_at": start.Format(time.RFC3339),
		"slept_at":         slept.Format(time.RFC3339),
		"session_key":      "019e7777-cccc-7000-8000-00000000000b",
		"work_dir":         t.TempDir(),
		"provider":         "codex",
		"builtin_ancestor": "codex",
		"molecule_id":      "run-T",
	})
	snapshot := []session.Info{h.info, terminal}

	h.cr.emitDueComputeFacts(context.Background(), snapshot, true)
	bootFacts, _, err := usage.ReadFacts(h.sinkPath)
	if err != nil {
		t.Fatalf("ReadFacts (boot pass): %v", err)
	}
	if got := kindCount(bootFacts, usage.KindModel); got != 0 {
		t.Fatalf("boot pass model facts = %d, want 0: the live lane must not run on the fleet-wide boot reconcile; facts: %+v", got, bootFacts)
	}
	if got := kindCount(bootFacts, usage.KindCompute); got != 1 {
		t.Fatalf("boot pass compute facts = %d, want 1: the terminal lane's cost profile is unchanged and must keep running on boot", got)
	}
	if got := h.cursor(t); got != "" {
		t.Fatalf("boot pass advanced the live session's invocation cursor to %q, want unset (it must not be swept at all)", got)
	}
	if memo := h.memo(); memo.path != "" || !memo.nextSweepAt.IsZero() {
		t.Fatalf("boot pass touched the live session's sweep memo: %+v", memo)
	}

	// The very next steady-state tick picks the live session up: boot DEFERS the
	// lane, it does not disable it.
	h.cr.emitDueComputeFacts(context.Background(), snapshot, false)
	steadyFacts, _, err := usage.ReadFacts(h.sinkPath)
	if err != nil {
		t.Fatalf("ReadFacts (steady tick): %v", err)
	}
	if got := kindCount(steadyFacts, usage.KindModel); got != 2 {
		t.Fatalf("steady tick model facts = %d, want 2 (the deferred live sweep runs on the first non-boot tick): %+v", got, steadyFacts)
	}
}

// TestEmitDueComputeFactsBacksOffUnresolvedLiveSweepDiscovery pins the miss
// memoization. A live session is a candidate on every tick, so an awake session
// whose transcript cannot be discovered yet would otherwise re-run the bounded
// rollout scan forever, once per tick, for its entire life. A TRANSIENT miss must
// be memoized as a backoff: re-attempted on the sweep floor rather than on every
// tick, and never dropped.
func TestEmitDueComputeFactsBacksOffUnresolvedLiveSweepDiscovery(t *testing.T) {
	workDir := t.TempDir()
	codexRoot := t.TempDir()
	start := liveSweepStart()
	h := newLiveSweepHarness(t, codexRoot, liveCodexSessionMeta(start, workDir, codexSweepSessionKey))

	// Tick 1: the keyed rollout is not on disk yet. That miss is transient — the
	// file may simply not be flushed — so it must not settle.
	h.tick()
	if got := rawSinkModelFactCount(t, h.sinkPath); got != 0 {
		t.Fatalf("tick 1 model facts = %d, want 0 (no transcript on disk yet)", got)
	}
	memo := h.memo()
	if memo.settledMiss {
		t.Fatal("a keyed rollout that is merely not flushed yet must not be memoized as a settled miss")
	}
	if memo.nextSweepAt.IsZero() {
		t.Fatal("an unresolved discovery must still arm the sweep floor, or discovery re-runs on every tick forever")
	}

	// The transcript lands immediately after. Tick 2 is inside the backoff, so
	// discovery is NOT re-attempted.
	writeCodexRolloutForSweepAt(t, codexRoot, start, workDir, codexSweepSessionKey, [][3]int{
		{150, 100, 50},
		{450, 200, 100},
	})
	h.tick()
	if got := rawSinkModelFactCount(t, h.sinkPath); got != 0 {
		t.Fatalf("tick 2 model facts = %d, want 0: a memoized miss must not re-run discovery inside its backoff", got)
	}

	// Once the floor elapses discovery is retried and the pending usage is
	// recovered.
	h.expireSweepThrottle()
	h.tick()
	if got := rawSinkModelFactCount(t, h.sinkPath); got != 2 {
		t.Fatalf("tick 3 model facts = %d, want 2 (discovery retried once the backoff elapsed)", got)
	}
}

// TestEmitDueComputeFactsStopsRetryingSettledLiveSweepMiss pins the other half of
// the miss memoization: a DEFINITIVE miss is not retried at all. A keyless codex
// session whose bounded (work_dir, wake-window) scan comes up empty on a CLEAN
// scan has nothing to find — the outcome is ambiguity, an out-of-window filename,
// or a TZ shift, none of which a retry resolves — so the live lane records the
// verdict once and stops scanning for that awake epoch.
func TestEmitDueComputeFactsStopsRetryingSettledLiveSweepMiss(t *testing.T) {
	workDir := t.TempDir()
	codexRoot := t.TempDir()
	start := liveSweepStart()
	// No session_key: discovery takes the keyless workdir+window fallback.
	h := newLiveSweepHarness(t, codexRoot, liveCodexSessionMeta(start, workDir, ""))

	h.tick()
	memo := h.memo()
	if !memo.settledMiss {
		t.Fatalf("a clean keyless scan that found nothing is definitive and must settle: memo=%+v", memo)
	}

	// Even with the floor cleared AND a matching rollout now on disk, the settled
	// miss is never re-attempted. Nothing is lost: the interval's usage is still
	// recovered by the terminal sweep when the session finally closes, and a re-wake
	// starts a fresh epoch that discovers again.
	writeCodexRolloutForSweepAt(t, codexRoot, start, workDir, "019e7777-cccc-7000-8000-00000000000c", [][3]int{
		{150, 100, 50},
	})
	h.expireSweepThrottle()
	h.tick()
	if got := rawSinkModelFactCount(t, h.sinkPath); got != 0 {
		t.Fatalf("model facts = %d, want 0: a settled discovery miss must never re-run discovery", got)
	}
	if got := h.cursor(t); got != "" {
		t.Fatalf("invocation_usage_cursor = %q, want unset (nothing was swept)", got)
	}
}

// TestEmitDueComputeFactsAlsoSweepsModelUsage is the CORE regression for the
// token-starvation bug: the controller reconcile tick emits per-interval compute
// facts but never any model facts for pool-routed, hook-self-driven codex agents,
// because the only model-fact emitter is coupled to prompt-op finish. The tick
// must, beside the compute fact, sweep the terminal session's transcript for the
// trailing invocations no prompt op recorded and emit one model fact per
// invocation. Before the sweep existed only the compute fact appeared.
func TestEmitDueComputeFactsAlsoSweepsModelUsage(t *testing.T) {
	cityPath := t.TempDir()
	workDir := t.TempDir()
	codexRoot := t.TempDir()
	sinkPath := filepath.Join(cityPath, ".gc", "usage.jsonl")

	// Codex names rollouts by the session_key uuid suffix; the keyed no-window
	// discovery matches on exactly that.
	sessionKey := codexSweepSessionKey
	writeCodexRolloutForSweep(t, codexRoot, workDir, [][3]int{
		{150, 100, 50},  // total=150, last input=100, output=50
		{450, 200, 100}, // total=450, last input=200, output=100
	})

	store := beads.NewMemStore()
	start := time.Date(2026, 6, 15, 10, 0, 0, 0, time.UTC)
	slept := start.Add(90 * time.Second)
	b, err := store.Create(beads.Bead{
		Type:   session.BeadType,
		Status: "open",
		Title:  "codex session",
		Labels: []string{session.LabelSession},
		Metadata: map[string]string{
			"state":            "asleep",
			"session_name":     "codex-1",
			"awake_started_at": start.Format(time.RFC3339),
			"slept_at":         slept.Format(time.RFC3339),
			"session_key":      sessionKey,
			"work_dir":         workDir,
			"provider":         "mc-codex-wrap", // wrapped manifold name
			"builtin_ancestor": "codex",         // canonical ladder resolves this to codex
			"molecule_id":      "run-Z",
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	cfg := &config.City{Daemon: config.DaemonConfig{ObservePaths: []string{codexRoot}}}
	cs := &controllerState{
		cityBeadStore: store,
		usageSink:     usage.NewLocalSink(sinkPath),
		cityName:      "demo",
		cityPath:      cityPath,
	}
	cr := &CityRuntime{cs: cs, cfg: cfg, sp: runtime.NewFake(), cityName: "demo", cityPath: cityPath, stderr: io.Discard}

	info := session.Info{ID: b.ID, MetadataState: "asleep", AwakeStartedAt: start.Format(time.RFC3339)}
	cr.emitDueComputeFacts(context.Background(), []session.Info{info}, false)

	facts, warnings, err := usage.ReadFacts(sinkPath)
	if err != nil {
		t.Fatalf("ReadFacts: %v", err)
	}
	if len(warnings) != 0 {
		t.Fatalf("unexpected sink warnings: %v", warnings)
	}
	if got := kindCount(facts, usage.KindCompute); got != 1 {
		t.Fatalf("compute facts = %d, want 1", got)
	}
	if got := kindCount(facts, usage.KindModel); got != 2 {
		t.Fatalf("model facts = %d, want 2 (the tick must sweep the two trailing invocations); got facts: %+v", got, facts)
	}

	// Every fact — compute and both model — must carry the SAME RunID so gc costs
	// groups them under one run.
	for _, f := range facts {
		if f.RunID != "run-Z" {
			t.Fatalf("fact RunID = %q, want run-Z (shared across kinds): %+v", f.RunID, f)
		}
	}
	// Token deltas: the two model facts carry the per-invocation last-usage.
	seen := map[string]bool{}
	for _, f := range facts {
		if f.Kind != usage.KindModel {
			continue
		}
		seen[fmt.Sprintf("%d/%d", f.InputTokens, f.OutputTokens)] = true
		if f.StepID != "" {
			t.Fatalf("model fact StepID = %q, want empty (run-level attribution)", f.StepID)
		}
		if f.Provider != "codex" {
			t.Fatalf("model fact Provider = %q, want codex (wrapped name resolved via builtin_ancestor)", f.Provider)
		}
	}
	if !seen["100/50"] || !seen["200/100"] {
		t.Fatalf("model facts missing expected token deltas; saw %v", seen)
	}

	// The invocation-usage cursor advanced to the newest invocation identity.
	refreshed, err := store.Get(b.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := refreshed.Metadata[session.MetadataKeyInvocationUsageCursor]; got != "total:450" {
		t.Fatalf("invocation_usage_cursor = %q, want total:450 (advanced past the swept batch)", got)
	}

	// A second tick must add no new facts: the cursor blocks the model re-record
	// and the emit marker blocks the compute re-emit; ReadFacts also dedups any
	// replay by IdempotencyKey.
	cr.emitDueComputeFacts(context.Background(), []session.Info{info}, false)
	facts2, _, err := usage.ReadFacts(sinkPath)
	if err != nil {
		t.Fatalf("ReadFacts (second tick): %v", err)
	}
	if kindCount(facts2, usage.KindCompute) != 1 || kindCount(facts2, usage.KindModel) != 2 {
		t.Fatalf("second tick changed fact counts: compute=%d model=%d, want 1/2",
			kindCount(facts2, usage.KindCompute), kindCount(facts2, usage.KindModel))
	}
}

// TestEmitDueComputeFactsRetriesUnsettledModelSweep pins P2-2: a transient
// model-sweep miss (here the codex rollout has not been flushed to disk yet at
// interval end) must NOT permanently lose the interval's model usage. Because the
// sweep is gated by its own marker (distinct from the compute marker), the
// interval stays a candidate and the sweep retries on the next tick once the
// transcript appears — recovering the model facts without duplicating the compute
// fact.
func TestEmitDueComputeFactsRetriesUnsettledModelSweep(t *testing.T) {
	cityPath := t.TempDir()
	workDir := t.TempDir()
	codexRoot := t.TempDir()
	sinkPath := filepath.Join(cityPath, ".gc", "usage.jsonl")
	sessionKey := codexSweepSessionKey

	store := beads.NewMemStore()
	start := time.Date(2026, 6, 15, 10, 0, 0, 0, time.UTC)
	slept := start.Add(90 * time.Second)
	b, err := store.Create(beads.Bead{
		Type:   session.BeadType,
		Status: "open",
		Title:  "codex session",
		Labels: []string{session.LabelSession},
		Metadata: map[string]string{
			"state":            "asleep",
			"session_name":     "codex-1",
			"awake_started_at": start.Format(time.RFC3339),
			"slept_at":         slept.Format(time.RFC3339),
			"session_key":      sessionKey,
			"work_dir":         workDir,
			"provider":         "codex",
			"builtin_ancestor": "codex",
			"molecule_id":      "run-Z",
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	cfg := &config.City{Daemon: config.DaemonConfig{ObservePaths: []string{codexRoot}}}
	cs := &controllerState{cityBeadStore: store, usageSink: usage.NewLocalSink(sinkPath), cityName: "demo", cityPath: cityPath}
	cr := &CityRuntime{cs: cs, cfg: cfg, sp: runtime.NewFake(), cityName: "demo", cityPath: cityPath, stderr: io.Discard}
	info := session.Info{ID: b.ID, MetadataState: "asleep", AwakeStartedAt: start.Format(time.RFC3339)}

	// Tick 1: the rollout is not on disk yet → the sweep misses (transient). The
	// compute fact still records, but neither the compute marker nor the sweep
	// marker is stamped, so the interval stays open for retry.
	cr.emitDueComputeFacts(context.Background(), []session.Info{info}, false)
	facts1, _, err := usage.ReadFacts(sinkPath)
	if err != nil {
		t.Fatalf("ReadFacts (tick 1): %v", err)
	}
	if kindCount(facts1, usage.KindCompute) != 1 {
		t.Fatalf("tick 1 compute facts = %d, want 1 (compute is never delayed by a pending sweep)", kindCount(facts1, usage.KindCompute))
	}
	if kindCount(facts1, usage.KindModel) != 0 {
		t.Fatalf("tick 1 model facts = %d, want 0 (rollout not flushed yet)", kindCount(facts1, usage.KindModel))
	}
	afterTick1, err := store.Get(b.ID)
	if err != nil {
		t.Fatal(err)
	}
	if afterTick1.Metadata[usageComputeEmittedAtKey] != "" {
		t.Fatal("tick 1 must leave usage_compute_emitted_at unset so the interval stays a candidate for the sweep retry")
	}
	if afterTick1.Metadata[usageModelSweptAtKey] != "" {
		t.Fatal("tick 1 must leave the sweep marker unset (the sweep did not settle)")
	}

	// The transcript is flushed to disk between ticks.
	writeCodexRolloutForSweep(t, codexRoot, workDir, [][3]int{
		{150, 100, 50},
		{450, 200, 100},
	})

	// Tick 2: the interval is still a candidate → the sweep retries, discovers the
	// rollout, and recovers the model facts. No duplicate compute fact.
	cr.emitDueComputeFacts(context.Background(), []session.Info{info}, false)
	facts2, _, err := usage.ReadFacts(sinkPath)
	if err != nil {
		t.Fatalf("ReadFacts (tick 2): %v", err)
	}
	if got := kindCount(facts2, usage.KindCompute); got != 1 {
		t.Fatalf("tick 2 compute facts = %d, want 1 (the re-recorded compute fact dedups by IdempotencyKey)", got)
	}
	if got := kindCount(facts2, usage.KindModel); got != 2 {
		t.Fatalf("tick 2 model facts = %d, want 2 (recovered on retry): %+v", got, facts2)
	}
	afterTick2, err := store.Get(b.ID)
	if err != nil {
		t.Fatal(err)
	}
	awake := start.Format(time.RFC3339)
	if afterTick2.Metadata[usageComputeEmittedAtKey] != awake {
		t.Fatalf("tick 2 must commit the interval (usage_compute_emitted_at=%q), got %q", awake, afterTick2.Metadata[usageComputeEmittedAtKey])
	}
	if afterTick2.Metadata[usageModelSweptAtKey] != awake {
		t.Fatalf("tick 2 must stamp the sweep marker (%q), got %q", awake, afterTick2.Metadata[usageModelSweptAtKey])
	}

	// Tick 3: both markers set → no re-Get work, no new facts.
	info.UsageComputeEmittedAt = awake // reflects the committed interval on the snapshot
	cr.emitDueComputeFacts(context.Background(), []session.Info{info}, false)
	facts3, _, err := usage.ReadFacts(sinkPath)
	if err != nil {
		t.Fatalf("ReadFacts (tick 3): %v", err)
	}
	if kindCount(facts3, usage.KindCompute) != 1 || kindCount(facts3, usage.KindModel) != 2 {
		t.Fatalf("tick 3 changed counts: compute=%d model=%d, want 1/2", kindCount(facts3, usage.KindCompute), kindCount(facts3, usage.KindModel))
	}
}

// writeCodexRolloutForSweepAt fabricates a codex rollout at the local-date path
// the codex CLI would use for `at` (session_meta cwd=workDir, a turn_context
// model, one token_count per {total, lastInput, lastOutput}). Unlike
// writeCodexRolloutForSweep — which hardcodes 2026-06-15, fine only for a fixture
// whose slept_at also pins the discovery window to that date — it derives BOTH the
// day dir and the filename timestamp from `at` in time.Local, matching how codex
// names rollouts and how discovery parses them. Any test whose discovery window
// runs to the wall clock (a live session has no slept_at) must use this: a
// hardcoded day falls out of the bounded lookback as real time advances, so the
// fixture would silently stop being discoverable.
func writeCodexRolloutForSweepAt(t *testing.T, root string, at time.Time, workDir, sessionID string, tokenCounts [][3]int) {
	t.Helper()
	local := at.In(time.Local)
	dayDir := filepath.Join(root, local.Format("2006"), local.Format("01"), local.Format("02"))
	if err := os.MkdirAll(dayDir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dayDir, "rollout-"+local.Format("2006-01-02T15-04-05")+"-"+sessionID+".jsonl")
	const ms = "2006-01-02T15:04:05.000Z07:00"
	lines := []string{
		fmt.Sprintf(`{"timestamp":%q,"type":"session_meta","payload":{"id":%q,"cwd":%q}}`,
			at.UTC().Format(ms), sessionID, workDir),
		fmt.Sprintf(`{"timestamp":%q,"type":"turn_context","payload":{"model":"gpt-5-codex"}}`,
			at.Add(time.Second).UTC().Format(ms)),
	}
	for i, tc := range tokenCounts {
		lines = append(lines, fmt.Sprintf(
			`{"timestamp":%q,"type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"total_tokens":%d},"last_token_usage":{"input_tokens":%d,"cached_input_tokens":0,"output_tokens":%d}}}}`,
			at.Add(time.Duration(i+2)*time.Second).UTC().Format(ms), tc[0], tc[1], tc[2]))
	}
	body := ""
	for _, l := range lines {
		body += l + "\n"
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestEmitDueComputeFactsSweepsKeylessCodexViaWorkdir is the maintainer-city
// production regression for Design B: graph.v2 wisp codex sessions NEVER captured
// a session_key (the split city's metadata table had zero session_key rows ever),
// so the model sweep minted nothing and factory token counts stayed 0 even though
// compute facts flowed fine. The end-of-interval sweep must recover them by
// discovering the rollout through (work_dir, interval-window) with no session_key,
// mint the trailing model facts, and settle the interval.
func TestEmitDueComputeFactsSweepsKeylessCodexViaWorkdir(t *testing.T) {
	cityPath := t.TempDir()
	workDir := t.TempDir()
	codexRoot := t.TempDir()
	sinkPath := filepath.Join(cityPath, ".gc", "usage.jsonl")

	start := time.Date(2026, 6, 15, 10, 0, 0, 0, time.UTC)
	slept := start.Add(90 * time.Second)
	// A keyless codex rollout in this wisp's unique worktree — no session_key keys
	// it; only the cwd + interval window resolve it.
	writeCodexRolloutForSweepAt(t, codexRoot, start, workDir, "019e7777-cccc-7000-8000-000000000009", [][3]int{
		{150, 100, 50},
		{450, 200, 100},
	})

	store := beads.NewMemStore()
	b, err := store.Create(beads.Bead{
		Type:   session.BeadType,
		Status: "open",
		Title:  "codex wisp session",
		Labels: []string{session.LabelSession},
		Metadata: map[string]string{
			"state":               "asleep",
			"session_name":        "codex-wisp-1",
			"awake_started_at":    start.Format(time.RFC3339),
			"slept_at":            slept.Format(time.RFC3339),
			"work_dir":            workDir,
			"provider":            "mc-codex-wrap", // wrapped manifold name
			"builtin_ancestor":    "codex",         // canonical ladder resolves to codex
			"molecule_id":         "run-Z",
			"gc.active_work_bead": "run-Z.step-1",
			// NB: NO session_key — the whole point.
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	cfg := &config.City{Daemon: config.DaemonConfig{ObservePaths: []string{codexRoot}}}
	cs := &controllerState{cityBeadStore: store, usageSink: usage.NewLocalSink(sinkPath), cityName: "demo", cityPath: cityPath}
	cr := &CityRuntime{cs: cs, cfg: cfg, sp: runtime.NewFake(), cityName: "demo", cityPath: cityPath, stderr: io.Discard}
	info := session.Info{ID: b.ID, MetadataState: "asleep", AwakeStartedAt: start.Format(time.RFC3339)}

	cr.emitDueComputeFacts(context.Background(), []session.Info{info}, false)

	facts, warnings, err := usage.ReadFacts(sinkPath)
	if err != nil {
		t.Fatalf("ReadFacts: %v", err)
	}
	if len(warnings) != 0 {
		t.Fatalf("unexpected sink warnings: %v", warnings)
	}
	if got := kindCount(facts, usage.KindCompute); got != 1 {
		t.Fatalf("compute facts = %d, want 1", got)
	}
	if got := kindCount(facts, usage.KindModel); got != 2 {
		t.Fatalf("model facts = %d, want 2 (keyless codex must be swept via work_dir); facts: %+v", got, facts)
	}
	for _, f := range facts {
		if f.RunID != "run-Z" {
			t.Fatalf("fact RunID = %q, want run-Z (shared across kinds): %+v", f.RunID, f)
		}
		if f.Kind == usage.KindModel && f.Provider != "codex" {
			t.Fatalf("model fact Provider = %q, want codex (wrapped name resolved via builtin_ancestor)", f.Provider)
		}
	}

	// The settled keyless sweep stamps the model-swept marker so the interval is not
	// re-swept every subsequent tick.
	refreshed, err := store.Get(b.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := refreshed.Metadata[usageModelSweptAtKey]; got != start.Format(time.RFC3339) {
		t.Fatalf("usage_model_swept_at = %q, want %q (a settled keyless sweep must mark the interval)", got, start.Format(time.RFC3339))
	}
}

func TestIsComputeTerminalState(t *testing.T) {
	// Every non-running endpoint the open-bead scan can observe.
	for _, s := range []string{"asleep", "drained", "archived", "suspended", "quarantined"} {
		if !isComputeTerminalState(s) {
			t.Errorf("%q should be terminal", s)
		}
	}
	// Running states, transient states, and closed (which leaves the open set the
	// scan reads) are not emitted by the scan.
	for _, s := range []string{"active", "awake", "creating", "draining", "closed", ""} {
		if isComputeTerminalState(s) {
			t.Errorf("%q should not be terminal", s)
		}
	}
}

func TestEmitRecentlyClosedUsageRecoversUnsweptSession(t *testing.T) {
	cityPath := t.TempDir()
	workDir := t.TempDir()
	codexRoot := t.TempDir()
	sinkPath := filepath.Join(cityPath, ".gc", "usage.jsonl")

	writeCodexRolloutForSweep(t, codexRoot, workDir, [][3]int{
		{150, 100, 50},
	})

	store := beads.NewMemStore()
	start := time.Date(2026, 6, 15, 10, 0, 0, 0, time.UTC)
	closedAt := time.Now().UTC().Add(-2 * time.Minute)
	b, err := store.Create(beads.Bead{
		Type:   session.BeadType,
		Status: "open",
		Title:  "closed before open-set sweep",
		Labels: []string{session.LabelSession},
		Metadata: map[string]string{
			"state":            "drained",
			"session_name":     "codex-closed-1",
			"awake_started_at": start.Format(time.RFC3339),
			"closed_at":        closedAt.Format(time.RFC3339),
			"session_key":      codexSweepSessionKey,
			"work_dir":         workDir,
			"provider":         "codex",
			"builtin_ancestor": "codex",
			"molecule_id":      "run-CLOSED",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	// MemStore.Create always stamps Status=open; close after create.
	closed := "closed"
	if err := store.Update(b.ID, beads.UpdateOpts{Status: &closed}); err != nil {
		t.Fatal(err)
	}

	cfg := &config.City{Daemon: config.DaemonConfig{ObservePaths: []string{codexRoot}}}
	cs := &controllerState{cityBeadStore: store, usageSink: usage.NewLocalSink(sinkPath), cityName: "demo", cityPath: cityPath}
	cr := &CityRuntime{cs: cs, cfg: cfg, sp: runtime.NewFake(), cityName: "demo", cityPath: cityPath, stderr: io.Discard}

	cr.emitRecentlyClosedUsage(context.Background())

	facts, warnings, err := usage.ReadFacts(sinkPath)
	if err != nil {
		t.Fatalf("ReadFacts: %v", err)
	}
	if len(warnings) != 0 {
		t.Fatalf("unexpected sink warnings: %v", warnings)
	}
	if got := kindCount(facts, usage.KindModel); got != 1 {
		t.Fatalf("model facts = %d, want 1 from recently-closed sweep; facts: %+v", got, facts)
	}
	if got := kindCount(facts, usage.KindCompute); got != 1 {
		t.Fatalf("compute facts = %d, want 1 from recently-closed sweep", got)
	}
	// The recovered interval must end at closed_at, not at the recovery pass's
	// wall clock: ClosePatch never stamps slept_at, so an unadjusted emit would
	// bill discovery time and inflate wall_seconds by the whole recovery lag.
	var compute usage.Fact
	for _, f := range facts {
		if f.Kind == usage.KindCompute {
			compute = f
			break
		}
	}
	wantWall := closedAt.Truncate(time.Second).Sub(start).Seconds()
	if delta := compute.WallSeconds - wantWall; delta < -1 || delta > 1 {
		t.Fatalf("compute wall_seconds = %v, want ~%v (interval must end at closed_at, not at recovery time)", compute.WallSeconds, wantWall)
	}
	refreshed, err := store.Get(b.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := refreshed.Metadata[usageModelSweptAtKey]; got != start.Format(time.RFC3339) {
		t.Fatalf("usage_model_swept_at = %q, want %q", got, start.Format(time.RFC3339))
	}
}

func TestEmitRecentlyClosedUsageThrottled(t *testing.T) {
	cityPath := t.TempDir()
	workDir := t.TempDir()
	codexRoot := t.TempDir()
	sinkPath := filepath.Join(cityPath, ".gc", "usage.jsonl")
	writeCodexRolloutForSweep(t, codexRoot, workDir, [][3]int{{150, 100, 50}})

	store := beads.NewMemStore()
	start := time.Date(2026, 6, 15, 10, 0, 0, 0, time.UTC)
	b, err := store.Create(beads.Bead{
		Type:   session.BeadType,
		Status: "open",
		Title:  "closed unswept",
		Labels: []string{session.LabelSession},
		Metadata: map[string]string{
			"state":            "drained",
			"session_name":     "codex-throttled",
			"awake_started_at": start.Format(time.RFC3339),
			"closed_at":        time.Now().UTC().Add(-time.Minute).Format(time.RFC3339),
			"session_key":      codexSweepSessionKey,
			"work_dir":         workDir,
			"provider":         "codex",
			"builtin_ancestor": "codex",
			"molecule_id":      "run-THROTTLE",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	closed := "closed"
	if err := store.Update(b.ID, beads.UpdateOpts{Status: &closed}); err != nil {
		t.Fatal(err)
	}

	cfg := &config.City{Daemon: config.DaemonConfig{ObservePaths: []string{codexRoot}}}
	cs := &controllerState{cityBeadStore: store, usageSink: usage.NewLocalSink(sinkPath), cityName: "demo", cityPath: cityPath}
	cr := &CityRuntime{
		cs:                        cs,
		cfg:                       cfg,
		sp:                        runtime.NewFake(),
		cityName:                  "demo",
		cityPath:                  cityPath,
		stderr:                    io.Discard,
		recentlyClosedUsageNextAt: time.Now().UTC().Add(time.Minute),
	}

	cr.emitRecentlyClosedUsage(context.Background())

	facts, _, err := usage.ReadFacts(sinkPath)
	if err != nil {
		t.Fatalf("ReadFacts: %v", err)
	}
	if got := kindCount(facts, usage.KindModel) + kindCount(facts, usage.KindCompute); got != 0 {
		t.Fatalf("throttled pass must not emit facts, got %d", got)
	}
}

func TestRecentlyClosedUsageCandidate(t *testing.T) {
	now := time.Now().UTC()
	start := now.Add(-time.Hour).Format(time.RFC3339)
	ok := beads.Bead{
		Status: "closed",
		Metadata: map[string]string{
			"awake_started_at": start,
			"closed_at":        now.Add(-time.Minute).Format(time.RFC3339),
		},
	}
	if !recentlyClosedUsageCandidate(ok, now) {
		t.Fatal("recent closed unswept bead should be a candidate")
	}
	ok.Metadata[usageModelSweptAtKey] = start
	ok.Metadata[usageComputeEmittedAtKey] = start
	if recentlyClosedUsageCandidate(ok, now) {
		t.Fatal("fully accounted closed bead must not be a candidate")
	}
	stale := beads.Bead{
		Status: "closed",
		Metadata: map[string]string{
			"awake_started_at": start,
			"closed_at":        now.Add(-time.Hour).Format(time.RFC3339),
		},
	}
	if recentlyClosedUsageCandidate(stale, now) {
		t.Fatal("closed outside the window must not be a candidate")
	}
}
