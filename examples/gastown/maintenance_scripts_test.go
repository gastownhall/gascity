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
	"time"

	"github.com/gastownhall/gascity/internal/beads"
)

var rawDoltSQLCallRe = regexp.MustCompile(`(?m)(^|[^A-Za-z0-9_-])dolt(?:[ \t]+|[ \t]*\\[ \t]*\r?\n[ \t]*)+sql([ \t]|$)`)

var (
	sqlFenceRe            = regexp.MustCompile("(?s)```sql\\s*\\n(.*?)```")
	mailTableRe           = regexp.MustCompile(`(?i)(?:FROM|UPDATE|INTO|JOIN|DELETE\s+FROM)\s+(?:\x60?[\w-]+\x60?\.)?\x60?mail\x60?\b`)
	rawDurationIntervalRe = regexp.MustCompile(`(?i)\bINTERVAL\s+\{\{(?:max_age|purge_age|stale_issue_age)\}\}`)
)

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

func TestOrphanSweepConfigShowFallbackPreservesQualifiedAssignees(t *testing.T) {
	cityDir := t.TempDir()
	binDir := t.TempDir()
	gcLog := filepath.Join(t.TempDir(), "gc.log")

	writeExecutable(t, filepath.Join(binDir, "gc"), `#!/bin/sh
printf '%s\n' "$*" >> "$GC_CALL_LOG"
case "$1" in
  config)
    if [ "$2" = "explain" ]; then
      exit 1
    fi
    if [ "$2" = "show" ]; then
      cat <<'EOF'
[[agent]]
  name = "deacon"
[[agent]]
  name = "polecat"
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
  {"id":"ga-valid","status":"in_progress","assignee":"gastown.deacon"},
  {"id":"ga-pool","status":"in_progress","assignee":"gastown.polecat-3"},
  {"id":"ga-orphan","status":"in_progress","assignee":"gastown.missing"}
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
	if !strings.Contains(log, "config show") {
		t.Fatalf("fallback config show path was not exercised:\n%s", log)
	}
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

func TestMaintenanceDoltScriptsSkipTestPatternDatabases(t *testing.T) {
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

	excludedDBs := []string{
		"benchdb",
		"testdb_foo",
		"beads_t1234abcd",
		"beads_t1234abcd9",
		"beads_ptbaz",
		"beads_vrqux",
		"doctest_xyz",
		"doctortest_abc",
	}
	includedDBs := []string{
		"beads",
		"customdb",
		"beads_team",
		"beads_t123",
		"beads_tABCDEF12",
		"beads_t1234abcg",
		"beads_t1234abcdx",
	}

	allDBs := append([]string{}, includedDBs...)
	allDBs = append(allDBs, excludedDBs...)

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
				"DOLT_DBS":            strings.Join(allDBs, " "),
				"GC_CALL_LOG":         gcLog,
				"GC_CITY":             cityDir,
				"GC_CITY_PATH":        cityDir,
				"GC_PACK_STATE_DIR":   stateDir,
				"GC_DOLT_HOST":        "127.0.0.1",
				"GC_DOLT_PORT":        "3307",
				"GC_DOLT_USER":        "root",
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
			for _, excluded := range excludedDBs {
				if strings.Contains(log, "`"+excluded+"`") {
					t.Errorf("dolt log references excluded test-pattern database %q:\n%s", excluded, log)
				}
			}
			for _, included := range includedDBs {
				if !strings.Contains(log, "`"+included+"`") {
					t.Errorf("dolt log missing included database %q:\n%s", included, log)
				}
			}
		})
	}
}

func TestMaintenanceDoltScriptsSkipUnsafeDatabaseIdentifiers(t *testing.T) {
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

			writeExecutable(t, filepath.Join(binDir, "dolt"), `#!/bin/sh
printf '%s\n' "$*" >> "$DOLT_ARGS_LOG"
case "$*" in
  *"SHOW TABLES FROM"*"LIKE 'wisps'"*)
    printf 'Tables_in_db\nwisps\n'
    ;;
  *"SHOW DATABASES"*)
    printf 'Database\nbeads\nfoo db\n'
    ;;
  *"COUNT("*)
    printf 'COUNT(*)\n0\n'
    ;;
  *"SELECT id"*)
    printf 'id\n'
    ;;
  *"SELECT *"*)
    printf '{"id":"ga-1"}\n'
    ;;
esac
exit 0
`)
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
				"GC_DOLT_HOST":        "127.0.0.1",
				"GC_DOLT_PORT":        "3307",
				"GC_DOLT_USER":        "root",
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
			if !strings.Contains(log, "`beads`") {
				t.Fatalf("script did not query safe database:\n%s", log)
			}
			for _, unsafe := range []string{"`foo db`", "`foo`", "`db`"} {
				if strings.Contains(log, unsafe) {
					t.Fatalf("script queried unsafe database token %s:\n%s", unsafe, log)
				}
			}
		})
	}
}

func TestReaperFormulaSQLReflectsCurrentSchema(t *testing.T) {
	path := filepath.Join(exampleDir(), "packs", "maintenance", "formulas", "mol-dog-reaper.toml")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%s): %v", path, err)
	}

	// Extract every ```sql ... ``` fence body and scan only those — prose
	// warnings about the deprecated patterns are intentional and must not
	// trip this guard.
	matches := sqlFenceRe.FindAllSubmatch(data, -1)
	if len(matches) == 0 {
		t.Fatalf("no ```sql fences found in %s; test is no-op", filepath.Base(path))
	}

	for i, m := range matches {
		fence := string(m[1])
		if strings.Contains(fence, "parent_id") {
			t.Errorf("formula sql fence %d references parent_id (column does not exist in wisps):\n%s", i, fence)
		}
		if strings.Contains(fence, "LEFT JOIN wisps parent ON") {
			t.Errorf("formula sql fence %d still has the broken parent self-join:\n%s", i, fence)
		}
		if mailTableRe.MatchString(fence) {
			t.Errorf("formula sql fence %d treats `mail` as a SQL table; mail messages are beads with Type=message:\n%s", i, fence)
		}
		if rawDurationIntervalRe.MatchString(fence) {
			t.Errorf("formula sql fence %d uses raw Go duration values in SQL INTERVAL; reaper.sh normalizes durations to integer hours:\n%s", i, fence)
		}
	}
}

func TestReaperParentIDIsParentChildDependencyProjection(t *testing.T) {
	runner := func(_, name string, args ...string) ([]byte, error) {
		call := name + " " + strings.Join(args, " ")
		switch call {
		case "bd list --json --label=parent-projection --include-infra --include-gates --limit 50":
			return []byte(`[
				{
					"id":"ga-child",
					"title":"child",
					"status":"open",
					"issue_type":"task",
					"created_at":"2026-05-06T00:00:00Z",
					"labels":["parent-projection"],
					"dependencies":[
						{"issue_id":"ga-child","depends_on_id":"ga-parent","type":"parent-child"}
					]
				}
			]`), nil
		default:
			return nil, fmt.Errorf("unexpected command: %s", call)
		}
	}
	store := beads.NewBdStore("/city", runner)

	got, err := store.List(beads.ListQuery{Label: "parent-projection", Limit: 50})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("List returned %d beads, want 1", len(got))
	}
	if got[0].ParentID != "ga-parent" {
		t.Fatalf("ParentID = %q, want dependency-projected parent ga-parent", got[0].ParentID)
	}

	scriptPath := filepath.Join(exampleDir(), "packs", "maintenance", "assets", "scripts", "reaper.sh")
	scriptData, err := os.ReadFile(scriptPath)
	if err != nil {
		t.Fatalf("ReadFile(%s): %v", scriptPath, err)
	}
	script := string(scriptData)
	if strings.Contains(script, "parent_id") {
		t.Fatalf("reaper queried parent_id directly; Dolt ParentID is projected from parent-child dependencies:\n%s", script)
	}
	if !strings.Contains(script, "dependencies d") || !strings.Contains(script, "d.type = 'parent-child'") {
		t.Fatalf("reaper does not follow the canonical Dolt parent-child projection:\n%s", script)
	}
}

func TestReaperSQLReflectsCurrentSchema(t *testing.T) {
	cityDir := t.TempDir()
	binDir := t.TempDir()
	doltLog := filepath.Join(t.TempDir(), "dolt-args.log")
	gcLog := filepath.Join(t.TempDir(), "gc.log")

	writeMaintenanceDoltStub(t, filepath.Join(binDir, "dolt"))
	writeExecutable(t, filepath.Join(binDir, "gc"), `#!/bin/sh
printf '%s\n' "$*" >> "$GC_CALL_LOG"
exit 0
`)

	env := map[string]string{
		"DOLT_ARGS_LOG":    doltLog,
		"GC_CALL_LOG":      gcLog,
		"DOLT_DBS":         "beads",
		"GC_CITY":          cityDir,
		"GC_CITY_PATH":     cityDir,
		"GC_DOLT_HOST":     "127.0.0.1",
		"GC_DOLT_PORT":     "3307",
		"GC_DOLT_USER":     "root",
		"GC_DOLT_PASSWORD": "",
		"DOLT_PURGE_COUNT": "1",
		"PATH":             binDir + string(os.PathListSeparator) + os.Getenv("PATH"),
		// No GC_REAPER_DRY_RUN — allow DOLT_COMMIT to fire.
	}

	runScript(t, filepath.Join(exampleDir(), "packs", "maintenance", "assets", "scripts", "reaper.sh"), env)

	logData, err := os.ReadFile(doltLog)
	if err != nil {
		t.Fatalf("ReadFile(dolt log): %v", err)
	}
	log := string(logData)

	// parent_id was removed: wisps schema has no such column.
	if strings.Contains(log, "parent_id") {
		t.Errorf("reaper SQL references parent_id (column does not exist in wisps):\n%s", log)
	}
	// mail was removed: not a SQL table; messages are beads with type=message.
	if strings.Contains(log, ".mail") {
		t.Errorf("reaper SQL references .mail table (does not exist in beads schema):\n%s", log)
	}
	// DOLT_COMMIT must use CALL, not SELECT.
	if strings.Contains(log, "SELECT DOLT_COMMIT") {
		t.Errorf("reaper uses SELECT DOLT_COMMIT; must use CALL DOLT_COMMIT:\n%s", log)
	}
	if !strings.Contains(log, "CALL DOLT_COMMIT") {
		t.Errorf("reaper missing CALL DOLT_COMMIT:\n%s", log)
	}
	// USE <db> must precede CALL DOLT_COMMIT so the procedure resolves.
	callIdx := strings.Index(log, "CALL DOLT_COMMIT")
	useIdx := strings.Index(log, "USE `beads`")
	if useIdx < 0 {
		t.Errorf("USE `beads` not found in dolt log:\n%s", log)
	} else if callIdx >= 0 && useIdx > callIdx {
		t.Errorf("USE `beads` appears after CALL DOLT_COMMIT:\n%s", log)
	}
	if strings.Contains(log, " mail=") || strings.Contains(log, " mail:") {
		t.Errorf("reaper still reports removed mail cleanup in Dolt commit message:\n%s", log)
	}
	purgeIdx := strings.Index(log, "DELETE FROM `beads`.wisps")
	if purgeIdx < 0 {
		t.Errorf("reaper missing closed-wisp purge delete:\n%s", log)
	} else {
		purgeSQL := log[purgeIdx:]
		if !strings.Contains(purgeSQL, "child_wisp.status IN ('open', 'hooked', 'in_progress')") ||
			!strings.Contains(purgeSQL, "d.type = 'parent-child'") ||
			!strings.Contains(purgeSQL, "d.depends_on_id IS NOT NULL") {
			t.Errorf("reaper purge can delete closed parents with non-closed children:\n%s", purgeSQL)
		}
	}

	gcData, err := os.ReadFile(gcLog)
	if err != nil {
		t.Fatalf("ReadFile(gc log): %v", err)
	}
	if strings.Contains(string(gcData), "mail:") {
		t.Errorf("reaper DOG_DONE still reports removed mail cleanup:\n%s", gcData)
	}
}

func TestReaperClosesStaleWispChainsToFixpoint(t *testing.T) {
	cityDir := t.TempDir()
	binDir := t.TempDir()
	doltLog := filepath.Join(t.TempDir(), "dolt-args.log")
	gcLog := filepath.Join(t.TempDir(), "gc.log")
	closeCountState := filepath.Join(t.TempDir(), "close-count-state")

	writeExecutable(t, filepath.Join(binDir, "dolt"), `#!/bin/sh
printf '%s\n' "$*" >> "$DOLT_ARGS_LOG"
case "$*" in
  *"SHOW TABLES FROM"*"LIKE 'wisps'"*)
    printf 'Tables_in_db\nwisps\n'
    ;;
  *"SHOW DATABASES"*)
    printf 'Database\nbeads\n'
    ;;
  *"COUNT(DISTINCT w.id)"*)
    n=0
    if [ -f "$CLOSE_COUNT_STATE" ]; then
      n=$(cat "$CLOSE_COUNT_STATE")
    fi
    case "$n" in
      0)
        printf '1\n' > "$CLOSE_COUNT_STATE"
        printf 'COUNT(*)\n1\n'
        ;;
      1)
        printf '2\n' > "$CLOSE_COUNT_STATE"
        printf 'COUNT(*)\n1\n'
        ;;
      *)
        printf 'COUNT(*)\n0\n'
        ;;
    esac
    ;;
  *"UPDATE "*"wisps SET status='closed'"*)
    printf 'ROW_COUNT()\n1\n'
    ;;
  *"SELECT COUNT(*) FROM "*"wisps"*"status IN ('open', 'hooked', 'in_progress')"*"created_at <"*)
    printf 'COUNT(*)\n2\n'
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
	writeExecutable(t, filepath.Join(binDir, "gc"), `#!/bin/sh
printf '%s\n' "$*" >> "$GC_CALL_LOG"
exit 0
`)

	env := map[string]string{
		"CLOSE_COUNT_STATE": closeCountState,
		"DOLT_ARGS_LOG":     doltLog,
		"GC_CALL_LOG":       gcLog,
		"GC_CITY":           cityDir,
		"GC_CITY_PATH":      cityDir,
		"GC_DOLT_HOST":      "127.0.0.1",
		"GC_DOLT_PORT":      "3307",
		"GC_DOLT_USER":      "root",
		"GC_DOLT_PASSWORD":  "",
		"PATH":              binDir + string(os.PathListSeparator) + os.Getenv("PATH"),
	}

	runScript(t, filepath.Join(exampleDir(), "packs", "maintenance", "assets", "scripts", "reaper.sh"), env)

	logData, err := os.ReadFile(doltLog)
	if err != nil {
		t.Fatalf("ReadFile(dolt log): %v", err)
	}
	log := string(logData)
	if got := strings.Count(log, "UPDATE `beads`.wisps SET status='closed'"); got != 2 {
		t.Fatalf("reaper closed only %d stale wisp chain level(s), want 2:\n%s", got, log)
	}
	if !strings.Contains(log, "closed_wisps=2") {
		t.Fatalf("reaper commit did not report all closed chain levels:\n%s", log)
	}

	gcData, err := os.ReadFile(gcLog)
	if err != nil {
		t.Fatalf("ReadFile(gc log): %v", err)
	}
	if !strings.Contains(string(gcData), "closed_wisps:2") {
		t.Fatalf("reaper summary did not report all closed chain levels:\n%s", gcData)
	}
}

func TestReaperCountQueriesIgnoreSuccessfulStderrWarnings(t *testing.T) {
	cityDir := t.TempDir()
	binDir := t.TempDir()
	doltLog := filepath.Join(t.TempDir(), "dolt-args.log")
	gcLog := filepath.Join(t.TempDir(), "gc.log")

	writeExecutable(t, filepath.Join(binDir, "dolt"), `#!/bin/sh
printf '%s\n' "$*" >> "$DOLT_ARGS_LOG"
case "$*" in
  *"SHOW TABLES FROM"*"LIKE 'wisps'"*)
    printf 'Tables_in_db\nwisps\n'
    ;;
  *"SHOW DATABASES"*)
    printf 'Database\nbeads\n'
    ;;
  *"DELETE FROM "*"wisps"*)
    printf 'ROW_COUNT()\n1\n'
    printf 'non-fatal mutation warning from dolt\n' >&2
    ;;
  *"status = 'closed'"*"closed_at <"*)
    printf 'COUNT(*)\n1\n'
    printf 'non-fatal warning from dolt\n' >&2
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
	writeExecutable(t, filepath.Join(binDir, "gc"), `#!/bin/sh
printf '%s\n' "$*" >> "$GC_CALL_LOG"
exit 0
`)

	env := map[string]string{
		"DOLT_ARGS_LOG":    doltLog,
		"GC_CALL_LOG":      gcLog,
		"GC_CITY":          cityDir,
		"GC_CITY_PATH":     cityDir,
		"GC_DOLT_HOST":     "127.0.0.1",
		"GC_DOLT_PORT":     "3307",
		"GC_DOLT_USER":     "root",
		"GC_DOLT_PASSWORD": "",
		"PATH":             binDir + string(os.PathListSeparator) + os.Getenv("PATH"),
	}

	runScript(t, filepath.Join(exampleDir(), "packs", "maintenance", "assets", "scripts", "reaper.sh"), env)

	doltData, err := os.ReadFile(doltLog)
	if err != nil {
		t.Fatalf("ReadFile(dolt log): %v", err)
	}
	if !strings.Contains(string(doltData), "DELETE FROM `beads`.wisps") {
		t.Fatalf("reaper did not act on count stdout when Dolt emitted stderr warning:\n%s", doltData)
	}

	gcData, err := os.ReadFile(gcLog)
	if err != nil {
		t.Fatalf("ReadFile(gc log): %v", err)
	}
	gcLogText := string(gcData)
	if strings.Contains(gcLogText, "ESCALATION") || strings.Contains(gcLogText, "count returned non-numeric") {
		t.Fatalf("reaper treated successful count stderr as an anomaly:\n%s", gcLogText)
	}
	if !strings.Contains(gcLogText, "purged:1") {
		t.Fatalf("reaper summary did not include purge count from stdout:\n%s", gcLogText)
	}
}

func TestReaperRowQueriesIgnoreSuccessfulStderrWarnings(t *testing.T) {
	cityDir := t.TempDir()
	writeCityBeadsMetadata(t, cityDir, "beads")
	binDir := t.TempDir()
	doltLog := filepath.Join(t.TempDir(), "dolt-args.log")
	bdLog := filepath.Join(t.TempDir(), "bd.log")
	gcLog := filepath.Join(t.TempDir(), "gc.log")

	writeExecutable(t, filepath.Join(binDir, "dolt"), `#!/bin/sh
printf '%s\n' "$*" >> "$DOLT_ARGS_LOG"
case "$*" in
  *"SHOW TABLES FROM"*"LIKE 'wisps'"*)
    printf 'Tables_in_db\nwisps\n'
    ;;
  *"SHOW DATABASES"*)
    printf 'Database\nbeads\n'
    ;;
  *"SELECT id FROM "*"issues"*)
    printf 'id\nga-old\n'
    printf 'non-fatal warning from dolt\n' >&2
    ;;
  *"COUNT("*)
    printf 'COUNT(*)\n0\n'
    ;;
esac
exit 0
`)
	writeExecutable(t, filepath.Join(binDir, "bd"), `#!/bin/sh
printf '%s\n' "$*" >> "$BD_CALL_LOG"
exit 0
`)
	writeExecutable(t, filepath.Join(binDir, "gc"), `#!/bin/sh
printf '%s\n' "$*" >> "$GC_CALL_LOG"
exit 0
`)

	env := map[string]string{
		"DOLT_ARGS_LOG":    doltLog,
		"BD_CALL_LOG":      bdLog,
		"GC_CALL_LOG":      gcLog,
		"GC_CITY":          cityDir,
		"GC_CITY_PATH":     cityDir,
		"GC_DOLT_HOST":     "127.0.0.1",
		"GC_DOLT_PORT":     "3307",
		"GC_DOLT_USER":     "root",
		"GC_DOLT_PASSWORD": "",
		"PATH":             binDir + string(os.PathListSeparator) + os.Getenv("PATH"),
	}

	runScript(t, filepath.Join(exampleDir(), "packs", "maintenance", "assets", "scripts", "reaper.sh"), env)

	bdData, err := os.ReadFile(bdLog)
	if err != nil {
		t.Fatalf("ReadFile(bd log): %v", err)
	}
	bdLogText := string(bdData)
	if !strings.Contains(bdLogText, "close ga-old --reason stale:auto-closed by reaper") {
		t.Fatalf("reaper did not act on row-query stdout when Dolt emitted stderr warning:\n%s", bdLogText)
	}
	if strings.Contains(bdLogText, "non-fatal warning") {
		t.Fatalf("reaper treated successful row-query stderr as an issue id:\n%s", bdLogText)
	}

	gcData, err := os.ReadFile(gcLog)
	if err != nil {
		t.Fatalf("ReadFile(gc log): %v", err)
	}
	gcLogText := string(gcData)
	if strings.Contains(gcLogText, "ESCALATION") || strings.Contains(gcLogText, "stale issue query failed") {
		t.Fatalf("reaper treated successful row-query stderr as an anomaly:\n%s", gcLogText)
	}
	if !strings.Contains(gcLogText, "closed:1") {
		t.Fatalf("reaper summary did not include city issue close from stdout:\n%s", gcLogText)
	}
}

func TestReaperDoesNotCloseNonClosedWispsByAgeOnly(t *testing.T) {
	cityDir := t.TempDir()
	binDir := t.TempDir()
	doltLog := filepath.Join(t.TempDir(), "dolt-args.log")
	gcLog := filepath.Join(t.TempDir(), "gc.log")

	writeExecutable(t, filepath.Join(binDir, "dolt"), `#!/bin/sh
printf '%s\n' "$*" >> "$DOLT_ARGS_LOG"
case "$*" in
  *"SHOW TABLES FROM"*"LIKE 'wisps'"*)
    printf 'Tables_in_db\nwisps\n'
    ;;
  *"SHOW DATABASES"*)
    printf 'Database\nbeads\n'
    ;;
  *"UPDATE "*"wisps SET status='closed'"*)
    printf 'ROW_COUNT()\n1\n'
    ;;
  *"COUNT("*"wisps w"*"dependencies d"*)
    printf 'COUNT(*)\n0\n'
    ;;
  *"status IN ('open', 'hooked', 'in_progress')"*"created_at <"*)
    printf 'COUNT(*)\n2\n'
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
	writeExecutable(t, filepath.Join(binDir, "gc"), `#!/bin/sh
printf '%s\n' "$*" >> "$GC_CALL_LOG"
exit 0
`)

	env := map[string]string{
		"DOLT_ARGS_LOG":    doltLog,
		"GC_CALL_LOG":      gcLog,
		"GC_CITY":          cityDir,
		"GC_CITY_PATH":     cityDir,
		"GC_DOLT_HOST":     "127.0.0.1",
		"GC_DOLT_PORT":     "3307",
		"GC_DOLT_USER":     "root",
		"GC_DOLT_PASSWORD": "",
		"PATH":             binDir + string(os.PathListSeparator) + os.Getenv("PATH"),
	}

	runScript(t, filepath.Join(exampleDir(), "packs", "maintenance", "assets", "scripts", "reaper.sh"), env)

	logData, err := os.ReadFile(doltLog)
	if err != nil {
		t.Fatalf("ReadFile(dolt log): %v", err)
	}
	log := string(logData)
	if strings.Contains(log, "UPDATE `beads`.wisps SET status='closed'") && !strings.Contains(log, "dependencies d") {
		t.Fatalf("reaper closed non-closed wisps by age alone instead of using parent-child dependencies:\n%s", log)
	}

	gcData, err := os.ReadFile(gcLog)
	if err != nil {
		t.Fatalf("ReadFile(gc log): %v", err)
	}
	if !strings.Contains(string(gcData), "stale_wisps:2") {
		t.Fatalf("reaper did not report observed stale non-closed wisps:\n%s", gcData)
	}
}

func TestReaperClosesStaleWispsOnlyWithClosedParent(t *testing.T) {
	cityDir := t.TempDir()
	binDir := t.TempDir()
	doltLog := filepath.Join(t.TempDir(), "dolt-args.log")
	gcLog := filepath.Join(t.TempDir(), "gc.log")
	closeCountState := filepath.Join(t.TempDir(), "close-count-state")

	writeExecutable(t, filepath.Join(binDir, "dolt"), `#!/bin/sh
printf '%s\n' "$*" >> "$DOLT_ARGS_LOG"
case "$*" in
  *"SHOW TABLES FROM"*"LIKE 'wisps'"*)
    printf 'Tables_in_db\nwisps\n'
    ;;
  *"SHOW DATABASES"*)
    printf 'Database\nbeads\n'
    ;;
  *"UPDATE "*"wisps SET status='closed'"*)
    printf 'ROW_COUNT()\n1\n'
    ;;
  *"COUNT("*"wisps w"*"dependencies d"*)
    n=0
    if [ -f "$CLOSE_COUNT_STATE" ]; then
      n=$(cat "$CLOSE_COUNT_STATE")
    fi
    if [ "$n" = "0" ]; then
      printf '1\n' > "$CLOSE_COUNT_STATE"
      printf 'COUNT(*)\n1\n'
    else
      printf 'COUNT(*)\n0\n'
    fi
    ;;
  *"status IN ('open', 'hooked', 'in_progress')"*"created_at <"*)
    printf 'COUNT(*)\n2\n'
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
	writeExecutable(t, filepath.Join(binDir, "gc"), `#!/bin/sh
printf '%s\n' "$*" >> "$GC_CALL_LOG"
exit 0
`)

	env := map[string]string{
		"CLOSE_COUNT_STATE": closeCountState,
		"DOLT_ARGS_LOG":     doltLog,
		"GC_CALL_LOG":       gcLog,
		"GC_CITY":           cityDir,
		"GC_CITY_PATH":      cityDir,
		"GC_DOLT_HOST":      "127.0.0.1",
		"GC_DOLT_PORT":      "3307",
		"GC_DOLT_USER":      "root",
		"GC_DOLT_PASSWORD":  "",
		"PATH":              binDir + string(os.PathListSeparator) + os.Getenv("PATH"),
	}

	runScript(t, filepath.Join(exampleDir(), "packs", "maintenance", "assets", "scripts", "reaper.sh"), env)

	logData, err := os.ReadFile(doltLog)
	if err != nil {
		t.Fatalf("ReadFile(dolt log): %v", err)
	}
	log := string(logData)
	if strings.Contains(log, "parent_id") {
		t.Fatalf("reaper used removed parent_id column:\n%s", log)
	}
	if !strings.Contains(log, "UPDATE `beads`.wisps SET status='closed'") {
		t.Fatalf("reaper did not close schema-safe stale wisp candidates:\n%s", log)
	}
	if !strings.Contains(log, "COUNT(DISTINCT w.id)") {
		t.Fatalf("reaper stale-wisp close count can be join-multiplied:\n%s", log)
	}
	if !strings.Contains(log, "dependencies d") || !strings.Contains(log, "d.type = 'parent-child'") {
		t.Fatalf("reaper stale-wisp close path does not use parent-child dependencies:\n%s", log)
	}
	if strings.Contains(log, "parent_wisp.id IS NULL AND parent_issue.id IS NULL") {
		t.Fatalf("reaper closes stale wisps when parent liveness is unresolved:\n%s", log)
	}
	if !strings.Contains(log, "parent_wisp.status = 'closed'") || !strings.Contains(log, "parent_issue.status = 'closed'") {
		t.Fatalf("reaper stale-wisp close path does not require a closed parent:\n%s", log)
	}

	gcData, err := os.ReadFile(gcLog)
	if err != nil {
		t.Fatalf("ReadFile(gc log): %v", err)
	}
	if !strings.Contains(string(gcData), "stale_wisps:2") || !strings.Contains(string(gcData), "closed_wisps:1") {
		t.Fatalf("reaper summary did not report observed and closed wisp counts:\n%s", gcData)
	}
}

