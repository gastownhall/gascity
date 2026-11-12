//go:build !windows

package main

import (
	"bytes"
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The recursion fuse (ga-kihcmff, incident ga-2t6u9dy) at the cmd/gc seams
// that launch bd: the `gc bd` passthrough, its show --watch poller, the
// inventory structural walk and the class-guard read. Each stamps
// GC_BD_DEPTH on the child and refuses at the bound, before launching.

// writeCmdDepthStub writes a bd that logs "<argv> depth=<GC_BD_DEPTH>" for
// every call but `version`, which it answers unlogged (the allowlist probes
// ask which bd build will run). The log path is baked into the script because
// some seams run bd under a fixed minimal environment.
func writeCmdDepthStub(t *testing.T, path, log string) {
	t.Helper()
	script := "#!/bin/sh\n" +
		"case \"$1\" in version) echo 'bd version 1.3.2 (test)'; exit 0 ;; esac\n" +
		"printf '%s depth=%s\\n' \"$*\" \"$GC_BD_DEPTH\" >> '" + log + "'\n" +
		"case \"$1\" in show) echo '[{\"id\":\"demo-abc\",\"title\":\"t\",\"status\":\"open\",\"issue_type\":\"task\"}]' ;; list) echo '[]' ;; *) : ;; esac\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
}

// depthStandalone returns a standalone depth-logging bd and its log.
func depthStandalone(t *testing.T) (bd, log string) {
	t.Helper()
	bd = filepath.Join(t.TempDir(), "bd")
	log = filepath.Join(t.TempDir(), "depth.log")
	writeCmdDepthStub(t, bd, log)
	return bd, log
}

// depthInCity builds the single-exec test city and swaps its PATH bd for a
// depth-logging one.
func depthInCity(t *testing.T) (log string) {
	t.Helper()
	newSingleExecTestCity(t)
	path, err := exec.LookPath("bd")
	if err != nil {
		t.Fatal(err)
	}
	log = filepath.Join(t.TempDir(), "depth.log")
	writeCmdDepthStub(t, path, log)
	return log
}

func readDepthLog(t *testing.T, log string) []string {
	t.Helper()
	data, err := os.ReadFile(log)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatal(err)
	}
	var lines []string
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		// preflight's `bd context` probe is not a bead read.
		if line == "" || strings.HasPrefix(line, "context") {
			continue
		}
		lines = append(lines, line)
	}
	return lines
}

func TestGcBdPassthroughStampsDepthOnTheBdChild(t *testing.T) {
	log := depthInCity(t)
	t.Setenv("GC_BD_DEPTH", "3")
	var stdout, stderr bytes.Buffer
	if code := doBd([]string{"list", "--json"}, &stdout, &stderr); code != 0 {
		t.Fatalf("doBd at depth 3 = %d; stderr=%q", code, stderr.String())
	}
	got := readDepthLog(t, log)
	if len(got) != 1 || got[0] != "list --json depth=4" {
		t.Fatalf("bd calls = %q, want exactly one `list --json` at depth=4", got)
	}
}

func TestGcBdPassthroughRefusesAtTheBound(t *testing.T) {
	log := depthInCity(t)
	t.Setenv("GC_BD_DEPTH", "16")
	var stdout, stderr bytes.Buffer
	code := doBd([]string{"list", "--json"}, &stdout, &stderr)
	if code == 0 {
		t.Fatalf("doBd at depth 16 = 0, want a non-zero refusal; stdout=%q", stdout.String())
	}
	if !strings.Contains(stderr.String(), "GC_BD_DEPTH") || !strings.Contains(stderr.String(), "16") {
		t.Fatalf("stderr = %q, want it to name GC_BD_DEPTH and the depth", stderr.String())
	}
	if got := readDepthLog(t, log); len(got) != 0 {
		t.Fatalf("bd ran %d time(s) at the bound: %q", len(got), got)
	}
}

// A malformed counter is refused like an exhausted one: a value the guard
// cannot read must never silently reset the count.
func TestGcBdPassthroughRefusesAMalformedDepth(t *testing.T) {
	log := depthInCity(t)
	t.Setenv("GC_BD_DEPTH", "-1")
	var stdout, stderr bytes.Buffer
	if code := doBd([]string{"list", "--json"}, &stdout, &stderr); code == 0 {
		t.Fatalf("doBd with GC_BD_DEPTH=-1 = 0, want a refusal")
	}
	if !strings.Contains(stderr.String(), "GC_BD_DEPTH") || !strings.Contains(stderr.String(), `"-1"`) {
		t.Fatalf("stderr = %q, want it to name GC_BD_DEPTH and quote the value", stderr.String())
	}
	if got := readDepthLog(t, log); len(got) != 0 {
		t.Fatalf("bd ran at a malformed depth: %q", got)
	}
}

