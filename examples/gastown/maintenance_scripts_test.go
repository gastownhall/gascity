package gastown_test

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

var rawDoltSQLCallRe = regexp.MustCompile(`(?m)(^|[^A-Za-z0-9_-])dolt(?:[ \t]+|[ \t]*\\[ \t]*\r?\n[ \t]*)+sql([ \t]|$)`)

func TestMaintenanceDoltScriptsUseProjectedConnectionTarget(t *testing.T) {
	tests := []struct {
		name   string
		script string
		env    map[string]string
	}{
		{
			name:   "reaper",
			script: filepath.Join("packs", "maintenance", "assets", "scripts", "reaper.sh"),
			env: map[string]string{
				"GC_REAPER_DRY_RUN": "1",
			},
		},
		{
			name:   "jsonl export",
			script: filepath.Join("packs", "maintenance", "assets", "scripts", "jsonl-export.sh"),
			env: map[string]string{
				"GC_JSONL_ARCHIVE_REPO":      "archive",
				"GC_JSONL_MAX_PUSH_FAILURES": "99",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cityDir := t.TempDir()
			binDir := t.TempDir()
			stateDir := t.TempDir()
			doltLog := filepath.Join(t.TempDir(), "dolt-args.log")
			gcLog := filepath.Join(t.TempDir(), "gc.log")

			writeMaintenanceDoltStub(t, filepath.Join(binDir, "dolt"))
			writeExecutable(t, filepath.Join(binDir, "gc"), `#!/bin/sh
printf '%s\n' "$*" >> "$GC_CALL_LOG"
exit 0
`)

			env := map[string]string{
				"DOLT_ARGS_LOG":       doltLog,
				"GC_CALL_LOG":         gcLog,
				"GC_CITY":             cityDir,
				"GC_CITY_PATH":        cityDir,
				"GC_PACK_STATE_DIR":   stateDir,
				"GC_DOLT_HOST":        "external.example.internal",
				"GC_DOLT_PORT":        "4406",
				"GC_DOLT_USER":        "maintenance-user",
				"GC_DOLT_PASSWORD":    "secret-password",
				"GIT_CONFIG_GLOBAL":   filepath.Join(t.TempDir(), "gitconfig"),
				"GIT_CONFIG_NOSYSTEM": "1",
				"PATH":                binDir + string(os.PathListSeparator) + os.Getenv("PATH"),
			}
			for key, value := range tt.env {
				if key == "GC_JSONL_ARCHIVE_REPO" {
					value = filepath.Join(cityDir, value)
				}
				env[key] = value
			}

			runScript(t, filepath.Join(exampleDir(), tt.script), env)

			logData, err := os.ReadFile(doltLog)
			if err != nil {
				t.Fatalf("ReadFile(dolt log): %v", err)
			}
			log := string(logData)
			for _, want := range []string{
				"--host external.example.internal",
				"--port 4406",
				"--user maintenance-user",
				"--no-tls",
			} {
				if !strings.Contains(log, want) {
					t.Fatalf("dolt calls missing %q:\n%s", want, log)
				}
			}
			if strings.Contains(log, "secret-password") {
				t.Fatalf("dolt password leaked into argv log:\n%s", log)
			}
		})
	}
}

func TestOrphanSweepPreservesQualifiedRigAssignees(t *testing.T) {
	cityDir := t.TempDir()
	binDir := t.TempDir()
	gcLog := filepath.Join(t.TempDir(), "gc.log")

	writeExecutable(t, filepath.Join(binDir, "gc"), `#!/bin/sh
printf '%s\n' "$*" >> "$GC_CALL_LOG"
case "$1" in
  config)
    if [ "$2" = "explain" ]; then
      cat <<'EOF'
Agent: gastown.deacon
  source: pack
Agent: project/gastown.refinery
  source: pack
Agent: project/gastown.polecat
  source: pack
EOF
      exit 0
    fi
    ;;
  rig)
    if [ "$2" = "list" ] && [ "$3" = "--json" ]; then
      printf '{"rigs":[{"name":"hq","hq":true},{"name":"project","hq":false}]}\n'
      exit 0
    fi
    ;;
  bd)
    if [ "$2" = "list" ]; then
      case "$*" in
        *"--rig project"*)
          cat <<'EOF'
[
  {"id":"ga-valid","status":"in_progress","assignee":"project/gastown.refinery"},
  {"id":"ga-pool","status":"in_progress","assignee":"project/gastown.polecat-3"},
  {"id":"ga-orphan","status":"in_progress","assignee":"project/gastown.missing"}
]
EOF
          ;;
        *)
          printf '[]\n'
          ;;
      esac
      exit 0
    fi
    if [ "$2" = "update" ]; then
      exit 0
    fi
    ;;
esac
exit 1
`)

	env := map[string]string{
		"GC_CITY":      cityDir,
		"GC_CITY_PATH": cityDir,
		"GC_CALL_LOG":  gcLog,
		"PATH":         binDir + string(os.PathListSeparator) + os.Getenv("PATH"),
	}

	script := filepath.Join(exampleDir(), "packs", "maintenance", "assets", "scripts", "orphan-sweep.sh")
	cmd := exec.Command(script)
	cmd.Env = mergeTestEnv(env)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s failed: %v\n%s", filepath.Base(script), err, out)
	}
	if !strings.Contains(string(out), "orphan-sweep: reset 1 orphaned beads") {
		t.Fatalf("unexpected orphan-sweep output:\n%s", out)
	}

	logData, err := os.ReadFile(gcLog)
	if err != nil {
		t.Fatalf("ReadFile(gc log): %v", err)
	}
	log := string(logData)
	if !strings.Contains(log, "bd update ga-orphan --status=open --assignee=") {
		t.Fatalf("orphan bead was not reset:\n%s", log)
	}
	for _, preserved := range []string{"ga-valid", "ga-pool"} {
		if strings.Contains(log, "bd update "+preserved+" ") {
			t.Fatalf("valid assignee %s was reset:\n%s", preserved, log)
		}
	}
}