func TestReaperEscalatesDoltCommitFailure(t *testing.T) {
	cityDir := t.TempDir()
	binDir := t.TempDir()
	doltLog := filepath.Join(t.TempDir(), "dolt-args.log")
	gcLog := filepath.Join(t.TempDir(), "gc.log")

	writeExecutable(t, filepath.Join(binDir, "dolt"), `#!/bin/sh
printf '%s\n' "$*" >> "$DOLT_ARGS_LOG"
case "$*" in
  *"SHOW TABLES FROM"*"LIKE 'wisps'"*)
    printf 'Tables_in_db\nwisps\n'
    ;;
  *"SHOW DATABASES"*)
    printf 'Database\nbeads\n'
    ;;
  *"CALL DOLT_COMMIT"*)
    printf 'commit failed\n' >&2
    exit 42
    ;;
  *"DELETE FROM "*"wisps"*)
    printf 'ROW_COUNT()\n1\n'
    ;;
  *"status = 'closed'"*"closed_at <"*)
    printf 'COUNT(*)\n1\n'
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
	writeExecutable(t, filepath.Join(binDir, "gc"), `#!/bin/sh
printf '%s\n' "$*" >> "$GC_CALL_LOG"
exit 0
`)

	env := map[string]string{
		"DOLT_ARGS_LOG":    doltLog,
		"GC_CALL_LOG":      gcLog,
		"GC_CITY":          cityDir,
		"GC_CITY_PATH":     cityDir,
		"GC_DOLT_HOST":     "127.0.0.1",
		"GC_DOLT_PORT":     "3307",
		"GC_DOLT_USER":     "root",
		"GC_DOLT_PASSWORD": "",
		"PATH":             binDir + string(os.PathListSeparator) + os.Getenv("PATH"),
	}

	runScript(t, filepath.Join(exampleDir(), "packs", "maintenance", "assets", "scripts", "reaper.sh"), env)

	logData, err := os.ReadFile(doltLog)
	if err != nil {
		t.Fatalf("ReadFile(dolt log): %v", err)
	}
	if !strings.Contains(string(logData), "CALL DOLT_COMMIT") {
		t.Fatalf("reaper did not exercise CALL DOLT_COMMIT path:\n%s", logData)
	}

	gcData, err := os.ReadFile(gcLog)
	if err != nil {
		t.Fatalf("ReadFile(gc log): %v", err)
	}
	gcLogText := string(gcData)
	if !strings.Contains(gcLogText, "mail send mayor/ -s ESCALATION: Reaper anomalies detected [MEDIUM]") {
		t.Fatalf("reaper did not escalate Dolt commit failure:\n%s", gcLogText)
	}
	if !strings.Contains(gcLogText, "Dolt commit failed for beads") {
		t.Fatalf("reaper escalation did not identify the failed database:\n%s", gcLogText)
	}
}

func TestReaperDoesNotCountFailedPurgeAsSuccess(t *testing.T) {
	cityDir := t.TempDir()
	binDir := t.TempDir()
	doltLog := filepath.Join(t.TempDir(), "dolt-args.log")
	gcLog := filepath.Join(t.TempDir(), "gc.log")

	writeExecutable(t, filepath.Join(binDir, "dolt"), `#!/bin/sh
printf '%s\n' "$*" >> "$DOLT_ARGS_LOG"
case "$*" in
  *"SHOW TABLES FROM"*"LIKE 'wisps'"*)
    printf 'Tables_in_db\nwisps\n'
    ;;
  *"SHOW DATABASES"*)
    printf 'Database\nbeads\n'
    ;;
  *"DELETE FROM "*"wisps"*)
    printf 'delete failed\n' >&2
    exit 42
    ;;
  *"status = 'closed'"*"closed_at <"*)
    printf 'COUNT(*)\n1\n'
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
	writeExecutable(t, filepath.Join(binDir, "gc"), `#!/bin/sh
printf '%s\n' "$*" >> "$GC_CALL_LOG"
exit 0
`)

	env := map[string]string{
		"DOLT_ARGS_LOG":    doltLog,
		"GC_CALL_LOG":      gcLog,
		"GC_CITY":          cityDir,
		"GC_CITY_PATH":     cityDir,
		"GC_DOLT_HOST":     "127.0.0.1",
		"GC_DOLT_PORT":     "3307",
		"GC_DOLT_USER":     "root",
		"GC_DOLT_PASSWORD": "",
		"PATH":             binDir + string(os.PathListSeparator) + os.Getenv("PATH"),
	}

	runScript(t, filepath.Join(exampleDir(), "packs", "maintenance", "assets", "scripts", "reaper.sh"), env)

	gcData, err := os.ReadFile(gcLog)
	if err != nil {
		t.Fatalf("ReadFile(gc log): %v", err)
	}
	gcLogText := string(gcData)
	if !strings.Contains(gcLogText, "purging closed wisps failed for beads") {
		t.Fatalf("reaper did not escalate failed purge:\n%s", gcLogText)
	}
	if strings.Contains(gcLogText, "purged:1") {
		t.Fatalf("reaper counted failed purge as success:\n%s", gcLogText)
	}
}

func TestReaperCommitReportsOnlySuccessfulPurgeRows(t *testing.T) {
	cityDir := t.TempDir()
	binDir := t.TempDir()
	doltLog := filepath.Join(t.TempDir(), "dolt-args.log")
	gcLog := filepath.Join(t.TempDir(), "gc.log")
	closeCountState := filepath.Join(t.TempDir(), "close-count-state")

	writeExecutable(t, filepath.Join(binDir, "dolt"), `#!/bin/sh
printf '%s\n' "$*" >> "$DOLT_ARGS_LOG"
case "$*" in
  *"SHOW TABLES FROM"*"LIKE 'wisps'"*)
    printf 'Tables_in_db\nwisps\n'
    ;;
  *"SHOW DATABASES"*)
    printf 'Database\nbeads\n'
    ;;
  *"UPDATE "*"wisps SET status='closed'"*)
    printf 'ROW_COUNT()\n1\n'
    ;;
  *"DELETE FROM "*"wisps"*)
    printf 'delete failed\n' >&2
    exit 42
    ;;
  *"COUNT("*"wisps w"*"dependencies d"*)
    n=0
    if [ -f "$CLOSE_COUNT_STATE" ]; then
      n=$(cat "$CLOSE_COUNT_STATE")
    fi
    if [ "$n" = "0" ]; then
      printf '1\n' > "$CLOSE_COUNT_STATE"
      printf 'COUNT(*)\n1\n'
    else
      printf 'COUNT(*)\n0\n'
    fi
    ;;
  *"SELECT COUNT(*) FROM "*"wisps"*"status IN ('open', 'hooked', 'in_progress')"*"created_at <"*)
    printf 'COUNT(*)\n1\n'
    ;;
  *"status = 'closed'"*"closed_at <"*)
    printf 'COUNT(*)\n1\n'
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
	writeExecutable(t, filepath.Join(binDir, "gc"), `#!/bin/sh
printf '%s\n' "$*" >> "$GC_CALL_LOG"
exit 0
`)

	env := map[string]string{
		"CLOSE_COUNT_STATE": closeCountState,
		"DOLT_ARGS_LOG":     doltLog,
		"GC_CALL_LOG":       gcLog,
		"GC_CITY":           cityDir,
		"GC_CITY_PATH":      cityDir,
		"GC_DOLT_HOST":      "127.0.0.1",
		"GC_DOLT_PORT":      "3307",
		"GC_DOLT_USER":      "root",
		"GC_DOLT_PASSWORD":  "",
		"PATH":              binDir + string(os.PathListSeparator) + os.Getenv("PATH"),
	}

	runScript(t, filepath.Join(exampleDir(), "packs", "maintenance", "assets", "scripts", "reaper.sh"), env)

	logData, err := os.ReadFile(doltLog)
	if err != nil {
		t.Fatalf("ReadFile(dolt log): %v", err)
	}
	log := string(logData)
	if !strings.Contains(log, "CALL DOLT_COMMIT") {
		t.Fatalf("reaper did not commit successful close after failed purge:\n%s", log)
	}
	if !strings.Contains(log, "closed_wisps=1 purged=0") {
		t.Fatalf("reaper commit did not report only successful purge rows:\n%s", log)
	}
	if strings.Contains(log, "purged=1") {
		t.Fatalf("reaper commit claimed failed purge rows:\n%s", log)
	}

	gcData, err := os.ReadFile(gcLog)
	if err != nil {
		t.Fatalf("ReadFile(gc log): %v", err)
	}
	if strings.Contains(string(gcData), "purged:1") {
		t.Fatalf("reaper summary claimed failed purge rows:\n%s", gcData)
	}
}

func TestReaperDoesNotCountFailedIssueCloseAsSuccess(t *testing.T) {
	cityDir := t.TempDir()
	writeCityBeadsMetadata(t, cityDir, "beads")
	binDir := t.TempDir()
	doltLog := filepath.Join(t.TempDir(), "dolt-args.log")
	gcLog := filepath.Join(t.TempDir(), "gc.log")

	writeExecutable(t, filepath.Join(binDir, "dolt"), `#!/bin/sh
printf '%s\n' "$*" >> "$DOLT_ARGS_LOG"
case "$*" in
  *"SHOW TABLES FROM"*"LIKE 'wisps'"*)
    printf 'Tables_in_db\nwisps\n'
    ;;
  *"SHOW DATABASES"*)
    printf 'Database\nbeads\n'
    ;;
  *"SELECT id FROM "*"issues"*)
    printf 'id\nga-old\n'
    ;;
  *"COUNT("*)
    printf 'COUNT(*)\n0\n'
    ;;
esac
exit 0
`)
	writeExecutable(t, filepath.Join(binDir, "bd"), `#!/bin/sh
printf '%s\n' "$*" >> "$BD_CALL_LOG"
case "$*" in
  close*)
    printf 'close failed\n' >&2
    exit 42
    ;;
esac
exit 0
`)
	writeExecutable(t, filepath.Join(binDir, "gc"), `#!/bin/sh
printf '%s\n' "$*" >> "$GC_CALL_LOG"
exit 0
`)

	env := map[string]string{
		"DOLT_ARGS_LOG":    doltLog,
		"BD_CALL_LOG":      filepath.Join(t.TempDir(), "bd.log"),
		"GC_CALL_LOG":      gcLog,
		"GC_CITY":          cityDir,
		"GC_CITY_PATH":     cityDir,
		"GC_DOLT_HOST":     "127.0.0.1",
		"GC_DOLT_PORT":     "3307",
		"GC_DOLT_USER":     "root",
		"GC_DOLT_PASSWORD": "",
		"PATH":             binDir + string(os.PathListSeparator) + os.Getenv("PATH"),
	}

	runScript(t, filepath.Join(exampleDir(), "packs", "maintenance", "assets", "scripts", "reaper.sh"), env)

	gcData, err := os.ReadFile(gcLog)
	if err != nil {
		t.Fatalf("ReadFile(gc log): %v", err)
	}
	gcLogText := string(gcData)
	if !strings.Contains(gcLogText, "closing stale issue ga-old failed for beads") {
		t.Fatalf("reaper did not escalate failed issue close:\n%s", gcLogText)
	}
	if strings.Contains(gcLogText, "closed:1") {
		t.Fatalf("reaper counted failed issue close as success:\n%s", gcLogText)
	}
}

func TestReaperAutoClosesIssuesOnlyInCityDatabase(t *testing.T) {
	cityDir := t.TempDir()
	writeCityBeadsMetadata(t, cityDir, "citydb")
	binDir := t.TempDir()
	doltLog := filepath.Join(t.TempDir(), "dolt-args.log")
	bdLog := filepath.Join(t.TempDir(), "bd.log")
	gcLog := filepath.Join(t.TempDir(), "gc.log")

	writeExecutable(t, filepath.Join(binDir, "dolt"), `#!/bin/sh
printf '%s\n' "$*" >> "$DOLT_ARGS_LOG"
case "$*" in
  *"SHOW TABLES FROM"*"LIKE 'wisps'"*)
    printf 'Tables_in_db\nwisps\n'
    ;;
  *"SHOW DATABASES"*)
    printf 'Database\ncitydb\nrigdb\n'
    ;;
  *"SELECT id FROM "*"citydb"*"issues"*)
    printf 'id\nga-city\n'
    ;;
  *"SELECT id FROM "*"rigdb"*"issues"*)
    printf 'id\nrig-old\n'
    ;;
  *"COUNT("*)
    printf 'COUNT(*)\n0\n'
    ;;
esac
exit 0
`)
	writeExecutable(t, filepath.Join(binDir, "bd"), `#!/bin/sh
printf '%s\n' "$*" >> "$BD_CALL_LOG"
exit 0
`)
	writeExecutable(t, filepath.Join(binDir, "gc"), `#!/bin/sh
printf '%s\n' "$*" >> "$GC_CALL_LOG"
exit 0
`)

	env := map[string]string{
		"DOLT_ARGS_LOG":    doltLog,
		"BD_CALL_LOG":      bdLog,
		"GC_CALL_LOG":      gcLog,
		"GC_CITY":          cityDir,
		"GC_CITY_PATH":     cityDir,
		"GC_DOLT_HOST":     "127.0.0.1",
		"GC_DOLT_PORT":     "3307",
		"GC_DOLT_USER":     "root",
		"GC_DOLT_PASSWORD": "",
		"PATH":             binDir + string(os.PathListSeparator) + os.Getenv("PATH"),
	}

	runScript(t, filepath.Join(exampleDir(), "packs", "maintenance", "assets", "scripts", "reaper.sh"), env)

	bdData, err := os.ReadFile(bdLog)
	if err != nil {
		t.Fatalf("ReadFile(bd log): %v", err)
	}
	bdLogText := string(bdData)
	if !strings.Contains(bdLogText, "close ga-city --reason stale:auto-closed by reaper") {
		t.Fatalf("reaper did not close city-scoped stale issue:\n%s", bdLogText)
	}
	if strings.Contains(bdLogText, "rig-old") {
		t.Fatalf("reaper attempted unscoped close for rig-scoped stale issue:\n%s", bdLogText)
	}

	gcData, err := os.ReadFile(gcLog)
	if err != nil {
		t.Fatalf("ReadFile(gc log): %v", err)
	}
	gcLogText := string(gcData)
	if !strings.Contains(gcLogText, "closed:1") || !strings.Contains(gcLogText, "skipped_non_city_issues:1") {
		t.Fatalf("reaper summary did not report city close and non-city skip:\n%s", gcLogText)
	}
	if strings.Contains(gcLogText, "mail send mayor/ -s ESCALATION") || strings.Contains(gcLogText, "non-city database") {
		t.Fatalf("reaper escalated expected non-city stale issue skips:\n%s", gcLogText)
	}
}

func TestReaperCityDatabaseUsesGCCityPathFallback(t *testing.T) {
	cityDir := t.TempDir()
	writeCityBeadsMetadata(t, cityDir, "citydb")
	binDir := t.TempDir()
	doltLog := filepath.Join(t.TempDir(), "dolt-args.log")
	bdLog := filepath.Join(t.TempDir(), "bd.log")
	gcLog := filepath.Join(t.TempDir(), "gc.log")

	writeExecutable(t, filepath.Join(binDir, "dolt"), `#!/bin/sh
printf '%s\n' "$*" >> "$DOLT_ARGS_LOG"
case "$*" in
  *"SHOW TABLES FROM"*"LIKE 'wisps'"*)
    printf 'Tables_in_db\nwisps\n'
    ;;
  *"SHOW DATABASES"*)
    printf 'Database\ncitydb\n'
    ;;
  *"SELECT id FROM "*"citydb"*"issues"*)
    printf 'id\nga-city\n'
    ;;
  *"COUNT("*)
    printf 'COUNT(*)\n0\n'
    ;;
esac
exit 0
`)
	writeExecutable(t, filepath.Join(binDir, "bd"), `#!/bin/sh
printf '%s\n' "$*" >> "$BD_CALL_LOG"
exit 0
`)
	writeExecutable(t, filepath.Join(binDir, "gc"), `#!/bin/sh
printf '%s\n' "$*" >> "$GC_CALL_LOG"
exit 0
`)

	env := map[string]string{
		"DOLT_ARGS_LOG":    doltLog,
		"BD_CALL_LOG":      bdLog,
		"GC_CALL_LOG":      gcLog,
		"GC_CITY":          "",
		"GC_CITY_PATH":     cityDir,
		"GC_DOLT_HOST":     "127.0.0.1",
		"GC_DOLT_PORT":     "3307",
		"GC_DOLT_USER":     "root",
		"GC_DOLT_PASSWORD": "",
		"PATH":             binDir + string(os.PathListSeparator) + os.Getenv("PATH"),
	}

	runScript(t, filepath.Join(exampleDir(), "packs", "maintenance", "assets", "scripts", "reaper.sh"), env)

	bdData, err := os.ReadFile(bdLog)
	if err != nil {
		t.Fatalf("ReadFile(bd log): %v", err)
	}
	if !strings.Contains(string(bdData), "close ga-city --reason stale:auto-closed by reaper") {
		t.Fatalf("reaper did not resolve city metadata through GC_CITY_PATH:\n%s", bdData)
	}

	gcData, err := os.ReadFile(gcLog)
	if err != nil {
		t.Fatalf("ReadFile(gc log): %v", err)
	}
	gcLogText := string(gcData)
	if strings.Contains(gcLogText, "stale issue auto-close disabled") {
		t.Fatalf("reaper disabled issue auto-close despite GC_CITY_PATH metadata:\n%s", gcLogText)
	}
	if !strings.Contains(gcLogText, "closed:1") {
		t.Fatalf("reaper summary did not report city issue close:\n%s", gcLogText)
	}
}

func TestReaperScopesIssueAutoCloseToCityBeadsDir(t *testing.T) {
	cityDir := t.TempDir()
	// reaper.sh canonicalizes its CITY arg via `pwd -P`, so on macOS
	// (where t.TempDir is under /var/folders -> /private/var/folders)
	// the logged $PWD will be the resolved form. Resolve here so the
	// assertions below compare apples to apples on every OS.
	if resolved, err := filepath.EvalSymlinks(cityDir); err == nil {
		cityDir = resolved
	}
	writeCityBeadsMetadata(t, cityDir, "citydb")
	canonicalCityDir, err := filepath.EvalSymlinks(cityDir)
	if err != nil {
		t.Fatalf("EvalSymlinks(city dir): %v", err)
	}
	binDir := t.TempDir()
	doltLog := filepath.Join(t.TempDir(), "dolt-args.log")
	bdLog := filepath.Join(t.TempDir(), "bd.log")
	gcLog := filepath.Join(t.TempDir(), "gc.log")
	ambientBeadsDir := filepath.Join(t.TempDir(), "wrong-beads")

	writeExecutable(t, filepath.Join(binDir, "dolt"), `#!/bin/sh
printf '%s\n' "$*" >> "$DOLT_ARGS_LOG"
case "$*" in
  *"SHOW TABLES FROM"*"LIKE 'wisps'"*)
    printf 'Tables_in_db\nwisps\n'
    ;;
  *"SHOW DATABASES"*)
    printf 'Database\ncitydb\n'
    ;;
  *"SELECT id FROM "*"citydb"*"issues"*)
    printf 'id\nga-city\n'
    ;;
  *"COUNT("*)
    printf 'COUNT(*)\n0\n'
    ;;
esac
exit 0
`)
	writeExecutable(t, filepath.Join(binDir, "bd"), `#!/bin/sh
printf 'pwd=%s beads=%s args=%s\n' "$PWD" "${BEADS_DIR:-}" "$*" >> "$BD_CALL_LOG"
exit 0
`)
	writeExecutable(t, filepath.Join(binDir, "gc"), `#!/bin/sh
printf '%s\n' "$*" >> "$GC_CALL_LOG"
exit 0
`)

	env := map[string]string{
		"BEADS_DIR":        ambientBeadsDir,
		"DOLT_ARGS_LOG":    doltLog,
		"BD_CALL_LOG":      bdLog,
		"GC_CALL_LOG":      gcLog,
		"GC_CITY":          cityDir,
		"GC_CITY_PATH":     cityDir,
		"GC_DOLT_HOST":     "127.0.0.1",
		"GC_DOLT_PORT":     "3307",
		"GC_DOLT_USER":     "root",
		"GC_DOLT_PASSWORD": "",
		"PATH":             binDir + string(os.PathListSeparator) + os.Getenv("PATH"),
	}

	runScript(t, filepath.Join(exampleDir(), "packs", "maintenance", "assets", "scripts", "reaper.sh"), env)

	bdData, err := os.ReadFile(bdLog)
	if err != nil {
		t.Fatalf("ReadFile(bd log): %v", err)
	}
	bdLogText := string(bdData)
	if !strings.Contains(bdLogText, "args=close ga-city --reason stale:auto-closed by reaper") {
		t.Fatalf("reaper did not close city issue:\n%s", bdLogText)
	}
	if !strings.Contains(bdLogText, "pwd="+canonicalCityDir) {
		t.Fatalf("reaper did not run bd close from city dir:\n%s", bdLogText)
	}
	if !strings.Contains(bdLogText, "beads="+filepath.Join(canonicalCityDir, ".beads")) {
		t.Fatalf("reaper did not scope bd close to the city beads dir:\n%s", bdLogText)
	}
	if strings.Contains(bdLogText, "beads="+ambientBeadsDir) {
		t.Fatalf("reaper used ambient BEADS_DIR for city auto-close:\n%s", bdLogText)
	}
}

func TestReaperSkipsIssueAutoCloseWhenConfiguredCityDatabaseDoesNotMatchMetadata(t *testing.T) {
	cityDir := t.TempDir()
	writeCityBeadsMetadata(t, cityDir, "citydb")
	binDir := t.TempDir()
	doltLog := filepath.Join(t.TempDir(), "dolt-args.log")
	bdLog := filepath.Join(t.TempDir(), "bd.log")
	gcLog := filepath.Join(t.TempDir(), "gc.log")

	writeExecutable(t, filepath.Join(binDir, "dolt"), `#!/bin/sh
printf '%s\n' "$*" >> "$DOLT_ARGS_LOG"
case "$*" in
  *"SHOW TABLES FROM"*"LIKE 'wisps'"*)
    printf 'Tables_in_db\nwisps\n'
    ;;
  *"SHOW DATABASES"*)
    printf 'Database\ncitydb\nwrongdb\n'
    ;;
  *"SELECT id FROM "*"citydb"*"issues"*)
    printf 'id\nga-city\n'
    ;;
  *"SELECT id FROM "*"wrongdb"*"issues"*)
    printf 'id\nga-wrong\n'
    ;;
  *"COUNT("*)
    printf 'COUNT(*)\n0\n'
    ;;
esac
exit 0
`)
	writeExecutable(t, filepath.Join(binDir, "bd"), `#!/bin/sh
printf '%s\n' "$*" >> "$BD_CALL_LOG"
exit 0
`)
	writeExecutable(t, filepath.Join(binDir, "gc"), `#!/bin/sh
