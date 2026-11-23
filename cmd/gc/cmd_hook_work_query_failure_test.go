package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// These tests drive the real `gc hook` entry point over the real generated
// default work query with `bd` broken, reproducing a live failure: a worker
// whose tool shell had lost `bd` from PATH ran `gc hook --claim`, the session
// fence logged "bd not found in PATH ... proceeding to claim", and the default
// work query swallowed the same failure into "[]". The claim answered
// {"action":"drain","reason":"no_work"}, the seat drained, and a new one was
// spawned every few minutes while the routed bead was never claimed.

// hookPathWithoutBD returns a PATH with sh, jq and coreutils but no `bd`.
func hookPathWithoutBD(t *testing.T) string {
	t.Helper()
	jq, err := exec.LookPath("jq")
	if err != nil {
		t.Skip("jq is required to run the generated work query")
	}
	toolDir := t.TempDir()
	if err := os.Symlink(jq, filepath.Join(toolDir, "jq")); err != nil {
		t.Fatalf("link jq: %v", err)
	}
	dirs := []string{toolDir}
	for _, dir := range filepath.SplitList(os.Getenv("PATH")) {
		if dir == "" {
			continue
		}
		if _, err := os.Stat(filepath.Join(dir, "bd")); err == nil {
			continue
		}
		dirs = append(dirs, dir)
	}
	return strings.Join(dirs, string(os.PathListSeparator))
}

// installFailingBD puts a `bd` first on PATH that fails every invocation the
// way an unreadable store makes the real one fail.
func installFailingBD(t *testing.T) {
	t.Helper()
	fakeBin := t.TempDir()
	script := "#!/bin/sh\nprintf 'bd: store unreadable\\n' >&2\nexit 1\n"
	if err := os.WriteFile(filepath.Join(fakeBin, "bd"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// brokenBDCases are the two ways the live failure presents.
func brokenBDCases() []struct {
	name       string
	install    func(t *testing.T)
	wantStderr string
} {
	return []struct {
		name       string
		install    func(t *testing.T)
		wantStderr string
	}{
		{
			name:       "bd missing from PATH",
			install:    func(t *testing.T) { t.Setenv("PATH", hookPathWithoutBD(t)) },
			wantStderr: "bd not found in PATH",
		},
		{
			name:       "bd exits non-zero",
			install:    installFailingBD,
			wantStderr: "bd: store unreadable",
		},
	}
}

// TestHookCommandDiscoveryReportsAFailedWorkQuery pins plain `gc hook`: a work
// query that could not read the store is an error with its own exit code, not
// the "1, empty" answer that means no work.
func TestHookCommandDiscoveryReportsAFailedWorkQuery(t *testing.T) {
	for _, tc := range brokenBDCases() {
		t.Run(tc.name, func(t *testing.T) {
			clearGCEnv(t)
			disableManagedDoltRecoveryForTest(t)
			t.Setenv("GC_BEADS", "file")
			cityDir := writeFenceTestCity(t)
			t.Setenv("GC_CITY", cityDir)
			tc.install(t)

			var stdout, stderr bytes.Buffer
			code := cmdHookWithOptions([]string{"worker"}, hookCommandOptions{}, &stdout, &stderr)

			if code != hookExitWorkQueryFailed {
				t.Fatalf("gc hook over a broken bd = exit %d, want %d (work query failed); a failed query must not share the no-work exit code; stdout=%q stderr=%s", code, hookExitWorkQueryFailed, stdout.String(), stderr.String())
			}
			if strings.Contains(stdout.String(), "[]") {
				t.Errorf("gc hook printed %q over a broken bd; \"[]\" reads as an empty queue", stdout.String())
			}
			if !strings.Contains(stderr.String(), tc.wantStderr) {
				t.Errorf("gc hook stderr = %q, want the failure named (%q)", stderr.String(), tc.wantStderr)
			}
		})
	}
}

// TestHookCommandClaimDoesNotDrainOnAFailedWorkQuery is the live failure end to
// end: the fence cannot verify the session and proceeds, the work query cannot
// read the store, and the claim must report the failure instead of answering
// drain/no_work — which a startup protocol obeys by exiting the seat.
func TestHookCommandClaimDoesNotDrainOnAFailedWorkQuery(t *testing.T) {
	for _, tc := range brokenBDCases() {
		t.Run(tc.name, func(t *testing.T) {
			clearGCEnv(t)
			disableManagedDoltRecoveryForTest(t)
			t.Setenv("GC_BEADS", "file")
			cityDir := writeFenceTestCity(t)
			// An unreadable session store makes the fence fail open, which is the
			// path the live worker took ("session fence unavailable ... proceeding
			// to claim").
			if err := os.WriteFile(filepath.Join(cityDir, ".gc", "beads.json"), []byte("{ this is not valid json"), 0o644); err != nil {
				t.Fatal(err)
			}
			setFenceClaimEnv(t, cityDir, "worker-1", "any-token")
			tc.install(t)
			// A failed read is retried before it is reported; drive those
			// retries without pacing so the case does not wait them out.
			prevInterval := hookClaimQueryRetryInterval
			hookClaimQueryRetryInterval = 0
			t.Cleanup(func() { hookClaimQueryRetryInterval = prevInterval })

			var stdout, stderr bytes.Buffer
			code := cmdHookWithOptions(nil, hookCommandOptions{Claim: true, DrainAck: true, JSON: true}, &stdout, &stderr)

			if !strings.Contains(stderr.String(), "proceeding to claim") {
				t.Fatalf("the claim never reached the work query (stderr=%s)", stderr.String())
			}
			if code == 0 {
				t.Fatalf("gc hook --claim over a broken bd exited 0; stdout=%q stderr=%s", stdout.String(), stderr.String())
			}
			if strings.Contains(stdout.String(), `"drain"`) || strings.Contains(stdout.String(), hookClaimReasonNoWork) {
				t.Fatalf("gc hook --claim answered %q over a broken bd; a failed read must not be a drain verdict", stdout.String())
			}
			if !strings.Contains(stderr.String(), tc.wantStderr) {
				t.Errorf("gc hook --claim stderr = %q, want the failure named (%q)", stderr.String(), tc.wantStderr)
			}
		})
	}
}
