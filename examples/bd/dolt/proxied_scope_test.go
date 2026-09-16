package dolt_test

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// proxiedNoOpMessage is the one line every managed-Dolt verb prints on a
// bd-owned proxied scope. It is duplicated here on purpose: the test asserts
// the contract text, not a shared constant that could drift with the scripts.
const proxiedNoOpMessage = "dolt lifecycle is owned by bd for proxied scopes; nothing to do"

// writeProxiedScope lays out the on-disk shape `bd init --proxied-server`
// leaves behind: the mode lives only in metadata.json, and the proxy root
// under .beads/dolt holds the sql-server config plus bd's proxy records.
func writeProxiedScope(t *testing.T, cityPath string) {
	t.Helper()
	root := filepath.Join(cityPath, ".beads", "dolt")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	metadata := `{
  "backend": "dolt",
  "database": "dolt",
  "dolt_mode": "proxied-server",
  "dolt_database": "hq",
  "project_id": "p"
}`
	if err := os.WriteFile(filepath.Join(cityPath, ".beads", "metadata.json"), []byte(metadata), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, ".beads", "config.yaml"), []byte("issue_prefix: hq\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "config.yaml"), []byte("listener:\n  host: 127.0.0.1\n  port: 32969\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "proxy.pid"),
		[]byte(`{"pid":4242,"port":35425,"schema":2,"kind":"db-proxy","control_port":46445}`), 0o600); err != nil {
		t.Fatal(err)
	}
}

// hostileToolPath shadows dolt, bd, lsof, nc and mysql with stubs that fail
// loudly, so any probe or lifecycle attempt against a bd-owned scope shows up
// as a test failure instead of a quiet success. The rest of the host PATH stays
// so the scripts still have sh, sed, head and friends.
func hostileToolPath(t *testing.T) string {
	t.Helper()
	binDir := t.TempDir()
	for _, tool := range []string{"dolt", "bd", "lsof", "nc", "mysql", "pkill"} {
		writeExecutable(t, filepath.Join(binDir, tool),
			"#!/bin/sh\necho \"FAIL: "+tool+" was invoked on a bd-owned proxied scope\" >&2\nexit 97\n")
	}
	return binDir + string(os.PathListSeparator) + os.Getenv("PATH")
}

func proxiedScopeEnv(t *testing.T, root, cityPath string) []string {
	t.Helper()
	return append(filteredEnv("GC_CITY_PATH", "GC_PACK_DIR", "PATH"),
		"GC_CITY_PATH="+cityPath,
		"GC_PACK_DIR="+root,
		"PATH="+hostileToolPath(t),
	)
}

func snapshotScope(t *testing.T, root string) map[string]string {
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

func assertScopeUnchanged(t *testing.T, before, after map[string]string) {
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

// TestDoltPackOrderEntryPointsNoOpOnProxiedScope is the R4 front-door case:
// the bd pack imports the dolt pack, whose orders fire on every fresh city.
// Each entry point those orders reach must exit 0 with the typed message and
// leave the scope untouched.
func TestDoltPackOrderEntryPointsNoOpOnProxiedScope(t *testing.T) {
	root := repoRoot(t)
	cases := []struct {
		name   string
		script string
		args   []string
	}{
		{"dolt-health order: gc dolt health", "commands/health/run.sh", nil},
		{"mol-dog-doctor order", "assets/scripts/mol-dog-doctor.sh", nil},
		{"mol-dog-stale-db order: gc dolt cleanup", "commands/cleanup/run.sh", nil},
		{"mol-dog-stale-db order: gc dolt cleanup --force", "commands/cleanup/run.sh", []string{"--force"}},
		{"mol-dog-phantom-db order", "assets/scripts/mol-dog-phantom-db.sh", nil},
		{"mol-dog-backup order", "assets/scripts/mol-dog-backup.sh", nil},
		{"mol-dog-compactor order: gc dolt compact", "commands/compact/run.sh", nil},
		{"dolt-remotes-patrol order: gc dolt sync", "commands/sync/run.sh", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cityPath := t.TempDir()
			writeProxiedScope(t, cityPath)
			before := snapshotScope(t, cityPath)

			// Run the scripts through their own shebangs: the mol-dog-*
			// entry points are bash and use `set -o pipefail`.
			cmd := exec.Command(filepath.Join(root, tc.script), tc.args...) //nolint:gosec // fixed pack script path
			cmd.Env = proxiedScopeEnv(t, root, cityPath)
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("%s exited non-zero: %v\n%s", tc.script, err, out)
			}
			if !strings.Contains(string(out), proxiedNoOpMessage) {
				t.Fatalf("%s did not report the typed no-op:\n%s", tc.script, out)
			}
			assertScopeUnchanged(t, before, snapshotScope(t, cityPath))
		})
	}
}