printf '%s\n' "$*" >> "$GC_CALL_LOG"
exit 0
`)

	env := map[string]string{
		"DOLT_ARGS_LOG":           doltLog,
		"BD_CALL_LOG":             bdLog,
		"GC_CALL_LOG":             gcLog,
		"GC_CITY":                 cityDir,
		"GC_CITY_PATH":            cityDir,
		"GC_REAPER_CITY_DATABASE": "wrongdb",
		"GC_DOLT_HOST":            "127.0.0.1",
		"GC_DOLT_PORT":            "3307",
		"GC_DOLT_USER":            "root",
		"GC_DOLT_PASSWORD":        "",
		"PATH":                    binDir + string(os.PathListSeparator) + os.Getenv("PATH"),
	}

	runScript(t, filepath.Join(exampleDir(), "packs", "maintenance", "assets", "scripts", "reaper.sh"), env)

	bdData, err := os.ReadFile(bdLog)
	if err != nil && !os.IsNotExist(err) {
		t.Fatalf("ReadFile(bd log): %v", err)
	}
	if strings.Contains(string(bdData), "close ") {
		t.Fatalf("reaper attempted issue auto-close with invalid city database override:\n%s", bdData)
	}

	gcData, err := os.ReadFile(gcLog)
	if err != nil {
		t.Fatalf("ReadFile(gc log): %v", err)
	}
	gcLogText := string(gcData)
	if !strings.Contains(gcLogText, "city database wrongdb from GC_REAPER_CITY_DATABASE does not match city metadata database citydb") {
		t.Fatalf("reaper did not report invalid city database override:\n%s", gcLogText)
	}
	if !strings.Contains(gcLogText, "stale issue auto-close disabled") {
		t.Fatalf("reaper did not disable stale issue auto-close for invalid city database override:\n%s", gcLogText)
	}
	if !strings.Contains(gcLogText, "skipped_non_city_issues:2") {
		t.Fatalf("reaper did not report skipped stale issue candidate:\n%s", gcLogText)
	}
}

func TestReaperSkipsIssueAutoCloseWhenCityMetadataIsNotJSON(t *testing.T) {
	cityDir := t.TempDir()
	metadataDir := filepath.Join(cityDir, ".beads")
	if err := os.MkdirAll(metadataDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(%s): %v", metadataDir, err)
	}
	if err := os.WriteFile(filepath.Join(metadataDir, "metadata.json"), []byte(`not-json`), 0o644); err != nil {
		t.Fatalf("WriteFile(metadata.json): %v", err)
	}
	binDir := t.TempDir()
	doltLog := filepath.Join(t.TempDir(), "dolt-args.log")
	bdLog := filepath.Join(t.TempDir(), "bd.log")
	gcLog := filepath.Join(t.TempDir(), "gc.log")

	writeExecutable(t, filepath.Join(binDir, "dolt"), `#!/bin/sh
printf '%s\n' "$*" >> "$DOLT_ARGS_LOG"
case "$*" in
  *"SHOW TABLES FROM"*"LIKE 'wisps'"*)
    printf 'Tables_in_db\nwisps\n'
    ;;
  *"SHOW DATABASES"*)
    printf 'Database\nbeads\n'
    ;;
  *"SELECT id FROM "*"issues"*)
    printf 'id\nga-old\n'
    ;;
  *"COUNT("*)
    printf 'COUNT(*)\n0\n'
    ;;
esac
exit 0
`)
	writeExecutable(t, filepath.Join(binDir, "bd"), `#!/bin/sh
printf '%s\n' "$*" >> "$BD_CALL_LOG"
exit 0
`)
	writeExecutable(t, filepath.Join(binDir, "gc"), `#!/bin/sh
printf '%s\n' "$*" >> "$GC_CALL_LOG"
exit 0
`)

	env := map[string]string{
		"DOLT_ARGS_LOG":    doltLog,
		"BD_CALL_LOG":      bdLog,
		"GC_CALL_LOG":      gcLog,
		"GC_CITY":          cityDir,
		"GC_CITY_PATH":     cityDir,
		"GC_DOLT_HOST":     "127.0.0.1",
		"GC_DOLT_PORT":     "3307",
		"GC_DOLT_USER":     "root",
		"GC_DOLT_PASSWORD": "",
		"PATH":             binDir + string(os.PathListSeparator) + os.Getenv("PATH"),
	}

	runScript(t, filepath.Join(exampleDir(), "packs", "maintenance", "assets", "scripts", "reaper.sh"), env)

	bdData, err := os.ReadFile(bdLog)
	if err != nil && !os.IsNotExist(err) {
		t.Fatalf("ReadFile(bd log): %v", err)
	}
	if strings.Contains(string(bdData), "close ") {
		t.Fatalf("reaper attempted issue auto-close after metadata parse failed:\n%s", bdData)
	}

	gcData, err := os.ReadFile(gcLog)
	if err != nil {
		t.Fatalf("ReadFile(gc log): %v", err)
	}
	gcLogText := string(gcData)
	if !strings.Contains(gcLogText, "stale issue auto-close disabled") {
		t.Fatalf("reaper did not degrade to disabled auto-close after metadata parse failure:\n%s", gcLogText)
	}
	if !strings.Contains(gcLogText, "skipped_non_city_issues:1") {
		t.Fatalf("reaper did not report skipped stale issue candidate:\n%s", gcLogText)
	}
}

func TestReaperCityDatabaseUsesShellFallbackWhenJSONParsersUnavailable(t *testing.T) {
	cityDir := t.TempDir()
	writeCityBeadsMetadata(t, cityDir, "citydb")
	binDir := t.TempDir()
	for _, tool := range []string{"bash", "dirname", "tail", "grep", "cut", "tr", "mktemp", "rm", "sed", "wc", "cat", "head"} {
		linkTestPathTool(t, binDir, tool)
	}
	doltLog := filepath.Join(t.TempDir(), "dolt-args.log")
	bdLog := filepath.Join(t.TempDir(), "bd.log")
	gcLog := filepath.Join(t.TempDir(), "gc.log")

	writeExecutable(t, filepath.Join(binDir, "dolt"), `#!/bin/sh
printf '%s\n' "$*" >> "$DOLT_ARGS_LOG"
case "$*" in
  *"SHOW TABLES FROM"*"LIKE 'wisps'"*)
    printf 'Tables_in_db\nwisps\n'
    ;;
  *"SHOW DATABASES"*)
    printf 'Database\ncitydb\n'
    ;;
  *"SELECT id FROM "*"citydb"*"issues"*)
    printf 'id\nga-city\n'
    ;;
  *"COUNT("*)
    printf 'COUNT(*)\n0\n'
    ;;
esac
exit 0
`)
	writeExecutable(t, filepath.Join(binDir, "bd"), `#!/bin/sh
printf '%s\n' "$*" >> "$BD_CALL_LOG"
exit 0
`)
	writeExecutable(t, filepath.Join(binDir, "gc"), `#!/bin/sh
printf '%s\n' "$*" >> "$GC_CALL_LOG"
exit 0
`)

	env := map[string]string{
		"DOLT_ARGS_LOG":    doltLog,
		"BD_CALL_LOG":      bdLog,
		"GC_CALL_LOG":      gcLog,
		"GC_CITY":          cityDir,
		"GC_CITY_PATH":     cityDir,
		"GC_DOLT_HOST":     "127.0.0.1",
		"GC_DOLT_PORT":     "3307",
		"GC_DOLT_USER":     "root",
		"GC_DOLT_PASSWORD": "",
		"PATH":             binDir,
	}

	runScript(t, filepath.Join(exampleDir(), "packs", "maintenance", "assets", "scripts", "reaper.sh"), env)

	bdData, err := os.ReadFile(bdLog)
	if err != nil {
		t.Fatalf("ReadFile(bd log): %v", err)
	}
	if !strings.Contains(string(bdData), "close ga-city --reason stale:auto-closed by reaper") {
		t.Fatalf("reaper did not close city issue through metadata fallback:\n%s", bdData)
	}

	gcData, err := os.ReadFile(gcLog)
	if err != nil {
		t.Fatalf("ReadFile(gc log): %v", err)
	}
	gcLogText := string(gcData)
	if strings.Contains(gcLogText, "ESCALATION") || strings.Contains(gcLogText, "stale issue auto-close disabled") {
		t.Fatalf("reaper escalated despite successful shell metadata fallback:\n%s", gcLogText)
	}
	if !strings.Contains(gcLogText, "closed:1") {
		t.Fatalf("reaper summary did not report city issue close:\n%s", gcLogText)
	}
}

func TestReaperSkipsIssueAutoCloseWhenCityMetadataIsMalformed(t *testing.T) {
	cityDir := t.TempDir()
	metadataDir := filepath.Join(cityDir, ".beads")
	if err := os.MkdirAll(metadataDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(%s): %v", metadataDir, err)
	}
	if err := os.WriteFile(filepath.Join(metadataDir, "metadata.json"), []byte(`{"dolt_database":"beads"`), 0o644); err != nil {
		t.Fatalf("WriteFile(metadata.json): %v", err)
	}
	binDir := t.TempDir()
	doltLog := filepath.Join(t.TempDir(), "dolt-args.log")
	bdLog := filepath.Join(t.TempDir(), "bd.log")
	gcLog := filepath.Join(t.TempDir(), "gc.log")

	writeExecutable(t, filepath.Join(binDir, "dolt"), `#!/bin/sh
printf '%s\n' "$*" >> "$DOLT_ARGS_LOG"
case "$*" in
  *"SHOW TABLES FROM"*"LIKE 'wisps'"*)
    printf 'Tables_in_db\nwisps\n'
    ;;
  *"SHOW DATABASES"*)
    printf 'Database\nbeads\n'
    ;;
  *"SELECT id FROM "*"issues"*)
    printf 'id\nga-old\n'
    ;;
  *"COUNT("*)
    printf 'COUNT(*)\n0\n'
    ;;
esac
exit 0
`)
	writeExecutable(t, filepath.Join(binDir, "bd"), `#!/bin/sh
printf '%s\n' "$*" >> "$BD_CALL_LOG"
exit 0
`)
	writeExecutable(t, filepath.Join(binDir, "gc"), `#!/bin/sh
printf '%s\n' "$*" >> "$GC_CALL_LOG"
exit 0
`)

	env := map[string]string{
		"DOLT_ARGS_LOG":    doltLog,
		"BD_CALL_LOG":      bdLog,
		"GC_CALL_LOG":      gcLog,
		"GC_CITY":          cityDir,
		"GC_CITY_PATH":     cityDir,
		"GC_DOLT_HOST":     "127.0.0.1",
		"GC_DOLT_PORT":     "3307",
		"GC_DOLT_USER":     "root",
		"GC_DOLT_PASSWORD": "",
		"PATH":             binDir + string(os.PathListSeparator) + os.Getenv("PATH"),
	}

	runScript(t, filepath.Join(exampleDir(), "packs", "maintenance", "assets", "scripts", "reaper.sh"), env)

	bdData, err := os.ReadFile(bdLog)
	if err != nil && !os.IsNotExist(err) {
		t.Fatalf("ReadFile(bd log): %v", err)
	}
	if strings.Contains(string(bdData), "close ") {
		t.Fatalf("reaper accepted malformed metadata and attempted issue auto-close:\n%s", bdData)
	}

	gcData, err := os.ReadFile(gcLog)
	if err != nil {
		t.Fatalf("ReadFile(gc log): %v", err)
	}
	gcLogText := string(gcData)
	if !strings.Contains(gcLogText, "stale issue auto-close disabled") {
		t.Fatalf("reaper did not disable auto-close for malformed city metadata:\n%s", gcLogText)
	}
	if !strings.Contains(gcLogText, "skipped_non_city_issues:1") {
		t.Fatalf("reaper did not report skipped stale issue candidate:\n%s", gcLogText)
	}
}

func TestReaperSkipsIssueAutoCloseWhenCityDatabaseUnknown(t *testing.T) {
	cityDir := t.TempDir()
	binDir := t.TempDir()
	doltLog := filepath.Join(t.TempDir(), "dolt-args.log")
	bdLog := filepath.Join(t.TempDir(), "bd.log")
	gcLog := filepath.Join(t.TempDir(), "gc.log")

	writeExecutable(t, filepath.Join(binDir, "dolt"), `#!/bin/sh
printf '%s\n' "$*" >> "$DOLT_ARGS_LOG"
case "$*" in
  *"SHOW TABLES FROM"*"LIKE 'wisps'"*)
    printf 'Tables_in_db\nwisps\n'
    ;;
  *"SHOW DATABASES"*)
    printf 'Database\nbeads\nrigdb\n'
    ;;
  *"SELECT id FROM "*"issues"*)
    printf 'id\nga-old\n'
    ;;
  *"COUNT("*)
    printf 'COUNT(*)\n0\n'
    ;;
esac
exit 0
`)
	writeExecutable(t, filepath.Join(binDir, "bd"), `#!/bin/sh
printf '%s\n' "$*" >> "$BD_CALL_LOG"
exit 0
`)
	writeExecutable(t, filepath.Join(binDir, "gc"), `#!/bin/sh
printf '%s\n' "$*" >> "$GC_CALL_LOG"
exit 0
`)

	env := map[string]string{
		"DOLT_ARGS_LOG":    doltLog,
		"BD_CALL_LOG":      bdLog,
		"GC_CALL_LOG":      gcLog,
		"GC_CITY":          cityDir,
		"GC_CITY_PATH":     cityDir,
		"GC_DOLT_HOST":     "127.0.0.1",
		"GC_DOLT_PORT":     "3307",
		"GC_DOLT_USER":     "root",
		"GC_DOLT_PASSWORD": "",
		"PATH":             binDir + string(os.PathListSeparator) + os.Getenv("PATH"),
	}

	runScript(t, filepath.Join(exampleDir(), "packs", "maintenance", "assets", "scripts", "reaper.sh"), env)

	bdData, err := os.ReadFile(bdLog)
	if err != nil && !os.IsNotExist(err) {
		t.Fatalf("ReadFile(bd log): %v", err)
	}
	if strings.Contains(string(bdData), "close ") {
		t.Fatalf("reaper attempted issue auto-close without city database identity:\n%s", bdData)
	}

	gcData, err := os.ReadFile(gcLog)
	if err != nil {
		t.Fatalf("ReadFile(gc log): %v", err)
	}
	gcLogText := string(gcData)
	if !strings.Contains(gcLogText, "stale issue auto-close disabled") {
		t.Fatalf("reaper did not escalate missing city database identity:\n%s", gcLogText)
	}
	if !strings.Contains(gcLogText, "skipped_non_city_issues:2") {
		t.Fatalf("reaper did not report skipped stale issue candidates:\n%s", gcLogText)
	}
}

func TestReaperIgnoresNothingToCommitAfterMutationRace(t *testing.T) {
	cityDir := t.TempDir()
	binDir := t.TempDir()
	doltLog := filepath.Join(t.TempDir(), "dolt-args.log")
	gcLog := filepath.Join(t.TempDir(), "gc.log")

	writeExecutable(t, filepath.Join(binDir, "dolt"), `#!/bin/sh
printf '%s\n' "$*" >> "$DOLT_ARGS_LOG"
case "$*" in
  *"SHOW TABLES FROM"*"LIKE 'wisps'"*)
    printf 'Tables_in_db\nwisps\n'
    ;;
  *"SHOW DATABASES"*)
    printf 'Database\nbeads\n'
    ;;
  *"CALL DOLT_COMMIT"*)
    printf 'nothing to commit\n' >&2
    exit 1
    ;;
  *"DELETE FROM "*"wisps"*)
    printf 'ROW_COUNT()\n1\n'
    ;;
  *"status = 'closed'"*"closed_at <"*)
    printf 'COUNT(*)\n1\n'
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
	writeExecutable(t, filepath.Join(binDir, "gc"), `#!/bin/sh
printf '%s\n' "$*" >> "$GC_CALL_LOG"
exit 0
`)

	env := map[string]string{
		"DOLT_ARGS_LOG":    doltLog,
		"GC_CALL_LOG":      gcLog,
		"GC_CITY":          cityDir,
		"GC_CITY_PATH":     cityDir,
		"GC_DOLT_HOST":     "127.0.0.1",
		"GC_DOLT_PORT":     "3307",
		"GC_DOLT_USER":     "root",
		"GC_DOLT_PASSWORD": "",
		"PATH":             binDir + string(os.PathListSeparator) + os.Getenv("PATH"),
	}

	runScript(t, filepath.Join(exampleDir(), "packs", "maintenance", "assets", "scripts", "reaper.sh"), env)

	gcData, err := os.ReadFile(gcLog)
	if err != nil {
		t.Fatalf("ReadFile(gc log): %v", err)
	}
	gcLogText := string(gcData)
	if strings.Contains(gcLogText, "mail send mayor/ -s ESCALATION") || strings.Contains(gcLogText, "Dolt commit found nothing to commit") {
		t.Fatalf("reaper escalated benign nothing-to-commit race:\n%s", gcLogText)
	}
}

func TestReaperFormulaMatchesScriptDefaults(t *testing.T) {
	scriptPath := filepath.Join(exampleDir(), "packs", "maintenance", "assets", "scripts", "reaper.sh")
	scriptData, err := os.ReadFile(scriptPath)
	if err != nil {
		t.Fatalf("ReadFile(%s): %v", scriptPath, err)
	}
	formulaPath := filepath.Join(exampleDir(), "packs", "maintenance", "formulas", "mol-dog-reaper.toml")
	formulaData, err := os.ReadFile(formulaPath)
	if err != nil {
		t.Fatalf("ReadFile(%s): %v", formulaPath, err)
	}

	script := string(scriptData)
	formula := string(formulaData)
	for _, check := range []struct {
		scriptEnv string
		formVar   string
	}{
		{scriptEnv: "GC_REAPER_MAX_AGE", formVar: "max_age"},
		{scriptEnv: "GC_REAPER_PURGE_AGE", formVar: "purge_age"},
		{scriptEnv: "GC_REAPER_STALE_ISSUE_AGE", formVar: "stale_issue_age"},
	} {
		scriptDefault := extractShellDefault(t, script, check.scriptEnv)
		formulaDefault := extractFormulaDefault(t, formula, check.formVar)
		if scriptDefault != formulaDefault {
			t.Errorf("%s default mismatch: script=%q formula=%q", check.formVar, scriptDefault, formulaDefault)
		}
	}
}

func extractShellDefault(t *testing.T, script, envName string) string {
	t.Helper()
	re := regexp.MustCompile(envName + `:-([^}"]+)`)
	m := re.FindStringSubmatch(script)
	if len(m) != 2 {
		t.Fatalf("default for %s not found in script", envName)
	}
	return m[1]
}

func extractFormulaDefault(t *testing.T, formula, varName string) string {
	t.Helper()
	re := regexp.MustCompile(`(?s)\[vars\.` + regexp.QuoteMeta(varName) + `\].*?default = "([^"]+)"`)
	m := re.FindStringSubmatch(formula)
	if len(m) != 2 {
		t.Fatalf("default for %s not found in formula", varName)
	}
	return m[1]
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

// TestMaintenanceDoltScriptsSkipDatabasesWithoutWispsTable pins the
// schemaless-DB precheck (gastownhall/gascity#1816). Both reaper.sh and
// jsonl-export.sh iterate user databases discovered by SHOW DATABASES,
// but a database that exists on the server without bd schema (orphan
// CREATE DATABASE, partial migration, system schemas not on the
// is_user_database blocklist) has nothing for them to do — querying its
// tables just produces spurious "table not found" anomalies (reaper) or
// failed-DB summary entries (jsonl-export). Both scripts now probe
// SHOW TABLES FROM <db> LIKE 'wisps' via the shared has_wisps_table
// helper in dolt-target.sh and skip silently when wisps is absent.
func TestMaintenanceDoltScriptsSkipDatabasesWithoutWispsTable(t *testing.T) {
	tests := []struct {
		name           string
		script         string
		env            map[string]string
		forbiddenLogs  []string
		gcLogForbidden string
	}{
		{
			name:   "reaper",
			script: filepath.Join("packs", "maintenance", "assets", "scripts", "reaper.sh"),
			env:    map[string]string{"GC_REAPER_DRY_RUN": "1"},
			forbiddenLogs: []string{
				"`empty_db`.wisps",
				"`empty_db`.issues",
				"`empty_db`.dependencies",
			},
			gcLogForbidden: "empty_db",
		},
		{
			name:   "jsonl export",
			script: filepath.Join("packs", "maintenance", "assets", "scripts", "jsonl-export.sh"),
			env: map[string]string{
				"GC_JSONL_ARCHIVE_REPO":      "archive",
				"GC_JSONL_MAX_PUSH_FAILURES": "99",
			},
			forbiddenLogs: []string{
				"`empty_db`.issues",
			},
			// jsonl-export reports failures via DOG_DONE summary line in
			// the gc nudge — empty_db must not show up there.
			gcLogForbidden: "empty_db",
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
				"DOLT_ARGS_LOG":          doltLog,
				"DOLT_DBS":               "real_beads empty_db",
				"DOLT_DBS_WITHOUT_WISPS": "empty_db",
				"GC_CALL_LOG":            gcLog,
				"GC_CITY":                cityDir,
				"GC_CITY_PATH":           cityDir,
				"GC_PACK_STATE_DIR":      stateDir,
				"GC_DOLT_HOST":           "127.0.0.1",
				"GC_DOLT_PORT":           "3307",
				"GC_DOLT_USER":           "root",
				"GC_DOLT_PASSWORD":       "",
				"GIT_CONFIG_GLOBAL":      filepath.Join(t.TempDir(), "gitconfig"),
				"GIT_CONFIG_NOSYSTEM":    "1",
				"PATH":                   binDir + string(os.PathListSeparator) + os.Getenv("PATH"),
			}
			for k, v := range tt.env {
				if k == "GC_JSONL_ARCHIVE_REPO" {
					v = filepath.Join(cityDir, v)
				}
				env[k] = v
			}

			runScript(t, filepath.Join(exampleDir(), tt.script), env)

			logData, err := os.ReadFile(doltLog)
			if err != nil {
				t.Fatalf("ReadFile(dolt log): %v", err)
			}
			log := string(logData)

			// The precheck itself ran for empty_db. Without this
			// assertion the test could pass via an unrelated
			// early-skip path that never reached the precheck.
			if !strings.Contains(log, "SHOW TABLES FROM `empty_db` LIKE 'wisps'") {
				t.Errorf("script did not run the SHOW TABLES precheck against empty_db:\n%s", log)
			}

			// empty_db has no wisps → script must skip without
			// querying its tables.
			for _, forbidden := range tt.forbiddenLogs {
				if strings.Contains(log, forbidden) {
					t.Errorf("script queried schemaless DB (%s); precheck did not skip:\n%s", forbidden, log)
				}
			}

			// No anomaly / failure escalation should mention empty_db.
			gcData, err := os.ReadFile(gcLog)
			if err != nil && !os.IsNotExist(err) {
				t.Fatalf("ReadFile(gc log): %v", err)
			}
			if strings.Contains(string(gcData), tt.gcLogForbidden) {
				t.Errorf("script surfaced empty_db to gc/mayor; precheck should have suppressed:\n%s", gcData)
			}
		})
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
	out, err := runScriptResult(t, script, env)
	if err != nil {
		t.Fatalf("%s failed: %v\n%s", filepath.Base(script), err, out)
	}
}

func runScriptResult(t *testing.T, script string, env map[string]string) ([]byte, error) {
	t.Helper()
	cmd := exec.Command(script)
	cmd.Env = mergeTestEnv(env)
	return cmd.CombinedOutput()
}

func writeExecutable(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatalf("WriteFile(%s): %v", path, err)
	}
}

func linkTestPathTool(t *testing.T, binDir, name string) {
	t.Helper()
	realPath, err := exec.LookPath(name)
	if err != nil {
		t.Fatalf("LookPath(%s): %v", name, err)
	}
	linkPath := filepath.Join(binDir, name)
	if err := os.Symlink(realPath, linkPath); err != nil {
		t.Fatalf("Symlink(%s, %s): %v", realPath, linkPath, err)
	}
}

func writeCityBeadsMetadata(t *testing.T, cityDir, db string) {
	t.Helper()
	metadataDir := filepath.Join(cityDir, ".beads")
	if err := os.MkdirAll(metadataDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(%s): %v", metadataDir, err)
	}
	metadata := fmt.Sprintf("{\n  \"dolt_database\": %q\n}\n", db)
	if err := os.WriteFile(filepath.Join(metadataDir, "metadata.json"), []byte(metadata), 0o644); err != nil {
		t.Fatalf("WriteFile(metadata.json): %v", err)
	}
}

