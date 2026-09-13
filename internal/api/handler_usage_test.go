package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/usage"
)

func usageLine(t *testing.T, fact usage.Fact) string {
	t.Helper()
	b, err := json.Marshal(fact)
	if err != nil {
		t.Fatalf("marshal usage fact: %v", err)
	}
	return string(b)
}

func writeUsageLog(t *testing.T, cityPath, data string) {
	t.Helper()
	dir := filepath.Join(cityPath, ".gc")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "usage.jsonl"), []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestBuildUsageBodyPreservesWindowAndPricingProvenance(t *testing.T) {
	now := time.Date(2026, 7, 14, 12, 0, 0, 0, time.FixedZone("test", -7*60*60))
	midnight := time.Date(2026, 7, 14, 0, 0, 0, 0, now.Location())
	facts := []usage.Fact{
		{Kind: usage.KindModel, Worker: "rig/worker-a", SessionID: "s-a", InputTokens: 10, OutputTokens: 2, CostUSDEstimate: 0.25, At: midnight.UnixMilli(), IdempotencyKey: "midnight"},
		{Kind: usage.KindModel, Worker: "rig/worker-a", SessionID: "s-a", InputTokens: 20, OutputTokens: 3, Unpriced: true, At: now.Add(-time.Minute).UnixMilli(), IdempotencyKey: "recent"},
		{Kind: usage.KindModel, InputTokens: 99, At: midnight.Add(-time.Millisecond).UnixMilli(), IdempotencyKey: "yesterday"},
	}

	body := buildUsageBody(facts, usage.RecentReadReport{Truncated: true, RecordLimited: true, Malformed: 2}, now)
	if body.Today.InputTokens != 30 || body.Recent.InputTokens != 20 {
		t.Fatalf("today/recent input = %d/%d, want 30/20", body.Today.InputTokens, body.Recent.InputTokens)
	}
	// The pre-midnight "yesterday" fact is outside today but still inside the
	// trailing 24h window (now is noon), so last_24h is a strict superset of today.
	if body.Last24H == nil {
		t.Fatal("last_24h aggregate is nil; a live reading must always populate it")
	}
	if body.Last24H.InputTokens != 129 {
		t.Fatalf("last_24h input = %d, want 129 (today 30 + pre-midnight 99)", body.Last24H.InputTokens)
	}
	if body.Today.Unpriced != 1 || body.Today.CostUSDEstimate != 0.25 {
		t.Fatalf("pricing provenance = %+v", body.Today)
	}
	if len(body.RecentBySession) != 1 || body.RecentBySession[0].Unpriced != 1 {
		t.Fatalf("recent sessions = %+v", body.RecentBySession)
	}
	if !body.Partial || len(body.PartialReasons) != 3 {
		t.Fatalf("partial provenance = %+v", body)
	}
	for _, reason := range body.PartialReasons {
		if strings.Contains(reason, string(filepath.Separator)) {
			t.Fatalf("partial reason leaks a filesystem path: %q", reason)
		}
	}
}

