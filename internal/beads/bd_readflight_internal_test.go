//go:build !windows

package beads

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// writeCountingFakeBD puts a fake `bd` on PATH that appends its argument
// vector to countFile, sleeps briefly so concurrent identical calls overlap
// inside one flight window, and prints '[]'. It returns the count file.
// Legacy OTEL env vars are blanked so translateBdLegacyOTELEnv cannot trigger
// a `bd version` capability probe that would add spurious count lines.
func writeCountingFakeBD(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	countFile := filepath.Join(dir, "count")
	binDir := t.TempDir()
	writeExecutable(t, filepath.Join(binDir, "bd"), fmt.Sprintf(`#!/bin/sh
printf '%%s\n' "$*" >> %q
sleep 0.5
printf '[]\n'
`, countFile))
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("BD_OTEL_METRICS_URL", "")
	t.Setenv("BD_OTEL_LOGS_URL", "")
	return countFile
}

// countBDSpawns returns how many times the fake bd ran, optionally filtered
// to argument vectors containing substr.
func countBDSpawns(t *testing.T, countFile string, substr ...string) int {
	t.Helper()
	data, err := os.ReadFile(countFile)
	if err != nil {
		if os.IsNotExist(err) {
			return 0
		}
		t.Fatalf("read count file: %v", err)
	}
	count := 0
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if line == "" {
			continue
		}
		match := true
		for _, s := range substr {
			if !strings.Contains(line, s) {
				match = false
				break
			}
		}
		if match {
			count++
		}
	}
	return count
}

// TestBDReadFlightDeduplicatesConcurrentReads is the workflow-rx4 regression:
// N goroutines issuing the identical `bd list` read must spawn ONE bd
// subprocess and all receive identical stdout. A different-args read issued in
// the same window must NOT share the flight.
func TestBDReadFlightDeduplicatesConcurrentReads(t *testing.T) {
	countFile := writeCountingFakeBD(t)
	runner := ExecCommandRunner()
	dir := t.TempDir()
	readArgs := []string{"list", "--json", "--assignee=alice", "--status=open"}

	const callers = 8
	var wg sync.WaitGroup
	start := make(chan struct{})
	outs := make([][]byte, callers)
	errs := make([]error, callers)
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			outs[i], errs[i] = runner(dir, "bd", readArgs...)
		}(i)
	}
	// One distinct read in the same window: it must lead its own flight.
	var otherErr error
	wg.Add(1)
	go func() {
		defer wg.Done()
		<-start
		_, otherErr = runner(dir, "bd", "list", "--json", "--assignee=bob", "--status=open")
	}()
	close(start)
	wg.Wait()

	for i := range outs {
		if errs[i] != nil {
			t.Fatalf("caller %d: %v", i, errs[i])
		}
		if string(outs[i]) != "[]\n" {
			t.Fatalf("caller %d stdout = %q, want shared []", i, outs[i])
		}
	}
	if otherErr != nil {
		t.Fatalf("distinct-args read: %v", otherErr)
	}
	if got := countBDSpawns(t, countFile, "--assignee=alice"); got != 1 {
		t.Fatalf("alice-args spawns = %d, want 1 (single flight)", got)
	}
	if got := countBDSpawns(t, countFile, "--assignee=bob"); got != 1 {
		t.Fatalf("bob-args spawns = %d, want 1", got)
	}
	if got := countBDSpawns(t, countFile); got != 2 {
		t.Fatalf("total spawns = %d, want 2", got)
	}
}

// TestBDReadFlightDoesNotCache proves single-flight semantics: a call arriving
// after the flight completes spawns fresh, because bd state mutates.
func TestBDReadFlightDoesNotCache(t *testing.T) {
	countFile := writeCountingFakeBD(t)
	runner := ExecCommandRunner()
	dir := t.TempDir()
	args := []string{"list", "--json", "--assignee=alice", "--status=open"}

	for i := 0; i < 2; i++ {
		out, err := runner(dir, "bd", args...)
		if err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
		if string(out) != "[]\n" {
			t.Fatalf("call %d stdout = %q", i, out)
		}
	}
	if got := countBDSpawns(t, countFile); got != 2 {
		t.Fatalf("spawns = %d, want 2 (no caching)", got)
	}
}