func writeMaintenanceDoltStub(t *testing.T, path string) {
	t.Helper()
	writeExecutable(t, path, `#!/bin/sh
printf '%s\n' "$*" >> "$DOLT_ARGS_LOG"
case "$*" in
*"SHOW DATABASES"*)
  printf 'Database\n'
  if [ -n "${DOLT_DBS:-}" ]; then
    for db in $DOLT_DBS; do
      printf '%s\n' "$db"
    done
  else
    printf 'beads\n'
  fi
  ;;
*"SHOW TABLES FROM"*"LIKE 'wisps'"*)
  # Schemaless-DB precheck (gastownhall/gascity#1816). By default, every
  # test DB is treated as having wisps so existing tests are unaffected.
  # Tests that exercise the precheck set DOLT_DBS_WITHOUT_WISPS to a
  # space-separated list of DB names that should report no wisps row.
  printf 'Tables_in_db\n'
  for skip in ${DOLT_DBS_WITHOUT_WISPS:-}; do
    case "$*" in
    *"FROM"*"$skip"*"LIKE 'wisps'"*) exit 0 ;;
    esac
  done
  printf 'wisps\n'
  ;;
*"SELECT *"*)
  printf '{"id":"ga-1","title":"sample"}\n'
  ;;
*"DELETE FROM "*"wisps"*)
  if [ -n "${DOLT_PURGE_COUNT:-}" ]; then
    printf 'ROW_COUNT()\n%s\n' "$DOLT_PURGE_COUNT"
  else
    printf 'ROW_COUNT()\n0\n'
  fi
  ;;
*"status = 'closed'"*"closed_at <"*)
  if [ -n "${DOLT_PURGE_COUNT:-}" ]; then
    printf 'COUNT(*)\n%s\n' "$DOLT_PURGE_COUNT"
  else
    printf 'COUNT(*)\n0\n'
  fi
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

// jsonlExportEnv builds the common env map used by the spike-detection tests
// below. Callers append per-test overrides on the returned map.
func jsonlExportEnv(t *testing.T, cityDir, binDir, stateDir, archiveRepo, gcLog, mailLog string) map[string]string {
	t.Helper()
	return map[string]string{
		"GC_CALL_LOG":                gcLog,
		"GC_MAIL_LOG":                mailLog,
		"GC_CITY":                    cityDir,
		"GC_CITY_PATH":               cityDir,
		"GC_PACK_STATE_DIR":          stateDir,
		"GC_DOLT_HOST":               "127.0.0.1",
		"GC_DOLT_PORT":               "3307",
		"GC_DOLT_USER":               "root",
		"GC_DOLT_PASSWORD":           "",
		"GC_JSONL_ARCHIVE_REPO":      archiveRepo,
		"GC_JSONL_MAX_PUSH_FAILURES": "99",
		"GC_JSONL_SCRUB":             "false",
		"GIT_CONFIG_GLOBAL":          filepath.Join(t.TempDir(), "gitconfig"),
		"GIT_CONFIG_NOSYSTEM":        "1",
		"PATH":                       binDir + string(os.PathListSeparator) + os.Getenv("PATH"),
	}
}

// writeJsonlExportGCStub installs a `gc` shim that mirrors mail-send calls into
// a separate log so tests can assert escalations independently of the noisier
// nudge stream.
func writeJsonlExportGCStub(t *testing.T, binDir string) {
	t.Helper()
	writeJsonlExportGCStubWithMailExitCode(t, binDir, 0)
}

func writeJsonlExportGCStubWithMailExitCode(t *testing.T, binDir string, mailExitCode int) {
	t.Helper()
	writeExecutable(t, filepath.Join(binDir, "gc"), `#!/bin/sh
printf '%s\n' "$*" >> "$GC_CALL_LOG"
if [ "$1" = "mail" ] && [ "$2" = "send" ]; then
    printf '%s\n' "$*" >> "$GC_MAIL_LOG"
    exit `+strconv.Itoa(mailExitCode)+`
fi
exit 0
`)
}

// initSeedArchive builds a git repo at archiveRepo with one committed copy of
// issues.jsonl whose .rows array length equals prevCount, then returns the
// resulting commit SHA. The default branch is forced to `main` so the script's
// later `git push origin main` would target the same ref the test verifies.
func initSeedArchive(t *testing.T, archiveRepo string, prevCount int) string {
	t.Helper()
	dbDir := filepath.Join(archiveRepo, "beads")
	if err := os.MkdirAll(dbDir, 0o755); err != nil {
		t.Fatal(err)
	}
	rows := make([]string, 0, prevCount)
	for i := 0; i < prevCount; i++ {
		rows = append(rows, fmt.Sprintf(`{"id":"p%d","title":"prev-%d"}`, i, i))
	}
	body := `{"rows":[` + strings.Join(rows, ",") + `]}` + "\n"
	if err := os.WriteFile(filepath.Join(dbDir, "issues.jsonl"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	// Persist identity to the repo's local config so the production script's
	// later `git commit` (no -c flags, no user-level config in the test env)
	// has a committer.
	steps := [][]string{
		{"-c", "init.defaultBranch=main", "init", "-q"},
		{"config", "user.email", "test@example.invalid"},
		{"config", "user.name", "test"},
		{"add", "-A"},
		{"commit", "-q", "-m", "seed"},
	}
	for _, args := range steps {
		full := append([]string{"-C", archiveRepo}, args...)
		cmd := exec.Command("git", full...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
	cmd := exec.Command("git", "-C", archiveRepo, "rev-parse", "HEAD")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git rev-parse: %v\n%s", err, out)
	}
	return strings.TrimSpace(string(out))
}

// writeMultiRecordDoltStub emits a `dolt` shim that returns a JSON object with
// the given record count for the issues table and an empty `{"rows":[]}` for
// the supplemental tables. Crucially the issues output is on a SINGLE physical
// line — the realistic shape of `dolt sql -r json` — so `wc -l` on it returns
// 1 regardless of record count.
func writeMultiRecordDoltStub(t *testing.T, binDir string, currentCount int) {
	t.Helper()
	rows := make([]string, 0, currentCount)
	for i := 0; i < currentCount; i++ {
		rows = append(rows, fmt.Sprintf(`{"id":"c%d","title":"cur-%d"}`, i, i))
	}
	writeIssuesPayloadDoltStub(t, binDir, `{"rows":[`+strings.Join(rows, ",")+`]}`)
}

func writeIssuesPayloadDoltStub(t *testing.T, binDir, issuesPayload string) {
	t.Helper()
	body := "#!/bin/sh\n" +
		"case \"$*\" in\n" +
		"  *\"SHOW TABLES FROM\"*\"LIKE 'wisps'\"*)\n" +
		"    printf 'Tables_in_db\\nwisps\\n'\n" +
		"    ;;\n" +
		"  *\"SHOW DATABASES\"*)\n" +
		"    printf 'Database\\nbeads\\n'\n" +
		"    ;;\n" +
		"  *\"FROM \\`beads\\`.issues\"*)\n" +
		"    printf '%s\\n' '" + issuesPayload + "'\n" +
		"    ;;\n" +
		"  *\"SELECT *\"*)\n" +
		"    printf '{\"rows\":[]}\\n'\n" +
		"    ;;\n" +
		"esac\n" +
		"exit 0\n"
	writeExecutable(t, filepath.Join(binDir, "dolt"), body)
}

func writeIssueRowsDoltStub(t *testing.T, binDir string, rows []string) {
	t.Helper()
	writeIssuesPayloadDoltStub(t, binDir, `{"rows":[`+strings.Join(rows, ",")+`]}`)
}

func writeNoUserDatabasesDoltStub(t *testing.T, binDir string) {
	t.Helper()
	writeExecutable(t, filepath.Join(binDir, "dolt"), `#!/bin/sh
case "$*" in
  *"SHOW TABLES FROM"*"LIKE 'wisps'"*)
    printf 'Tables_in_db\nwisps\n'
    ;;
  *"SHOW DATABASES"*)
    printf 'Database\n'
    ;;
esac
exit 0
`)
}

func writeEmptyIssuesPayloadDoltStub(t *testing.T, binDir string) {
	t.Helper()
	body := "#!/bin/sh\n" +
		"case \"$*\" in\n" +
		"  *\"SHOW TABLES FROM\"*\"LIKE 'wisps'\"*)\n" +
		"    printf 'Tables_in_db\\nwisps\\n'\n" +
		"    ;;\n" +
		"  *\"SHOW DATABASES\"*)\n" +
		"    printf 'Database\\nbeads\\n'\n" +
		"    ;;\n" +
		"  *\"FROM \\`beads\\`.issues\"*)\n" +
		"    ;;\n" +
		"  *\"SELECT *\"*)\n" +
		"    printf '{\"rows\":[]}\\n'\n" +
		"    ;;\n" +
		"esac\n" +
		"exit 0\n"
	writeExecutable(t, filepath.Join(binDir, "dolt"), body)
}

func writeIssuesExportFailureDoltStub(t *testing.T, binDir string) {
	t.Helper()
	body := "#!/bin/sh\n" +
		"case \"$*\" in\n" +
		"  *\"SHOW TABLES FROM\"*\"LIKE 'wisps'\"*)\n" +
		"    printf 'Tables_in_db\\nwisps\\n'\n" +
		"    ;;\n" +
		"  *\"SHOW DATABASES\"*)\n" +
		"    printf 'Database\\nbeads\\n'\n" +
		"    ;;\n" +
		"  *\"FROM \\`beads\\`.issues\"*)\n" +
		"    echo 'simulated issues export failure' >&2\n" +
		"    exit 1\n" +
		"    ;;\n" +
		"  *\"SELECT *\"*)\n" +
		"    printf '{\"rows\":[]}\\n'\n" +
		"    ;;\n" +
		"esac\n" +
		"exit 0\n"
	writeExecutable(t, filepath.Join(binDir, "dolt"), body)
}

func writeGitSubcommandFailureStub(t *testing.T, binDir, realGit, subcommand string) {
	t.Helper()
	writeExecutable(t, filepath.Join(binDir, "git"), fmt.Sprintf(`#!/bin/sh
for arg in "$@"; do
    if [ "$arg" = "%s" ]; then
        echo "simulated git %s failure" >&2
        exit 1
    fi