func TestMaintenanceDoltScriptsFallbackToManagedRuntimePorts(t *testing.T) {
	scripts := []struct {
		name   string
		script string
		env    map[string]string
	}{
		{
			name:   "reaper",
			script: filepath.Join("packs", "maintenance", "assets", "scripts", "reaper.sh"),
			env: map[string]string{
				"GC_REAPER_DRY_RUN": "1",
			},
		},
		{
			name:   "jsonl export",
			script: filepath.Join("packs", "maintenance", "assets", "scripts", "jsonl-export.sh"),
			env: map[string]string{
				"GC_JSONL_ARCHIVE_REPO":      "archive",
				"GC_JSONL_MAX_PUSH_FAILURES": "99",
			},
		},
	}

	fallbacks := []struct {
		name  string
		setup func(t *testing.T, cityDir string) string
	}{
		{
			name: "compatibility port mirror ignored without managed runtime state",
			setup: func(t *testing.T, cityDir string) string {
				t.Helper()
				if err := os.MkdirAll(filepath.Join(cityDir, ".beads"), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(cityDir, ".beads", "dolt-server.port"), []byte("45781\n"), 0o644); err != nil {
					t.Fatal(err)
				}
				return "3307"
			},
		},
		{
			name: "managed runtime state",
			setup: func(t *testing.T, cityDir string) string {
				t.Helper()
				listener := listenManagedDoltPort(t)
				port := listener.Addr().(*net.TCPAddr).Port
				writeManagedRuntimeState(t, cityDir, port)
				return strconv.Itoa(port)
			},
		},
		{
			name: "managed state beats compatibility port mirror",
			setup: func(t *testing.T, cityDir string) string {
				t.Helper()
				if err := os.MkdirAll(filepath.Join(cityDir, ".beads"), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(cityDir, ".beads", "dolt-server.port"), []byte("1111\n"), 0o644); err != nil {
					t.Fatal(err)
				}
				listener := listenManagedDoltPort(t)
				port := listener.Addr().(*net.TCPAddr).Port
				writeManagedRuntimeState(t, cityDir, port)
				return strconv.Itoa(port)
			},
		},
		{
			name: "corrupt managed state ignores compatibility port mirror",
			setup: func(t *testing.T, cityDir string) string {
				t.Helper()
				stateDir := filepath.Join(cityDir, ".gc", "runtime", "packs", "dolt")
				if err := os.MkdirAll(stateDir, 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(stateDir, "dolt-state.json"), []byte(`not-json`), 0o644); err != nil {
					t.Fatal(err)
				}
				if err := os.MkdirAll(filepath.Join(cityDir, ".beads"), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(cityDir, ".beads", "dolt-server.port"), []byte("45785\n"), 0o644); err != nil {
					t.Fatal(err)
				}
				return "3307"
			},
		},
	}

	for _, tt := range scripts {
		for _, fb := range fallbacks {
			t.Run(tt.name+"/"+fb.name, func(t *testing.T) {
				cityDir := t.TempDir()
				binDir := t.TempDir()
				stateDir := t.TempDir()
				doltLog := filepath.Join(t.TempDir(), "dolt-args.log")
				wantPort := fb.setup(t, cityDir)

				writeMaintenanceDoltStub(t, filepath.Join(binDir, "dolt"))
				writeExecutable(t, filepath.Join(binDir, "gc"), `#!/bin/sh
exit 0
`)

				env := map[string]string{
					"DOLT_ARGS_LOG":       doltLog,
					"GC_CITY":             cityDir,
					"GC_CITY_PATH":        cityDir,
					"GC_PACK_STATE_DIR":   stateDir,
					"GC_DOLT_HOST":        "",
					"GC_DOLT_PORT":        "",
					"GC_DOLT_USER":        "",
					"GC_DOLT_PASSWORD":    "",
					"GIT_CONFIG_GLOBAL":   filepath.Join(t.TempDir(), "gitconfig"),
					"GIT_CONFIG_NOSYSTEM": "1",
					"PATH":                binDir + string(os.PathListSeparator) + os.Getenv("PATH"),
				}
				for key, value := range tt.env {
					if key == "GC_JSONL_ARCHIVE_REPO" {
						value = filepath.Join(cityDir, value)
					}
					env[key] = value
				}

				runScript(t, filepath.Join(exampleDir(), tt.script), env)

				logData, err := os.ReadFile(doltLog)
				if err != nil {
					t.Fatalf("ReadFile(dolt log): %v", err)
				}
				log := string(logData)
				for _, want := range []string{
					"--host 127.0.0.1",
					"--port " + wantPort,
					"--user root",
				} {
					if !strings.Contains(log, want) {
						t.Fatalf("dolt calls missing %q:\n%s", want, log)
					}
				}
			})
		}
	}
}

