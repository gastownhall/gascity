package main

import (
	"fmt"
	"os"
	"strings"
)

// Workflow trace rotation tunables. The control dispatcher's --serve loop
// traces every sweep, so without a cap the trace file grows unbounded
// (823MB observed in production, ga-361vjg). This mirrors the size-gated
// rotation on events.FileRecorder — a write that finds the active file at
// or over the cap rotates it aside first — but uses plain numbered copies
// because the trace is free-form text, not a seq-stamped JSONL event log.
// Vars (not consts) so tests can lower the threshold.
var (
	// workflowTraceMaxBytes is the size threshold above which the next
	// trace write rotates the active file. Non-positive disables rotation.
	workflowTraceMaxBytes int64 = 200 * 1024 * 1024 // 200 MiB

	// workflowTraceKeepRotated is how many rotated copies to keep
	// (<path>.1 .. <path>.N); the oldest is dropped on rotation.
	workflowTraceKeepRotated = 2
)

// maybeRotateWorkflowTrace applies size-gated rotation to the workflow
// trace log before a write. Rotation is best-effort: failures are warned
// once per path via the workflow trace warning sink and never block the
// trace write itself. Concurrent writers may race the rename cascade; the
// worst case is a rotated copy aging out one slot early, which is
// acceptable for a diagnostic trace.
func maybeRotateWorkflowTrace(path string) {
	if workflowTraceMaxBytes <= 0 {
		return
	}
	info, err := os.Stat(path)
	if err != nil || info.IsDir() || info.Size() < workflowTraceMaxBytes {
		return
	}
	if err := rotateWorkflowTrace(path, workflowTraceKeepRotated); err != nil {
		workflowTraceWarnRotateFailure(path, err)
	}
}

// rotateWorkflowTrace shifts numbered rotations up one slot
// (<path>.N-1 → <path>.N, ..., <path>.1 → <path>.2) and then moves the
// active file to <path>.1, dropping whatever was in the last slot. The
// next trace write recreates the active file.
func rotateWorkflowTrace(path string, keep int) error {
	if keep < 1 {
		keep = 1
	}
	for i := keep - 1; i >= 1; i-- {
		src := fmt.Sprintf("%s.%d", path, i)
		dst := fmt.Sprintf("%s.%d", path, i+1)
		if _, err := os.Stat(src); err != nil {
			continue
		}
		if err := os.Rename(src, dst); err != nil {
			return fmt.Errorf("shifting rotated workflow trace %q to %q: %w", src, dst, err)
		}
	}
	if err := os.Rename(path, path+".1"); err != nil {
		return fmt.Errorf("rotating active workflow trace %q: %w", path, err)
	}
	return nil
}

// workflowTraceWarnRotateFailure surfaces a failed trace rotation on the
// active warning sink, deduped per path so a persistently failing rotation
// warns once per command invocation instead of once per trace line.
func workflowTraceWarnRotateFailure(path string, err error) {
	if strings.TrimSpace(path) == "" || err == nil {
		return
	}
	workflowTraceWarnings.mu.Lock()
	writer := workflowTraceWarnings.writer
	workflowTraceWarnings.mu.Unlock()
	workflowTraceWarnf(writer, "trace-rotate:"+normalizePathForCompare(path), "gc convoy control --serve: warning: rotating workflow trace %q: %v\n", path, err)
}
