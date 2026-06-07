package gastown_test

import (
	"encoding/json"
	"errors"
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

const (
	reaperCloseCleanupEdgeSQL   = "(d.type = 'parent-child' OR (d.type = 'tracks' AND JSON_UNQUOTE(JSON_EXTRACT(w.metadata, '$.\"gc.root_bead_id\"')) = COALESCE(d.depends_on_issue_id, d.depends_on_wisp_id, d.depends_on_external)))"
	reaperPurgeProtectEdgeSQL   = "d.type IN ('parent-child', 'tracks', 'blocks')"
	reaperCloseCleanupPredicate = "WISP_CLOSE_EDGE_PREDICATE="
	reaperPurgeProtectTypes     = "WISP_PURGE_PROTECT_EDGE_TYPES="
)

func containsReaperCloseCleanupEdgePredicate(text string) bool {
	if containsSQLFragment(text, reaperCloseCleanupEdgeSQL) {
		return true
	}
	return strings.Contains(text, reaperCloseCleanupPredicate) &&
		strings.Contains(text, "$WISP_CLOSE_EDGE_PREDICATE")
}

func containsReaperPurgeProtectEdgePredicate(text string) bool {
	if containsSQLFragment(text, reaperPurgeProtectEdgeSQL) {
		return true
	}
	return strings.Contains(text, reaperPurgeProtectTypes) &&
		strings.Contains(text, "d.type IN ($WISP_PURGE_PROTECT_EDGE_TYPES)")
}

func containsSQLFragment(text, fragment string) bool {
	return strings.Contains(strings.Join(strings.Fields(text), ""), strings.Join(strings.Fields(fragment), ""))
}

func TestMaintenanceCheckBinariesTreatsGhAsOptional(t *testing.T) {
	binDir := t.TempDir()
	bashPath, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash not available")
	}
	if err := os.Symlink(bashPath, filepath.Join(binDir, "bash")); err != nil {
		t.Fatalf("Symlink(bash): %v", err)
	}
	writeExecutable(t, filepath.Join(binDir, "jq"), "#!/bin/sh\nexit 0\n")

	cmd := exec.Command(filepath.Join(exampleDir(), "packs", "maintenance", "doctor", "check-binaries", "run.sh"))
	cmd.Env = mergeTestEnv(map[string]string{"PATH": binDir})
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("check-binaries failed without gh: %v\n%s", err, out)
	}
	text := string(out)
	if !strings.Contains(text, "all required binaries available (jq)") {
		t.Fatalf("output = %q, want required jq success", text)
	}
	if !strings.Contains(text, "optional gh not found") {
		t.Fatalf("output = %q, want optional gh warning", text)
	}
}

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
if [ "$1" = "--rig" ]; then
  shift 2
fi
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
	  session)
	    if [ "$2" = "list" ] && [ "$3" = "--json" ]; then
	      printf '{"sessions":[],"summary":{},"filters":{},"schema_version":"1"}\n'
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
	    if [ "$2" = "show" ] && [ "$3" = "ga-orphan" ] && [ "$4" = "--json" ]; then
	      cat <<'EOF'
[
  {"id":"ga-orphan","status":"in_progress","assignee":"project/gastown.missing"}
]
EOF
	      exit 0
	    fi
	    if [ "$2" = "release-if-current" ]; then
	      printf 'released\n'
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
	if !strings.Contains(log, "bd release-if-current ga-orphan project/gastown.missing") {
		t.Fatalf("orphan bead was not reset:\n%s", log)
	}
	for _, preserved := range []string{"ga-valid", "ga-pool"} {
		if strings.Contains(log, "bd release-if-current "+preserved+" ") {
			t.Fatalf("valid assignee %s was reset:\n%s", preserved, log)
		}
	}
}

// orphanSweepBareShortFormGCStub writes a gc stub whose only live session is
// the qualified agent "thriva/devpipeline.backend_dev", while the sole
// in-progress bead is assigned to the bare short form "backend_dev". When
// sessionLive is false the session list is empty, so the canonical agent looks
// dead. The bare assignee never matches a configured name, a pool template, a
// dot-stripped form, or a live session identity directly — it can only be
// resolved through the qualified-agent-is-live path under test.
func orphanSweepBareShortFormGCStub(t *testing.T, binDir string, sessionLive bool) {
	t.Helper()
	sessionList := `{"sessions":[],"summary":{},"filters":{},"schema_version":"1"}`
	if sessionLive {
		sessionList = `{"sessions":[` +
			`{"id":"mc-bare-live","session_name":"thriva__devpipeline-backend-dev",` +
			`"alias":"backend_dev-1","agent_name":"thriva/devpipeline.backend_dev","closed":false}` +
			`],"summary":{},"filters":{},"schema_version":"1"}`
	}
	writeExecutable(t, filepath.Join(binDir, "gc"), `#!/bin/sh
printf '%s\n' "$*" >> "$GC_CALL_LOG"
if [ "$1" = "--rig" ]; then
  shift 2
fi
case "$1" in
  config)
    if [ "$2" = "explain" ]; then
      cat <<'EOF'
Agent: thriva/devpipeline.backend_dev
  source: pack
EOF
      exit 0
    fi
    ;;
  rig)
    if [ "$2" = "list" ] && [ "$3" = "--json" ]; then
      printf '{"rigs":[{"name":"hq","hq":true}]}\n'
      exit 0
    fi
    ;;
  session)
    if [ "$2" = "list" ] && [ "$3" = "--json" ]; then
      printf '%s\n' '`+sessionList+`'
      exit 0
    fi
    ;;
  bd)
    if [ "$2" = "show" ] && [ "$3" = "ga-bare" ] && [ "$4" = "--json" ]; then
      cat <<'EOF'
[
  {"id":"ga-bare","status":"in_progress","assignee":"backend_dev"}
]
EOF
      exit 0
    fi
    if [ "$2" = "list" ]; then
      cat <<'EOF'
[
  {"id":"ga-bare","status":"in_progress","assignee":"backend_dev"}
]
EOF
      exit 0
    fi
    if [ "$2" = "release-if-current" ]; then
      printf 'released\n'
      exit 0
    fi
    ;;
esac
exit 1
`)
}

// TestOrphanSweepPreservesBareShortFormOfLiveQualifiedAgent verifies that a
// bead assigned to the bare short form "backend_dev" is preserved when the
// configured qualified agent "thriva/devpipeline.backend_dev" has a live
// session known only by its qualified name. Without the qualified-agent-is-live
// resolution, the live owner's in-progress work would be reset every cycle.
func TestOrphanSweepPreservesBareShortFormOfLiveQualifiedAgent(t *testing.T) {
	cityDir := t.TempDir()
	binDir := t.TempDir()
	gcLog := filepath.Join(t.TempDir(), "gc.log")

	orphanSweepBareShortFormGCStub(t, binDir, true)

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
	if strings.Contains(string(out), "orphan-sweep: reset") {
		t.Fatalf("live qualified agent's bare-short-form assignee was swept:\n%s", out)
	}

	logData, err := os.ReadFile(gcLog)
	if err != nil {
		t.Fatalf("ReadFile(gc log): %v", err)
	}
	if log := string(logData); strings.Contains(log, "bd release-if-current ga-bare ") {
		t.Fatalf("bare-short-form bead of live qualified agent was reset:\n%s", log)
	}
}

// TestOrphanSweepResetsBareShortFormWhenQualifiedAgentDead is the negative
// control for TestOrphanSweepPreservesBareShortFormOfLiveQualifiedAgent: with
// no live session for the qualified agent, the same bare "backend_dev" bead is
// a genuine orphan and must still be reset. This proves the live-owner
// preservation did not weaken the sweep.
func TestOrphanSweepResetsBareShortFormWhenQualifiedAgentDead(t *testing.T) {
	cityDir := t.TempDir()
	binDir := t.TempDir()
	gcLog := filepath.Join(t.TempDir(), "gc.log")

	orphanSweepBareShortFormGCStub(t, binDir, false)

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
		t.Fatalf("dead qualified agent's bare-short-form bead was not swept:\n%s", out)
	}

	logData, err := os.ReadFile(gcLog)
	if err != nil {
		t.Fatalf("ReadFile(gc log): %v", err)
	}
	if log := string(logData); !strings.Contains(log, "bd release-if-current ga-bare backend_dev") {
		t.Fatalf("orphan bead was not reset:\n%s", log)
	}
}

func TestOrphanSweepConfigShowFallbackPreservesQualifiedAssignees(t *testing.T) {
	cityDir := t.TempDir()
	binDir := t.TempDir()
	gcLog := filepath.Join(t.TempDir(), "gc.log")

	writeExecutable(t, filepath.Join(binDir, "gc"), `#!/bin/sh
printf '%s\n' "$*" >> "$GC_CALL_LOG"
if [ "$1" = "--rig" ]; then
  shift 2
fi
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
	  session)
	    if [ "$2" = "list" ] && [ "$3" = "--json" ]; then
	      printf '{"sessions":[],"summary":{},"filters":{},"schema_version":"1"}\n'
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
	    if [ "$2" = "show" ] && [ "$3" = "ga-orphan" ] && [ "$4" = "--json" ]; then
	      cat <<'EOF'
[
  {"id":"ga-orphan","status":"in_progress","assignee":"gastown.missing"}
]
EOF
	      exit 0
	    fi
	    if [ "$2" = "release-if-current" ]; then
	      printf 'released\n'
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
	if !strings.Contains(log, "bd release-if-current ga-orphan gastown.missing") {
		t.Fatalf("orphan bead was not reset:\n%s", log)
	}
	for _, preserved := range []string{"ga-valid", "ga-pool"} {
		if strings.Contains(log, "bd release-if-current "+preserved+" ") {
			t.Fatalf("valid assignee %s was reset:\n%s", preserved, log)
		}
	}
}

func TestOrphanSweepRefreshesLivenessAfterBeadList(t *testing.T) {
	cityDir := t.TempDir()
	binDir := t.TempDir()
	tmpDir := t.TempDir()
	gcLog := filepath.Join(tmpDir, "gc.log")
	sessionCountFile := filepath.Join(tmpDir, "session-count")

	writeExecutable(t, filepath.Join(binDir, "gc"), `#!/bin/sh
printf '%s\n' "$*" >> "$GC_CALL_LOG"
case "$1" in
  config)
    if [ "$2" = "explain" ]; then
      cat <<'EOF'
Agent: project/worker
  source: pack
EOF
      exit 0
    fi
    ;;
  rig)
    if [ "$2" = "list" ] && [ "$3" = "--json" ]; then
      printf '{"rigs":[{"name":"hq","hq":true}]}\n'
      exit 0
    fi
    ;;
  session)
    if [ "$2" = "list" ] && [ "$3" = "--json" ]; then
      count="$(cat "$GC_SESSION_COUNT_FILE" 2>/dev/null || printf '0')"
      count=$((count + 1))
      printf '%s' "$count" > "$GC_SESSION_COUNT_FILE"
      if [ "$count" -eq 1 ]; then
        printf '{"sessions":[],"summary":{},"filters":{},"schema_version":"1"}\n'
      else
        cat <<'EOF'
{"sessions":[
  {"id":"mc-live","session_name":"project__worker-gc-race","alias":"project/worker-1","agent_name":"project/worker","closed":false}
],"summary":{},"filters":{},"schema_version":"1"}
EOF
      fi
      exit 0
    fi
    ;;
  bd)
    if [ "$2" = "list" ]; then
      cat <<'EOF'
[
  {"id":"ga-live-race","status":"in_progress","assignee":"project__worker-gc-race"}
]
EOF
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
		"GC_CITY":               cityDir,
		"GC_CITY_PATH":          cityDir,
		"GC_CALL_LOG":           gcLog,
		"GC_SESSION_COUNT_FILE": sessionCountFile,
		"PATH":                  binDir + string(os.PathListSeparator) + os.Getenv("PATH"),
	}

	script := filepath.Join(exampleDir(), "packs", "maintenance", "assets", "scripts", "orphan-sweep.sh")
	cmd := exec.Command(script)
	cmd.Env = mergeTestEnv(env)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s failed: %v\n%s", filepath.Base(script), err, out)
	}
	if strings.Contains(string(out), "orphan-sweep: reset") {
		t.Fatalf("unexpected orphan reset output:\n%s", out)
	}

	logData, err := os.ReadFile(gcLog)
	if err != nil {
		t.Fatalf("ReadFile(gc log): %v", err)
	}
	log := string(logData)
	if got := strings.Count(log, "session list --json"); got != 2 {
		t.Fatalf("session list count = %d, want 2 post-bd refresh:\n%s", got, log)
	}
	if strings.Contains(log, "bd update ga-live-race ") {
		t.Fatalf("live session claimed between liveness and bead list was reset:\n%s", log)
	}
}

func TestOrphanSweepRefreshesRigLivenessAfterBeadList(t *testing.T) {
	cityDir := t.TempDir()
	binDir := t.TempDir()
	tmpDir := t.TempDir()
	gcLog := filepath.Join(tmpDir, "gc.log")
	rigSessionCountFile := filepath.Join(tmpDir, "rig-session-count")

	writeExecutable(t, filepath.Join(binDir, "gc"), `#!/bin/sh
printf '%s\n' "$*" >> "$GC_CALL_LOG"
if [ "$1" = "--rig" ]; then
  rig="$2"
  shift 2
else
  rig=""
fi
case "$1" in
  config)
    if [ "$2" = "explain" ]; then
      cat <<'EOF'
Agent: project/worker
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
  session)
    if [ "$2" = "list" ] && [ "$3" = "--json" ]; then
      if [ "$rig" = "project" ]; then
        count="$(cat "$GC_RIG_SESSION_COUNT_FILE" 2>/dev/null || printf '0')"
        count=$((count + 1))
        printf '%s' "$count" > "$GC_RIG_SESSION_COUNT_FILE"
        if [ "$count" -eq 1 ]; then
          printf '{"sessions":[],"summary":{},"filters":{},"schema_version":"1"}\n'
        else
          cat <<'EOF'
{"sessions":[
  {"id":"mc-rig-live","session_name":"project__worker-gc-race","alias":"project/worker-1","agent_name":"project/worker","closed":false}
],"summary":{},"filters":{},"schema_version":"1"}
EOF
        fi
        exit 0
      fi
      printf '{"sessions":[],"summary":{},"filters":{},"schema_version":"1"}\n'
      exit 0
    fi
    ;;
  bd)
    if [ "$2" = "list" ]; then
      if [ "$rig" = "project" ]; then
        cat <<'EOF'
[
  {"id":"ga-rig-live-race","status":"in_progress","assignee":"project__worker-gc-race"}
]
EOF
      else
        printf '[]\n'
      fi
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
		"GC_CITY":                   cityDir,
		"GC_CITY_PATH":              cityDir,
		"GC_CALL_LOG":               gcLog,
		"GC_RIG_SESSION_COUNT_FILE": rigSessionCountFile,
		"PATH":                      binDir + string(os.PathListSeparator) + os.Getenv("PATH"),
	}

	script := filepath.Join(exampleDir(), "packs", "maintenance", "assets", "scripts", "orphan-sweep.sh")
	cmd := exec.Command(script)
	cmd.Env = mergeTestEnv(env)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s failed: %v\n%s", filepath.Base(script), err, out)
	}
	if strings.Contains(string(out), "orphan-sweep: reset") {
		t.Fatalf("unexpected orphan reset output:\n%s", out)
	}

	logData, err := os.ReadFile(gcLog)
	if err != nil {
		t.Fatalf("ReadFile(gc log): %v", err)
	}
	log := string(logData)
	if got := strings.Count(log, "session list --json"); got != 4 {
		t.Fatalf("session list count = %d, want 4 including rig post-bd refresh:\n%s", got, log)
	}
	if strings.Contains(log, "bd update ga-rig-live-race ") {
		t.Fatalf("rig live session claimed between liveness and bead list was reset:\n%s", log)
	}
}

func TestOrphanSweepRevalidatesWorkBeadBeforeReset(t *testing.T) {
	cityDir := t.TempDir()
	binDir := t.TempDir()
	gcLog := filepath.Join(t.TempDir(), "gc.log")

	writeExecutable(t, filepath.Join(binDir, "gc"), `#!/bin/sh
printf '%s\n' "$*" >> "$GC_CALL_LOG"
case "$1" in
  config)
    if [ "$2" = "explain" ]; then
      cat <<'EOF'
Agent: project/worker
  source: pack
EOF
      exit 0
    fi
    ;;
  rig)
    if [ "$2" = "list" ] && [ "$3" = "--json" ]; then
      printf '{"rigs":[{"name":"hq","hq":true}]}\n'
      exit 0
    fi
    ;;
  session)
    if [ "$2" = "list" ] && [ "$3" = "--json" ]; then
      printf '{"sessions":[],"summary":{},"filters":{},"schema_version":"1"}\n'
      exit 0
    fi
    ;;
  bd)
    if [ "$2" = "list" ]; then
      cat <<'EOF'
[
  {"id":"ga-raced-closed","status":"in_progress","assignee":"project__worker-gc-mc-wisp-raced"}
]
EOF
      exit 0
    fi
    if [ "$2" = "show" ] && [ "$3" = "ga-raced-closed" ] && [ "$4" = "--json" ]; then
      cat <<'EOF'
[
  {"id":"ga-raced-closed","status":"closed","assignee":"project__worker-gc-mc-wisp-raced","metadata":{"gc.outcome":"pass"}}
]
EOF
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
	if strings.Contains(string(out), "orphan-sweep: reset") {
		t.Fatalf("unexpected orphan reset output:\n%s", out)
	}

	logData, err := os.ReadFile(gcLog)
	if err != nil {
		t.Fatalf("ReadFile(gc log): %v", err)
	}
	log := string(logData)
	if !strings.Contains(log, "bd show ga-raced-closed --json") {
		t.Fatalf("work bead was not revalidated before reset:\n%s", log)
	}
	if strings.Contains(log, "bd update ga-raced-closed ") {
		t.Fatalf("closed work bead was reopened by orphan-sweep:\n%s", log)
	}
}

func TestOrphanSweepUsesConditionalResetAfterRevalidation(t *testing.T) {
	cityDir := t.TempDir()
	binDir := t.TempDir()
	gcLog := filepath.Join(t.TempDir(), "gc.log")

	writeExecutable(t, filepath.Join(binDir, "gc"), `#!/bin/sh
printf '%s\n' "$*" >> "$GC_CALL_LOG"
case "$1" in
  config)
    if [ "$2" = "explain" ]; then
      cat <<'EOF'
Agent: project/worker
  source: pack
EOF
      exit 0
    fi
    ;;
  rig)
    if [ "$2" = "list" ] && [ "$3" = "--json" ]; then
      printf '{"rigs":[{"name":"hq","hq":true}]}\n'
      exit 0
    fi
    ;;
  session)
    if [ "$2" = "list" ] && [ "$3" = "--json" ]; then
      printf '{"sessions":[],"summary":{},"filters":{},"schema_version":"1"}\n'
      exit 0
    fi
    ;;
  bd)
    if [ "$2" = "list" ]; then
      cat <<'EOF'
[
  {"id":"ga-raced-after-show","status":"in_progress","assignee":"project__worker-gc-mc-wisp-raced"}
]
EOF
      exit 0
    fi
    if [ "$2" = "show" ] && [ "$3" = "ga-raced-after-show" ] && [ "$4" = "--json" ]; then
      cat <<'EOF'
[
  {"id":"ga-raced-after-show","status":"in_progress","assignee":"project__worker-gc-mc-wisp-raced","metadata":{}}
]
EOF
      exit 0
    fi
    if [ "$2" = "show" ] && [ "$3" = "mc-wisp-raced" ] && [ "$4" = "--json" ]; then
      cat <<'EOF'
[
  {"id":"mc-wisp-raced","status":"closed","issue_type":"session","metadata":{"state":"closed"}}
]
EOF
      exit 0
    fi
    if [ "$2" = "release-if-current" ] && [ "$3" = "ga-raced-after-show" ] && [ "$4" = "project__worker-gc-mc-wisp-raced" ]; then
      printf 'skipped\n'
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
	if strings.Contains(string(out), "orphan-sweep: reset") {
		t.Fatalf("conditional reset skip was counted as a reset:\n%s", out)
	}

	logData, err := os.ReadFile(gcLog)
	if err != nil {
		t.Fatalf("ReadFile(gc log): %v", err)
	}
	log := string(logData)
	if !strings.Contains(log, "bd show ga-raced-after-show --json") {
		t.Fatalf("work bead was not revalidated before reset:\n%s", log)
	}
	if !strings.Contains(log, "bd release-if-current ga-raced-after-show project__worker-gc-mc-wisp-raced") {
		t.Fatalf("work bead reset did not use conditional helper:\n%s", log)
	}
	if strings.Contains(log, "bd update ga-raced-after-show ") {
		t.Fatalf("stale bead was reset with unconditional update:\n%s", log)
	}
}

func TestOrphanSweepUsesSessionBeadShowWhenSessionListLags(t *testing.T) {
	cityDir := t.TempDir()
	binDir := t.TempDir()
	gcLog := filepath.Join(t.TempDir(), "gc.log")

	writeExecutable(t, filepath.Join(binDir, "gc"), `#!/bin/sh
printf '%s\n' "$*" >> "$GC_CALL_LOG"
case "$1" in
  config)
    if [ "$2" = "explain" ]; then
      cat <<'EOF'
Agent: project/worker
  source: pack
EOF
      exit 0
    fi
    ;;
  rig)
    if [ "$2" = "list" ] && [ "$3" = "--json" ]; then
      printf '{"rigs":[{"name":"hq","hq":true}]}\n'
      exit 0
    fi
    ;;
  session)
    if [ "$2" = "list" ] && [ "$3" = "--json" ]; then
      printf '{"sessions":[],"summary":{},"filters":{},"schema_version":"1"}\n'
      exit 0
    fi
    ;;
  bd)
    if [ "$2" = "list" ]; then
      cat <<'EOF'