func TestMaintenanceDoltScriptsFallbackToManagedRuntimePortsWithInconclusiveLsof(t *testing.T) {
	scripts := []struct {
		name   string
		script string
		env    map[string]string
	}{
		{
			name:   "reaper",
			script: filepath.Join("packs", "maintenance", "assets", "scripts", "reaper.sh"),
			env: map[string]string{
				"GC_REAPER_DRY_RUN": "1",
			},
		},
		{
			name:   "jsonl export",
			script: filepath.Join("packs", "maintenance", "assets", "scripts", "jsonl-export.sh"),
			env: map[string]string{
				"GC_JSONL_ARCHIVE_REPO":      "archive",
				"GC_JSONL_MAX_PUSH_FAILURES": "99",
			},
		},
	}

	cases := []struct {
		name        string
		lsofBody    string
		ncBody      func(port string) string
		wantManaged bool
	}{
		{
			name:     "inconclusive lsof accepts reachable port",
			lsofBody: "#!/bin/sh\nexit 0\n",
			ncBody: func(port string) string {
				return `#!/bin/sh
host="$2"
probe_port="$3"
if [ "$1" = "-z" ] && [ "$host" = "127.0.0.1" ] && [ "$probe_port" = "` + port + `" ]; then
  exit 0
fi
exit 1
`
			},
			wantManaged: true,
		},
		{
			name:     "mismatched lsof pid still rejects port",
			lsofBody: "#!/bin/sh\necho $$\nsleep 5\n",
			ncBody: func(_ string) string {
				return `#!/bin/sh
exit 0
`
			},
			wantManaged: false,
		},
		{
			name:     "inconclusive lsof with unreachable port still rejects port",
			lsofBody: "#!/bin/sh\nexit 0\n",
			ncBody: func(_ string) string {
				return `#!/bin/sh
exit 1
`
			},
			wantManaged: false,
		},
	}

	for _, tt := range scripts {
		for _, tc := range cases {
			t.Run(tt.name+"/"+tc.name, func(t *testing.T) {
				cityDir := t.TempDir()
				binDir := t.TempDir()
				doltLog := filepath.Join(t.TempDir(), "dolt-args.log")

				listener := listenManagedDoltPort(t)
				managedPort := strconv.Itoa(listener.Addr().(*net.TCPAddr).Port)
				wantPort := "3307"
				if tc.wantManaged {
					wantPort = managedPort
				}
				writeManagedRuntimeState(t, cityDir, listener.Addr().(*net.TCPAddr).Port)

				writeMaintenanceDoltStub(t, filepath.Join(binDir, "dolt"))
				writeExecutable(t, filepath.Join(binDir, "gc"), `#!/bin/sh
exit 0
`)
				writeExecutable(t, filepath.Join(binDir, "lsof"), tc.lsofBody)
				writeExecutable(t, filepath.Join(binDir, "nc"), tc.ncBody(managedPort))

				env := map[string]string{
					"DOLT_ARGS_LOG":       doltLog,
					"GC_CITY":             cityDir,
					"GC_CITY_PATH":        cityDir,
					"GC_DOLT_HOST":        "",
					"GC_DOLT_PORT":        "",
					"GC_DOLT_USER":        "",
					"GC_DOLT_PASSWORD":    "",
					"GIT_CONFIG_GLOBAL":   filepath.Join(t.TempDir(), "gitconfig"),
					"GIT_CONFIG_NOSYSTEM": "1",
					"PATH":                binDir + string(os.PathListSeparator) + os.Getenv("PATH"),
				}
				for key, value := range tt.env {
					if key == "GC_JSONL_ARCHIVE_REPO" {
						value = filepath.Join(cityDir, value)
					}
					env[key] = value
				}

				runScript(t, filepath.Join(exampleDir(), tt.script), env)

				logData, err := os.ReadFile(doltLog)
				if err != nil {
					t.Fatalf("ReadFile(dolt log): %v", err)
				}
				log := string(logData)
				for _, want := range []string{
					"--host 127.0.0.1",
					"--port " + wantPort,
					"--user root",
				} {
					if !strings.Contains(log, want) {
						t.Fatalf("dolt calls missing %q:\n%s", want, log)
					}
				}
			})
		}
	}
}