done
exec '%s' "$@"
`, subcommand, subcommand, realGit))
}

func initSeedArchiveWithoutLocalIdentity(t *testing.T, archiveRepo string, prevCount int) string {
	t.Helper()
	dbDir := filepath.Join(archiveRepo, "beads")
	if err := os.MkdirAll(dbDir, 0o755); err != nil {
		t.Fatal(err)
	}
	rows := make([]string, 0, prevCount)
	for i := 0; i < prevCount; i++ {
		rows = append(rows, fmt.Sprintf(`{"id":"p%d","title":"prev-%d"}`, i, i))
	}
	body := `{"rows":[` + strings.Join(rows, ",") + `]}` + "\n"
	if err := os.WriteFile(filepath.Join(dbDir, "issues.jsonl"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	steps := [][]string{
		{"-c", "init.defaultBranch=main", "init", "-q"},
		{"add", "-A"},
		{"commit", "-q", "-m", "seed"},
	}
	commitEnv := append(os.Environ(),
		"GIT_AUTHOR_NAME=seed-author",
		"GIT_AUTHOR_EMAIL=seed-author@example.invalid",
		"GIT_COMMITTER_NAME=seed-committer",
		"GIT_COMMITTER_EMAIL=seed-committer@example.invalid",
	)
	for _, args := range steps {
		full := append([]string{"-C", archiveRepo}, args...)
		cmd := exec.Command("git", full...)
		if len(args) > 0 && args[len(args)-1] == "seed" {
			cmd.Env = commitEnv
		}
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
	cmd := exec.Command("git", "-C", archiveRepo, "rev-parse", "HEAD")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git rev-parse: %v\n%s", err, out)
	}
	return strings.TrimSpace(string(out))
}

func initSeedArchiveWithRemote(t *testing.T, archiveRepo string) (string, string) {
	t.Helper()
	remoteRepo := filepath.Join(t.TempDir(), "archive-remote.git")
	if out, err := exec.Command("git", "init", "--bare", "-q", remoteRepo).CombinedOutput(); err != nil {
		t.Fatalf("git init --bare: %v\n%s", err, out)
	}
	if out, err := exec.Command("git", "clone", "-q", remoteRepo, archiveRepo).CombinedOutput(); err != nil {
		t.Fatalf("git clone: %v\n%s", err, out)
	}

	dbDir := filepath.Join(archiveRepo, "beads")
	if err := os.MkdirAll(dbDir, 0o755); err != nil {
		t.Fatal(err)
	}
	rows := make([]string, 0, 100)
	for i := 0; i < 100; i++ {
		rows = append(rows, fmt.Sprintf(`{"id":"p%d","title":"prev-%d"}`, i, i))
	}
	body := `{"rows":[` + strings.Join(rows, ",") + `]}` + "\n"
	if err := os.WriteFile(filepath.Join(dbDir, "issues.jsonl"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	steps := [][]string{
		{"checkout", "-q", "-b", "main"},
		{"config", "user.email", "test@example.invalid"},
		{"config", "user.name", "test"},
		{"add", "-A"},
		{"commit", "-q", "-m", "seed"},
		{"push", "-q", "-u", "origin", "main"},
	}
	for _, args := range steps {
		full := append([]string{"-C", archiveRepo}, args...)
		if out, err := exec.Command("git", full...).CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}

	headOut, err := exec.Command("git", "--git-dir", remoteRepo, "rev-parse", "refs/heads/main").CombinedOutput()
	if err != nil {
		t.Fatalf("git rev-parse remote main: %v\n%s", err, headOut)
	}
	return remoteRepo, strings.TrimSpace(string(headOut))
}

func initEmptyArchiveRemote(t *testing.T, archiveRepo string, prevCount int) string {
	t.Helper()
	remoteRepo := filepath.Join(t.TempDir(), "archive-remote.git")
	if out, err := exec.Command("git", "init", "--bare", "-q", remoteRepo).CombinedOutput(); err != nil {
		t.Fatalf("git init --bare: %v\n%s", err, out)
	}
	initSeedArchive(t, archiveRepo, prevCount)
	if out, err := exec.Command("git", "-C", archiveRepo, "remote", "add", "origin", remoteRepo).CombinedOutput(); err != nil {
		t.Fatalf("git remote add origin: %v\n%s", err, out)
	}
	return remoteRepo
}

// initSeedArchiveWithUnreachableRemote seeds the archive and adds an `origin`
// that points at a nonexistent path, so any `git fetch`/`git push` fails.
// Used by tests that specifically exercise the push-failure recovery paths:
// push mode is active (so `should_attempt_push` returns true) but the remote
// cannot be reached.
func initSeedArchiveWithUnreachableRemote(t *testing.T, archiveRepo string, prevCount int) {
	t.Helper()
	initSeedArchive(t, archiveRepo, prevCount)
	unreachable := filepath.Join(t.TempDir(), "nonexistent-remote.git")
	if out, err := exec.Command("git", "-C", archiveRepo, "remote", "add", "origin", unreachable).CombinedOutput(); err != nil {
		t.Fatalf("git remote add origin: %v\n%s", err, out)
	}
}

func advanceArchiveRemoteMain(t *testing.T, remoteRepo string) string {
	t.Helper()
	worktree := t.TempDir()
	if out, err := exec.Command("git", "clone", "-q", remoteRepo, worktree).CombinedOutput(); err != nil {
		t.Fatalf("git clone remote advance worktree: %v\n%s", err, out)
	}
	steps := [][]string{
		{"-C", worktree, "checkout", "-q", "main"},
		{"-C", worktree, "config", "user.email", "test@example.invalid"},
		{"-C", worktree, "config", "user.name", "test"},
	}
	for _, args := range steps {
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args[2:], " "), err, out)
		}
	}
	if err := os.WriteFile(filepath.Join(worktree, "remote-marker.txt"), []byte("remote-advance\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(remote marker): %v", err)
	}
	for _, args := range [][]string{
		{"-C", worktree, "add", "-A"},
		{"-C", worktree, "commit", "-q", "-m", "remote advance"},
		{"-C", worktree, "push", "-q", "origin", "main"},
	} {
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args[2:], " "), err, out)
		}
	}
	headOut, err := exec.Command("git", "--git-dir", remoteRepo, "rev-parse", "refs/heads/main").CombinedOutput()
	if err != nil {
		t.Fatalf("git rev-parse remote main after advance: %v\n%s", err, headOut)
	}
	return strings.TrimSpace(string(headOut))
}

func TestJsonlExportCountsRecordsViaJq(t *testing.T) {
	// Bug 1 (#1547): `wc -l` on `dolt -r json` output measures formatting, not
	// records — the JSON object is one physical line regardless of row count.
	// Verify CURRENT_COUNT reflects the actual record count (3).
	cityDir := t.TempDir()
	binDir := t.TempDir()
	stateDir := t.TempDir()
	gcLog := filepath.Join(t.TempDir(), "gc.log")
	mailLog := filepath.Join(t.TempDir(), "gc-mail.log")
	archiveRepo := filepath.Join(cityDir, "archive")

	writeMultiRecordDoltStub(t, binDir, 3)
	writeJsonlExportGCStub(t, binDir)

	env := jsonlExportEnv(t, cityDir, binDir, stateDir, archiveRepo, gcLog, mailLog)

	runScript(t, filepath.Join(exampleDir(), "packs", "maintenance", "assets", "scripts", "jsonl-export.sh"), env)

	gcData, err := os.ReadFile(gcLog)
	if err != nil {
		t.Fatalf("ReadFile(gc log): %v", err)
	}
	log := string(gcData)
	if !strings.Contains(log, "records: 3") {
		t.Fatalf("expected DOG_DONE summary to report records: 3 (jq counted .rows length); got:\n%s", log)
	}
}

func TestJsonlExportSkipsSpikeCheckBelowMinPrev(t *testing.T) {
	// Bug 2 (#1547): percent-delta with no absolute floor escalates on tiny
	// counts. prev=2, current=1 → 50% delta would cross the 20% threshold.
	// With the fix, no escalation when prev < GC_JSONL_MIN_PREV_FOR_SPIKE
	// (default 10).
	cityDir := t.TempDir()
	binDir := t.TempDir()
	stateDir := t.TempDir()
	gcLog := filepath.Join(t.TempDir(), "gc.log")
	mailLog := filepath.Join(t.TempDir(), "gc-mail.log")
	archiveRepo := filepath.Join(cityDir, "archive")

	initSeedArchive(t, archiveRepo, 2)
	writeMultiRecordDoltStub(t, binDir, 1)
	writeJsonlExportGCStub(t, binDir)

	env := jsonlExportEnv(t, cityDir, binDir, stateDir, archiveRepo, gcLog, mailLog)

	runScript(t, filepath.Join(exampleDir(), "packs", "maintenance", "assets", "scripts", "jsonl-export.sh"), env)

	mailData, err := os.ReadFile(mailLog)
	if err != nil && !os.IsNotExist(err) {
		t.Fatalf("ReadFile(mail log): %v", err)
	}
	if strings.Contains(string(mailData), "ESCALATION: JSONL spike") {
		t.Fatalf("spike escalation fired despite prev<MIN_PREV; mail log:\n%s", mailData)
	}
}

func TestJsonlExportCommitsOnHaltToAdvanceBaseline(t *testing.T) {
	// Bug 3 (#1547): HALT path skipped `git commit`, so PREV_COUNT was frozen
	// and the spike re-fired every cooldown. With the fix, HALT still commits
	// the new file (baseline advances) and tags the commit `[HALT]`, but skips
	// `git push`.
	cityDir := t.TempDir()
	binDir := t.TempDir()
	stateDir := t.TempDir()
	gcLog := filepath.Join(t.TempDir(), "gc.log")
	mailLog := filepath.Join(t.TempDir(), "gc-mail.log")
	archiveRepo := filepath.Join(cityDir, "archive")

	prevHead := initSeedArchive(t, archiveRepo, 100)
	writeMultiRecordDoltStub(t, binDir, 10)
	writeJsonlExportGCStub(t, binDir)

	env := jsonlExportEnv(t, cityDir, binDir, stateDir, archiveRepo, gcLog, mailLog)

	runScript(t, filepath.Join(exampleDir(), "packs", "maintenance", "assets", "scripts", "jsonl-export.sh"), env)

	// Sanity: the spike (90% drop, prev=100, current=10) was escalated.
	mailData, err := os.ReadFile(mailLog)
	if err != nil && !os.IsNotExist(err) {
		t.Fatalf("ReadFile(mail log): %v", err)
	}
	if !strings.Contains(string(mailData), "ESCALATION: JSONL spike") {
		t.Fatalf("expected spike escalation as preconditions for the HALT-baseline test; mail log:\n%s", mailData)
	}

	// Baseline must advance: HEAD past the seed.
	revOut, err := exec.Command("git", "-C", archiveRepo, "rev-parse", "HEAD").CombinedOutput()
	if err != nil {
		t.Fatalf("git rev-parse: %v\n%s", err, revOut)
	}
	newHead := strings.TrimSpace(string(revOut))
	if newHead == prevHead {
		t.Fatalf("HEAD did not advance after HALT; baseline is still frozen at %s", prevHead)
	}

	// Commit message tagged [HALT] so operators reading the archive log can
	// distinguish baseline-only commits from full backups.
	logOut, err := exec.Command("git", "-C", archiveRepo, "log", "-1", "--format=%s").CombinedOutput()
	if err != nil {
		t.Fatalf("git log: %v\n%s", err, logOut)
	}
	headMsg := strings.TrimSpace(string(logOut))
	if !strings.Contains(headMsg, "HALT") {
		t.Fatalf("HALT-baseline commit must include HALT marker; got: %q", headMsg)
	}

	// The DOG_DONE summary on HALT should be the spike-halt nudge, not the
	// regular exported/records/push summary line.
	gcData, err := os.ReadFile(gcLog)
	if err != nil {
		t.Fatalf("ReadFile(gc log): %v", err)
	}
	if !strings.Contains(string(gcData), "HALTED on spike detection") {
		t.Fatalf("expected HALT nudge in gc log:\n%s", gcData)
	}
	if strings.Contains(string(gcData), "DOG_DONE: jsonl — exported") {
		t.Fatalf("HALT path must not emit the success summary nudge; gc log:\n%s", gcData)
	}
}

func TestJsonlExportFirstRunWithDisabledFloorSkipsSpikeCheck(t *testing.T) {
	// Regression: GC_JSONL_MIN_PREV_FOR_SPIKE=0 is documented as "disable the
	// floor", but combined with a first run (no archive yet → PREV_COUNT=0)
	// the spike calculation would divide by zero and `set -e` would kill the
	// script. The guard must skip the spike check when PREV_COUNT == 0
	// regardless of the floor setting.
	cityDir := t.TempDir()
	binDir := t.TempDir()
	stateDir := t.TempDir()
	gcLog := filepath.Join(t.TempDir(), "gc.log")
	mailLog := filepath.Join(t.TempDir(), "gc-mail.log")
	archiveRepo := filepath.Join(cityDir, "archive")

	// No initSeedArchive call — first run, archive does not yet exist.
	writeMultiRecordDoltStub(t, binDir, 5)
	writeJsonlExportGCStub(t, binDir)

	env := jsonlExportEnv(t, cityDir, binDir, stateDir, archiveRepo, gcLog, mailLog)
	env["GC_JSONL_MIN_PREV_FOR_SPIKE"] = "0"

	runScript(t, filepath.Join(exampleDir(), "packs", "maintenance", "assets", "scripts", "jsonl-export.sh"), env)

	// Should not have escalated (no prior baseline).
	if mailData, _ := os.ReadFile(mailLog); strings.Contains(string(mailData), "ESCALATION: JSONL spike") {
		t.Fatalf("first run with disabled floor must not escalate; mail log:\n%s", mailData)
	}
	// Sanity: the success summary nudge fired (script reached the end).
	gcData, err := os.ReadFile(gcLog)
	if err != nil {
		t.Fatalf("ReadFile(gc log): %v", err)
	}
	if !strings.Contains(string(gcData), "DOG_DONE: jsonl") {
		t.Fatalf("expected DOG_DONE nudge in gc log:\n%s", gcData)
	}
}

func TestJsonlExportScrubTrueFiltersRowsWithoutDroppingWholePayload(t *testing.T) {
	cityDir := t.TempDir()
	binDir := t.TempDir()
	stateDir := t.TempDir()
	gcLog := filepath.Join(t.TempDir(), "gc.log")
	mailLog := filepath.Join(t.TempDir(), "gc-mail.log")
	archiveRepo := filepath.Join(cityDir, "archive")

	initSeedArchive(t, archiveRepo, 12)
	rows := make([]string, 0, 13)
	rows = append(rows, `{"id":"bd-100","title":"real-leading-prefix"}`)
	for i := 1; i < 12; i++ {
		rows = append(rows, fmt.Sprintf(`{"id":"prod-%d","title":"real-%d"}`, i, i))
	}
	rows = append(rows, `{"id":"prod-test","title":"Test Issue 99"}`)
	writeIssueRowsDoltStub(t, binDir, rows)
	writeJsonlExportGCStub(t, binDir)

	env := jsonlExportEnv(t, cityDir, binDir, stateDir, archiveRepo, gcLog, mailLog)
	env["GC_JSONL_SCRUB"] = "true"

	runScript(t, filepath.Join(exampleDir(), "packs", "maintenance", "assets", "scripts", "jsonl-export.sh"), env)

	mailData, err := os.ReadFile(mailLog)
	if err != nil && !os.IsNotExist(err) {
		t.Fatalf("ReadFile(mail log): %v", err)
	}
	if strings.Contains(string(mailData), "ESCALATION: JSONL spike") {
		t.Fatalf("row-level scrub should preserve legitimate rows and avoid false spikes; mail log:\n%s", mailData)
	}

	exported, err := os.ReadFile(filepath.Join(archiveRepo, "beads", "issues.jsonl"))
	if err != nil {
		t.Fatalf("ReadFile(issues.jsonl): %v", err)
	}
	if got := strings.Count(string(exported), `"id":`); got != 12 {
		t.Fatalf("expected scrubbed export to retain 12 legitimate rows, got %d rows:\n%s", got, exported)
	}
	if !strings.Contains(string(exported), `"id":"bd-100"`) {
		t.Fatalf("expected scrubbed export to preserve the legitimate bd-100 row, got:\n%s", exported)
	}
	if strings.Contains(string(exported), "Test Issue 99") {
		t.Fatalf("expected scrubbed export to remove the test row, got:\n%s", exported)
	}

	legacyExported, err := os.ReadFile(filepath.Join(archiveRepo, "beads.jsonl"))
	if err != nil {
		t.Fatalf("ReadFile(beads.jsonl): %v", err)
	}
	if got := strings.Count(string(legacyExported), `"id":`); got != 12 {
		t.Fatalf("expected legacy flat export to retain 12 legitimate rows, got %d rows:\n%s", got, legacyExported)
	}
	if !strings.Contains(string(legacyExported), `"id":"bd-100"`) {
		t.Fatalf("expected legacy flat export to preserve the legitimate bd-100 row, got:\n%s", legacyExported)
	}
	if strings.Contains(string(legacyExported), "Test Issue 99") {
		t.Fatalf("expected legacy flat export to remove the test row, got:\n%s", legacyExported)
	}

	gcData, err := os.ReadFile(gcLog)
	if err != nil {
		t.Fatalf("ReadFile(gc log): %v", err)
	}
	if !strings.Contains(string(gcData), "records: 12") {
		t.Fatalf("expected DOG_DONE summary to report the scrubbed record count, got:\n%s", gcData)
	}
}

func TestJsonlExportHaltCommitAdvancesBaselineWithoutLocalGitIdentity(t *testing.T) {
	cityDir := t.TempDir()
	binDir := t.TempDir()
	stateDir := t.TempDir()
	gcLog := filepath.Join(t.TempDir(), "gc.log")
	mailLog := filepath.Join(t.TempDir(), "gc-mail.log")
	archiveRepo := filepath.Join(cityDir, "archive")

	prevHead := initSeedArchiveWithoutLocalIdentity(t, archiveRepo, 100)
	writeMultiRecordDoltStub(t, binDir, 10)
	writeJsonlExportGCStub(t, binDir)

	env := jsonlExportEnv(t, cityDir, binDir, stateDir, archiveRepo, gcLog, mailLog)

	runScript(t, filepath.Join(exampleDir(), "packs", "maintenance", "assets", "scripts", "jsonl-export.sh"), env)

	revOut, revErr := exec.Command("git", "-C", archiveRepo, "rev-parse", "HEAD").CombinedOutput()
	if revErr != nil {
		t.Fatalf("git rev-parse: %v\n%s", revErr, revOut)
	}
	if newHead := strings.TrimSpace(string(revOut)); newHead == prevHead {
		t.Fatalf("HEAD did not advance without repo-local git identity; baseline stayed frozen at %s", prevHead)
	}

	gcData, readErr := os.ReadFile(gcLog)
	if readErr != nil && !os.IsNotExist(readErr) {
		t.Fatalf("ReadFile(gc log): %v", readErr)
	}
	if !strings.Contains(string(gcData), "HALTED on spike detection") {
		t.Fatalf("expected HALT success nudge after baseline advance, got:\n%s", gcData)
	}
}

func TestJsonlExportDeletedHeadBaselineSkipsPreviousCountLookup(t *testing.T) {
	cityDir := t.TempDir()
	binDir := t.TempDir()
	stateDir := t.TempDir()
	gcLog := filepath.Join(t.TempDir(), "gc.log")
	mailLog := filepath.Join(t.TempDir(), "gc-mail.log")
	archiveRepo := filepath.Join(cityDir, "archive")

	initSeedArchive(t, archiveRepo, 3)
	steps := [][]string{
		{"rm", "-q", "beads/issues.jsonl"},
		{"commit", "-q", "-m", "delete issues baseline"},
	}
	for _, args := range steps {
		cmd := exec.Command("git", append([]string{"-C", archiveRepo}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}

	writeMultiRecordDoltStub(t, binDir, 5)
	writeJsonlExportGCStub(t, binDir)

	env := jsonlExportEnv(t, cityDir, binDir, stateDir, archiveRepo, gcLog, mailLog)

	runScript(t, filepath.Join(exampleDir(), "packs", "maintenance", "assets", "scripts", "jsonl-export.sh"), env)

	if mailData, _ := os.ReadFile(mailLog); strings.Contains(string(mailData), "ESCALATION: JSONL spike") {
		t.Fatalf("deleted HEAD baseline should behave like no baseline; mail log:\n%s", mailData)
	}
	gcData, err := os.ReadFile(gcLog)
	if err != nil {
		t.Fatalf("ReadFile(gc log): %v", err)
	}
	if !strings.Contains(string(gcData), "DOG_DONE: jsonl") {
		t.Fatalf("expected DOG_DONE summary after deleted HEAD baseline, got:\n%s", gcData)
	}
}

func TestJsonlExportScrubFailureDoesNotCommitBrokenOutputs(t *testing.T) {
	cityDir := t.TempDir()
	binDir := t.TempDir()
	stateDir := t.TempDir()
	gcLog := filepath.Join(t.TempDir(), "gc.log")
	mailLog := filepath.Join(t.TempDir(), "gc-mail.log")
	archiveRepo := filepath.Join(cityDir, "archive")

	prevHead := initSeedArchive(t, archiveRepo, 3)
	writeIssuesPayloadDoltStub(t, binDir, `{bad json`)
	writeJsonlExportGCStub(t, binDir)

	env := jsonlExportEnv(t, cityDir, binDir, stateDir, archiveRepo, gcLog, mailLog)
	env["GC_JSONL_SCRUB"] = "true"

	runScript(t, filepath.Join(exampleDir(), "packs", "maintenance", "assets", "scripts", "jsonl-export.sh"), env)

	revOut, err := exec.Command("git", "-C", archiveRepo, "rev-parse", "HEAD").CombinedOutput()
	if err != nil {
		t.Fatalf("git rev-parse: %v\n%s", err, revOut)
	}
	if newHead := strings.TrimSpace(string(revOut)); newHead != prevHead {
		t.Fatalf("scrub failure must not advance HEAD: got %s want %s", newHead, prevHead)
	}

	statusOut, err := exec.Command("git", "-C", archiveRepo, "status", "--short").CombinedOutput()
	if err != nil {
		t.Fatalf("git status: %v\n%s", err, statusOut)
	}
	if strings.TrimSpace(string(statusOut)) != "" {
		t.Fatalf("scrub failure must leave the archive worktree clean, got:\n%s", statusOut)
	}

	gcData, err := os.ReadFile(gcLog)
	if err != nil {
		t.Fatalf("ReadFile(gc log): %v", err)
	}
	if !strings.Contains(string(gcData), "failed: beads ") {
		t.Fatalf("expected scrub failure to report failed dbs, got:\n%s", gcData)
	}
}

func TestJsonlExportMalformedPayloadWithoutScrubDoesNotCommitBrokenOutputs(t *testing.T) {
	cityDir := t.TempDir()
	binDir := t.TempDir()
	stateDir := t.TempDir()
	gcLog := filepath.Join(t.TempDir(), "gc.log")
	mailLog := filepath.Join(t.TempDir(), "gc-mail.log")
	archiveRepo := filepath.Join(cityDir, "archive")

	prevHead := initSeedArchive(t, archiveRepo, 3)
	writeIssuesPayloadDoltStub(t, binDir, `{bad json`)
	writeJsonlExportGCStub(t, binDir)

	env := jsonlExportEnv(t, cityDir, binDir, stateDir, archiveRepo, gcLog, mailLog)
	env["GC_JSONL_SCRUB"] = "false"

	runScript(t, filepath.Join(exampleDir(), "packs", "maintenance", "assets", "scripts", "jsonl-export.sh"), env)

	revOut, err := exec.Command("git", "-C", archiveRepo, "rev-parse", "HEAD").CombinedOutput()
	if err != nil {
		t.Fatalf("git rev-parse: %v\n%s", err, revOut)
	}
	if newHead := strings.TrimSpace(string(revOut)); newHead != prevHead {
		t.Fatalf("malformed payload without scrub must not advance HEAD: got %s want %s", newHead, prevHead)
	}

	statusOut, err := exec.Command("git", "-C", archiveRepo, "status", "--short").CombinedOutput()
	if err != nil {
		t.Fatalf("git status: %v\n%s", err, statusOut)
	}
	if strings.TrimSpace(string(statusOut)) != "" {
		t.Fatalf("malformed payload without scrub must leave the archive worktree clean, got:\n%s", statusOut)
	}

	gcData, err := os.ReadFile(gcLog)
	if err != nil {
		t.Fatalf("ReadFile(gc log): %v", err)
	}
	if !strings.Contains(string(gcData), "failed: beads ") {
		t.Fatalf("expected malformed payload without scrub to report failed dbs, got:\n%s", gcData)
	}
}

func TestJsonlExportHaltStagingFailureExitsWithoutAdvancingBaseline(t *testing.T) {
	cityDir := t.TempDir()
	binDir := t.TempDir()
	stateDir := t.TempDir()
	gcLog := filepath.Join(t.TempDir(), "gc.log")
	mailLog := filepath.Join(t.TempDir(), "gc-mail.log")
	archiveRepo := filepath.Join(cityDir, "archive")

	prevHead := initSeedArchive(t, archiveRepo, 100)
	writeMultiRecordDoltStub(t, binDir, 10)
	writeJsonlExportGCStub(t, binDir)

	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatalf("LookPath(git): %v", err)
	}
	writeGitSubcommandFailureStub(t, binDir, realGit, "add")

	env := jsonlExportEnv(t, cityDir, binDir, stateDir, archiveRepo, gcLog, mailLog)

	out, runErr := runScriptResult(t, filepath.Join(exampleDir(), "packs", "maintenance", "assets", "scripts", "jsonl-export.sh"), env)
	if runErr == nil {
		t.Fatalf("expected script to fail when git add fails on HALT path; output:\n%s", out)
	}
	if !strings.Contains(string(out), "staging archive outputs failed") {
		t.Fatalf("expected staging failure diagnostic, got:\n%s", out)
	}

	revOut, err := exec.Command("git", "-C", archiveRepo, "rev-parse", "HEAD").CombinedOutput()
	if err != nil {
		t.Fatalf("git rev-parse: %v\n%s", err, revOut)
	}
	if newHead := strings.TrimSpace(string(revOut)); newHead != prevHead {
		t.Fatalf("staging failure must not advance HEAD: got %s want %s", newHead, prevHead)
	}

	statusOut, err := exec.Command("git", "-C", archiveRepo, "status", "--short").CombinedOutput()
	if err != nil {
		t.Fatalf("git status: %v\n%s", err, statusOut)
	}
	if strings.TrimSpace(string(statusOut)) != "" {
		t.Fatalf("staging failure must leave the archive worktree clean, got:\n%s", statusOut)
	}
}

func TestJsonlExportHaltCommitFailureLeavesArchiveClean(t *testing.T) {
	cityDir := t.TempDir()
	binDir := t.TempDir()
	stateDir := t.TempDir()
	gcLog := filepath.Join(t.TempDir(), "gc.log")
	mailLog := filepath.Join(t.TempDir(), "gc-mail.log")
	archiveRepo := filepath.Join(cityDir, "archive")

	prevHead := initSeedArchive(t, archiveRepo, 100)
	writeMultiRecordDoltStub(t, binDir, 10)
	writeJsonlExportGCStub(t, binDir)

	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatalf("LookPath(git): %v", err)
	}
	writeGitSubcommandFailureStub(t, binDir, realGit, "commit")

	env := jsonlExportEnv(t, cityDir, binDir, stateDir, archiveRepo, gcLog, mailLog)

	out, runErr := runScriptResult(t, filepath.Join(exampleDir(), "packs", "maintenance", "assets", "scripts", "jsonl-export.sh"), env)
	if runErr == nil {
		t.Fatalf("expected script to fail when git commit fails on HALT path; output:\n%s", out)
	}
	if !strings.Contains(string(out), "HALT baseline commit failed") {
		t.Fatalf("expected commit failure diagnostic, got:\n%s", out)
	}

	revOut, err := exec.Command("git", "-C", archiveRepo, "rev-parse", "HEAD").CombinedOutput()
	if err != nil {
		t.Fatalf("git rev-parse: %v\n%s", err, revOut)
	}
	if newHead := strings.TrimSpace(string(revOut)); newHead != prevHead {
		t.Fatalf("commit failure must not advance HEAD: got %s want %s", newHead, prevHead)
	}

	statusOut, err := exec.Command("git", "-C", archiveRepo, "status", "--short").CombinedOutput()
	if err != nil {
		t.Fatalf("git status: %v\n%s", err, statusOut)
	}
	if strings.TrimSpace(string(statusOut)) != "" {
		t.Fatalf("commit failure must leave the archive worktree clean, got:\n%s", statusOut)
	}
}

func TestJsonlExportHaltMailFailurePersistsPendingAlertAndRetriesNextRun(t *testing.T) {
	cityDir := t.TempDir()
	binDir := t.TempDir()
	stateDir := t.TempDir()
	gcLog := filepath.Join(t.TempDir(), "gc.log")
	mailLog := filepath.Join(t.TempDir(), "gc-mail.log")
	archiveRepo := filepath.Join(cityDir, "archive")
	stateFile := filepath.Join(stateDir, "jsonl-export-state.json")

	initSeedArchive(t, archiveRepo, 100)
	writeMultiRecordDoltStub(t, binDir, 10)
	writeJsonlExportGCStubWithMailExitCode(t, binDir, 1)

	env := jsonlExportEnv(t, cityDir, binDir, stateDir, archiveRepo, gcLog, mailLog)

	runScript(t, filepath.Join(exampleDir(), "packs", "maintenance", "assets", "scripts", "jsonl-export.sh"), env)

	stateData, err := os.ReadFile(stateFile)
	if err != nil {
		t.Fatalf("ReadFile(state file): %v", err)
	}
	var state map[string]any
	if err := json.Unmarshal(stateData, &state); err != nil {
		t.Fatalf("Unmarshal(state file): %v\n%s", err, stateData)
	}
	pendingAlerts, ok := state["pending_spike_alerts"].(map[string]any)
	if !ok {
		t.Fatalf("expected pending_spike_alerts after mail failure, got:\n%s", stateData)
	}
	if _, ok := pendingAlerts["beads"]; !ok {
		t.Fatalf("expected beads pending alert after mail failure, got:\n%s", stateData)
	}

	mailData, err := os.ReadFile(mailLog)
	if err != nil {
		t.Fatalf("ReadFile(mail log): %v", err)
	}
	if !strings.Contains(string(mailData), "ESCALATION: JSONL spike") {
		t.Fatalf("expected initial failed mail attempt to be logged, got:\n%s", mailData)
	}

	writeMultiRecordDoltStub(t, binDir, 10)
	writeJsonlExportGCStub(t, binDir)

	runScript(t, filepath.Join(exampleDir(), "packs", "maintenance", "assets", "scripts", "jsonl-export.sh"), env)

	stateData, err = os.ReadFile(stateFile)
	if err != nil {
		t.Fatalf("ReadFile(state file): %v", err)
	}
	if strings.Contains(string(stateData), `"pending_spike_alert"`) {
		t.Fatalf("expected pending spike alert to clear after retry, got:\n%s", stateData)
	}

	mailData, err = os.ReadFile(mailLog)
	if err != nil {
		t.Fatalf("ReadFile(mail log): %v", err)
	}
	if got := strings.Count(string(mailData), "ESCALATION: JSONL spike"); got != 2 {
		t.Fatalf("expected one failed attempt and one retry delivery, got %d entries:\n%s", got, mailData)
	}
}

func TestJsonlExportNoChangePushesPendingArchiveCommitAfterHalt(t *testing.T) {
	cityDir := t.TempDir()
	binDir := t.TempDir()
	stateDir := t.TempDir()
	gcLog := filepath.Join(t.TempDir(), "gc.log")
	mailLog := filepath.Join(t.TempDir(), "gc-mail.log")
	archiveRepo := filepath.Join(cityDir, "archive")
	stateFile := filepath.Join(stateDir, "jsonl-export-state.json")

	remoteRepo, remoteHead := initSeedArchiveWithRemote(t, archiveRepo)
	writeMultiRecordDoltStub(t, binDir, 10)
	writeJsonlExportGCStub(t, binDir)

	env := jsonlExportEnv(t, cityDir, binDir, stateDir, archiveRepo, gcLog, mailLog)

	runScript(t, filepath.Join(exampleDir(), "packs", "maintenance", "assets", "scripts", "jsonl-export.sh"), env)

	localHead, err := exec.Command("git", "-C", archiveRepo, "rev-parse", "HEAD").CombinedOutput()
	if err != nil {
		t.Fatalf("git rev-parse local HEAD: %v\n%s", err, localHead)
	}
	localHaltHead := strings.TrimSpace(string(localHead))

	remoteHeadOut, err := exec.Command("git", "--git-dir", remoteRepo, "rev-parse", "refs/heads/main").CombinedOutput()
	if err != nil {
		t.Fatalf("git rev-parse remote main after halt: %v\n%s", err, remoteHeadOut)
	}
	if got := strings.TrimSpace(string(remoteHeadOut)); got != remoteHead {
		t.Fatalf("HALT run must not push remote main: got %s want %s", got, remoteHead)
	}
	if localHaltHead == remoteHead {
		t.Fatalf("HALT run must create a local-only commit")
	}

	stateData, err := os.ReadFile(stateFile)
	if err != nil {
		t.Fatalf("ReadFile(state file): %v", err)
	}
	if !strings.Contains(string(stateData), `"pending_archive_push":true`) {
		t.Fatalf("expected pending_archive_push after HALT, got:\n%s", stateData)
	}

	runScript(t, filepath.Join(exampleDir(), "packs", "maintenance", "assets", "scripts", "jsonl-export.sh"), env)

	remoteHeadOut, err = exec.Command("git", "--git-dir", remoteRepo, "rev-parse", "refs/heads/main").CombinedOutput()
	if err != nil {
		t.Fatalf("git rev-parse remote main after retry: %v\n%s", err, remoteHeadOut)
	}
	if got := strings.TrimSpace(string(remoteHeadOut)); got != localHaltHead {
		t.Fatalf("expected no-change run to push pending local commit: got %s want %s", got, localHaltHead)
	}

	stateData, err = os.ReadFile(stateFile)
	if err != nil {
		t.Fatalf("ReadFile(state file): %v", err)
	}
	if strings.Contains(string(stateData), `"pending_archive_push":true`) {
		t.Fatalf("expected pending_archive_push to clear after push, got:\n%s", stateData)
	}
}

func TestJsonlExportNoChangePushesPendingArchiveCommitWithoutPendingState(t *testing.T) {
	cityDir := t.TempDir()
	binDir := t.TempDir()
	stateDir := t.TempDir()
	gcLog := filepath.Join(t.TempDir(), "gc.log")
	mailLog := filepath.Join(t.TempDir(), "gc-mail.log")
	archiveRepo := filepath.Join(cityDir, "archive")
	stateFile := filepath.Join(stateDir, "jsonl-export-state.json")

	remoteRepo, remoteHead := initSeedArchiveWithRemote(t, archiveRepo)
	writeMultiRecordDoltStub(t, binDir, 10)
	writeJsonlExportGCStub(t, binDir)

	env := jsonlExportEnv(t, cityDir, binDir, stateDir, archiveRepo, gcLog, mailLog)

	runScript(t, filepath.Join(exampleDir(), "packs", "maintenance", "assets", "scripts", "jsonl-export.sh"), env)

	localHead, err := exec.Command("git", "-C", archiveRepo, "rev-parse", "HEAD").CombinedOutput()
	if err != nil {
		t.Fatalf("git rev-parse local HEAD: %v\n%s", err, localHead)
	}
	localHaltHead := strings.TrimSpace(string(localHead))
	if localHaltHead == remoteHead {
		t.Fatalf("HALT run must create a local-only commit")
	}

	if err := os.WriteFile(stateFile, []byte("not-json\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(state file): %v", err)
	}

	runScript(t, filepath.Join(exampleDir(), "packs", "maintenance", "assets", "scripts", "jsonl-export.sh"), env)

	remoteHeadOut, err := exec.Command("git", "--git-dir", remoteRepo, "rev-parse", "refs/heads/main").CombinedOutput()
	if err != nil {
		t.Fatalf("git rev-parse remote main after replay: %v\n%s", err, remoteHeadOut)
	}
	if got := strings.TrimSpace(string(remoteHeadOut)); got != localHaltHead {
		t.Fatalf("expected git-state fallback to push stranded local commit: got %s want %s", got, localHaltHead)
	}
}

func TestJsonlExportNoUserDatabasesPushesPendingArchiveCommit(t *testing.T) {
	cityDir := t.TempDir()
	binDir := t.TempDir()
	stateDir := t.TempDir()
	gcLog := filepath.Join(t.TempDir(), "gc.log")
	mailLog := filepath.Join(t.TempDir(), "gc-mail.log")
	archiveRepo := filepath.Join(cityDir, "archive")
	stateFile := filepath.Join(stateDir, "jsonl-export-state.json")

	remoteRepo, remoteHead := initSeedArchiveWithRemote(t, archiveRepo)
	writeMultiRecordDoltStub(t, binDir, 10)
	writeJsonlExportGCStub(t, binDir)

	env := jsonlExportEnv(t, cityDir, binDir, stateDir, archiveRepo, gcLog, mailLog)

	runScript(t, filepath.Join(exampleDir(), "packs", "maintenance", "assets", "scripts", "jsonl-export.sh"), env)

	localHeadOut, err := exec.Command("git", "-C", archiveRepo, "rev-parse", "HEAD").CombinedOutput()
	if err != nil {
		t.Fatalf("git rev-parse local HALT HEAD: %v\n%s", err, localHeadOut)
	}
	localHaltHead := strings.TrimSpace(string(localHeadOut))
	if localHaltHead == remoteHead {
		t.Fatalf("HALT run must create a local-only commit")
	}

	writeNoUserDatabasesDoltStub(t, binDir)

	runScript(t, filepath.Join(exampleDir(), "packs", "maintenance", "assets", "scripts", "jsonl-export.sh"), env)

	remoteHeadOut, err := exec.Command("git", "--git-dir", remoteRepo, "rev-parse", "refs/heads/main").CombinedOutput()
	if err != nil {
		t.Fatalf("git rev-parse remote main after empty-db replay: %v\n%s", err, remoteHeadOut)
	}
	if got := strings.TrimSpace(string(remoteHeadOut)); got != localHaltHead {
		t.Fatalf("expected empty-db run to publish pending archive commit: got %s want %s", got, localHaltHead)
	}

	stateData, err := os.ReadFile(stateFile)
	if err != nil {
		t.Fatalf("ReadFile(state file): %v", err)
	}
	if strings.Contains(string(stateData), `"pending_archive_push":true`) {
		t.Fatalf("expected pending_archive_push to clear after empty-db replay, got:\n%s", stateData)
	}
}

func TestJsonlExportNoChangeRebasesPendingArchiveCommitOntoAdvancedRemote(t *testing.T) {
	cityDir := t.TempDir()
	binDir := t.TempDir()
	stateDir := t.TempDir()
	gcLog := filepath.Join(t.TempDir(), "gc.log")
	mailLog := filepath.Join(t.TempDir(), "gc-mail.log")
	archiveRepo := filepath.Join(cityDir, "archive")

	remoteRepo, _ := initSeedArchiveWithRemote(t, archiveRepo)
	writeMultiRecordDoltStub(t, binDir, 10)
	writeJsonlExportGCStub(t, binDir)

	env := jsonlExportEnv(t, cityDir, binDir, stateDir, archiveRepo, gcLog, mailLog)

	runScript(t, filepath.Join(exampleDir(), "packs", "maintenance", "assets", "scripts", "jsonl-export.sh"), env)

	localHeadBeforeReplay, err := exec.Command("git", "-C", archiveRepo, "rev-parse", "HEAD").CombinedOutput()
	if err != nil {
		t.Fatalf("git rev-parse local HALT HEAD: %v\n%s", err, localHeadBeforeReplay)
	}
	haltHead := strings.TrimSpace(string(localHeadBeforeReplay))

	advancedRemoteHead := advanceArchiveRemoteMain(t, remoteRepo)
	if advancedRemoteHead == haltHead {
		t.Fatalf("remote advance must create a new remote commit")
	}

	runScript(t, filepath.Join(exampleDir(), "packs", "maintenance", "assets", "scripts", "jsonl-export.sh"), env)

	localHeadAfterReplay, err := exec.Command("git", "-C", archiveRepo, "rev-parse", "HEAD").CombinedOutput()
	if err != nil {
		t.Fatalf("git rev-parse local replay HEAD: %v\n%s", err, localHeadAfterReplay)
	}
	replayedHead := strings.TrimSpace(string(localHeadAfterReplay))
	if replayedHead == haltHead {
		t.Fatalf("expected replay to rebase HALT commit onto advanced remote")
	}

	remoteHeadOut, err := exec.Command("git", "--git-dir", remoteRepo, "rev-parse", "refs/heads/main").CombinedOutput()
	if err != nil {
		t.Fatalf("git rev-parse remote main after replay: %v\n%s", err, remoteHeadOut)
	}
	if got := strings.TrimSpace(string(remoteHeadOut)); got != replayedHead {
		t.Fatalf("expected replayed local HEAD to publish after remote advance: got remote %s want local %s", got, replayedHead)
	}

	logOut, err := exec.Command("git", "--git-dir", remoteRepo, "log", "--format=%s", "-2", "refs/heads/main").CombinedOutput()
	if err != nil {
		t.Fatalf("git log remote main: %v\n%s", err, logOut)
	}
	remoteLog := string(logOut)
	if !strings.Contains(remoteLog, "remote advance") || !strings.Contains(remoteLog, "HALT") {
		t.Fatalf("expected remote history to contain both remote advance and replayed HALT commit, got:\n%s", remoteLog)
	}
}

func TestJsonlExportNoChangePushFailureWithMalformedStateUsesTrackingRef(t *testing.T) {
	cityDir := t.TempDir()
	binDir := t.TempDir()
	stateDir := t.TempDir()
	gcLog := filepath.Join(t.TempDir(), "gc.log")
	mailLog := filepath.Join(t.TempDir(), "gc-mail.log")
	archiveRepo := filepath.Join(cityDir, "archive")
	stateFile := filepath.Join(stateDir, "jsonl-export-state.json")

	remoteRepo, remoteHead := initSeedArchiveWithRemote(t, archiveRepo)
	writeMultiRecordDoltStub(t, binDir, 10)
	writeJsonlExportGCStub(t, binDir)

	env := jsonlExportEnv(t, cityDir, binDir, stateDir, archiveRepo, gcLog, mailLog)

	runScript(t, filepath.Join(exampleDir(), "packs", "maintenance", "assets", "scripts", "jsonl-export.sh"), env)

	localHeadOut, err := exec.Command("git", "-C", archiveRepo, "rev-parse", "HEAD").CombinedOutput()
	if err != nil {
		t.Fatalf("git rev-parse local HALT HEAD: %v\n%s", err, localHeadOut)
	}
	localHaltHead := strings.TrimSpace(string(localHeadOut))
	if localHaltHead == remoteHead {
		t.Fatalf("HALT run must create a local-only commit")
	}

	if err := os.WriteFile(stateFile, []byte("not-json\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(state file): %v", err)
	}

	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatalf("LookPath(git): %v", err)
	}
	writeGitSubcommandFailureStub(t, binDir, realGit, "fetch")

	runScript(t, filepath.Join(exampleDir(), "packs", "maintenance", "assets", "scripts", "jsonl-export.sh"), env)

	stateData, err := os.ReadFile(stateFile)
	if err != nil {
		t.Fatalf("ReadFile(state file): %v", err)
	}
	var state map[string]any
	if err := json.Unmarshal(stateData, &state); err != nil {
		t.Fatalf("Unmarshal(state file): %v\n%s", err, stateData)
	}
	if got := state["consecutive_push_failures"]; got != float64(1) {
		t.Fatalf("consecutive_push_failures = %v, want 1\nstate: %s", got, stateData)
	}
	if got := state["pending_archive_push"]; got != true {
		t.Fatalf("pending_archive_push = %v, want true\nstate: %s", got, stateData)
	}

	remoteHeadOut, err := exec.Command("git", "--git-dir", remoteRepo, "rev-parse", "refs/heads/main").CombinedOutput()
	if err != nil {
		t.Fatalf("git rev-parse remote main after failed replay: %v\n%s", err, remoteHeadOut)
	}
	if got := strings.TrimSpace(string(remoteHeadOut)); got != remoteHead {
		t.Fatalf("expected fetch failure to leave remote main unchanged: got %s want %s", got, remoteHead)
	}

	gcData, err := os.ReadFile(gcLog)
	if err != nil {
		t.Fatalf("ReadFile(gc log): %v", err)
	}
	if !strings.Contains(string(gcData), "push: failed") {
		t.Fatalf("expected replay failure to surface push failure summary, got:\n%s", gcData)
	}
}

func TestJsonlExportExportFailureDoesNotBlockPendingArchiveReplay(t *testing.T) {
	cityDir := t.TempDir()
	binDir := t.TempDir()
	stateDir := t.TempDir()
	gcLog := filepath.Join(t.TempDir(), "gc.log")
	mailLog := filepath.Join(t.TempDir(), "gc-mail.log")
	archiveRepo := filepath.Join(cityDir, "archive")
	stateFile := filepath.Join(stateDir, "jsonl-export-state.json")

	remoteRepo, remoteHead := initSeedArchiveWithRemote(t, archiveRepo)
	writeMultiRecordDoltStub(t, binDir, 10)
	writeJsonlExportGCStub(t, binDir)

	env := jsonlExportEnv(t, cityDir, binDir, stateDir, archiveRepo, gcLog, mailLog)

	runScript(t, filepath.Join(exampleDir(), "packs", "maintenance", "assets", "scripts", "jsonl-export.sh"), env)

	localHeadBeforeReplay, err := exec.Command("git", "-C", archiveRepo, "rev-parse", "HEAD").CombinedOutput()
	if err != nil {
		t.Fatalf("git rev-parse local HALT HEAD: %v\n%s", err, localHeadBeforeReplay)
	}
	haltHead := strings.TrimSpace(string(localHeadBeforeReplay))
	if haltHead == remoteHead {
		t.Fatalf("HALT run must create a local-only commit")
	}

	advancedRemoteHead := advanceArchiveRemoteMain(t, remoteRepo)
	if advancedRemoteHead == haltHead {
		t.Fatalf("remote advance must create a new remote commit")
	}

	writeIssuesExportFailureDoltStub(t, binDir)

	runScript(t, filepath.Join(exampleDir(), "packs", "maintenance", "assets", "scripts", "jsonl-export.sh"), env)

	localHeadAfterReplay, err := exec.Command("git", "-C", archiveRepo, "rev-parse", "HEAD").CombinedOutput()
	if err != nil {
		t.Fatalf("git rev-parse local replay HEAD: %v\n%s", err, localHeadAfterReplay)
	}
	replayedHead := strings.TrimSpace(string(localHeadAfterReplay))
	if replayedHead == haltHead {
		t.Fatalf("expected replay to rebase HALT commit onto advanced remote")
	}

	remoteHeadOut, err := exec.Command("git", "--git-dir", remoteRepo, "rev-parse", "refs/heads/main").CombinedOutput()
	if err != nil {
		t.Fatalf("git rev-parse remote main after replay: %v\n%s", err, remoteHeadOut)
	}
	if got := strings.TrimSpace(string(remoteHeadOut)); got != replayedHead {
		t.Fatalf("expected replayed local HEAD to publish after export failure: got remote %s want local %s", got, replayedHead)
	}

	statusOut, err := exec.Command("git", "-C", archiveRepo, "status", "--short").CombinedOutput()
	if err != nil {
		t.Fatalf("git status: %v\n%s", err, statusOut)
	}
	if strings.TrimSpace(string(statusOut)) != "" {
		t.Fatalf("export failure must leave the archive worktree clean, got:\n%s", statusOut)
	}

	stateData, err := os.ReadFile(stateFile)
	if err != nil {
		t.Fatalf("ReadFile(state file): %v", err)
	}
	if strings.Contains(string(stateData), `"pending_archive_push":true`) {
		t.Fatalf("expected pending_archive_push to clear after replay, got:\n%s", stateData)
	}
}

func TestJsonlExportPushBootstrapCreatesRemoteMainWhenMissing(t *testing.T) {
	cityDir := t.TempDir()
	binDir := t.TempDir()
	stateDir := t.TempDir()
	gcLog := filepath.Join(t.TempDir(), "gc.log")
	mailLog := filepath.Join(t.TempDir(), "gc-mail.log")
	archiveRepo := filepath.Join(cityDir, "archive")
	stateFile := filepath.Join(stateDir, "jsonl-export-state.json")

	remoteRepo := initEmptyArchiveRemote(t, archiveRepo, 3)
	writeMultiRecordDoltStub(t, binDir, 5)
	writeJsonlExportGCStub(t, binDir)

	env := jsonlExportEnv(t, cityDir, binDir, stateDir, archiveRepo, gcLog, mailLog)

	runScript(t, filepath.Join(exampleDir(), "packs", "maintenance", "assets", "scripts", "jsonl-export.sh"), env)

	localHeadOut, err := exec.Command("git", "-C", archiveRepo, "rev-parse", "HEAD").CombinedOutput()
	if err != nil {
		t.Fatalf("git rev-parse local HEAD: %v\n%s", err, localHeadOut)
	}
	localHead := strings.TrimSpace(string(localHeadOut))

	remoteHeadOut, err := exec.Command("git", "--git-dir", remoteRepo, "rev-parse", "refs/heads/main").CombinedOutput()
	if err != nil {
		t.Fatalf("git rev-parse remote main: %v\n%s", err, remoteHeadOut)
	}
	if got := strings.TrimSpace(string(remoteHeadOut)); got != localHead {
		t.Fatalf("expected bootstrap push to publish local HEAD: got remote %s want local %s", got, localHead)
	}

	stateData, err := os.ReadFile(stateFile)
	if err != nil {
		t.Fatalf("ReadFile(state file): %v", err)
	}
	if strings.Contains(string(stateData), `"pending_archive_push":true`) {
		t.Fatalf("expected pending_archive_push to clear after bootstrap push, got:\n%s", stateData)
	}
}

func TestJsonlExportLegacyStateBackupRecoversPendingArchiveReplay(t *testing.T) {
	cityDir := t.TempDir()
	binDir := t.TempDir()
	stateDir := t.TempDir()
	gcLog := filepath.Join(t.TempDir(), "gc.log")
	mailLog := filepath.Join(t.TempDir(), "gc-mail.log")
	archiveRepo := filepath.Join(cityDir, "archive")
	legacyStateFile := filepath.Join(cityDir, ".gc", "jsonl-export-state.json")

	remoteRepo, remoteHead := initSeedArchiveWithRemote(t, archiveRepo)
	writeMultiRecordDoltStub(t, binDir, 10)
	writeJsonlExportGCStub(t, binDir)

	if err := os.MkdirAll(filepath.Dir(legacyStateFile), 0o755); err != nil {
		t.Fatalf("MkdirAll(legacy state dir): %v", err)
	}
	if err := os.WriteFile(legacyStateFile, []byte("{}\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(legacy state file): %v", err)
	}

	env := jsonlExportEnv(t, cityDir, binDir, stateDir, archiveRepo, gcLog, mailLog)

	runScript(t, filepath.Join(exampleDir(), "packs", "maintenance", "assets", "scripts", "jsonl-export.sh"), env)

	localHeadOut, err := exec.Command("git", "-C", archiveRepo, "rev-parse", "HEAD").CombinedOutput()
	if err != nil {
		t.Fatalf("git rev-parse local HALT HEAD: %v\n%s", err, localHeadOut)
	}
	localHaltHead := strings.TrimSpace(string(localHeadOut))
	if localHaltHead == remoteHead {
		t.Fatalf("HALT run must create a local-only commit")
	}

	backupData, err := os.ReadFile(legacyStateFile + ".bak")
	if err != nil {
		t.Fatalf("ReadFile(legacy state backup): %v", err)
	}
	if !strings.Contains(string(backupData), `"pending_archive_push":true`) {
		t.Fatalf("expected legacy backup to preserve pending archive push, got:\n%s", backupData)
	}

	if err := os.WriteFile(legacyStateFile, []byte("not-json\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(legacy state file): %v", err)
	}

	writeNoUserDatabasesDoltStub(t, binDir)

	runScript(t, filepath.Join(exampleDir(), "packs", "maintenance", "assets", "scripts", "jsonl-export.sh"), env)

	remoteHeadOut, err := exec.Command("git", "--git-dir", remoteRepo, "rev-parse", "refs/heads/main").CombinedOutput()
	if err != nil {
		t.Fatalf("git rev-parse remote main after replay: %v\n%s", err, remoteHeadOut)
	}
	if got := strings.TrimSpace(string(remoteHeadOut)); got != localHaltHead {
		t.Fatalf("expected legacy backup replay to publish pending archive commit: got %s want %s", got, localHaltHead)
	}

	stateData, err := os.ReadFile(legacyStateFile)
	if err != nil {
		t.Fatalf("ReadFile(legacy state file): %v", err)
	}
	if strings.Contains(string(stateData), `"pending_archive_push":true`) {
		t.Fatalf("expected legacy pending_archive_push to clear after replay, got:\n%s", stateData)
	}
}

func TestJsonlExportEmptyIssuesPayloadDoesNotCommitBrokenOutputs(t *testing.T) {
	cityDir := t.TempDir()
	binDir := t.TempDir()
	stateDir := t.TempDir()
	gcLog := filepath.Join(t.TempDir(), "gc.log")
	mailLog := filepath.Join(t.TempDir(), "gc-mail.log")
	archiveRepo := filepath.Join(cityDir, "archive")

	prevHead := initSeedArchive(t, archiveRepo, 3)
	writeEmptyIssuesPayloadDoltStub(t, binDir)
	writeJsonlExportGCStub(t, binDir)

	env := jsonlExportEnv(t, cityDir, binDir, stateDir, archiveRepo, gcLog, mailLog)
	env["GC_JSONL_SCRUB"] = "false"

	runScript(t, filepath.Join(exampleDir(), "packs", "maintenance", "assets", "scripts", "jsonl-export.sh"), env)

	revOut, err := exec.Command("git", "-C", archiveRepo, "rev-parse", "HEAD").CombinedOutput()
	if err != nil {
		t.Fatalf("git rev-parse: %v\n%s", err, revOut)
	}
	if newHead := strings.TrimSpace(string(revOut)); newHead != prevHead {
		t.Fatalf("empty payload must not advance HEAD: got %s want %s", newHead, prevHead)
	}

	statusOut, err := exec.Command("git", "-C", archiveRepo, "status", "--short").CombinedOutput()
	if err != nil {
		t.Fatalf("git status: %v\n%s", err, statusOut)
	}
	if strings.TrimSpace(string(statusOut)) != "" {
		t.Fatalf("empty payload must leave the archive worktree clean, got:\n%s", statusOut)
	}

	gcData, err := os.ReadFile(gcLog)
	if err != nil {
		t.Fatalf("ReadFile(gc log): %v", err)
	}
	if !strings.Contains(string(gcData), "failed: beads ") {
		t.Fatalf("expected empty payload to report failed dbs, got:\n%s", gcData)
	}
}

// TestJsonlExportEmptyDatabaseDoesNotAppearInFailedSummary is the regression
// test for #1898: an empty `issues` table in dolt produces `{}` (not
// `{"rows":[]}`) from `dolt sql -r json`. Before the fix in
// validate_exported_issues, that bare-object payload was rejected as malformed
// JSON and the database was logged in `failed:` even though nothing was wrong
// with it. After widening the type check (`.rows? // [] | type == "array"`),
// `{}` is treated as zero rows and the DB lands in the success path with an
// `issues.jsonl` committed to the archive.
func TestJsonlExportEmptyDatabaseDoesNotAppearInFailedSummary(t *testing.T) {
	cityDir := t.TempDir()
	binDir := t.TempDir()
	stateDir := t.TempDir()
	gcLog := filepath.Join(t.TempDir(), "gc.log")
	mailLog := filepath.Join(t.TempDir(), "gc-mail.log")
	archiveRepo := filepath.Join(cityDir, "archive")

	initSeedArchive(t, archiveRepo, 0)
	// `{}` is the dolt-empty-result encoding the validator must accept.
	writeIssuesPayloadDoltStub(t, binDir, `{}`)
	writeJsonlExportGCStub(t, binDir)

	env := jsonlExportEnv(t, cityDir, binDir, stateDir, archiveRepo, gcLog, mailLog)

	runScript(t, filepath.Join(exampleDir(), "packs", "maintenance", "assets", "scripts", "jsonl-export.sh"), env)

	gcData, err := os.ReadFile(gcLog)
	if err != nil {
		t.Fatalf("ReadFile(gc log): %v", err)
	}
	log := string(gcData)
	if strings.Contains(log, "failed: beads") {
		t.Fatalf("empty issues table must not land in failed: summary; gc log:\n%s", log)
	}
	if !strings.Contains(log, "DOG_DONE: jsonl — exported 1/1") {
		t.Fatalf("expected success summary `exported 1/1`, got:\n%s", log)
	}

	// The DB should have an issues.jsonl committed in the archive, even though
	// the payload is the empty `{}` form.
	committed, err := exec.Command("git", "-C", archiveRepo, "show", "HEAD:beads/issues.jsonl").CombinedOutput()
	if err != nil {
		t.Fatalf("git show HEAD:beads/issues.jsonl: %v\n%s", err, committed)
	}
	if len(strings.TrimSpace(string(committed))) == 0 {
		t.Fatalf("expected issues.jsonl to be committed (even if empty rows), got empty file")
	}
}

func TestJsonlExportPushFailureRecoversFromMalformedState(t *testing.T) {
	cityDir := t.TempDir()
	binDir := t.TempDir()
	stateDir := t.TempDir()
	gcLog := filepath.Join(t.TempDir(), "gc.log")
	mailLog := filepath.Join(t.TempDir(), "gc-mail.log")
	archiveRepo := filepath.Join(cityDir, "archive")
	stateFile := filepath.Join(stateDir, "jsonl-export-state.json")

	initSeedArchiveWithUnreachableRemote(t, archiveRepo, 3)
	writeMultiRecordDoltStub(t, binDir, 5)
	writeJsonlExportGCStub(t, binDir)

	if err := os.WriteFile(stateFile, []byte("not-json\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(state file): %v", err)
	}

	env := jsonlExportEnv(t, cityDir, binDir, stateDir, archiveRepo, gcLog, mailLog)

	runScript(t, filepath.Join(exampleDir(), "packs", "maintenance", "assets", "scripts", "jsonl-export.sh"), env)

	stateData, err := os.ReadFile(stateFile)
	if err != nil {
		t.Fatalf("ReadFile(state file): %v", err)
	}
	var state map[string]any
	if err := json.Unmarshal(stateData, &state); err != nil {
		t.Fatalf("Unmarshal(state file): %v\n%s", err, stateData)
	}
	if got := state["consecutive_push_failures"]; got != float64(1) {
		t.Fatalf("consecutive_push_failures = %v, want 1\nstate: %s", got, stateData)
	}
}

func TestJsonlExportPushFailureRecoversFromWrongShapeState(t *testing.T) {
	cityDir := t.TempDir()
	binDir := t.TempDir()
	stateDir := t.TempDir()
	gcLog := filepath.Join(t.TempDir(), "gc.log")
	mailLog := filepath.Join(t.TempDir(), "gc-mail.log")
	archiveRepo := filepath.Join(cityDir, "archive")
	stateFile := filepath.Join(stateDir, "jsonl-export-state.json")

	initSeedArchiveWithUnreachableRemote(t, archiveRepo, 3)
	writeMultiRecordDoltStub(t, binDir, 5)
	writeJsonlExportGCStub(t, binDir)

	if err := os.WriteFile(stateFile, []byte("[]\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(state file): %v", err)
	}

	env := jsonlExportEnv(t, cityDir, binDir, stateDir, archiveRepo, gcLog, mailLog)

	runScript(t, filepath.Join(exampleDir(), "packs", "maintenance", "assets", "scripts", "jsonl-export.sh"), env)

	stateData, err := os.ReadFile(stateFile)
	if err != nil {
		t.Fatalf("ReadFile(state file): %v", err)
	}
	var state map[string]any
	if err := json.Unmarshal(stateData, &state); err != nil {
		t.Fatalf("Unmarshal(state file): %v\n%s", err, stateData)
	}
	if got := state["consecutive_push_failures"]; got != float64(1) {
		t.Fatalf("consecutive_push_failures = %v, want 1\nstate: %s", got, stateData)
	}
}

func TestJsonlExportHaltMailFailureRecoversFromMalformedState(t *testing.T) {
	cityDir := t.TempDir()
	binDir := t.TempDir()
	stateDir := t.TempDir()
	gcLog := filepath.Join(t.TempDir(), "gc.log")
	mailLog := filepath.Join(t.TempDir(), "gc-mail.log")
	archiveRepo := filepath.Join(cityDir, "archive")
	stateFile := filepath.Join(stateDir, "jsonl-export-state.json")

	initSeedArchive(t, archiveRepo, 100)
	writeMultiRecordDoltStub(t, binDir, 10)
	writeJsonlExportGCStubWithMailExitCode(t, binDir, 1)

	if err := os.WriteFile(stateFile, []byte("not-json\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(state file): %v", err)
	}

	env := jsonlExportEnv(t, cityDir, binDir, stateDir, archiveRepo, gcLog, mailLog)

	runScript(t, filepath.Join(exampleDir(), "packs", "maintenance", "assets", "scripts", "jsonl-export.sh"), env)

	stateData, err := os.ReadFile(stateFile)
	if err != nil {
		t.Fatalf("ReadFile(state file): %v", err)
	}
	state := map[string]any{}
	if err := json.Unmarshal(stateData, &state); err != nil {
		t.Fatalf("Unmarshal(state file): %v\n%s", err, stateData)
	}
	pendingAlerts, ok := state["pending_spike_alerts"].(map[string]any)
	if !ok {
		t.Fatalf("expected pending_spike_alerts map, got: %s", stateData)
	}
	pending, ok := pendingAlerts["beads"].(map[string]any)
	if !ok {
		t.Fatalf("expected beads pending alert entry, got: %s", stateData)
	}
	if got := pending["database"]; got != "beads" {
		t.Fatalf("pending_spike_alert.database = %v, want beads\nstate: %s", got, stateData)
	}
}

func TestJsonlExportRetriesPendingAlertFromBackupAfterPrimaryCorruption(t *testing.T) {
	cityDir := t.TempDir()
	binDir := t.TempDir()
	stateDir := t.TempDir()
	gcLog := filepath.Join(t.TempDir(), "gc.log")
	mailLog := filepath.Join(t.TempDir(), "gc-mail.log")
	archiveRepo := filepath.Join(cityDir, "archive")
	stateFile := filepath.Join(stateDir, "jsonl-export-state.json")

	initSeedArchiveWithRemote(t, archiveRepo)
	writeMultiRecordDoltStub(t, binDir, 10)
	writeJsonlExportGCStubWithMailExitCode(t, binDir, 1)

	env := jsonlExportEnv(t, cityDir, binDir, stateDir, archiveRepo, gcLog, mailLog)

	runScript(t, filepath.Join(exampleDir(), "packs", "maintenance", "assets", "scripts", "jsonl-export.sh"), env)

	backupData, err := os.ReadFile(stateFile + ".bak")
	if err != nil {
		t.Fatalf("ReadFile(state backup): %v", err)
	}
	if !strings.Contains(string(backupData), `"pending_spike_alerts"`) {
		t.Fatalf("expected backup state to preserve pending spike alert, got:\n%s", backupData)
	}
	if err := os.WriteFile(stateFile, []byte("not-json\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(state file): %v", err)
	}

	writeNoUserDatabasesDoltStub(t, binDir)
	writeJsonlExportGCStub(t, binDir)

	runScript(t, filepath.Join(exampleDir(), "packs", "maintenance", "assets", "scripts", "jsonl-export.sh"), env)

	mailData, err := os.ReadFile(mailLog)
	if err != nil {
		t.Fatalf("ReadFile(mail log): %v", err)
	}
	if got := strings.Count(string(mailData), "ESCALATION: JSONL spike"); got != 2 {
		t.Fatalf("expected failed attempt plus backup-backed retry, got %d entries:\n%s", got, mailData)
	}

	stateData, err := os.ReadFile(stateFile)
	if err != nil {
		t.Fatalf("ReadFile(state file): %v", err)
	}
	if strings.Contains(string(stateData), `"pending_spike_alert"`) {
		t.Fatalf("expected pending spike alert to clear after backup-backed retry, got:\n%s", stateData)
	}
}

func TestJsonlExportRetriesPendingAlertWithoutUserDatabases(t *testing.T) {
	cityDir := t.TempDir()
	binDir := t.TempDir()
	stateDir := t.TempDir()
	gcLog := filepath.Join(t.TempDir(), "gc.log")
	mailLog := filepath.Join(t.TempDir(), "gc-mail.log")
	archiveRepo := filepath.Join(cityDir, "archive")
	stateFile := filepath.Join(stateDir, "jsonl-export-state.json")

	writeNoUserDatabasesDoltStub(t, binDir)
	writeJsonlExportGCStub(t, binDir)

	if err := os.WriteFile(stateFile, []byte(`{"pending_spike_alert":{"database":"beads","prev_count":100,"current_count":10,"delta":90,"threshold":20}}`+"\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(state file): %v", err)
	}

	env := jsonlExportEnv(t, cityDir, binDir, stateDir, archiveRepo, gcLog, mailLog)

	runScript(t, filepath.Join(exampleDir(), "packs", "maintenance", "assets", "scripts", "jsonl-export.sh"), env)

	mailData, err := os.ReadFile(mailLog)
	if err != nil {
		t.Fatalf("ReadFile(mail log): %v", err)
	}
	if got := strings.Count(string(mailData), "ESCALATION: JSONL spike"); got != 1 {
		t.Fatalf("expected pending spike alert retry on empty-db run, got %d entries:\n%s", got, mailData)
	}

	stateData, err := os.ReadFile(stateFile)
	if err != nil {
		t.Fatalf("ReadFile(state file): %v", err)
	}
	if strings.Contains(string(stateData), `"pending_spike_alert"`) {
		t.Fatalf("expected pending spike alert to clear after retry, got:\n%s", stateData)
	}
}

func TestJsonlExportRetriesMultiplePendingAlertsWithoutUserDatabases(t *testing.T) {
	cityDir := t.TempDir()
	binDir := t.TempDir()
	stateDir := t.TempDir()
	gcLog := filepath.Join(t.TempDir(), "gc.log")
	mailLog := filepath.Join(t.TempDir(), "gc-mail.log")
	archiveRepo := filepath.Join(cityDir, "archive")
	stateFile := filepath.Join(stateDir, "jsonl-export-state.json")

	writeNoUserDatabasesDoltStub(t, binDir)
	writeJsonlExportGCStub(t, binDir)

	if err := os.WriteFile(stateFile, []byte(`{"pending_spike_alerts":{"alpha":{"database":"alpha","prev_count":100,"current_count":10,"delta":90,"threshold":20},"beta":{"database":"beta","prev_count":80,"current_count":20,"delta":75,"threshold":20}}}`+"\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(state file): %v", err)
	}

	env := jsonlExportEnv(t, cityDir, binDir, stateDir, archiveRepo, gcLog, mailLog)

	runScript(t, filepath.Join(exampleDir(), "packs", "maintenance", "assets", "scripts", "jsonl-export.sh"), env)

	mailData, err := os.ReadFile(mailLog)
	if err != nil {
		t.Fatalf("ReadFile(mail log): %v", err)
	}
	if got := strings.Count(string(mailData), "ESCALATION: JSONL spike"); got != 2 {
		t.Fatalf("expected both pending spike alerts to retry, got %d entries:\n%s", got, mailData)
	}
	if !strings.Contains(string(mailData), "Database: alpha") || !strings.Contains(string(mailData), "Database: beta") {
		t.Fatalf("expected both pending spike alerts to be delivered, got:\n%s", mailData)
	}

	stateData, err := os.ReadFile(stateFile)
	if err != nil {
		t.Fatalf("ReadFile(state file): %v", err)
	}
	if strings.Contains(string(stateData), `"pending_spike_alert"`) {
		t.Fatalf("expected all pending spike alerts to clear after retry, got:\n%s", stateData)
	}
}

func TestJsonlExportHaltMailFailurePreservesExistingPendingAlerts(t *testing.T) {
	cityDir := t.TempDir()
	binDir := t.TempDir()
	stateDir := t.TempDir()
	gcLog := filepath.Join(t.TempDir(), "gc.log")
	mailLog := filepath.Join(t.TempDir(), "gc-mail.log")
	archiveRepo := filepath.Join(cityDir, "archive")
	stateFile := filepath.Join(stateDir, "jsonl-export-state.json")

	initSeedArchive(t, archiveRepo, 100)
	writeMultiRecordDoltStub(t, binDir, 10)
	writeJsonlExportGCStubWithMailExitCode(t, binDir, 1)

	if err := os.WriteFile(stateFile, []byte(`{"pending_spike_alert":{"database":"oldbeads","prev_count":90,"current_count":45,"delta":50,"threshold":20}}`+"\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(state file): %v", err)
	}

	env := jsonlExportEnv(t, cityDir, binDir, stateDir, archiveRepo, gcLog, mailLog)

	runScript(t, filepath.Join(exampleDir(), "packs", "maintenance", "assets", "scripts", "jsonl-export.sh"), env)

	stateData, err := os.ReadFile(stateFile)
	if err != nil {
		t.Fatalf("ReadFile(state file): %v", err)
	}
	var state map[string]any
	if err := json.Unmarshal(stateData, &state); err != nil {
		t.Fatalf("Unmarshal(state file): %v\n%s", err, stateData)
	}
	pendingAlerts, ok := state["pending_spike_alerts"].(map[string]any)
	if !ok {
		t.Fatalf("expected pending_spike_alerts map, got:\n%s", stateData)
	}
	if _, ok := pendingAlerts["oldbeads"]; !ok {
		t.Fatalf("expected existing pending alert to survive, got:\n%s", stateData)
	}
	if _, ok := pendingAlerts["beads"]; !ok {
		t.Fatalf("expected new pending alert to be added, got:\n%s", stateData)
	}
}

// TestJsonlExportLocalOnlyModeSkipsPushAndLogsMode covers the default setup
// where no `origin` remote has been configured on the archive. The script
// must log the mode, skip the push path entirely, and leave push-failure
// state untouched.
func TestJsonlExportLocalOnlyModeSkipsPushAndLogsMode(t *testing.T) {
	cityDir := t.TempDir()
	binDir := t.TempDir()
	stateDir := t.TempDir()
	gcLog := filepath.Join(t.TempDir(), "gc.log")
	mailLog := filepath.Join(t.TempDir(), "gc-mail.log")
	archiveRepo := filepath.Join(cityDir, "archive")
	stateFile := filepath.Join(stateDir, "jsonl-export-state.json")

	writeMultiRecordDoltStub(t, binDir, 3)
	writeJsonlExportGCStub(t, binDir)

	env := jsonlExportEnv(t, cityDir, binDir, stateDir, archiveRepo, gcLog, mailLog)
	delete(env, "GC_JSONL_MAX_PUSH_FAILURES")

	out, err := runScriptResult(t, filepath.Join(exampleDir(), "packs", "maintenance", "assets", "scripts", "jsonl-export.sh"), env)
	if err != nil {
		t.Fatalf("jsonl-export.sh: %v\n%s", err, out)
	}

	if !strings.Contains(string(out), "archive running in local-only mode") {
		t.Fatalf("expected local-only mode log, got:\n%s", out)
	}
	if !strings.Contains(string(out), "push: skipped (local-only)") {
		t.Fatalf("expected push: skipped (local-only) summary, got:\n%s", out)
	}

	mailData, _ := os.ReadFile(mailLog)
	if strings.Contains(string(mailData), "JSONL push failed") {
		t.Fatalf("local-only mode must not trigger push-failure escalation; mail log:\n%s", mailData)
	}

	stateData, err := os.ReadFile(stateFile)
	if err != nil {
		t.Fatalf("ReadFile(state file): %v", err)
	}
	var state map[string]any
	if err := json.Unmarshal(stateData, &state); err != nil {
		t.Fatalf("Unmarshal(state file): %v\n%s", err, stateData)
	}
	if got := state["consecutive_push_failures"]; got != nil && got != float64(0) {
		t.Fatalf("consecutive_push_failures = %v, expected unset or 0\nstate: %s", got, stateData)
	}
	if got := state["last_logged_mode"]; got != "local-only" {
		t.Fatalf("last_logged_mode = %v, want local-only\nstate: %s", got, stateData)
	}
	if _, ok := state["last_logged_at"].(string); !ok {
		t.Fatalf("last_logged_at missing or not a string\nstate: %s", stateData)
	}
}

// TestJsonlExportPushModeAttemptsPushWhenOriginConfigured covers the operator
// who has opted into off-box backup: origin is configured and reachable, so
// the mode log reports push mode and the push actually happens.
func TestJsonlExportPushModeAttemptsPushWhenOriginConfigured(t *testing.T) {
	cityDir := t.TempDir()
	binDir := t.TempDir()
	stateDir := t.TempDir()
	gcLog := filepath.Join(t.TempDir(), "gc.log")
	mailLog := filepath.Join(t.TempDir(), "gc-mail.log")
	archiveRepo := filepath.Join(cityDir, "archive")
	stateFile := filepath.Join(stateDir, "jsonl-export-state.json")

	remoteRepo, priorHead := initSeedArchiveWithRemote(t, archiveRepo)
	writeMultiRecordDoltStub(t, binDir, 5)
	writeJsonlExportGCStub(t, binDir)

	env := jsonlExportEnv(t, cityDir, binDir, stateDir, archiveRepo, gcLog, mailLog)
	// initSeedArchiveWithRemote seeds 100 prev rows; the multi-record stub
	// returns 5. The default 20% spike threshold would flag this 95% drop and
	// route the run through the HALT path, which suppresses the push. This
	// test is scoped to push behavior, not spike detection — raise MIN_PREV
	// above 100 so the percent check is skipped here.
	env["GC_JSONL_MIN_PREV_FOR_SPIKE"] = "1000"

	out, err := runScriptResult(t, filepath.Join(exampleDir(), "packs", "maintenance", "assets", "scripts", "jsonl-export.sh"), env)
	if err != nil {
		t.Fatalf("jsonl-export.sh: %v\n%s", err, out)
	}

	if !strings.Contains(string(out), "archive running in push mode") {
		t.Fatalf("expected push mode log, got:\n%s", out)
	}

	remoteHeadOut, err := exec.Command("git", "--git-dir", remoteRepo, "rev-parse", "refs/heads/main").CombinedOutput()
	if err != nil {
		t.Fatalf("git rev-parse remote main: %v\n%s", err, remoteHeadOut)
	}
	newRemoteHead := strings.TrimSpace(string(remoteHeadOut))
	if newRemoteHead == priorHead {
		t.Fatalf("expected push mode to advance the remote main: still at %s", priorHead)
	}

	stateData, err := os.ReadFile(stateFile)
	if err != nil {
		t.Fatalf("ReadFile(state file): %v", err)
	}
	var state map[string]any
	if err := json.Unmarshal(stateData, &state); err != nil {
		t.Fatalf("Unmarshal(state file): %v\n%s", err, stateData)
	}
	if got := state["last_logged_mode"]; got != "push" {
		t.Fatalf("last_logged_mode = %v, want push\nstate: %s", got, stateData)
	}
}

// TestJsonlExportLocalOnlyTransitionClearsStalePushFailureState covers the
// push→local-only transition: when the operator removes origin after
// push-failure state has accumulated, the next run must clear
// consecutive_push_failures so a later push→local-only→push round-trip
// starts from a clean counter (not from the stale value, which could trigger
// a premature HIGH escalation on the very first failure after origin
// returns). pending_archive_push is intentionally retained — it tracks that
// local commits still need to be pushed once origin returns.
func TestJsonlExportLocalOnlyTransitionClearsStalePushFailureState(t *testing.T) {
	cityDir := t.TempDir()
	binDir := t.TempDir()
	stateDir := t.TempDir()
	gcLog := filepath.Join(t.TempDir(), "gc.log")
	mailLog := filepath.Join(t.TempDir(), "gc-mail.log")
	archiveRepo := filepath.Join(cityDir, "archive")
	stateFile := filepath.Join(stateDir, "jsonl-export-state.json")

	initSeedArchive(t, archiveRepo, 3)
	writeMultiRecordDoltStub(t, binDir, 5)
	writeJsonlExportGCStub(t, binDir)

	if err := os.MkdirAll(filepath.Dir(stateFile), 0o755); err != nil {
		t.Fatalf("MkdirAll(state dir): %v", err)
	}
	// Seed state: push mode was active, two push failures accumulated, the
	// pending-push flag is set. Then operator removed origin (no remote on
	// archive). Next tick should detect the transition and reset both fields.
	priorState := `{"last_logged_mode":"push","last_logged_at":"2026-05-01T00:00:00Z","consecutive_push_failures":2,"pending_archive_push":true}` + "\n"
	if err := os.WriteFile(stateFile, []byte(priorState), 0o644); err != nil {
		t.Fatalf("WriteFile(state file): %v", err)
	}

	env := jsonlExportEnv(t, cityDir, binDir, stateDir, archiveRepo, gcLog, mailLog)
	delete(env, "GC_JSONL_MAX_PUSH_FAILURES")

	out, err := runScriptResult(t, filepath.Join(exampleDir(), "packs", "maintenance", "assets", "scripts", "jsonl-export.sh"), env)
	if err != nil {
		t.Fatalf("jsonl-export.sh: %v\n%s", err, out)
	}

	if !strings.Contains(string(out), "archive running in local-only mode") {
		t.Fatalf("expected local-only transition log, got:\n%s", out)
	}

	stateData, err := os.ReadFile(stateFile)
	if err != nil {
		t.Fatalf("ReadFile(state file): %v", err)
	}
	var state map[string]any
	if err := json.Unmarshal(stateData, &state); err != nil {
		t.Fatalf("Unmarshal(state file): %v\n%s", err, stateData)
	}
	if got := state["last_logged_mode"]; got != "local-only" {
		t.Fatalf("last_logged_mode = %v, want local-only\nstate: %s", got, stateData)
	}
	// consecutive_push_failures must be cleared (json.Unmarshal decodes
	// numbers as float64).
	if got, ok := state["consecutive_push_failures"].(float64); !ok || got != 0 {
		t.Fatalf("consecutive_push_failures = %v, want 0\nstate: %s", state["consecutive_push_failures"], stateData)
	}
	// pending_archive_push is retained — local commits still need to be
	// pushed when origin returns. Verify it's present and true.
	if got, ok := state["pending_archive_push"].(bool); !ok || !got {
		t.Fatalf("pending_archive_push must remain true to track deferred push\nstate: %s", stateData)
	}

	// No HIGH escalation should have fired during the transition itself.
	mailContents, err := os.ReadFile(mailLog)
	if err == nil && strings.Contains(string(mailContents), "ESCALATION: JSONL push failed [HIGH]") {
		t.Fatalf("local-only transition must not escalate; mail log:\n%s", mailContents)
	}
}

// TestJsonlExportModeTransitionFromPushToLocalOnlyRelogs covers the operator
// who previously had origin configured, ran the archive (so state already
// carries last_logged_mode=push), then removed origin. The next run must log
// the transition to local-only and update state — without escalating.
func TestJsonlExportModeTransitionFromPushToLocalOnlyRelogs(t *testing.T) {
	cityDir := t.TempDir()
	binDir := t.TempDir()
	stateDir := t.TempDir()
	gcLog := filepath.Join(t.TempDir(), "gc.log")
	mailLog := filepath.Join(t.TempDir(), "gc-mail.log")
	archiveRepo := filepath.Join(cityDir, "archive")
	stateFile := filepath.Join(stateDir, "jsonl-export-state.json")

	initSeedArchive(t, archiveRepo, 3)
	writeMultiRecordDoltStub(t, binDir, 5)
	writeJsonlExportGCStub(t, binDir)

	if err := os.MkdirAll(filepath.Dir(stateFile), 0o755); err != nil {
		t.Fatalf("MkdirAll(state dir): %v", err)
	}
	priorState := `{"last_logged_mode":"push","last_logged_at":"2026-05-01T00:00:00Z"}` + "\n"
	if err := os.WriteFile(stateFile, []byte(priorState), 0o644); err != nil {
		t.Fatalf("WriteFile(state file): %v", err)
	}

	env := jsonlExportEnv(t, cityDir, binDir, stateDir, archiveRepo, gcLog, mailLog)
	delete(env, "GC_JSONL_MAX_PUSH_FAILURES")

	out, err := runScriptResult(t, filepath.Join(exampleDir(), "packs", "maintenance", "assets", "scripts", "jsonl-export.sh"), env)
	if err != nil {
		t.Fatalf("jsonl-export.sh: %v\n%s", err, out)
	}

	if !strings.Contains(string(out), "archive running in local-only mode") {
		t.Fatalf("expected transition log to local-only mode, got:\n%s", out)
	}

	mailData, _ := os.ReadFile(mailLog)
	if strings.Contains(string(mailData), "JSONL push failed") {
		t.Fatalf("transition to local-only must not escalate; mail log:\n%s", mailData)
	}

	stateData, err := os.ReadFile(stateFile)
	if err != nil {
		t.Fatalf("ReadFile(state file): %v", err)
	}
	var state map[string]any
	if err := json.Unmarshal(stateData, &state); err != nil {
		t.Fatalf("Unmarshal(state file): %v\n%s", err, stateData)
	}
	if got := state["last_logged_mode"]; got != "local-only" {
		t.Fatalf("last_logged_mode = %v, want local-only after transition\nstate: %s", got, stateData)
	}
	if got := state["last_logged_at"]; got == "2026-05-01T00:00:00Z" {
		t.Fatalf("last_logged_at not refreshed after transition\nstate: %s", stateData)
	}
}

// TestJsonlExportPushFailureEscalationBodyIncludesStderrAndRemediation
// verifies that the enriched escalation body reaches the mayor with the
// captured git stderr and the remediation pointer. Uses an unreachable
// origin so push fails on the first run.
func TestJsonlExportPushFailureEscalationBodyIncludesStderrAndRemediation(t *testing.T) {
	cityDir := t.TempDir()
	binDir := t.TempDir()
	stateDir := t.TempDir()
	gcLog := filepath.Join(t.TempDir(), "gc.log")
	mailLog := filepath.Join(t.TempDir(), "gc-mail.log")
	archiveRepo := filepath.Join(cityDir, "archive")

	initSeedArchiveWithUnreachableRemote(t, archiveRepo, 3)
	writeMultiRecordDoltStub(t, binDir, 5)
	writeJsonlExportGCStub(t, binDir)

	env := jsonlExportEnv(t, cityDir, binDir, stateDir, archiveRepo, gcLog, mailLog)
	env["GC_JSONL_MAX_PUSH_FAILURES"] = "1"

	runScript(t, filepath.Join(exampleDir(), "packs", "maintenance", "assets", "scripts", "jsonl-export.sh"), env)

	mailData, err := os.ReadFile(mailLog)
	if err != nil {
		t.Fatalf("ReadFile(mail log): %v", err)
	}
	body := string(mailData)
	wants := []string{
		"ESCALATION: JSONL push failed",
		"Order: mol-dog-jsonl",
		"Archive: " + archiveRepo,
		"Consecutive failures: 1 (threshold: 1)",
		"Last git push stderr:",
		"Remediation:",
		"docs/getting-started/troubleshooting.md#jsonl-archive-push-failures",
	}
	for _, want := range wants {
		if !strings.Contains(body, want) {
			t.Fatalf("escalation body missing %q:\n%s", want, body)
		}
	}
}

// gateSweepEnv constructs the env for a gate-sweep.sh invocation with a
// PATH-shimmed bd stub that logs every call to BD_LOG.
func gateSweepEnv(t *testing.T) (binDir, bdLog string, env map[string]string) {
	t.Helper()
	binDir = t.TempDir()
	bdLog = filepath.Join(t.TempDir(), "bd.log")
	env = map[string]string{
		"BD_LOG":       bdLog,
		"GC_CITY":      t.TempDir(),
		"GC_CITY_PATH": t.TempDir(),
		"PATH":         binDir + string(os.PathListSeparator) + os.Getenv("PATH"),
	}
	return binDir, bdLog, env
}

func TestGateSweepInvokesTimerAndGhGateChecks(t *testing.T) {
	binDir, bdLog, env := gateSweepEnv(t)
	writeExecutable(t, filepath.Join(binDir, "bd"), `#!/bin/sh
printf '%s\n' "$*" >> "$BD_LOG"
exit 0
`)

	runScript(t, filepath.Join(exampleDir(), "packs", "maintenance", "assets", "scripts", "gate-sweep.sh"), env)

	log, err := os.ReadFile(bdLog)
	if err != nil {
		t.Fatalf("ReadFile(bd log): %v", err)
	}
	s := string(log)
	for _, want := range []string{
		"gate check --type=timer --escalate",
		"gate check --type=gh --escalate",
	} {
		if !strings.Contains(s, want) {
			t.Fatalf("missing %q in bd log:\n%s", want, s)
		}
	}
}

func TestGateSweepSkipsBeadAndUnsupportedGateTypes(t *testing.T) {
	binDir, bdLog, env := gateSweepEnv(t)
	writeExecutable(t, filepath.Join(binDir, "bd"), `#!/bin/sh
printf '%s\n' "$*" >> "$BD_LOG"
exit 0
`)

	runScript(t, filepath.Join(exampleDir(), "packs", "maintenance", "assets", "scripts", "gate-sweep.sh"), env)

	log, err := os.ReadFile(bdLog)
	if err != nil {
		t.Fatalf("ReadFile(bd log): %v", err)
	}
	s := string(log)
	// Sanity: the script must actually invoke at least one gate check, so
	// this test can't pass vacuously if the script regressed to a no-op.
	if !strings.Contains(s, "gate check") {
		t.Fatalf("gate-sweep should call `bd gate check` at least once; bd log:\n%s", s)
	}
	// bead-type is no-op upstream (beads v1.0.2 multi-rig removal); the
	// script intentionally skips it. condition-type doesn't exist at all.
	for _, banned := range []string{"type=bead", "type=condition"} {
		if strings.Contains(s, banned) {
			t.Fatalf("gate-sweep should not invoke %q; bd log:\n%s", banned, s)
		}
	}
	// gate list is the broken pre-fix call shape (gc-mrg). Must not regress.
	if strings.Contains(s, "gate list") {
		t.Fatalf("gate-sweep should call `gate check`, not `gate list`; bd log:\n%s", s)
	}
}

// TestGateSweepToleratesGhGateBdFailures verifies the surviving '|| true':
// bd failures on the gh-gate evaluation path are tolerated because fresh
// cities without 'gh auth' would otherwise fail this order on every 30s
// cooldown. Timer-gate failures are NOT tolerated (see
// TestGateSweepPropagatesTimerGateBdFailures) since #1734.
func TestGateSweepToleratesGhGateBdFailures(t *testing.T) {
	binDir, _, env := gateSweepEnv(t)
	writeExecutable(t, filepath.Join(binDir, "bd"), `#!/bin/sh
case "$*" in
  *--type=gh*)
    echo "bd: simulated gh-gate failure (e.g., missing gh auth)" >&2
    exit 1
    ;;
  *)
    exit 0
    ;;
esac
`)

	script := filepath.Join(exampleDir(), "packs", "maintenance", "assets", "scripts", "gate-sweep.sh")
	cmd := exec.Command(script)
	cmd.Env = mergeTestEnv(env)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("gate-sweep should exit 0 when only the gh-gate bd call fails (|| true is load-bearing for fresh cities without gh auth); got %v\n%s", err, out)
	}
}

// TestGateSweepPropagatesTimerGateBdFailures verifies the #1734 fix:
// failures on the timer-gate evaluation path must propagate (no '|| true'
// suppression) so real bd regressions surface in the controller log.
// Timer-gate evaluation is local-only and has no auth requirement that
// would justify swallowing errors.
func TestGateSweepPropagatesTimerGateBdFailures(t *testing.T) {
	binDir, _, env := gateSweepEnv(t)
	writeExecutable(t, filepath.Join(binDir, "bd"), `#!/bin/sh
case "$*" in
  *--type=timer*)
    echo "bd: simulated timer-gate failure" >&2
    exit 1
    ;;
  *)
    exit 0
    ;;
esac
`)

	script := filepath.Join(exampleDir(), "packs", "maintenance", "assets", "scripts", "gate-sweep.sh")
	cmd := exec.Command(script)
	cmd.Env = mergeTestEnv(env)
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("gate-sweep should exit non-zero when the timer-gate bd call fails (no || true suppression on that line); got success\n%s", out)
	}
}

