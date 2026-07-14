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
	if !body.Available || !body.Recording || body.Source != UsageSourceLocalEstimate {
		t.Fatalf("availability provenance = %+v", body)
	}
	if !body.Partial {
		t.Fatal("Partial = false, want malformed input surfaced as partial")
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
