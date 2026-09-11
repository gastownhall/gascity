package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// writeProxiedCityFixture builds the on-disk shape bd v1.3.0-rc.2 leaves after
// `bd init --proxied-server`: the mode lives only in metadata.json, and the
// proxy root under .beads/dolt carries the sql-server config plus the proxy
// pid record.
func writeProxiedCityFixture(t *testing.T, dir string, pid int) {
	t.Helper()
	writeCityTOML(t, dir, "proxied")
	beads := filepath.Join(dir, ".beads")
	root := filepath.Join(beads, "dolt")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	metadata := `{"backend":"dolt","database":"dolt","dolt_mode":"proxied-server","dolt_database":"hq","project_id":"p"}`
	if err := os.WriteFile(filepath.Join(beads, "metadata.json"), []byte(metadata), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(beads, "config.yaml"), []byte("issue_prefix: hq\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "config.yaml"), []byte("listener:\n  host: 127.0.0.1\n  port: 32969\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	proxyPID := fmt.Sprintf(`{"pid":%d,"port":35425,"schema":2,"kind":"db-proxy","control_port":46445}`, pid)
	if err := os.WriteFile(filepath.Join(root, "proxy.pid"), []byte(proxyPID), 0o600); err != nil {
		t.Fatal(err)
	}
}

// snapshotTree fingerprints every file under root so a "no mutation" assertion
// covers creations, deletions and content edits alike.
func snapshotTree(t *testing.T, root string) map[string]string {
	t.Helper()
	snapshot := map[string]string{}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return relErr
		}
		if d.IsDir() {
			snapshot[rel+"/"] = "dir"
			return nil
		}
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		sum := sha256.Sum256(data)
		snapshot[rel] = hex.EncodeToString(sum[:])
		return nil
	})
	if err != nil {
		t.Fatalf("snapshot %s: %v", root, err)
	}
	return snapshot
}

func assertTreeUnchanged(t *testing.T, before, after map[string]string) {
	t.Helper()
	var diffs []string
	for path, sum := range after {
		if prev, ok := before[path]; !ok {
			diffs = append(diffs, "created "+path)
		} else if prev != sum {
			diffs = append(diffs, "modified "+path)
		}
	}
	for path := range before {
		if _, ok := after[path]; !ok {
			diffs = append(diffs, "removed "+path)
		}
	}
	if len(diffs) > 0 {
		sort.Strings(diffs)
		t.Fatalf("proxied scope mutated:\n%s", strings.Join(diffs, "\n"))
	}
}