[
  {"id":"ga-session-lag","status":"in_progress","assignee":"project__worker-gc-mc-wisp-live123"}
]
EOF
      exit 0
    fi
    if [ "$2" = "show" ] && [ "$3" = "ga-session-lag" ] && [ "$4" = "--json" ]; then
      cat <<'EOF'
[
  {"id":"ga-session-lag","status":"in_progress","assignee":"project__worker-gc-mc-wisp-live123","metadata":{"gc.session_name":"project__worker-gc-mc-wisp-live123"}}
]
EOF
      exit 0
    fi
    if [ "$2" = "show" ] && [ "$3" = "mc-wisp-live123" ] && [ "$4" = "--json" ]; then
      cat <<'EOF'
[
  {"id":"mc-wisp-live123","status":"open","issue_type":"session","metadata":{"state":"start-pending","session_name":"project__worker-gc-mc-wisp-live123"}}
]
EOF
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
	if strings.Contains(string(out), "orphan-sweep: reset") {
		t.Fatalf("unexpected orphan reset output:\n%s", out)
	}

	logData, err := os.ReadFile(gcLog)
	if err != nil {
		t.Fatalf("ReadFile(gc log): %v", err)
	}
	log := string(logData)
	for _, want := range []string{
		"bd show ga-session-lag --json",
		"bd show mc-wisp-live123 --json",
	} {
		if !strings.Contains(log, want) {
			t.Fatalf("missing required gc call %q:\n%s", want, log)
		}
	}
	if strings.Contains(log, "bd update ga-session-lag ") {
		t.Fatalf("session-list lag caused live assigned work to reset:\n%s", log)
	}
}

func TestOrphanSweepPreservesWorkWhenSessionBeadProbeErrors(t *testing.T) {
	cityDir := t.TempDir()
	binDir := t.TempDir()
	gcLog := filepath.Join(t.TempDir(), "gc.log")

	writeExecutable(t, filepath.Join(binDir, "gc"), `#!/bin/sh
printf '%s\n' "$*" >> "$GC_CALL_LOG"
case "$1" in
  config)
    if [ "$2" = "explain" ]; then
      cat <<'EOF'
Agent: project/worker
  source: pack
EOF
      exit 0
    fi
    ;;
  rig)
    if [ "$2" = "list" ] && [ "$3" = "--json" ]; then
      printf '{"rigs":[{"name":"hq","hq":true}]}\n'
      exit 0
    fi
    ;;
  session)
    if [ "$2" = "list" ] && [ "$3" = "--json" ]; then
      printf '{"sessions":[],"summary":{},"filters":{},"schema_version":"1"}\n'
      exit 0
    fi
    ;;
  bd)
    if [ "$2" = "list" ]; then
      cat <<'EOF'
[
  {"id":"ga-session-probe-error","status":"in_progress","assignee":"project__worker-gc-mc-wisp-live123"}
]
EOF
      exit 0
    fi
    if [ "$2" = "show" ] && [ "$3" = "ga-session-probe-error" ] && [ "$4" = "--json" ]; then
      cat <<'EOF'
[
  {"id":"ga-session-probe-error","status":"in_progress","assignee":"project__worker-gc-mc-wisp-live123","metadata":{"gc.session_name":"project__worker-gc-mc-wisp-live123"}}
]
EOF
      exit 0
    fi
    if [ "$2" = "show" ] && [ "$3" = "mc-wisp-live123" ] && [ "$4" = "--json" ]; then
      printf 'transient read failure\n' >&2
      exit 2
    fi
    if [ "$2" = "release-if-current" ]; then
      printf 'released\n'
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
	if !strings.Contains(string(out), "orphan-sweep: reset 0 orphaned beads, skipped 1 unverifiable") {
		t.Fatalf("unexpected orphan-sweep output:\n%s", out)
	}

	logData, err := os.ReadFile(gcLog)
	if err != nil {
		t.Fatalf("ReadFile(gc log): %v", err)
	}
	log := string(logData)
	if !strings.Contains(log, "bd show mc-wisp-live123 --json") {
		t.Fatalf("session bead candidate was not probed:\n%s", log)
	}
	if strings.Contains(log, "bd release-if-current ga-session-probe-error ") {
		t.Fatalf("session probe error fell through to reset:\n%s", log)
	}
}

func TestOrphanSweepUsesDirectSessionBeadCandidatesWhenSessionListLags(t *testing.T) {
	tests := []struct {
		name      string
		workID    string
		assignee  string
		sessionID string
	}{
		{
			name:      "bare session bead assignee",
			workID:    "ga-bare-session-bead",
			assignee:  "mc-live-rig-asleep",
			sessionID: "mc-live-rig-asleep",
		},
		{
			name:      "generic mc suffix",
			workID:    "ga-generic-mc-suffix",
			assignee:  "project__worker-gc-mc-live-rig-asleep",
			sessionID: "mc-live-rig-asleep",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cityDir := t.TempDir()
			binDir := t.TempDir()
			gcLog := filepath.Join(t.TempDir(), "gc.log")

			writeExecutable(t, filepath.Join(binDir, "gc"), fmt.Sprintf(`#!/bin/sh
printf '%%s\n' "$*" >> "$GC_CALL_LOG"
case "$1" in
  config)
    if [ "$2" = "explain" ]; then
      cat <<'EOF'
Agent: project/worker
  source: pack
EOF
      exit 0
    fi
    ;;
  rig)
    if [ "$2" = "list" ] && [ "$3" = "--json" ]; then
      printf '{"rigs":[{"name":"hq","hq":true}]}\n'
      exit 0
    fi
    ;;
  session)
    if [ "$2" = "list" ] && [ "$3" = "--json" ]; then
      printf '{"sessions":[],"summary":{},"filters":{},"schema_version":"1"}\n'
      exit 0
    fi
    ;;
  bd)
    if [ "$2" = "list" ]; then
      cat <<'EOF'
[
  {"id":%q,"status":"in_progress","assignee":%q}
]
EOF
      exit 0
    fi
    if [ "$2" = "show" ] && [ "$3" = %q ] && [ "$4" = "--json" ]; then
      cat <<'EOF'
[
  {"id":%q,"status":"in_progress","assignee":%q}
]
EOF
      exit 0
    fi
    if [ "$2" = "show" ] && [ "$3" = %q ] && [ "$4" = "--json" ]; then
      cat <<'EOF'
[
  {"id":%q,"status":"open","issue_type":"session","metadata":{"state":"active","session_name":%q}}
]
EOF
      exit 0
    fi
    if [ "$2" = "update" ]; then
      exit 0
    fi
    ;;
esac
exit 1
`, tt.workID, tt.assignee, tt.workID, tt.workID, tt.assignee, tt.sessionID, tt.sessionID, tt.assignee))

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
			if strings.Contains(string(out), "orphan-sweep: reset") {
				t.Fatalf("unexpected orphan reset output:\n%s", out)
			}

			logData, err := os.ReadFile(gcLog)
			if err != nil {
				t.Fatalf("ReadFile(gc log): %v", err)
			}
			log := string(logData)
			for _, want := range []string{
				"bd show " + tt.workID + " --json",
				"bd show " + tt.sessionID + " --json",
			} {
				if !strings.Contains(log, want) {
					t.Fatalf("missing required gc call %q:\n%s", want, log)
				}
			}
			if strings.Contains(log, "bd update "+tt.workID+" ") {
				t.Fatalf("session-list lag caused live assigned work to reset:\n%s", log)
			}
		})
	}
}

func TestOrphanSweepPreservesRigScopedLiveEphemeralSessionAssigneesFromCurrentSchema(t *testing.T) {
	cityDir := t.TempDir()
	binDir := t.TempDir()
	gcLog := filepath.Join(t.TempDir(), "gc.log")

	writeExecutable(t, filepath.Join(binDir, "gc"), `#!/bin/sh
printf '%s\n' "$*" >> "$GC_CALL_LOG"
if [ "$1" = "--rig" ]; then
  rig="$2"
  shift 2
else
  rig=""
fi
case "$1" in
  config)
    if [ "$2" = "explain" ]; then
      cat <<'EOF'
Agent: project/worker
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
  session)
    if [ "$2" = "list" ] && [ "$3" = "--json" ]; then
      if [ "$rig" = "project" ]; then
        cat <<'EOF'
{"sessions":[
  {"id":"mc-live-rig","name":"project/worker-1","template":"project/worker","state":"active","session_name":"project__worker-gc-rig123","alias":"project/worker-1","agent_name":"project/worker","closed":false},
  {"id":"mc-live-rig-asleep","name":"project/worker-2","template":"project/worker","state":"asleep","session_name":"project__worker-gc-asleep","alias":"project/worker-2","agent_name":"project/worker","closed":false}
],"summary":{},"filters":{},"schema_version":"1"}
EOF
        exit 0
      fi
      printf '{"sessions":[],"summary":{},"filters":{},"schema_version":"1"}\n'
      exit 0
    fi
    ;;
  bd)
	    if [ "$2" = "list" ]; then
	      if [ "$4" = "project" ]; then
	        cat <<'EOF'
[
  {"id":"ga-rig-live-by-session-name","status":"in_progress","assignee":"project__worker-gc-rig123"},
  {"id":"ga-rig-live-by-id","status":"in_progress","assignee":"mc-live-rig-asleep"},
  {"id":"ga-rig-closed-default-filtered","status":"in_progress","assignee":"project__worker-gc-closed"},
  {"id":"ga-rig-orphan","status":"in_progress","assignee":"missing-rig-session"}
]
EOF
      else
        printf '[]\n'
	      fi
	      exit 0
	    fi
	    if [ "$2" = "show" ] && [ "$3" = "ga-rig-orphan" ] && [ "$4" = "--json" ]; then
	      cat <<'EOF'
[
  {"id":"ga-rig-orphan","status":"in_progress","assignee":"missing-rig-session"}
]
EOF
	      exit 0
	    fi
	    if [ "$2" = "show" ] && [ "$3" = "ga-rig-closed-default-filtered" ] && [ "$4" = "--json" ]; then
	      cat <<'EOF'
[
  {"id":"ga-rig-closed-default-filtered","status":"in_progress","assignee":"project__worker-gc-closed"}
]
EOF
	      exit 0
	    fi
	    if [ "$2" = "release-if-current" ]; then
	      printf 'released\n'
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
	if !strings.Contains(string(out), "orphan-sweep: reset 2 orphaned beads") {
		t.Fatalf("unexpected orphan-sweep output:\n%s", out)
	}

	logData, err := os.ReadFile(gcLog)
	if err != nil {
		t.Fatalf("ReadFile(gc log): %v", err)
	}
	log := string(logData)
	if !strings.Contains(log, "--rig project session list --json") {
		t.Fatalf("rig-scoped session list was not queried:\n%s", log)
	}
	if !strings.Contains(log, "bd release-if-current ga-rig-orphan missing-rig-session") {
		t.Fatalf("rig orphan bead was not reset:\n%s", log)
	}
	if !strings.Contains(log, "bd release-if-current ga-rig-closed-default-filtered project__worker-gc-closed") {
		t.Fatalf("closed/default-filtered session assignee was not reset:\n%s", log)
	}
	for _, preserved := range []string{"ga-rig-live-by-session-name", "ga-rig-live-by-id"} {
		if strings.Contains(log, "bd release-if-current "+preserved+" ") {
			t.Fatalf("rig live ephemeral session assignee %s was reset:\n%s", preserved, log)
		}
	}
}

func TestOrphanSweepPreservesPascalCaseLiveSessionIdentitiesAsForwardCompat(t *testing.T) {
	cityDir := t.TempDir()
	binDir := t.TempDir()
	gcLog := filepath.Join(t.TempDir(), "gc.log")

	writeExecutable(t, filepath.Join(binDir, "gc"), `#!/bin/sh
printf '%s\n' "$*" >> "$GC_CALL_LOG"
if [ "$1" = "--rig" ]; then
  rig="$2"
  shift 2
else
  rig=""
fi
case "$1" in
  config)
    if [ "$2" = "explain" ]; then
      cat <<'EOF'
Agent: project/worker
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
  session)
    if [ "$2" = "list" ] && [ "$3" = "--json" ]; then
      if [ "$rig" = "project" ]; then
        cat <<'EOF'
{"sessions":[
  {"ID":"vgc-live-id","SessionName":"project__worker-vgc-live-name","Alias":"project/worker-1","Template":"project/worker","State":"active","Closed":false},
  {"ID":"vgc-closed","SessionName":"project__worker-vgc-closed","State":"closed","Closed":true}
],"summary":{},"filters":{},"schema_version":"1"}
EOF
        exit 0
      fi
      printf '{"sessions":[],"summary":{},"filters":{},"schema_version":"1"}\n'
      exit 0
    fi
    ;;
  bd)
	    if [ "$2" = "list" ]; then
	      if [ "$3" = "--rig" ] && [ "$4" = "project" ]; then
	        cat <<'EOF'
[
  {"id":"ga-live-by-id","status":"in_progress","assignee":"vgc-live-id"},
  {"id":"ga-live-by-session-name","status":"in_progress","assignee":"project__worker-vgc-live-name"},
  {"id":"ga-closed-session","status":"in_progress","assignee":"vgc-closed"},
  {"id":"ga-missing-session","status":"in_progress","assignee":"missing-session"}
]
EOF
      else
        printf '[]\n'
	      fi
	      exit 0
	    fi
	    if [ "$2" = "show" ] && [ "$3" = "ga-closed-session" ] && [ "$4" = "--json" ]; then
	      cat <<'EOF'
[
  {"id":"ga-closed-session","status":"in_progress","assignee":"vgc-closed"}
]
EOF
	      exit 0
	    fi
	    if [ "$2" = "show" ] && [ "$3" = "ga-missing-session" ] && [ "$4" = "--json" ]; then
	      cat <<'EOF'
[
  {"id":"ga-missing-session","status":"in_progress","assignee":"missing-session"}
]
EOF
	      exit 0
	    fi
	    if [ "$2" = "release-if-current" ]; then
	      printf 'released\n'
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
	if !strings.Contains(string(out), "orphan-sweep: reset 2 orphaned beads") {
		t.Fatalf("unexpected orphan-sweep output:\n%s", out)
	}

	logData, err := os.ReadFile(gcLog)
	if err != nil {
		t.Fatalf("ReadFile(gc log): %v", err)
	}
	log := string(logData)
	for _, preserved := range []string{"ga-live-by-id", "ga-live-by-session-name"} {
		if strings.Contains(log, "bd release-if-current "+preserved+" ") {
			t.Fatalf("forward-compatible PascalCase session assignee %s was reset:\n%s", preserved, log)
		}
	}
	for reset, assignee := range map[string]string{
		"ga-closed-session":  "vgc-closed",
		"ga-missing-session": "missing-session",
	} {
		if !strings.Contains(log, "bd release-if-current "+reset+" "+assignee) {
			t.Fatalf("expected %s to be reset:\n%s", reset, log)
		}
	}
}

func TestOrphanSweepContinuesAfterSingleRigSessionListFailure(t *testing.T) {
	cityDir := t.TempDir()
	binDir := t.TempDir()
	gcLog := filepath.Join(t.TempDir(), "gc.log")

	writeExecutable(t, filepath.Join(binDir, "gc"), `#!/bin/sh
printf '%s\n' "$*" >> "$GC_CALL_LOG"
if [ "$1" = "--rig" ]; then
  rig="$2"
  shift 2
else
  rig=""
fi
case "$1" in
  config)
    if [ "$2" = "explain" ]; then
      cat <<'EOF'
Agent: project/worker
  source: pack
EOF
      exit 0
    fi
    ;;
  rig)
    if [ "$2" = "list" ] && [ "$3" = "--json" ]; then
      printf '{"rigs":[{"name":"hq","hq":true},{"name":"broken","hq":false},{"name":"healthy","hq":false}]}\n'
      exit 0
    fi
    ;;
  session)
    if [ "$2" = "list" ] && [ "$3" = "--json" ]; then
      if [ "$rig" = "broken" ]; then
        exit 1
      fi
      printf '{"sessions":[],"summary":{},"filters":{},"schema_version":"1"}\n'
      exit 0
    fi
    ;;
  bd)
	    if [ "$2" = "list" ]; then
	      if [ "$3" = "--rig" ] && [ "$4" = "broken" ]; then
	        cat <<'EOF'
[
  {"id":"ga-broken-orphan","status":"in_progress","assignee":"missing-broken-session"}
]
EOF
      elif [ "$3" = "--rig" ] && [ "$4" = "healthy" ]; then
        cat <<'EOF'
[
  {"id":"ga-healthy-orphan","status":"in_progress","assignee":"missing-healthy-session"}
]
EOF
      else
        cat <<'EOF'
[
  {"id":"ga-hq-orphan","status":"in_progress","assignee":"missing-hq-session"}
]
EOF
	      fi
	      exit 0
	    fi
	    if [ "$2" = "show" ] && [ "$3" = "ga-hq-orphan" ] && [ "$4" = "--json" ]; then
	      cat <<'EOF'
[
  {"id":"ga-hq-orphan","status":"in_progress","assignee":"missing-hq-session"}
]
EOF
	      exit 0
	    fi
	    if [ "$2" = "show" ] && [ "$3" = "ga-healthy-orphan" ] && [ "$4" = "--json" ]; then
	      cat <<'EOF'
[
  {"id":"ga-healthy-orphan","status":"in_progress","assignee":"missing-healthy-session"}
]
EOF
	      exit 0
	    fi
	    if [ "$2" = "release-if-current" ]; then
	      printf 'released\n'
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
	if !strings.Contains(string(out), "orphan-sweep: reset 2 orphaned beads") {
		t.Fatalf("unexpected orphan-sweep output:\n%s", out)
	}

	logData, err := os.ReadFile(gcLog)
	if err != nil {
		t.Fatalf("ReadFile(gc log): %v", err)
	}
	log := string(logData)
	for reset, assignee := range map[string]string{
		"ga-hq-orphan":      "missing-hq-session",
		"ga-healthy-orphan": "missing-healthy-session",
	} {
		if !strings.Contains(log, "bd release-if-current "+reset+" "+assignee) {
			t.Fatalf("expected %s to be reset after partial rig failure:\n%s", reset, log)
		}
	}
	if strings.Contains(log, "bd release-if-current ga-broken-orphan ") {
		t.Fatalf("bead from rig with unknown session liveness was reset:\n%s", log)
	}
}

