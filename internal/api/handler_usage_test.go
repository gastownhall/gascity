package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/usage"
)

func writeUsageLines(t *testing.T, cityPath string, lines []string) {
	t.Helper()
	dir := filepath.Join(cityPath, ".gc")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	data := ""
	for _, l := range lines {
		data += l + "\n"
	}
	if err := os.WriteFile(filepath.Join(dir, "usage.jsonl"), []byte(data), 0o644); err != nil {
		t.Fatalf("write usage.jsonl: %v", err)
	}
}

func usageFactLine(t *testing.T, f usage.Fact) string {
	t.Helper()
	b, err := json.Marshal(f)
	if err != nil {
		t.Fatalf("marshal fact: %v", err)
	}
	return string(b)
}

func TestHandleUsageAggregatesWindows(t *testing.T) {
	state := newFakeState(t)
	nowMs := time.Now().UnixMilli()
	oldMs := time.Now().Add(-48 * time.Hour).UnixMilli()
	nowFact := usage.Fact{
		Kind: usage.KindModel, Model: "claude-sonnet-5",
		Worker: "myrig/pack.worker_a", SessionID: "sess-a",
		InputTokens: 1000, OutputTokens: 500,
		CacheReadTokens: 200, CacheCreationTokens: 100,
		CostUSDEstimate: 0.25, At: nowMs, IdempotencyKey: "k-now",
	}
	lines := []string{
		usageFactLine(t, nowFact),
		// Same idempotency key appended twice: ReadFacts dedups; totals must
		// count it once.
		usageFactLine(t, nowFact),
		usageFactLine(t, usage.Fact{
			Kind: usage.KindModel, Model: "claude-sonnet-5",
			InputTokens: 2000, OutputTokens: 1000,
			CostUSDEstimate: 0.5, At: oldMs, IdempotencyKey: "k-old",
		}),
		usageFactLine(t, usage.Fact{
			Kind: usage.KindCompute, Runtime: "local",
			WallSeconds: 12.5, At: nowMs, IdempotencyKey: "k-compute",
		}),
		usageFactLine(t, usage.Fact{
			Kind: usage.KindModel, Model: "mystery",
			Worker: "myrig/pack.worker_b", SessionID: "sess-b",
			InputTokens: 10, OutputTokens: 5,
			// Non-conformant on purpose: unpriced facts should carry zero
			// cost, but a corrupt record must still never count as spend.
			Unpriced: true, CostUSDEstimate: 9.99, At: nowMs, IdempotencyKey: "k-unpriced",
		}),
		`{not json`,
	}
	writeUsageLines(t, state.cityPath, lines)
	h := newTestCityHandler(t, state)

	req := httptest.NewRequest("GET", cityURL(state, "/usage"), nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d (body: %s)", rec.Code, http.StatusOK, rec.Body.String())
	}
	var body UsageBody
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if body.Totals.Invocations != 3 {
		t.Errorf("Totals.Invocations = %d, want 3", body.Totals.Invocations)
	}
	if body.Totals.ComputeFacts != 1 {
		t.Errorf("Totals.ComputeFacts = %d, want 1", body.Totals.ComputeFacts)
	}
	if body.Totals.InputTokens != 3010 {
		t.Errorf("Totals.InputTokens = %d, want 3010", body.Totals.InputTokens)
	}
	if body.Totals.OutputTokens != 1505 {
		t.Errorf("Totals.OutputTokens = %d, want 1505", body.Totals.OutputTokens)
	}
	if body.Totals.CacheReadTokens != 200 || body.Totals.CacheCreationTokens != 100 {
		t.Errorf("Totals cache tokens = %d/%d, want 200/100",
			body.Totals.CacheReadTokens, body.Totals.CacheCreationTokens)
	}
	if body.Totals.CostUSDEstimate != 0.75 {
		t.Errorf("Totals.CostUSDEstimate = %v, want 0.75", body.Totals.CostUSDEstimate)
	}
	if body.Totals.Unpriced != 1 {
		t.Errorf("Totals.Unpriced = %d, want 1", body.Totals.Unpriced)
	}
	if body.Totals.WallSeconds != 12.5 {
		t.Errorf("Totals.WallSeconds = %v, want 12.5", body.Totals.WallSeconds)
	}

	// The 48h-old fact is in totals but not today/recent.
	if body.Today.Invocations != 2 {
		t.Errorf("Today.Invocations = %d, want 2", body.Today.Invocations)
	}
	if body.Today.InputTokens != 1010 {
		t.Errorf("Today.InputTokens = %d, want 1010", body.Today.InputTokens)
	}
	if body.Today.CostUSDEstimate != 0.25 {
		t.Errorf("Today.CostUSDEstimate = %v, want 0.25", body.Today.CostUSDEstimate)
	}
	if body.Today.ComputeFacts != 1 || body.Today.WallSeconds != 12.5 {
		t.Errorf("Today compute = %d/%v, want 1/12.5",
			body.Today.ComputeFacts, body.Today.WallSeconds)
	}
	if body.Recent.Invocations != 2 || body.Recent.InputTokens != 1010 {
		t.Errorf("Recent = %d invocations/%d input tokens, want 2/1010",
			body.Recent.Invocations, body.Recent.InputTokens)
	}
	if body.RecentWindowSecs <= 0 {
		t.Errorf("RecentWindowSecs = %d, want > 0", body.RecentWindowSecs)
	}

	// Per-session recent breakdown: sorted by total tokens desc, only
	// sessions with recent-window model facts, keyed by worker name.
	if len(body.RecentBySession) != 2 {
		t.Fatalf("RecentBySession = %+v, want 2 sessions", body.RecentBySession)
	}
	if body.RecentBySession[0].Session != "myrig/pack.worker_a" ||
		body.RecentBySession[0].SessionID != "sess-a" {
		t.Errorf("RecentBySession[0] = %+v, want worker_a/sess-a first (most tokens)",
			body.RecentBySession[0])
	}
	if got := body.RecentBySession[0].InputTokens + body.RecentBySession[0].OutputTokens; got != 1500 {
		t.Errorf("RecentBySession[0] tokens = %d, want 1500", got)
	}
	if body.RecentBySession[1].Session != "myrig/pack.worker_b" {
		t.Errorf("RecentBySession[1] = %+v, want worker_b second", body.RecentBySession[1])
	}
	if body.RecentBySession[1].CostUSDEstimate != 0 {
		t.Errorf("RecentBySession[1].CostUSDEstimate = %v, want 0 — unpriced cost must not count as per-session spend",
			body.RecentBySession[1].CostUSDEstimate)
	}
	if len(body.Warnings) != 1 {
		t.Errorf("Warnings = %v, want exactly 1 (the malformed line)", body.Warnings)
	}
}