// loudFailingDoltPath returns a PATH whose only `dolt` fails loudly, so any
// probe attempt shows up as a test failure rather than a silent success.
func loudFailingDoltPath(t *testing.T) string {
	t.Helper()
	binDir := t.TempDir()
	script := "#!/bin/sh\necho 'FAIL: dolt was invoked on a bd-owned proxied scope' >&2\nexit 97\n"
	if err := os.WriteFile(filepath.Join(binDir, "dolt"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return binDir
}

func TestDoltScopeIsBdOwnedProxiedReadsPersistedMetadata(t *testing.T) {
	proxied := t.TempDir()
	writeProxiedCityFixture(t, proxied, os.Getpid())
	if !doltScopeIsBdOwnedProxied(proxied) {
		t.Error("proxied-server metadata not classified as bd-owned")
	}

	direct := t.TempDir()
	writeCityTOML(t, direct, "direct")
	if err := os.MkdirAll(filepath.Join(direct, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(direct, ".beads", "metadata.json"),
		[]byte(`{"backend":"dolt","dolt_mode":"server","dolt_database":"hq"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if doltScopeIsBdOwnedProxied(direct) {
		t.Error("server-mode metadata classified as bd-owned proxied")
	}

	fresh := t.TempDir()
	writeCityTOML(t, fresh, "fresh")
	if doltScopeIsBdOwnedProxied(fresh) {
		t.Error("unbound scope classified as bd-owned proxied; only a persisted binding counts")
	}

	doltlite := t.TempDir()
	writeCityTOML(t, doltlite, "doltlite")
	if err := os.MkdirAll(filepath.Join(doltlite, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(doltlite, ".beads", "metadata.json"),
		[]byte(`{"backend":"doltlite","dolt_mode":"proxied-server"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if doltScopeIsBdOwnedProxied(doltlite) {
		t.Error("stale dolt_mode marker on a doltlite scope classified as bd-owned proxied")
	}
}

// TestDoltCleanupJSONIsTypedNoOpOnProxiedScope is the R4 front-door case: the
// mol-dog-stale-db order runs `gc dolt-cleanup --json --probe` on every city,
// including proxied ones. It must exit 0 with a parseable envelope and touch
// nothing.
func TestDoltCleanupJSONIsTypedNoOpOnProxiedScope(t *testing.T) {
	dir := t.TempDir()
	writeProxiedCityFixture(t, dir, os.Getpid())
	t.Setenv("PATH", loudFailingDoltPath(t))
	t.Setenv("GC_CITY_PATH", dir)
	t.Chdir(dir)

	before := snapshotTree(t, dir)
	var stdout, stderr bytes.Buffer
	cmd := newDoltCleanupCmd(&stdout, &stderr)
	cmd.SetArgs([]string{"--json", "--probe"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute: %v\nstdout=%s\nstderr=%s", err, stdout.String(), stderr.String())
	}

	var report struct {
		Schema  string `json:"schema"`
		Skipped *struct {
			Reason  string `json:"reason"`
			Message string `json:"message"`
		} `json:"skipped"`
		Dropped struct {
			Count int `json:"count"`
		} `json:"dropped"`
		Reaped struct {
			Targets []any `json:"targets"`
		} `json:"reaped"`
		Summary struct {
			ErrorsTotal int `json:"errors_total"`
		} `json:"summary"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
		t.Fatalf("parse report: %v\nstdout=%s", err, stdout.String())
	}
	if report.Schema != CleanupSchemaVersion {
		t.Errorf("schema = %q, want %q", report.Schema, CleanupSchemaVersion)
	}
	if report.Skipped == nil || report.Skipped.Reason != cleanupSkipReasonProxiedScope {
		t.Fatalf("skip envelope = %+v, want reason %q", report.Skipped, cleanupSkipReasonProxiedScope)
	}
	if report.Skipped.Message != proxiedScopeNoOpMessage {
		t.Errorf("skip message = %q, want %q", report.Skipped.Message, proxiedScopeNoOpMessage)
	}
	if report.Dropped.Count != 0 || len(report.Reaped.Targets) != 0 || report.Summary.ErrorsTotal != 0 {
		t.Errorf("skip envelope is not a zero report: %s", stdout.String())
	}
	if stderr.Len() != 0 {
		t.Errorf("stderr = %q, want silence", stderr.String())
	}
	assertTreeUnchanged(t, before, snapshotTree(t, dir))
}

// TestDoltCleanupForceIsTypedNoOpOnProxiedScope covers the destructive branch:
// even --force must not drop, purge or reap on a scope bd owns.
func TestDoltCleanupForceIsTypedNoOpOnProxiedScope(t *testing.T) {
	dir := t.TempDir()
	writeProxiedCityFixture(t, dir, os.Getpid())
	t.Setenv("PATH", loudFailingDoltPath(t))
	t.Setenv("GC_CITY_PATH", dir)
	t.Chdir(dir)

	before := snapshotTree(t, dir)
	var stdout, stderr bytes.Buffer
	cmd := newDoltCleanupCmd(&stdout, &stderr)
	cmd.SetArgs([]string{"--json", "--probe", "--force", "--max-orphan-dbs", "20"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute: %v\nstdout=%s\nstderr=%s", err, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), cleanupSkipReasonProxiedScope) {
		t.Fatalf("forced cleanup did not skip on a proxied scope:\n%s", stdout.String())
	}
	assertTreeUnchanged(t, before, snapshotTree(t, dir))
}

// TestDoltCleanupTextIsTypedNoOpOnProxiedScope covers the human front door.
func TestDoltCleanupTextIsTypedNoOpOnProxiedScope(t *testing.T) {
	dir := t.TempDir()
	writeProxiedCityFixture(t, dir, os.Getpid())
	t.Setenv("PATH", loudFailingDoltPath(t))
	t.Setenv("GC_CITY_PATH", dir)
	t.Chdir(dir)

	before := snapshotTree(t, dir)
	var stdout, stderr bytes.Buffer
	cmd := newDoltCleanupCmd(&stdout, &stderr)
	cmd.SetArgs(nil)
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute: %v\nstdout=%s\nstderr=%s", err, stdout.String(), stderr.String())
	}
	if strings.TrimSpace(stdout.String()) != proxiedScopeNoOpMessage {
		t.Fatalf("stdout = %q, want %q", stdout.String(), proxiedScopeNoOpMessage)
	}
	assertTreeUnchanged(t, before, snapshotTree(t, dir))
}