func TestBdWatchRunnerStampsAndRefuses(t *testing.T) {
	t.Run("stamps", func(t *testing.T) {
		bd, log := depthStandalone(t)
		t.Setenv("GC_BD_DEPTH", "3")
		var diagnostics, callStderr bytes.Buffer
		r := &bdWatchRunner{bdPath: bd, dir: t.TempDir(), env: os.Environ(), stderr: &diagnostics}
		if code := r.run(context.Background(), []string{"show", "demo-abc"}, io.Discard, &callStderr); code != 0 {
			t.Fatalf("run at depth 3 = %d; stderr=%q", code, callStderr.String())
		}
		if got := readDepthLog(t, log); len(got) != 1 || got[0] != "show demo-abc depth=4" {
			t.Fatalf("bd calls = %q, want one `show demo-abc` at depth=4", got)
		}
	})
	t.Run("refuses", func(t *testing.T) {
		bd, log := depthStandalone(t)
		t.Setenv("GC_BD_DEPTH", "16")
		var diagnostics, callStderr bytes.Buffer
		r := &bdWatchRunner{bdPath: bd, dir: t.TempDir(), env: os.Environ(), stderr: &diagnostics}
		if code := r.run(context.Background(), []string{"show", "demo-abc"}, io.Discard, &callStderr); code == 0 {
			t.Fatal("run at depth 16 = 0, want a refusal")
		}
		if !strings.Contains(callStderr.String(), "GC_BD_DEPTH") || !strings.Contains(callStderr.String(), bd) {
			t.Fatalf("call stderr = %q, want it to name GC_BD_DEPTH and %s", callStderr.String(), bd)
		}
		if got := readDepthLog(t, log); len(got) != 0 {
			t.Fatalf("bd ran at the bound: %q", got)
		}
	})
}

// The structural walk runs bd under a fixed minimal environment. The counter
// is the one thing that crosses that scrub.
func TestBdHelpRunnerStampsAndRefuses(t *testing.T) {
	t.Run("stamps", func(t *testing.T) {
		bd, log := depthStandalone(t)
		t.Setenv("GC_BD_DEPTH", "3")
		if _, err := bdHelpRunnerFor(bd, t.TempDir())(context.Background(), nil); err != nil {
			t.Fatalf("help walk at depth 3: %v", err)
		}
		if got := readDepthLog(t, log); len(got) != 1 || got[0] != "--help depth=4" {
			t.Fatalf("bd calls = %q, want one `--help` at depth=4", got)
		}
	})
	t.Run("refuses", func(t *testing.T) {
		bd, log := depthStandalone(t)
		t.Setenv("GC_BD_DEPTH", "16")
		_, err := bdHelpRunnerFor(bd, t.TempDir())(context.Background(), nil)
		if err == nil || !strings.Contains(err.Error(), "GC_BD_DEPTH") {
			t.Fatalf("help walk at depth 16 error = %v, want a refusal naming GC_BD_DEPTH", err)
		}
		if got := readDepthLog(t, log); len(got) != 0 {
			t.Fatalf("bd ran at the bound: %q", got)
		}
	})
}

func TestBdGuardReadStampsAndRefuses(t *testing.T) {
	setup := func(t *testing.T) (log, cityDir string) {
		log = depthInCity(t)
		cwd, err := os.Getwd()
		if err != nil {
			t.Fatal(err)
		}
		return log, cwd
	}
	call := func(t *testing.T, cityDir string) ([]byte, error) {
		t.Helper()
		var stderr bytes.Buffer
		cfg, err := loadBdPassthroughConfig(cityDir, &stderr)
		if err != nil {
			t.Fatalf("loadBdPassthroughConfig: %v (%s)", err, stderr.String())
		}
		return bdGuardRead(cityDir, cfg, execStoreTarget{ScopeRoot: cityDir, ScopeKind: "city", Prefix: "demo"}, []string{"list", "--json"})
	}
	t.Run("stamps", func(t *testing.T) {
		log, cityDir := setup(t)
		t.Setenv("GC_BD_DEPTH", "3")
		if _, err := call(t, cityDir); err != nil {
			t.Fatalf("bdGuardRead at depth 3: %v", err)
		}
		if got := readDepthLog(t, log); len(got) != 1 || got[0] != "list --json depth=4" {
			t.Fatalf("bd calls = %q, want one `list --json` at depth=4", got)
		}
	})
	t.Run("refuses", func(t *testing.T) {
		log, cityDir := setup(t)
		t.Setenv("GC_BD_DEPTH", "16")
		_, err := call(t, cityDir)
		if err == nil || !strings.Contains(err.Error(), "GC_BD_DEPTH") {
			t.Fatalf("bdGuardRead at depth 16 error = %v, want a refusal naming GC_BD_DEPTH", err)
		}
		if got := readDepthLog(t, log); len(got) != 0 {
			t.Fatalf("bd ran at the bound: %q", got)
		}
	})
}