func TestBuildUsageBodyLast24HIsTodaySupersetIncludingPreMidnight(t *testing.T) {
	now := time.Date(2026, 7, 14, 12, 0, 0, 0, time.FixedZone("test", -7*60*60))
	midnight := time.Date(2026, 7, 14, 0, 0, 0, 0, now.Location())
	facts := []usage.Fact{
		// Inside the trailing 24h but before local midnight: last_24h only, never today.
		{Kind: usage.KindModel, InputTokens: 100, OutputTokens: 40, CostUSDEstimate: 1.50, At: midnight.Add(-2 * time.Hour).UnixMilli(), IdempotencyKey: "pre-midnight-priced"},
		// Pre-midnight, inside 24h, unpriced: token volume + unpriced provenance, no cost.
		{Kind: usage.KindModel, InputTokens: 5, Unpriced: true, At: midnight.Add(-3 * time.Hour).UnixMilli(), IdempotencyKey: "pre-midnight-unpriced"},
		// After midnight (today): counted in both windows.
		{Kind: usage.KindModel, InputTokens: 10, OutputTokens: 2, CostUSDEstimate: 0.25, At: now.Add(-time.Minute).UnixMilli(), IdempotencyKey: "today"},
		// Older than 24h: outside every window (valid, just stale).
		{Kind: usage.KindModel, InputTokens: 9999, OutputTokens: 9999, CostUSDEstimate: 9.99, At: now.Add(-25 * time.Hour).UnixMilli(), IdempotencyKey: "older-than-24h"},
	}

	body := buildUsageBody(facts, usage.RecentReadReport{}, now)

	// today sees only the post-midnight fact — this is the amnesiac surface the bug is about.
	if body.Today.InputTokens != 10 || body.Today.OutputTokens != 2 || body.Today.Invocations != 1 {
		t.Fatalf("today = %+v, want only the post-midnight fact (in=10 out=2 calls=1)", body.Today)
	}
	if body.Today.Unpriced != 0 || body.Today.CostUSDEstimate != 0.25 {
		t.Fatalf("today pricing = cost %v unpriced %d, want 0.25/0", body.Today.CostUSDEstimate, body.Today.Unpriced)
	}
	// A live reading always populates the pointer; only pre-field servers omit it.
	if body.Last24H == nil {
		t.Fatal("last_24h aggregate is nil; a live reading must always populate it")
	}
	last24h := *body.Last24H
	// last_24h is a strict superset of today: both pre-midnight facts plus today,
	// dropping only the 25h-old fact.
	if last24h.InputTokens != 115 || last24h.OutputTokens != 42 {
		t.Fatalf("last_24h tokens = in %d/out %d, want 115/42", last24h.InputTokens, last24h.OutputTokens)
	}
	if last24h.Invocations != 3 {
		t.Fatalf("last_24h invocations = %d, want 3", last24h.Invocations)
	}
	if last24h.Unpriced != 1 {
		t.Fatalf("last_24h unpriced = %d, want 1 (the pre-midnight unpriced fact)", last24h.Unpriced)
	}
	if last24h.CostUSDEstimate != 1.75 {
		t.Fatalf("last_24h cost = %v, want 1.75 (1.50 pre-midnight + 0.25 today; unpriced adds none)", last24h.CostUSDEstimate)
	}
	// The rate window is unchanged: only the fact inside the 5-minute recent window.
	if body.Recent.InputTokens != 10 || body.Recent.Invocations != 1 {
		t.Fatalf("recent = %+v, want only the fact inside the 5m window", body.Recent)
	}
}

func TestBuildUsageBodySkipsInvalidFactsAndKeepsSessionIDsDistinct(t *testing.T) {
	now := time.Date(2026, 7, 14, 12, 0, 0, 0, time.UTC)
	facts := []usage.Fact{
		{Kind: usage.KindModel, Worker: "same-worker", SessionID: "s-1", InputTokens: 10, At: now.UnixMilli(), IdempotencyKey: "one"},
		{Kind: usage.KindModel, Worker: "same-worker", SessionID: "s-2", InputTokens: 20, At: now.UnixMilli(), IdempotencyKey: "two"},
		{Kind: usage.Kind("unknown"), InputTokens: 30, At: now.UnixMilli(), IdempotencyKey: "bad-kind"},
		{Kind: usage.KindModel, InputTokens: -1, At: now.UnixMilli(), IdempotencyKey: "negative"},
		{Kind: usage.KindModel, InputTokens: 40, At: now.Add(2 * time.Minute).UnixMilli(), IdempotencyKey: "future"},
	}
	body := buildUsageBody(facts, usage.RecentReadReport{}, now)
	if body.Today.InputTokens != 30 || body.Recent.InputTokens != 30 {
		t.Fatalf("valid token total = %d/%d, want 30/30", body.Today.InputTokens, body.Recent.InputTokens)
	}
	if len(body.RecentBySession) != 2 {
		t.Fatalf("recent_by_session = %+v, want two IDs sharing one worker", body.RecentBySession)
	}
	if !body.Partial || len(body.PartialReasons) != 1 || !strings.Contains(body.PartialReasons[0], "3 invalid") {
		t.Fatalf("invalid provenance = %+v", body)
	}
}