func TestOrphanSweepPreservesProtectedInProgressEphemeralMoleculeWisp(t *testing.T) {
	tests := []struct {
		name               string
		scope              string
		configuredIdentity string
		protectedID        string
		protectedAssignee  string
		orphanID           string
		orphanAssignee     string
		liveSessionName    string
	}{
		{
			name:               "hq-reported-shape",
			scope:              "hq",
			configuredIdentity: "gastown.deacon",
			protectedID:        "gc-wisp-protected-hq-1578",
			protectedAssignee:  "gastown.deacon",
			orphanID:           "gc-wisp-orphan-hq-1578",
			orphanAssignee:     "ghost.worker-404",
		},
		{
			name:               "rig-neutral-direct",
			scope:              "project-alpha",
			configuredIdentity: "project-alpha/custom.worker",
			protectedID:        "gc-wisp-protected-neutral-1578",
			protectedAssignee:  "project-alpha/custom.worker",
			orphanID:           "gc-wisp-orphan-neutral-1578",
			orphanAssignee:     "project-alpha/missing.worker-404",
		},
		{
			name:               "rig-refinery-direct",
			scope:              "project-alpha",
			configuredIdentity: "project-alpha/gastown.refinery",
			protectedID:        "gc-wisp-protected-refinery-1578",
			protectedAssignee:  "project-alpha/gastown.refinery",
			orphanID:           "gc-wisp-orphan-refinery-1578",
			orphanAssignee:     "project-alpha/gastown.retired-404",
		},
		{
			name:               "rig-witness-direct",
			scope:              "project-alpha",
			configuredIdentity: "project-alpha/gastown.witness",
			protectedID:        "gc-wisp-protected-witness-1578",
			protectedAssignee:  "project-alpha/gastown.witness",
			orphanID:           "gc-wisp-orphan-witness-1578",
			orphanAssignee:     "project-alpha/gastown.missing-404",
		},
		{
			name:               "rig-pool-instance",
			scope:              "project-alpha",
			configuredIdentity: "project-alpha/gastown.refinery",
			protectedID:        "gc-wisp-protected-pool-1578",
			protectedAssignee:  "project-alpha/gastown.refinery-3",
			orphanID:           "gc-wisp-orphan-pool-1578",
			orphanAssignee:     "project-alpha/gastown.retired-3",
		},
		{
			name:               "rig-live-session-only",
			scope:              "project-alpha",
			configuredIdentity: "project-alpha/gastown.refinery",
			protectedID:        "gc-wisp-protected-live-1578",
			protectedAssignee:  "project-alpha__gastown-refinery-gc-live1578",
			orphanID:           "gc-wisp-orphan-live-1578",
			orphanAssignee:     "project-alpha__gastown-retired-gc-live1578",
			liveSessionName:    "project-alpha__gastown-refinery-gc-live1578",
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			binDir := filepath.Join(root, "bin")
			if err := os.MkdirAll(binDir, 0o755); err != nil {
				t.Fatalf("MkdirAll(%s): %v", binDir, err)
			}
			for _, name := range []string{"bash", "cat", "dirname", "mktemp", "jq", "awk", "grep", "sed", "rm"} {
				linkTestPathTool(t, binDir, name)
			}

			gcLog := filepath.Join(root, "gc.log")
			if err := os.WriteFile(gcLog, nil, 0o644); err != nil {
				t.Fatalf("WriteFile(%s): %v", gcLog, err)
			}
			fakeGC := filepath.Join(binDir, "gc")
			writeStrictOrphanSweepGCStub(t, fakeGC)

			beadsJSON := orphanSweepProtectedWispBeadsJSON(t, tt.protectedID, tt.protectedAssignee, tt.orphanID, tt.orphanAssignee)
			hqJSON := "[]"
			rigJSON := "[]"
			hqSessionsJSON := orphanSweepSessionListJSON(t)
			rigSessionsJSON := orphanSweepSessionListJSON(t)
			switch tt.scope {
			case "hq":
				hqJSON = beadsJSON
				if tt.liveSessionName != "" {
					hqSessionsJSON = orphanSweepSessionListJSON(t, tt.liveSessionName)
				}
			case "project-alpha":
				rigJSON = beadsJSON
				if tt.liveSessionName != "" {
					rigSessionsJSON = orphanSweepSessionListJSON(t, tt.liveSessionName)
				}
			default:
				t.Fatalf("unsupported scope %q", tt.scope)
			}

			env := orphanSweepCleanroomEnv(t, root, binDir, gcLog, orphanSweepCleanroomEnvConfig{
				hqJSON:             hqJSON,
				rigJSON:            rigJSON,
				hqSessionsJSON:     hqSessionsJSON,
				rigSessionsJSON:    rigSessionsJSON,
				configuredIdentity: tt.configuredIdentity,
				orphanID:           tt.orphanID,
				orphanAssignee:     tt.orphanAssignee,
			})
			assertOrphanSweepFakeGC(t, env, filepath.Join(binDir, "bash"), fakeGC, gcLog)

			script := filepath.Join(exampleDir(), "packs", "maintenance", "assets", "scripts", "orphan-sweep.sh")
			cmd := exec.Command(script)
			cmd.Env = env
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("%s failed: %v\n%s", filepath.Base(script), err, orphanSweepFailureContext(out, gcLog))
			}
			if got, want := strings.TrimSpace(string(out)), "orphan-sweep: reset 1 orphaned beads"; got != want {
				t.Fatalf("orphan-sweep output = %q, want %q\n%s", got, want, orphanSweepFailureContext(out, gcLog))
			}

			logData, err := os.ReadFile(gcLog)
			if err != nil {
				t.Fatalf("ReadFile(%s): %v", gcLog, err)
			}
			log := string(logData)
			lines := nonEmptyLogLines(log)
			orphanUpdate := "bd release-if-current " + tt.orphanID + " " + tt.orphanAssignee
			if got := countExactLine(lines, orphanUpdate); got != 1 {
				t.Fatalf("orphan update count = %d, want 1 for %q\n%s", got, orphanUpdate, orphanSweepFailureContext(out, gcLog))
			}
			if strings.Contains(log, "bd release-if-current "+tt.protectedID+" ") {
				t.Fatalf("protected wisp %s was reset\n%s", tt.protectedID, orphanSweepFailureContext(out, gcLog))
			}
			if strings.Contains(log, "UNEXPECTED:") {
				t.Fatalf("unexpected fake gc invocation\n%s", orphanSweepFailureContext(out, gcLog))
			}
			if strings.Contains(log, "config show") {
				t.Fatalf("primary regression must not use config show fallback\n%s", orphanSweepFailureContext(out, gcLog))
			}
			if got := countExactLine(lines, "session list --json"); got < 2 {
				t.Fatalf("HQ session probe count = %d, want at least 2\n%s", got, orphanSweepFailureContext(out, gcLog))
			}
			// Keep before/after rig liveness probes covered on the protected-wisp path.
			if got := countExactLine(lines, "--rig project-alpha session list --json"); got < 2 {
				t.Fatalf("rig session probe count = %d, want at least 2\n%s", got, orphanSweepFailureContext(out, gcLog))
			}
			for _, want := range []string{
				"bd list --status=in_progress --json --limit=0",
				"rig list --json",
				"bd list --rig project-alpha --status=in_progress --json --limit=0",
				"config explain",
				"bd show " + tt.orphanID + " --json",
				orphanUpdate,
			} {
				if countExactLine(lines, want) == 0 {
					t.Fatalf("missing required gc call %q\n%s", want, orphanSweepFailureContext(out, gcLog))
				}
			}
		})
	}
}

func writeStrictOrphanSweepGCStub(t *testing.T, path string) {
	t.Helper()
	writeExecutable(t, path, `#!/bin/sh
set -eu
rig=""
if [ "${1:-}" = "--rig" ]; then
  rig="$2"
  shift 2
fi
if [ -n "$rig" ]; then
  printf '%s %s %s\n' "--rig" "$rig" "$*" >> "$GC_CALL_LOG"
else
  printf '%s\n' "$*" >> "$GC_CALL_LOG"
fi
if [ "$*" = "bd list --status=in_progress --json --limit=0" ]; then
  printf '%s\n' "$ORPHAN_SWEEP_HQ_JSON"
  exit 0
fi
if [ "$*" = "rig list --json" ]; then
  printf '{"rigs":[{"name":"hq","hq":true},{"name":"project-alpha","hq":false}]}\n'
  exit 0
fi
if [ "$*" = "bd list --rig project-alpha --status=in_progress --json --limit=0" ]; then
  printf '%s\n' "$ORPHAN_SWEEP_RIG_JSON"
  exit 0
fi
if [ "$*" = "config explain" ]; then
  printf 'Agent: %s\n  source: pack\n' "$ORPHAN_SWEEP_CONFIGURED_IDENTITY"
  exit 0
fi
if [ "$*" = "session list --json" ]; then
  if [ "$rig" = "project-alpha" ]; then
    printf '%s\n' "$ORPHAN_SWEEP_RIG_SESSIONS_JSON"
  else
    printf '%s\n' "$ORPHAN_SWEEP_HQ_SESSIONS_JSON"
  fi
  exit 0
fi
if [ "$*" = "bd release-if-current $ORPHAN_SWEEP_ORPHAN_ID $ORPHAN_SWEEP_ORPHAN_ASSIGNEE" ]; then
  printf 'released\n'
  exit 0
fi
if [ "$*" = "bd show $ORPHAN_SWEEP_ORPHAN_ID --json" ]; then
  printf '[{"id":"%s","status":"in_progress","assignee":"%s","metadata":{}}]\n' "$ORPHAN_SWEEP_ORPHAN_ID" "$ORPHAN_SWEEP_ORPHAN_ASSIGNEE"
  exit 0
fi
if [ "$*" = "bd show $ORPHAN_SWEEP_ORPHAN_ASSIGNEE --json" ]; then
  exit 1
fi
printf 'UNEXPECTED: %s\n' "$*" >> "$GC_CALL_LOG"
printf 'UNEXPECTED: %s\n' "$*" >&2
exit 2
`)
}

type orphanSweepCleanroomEnvConfig struct {
	hqJSON             string
	rigJSON            string
	hqSessionsJSON     string
	rigSessionsJSON    string
	configuredIdentity string
	orphanID           string
	orphanAssignee     string
}

func orphanSweepProtectedWispBeadsJSON(t *testing.T, protectedID, protectedAssignee, orphanID, orphanAssignee string) string {
	t.Helper()
	type bead struct {
		ID        string `json:"id"`
		Status    string `json:"status"`
		Assignee  string `json:"assignee"`
		Ephemeral bool   `json:"ephemeral"`
		IssueType string `json:"issue_type"`
	}
	data, err := json.Marshal([]bead{
		{
			ID:        protectedID,
			Status:    "in_progress",
			Assignee:  protectedAssignee,
			Ephemeral: true,
			IssueType: "molecule",
		},
		{
			ID:        orphanID,
			Status:    "in_progress",
			Assignee:  orphanAssignee,
			Ephemeral: true,
			IssueType: "molecule",
		},
	})
	if err != nil {
		t.Fatalf("Marshal(orphan-sweep beads): %v", err)
	}
	return string(data)
}

func orphanSweepSessionListJSON(t *testing.T, liveSessionNames ...string) string {
	t.Helper()
	type session struct {
		ID          string `json:"id"`
		SessionName string `json:"session_name"`
		Alias       string `json:"alias,omitempty"`
		AgentName   string `json:"agent_name,omitempty"`
		Closed      bool   `json:"closed"`
	}
	type response struct {
		SchemaVersion string         `json:"schema_version"`
		Filters       map[string]any `json:"filters"`
		Sessions      []session      `json:"sessions"`
		Summary       map[string]any `json:"summary"`
	}
	sessions := make([]session, 0, len(liveSessionNames)+1)
	for i, name := range liveSessionNames {
		sessions = append(sessions, session{
			ID:          fmt.Sprintf("mc-live-%d", i),
			SessionName: name,
			Closed:      false,
		})
	}
	sessions = append(sessions, session{
		ID:          "mc-closed",
		SessionName: "closed-session",
		Closed:      true,
	})
	data, err := json.Marshal(response{
		SchemaVersion: "1",
		Filters:       map[string]any{},
		Sessions:      sessions,
		Summary:       map[string]any{},
	})
	if err != nil {
		t.Fatalf("Marshal(orphan-sweep sessions): %v", err)
	}
	return string(data)
}

func orphanSweepCleanroomEnv(t *testing.T, root, binDir, gcLog string, cfg orphanSweepCleanroomEnvConfig) []string {
	t.Helper()
	rigList := `{"rigs":[{"name":"hq","hq":true},{"name":"project-alpha","hq":false}]}`
	dirs := map[string]string{
		"HOME":              filepath.Join(root, "home"),
		"XDG_CONFIG_HOME":   filepath.Join(root, "xdg-config"),
		"XDG_CACHE_HOME":    filepath.Join(root, "xdg-cache"),
		"XDG_STATE_HOME":    filepath.Join(root, "xdg-state"),
		"TMPDIR":            filepath.Join(root, "tmp"),
		"GC_CITY":           filepath.Join(root, "city"),
		"GC_CITY_PATH":      filepath.Join(root, "city"),
		"BEADS_DIR":         filepath.Join(root, "beads"),
		"GIT_CONFIG_GLOBAL": filepath.Join(root, "gitconfig"),
	}
	for key, path := range dirs {
		if key == "GIT_CONFIG_GLOBAL" {
			if err := os.WriteFile(path, nil, 0o644); err != nil {
				t.Fatalf("WriteFile(%s): %v", path, err)
			}
			continue
		}
		if err := os.MkdirAll(path, 0o755); err != nil {
			t.Fatalf("MkdirAll(%s): %v", path, err)
		}
	}
	return []string{
		"HOME=" + dirs["HOME"],
		"XDG_CONFIG_HOME=" + dirs["XDG_CONFIG_HOME"],
		"XDG_CACHE_HOME=" + dirs["XDG_CACHE_HOME"],
		"XDG_STATE_HOME=" + dirs["XDG_STATE_HOME"],
		"TMPDIR=" + dirs["TMPDIR"],
		"GC_CITY=" + dirs["GC_CITY"],
		"GC_CITY_PATH=" + dirs["GC_CITY_PATH"],
		"GC_CALL_LOG=" + gcLog,
		"BEADS_DIR=" + dirs["BEADS_DIR"],
		"GIT_CONFIG_GLOBAL=" + dirs["GIT_CONFIG_GLOBAL"],
		"GIT_CONFIG_NOSYSTEM=1",
		"ORPHAN_SWEEP_HQ_JSON=" + cfg.hqJSON,
		"ORPHAN_SWEEP_RIG_JSON=" + cfg.rigJSON,
		"ORPHAN_SWEEP_RIG_LIST_JSON=" + rigList,
		"ORPHAN_SWEEP_HQ_SESSIONS_JSON=" + cfg.hqSessionsJSON,
		"ORPHAN_SWEEP_RIG_SESSIONS_JSON=" + cfg.rigSessionsJSON,
		"ORPHAN_SWEEP_CONFIGURED_IDENTITY=" + cfg.configuredIdentity,
		"ORPHAN_SWEEP_ORPHAN_ID=" + cfg.orphanID,
		"ORPHAN_SWEEP_ORPHAN_ASSIGNEE=" + cfg.orphanAssignee,
		"PATH=" + binDir,
	}
}

func assertOrphanSweepFakeGC(t *testing.T, env []string, bashPath, fakeGC, gcLog string) {
	t.Helper()
	cmd := exec.Command(bashPath, "-c", "command -v gc")
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("command -v gc failed: %v\n%s", err, orphanSweepFailureContext(out, gcLog))
	}
	if got := strings.TrimSpace(string(out)); got != fakeGC {
		t.Fatalf("command -v gc = %q, want %q\n%s", got, fakeGC, orphanSweepFailureContext(out, gcLog))
	}
}

func orphanSweepFailureContext(output []byte, callLogPath string) string {
	logData, err := os.ReadFile(callLogPath)
	if err != nil {
		return fmt.Sprintf("captured output:\n%s\nrecent GC_CALL_LOG (%s): <read error: %v>", output, callLogPath, err)
	}
	return fmt.Sprintf("captured output:\n%s\nrecent GC_CALL_LOG (%s):\n%s", output, callLogPath, logData)
}

func nonEmptyLogLines(log string) []string {
	log = strings.TrimSpace(log)
	if log == "" {
		return nil
	}
	return strings.Split(log, "\n")
}

func countExactLine(lines []string, want string) int {
	count := 0
	for _, line := range lines {
		if line == want {
			count++
		}
	}
	return count
}

func TestMaintenanceDoltScriptsUseManagedRuntimePorts(t *testing.T) {
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
		name       string
		setup      func(t *testing.T, cityDir string) string
		wantExit78 bool
	}{
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
			name: "invalid managed state falls back to provider state",
			setup: func(t *testing.T, cityDir string) string {
				t.Helper()
				stateDir := filepath.Join(cityDir, ".gc", "runtime", "packs", "dolt")
				if err := os.MkdirAll(stateDir, 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(stateDir, "dolt-state.json"), []byte(`not-json`), 0o644); err != nil {
					t.Fatal(err)
				}
				listener := listenManagedDoltPort(t)
				port := listener.Addr().(*net.TCPAddr).Port
				writeProviderRuntimeState(t, cityDir, port)
				return strconv.Itoa(port)
			},
		},
		{
			name: "corrupt managed state exits 78 despite compatibility port mirror",
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
				return ""
			},
			wantExit78: true,
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

				script := filepath.Join(exampleDir(), tt.script)
				if fb.wantExit78 {
					out, err := runScriptResult(t, script, env)
					assertMaintenanceScriptExit78(t, err, out)
					return
				}
				runScript(t, script, env)

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
		wantExit78  bool
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
			wantExit78: true,
		},
		{
			name:     "inconclusive lsof with unreachable port still rejects port",
			lsofBody: "#!/bin/sh\nexit 0\n",
			ncBody: func(_ string) string {
				return `#!/bin/sh
exit 1
`
			},
			wantExit78: true,
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
				wantPort := managedPort
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

				script := filepath.Join(exampleDir(), tt.script)
				if tc.wantExit78 {
					out, err := runScriptResult(t, script, env)
					assertMaintenanceScriptExit78(t, err, out)
					return
				}
				runScript(t, script, env)

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

func assertMaintenanceScriptExit78(t *testing.T, err error, out []byte) {
	t.Helper()
	if err == nil {
		t.Fatalf("maintenance script exited 0, want exit 78\n%s", out)
	}
	exitErr := &exec.ExitError{}
	ok := errors.As(err, &exitErr)
	if !ok {
		t.Fatalf("maintenance script returned non-exit error: %v\n%s", err, out)
	}
	if exitErr.ExitCode() != 78 {
		t.Fatalf("maintenance script exit code = %d, want 78\n%s", exitErr.ExitCode(), out)
	}
	if !strings.Contains(string(out), "gc dolt: cannot resolve runtime port") {
		t.Fatalf("maintenance script output missing port-resolution error:\n%s", out)
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

func TestReaperMessageWispsAboveAlertThresholdDoNotTriggerReapFailureAnomaly(t *testing.T) {
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
  *"WITH RECURSIVE workflow_issue_root_candidates"*"SELECT DISTINCT root.id"*)
    printf 'id\n'
    ;;
  *"issue_type NOT IN"*)
    printf 'COUNT(*)\n0\n'
    ;;
  *"issue_type = 'message'"*)
    printf 'COUNT(*)\n600\n'
    ;;
  *"created_at < DATE_SUB"*)
    printf 'COUNT(*)\n0\n'
    ;;
  *"status IN ('open', 'hooked', 'in_progress')"*)
    printf 'COUNT(*)\n600\n'
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
		"DOLT_ARGS_LOG":             doltLog,
		"GC_CALL_LOG":               gcLog,
		"GC_CITY":                   cityDir,
		"GC_CITY_PATH":              cityDir,
		"GC_DOLT_HOST":              "127.0.0.1",
		"GC_DOLT_PORT":              "3307",
		"GC_DOLT_USER":              "root",
		"GC_DOLT_PASSWORD":          "",
		"GC_REAPER_ALERT_THRESHOLD": "500",
		"PATH":                      binDir + string(os.PathListSeparator) + os.Getenv("PATH"),
	}

	runScript(t, filepath.Join(exampleDir(), "packs", "maintenance", "assets", "scripts", "reaper.sh"), env)

	gcData, err := os.ReadFile(gcLog)
	if err != nil {
		t.Fatalf("ReadFile(gc log): %v", err)
	}
	gcLogText := string(gcData)
	if strings.Contains(gcLogText, "ESCALATION") {
		t.Fatalf("reaper fired false-positive escalation for message-type wisps above alert threshold:\n%s", gcLogText)
	}
	if !strings.Contains(gcLogText, "mail_wisps:600") {
		t.Fatalf("reaper summary missing mail_wisps:600 for message-type wisp backlog:\n%s", gcLogText)
	}
}

