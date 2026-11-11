package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/shellquote"
)

// The query is operator-supplied shell. These checks permit silence or a
// conditional advisory without requiring a parser or a replacement sentence.
func assertHookCoverageAdvisoryIsTruthful(t *testing.T, diagnostic string) {
	t.Helper()
	categorical := regexp.MustCompile(`(?i)\b(reads (only )?one store|cannot see graph-class work|will not see.*coordination)\b`)
	conditional := regexp.MustCompile(`(?i)\b(if|unless|when|may|might|could|whether)\b`)
	for _, line := range strings.Split(strings.TrimSpace(diagnostic), "\n") {
		if categorical.MatchString(line) && !conditional.MatchString(line) {
			t.Errorf("custom-reader coverage was asserted without a condition: %s", line)
		}
	}
}

func TestHookFederationAdvisoryDoesNotAssumeCustomReaderCoverage(t *testing.T) {
	for _, tc := range []struct {
		name  string
		agent config.Agent
	}{
		{"direct federated reader", config.Agent{Name: "worker", WorkQuery: "gc ready --json"}},
		{"wrapped federated reader", config.Agent{Name: "worker", WorkQuery: "sh -c 'gc ready --json'"}},
		{"piped federated reader", config.Agent{Name: "worker", WorkQuery: "gc ready --json | jq '.'"}},
		{"single-store reader", config.Agent{Name: "worker", WorkQuery: "bd ready --json"}},
		{"federated scale check", config.Agent{Name: "worker", ScaleCheck: "gc ready --json | jq length"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			city := newHookWarningCity(t, true, "printf '[]'")
			cfg, err := loadCityConfig(city, &bytes.Buffer{})
			if err != nil {
				t.Fatal(err)
			}
			var stderr bytes.Buffer
			hookWorkQuery(city, "warning-city", cfg, &tc.agent, &stderr)
			assertHookCoverageAdvisoryIsTruthful(t, stderr.String())
		})
	}
}

func TestHookFederationAdvisoryIdentifiesTheOverride(t *testing.T) {
	for _, key := range []string{"work_query", "scale_check"} {
		t.Run(key, func(t *testing.T) {
			a := config.Agent{Name: "worker", Dir: "project"}
			if key == "work_query" {
				a.WorkQuery = "bd ready --json"
			} else {
				a.ScaleCheck = "bd ready --json | jq length"
			}
			var stderr bytes.Buffer
			warnFederationBlindOverrides(&stderr, &a, config.QueryTopology{FederatedReady: true})
			if stderr.Len() == 0 {
				return
			}
			for _, context := range []string{a.QualifiedName(), key, "gc ready"} {
				if !strings.Contains(stderr.String(), context) {
					t.Errorf("advisory %q lacks operator context %q", stderr.String(), context)
				}
			}
		})
	}
}

func TestHookFederationAdvisoryIsQuietWithoutAnApplicableOverride(t *testing.T) {
	for _, tc := range []struct {
		name  string
		agent *config.Agent
		split bool
	}{
		{"nil agent", nil, true},
		{"no override", &config.Agent{Name: "worker"}, true},
		{"blank overrides", &config.Agent{Name: "worker", WorkQuery: " \t", ScaleCheck: "\n"}, true},
		{"single-store city", &config.Agent{Name: "worker", WorkQuery: "gc ready --json", ScaleCheck: "echo 1"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var stderr bytes.Buffer
			warnFederationBlindOverrides(&stderr, tc.agent, config.QueryTopology{FederatedReady: tc.split})
			if stderr.Len() != 0 {
				t.Errorf("inapplicable coverage advisory: %s", stderr.String())
			}
		})
	}
}

func TestHookFederationAdvisoryAcceptsANilWriter(_ *testing.T) {
	warnFederationBlindOverrides(nil, &config.Agent{Name: "worker", WorkQuery: "gc ready --json"}, config.QueryTopology{FederatedReady: true})
}

