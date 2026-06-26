package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// setWorkflowTraceCap lowers the size-gated rotation threshold for the
// duration of a test and restores the default afterwards.
func setWorkflowTraceCap(t *testing.T, maxBytes int64) {
	t.Helper()
	prev := workflowTraceMaxBytes
	workflowTraceMaxBytes = maxBytes
	t.Cleanup(func() { workflowTraceMaxBytes = prev })
}

func TestWorkflowTracefRotatesWhenTraceExceedsCap(t *testing.T) {
	tracePath := filepath.Join(t.TempDir(), "control-dispatcher-trace.log")
	t.Setenv("GC_WORKFLOW_TRACE", tracePath)
	setWorkflowTraceCap(t, 1)

	workflowTracef("first generation line")
	// The active file now exceeds the 1-byte cap, so the next write must
	// rotate it aside before appending.
	workflowTracef("second generation line")

	rotated, err := os.ReadFile(tracePath + ".1")
	if err != nil {
		t.Fatalf("read rotated trace: %v", err)
	}
	if !strings.Contains(string(rotated), "first generation line") {
		t.Fatalf("rotated trace = %q, want first generation line", rotated)
	}
	active, err := os.ReadFile(tracePath)
	if err != nil {
		t.Fatalf("read active trace: %v", err)
	}
	if strings.Contains(string(active), "first generation line") {
		t.Fatalf("active trace = %q, want first generation rotated out", active)
	}
	if !strings.Contains(string(active), "second generation line") {
		t.Fatalf("active trace = %q, want second generation line", active)
	}
}

func TestWorkflowTracefDoesNotRotateBelowCap(t *testing.T) {
	tracePath := filepath.Join(t.TempDir(), "control-dispatcher-trace.log")
	t.Setenv("GC_WORKFLOW_TRACE", tracePath)
	setWorkflowTraceCap(t, 1<<20)

	workflowTracef("line one")
	workflowTracef("line two")

	if _, err := os.Stat(tracePath + ".1"); !os.IsNotExist(err) {
		t.Fatalf("stat rotated trace = %v, want not-exist below cap", err)
	}
	active, err := os.ReadFile(tracePath)
	if err != nil {
		t.Fatalf("read active trace: %v", err)
	}
	if !strings.Contains(string(active), "line one") || !strings.Contains(string(active), "line two") {
		t.Fatalf("active trace = %q, want both lines retained", active)
	}
}

func TestWorkflowTracefKeepsBoundedRotations(t *testing.T) {
	tracePath := filepath.Join(t.TempDir(), "control-dispatcher-trace.log")
	t.Setenv("GC_WORKFLOW_TRACE", tracePath)
	setWorkflowTraceCap(t, 1)

	workflowTracef("generation one")
	workflowTracef("generation two")
	workflowTracef("generation three")
	// Fourth write rotates a third time; generation one falls off the end.
	workflowTracef("generation four")

	one, err := os.ReadFile(tracePath + ".1")
	if err != nil {
		t.Fatalf("read trace .1: %v", err)
	}
	if !strings.Contains(string(one), "generation three") {
		t.Fatalf("trace .1 = %q, want generation three", one)
	}
	two, err := os.ReadFile(tracePath + ".2")
	if err != nil {
		t.Fatalf("read trace .2: %v", err)
	}
	if !strings.Contains(string(two), "generation two") {
		t.Fatalf("trace .2 = %q, want generation two", two)
	}
	if _, err := os.Stat(tracePath + ".3"); !os.IsNotExist(err) {
		t.Fatalf("stat trace .3 = %v, want not-exist (keep %d rotations)", err, workflowTraceKeepRotated)
	}
	active, err := os.ReadFile(tracePath)
	if err != nil {
		t.Fatalf("read active trace: %v", err)
	}
	if !strings.Contains(string(active), "generation four") {
		t.Fatalf("active trace = %q, want generation four", active)
	}
}

func TestWorkflowTracefWarnsOnceWhenRotationFails(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("directory permissions do not block root")
	}
	dir := t.TempDir()
	tracePath := filepath.Join(dir, "control-dispatcher-trace.log")
	t.Setenv("GC_WORKFLOW_TRACE", tracePath)
	setWorkflowTraceCap(t, 1)

	workflowTracef("first generation line")

	// Make the directory read-only so the rotation rename fails while
	// appends to the existing file still succeed.
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatalf("chmod trace dir: %v", err)
	}
	t.Cleanup(func() {
		if err := os.Chmod(dir, 0o755); err != nil {
			t.Errorf("restore trace dir perms: %v", err)
		}
	})

	var stderr bytes.Buffer
	restoreWarnings := useWorkflowTraceWarnings(&stderr)
	defer restoreWarnings()

	workflowTracef("second generation line")
	workflowTracef("third generation line")

	got := stderr.String()
	if count := strings.Count(got, "rotating workflow trace"); count != 1 {
		t.Fatalf("rotation warning count = %d, want 1; stderr=%q", count, got)
	}
	if !strings.Contains(got, tracePath) {
		t.Fatalf("stderr = %q, want trace path %q", got, tracePath)
	}
	// Writes must keep flowing into the un-rotated active file.
	active, err := os.ReadFile(tracePath)
	if err != nil {
		t.Fatalf("read active trace: %v", err)
	}
	if !strings.Contains(string(active), "third generation line") {
		t.Fatalf("active trace = %q, want appends to continue after failed rotation", active)
	}
}