func TestReaperFreshNonMessageWispsAboveAlertThresholdDoNotTriggerReapFailureAnomaly(t *testing.T) {
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
  *"WITH RECURSIVE workflow_issue_root_candidates"*"SELECT DISTINCT root.id"*)
    printf 'id\n'
    ;;
  *"issue_type NOT IN"*"created_at < DATE_SUB"*)
    printf 'COUNT(*)\n0\n'
    ;;
  *"issue_type NOT IN"*)
    printf 'COUNT(*)\n600\n'
    ;;
  *"issue_type = 'message'"*)
    printf 'COUNT(*)\n0\n'
    ;;
  *"status = 'closed'"*|*"closed_at <"*)
    printf 'COUNT(*)\n0\n'
    ;;
  *"created_at < DATE_SUB"*)
    printf 'COUNT(*)\n0\n'
    ;;
  *"status IN ('open', 'hooked', 'in_progress')"*)
    printf 'COUNT(*)\n600\n'
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
		"DOLT_ARGS_LOG":             doltLog,
		"GC_CALL_LOG":               gcLog,
		"GC_CITY":                   cityDir,
		"GC_CITY_PATH":              cityDir,
		"GC_DOLT_HOST":              "127.0.0.1",
		"GC_DOLT_PORT":              "3307",
		"GC_DOLT_USER":              "root",
		"GC_DOLT_PASSWORD":          "",
		"GC_REAPER_ALERT_THRESHOLD": "500",
		"PATH":                      binDir + string(os.PathListSeparator) + os.Getenv("PATH"),
	}

	runScript(t, filepath.Join(exampleDir(), "packs", "maintenance", "assets", "scripts", "reaper.sh"), env)

	gcData, err := os.ReadFile(gcLog)
	if err != nil {
		t.Fatalf("ReadFile(gc log): %v", err)
	}
	gcLogText := string(gcData)
	if strings.Contains(gcLogText, "ESCALATION") {
		t.Fatalf("reaper fired false-positive escalation for fresh non-message wisps above alert threshold:\n%s", gcLogText)
	}
}

func TestReaperStaleNonMessageWispsAboveAlertThresholdStillTriggerReapFailureAnomaly(t *testing.T) {
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
  *"issue_type NOT IN"*"created_at < DATE_SUB"*)
    printf 'COUNT(*)\n600\n'
    ;;
  *"issue_type NOT IN"*)
    printf 'COUNT(*)\n600\n'
    ;;
  *"issue_type = 'message'"*)
    printf 'COUNT(*)\n0\n'
    ;;
  *"status = 'closed'"*|*"closed_at <"*)
    printf 'COUNT(*)\n0\n'
    ;;
  *"created_at < DATE_SUB"*)
    printf 'COUNT(*)\n0\n'
    ;;
  *"status IN ('open', 'hooked', 'in_progress')"*)
    printf 'COUNT(*)\n600\n'
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
		"DOLT_ARGS_LOG":             doltLog,
		"GC_CALL_LOG":               gcLog,
		"GC_CITY":                   cityDir,
		"GC_CITY_PATH":              cityDir,
		"GC_DOLT_HOST":              "127.0.0.1",
		"GC_DOLT_PORT":              "3307",
		"GC_DOLT_USER":              "root",
		"GC_DOLT_PASSWORD":          "",
		"GC_REAPER_ALERT_THRESHOLD": "500",
		"PATH":                      binDir + string(os.PathListSeparator) + os.Getenv("PATH"),
	}

	runScript(t, filepath.Join(exampleDir(), "packs", "maintenance", "assets", "scripts", "reaper.sh"), env)

	gcData, err := os.ReadFile(gcLog)
	if err != nil {
		t.Fatalf("ReadFile(gc log): %v", err)
	}
	gcLogText := string(gcData)
	if !strings.Contains(gcLogText, "ESCALATION: Reaper anomalies detected [MEDIUM]") {
		t.Fatalf("reaper did not fire reap-failure anomaly for non-message wisps above threshold:\n%s", gcLogText)
	}
	if !strings.Contains(gcLogText, "stale open wisps (threshold: 500, age: 24h)") {
		t.Fatalf("reaper anomaly body missing stale open wisp count format:\n%s", gcLogText)
	}
}

func TestReaperMailAlertThresholdPositiveBranchEmitsMailBacklogAnomaly(t *testing.T) {
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
  *"issue_type NOT IN"*)
    printf 'COUNT(*)\n0\n'
    ;;
  *"issue_type = 'message'"*)
    printf 'COUNT(*)\n300\n'
    ;;
  *"created_at < DATE_SUB"*)
    printf 'COUNT(*)\n0\n'
    ;;
  *"status IN ('open', 'hooked', 'in_progress')"*)
    printf 'COUNT(*)\n300\n'
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
		"DOLT_ARGS_LOG":                  doltLog,
		"GC_CALL_LOG":                    gcLog,
		"GC_CITY":                        cityDir,
		"GC_CITY_PATH":                   cityDir,
		"GC_DOLT_HOST":                   "127.0.0.1",
		"GC_DOLT_PORT":                   "3307",
		"GC_DOLT_USER":                   "root",
		"GC_DOLT_PASSWORD":               "",
		"GC_REAPER_ALERT_THRESHOLD":      "500",
		"GC_REAPER_MAIL_ALERT_THRESHOLD": "200",
		"PATH":                           binDir + string(os.PathListSeparator) + os.Getenv("PATH"),
	}

	runScript(t, filepath.Join(exampleDir(), "packs", "maintenance", "assets", "scripts", "reaper.sh"), env)

	gcData, err := os.ReadFile(gcLog)
	if err != nil {
		t.Fatalf("ReadFile(gc log): %v", err)
	}
	gcLogText := string(gcData)
	if !strings.Contains(gcLogText, "ESCALATION: Reaper anomalies detected [MEDIUM]") {
		t.Fatalf("reaper did not fire mail backlog anomaly above mail threshold:\n%s", gcLogText)
	}
	if !strings.Contains(gcLogText, "open mail-wisps (mail threshold: 200)") {
		t.Fatalf("reaper anomaly body missing mail backlog threshold format:\n%s", gcLogText)
	}
	if strings.Contains(gcLogText, "stale open wisps (threshold: 500") {
		t.Fatalf("reaper fired reapable-wisp anomaly for message-only backlog:\n%s", gcLogText)
	}
}