func TestHandleUsageNoFile(t *testing.T) {
	state := newFakeState(t)
	h := newTestCityHandler(t, state)

	req := httptest.NewRequest("GET", cityURL(state, "/usage"), nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d (body: %s)", rec.Code, http.StatusOK, rec.Body.String())
	}
	var body UsageBody
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Totals != (UsageTotals{}) || body.Today != (UsageTotals{}) {
		t.Errorf("expected zero totals without a usage file, got %+v", body)
	}
	if len(body.Warnings) != 0 {
		t.Errorf("Warnings = %v, want none", body.Warnings)
	}
}

func TestBuildUsageBodyWindowBoundaries(t *testing.T) {
	// Fixed local clock: windowing is local-midnight and trailing-5-minute
	// based, so boundary facts must land deterministically.
	now := time.Date(2026, 7, 11, 12, 0, 0, 0, time.Local)
	midnight := time.Date(2026, 7, 11, 0, 0, 0, 0, time.Local)
	facts := []usage.Fact{
		{Kind: usage.KindModel, InputTokens: 1, At: midnight.UnixMilli(), IdempotencyKey: "a"},                        // today, not recent
		{Kind: usage.KindModel, InputTokens: 2, At: midnight.Add(-time.Millisecond).UnixMilli(), IdempotencyKey: "b"}, // yesterday
		{Kind: usage.KindModel, InputTokens: 4, At: now.Add(-299 * time.Second).UnixMilli(), IdempotencyKey: "c"},     // recent
		{Kind: usage.KindModel, InputTokens: 8, At: now.Add(-301 * time.Second).UnixMilli(), IdempotencyKey: "d"},     // today, not recent
	}
	body := buildUsageBody(facts, nil, now)

	if body.Totals.InputTokens != 15 {
		t.Errorf("Totals.InputTokens = %d, want 15", body.Totals.InputTokens)
	}
	if body.Today.InputTokens != 13 {
		t.Errorf("Today.InputTokens = %d, want 13 (midnight fact counts, yesterday's does not)", body.Today.InputTokens)
	}
	if body.Recent.InputTokens != 4 {
		t.Errorf("Recent.InputTokens = %d, want 4 (only the -299s fact)", body.Recent.InputTokens)
	}
}
