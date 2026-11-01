package main

import (
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestTraceAppendBatchFailedWriteDoesNotPoisonSegment reproduces issue #7954:
// a batch write that fails after a partial write must not leave the segment
// unreadable, and the next batch must not reuse the failed batch's seqs.
func TestTraceAppendBatchFailedWriteDoesNotPoisonSegment(t *testing.T) {
	cityDir := t.TempDir()
	store, err := newSessionReconcilerTraceStore(cityDir, io.Discard)
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	defer store.Close() //nolint:errcheck

	rec := func(tick string) SessionReconcilerTraceRecord {
		return SessionReconcilerTraceRecord{
			TraceSchemaVersion: sessionReconcilerTraceSchemaVersion,
			TraceID:            "cycle-" + tick,
			TickID:             tick,
			RecordType:         TraceRecordDecision,
			Template:           "repo/worker",
			SessionName:        "worker-1",
			SiteCode:           TraceSiteReconcilerWakeDecision,
			TraceMode:          TraceModeBaseline,
			TraceSource:        TraceSourceAlwaysOn,
			Ts:                 time.Now().UTC(),
		}
	}
	if err := store.AppendBatch([]SessionReconcilerTraceRecord{rec("tick-1")}, TraceDurabilityDurable); err != nil {
		t.Fatalf("append batch 1: %v", err)
	}
	segPath := store.currentPath
	writable := store.currentFile

	// Simulate ENOSPC mid-write: the write returns an error after some bytes
	// reached the segment. A read-only handle makes Write fail; the torn
	// prefix is what the short write left behind.
	ro, err := os.Open(segPath)
	if err != nil {
		t.Fatalf("open read-only: %v", err)
	}
	store.currentFile = ro
	if err := store.AppendBatch([]SessionReconcilerTraceRecord{rec("tick-2")}, TraceDurabilityDurable); err == nil {
		t.Fatalf("append batch 2: want write error, got nil")
	}
	ro.Close() //nolint:errcheck
	torn, err := os.OpenFile(segPath, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatalf("open segment for torn bytes: %v", err)
	}
	if _, err := torn.WriteString(`{"trace_schema_version":1,"seq":3,"tick_id":"tick-2","ts":"2026-1`); err != nil {
		t.Fatalf("write torn bytes: %v", err)
	}
	torn.Close() //nolint:errcheck

	// Space comes back; the controller keeps ticking on the same handle.
	store.currentFile = writable
	if err := store.AppendBatch([]SessionReconcilerTraceRecord{rec("tick-3")}, TraceDurabilityDurable); err != nil {
		t.Fatalf("append batch 3: %v", err)
	}

	records, err := ReadTraceRecords(filepath.Join(cityDir, ".gc", "runtime", sessionReconcilerTraceRootDir), TraceFilter{})
	if err != nil {
		t.Fatalf("ReadTraceRecords after failed batch write: %v", err)
	}
	seen := map[uint64]string{}
	for _, r := range records {
		if prev, ok := seen[r.Seq]; ok {
			t.Fatalf("seq %d reused: tick %s and tick %s", r.Seq, prev, r.TickID)
		}
		seen[r.Seq] = r.TickID
	}
	var ticks []string
	for _, r := range records {
		if r.RecordType == TraceRecordDecision {
			ticks = append(ticks, r.TickID)
		}
	}
	if len(ticks) != 2 || ticks[0] != "tick-1" || ticks[1] != "tick-3" {
		t.Fatalf("committed decision ticks = %v, want [tick-1 tick-3]", ticks)
	}
}
