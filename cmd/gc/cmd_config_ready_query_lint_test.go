package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rogpeppe/go-internal/testscript"
)

// T-A8 exercises the resolved configuration through an existing operator
// command. No query is executed and no store, session, or controller is needed.
const readyLintReader = `gc ready --metadata-field "gc.routed_to=forge/worker" --unassigned --exclude-type step --exclude-type molecule --exclude-type convoy --exclude-label hold:mayor --exclude-label hold:external --exclude-label human --exclude-label parent-record --limit 100 --json`

func readyLintLoudQuery(reader string) string {
	return `gc_out=$(` + reader + `) || { gc_rc=$?; printf 'gc ready failed (exit %s)\n' "$gc_rc" >&2; exit "$gc_rc"; }
printf '%s\n' "$gc_out" | jq -c '[.[]|select((.metadata."gc.budget_deferred_until"//"")=="" and (.metadata."gc.rebuild_deferred_until"//"")=="")]' | if command -v quiet-window >/dev/null 2>&1; then quiet-window claim-filter; else cat; fi`
}

type readyLintCase struct {
	name  string
	agent string
	query string
	split bool
	// Each entry identifies one diagnostic without fixing its prose.
	want [][]string
}

func readyLintCases() []readyLintCase {
	loud := readyLintLoudQuery(readyLintReader)
	return []readyLintCase{
		{name: "S1 single label reader cannot see the binding", query: `gc bd ready --label needs-tests --json`, split: true, want: [][]string{{"gc bd", "ready"}}},
		{name: "S2 label OR reader cannot see the binding", query: `gc bd ready --label-any needs-tests,needs-check --json`, split: true, want: [][]string{{"gc bd", "ready"}}},
		{name: "S3 builder pipeline cannot see the binding", query: `gc bd ready --label ready-to-build --json 2>/dev/null | jq -c '[.[]]' | cat`, split: true, want: [][]string{{"gc bd", "ready"}}},
		{name: "S4 single store route reader cannot see the binding", query: `gc bd --rig forge ready --metadata-field gc.routed_to=forge/worker --json`, split: true, want: [][]string{{"gc bd", "ready"}}},
		{name: "list ready alias cannot see the binding", query: `gc bd list --ready --json`, split: true, want: [][]string{{"gc bd", "--ready"}}},
		{name: "one ledger override does not make dispatch safe", query: `GC_BD_ALLOW_RELOCATED_CLASS_READ=1 gc bd ready --json`, split: true, want: [][]string{{"gc bd", "ready"}}},
		{name: "single store city retains its reader", query: `gc bd ready --label needs-tests --json`},
		{name: "single store city retains list ready", query: `gc bd list --ready --json`},
		{name: "comma type exclusion fails open", query: readyLintLoudQuery(strings.Replace(readyLintReader, "--exclude-type step --exclude-type molecule --exclude-type convoy", "--exclude-type step,molecule,convoy", 1)), split: true, want: [][]string{{"--exclude-type", "comma"}}},
		{name: "comma label exclusion fails open", query: readyLintLoudQuery(strings.Replace(readyLintReader, "--exclude-label hold:mayor --exclude-label hold:external", `--exclude-label="hold:mayor,hold:external"`, 1)), split: true, want: [][]string{{"--exclude-label", "comma"}}},
		{name: "comma exclusion is unsafe before cutover too", query: readyLintLoudQuery(strings.Replace(readyLintReader, "--exclude-type step --exclude-type molecule", "--exclude-type step,molecule", 1)), want: [][]string{{"--exclude-type", "comma"}}},
		{name: "global rig before ready is ignored", query: readyLintLoudQuery(strings.Replace(readyLintReader, "gc ready", "gc --rig forge ready", 1)), split: true, want: [][]string{{"--rig", "ignored"}}},
		{name: "global rig after ready is ignored", query: readyLintLoudQuery(readyLintReader + " --rig=forge"), split: true, want: [][]string{{"--rig", "ignored"}}},
		{name: "converted reader needs an explicit limit", query: readyLintLoudQuery(strings.Replace(readyLintReader, " --limit 100", "", 1)), split: true, want: [][]string{{"--limit"}}},
		{name: "last pipeline stage hides reader failure", query: readyLintReader + " 2>/dev/null | jq -c '[.[]]' | cat", split: true, want: [][]string{{"reader", "exit"}}},
		{name: "capture without a guard hides reader failure", query: `gc_out=$(` + readyLintReader + `); printf '%s\n' "$gc_out" | cat`, split: true, want: [][]string{{"reader", "exit"}}},
		{name: "exit inside pipeline element does not exit the script", query: `{ ` + loud + `; } | cat`, split: true, want: [][]string{{"reader", "exit"}}},
		{name: "fallback array hides reader failure", query: readyLintReader + ` || printf '[]\n'`, split: true, want: [][]string{{"reader", "exit"}}},
		{name: "S1 and S2 converted route query is safe", query: readyLintLoudQuery(readyLintReader), split: true},
		{name: "S3 converted filters stay after the loud reader", query: loud, split: true},
		{name: "S4 converted route query with equals limit is safe", query: readyLintLoudQuery(strings.Replace(readyLintReader, "--limit 100", "--limit=100", 1)), split: true},
		{name: "standalone reader propagates its own status", query: readyLintReader, split: true},
		{name: "empty query uses the generated reader", split: true},
		{name: "unrelated shell command is outside this lint", query: `printf '[]\n'`, split: true},
		{name: "S5 deep investigator retains raw bd", agent: "deep-investigator", query: `bd ready --label needs-cross-rig-investigation --json 2>/dev/null | cat`, split: true},
		{name: "S5 pack author retains raw bd", agent: "pack-author", query: `bd ready --label-any needs-pack-work,needs-cross-rig-investigation --json 2>/dev/null | cat`, split: true},
		{name: "S6 city guard and global jq cap stay safe", agent: "triager", query: `if [ "${GC_STORE_SCOPE:-}" = rig ]; then echo '[]'; exit 0; fi
gc_out=$(gc ready --metadata-field gh.repo --limit 0 --json) || { gc_rc=$?; exit "$gc_rc"; }
printf '%s\n' "$gc_out" | jq -c '[.[]|select((.labels//[])|index("needs-triage"))] | .[0:5]'`, split: true},
		{name: "S7 one ledger scan warning stays advisory", agent: "labeller", query: `GC_BD_ALLOW_RELOCATED_CLASS_READ=1 gc bd ready --json`, split: true, want: [][]string{{"gc bd", "ready"}}},
	}
}

