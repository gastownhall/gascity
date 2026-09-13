package beads

import (
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"
)

func TestTraceBDCall_WritesJSONL(t *testing.T) {
	tmp := t.TempDir()
	tracePath := tmp + "/trace.jsonl"
	t.Setenv("GC_BD_TRACE_JSON", tracePath)

	start := time.Now().Add(-12 * time.Millisecond)
	TraceBDCall("go:test.success", "/work", []string{"list", "--json"}, start, 0, nil)
	TraceBDCall("go:test.failure", "/other", []string{"show", "sc-xyz", "--json"}, start, 1, errors.New("boom"))

	data, err := os.ReadFile(tracePath)
	if err != nil {
		t.Fatalf("read trace: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 2 {
		t.Fatalf("got %d lines, want 2: %s", len(lines), string(data))
	}
	for _, line := range lines {
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("invalid JSON: %v\nline: %s", err, line)
		}
		for _, k := range []string{"ts", "source", "scope", "args", "dur_ms", "exit_code", "pid", "ppid"} {
			if _, ok := rec[k]; !ok {
				t.Errorf("missing key %q in record: %s", k, line)
			}
		}
	}

	// Verify env-scope is captured when set.
	t.Setenv("GC_BD_TRACE_SCOPE", "hook:bead.closed")
	tracePath2 := tmp + "/trace2.jsonl"
	t.Setenv("GC_BD_TRACE_JSON", tracePath2)
	TraceBDCall("go:test.envscope", "/work", []string{"list"}, start, 0, nil)
	envData, err := os.ReadFile(tracePath2)
	if err != nil {
		t.Fatalf("read env trace: %v", err)
	}
	var envRec map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(string(envData))), &envRec); err != nil {
		t.Fatalf("env trace JSON parse: %v", err)
	}
	if envRec["env_scope"] != "hook:bead.closed" {
		t.Errorf("env_scope = %v, want hook:bead.closed", envRec["env_scope"])
	}

	// Verify tick-trigger plumbing.
	t.Setenv("GC_BD_TRACE_SCOPE", "")
	tracePath3 := tmp + "/trace3.jsonl"
	t.Setenv("GC_BD_TRACE_JSON", tracePath3)
	prev := SetReconcilerTickTrigger("patrol")
	TraceBDCall("go:test.tick", "/work", []string{"list"}, start, 0, nil)
	RestoreReconcilerTickTrigger(prev)
	tickData, err := os.ReadFile(tracePath3)
	if err != nil {
		t.Fatalf("read tick trace: %v", err)
	}
	var tickRec map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(string(tickData))), &tickRec); err != nil {
		t.Fatalf("tick trace JSON parse: %v", err)
	}
	if tickRec["tick_trigger"] != "patrol" {
		t.Errorf("tick_trigger = %v, want patrol", tickRec["tick_trigger"])
	}

	// Verify GC_BD_TRACE_JSON=unset is a no-op
	t.Setenv("GC_BD_TRACE_JSON", "")
	if err := os.Remove(tracePath); err != nil {
		t.Fatalf("rm: %v", err)
	}
	TraceBDCall("go:test.noop", "/work", []string{"list"}, start, 0, nil)
	if _, err := os.Stat(tracePath); !os.IsNotExist(err) {
		t.Errorf("expected no file when GC_BD_TRACE_JSON unset, got: %v", err)
	}
}