// hermeticGitEnv builds a git invocation env that strips any pre-existing
// GIT_* control variables from the parent environment before applying the
// overrides — same approach as mergeTestEnv. This avoids duplicate keys
// where libc's getenv may return the first (parent) occurrence and silently
// defeat the intended hermeticity.
func hermeticGitEnv(t *testing.T, overrides map[string]string) []string {
	t.Helper()
	return mergeTestEnv(overrides)
}

// runGit runs a git subcommand in the given directory and fails the test on
// non-zero exit. The git binary used is whatever the developer has on PATH.
func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = hermeticGitEnv(t, map[string]string{
		"GIT_AUTHOR_NAME":     "Test",
		"GIT_AUTHOR_EMAIL":    "test@example.com",
		"GIT_COMMITTER_NAME":  "Test",
		"GIT_COMMITTER_EMAIL": "test@example.com",
		"GIT_CONFIG_NOSYSTEM": "1",
		"GIT_CONFIG_GLOBAL":   filepath.Join(t.TempDir(), "gitconfig"),
	})
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %s in %s: %v\n%s", strings.Join(args, " "), dir, err, out)
	}
}

// runGitOut is runGit but returns trimmed stdout. On failure the test fatal
// includes combined stderr+stdout so CI-only git failures are diagnosable.
func runGitOut(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = hermeticGitEnv(t, map[string]string{
		"GIT_CONFIG_NOSYSTEM": "1",
		"GIT_CONFIG_GLOBAL":   filepath.Join(t.TempDir(), "gitconfig"),
	})
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git %s in %s: %v\nstderr: %s", strings.Join(args, " "), dir, err, stderr.String())
	}
	return strings.TrimSpace(string(out))
}

