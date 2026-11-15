package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
)

// doltliteInitCapabilityPhrase is the one stable phrase gc-beads-bd.sh prints
// when the doltlite init arm finds no DoltLite store after bd's init: the bd on
// PATH does not speak DoltLite. Define it once; the init tests assert it.
const doltliteInitCapabilityPhrase = "doltlite store not created"

// fakeDoltliteBd models a bd for the doltlite init arm. Every invocation is
// appended to $FAKE_BD_LOG as one line: argv, whether metadata.json carried the
// doltlite label at that moment, and the two shared-server pin variables.
//
//	FAKE_BD_STORE=1        init creates .beads/doltlite/<database>.db (a capable bd)
//	FAKE_BD_REOPEN_FAIL=1  config get issue_prefix answers nothing once the
//	                       doltlite label is on disk
//
// A stock bd (bd 1.1.0 .. 1.3.x) is the default: init succeeds, creates an
// embedded store, creates no DoltLite file, and answers config get
// issue_prefix over that embedded store.
const fakeDoltliteBd = `#!/bin/sh
meta="${BEADS_DIR:-.beads}/metadata.json"
label=no
if [ -f "$meta" ] && grep -q '"backend": "doltlite"' "$meta"; then label=yes; fi
printf '%s | label=%s | BEADS_DOLT_SHARED_SERVER=%s BD_DOLT_SHARED_SERVER=%s\n' \
  "$*" "$label" "${BEADS_DOLT_SHARED_SERVER-unset}" "${BD_DOLT_SHARED_SERVER-unset}" >> "$FAKE_BD_LOG"
case "${1:-}" in
  version|--version)
    echo "bd version 1.1.0 (fake)"
    exit 0
    ;;
  init)
    prefix=""
    database=""
    shift
    while [ $# -gt 0 ]; do
      case "$1" in
        -p) prefix="$2"; shift ;;
        --database) database="$2"; shift ;;
      esac
      shift
    done
    mkdir -p "${BEADS_DIR:-.beads}/embeddeddolt/$database"
    printf '%s' "$prefix" > "$FAKE_BD_STATE"
    if [ "${FAKE_BD_STORE:-}" = 1 ]; then
      mkdir -p "${BEADS_DIR:-.beads}/doltlite"
      : > "${BEADS_DIR:-.beads}/doltlite/$database.db"
    fi
    exit 0
    ;;
  config)
    if [ "${2:-}" = get ] && [ "${3:-}" = issue_prefix ]; then
      if [ "${FAKE_BD_REOPEN_FAIL:-}" = 1 ] && [ "$label" = yes ]; then
        exit 1
      fi
      if [ -f "$FAKE_BD_STATE" ]; then
        cat "$FAKE_BD_STATE"
        echo
        exit 0
      fi
      exit 1
    fi
    exit 0
    ;;
esac
exit 0
`

// doltliteInitFixture is one scope plus a recording fake bd for an init run.
type doltliteInitFixture struct {
	cityPath string
	script   string
	binDir   string
	bdPath   string
	logFile  string
	stateFil string
}