// TestBDReadFlightNeverDeduplicatesWrites: mutations must execute, so a write
// verb bypasses the flight entirely even when issued concurrently with
// identical arguments.
func TestBDReadFlightNeverDeduplicatesWrites(t *testing.T) {
	countFile := writeCountingFakeBD(t)
	runner := ExecCommandRunner()
	dir := t.TempDir()

	const callers = 4
	var wg sync.WaitGroup
	start := make(chan struct{})
	errs := make([]error, callers)
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			_, errs[i] = runner(dir, "bd", "update", "gc-1", "--status", "open")
		}(i)
	}
	close(start)
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("writer %d: %v", i, err)
		}
	}
	if got := countBDSpawns(t, countFile, "update"); got != callers {
		t.Fatalf("update spawns = %d, want %d (writes never share)", got, callers)
	}
}

// TestBDReadFlightKeyIncludesEnv: two runners whose effective child
// environments differ must not share a flight — bd can read different state
// under different BEADS_*/BD_* configuration.
func TestBDReadFlightKeyIncludesEnv(t *testing.T) {
	countFile := writeCountingFakeBD(t)
	dir := t.TempDir()
	args := []string{"list", "--json", "--status=open"}

	runnerA := ExecCommandRunnerWithEnv(map[string]string{"GC_ALIAS": "agent-a"})
	runnerB := ExecCommandRunnerWithEnv(map[string]string{"GC_ALIAS": "agent-b"})

	var wg sync.WaitGroup
	start := make(chan struct{})
	var errA, errB error
	wg.Add(2)
	go func() {
		defer wg.Done()
		<-start
		_, errA = runnerA(dir, "bd", args...)
	}()
	go func() {
		defer wg.Done()
		<-start
		_, errB = runnerB(dir, "bd", args...)
	}()
	close(start)
	wg.Wait()

	if errA != nil || errB != nil {
		t.Fatalf("reads failed: A=%v B=%v", errA, errB)
	}
	if got := countBDSpawns(t, countFile); got != 2 {
		t.Fatalf("spawns = %d, want 2 (different env must not share a flight)", got)
	}
}

// TestBDReadFlightWaiterDetachesOnDeadline: a caller whose own context ends
// while the shared flight is still running returns promptly with a timeout
// instead of waiting for the leader; the leader still completes for the
// remaining callers and only one subprocess ran.
func TestBDReadFlightWaiterDetachesOnDeadline(t *testing.T) {
	countFile := writeCountingFakeBD(t)
	dir := t.TempDir()
	args := []string{"list", "--json", "--assignee=alice", "--status=open"}
	env := map[string]string{"GC_ALIAS": "detach"}

	leaderCtx, leaderCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer leaderCancel()
	waiterCtx, waiterCancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer waiterCancel()

	leader := ExecCommandRunnerWithEnvContext(leaderCtx, env)
	waiter := ExecCommandRunnerWithEnvContext(waiterCtx, env)

	leaderOut := make(chan []byte, 1)
	leaderErr := make(chan error, 1)
	go func() {
		out, err := leader(dir, "bd", args...)
		leaderOut <- out
		leaderErr <- err
	}()

	// Let the leader establish the flight before the waiter joins.
	time.Sleep(50 * time.Millisecond)
	start := time.Now()
	_, waiterErr := waiter(dir, "bd", args...)
	waiterElapsed := time.Since(start)

	if waiterErr == nil {
		t.Fatal("waiter unexpectedly shared the result after its deadline")
	}
	if waiterElapsed > 400*time.Millisecond {
		t.Fatalf("waiter blocked %s past its 100ms deadline", waiterElapsed)
	}

	select {
	case out := <-leaderOut:
		if err := <-leaderErr; err != nil {
			t.Fatalf("leader: %v", err)
		}
		if string(out) != "[]\n" {
			t.Fatalf("leader stdout = %q", out)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("leader did not finish")
	}
	if got := countBDSpawns(t, countFile); got != 1 {
		t.Fatalf("spawns = %d, want 1 (waiter detach must not spawn)", got)
	}
}