func TestHookCommandFederatedReaderReceivesNoUnsupportedCoverageAssertion(t *testing.T) {
	city := newHookWarningCity(t, true, "printf '[{\"id\":\"test-work\",\"status\":\"open\"}]'")
	var stdout, stderr bytes.Buffer
	if code := run([]string{"--city", city, "hook", "worker", "--json"}, &stdout, &stderr); code != 0 {
		t.Fatalf("gc hook = %d; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	assertHookCoverageAdvisoryIsTruthful(t, stderr.String())
}

func TestHookCommandSingleStoreCustomQueryStaysQuiet(t *testing.T) {
	city := newHookWarningCity(t, false, "printf '[]'")
	var stdout, stderr bytes.Buffer
	if code := run([]string{"--city", city, "hook", "worker", "--json"}, &stdout, &stderr); code != 1 {
		t.Fatalf("empty hook = %d, want 1; stderr=%s", code, stderr.String())
	}
	if stderr.Len() != 0 {
		t.Errorf("single-store hook emitted a warning: %s", stderr.String())
	}
}

func TestHookCommandFederationAdvisoryPreservesReadOutcomes(t *testing.T) {
	for _, tc := range []struct {
		name   string
		script string
		code   int
		output string
	}{
		{"nonempty queue", `printf '[{"id":"test-work","status":"open"}]'`, 0, `[{"id":"test-work","status":"open"}]`},
		{"empty queue", `printf '[]'`, 1, `[]`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			city := newHookWarningCity(t, true, tc.script)
			var stdout, stderr bytes.Buffer
			code := run([]string{"--city", city, "hook", "worker", "--json"}, &stdout, &stderr)
			if code != tc.code {
				t.Errorf("gc hook = %d, want %d; stderr=%s", code, tc.code, stderr.String())
			}
			if got := strings.TrimSpace(stdout.String()); got != tc.output {
				t.Errorf("hook JSON = %q, want %q", got, tc.output)
			}
		})
	}
}

func TestHookCommandFederationAdvisoryPreservesFailedRead(t *testing.T) {
	city := newHookWarningCity(t, true, `printf 'fixture query failed\n' >&2; exit 7`)
	var stdout, stderr bytes.Buffer
	if code := run([]string{"--city", city, "hook", "worker", "--json"}, &stdout, &stderr); code != 2 {
		t.Errorf("failed read = %d, want 2; stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "fixture query failed") {
		t.Errorf("failed-read diagnostic was lost: %s", stderr.String())
	}
	var result jsonSchemaErrorPayload
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatalf("failure JSON: %v; stdout=%s", err, stdout.String())
	}
	if result.OK || result.Error.Code != "command_failed" || result.Error.ExitCode != 2 {
		t.Errorf("failure JSON = %+v, want command_failed with exit_code=2", result)
	}
}

func TestHookCommandFederationAdvisoryPreservesEmptyClaim(t *testing.T) {
	city := newHookWarningCity(t, true, "printf '[]'")
	var stdout, stderr bytes.Buffer
	code := run([]string{"--city", city, "hook", "worker", "--claim", "--json"}, &stdout, &stderr)
	if code != 1 {
		t.Errorf("empty claim = %d, want 1; stderr=%s", code, stderr.String())
	}
	var result hookClaimJSONResult
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatalf("claim JSON: %v; stdout=%s stderr=%s", err, stdout.String(), stderr.String())
	}
	if result.Action != "drain" || result.Reason != hookClaimReasonNoWork {
		t.Errorf("empty claim = %+v, want drain/no_work", result)
	}
}

func TestHookWarningPreservesCustomQueryAndTemplateExpansion(t *testing.T) {
	for _, tc := range []struct {
		query string
		want  string
	}{
		{"gc ready --json", "gc ready --json"},
		{"sh -c 'gc ready --json'", "sh -c 'gc ready --json'"},
		{"gc ready --json | jq '.'", "gc ready --json | jq '.'"},
		{"gc ready --assignee={{.AgentBase}} --json", "gc ready --assignee=worker --json"},
	} {
		t.Run(tc.query, func(t *testing.T) {
			city := newHookWarningCity(t, true, "printf '[]'")
			cfg, err := loadCityConfig(city, &bytes.Buffer{})
			if err != nil {
				t.Fatal(err)
			}
			a := config.Agent{Name: "worker", WorkQuery: tc.query}
			_, got := hookWorkQuery(city, "warning-city", cfg, &a, &bytes.Buffer{})
			if got != tc.want {
				t.Errorf("custom shell = %q, want %q", got, tc.want)
			}
		})
	}
}

// newHookWarningCity uses the real CLI path and a test-owned executable named
// gc. The custom query resolves that stub locally, never a running city's gc.
// Every invocation waits for its shell; no sleeps or background workers exist.
func newHookWarningCity(t *testing.T, split bool, script string) string {
	t.Helper()
	clearGCEnv(t)
	disableManagedDoltRecoveryForTest(t)
	resetCLIStorageRoutes(t)
	city := t.TempDir()
	binding := ""
	if split {
		binding = filepath.Join(t.TempDir(), "infra")
	}
	writeOneShotCityTOML(t, city, binding)
	bin := t.TempDir()
	stub := "#!/bin/sh\n[ \"$*\" = 'ready --json' ] || exit 95\n" + script + "\n"
	if err := os.WriteFile(filepath.Join(bin, "gc"), []byte(stub), 0o755); err != nil {
		t.Fatal(err)
	}
	query := fmt.Sprintf("PATH=%s:\"$PATH\" gc ready --json", shellquote.Quote(bin))
	body, err := os.ReadFile(filepath.Join(city, "city.toml"))
	if err != nil {
		t.Fatal(err)
	}
	body = append(body, fmt.Appendf(nil, "\n[beads]\nprovider = \"file\"\n\n[[agent]]\nname = \"worker\"\nwork_query = %q\n", query)...)
	body = append(body, builtinImportsTOML("core")...)
	writeBuiltinImportsLock(t, city, "core")
	if err := os.WriteFile(filepath.Join(city, "city.toml"), body, 0o644); err != nil {
		t.Fatal(err)
	}
	return city
}