func TestMaintenanceDoltScriptsUsePsConfirmedManagedRuntimePorts(t *testing.T) {
	scripts := []struct {
		name   string
		script string
		env    map[string]string
	}{
		{
			name:   "reaper",
			script: filepath.Join("packs", "maintenance", "assets", "scripts", "reaper.sh"),
			env: map[string]string{
				"GC_REAPER_DRY_RUN": "1",
			},
		},
		{
			name:   "jsonl export",
			script: filepath.Join("packs", "maintenance", "assets", "scripts", "jsonl-export.sh"),
			env: map[string]string{
				"GC_JSONL_ARCHIVE_REPO":      "archive",
				"GC_JSONL_MAX_PUSH_FAILURES": "99",
			},
		},
	}

	cases := []struct {
		name     string
		lsofBody string
		ncBody   func(port string) string
	}{
		{
			name:     "listener pid match via ps fallback",
			lsofBody: "#!/bin/sh\necho 424242\n",
			ncBody: func(_ string) string {
				return `#!/bin/sh
exit 1
`
			},
		},
		{
			name:     "reachable port via ps fallback when lsof is inconclusive",
			lsofBody: "#!/bin/sh\nexit 0\n",
			ncBody: func(port string) string {
				return `#!/bin/sh
host="$2"
probe_port="$3"
if [ "$1" = "-z" ] && [ "$host" = "127.0.0.1" ] && [ "$probe_port" = "` + port + `" ]; then
  exit 0
fi
exit 1
`
			},
		},
	}

	for _, tt := range scripts {
		for _, tc := range cases {
			t.Run(tt.name+"/"+tc.name, func(t *testing.T) {
				cityDir := t.TempDir()
				binDir := t.TempDir()
				doltLog := filepath.Join(t.TempDir(), "dolt-args.log")

				listener := listenManagedDoltPort(t)
				managedPort := strconv.Itoa(listener.Addr().(*net.TCPAddr).Port)
				writeManagedRuntimeStateWithPID(t, cityDir, listener.Addr().(*net.TCPAddr).Port, 424242)

				writeMaintenanceDoltStub(t, filepath.Join(binDir, "dolt"))
				writeExecutable(t, filepath.Join(binDir, "gc"), `#!/bin/sh
exit 0
`)
				writeExecutable(t, filepath.Join(binDir, "lsof"), tc.lsofBody)
				writeExecutable(t, filepath.Join(binDir, "nc"), tc.ncBody(managedPort))
				writeExecutable(t, filepath.Join(binDir, "ps"), `#!/bin/sh
if [ "$1" = "-p" ] && [ "$2" = "424242" ]; then
  echo " 424242"
  exit 0
fi
exit 1
`)

				env := map[string]string{
					"DOLT_ARGS_LOG":       doltLog,
					"GC_CITY":             cityDir,
					"GC_CITY_PATH":        cityDir,
					"GC_DOLT_HOST":        "",
					"GC_DOLT_PORT":        "",
					"GC_DOLT_USER":        "",
					"GC_DOLT_PASSWORD":    "",
					"GIT_CONFIG_GLOBAL":   filepath.Join(t.TempDir(), "gitconfig"),
					"GIT_CONFIG_NOSYSTEM": "1",
					"PATH":                binDir + string(os.PathListSeparator) + os.Getenv("PATH"),
				}
				for key, value := range tt.env {
					if key == "GC_JSONL_ARCHIVE_REPO" {
						value = filepath.Join(cityDir, value)
					}
					env[key] = value
				}

				runScript(t, filepath.Join(exampleDir(), tt.script), env)

				logData, err := os.ReadFile(doltLog)
				if err != nil {
					t.Fatalf("ReadFile(dolt log): %v", err)
				}
				log := string(logData)
				for _, want := range []string{
					"--host 127.0.0.1",
					"--port " + managedPort,
					"--user root",
				} {
					if !strings.Contains(log, want) {
						t.Fatalf("dolt calls missing %q:\n%s", want, log)
					}
				}
			})
		}
	}
}