func TestReaperMailWispsSummaryFieldAlwaysPresent(t *testing.T) {
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
		"DOLT_DBS":         "beads",
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
	if !strings.Contains(gcLogText, "mail_wisps:0") {
		t.Fatalf("reaper summary missing mail_wisps field when wisp counts are zero:\n%s", gcLogText)
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
		"beads_test_bench_1780469138694213039",
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
	formula := string(data)

	for _, stalePhrase := range []string{
		"Total open wisps (for alert threshold)",
		"If total open wisps",
		"Open wisp count exceeding",
	} {
		if strings.Contains(formula, stalePhrase) {
			t.Errorf("formula still describes total-open-wisp alerting with %q; reaper alerts on stale non-message open wisps", stalePhrase)
		}
	}
	for _, required := range []string{
		"Stale non-message open wisps (for alert threshold)",
		"issue_type NOT IN ('message')",
		"created_at < DATE_SUB(NOW(), INTERVAL <max_age_hours> HOUR)",
	} {
		if !strings.Contains(formula, required) {
			t.Errorf("formula is missing stale-only alert text/query fragment %q", required)
		}
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
		if strings.Contains(fence, "depends_on_id") && !strings.Contains(fence, "depends_on_issue_id") && !strings.Contains(fence, "depends_on_wisp_id") {
			t.Errorf("formula sql fence %d references removed depends_on_id column; schema uses typed split columns:\n%s", i, fence)
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
	if !containsReaperCloseCleanupEdgePredicate(formula) {
		t.Fatalf("formula does not document the reaper close ownership predicate:\n%s", formula)
	}
	if !containsReaperPurgeProtectEdgePredicate(formula) {
		t.Fatalf("formula does not document the reaper purge-protection predicate:\n%s", formula)
	}
}

func TestReaperParentIDIsParentChildDependencyProjection(t *testing.T) {
	runner := func(_, name string, args ...string) ([]byte, error) {
		call := name + " " + strings.Join(args, " ")
		switch call {
		case "bd list --json --label=parent-projection --include-infra --include-gates --limit 0":
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
	if !strings.Contains(script, "wisp_dependencies d") || !containsReaperCloseCleanupEdgePredicate(script) {
		t.Fatalf("reaper does not follow the canonical Dolt cleanup-edge projection:\n%s", script)
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
	for _, want := range []string{"depends_on_wisp_id", "depends_on_issue_id"} {
		if !strings.Contains(log, want) {
			t.Errorf("reaper SQL missing split dependency target column %q; schema uses typed columns:\n%s", want, log)
		}
	}
	// mail was removed: not a SQL table; messages are beads with type=message.
	if strings.Contains(log, ".mail") {
		t.Errorf("reaper SQL references .mail table (does not exist in beads schema):\n%s", log)
	}
	for _, want := range []string{
		"SHOW COLUMNS FROM `beads`.dependencies",
		"SHOW COLUMNS FROM `beads`.wisp_dependencies",
		"FROM `beads`.wisp_dependencies d",
		"SELECT DISTINCT d.depends_on_wisp_id",
	} {
		if !strings.Contains(log, want) {
			t.Errorf("reaper SQL missing %q:\n%s", want, log)
		}
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
			!containsReaperPurgeProtectEdgePredicate(purgeSQL) ||
			!strings.Contains(purgeSQL, "SELECT DISTINCT d.depends_on_wisp_id") {
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

func TestReaperSkipsDependencyQueriesWithoutGenericDependencyTargets(t *testing.T) {
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
		"DOLT_ARGS_LOG":          doltLog,
		"DOLT_DBS":               "beads",
		"DOLT_DEPENDENCY_SCHEMA": "missing-target",
		"GC_CALL_LOG":            gcLog,
		"GC_CITY":                cityDir,
		"GC_CITY_PATH":           cityDir,
		"GC_DOLT_HOST":           "127.0.0.1",
		"GC_DOLT_PORT":           "3307",
		"GC_DOLT_USER":           "root",
		"GC_DOLT_PASSWORD":       "",
		"PATH":                   binDir + string(os.PathListSeparator) + os.Getenv("PATH"),
	}

	runScript(t, filepath.Join(exampleDir(), "packs", "maintenance", "assets", "scripts", "reaper.sh"), env)

	logData, err := os.ReadFile(doltLog)
	if err != nil {
		t.Fatalf("ReadFile(dolt log): %v", err)
	}
	log := string(logData)
	if !strings.Contains(log, "SHOW COLUMNS FROM `beads`.dependencies") {
		t.Fatalf("reaper did not probe dependency target columns:\n%s", log)
	}
	if strings.Contains(log, "FROM `beads`.wisp_dependencies d") || strings.Contains(log, "JOIN `beads`.wisp_dependencies d") {
		t.Fatalf("reaper ran dependency-aware queries against schema without typed dependency target columns:\n%s", log)
	}

	// A silently-skipped DB may make no gc calls at all, so a missing
	// gc log is a valid no-escalation outcome.
	gcData, err := os.ReadFile(gcLog)
	if err != nil && !os.IsNotExist(err) {
		t.Fatalf("ReadFile(gc log): %v", err)
	}
	if strings.Contains(string(gcData), "dependencies table lacks") {
		t.Errorf("reaper escalated the dependency schema as an anomaly; the target-column gate must skip silently:\n%s", gcData)
	}
}

func TestReaperSkipsDependencyQueriesWithoutWispDependencyTable(t *testing.T) {
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
		"DOLT_ARGS_LOG":               doltLog,
		"DOLT_DBS":                    "beads",
		"DOLT_WISP_DEPENDENCY_SCHEMA": "missing-table",
		"GC_CALL_LOG":                 gcLog,
		"GC_CITY":                     cityDir,
		"GC_CITY_PATH":                cityDir,
		"GC_DOLT_HOST":                "127.0.0.1",
		"GC_DOLT_PORT":                "3307",
		"GC_DOLT_USER":                "root",
		"GC_DOLT_PASSWORD":            "",
		"PATH":                        binDir + string(os.PathListSeparator) + os.Getenv("PATH"),
	}

	runScript(t, filepath.Join(exampleDir(), "packs", "maintenance", "assets", "scripts", "reaper.sh"), env)

	logData, err := os.ReadFile(doltLog)
	if err != nil {
		t.Fatalf("ReadFile(dolt log): %v", err)
	}
	log := string(logData)
	if !strings.Contains(log, "SHOW COLUMNS FROM `beads`.wisp_dependencies") {
		t.Fatalf("reaper did not probe wisp dependency target columns:\n%s", log)
	}
	if strings.Contains(log, "FROM `beads`.wisp_dependencies d") || strings.Contains(log, "JOIN `beads`.wisp_dependencies d") {
		t.Fatalf("reaper ran dependency-aware queries without a wisp_dependencies table:\n%s", log)
	}

	gcData, err := os.ReadFile(gcLog)
	if err != nil && !os.IsNotExist(err) {
		t.Fatalf("ReadFile(gc log): %v", err)
	}
	if strings.Contains(string(gcData), "wisp_dependencies") {
		t.Errorf("reaper escalated the missing wisp dependency table as an anomaly; the schema gate must skip silently:\n%s", gcData)
	}
}

func TestReaperSplitSchemaQueriesUseSplitColumns(t *testing.T) {
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
		"DOLT_ARGS_LOG":               doltLog,
		"GC_CALL_LOG":                 gcLog,
		"DOLT_DBS":                    "beads",
		"DOLT_DEPENDENCY_SCHEMA":      "split",
		"DOLT_WISP_DEPENDENCY_SCHEMA": "split",
		"GC_CITY":                     cityDir,
		"GC_CITY_PATH":                cityDir,
		"GC_DOLT_HOST":                "127.0.0.1",
		"GC_DOLT_PORT":                "3307",
		"GC_DOLT_USER":                "root",
		"GC_DOLT_PASSWORD":            "",
		"DOLT_PURGE_COUNT":            "1",
		"PATH":                        binDir + string(os.PathListSeparator) + os.Getenv("PATH"),
	}

	runScript(t, filepath.Join(exampleDir(), "packs", "maintenance", "assets", "scripts", "reaper.sh"), env)

	logData, err := os.ReadFile(doltLog)
	if err != nil {
		t.Fatalf("ReadFile(dolt log): %v", err)
	}
	log := string(logData)

	for _, want := range []string{
		"SHOW COLUMNS FROM `beads`.dependencies",
		"SHOW COLUMNS FROM `beads`.wisp_dependencies",
		"FROM `beads`.wisp_dependencies d",
	} {
		if !strings.Contains(log, want) {
			t.Errorf("reaper split-schema log missing %q:\n%s", want, log)
		}
	}

	for _, splitCol := range []string{"depends_on_issue_id", "depends_on_wisp_id"} {
		if !strings.Contains(log, splitCol) {
			t.Errorf("reaper split-schema log missing split column %q:\n%s", splitCol, log)
		}
	}

	// With split schema, queries against dependencies must not use the removed depends_on_id column.
	// Filter out SHOW COLUMNS lines (which contain the table name, not the column reference in queries).
	var queryLines []string
	for _, line := range strings.Split(log, "\n") {
		if !strings.Contains(line, "SHOW COLUMNS") {
			queryLines = append(queryLines, line)
		}
	}
	queryLog := strings.Join(queryLines, "\n")
	if strings.Contains(queryLog, "d.depends_on_id") {
		t.Errorf("reaper split-schema queries reference removed column d.depends_on_id:\n%s", queryLog)
	}
}

func TestReaperPrunesClosedSessionBeadsWithBdPrune(t *testing.T) {
	cityDir := t.TempDir()
	if resolved, err := filepath.EvalSymlinks(cityDir); err == nil {
		cityDir = resolved
	}
	writeCityBeadsMetadata(t, cityDir, "beads")
	canonicalCityDir, err := filepath.EvalSymlinks(cityDir)
	if err != nil {
		t.Fatalf("EvalSymlinks(city dir): %v", err)
	}
	binDir := t.TempDir()
	doltLog := filepath.Join(t.TempDir(), "dolt-args.log")
	bdLog := filepath.Join(t.TempDir(), "bd.log")
	gcLog := filepath.Join(t.TempDir(), "gc.log")

	writeMaintenanceDoltStub(t, filepath.Join(binDir, "dolt"))
	writeMaintenanceBdStub(t, filepath.Join(binDir, "bd"))
	writeExecutable(t, filepath.Join(binDir, "gc"), `#!/bin/sh
printf '%s\n' "$*" >> "$GC_CALL_LOG"
exit 0
`)

	env := map[string]string{
		"BD_CALL_LOG":      bdLog,
		"BD_PRUNE_COUNT":   "7",
		"DOLT_ARGS_LOG":    doltLog,
		"DOLT_DBS":         "beads",
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
	wantArgs := "args=prune --pattern gm-* --older-than 720h --force --json"
	if !strings.Contains(bdLogText, wantArgs) {
		t.Fatalf("reaper did not call bd prune with the gm session-bead retention args %q:\n%s", wantArgs, bdLogText)
	}
	if got := strings.Count(bdLogText, "args=prune "); got != 1 {
		t.Fatalf("reaper called bd prune %d times, want once:\n%s", got, bdLogText)
	}
	if !strings.Contains(bdLogText, "pwd="+canonicalCityDir) {
		t.Fatalf("reaper did not run bd prune from the city dir:\n%s", bdLogText)
	}
	if !strings.Contains(bdLogText, "beads="+filepath.Join(canonicalCityDir, ".beads")) {
		t.Fatalf("reaper did not scope bd prune to the city beads dir:\n%s", bdLogText)
	}

	gcData, err := os.ReadFile(gcLog)
	if err != nil {
		t.Fatalf("ReadFile(gc log): %v", err)
	}
	if !strings.Contains(string(gcData), "sessions-pruned:7") {
		t.Fatalf("reaper summary did not report pruned sessions:\n%s", gcData)
	}
}

func TestReaperPrunesTerminalSessionStatesWithGcSessionPrune(t *testing.T) {
	cityDir := t.TempDir()
	if resolved, err := filepath.EvalSymlinks(cityDir); err == nil {
		cityDir = resolved
	}
	writeCityBeadsMetadata(t, cityDir, "beads")
	binDir := t.TempDir()
	doltLog := filepath.Join(t.TempDir(), "dolt-args.log")
	bdLog := filepath.Join(t.TempDir(), "bd.log")
	gcLog := filepath.Join(t.TempDir(), "gc.log")

	writeMaintenanceDoltStub(t, filepath.Join(binDir, "dolt"))
	writeMaintenanceBdStub(t, filepath.Join(binDir, "bd"))
	writeExecutable(t, filepath.Join(binDir, "gc"), `#!/bin/sh
printf '%s\n' "$*" >> "$GC_CALL_LOG"
case "$*" in
  "session prune --state drained --before 24h --json")
    printf '{"action":"prune","count":5}\n'
    ;;
esac
exit 0
`)

	env := map[string]string{
		"BD_CALL_LOG":      bdLog,
		"BD_PRUNE_COUNT":   "7",
		"DOLT_ARGS_LOG":    doltLog,
		"DOLT_DBS":         "beads",
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
	if !strings.Contains(gcLogText, "session prune --state drained --before 24h --json") {
		t.Fatalf("reaper did not call gc session prune for terminal drained sessions:\n%s", gcLogText)
	}
	if !strings.Contains(gcLogText, "sessions-pruned:12") {
		t.Fatalf("reaper summary did not include bd and gc session prune counts:\n%s", gcLogText)
	}
}

func TestReaperSessionStatePruneFailureEscalates(t *testing.T) {
	cityDir := t.TempDir()
	writeCityBeadsMetadata(t, cityDir, "beads")
	binDir := t.TempDir()
	doltLog := filepath.Join(t.TempDir(), "dolt-args.log")
	bdLog := filepath.Join(t.TempDir(), "bd.log")
	gcLog := filepath.Join(t.TempDir(), "gc.log")

	writeMaintenanceDoltStub(t, filepath.Join(binDir, "dolt"))
	writeMaintenanceBdStub(t, filepath.Join(binDir, "bd"))
	writeExecutable(t, filepath.Join(binDir, "gc"), `#!/bin/sh
printf '%s\n' "$*" >> "$GC_CALL_LOG"
case "$*" in
  "session prune --state drained --before 24h --json")
    printf 'session prune exploded\n' >&2
    exit 42
    ;;
esac
exit 0
`)

	env := map[string]string{
		"BD_CALL_LOG":      bdLog,
		"BD_PRUNE_COUNT":   "0",
		"DOLT_ARGS_LOG":    doltLog,
		"DOLT_DBS":         "beads",
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
	if !strings.Contains(gcLogText, "mail send mayor/ -s ESCALATION: Reaper anomalies detected [MEDIUM]") {
		t.Fatalf("reaper did not send escalation mail for session-state prune failure:\n%s", gcLogText)
	}
	if !strings.Contains(gcLogText, "gm: terminal session-state prune failed: session prune exploded") {
		t.Fatalf("reaper escalation did not include session-state prune failure details:\n%s", gcLogText)
	}
	if !strings.Contains(gcLogText, "sessions-pruned:0") {
		t.Fatalf("reaper summary counted failed session-state prune as success:\n%s", gcLogText)
	}
}

func TestReaperSessionPruneDryRunOmitsForce(t *testing.T) {
	cityDir := t.TempDir()
	writeCityBeadsMetadata(t, cityDir, "beads")
	binDir := t.TempDir()
	doltLog := filepath.Join(t.TempDir(), "dolt-args.log")
	bdLog := filepath.Join(t.TempDir(), "bd.log")
	gcLog := filepath.Join(t.TempDir(), "gc.log")

	writeMaintenanceDoltStub(t, filepath.Join(binDir, "dolt"))
	writeMaintenanceBdStub(t, filepath.Join(binDir, "bd"))
	writeExecutable(t, filepath.Join(binDir, "gc"), `#!/bin/sh
printf '%s\n' "$*" >> "$GC_CALL_LOG"
exit 0
`)

	env := map[string]string{
		"BD_CALL_LOG":                 bdLog,
		"BD_PRUNE_COUNT":              "3",
		"DOLT_ARGS_LOG":               doltLog,
		"DOLT_DBS":                    "beads",
		"GC_CALL_LOG":                 gcLog,
		"GC_CITY":                     cityDir,
		"GC_CITY_PATH":                cityDir,
		"GC_DOLT_HOST":                "127.0.0.1",
		"GC_DOLT_PORT":                "3307",
		"GC_DOLT_USER":                "root",
		"GC_DOLT_PASSWORD":            "",
		"GC_REAPER_DRY_RUN":           "1",
		"GC_REAPER_SESSION_PURGE_AGE": "24h",
		"PATH":                        binDir + string(os.PathListSeparator) + os.Getenv("PATH"),
	}

	runScript(t, filepath.Join(exampleDir(), "packs", "maintenance", "assets", "scripts", "reaper.sh"), env)

	bdData, err := os.ReadFile(bdLog)
	if err != nil {
		t.Fatalf("ReadFile(bd log): %v", err)
	}
	bdLogText := string(bdData)
	wantArgs := "args=prune --pattern gm-* --older-than 24h --json"
	if !strings.Contains(bdLogText, wantArgs) {
		t.Fatalf("reaper dry-run did not call bd prune with preview args %q:\n%s", wantArgs, bdLogText)
	}
	if strings.Contains(bdLogText, "--force") {
		t.Fatalf("reaper dry-run passed --force to bd prune:\n%s", bdLogText)
	}

	gcData, err := os.ReadFile(gcLog)
	if err != nil {
		t.Fatalf("ReadFile(gc log): %v", err)
	}
	gcLogText := string(gcData)
	if !strings.Contains(gcLogText, "sessions-pruned:3") || !strings.Contains(gcLogText, "(dry run)") {
		t.Fatalf("reaper dry-run summary did not report session prune preview count:\n%s", gcLogText)
	}
	if strings.Contains(gcLogText, "session prune ") {
		t.Fatalf("reaper dry-run called mutating gc session prune:\n%s", gcLogText)
	}
}

func TestReaperSessionPruneAnomalyEscalates(t *testing.T) {
	cityDir := t.TempDir()
	writeCityBeadsMetadata(t, cityDir, "beads")
	binDir := t.TempDir()
	doltLog := filepath.Join(t.TempDir(), "dolt-args.log")
	bdLog := filepath.Join(t.TempDir(), "bd.log")
	gcLog := filepath.Join(t.TempDir(), "gc.log")

	writeMaintenanceDoltStub(t, filepath.Join(binDir, "dolt"))
	writeMaintenanceBdStub(t, filepath.Join(binDir, "bd"))
	writeExecutable(t, filepath.Join(binDir, "gc"), `#!/bin/sh
printf '%s\n' "$*" >> "$GC_CALL_LOG"
exit 0
`)

	env := map[string]string{
		"BD_CALL_LOG":      bdLog,
		"BD_PRUNE_COUNT":   "1500",
		"DOLT_ARGS_LOG":    doltLog,
		"DOLT_DBS":         "beads",
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
	if !strings.Contains(gcLogText, "mail send mayor/ -s ESCALATION: Reaper anomalies detected [MEDIUM]") {
		t.Fatalf("reaper did not send escalation mail for session-prune anomaly:\n%s", gcLogText)
	}
	if !strings.Contains(gcLogText, "gm: 1500 closed session beads pruned in one run (threshold: 1000)") {
		t.Fatalf("reaper escalation did not include session-prune anomaly:\n%s", gcLogText)
	}
	if !strings.Contains(gcLogText, "sessions-pruned:1500") {
		t.Fatalf("reaper summary did not include anomalous session-prune count:\n%s", gcLogText)
	}
}

func TestReaperSessionPruneMissingBdDegradesToZero(t *testing.T) {
	cityDir := t.TempDir()
	writeCityBeadsMetadata(t, cityDir, "beads")
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
		"DOLT_DBS":         "beads",
		"GC_CALL_LOG":      gcLog,
		"GC_CITY":          cityDir,
		"GC_CITY_PATH":     cityDir,
		"GC_DOLT_HOST":     "127.0.0.1",
		"GC_DOLT_PORT":     "3307",
		"GC_DOLT_USER":     "root",
		"GC_DOLT_PASSWORD": "",
		"PATH":             binDir + string(os.PathListSeparator) + "/usr/bin:/bin",
	}

	runScript(t, filepath.Join(exampleDir(), "packs", "maintenance", "assets", "scripts", "reaper.sh"), env)

	gcData, err := os.ReadFile(gcLog)
	if err != nil {
		t.Fatalf("ReadFile(gc log): %v", err)
	}
	gcLogText := string(gcData)
	if strings.Contains(gcLogText, "ESCALATION") {
		t.Fatalf("reaper escalated missing bd binary instead of degrading:\n%s", gcLogText)
	}
	if !strings.Contains(gcLogText, "sessions-pruned:0") {
		t.Fatalf("reaper summary did not report zero pruned sessions without bd:\n%s", gcLogText)
	}
}

func TestReaperSessionPruneRunsWhenNoDoltDatabases(t *testing.T) {
	cityDir := t.TempDir()
	writeCityBeadsMetadata(t, cityDir, "beads")
	binDir := t.TempDir()
	doltLog := filepath.Join(t.TempDir(), "dolt-args.log")
	bdLog := filepath.Join(t.TempDir(), "bd.log")
	gcLog := filepath.Join(t.TempDir(), "gc.log")

	writeExecutable(t, filepath.Join(binDir, "dolt"), `#!/bin/sh
printf '%s\n' "$*" >> "$DOLT_ARGS_LOG"
case "$*" in
  *"SHOW DATABASES"*)
    printf 'Database\n'
    ;;
esac
exit 0
`)
	writeMaintenanceBdStub(t, filepath.Join(binDir, "bd"))
	writeExecutable(t, filepath.Join(binDir, "gc"), `#!/bin/sh
printf '%s\n' "$*" >> "$GC_CALL_LOG"
exit 0
`)

	env := map[string]string{
		"BD_CALL_LOG":       bdLog,
		"BD_PRUNE_COUNT":    "0",
		"DOLT_ARGS_LOG":     doltLog,
		"GC_CALL_LOG":       gcLog,
		"GC_CITY":           cityDir,
		"GC_CITY_PATH":      cityDir,
		"GC_DOLT_HOST":      "127.0.0.1",
		"GC_DOLT_PORT":      "3307",
		"GC_DOLT_USER":      "root",
		"GC_DOLT_PASSWORD":  "",
		"GC_REAPER_DRY_RUN": "1",
		"PATH":              binDir + string(os.PathListSeparator) + os.Getenv("PATH"),
	}

	runScript(t, filepath.Join(exampleDir(), "packs", "maintenance", "assets", "scripts", "reaper.sh"), env)

	bdData, err := os.ReadFile(bdLog)
	if err != nil {
		t.Fatalf("ReadFile(bd log): %v", err)
	}
	if !strings.Contains(string(bdData), "args=prune --pattern gm-* --older-than 720h --json") {
		t.Fatalf("reaper did not run session prune when Dolt had no databases:\n%s", bdData)
	}

	gcData, err := os.ReadFile(gcLog)
	if err != nil {
		t.Fatalf("ReadFile(gc log): %v", err)
	}
	if !strings.Contains(string(gcData), "sessions-pruned:0") {
		t.Fatalf("reaper summary did not report zero session prune count without Dolt databases:\n%s", gcData)
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

func TestReaperDryRunReportsWouldCloseStaleWisps(t *testing.T) {
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
  *"COUNT(DISTINCT w.id)"*)
    printf 'COUNT(*)\n2\n'
    ;;
  *"UPDATE "*"wisps SET status='closed'"*)
    printf 'dry-run should not update wisps\n' >&2
    exit 42
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
		"DOLT_ARGS_LOG":     doltLog,
		"GC_CALL_LOG":       gcLog,
		"GC_CITY":           cityDir,
		"GC_CITY_PATH":      cityDir,
		"GC_DOLT_HOST":      "127.0.0.1",
		"GC_DOLT_PORT":      "3307",
		"GC_DOLT_USER":      "root",
		"GC_DOLT_PASSWORD":  "",
		"GC_REAPER_DRY_RUN": "1",
		"PATH":              binDir + string(os.PathListSeparator) + os.Getenv("PATH"),
	}

	runScript(t, filepath.Join(exampleDir(), "packs", "maintenance", "assets", "scripts", "reaper.sh"), env)

	logData, err := os.ReadFile(doltLog)
	if err != nil {
		t.Fatalf("ReadFile(dolt log): %v", err)
	}
	if strings.Contains(string(logData), "UPDATE `beads`.wisps SET status='closed'") {
		t.Fatalf("dry-run executed stale-wisp update:\n%s", logData)
	}

	gcData, err := os.ReadFile(gcLog)
	if err != nil {
		t.Fatalf("ReadFile(gc log): %v", err)
	}
	gcText := string(gcData)
	if !strings.Contains(gcText, "closed_wisps:0") ||
		!strings.Contains(gcText, "would_close_wisps:2") ||
		!strings.Contains(gcText, "(dry run)") {
		t.Fatalf("dry-run summary did not report non-mutating would-close count:\n%s", gcText)
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
  *"STR_TO_DATE(JSON_UNQUOTE(JSON_EXTRACT(metadata, '$.expires_at'))"*)
    printf 'id\n'
    printf 'non-fatal warning from dolt\n' >&2
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
  *"COUNT("*"wisps w"*"wisp_dependencies d"*)
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
	if strings.Contains(log, "UPDATE `beads`.wisps SET status='closed'") && !strings.Contains(log, "wisp_dependencies d") {
		t.Fatalf("reaper closed non-closed wisps by age alone instead of using cleanup-edge dependencies:\n%s", log)
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
  *"COUNT("*"wisps w"*"wisp_dependencies d"*)
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
	if !strings.Contains(log, "wisp_dependencies d") || !containsReaperCloseCleanupEdgePredicate(log) {
		t.Fatalf("reaper stale-wisp close path does not use graph cleanup-edge dependencies:\n%s", log)
	}
	if !strings.Contains(log, "d.depends_on_wisp_id = parent_wisp.id") || !strings.Contains(log, "d.depends_on_issue_id = parent_issue.id") {
		t.Fatalf("reaper stale-wisp close path does not use typed dependency target columns:\n%s", log)
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

func TestReaperClosesGraphWorkflowWispTrackedToClosedRoot(t *testing.T) {
	doltLog, gcLog := runReaperCloseFixture(t, "tracks_owned_root")

	logData, err := os.ReadFile(doltLog)
	if err != nil {
		t.Fatalf("ReadFile(dolt log): %v", err)
	}
	log := string(logData)
	if !containsReaperCloseCleanupEdgePredicate(log) {
		t.Fatalf("reaper close path does not require graph-v2 tracks ownership:\n%s", log)
	}
	if !strings.Contains(log, "UPDATE `beads`.wisps SET status='closed'") {
		t.Fatalf("reaper did not close stale graph workflow wisp tracked to a closed root:\n%s", log)
	}

	gcData, err := os.ReadFile(gcLog)
	if err != nil {
		t.Fatalf("ReadFile(gc log): %v", err)
	}
	if !strings.Contains(string(gcData), "stale_wisps:1") || !strings.Contains(string(gcData), "closed_wisps:1") {
		t.Fatalf("reaper summary did not report tracked-root wisp close:\n%s", gcData)
	}
}

func TestReaperDoesNotCloseStaleWispWithClosedBlocksPredecessor(t *testing.T) {
	doltLog, gcLog := runReaperCloseFixture(t, "blocks_closed_predecessor")

	logData, err := os.ReadFile(doltLog)
	if err != nil {
		t.Fatalf("ReadFile(dolt log): %v", err)
	}
	log := string(logData)
	if strings.Contains(log, "reaper_wisp_candidates") {
		t.Fatalf("reaper closed a stale wisp through an ordinary closed blocks predecessor:\n%s", log)
	}

	gcData, err := os.ReadFile(gcLog)
	if err != nil {
		t.Fatalf("ReadFile(gc log): %v", err)
	}
	if !strings.Contains(string(gcData), "stale_wisps:1") || !strings.Contains(string(gcData), "closed_wisps:0") {
		t.Fatalf("reaper summary did not keep closed blocks predecessor as non-closing:\n%s", gcData)
	}
}

func TestReaperClosesStaleInactiveWorkflowRoots(t *testing.T) {
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
    printf 'Database\nbeads\n'
    ;;
  *"SHOW COLUMNS FROM"*"dependencies"*)
    printf 'Field,Type,Null,Key,Default,Extra\n'
    printf 'issue_id,varchar,NO,,,\n'
    printf 'depends_on_issue_id,varchar,YES,,,\n'
    printf 'depends_on_wisp_id,varchar,YES,,,\n'
    printf 'depends_on_external,varchar,YES,,,\n'
    printf 'type,varchar,NO,,,\n'
    ;;
  *"WITH RECURSIVE workflow_wisp_root_candidates"*"UPDATE "*"wisps SET status='closed'"*"JSON_SET(COALESCE(metadata, JSON_OBJECT())"*)
    printf 'ROW_COUNT()\n1\n'
    ;;
  *"WITH RECURSIVE workflow_wisp_root_candidates"*"SELECT COUNT(*) FROM ("*)
    printf 'COUNT(*)\n1\n'
    ;;
  *"WITH RECURSIVE workflow_issue_root_candidates"*"SELECT DISTINCT root.id"*)
    printf 'id\nissue-close\n'
    ;;
  *"SELECT COUNT(*) FROM "*"wisps"*"status IN ('open', 'hooked', 'in_progress')"*"created_at <"*)
    printf 'COUNT(*)\n0\n'
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
	writeExecutable(t, filepath.Join(binDir, "bd"), `#!/bin/sh
printf '%s\n' "$*" >> "$BD_CALL_LOG"
exit 0
`)
	writeExecutable(t, filepath.Join(binDir, "gc"), `#!/bin/sh
printf '%s\n' "$*" >> "$GC_CALL_LOG"
exit 0
`)
	writeCityBeadsMetadata(t, cityDir, "beads")
	rigDir := filepath.Join(cityDir, "rigs", "beads-rig")
	writeCityBeadsMetadata(t, rigDir, "beads")
	writeSiteRigBinding(t, cityDir, "beads-rig", rigDir)

	env := map[string]string{
		"BD_CALL_LOG":      bdLog,
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
	for _, want := range []string{
		"WITH RECURSIVE workflow_wisp_root_candidates",
		"WITH RECURSIVE workflow_issue_root_candidates",
		"workflow_descendants(root_id, id)",
		"roots_with_live_descendants",
		"UPDATE `beads`.wisps SET status='closed', closed_at=NOW(), metadata = JSON_SET(COALESCE(metadata, JSON_OBJECT())",
		"'$.\"gc.outcome\"', 'skipped'",
		"'$.\"close_reason\"', 'stale inactive workflow root auto-closed by reaper'",
		"JSON_UNQUOTE(JSON_EXTRACT(w.metadata, '$.\"gc.kind\"')) = 'workflow'",
		"JSON_UNQUOTE(JSON_EXTRACT(w.metadata, '$.\"gc.formula_contract\"')) = 'graph.v2'",
		"COALESCE(JSON_UNQUOTE(JSON_EXTRACT(w.metadata, '$.\"gc.root_bead_id\"')), '') IN ('', w.id)",
		"COALESCE(JSON_UNQUOTE(JSON_EXTRACT(w.metadata, '$.\"gc.root_store_ref\"')), '') = ''",
		"JSON_UNQUOTE(JSON_EXTRACT(w.metadata, '$.\"gc.root_store_ref\"')) = 'beads'",
		"JSON_UNQUOTE(JSON_EXTRACT(w.metadata, '$.\"gc.root_store_ref\"')) IN ('rig:beads-rig')",
		"JSON_UNQUOTE(JSON_EXTRACT(child_wisp.metadata, '$.\"gc.root_bead_id\"')) = root.id",
		"JSON_UNQUOTE(JSON_EXTRACT(i.metadata, '$.\"gc.kind\"')) = 'workflow'",
		"JSON_UNQUOTE(JSON_EXTRACT(i.metadata, '$.\"gc.formula_contract\"')) = 'graph.v2'",
		"COALESCE(JSON_UNQUOTE(JSON_EXTRACT(i.metadata, '$.\"gc.root_bead_id\"')), '') IN ('', i.id)",
		"JSON_UNQUOTE(JSON_EXTRACT(i.metadata, '$.\"gc.root_store_ref\"')) LIKE 'city:%'",
		"JSON_UNQUOTE(JSON_EXTRACT(i.metadata, '$.\"gc.root_store_ref\"')) IN ('rig:beads-rig')",
		"JSON_UNQUOTE(JSON_EXTRACT(child_issue.metadata, '$.\"gc.root_bead_id\"')) = root.id",
		"COALESCE(w.assignee, '') = ''",
		"COALESCE(i.assignee, '') = ''",
		"COALESCE(w.updated_at, w.created_at) < DATE_SUB(NOW(), INTERVAL",
		"COALESCE(i.updated_at, i.created_at) < DATE_SUB(NOW(), INTERVAL",
		"descendant_wisp.status, descendant_issue.status) IN ('open', 'hooked', 'in_progress', 'blocked', 'deferred', 'pinned', 'review', 'testing')",
		"roots_with_recent_descendants",
		"child_dep.type IN ('parent-child', 'tracks', 'blocks')",
		"COALESCE(child_dep.depends_on_issue_id, child_dep.depends_on_wisp_id, child_dep.depends_on_external) = root.id",
		"COALESCE(child_dep.depends_on_issue_id, child_dep.depends_on_wisp_id, child_dep.depends_on_external) = parent.id",
		"workflow_roots=2",
	} {
		if !strings.Contains(log, want) {
			t.Fatalf("reaper workflow-root SQL missing %q:\n%s", want, log)
		}
	}
	if strings.Contains(log, "parent_id") {
		t.Fatalf("reaper workflow-root cleanup used removed parent_id column:\n%s", log)
	}
	if strings.Contains(log, "UPDATE `beads`.issues SET status='closed'") {
		t.Fatalf("reaper closed city workflow issue roots with raw SQL instead of bd close:\n%s", log)
	}

	bdData, err := os.ReadFile(bdLog)
	if err != nil {
		t.Fatalf("ReadFile(bd log): %v", err)
	}
	if !strings.Contains(string(bdData), "close issue-close --reason stale inactive workflow root auto-closed by reaper") {
		t.Fatalf("reaper did not close city workflow issue root through bd close:\n%s", bdData)
	}

	gcData, err := os.ReadFile(gcLog)
	if err != nil {
		t.Fatalf("ReadFile(gc log): %v", err)
	}
	if !strings.Contains(string(gcData), "workflow_roots:2") ||
		!strings.Contains(string(gcData), "skipped_cross_store_workflow_roots:0") {
		t.Fatalf("reaper summary did not report closed workflow roots:\n%s", gcData)
	}
}

func TestReaperWorkflowRootPredicateIsGeneratedFromOneHelper(t *testing.T) {
	data, err := os.ReadFile(filepath.Join(exampleDir(), "packs", "maintenance", "assets", "scripts", "reaper.sh"))
	if err != nil {
		t.Fatalf("ReadFile(reaper.sh): %v", err)
	}
	script := string(data)
	if got := strings.Count(script, "workflow_descendants(root_id, id) AS"); got != 1 {
		t.Fatalf("workflow-root recursive CTE body appears %d times, want one helper definition", got)
	}
	if got := strings.Count(script, "workflow_root_candidates_cte()"); got != 1 {
		t.Fatalf("workflow-root candidate helper appears %d times, want one definition", got)
	}
}

func TestReaperPreservesWorkflowRootsWithLiveDescendants(t *testing.T) {
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
  *"SHOW COLUMNS FROM"*"dependencies"*)
    printf 'Field,Type,Null,Key,Default,Extra\n'
    printf 'issue_id,varchar,NO,,,\n'
    printf 'depends_on_issue_id,varchar,YES,,,\n'
    printf 'depends_on_wisp_id,varchar,YES,,,\n'
    printf 'depends_on_external,varchar,YES,,,\n'
    printf 'type,varchar,NO,,,\n'
    ;;
  *"WITH RECURSIVE workflow_wisp_root_candidates"*"SELECT COUNT(*) FROM ("*)
    printf 'COUNT(*)\n0\n'
    ;;
  *"WITH RECURSIVE workflow_issue_root_candidates"*"SELECT DISTINCT root.id"*)
    printf 'id\n'
    ;;
  *"WITH RECURSIVE workflow_wisp_root_candidates"*"UPDATE "*"wisps SET status='closed'"*)
    printf 'workflow roots with live descendants must be preserved\n' >&2
    exit 42
    ;;
  *"SELECT COUNT(*) FROM "*"wisps"*"status IN ('open', 'hooked', 'in_progress')"*"created_at <"*)
    printf 'COUNT(*)\n0\n'
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
	writeCityBeadsMetadata(t, cityDir, "beads")

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
	for _, want := range []string{
		"roots_with_live_descendants",
		"roots_with_recent_descendants",
		"workflow_descendants(root_id, id)",
		"descendant_wisp.status, descendant_issue.status) IN ('open', 'hooked', 'in_progress', 'blocked', 'deferred', 'pinned', 'review', 'testing')",
		"child_dep.type IN ('parent-child', 'tracks', 'blocks')",
	} {
		if !strings.Contains(log, want) {
			t.Fatalf("reaper workflow-root preserve guard missing %q:\n%s", want, log)
		}
	}
	if strings.Contains(log, "UPDATE `beads`.wisps SET status='closed', closed_at=NOW(), metadata = JSON_SET") ||
		strings.Contains(log, "UPDATE `beads`.issues SET status='closed'") {
		t.Fatalf("reaper closed workflow roots after live-descendant counts returned zero:\n%s", log)
	}

	gcData, err := os.ReadFile(gcLog)
	if err != nil {
		t.Fatalf("ReadFile(gc log): %v", err)
	}
	if strings.Contains(string(gcData), "workflow_roots:1") {
		t.Fatalf("reaper summary reported closed workflow roots despite live descendants:\n%s", gcData)
	}
}

