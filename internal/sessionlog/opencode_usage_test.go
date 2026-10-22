package sessionlog

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

const openCodeUsageExport = `{
  "info": {"id": "ses_abc123", "directory": "/work/city"},
  "messages": [
    {"info": {"id": "msg_user1", "role": "user", "time": {"created": 1792100000000}}, "parts": []},
    {"info": {"id": "msg_a1", "role": "assistant", "modelID": "deepseek/deepseek-v4.1-flash", "providerID": "openrouter",
      "tokens": {"total": 16111, "input": 15964, "output": 88, "reasoning": 59, "cache": {"read": 10, "write": 20}},
      "cost": 0.0024828, "time": {"created": 1792100522671, "completed": 1792100545840}}, "parts": []},
    {"info": {"id": "msg_a2", "role": "assistant", "modelID": "deepseek/deepseek-v4.1-flash", "providerID": "openrouter",
      "tokens": {"input": 100, "output": 5, "reasoning": 0, "cache": {"read": 0, "write": 0}},
      "cost": 0, "time": {"created": 1792100600000, "completed": 1792100601000}}, "parts": []},
    {"info": {"id": "msg_pending", "role": "assistant", "modelID": "m", "providerID": "p",
      "tokens": {"input": 0, "output": 0, "reasoning": 0, "cache": {"read": 0, "write": 0}},
      "time": {"created": 1792100700000}}, "parts": []},
    {"info": {"id": "msg_nocost", "role": "assistant", "modelID": "m2", "providerID": "p",
      "tokens": {"input": 7, "output": 3, "cache": {"read": 0, "write": 0}},
      "time": {"created": 1792100800000, "completed": 1792100801000}}, "parts": []}
  ]
}`

func writeOpenCodeUsageExport(t *testing.T, dir, name, body string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestExtractOpenCodeTailUsage(t *testing.T) {
	path := writeOpenCodeUsageExport(t, t.TempDir(), "ses_abc123.json", openCodeUsageExport)

	got, err := ExtractOpenCodeTailUsage(path)
	if err != nil {
		t.Fatalf("ExtractOpenCodeTailUsage: %v", err)
	}
	// User turns and the not-yet-completed, zero-token assistant message are skipped.
	if len(got) != 3 {
		t.Fatalf("got %d usages, want 3: %+v", len(got), got)
	}

	first := got[0]
	if first.MessageID != "msg_a1" || first.EntryUUID != "msg_a1" {
		t.Errorf("identity = %q/%q, want msg_a1", first.MessageID, first.EntryUUID)
	}
	if first.Model != "deepseek/deepseek-v4.1-flash" {
		t.Errorf("Model = %q", first.Model)
	}
	if first.InputTokens != 15964 || first.OutputTokens != 88 || first.ReasoningTokens != 59 ||
		first.CacheReadTokens != 10 || first.CacheCreationTokens != 20 {
		t.Errorf("tokens = %+v", first)
	}
	if !first.HasReportedCost || first.ReportedCostUSD != 0.0024828 {
		t.Errorf("reported cost = %v/%v, want 0.0024828/true", first.ReportedCostUSD, first.HasReportedCost)
	}
	if want := time.UnixMilli(1792100545840); !first.Timestamp.Equal(want) {
		t.Errorf("Timestamp = %v, want %v", first.Timestamp, want)
	}

	// An explicit zero cost (a free model) is a reported cost, not a missing one.
	if !got[1].HasReportedCost || got[1].ReportedCostUSD != 0 {
		t.Errorf("zero cost = %v/%v, want 0/true", got[1].ReportedCostUSD, got[1].HasReportedCost)
	}
	// An absent cost field is not reported, so callers fall back to the price table.
	if got[2].MessageID != "msg_nocost" || got[2].HasReportedCost {
		t.Errorf("no-cost entry = %+v", got[2])
	}
}

func TestExtractOpenCodeTailUsageFromSearchPaths(t *testing.T) {
	root := t.TempDir()
	path := writeOpenCodeUsageExport(t, root, "ses_abc123.json", openCodeUsageExport)

	if _, err := ExtractOpenCodeTailUsageFromSearchPaths([]string{root}, path); err != nil {
		t.Fatalf("inside search path: %v", err)
	}
	outside := writeOpenCodeUsageExport(t, t.TempDir(), "ses_other.json", openCodeUsageExport)
	if _, err := ExtractOpenCodeTailUsageFromSearchPaths([]string{root}, outside); err == nil {
		t.Fatal("path outside the search roots was read")
	}
}

func TestExtractOpenCodeTailUsageTornFile(t *testing.T) {
	path := writeOpenCodeUsageExport(t, t.TempDir(), "ses_torn.json", openCodeUsageExport[:80])
	if _, err := ExtractOpenCodeTailUsage(path); err == nil {
		t.Fatal("a torn export parsed as complete; callers must see an error and retry")
	}
}

func TestFindOpenCodeSessionFileByID(t *testing.T) {
	root := t.TempDir()
	want := writeOpenCodeUsageExport(t, root, "ses_abc123.json", openCodeUsageExport)
	writeOpenCodeUsageExport(t, root, "ses_other.json", openCodeUsageExport)

	if got := FindOpenCodeSessionFileByID([]string{root}, "ses_abc123"); got != want {
		t.Errorf("FindOpenCodeSessionFileByID = %q, want %q", got, want)
	}
	if got := FindOpenCodeSessionFileByID([]string{root}, "ses_missing"); got != "" {
		t.Errorf("missing id resolved to %q", got)
	}
	for _, bad := range []string{"", "  ", "../ses_abc123", "a/b", `a\b`} {
		if got := FindOpenCodeSessionFileByID([]string{root}, bad); got != "" {
			t.Errorf("key %q resolved to %q", bad, got)
		}
	}
}