// pruneBranchesRig sets up a temp git repo with an `origin` remote that
// has a single commit on `main`, then invokes setup(rigPath, originPath)
// to populate gc/* branches and exercise specific scenarios. Returns
// (rigPath, gcStubBin) where gcStubBin is a PATH dir containing a `gc` stub
// that reports the rig path via `gc rig list --json`.
func pruneBranchesRig(t *testing.T, setup func(rigPath, originPath string)) (string, string) {
	t.Helper()
	rigPath := t.TempDir()
	originPath := filepath.Join(t.TempDir(), "origin.git")

	runGit(t, t.TempDir(), "init", "-q", "--bare", "-b", "main", originPath)

	runGit(t, rigPath, "init", "-q", "-b", "main", ".")
	runGit(t, rigPath, "remote", "add", "origin", originPath)
	if err := os.WriteFile(filepath.Join(rigPath, "README"), []byte("seed"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, rigPath, "add", "README")
	runGit(t, rigPath, "commit", "-q", "-m", "seed")
	runGit(t, rigPath, "push", "-q", "-u", "origin", "main")

	setup(rigPath, originPath)

	binDir := t.TempDir()
	// Stub mirrors the real `gc rig list --json` schema (RigListJSON in
	// cmd/gc/cmd_rig.go: {"city_path":..., "city_name":..., "rigs":[...]}).
	// Older versions of prune-branches.sh used `.[].path` against this
	// output and silently no-op'd via `|| exit 0`; pinning the real schema
	// here means the test actually catches that regression now.
	writeExecutable(t, filepath.Join(binDir, "gc"), fmt.Sprintf(`#!/bin/sh
case "$1 $2 $3" in
  "rig list --json")
    printf '{"city_path":"/tmp","city_name":"test","rigs":[{"name":"r","path":"%s","prefix":"r","hq":true,"suspended":false,"beads":""}]}\n'
    exit 0
    ;;
esac
exit 1
`, rigPath))
	return rigPath, binDir
}

func TestPruneBranchesPrunesMergedGcBranches(t *testing.T) {
	rigPath, binDir := pruneBranchesRig(t, func(rigPath, _ string) {
		// gc/merged tip == main tip → merge-base --is-ancestor succeeds.
		runGit(t, rigPath, "branch", "gc/merged")
	})

	env := map[string]string{
		"GC_CITY":      t.TempDir(),
		"GC_CITY_PATH": t.TempDir(),
		"PATH":         binDir + string(os.PathListSeparator) + os.Getenv("PATH"),
	}

	script := filepath.Join(exampleDir(), "packs", "maintenance", "assets", "scripts", "prune-branches.sh")
	cmd := exec.Command(script)
	cmd.Env = mergeTestEnv(env)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("prune-branches: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "deleted 1 stale branches") {
		t.Fatalf("expected deletion summary; got:\n%s", out)
	}

	branches := runGitOut(t, rigPath, "branch", "--list", "gc/*")
	if branches != "" {
		t.Fatalf("gc/merged not pruned, branches:\n%s", branches)
	}
}

func TestPruneBranchesSkipsCurrentBranch(t *testing.T) {
	rigPath, binDir := pruneBranchesRig(t, func(rigPath, _ string) {
		runGit(t, rigPath, "checkout", "-q", "-b", "gc/active")
	})

	env := map[string]string{
		"GC_CITY":      t.TempDir(),
		"GC_CITY_PATH": t.TempDir(),
		"PATH":         binDir + string(os.PathListSeparator) + os.Getenv("PATH"),
	}

	script := filepath.Join(exampleDir(), "packs", "maintenance", "assets", "scripts", "prune-branches.sh")
	cmd := exec.Command(script)
	cmd.Env = mergeTestEnv(env)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("prune-branches: %v\n%s", err, out)
	}
	// No deletion summary means PRUNED stayed 0.
	if strings.Contains(string(out), "deleted") {
		t.Fatalf("current branch must not be pruned; got:\n%s", out)
	}

	if got := runGitOut(t, rigPath, "branch", "--show-current"); got != "gc/active" {
		t.Fatalf("current branch changed; got %q", got)
	}
}

func TestPruneBranchesPreservesBranchWithUnmergedWork(t *testing.T) {
	// gc/* branch with a commit not in origin/main and no remote tracking
	// ref should NOT be deleted: prune-branches uses safe `branch -d` which
	// refuses unmerged work. This test pins that safety behavior.
	rigPath, binDir := pruneBranchesRig(t, func(rigPath, _ string) {
		runGit(t, rigPath, "checkout", "-q", "-b", "gc/unmerged")
		if err := os.WriteFile(filepath.Join(rigPath, "WIP"), []byte("wip"), 0o644); err != nil {
			t.Fatal(err)
		}
		runGit(t, rigPath, "add", "WIP")
		runGit(t, rigPath, "commit", "-q", "-m", "wip work")
		runGit(t, rigPath, "checkout", "-q", "main")
		// Never push gc/unmerged → no refs/remotes/origin/gc/unmerged exists.
	})

	env := map[string]string{
		"GC_CITY":      t.TempDir(),
		"GC_CITY_PATH": t.TempDir(),
		"PATH":         binDir + string(os.PathListSeparator) + os.Getenv("PATH"),
	}

	script := filepath.Join(exampleDir(), "packs", "maintenance", "assets", "scripts", "prune-branches.sh")
	cmd := exec.Command(script)
	cmd.Env = mergeTestEnv(env)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("prune-branches: %v\n%s", err, out)
	}
	if strings.Contains(string(out), "deleted") {
		t.Fatalf("unmerged branch must not be force-deleted; got:\n%s", out)
	}

	branches := runGitOut(t, rigPath, "branch", "--list", "gc/*")
	if !strings.Contains(branches, "gc/unmerged") {
		t.Fatalf("gc/unmerged was pruned despite unmerged commits:\n%s", branches)
	}
}