func TestReaperDryRunReportsWouldCloseWorkflowRoots(t *testing.T) {
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
  *"SHOW COLUMNS FROM"*"dependencies"*)
    printf 'Field,Type,Null,Key,Default,Extra\n'
    printf 'issue_id,varchar,NO,,,\n'
    printf 'depends_on_issue_id,varchar,YES,,,\n'
    printf 'depends_on_wisp_id,varchar,YES,,,\n'
    printf 'depends_on_external,varchar,YES,,,\n'
    printf 'type,varchar,NO,,,\n'
    ;;
  *"WITH RECURSIVE workflow_wisp_root_candidates"*"SELECT COUNT(*) FROM ("*)
    printf 'COUNT(*)\n1\n'
    ;;
  *"WITH RECURSIVE workflow_issue_root_candidates"*"SELECT DISTINCT root.id"*)
    printf 'id\nissue-close\n'
    ;;
  *"WITH RECURSIVE workflow_wisp_root_candidates"*"UPDATE "*"wisps SET status='closed'"*)
    printf 'dry-run should not update workflow wisp roots\n' >&2
    exit 42
    ;;
  *"SELECT COUNT(*) FROM "*"wisps"*"status IN ('open', 'hooked', 'in_progress')"*"created_at <"*)
    printf 'COUNT(*)\n0\n'
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
	writeCityBeadsMetadata(t, cityDir, "beads")

	env := map[string]string{
		"DOLT_ARGS_LOG":     doltLog,
		"GC_CALL_LOG":       gcLog,
		"GC_CITY":           cityDir,
		"GC_CITY_PATH":      cityDir,
		"GC_DOLT_HOST":      "127.0.0.1",
		"GC_DOLT_PORT":      "3307",
		"GC_DOLT_USER":      "root",
		"GC_DOLT_PASSWORD":  "",
		"GC_REAPER_DRY_RUN": "1",
		"PATH":              binDir + string(os.PathListSeparator) + os.Getenv("PATH"),
	}

	runScript(t, filepath.Join(exampleDir(), "packs", "maintenance", "assets", "scripts", "reaper.sh"), env)

	logData, err := os.ReadFile(doltLog)
	if err != nil {
		t.Fatalf("ReadFile(dolt log): %v", err)
	}
	if strings.Contains(string(logData), "UPDATE `beads`.wisps SET status='closed', closed_at=NOW(), metadata = JSON_SET") ||
		strings.Contains(string(logData), "UPDATE `beads`.issues SET status='closed', closed_at=NOW(), metadata = JSON_SET") {
		t.Fatalf("dry-run executed workflow-root update:\n%s", logData)
	}

	gcData, err := os.ReadFile(gcLog)
	if err != nil {
		t.Fatalf("ReadFile(gc log): %v", err)
	}
	gcText := string(gcData)
	if !strings.Contains(gcText, "workflow_roots:0") ||
		!strings.Contains(gcText, "would_close_workflow_roots:2") ||
		!strings.Contains(gcText, "(dry run)") {
		t.Fatalf("dry-run summary did not report workflow-root would-close count:\n%s", gcText)
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

func TestReaperFailureAnomalyPreservesDoltErrorTail(t *testing.T) {
	cityDir := t.TempDir()
	binDir := t.TempDir()
	gcLog := filepath.Join(t.TempDir(), "gc.log")

	writeExecutable(t, filepath.Join(binDir, "dolt"), `#!/bin/sh
case "$*" in
  *"SHOW TABLES FROM"*"LIKE 'wisps'"*)
    printf 'Tables_in_db\nwisps\n'
    ;;
  *"SHOW DATABASES"*)
    printf 'Database\nbeads\n'
    ;;
  *"DELETE FROM "*"wisps"*)
    printf 'stdout-query-preview:'
    i=0
    while [ "$i" -lt 700 ]; do
      printf 'Q'
      i=$((i + 1))
    done
    printf '\n'
    printf 'Error 1105 (HY000): wisp_dependencies.depends_on_id missing from schema\n' >&2
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
	if !strings.Contains(gcLogText, "Error 1105 (HY000): wisp_dependencies.depends_on_id missing from schema") {
		t.Fatalf("reaper escalation lost Dolt stderr error tail:\n%s", gcLogText)
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
  *"COUNT("*"wisps w"*"wisp_dependencies d"*)
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
	if !strings.Contains(log, "closed_wisps=1 workflow_roots=0 purged=0") {
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
  *"STR_TO_DATE(JSON_UNQUOTE(JSON_EXTRACT(metadata, '$.expires_at'))"*)
    printf 'id\n'
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
  *"STR_TO_DATE(JSON_UNQUOTE(JSON_EXTRACT(metadata, '$.expires_at'))"*)
    printf 'id\n'
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

func TestReaperDoesNotStaleCloseIssueWithFutureExpiresAt(t *testing.T) {
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
  *"STR_TO_DATE(JSON_UNQUOTE(JSON_EXTRACT(metadata, '$.expires_at'))"*)
    printf 'id\n'
    ;;
  *"SELECT id FROM "*"citydb"*"issues"*)
    case "$*" in
      *"JSON_UNQUOTE(JSON_EXTRACT(metadata, '$.expires_at')) = ''"*)
        printf 'id\n'
        ;;
      *)
        printf 'id\nga-future\n'
        ;;
    esac
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
	if strings.Contains(string(bdData), "close ga-future") {
		t.Fatalf("reaper stale-closed issue with explicit future expires_at:\n%s", bdData)
	}

	gcData, err := os.ReadFile(gcLog)
	if err != nil {
		t.Fatalf("ReadFile(gc log): %v", err)
	}
	gcLogText := string(gcData)
	if !strings.Contains(gcLogText, "closed:0") || !strings.Contains(gcLogText, "expired:0") {
		t.Fatalf("reaper summary reported an issue close despite future expires_at:\n%s", gcLogText)
	}
}

func TestReaperClosesNudgeBeadWithElapsedExpiresAt(t *testing.T) {
	cityDir := t.TempDir()
	writeCityBeadsMetadata(t, cityDir, "citydb")
	binDir := t.TempDir()
	doltLog := filepath.Join(t.TempDir(), "dolt-args.log")
	bdLog := filepath.Join(t.TempDir(), "bd.log")
	gcLog := filepath.Join(t.TempDir(), "gc.log")

	// The Step 3 close query is the only one that compares against
	// UTC_TIMESTAMP(); the gc:nudge-scoped anomaly pre-scan ends in IS NULL.
	// Returning a row from the close query exercises the positive TTL-expiry
	// path: an elapsed nudge bead is closed with reason "ttl:expired by reaper"
	// and counted in the summary as expired:1.
	writeExecutable(t, filepath.Join(binDir, "dolt"), `#!/bin/sh
printf '%s\n' "$*" >> "$DOLT_ARGS_LOG"
case "$*" in
  *"SHOW TABLES FROM"*"LIKE 'wisps'"*)
    printf 'Tables_in_db\nwisps\n'
    ;;
  *"SHOW DATABASES"*)
    printf 'Database\ncitydb\n'
    ;;
  *"UTC_TIMESTAMP()"*)
    printf 'id\nga-expired\n'
    ;;
  *"gc:nudge"*)
    printf 'id\n'
    ;;
  *"SELECT id FROM "*"citydb"*"issues"*)
    printf 'id\n'
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
	if !strings.Contains(bdLogText, "close ga-expired") {
		t.Fatalf("reaper did not close elapsed nudge bead:\n%s", bdLogText)
	}
	if !strings.Contains(bdLogText, "ttl:expired by reaper") {
		t.Fatalf("reaper closed nudge bead without ttl:expired reason:\n%s", bdLogText)
	}

	gcData, err := os.ReadFile(gcLog)
	if err != nil {
		t.Fatalf("ReadFile(gc log): %v", err)
	}
	gcLogText := string(gcData)
	if !strings.Contains(gcLogText, "expired:1") {
		t.Fatalf("reaper summary did not report expired:1:\n%s", gcLogText)
	}
}

func TestReaperDoesNotTTLCloseNonNudgeBeadWithElapsedExpiresAt(t *testing.T) {
	cityDir := t.TempDir()
	writeCityBeadsMetadata(t, cityDir, "citydb")
	binDir := t.TempDir()
	doltLog := filepath.Join(t.TempDir(), "dolt-args.log")
	bdLog := filepath.Join(t.TempDir(), "bd.log")
	gcLog := filepath.Join(t.TempDir(), "gc.log")

	// ga-binding is an expired bead that is NOT labeled gc:nudge (e.g. a
	// gc:extmsg-binding session binding). The Step 3 queries INNER JOIN on the
	// gc:nudge label, so they return no rows for it, and the Step 4 stale path
	// explicitly excludes any bead carrying expires_at. Only a query lacking
	// both guards would surface ga-binding — which the reaper never issues — so
	// the bead must never be closed.
	writeExecutable(t, filepath.Join(binDir, "dolt"), `#!/bin/sh
printf '%s\n' "$*" >> "$DOLT_ARGS_LOG"
case "$*" in
  *"SHOW TABLES FROM"*"LIKE 'wisps'"*)
    printf 'Tables_in_db\nwisps\n'
    ;;
  *"SHOW DATABASES"*)
    printf 'Database\ncitydb\n'
    ;;
  *"gc:nudge"*)
    printf 'id\n'
    ;;
  *"SELECT id FROM "*"citydb"*"issues"*)
    case "$*" in
      *"JSON_UNQUOTE(JSON_EXTRACT(metadata, '$.expires_at')) = ''"*)
        printf 'id\n'
        ;;
      *)
        printf 'id\nga-binding\n'
        ;;
    esac
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
	if strings.Contains(string(bdData), "ga-binding") {
		t.Fatalf("reaper closed a non-nudge bead with expires_at:\n%s", bdData)
	}

	gcData, err := os.ReadFile(gcLog)
	if err != nil {
		t.Fatalf("ReadFile(gc log): %v", err)
	}
	gcLogText := string(gcData)
	if !strings.Contains(gcLogText, "closed:0") || !strings.Contains(gcLogText, "expired:0") {
		t.Fatalf("reaper summary reported a close despite no eligible bead:\n%s", gcLogText)
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
  *"STR_TO_DATE(JSON_UNQUOTE(JSON_EXTRACT(metadata, '$.expires_at'))"*)
    printf 'id\n'
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
  *"STR_TO_DATE(JSON_UNQUOTE(JSON_EXTRACT(metadata, '$.expires_at'))"*)
    printf 'id\n'
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
  *"STR_TO_DATE(JSON_UNQUOTE(JSON_EXTRACT(metadata, '$.expires_at'))"*)
    printf 'id\n'
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
  *"STR_TO_DATE(JSON_UNQUOTE(JSON_EXTRACT(metadata, '$.expires_at'))"*)
    printf 'id\n'
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
  *"STR_TO_DATE(JSON_UNQUOTE(JSON_EXTRACT(metadata, '$.expires_at'))"*)
    printf 'id\n'
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
  *"STR_TO_DATE(JSON_UNQUOTE(JSON_EXTRACT(metadata, '$.expires_at'))"*)
    printf 'id\n'
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
  *"STR_TO_DATE(JSON_UNQUOTE(JSON_EXTRACT(metadata, '$.expires_at'))"*)
    printf 'id\n'
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

func writeProviderRuntimeState(t *testing.T, cityDir string, port int) {
	t.Helper()
	writeRuntimeStateFile(t, cityDir, "dolt-provider-state.json", port, os.Getpid())
}

func writeManagedRuntimeStateWithPID(t *testing.T, cityDir string, port int, pid int) {
	t.Helper()
	writeRuntimeStateFile(t, cityDir, "dolt-state.json", port, pid)
}

func writeRuntimeStateFile(t *testing.T, cityDir string, filename string, port int, pid int) {
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
	if err := os.WriteFile(filepath.Join(stateDir, filename), payload, 0o644); err != nil {
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

func runReaperCloseFixture(t *testing.T, fixture string) (doltLog string, gcLog string) {
	t.Helper()
	cityDir := t.TempDir()
	binDir := t.TempDir()
	doltLog = filepath.Join(t.TempDir(), "dolt-args.log")
	gcLog = filepath.Join(t.TempDir(), "gc.log")

	writeReaperCloseFixtureDoltStub(t, filepath.Join(binDir, "dolt"))
	writeExecutable(t, filepath.Join(binDir, "gc"), `#!/bin/sh
printf '%s\n' "$*" >> "$GC_CALL_LOG"
exit 0
`)

	env := map[string]string{
		"DOLT_ARGS_LOG":        doltLog,
		"GC_CALL_LOG":          gcLog,
		"GC_CITY":              cityDir,
		"GC_CITY_PATH":         cityDir,
		"GC_DOLT_HOST":         "127.0.0.1",
		"GC_DOLT_PORT":         "3307",
		"GC_DOLT_USER":         "root",
		"GC_DOLT_PASSWORD":     "",
		"REAPER_CLOSE_FIXTURE": fixture,
		"PATH":                 binDir + string(os.PathListSeparator) + os.Getenv("PATH"),
	}

	runScript(t, filepath.Join(exampleDir(), "packs", "maintenance", "assets", "scripts", "reaper.sh"), env)
	return doltLog, gcLog
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

func writeSiteRigBinding(t *testing.T, cityDir, rigName, rigDir string) {
	t.Helper()
	gcDir := filepath.Join(cityDir, ".gc")
	if err := os.MkdirAll(gcDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(%s): %v", gcDir, err)
	}
	content := fmt.Sprintf("[[rig]]\nname = %q\npath = %q\n", rigName, rigDir)
	if err := os.WriteFile(filepath.Join(gcDir, "site.toml"), []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile(site.toml): %v", err)
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
*"SHOW COLUMNS FROM"*"dependencies"*)
  printf 'Field,Type,Null,Key,Default,Extra\n'
  dependency_schema="${DOLT_DEPENDENCY_SCHEMA:-split}"
  case "$*" in
    *"wisp_dependencies"*) dependency_schema="${DOLT_WISP_DEPENDENCY_SCHEMA:-$dependency_schema}" ;;
  esac
  if [ "$dependency_schema" = "missing-table" ]; then
    printf 'table not found\n' >&2
    exit 42
  elif [ "$dependency_schema" = "missing-target" ]; then
    printf 'issue_id,varchar,NO,,,\n'
    printf 'type,varchar,NO,,,\n'
  elif [ "$dependency_schema" = "split" ]; then
    printf 'issue_id,varchar,NO,,,\n'
    printf 'depends_on_issue_id,varchar,YES,,,\n'
    printf 'depends_on_wisp_id,varchar,YES,,,\n'
    printf 'depends_on_external,varchar,YES,,,\n'
    printf 'type,varchar,NO,,,\n'
  else
    printf 'issue_id,varchar,NO,,,\n'
    printf 'depends_on_issue_id,varchar,YES,,,\n'
    printf 'depends_on_wisp_id,varchar,YES,,,\n'
    printf 'depends_on_external,varchar,YES,,,\n'
    printf 'type,varchar,NO,,,\n'
  fi
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

func writeReaperCloseFixtureDoltStub(t *testing.T, path string) {
	t.Helper()
	writeExecutable(t, path, `#!/bin/sh
printf '%s\n' "$*" >> "$DOLT_ARGS_LOG"

close_fixture_matches() {
  case "${REAPER_CLOSE_FIXTURE:-}" in
    tracks_owned_root)
      printf '%s' "$*" | grep -F "d.type = 'tracks'" >/dev/null 2>&1 &&
        printf '%s' "$*" | grep -F "gc.root_bead_id" >/dev/null 2>&1
      ;;
    blocks_closed_predecessor)
      printf '%s' "$*" | grep -F "wisp_dependencies d" >/dev/null 2>&1 &&
        printf '%s' "$*" | grep -F "blocks" >/dev/null 2>&1
      ;;
    *)
      return 1
      ;;
  esac
}

case "$*" in
*"SHOW DATABASES"*)
  printf 'Database\nbeads\n'
  ;;
*"SHOW TABLES FROM"*"LIKE 'wisps'"*)
  printf 'Tables_in_db\nwisps\n'
  ;;
*"SHOW COLUMNS FROM"*"dependencies"*)
  printf 'Field,Type,Null,Key,Default,Extra\n'
  printf 'issue_id,varchar,NO,,,\n'
  printf 'depends_on_issue_id,varchar,YES,,,\n'
  printf 'depends_on_wisp_id,varchar,YES,,,\n'
  printf 'depends_on_external,varchar,YES,,,\n'
  printf 'type,varchar,NO,,,\n'
  ;;
*"UPDATE "*"wisps SET status='closed'"*)
  if close_fixture_matches "$*"; then
    printf 'ROW_COUNT()\n1\n'
  else
    printf 'ROW_COUNT()\n0\n'
  fi
  ;;
*"COUNT("*"wisps w"*"wisp_dependencies d"*)
  if close_fixture_matches "$*"; then
    printf 'COUNT(*)\n1\n'
  else
    printf 'COUNT(*)\n0\n'
  fi
  ;;
*"status IN ('open', 'hooked', 'in_progress')"*"created_at <"*)
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
}