// TestHealthJSONEmitsSkipDocumentOnProxiedScope pins the machine-readable
// form: the dolt-health order pipes `gc dolt health --json` into
// `gc dolt health-check`, so the document has to stay parseable JSON.
func TestHealthJSONEmitsSkipDocumentOnProxiedScope(t *testing.T) {
	root := repoRoot(t)
	cityPath := t.TempDir()
	writeProxiedScope(t, cityPath)
	before := snapshotScope(t, cityPath)

	cmd := exec.Command("sh", filepath.Join(root, "commands/health/run.sh"), "--json")
	cmd.Env = proxiedScopeEnv(t, root, cityPath)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("health --json exited non-zero: %v\n%s", err, out)
	}
	var doc struct {
		Timestamp string `json:"timestamp"`
		Skipped   *struct {
			Reason  string `json:"reason"`
			Message string `json:"message"`
		} `json:"skipped"`
	}
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatalf("health --json is not valid JSON: %v\n%s", err, out)
	}
	if doc.Skipped == nil || doc.Skipped.Reason != "bd-owned-proxied-scope" {
		t.Fatalf("skip document = %+v, want reason bd-owned-proxied-scope\n%s", doc.Skipped, out)
	}
	if doc.Skipped.Message != proxiedNoOpMessage {
		t.Errorf("skip message = %q, want %q", doc.Skipped.Message, proxiedNoOpMessage)
	}
	if doc.Timestamp == "" {
		t.Error("skip document has no timestamp")
	}
	assertScopeUnchanged(t, before, snapshotScope(t, cityPath))
}

// TestRuntimeGuardEmitsSkipJSONForJSONCallers pins the machine-readable
// contract for the commands that do not shape their own output: the guard runs
// before they parse flags, so a plain sentence on stdout would corrupt a
// --json consumer's stream.
func TestRuntimeGuardEmitsSkipJSONForJSONCallers(t *testing.T) {
	root := repoRoot(t)
	// compact and sync parse their own flags before sourcing runtime.sh and
	// reject --json as unknown; the guard neither sees nor loosens that, which
	// TestProxiedGuardDoesNotLoosenFlagContracts pins.
	for _, script := range []string{
		"commands/status/run.sh",
		"commands/cleanup/run.sh",
	} {
		t.Run(script, func(t *testing.T) {
			cityPath := t.TempDir()
			writeProxiedScope(t, cityPath)
			before := snapshotScope(t, cityPath)

			cmd := exec.Command("sh", filepath.Join(root, script), "--json") //nolint:gosec // fixed pack script path
			cmd.Env = proxiedScopeEnv(t, root, cityPath)
			out, err := cmd.Output()
			if err != nil {
				t.Fatalf("%s --json exited non-zero: %v\n%s", script, err, out)
			}
			var doc struct {
				Skipped *struct {
					Reason  string `json:"reason"`
					Message string `json:"message"`
				} `json:"skipped"`
			}
			if err := json.Unmarshal(out, &doc); err != nil {
				t.Fatalf("%s --json is not valid JSON: %v\n%s", script, err, out)
			}
			if doc.Skipped == nil || doc.Skipped.Reason != "bd-owned-proxied-scope" {
				t.Fatalf("%s skip document = %+v, want reason bd-owned-proxied-scope\n%s", script, doc.Skipped, out)
			}
			if doc.Skipped.Message != proxiedNoOpMessage {
				t.Errorf("%s skip message = %q, want %q", script, doc.Skipped.Message, proxiedNoOpMessage)
			}
			assertScopeUnchanged(t, before, snapshotScope(t, cityPath))
		})
	}
}

// TestProxiedGuardDoesNotLoosenFlagContracts pins the other half of the --json
// contract: commands that reject unknown flags do so on a bd-owned scope too.
// compact and sync parse before sourcing runtime.sh, so their refusal is what
// the operator sees — the guard must not turn an unsupported flag into a
// success document.
func TestProxiedGuardDoesNotLoosenFlagContracts(t *testing.T) {
	root := repoRoot(t)
	for _, script := range []string{"commands/compact/run.sh", "commands/sync/run.sh"} {
		t.Run(script, func(t *testing.T) {
			cityPath := t.TempDir()
			writeProxiedScope(t, cityPath)

			cmd := exec.Command("sh", filepath.Join(root, script), "--json") //nolint:gosec // fixed pack script path
			cmd.Env = proxiedScopeEnv(t, root, cityPath)
			out, err := cmd.CombinedOutput()
			if err == nil {
				t.Fatalf("%s accepted an unsupported --json flag:\n%s", script, out)
			}
			if !strings.Contains(string(out), "unknown flag") {
				t.Fatalf("%s did not report the unknown flag:\n%s", script, out)
			}
		})
	}
}