func TestPruneBranchesNoOpWhenNoGcBranches(t *testing.T) {
	_, binDir := pruneBranchesRig(t, func(_, _ string) {
		// No gc/* branches created; only main exists.
	})

	env := map[string]string{
		"GC_CITY":      t.TempDir(),
		"GC_CITY_PATH": t.TempDir(),
		"PATH":         binDir + string(os.PathListSeparator) + os.Getenv("PATH"),
	}

	script := filepath.Join(exampleDir(), "packs", "maintenance", "assets", "scripts", "prune-branches.sh")
	cmd := exec.Command(script)
	cmd.Env = mergeTestEnv(env)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("prune-branches: %v\n%s", err, out)
	}
	if strings.TrimSpace(string(out)) != "" {
		t.Fatalf("expected silent no-op when no gc/* branches exist; got:\n%s", out)
	}
}

// wispTimestampLayout produces no-Z timestamps that both GNU `date -d` and
// BSD `date -j -f "%Y-%m-%dT%H:%M:%S"` accept; wisp-compact.sh also accepts
// RFC3339 timestamps with trailing Z.
const wispTimestampLayout = "2006-01-02T15:04:05"

// wispCompactEnv installs a `bd` stub that returns the supplied beadsJSON on
// `bd list --json --all -n 0` and logs all other bd subcommands to BD_LOG.
// BD_LOG is pre-created empty so skip-path tests can still assert on its
// (empty) contents. TZ=UTC is pinned for cross-platform date parsing — see
// wispTimestampLayout. jq is whatever is on PATH.
func wispCompactEnv(t *testing.T, beadsJSON string) (bdLog string, env map[string]string) {
	t.Helper()
	binDir := t.TempDir()
	bdLog = filepath.Join(t.TempDir(), "bd.log")
	if err := os.WriteFile(bdLog, nil, 0o644); err != nil {
		t.Fatalf("WriteFile(bd log): %v", err)
	}

	stubPath := filepath.Join(binDir, "bd")
	// Stub fails fast on any subcommand or flag shape the script doesn't
	// currently use. This pins the script's bd contract — a regression that
	// dropped `--json` or `--all` from `bd list` would otherwise silently
	// pass because cat would still emit valid JSON.
	writeExecutable(t, stubPath, fmt.Sprintf(`#!/bin/sh
case "$1" in
  list)
    case "$*" in
      *"--json"*"--all"*"-n 0"*)
        cat <<'EOF'
%s
EOF
        exit 0
        ;;
      *)
        echo "bd list called with unexpected args: $*" >&2
        exit 2
        ;;
    esac
    ;;
  update|comment|delete)
    printf '%%s\n' "$*" >> "$BD_LOG"
    exit 0
    ;;
  *)
    echo "bd called with unexpected subcommand: $*" >&2
    exit 2
    ;;
esac
`, beadsJSON))

	env = map[string]string{
		"BD_LOG":       bdLog,
		"GC_CITY":      t.TempDir(),
		"GC_CITY_PATH": t.TempDir(),
		"PATH":         binDir + string(os.PathListSeparator) + os.Getenv("PATH"),
		"TZ":           "UTC",
	}
	return bdLog, env
}

func TestWispCompactDeletesClosedPastTTL(t *testing.T) {
	pastTTL := time.Now().Add(-48 * time.Hour).UTC().Format(wispTimestampLayout)
	beads := fmt.Sprintf(`[
  {"id":"ga-old","status":"closed","ephemeral":true,"updated_at":%q,"comment_count":0,"labels":[]}
]`, pastTTL)

	bdLog, env := wispCompactEnv(t, beads)
	runScript(t, filepath.Join(exampleDir(), "packs", "maintenance", "assets", "scripts", "wisp-compact.sh"), env)

	log, err := os.ReadFile(bdLog)
	if err != nil {
		t.Fatalf("ReadFile(bd log): %v", err)
	}
	s := string(log)
	if !strings.Contains(s, "delete ga-old --force") {
		t.Fatalf("expected `bd delete ga-old --force`; bd log:\n%s", s)
	}
	for _, banned := range []string{"update ga-old --persistent", "comment ga-old"} {
		if strings.Contains(s, banned) {
			t.Fatalf("closed+past-TTL+no-comments should be deleted, not %q; bd log:\n%s", banned, s)
		}
	}
}

func TestWispCompactReportsSummaryForActions(t *testing.T) {
	pastTTL := time.Now().Add(-48 * time.Hour).UTC().Format(wispTimestampLayout)
	withinTTL := time.Now().Add(-1 * time.Hour).UTC().Format(wispTimestampLayout)
	beads := fmt.Sprintf(`[
  {"id":"ga-old","status":"closed","ephemeral":true,"updated_at":%q,"comment_count":0,"labels":[]},
  {"id":"ga-fresh","status":"closed","ephemeral":true,"updated_at":%q,"comment_count":0,"labels":[]}
]`, pastTTL, withinTTL)

	_, env := wispCompactEnv(t, beads)
	out, err := runScriptResult(t, filepath.Join(exampleDir(), "packs", "maintenance", "assets", "scripts", "wisp-compact.sh"), env)
	if err != nil {
		t.Fatalf("wisp-compact.sh failed: %v\n%s", err, out)
	}
	if got, want := strings.TrimSpace(string(out)), "wisp-compact: promoted=0 deleted=1 skipped=1"; got != want {
		t.Fatalf("summary = %q, want %q", got, want)
	}
}

func TestWispCompactPromotesNonClosedPastTTL(t *testing.T) {
	pastTTL := time.Now().Add(-48 * time.Hour).UTC().Format(wispTimestampLayout)
	beads := fmt.Sprintf(`[
  {"id":"ga-stuck","status":"open","ephemeral":true,"updated_at":%q,"comment_count":0,"labels":[]}
]`, pastTTL)

	bdLog, env := wispCompactEnv(t, beads)
	runScript(t, filepath.Join(exampleDir(), "packs", "maintenance", "assets", "scripts", "wisp-compact.sh"), env)

	log, err := os.ReadFile(bdLog)
	if err != nil {
		t.Fatalf("ReadFile(bd log): %v", err)
	}
	s := string(log)
	if !strings.Contains(s, "update ga-stuck --persistent") {
		t.Fatalf("expected `bd update ga-stuck --persistent`; bd log:\n%s", s)
	}
	if !strings.Contains(s, "stuck detection") {
		t.Fatalf("expected promotion comment to mention stuck detection; bd log:\n%s", s)
	}
	if strings.Contains(s, "delete ga-stuck") {
		t.Fatalf("non-closed wisp must be promoted, not deleted; bd log:\n%s", s)
	}
}

func TestWispCompactPromotesClosedWispsWithComments(t *testing.T) {
	pastTTL := time.Now().Add(-48 * time.Hour).UTC().Format(wispTimestampLayout)
	beads := fmt.Sprintf(`[
  {"id":"ga-discussed","status":"closed","ephemeral":true,"updated_at":%q,"comment_count":3,"labels":[]}
]`, pastTTL)

	bdLog, env := wispCompactEnv(t, beads)
	runScript(t, filepath.Join(exampleDir(), "packs", "maintenance", "assets", "scripts", "wisp-compact.sh"), env)

	log, err := os.ReadFile(bdLog)
	if err != nil {
		t.Fatalf("ReadFile(bd log): %v", err)
	}
	s := string(log)
	if !strings.Contains(s, "update ga-discussed --persistent") {
		t.Fatalf("expected `bd update ga-discussed --persistent`; bd log:\n%s", s)
	}
	if !strings.Contains(s, "proven value") {
		t.Fatalf("expected promotion comment to mention proven value; bd log:\n%s", s)
	}
	if strings.Contains(s, "delete ga-discussed") {
		t.Fatalf("wisp with comments must be preserved, not deleted; bd log:\n%s", s)
	}
}

func TestWispCompactSkipsBeadsWithinTTL(t *testing.T) {
	withinTTL := time.Now().Add(-1 * time.Hour).UTC().Format(wispTimestampLayout)
	beads := fmt.Sprintf(`[
  {"id":"ga-fresh","status":"closed","ephemeral":true,"updated_at":%q,"comment_count":0,"labels":[]}
]`, withinTTL)

	bdLog, env := wispCompactEnv(t, beads)
	runScript(t, filepath.Join(exampleDir(), "packs", "maintenance", "assets", "scripts", "wisp-compact.sh"), env)

	log, err := os.ReadFile(bdLog)
	if err != nil {
		t.Fatalf("ReadFile(bd log): %v", err)
	}
	s := string(log)
	for _, banned := range []string{"delete ga-fresh", "update ga-fresh", "comment ga-fresh"} {
		if strings.Contains(s, banned) {
			t.Fatalf("within-TTL bead must not be touched; saw %q in bd log:\n%s", banned, s)
		}
	}
}

func TestWispCompactRespectsHeartbeatTTL(t *testing.T) {
	// wisp_type:heartbeat has a 6h TTL. A bead aged 7h should be acted on
	// (delete, since closed + no comments + no keep label) even though the
	// default 24h TTL would skip it.
	aged7h := time.Now().Add(-7 * time.Hour).UTC().Format(wispTimestampLayout)
	beads := fmt.Sprintf(`[
  {"id":"ga-hb","status":"closed","ephemeral":true,"updated_at":%q,"comment_count":0,"labels":["wisp_type:heartbeat"]}
]`, aged7h)

	bdLog, env := wispCompactEnv(t, beads)
	runScript(t, filepath.Join(exampleDir(), "packs", "maintenance", "assets", "scripts", "wisp-compact.sh"), env)

	log, err := os.ReadFile(bdLog)
	if err != nil {
		t.Fatalf("ReadFile(bd log): %v", err)
	}
	s := string(log)
	if !strings.Contains(s, "delete ga-hb --force") {
		t.Fatalf("heartbeat aged 7h should be deleted past 6h TTL; bd log:\n%s", s)
	}
}

func TestWispCompactSkipsNonEphemeralBeads(t *testing.T) {
	pastTTL := time.Now().Add(-48 * time.Hour).UTC().Format(wispTimestampLayout)
	beads := fmt.Sprintf(`[
  {"id":"ga-perm","status":"closed","ephemeral":false,"updated_at":%q,"comment_count":0,"labels":[]}
]`, pastTTL)

	bdLog, env := wispCompactEnv(t, beads)
	runScript(t, filepath.Join(exampleDir(), "packs", "maintenance", "assets", "scripts", "wisp-compact.sh"), env)

	log, err := os.ReadFile(bdLog)
	if err != nil {
		t.Fatalf("ReadFile(bd log): %v", err)
	}
	s := string(log)
	for _, banned := range []string{"delete ga-perm", "update ga-perm", "comment ga-perm"} {
		if strings.Contains(s, banned) {
			t.Fatalf("non-ephemeral bead must be ignored; saw %q in bd log:\n%s", banned, s)
		}
	}
}

// crossRigDepsEnv installs a `bd` stub that handles three subcommand shapes:
//   - `bd list --status=closed --closed-after=... --json` → returns closedJSON
//   - `bd dep list <id> --direction=up --type=blocks --json` → returns depsJSON
//   - `bd dep remove ...` and `bd dep add ...` → appended to BD_LOG
//
// BD_LOG is pre-created empty so skip-path tests can still read it.
func crossRigDepsEnv(t *testing.T, closedJSON, depsJSON string) (bdLog string, env map[string]string) {
	t.Helper()
	binDir := t.TempDir()
	bdLog = filepath.Join(t.TempDir(), "bd.log")
	if err := os.WriteFile(bdLog, nil, 0o644); err != nil {
		t.Fatalf("WriteFile(bd log): %v", err)
	}

	stubPath := filepath.Join(binDir, "bd")
	// Stub fails fast on unexpected subcommands or flag shapes so the test
	// pins the script's bd contract; a regression dropping --json from
	// `bd list` or --type=blocks from `bd dep list` would otherwise still
	// pass.
	writeExecutable(t, stubPath, fmt.Sprintf(`#!/bin/sh
case "$1" in
  list)
    case "$*" in
      *"--status=closed"*"--closed-after"*"--json"*)
        cat <<'EOF'
%s
EOF
        exit 0
        ;;
      *)
        echo "bd list called with unexpected args: $*" >&2
        exit 2
        ;;
    esac
    ;;
  dep)
    case "$2" in
      list)
        case "$*" in
          *"--direction=up"*"--type=blocks"*"--json"*)
            cat <<'EOF'
%s
EOF
            exit 0
            ;;
          *)
            echo "bd dep list called with unexpected args: $*" >&2
            exit 2
            ;;
        esac
        ;;
      remove|add)
        printf '%%s\n' "$*" >> "$BD_LOG"
        exit 0
        ;;
      *)
        echo "bd dep called with unexpected subcommand: $*" >&2
        exit 2
        ;;
    esac
    ;;
  *)
    echo "bd called with unexpected subcommand: $*" >&2
    exit 2
    ;;
esac
`, closedJSON, depsJSON))

	env = map[string]string{
		"BD_LOG":       bdLog,
		"GC_CITY":      t.TempDir(),
		"GC_CITY_PATH": t.TempDir(),
		"PATH":         binDir + string(os.PathListSeparator) + os.Getenv("PATH"),
	}
	return bdLog, env
}

func TestCrossRigDepsConvertsExternalBlocksToRelated(t *testing.T) {
	closed := `[{"id":"ga-blocker"}]`
	deps := `[{"id":"external:other-rig:rig-dep-1"}]`

	bdLog, env := crossRigDepsEnv(t, closed, deps)
	runScript(t, filepath.Join(exampleDir(), "packs", "maintenance", "assets", "scripts", "cross-rig-deps.sh"), env)

	log, err := os.ReadFile(bdLog)
	if err != nil {
		t.Fatalf("ReadFile(bd log): %v", err)
	}
	s := string(log)
	if !strings.Contains(s, "dep remove external:other-rig:rig-dep-1 external:ga-blocker") {
		t.Fatalf("missing `bd dep remove` for external dep; bd log:\n%s", s)
	}
	if !strings.Contains(s, "dep add external:other-rig:rig-dep-1 external:ga-blocker --type=related") {
		t.Fatalf("missing `bd dep add ... --type=related` for external dep; bd log:\n%s", s)
	}
}

func TestCrossRigDepsReportsResolvedSummary(t *testing.T) {
	closed := `[{"id":"ga-blocker"}]`
	deps := `[{"id":"external:other-rig:rig-dep-1"}]`

	_, env := crossRigDepsEnv(t, closed, deps)
	out, err := runScriptResult(t, filepath.Join(exampleDir(), "packs", "maintenance", "assets", "scripts", "cross-rig-deps.sh"), env)
	if err != nil {
		t.Fatalf("cross-rig-deps.sh failed: %v\n%s", err, out)
	}
	if got, want := strings.TrimSpace(string(out)), "cross-rig-deps: resolved 1 cross-rig dependencies"; got != want {
		t.Fatalf("summary = %q, want %q", got, want)
	}
}

func TestCrossRigDepsSkipsInternalDeps(t *testing.T) {
	closed := `[{"id":"ga-blocker"}]`
	// Internal deps lack the "external:" prefix and must be left untouched
	// — internal blocking semantics are bd's normal computeBlockedIDs path.
	deps := `[{"id":"local-rig-dep"},{"id":"another-internal"}]`

	bdLog, env := crossRigDepsEnv(t, closed, deps)
	runScript(t, filepath.Join(exampleDir(), "packs", "maintenance", "assets", "scripts", "cross-rig-deps.sh"), env)

	log, err := os.ReadFile(bdLog)
	if err != nil {
		t.Fatalf("ReadFile(bd log): %v", err)
	}
	s := string(log)
	if strings.Contains(s, "dep remove") || strings.Contains(s, "dep add") {
		t.Fatalf("internal-only deps must not trigger bd dep remove/add; bd log:\n%s", s)
	}
}

func TestCrossRigDepsNoOpWhenNothingClosed(t *testing.T) {
	bdLog, env := crossRigDepsEnv(t, `[]`, `[]`)
	runScript(t, filepath.Join(exampleDir(), "packs", "maintenance", "assets", "scripts", "cross-rig-deps.sh"), env)

	log, err := os.ReadFile(bdLog)
	if err != nil {
		t.Fatalf("ReadFile(bd log): %v", err)
	}
	if len(log) != 0 {
		t.Fatalf("expected no bd dep calls when nothing recently closed; bd log:\n%s", log)
	}
}

func TestCrossRigDepsHandlesEmptyDepsForClosedBead(t *testing.T) {
	closed := `[{"id":"ga-blocker"}]`
	bdLog, env := crossRigDepsEnv(t, closed, `[]`)
	runScript(t, filepath.Join(exampleDir(), "packs", "maintenance", "assets", "scripts", "cross-rig-deps.sh"), env)

	log, err := os.ReadFile(bdLog)
	if err != nil {
		t.Fatalf("ReadFile(bd log): %v", err)
	}
	if len(log) != 0 {
		t.Fatalf("closed bead with no upward deps should not call bd dep remove/add; bd log:\n%s", log)
	}
}

func TestWispCompactReportsNonZeroCounters(t *testing.T) {
	cityDir := t.TempDir()
	binDir := t.TempDir()
	bdLog := filepath.Join(t.TempDir(), "bd.log")

	pastTTL := "2020-01-01T00:00:00Z"
	withinTTL := time.Now().UTC().Format(time.RFC3339)
	beadsJSON := fmt.Sprintf(`[
  {"id":"ga-old-1","status":"closed","ephemeral":true,"updated_at":"%s","comment_count":0,"labels":[]},
  {"id":"ga-old-2","status":"closed","ephemeral":true,"updated_at":"%s","comment_count":0,"labels":[]},
  {"id":"ga-stuck","status":"open","ephemeral":true,"updated_at":"%s","comment_count":0,"labels":[]},
  {"id":"ga-fresh","status":"closed","ephemeral":true,"updated_at":"%s","comment_count":0,"labels":[]}
]`, pastTTL, pastTTL, pastTTL, withinTTL)

	writeExecutable(t, filepath.Join(binDir, "bd"), fmt.Sprintf(`#!/bin/sh
printf '%%s\n' "$*" >> "$BD_LOG"
case "$1 $2" in
  "list --json")
    cat <<'JSON'
%s
JSON
    ;;
esac
exit 0
`, beadsJSON))

	env := map[string]string{
		"BD_LOG":       bdLog,
		"GC_CITY":      cityDir,
		"GC_CITY_PATH": cityDir,
		"PATH":         binDir + string(os.PathListSeparator) + os.Getenv("PATH"),
	}

	script := filepath.Join(exampleDir(), "packs", "maintenance", "assets", "scripts", "wisp-compact.sh")
	cmd := exec.Command(script)
	cmd.Env = mergeTestEnv(env)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s failed: %v\n%s", filepath.Base(script), err, out)
	}

	logData, err := os.ReadFile(bdLog)
	if err != nil {
		t.Fatalf("ReadFile(bd log): %v", err)
	}
	if !strings.Contains(string(logData), "list --json --all -n 0") {
		t.Fatalf("bd list call not observed:\n%s", logData)
	}

	want := "wisp-compact: promoted=1 deleted=2 skipped=1"
	if !strings.Contains(string(out), want) {
		t.Fatalf("wisp-compact summary missing or wrong (subshell counter regression?)\nwant substring: %q\ngot output:\n%s", want, out)
	}
}

func TestWispCompactBSDDateZFallbackUsesUTC(t *testing.T) {
	cityDir := t.TempDir()
	binDir := t.TempDir()
	bdLog := filepath.Join(t.TempDir(), "bd.log")
	dateLog := filepath.Join(t.TempDir(), "date.log")

	nearBoundary := "2033-05-17T20:33:20Z"
	beadsJSON := fmt.Sprintf(`[
  {"id":"ga-heartbeat","status":"open","ephemeral":true,"updated_at":"%s","comment_count":0,"labels":["wisp_type:heartbeat"]}
]`, nearBoundary)

	writeExecutable(t, filepath.Join(binDir, "bd"), fmt.Sprintf(`#!/bin/sh
printf '%%s\n' "$*" >> "$BD_LOG"
case "$1 $2" in
  "list --json")
    cat <<'JSON'
%s
JSON
    ;;
esac
exit 0
`, beadsJSON))

	writeExecutable(t, filepath.Join(binDir, "date"), fmt.Sprintf(`#!/bin/sh
printf '%%s\n' "$*" >> "$DATE_LOG"
if [ "$1" = "+%%s" ]; then
  echo 2000000000
  exit 0
fi
if [ "$1" = "-d" ]; then
  exit 1
fi
if [ "$1" = "-ju" ] && [ "$2" = "-f" ] && [ "$4" = "%s" ]; then
  echo 1999974800
  exit 0
fi
if [ "$1" = "-j" ] && [ "$2" = "-f" ] && [ "$4" = "%s" ]; then
  echo 2000000000
  exit 0
fi
exit 1
`, nearBoundary, nearBoundary))

	env := map[string]string{
		"BD_LOG":       bdLog,
		"DATE_LOG":     dateLog,
		"GC_CITY":      cityDir,
		"GC_CITY_PATH": cityDir,
		"PATH":         binDir + string(os.PathListSeparator) + os.Getenv("PATH"),
		"TZ":           "America/Los_Angeles",
	}

	script := filepath.Join(exampleDir(), "packs", "maintenance", "assets", "scripts", "wisp-compact.sh")
	cmd := exec.Command(script)
	cmd.Env = mergeTestEnv(env)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s failed: %v\n%s", filepath.Base(script), err, out)
	}

	want := "wisp-compact: promoted=1 deleted=0 skipped=0"
	if !strings.Contains(string(out), want) {
		t.Fatalf("wisp-compact should parse BSD Z timestamps as UTC at the heartbeat TTL boundary\nwant substring: %q\ngot output:\n%s", want, out)
	}

	dateData, err := os.ReadFile(dateLog)
	if err != nil {
		t.Fatalf("ReadFile(date log): %v", err)
	}
	if !strings.Contains(string(dateData), "-ju -f %Y-%m-%dT%H:%M:%SZ "+nearBoundary+" +%s") {
		t.Fatalf("BSD Z fallback did not force UTC:\n%s", dateData)
	}

	bdData, err := os.ReadFile(bdLog)
	if err != nil {
		t.Fatalf("ReadFile(bd log): %v", err)
	}
	if !strings.Contains(string(bdData), "update ga-heartbeat --persistent") {
		t.Fatalf("expected expired heartbeat to be promoted, got bd calls:\n%s", bdData)
	}
}

func TestCrossRigDepsReportsNonZeroCounter(t *testing.T) {
	cityDir := t.TempDir()
	binDir := t.TempDir()
	bdLog := filepath.Join(t.TempDir(), "bd.log")

	closedJSON := `[{"id":"ga-closed-1"},{"id":"ga-closed-2"},{"id":"ga-closed-internal"}]`
	depsForClosed1 := `[{"id":"external:rig-a/ga-dep-1"},{"id":"external:rig-b/ga-dep-2"}]`
	depsForClosed2 := `[{"id":"external:rig-a/ga-dep-3"},{"id":"external:rig-c/ga-dep-4"}]`
	depsForClosedInternal := `[{"id":"ga-internal-1"},{"id":"ga-internal-2"}]`

	writeExecutable(t, filepath.Join(binDir, "bd"), fmt.Sprintf(`#!/bin/sh
printf '%%s\n' "$*" >> "$BD_LOG"
case "$1" in
  list)
    cat <<'JSON'
%s
JSON
    exit 0
    ;;
  dep)
    case "$2 $3" in
      "list ga-closed-1")
        cat <<'JSON'
%s
JSON
        exit 0
        ;;
      "list ga-closed-2")
        cat <<'JSON'
%s
JSON
        exit 0
        ;;
      "list ga-closed-internal")
        cat <<'JSON'
%s
JSON
        exit 0
        ;;
      "remove "*|"add "*)
        exit 0
        ;;
    esac
    ;;
esac
exit 0
`, closedJSON, depsForClosed1, depsForClosed2, depsForClosedInternal))

	env := map[string]string{
		"BD_LOG":       bdLog,
		"GC_CITY":      cityDir,
		"GC_CITY_PATH": cityDir,
		"PATH":         binDir + string(os.PathListSeparator) + os.Getenv("PATH"),
	}

	script := filepath.Join(exampleDir(), "packs", "maintenance", "assets", "scripts", "cross-rig-deps.sh")
	cmd := exec.Command(script)
	cmd.Env = mergeTestEnv(env)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s failed: %v\n%s", filepath.Base(script), err, out)
	}

	logData, err := os.ReadFile(bdLog)
	if err != nil {
		t.Fatalf("ReadFile(bd log): %v", err)
	}
	for _, want := range []string{
		"dep list ga-closed-1",
		"dep list ga-closed-2",
		"dep list ga-closed-internal",
	} {
		if !strings.Contains(string(logData), want) {
			t.Fatalf("bd dep list call %q not observed:\n%s", want, logData)
		}
	}
	if strings.Contains(string(logData), `dep remove "" `) || strings.Contains(string(logData), "dep remove  ") {
		t.Fatalf("bogus empty-dep_id call observed (empty-filter guard regression?):\n%s", logData)
	}

	want := "cross-rig-deps: resolved 4 cross-rig dependencies"
	if !strings.Contains(string(out), want) {
		t.Fatalf("cross-rig-deps summary missing or wrong (subshell counter regression?)\nwant substring: %q\ngot output:\n%s\nbd log:\n%s", want, out, logData)
	}
}