func TestHandleUsageIsRegisteredAndReturnsSanitizedAggregate(t *testing.T) {
	state := newFakeState(t)
	state.usageSink = usage.NewLocalSink(filepath.Join(state.cityPath, ".gc", "usage.jsonl"))
	now := time.Now()
	writeUsageLog(t, state.cityPath,
		"{malformed\n"+usageLine(t, usage.Fact{
			Kind: usage.KindModel, Worker: "rig/worker", SessionID: "session-1",
			InputTokens: 100, OutputTokens: 25, CostUSDEstimate: 0.10,
			At: now.UnixMilli(), IdempotencyKey: "fact-1",
		})+"\n")

	h := newTestCityHandler(t, state)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, cityURL(state, "/usage"), nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), state.cityPath) {
		t.Fatalf("response leaks city path: %s", rec.Body.String())
	}
	var body UsageBody
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body.Today.InputTokens != 100 || body.Recent.InputTokens != 100 {
		t.Fatalf("body = %+v", body)
	}
	if body.Last24H == nil || body.Last24H.InputTokens != 100 {
		t.Fatalf("last_24h did not survive the HTTP projection: %+v", body.Last24H)
	}
	if len(body.RecentBySession) != 1 || body.RecentBySession[0].SessionID != "session-1" {
		t.Fatalf("default usage response lost its session breakdown: %+v", body.RecentBySession)
	}
	if !body.Available || !body.Recording || body.Source != UsageSourceLocalEstimate {
		t.Fatalf("availability provenance = %+v", body)
	}
	if !body.Partial {
		t.Fatal("Partial = false, want malformed input surfaced as partial")
	}
}

func TestHandleUsageAggregateOnlyOmitsSessionBreakdown(t *testing.T) {
	state := newFakeState(t)
	state.usageSink = usage.NewLocalSink(filepath.Join(state.cityPath, ".gc", "usage.jsonl"))
	writeUsageLog(t, state.cityPath, usageLine(t, usage.Fact{
		Kind: usage.KindModel, Worker: "private-worker", SessionID: "private-session",
		InputTokens: 100, At: time.Now().UnixMilli(), IdempotencyKey: "fact-1",
	})+"\n")
	h := newTestCityHandler(t, state)

	rec := httptest.NewRecorder()
	request := httptest.NewRequest(
		http.MethodGet,
		cityURL(state, "/usage")+"?aggregate_only=true",
		nil,
	)
	h.ServeHTTP(rec, request)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "private-worker") || strings.Contains(rec.Body.String(), "private-session") {
		t.Fatalf("aggregate response leaked per-session identity: %s", rec.Body.String())
	}
	var body UsageBody
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body.Today.InputTokens != 100 || body.Recent.InputTokens != 100 {
		t.Fatalf("aggregate totals = %+v, want input_tokens=100", body)
	}
	if len(body.RecentBySession) != 0 {
		t.Fatalf("recent_by_session = %+v, want empty", body.RecentBySession)
	}
}

func TestHandleUsageMissingLogIsAnAvailableEmptyReading(t *testing.T) {
	state := newFakeState(t)
	state.usageSink = usage.NewLocalSink(filepath.Join(state.cityPath, ".gc", "usage.jsonl"))
	h := newTestCityHandler(t, state)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, cityURL(state, "/usage"), nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var body UsageBody
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body.Today != (UsageTotals{}) || body.Recent != (UsageTotals{}) || body.Partial || !body.Available {
		t.Fatalf("empty body = %+v", body)
	}
}

func TestHandleUsageDoesNotServeAStaleLocalFileForANonLocalSink(t *testing.T) {
	state := newFakeState(t) // default is usage.Discard
	writeUsageLog(t, state.cityPath, usageLine(t, usage.Fact{
		Kind: usage.KindModel, InputTokens: 999, At: time.Now().UnixMilli(), IdempotencyKey: "stale",
	})+"\n")
	h := newTestCityHandler(t, state)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, cityURL(state, "/usage"), nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var body UsageBody
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body.Available || body.Recording || body.Source != UsageSourceUnavailable {
		t.Fatalf("availability provenance = %+v", body)
	}
	if body.Today.InputTokens != 0 || body.Recent.InputTokens != 0 {
		t.Fatalf("non-local sink served stale local usage: %+v", body)
	}
}