func TestMaintenanceDoltScriptsParseManagedRuntimeStateWithPortableSed(t *testing.T) {
	realSed, err := exec.LookPath("sed")
	if err != nil {
		t.Fatalf("LookPath(sed): %v", err)
	}

	scripts := []struct {
		name   string
		script string
		env    map[string]string
	}{
		{
			name:   "reaper",
			script: filepath.Join("packs", "maintenance", "assets", "scripts", "reaper.sh"),
			env: map[string]string{
				"GC_REAPER_DRY_RUN": "1",
			},
		},
		{
			name:   "jsonl export",
			script: filepath.Join("packs", "maintenance", "assets", "scripts", "jsonl-export.sh"),
			env: map[string]string{
				"GC_JSONL_ARCHIVE_REPO":      "archive",
				"GC_JSONL_MAX_PUSH_FAILURES": "99",
			},
		},
	}

	for _, tt := range scripts {
		t.Run(tt.name, func(t *testing.T) {
			cityDir := t.TempDir()
			binDir := t.TempDir()
			doltLog := filepath.Join(t.TempDir(), "dolt-args.log")

			listener := listenManagedDoltPort(t)
			managedPort := strconv.Itoa(listener.Addr().(*net.TCPAddr).Port)
			writeManagedRuntimeState(t, cityDir, listener.Addr().(*net.TCPAddr).Port)

			writeMaintenanceDoltStub(t, filepath.Join(binDir, "dolt"))
			writeExecutable(t, filepath.Join(binDir, "gc"), `#!/bin/sh
exit 0
`)
			writeExecutable(t, filepath.Join(binDir, "sed"), fmt.Sprintf(`#!/bin/sh
case "$2" in
  *'\\(true\\|false\\)'*)
    exit 0
    ;;
esac
exec %q "$@"
`, realSed))

			env := map[string]string{
				"DOLT_ARGS_LOG":       doltLog,
				"GC_CITY":             cityDir,
				"GC_CITY_PATH":        cityDir,
				"GC_DOLT_HOST":        "",
				"GC_DOLT_PORT":        "",
				"GC_DOLT_USER":        "",
				"GC_DOLT_PASSWORD":    "",
				"GIT_CONFIG_GLOBAL":   filepath.Join(t.TempDir(), "gitconfig"),
				"GIT_CONFIG_NOSYSTEM": "1",
				"PATH":                binDir + string(os.PathListSeparator) + os.Getenv("PATH"),
			}
			for key, value := range tt.env {
				if key == "GC_JSONL_ARCHIVE_REPO" {
					value = filepath.Join(cityDir, value)
				}
				env[key] = value
			}

			runScript(t, filepath.Join(exampleDir(), tt.script), env)

			logData, err := os.ReadFile(doltLog)
			if err != nil {
				t.Fatalf("ReadFile(dolt log): %v", err)
			}
			log := string(logData)
			for _, want := range []string{
				"--host 127.0.0.1",
				"--port " + managedPort,
				"--user root",
			} {
				if !strings.Contains(log, want) {
					t.Fatalf("dolt calls missing %q:\n%s", want, log)
				}
			}
		})
	}
}

func TestMaintenanceDoltScriptsRejectInvalidManagedPort(t *testing.T) {
	cityDir := t.TempDir()
	binDir := t.TempDir()
	writeMaintenanceDoltStub(t, filepath.Join(binDir, "dolt"))

	env := map[string]string{
		"DOLT_ARGS_LOG":    filepath.Join(t.TempDir(), "dolt-args.log"),
		"GC_CITY":          cityDir,
		"GC_CITY_PATH":     cityDir,
		"GC_DOLT_HOST":     "",
		"GC_DOLT_PORT":     "not-a-port",
		"GC_DOLT_USER":     "",
		"GC_DOLT_PASSWORD": "",
		"PATH":             binDir + string(os.PathListSeparator) + os.Getenv("PATH"),
	}

	script := filepath.Join(exampleDir(), "packs", "maintenance", "assets", "scripts", "reaper.sh")
	cmd := exec.Command(script)
	cmd.Env = mergeTestEnv(env)
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("%s succeeded with invalid port; output:\n%s", filepath.Base(script), out)
	}
	if !strings.Contains(string(out), "invalid GC_DOLT_PORT") {
		t.Fatalf("invalid port output missing diagnostic:\n%s", out)
	}
}

func listenManagedDoltPort(t *testing.T) net.Listener {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	return listener
}