func TestReadyQueryLintReportsAdvisoriesForResolvedReaders(t *testing.T) {
	for _, tc := range readyLintCases() {
		for _, field := range []string{"work_query", "scale_check"} {
			t.Run(tc.name+"/"+field, func(t *testing.T) {
				agent := tc.agent
				if agent == "" {
					agent = "worker"
				}
				readyLintCity(t, tc.split, agent, field, tc.query)
				warnings := readyLintValidation(t)
				if len(warnings) != len(tc.want) {
					t.Fatalf("%s advisories = %q, want %d for %q", field, warnings, len(tc.want), tc.query)
				}
				for i, terms := range tc.want {
					for _, term := range append([]string{agent, field}, terms...) {
						if !strings.Contains(strings.ToLower(warnings[i]), strings.ToLower(term)) {
							t.Errorf("diagnostic %q does not identify %q", warnings[i], term)
						}
					}
				}
			})
		}
	}
}

func TestReadyQueryLintLeavesCustomQueriesVerbatim(t *testing.T) {
	for _, field := range []string{"work_query", "scale_check"} {
		t.Run(field, func(t *testing.T) {
			const query = `gc bd --rig forge ready --label needs-tests --json 2>/dev/null | cat`
			city := readyLintCity(t, true, "worker", field, query)
			readyLintValidation(t)
			after, _, err := loadConfigCommandCityConfig(city)
			if err != nil {
				t.Fatal(err)
			}
			for _, agent := range after.Agents {
				if agent.Name != "worker" {
					continue
				}
				got := agent.WorkQuery
				if field == "scale_check" {
					got = agent.ScaleCheck
				}
				if got != query {
					t.Fatalf("%s after advisory validation = %q, want authored query %q", field, got, query)
				}
				return
			}
			t.Fatal("resolved configuration lost the worker agent")
		})
	}
}