// TestProxiedOwnershipPredicateMatchesCanonicalBackends keeps the sh guard in
// step with cmd/gc's scopeBindingIsProviderOwnedProxied: backend dolt, bd or
// absent all mean Dolt, case-insensitively, and anything else does not.
func TestProxiedOwnershipPredicateMatchesCanonicalBackends(t *testing.T) {
	root := repoRoot(t)
	cases := []struct {
		name     string
		metadata string
		bdOwned  bool
	}{
		{"backend dolt", `{"backend":"dolt","dolt_mode":"proxied-server"}`, true},
		{"backend bd", `{"backend":"bd","dolt_mode":"proxied-server"}`, true},
		{"backend absent", `{"dolt_mode":"proxied-server"}`, true},
		{"mixed case", `{"backend":"BD","dolt_mode":"Proxied-Server"}`, true},
		{"backend doltlite", `{"backend":"doltlite","dolt_mode":"proxied-server"}`, false},
		{"backend sqlite", `{"backend":"sqlite","dolt_mode":"proxied-server"}`, false},
		{"server mode", `{"backend":"dolt","dolt_mode":"server"}`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cityPath := t.TempDir()
			if err := os.MkdirAll(filepath.Join(cityPath, ".beads"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(cityPath, ".beads", "metadata.json"), []byte(tc.metadata), 0o600); err != nil {
				t.Fatal(err)
			}

			cmd := exec.Command("sh", "-c", //nolint:gosec // fixed pack script path
				". "+filepath.Join(root, "assets/scripts/proxied_scope.sh")+"; bd_owns_proxied_scope && echo owned || echo not-owned")
			cmd.Env = proxiedScopeEnv(t, root, cityPath)
			out, err := cmd.Output()
			if err != nil {
				t.Fatalf("predicate probe failed: %v\n%s", err, out)
			}
			got := strings.TrimSpace(string(out)) == "owned"
			if got != tc.bdOwned {
				t.Fatalf("bd_owns_proxied_scope(%s) = %v, want %v", tc.metadata, got, tc.bdOwned)
			}
		})
	}
}

// TestHealthCheckAcceptsProxiedSkipDocument covers the second half of the
// dolt-health order: the parser must not record order.failed for a report
// that has no server section because bd owns the scope.
func TestHealthCheckAcceptsProxiedSkipDocument(t *testing.T) {
	root := repoRoot(t)
	cityPath := t.TempDir()
	writeProxiedScope(t, cityPath)

	pipeline := exec.Command("sh", "-c", //nolint:gosec // fixed pack script paths
		"sh "+filepath.Join(root, "commands/health/run.sh")+" --json | sh "+filepath.Join(root, "commands/health-check/run.sh"))
	pipeline.Env = proxiedScopeEnv(t, root, cityPath)
	out, err := pipeline.CombinedOutput()
	if err != nil {
		t.Fatalf("dolt-health order pipeline failed: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "bd-owned-proxied-scope") {
		t.Fatalf("pipeline lost the skip document:\n%s", out)
	}
}

// TestHealthCheckStillFailsOnUnreachableServer guards the direct path: the
// proxied short-circuit must not swallow a real managed-Dolt outage.
func TestHealthCheckStillFailsOnUnreachableServer(t *testing.T) {
	root := repoRoot(t)
	cityPath := t.TempDir()

	cmd := exec.Command("sh", filepath.Join(root, "commands/health-check/run.sh"))
	cmd.Env = proxiedScopeEnv(t, root, cityPath)
	cmd.Stdin = strings.NewReader(`{"server":{"reachable":false,"running":false,"pid":0,"port":3307,"latency_ms":0}}`)
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("health-check exited 0 for an unreachable server:\n%s", out)
	}
	if !strings.Contains(string(out), "Dolt server unreachable") {
		t.Fatalf("health-check lost its unreachable message:\n%s", out)
	}
}

// TestDirectScopeStillResolvesManagedPort guards the non-proxied lens: a city
// with no proxied binding keeps the old runtime.sh behavior (port resolution,
// exit 78 when there is nothing to resolve), not a silent no-op.
func TestDirectScopeStillResolvesManagedPort(t *testing.T) {
	root := repoRoot(t)
	cityPath := t.TempDir()

	cmd := exec.Command("sh", filepath.Join(root, "commands/health/run.sh"), "--json")
	cmd.Env = proxiedScopeEnv(t, root, cityPath)
	out, _ := cmd.CombinedOutput()
	if strings.Contains(string(out), proxiedNoOpMessage) {
		t.Fatalf("a city with no proxied binding was treated as bd-owned:\n%s", out)
	}
}

// TestServerModeScopeIsNotBdOwned pins that only proxied-server counts: a
// direct managed-Dolt city must keep the managed lens even though it has the
// same metadata file.
func TestServerModeScopeIsNotBdOwned(t *testing.T) {
	root := repoRoot(t)
	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, ".beads", "metadata.json"),
		[]byte(`{"backend":"dolt","dolt_mode":"server","dolt_database":"hq"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command("sh", filepath.Join(root, "commands/health/run.sh"), "--json")
	cmd.Env = proxiedScopeEnv(t, root, cityPath)
	out, _ := cmd.CombinedOutput()
	if strings.Contains(string(out), proxiedNoOpMessage) {
		t.Fatalf("a server-mode city was treated as bd-owned:\n%s", out)
	}
}