func writeManagedRuntimeState(t *testing.T, cityDir string, port int) {
	t.Helper()
	writeManagedRuntimeStateWithPID(t, cityDir, port, os.Getpid())
}

func writeManagedRuntimeStateWithPID(t *testing.T, cityDir string, port int, pid int) {
	t.Helper()
	stateDir := filepath.Join(cityDir, ".gc", "runtime", "packs", "dolt")
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(map[string]any{
		"running":    true,
		"pid":        pid,
		"port":       port,
		"data_dir":   filepath.Join(cityDir, ".beads", "dolt"),
		"started_at": "2026-04-20T00:00:00Z",
	})
	if err != nil {
		t.Fatalf("Marshal(managed runtime state): %v", err)
	}
	if err := os.WriteFile(filepath.Join(stateDir, "dolt-state.json"), payload, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestFormulaDoltSQLExamplesUseExplicitTarget(t *testing.T) {
	examplesDir := filepath.Dir(exampleDir())
	paths := []string{
		filepath.Join(examplesDir, "dolt", "formulas", "mol-dog-doctor.toml"),
		filepath.Join(exampleDir(), "packs", "maintenance", "formulas", "mol-dog-jsonl.toml"),
	}
	for _, path := range paths {
		t.Run(filepath.Base(path), func(t *testing.T) {
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("ReadFile(%s): %v", path, err)
			}
			if match := rawDoltSQLCallRe.Find(data); match != nil {
				t.Fatalf("formula contains unqualified Dolt SQL command %q; include host, port, user, and no-tls args", match)
			}
		})
	}
}

func TestSpawnStormDetectPersistsNewLedgerCounts(t *testing.T) {
	cityDir := t.TempDir()
	binDir := t.TempDir()
	stateDir := t.TempDir()
	gcLog := filepath.Join(t.TempDir(), "gc.log")

	writeExecutable(t, filepath.Join(binDir, "bd"), `#!/bin/sh
case "$1" in
  list)
    printf '[{"id":"ga-loop","status":"open","metadata":{"recovered":"true"}}]\n'
    ;;
  show)
    printf '[{"id":"%s","status":"open","title":"Looping bead"}]\n' "$2"
    ;;
esac
exit 0
`)
	writeExecutable(t, filepath.Join(binDir, "gc"), `#!/bin/sh
printf '%s\n' "$*" >> "$GC_CALL_LOG"
exit 0
`)

	env := map[string]string{
		"GC_CITY":               cityDir,
		"GC_CITY_PATH":          cityDir,
		"GC_PACK_STATE_DIR":     stateDir,
		"GC_CALL_LOG":           gcLog,
		"SPAWN_STORM_THRESHOLD": "1",
		"PATH":                  binDir + string(os.PathListSeparator) + os.Getenv("PATH"),
	}

	runScript(t, filepath.Join(exampleDir(), "packs", "maintenance", "assets", "scripts", "spawn-storm-detect.sh"), env)

	ledgerData, err := os.ReadFile(filepath.Join(stateDir, "spawn-storm-counts.json"))
	if err != nil {
		t.Fatalf("ReadFile(ledger): %v", err)
	}
	var counts map[string]int
	if err := json.Unmarshal(ledgerData, &counts); err != nil {
		t.Fatalf("Unmarshal(ledger): %v\n%s", err, ledgerData)
	}
	if got := counts["ga-loop"]; got != 1 {
		t.Fatalf("ledger count for ga-loop = %d, want 1\nledger: %s", got, ledgerData)
	}

	gcData, err := os.ReadFile(gcLog)
	if err != nil {
		t.Fatalf("ReadFile(gc log): %v", err)
	}
	if !strings.Contains(string(gcData), "SPAWN_STORM: bead ga-loop reset 1x") {
		t.Fatalf("gc log missing spawn storm notification:\n%s", gcData)
	}
}

func TestSpawnStormDetectPersistsCountWhenTitleLookupFails(t *testing.T) {
	cityDir := t.TempDir()
	binDir := t.TempDir()
	stateDir := t.TempDir()
	gcLog := filepath.Join(t.TempDir(), "gc.log")

	writeExecutable(t, filepath.Join(binDir, "bd"), `#!/bin/sh
case "$1" in
  list)
    printf '[{"id":"ga-loop","status":"open","metadata":{"recovered":"true"}}]\n'
    ;;
  show)
    printf 'temporary backend failure\n' >&2
    exit 1
    ;;
esac
exit 0
`)
	writeExecutable(t, filepath.Join(binDir, "gc"), `#!/bin/sh
printf '%s\n' "$*" >> "$GC_CALL_LOG"
exit 0
`)

	env := map[string]string{
		"GC_CITY":               cityDir,
		"GC_CITY_PATH":          cityDir,
		"GC_PACK_STATE_DIR":     stateDir,
		"GC_CALL_LOG":           gcLog,
		"SPAWN_STORM_THRESHOLD": "1",
		"PATH":                  binDir + string(os.PathListSeparator) + os.Getenv("PATH"),
	}

	runScript(t, filepath.Join(exampleDir(), "packs", "maintenance", "assets", "scripts", "spawn-storm-detect.sh"), env)

	ledgerData, err := os.ReadFile(filepath.Join(stateDir, "spawn-storm-counts.json"))
	if err != nil {
		t.Fatalf("ReadFile(ledger): %v", err)
	}
	var counts map[string]int
	if err := json.Unmarshal(ledgerData, &counts); err != nil {
		t.Fatalf("Unmarshal(ledger): %v\n%s", err, ledgerData)
	}
	if got := counts["ga-loop"]; got != 1 {
		t.Fatalf("ledger count for ga-loop = %d, want 1\nledger: %s", got, ledgerData)
	}

	gcData, err := os.ReadFile(gcLog)
	if err != nil {
		t.Fatalf("ReadFile(gc log): %v", err)
	}
	if !strings.Contains(string(gcData), "SPAWN_STORM: bead ga-loop reset 1x") {
		t.Fatalf("gc log missing spawn storm notification:\n%s", gcData)
	}
}

func TestSpawnStormDetectFailsOnMalformedOpenBeadJSON(t *testing.T) {
	cityDir := t.TempDir()
	binDir := t.TempDir()
	stateDir := t.TempDir()
	ledger := filepath.Join(stateDir, "spawn-storm-counts.json")
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(ledger, []byte(`{"ga-existing":2}`), 0o644); err != nil {
		t.Fatal(err)
	}

	writeExecutable(t, filepath.Join(binDir, "bd"), `#!/bin/sh
case "$1" in
  list)
    printf 'not-json\n'
    ;;
  show)
    printf '[{"id":"%s","status":"open","title":"Looping bead"}]\n' "$2"
    ;;
esac
exit 0
`)
	writeExecutable(t, filepath.Join(binDir, "gc"), `#!/bin/sh
exit 0
`)

	env := map[string]string{
		"GC_CITY":           cityDir,
		"GC_CITY_PATH":      cityDir,
		"GC_PACK_STATE_DIR": stateDir,
		"PATH":              binDir + string(os.PathListSeparator) + os.Getenv("PATH"),
	}

	script := filepath.Join(exampleDir(), "packs", "maintenance", "assets", "scripts", "spawn-storm-detect.sh")
	cmd := exec.Command(script)
	cmd.Env = mergeTestEnv(env)
	if out, err := cmd.CombinedOutput(); err == nil {
		t.Fatalf("%s succeeded with malformed bd JSON; output:\n%s", filepath.Base(script), out)
	}

	ledgerData, err := os.ReadFile(ledger)
	if err != nil {
		t.Fatalf("ReadFile(ledger): %v", err)
	}
	if got, want := string(ledgerData), `{"ga-existing":2}`; got != want {
		t.Fatalf("ledger changed after malformed JSON: got %s, want %s", got, want)
	}
}

func TestSpawnStormDetectPrunesClosedAndDeletedLedgerEntries(t *testing.T) {
	cityDir := t.TempDir()
	binDir := t.TempDir()
	stateDir := t.TempDir()
	ledger := filepath.Join(stateDir, "spawn-storm-counts.json")
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(ledger, []byte(`{"ga-closed":2,"ga-deleted":3,"ga-open":4}`), 0o644); err != nil {
		t.Fatal(err)
	}

	writeExecutable(t, filepath.Join(binDir, "bd"), `#!/bin/sh
case "$1" in
  list)
    printf '[{"id":"ga-loop","status":"open","metadata":{"recovered":"true"}}]\n'
    ;;
  show)
    case "$2" in
      ga-closed)
        printf '[{"id":"ga-closed","status":"closed","title":"Closed bead"}]\n'
        ;;
      ga-open|ga-loop)
        printf '[{"id":"%s","status":"open","title":"Open bead"}]\n' "$2"
        ;;
      ga-deleted)
        printf 'issue not found\n' >&2
        exit 1
        ;;
    esac
    ;;
esac
exit 0
`)
	writeExecutable(t, filepath.Join(binDir, "gc"), `#!/bin/sh
exit 0
`)

	env := map[string]string{
		"GC_CITY":               cityDir,
		"GC_CITY_PATH":          cityDir,
		"GC_PACK_STATE_DIR":     stateDir,
		"SPAWN_STORM_THRESHOLD": "99",
		"PATH":                  binDir + string(os.PathListSeparator) + os.Getenv("PATH"),
	}

	runScript(t, filepath.Join(exampleDir(), "packs", "maintenance", "assets", "scripts", "spawn-storm-detect.sh"), env)

	ledgerData, err := os.ReadFile(ledger)
	if err != nil {
		t.Fatalf("ReadFile(ledger): %v", err)
	}
	var counts map[string]int
	if err := json.Unmarshal(ledgerData, &counts); err != nil {
		t.Fatalf("Unmarshal(ledger): %v\n%s", err, ledgerData)
	}
	if _, ok := counts["ga-closed"]; ok {
		t.Fatalf("closed bead was not pruned: %s", ledgerData)
	}
	if _, ok := counts["ga-deleted"]; ok {
		t.Fatalf("deleted bead was not pruned: %s", ledgerData)
	}
	if got := counts["ga-open"]; got != 4 {
		t.Fatalf("open bead count = %d, want 4\nledger: %s", got, ledgerData)
	}
	if got := counts["ga-loop"]; got != 1 {
		t.Fatalf("new loop count = %d, want 1\nledger: %s", got, ledgerData)
	}
}

func TestSpawnStormDetectPreservesLedgerOnTransientShowFailure(t *testing.T) {
	cityDir := t.TempDir()
	binDir := t.TempDir()
	stateDir := t.TempDir()
	ledger := filepath.Join(stateDir, "spawn-storm-counts.json")
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(ledger, []byte(`{"ga-transient":5}`), 0o644); err != nil {
		t.Fatal(err)
	}

	writeExecutable(t, filepath.Join(binDir, "bd"), `#!/bin/sh
case "$1" in
  list)
    printf '[{"id":"ga-loop","status":"open","metadata":{"recovered":"true"}}]\n'
    ;;
  show)
    case "$2" in
      ga-transient)
        printf '{"error":"temporary backend failure"}\n'
        exit 1
        ;;
      ga-loop)
        printf '[{"id":"ga-loop","status":"open","title":"Open bead"}]\n'
        ;;
    esac
    ;;
esac
exit 0
`)
	writeExecutable(t, filepath.Join(binDir, "gc"), `#!/bin/sh
exit 0
`)

	env := map[string]string{
		"GC_CITY":               cityDir,
		"GC_CITY_PATH":          cityDir,
		"GC_PACK_STATE_DIR":     stateDir,
		"SPAWN_STORM_THRESHOLD": "99",
		"PATH":                  binDir + string(os.PathListSeparator) + os.Getenv("PATH"),
	}

	runScript(t, filepath.Join(exampleDir(), "packs", "maintenance", "assets", "scripts", "spawn-storm-detect.sh"), env)

	ledgerData, err := os.ReadFile(ledger)
	if err != nil {
		t.Fatalf("ReadFile(ledger): %v", err)
	}
	var counts map[string]int
	if err := json.Unmarshal(ledgerData, &counts); err != nil {
		t.Fatalf("Unmarshal(ledger): %v\n%s", err, ledgerData)
	}
	if got := counts["ga-transient"]; got != 5 {
		t.Fatalf("transient failure pruned or changed ledger count: got %d, want 5\nledger: %s", got, ledgerData)
	}
	if got := counts["ga-loop"]; got != 1 {
		t.Fatalf("new loop count = %d, want 1\nledger: %s", got, ledgerData)
	}
}

func runScript(t *testing.T, script string, env map[string]string) {
	t.Helper()
	cmd := exec.Command(script)
	cmd.Env = mergeTestEnv(env)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s failed: %v\n%s", filepath.Base(script), err, out)
	}
}

func writeExecutable(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatalf("WriteFile(%s): %v", path, err)
	}
}

func writeMaintenanceDoltStub(t *testing.T, path string) {
	t.Helper()
	writeExecutable(t, path, `#!/bin/sh
printf '%s\n' "$*" >> "$DOLT_ARGS_LOG"
case "$*" in
  *"SHOW DATABASES"*)
    printf 'Database\nbeads\n'
    ;;
  *"SELECT *"*)
    printf '{"id":"ga-1","title":"sample"}\n'
    ;;
  *"COUNT("*)
    printf 'COUNT(*)\n0\n'
    ;;
  *"SELECT id"*)
    printf 'id\n'
    ;;
esac
exit 0
`)
}

func mergeTestEnv(overrides map[string]string) []string {
	env := os.Environ()
	for key := range overrides {
		prefix := key + "="
		filtered := env[:0]
		for _, entry := range env {
			if !strings.HasPrefix(entry, prefix) {
				filtered = append(filtered, entry)
			}
		}
		env = filtered
	}
	keys := make([]string, 0, len(overrides))
	for key := range overrides {
		keys = append(keys, key)
	}
	for _, key := range keys {
		env = append(env, key+"="+overrides[key])
	}
	return env
}