func newDoltliteInitFixture(t *testing.T) doltliteInitFixture {
	t.Helper()
	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte("[workspace]\nname = \"demo\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	materializeBuiltinPacksForTest(t, cityPath)
	binDir := filepath.Join(t.TempDir(), "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	bdPath := filepath.Join(binDir, "bd")
	if err := os.WriteFile(bdPath, []byte(fakeDoltliteBd), 0o755); err != nil {
		t.Fatal(err)
	}
	stateDir := t.TempDir()
	return doltliteInitFixture{
		cityPath: cityPath,
		script:   gcBeadsBdScriptPath(cityPath),
		binDir:   binDir,
		bdPath:   bdPath,
		logFile:  filepath.Join(stateDir, "bd.log"),
		stateFil: filepath.Join(stateDir, "bd.state"),
	}
}

// init runs the doltlite init arm for prefix "gc", database "hq".
func (f doltliteInitFixture) init(t *testing.T, extraEnv ...string) (string, error) {
	t.Helper()
	env := append([]string{
		"GC_BEADS_BACKEND=doltlite",
		"BEADS_BACKEND=doltlite",
		"BD_NON_INTERACTIVE=1",
		"BD_BIN=" + f.bdPath,
		"FAKE_BD_LOG=" + f.logFile,
		"FAKE_BD_STATE=" + f.stateFil,
	}, extraEnv...)
	out, err := runGcBeadsBdHQInitForTest(t, f.script, f.cityPath, f.binDir, env...)
	return string(out), err
}

func (f doltliteInitFixture) metadataPath() string {
	return filepath.Join(f.cityPath, ".beads", "metadata.json")
}

func (f doltliteInitFixture) bdCalls(t *testing.T) []string {
	t.Helper()
	data, err := os.ReadFile(f.logFile)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatal(err)
	}
	return strings.Split(strings.TrimRight(string(data), "\n"), "\n")
}

// AC-B1: a bd whose init exits 0 and creates no DoltLite store (every stock bd)
// must not get its scope labeled doltlite.
func TestGcBeadsBdInitDoltliteFailsClosedWhenBdCreatesNoDoltliteStore(t *testing.T) {
	f := newDoltliteInitFixture(t)
	out, err := f.init(t)
	if err == nil {
		t.Fatalf("doltlite init succeeded although bd created no DoltLite store:\n%s", out)
	}
	if !strings.Contains(out, doltliteInitCapabilityPhrase) {
		t.Errorf("init output lacks %q:\n%s", doltliteInitCapabilityPhrase, out)
	}
	if want := filepath.Join(".beads", "doltlite", "hq.db"); !strings.Contains(out, want) {
		t.Errorf("init output does not name %s:\n%s", want, out)
	}
	if !strings.Contains(out, "bd version 1.1.0 (fake)") {
		t.Errorf("init output does not carry bd's version line:\n%s", out)
	}
	err = filepath.WalkDir(f.cityPath, func(p string, d os.DirEntry, walkErr error) error {
		if walkErr != nil || d.IsDir() {
			return walkErr
		}
		data, readErr := os.ReadFile(p)
		if readErr != nil {
			return readErr
		}
		if strings.Contains(string(data), `"backend": "doltlite"`) {
			t.Errorf("%s carries the doltlite label although no DoltLite store exists:\n%s", p, data)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// AC-B1 (already_ready branch): the prefix probe alone proves nothing, because
// a stock bd answers config get issue_prefix over its embedded store.
func TestGcBeadsBdInitDoltliteFailsClosedWhenPrefixIsReadyButStoreIsMissing(t *testing.T) {
	f := newDoltliteInitFixture(t)
	if err := os.WriteFile(f.stateFil, []byte("gc"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := f.init(t)
	if err == nil {
		t.Fatalf("doltlite init succeeded although the store file is missing:\n%s", out)
	}
	if !strings.Contains(out, doltliteInitCapabilityPhrase) {
		t.Errorf("init output lacks %q:\n%s", doltliteInitCapabilityPhrase, out)
	}
	if _, statErr := os.Stat(f.metadataPath()); statErr == nil {
		data, _ := os.ReadFile(f.metadataPath())
		t.Errorf("metadata.json was written although no DoltLite store exists:\n%s", data)
	}
}

// AC-B2: a capable bd creates the store; the arm labels the scope and then
// reopens it through bd.
func TestGcBeadsBdInitDoltliteLabelsAndReopensWhenStoreExists(t *testing.T) {
	f := newDoltliteInitFixture(t)
	out, err := f.init(t, "FAKE_BD_STORE=1")
	if err != nil {
		t.Fatalf("doltlite init failed: %v\n%s", err, out)
	}
	data, readErr := os.ReadFile(f.metadataPath())
	if readErr != nil {
		t.Fatalf("read metadata: %v", readErr)
	}
	for _, want := range []string{`"backend": "doltlite"`, `"database": "doltlite"`, `"dolt_database": "hq"`} {
		if !strings.Contains(string(data), want) {
			t.Errorf("metadata missing %q:\n%s", want, data)
		}
	}
	reopened := false
	for _, call := range f.bdCalls(t) {
		if strings.HasPrefix(call, "config get issue_prefix |") && strings.Contains(call, "label=yes") {
			reopened = true
		}
	}
	if !reopened {
		t.Errorf("no bd config get issue_prefix ran after the label write; calls:\n%s", strings.Join(f.bdCalls(t), "\n"))
	}
}

// AC-B3: the label write is undone when bd cannot reopen the labeled scope.
func TestGcBeadsBdInitDoltliteRestoresMetadataWhenReopenFails(t *testing.T) {
	for _, tc := range []struct {
		name  string
		prior string // "" means metadata.json is absent before the arm writes
	}{
		{name: "absent", prior: ""},
		{name: "present", prior: `{"database":"dolt","backend":"dolt","dolt_database":"hq"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newDoltliteInitFixture(t)
			if tc.prior != "" {
				if err := os.MkdirAll(filepath.Dir(f.metadataPath()), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(f.metadataPath(), []byte(tc.prior), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			out, err := f.init(t, "FAKE_BD_STORE=1", "FAKE_BD_REOPEN_FAIL=1")
			if err == nil {
				t.Fatalf("doltlite init succeeded although the labeled scope cannot be reopened:\n%s", out)
			}
			got, readErr := os.ReadFile(f.metadataPath())
			switch {
			case tc.prior == "" && readErr == nil:
				t.Errorf("metadata.json must be absent after a failed reopen, got:\n%s", got)
			case tc.prior == "" && !os.IsNotExist(readErr):
				t.Fatal(readErr)
			case tc.prior != "" && readErr != nil:
				t.Fatalf("metadata.json must survive a failed reopen: %v", readErr)
			case tc.prior != "" && string(got) != tc.prior:
				t.Errorf("metadata.json = %q after a failed reopen, want the pre-write bytes %q", got, tc.prior)
			}
			leftovers, globErr := filepath.Glob(filepath.Join(f.cityPath, ".beads", "metadata.json.tmp.*"))
			if globErr != nil {
				t.Fatal(globErr)
			}
			if len(leftovers) != 0 {
				t.Errorf("temp files left behind: %v", leftovers)
			}
		})
	}
}

// AC-B4: every bd call the doltlite arm makes runs with shared-server mode
// pinned off, whatever the caller's environment says.
func TestGcBeadsBdInitDoltlitePinsSharedServerOffOnEveryBdCall(t *testing.T) {
	f := newDoltliteInitFixture(t)
	out, err := f.init(t, "FAKE_BD_STORE=1", "BEADS_DOLT_SHARED_SERVER=1", "BD_DOLT_SHARED_SERVER=true")
	if err != nil {
		t.Fatalf("doltlite init failed: %v\n%s", err, out)
	}
	calls := f.bdCalls(t)
	if len(calls) == 0 {
		t.Fatal("fake bd recorded no calls")
	}
	for _, call := range calls {
		if !strings.HasSuffix(call, "BEADS_DOLT_SHARED_SERVER=0 BD_DOLT_SHARED_SERVER=false") {
			t.Errorf("bd call ran without the shared-server pin: %s", call)
		}
	}
}

// AC-B5: the four doltlite sites in bd_env.go project the shared-server pin
// pair, even when the parent environment turns shared-server mode on. A dolt
// scope's projection stays free of it.
func TestDoltliteBdEnvProjectionPinsSharedServerOffAtEverySite(t *testing.T) {
	writeScopeMeta := func(t *testing.T, dir, meta string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Join(dir, ".beads"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, ".beads", "metadata.json"), []byte(meta), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	writeScopeConfig := func(t *testing.T, dir, cfg string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, ".beads", "config.yaml"), []byte(cfg), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	newCity := func(t *testing.T, cityMeta string) string {
		t.Helper()
		cityDir := t.TempDir()
		if err := os.MkdirAll(filepath.Join(cityDir, ".gc"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte("[workspace]\nname = \"demo\"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		writeScopeMeta(t, cityDir, cityMeta)
		writeScopeConfig(t, cityDir, "issue_prefix: gc\ngc.endpoint_origin: managed_city\ngc.endpoint_status: verified\ndolt.auto-start: false\n")
		return cityDir
	}
	const inheritedRigConfig = "issue_prefix: r\ngc.endpoint_origin: inherited_city\ngc.endpoint_status: verified\ndolt.auto-start: false\n"
	const doltliteMeta = `{"backend":"doltlite","database":"doltlite","dolt_database":"hq"}`

	sites := []struct {
		name string
		env  func(t *testing.T) map[string]string
	}{
		{"applyCanonicalScopeBackendEnv inherited-city", func(t *testing.T) map[string]string {
			cityDir := newCity(t, doltliteMeta)
			rigDir := filepath.Join(cityDir, "rigs", "r")
			writeScopeMeta(t, rigDir, `{"database":"dolt","dolt_database":"r"}`)
			writeScopeConfig(t, rigDir, inheritedRigConfig)
			env := map[string]string{}
			t.Setenv("GC_BEADS_BACKEND", "doltlite")
			if _, err := applyCanonicalScopeBackendEnv(env, cityDir, rigDir); err != nil {
				t.Fatalf("applyCanonicalScopeBackendEnv: %v", err)
			}
			return env
		}},
		{"applyCanonicalScopeBackendEnv doltlite metadata", func(t *testing.T) map[string]string {
			cityDir := newCity(t, doltliteMeta)
			env := map[string]string{}
			if _, err := applyCanonicalScopeBackendEnv(env, cityDir, cityDir); err != nil {
				t.Fatalf("applyCanonicalScopeBackendEnv: %v", err)
			}
			return env
		}},
		{"bdRuntimeEnvForRigReportingContext", func(t *testing.T) map[string]string {
			cityDir := newCity(t, doltliteMeta)
			rigDir := filepath.Join(cityDir, "rigs", "r")
			writeScopeMeta(t, rigDir, doltliteMeta)
			writeScopeConfig(t, rigDir, inheritedRigConfig)
			env, _, err := bdRuntimeEnvForRigReportingContext(t.Context(), cityDir, &config.City{}, rigDir, false)
			if err != nil {
				t.Fatalf("bdRuntimeEnvForRigReportingContext: %v", err)
			}
			return env
		}},
		{"bdRuntimeEnvWithErrorRecoveryContext", func(t *testing.T) map[string]string {
			cityDir := newCity(t, doltliteMeta)
			env, err := bdRuntimeEnvWithErrorRecoveryContext(t.Context(), cityDir, false)
			if err != nil {
				t.Fatalf("bdRuntimeEnvWithErrorRecoveryContext: %v", err)
			}
			return env
		}},
	}
	for _, site := range sites {
		t.Run(site.name, func(t *testing.T) {
			t.Setenv("BEADS_DOLT_SHARED_SERVER", "1")
			t.Setenv("BD_DOLT_SHARED_SERVER", "true")
			env := site.env(t)
			if got := env["GC_BEADS_BACKEND"]; got != "doltlite" {
				t.Fatalf("GC_BEADS_BACKEND = %q, want doltlite (site did not take the doltlite arm): %v", got, env)
			}
			if got := env["BEADS_DOLT_SHARED_SERVER"]; got != "0" {
				t.Errorf("BEADS_DOLT_SHARED_SERVER = %q, want 0", got)
			}
			if got := env["BD_DOLT_SHARED_SERVER"]; got != "false" {
				t.Errorf("BD_DOLT_SHARED_SERVER = %q, want false", got)
			}
			merged := map[string]string{}
			for _, kv := range mergeRuntimeEnv(os.Environ(), env) {
				k, v, _ := strings.Cut(kv, "=")
				merged[k] = v
			}
			if merged["BEADS_DOLT_SHARED_SERVER"] != "0" || merged["BD_DOLT_SHARED_SERVER"] != "false" {
				t.Errorf("pin did not survive layering over the inherited environment: BEADS_DOLT_SHARED_SERVER=%q BD_DOLT_SHARED_SERVER=%q",
					merged["BEADS_DOLT_SHARED_SERVER"], merged["BD_DOLT_SHARED_SERVER"])
			}
		})
	}

	t.Run("dolt scope projection carries no pin", func(t *testing.T) {
		cityDir := newCity(t, `{"backend":"dolt","database":"dolt","dolt_database":"hq"}`)
		writeScopeConfig(t, cityDir, "issue_prefix: gc\ngc.endpoint_origin: city_canonical\ngc.endpoint_status: verified\ndolt.host: db.example.internal\ndolt.port: 3307\ndolt.auto-start: false\n")
		t.Setenv("BEADS_DOLT_SHARED_SERVER", "1")
		env := map[string]string{}
		if _, err := applyCanonicalScopeBackendEnv(env, cityDir, cityDir); err != nil {
			t.Fatalf("applyCanonicalScopeBackendEnv: %v", err)
		}
		for _, key := range []string{"BEADS_DOLT_SHARED_SERVER", "BD_DOLT_SHARED_SERVER"} {
			if v, ok := env[key]; ok {
				t.Errorf("dolt projection sets %s=%q; the pin belongs to doltlite and proxied scopes only", key, v)
			}
		}
	})
}