func TestReadyQueryLintPreservesValidationFailure(t *testing.T) {
	city := readyLintCity(t, true, "worker", "work_query", `gc bd ready --json`)
	path := filepath.Join(city, "city.toml")
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	writeLintFile(t, path, string(body)+"\n[[rigs]]\nname = 'broken'\n")
	var stdout, stderr bytes.Buffer
	if code := run([]string{"config", "show", "--validate"}, &stdout, &stderr); code == 0 {
		t.Fatalf("query advisories masked invalid configuration: stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
}

func TestReadyQueryLintIgnoresPromptAndStartupScans(t *testing.T) {
	city := readyLintCity(t, true, "worker", "work_query", "")
	writeLintFile(t, filepath.Join(city, "agents", "worker", "agent.toml"), "max_active_sessions = 2\nstart_command = 'gc bd ready --json'\n")
	writeLintFile(t, filepath.Join(city, "agents", "worker", "prompt.md"), "For a non-dispatch scan: gc bd ready --json\n")
	warnings := readyLintValidation(t)
	if len(warnings) != 0 {
		t.Fatalf("S8 prompt/startup scan was treated as a dispatch query: %q", warnings)
	}
}

func TestReadyQueryLintUsesComposedOwnerAndEffectiveQuery(t *testing.T) {
	city := readyLintCity(t, true, "worker", "work_query", "")
	if err := os.RemoveAll(filepath.Join(city, "agents")); err != nil {
		t.Fatal(err)
	}
	writeLintFile(t, filepath.Join(city, "pack.toml"), "[pack]\nname = 'lint-city'\nschema = 2\n\n[imports.tools]\nsource = './assets/tools'\n")
	writeLintFile(t, filepath.Join(city, "assets", "tools", "pack.toml"), "[pack]\nname = 'tools'\nschema = 2\n")
	writeLintFile(t, filepath.Join(city, "assets", "tools", "agents", "worker", "agent.toml"), "max_active_sessions = 2\nwork_query = 'gc bd ready --json'\n")
	warnings := readyLintValidation(t)
	if len(warnings) != 1 || !strings.Contains(warnings[0], "tools.worker") || !strings.Contains(warnings[0], "work_query") {
		t.Fatalf("advisories = %q, want the composed tools.worker work_query owner", warnings)
	}
}

func TestReadyQueryLintChecksTheEffectiveOverlay(t *testing.T) {
	for _, unsafeOverlay := range []bool{false, true} {
		t.Run(fmt.Sprintf("unsafe overlay %t", unsafeOverlay), func(t *testing.T) {
			base, overlay := `gc bd ready --json`, readyLintReader
			want := 0
			if unsafeOverlay {
				base, overlay = overlay, base
				want = 1
			}
			city := readyLintCity(t, true, "worker", "work_query", base)
			configPath := filepath.Join(city, "city.toml")
			body, err := os.ReadFile(configPath)
			if err != nil {
				t.Fatal(err)
			}
			writeLintFile(t, configPath, "include = ['overlay.toml']\n"+string(body))
			writeLintFile(t, filepath.Join(city, "overlay.toml"), fmt.Sprintf("[[patches.agent]]\nname = 'worker'\nwork_query = '''%s'''\n", overlay))
			warnings := readyLintValidation(t)
			if len(warnings) != want {
				t.Fatalf("advisories for effective overlay = %q, want %d", warnings, want)
			}
		})
	}
}

func TestReadyQueryLintDoesNotExecuteTheQuery(t *testing.T) {
	city := readyLintCity(t, true, "worker", "work_query", "")
	marker := filepath.Join(city, "query-executed")
	writeLintFile(t, filepath.Join(city, "agents", "worker", "agent.toml"), fmt.Sprintf("max_active_sessions = 2\nwork_query = '''touch %q; gc bd ready --json'''\n", marker))
	readyLintValidation(t)
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("advisory validation executed the custom query: stat = %v", err)
	}
}

func TestReadyQueryLintScript(t *testing.T) {
	testscript.Run(t, newTestscriptParams(t, filepath.Join("testdata", "ready-query-lint.txtar")))
}

func readyLintCity(t *testing.T, split bool, agent, field, query string) string {
	t.Helper()
	clearGCEnv(t)
	city := t.TempDir()
	t.Chdir(city)
	t.Setenv("GC_CITY_PATH", city)
	writeLintFile(t, filepath.Join(city, ".gc", ".keep"), "")
	body := "[workspace]\n"
	if split {
		body += `
[storage.classes]
work = "work"
graph = "infra"
sessions = "infra"
messaging = "infra"
orders = "infra"
nudges = "infra"

[storage.bindings.infra]
provider = "sqlite-beads"
path = ".gc/store"
`
	}
	writeLintFile(t, filepath.Join(city, "city.toml"), body)
	writeLintFile(t, filepath.Join(city, "pack.toml"), "[pack]\nname = 'lint-city'\nschema = 2\n")
	writeLintFile(t, filepath.Join(city, "agents", agent, "agent.toml"), fmt.Sprintf("max_active_sessions = 2\n%s = '''%s'''\n", field, query))
	cfg, _, err := loadConfigCommandCityConfig(city)
	if err != nil {
		t.Fatalf("loading query fixture: %v", err)
	}
	for _, resolved := range cfg.Agents {
		if resolved.Name != agent {
			continue
		}
		got := resolved.WorkQuery
		if field == "scale_check" {
			got = resolved.ScaleCheck
		}
		if got != query {
			t.Fatalf("fixture %s = %q, want %q", field, got, query)
		}
		return city
	}
	t.Fatalf("fixture did not resolve agent %q", agent)
	return city
}

func readyLintValidation(t *testing.T) []string {
	t.Helper()
	var stdout, stderr bytes.Buffer
	if code := run([]string{"config", "show", "--validate", "--json"}, &stdout, &stderr); code != 0 {
		t.Fatalf("advisory validation changed successful exit status to %d: stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	var report struct {
		Validation struct {
			Warnings []string `json:"warnings"`
		} `json:"validation"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
		t.Fatalf("validation output is not JSON: %v\n%s", err, stdout.String())
	}
	return report.Validation.Warnings
}