func writeMaintenanceBdStub(t *testing.T, path string) {
	t.Helper()
	writeExecutable(t, path, `#!/bin/sh
printf 'pwd=%s beads=%s args=%s\n' "$PWD" "${BEADS_DIR:-}" "$*" >> "$BD_CALL_LOG"
case "$1" in
  prune)
    printf '{"pruned_count":%s}\n' "${BD_PRUNE_COUNT:-0}"
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
		"GC_CALL_LOG":                    gcLog,
		"GC_MAIL_LOG":                    mailLog,
		"GC_CITY":                        cityDir,
		"GC_CITY_PATH":                   cityDir,
		"GC_PACK_STATE_DIR":              stateDir,
		"GC_DOLT_HOST":                   "127.0.0.1",
		"GC_DOLT_PORT":                   "3307",
		"GC_DOLT_USER":                   "root",
		"GC_DOLT_PASSWORD":               "",
		"GC_JSONL_ARCHIVE_REPO":          archiveRepo,
		"GC_JSONL_MAX_PUSH_FAILURES":     "99",
		"GC_JSONL_PUSH_RETRY_DELAY_MIN":  "0",
		"GC_JSONL_PUSH_RETRY_DELAY_SPAN": "0",
		"GC_JSONL_SCRUB":                 "false",
		"GIT_CONFIG_GLOBAL":              filepath.Join(t.TempDir(), "gitconfig"),
		"GIT_CONFIG_NOSYSTEM":            "1",
		"PATH":                           binDir + string(os.PathListSeparator) + os.Getenv("PATH"),
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
	writeIssuesPayloadDoltStubWithPrelude(t, binDir, issuesPayload, "")
}

func writeOriginRemovingMultiRecordDoltStub(t *testing.T, binDir string, currentCount int) {
	t.Helper()
	rows := make([]string, 0, currentCount)
	for i := 0; i < currentCount; i++ {
		rows = append(rows, fmt.Sprintf(`{"id":"c%d","title":"cur-%d"}`, i, i))
	}
	writeIssuesPayloadDoltStubWithPrelude(t, binDir, `{"rows":[`+strings.Join(rows, ",")+`]}`, `
if [ -n "${DOLT_REMOVE_ORIGIN_FLAG:-}" ] && [ ! -e "$DOLT_REMOVE_ORIGIN_FLAG" ]; then
  : > "$DOLT_REMOVE_ORIGIN_FLAG"
  git -C "$GC_JSONL_ARCHIVE_REPO" remote remove origin 2>/dev/null || true
fi
`)
}

func writeIssuesPayloadDoltStubWithPrelude(t *testing.T, binDir, issuesPayload, prelude string) {
	t.Helper()
	body := "#!/bin/sh\n" +
		"if [ -n \"${DOLT_ARGS_LOG:-}\" ]; then\n" +
		"  printf '%s\\n' \"$*\" >> \"$DOLT_ARGS_LOG\"\n" +
		"fi\n" +
		prelude +
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

func writeIssuesPayloadWithSourceCountDoltStub(t *testing.T, binDir, issuesPayload string, sourceCount int) {
	t.Helper()
	body := "#!/bin/sh\n" +
		"if [ -n \"${DOLT_ARGS_LOG:-}\" ]; then\n" +
		"  printf '%s\\n' \"$*\" >> \"$DOLT_ARGS_LOG\"\n" +
		"fi\n" +
		"case \"$*\" in\n" +
		"  *\"SHOW TABLES FROM\"*\"LIKE 'wisps'\"*)\n" +
		"    printf 'Tables_in_db\\nwisps\\n'\n" +
		"    ;;\n" +
		"  *\"SHOW DATABASES\"*)\n" +
		"    printf 'Database\\nbeads\\n'\n" +
		"    ;;\n" +
		"  *\"COUNT(\"*\"FROM \\`beads\\`.issues\"*)\n" +
		"    printf 'row_count\\n" + strconv.Itoa(sourceCount) + "\\n'\n" +
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

func writeGitPushAttemptStub(t *testing.T, binDir, realGit, mode, logFile string) {
	t.Helper()
	countFile := filepath.Join(t.TempDir(), "push-count")
	writeExecutable(t, filepath.Join(binDir, "git"), fmt.Sprintf(`#!/bin/sh
count_file=%s
log_file=%s
mode=%s
real_git=%s
for arg in "$@"; do
    if [ "$arg" = "push" ]; then
        count=0
        if [ -f "$count_file" ]; then
            count="$(cat "$count_file")"
        fi
        count=$((count + 1))
        printf '%%s\n' "$count" > "$count_file"
        printf '%%s\n' "$count" >> "$log_file"
        if [ "$mode" = "fail-first" ] && [ "$count" -eq 1 ]; then
            echo "simulated git push failure on attempt $count" >&2
            exit 1
        fi
        if [ "$mode" = "always-fail" ]; then
            echo "simulated git push failure on attempt $count" >&2
            exit 1
        fi
        break
    fi
done
exec "$real_git" "$@"
`, strconv.Quote(countFile), strconv.Quote(logFile), strconv.Quote(mode), strconv.Quote(realGit)))
}

func writeGitPushRemoteAdvanceRaceStub(t *testing.T, binDir, realGit, remoteRepo, logFile string) {
	t.Helper()
	countFile := filepath.Join(t.TempDir(), "push-count")
	writeExecutable(t, filepath.Join(binDir, "git"), fmt.Sprintf(`#!/bin/sh
count_file=%s
log_file=%s
remote_repo=%s
real_git=%s
advance_dir=""
cleanup_advance() {
    if [ -n "$advance_dir" ]; then
        rm -rf "$advance_dir"
    fi
}
trap cleanup_advance EXIT
for arg in "$@"; do
    if [ "$arg" = "push" ]; then
        count=0
        if [ -f "$count_file" ]; then
            count="$(cat "$count_file")"
        fi
        count=$((count + 1))
        printf '%%s\n' "$count" > "$count_file"
        printf '%%s\n' "$count" >> "$log_file"
        if [ "$count" -eq 1 ]; then
            advance_dir="$(mktemp -d)"
            "$real_git" clone -q "$remote_repo" "$advance_dir" || exit $?
            "$real_git" -C "$advance_dir" checkout -q main || exit $?
            "$real_git" -C "$advance_dir" config user.email test@example.invalid || exit $?
            "$real_git" -C "$advance_dir" config user.name test || exit $?
            printf 'remote advance during push\n' > "$advance_dir/remote-push-race.txt" || exit $?
            "$real_git" -C "$advance_dir" add -A || exit $?
            "$real_git" -C "$advance_dir" commit -q -m "remote advance during push" || exit $?
            "$real_git" -C "$advance_dir" push -q origin main || exit $?
        fi
        break
    fi
done
"$real_git" "$@"
exit $?
`, strconv.Quote(countFile), strconv.Quote(logFile), strconv.Quote(remoteRepo), strconv.Quote(realGit)))
}

func writeSleepLogStub(t *testing.T, binDir, logFile string) {
	t.Helper()
	writeExecutable(t, filepath.Join(binDir, "sleep"), fmt.Sprintf(`#!/bin/sh
printf '%%s\n' "$*" >> %s
exit 0
`, strconv.Quote(logFile)))
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
// cannot be reached. Seed count is fixed because push-failure tests care only
// about whether a commit exists, not about its row count.
func initSeedArchiveWithUnreachableRemote(t *testing.T, archiveRepo string) {
	t.Helper()
	initSeedArchive(t, archiveRepo, 3)
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
	// (default 100).
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

func TestJsonlExportSuppressesDropSpikeWhenDoltSourceCountHealthy(t *testing.T) {
	cityDir := t.TempDir()
	binDir := t.TempDir()
	stateDir := t.TempDir()
	gcLog := filepath.Join(t.TempDir(), "gc.log")
	mailLog := filepath.Join(t.TempDir(), "gc-mail.log")
	archiveRepo := filepath.Join(cityDir, "archive")

	initSeedArchive(t, archiveRepo, 100)
	writeIssuesPayloadWithSourceCountDoltStub(t, binDir, `{"rows":[]}`, 120)
	writeJsonlExportGCStub(t, binDir)

	env := jsonlExportEnv(t, cityDir, binDir, stateDir, archiveRepo, gcLog, mailLog)

	runScript(t, filepath.Join(exampleDir(), "packs", "maintenance", "assets", "scripts", "jsonl-export.sh"), env)

	mailData, err := os.ReadFile(mailLog)
	if err != nil && !os.IsNotExist(err) {
		t.Fatalf("ReadFile(mail log): %v", err)
	}
	if strings.Contains(string(mailData), "ESCALATION: JSONL spike") {
		t.Fatalf("spike escalation fired despite healthy Dolt source-of-truth count; mail log:\n%s", mailData)
	}

	gcData, err := os.ReadFile(gcLog)
	if err != nil {
		t.Fatalf("ReadFile(gc log): %v", err)
	}
	if strings.Contains(string(gcData), "HALTED on spike detection") {
		t.Fatalf("healthy Dolt source-of-truth count should suppress HALT; gc log:\n%s", gcData)
	}
	if !strings.Contains(string(gcData), "DOG_DONE: jsonl — exported") {
		t.Fatalf("expected normal export summary after source-of-truth suppression; gc log:\n%s", gcData)
	}
}

func TestJsonlExportPreservesDropSpikeWhenDoltSourceCountAlsoShrank(t *testing.T) {
	cityDir := t.TempDir()
	binDir := t.TempDir()
	stateDir := t.TempDir()
	gcLog := filepath.Join(t.TempDir(), "gc.log")
	mailLog := filepath.Join(t.TempDir(), "gc-mail.log")
	archiveRepo := filepath.Join(cityDir, "archive")

	initSeedArchive(t, archiveRepo, 100)
	writeIssuesPayloadWithSourceCountDoltStub(t, binDir, `{"rows":[]}`, 10)
	writeJsonlExportGCStub(t, binDir)

	env := jsonlExportEnv(t, cityDir, binDir, stateDir, archiveRepo, gcLog, mailLog)

	runScript(t, filepath.Join(exampleDir(), "packs", "maintenance", "assets", "scripts", "jsonl-export.sh"), env)

	mailData, err := os.ReadFile(mailLog)
	if err != nil && !os.IsNotExist(err) {
		t.Fatalf("ReadFile(mail log): %v", err)
	}
	if !strings.Contains(string(mailData), "ESCALATION: JSONL spike") {
		t.Fatalf("expected spike escalation when Dolt source-of-truth count also shrank; mail log:\n%s", mailData)
	}

	gcData, err := os.ReadFile(gcLog)
	if err != nil {
		t.Fatalf("ReadFile(gc log): %v", err)
	}
	if !strings.Contains(string(gcData), "HALTED on spike detection") {
		t.Fatalf("expected HALT when source-of-truth also confirms the drop; gc log:\n%s", gcData)
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
	doltLog := filepath.Join(t.TempDir(), "dolt.log")
	archiveRepo := filepath.Join(cityDir, "archive")

	initSeedArchive(t, archiveRepo, 12)
	rows := []string{
		`{"id":"bd-100","title":"real-leading-prefix","issue_type":"task"}`,
		`{"id":"prod-gc-near","title":"agc:kept","issue_type":"task"}`,
		`{"id":"prod-order-near","title":"preorder:kept","issue_type":"task"}`,
		`{"id":"prod-sling-task","title":"sling-ga-user-task","issue_type":"task"}`,
		`{"id":"prod-manual-convoy","title":"manual-convoy","issue_type":"convoy"}`,
	}
	for i := 1; i <= 7; i++ {
		rows = append(rows, fmt.Sprintf(`{"id":"prod-%d","title":"real-%d","issue_type":"task"}`, i, i))
	}
	rows = append(rows,
		`{"id":"prod-test","title":"Test Issue 99","issue_type":"task"}`,
		`{"id":"sys-gc","title":"gc:status","issue_type":"task"}`,
		`{"id":"sys-order","title":"order:beads-health","issue_type":"task"}`,
		`{"id":"sys-auto-convoy","title":"sling-ga-auto","issue_type":"convoy"}`,
		`{"id":"sys-message","title":"message-row","issue_type":"message"}`,
		`{"id":"sys-event","title":"event-row","issue_type":"event"}`,
		`{"id":"sys-wisp","title":"wisp-row","issue_type":"wisp"}`,
		`{"id":"sys-agent","title":"agent-row","issue_type":"agent"}`,
	)
	if len(rows) != 20 {
		t.Fatalf("test fixture row count drifted: got %d, want 20", len(rows))
	}
	writeIssueRowsDoltStub(t, binDir, rows)
	writeJsonlExportGCStub(t, binDir)

	env := jsonlExportEnv(t, cityDir, binDir, stateDir, archiveRepo, gcLog, mailLog)
	env["GC_JSONL_SCRUB"] = "true"
	env["DOLT_ARGS_LOG"] = doltLog

	runScript(t, filepath.Join(exampleDir(), "packs", "maintenance", "assets", "scripts", "jsonl-export.sh"), env)

	doltData, err := os.ReadFile(doltLog)
	if err != nil {
		t.Fatalf("ReadFile(dolt log): %v", err)
	}
	doltSQL := string(doltData)
	for _, want := range []string{
		"issue_type NOT IN ('message', 'event', 'wisp', 'agent')",
		"title NOT LIKE 'gc:%'",
		"title NOT LIKE 'order:%'",
		"NOT (issue_type = 'convoy' AND title LIKE 'sling-%')",
	} {
		if !strings.Contains(doltSQL, want) {
			t.Fatalf("expected scrub SQL to contain %q, got:\n%s", want, doltSQL)
		}
	}

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
	for _, want := range []string{"prod-gc-near", "prod-order-near", "prod-sling-task", "prod-manual-convoy"} {
		if !strings.Contains(string(exported), want) {
			t.Fatalf("expected scrubbed export to preserve near-miss row %q, got:\n%s", want, exported)
		}
	}
	for _, unwanted := range []string{
		"Test Issue 99",
		"sys-gc",
		"sys-order",
		"sys-auto-convoy",
		"sys-message",
		"sys-event",
		"sys-wisp",
		"sys-agent",
	} {
		if strings.Contains(string(exported), unwanted) {
			t.Fatalf("expected scrubbed export to remove %q, got:\n%s", unwanted, exported)
		}
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
	for _, want := range []string{"prod-gc-near", "prod-order-near", "prod-sling-task", "prod-manual-convoy"} {
		if !strings.Contains(string(legacyExported), want) {
			t.Fatalf("expected legacy flat export to preserve near-miss row %q, got:\n%s", want, legacyExported)
		}
	}
	for _, unwanted := range []string{
		"Test Issue 99",
		"sys-gc",
		"sys-order",
		"sys-auto-convoy",
		"sys-message",
		"sys-event",
		"sys-wisp",
		"sys-agent",
	} {
		if strings.Contains(string(legacyExported), unwanted) {
			t.Fatalf("expected legacy flat export to remove %q, got:\n%s", unwanted, legacyExported)
		}
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

	initSeedArchiveWithUnreachableRemote(t, archiveRepo)
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

	initSeedArchiveWithUnreachableRemote(t, archiveRepo)
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

func TestJsonlExportPushSuccessWritesLastPushAt(t *testing.T) {
	cityDir := t.TempDir()
	binDir := t.TempDir()
	stateDir := t.TempDir()
	gcLog := filepath.Join(t.TempDir(), "gc.log")
	mailLog := filepath.Join(t.TempDir(), "gc-mail.log")
	archiveRepo := filepath.Join(cityDir, "archive")
	stateFile := filepath.Join(stateDir, "jsonl-export-state.json")

	initSeedArchiveWithRemote(t, archiveRepo)
	writeMultiRecordDoltStub(t, binDir, 100)
	writeJsonlExportGCStub(t, binDir)

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
	ts, ok := state["last_push_at"].(string)
	if !ok || ts == "" {
		t.Fatalf("last_push_at missing from state:\n%s", stateData)
	}
	if _, err := time.Parse(time.RFC3339, ts); err != nil {
		t.Fatalf("last_push_at = %q is not RFC3339: %v", ts, err)
	}
	if _, has := state["last_push_stderr"]; has {
		t.Fatalf("last_push_stderr should not be present after a successful push:\n%s", stateData)
	}
}

func TestJsonlExportPushFailureWritesLastPushStderr(t *testing.T) {
	cityDir := t.TempDir()
	binDir := t.TempDir()
	stateDir := t.TempDir()
	gcLog := filepath.Join(t.TempDir(), "gc.log")
	mailLog := filepath.Join(t.TempDir(), "gc-mail.log")
	archiveRepo := filepath.Join(cityDir, "archive")
	stateFile := filepath.Join(stateDir, "jsonl-export-state.json")
	pushLog := filepath.Join(t.TempDir(), "git-push.log")

	// Must have an origin remote — auto-detect mode skips push (and therefore
	// the failure path) when origin is unset. An unreachable remote triggers
	// the real push failure in push_archive_main.
	initSeedArchiveWithUnreachableRemote(t, archiveRepo)
	writeMultiRecordDoltStub(t, binDir, 5)
	writeJsonlExportGCStub(t, binDir)
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatalf("LookPath(git): %v", err)
	}
	writeGitPushAttemptStub(t, binDir, realGit, "pass-through", pushLog)

	env := jsonlExportEnv(t, cityDir, binDir, stateDir, archiveRepo, gcLog, mailLog)

	out, err := runScriptResult(t, filepath.Join(exampleDir(), "packs", "maintenance", "assets", "scripts", "jsonl-export.sh"), env)
	if err != nil {
		t.Fatalf("jsonl-export.sh should report push failure in summary without exiting non-zero: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "pushing archive main failed after 3 attempts") {
		t.Fatalf("expected terminal retry message, got:\n%s", out)
	}

	pushData, err := os.ReadFile(pushLog)
	if err != nil {
		t.Fatalf("ReadFile(push log): %v", err)
	}
	if got := strings.Count(string(pushData), "\n"); got != 3 {
		t.Fatalf("unreachable remote should be retried exactly 3 times, got %d:\n%s", got, pushData)
	}

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
	stderrVal, ok := state["last_push_stderr"].(string)
	if !ok || stderrVal == "" {
		t.Fatalf("last_push_stderr missing from state after push failure:\n%s", stateData)
	}
}

func TestJsonlExportPushRetriesAndRecordsSuccessAfterTransientFailure(t *testing.T) {
	cityDir := t.TempDir()
	binDir := t.TempDir()
	stateDir := t.TempDir()
	gcLog := filepath.Join(t.TempDir(), "gc.log")
	mailLog := filepath.Join(t.TempDir(), "gc-mail.log")
	archiveRepo := filepath.Join(cityDir, "archive")
	stateFile := filepath.Join(stateDir, "jsonl-export-state.json")
	pushLog := filepath.Join(t.TempDir(), "git-push.log")
	sleepLog := filepath.Join(t.TempDir(), "sleep.log")

	remoteRepo, priorHead := initSeedArchiveWithRemote(t, archiveRepo)
	writeMultiRecordDoltStub(t, binDir, 100)
	writeJsonlExportGCStub(t, binDir)
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatalf("LookPath(git): %v", err)
	}
	writeGitPushAttemptStub(t, binDir, realGit, "fail-first", pushLog)
	writeSleepLogStub(t, binDir, sleepLog)

	env := jsonlExportEnv(t, cityDir, binDir, stateDir, archiveRepo, gcLog, mailLog)
	env["GC_JSONL_PUSH_RETRY_DELAY_MIN"] = "0"
	env["GC_JSONL_PUSH_RETRY_DELAY_SPAN"] = "0"

	out, err := runScriptResult(t, filepath.Join(exampleDir(), "packs", "maintenance", "assets", "scripts", "jsonl-export.sh"), env)
	if err != nil {
		t.Fatalf("jsonl-export.sh: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "push succeeded on retry attempt 2") {
		t.Fatalf("expected successful retry to be operator-visible, got:\n%s", out)
	}

	pushData, err := os.ReadFile(pushLog)
	if err != nil {
		t.Fatalf("ReadFile(push log): %v", err)
	}
	if got := strings.Count(string(pushData), "\n"); got != 2 {
		t.Fatalf("expected exactly 2 push attempts, got %d:\n%s", got, pushData)
	}
	sleepData, err := os.ReadFile(sleepLog)
	if err != nil {
		t.Fatalf("ReadFile(sleep log): %v", err)
	}
	if got := strings.TrimSpace(string(sleepData)); got != "0.00" {
		t.Fatalf("retry delay override must produce one zero-second sleep, got %q", got)
	}

	remoteHeadOut, err := exec.Command("git", "--git-dir", remoteRepo, "rev-parse", "refs/heads/main").CombinedOutput()
	if err != nil {
		t.Fatalf("git rev-parse remote main: %v\n%s", err, remoteHeadOut)
	}
	if got := strings.TrimSpace(string(remoteHeadOut)); got == priorHead {
		t.Fatalf("expected retry success to advance remote main from %s", priorHead)
	}

	stateData, err := os.ReadFile(stateFile)
	if err != nil {
		t.Fatalf("ReadFile(state file): %v", err)
	}
	var state map[string]any
	if err := json.Unmarshal(stateData, &state); err != nil {
		t.Fatalf("Unmarshal(state file): %v\n%s", err, stateData)
	}
	if got := state["consecutive_push_failures"]; got != float64(0) {
		t.Fatalf("consecutive_push_failures = %v, want 0\nstate: %s", got, stateData)
	}
	if got := state["pending_archive_push"]; got == true {
		t.Fatalf("pending_archive_push should clear after retry success\nstate: %s", stateData)
	}
	if _, ok := state["last_push_at"].(string); !ok {
		t.Fatalf("last_push_at should be set after retry success:\n%s", stateData)
	}
	if _, has := state["last_push_stderr"]; has {
		t.Fatalf("last_push_stderr should clear after retry success:\n%s", stateData)
	}
}

func TestJsonlExportPushRetryRebasesAfterRemoteAdvanceRace(t *testing.T) {
	cityDir := t.TempDir()
	binDir := t.TempDir()
	stateDir := t.TempDir()
	gcLog := filepath.Join(t.TempDir(), "gc.log")
	mailLog := filepath.Join(t.TempDir(), "gc-mail.log")
	archiveRepo := filepath.Join(cityDir, "archive")
	stateFile := filepath.Join(stateDir, "jsonl-export-state.json")
	pushLog := filepath.Join(t.TempDir(), "git-push.log")
	sleepLog := filepath.Join(t.TempDir(), "sleep.log")

	remoteRepo, priorHead := initSeedArchiveWithRemote(t, archiveRepo)
	writeMultiRecordDoltStub(t, binDir, 101)
	writeJsonlExportGCStub(t, binDir)
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatalf("LookPath(git): %v", err)
	}
	writeGitPushRemoteAdvanceRaceStub(t, binDir, realGit, remoteRepo, pushLog)
	writeSleepLogStub(t, binDir, sleepLog)

	env := jsonlExportEnv(t, cityDir, binDir, stateDir, archiveRepo, gcLog, mailLog)

	out, err := runScriptResult(t, filepath.Join(exampleDir(), "packs", "maintenance", "assets", "scripts", "jsonl-export.sh"), env)
	if err != nil {
		t.Fatalf("jsonl-export.sh: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "push succeeded on retry attempt 2") {
		t.Fatalf("expected retry success after remote advance, got:\n%s", out)
	}

	pushData, err := os.ReadFile(pushLog)
	if err != nil {
		t.Fatalf("ReadFile(push log): %v", err)
	}
	if got := strings.Count(string(pushData), "\n"); got != 2 {
		t.Fatalf("expected first non-fast-forward push plus one retry, got %d attempts:\n%s", got, pushData)
	}
	sleepData, err := os.ReadFile(sleepLog)
	if err != nil {
		t.Fatalf("ReadFile(sleep log): %v", err)
	}
	if got := strings.TrimSpace(string(sleepData)); got != "0.00" {
		t.Fatalf("retry delay override must produce one zero-second sleep, got %q", got)
	}

	remoteHeadOut, err := exec.Command("git", "--git-dir", remoteRepo, "rev-parse", "refs/heads/main").CombinedOutput()
	if err != nil {
		t.Fatalf("git rev-parse remote main: %v\n%s", err, remoteHeadOut)
	}
	if got := strings.TrimSpace(string(remoteHeadOut)); got == priorHead {
		t.Fatalf("expected retry success to advance remote main from %s", priorHead)
	}

	logOut, err := exec.Command("git", "--git-dir", remoteRepo, "log", "--format=%s", "-3", "refs/heads/main").CombinedOutput()
	if err != nil {
		t.Fatalf("git log remote main: %v\n%s", err, logOut)
	}
	remoteLog := string(logOut)
	if !strings.Contains(remoteLog, "remote advance during push") || !strings.Contains(remoteLog, "records=101") {
		t.Fatalf("expected remote history to contain both sibling advance and rebased export commit, got:\n%s", remoteLog)
	}

	stateData, err := os.ReadFile(stateFile)
	if err != nil {
		t.Fatalf("ReadFile(state file): %v", err)
	}
	var state map[string]any
	if err := json.Unmarshal(stateData, &state); err != nil {
		t.Fatalf("Unmarshal(state file): %v\n%s", err, stateData)
	}
	if got := state["consecutive_push_failures"]; got != float64(0) {
		t.Fatalf("consecutive_push_failures = %v, want 0\nstate: %s", got, stateData)
	}
	if got := state["pending_archive_push"]; got == true {
		t.Fatalf("pending_archive_push should clear after retry success\nstate: %s", stateData)
	}
	if _, ok := state["last_push_at"].(string); !ok {
		t.Fatalf("last_push_at should be set after retry success:\n%s", stateData)
	}
	if _, has := state["last_push_stderr"]; has {
		t.Fatalf("last_push_stderr should clear after retry success:\n%s", stateData)
	}
}

func TestJsonlExportPushRetriesThreeTimesBeforeRecordingFailure(t *testing.T) {
	cityDir := t.TempDir()
	binDir := t.TempDir()
	stateDir := t.TempDir()
	gcLog := filepath.Join(t.TempDir(), "gc.log")
	mailLog := filepath.Join(t.TempDir(), "gc-mail.log")
	archiveRepo := filepath.Join(cityDir, "archive")
	stateFile := filepath.Join(stateDir, "jsonl-export-state.json")
	pushLog := filepath.Join(t.TempDir(), "git-push.log")
	sleepLog := filepath.Join(t.TempDir(), "sleep.log")

	remoteRepo, priorHead := initSeedArchiveWithRemote(t, archiveRepo)
	writeMultiRecordDoltStub(t, binDir, 100)
	writeJsonlExportGCStub(t, binDir)
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatalf("LookPath(git): %v", err)
	}
	writeGitPushAttemptStub(t, binDir, realGit, "always-fail", pushLog)
	writeSleepLogStub(t, binDir, sleepLog)

	env := jsonlExportEnv(t, cityDir, binDir, stateDir, archiveRepo, gcLog, mailLog)
	env["GC_JSONL_PUSH_RETRY_DELAY_MIN"] = "0"
	env["GC_JSONL_PUSH_RETRY_DELAY_SPAN"] = "0"

	out, err := runScriptResult(t, filepath.Join(exampleDir(), "packs", "maintenance", "assets", "scripts", "jsonl-export.sh"), env)
	if err != nil {
		t.Fatalf("jsonl-export.sh should report push failure in summary without exiting non-zero: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "pushing archive main failed after 3 attempts") {
		t.Fatalf("expected terminal retry message, got:\n%s", out)
	}

	pushData, err := os.ReadFile(pushLog)
	if err != nil {
		t.Fatalf("ReadFile(push log): %v", err)
	}
	if got := strings.Count(string(pushData), "\n"); got != 3 {
		t.Fatalf("expected exactly 3 push attempts, got %d:\n%s", got, pushData)
	}
	sleepData, err := os.ReadFile(sleepLog)
	if err != nil {
		t.Fatalf("ReadFile(sleep log): %v", err)
	}
	if got := strings.TrimSpace(string(sleepData)); got != "0.00\n0.00" {
		t.Fatalf("retry delay override must produce two zero-second sleeps, got %q", got)
	}

	remoteHeadOut, err := exec.Command("git", "--git-dir", remoteRepo, "rev-parse", "refs/heads/main").CombinedOutput()
	if err != nil {
		t.Fatalf("git rev-parse remote main: %v\n%s", err, remoteHeadOut)
	}
	if got := strings.TrimSpace(string(remoteHeadOut)); got != priorHead {
		t.Fatalf("terminal push failure must leave remote unchanged: got %s want %s", got, priorHead)
	}

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
	stderrVal, ok := state["last_push_stderr"].(string)
	if !ok || !strings.Contains(stderrVal, "simulated git push failure on attempt 3") {
		t.Fatalf("last_push_stderr should capture final push failure, got %q\nstate: %s", stderrVal, stateData)
	}
	if _, has := state["last_push_at"]; has {
		t.Fatalf("last_push_at should not be set after terminal push failure:\n%s", stateData)
	}
}

func TestJsonlExportPushSuccessAfterFailureClearsLastPushStderr(t *testing.T) {
	cityDir := t.TempDir()
	binDir := t.TempDir()
	stateDir := t.TempDir()
	gcLog := filepath.Join(t.TempDir(), "gc.log")
	mailLog := filepath.Join(t.TempDir(), "gc-mail.log")
	archiveRepo := filepath.Join(cityDir, "archive")
	stateFile := filepath.Join(stateDir, "jsonl-export-state.json")

	initSeedArchiveWithRemote(t, archiveRepo)
	writeMultiRecordDoltStub(t, binDir, 100)
	writeJsonlExportGCStub(t, binDir)

	if err := os.WriteFile(stateFile, []byte(`{"consecutive_push_failures":2,"last_push_stderr":"old boom","pending_archive_push":true}`+"\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(seed state): %v", err)
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
	if _, has := state["last_push_stderr"]; has {
		t.Fatalf("last_push_stderr should be cleared after a successful push:\n%s", stateData)
	}
	if got := state["consecutive_push_failures"]; got != float64(0) {
		t.Fatalf("consecutive_push_failures = %v, want 0\nstate: %s", got, stateData)
	}
	if _, ok := state["last_push_at"].(string); !ok {
		t.Fatalf("last_push_at should be set after a successful push:\n%s", stateData)
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

func TestJsonlExportPushModeMemoizesOriginForRun(t *testing.T) {
	cityDir := t.TempDir()
	binDir := t.TempDir()
	stateDir := t.TempDir()
	gcLog := filepath.Join(t.TempDir(), "gc.log")
	mailLog := filepath.Join(t.TempDir(), "gc-mail.log")
	archiveRepo := filepath.Join(cityDir, "archive")
	stateFile := filepath.Join(stateDir, "jsonl-export-state.json")
	originRemovedFlag := filepath.Join(t.TempDir(), "origin-removed")

	initSeedArchiveWithRemote(t, archiveRepo)
	writeOriginRemovingMultiRecordDoltStub(t, binDir, 5)
	writeJsonlExportGCStub(t, binDir)

	env := jsonlExportEnv(t, cityDir, binDir, stateDir, archiveRepo, gcLog, mailLog)
	env["GC_JSONL_MIN_PREV_FOR_SPIKE"] = "1000"
	env["DOLT_REMOVE_ORIGIN_FLAG"] = originRemovedFlag

	out, err := runScriptResult(t, filepath.Join(exampleDir(), "packs", "maintenance", "assets", "scripts", "jsonl-export.sh"), env)
	if err != nil {
		t.Fatalf("jsonl-export.sh: %v\n%s", err, out)
	}

	if !strings.Contains(string(out), "archive running in push mode") {
		t.Fatalf("expected first mode probe to log push mode, got:\n%s", out)
	}
	if !strings.Contains(string(out), "push: failed") {
		t.Fatalf("expected cached push mode to still attempt push after origin removal, got:\n%s", out)
	}
	if strings.Contains(string(out), "push: skipped (local-only)") {
		t.Fatalf("cached push mode must not fall back to local-only mid-run, got:\n%s", out)
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
	if got, ok := state["consecutive_push_failures"].(float64); !ok || got != 1 {
		t.Fatalf("consecutive_push_failures = %v, want 1\nstate: %s", state["consecutive_push_failures"], stateData)
	}
}

func TestJsonlExportModeRelogIntervalOverrideRelogsSameMode(t *testing.T) {
	cityDir := t.TempDir()
	binDir := t.TempDir()
	stateDir := t.TempDir()
	gcLog := filepath.Join(t.TempDir(), "gc.log")
	mailLog := filepath.Join(t.TempDir(), "gc-mail.log")
	archiveRepo := filepath.Join(cityDir, "archive")
	stateFile := filepath.Join(stateDir, "jsonl-export-state.json")

	initSeedArchive(t, archiveRepo, 3)
	writeMultiRecordDoltStub(t, binDir, 3)
	writeJsonlExportGCStub(t, binDir)

	if err := os.MkdirAll(filepath.Dir(stateFile), 0o755); err != nil {
		t.Fatalf("MkdirAll(state dir): %v", err)
	}
	priorState := `{"last_logged_mode":"local-only","last_logged_at":"2026-05-01T00:00:00Z"}` + "\n"
	if err := os.WriteFile(stateFile, []byte(priorState), 0o644); err != nil {
		t.Fatalf("WriteFile(state file): %v", err)
	}

	env := jsonlExportEnv(t, cityDir, binDir, stateDir, archiveRepo, gcLog, mailLog)
	env["GC_JSONL_MODE_RELOG_INTERVAL"] = "1"
	delete(env, "GC_JSONL_MAX_PUSH_FAILURES")

	out, err := runScriptResult(t, filepath.Join(exampleDir(), "packs", "maintenance", "assets", "scripts", "jsonl-export.sh"), env)
	if err != nil {
		t.Fatalf("jsonl-export.sh: %v\n%s", err, out)
	}

	if !strings.Contains(string(out), "archive running in local-only mode") {
		t.Fatalf("expected expired override interval to re-log local-only mode, got:\n%s", out)
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
	if got := state["last_logged_at"]; got == "2026-05-01T00:00:00Z" {
		t.Fatalf("last_logged_at not refreshed after interval expiry\nstate: %s", stateData)
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

func TestJsonlExportLocalOnlyModeClearsStalePushFailureState(t *testing.T) {
	cityDir := t.TempDir()
	binDir := t.TempDir()
	stateDir := t.TempDir()
	gcLog := filepath.Join(t.TempDir(), "gc.log")
	mailLog := filepath.Join(t.TempDir(), "gc-mail.log")
	archiveRepo := filepath.Join(cityDir, "archive")
	stateFile := filepath.Join(stateDir, "jsonl-export-state.json")

	initSeedArchive(t, archiveRepo, 3)
	writeMultiRecordDoltStub(t, binDir, 3)
	writeJsonlExportGCStub(t, binDir)

	if err := os.MkdirAll(filepath.Dir(stateFile), 0o755); err != nil {
		t.Fatalf("MkdirAll(state dir): %v", err)
	}
	priorState := `{"last_logged_mode":"local-only","last_logged_at":"2026-05-10T00:00:00Z","consecutive_push_failures":38,"pending_archive_push":true,"push_failure_escalated":true}` + "\n"
	if err := os.WriteFile(stateFile, []byte(priorState), 0o644); err != nil {
		t.Fatalf("WriteFile(state file): %v", err)
	}

	env := jsonlExportEnv(t, cityDir, binDir, stateDir, archiveRepo, gcLog, mailLog)
	delete(env, "GC_JSONL_MAX_PUSH_FAILURES")

	out, err := runScriptResult(t, filepath.Join(exampleDir(), "packs", "maintenance", "assets", "scripts", "jsonl-export.sh"), env)
	if err != nil {
		t.Fatalf("jsonl-export.sh: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "push: skipped (local-only)") {
		t.Fatalf("expected local-only pending-push summary, got:\n%s", out)
	}

	stateData, err := os.ReadFile(stateFile)
	if err != nil {
		t.Fatalf("ReadFile(state file): %v", err)
	}
	var state map[string]any
	if err := json.Unmarshal(stateData, &state); err != nil {
		t.Fatalf("Unmarshal(state file): %v\n%s", err, stateData)
	}
	if got, ok := state["consecutive_push_failures"].(float64); !ok || got != 0 {
		t.Fatalf("consecutive_push_failures = %v, want 0\nstate: %s", state["consecutive_push_failures"], stateData)
	}
	if got, ok := state["pending_archive_push"].(bool); !ok || !got {
		t.Fatalf("pending_archive_push must remain true to track deferred push\nstate: %s", stateData)
	}
	if _, ok := state["push_failure_escalated"]; ok {
		t.Fatalf("push_failure_escalated must clear in local-only mode\nstate: %s", stateData)
	}

	mailContents, err := os.ReadFile(mailLog)
	if err == nil && strings.Contains(string(mailContents), "ESCALATION: JSONL push failed [HIGH]") {
		t.Fatalf("local-only cleanup must not escalate; mail log:\n%s", mailContents)
	}
}

func TestJsonlExportLocalOnlyModeClearsStalePushEscalationMarker(t *testing.T) {
	cityDir := t.TempDir()
	binDir := t.TempDir()
	stateDir := t.TempDir()
	gcLog := filepath.Join(t.TempDir(), "gc.log")
	mailLog := filepath.Join(t.TempDir(), "gc-mail.log")
	archiveRepo := filepath.Join(cityDir, "archive")
	stateFile := filepath.Join(stateDir, "jsonl-export-state.json")

	initSeedArchive(t, archiveRepo, 3)
	writeMultiRecordDoltStub(t, binDir, 3)
	writeJsonlExportGCStub(t, binDir)

	if err := os.MkdirAll(filepath.Dir(stateFile), 0o755); err != nil {
		t.Fatalf("MkdirAll(state dir): %v", err)
	}
	priorState := `{"last_logged_mode":"local-only","last_logged_at":"2026-05-10T00:00:00Z","consecutive_push_failures":0,"pending_archive_push":true,"push_failure_escalated":true}` + "\n"
	if err := os.WriteFile(stateFile, []byte(priorState), 0o644); err != nil {
		t.Fatalf("WriteFile(state file): %v", err)
	}

	env := jsonlExportEnv(t, cityDir, binDir, stateDir, archiveRepo, gcLog, mailLog)
	delete(env, "GC_JSONL_MAX_PUSH_FAILURES")

	out, err := runScriptResult(t, filepath.Join(exampleDir(), "packs", "maintenance", "assets", "scripts", "jsonl-export.sh"), env)
	if err != nil {
		t.Fatalf("jsonl-export.sh: %v\n%s", err, out)
	}

	stateData, err := os.ReadFile(stateFile)
	if err != nil {
		t.Fatalf("ReadFile(state file): %v", err)
	}
	var state map[string]any
	if err := json.Unmarshal(stateData, &state); err != nil {
		t.Fatalf("Unmarshal(state file): %v\n%s", err, stateData)
	}
	if _, ok := state["push_failure_escalated"]; ok {
		t.Fatalf("push_failure_escalated must clear in local-only mode\nstate: %s", stateData)
	}
	if got, ok := state["pending_archive_push"].(bool); !ok || !got {
		t.Fatalf("pending_archive_push must remain true to track deferred push\nstate: %s", stateData)
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

	initSeedArchiveWithUnreachableRemote(t, archiveRepo)
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

func TestJsonlExportPushFailureEscalatesOncePerUnresolvedFailure(t *testing.T) {
	cityDir := t.TempDir()
	binDir := t.TempDir()
	stateDir := t.TempDir()
	gcLog := filepath.Join(t.TempDir(), "gc.log")
	mailLog := filepath.Join(t.TempDir(), "gc-mail.log")
	archiveRepo := filepath.Join(cityDir, "archive")
	stateFile := filepath.Join(stateDir, "jsonl-export-state.json")

	initSeedArchiveWithUnreachableRemote(t, archiveRepo)
	writeMultiRecordDoltStub(t, binDir, 5)
	writeJsonlExportGCStub(t, binDir)

	env := jsonlExportEnv(t, cityDir, binDir, stateDir, archiveRepo, gcLog, mailLog)
	env["GC_JSONL_MAX_PUSH_FAILURES"] = "1"

	script := filepath.Join(exampleDir(), "packs", "maintenance", "assets", "scripts", "jsonl-export.sh")
	runScript(t, script, env)
	runScript(t, script, env)
	runScript(t, script, env)

	mailData, err := os.ReadFile(mailLog)
	if err != nil {
		t.Fatalf("ReadFile(mail log): %v", err)
	}
	if got := strings.Count(string(mailData), "ESCALATION: JSONL push failed [HIGH]"); got != 1 {
		t.Fatalf("push failure must escalate once per unresolved failure, got %d mails:\n%s", got, mailData)
	}

	stateData, err := os.ReadFile(stateFile)
	if err != nil {
		t.Fatalf("ReadFile(state file): %v", err)
	}
	var state map[string]any
	if err := json.Unmarshal(stateData, &state); err != nil {
		t.Fatalf("Unmarshal(state file): %v\n%s", err, stateData)
	}
	if got, ok := state["push_failure_escalated"].(bool); !ok || !got {
		t.Fatalf("push_failure_escalated = %v, want true\nstate: %s", state["push_failure_escalated"], stateData)
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
