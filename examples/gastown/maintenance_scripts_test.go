package gastown_test

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/extmsg"
)

var rawDoltSQLCallRe = regexp.MustCompile(`(?m)(^|[^A-Za-z0-9_-])dolt(?:[ \t]+|[ \t]*\\[ \t]*\r?\n[ \t]*)+sql([ \t]|$)`)

var mailTableRe = regexp.MustCompile(`(?i)(?:FROM|UPDATE|INTO|JOIN|DELETE\s+FROM)\s+(?:\x60?[\w-]+\x60?\.)?\x60?mail\x60?\b`)

const (
	reaperCloseCleanupEdgeSQL   = "(d.type = 'parent-child' OR (d.type = 'tracks' AND JSON_UNQUOTE(JSON_EXTRACT(w.metadata, '$.\"gc.root_bead_id\"')) = COALESCE(d.depends_on_issue_id, d.depends_on_wisp_id, d.depends_on_external)))"
	reaperPurgeProtectEdgeSQL   = "d.type IN ('parent-child', 'tracks', 'blocks')"
	reaperCloseCleanupPredicate = "WISP_CLOSE_EDGE_PREDICATE="
)

func corePackDir() string {
	return filepath.Clean(filepath.Join(exampleDir(), "..", "..", "internal", "bootstrap", "packs", "core"))
}

func coreScriptPath(name string) string {
	return filepath.Join(corePackDir(), "assets", "scripts", name)
}

func scriptPath(path string) string {
	if filepath.IsAbs(path) {
		return path
	}
	return filepath.Join(exampleDir(), path)
}

func containsReaperCloseCleanupEdgePredicate(text string) bool {
	if containsSQLFragment(text, reaperCloseCleanupEdgeSQL) {
		return true
	}
	return strings.Contains(text, reaperCloseCleanupPredicate) &&
		strings.Contains(text, "$WISP_CLOSE_EDGE_PREDICATE")
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

	cmd := exec.Command(filepath.Join(corePackDir(), "doctor", "check-binaries", "run.sh"))
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

func TestOrphanSweepPreservesQualifiedRigAssignees(t *testing.T) {
	cityDir := t.TempDir()
	binDir := t.TempDir()
	gcLog := filepath.Join(t.TempDir(), "gc.log")

	writeExecutable(t, filepath.Join(binDir, "gc"), `#!/bin/sh
printf '%s\n' "$*" >> "$GC_CALL_LOG"
`+orphanSweepGateStubArm+`if [ "$1" = "--rig" ]; then
  shift 2
fi
case "$1" in
  mail)
    exit 0
    ;;
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
	      printf '{"sessions":[{"id":"orphan-sweep-test-keepalive","session_name":"orphan-sweep-test-keepalive","closed":false}],"summary":{},"filters":{},"schema_version":"1"}\n'
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

	script := coreScriptPath("orphan-sweep.sh")
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
	sessionList := `{"sessions":[{"id":"orphan-sweep-test-keepalive","session_name":"orphan-sweep-test-keepalive","closed":false}],"summary":{},"filters":{},"schema_version":"1"}`
	if sessionLive {
		sessionList = `{"sessions":[` +
			`{"id":"mc-bare-live","session_name":"thriva__devpipeline-backend-dev",` +
			`"alias":"backend_dev-1","agent_name":"thriva/devpipeline.backend_dev","closed":false}` +
			`],"summary":{},"filters":{},"schema_version":"1"}`
	}
	writeExecutable(t, filepath.Join(binDir, "gc"), `#!/bin/sh
printf '%s\n' "$*" >> "$GC_CALL_LOG"
`+orphanSweepGateStubArm+`if [ "$1" = "--rig" ]; then
  shift 2
fi
case "$1" in
  mail)
    exit 0
    ;;
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

	script := coreScriptPath("orphan-sweep.sh")
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

	script := coreScriptPath("orphan-sweep.sh")
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

// orphanSweepOperatorAssigneeGCStub fakes a city with one configured agent
// (gastown.deacon), no live sessions, and a single in-progress bead "ga-op"
// assigned to the given assignee. Used to prove the human-operator guard:
// assignee "human" must be preserved while a human-LIKE dead assignee
// (e.g. "humanoid") must still reset.
func orphanSweepOperatorAssigneeGCStub(t *testing.T, binDir, assignee string) {
	t.Helper()
	writeExecutable(t, filepath.Join(binDir, "gc"), `#!/bin/sh
printf '%s\n' "$*" >> "$GC_CALL_LOG"
`+orphanSweepGateStubArm+`if [ "$1" = "--rig" ]; then
  shift 2
fi
case "$1" in
  mail)
    exit 0
    ;;
  config)
    if [ "$2" = "explain" ]; then
      cat <<'EOF'
Agent: gastown.deacon
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
      printf '%s\n' '{"sessions":[{"id":"orphan-sweep-test-keepalive","session_name":"orphan-sweep-test-keepalive","closed":false}],"summary":{},"filters":{},"schema_version":"1"}'
      exit 0
    fi
    ;;
  bd)
    if [ "$2" = "show" ] && [ "$3" = "ga-op" ] && [ "$4" = "--json" ]; then
      cat <<'EOF'
[
  {"id":"ga-op","status":"in_progress","assignee":"`+assignee+`"}
]
EOF
      exit 0
    fi
    if [ "$2" = "list" ]; then
      cat <<'EOF'
[
  {"id":"ga-op","status":"in_progress","assignee":"`+assignee+`"}
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

// TestOrphanSweepPreservesHumanOperatorAssignee verifies that a bead assigned
// to the canonical human operator alias "human" is never treated as orphaned.
// The operator is not a configured agent and has no session, but operator
// action items are claims by a person, not by a dead agent — resetting them
// silently wipes the operator's claim.
func TestOrphanSweepPreservesHumanOperatorAssignee(t *testing.T) {
	cityDir := t.TempDir()
	binDir := t.TempDir()
	gcLog := filepath.Join(t.TempDir(), "gc.log")

	orphanSweepOperatorAssigneeGCStub(t, binDir, "human")

	env := map[string]string{
		"GC_CITY":      cityDir,
		"GC_CITY_PATH": cityDir,
		"GC_CALL_LOG":  gcLog,
		"PATH":         binDir + string(os.PathListSeparator) + os.Getenv("PATH"),
	}

	script := coreScriptPath("orphan-sweep.sh")
	cmd := exec.Command(script)
	cmd.Env = mergeTestEnv(env)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s failed: %v\n%s", filepath.Base(script), err, out)
	}
	if strings.Contains(string(out), "orphan-sweep: reset") {
		t.Fatalf("human-operator assignee was swept:\n%s", out)
	}

	logData, err := os.ReadFile(gcLog)
	if err != nil {
		t.Fatalf("ReadFile(gc log): %v", err)
	}
	if log := string(logData); strings.Contains(log, "bd release-if-current ga-op ") {
		t.Fatalf("human-operator bead was reset:\n%s", log)
	}
}

// TestOrphanSweepResetsHumanLikeDeadAssignee is the negative control for
// TestOrphanSweepPreservesHumanOperatorAssignee: an assignee that merely
// starts with "human" but matches no agent and no session is a genuine
// orphan and must still be reset. This proves the operator guard is an
// exact match and did not weaken the sweep.
func TestOrphanSweepResetsHumanLikeDeadAssignee(t *testing.T) {
	cityDir := t.TempDir()
	binDir := t.TempDir()
	gcLog := filepath.Join(t.TempDir(), "gc.log")

	orphanSweepOperatorAssigneeGCStub(t, binDir, "humanoid")

	env := map[string]string{
		"GC_CITY":      cityDir,
		"GC_CITY_PATH": cityDir,
		"GC_CALL_LOG":  gcLog,
		"PATH":         binDir + string(os.PathListSeparator) + os.Getenv("PATH"),
	}

	script := coreScriptPath("orphan-sweep.sh")
	cmd := exec.Command(script)
	cmd.Env = mergeTestEnv(env)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s failed: %v\n%s", filepath.Base(script), err, out)
	}
	if !strings.Contains(string(out), "orphan-sweep: reset 1 orphaned beads") {
		t.Fatalf("human-like dead assignee was not swept:\n%s", out)
	}

	logData, err := os.ReadFile(gcLog)
	if err != nil {
		t.Fatalf("ReadFile(gc log): %v", err)
	}
	if log := string(logData); !strings.Contains(log, "bd release-if-current ga-op humanoid") {
		t.Fatalf("orphan bead was not reset:\n%s", log)
	}
}

func TestOrphanSweepConfigShowFallbackPreservesQualifiedAssignees(t *testing.T) {
	cityDir := t.TempDir()
	binDir := t.TempDir()
	gcLog := filepath.Join(t.TempDir(), "gc.log")

	writeExecutable(t, filepath.Join(binDir, "gc"), `#!/bin/sh
printf '%s\n' "$*" >> "$GC_CALL_LOG"
`+orphanSweepGateStubArm+`if [ "$1" = "--rig" ]; then
  shift 2
fi
case "$1" in
  mail)
    exit 0
    ;;
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
	      printf '{"sessions":[{"id":"orphan-sweep-test-keepalive","session_name":"orphan-sweep-test-keepalive","closed":false}],"summary":{},"filters":{},"schema_version":"1"}\n'
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

	script := coreScriptPath("orphan-sweep.sh")
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
`+orphanSweepGateStubArm+`case "$1" in
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
        printf '{"sessions":[{"id":"orphan-sweep-test-keepalive","session_name":"orphan-sweep-test-keepalive","closed":false}],"summary":{},"filters":{},"schema_version":"1"}\n'
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

	script := coreScriptPath("orphan-sweep.sh")
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
`+orphanSweepGateStubArm+`if [ "$1" = "--rig" ]; then
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
          printf '{"sessions":[{"id":"orphan-sweep-test-keepalive","session_name":"orphan-sweep-test-keepalive","closed":false}],"summary":{},"filters":{},"schema_version":"1"}\n'
        else
          cat <<'EOF'
{"sessions":[
  {"id":"mc-rig-live","session_name":"project__worker-gc-race","alias":"project/worker-1","agent_name":"project/worker","closed":false}
],"summary":{},"filters":{},"schema_version":"1"}
EOF
        fi
        exit 0
      fi
      printf '{"sessions":[{"id":"orphan-sweep-test-keepalive","session_name":"orphan-sweep-test-keepalive","closed":false}],"summary":{},"filters":{},"schema_version":"1"}\n'
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

	script := coreScriptPath("orphan-sweep.sh")
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
`+orphanSweepGateStubArm+`case "$1" in
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
      printf '{"sessions":[{"id":"orphan-sweep-test-keepalive","session_name":"orphan-sweep-test-keepalive","closed":false}],"summary":{},"filters":{},"schema_version":"1"}\n'
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

	script := coreScriptPath("orphan-sweep.sh")
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
`+orphanSweepGateStubArm+`case "$1" in
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
      printf '{"sessions":[{"id":"orphan-sweep-test-keepalive","session_name":"orphan-sweep-test-keepalive","closed":false}],"summary":{},"filters":{},"schema_version":"1"}\n'
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

	script := coreScriptPath("orphan-sweep.sh")
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
`+orphanSweepGateStubArm+`case "$1" in
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
      printf '{"sessions":[{"id":"orphan-sweep-test-keepalive","session_name":"orphan-sweep-test-keepalive","closed":false}],"summary":{},"filters":{},"schema_version":"1"}\n'
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

	script := coreScriptPath("orphan-sweep.sh")
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
`+orphanSweepGateStubArm+`case "$1" in
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
      printf '{"sessions":[{"id":"orphan-sweep-test-keepalive","session_name":"orphan-sweep-test-keepalive","closed":false}],"summary":{},"filters":{},"schema_version":"1"}\n'
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

	script := coreScriptPath("orphan-sweep.sh")
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

// TestOrphanSweepTreatsPoolSessionNameSelfProbeAsUnverifiable verifies that a
// pool-seat assignee whose only probe candidate is its own session name is
// treated as unverifiable for BOTH tmux-safe session-name encodings.
// agent.SanitizeQualifiedNameForSession encodes "/" as "--" (rig-scope:
// "beads/deployer" -> "beads--deployer") and "." as "__" (pack-qualified
// city-scope: "pack-author.pack-author" -> "pack-author__pack-author").
// `gc bd show <session name>` cannot resolve either shape to a bead, so both
// are failed probes, not dead seats.
//
// The self-probe fail-safe landed recognizing only "--" (#5841), which left
// every "__" seat resetting whenever the liveness snapshot momentarily lacked
// its row: measured as 7 resets over 2026-09-10..12, every one a "__" seat and
// none a "--" seat, while 3 "__" seats were live (ga-dei7xx).
func TestOrphanSweepTreatsPoolSessionNameSelfProbeAsUnverifiable(t *testing.T) {
	tests := []struct {
		name     string
		workID   string
		assignee string
	}{
		{
			// Control: already protected by the "--" arm.
			name:     "rig scope double dash",
			workID:   "ga-rig-pool-self-probe",
			assignee: "beads--deployer-pool",
		},
		{
			name:     "city scope double underscore",
			workID:   "ga-city-pool-self-probe",
			assignee: "pack-author__pack-author-pool",
		},
		{
			// Slot-numbered seat: the "-<slot>" sits between the sanitized
			// agent and the "-pool" suffix (live example: bd__dog-1-pool).
			name:     "city scope numbered slot",
			workID:   "ga-city-slot-pool-self-probe",
			assignee: "bd__dog-1-pool",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cityDir := t.TempDir()
			binDir := t.TempDir()
			gcLog := filepath.Join(t.TempDir(), "gc.log")

			// The session list carries an unrelated keepalive row but not the
			// pool seat: the transient window in which a cycling seat is
			// absent from the snapshot, which drops the sweep through to the
			// self-probe fail-safe. `bd show <assignee>` is left unhandled so
			// it exits non-zero, exactly as the real binary does when handed a
			// session name instead of a bead id. Both seats reconstruct from
			// these configured agents, which is what marks them as seats
			// rather than ephemeral sessions.
			writeExecutable(t, filepath.Join(binDir, "gc"), fmt.Sprintf(`#!/bin/sh
printf '%%s\n' "$*" >> "$GC_CALL_LOG"
`+orphanSweepGateStubArm+`case "$1" in
  config)
    if [ "$2" = "explain" ]; then
      cat <<'EOF'
Agent: beads/deployer
  source: pack
Agent: pack-author.pack-author
  source: pack
Agent: bd.dog
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
      printf '{"sessions":[{"id":"orphan-sweep-test-keepalive","session_name":"orphan-sweep-test-keepalive","closed":false}],"summary":{},"filters":{},"schema_version":"1"}\n'
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
    if [ "$2" = "release-if-current" ]; then
      printf 'released\n'
      exit 0
    fi
    if [ "$2" = "update" ]; then
      exit 0
    fi
    ;;
esac
exit 1
`, tt.workID, tt.assignee, tt.workID, tt.workID, tt.assignee))

			env := map[string]string{
				"GC_CITY":      cityDir,
				"GC_CITY_PATH": cityDir,
				"GC_CALL_LOG":  gcLog,
				"PATH":         binDir + string(os.PathListSeparator) + os.Getenv("PATH"),
			}

			script := coreScriptPath("orphan-sweep.sh")
			cmd := exec.Command(script)
			cmd.Env = mergeTestEnv(env)
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("%s failed: %v\n%s", filepath.Base(script), err, out)
			}
			if !strings.Contains(string(out), "orphan-sweep: reset 0 orphaned beads, skipped 1 unverifiable") {
				t.Fatalf("session-name self-probe was not treated as unverifiable:\n%s", out)
			}

			logData, err := os.ReadFile(gcLog)
			if err != nil {
				t.Fatalf("ReadFile(gc log): %v", err)
			}
			log := string(logData)
			if !strings.Contains(log, "bd show "+tt.assignee+" --json") {
				t.Fatalf("session-name self-probe was never attempted:\n%s", log)
			}
			if strings.Contains(log, "bd release-if-current "+tt.workID+" ") {
				t.Fatalf("live pool seat %q lost its claim on %s:\n%s", tt.assignee, tt.workID, log)
			}
		})
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
`+orphanSweepGateStubArm+`case "$1" in
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
      printf '{"sessions":[{"id":"orphan-sweep-test-keepalive","session_name":"orphan-sweep-test-keepalive","closed":false}],"summary":{},"filters":{},"schema_version":"1"}\n'
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

			script := coreScriptPath("orphan-sweep.sh")
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
`+orphanSweepGateStubArm+`if [ "$1" = "--rig" ]; then
  rig="$2"
  shift 2
else
  rig=""
fi
case "$1" in
  mail)
    exit 0
    ;;
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
      printf '{"sessions":[{"id":"orphan-sweep-test-keepalive","session_name":"orphan-sweep-test-keepalive","closed":false}],"summary":{},"filters":{},"schema_version":"1"}\n'
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

	script := coreScriptPath("orphan-sweep.sh")
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
`+orphanSweepGateStubArm+`if [ "$1" = "--rig" ]; then
  rig="$2"
  shift 2
else
  rig=""
fi
case "$1" in
  mail)
    exit 0
    ;;
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
      printf '{"sessions":[{"id":"orphan-sweep-test-keepalive","session_name":"orphan-sweep-test-keepalive","closed":false}],"summary":{},"filters":{},"schema_version":"1"}\n'
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

	script := coreScriptPath("orphan-sweep.sh")
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
`+orphanSweepGateStubArm+`if [ "$1" = "--rig" ]; then
  rig="$2"
  shift 2
else
  rig=""
fi
case "$1" in
  mail)
    exit 0
    ;;
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
      printf '{"sessions":[{"id":"orphan-sweep-test-keepalive","session_name":"orphan-sweep-test-keepalive","closed":false}],"summary":{},"filters":{},"schema_version":"1"}\n'
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

	script := coreScriptPath("orphan-sweep.sh")
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

			script := coreScriptPath("orphan-sweep.sh")
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

func TestOrphanSweepPreservesLivePoolOwnerInOversizedSessionSnapshot(t *testing.T) {
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
	writeStrictOrphanSweepGCStub(t, filepath.Join(binDir, "gc"))

	liveOwner := "project-alpha__gastown-refinery-gc-live6714"
	sessionNames := []string{liveOwner}
	for i := 0; i < 400; i++ {
		sessionNames = append(sessionNames, fmt.Sprintf("project-alpha__gastown-worker-%04d-%080d", i, i))
	}
	sessionsJSON := orphanSweepSessionListJSON(t, sessionNames...)
	protectedID := "gc-wisp-protected-live-6714"
	orphanID := "gc-wisp-orphan-live-6714"
	orphanAssignee := "project-alpha__gastown-retired-gc-dead6714"
	rigJSON := orphanSweepProtectedWispBeadsJSON(t, protectedID, liveOwner, orphanID, orphanAssignee)
	env := orphanSweepCleanroomEnv(t, root, binDir, gcLog, orphanSweepCleanroomEnvConfig{
		hqJSON:             "[]",
		rigJSON:            rigJSON,
		hqSessionsJSON:     sessionsJSON,
		rigSessionsJSON:    sessionsJSON,
		configuredIdentity: "project-alpha/gastown.refinery",
		orphanID:           orphanID,
		orphanAssignee:     orphanAssignee,
	})

	script := coreScriptPath("orphan-sweep.sh")
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
	if strings.Contains(log, "bd release-if-current "+protectedID+" ") {
		t.Fatalf("oversized snapshot caused live pool owner to be reset\n%s", orphanSweepFailureContext(out, gcLog))
	}
	if !strings.Contains(log, "bd release-if-current "+orphanID+" "+orphanAssignee) {
		t.Fatalf("dead negative-control owner was not reset\n%s", orphanSweepFailureContext(out, gcLog))
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
if [ "$*" = "agent is-foreign --json -- $ORPHAN_SWEEP_ORPHAN_ASSIGNEE" ]; then
  echo '{"schema_version":"1","ok":true,"verdict":"local","reason":"session_record","roster_source":"stub"}'
  exit 0
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
if [ "$*" = "bd update $ORPHAN_SWEEP_ORPHAN_ID --append-notes orphan-sweep: reset from assignee $ORPHAN_SWEEP_ORPHAN_ASSIGNEE -- no live session matched" ]; then
  exit 0
fi
case "$*" in
  "mail send human "*) exit 0 ;;
esac
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

// TestOrphanSweepSkipsRigWhenSessionListSucceedsButReportsZeroSessions covers
// ga-7p4aab fix 1: an exit-0 session list that parses to zero rows must be
// treated as a liveness-evidence failure, not success. Before the fix,
// append_session_list only gated on exit status, so a rig whose session list
// legitimately (or transiently) reports no rows had every in-progress bead in
// that rig staged with no liveness evidence at all -- indistinguishable from a
// rig with no live agents. HQ and other rigs must be unaffected.
func TestOrphanSweepSkipsRigWhenSessionListSucceedsButReportsZeroSessions(t *testing.T) {
	cityDir := t.TempDir()
	binDir := t.TempDir()
	gcLog := filepath.Join(t.TempDir(), "gc.log")

	writeExecutable(t, filepath.Join(binDir, "gc"), `#!/bin/sh
printf '%s\n' "$*" >> "$GC_CALL_LOG"
`+orphanSweepGateStubArm+`if [ "$1" = "--rig" ]; then
  rig="$2"
  shift 2
else
  rig=""
fi
case "$1" in
  mail)
    exit 0
    ;;
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
      printf '{"rigs":[{"name":"hq","hq":true},{"name":"empty","hq":false},{"name":"healthy","hq":false}]}\n'
      exit 0
    fi
    ;;
  session)
    if [ "$2" = "list" ] && [ "$3" = "--json" ]; then
      if [ "$rig" = "empty" ]; then
        printf '{"sessions":[],"summary":{},"filters":{},"schema_version":"1"}\n'
        exit 0
      fi
      printf '{"sessions":[{"id":"orphan-sweep-test-keepalive","session_name":"orphan-sweep-test-keepalive","closed":false}],"summary":{},"filters":{},"schema_version":"1"}\n'
      exit 0
    fi
    ;;
  bd)
	    if [ "$2" = "list" ]; then
	      if [ "$3" = "--rig" ] && [ "$4" = "empty" ]; then
	        printf '[]\n'
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

	script := coreScriptPath("orphan-sweep.sh")
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
			t.Fatalf("expected %s to be reset after unrelated rig reported zero sessions:\n%s", reset, log)
		}
	}
	if strings.Contains(log, "bd list --rig empty --status=in_progress") {
		t.Fatalf("rig whose session list succeeded but reported zero sessions was queried for beads instead of being skipped like a hard session-list failure:\n%s", log)
	}
}

// TestOrphanSweepPreservesPoolSeatWithOnlySessionNameMetadataWhenLive covers
// ga-7p4aab fix 2 (the incident's actual shape): a pool seat claims work with
// its session NAME as the assignee (e.g. "beads--deployer-pool"), which never
// matches is_known_agent's agent/pool/dot-strip branches. When the session
// list evidence for that identity is unavailable, the only remaining source
// of truth is the work bead's own gc.session_name metadata resolving directly
// via `gc bd show`. Before the fix, session_bead_candidates never read that
// key, so the seat had no path to prove liveness once list-based evidence
// missed it.
func TestOrphanSweepPreservesPoolSeatWithOnlySessionNameMetadataWhenLive(t *testing.T) {
	cityDir := t.TempDir()
	binDir := t.TempDir()
	gcLog := filepath.Join(t.TempDir(), "gc.log")

	writeExecutable(t, filepath.Join(binDir, "gc"), `#!/bin/sh
printf '%s\n' "$*" >> "$GC_CALL_LOG"
`+orphanSweepGateStubArm+`case "$1" in
  config)
    if [ "$2" = "explain" ]; then
      cat <<'EOF'
Agent: deployer
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
      printf '{"sessions":[{"id":"orphan-sweep-test-keepalive","session_name":"orphan-sweep-test-keepalive","closed":false}],"summary":{},"filters":{},"schema_version":"1"}\n'
      exit 0
    fi
    ;;
  bd)
    if [ "$2" = "list" ]; then
      cat <<'EOF'
[
  {"id":"ga-pool-seat-orphan","status":"in_progress","assignee":"deployer-pool-3"}
]
EOF
      exit 0
    fi
    if [ "$2" = "show" ] && [ "$3" = "ga-pool-seat-orphan" ] && [ "$4" = "--json" ]; then
      cat <<'EOF'
[
  {"id":"ga-pool-seat-orphan","status":"in_progress","assignee":"deployer-pool-3","metadata":{"gc.session_name":"beads--deployer-pool"}}
]
EOF
      exit 0
    fi
    if [ "$2" = "show" ] && [ "$3" = "beads--deployer-pool" ] && [ "$4" = "--json" ]; then
      cat <<'EOF'
[
  {"id":"beads--deployer-pool","status":"open","issue_type":"session","metadata":{"state":"active","session_name":"beads--deployer-pool"}}
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

	script := coreScriptPath("orphan-sweep.sh")
	cmd := exec.Command(script)
	cmd.Env = mergeTestEnv(env)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s failed: %v\n%s", filepath.Base(script), err, out)
	}
	if strings.Contains(string(out), "orphan-sweep: reset") {
		t.Fatalf("pool seat with live session reachable only via gc.session_name metadata was reset:\n%s", out)
	}

	logData, err := os.ReadFile(gcLog)
	if err != nil {
		t.Fatalf("ReadFile(gc log): %v", err)
	}
	log := string(logData)
	if !strings.Contains(log, "bd show beads--deployer-pool --json") {
		t.Fatalf("gc.session_name metadata candidate was never probed:\n%s", log)
	}
	if strings.Contains(log, "bd release-if-current ga-pool-seat-orphan ") {
		t.Fatalf("pool seat bead was reset despite a live session found via gc.session_name:\n%s", log)
	}
}

// TestOrphanSweepTreatsUnresolvableDoubleDashAssigneeAsUnverifiable covers
// ga-7p4aab fix 3: when a work bead's only session-bead candidate is the
// assignee itself and that assignee is a session-name shape (not a
// resolvable bead id), a failed probe must be classified UNVERIFIABLE, not
// VERIFIABLE-DEAD. Before the fix, session_probe_failure_is_unverifiable
// returned "dead" for any self-probe failure that wasn't mc-*-prefixed, so a
// probe that had nothing resolvable to ask about was read as proof of death.
func TestOrphanSweepTreatsUnresolvableDoubleDashAssigneeAsUnverifiable(t *testing.T) {
	cityDir := t.TempDir()
	binDir := t.TempDir()
	gcLog := filepath.Join(t.TempDir(), "gc.log")

	writeExecutable(t, filepath.Join(binDir, "gc"), `#!/bin/sh
printf '%s\n' "$*" >> "$GC_CALL_LOG"
`+orphanSweepGateStubArm+`case "$1" in
  config)
    if [ "$2" = "explain" ]; then
      cat <<'EOF'
Agent: polecat
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
      printf '{"sessions":[{"id":"orphan-sweep-test-keepalive","session_name":"orphan-sweep-test-keepalive","closed":false}],"summary":{},"filters":{},"schema_version":"1"}\n'
      exit 0
    fi
    ;;
  bd)
    if [ "$2" = "list" ]; then
      cat <<'EOF'
[
  {"id":"ga-unresolvable-orphan","status":"in_progress","assignee":"rig--polecat"}
]
EOF
      exit 0
    fi
    if [ "$2" = "show" ] && [ "$3" = "ga-unresolvable-orphan" ] && [ "$4" = "--json" ]; then
      cat <<'EOF'
[
  {"id":"ga-unresolvable-orphan","status":"in_progress","assignee":"rig--polecat"}
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

	script := coreScriptPath("orphan-sweep.sh")
	cmd := exec.Command(script)
	cmd.Env = mergeTestEnv(env)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s failed: %v\n%s", filepath.Base(script), err, out)
	}
	if !strings.Contains(string(out), "skipped 1 unverifiable") {
		t.Fatalf("unresolvable double-dash assignee was not counted as unverifiable:\n%s", out)
	}
	if strings.Contains(string(out), "orphan-sweep: reset 1") {
		t.Fatalf("unresolvable double-dash assignee was reset instead of treated as unverifiable:\n%s", out)
	}

	logData, err := os.ReadFile(gcLog)
	if err != nil {
		t.Fatalf("ReadFile(gc log): %v", err)
	}
	log := string(logData)
	if strings.Contains(log, "bd release-if-current ga-unresolvable-orphan ") {
		t.Fatalf("unresolvable double-dash assignee was reset:\n%s", log)
	}
}

// TestOrphanSweepRecordsCauseNoteOnEveryReset covers ga-7p4aab fix 4 (the
// bead's ask 1): every reset must leave a durable, one-line trace on the work
// bead naming orphan-sweep as the actor and the assignee it was taken from.
// Before the fix, reset_orphan_if_current called only gc bd
// release-if-current, which writes no note, no mail, and no gc metadata --
// bd's events.actor was the only trace anywhere.
func TestOrphanSweepRecordsCauseNoteOnEveryReset(t *testing.T) {
	cityDir := t.TempDir()
	binDir := t.TempDir()
	gcLog := filepath.Join(t.TempDir(), "gc.log")

	writeExecutable(t, filepath.Join(binDir, "gc"), `#!/bin/sh
printf '%s\n' "$*" >> "$GC_CALL_LOG"
`+orphanSweepGateStubArm+`case "$1" in
  mail)
    exit 0
    ;;
  config)
    if [ "$2" = "explain" ]; then
      cat <<'EOF'
Agent: deacon
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
      printf '{"sessions":[{"id":"orphan-sweep-test-keepalive","session_name":"orphan-sweep-test-keepalive","closed":false}],"summary":{},"filters":{},"schema_version":"1"}\n'
      exit 0
    fi
    ;;
  bd)
    if [ "$2" = "list" ]; then
      cat <<'EOF'
[
  {"id":"ga-genuinely-dead","status":"in_progress","assignee":"gastown.longgone"}
]
EOF
      exit 0
    fi
    if [ "$2" = "show" ] && [ "$3" = "ga-genuinely-dead" ] && [ "$4" = "--json" ]; then
      cat <<'EOF'
[
  {"id":"ga-genuinely-dead","status":"in_progress","assignee":"gastown.longgone"}
]
EOF
      exit 0
    fi
    if [ "$2" = "release-if-current" ]; then
      printf 'released\n'
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

	script := coreScriptPath("orphan-sweep.sh")
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
	if !strings.Contains(log, "bd release-if-current ga-genuinely-dead gastown.longgone") {
		t.Fatalf("dead-assigned bead was not reset:\n%s", log)
	}
	if !strings.Contains(log, "bd update ga-genuinely-dead --append-notes") {
		t.Fatalf("reset did not append a cause note to the bead via bd update --append-notes:\n%s", log)
	}
	if !strings.Contains(log, "orphan-sweep") {
		t.Fatalf("cause note did not name orphan-sweep as the actor:\n%s", log)
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
  *"FROM workflow_issue_root_candidates_page root"*)
    printf 'id,verdict\n'
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
	writeMaintenanceGCStub(t, filepath.Join(binDir, "gc"), `#!/bin/sh
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

	runScript(t, coreScriptPath("reaper.sh"), env)

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
  *"FROM workflow_issue_root_candidates_page root"*)
    printf 'id,verdict\n'
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
	writeMaintenanceGCStub(t, filepath.Join(binDir, "gc"), `#!/bin/sh
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

	runScript(t, coreScriptPath("reaper.sh"), env)

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
  *"SELECT COUNT(*) FROM workflow_wisp_root_candidates"[!_]*|*"SELECT COUNT(*) FROM workflow_issue_root_candidates"[!_]*)
    # No workflow roots: each candidate costs the root census a page query.
    printf 'COUNT(*)\n0\n'
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
	writeMaintenanceGCStub(t, filepath.Join(binDir, "gc"), `#!/bin/sh
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

	runScript(t, coreScriptPath("reaper.sh"), env)

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
	writeMaintenanceGCStub(t, filepath.Join(binDir, "gc"), `#!/bin/sh
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

	runScript(t, coreScriptPath("reaper.sh"), env)

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
	writeMaintenanceGCStub(t, filepath.Join(binDir, "gc"), `#!/bin/sh
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

	runScript(t, coreScriptPath("reaper.sh"), env)

	gcData, err := os.ReadFile(gcLog)
	if err != nil {
		t.Fatalf("ReadFile(gc log): %v", err)
	}
	gcLogText := string(gcData)
	if !strings.Contains(gcLogText, "mail_wisps:0") {
		t.Fatalf("reaper summary missing mail_wisps field when wisp counts are zero:\n%s", gcLogText)
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
			script: coreScriptPath("reaper.sh"),
			env: map[string]string{
				"GC_REAPER_DRY_RUN": "1",
			},
		},
		{
			name:   "jsonl export",
			script: coreScriptPath("jsonl-export.sh"),
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
  *"SELECT * FROM "[!\(]*)
    printf '{"id":"ga-1"}\n'
    ;;
esac
exit 0
`)
			writeMaintenanceGCStub(t, filepath.Join(binDir, "gc"), `#!/bin/sh
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

			runScript(t, scriptPath(tt.script), env)

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

func TestReaperScriptSQLReflectsCurrentSchema(t *testing.T) {
	path := coreScriptPath("reaper.sh")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%s): %v", path, err)
	}
	script := string(data)

	for _, stalePhrase := range []string{
		"Total open wisps (for alert threshold)",
		"If total open wisps",
		"Open wisp count exceeding",
	} {
		if strings.Contains(script, stalePhrase) {
			t.Errorf("reaper script still describes total-open-wisp alerting with %q; reaper alerts on stale non-message open wisps", stalePhrase)
		}
	}
	for _, required := range []string{
		"issue_type NOT IN ('message')",
		"created_at < DATE_SUB(UTC_TIMESTAMP(), INTERVAL $MAX_AGE_H HOUR)",
	} {
		if !strings.Contains(script, required) {
			t.Errorf("reaper script is missing stale-only query fragment %q", required)
		}
	}

	if strings.Contains(script, "parent_id") {
		t.Errorf("reaper script references parent_id (column does not exist in wisps):\n%s", script)
	}
	if strings.Contains(script, "depends_on_id") && !strings.Contains(script, "depends_on_issue_id") && !strings.Contains(script, "depends_on_wisp_id") {
		t.Errorf("reaper script references removed depends_on_id column; schema uses typed split columns:\n%s", script)
	}
	if strings.Contains(script, "LEFT JOIN wisps parent ON") {
		t.Errorf("reaper script still has the broken parent self-join:\n%s", script)
	}
	if mailTableRe.MatchString(script) {
		t.Errorf("reaper script treats `mail` as a SQL table; mail messages are beads with Type=message:\n%s", script)
	}
	if !containsReaperCloseCleanupEdgePredicate(script) {
		t.Fatalf("reaper script does not include the close ownership predicate:\n%s", script)
	}
	// The closed-wisp purge is bd purge's job, including the live-dependent
	// protection; the script carries no SQL mutation of its own.
	if !strings.Contains(script, `purge_args=(purge "$PURGE_PLANE_FLAG" --older-than "$PURGE_AGE" --json)`) {
		t.Fatalf("reaper script does not purge closed wisps through bd purge over the wisps plane:\n%s", script)
	}
	for _, forbidden := range []string{"DELETE FROM", "UPDATE `", "DOLT_COMMIT", "dolt_sql", "dolt --host"} {
		if strings.Contains(script, forbidden) {
			t.Errorf("reaper script contains %q; mutations must go through bd verbs", forbidden)
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

	scriptPath := coreScriptPath("reaper.sh")
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
	bdLog := filepath.Join(t.TempDir(), "bd.log")

	writeMaintenanceDoltStub(t, filepath.Join(binDir, "dolt"))
	writeMaintenanceGCStub(t, filepath.Join(binDir, "gc"), `#!/bin/sh
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
		// One candidate root of each kind, so the census sends the
		// descendant walk this test inspects.
		"DOLT_WORKFLOW_ROOT_CANDIDATES": "1",
		"BD_CALL_LOG":                   bdLog,
		"PATH":                          binDir + string(os.PathListSeparator) + os.Getenv("PATH"),
		// No GC_REAPER_DRY_RUN — the purge runs for real.
	}

	runScript(t, coreScriptPath("reaper.sh"), env)

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
	// Every query the reaper sends through bd sql is a read: mutations are
	// bd verbs that own their commits.
	for _, forbidden := range []string{"DELETE FROM", "UPDATE `", "DOLT_COMMIT", "USE `"} {
		if strings.Contains(log, forbidden) {
			t.Errorf("reaper sent %q through bd sql; mutations must be bd verbs:\n%s", forbidden, log)
		}
	}
	bdData, err := os.ReadFile(bdLog)
	if err != nil {
		t.Fatalf("ReadFile(bd log): %v", err)
	}
	if !strings.Contains(string(bdData), "args=purge --wisps-plane --older-than 168h --json --force --limit 500") {
		t.Errorf("reaper did not purge closed wisps through bd purge over the wisps plane:\n%s", bdData)
	}

	gcData, err := os.ReadFile(gcLog)
	if err != nil {
		t.Fatalf("ReadFile(gc log): %v", err)
	}
	if strings.Contains(string(gcData), "mail:") {
		t.Errorf("reaper MAINTENANCE_DONE still reports removed mail cleanup:\n%s", gcData)
	}
}

func TestReaperSplitSchemaQueriesUseSplitColumns(t *testing.T) {
	cityDir := t.TempDir()
	binDir := t.TempDir()
	doltLog := filepath.Join(t.TempDir(), "dolt-args.log")
	gcLog := filepath.Join(t.TempDir(), "gc.log")

	writeMaintenanceDoltStub(t, filepath.Join(binDir, "dolt"))
	writeMaintenanceGCStub(t, filepath.Join(binDir, "gc"), `#!/bin/sh
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
		// One candidate root of each kind, so the census sends the
		// descendant walk this test inspects.
		"DOLT_WORKFLOW_ROOT_CANDIDATES": "1",
		"PATH":                          binDir + string(os.PathListSeparator) + os.Getenv("PATH"),
	}

	runScript(t, coreScriptPath("reaper.sh"), env)

	logData, err := os.ReadFile(doltLog)
	if err != nil {
		t.Fatalf("ReadFile(dolt log): %v", err)
	}
	log := string(logData)

	if !strings.Contains(log, "`beads`.wisp_dependencies child_dep") {
		t.Errorf("reaper split-schema log missing the wisp dependency traversal:\n%s", log)
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
	writeFreshBackupState(t, cityDir)
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
	writeMaintenanceGCStub(t, filepath.Join(binDir, "gc"), `#!/bin/sh
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

	runScript(t, coreScriptPath("reaper.sh"), env)

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
	wantRoute := "bd --city " + canonicalCityDir + " prune --pattern gm-* --older-than 720h --force --json"
	if !strings.Contains(string(gcData), wantRoute) {
		t.Fatalf("reaper did not route session pruning through the explicit city scope %q:\n%s", wantRoute, gcData)
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
	writeFreshBackupState(t, cityDir)
	binDir := t.TempDir()
	doltLog := filepath.Join(t.TempDir(), "dolt-args.log")
	bdLog := filepath.Join(t.TempDir(), "bd.log")
	gcLog := filepath.Join(t.TempDir(), "gc.log")

	writeMaintenanceDoltStub(t, filepath.Join(binDir, "dolt"))
	writeMaintenanceBdStub(t, filepath.Join(binDir, "bd"))
	writeMaintenanceGCStub(t, filepath.Join(binDir, "gc"), `#!/bin/sh
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

	runScript(t, coreScriptPath("reaper.sh"), env)

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
	writeMaintenanceGCStub(t, filepath.Join(binDir, "gc"), `#!/bin/sh
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

	runScript(t, coreScriptPath("reaper.sh"), env)

	gcData, err := os.ReadFile(gcLog)
	if err != nil {
		t.Fatalf("ReadFile(gc log): %v", err)
	}
	gcLogText := string(gcData)
	if !strings.Contains(gcLogText, "mail send human -s ESCALATION: Reaper anomalies detected [MEDIUM]") {
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
	writeFreshBackupState(t, cityDir)
	binDir := t.TempDir()
	doltLog := filepath.Join(t.TempDir(), "dolt-args.log")
	bdLog := filepath.Join(t.TempDir(), "bd.log")
	gcLog := filepath.Join(t.TempDir(), "gc.log")

	writeMaintenanceDoltStub(t, filepath.Join(binDir, "dolt"))
	writeMaintenanceBdStub(t, filepath.Join(binDir, "bd"))
	writeMaintenanceGCStub(t, filepath.Join(binDir, "gc"), `#!/bin/sh
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

	runScript(t, coreScriptPath("reaper.sh"), env)

	bdData, err := os.ReadFile(bdLog)
	if err != nil {
		t.Fatalf("ReadFile(bd log): %v", err)
	}
	bdLogText := string(bdData)
	wantArgs := "args=prune --pattern gm-* --older-than 24h --dry-run --json"
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
	writeFreshBackupState(t, cityDir)
	binDir := t.TempDir()
	doltLog := filepath.Join(t.TempDir(), "dolt-args.log")
	bdLog := filepath.Join(t.TempDir(), "bd.log")
	gcLog := filepath.Join(t.TempDir(), "gc.log")

	writeMaintenanceDoltStub(t, filepath.Join(binDir, "dolt"))
	writeMaintenanceBdStub(t, filepath.Join(binDir, "bd"))
	writeMaintenanceGCStub(t, filepath.Join(binDir, "gc"), `#!/bin/sh
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

	runScript(t, coreScriptPath("reaper.sh"), env)

	gcData, err := os.ReadFile(gcLog)
	if err != nil {
		t.Fatalf("ReadFile(gc log): %v", err)
	}
	gcLogText := string(gcData)
	if !strings.Contains(gcLogText, "mail send human -s ESCALATION: Reaper anomalies detected [MEDIUM]") {
		t.Fatalf("reaper did not send escalation mail for session-prune anomaly:\n%s", gcLogText)
	}
	if !strings.Contains(gcLogText, "gm: 1500 closed session beads pruned (pattern=gm-* threshold: 1000)") {
		t.Fatalf("reaper escalation did not include session-prune anomaly:\n%s", gcLogText)
	}
	if !strings.Contains(gcLogText, "sessions-pruned:1500") {
		t.Fatalf("reaper summary did not include anomalous session-prune count:\n%s", gcLogText)
	}
}

func TestReaperSessionPruneMissingBdDegradesToZero(t *testing.T) {
	cityDir := t.TempDir()
	writeCityBeadsMetadata(t, cityDir, "beads")
	writeFreshBackupState(t, cityDir)
	binDir := t.TempDir()
	doltLog := filepath.Join(t.TempDir(), "dolt-args.log")
	gcLog := filepath.Join(t.TempDir(), "gc.log")

	writeMaintenanceDoltStub(t, filepath.Join(binDir, "dolt"))
	writeMaintenanceGCStub(t, filepath.Join(binDir, "gc"), `#!/bin/sh
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

	runScript(t, coreScriptPath("reaper.sh"), env)

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

func TestReaperDryRunReportsWouldCloseStaleWisps(t *testing.T) {
	cityDir := t.TempDir()
	binDir := t.TempDir()
	doltLog := filepath.Join(t.TempDir(), "dolt-args.log")
	gcLog := filepath.Join(t.TempDir(), "gc.log")
	bdLog := filepath.Join(t.TempDir(), "bd.log")

	writeExecutable(t, filepath.Join(binDir, "dolt"), `#!/bin/sh
printf '%s\n' "$*" >> "$DOLT_ARGS_LOG"
case "$*" in
  *"SHOW TABLES FROM"*"LIKE 'wisps'"*)
    printf 'Tables_in_db\nwisps\n'
    ;;
  *"SHOW DATABASES"*)
    printf 'Database\nbeads\n'
    ;;
  *"SELECT DISTINCT w.id"*)
    printf 'id,owner_id,depth,state,mode\nbd-wisp-a,,0,ok,bare\nbd-wisp-b,,0,ok,force\n'
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
	writeMaintenanceGCStub(t, filepath.Join(binDir, "gc"), `#!/bin/sh
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
		"BD_CALL_LOG":       bdLog,
		"PATH":              binDir + string(os.PathListSeparator) + os.Getenv("PATH"),
	}

	runScript(t, coreScriptPath("reaper.sh"), env)

	bdData, _ := os.ReadFile(bdLog)
	for _, verb := range []string{"args=close", "args=update", "--force"} {
		if strings.Contains(string(bdData), verb) {
			t.Fatalf("dry-run mutated through bd (%q):\n%s", verb, bdData)
		}
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
	bdLog := filepath.Join(t.TempDir(), "bd.log")

	writeExecutable(t, filepath.Join(binDir, "dolt"), `#!/bin/sh
printf '%s\n' "$*" >> "$DOLT_ARGS_LOG"
case "$*" in
  *"SELECT DISTINCT w.id"*)
    if [ -f "$CLOSED_FLAG" ]; then
      printf 'id,owner_id,depth,state,mode\n'
    else
      : > "$CLOSED_FLAG"
      printf 'id,owner_id,depth,state,mode\nbd-wisp-stale,,0,ok,bare\n'
    fi
    ;;
  *"SELECT COUNT(*) FROM"*"wisps"*"issue_type NOT IN ('message')"*"created_at <"*)
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
	writeMaintenanceGCStub(t, filepath.Join(binDir, "gc"), `#!/bin/sh
printf '%s\n' "$*" >> "$GC_CALL_LOG"
exit 0
`)

	env := map[string]string{
		"DOLT_ARGS_LOG":  doltLog,
		"GC_CALL_LOG":    gcLog,
		"BD_CALL_LOG":    bdLog,
		"BD_PURGE_COUNT": "1",
		"CLOSED_FLAG":    filepath.Join(t.TempDir(), "closed"),
		"GC_CITY":        cityDir,
		"GC_CITY_PATH":   cityDir,
		"PATH":           binDir + string(os.PathListSeparator) + os.Getenv("PATH"),
	}

	runScript(t, coreScriptPath("reaper.sh"), env)

	bdData, err := os.ReadFile(bdLog)
	if err != nil {
		t.Fatalf("ReadFile(bd log): %v", err)
	}
	if !strings.Contains(string(bdData), "bd-wisp-stale") {
		t.Fatalf("reaper did not act on count stdout when the store emitted a stderr warning:\n%s", bdData)
	}

	gcData, err := os.ReadFile(gcLog)
	if err != nil {
		t.Fatalf("ReadFile(gc log): %v", err)
	}
	gcLogText := string(gcData)
	if strings.Contains(gcLogText, "ESCALATION") || strings.Contains(gcLogText, "count returned non-numeric") {
		t.Fatalf("reaper treated successful count stderr as an anomaly:\n%s", gcLogText)
	}
	if !strings.Contains(gcLogText, "closed_wisps:1") || !strings.Contains(gcLogText, "purged:1") {
		t.Fatalf("reaper summary did not report the close and purge counts:\n%s", gcLogText)
	}
}

func TestReaperRowQueriesIgnoreSuccessfulStderrWarnings(t *testing.T) {
	cityDir := t.TempDir()
	writeCityBeadsMetadata(t, cityDir, "beads")
	writeFreshBackupState(t, cityDir)
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
  *"SELECT id, CASE WHEN COALESCE(assignee"*"issues"*)
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
`+maintenanceBdPurgeAndBackupVerbs+`exit 0
`)
	writeMaintenanceGCStub(t, filepath.Join(binDir, "gc"), `#!/bin/sh
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

	runScript(t, coreScriptPath("reaper.sh"), env)

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
  *"SELECT DISTINCT w.id"*"wisps w"*"wisp_dependencies d"*)
    printf 'id,owner_id,depth,state,mode\n'
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
	writeMaintenanceGCStub(t, filepath.Join(binDir, "gc"), `#!/bin/sh
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

	runScript(t, coreScriptPath("reaper.sh"), env)

	logData, err := os.ReadFile(doltLog)
	if err != nil {
		t.Fatalf("ReadFile(dolt log): %v", err)
	}
	log := string(logData)
	if !strings.Contains(log, "wisp_dependencies d") {
		t.Fatalf("reaper stale-wisp selection does not use cleanup-edge dependencies:\n%s", log)
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
  *"SELECT DISTINCT w.id"*"wisps w"*"wisp_dependencies d"*)
    n=0
    if [ -f "$CLOSE_COUNT_STATE" ]; then
      n=$(cat "$CLOSE_COUNT_STATE")
    fi
    if [ "$n" = "0" ]; then
      printf '1\n' > "$CLOSE_COUNT_STATE"
      printf 'id,owner_id,depth,state,mode\nbd-wisp-owned,,0,ok,bare\n'
    else
      printf 'id,owner_id,depth,state,mode\n'
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
	writeMaintenanceGCStub(t, filepath.Join(binDir, "gc"), `#!/bin/sh
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

	runScript(t, coreScriptPath("reaper.sh"), env)

	logData, err := os.ReadFile(doltLog)
	if err != nil {
		t.Fatalf("ReadFile(dolt log): %v", err)
	}
	log := string(logData)
	if strings.Contains(log, "parent_id") {
		t.Fatalf("reaper used removed parent_id column:\n%s", log)
	}
	if !strings.Contains(log, "SELECT DISTINCT w.id") {
		t.Fatalf("reaper stale-wisp close candidates can be join-multiplied:\n%s", log)
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
	if !strings.Contains(string(gcData), " close bd-wisp-owned --reason ") {
		t.Fatalf("reaper did not close schema-safe stale wisp candidates through bd close:\n%s", gcData)
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
	gcData, err := os.ReadFile(gcLog)
	if err != nil {
		t.Fatalf("ReadFile(gc log): %v", err)
	}
	if !strings.Contains(string(gcData), " close bd-wisp-fixture --reason ") {
		t.Fatalf("reaper did not close stale graph workflow wisp tracked to a closed root:\n%s", gcData)
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
	if !strings.Contains(log, "SELECT DISTINCT w.id") {
		t.Fatalf("reaper never selected stale wisp close candidates:\n%s", log)
	}

	gcData, err := os.ReadFile(gcLog)
	if err != nil {
		t.Fatalf("ReadFile(gc log): %v", err)
	}
	if strings.Contains(string(gcData), " close bd-wisp-fixture") {
		t.Fatalf("reaper closed a stale wisp through an ordinary closed blocks predecessor:\n%s", gcData)
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
  *"workflow_wisp_root_candidates_base"*"LEFT JOIN workflow_wisp_root_candidates"*|*"workflow_issue_root_candidates_base"*"LEFT JOIN workflow_issue_root_candidates"*)
    printf 'COUNT(*)\n0\n'
    ;;
  *"WITH workflow_wisp_root_candidates_base"*"SELECT COUNT(*) FROM workflow_wisp_root_candidates"*)
    printf 'COUNT(*)\n1\n'
    ;;
  *"WITH workflow_issue_root_candidates_base"*"SELECT COUNT(*) FROM workflow_issue_root_candidates"*)
    printf 'COUNT(*)\n1\n'
    ;;
  *"FROM workflow_wisp_root_candidates_page root"*)
    printf 'id,verdict\nwisp-root-close,close\n'
    ;;
  *"FROM workflow_issue_root_candidates_page root"*)
    printf 'id,verdict\nissue-close,close\n'
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
`+maintenanceBdPurgeAndBackupVerbs+`exit 0
`)
	writeMaintenanceGCStub(t, filepath.Join(binDir, "gc"), `#!/bin/sh
printf '%s\n' "$*" >> "$GC_CALL_LOG"
exit 0
`)
	writeCityBeadsMetadata(t, cityDir, "beads")
	rigDir := filepath.Join(cityDir, "rigs", "beads-rig")
	writeCityBeadsMetadata(t, rigDir, "beads")
	writeSiteRigBinding(t, cityDir, "beads-rig", rigDir)

	env := map[string]string{
		"BD_CALL_LOG":        bdLog,
		"FAKE_RIG_LIST_JSON": `{"rigs":[{"name":"beads-rig","hq":false}]}`,
		"FAKE_SCOPE_DBS":     "city=beads rig:beads-rig=beads",
		"DOLT_ARGS_LOG":      doltLog,
		"GC_CALL_LOG":        gcLog,
		"GC_CITY":            cityDir,
		"GC_CITY_PATH":       cityDir,
		"GC_DOLT_HOST":       "127.0.0.1",
		"GC_DOLT_PORT":       "3307",
		"GC_DOLT_USER":       "root",
		"GC_DOLT_PASSWORD":   "",
		"PATH":               binDir + string(os.PathListSeparator) + os.Getenv("PATH"),
	}

	runScript(t, coreScriptPath("reaper.sh"), env)

	logData, err := os.ReadFile(doltLog)
	if err != nil {
		t.Fatalf("ReadFile(dolt log): %v", err)
	}
	log := string(logData)
	for _, want := range []string{
		"WITH RECURSIVE workflow_wisp_root_candidates",
		"WITH RECURSIVE workflow_issue_root_candidates",
		"WITH workflow_wisp_root_candidates_base",
		"WITH workflow_issue_root_candidates_base",
		// By default the census checks one candidate root per query, from
		// the first root in id order.
		"WHERE id > ''\n",
		"LIMIT 1\n",
		"workflow_descendants(root_id, id)",
		"roots_with_live_descendants",
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
		"COALESCE(w.updated_at, w.created_at) < DATE_SUB(UTC_TIMESTAMP(), INTERVAL",
		"COALESCE(i.updated_at, i.created_at) < DATE_SUB(UTC_TIMESTAMP(), INTERVAL",
		"descendant_wisp.status, descendant_issue.status) IN ('open', 'hooked', 'in_progress', 'blocked', 'deferred', 'pinned', 'review', 'testing')",
		"roots_with_recent_descendants",
		"child_dep.type IN ('parent-child', 'tracks', 'blocks')",
		"COALESCE(child_dep.depends_on_issue_id, child_dep.depends_on_wisp_id, child_dep.depends_on_external) = root.id",
		"COALESCE(child_dep.depends_on_issue_id, child_dep.depends_on_wisp_id, child_dep.depends_on_external) = parent.id",
	} {
		if !strings.Contains(log, want) {
			t.Fatalf("reaper workflow-root SQL missing %q:\n%s", want, log)
		}
	}
	if strings.Contains(log, "parent_id") {
		t.Fatalf("reaper workflow-root cleanup used removed parent_id column:\n%s", log)
	}
	if strings.Contains(log, "OFFSET") {
		t.Fatalf("reaper paged the workflow-root census by offset, which closing a page shifts:\n%s", log)
	}
	if strings.Contains(log, "UPDATE `beads`.issues SET status='closed'") {
		t.Fatalf("reaper closed city workflow issue roots with raw SQL instead of bd close:\n%s", log)
	}

	bdData, err := os.ReadFile(bdLog)
	if err != nil {
		t.Fatalf("ReadFile(bd log): %v", err)
	}
	bdText := string(bdData)
	if !strings.Contains(bdText, "update wisp-root-close --set-metadata gc.outcome=skipped --set-metadata close_reason=stale inactive workflow root auto-closed by reaper") {
		t.Fatalf("reaper did not stamp the stale workflow wisp root's skipped outcome through bd update:\n%s", bdText)
	}
	if !strings.Contains(bdText, "close wisp-root-close --reason stale inactive workflow root auto-closed by reaper") {
		t.Fatalf("reaper did not close the stale workflow wisp root through bd close:\n%s", bdText)
	}
	if !strings.Contains(bdText, "close issue-close --reason stale inactive workflow root auto-closed by reaper") {
		t.Fatalf("reaper did not close city workflow issue root through bd close:\n%s", bdData)
	}

	gcData, err := os.ReadFile(gcLog)
	if err != nil {
		t.Fatalf("ReadFile(gc log): %v", err)
	}
	if !strings.Contains(string(gcData), "workflow_roots:2") ||
		!strings.Contains(string(gcData), "workflow_root_census:2/2,") ||
		!strings.Contains(string(gcData), "skipped_cross_store_workflow_roots:0") {
		t.Fatalf("reaper summary did not report closed workflow roots:\n%s", gcData)
	}
}

// workflowRootCensusStore is a dolt stub that plays the store for the Step 2
// workflow-root census. CENSUS_ROOTS ("<id>:<close|keep> ...") are the
// CENSUS_KIND (wisp or issue) candidate roots of CENSUS_SCOPE (default city),
// each with the verdict its descendants give. A root bd has closed (read back
// from BD_CALL_LOG) is no longer a candidate, so consecutive runs see each
// other's closes. Census pages are served in id order after the query's keyset
// cursor (and up to its upper bound after a wrap) and logged into BD_CALL_LOG,
// with the census start lookups, so a test reads pages and bd verbs on one
// timeline. CENSUS_COUNT_EXTRA adds that many roots to the candidate count
// that are gone by the time the pages are read, CENSUS_COUNT_FAIL fails the
// candidate count, CENSUS_START_FAIL fails the start lookup and
// CENSUS_FAIL_AFTER fails the page after that cursor. Every other query gets
// the scope fixture's empty answers.
const workflowRootCensusStore = `#!/bin/sh
if [ -z "${GC_FAKE_SCOPE:-}" ]; then
  printf 'DIRECT DOLT CALL: %s\n' "$*" >> "$DOLT_ARGS_LOG"
  exit 97
fi
printf '%s|%s\n' "$GC_FAKE_SCOPE" "$*" >> "$DOLT_ARGS_LOG"
census_open() {
  closed=$(sed -n 's/.* args=close \(.*\) --reason .*/\1/p' "$BD_CALL_LOG" 2>/dev/null | tr '\n' ' ')
  printf '%s\n' $CENSUS_ROOTS | LC_ALL=C sort -t: -k1,1 | LC_ALL=C awk -F: -v closed="$closed" '
    BEGIN { n = split(closed, c, " "); for (i = 1; i <= n; i++) gone[c[i]] = 1 }
    NF && !($1 in gone) { print $1 "," $2 }'
}
for query; do :; done
kind="workflow_${CENSUS_KIND}_root_candidates"
case "$GC_FAKE_SCOPE|$query" in
  "${CENSUS_SCOPE:-city}|"*"LEFT JOIN $kind candidate"*)
    printf 'COUNT(*)\n0\n'
    ;;
  "${CENSUS_SCOPE:-city}|"*"SELECT COUNT(*) FROM $kind"*)
    if [ -n "${CENSUS_COUNT_FAIL:-}" ]; then
      printf 'injected census count timeout\n' >&2
      exit 42
    fi
    printf 'COUNT(*)\n%s\n' "$(( $(census_open | wc -l) + ${CENSUS_COUNT_EXTRA:-0} ))"
    ;;
  "${CENSUS_SCOPE:-city}|"*"FROM ${kind}_page root"*)
    after=$(printf '%s\n' "$query" | sed -n "s/.*WHERE id > '\([^']*\)'.*/\1/p" | head -n 1)
    until=$(printf '%s\n' "$query" | sed -n "s/.*AND id <= '\([^']*\)'.*/\1/p" | head -n 1)
    limit=$(printf '%s\n' "$query" | sed -n 's/.*LIMIT \([0-9][0-9]*\).*/\1/p' | head -n 1)
    if [ -n "$until" ]; then
      printf 'dolt census page after=%s until=%s limit=%s\n' "$after" "$until" "$limit" >> "$BD_CALL_LOG"
    else
      printf 'dolt census page after=%s limit=%s\n' "$after" "$limit" >> "$BD_CALL_LOG"
    fi
    if [ -n "${CENSUS_FAIL_AFTER:-}" ] && [ "$after" = "$CENSUS_FAIL_AFTER" ]; then
      printf 'injected census page timeout\n' >&2
      exit 42
    fi
    printf 'id,verdict\n'
    census_open | LC_ALL=C awk -F, -v after="$after" -v until="$until" -v limit="$limit" \
      '$1 > after && (until == "" || $1 <= until) && n < limit { print; n++ }'
    ;;
  "${CENSUS_SCOPE:-city}|"*"SELECT id FROM $kind"*"OFFSET "*)
    offset=$(printf '%s\n' "$query" | sed -n 's/.*OFFSET \([0-9][0-9]*\).*/\1/p' | head -n 1)
    printf 'dolt census start offset=%s\n' "$offset" >> "$BD_CALL_LOG"
    if [ -n "${CENSUS_START_FAIL:-}" ]; then
      printf 'injected census start timeout\n' >&2
      exit 42
    fi
    printf 'id\n'
    census_open | sed -n "$((offset + 1))p" | cut -d, -f1
    ;;
  *"COUNT("*)
    printf 'COUNT(*)\n0\n'
    ;;
  *"SELECT * FROM "[!\(]*".issues"*)
    printf '{}\n'
    ;;
  *"SELECT DISTINCT w.id"*)
    printf 'id,owner_id,depth,state,mode\n'
    ;;
  *"SELECT"*)
    printf 'id\n'
    ;;
esac
exit 0
`

// workflowRootReasonFlag is the close reason bd is given for a stale workflow
// root.
const workflowRootReasonFlag = "--reason stale inactive workflow root auto-closed by reaper"

// newWorkflowRootCensusFixture is a scope fixture whose store serves the given
// workflow roots of one kind (see workflowRootCensusStore). Its censuses start
// at the first root unless a test overrides or deletes
// GC_REAPER_WORKFLOW_ROOT_CENSUS_START. A failing test logs the bd calls and
// census pages in the order they happened.
func newWorkflowRootCensusFixture(t *testing.T, kind, roots string) scopeFixture {
	t.Helper()
	f := newScopeFixture(t, "")
	writeExecutable(t, filepath.Join(f.binDir, "dolt"), workflowRootCensusStore)
	f.env["CENSUS_KIND"] = kind
	f.env["CENSUS_ROOTS"] = roots
	f.env["GC_REAPER_WORKFLOW_ROOT_CENSUS_START"] = "0"
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("bd calls and census pages:\n%s", f.read(t, f.bdLog))
		}
	})
	return f
}

// jumpRunClockAfterFirstRootClose makes the run clock (date +%s) jump past
// every budget once bd has closed a workflow root, while CENSUS_CLOCK_JUMP is
// set.
func jumpRunClockAfterFirstRootClose(t *testing.T, binDir string) {
	t.Helper()
	realDate, err := exec.LookPath("date")
	if err != nil {
		t.Fatalf("LookPath(date): %v", err)
	}
	writeExecutable(t, filepath.Join(binDir, "date"), `#!/bin/sh
if [ "$1" = "+%s" ] && [ -n "${CENSUS_CLOCK_JUMP:-}" ] && grep -q ' args=close root-' "$BD_CALL_LOG" 2>/dev/null; then
  echo 4102444800
  exit 0
fi
exec '`+realDate+`' "$@"
`)
}

// freezeRunClock stops the run clock (date +%s) at a time every candidate
// count from 2 to 6 divides, so a census start derived from the clock would
// land on the first root every run.
func freezeRunClock(t *testing.T, binDir string) {
	t.Helper()
	realDate, err := exec.LookPath("date")
	if err != nil {
		t.Fatalf("LookPath(date): %v", err)
	}
	writeExecutable(t, filepath.Join(binDir, "date"), `#!/bin/sh
if [ "$1" = "+%s" ]; then
  echo 4102444800
  exit 0
fi
exec '`+realDate+`' "$@"
`)
}

// workflowRootVerbIDs lists, space-separated in call order, the ids of every
// bd <verb> call in a bd call log.
func workflowRootVerbIDs(log, verb string) string {
	var ids []string
	for _, line := range strings.Split(log, "\n") {
		_, args, ok := strings.Cut(line, " args="+verb+" ")
		if !ok {
			continue
		}
		args, _, _ = strings.Cut(args, " --")
		ids = append(ids, strings.Fields(args)...)
	}
	return strings.Join(ids, " ")
}

func TestReaperWorkflowRootCensusClosesEachPageBeforeReadingTheNext(t *testing.T) {
	for _, kind := range []string{"wisp", "issue"} {
		t.Run(kind, func(t *testing.T) {
			f := newWorkflowRootCensusFixture(t, kind, "root-a:close root-b:keep root-c:close root-d:close")
			f.env["GC_REAPER_WORKFLOW_ROOT_BATCH_SIZE"] = "2"

			f.runReaper(t)

			bdText := f.read(t, f.bdLog)
			// A wisp root is stamped skipped before it is closed.
			stamp := func(ids string) []string {
				if kind != "wisp" {
					return nil
				}
				return []string{" args=update " + ids + " --set-metadata gc.outcome=skipped"}
			}
			steps := []string{"dolt census page after= limit=2\n"}
			steps = append(steps, stamp("root-a")...)
			steps = append(steps, " args=close root-a "+workflowRootReasonFlag, "dolt census page after=root-b limit=2\n")
			steps = append(steps, stamp("root-c root-d")...)
			steps = append(steps, " args=close root-c root-d "+workflowRootReasonFlag)
			assertContainsInOrder(t, bdText, steps...)
			if got := strings.Count(bdText, "dolt census page "); got != 2 {
				t.Fatalf("census read %d pages, want 2", got)
			}
			if got := workflowRootVerbIDs(bdText, "close"); got != "root-a root-c root-d" {
				t.Fatalf("closed %q, want each stale root once", got)
			}
			if got := workflowRootVerbIDs(bdText, "update"); kind == "wisp" && got != "root-a root-c root-d" {
				t.Fatalf("stamped %q, want each stale wisp root once", got)
			}
			if strings.Contains(f.read(t, f.doltLog), "OFFSET") {
				t.Fatalf("census paged by offset, which closing a page shifts")
			}
			gcText := f.read(t, f.gcLog)
			for _, want := range []string{"workflow_roots:3,", "workflow_root_census:4/4,"} {
				if !strings.Contains(gcText, want) {
					t.Fatalf("summary missing %q:\n%s", want, gcText)
				}
			}
			if strings.Contains(gcText, "ESCALATION") {
				t.Fatalf("a complete census is not an anomaly:\n%s", gcText)
			}
			if _, declared := f.outcome(t); declared {
				t.Fatalf("a complete census must not declare a skip:\n%s", f.read(t, f.outcomeFile))
			}
		})
	}
}

func TestReaperWorkflowRootCensusChecksTheCandidatesItFinds(t *testing.T) {
	for _, tc := range []struct {
		name   string
		roots  string
		extra  string
		closed string
		census string
	}{
		{
			// Roots that leave the candidates after the count (their workflow
			// closed them, say) end the census at its first short page
			// instead of sending it around again.
			name:   "roots left after the count",
			roots:  "root-a:close root-b:keep",
			extra:  "3",
			closed: "root-a",
			census: "workflow_root_census:2/2,",
		},
		{
			// Roots that join the candidates after the count are checked
			// and counted when their page is read (root-d shares the last
			// page here); otherwise the next run's count picks them up.
			name:   "roots joined after the count",
			roots:  "root-a:close root-b:keep root-c:close root-d:close",
			extra:  "-1",
			closed: "root-a root-c root-d",
			census: "workflow_root_census:4/4,",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newWorkflowRootCensusFixture(t, "wisp", tc.roots)
			f.env["GC_REAPER_WORKFLOW_ROOT_BATCH_SIZE"] = "2"
			f.env["CENSUS_COUNT_EXTRA"] = tc.extra

			f.runReaper(t)

			bdText := f.read(t, f.bdLog)
			assertContainsInOrder(t, bdText,
				"dolt census page after= limit=2\n",
				" args=close root-a "+workflowRootReasonFlag,
				"dolt census page after=root-b limit=2\n",
			)
			if got := strings.Count(bdText, "dolt census page "); got != 2 {
				t.Fatalf("census read %d pages, want 2", got)
			}
			if got := workflowRootVerbIDs(bdText, "close"); got != tc.closed {
				t.Fatalf("closed %q, want %q", got, tc.closed)
			}
			gcText := f.read(t, f.gcLog)
			if !strings.Contains(gcText, tc.census) {
				t.Fatalf("summary missing %q:\n%s", tc.census, gcText)
			}
			if _, declared := f.outcome(t); declared {
				t.Fatalf("a complete census must not declare a skip:\n%s", f.read(t, f.outcomeFile))
			}
		})
	}
}

func TestReaperWorkflowRootCensusKeepsItsClosesWhenTheRunBudgetRunsOut(t *testing.T) {
	for _, kind := range []string{"wisp", "issue"} {
		t.Run(kind, func(t *testing.T) {
			f := newWorkflowRootCensusFixture(t, kind, "root-a:close root-b:close root-c:close")
			f.env["GC_REAPER_WORKFLOW_ROOT_BATCH_SIZE"] = "2"
			jumpRunClockAfterFirstRootClose(t, f.binDir)
			f.env["CENSUS_CLOCK_JUMP"] = "1"

			f.runReaper(t)

			first := f.read(t, f.bdLog)
			if got := workflowRootVerbIDs(first, "close"); got != "root-a root-b" {
				t.Fatalf("first run closed %q, want the page it checked before the run budget ran out", got)
			}
			if strings.Contains(first, "dolt census page after=root-b") {
				t.Fatalf("census read another page after the run budget ran out")
			}
			if strings.Contains(first, " args=purge") {
				t.Fatalf("the scope kept going after the run budget ran out")
			}
			if kind == "wisp" && strings.Contains(f.read(t, f.doltLog), "workflow_issue_root_candidates") {
				t.Fatalf("the issue-root census started after the run budget ran out")
			}
			out, ok := f.outcome(t)
			if !ok || !out.hasScope("city", "run budget exhausted; remaining work carries to the next run") {
				t.Fatalf("outcome = %+v (declared %v), want the city's remaining work carried to the next run", out, ok)
			}
			gcText := f.read(t, f.gcLog)
			if strings.Contains(gcText, "ESCALATION") {
				t.Fatalf("a spent run budget is not an anomaly:\n%s", gcText)
			}
			for _, want := range []string{"workflow_roots:2,", "workflow_root_census:2/3,"} {
				if !strings.Contains(gcText, want) {
					t.Fatalf("first run summary missing %q:\n%s", want, gcText)
				}
			}

			// The next run, on time, closes the root the first could not
			// reach.
			delete(f.env, "CENSUS_CLOCK_JUMP")
			f.gcLog = filepath.Join(t.TempDir(), "gc-second.log")
			f.outcomeFile = filepath.Join(t.TempDir(), "outcome-second.json")
			f.env["GC_CALL_LOG"] = f.gcLog
			f.env["GC_ORDER_OUTCOME_FILE"] = f.outcomeFile

			f.runReaper(t)

			second := strings.TrimPrefix(f.read(t, f.bdLog), first)
			if got := workflowRootVerbIDs(second, "close"); got != "root-c" {
				t.Fatalf("second run closed %q, want the root the first run did not reach", got)
			}
			gcText = f.read(t, f.gcLog)
			for _, want := range []string{"workflow_roots:1,", "workflow_root_census:1/1,"} {
				if !strings.Contains(gcText, want) {
					t.Fatalf("second run summary missing %q:\n%s", want, gcText)
				}
			}
			if _, declared := f.outcome(t); declared {
				t.Fatalf("the second run finished the census and must not declare a skip:\n%s", f.read(t, f.outcomeFile))
			}
		})
	}
}

func TestReaperWorkflowRootCensusFailureClosesOnlyTheCheckedPages(t *testing.T) {
	for _, kind := range []string{"wisp", "issue"} {
		for _, tc := range []struct {
			name    string
			roots   string
			batch   string
			env     map[string]string
			closed  string
			pages   int
			anomaly string
		}{
			{
				name:    "failed page",
				roots:   "root-a:close root-b:close root-c:close",
				batch:   "2",
				env:     map[string]string{"CENSUS_FAIL_AFTER": "root-b"},
				closed:  "root-a root-b",
				pages:   2,
				anomaly: "workflow " + kind + " root census incomplete: page after root-b failed",
			},
			{
				name:    "failed count",
				roots:   "root-a:close root-b:close root-c:close",
				batch:   "2",
				env:     map[string]string{"CENSUS_COUNT_FAIL": "1"},
				anomaly: "workflow " + kind + " root census incomplete: candidate count failed",
			},
			{
				// A page row that is not "<id>,<close|keep>" with a plain id
				// is never closed or used as the next page's cursor.
				name:    "unreadable row",
				roots:   "root'a:close root-b:close",
				batch:   "1",
				pages:   1,
				anomaly: "workflow " + kind + " root census stopped at an unreadable page row",
			},
			{
				name:    "failed start lookup",
				roots:   "root-a:close root-b:close root-c:close",
				batch:   "2",
				env:     map[string]string{"CENSUS_START_FAIL": "1", "GC_REAPER_WORKFLOW_ROOT_CENSUS_START": "1"},
				anomaly: "workflow " + kind + " root census incomplete: start lookup failed",
			},
			{
				// The start is a page cursor and the wrap's upper bound, so
				// it must be a plain id too.
				name:    "unreadable start",
				roots:   "root'a:close root-b:close",
				batch:   "1",
				env:     map[string]string{"GC_REAPER_WORKFLOW_ROOT_CENSUS_START": "1"},
				anomaly: "workflow " + kind + " root census stopped at an unreadable start row",
			},
		} {
			t.Run(kind+"/"+tc.name, func(t *testing.T) {
				f := newWorkflowRootCensusFixture(t, kind, tc.roots)
				f.env["GC_REAPER_WORKFLOW_ROOT_BATCH_SIZE"] = tc.batch
				for key, value := range tc.env {
					f.env[key] = value
				}

				f.runReaper(t)

				bdText := f.read(t, f.bdLog)
				if got := workflowRootVerbIDs(bdText, "close"); got != tc.closed {
					t.Fatalf("closed %q, want %q", got, tc.closed)
				}
				if got := strings.Count(bdText, "dolt census page "); got != tc.pages {
					t.Fatalf("census read %d pages, want %d", got, tc.pages)
				}
				if !strings.Contains(bdText, " args=purge") {
					t.Fatalf("a failed census stopped the scope's later steps")
				}
				gcText := f.read(t, f.gcLog)
				if !strings.Contains(gcText, "ESCALATION") || !strings.Contains(gcText, tc.anomaly) {
					t.Fatalf("reaper did not escalate %q:\n%s", tc.anomaly, gcText)
				}
			})
		}
	}
}

func TestReaperWorkflowRootCensusFallsBackToOneRootPerPage(t *testing.T) {
	for _, batch := range []string{"0", "abc", "-2", "1.5"} {
		t.Run("batch="+batch, func(t *testing.T) {
			f := newWorkflowRootCensusFixture(t, "wisp", "root-a:close root-b:close")
			f.env["GC_REAPER_WORKFLOW_ROOT_BATCH_SIZE"] = batch

			f.runReaper(t)

			bdText := f.read(t, f.bdLog)
			assertContainsInOrder(t, bdText,
				"dolt census page after= limit=1\n",
				" args=close root-a "+workflowRootReasonFlag,
				"dolt census page after=root-a limit=1\n",
				" args=close root-b "+workflowRootReasonFlag,
			)
			if got := strings.Count(bdText, "dolt census page "); got != 2 {
				t.Fatalf("census read %d pages, want one per root", got)
			}
		})
	}
}

func TestReaperWorkflowRootCensusBudgetLeavesTimeForTheLaterSteps(t *testing.T) {
	f := newWorkflowRootCensusFixture(t, "wisp", "root-a:close root-b:close root-c:close")
	f.env["GC_REAPER_WORKFLOW_ROOT_BATCH_SIZE"] = "1"
	f.env["GC_REAPER_WORKFLOW_ROOT_BUDGET_SECS"] = "0"
	f.env["FAKE_RIG_LIST_JSON"] = `{"rigs":[{"name":"api","hq":false}]}`
	f.env["FAKE_SCOPE_DBS"] = "rig:api=apidb"

	f.runReaper(t)

	bdText := f.read(t, f.bdLog)
	// A spent census budget still lets each census check one page.
	if got := workflowRootVerbIDs(bdText, "close"); got != "root-a" {
		t.Fatalf("closed %q, want the one page the spent census budget allows", got)
	}
	if got := strings.Count(bdText, "dolt census page "); got != 1 {
		t.Fatalf("census read %d pages, want 1", got)
	}
	out, ok := f.outcome(t)
	if !ok || !out.hasScope("city", "workflow-root census remains after this run's census budget") {
		t.Fatalf("outcome = %+v (declared %v), want the city's remaining census carried to the next run", out, ok)
	}
	if !strings.Contains(bdText, "scope=city args=purge") {
		t.Fatalf("the census budget did not leave the city's purge its turn")
	}
	gcText := f.read(t, f.gcLog)
	if !strings.Contains(gcText, "--rig api purge") {
		t.Fatalf("the census budget did not leave the rig scope its turn:\n%s", gcText)
	}
	if strings.Contains(gcText, "ESCALATION") {
		t.Fatalf("a spent census budget is not an anomaly:\n%s", gcText)
	}
	if !strings.Contains(gcText, "workflow_root_census:1/3,") {
		t.Fatalf("summary does not show the census was cut short:\n%s", gcText)
	}
}

func TestReaperWorkflowRootCensusStartsAtItsStartAndWrapsAround(t *testing.T) {
	for _, kind := range []string{"wisp", "issue"} {
		for _, tc := range []struct {
			name   string
			roots  string
			batch  string
			start  string
			steps  []string
			pages  int
			closed string
		}{
			{
				name:  "start inside the candidates",
				roots: "root-a:close root-b:keep root-c:close root-d:close",
				batch: "2",
				start: "2",
				steps: []string{
					"dolt census start offset=1\n",
					"dolt census page after=root-b limit=2\n",
					" args=close root-c root-d " + workflowRootReasonFlag,
					"dolt census page after= until=root-b limit=2\n",
					" args=close root-a " + workflowRootReasonFlag,
				},
				pages:  2,
				closed: "root-c root-d root-a",
			},
			{
				// The start is taken modulo the candidate count, and the
				// census stops at the root before its start even when that
				// root is kept.
				name:  "start past the candidates",
				roots: "root-a:keep root-b:close root-c:close",
				batch: "1",
				start: "4",
				steps: []string{
					"dolt census start offset=0\n",
					"dolt census page after=root-a limit=1\n",
					" args=close root-b " + workflowRootReasonFlag,
					"dolt census page after=root-b limit=1\n",
					" args=close root-c " + workflowRootReasonFlag,
					"dolt census page after= until=root-a limit=1\n",
				},
				pages:  3,
				closed: "root-b root-c",
			},
		} {
			t.Run(kind+"/"+tc.name, func(t *testing.T) {
				f := newWorkflowRootCensusFixture(t, kind, tc.roots)
				f.env["GC_REAPER_WORKFLOW_ROOT_BATCH_SIZE"] = tc.batch
				f.env["GC_REAPER_WORKFLOW_ROOT_CENSUS_START"] = tc.start

				f.runReaper(t)

				bdText := f.read(t, f.bdLog)
				assertContainsInOrder(t, bdText, tc.steps...)
				if got := strings.Count(bdText, "dolt census page "); got != tc.pages {
					t.Fatalf("census read %d pages, want %d", got, tc.pages)
				}
				if got := workflowRootVerbIDs(bdText, "close"); got != tc.closed {
					t.Fatalf("closed %q, want %q", got, tc.closed)
				}
				gcText := f.read(t, f.gcLog)
				if strings.Contains(gcText, "ESCALATION") {
					t.Fatalf("a complete census is not an anomaly:\n%s", gcText)
				}
				census := fmt.Sprintf("workflow_root_census:%d/%d,", len(strings.Fields(tc.roots)), len(strings.Fields(tc.roots)))
				if !strings.Contains(gcText, census) {
					t.Fatalf("summary missing %q:\n%s", census, gcText)
				}
				if _, declared := f.outcome(t); declared {
					t.Fatalf("a complete census must not declare a skip:\n%s", f.read(t, f.outcomeFile))
				}
			})
		}
	}
}

// TestReaperWorkflowRootCensusReachesTheRootsBehindTheOnesItKeeps runs the
// reaper with a spent census window, so each run's census checks one page,
// until the roots behind a kept root have closed. A census that started at
// the same root every run would check the kept root forever. The census
// start is random, so each variant gets up to 30 runs: a correct reaper
// needs about 2 or 4 and misses the bound with odds under 1e-8.
func TestReaperWorkflowRootCensusReachesTheRootsBehindTheOnesItKeeps(t *testing.T) {
	for _, tc := range []struct {
		name  string
		roots string
		close []string
	}{
		{name: "kept root first", roots: "root-a:keep root-b:close", close: []string{"root-b"}},
		{name: "kept root between", roots: "root-a:close root-b:keep root-c:close", close: []string{"root-a", "root-c"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newWorkflowRootCensusFixture(t, "wisp", tc.roots)
			// A rig scope with its own database: the window opens at the
			// city's census and is spent before the rig's census starts.
			f.env["CENSUS_SCOPE"] = "rig:api"
			f.env["FAKE_RIG_LIST_JSON"] = `{"rigs":[{"name":"api","hq":false}]}`
			f.env["FAKE_SCOPE_DBS"] = "rig:api=apidb"
			f.env["GC_REAPER_WORKFLOW_ROOT_BUDGET_SECS"] = "0"
			f.env["GC_REAPER_WORKFLOW_ROOT_CENSUS_START"] = ""
			freezeRunClock(t, f.binDir)

			const maxRuns = 30
			pages := 0
			for run := 1; ; run++ {
				f.gcLog = filepath.Join(t.TempDir(), "gc.log")
				f.env["GC_CALL_LOG"] = f.gcLog

				f.runReaper(t)

				bdText := f.read(t, f.bdLog)
				if got := strings.Count(bdText, "dolt census page "); got != pages+1 {
					t.Fatalf("run %d read %d census pages, want the one page a spent census window allows", run, got-pages)
				}
				pages++
				if gcText := f.read(t, f.gcLog); strings.Contains(gcText, "ESCALATION") {
					t.Fatalf("run %d escalated:\n%s", run, gcText)
				}
				closed := strings.Fields(workflowRootVerbIDs(bdText, "close"))
				for _, id := range closed {
					if !slices.Contains(tc.close, id) {
						t.Fatalf("run %d closed %s, which is kept", run, id)
					}
				}
				if len(closed) == len(tc.close) {
					return
				}
				if run == maxRuns {
					t.Fatalf("after %d runs the census closed only %q of %q: it keeps starting at the same roots", maxRuns, closed, tc.close)
				}
			}
		})
	}
}

func TestReaperOnlyCountsWorkflowIssueRootsOutsideTheCityScope(t *testing.T) {
	f := newWorkflowRootCensusFixture(t, "issue", "root-a:close root-b:keep root-c:close")
	f.env["CENSUS_SCOPE"] = "rig:api"
	f.env["FAKE_RIG_LIST_JSON"] = `{"rigs":[{"name":"api","hq":false}]}`
	f.env["FAKE_SCOPE_DBS"] = "rig:api=apidb"

	f.runReaper(t)

	bdText := f.read(t, f.bdLog)
	if strings.Contains(bdText, "dolt census page ") {
		t.Fatalf("reaper walked the descendants of workflow issue roots it never closes outside the city scope")
	}
	if got := workflowRootVerbIDs(bdText, "close"); got != "" {
		t.Fatalf("closed %q outside the city scope, want none", got)
	}
	gcText := f.read(t, f.gcLog)
	for _, want := range []string{"skipped_non_city_workflow_issue_roots:3,", "workflow_root_census:0/0,"} {
		if !strings.Contains(gcText, want) {
			t.Fatalf("summary missing %q:\n%s", want, gcText)
		}
	}
}

func TestReaperWorkflowRootPredicateIsGeneratedFromOneHelper(t *testing.T) {
	data, err := os.ReadFile(coreScriptPath("reaper.sh"))
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
  *"workflow_wisp_root_candidates_base"*"LEFT JOIN workflow_wisp_root_candidates"*|*"workflow_issue_root_candidates_base"*"LEFT JOIN workflow_issue_root_candidates"*)
    printf 'COUNT(*)\n0\n'
    ;;
  *"WITH workflow_wisp_root_candidates_base"*"SELECT COUNT(*) FROM workflow_wisp_root_candidates"*)
    printf 'COUNT(*)\n1\n'
    ;;
  *"WITH workflow_issue_root_candidates_base"*"SELECT COUNT(*) FROM workflow_issue_root_candidates"*)
    printf 'COUNT(*)\n1\n'
    ;;
  *"FROM workflow_wisp_root_candidates_page root"*)
    printf 'id,verdict\nwisp-root-live,keep\n'
    ;;
  *"FROM workflow_issue_root_candidates_page root"*)
    printf 'id,verdict\nissue-root-live,keep\n'
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
	writeMaintenanceGCStub(t, filepath.Join(binDir, "gc"), `#!/bin/sh
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

	runScript(t, coreScriptPath("reaper.sh"), env)

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
	gcData, err := os.ReadFile(gcLog)
	if err != nil {
		t.Fatalf("ReadFile(gc log): %v", err)
	}
	if strings.Contains(string(gcData), " close ") || strings.Contains(string(gcData), " update ") {
		t.Fatalf("reaper closed workflow roots after the closeable selection returned none:\n%s", gcData)
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
  *"workflow_wisp_root_candidates_base"*"LEFT JOIN workflow_wisp_root_candidates"*|*"workflow_issue_root_candidates_base"*"LEFT JOIN workflow_issue_root_candidates"*)
    printf 'COUNT(*)\n0\n'
    ;;
  *"WITH workflow_wisp_root_candidates_base"*"SELECT COUNT(*) FROM workflow_wisp_root_candidates"*)
    printf 'COUNT(*)\n1\n'
    ;;
  *"WITH workflow_issue_root_candidates_base"*"SELECT COUNT(*) FROM workflow_issue_root_candidates"*)
    printf 'COUNT(*)\n1\n'
    ;;
  *"FROM workflow_wisp_root_candidates_page root"*)
    printf 'id,verdict\nwisp-root-close,close\n'
    ;;
  *"FROM workflow_issue_root_candidates_page root"*)
    printf 'id,verdict\nissue-close,close\n'
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
	writeMaintenanceGCStub(t, filepath.Join(binDir, "gc"), `#!/bin/sh
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

	runScript(t, coreScriptPath("reaper.sh"), env)

	gcData, err := os.ReadFile(gcLog)
	if err != nil {
		t.Fatalf("ReadFile(gc log): %v", err)
	}
	if strings.Contains(string(gcData), " close ") || strings.Contains(string(gcData), " update ") {
		t.Fatalf("dry-run closed or stamped workflow roots:\n%s", gcData)
	}
	gcText := string(gcData)
	if !strings.Contains(gcText, "workflow_roots:0") ||
		!strings.Contains(gcText, "would_close_workflow_roots:2") ||
		!strings.Contains(gcText, "(dry run)") {
		t.Fatalf("dry-run summary did not report workflow-root would-close count:\n%s", gcText)
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
	writeMaintenanceGCStub(t, filepath.Join(binDir, "gc"), `#!/bin/sh
printf '%s\n' "$*" >> "$GC_CALL_LOG"
exit 0
`)

	env := map[string]string{
		"DOLT_ARGS_LOG":    doltLog,
		"BD_PURGE_FAIL":    "Error: purge failed: delete failed",
		"GC_CALL_LOG":      gcLog,
		"GC_CITY":          cityDir,
		"GC_CITY_PATH":     cityDir,
		"GC_DOLT_HOST":     "127.0.0.1",
		"GC_DOLT_PORT":     "3307",
		"GC_DOLT_USER":     "root",
		"GC_DOLT_PASSWORD": "",
		"PATH":             binDir + string(os.PathListSeparator) + os.Getenv("PATH"),
	}

	runScript(t, coreScriptPath("reaper.sh"), env)

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
	writeMaintenanceGCStub(t, filepath.Join(binDir, "gc"), `#!/bin/sh
printf '%s\n' "$*" >> "$GC_CALL_LOG"
exit 0
`)

	env := map[string]string{
		"GC_CALL_LOG":      gcLog,
		"BD_PURGE_FAIL":    "stdout-query-preview:" + strings.Repeat("Q", 700) + "\nError 1105 (HY000): wisp_dependencies.depends_on_id missing from schema",
		"GC_CITY":          cityDir,
		"GC_CITY_PATH":     cityDir,
		"GC_DOLT_HOST":     "127.0.0.1",
		"GC_DOLT_PORT":     "3307",
		"GC_DOLT_USER":     "root",
		"GC_DOLT_PASSWORD": "",
		"PATH":             binDir + string(os.PathListSeparator) + os.Getenv("PATH"),
	}

	runScript(t, coreScriptPath("reaper.sh"), env)

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

// TestReaperFailureAnomalyPreservesDoltErrorTailForLongQuery covers the
// regression reported in #4161: dolt sql embeds the full failing query in
// its stderr ("error on line 1 for query <query>: <error>"), so once the
// query text alone exceeds sanitize_output's window the trailing error
// message is silently dropped. TestReaperFailureAnomalyPreservesDoltErrorTail
// above only exercises a short (~90 char) stderr and cannot catch this.
func TestReaperFailureAnomalyPreservesDoltErrorTailForLongQuery(t *testing.T) {
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
	writeMaintenanceGCStub(t, filepath.Join(binDir, "gc"), `#!/bin/sh
printf '%s\n' "$*" >> "$GC_CALL_LOG"
exit 0
`)

	env := map[string]string{
		"GC_CALL_LOG":      gcLog,
		"BD_PURGE_FAIL":    "error on line 1 for query " + strings.Repeat("x", 4200) + ": WITH RECURSIVE iteration limit exceeded",
		"GC_CITY":          cityDir,
		"GC_CITY_PATH":     cityDir,
		"GC_DOLT_HOST":     "127.0.0.1",
		"GC_DOLT_PORT":     "3307",
		"GC_DOLT_USER":     "root",
		"GC_DOLT_PASSWORD": "",
		"PATH":             binDir + string(os.PathListSeparator) + os.Getenv("PATH"),
	}

	runScript(t, coreScriptPath("reaper.sh"), env)

	gcData, err := os.ReadFile(gcLog)
	if err != nil {
		t.Fatalf("ReadFile(gc log): %v", err)
	}
	gcLogText := string(gcData)
	if !strings.Contains(gcLogText, "purging closed wisps failed for beads") {
		t.Fatalf("reaper did not escalate failed purge:\n%s", gcLogText)
	}
	if !strings.Contains(gcLogText, "WITH RECURSIVE iteration limit exceeded") {
		t.Fatalf("reaper escalation lost Dolt error tail for a long query:\n%s", gcLogText)
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
  *"SELECT DISTINCT w.id"*"wisps w"*"wisp_dependencies d"*)
    n=0
    if [ -f "$CLOSE_COUNT_STATE" ]; then
      n=$(cat "$CLOSE_COUNT_STATE")
    fi
    if [ "$n" = "0" ]; then
      printf '1\n' > "$CLOSE_COUNT_STATE"
      printf 'id,owner_id,depth,state,mode\nbd-wisp-owned,,0,ok,bare\n'
    else
      printf 'id,owner_id,depth,state,mode\n'
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
	writeMaintenanceGCStub(t, filepath.Join(binDir, "gc"), `#!/bin/sh
printf '%s\n' "$*" >> "$GC_CALL_LOG"
exit 0
`)

	env := map[string]string{
		"CLOSE_COUNT_STATE": closeCountState,
		"BD_PURGE_FAIL":     "delete failed",
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

	runScript(t, coreScriptPath("reaper.sh"), env)

	gcData, err := os.ReadFile(gcLog)
	if err != nil {
		t.Fatalf("ReadFile(gc log): %v", err)
	}
	if !strings.Contains(string(gcData), " close bd-wisp-owned --reason ") || !strings.Contains(string(gcData), "closed_wisps:1") {
		t.Fatalf("reaper did not keep the successful close after a failed purge:\n%s", gcData)
	}
	if !strings.Contains(string(gcData), "purged:0") || strings.Contains(string(gcData), "purged:1") {
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
  *"SELECT id, CASE WHEN COALESCE(assignee"*"issues"*)
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
`+maintenanceBdPurgeAndBackupVerbs+`exit 0
`)
	writeMaintenanceGCStub(t, filepath.Join(binDir, "gc"), `#!/bin/sh
printf '%s\n' "$*" >> "$GC_CALL_LOG"
exit 0
`)

	env := map[string]string{
		"DOLT_ARGS_LOG":    doltLog,
		"BD_CALL_LOG":      filepath.Join(t.TempDir(), "bd.log"),
		"BD_CLOSE_FAIL":    "close failed",
		"GC_CALL_LOG":      gcLog,
		"GC_CITY":          cityDir,
		"GC_CITY_PATH":     cityDir,
		"GC_DOLT_HOST":     "127.0.0.1",
		"GC_DOLT_PORT":     "3307",
		"GC_DOLT_USER":     "root",
		"GC_DOLT_PASSWORD": "",
		"PATH":             binDir + string(os.PathListSeparator) + os.Getenv("PATH"),
	}

	runScript(t, coreScriptPath("reaper.sh"), env)

	gcData, err := os.ReadFile(gcLog)
	if err != nil {
		t.Fatalf("ReadFile(gc log): %v", err)
	}
	gcLogText := string(gcData)
	if !strings.Contains(gcLogText, "closing stale issues: gc bd close failed for 1 bead(s) in beads (ga-old)") {
		t.Fatalf("reaper did not escalate failed issue close:\n%s", gcLogText)
	}
	if strings.Contains(gcLogText, "closed:1") {
		t.Fatalf("reaper counted failed issue close as success:\n%s", gcLogText)
	}
}

func TestReaperAutoClosesIssuesOnlyInCityDatabase(t *testing.T) {
	cityDir := t.TempDir()
	writeCityBeadsMetadata(t, cityDir, "citydb")
	writeFreshBackupState(t, cityDir)
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
  *"SELECT id, CASE WHEN COALESCE(assignee"*"citydb"*"issues"*)
    printf 'id\nga-city\n'
    ;;
  *"SELECT id, CASE WHEN COALESCE(assignee"*"rigdb"*"issues"*)
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
`+maintenanceBdPurgeAndBackupVerbs+`exit 0
`)
	writeMaintenanceGCStub(t, filepath.Join(binDir, "gc"), `#!/bin/sh
printf '%s\n' "$*" >> "$GC_CALL_LOG"
exit 0
`)

	env := map[string]string{
		"DOLT_ARGS_LOG":      doltLog,
		"BD_CALL_LOG":        bdLog,
		"FAKE_RIG_LIST_JSON": `{"rigs":[{"name":"rig-a","hq":false}]}`,
		"FAKE_SCOPE_DBS":     "rig:rig-a=rigdb",
		"GC_CALL_LOG":        gcLog,
		"GC_CITY":            cityDir,
		"GC_CITY_PATH":       cityDir,
		"GC_DOLT_HOST":       "127.0.0.1",
		"GC_DOLT_PORT":       "3307",
		"GC_DOLT_USER":       "root",
		"GC_DOLT_PASSWORD":   "",
		"PATH":               binDir + string(os.PathListSeparator) + os.Getenv("PATH"),
	}

	runScript(t, coreScriptPath("reaper.sh"), env)

	bdData, err := os.ReadFile(bdLog)
	if err != nil {
		t.Fatalf("ReadFile(bd log): %v", err)
	}
	bdLogText := string(bdData)
	// ga-city is unassigned, so its stale close is bare. The assigned/unassigned
	// force split itself is covered by
	// TestReaperStaleAutoCloseForcesAssignedIssueButKeepsUnassignedBare.
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
	if strings.Contains(gcLogText, "mail send human -s ESCALATION") || strings.Contains(gcLogText, "non-city database") {
		t.Fatalf("reaper escalated expected non-city stale issue skips:\n%s", gcLogText)
	}
}

// TestReaperStaleAutoCloseForcesAssignedIssueButKeepsUnassignedBare locks in the
// per-row force decision: the reaper (order:reaper) closes an assigned stale bead
// with --force because that is a cross-actor close under bd's guard, but closes an
// open/unassigned stale bead bare so a concurrent re-claim after the select is
// still rejected by the guard rather than clobbered.
func TestReaperStaleAutoCloseForcesAssignedIssueButKeepsUnassignedBare(t *testing.T) {
	cityDir := t.TempDir()
	writeCityBeadsMetadata(t, cityDir, "citydb")
	binDir := t.TempDir()
	doltLog := filepath.Join(t.TempDir(), "dolt-args.log")
	bdLog := filepath.Join(t.TempDir(), "bd.log")
	gcLog := filepath.Join(t.TempDir(), "gc.log")

	// The stale query selects id plus a per-row 'force'/'bare' flag computed from
	// the bead's assignee. This stub stands in for Dolt and returns one assigned
	// row (force) and one unassigned row (bare).
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
  *"SELECT id, CASE WHEN COALESCE(assignee"*"citydb"*"issues"*)
    printf 'id,close_mode\nga-assigned,force\nga-open,bare\n'
    ;;
  *"COUNT("*)
    printf 'COUNT(*)\n0\n'
    ;;
esac
exit 0
`)
	writeExecutable(t, filepath.Join(binDir, "bd"), `#!/bin/sh
printf '%s\n' "$*" >> "$BD_CALL_LOG"
`+maintenanceBdPurgeAndBackupVerbs+`exit 0
`)
	writeMaintenanceGCStub(t, filepath.Join(binDir, "gc"), `#!/bin/sh
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

	runScript(t, coreScriptPath("reaper.sh"), env)

	doltData, err := os.ReadFile(doltLog)
	if err != nil {
		t.Fatalf("ReadFile(dolt log): %v", err)
	}
	// The reaper must ask Dolt for the per-row close mode; otherwise the stub's
	// force/bare split would not reflect the real query.
	if !strings.Contains(string(doltData), "CASE WHEN COALESCE(assignee, '') = '' THEN 'bare' ELSE 'force' END") {
		t.Fatalf("stale query did not carry the per-row assignment-state flag:\n%s", doltData)
	}

	bdData, err := os.ReadFile(bdLog)
	if err != nil {
		t.Fatalf("ReadFile(bd log): %v", err)
	}
	bdLogText := string(bdData)
	if !strings.Contains(bdLogText, "close ga-assigned --force --reason stale:auto-closed by reaper") {
		t.Fatalf("reaper did not force-close the assigned stale issue:\n%s", bdLogText)
	}
	if !strings.Contains(bdLogText, "close ga-open --reason stale:auto-closed by reaper") {
		t.Fatalf("reaper did not close the unassigned stale issue bare:\n%s", bdLogText)
	}
	if strings.Contains(bdLogText, "close ga-open --force") {
		t.Fatalf("reaper force-closed an open/unassigned stale issue; it must stay bare:\n%s", bdLogText)
	}

	gcData, err := os.ReadFile(gcLog)
	if err != nil {
		t.Fatalf("ReadFile(gc log): %v", err)
	}
	if !strings.Contains(string(gcData), "closed:2") {
		t.Fatalf("reaper summary did not report both stale closes:\n%s", gcData)
	}
}

func TestReaperStaleAutoClosePreservesDurableExtmsgRecords(t *testing.T) {
	durableIDs := []string{
		"ga-extmsg-group",
		"ga-extmsg-participant",
		"ga-extmsg-binding",
		"ga-extmsg-membership",
		"ga-extmsg-transcript-state",
		"ga-extmsg-transcript",
	}
	closedIDs := runReaperExtmsgRoomFixture(t, durableIDs, "ga-ordinary")
	for _, id := range durableIDs {
		if slices.Contains(closedIDs, id) {
			t.Errorf("reaper closed durable extmsg record %s; all closes: %v", id, closedIDs)
		}
	}
	if !slices.Contains(closedIDs, "ga-ordinary") {
		t.Fatalf("reaper did not close the ordinary stale task; all closes: %v", closedIDs)
	}
}

func TestReaperPreservedExtmsgRoomStillRoutesInboundAndAdvancesTranscript(t *testing.T) {
	ctx := context.Background()
	store := beads.NewMemStore()
	services := extmsg.NewServices(store)
	caller := extmsg.Caller{Kind: extmsg.CallerController, ID: "reaper-integration-test"}
	room := extmsg.ConversationRef{
		ScopeID:        "test-city",
		Provider:       "slack",
		AccountID:      "T-test",
		ConversationID: "C-test-room",
		Kind:           extmsg.ConversationRoom,
	}
	group, err := services.Groups.EnsureGroup(ctx, caller, extmsg.EnsureGroupInput{
		RootConversation: room,
		Mode:             extmsg.GroupModeLauncher,
		DefaultHandle:    "builder",
	})
	if err != nil {
		t.Fatalf("EnsureGroup: %v", err)
	}
	if _, err := services.Groups.UpsertParticipant(ctx, caller, extmsg.UpsertParticipantInput{
		GroupID:   group.ID,
		Handle:    "builder",
		SessionID: "sess-room",
	}); err != nil {
		t.Fatalf("UpsertParticipant: %v", err)
	}
	if _, err := services.Bindings.Bind(ctx, caller, extmsg.BindInput{
		Conversation: extmsg.ConversationRef{
			ScopeID:        "test-city",
			Provider:       "slack",
			AccountID:      "T-test",
			ConversationID: "D-test-dm",
			Kind:           extmsg.ConversationDM,
		},
		SessionID: "sess-dm",
	}); err != nil {
		t.Fatalf("Bind: %v", err)
	}

	first, err := extmsg.HandleInboundNormalized(ctx, extmsg.InboundDeps{Services: services}, extmsg.ExternalInboundMessage{
		ProviderMessageID: "provider-before-reaper",
		Conversation:      room,
		Actor:             extmsg.ExternalActor{ID: "U-test", DisplayName: "Test User"},
		Text:              "before reaper",
		ReceivedAt:        time.Now().Add(-48 * time.Hour),
	})
	if err != nil {
		t.Fatalf("HandleInboundNormalized(before reaper): %v", err)
	}
	if first.TranscriptEntry == nil {
		t.Fatal("first inbound did not create a canonical transcript entry")
	}

	priority := 2
	ordinary, err := store.Create(beads.Bead{Title: "ordinary stale task", Type: "task", Priority: &priority})
	if err != nil {
		t.Fatalf("Create(ordinary stale task): %v", err)
	}
	items, err := store.List(beads.ListQuery{AllowScan: true})
	if err != nil {
		t.Fatalf("List(extmsg fixture): %v", err)
	}
	durableLabels := []string{
		"gc:extmsg-group",
		"gc:extmsg-participant",
		"gc:extmsg-binding",
		"gc:extmsg-membership",
		"gc:extmsg-transcript-state",
		"gc:extmsg-transcript",
	}
	durableIDs := make([]string, 0, len(items))
	seenLabels := make(map[string]bool, len(durableLabels))
	for _, item := range items {
		for _, label := range durableLabels {
			if slices.Contains(item.Labels, label) {
				seenLabels[label] = true
				durableIDs = append(durableIDs, item.ID)
				break
			}
		}
	}
	for _, label := range durableLabels {
		if !seenLabels[label] {
			t.Fatalf("extmsg fixture did not create a record carrying %q", label)
		}
	}

	closedIDs := runReaperExtmsgRoomFixture(t, durableIDs, ordinary.ID)
	for _, id := range closedIDs {
		if err := store.Close(id); err != nil {
			t.Fatalf("apply reaper close for %s: %v", id, err)
		}
	}
	for _, id := range durableIDs {
		item, err := store.Get(id)
		if err != nil {
			t.Fatalf("Get(durable extmsg record %s): %v", id, err)
		}
		if item.Status == "closed" {
			t.Fatalf("durable extmsg record %s was closed by the generic reaper", id)
		}
	}
	ordinaryAfter, err := store.Get(ordinary.ID)
	if err != nil {
		t.Fatalf("Get(ordinary stale task): %v", err)
	}
	if ordinaryAfter.Status != "closed" {
		t.Fatalf("ordinary stale task status = %q, want closed", ordinaryAfter.Status)
	}

	later, err := extmsg.HandleInboundNormalized(ctx, extmsg.InboundDeps{Services: services}, extmsg.ExternalInboundMessage{
		ProviderMessageID: "provider-after-reaper",
		Conversation:      room,
		Actor:             extmsg.ExternalActor{ID: "U-test", DisplayName: "Test User"},
		Text:              "after reaper",
		ReceivedAt:        time.Now(),
	})
	if err != nil {
		t.Fatalf("HandleInboundNormalized(after reaper): %v", err)
	}
	if later.TargetSessionID != "sess-room" {
		t.Fatalf("TargetSessionID = %q, want sess-room", later.TargetSessionID)
	}
	if later.TranscriptEntry == nil {
		t.Fatal("later inbound did not create a canonical transcript entry")
	}
	if later.TranscriptEntry.Sequence != first.TranscriptEntry.Sequence+1 {
		t.Fatalf("later transcript sequence = %d, want %d", later.TranscriptEntry.Sequence, first.TranscriptEntry.Sequence+1)
	}
}

func runReaperExtmsgRoomFixture(t *testing.T, durableIDs []string, ordinaryID string) []string {
	t.Helper()
	cityDir := t.TempDir()
	writeCityBeadsMetadata(t, cityDir, "citydb")
	binDir := t.TempDir()
	doltLog := filepath.Join(t.TempDir(), "dolt-args.log")
	bdLog := filepath.Join(t.TempDir(), "bd.log")

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
  *"SELECT id, CASE WHEN COALESCE(assignee"*"citydb"*"issues"*)
    protected=1
    case "$*" in *"NOT EXISTS"*) ;; *) protected=0 ;; esac
    for label in \
      gc:extmsg-group \
      gc:extmsg-participant \
      gc:extmsg-binding \
      gc:extmsg-membership \
      gc:extmsg-transcript-state \
      gc:extmsg-transcript
    do
      case "$*" in *"'$label'"*) ;; *) protected=0 ;; esac
    done
    printf 'id,close_mode\n'
    if [ "$protected" -ne 1 ]; then
      for id in $DURABLE_IDS; do
        printf '%s,bare\n' "$id"
      done
    fi
    printf '%s,bare\n' "$ORDINARY_ID"
    ;;
  *"COUNT("*)
    printf 'COUNT(*)\n0\n'
    ;;
esac
exit 0
`)
	writeExecutable(t, filepath.Join(binDir, "bd"), `#!/bin/sh
printf '%s\n' "$*" >> "$BD_CALL_LOG"
`+maintenanceBdPurgeAndBackupVerbs+`exit 0
`)
	writeMaintenanceGCStub(t, filepath.Join(binDir, "gc"), "#!/bin/sh\nexit 0\n")

	runScript(t, coreScriptPath("reaper.sh"), map[string]string{
		"DURABLE_IDS":      strings.Join(durableIDs, " "),
		"ORDINARY_ID":      ordinaryID,
		"DOLT_ARGS_LOG":    doltLog,
		"BD_CALL_LOG":      bdLog,
		"GC_CITY":          cityDir,
		"GC_CITY_PATH":     cityDir,
		"GC_DOLT_HOST":     "127.0.0.1",
		"GC_DOLT_PORT":     "3307",
		"GC_DOLT_USER":     "root",
		"GC_DOLT_PASSWORD": "",
		"PATH":             binDir + string(os.PathListSeparator) + os.Getenv("PATH"),
	})
	doltData, err := os.ReadFile(doltLog)
	if err != nil {
		t.Fatalf("ReadFile(dolt log): %v", err)
	}
	if strings.Contains(string(doltData), "gc:extmsg-%") {
		t.Fatalf("stale query used a broad extmsg wildcard instead of explicit durable labels:\n%s", doltData)
	}

	data, err := os.ReadFile(bdLog)
	if err != nil {
		t.Fatalf("ReadFile(bd log): %v", err)
	}
	if !strings.Contains(string(data), "close "+ordinaryID+" --reason stale:auto-closed by reaper") {
		t.Fatalf("reaper did not close ordinary stale task with the generic stale reason:\n%s", data)
	}
	var closed []string
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 && fields[0] == "close" {
			closed = append(closed, fields[1])
		}
	}
	return closed
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
  *"SELECT id, CASE WHEN COALESCE(assignee"*"citydb"*"issues"*)
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
`+maintenanceBdPurgeAndBackupVerbs+`exit 0
`)
	writeMaintenanceGCStub(t, filepath.Join(binDir, "gc"), `#!/bin/sh
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

	runScript(t, coreScriptPath("reaper.sh"), env)

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
  *"SELECT id, CASE WHEN COALESCE(assignee"*"citydb"*"issues"*)
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
`+maintenanceBdPurgeAndBackupVerbs+`exit 0
`)
	writeMaintenanceGCStub(t, filepath.Join(binDir, "gc"), `#!/bin/sh
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

	runScript(t, coreScriptPath("reaper.sh"), env)

	bdData, err := os.ReadFile(bdLog)
	if err != nil {
		t.Fatalf("ReadFile(bd log): %v", err)
	}
	bdLogText := string(bdData)
	// The expired nudge bead is unassigned, so the reaper must close it bare; an
	// inserted --force would defeat bd's cross-actor guard. Pin the full argv and
	// assert --force is absent so a regression toward forcing is caught.
	if !strings.Contains(bdLogText, "close ga-expired --reason ttl:expired by reaper") {
		t.Fatalf("reaper did not close elapsed nudge bead with the expected bare argv:\n%s", bdLogText)
	}
	if strings.Contains(bdLogText, "close ga-expired --force") {
		t.Fatalf("reaper force-closed an unassigned expired nudge bead; the bare close must be preserved:\n%s", bdLogText)
	}

	gcData, err := os.ReadFile(gcLog)
	if err != nil {
		t.Fatalf("ReadFile(gc log): %v", err)
	}
	gcLogText := string(gcData)
	if !strings.Contains(gcLogText, "expired:1") {
		t.Fatalf("reaper summary did not report expired:1:\n%s", gcLogText)
	}

	// The bare close above is only safe because the Step 3 expired-nudge query
	// restricts to unassigned beads; that COALESCE(i.assignee,'')='' filter is
	// the invariant that keeps the bare close out of bd's cross-actor guard.
	// Pin it against the emitted SQL so a future edit that drops the filter
	// while keeping the bare close fails here instead of silently leaking an
	// assigned expired nudge at the next BD_VERSION bump.
	doltData, err := os.ReadFile(doltLog)
	if err != nil {
		t.Fatalf("ReadFile(dolt args log): %v", err)
	}
	if !strings.Contains(string(doltData), "COALESCE(i.assignee, '') = ''") {
		t.Fatalf("expired-nudge query dropped the unassigned filter; bare close would become cross-actor:\n%s", doltData)
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
  *"SELECT id, CASE WHEN COALESCE(assignee"*"citydb"*"issues"*)
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
`+maintenanceBdPurgeAndBackupVerbs+`exit 0
`)
	writeMaintenanceGCStub(t, filepath.Join(binDir, "gc"), `#!/bin/sh
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

	runScript(t, coreScriptPath("reaper.sh"), env)

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
  *"SELECT id, CASE WHEN COALESCE(assignee"*"citydb"*"issues"*)
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
`+maintenanceBdPurgeAndBackupVerbs+`exit 0
`)
	writeMaintenanceGCStub(t, filepath.Join(binDir, "gc"), `#!/bin/sh
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

	runScript(t, coreScriptPath("reaper.sh"), env)

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
  *"SELECT id, CASE WHEN COALESCE(assignee"*"citydb"*"issues"*)
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
`+maintenanceBdPurgeAndBackupVerbs+`exit 0
`)
	writeMaintenanceGCStub(t, filepath.Join(binDir, "gc"), `#!/bin/sh
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

	runScript(t, coreScriptPath("reaper.sh"), env)

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
	gcData, err := os.ReadFile(gcLog)
	if err != nil {
		t.Fatalf("ReadFile(gc log): %v", err)
	}
	wantRoute := "bd --city " + canonicalCityDir + " close ga-city --reason stale:auto-closed by reaper"
	if !strings.Contains(string(gcData), wantRoute) {
		t.Fatalf("reaper did not route issue close through the explicit city scope %q:\n%s", wantRoute, gcData)
	}
}

func TestReaperOrderAndScriptDefaults(t *testing.T) {
	scriptPath := coreScriptPath("reaper.sh")
	scriptData, err := os.ReadFile(scriptPath)
	if err != nil {
		t.Fatalf("ReadFile(%s): %v", scriptPath, err)
	}
	orderPath := filepath.Join(corePackDir(), "orders", "reaper.toml")
	orderData, err := os.ReadFile(orderPath)
	if err != nil {
		t.Fatalf("ReadFile(%s): %v", orderPath, err)
	}

	script := string(scriptData)
	for _, check := range []struct {
		envName string
		want    string
	}{
		{envName: "GC_REAPER_MAX_AGE", want: "24h"},
		{envName: "GC_REAPER_PURGE_AGE", want: "168h"},
		{envName: "GC_REAPER_STALE_ISSUE_AGE", want: "720h"},
	} {
		if got := extractShellDefault(t, script, check.envName); got != check.want {
			t.Errorf("%s default = %q, want %q", check.envName, got, check.want)
		}
	}
	if !strings.Contains(string(orderData), `exec = "$PACK_DIR/assets/scripts/reaper.sh"`) {
		t.Fatalf("reaper order does not execute the Core reaper script:\n%s", orderData)
	}
	if !strings.Contains(string(orderData), `interval = "30m"`) {
		t.Fatalf("reaper order interval changed unexpectedly:\n%s", orderData)
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

func TestDoltDoctorScriptUsesExplicitSQLTarget(t *testing.T) {
	examplesDir := filepath.Dir(exampleDir())
	paths := []string{
		filepath.Join(examplesDir, "bd", "dolt", "assets", "scripts", "mol-dog-doctor.sh"),
	}
	for _, path := range paths {
		t.Run(filepath.Base(path), func(t *testing.T) {
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("ReadFile(%s): %v", path, err)
			}
			if match := rawDoltSQLCallRe.Find(data); match != nil {
				t.Fatalf("script contains unqualified Dolt SQL command %q; include host, port, user, and no-tls args", match)
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
	writeMaintenanceGCStub(t, filepath.Join(binDir, "gc"), `#!/bin/sh
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

	runScript(t, coreScriptPath("spawn-storm-detect.sh"), env)

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
	writeMaintenanceGCStub(t, filepath.Join(binDir, "gc"), `#!/bin/sh
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

	runScript(t, coreScriptPath("spawn-storm-detect.sh"), env)

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
	writeMaintenanceGCStub(t, filepath.Join(binDir, "gc"), `#!/bin/sh
exit 0
`)

	env := map[string]string{
		"GC_CITY":           cityDir,
		"GC_CITY_PATH":      cityDir,
		"GC_PACK_STATE_DIR": stateDir,
		"PATH":              binDir + string(os.PathListSeparator) + os.Getenv("PATH"),
	}

	script := coreScriptPath("spawn-storm-detect.sh")
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
    # gc writes diagnostics to stderr on a read (scope disclosure, doctor
    # warnings). The prune loop must not fold them into the JSON it parses,
    # or every bead reads as "unknown" and nothing is ever pruned.
    printf 'gc bd: answering from the city store\n' >&2
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
	writeMaintenanceGCStub(t, filepath.Join(binDir, "gc"), `#!/bin/sh
exit 0
`)

	env := map[string]string{
		"GC_CITY":               cityDir,
		"GC_CITY_PATH":          cityDir,
		"GC_PACK_STATE_DIR":     stateDir,
		"SPAWN_STORM_THRESHOLD": "99",
		"PATH":                  binDir + string(os.PathListSeparator) + os.Getenv("PATH"),
	}

	runScript(t, coreScriptPath("spawn-storm-detect.sh"), env)

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
	writeMaintenanceGCStub(t, filepath.Join(binDir, "gc"), `#!/bin/sh
exit 0
`)

	env := map[string]string{
		"GC_CITY":               cityDir,
		"GC_CITY_PATH":          cityDir,
		"GC_PACK_STATE_DIR":     stateDir,
		"SPAWN_STORM_THRESHOLD": "99",
		"PATH":                  binDir + string(os.PathListSeparator) + os.Getenv("PATH"),
	}

	runScript(t, coreScriptPath("spawn-storm-detect.sh"), env)

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

// TestSpawnStormDetectRollsBackLedgerWhenAlertUndeliverable pins the rollback
// half of the edge trigger. With -eq, a count left sitting AT the threshold
// never equals it again, so a sweep whose alert could not be delivered has to
// put the count back where it was or the storm is never reported at all. The
// failure must also reach the controller log, which takes a non-zero exit.
func TestSpawnStormDetectRollsBackLedgerWhenAlertUndeliverable(t *testing.T) {
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
	// Every mail send fails the way an unreachable backend does.
	writeMaintenanceGCStub(t, filepath.Join(binDir, "gc"), `#!/bin/sh
printf '%s\n' "$*" >> "$GC_CALL_LOG"
if [ "${1:-}" = "mail" ]; then
  printf 'mail backend unavailable\n' >&2
  exit 1
fi
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

	out, err := runScriptResult(t, coreScriptPath("spawn-storm-detect.sh"), env)
	if err == nil {
		t.Fatalf("spawn-storm-detect exited 0 with an undeliverable alert; want non-zero so the controller logs it\n%s", out)
	}

	ledgerData, readErr := os.ReadFile(filepath.Join(stateDir, "spawn-storm-counts.json"))
	if readErr != nil {
		t.Fatalf("ReadFile(ledger): %v", readErr)
	}
	var counts map[string]int
	if err := json.Unmarshal(ledgerData, &counts); err != nil {
		t.Fatalf("Unmarshal(ledger): %v\n%s", err, ledgerData)
	}
	got, ok := counts["ga-loop"]
	if !ok {
		t.Fatalf("ledger dropped ga-loop entirely; want the pre-sweep count 0 recorded\nledger: %s", ledgerData)
	}
	if got != 0 {
		t.Fatalf("ledger count for ga-loop = %d, want 0 (rolled back); at %d the -eq trigger never fires again\nledger: %s", got, got, ledgerData)
	}

	gcData, err := os.ReadFile(gcLog)
	if err != nil {
		t.Fatalf("ReadFile(gc log): %v", err)
	}
	if !strings.Contains(string(gcData), "SPAWN_STORM: bead ga-loop reset 1x") {
		t.Fatalf("gc log missing the attempted spawn storm notification:\n%s", gcData)
	}
}

// TestSpawnStormDetectAlertsOnceAtThresholdCrossing pins the edge trigger
// itself. A bead already at the threshold whose count moves PAST it is an
// ongoing storm the operator was told about on the crossing sweep, so it must
// not mail again every five minutes for as long as the storm lasts. -ge would
// alert here; -eq does not.
func TestSpawnStormDetectAlertsOnceAtThresholdCrossing(t *testing.T) {
	cityDir := t.TempDir()
	binDir := t.TempDir()
	stateDir := t.TempDir()
	gcLog := filepath.Join(t.TempDir(), "gc.log")
	ledger := filepath.Join(stateDir, "spawn-storm-counts.json")
	// Seeded AT the threshold: the crossing sweep already happened and
	// already alerted.
	if err := os.WriteFile(ledger, []byte(`{"ga-loop":2}`), 0o644); err != nil {
		t.Fatal(err)
	}

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
	writeMaintenanceGCStub(t, filepath.Join(binDir, "gc"), `#!/bin/sh
printf '%s\n' "$*" >> "$GC_CALL_LOG"
exit 0
`)

	env := map[string]string{
		"GC_CITY":               cityDir,
		"GC_CITY_PATH":          cityDir,
		"GC_PACK_STATE_DIR":     stateDir,
		"GC_CALL_LOG":           gcLog,
		"SPAWN_STORM_THRESHOLD": "2",
		"PATH":                  binDir + string(os.PathListSeparator) + os.Getenv("PATH"),
	}

	runScript(t, coreScriptPath("spawn-storm-detect.sh"), env)

	ledgerData, err := os.ReadFile(ledger)
	if err != nil {
		t.Fatalf("ReadFile(ledger): %v", err)
	}
	var counts map[string]int
	if err := json.Unmarshal(ledgerData, &counts); err != nil {
		t.Fatalf("Unmarshal(ledger): %v\n%s", err, ledgerData)
	}
	// The sweep really ran and really counted, so the silence below is the
	// trigger declining rather than the loop never reaching it.
	if got := counts["ga-loop"]; got != 3 {
		t.Fatalf("ledger count for ga-loop = %d, want 3\nledger: %s", got, ledgerData)
	}

	gcData, err := os.ReadFile(gcLog)
	if err != nil {
		t.Fatalf("ReadFile(gc log): %v", err)
	}
	if strings.Contains(string(gcData), "SPAWN_STORM:") {
		t.Fatalf("alerted again at count 3 past threshold 2; the trigger is edge-triggered, not level-triggered\ngc log:\n%s", gcData)
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
	return runScriptResultInDir(t, "", script, env)
}

// runScriptResultInDir runs script from dir; an empty dir keeps the test's
// working directory.
func runScriptResultInDir(t *testing.T, dir, script string, env map[string]string) ([]byte, error) {
	t.Helper()
	return scriptCmd(dir, script, env).CombinedOutput()
}

// scriptCmd is the command that runs script from dir (an empty dir keeps the
// test's working directory) with env merged over the test environment.
func scriptCmd(dir, script string, env map[string]string) *exec.Cmd {
	cmd := exec.Command(script)
	cmd.Dir = dir
	cmd.Env = mergeTestEnv(env)
	return cmd
}

func runReaperCloseFixture(t *testing.T, fixture string) (doltLog string, gcLog string) {
	t.Helper()
	cityDir := t.TempDir()
	binDir := t.TempDir()
	doltLog = filepath.Join(t.TempDir(), "dolt-args.log")
	gcLog = filepath.Join(t.TempDir(), "gc.log")

	writeReaperCloseFixtureDoltStub(t, filepath.Join(binDir, "dolt"))
	writeMaintenanceGCStub(t, filepath.Join(binDir, "gc"), `#!/bin/sh
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
		"REAPER_CLOSED_FLAG":   filepath.Join(t.TempDir(), "closed"),
		"PATH":                 binDir + string(os.PathListSeparator) + os.Getenv("PATH"),
	}

	runScript(t, coreScriptPath("reaper.sh"), env)
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

func writeFreshBackupState(t *testing.T, cityDir string) {
	t.Helper()
	backupDir := filepath.Join(cityDir, ".beads", "backup")
	if err := os.MkdirAll(backupDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(backup dir): %v", err)
	}
	ts := time.Now().UTC().Format(time.RFC3339)
	content := fmt.Sprintf(`{"timestamp":%q}`, ts)
	if err := os.WriteFile(filepath.Join(backupDir, "backup_state.json"), []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile(backup_state.json): %v", err)
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
*"SELECT * FROM "[!\(]*)
  printf '{"id":"ga-1","title":"sample"}\n'
  ;;
*"DELETE FROM "*"wisps"*)
  if [ -n "${DOLT_PURGE_COUNT:-}" ]; then
    printf 'ROW_COUNT()\n%s\n' "$DOLT_PURGE_COUNT"
  else
    printf 'ROW_COUNT()\n0\n'
  fi
  ;;
*"session_prune_candidates"*)
  # The reaper's count of bd prune's candidates: as many as the bd double
  # prunes.
  printf 'COUNT(*)\n%s\n' "${BD_PRUNE_COUNT:-0}"
  ;;
*"status = 'closed'"*"closed_at <"*)
  if [ -n "${DOLT_PURGE_COUNT:-}" ]; then
    printf 'COUNT(*)\n%s\n' "$DOLT_PURGE_COUNT"
  else
    printf 'COUNT(*)\n0\n'
  fi
  ;;
*"SELECT COUNT(*) FROM workflow_wisp_root_candidates"[!_]*|*"SELECT COUNT(*) FROM workflow_issue_root_candidates"[!_]*)
  # Candidate workflow roots of each kind. The reaper walks descendants only
  # when there is a candidate, so tests that inspect that SQL set this.
  printf 'COUNT(*)\n%s\n' "${DOLT_WORKFLOW_ROOT_CANDIDATES:-0}"
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
*"SELECT DISTINCT w.id"*"wisps w"*"wisp_dependencies d"*)
  if close_fixture_matches "$*" && [ ! -f "${REAPER_CLOSED_FLAG:-/nonexistent}" ]; then
    : > "$REAPER_CLOSED_FLAG"
    printf 'id,owner_id,depth,state,mode\nbd-wisp-fixture,,0,ok,bare\n'
  else
    printf 'id,owner_id,depth,state,mode\n'
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
	writeExecutable(t, path, maintenanceBdStubBody)
}

// maintenanceBdStubBody is the default bd test double behind the fake gc's
// `gc bd` route. It logs every call (with the gc scope it was routed to) to
// BD_CALL_LOG and answers the verbs the maintenance orders use:
//   - prune/purge report BD_PRUNE_COUNT / BD_PURGE_COUNT; BD_PURGE_FAIL makes
//     purge fail with that text on stderr.
//   - backup status reports the timestamp in $BEADS_DIR/backup/backup_state.json
//     (written by writeFreshBackupState) or BD_BACKUP_STATUS_JSON verbatim;
//     BD_BACKUP_STATUS_FAIL makes it fail with that text.
//   - close/update/delete succeed unless BD_CLOSE_FAIL is set.
//   - `<verb> --help` prints a help line followed by BD_VERB_HELP_EXTRA.
//   - `config get --json <key>` reports BD_TYPES_RETAINED as the value.
//
// maintenanceBdPurgeAndBackupVerbs answers `bd prune`, `bd purge` and `bd backup status`
// the way maintenanceBdStubBody does, for test-specific bd doubles that
// otherwise only log their calls.
const maintenanceBdPurgeAndBackupVerbs = `case "$1" in
  prune)
    printf '{"pruned_count":%s}\n' "${BD_PRUNE_COUNT:-0}"
    exit 0
    ;;
  close|update|delete)
` + maintenanceBdAppliedIDsJSON + `
    exit 0
    ;;
  purge)
    printf '{"purged_count":%s,"purge_count":%s}\n' "${BD_PURGE_COUNT:-0}" "${BD_PURGE_COUNT:-0}"
    exit 0
    ;;
  backup)
    state="${BEADS_DIR:-.beads}/backup/backup_state.json"
    if [ -f "$state" ]; then
      ts=$(sed -n 's/.*"timestamp"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' "$state" | head -1)
      printf '{"backup":{"timestamp":"%s"},"dolt":{"configured":false}}\n' "$ts"
    else
      printf '{"backup":{},"dolt":{"configured":false}}\n'
    fi
    exit 0
    ;;
esac
`

const maintenanceBdStubBody = `#!/bin/sh
if [ -n "${BD_CALL_LOG:-}" ]; then
  printf 'pwd=%s beads=%s scope=%s args=%s\n' "$PWD" "${BEADS_DIR:-}" "${GC_FAKE_SCOPE:-}" "$*" >> "$BD_CALL_LOG"
fi
if [ "${2:-}" = "--help" ]; then
  printf 'bd %s help\n%s\n' "$1" "${BD_VERB_HELP_EXTRA:-}"
  exit 0
fi
case "$1" in
  config)
    printf '{"key":"%s","value":%s}\n' "${4:-}" "$(printf '%s' "${BD_TYPES_RETAINED:-}" | jq -Rs .)"
    ;;
  prune)
    printf '{"pruned_count":%s}\n' "${BD_PRUNE_COUNT:-0}"
    ;;
  purge)
    if [ -n "${BD_PURGE_FAIL:-}" ]; then
      printf '%s\n' "$BD_PURGE_FAIL" >&2
      exit 1
    fi
    # BD_PURGE_MORE_BATCHES=N reports has_more for the first N calls
    # (counted in BD_PURGE_STATE).
    more=false
    if [ -n "${BD_PURGE_STATE:-}" ]; then
      printf 'x' >> "$BD_PURGE_STATE"
      if [ "$(wc -c < "$BD_PURGE_STATE" | tr -d ' ')" -le "${BD_PURGE_MORE_BATCHES:-0}" ]; then
        more=true
      fi
    fi
    printf '{"purged_count":%s,"purge_count":%s,"has_more":%s}\n' "${BD_PURGE_COUNT:-0}" "${BD_PURGE_COUNT:-0}" "$more"
    ;;
  backup)
    if [ "${2:-}" = "status" ]; then
      if [ -n "${BD_BACKUP_STATUS_FAIL:-}" ]; then
        printf '%s\n' "$BD_BACKUP_STATUS_FAIL" >&2
        exit 1
      fi
      if [ -n "${BD_BACKUP_STATUS_JSON:-}" ]; then
        printf '%s\n' "$BD_BACKUP_STATUS_JSON"
        exit 0
      fi
      state="${BEADS_DIR:-.beads}/backup/backup_state.json"
      if [ -f "$state" ]; then
        ts=$(sed -n 's/.*"timestamp"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' "$state" | head -1)
        printf '{"backup":{"timestamp":"%s"},"dolt":{"configured":false}}\n' "$ts"
      else
        printf '{"backup":{},"dolt":{"configured":false}}\n'
      fi
    fi
    ;;
  close|update)
` + maintenanceBdAppliedIDsJSON + `
    ;;
  delete)
    if [ -n "${BD_CLOSE_FAIL:-}" ]; then
      printf '%s\n' "$BD_CLOSE_FAIL" >&2
      exit 1
    fi
    n=0
    while [ $# -gt 0 ]; do
      if [ "$1" = "--from-file" ]; then
        n=$(grep -c . "$2")
      fi
      shift
    done
    printf '{"deleted_count":%s}\n' "$n"
    ;;
esac
exit 0
`

// maintenanceBdAppliedIDsJSON is the body of a bd double's close/update/
// delete arm: it fails every id with BD_CLOSE_FAIL on stderr when that is
// set, and otherwise reports every id argument as applied in bd's --json
// array shape (the maintenance orders count applied ids from that array).
const maintenanceBdAppliedIDsJSON = `    if [ -n "${BD_CLOSE_FAIL:-}" ]; then
      printf '%s\n' "$BD_CLOSE_FAIL" >&2
      exit 1
    fi
    shift
    sep=""
    printf '['
    for arg in "$@"; do
      case "$arg" in
        -*) break ;;
      esac
      printf '%s{"id":"%s"}' "$sep" "$arg"
      sep=","
    done
    printf ']\n'`

// writeMaintenanceGCStub installs a gc test double whose `gc rig list` and
// `gc bd` branches stand in for gc's scope routing, the only way the shipped
// maintenance orders reach a bead store. The caller-provided body continues to
// define every other command.
//
// The fake `gc bd --city C [--rig R]` route:
//   - answers `sql ... "SELECT DATABASE()"` with the scope's database
//     (FAKE_SCOPE_DBS="city=beads rig:api=apidb"; the city defaults to its
//     .beads/metadata.json dolt_database, then to beads);
//   - serves every other `sql --csv|--json <query>` from the test's `dolt`
//     (a stub, or a real dolt sql-server at GC_DOLT_HOST/GC_DOLT_PORT in the
//     dolt_integration tests), which plays the scope's store, so the
//     store-content fixtures read the exact queries the scripts send through bd;
//   - serves `export --all -o FILE` from the same store, converting the
//     stub's issues rows into bd export JSONL (one record per line);
//   - refuses scopes listed in FAKE_NOT_BD_SCOPES the way gc refuses a
//     non-bd beads provider;
//   - fails for scopes listed in FAKE_UNREACHABLE_SCOPES or in the
//     unreachable-scopes file next to the stub (writeUnreachableScopesStub);
//   - hands every other verb to the `bd` test double on PATH (a default one
//     is installed next to gc unless the test brings its own), exporting the
//     scope as GC_FAKE_SCOPE.
//
// `gc rig list --json` prints FAKE_RIG_LIST_JSON (default: no rigs), or fails
// when FAKE_RIG_LIST_FAIL is set.
func writeMaintenanceGCStub(t *testing.T, path, body string) {
	t.Helper()
	const shebang = "#!/bin/sh\n"
	if !strings.HasPrefix(body, shebang) {
		t.Fatalf("gc stub must start with %q", strings.TrimSpace(shebang))
	}
	writeExecutable(t, path, shebang+maintenanceGCScopeRoute+strings.TrimPrefix(body, shebang))
	if bdPath := filepath.Join(filepath.Dir(path), "bd"); !fileExists(bdPath) {
		writeMaintenanceBdStub(t, bdPath)
	}
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// maintenanceGCStderrNoise is the warning the maintenance gc stub prints on
// stderr for every `gc bd` call.
const maintenanceGCStderrNoise = "warning: fake gc stderr noise for this city"

const maintenanceGCScopeRoute = `if [ "${1:-}" = "rig" ] && [ "${2:-}" = "list" ]; then
  if [ -n "${GC_CALL_LOG:-}" ]; then
    printf '%s\n' "$*" >> "$GC_CALL_LOG"
  fi
  if [ -n "${FAKE_RIG_LIST_FAIL:-}" ]; then
    exit 1
  fi
  if [ -n "${FAKE_RIG_LIST_JSON:-}" ]; then
    printf '%s\n' "$FAKE_RIG_LIST_JSON"
  else
    printf '{"rigs":[]}\n'
  fi
  exit 0
fi
if [ "${1:-}" = "bd" ]; then
  if [ -n "${GC_CALL_LOG:-}" ]; then
    printf '%s\n' "$*" >> "$GC_CALL_LOG"
  fi
  # Real gc prints config warnings on stderr ahead of bd's output; the orders
  # must parse only stdout.
  printf '` + maintenanceGCStderrNoise + `\n' >&2
  shift
  fake_scope=city
  if [ "${1:-}" = "--city" ]; then
    city="$2"
    shift 2
    cd "$city" || exit 1
    export BEADS_DIR="$city/.beads"
  fi
  if [ "${1:-}" = "--rig" ]; then
    fake_scope="rig:$2"
    shift 2
  fi
  export GC_FAKE_SCOPE="$fake_scope"
  case " ${FAKE_NOT_BD_SCOPES:-} " in
    *" $fake_scope "*)
      printf 'gc bd: only supported for bd-backed beads providers (resolved "file" for %s)\n' "$fake_scope" >&2
      exit 1
      ;;
  esac
  fake_unreachable="${FAKE_UNREACHABLE_SCOPES:-}"
  if [ -f "$(dirname "$0")/unreachable-scopes" ]; then
    fake_unreachable="$fake_unreachable $(cat "$(dirname "$0")/unreachable-scopes")"
  fi
  case " $fake_unreachable " in
    *" $fake_scope "*)
      printf 'fake gc: %s bead store unreachable\n' "$fake_scope" >&2
      exit 1
      ;;
  esac
  fake_db=beads
  if [ "$fake_scope" = city ] && [ -f "${BEADS_DIR:-}/metadata.json" ]; then
    fake_meta_db=$(sed -n 's/.*"dolt_database"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' "$BEADS_DIR/metadata.json" | head -1)
    if [ -n "$fake_meta_db" ]; then
      fake_db="$fake_meta_db"
    fi
  fi
  for fake_pair in ${FAKE_SCOPE_DBS:-}; do
    case "$fake_pair" in
      "$fake_scope="*) fake_db="${fake_pair#*=}" ;;
    esac
  done
  case "${1:-}" in
    sql)
      fake_format=csv
      if [ "${2:-}" = "--json" ]; then
        fake_format=json
      fi
      case "${3:-}" in
        *"SELECT DATABASE()"*)
          printf 'DATABASE()\n%s\n' "$fake_db"
          exit 0
          ;;
      esac
      DOLT_CLI_PASSWORD="${GC_DOLT_PASSWORD:-}" exec dolt --host "${GC_DOLT_HOST:-127.0.0.1}" --port "${GC_DOLT_PORT:-3307}" --user "${GC_DOLT_USER:-root}" --no-tls sql -r "$fake_format" -q "${3:-}"
      ;;
    export)
      fake_out=""
      while [ $# -gt 0 ]; do
        if [ "$1" = "-o" ]; then
          fake_out="$2"
          shift
        fi
        shift
      done
      fake_payload=$(DOLT_CLI_PASSWORD="${GC_DOLT_PASSWORD:-}" dolt --host "${GC_DOLT_HOST:-127.0.0.1}" --port "${GC_DOLT_PORT:-3307}" --user "${GC_DOLT_USER:-root}" --no-tls sql -r json -q "SELECT * FROM \` + "`" + `$fake_db\` + "`" + `.issues") || exit 1
      if [ -z "$fake_payload" ]; then
        : > "$fake_out"
      elif ! printf '%s\n' "$fake_payload" | jq -c '(.rows // [])[]' > "$fake_out" 2>/dev/null; then
        printf '%s\n' "$fake_payload" > "$fake_out"
      fi
      exit 0
      ;;
  esac
  exec bd "$@"
fi
`

func mergeTestEnv(overrides map[string]string) []string {
	if _, ok := overrides["GC_MAINTENANCE_DONE_TARGET"]; !ok {
		overrides["GC_MAINTENANCE_DONE_TARGET"] = "deacon/"
	}
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
	writeMaintenanceGCStub(t, filepath.Join(binDir, "gc"), `#!/bin/sh
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
		// Git 2.55 and newer start a detached maintenance run after a
		// commit, which would repack a fixture's objects behind its tests.
		{"-c", "maintenance.auto=false", "-c", "gc.auto=0", "commit", "-q", "-m", "seed"},
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

// writeUnreachableScopesStub makes every bead scope unreachable through the
// fake `gc bd` route (the run exports nothing), by listing the city scope in
// the binDir marker the fake gc reads.
func writeUnreachableScopesStub(t *testing.T, binDir string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(binDir, "unreachable-scopes"), []byte("city\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(unreachable-scopes): %v", err)
	}
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

func initEmptyArchiveRemoteOnBranch(t *testing.T, archiveRepo string, prevCount int, branch string) string {
	t.Helper()
	remoteRepo := initEmptyArchiveRemote(t, archiveRepo, prevCount)
	runGit(t, archiveRepo, "branch", "-m", branch)
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

	runScript(t, coreScriptPath("jsonl-export.sh"), env)

	gcData, err := os.ReadFile(gcLog)
	if err != nil {
		t.Fatalf("ReadFile(gc log): %v", err)
	}
	log := string(gcData)
	if !strings.Contains(log, "records: 3") {
		t.Fatalf("expected MAINTENANCE_DONE summary to report records: 3 (jq counted .rows length); got:\n%s", log)
	}
}

// TestJsonlExportFirstRunNeverLeavesTheArchivePathWithoutGit is ga-ooxzxz:
// the first export ran mkdir on the archive path and then git init, so for
// the length of the init the path existed with no .git. A gc doctor run in
// that window, as on a freshly started city whose first jsonl-export order
// was firing, failed its blocking jsonl-archive check ("archive repo ... is
// malformed (no .git)"). A git shim records the archive path's state each
// time git init runs: it must be absent or already a repo, never a bare
// directory, and the finished archive is a repo.
func TestJsonlExportFirstRunNeverLeavesTheArchivePathWithoutGit(t *testing.T) {
	cityDir := t.TempDir()
	binDir := t.TempDir()
	stateDir := t.TempDir()
	gcLog := filepath.Join(t.TempDir(), "gc.log")
	mailLog := filepath.Join(t.TempDir(), "gc-mail.log")
	archiveRepo := filepath.Join(cityDir, "packs", "core", "jsonl-archive")
	initLog := filepath.Join(t.TempDir(), "git-init.log")

	writeMultiRecordDoltStub(t, binDir, 3)
	writeJsonlExportGCStub(t, binDir)
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git not on PATH")
	}
	shim := "#!/bin/sh\n" +
		"for arg in \"$@\"; do\n" +
		"  if [ \"$arg\" = init ]; then\n" +
		"    if [ -d \"$ARCHIVE_PROBE\" ] && [ ! -d \"$ARCHIVE_PROBE/.git\" ]; then echo bare >>\"$INIT_LOG\"; else echo ok >>\"$INIT_LOG\"; fi\n" +
		"  fi\n" +
		"done\n" +
		"exec " + realGit + " \"$@\"\n"
	if err := os.WriteFile(filepath.Join(binDir, "git"), []byte(shim), 0o755); err != nil {
		t.Fatal(err)
	}

	env := jsonlExportEnv(t, cityDir, binDir, stateDir, archiveRepo, gcLog, mailLog)
	env["ARCHIVE_PROBE"] = archiveRepo
	env["INIT_LOG"] = initLog
	runScript(t, coreScriptPath("jsonl-export.sh"), env)

	data, err := os.ReadFile(initLog)
	if err != nil {
		t.Fatalf("git init never ran: %v", err)
	}
	if strings.Contains(string(data), "bare") {
		t.Fatalf("git init ran while the archive path existed without .git (%q): a doctor run then reports the archive malformed", strings.TrimSpace(string(data)))
	}
	if _, err := os.Stat(filepath.Join(archiveRepo, ".git")); err != nil {
		t.Fatalf("archive repo not initialized: %v", err)
	}
	leftovers, err := filepath.Glob(filepath.Join(filepath.Dir(archiveRepo), ".jsonl-archive-init.*"))
	if err != nil || len(leftovers) != 0 {
		t.Fatalf("init staging dirs left behind: %v (%v)", leftovers, err)
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

	runScript(t, coreScriptPath("jsonl-export.sh"), env)

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

	runScript(t, coreScriptPath("jsonl-export.sh"), env)

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
	if !strings.Contains(string(gcData), "MAINTENANCE_DONE: jsonl — exported") {
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

	runScript(t, coreScriptPath("jsonl-export.sh"), env)

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

	runScript(t, coreScriptPath("jsonl-export.sh"), env)

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

	// The MAINTENANCE_DONE summary on HALT should be the spike-halt nudge, not the
	// regular exported/records/push summary line.
	gcData, err := os.ReadFile(gcLog)
	if err != nil {
		t.Fatalf("ReadFile(gc log): %v", err)
	}
	if !strings.Contains(string(gcData), "HALTED on spike detection") {
		t.Fatalf("expected HALT nudge in gc log:\n%s", gcData)
	}
	if strings.Contains(string(gcData), "MAINTENANCE_DONE: jsonl — exported") {
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

	runScript(t, coreScriptPath("jsonl-export.sh"), env)

	// Should not have escalated (no prior baseline).
	if mailData, _ := os.ReadFile(mailLog); strings.Contains(string(mailData), "ESCALATION: JSONL spike") {
		t.Fatalf("first run with disabled floor must not escalate; mail log:\n%s", mailData)
	}
	// Sanity: the success summary nudge fired (script reached the end).
	gcData, err := os.ReadFile(gcLog)
	if err != nil {
		t.Fatalf("ReadFile(gc log): %v", err)
	}
	if !strings.Contains(string(gcData), "MAINTENANCE_DONE: jsonl") {
		t.Fatalf("expected MAINTENANCE_DONE nudge in gc log:\n%s", gcData)
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

	runScript(t, coreScriptPath("jsonl-export.sh"), env)

	// The same scrub rules bound the store-side source count that a drop
	// spike is checked against.
	scriptData, err := os.ReadFile(coreScriptPath("jsonl-export.sh"))
	if err != nil {
		t.Fatalf("ReadFile(jsonl-export.sh): %v", err)
	}
	for _, want := range []string{
		"issue_type NOT IN ('message', 'event', 'wisp', 'agent')",
		"title NOT LIKE 'gc:%'",
		"title NOT LIKE 'order:%'",
		"NOT (issue_type = 'convoy' AND title LIKE 'sling-%')",
	} {
		if !strings.Contains(string(scriptData), want) {
			t.Fatalf("expected the source-count scrub SQL to contain %q", want)
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
		t.Fatalf("expected MAINTENANCE_DONE summary to report the scrubbed record count, got:\n%s", gcData)
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

	runScript(t, coreScriptPath("jsonl-export.sh"), env)

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

	runScript(t, coreScriptPath("jsonl-export.sh"), env)

	if mailData, _ := os.ReadFile(mailLog); strings.Contains(string(mailData), "ESCALATION: JSONL spike") {
		t.Fatalf("deleted HEAD baseline should behave like no baseline; mail log:\n%s", mailData)
	}
	gcData, err := os.ReadFile(gcLog)
	if err != nil {
		t.Fatalf("ReadFile(gc log): %v", err)
	}
	if !strings.Contains(string(gcData), "MAINTENANCE_DONE: jsonl") {
		t.Fatalf("expected MAINTENANCE_DONE summary after deleted HEAD baseline, got:\n%s", gcData)
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

	runScript(t, coreScriptPath("jsonl-export.sh"), env)

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

	runScript(t, coreScriptPath("jsonl-export.sh"), env)

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

	out, runErr := runScriptResult(t, coreScriptPath("jsonl-export.sh"), env)
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

	out, runErr := runScriptResult(t, coreScriptPath("jsonl-export.sh"), env)
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

	runScript(t, coreScriptPath("jsonl-export.sh"), env)

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

	runScript(t, coreScriptPath("jsonl-export.sh"), env)

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

	runScript(t, coreScriptPath("jsonl-export.sh"), env)

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

	runScript(t, coreScriptPath("jsonl-export.sh"), env)

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

	runScript(t, coreScriptPath("jsonl-export.sh"), env)

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

	runScript(t, coreScriptPath("jsonl-export.sh"), env)

	remoteHeadOut, err := exec.Command("git", "--git-dir", remoteRepo, "rev-parse", "refs/heads/main").CombinedOutput()
	if err != nil {
		t.Fatalf("git rev-parse remote main after replay: %v\n%s", err, remoteHeadOut)
	}
	if got := strings.TrimSpace(string(remoteHeadOut)); got != localHaltHead {
		t.Fatalf("expected git-state fallback to push stranded local commit: got %s want %s", got, localHaltHead)
	}
}

func TestJsonlExportUnreachableScopesStillPushPendingArchiveCommit(t *testing.T) {
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

	runScript(t, coreScriptPath("jsonl-export.sh"), env)

	localHeadOut, err := exec.Command("git", "-C", archiveRepo, "rev-parse", "HEAD").CombinedOutput()
	if err != nil {
		t.Fatalf("git rev-parse local HALT HEAD: %v\n%s", err, localHeadOut)
	}
	localHaltHead := strings.TrimSpace(string(localHeadOut))
	if localHaltHead == remoteHead {
		t.Fatalf("HALT run must create a local-only commit")
	}

	writeUnreachableScopesStub(t, binDir)

	runScript(t, coreScriptPath("jsonl-export.sh"), env)

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

	runScript(t, coreScriptPath("jsonl-export.sh"), env)

	localHeadBeforeReplay, err := exec.Command("git", "-C", archiveRepo, "rev-parse", "HEAD").CombinedOutput()
	if err != nil {
		t.Fatalf("git rev-parse local HALT HEAD: %v\n%s", err, localHeadBeforeReplay)
	}
	haltHead := strings.TrimSpace(string(localHeadBeforeReplay))

	advancedRemoteHead := advanceArchiveRemoteMain(t, remoteRepo)
	if advancedRemoteHead == haltHead {
		t.Fatalf("remote advance must create a new remote commit")
	}

	runScript(t, coreScriptPath("jsonl-export.sh"), env)

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

	runScript(t, coreScriptPath("jsonl-export.sh"), env)

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

	runScript(t, coreScriptPath("jsonl-export.sh"), env)

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

	runScript(t, coreScriptPath("jsonl-export.sh"), env)

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

	runScript(t, coreScriptPath("jsonl-export.sh"), env)

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

	runScript(t, coreScriptPath("jsonl-export.sh"), env)

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

func TestJsonlExportPushUsesExistingArchiveBranch(t *testing.T) {
	cityDir := t.TempDir()
	binDir := t.TempDir()
	stateDir := t.TempDir()
	gcLog := filepath.Join(t.TempDir(), "gc.log")
	mailLog := filepath.Join(t.TempDir(), "gc-mail.log")
	archiveRepo := filepath.Join(cityDir, "archive")

	remoteRepo := initEmptyArchiveRemoteOnBranch(t, archiveRepo, 3, "archive")
	writeMultiRecordDoltStub(t, binDir, 5)
	writeJsonlExportGCStub(t, binDir)

	env := jsonlExportEnv(t, cityDir, binDir, stateDir, archiveRepo, gcLog, mailLog)
	runScript(t, coreScriptPath("jsonl-export.sh"), env)

	localHead := runGitOut(t, archiveRepo, "rev-parse", "HEAD")
	remoteHead := runGitOut(t, remoteRepo, "rev-parse", "refs/heads/archive")
	if remoteHead != localHead {
		t.Fatalf("remote archive branch = %s, want local HEAD %s", remoteHead, localHead)
	}
}

func TestJsonlExportPushHonorsArchiveBranchOverride(t *testing.T) {
	cityDir := t.TempDir()
	binDir := t.TempDir()
	stateDir := t.TempDir()
	gcLog := filepath.Join(t.TempDir(), "gc.log")
	mailLog := filepath.Join(t.TempDir(), "gc-mail.log")
	archiveRepo := filepath.Join(cityDir, "archive")

	remoteRepo := initEmptyArchiveRemote(t, archiveRepo, 3)
	writeMultiRecordDoltStub(t, binDir, 5)
	writeJsonlExportGCStub(t, binDir)

	env := jsonlExportEnv(t, cityDir, binDir, stateDir, archiveRepo, gcLog, mailLog)
	env["GC_JSONL_ARCHIVE_BRANCH"] = "archive-x"
	runScript(t, coreScriptPath("jsonl-export.sh"), env)

	localHead := runGitOut(t, archiveRepo, "rev-parse", "HEAD")
	remoteHead := runGitOut(t, remoteRepo, "rev-parse", "refs/heads/archive-x")
	if remoteHead != localHead {
		t.Fatalf("remote archive-x = %s, want local HEAD %s", remoteHead, localHead)
	}
	if branch := runGitOut(t, archiveRepo, "symbolic-ref", "--short", "HEAD"); branch != "main" {
		t.Fatalf("local branch = %q, want main (override must not rename it)", branch)
	}
}

func TestJsonlExportPushBootstrapFromDetachedHeadCreatesRemoteMain(t *testing.T) {
	cityDir := t.TempDir()
	binDir := t.TempDir()
	stateDir := t.TempDir()
	gcLog := filepath.Join(t.TempDir(), "gc.log")
	mailLog := filepath.Join(t.TempDir(), "gc-mail.log")
	archiveRepo := filepath.Join(cityDir, "archive")
	stateFile := filepath.Join(stateDir, "jsonl-export-state.json")

	remoteRepo := initEmptyArchiveRemote(t, archiveRepo, 3)
	runGit(t, archiveRepo, "checkout", "-q", "--detach")
	writeMultiRecordDoltStub(t, binDir, 5)
	writeJsonlExportGCStub(t, binDir)

	env := jsonlExportEnv(t, cityDir, binDir, stateDir, archiveRepo, gcLog, mailLog)
	runScript(t, coreScriptPath("jsonl-export.sh"), env)

	localHead := runGitOut(t, archiveRepo, "rev-parse", "HEAD")
	remoteHead := runGitOut(t, remoteRepo, "rev-parse", "refs/heads/main")
	if remoteHead != localHead {
		t.Fatalf("remote main = %s, want local HEAD %s", remoteHead, localHead)
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
		t.Fatalf("consecutive_push_failures = %v, want unset or 0\nstate: %s", got, stateData)
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

	runScript(t, coreScriptPath("jsonl-export.sh"), env)

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

	writeUnreachableScopesStub(t, binDir)

	runScript(t, coreScriptPath("jsonl-export.sh"), env)

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

func TestJsonlExportReusesMaintenancePackArchiveRepo(t *testing.T) {
	cityDir := t.TempDir()
	binDir := t.TempDir()
	runtimeDir := filepath.Join(cityDir, ".gc", "runtime")
	stateDir := filepath.Join(runtimeDir, "packs", "core")
	coreArchiveRepo := filepath.Join(stateDir, "jsonl-archive")
	maintenanceArchiveRepo := filepath.Join(runtimeDir, "packs", "maintenance", "jsonl-archive")
	gcLog := filepath.Join(t.TempDir(), "gc.log")
	mailLog := filepath.Join(t.TempDir(), "gc-mail.log")

	prevHead := initSeedArchive(t, maintenanceArchiveRepo, 3)
	writeMultiRecordDoltStub(t, binDir, 5)
	writeJsonlExportGCStub(t, binDir)

	env := jsonlExportEnv(t, cityDir, binDir, stateDir, coreArchiveRepo, gcLog, mailLog)
	env["GC_CITY_RUNTIME_DIR"] = runtimeDir
	delete(env, "GC_JSONL_ARCHIVE_REPO")

	runScript(t, coreScriptPath("jsonl-export.sh"), env)

	if _, err := os.Stat(filepath.Join(coreArchiveRepo, ".git")); err == nil {
		t.Fatalf("jsonl-export.sh created a fresh core archive repo at %s instead of reusing %s", coreArchiveRepo, maintenanceArchiveRepo)
	} else if !os.IsNotExist(err) {
		t.Fatalf("Stat(core archive .git): %v", err)
	}

	headOut, err := exec.Command("git", "-C", maintenanceArchiveRepo, "rev-parse", "HEAD").CombinedOutput()
	if err != nil {
		t.Fatalf("git rev-parse maintenance archive HEAD: %v\n%s", err, headOut)
	}
	if got := strings.TrimSpace(string(headOut)); got == prevHead {
		t.Fatalf("maintenance archive HEAD did not advance; script may not have reused %s", maintenanceArchiveRepo)
	}
}

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

	runScript(t, coreScriptPath("jsonl-export.sh"), env)

	gcData, err := os.ReadFile(gcLog)
	if err != nil {
		t.Fatalf("ReadFile(gc log): %v", err)
	}
	log := string(gcData)
	if strings.Contains(log, "failed: beads") {
		t.Fatalf("empty issues table must not land in failed: summary; gc log:\n%s", log)
	}
	if !strings.Contains(log, "MAINTENANCE_DONE: jsonl — exported 1/1") {
		t.Fatalf("expected success summary `exported 1/1`, got:\n%s", log)
	}

	// The DB should have an issues.jsonl committed in the archive, even though
	// the payload is the empty `{}` form.
	committed, err := exec.Command("git", "-C", archiveRepo, "show", "HEAD:beads/issues.jsonl").CombinedOutput()
	if err != nil {
		t.Fatalf("git show HEAD:beads/issues.jsonl: %v\n%s", err, committed)
	}
	if len(strings.TrimSpace(string(committed))) != 0 {
		t.Fatalf("expected an empty store to archive an empty bd-export snapshot, got:\n%s", committed)
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

	runScript(t, coreScriptPath("jsonl-export.sh"), env)

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

	runScript(t, coreScriptPath("jsonl-export.sh"), env)

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

	runScript(t, coreScriptPath("jsonl-export.sh"), env)

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

	out, err := runScriptResult(t, coreScriptPath("jsonl-export.sh"), env)
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

	out, err := runScriptResult(t, coreScriptPath("jsonl-export.sh"), env)
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

// TestJsonlExportPushRetryDelayIsLocaleIndependent guards against the retry
// delay being formatted with a locale decimal separator ("1,50"), which
// `sleep` rejects. POSIXLY_CORRECT makes gawk honor LC_NUMERIC on output the
// way mawk and BSD awk do by default.
func TestJsonlExportPushRetryDelayIsLocaleIndependent(t *testing.T) {
	probeScript := filepath.Join(t.TempDir(), "locale-probe.sh")
	writeExecutable(t, probeScript, "#!/bin/sh\nawk 'BEGIN{printf \"%.2f\",1.5}'\n")
	probeOut, err := runScriptResult(t, probeScript, map[string]string{
		"LC_ALL":          "de_DE.UTF-8",
		"POSIXLY_CORRECT": "1",
	})
	if err != nil || string(probeOut) != "1,50" {
		t.Skipf("awk does not emit a comma decimal under de_DE.UTF-8 (locale missing?): %q, %v", probeOut, err)
	}

	cityDir := t.TempDir()
	binDir := t.TempDir()
	stateDir := t.TempDir()
	gcLog := filepath.Join(t.TempDir(), "gc.log")
	mailLog := filepath.Join(t.TempDir(), "gc-mail.log")
	archiveRepo := filepath.Join(cityDir, "archive")
	pushLog := filepath.Join(t.TempDir(), "git-push.log")
	sleepLog := filepath.Join(t.TempDir(), "sleep.log")

	initSeedArchiveWithRemote(t, archiveRepo)
	writeMultiRecordDoltStub(t, binDir, 100)
	writeJsonlExportGCStub(t, binDir)
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatalf("LookPath(git): %v", err)
	}
	writeGitPushAttemptStub(t, binDir, realGit, "fail-first", pushLog)
	writeSleepLogStub(t, binDir, sleepLog)

	env := jsonlExportEnv(t, cityDir, binDir, stateDir, archiveRepo, gcLog, mailLog)
	env["GC_JSONL_PUSH_RETRY_DELAY_MIN"] = "1"
	env["GC_JSONL_PUSH_RETRY_DELAY_SPAN"] = "1"
	env["LC_ALL"] = "de_DE.UTF-8"
	env["POSIXLY_CORRECT"] = "1"

	out, err := runScriptResult(t, coreScriptPath("jsonl-export.sh"), env)
	if err != nil {
		t.Fatalf("jsonl-export.sh: %v\n%s", err, out)
	}

	sleepData, err := os.ReadFile(sleepLog)
	if err != nil {
		t.Fatalf("ReadFile(sleep log): %v", err)
	}
	delays := strings.Fields(string(sleepData))
	if len(delays) == 0 {
		t.Fatalf("expected at least one retry sleep, got none\n%s", out)
	}
	delayRE := regexp.MustCompile(`^[0-9]+\.[0-9]{2}$`)
	for _, d := range delays {
		if !delayRE.MatchString(d) {
			t.Fatalf("retry delay %q is not a dot-decimal sleep argument; all delays: %q", d, delays)
		}
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

	out, err := runScriptResult(t, coreScriptPath("jsonl-export.sh"), env)
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

	out, err := runScriptResult(t, coreScriptPath("jsonl-export.sh"), env)
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

	runScript(t, coreScriptPath("jsonl-export.sh"), env)

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

	runScript(t, coreScriptPath("jsonl-export.sh"), env)

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

	runScript(t, coreScriptPath("jsonl-export.sh"), env)

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

	writeUnreachableScopesStub(t, binDir)
	writeJsonlExportGCStub(t, binDir)

	runScript(t, coreScriptPath("jsonl-export.sh"), env)

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

func TestJsonlExportRetriesPendingAlertWithoutReachableScopes(t *testing.T) {
	cityDir := t.TempDir()
	binDir := t.TempDir()
	stateDir := t.TempDir()
	gcLog := filepath.Join(t.TempDir(), "gc.log")
	mailLog := filepath.Join(t.TempDir(), "gc-mail.log")
	archiveRepo := filepath.Join(cityDir, "archive")
	stateFile := filepath.Join(stateDir, "jsonl-export-state.json")

	writeUnreachableScopesStub(t, binDir)
	writeJsonlExportGCStub(t, binDir)

	if err := os.WriteFile(stateFile, []byte(`{"pending_spike_alert":{"database":"beads","prev_count":100,"current_count":10,"delta":90,"threshold":20}}`+"\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(state file): %v", err)
	}

	env := jsonlExportEnv(t, cityDir, binDir, stateDir, archiveRepo, gcLog, mailLog)

	runScript(t, coreScriptPath("jsonl-export.sh"), env)

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

func TestJsonlExportRetriesMultiplePendingAlertsWithoutReachableScopes(t *testing.T) {
	cityDir := t.TempDir()
	binDir := t.TempDir()
	stateDir := t.TempDir()
	gcLog := filepath.Join(t.TempDir(), "gc.log")
	mailLog := filepath.Join(t.TempDir(), "gc-mail.log")
	archiveRepo := filepath.Join(cityDir, "archive")
	stateFile := filepath.Join(stateDir, "jsonl-export-state.json")

	writeUnreachableScopesStub(t, binDir)
	writeJsonlExportGCStub(t, binDir)

	if err := os.WriteFile(stateFile, []byte(`{"pending_spike_alerts":{"alpha":{"database":"alpha","prev_count":100,"current_count":10,"delta":90,"threshold":20},"beta":{"database":"beta","prev_count":80,"current_count":20,"delta":75,"threshold":20}}}`+"\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(state file): %v", err)
	}

	env := jsonlExportEnv(t, cityDir, binDir, stateDir, archiveRepo, gcLog, mailLog)

	runScript(t, coreScriptPath("jsonl-export.sh"), env)

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

	runScript(t, coreScriptPath("jsonl-export.sh"), env)

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

	out, err := runScriptResult(t, coreScriptPath("jsonl-export.sh"), env)
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

	out, err := runScriptResult(t, coreScriptPath("jsonl-export.sh"), env)
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

	out, err := runScriptResult(t, coreScriptPath("jsonl-export.sh"), env)
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

	out, err := runScriptResult(t, coreScriptPath("jsonl-export.sh"), env)
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

	out, err := runScriptResult(t, coreScriptPath("jsonl-export.sh"), env)
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

	out, err := runScriptResult(t, coreScriptPath("jsonl-export.sh"), env)
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

	out, err := runScriptResult(t, coreScriptPath("jsonl-export.sh"), env)
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

	out, err := runScriptResult(t, coreScriptPath("jsonl-export.sh"), env)
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

	runScript(t, coreScriptPath("jsonl-export.sh"), env)

	mailData, err := os.ReadFile(mailLog)
	if err != nil {
		t.Fatalf("ReadFile(mail log): %v", err)
	}
	body := string(mailData)
	wants := []string{
		"ESCALATION: JSONL push failed",
		"Order: jsonl-export",
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

	script := coreScriptPath("jsonl-export.sh")
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

// archiveLimitOutOfReach is an archive-maintenance limit no test archive
// reaches, so only the other limit can trigger packing.
const archiveLimitOutOfReach = "1000000000"

// archiveObjectStats holds the `git count-objects -v` fields the archive
// maintenance tests place their limits against.
type archiveObjectStats struct {
	looseObjects int
	looseKB      int
	packs        int
	packKB       int
}

func readArchiveObjectStats(t *testing.T, archiveRepo string) archiveObjectStats {
	t.Helper()
	var stats archiveObjectStats
	for _, line := range strings.Split(runGitOut(t, archiveRepo, "count-objects", "-v"), "\n") {
		key, value, ok := strings.Cut(line, ": ")
		if !ok {
			t.Fatalf("git count-objects -v printed %q", line)
		}
		n, err := strconv.Atoi(value)
		if err != nil {
			t.Fatalf("git count-objects -v %s: %v", key, err)
		}
		switch key {
		case "count":
			stats.looseObjects = n
		case "size":
			stats.looseKB = n
		case "packs":
			stats.packs = n
		case "size-pack":
			stats.packKB = n
		}
	}
	return stats
}

// commitArchiveBloat commits sizeKB of random, incompressible bytes at the
// archive root and returns the blob's object ID. The archive's loose objects
// then take many more KiB than they number, so a size limit can sit between
// the two values.
func commitArchiveBloat(t *testing.T, archiveRepo string, sizeKB int) string {
	t.Helper()
	data := make([]byte, sizeKB*1024)
	if _, err := rand.Read(data); err != nil {
		t.Fatalf("rand.Read: %v", err)
	}
	if err := os.WriteFile(filepath.Join(archiveRepo, "bloat.bin"), data, 0o644); err != nil {
		t.Fatalf("WriteFile(bloat.bin): %v", err)
	}
	runGit(t, archiveRepo, "add", "bloat.bin")
	runGit(t, archiveRepo, "-c", "maintenance.auto=false", "-c", "gc.auto=0", "commit", "-q", "-m", "bloat")
	return runGitOut(t, archiveRepo, "rev-parse", "HEAD:bloat.bin")
}

// archiveMaintenanceFixture is a seeded archive plus the env for one
// jsonl-export run over it.
type archiveMaintenanceFixture struct {
	archiveRepo string
	binDir      string
	mailLog     string
	env         map[string]string
}

// newArchiveMaintenanceFixture seeds the archive with prevCount records and
// has the export return currentCount. A drop of more than 20% from at least
// 100 records halts the run on the spike check.
func newArchiveMaintenanceFixture(t *testing.T, prevCount, currentCount int) archiveMaintenanceFixture {
	t.Helper()
	cityDir := t.TempDir()
	binDir := t.TempDir()
	mailLog := filepath.Join(t.TempDir(), "gc-mail.log")
	archiveRepo := filepath.Join(cityDir, "archive")
	initSeedArchive(t, archiveRepo, prevCount)
	writeMultiRecordDoltStub(t, binDir, currentCount)
	writeJsonlExportGCStub(t, binDir)
	env := jsonlExportEnv(t, cityDir, binDir, t.TempDir(), archiveRepo, filepath.Join(t.TempDir(), "gc.log"), mailLog)
	// A token floor keeps the free-space check on its df path without
	// making the tests depend on how much space the host has.
	env["GC_JSONL_GC_MIN_FREE_KB"] = "1"
	return archiveMaintenanceFixture{archiveRepo: archiveRepo, binDir: binDir, mailLog: mailLog, env: env}
}

func (f archiveMaintenanceFixture) run(t *testing.T) (string, error) {
	t.Helper()
	return f.runFrom(t, "")
}

// runFrom is run with the script started in dir.
func (f archiveMaintenanceFixture) runFrom(t *testing.T, dir string) (string, error) {
	t.Helper()
	out, err := runScriptResultInDir(t, dir, coreScriptPath("jsonl-export.sh"), f.env)
	return string(out), err
}

func (f archiveMaintenanceFixture) head(t *testing.T) string {
	t.Helper()
	return runGitOut(t, f.archiveRepo, "rev-parse", "HEAD")
}

// addOrigin gives the archive a bare origin holding its current main and
// returns the origin's path, so runs push.
func (f archiveMaintenanceFixture) addOrigin(t *testing.T) string {
	t.Helper()
	remoteRepo := filepath.Join(t.TempDir(), "archive-remote.git")
	runGit(t, f.archiveRepo, "init", "--bare", "-q", remoteRepo)
	runGit(t, f.archiveRepo, "remote", "add", "origin", remoteRepo)
	runGit(t, f.archiveRepo, "push", "-q", "-u", "origin", "main")
	return remoteRepo
}

func (f archiveMaintenanceFixture) originMain(t *testing.T, remoteRepo string) string {
	t.Helper()
	return runGitOut(t, f.archiveRepo, "--git-dir", remoteRepo, "rev-parse", "refs/heads/main")
}

func (f archiveMaintenanceFixture) stateFile() string {
	return filepath.Join(f.env["GC_PACK_STATE_DIR"], "jsonl-export-state.json")
}

func (f archiveMaintenanceFixture) state(t *testing.T) map[string]any {
	t.Helper()
	return readStateObject(t, f.stateFile())
}

// dropStateKey removes key from the state file and from the backup the
// script falls back to, failing if either lacks it.
func (f archiveMaintenanceFixture) dropStateKey(t *testing.T, key string) {
	t.Helper()
	for _, path := range []string{f.stateFile(), f.stateFile() + ".bak"} {
		state := readStateObject(t, path)
		if _, ok := state[key]; !ok {
			t.Fatalf("%s has no %s to drop: %v", path, key, state)
		}
		delete(state, key)
		data, err := json.Marshal(state)
		if err != nil {
			t.Fatalf("Marshal(%s): %v", path, err)
		}
		if err := os.WriteFile(path, append(data, '\n'), 0o644); err != nil {
			t.Fatalf("WriteFile(%s): %v", path, err)
		}
	}
}

func readStateObject(t *testing.T, path string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%s): %v", path, err)
	}
	var state map[string]any
	if err := json.Unmarshal(data, &state); err != nil {
		t.Fatalf("Unmarshal(%s): %v\n%s", path, err, data)
	}
	return state
}

func (f archiveMaintenanceFixture) mail(t *testing.T) string {
	t.Helper()
	mailData, err := os.ReadFile(f.mailLog)
	if err != nil && !os.IsNotExist(err) {
		t.Fatalf("ReadFile(mail log): %v", err)
	}
	return string(mailData)
}

// limitPackingToSnapshot places the loose-object limit one above the
// archive's current count, so the archive is under it until the run's snapshot
// commit reaches it and maintenance packs after the export.
func (f archiveMaintenanceFixture) limitPackingToSnapshot(t *testing.T) {
	t.Helper()
	f.env["GC_JSONL_GC_LOOSE_OBJECTS"] = strconv.Itoa(readArchiveObjectStats(t, f.archiveRepo).looseObjects + 1)
	f.env["GC_JSONL_GC_LOOSE_KB"] = archiveLimitOutOfReach
}

func (f archiveMaintenanceFixture) headSubject(t *testing.T) string {
	t.Helper()
	return runGitOut(t, f.archiveRepo, "log", "-1", "--format=%s")
}

func lookPathGit(t *testing.T) string {
	t.Helper()
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatalf("LookPath(git): %v", err)
	}
	return realGit
}

func (f archiveMaintenanceFixture) failGitRepack(t *testing.T) {
	t.Helper()
	writeGitSubcommandFailureStub(t, f.binDir, lookPathGit(t), "repack")
}

// runSeparate is run with stdout and stderr kept apart.
func (f archiveMaintenanceFixture) runSeparate(t *testing.T) (stdout, stderr string, err error) {
	t.Helper()
	cmd := scriptCmd("", coreScriptPath("jsonl-export.sh"), f.env)
	var so, se bytes.Buffer
	cmd.Stdout, cmd.Stderr = &so, &se
	err = cmd.Run()
	return so.String(), se.String(), err
}

// gcCalls is the log of every `gc` call the runs so far made.
func (f archiveMaintenanceFixture) gcCalls(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(f.env["GC_CALL_LOG"])
	if err != nil && !os.IsNotExist(err) {
		t.Fatalf("ReadFile(gc call log): %v", err)
	}
	return string(data)
}

// startNewGCCallLog points the next run at an empty gc call log.
func (f archiveMaintenanceFixture) startNewGCCallLog(t *testing.T) {
	t.Helper()
	f.env["GC_CALL_LOG"] = filepath.Join(t.TempDir(), "gc-calls.log")
}

// runKilledAtRepack runs the export with a git whose repack never returns.
// The repack signals on fd 3 when it starts, and the run's process group is
// then killed, so what the archive holds is what the export had done before
// maintenance began. It returns the output captured up to the kill.
func (f archiveMaintenanceFixture) runKilledAtRepack(t *testing.T) string {
	t.Helper()
	writeExecutable(t, filepath.Join(f.binDir, "git"), fmt.Sprintf(`#!/bin/sh
for arg in "$@"; do
    if [ "$arg" = repack ]; then
        echo started >&3
        exec sleep 3600
    fi
done
exec '%s' "$@"
`, lookPathGit(t)))
	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	cmd := scriptCmd("", coreScriptPath("jsonl-export.sh"), f.env)
	cmd.ExtraFiles = []*os.File{pw}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting jsonl-export.sh: %v", err)
	}
	_ = pw.Close()
	kill := func() { _ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	// A run stuck before its repack ends at the deadline instead of hanging.
	watchdog := time.AfterFunc(2*time.Minute, kill)
	defer watchdog.Stop()
	// The read returns when the repack signals, or at EOF when every holder
	// of the pipe's write end has exited without reaching a repack.
	started, _ := bufio.NewReader(pr).ReadString('\n')
	_ = pr.Close()
	kill()
	_ = cmd.Wait()
	if started == "" {
		t.Fatalf("the export ended without starting a repack:\n%s", out.String())
	}
	return out.String()
}

// writeGitArgvLogStub installs a git that logs each call's arguments to
// logFile, and any GIT_CONFIG_* variable that injects configuration, before it
// runs the real git.
func writeGitArgvLogStub(t *testing.T, binDir, realGit, logFile string) {
	t.Helper()
	writeExecutable(t, filepath.Join(binDir, "git"), fmt.Sprintf(`#!/bin/sh
printf 'ARGV %%s\n' "$*" >> '%s'
env | grep -E '^GIT_CONFIG_(COUNT|KEY_|VALUE_|PARAMETERS)' | sed 's/^/ENV /' >> '%s'
exec '%s' "$@"
`, logFile, logFile, realGit))
}

// largestArchivePack is the path of the archive's biggest .pack file.
func largestArchivePack(t *testing.T, archiveRepo string) string {
	t.Helper()
	packs, err := filepath.Glob(filepath.Join(archiveRepo, ".git", "objects", "pack", "pack-*.pack"))
	if err != nil || len(packs) == 0 {
		t.Fatalf("archive has no pack (glob err: %v)", err)
	}
	largest, largestSize := "", int64(-1)
	for _, pack := range packs {
		info, err := os.Stat(pack)
		if err != nil {
			t.Fatalf("Stat(%s): %v", pack, err)
		}
		if info.Size() > largestSize {
			largest, largestSize = pack, info.Size()
		}
	}
	return largest
}

func fileSHA256(t *testing.T, path string) [sha256.Size]byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%s): %v", path, err)
	}
	return sha256.Sum256(data)
}

// seedLargePackAndBacklog packs the archive around a 512 KiB blob and 300
// small files into one large pack, then leaves three small commits loose on
// top. A geometric repack weighs packs by object count, so the large pack
// holds far more objects than the backlog adds. It returns the large pack's
// path.
func (f archiveMaintenanceFixture) seedLargePackAndBacklog(t *testing.T) string {
	t.Helper()
	filesDir := filepath.Join(f.archiveRepo, "files")
	if err := os.MkdirAll(filesDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(%s): %v", filesDir, err)
	}
	for i := 0; i < 300; i++ {
		if err := os.WriteFile(filepath.Join(filesDir, fmt.Sprintf("f%03d.txt", i)), []byte(fmt.Sprintf("file %d\n", i)), 0o644); err != nil {
			t.Fatalf("WriteFile(file %d): %v", i, err)
		}
	}
	runGit(t, f.archiveRepo, "add", "files")
	commitArchiveBloat(t, f.archiveRepo, 512)
	runGit(t, f.archiveRepo, "gc", "--quiet")
	for i := 0; i < 3; i++ {
		commitArchiveBloat(t, f.archiveRepo, 4)
	}
	return largestArchivePack(t, f.archiveRepo)
}

// exportOnce runs the export so the archive holds the fixture's data, and the
// next run with the same data has nothing to export.
func (f archiveMaintenanceFixture) exportOnce(t *testing.T) {
	t.Helper()
	if out, err := f.run(t); err != nil {
		t.Fatalf("first export run failed: %v\n%s", err, out)
	}
}

var archiveMaintenanceTelemetry = regexp.MustCompile(`jsonl-export: archive maintenance: mode=(\S+) wall=(\d+)s loose_before=(\d+) loose_after=(\d+) packs_before=(\d+) packs_after=(\d+) pack_kb_after=(\d+)`)

func TestJsonlExportPacksArchiveWhenLooseObjectLimitReached(t *testing.T) {
	f := newArchiveMaintenanceFixture(t, 3, 5)
	f.limitPackingToSnapshot(t)
	f.env["GC_JSONL_GC_WINDOW_MEMORY"] = "64M"

	out, err := f.run(t)
	if err != nil {
		t.Fatalf("jsonl-export.sh failed: %v\n%s", err, out)
	}
	if got := strings.Count(out, "packing archive object database"); got != 1 {
		t.Fatalf("packing passes = %d, want 1, after the snapshot commit:\n%s", got, out)
	}
	if got := readArchiveObjectStats(t, f.archiveRepo).looseObjects; got != 0 {
		t.Fatalf("loose objects after packing = %d, want 0:\n%s", got, out)
	}
	if subject := f.headSubject(t); !strings.HasPrefix(subject, "backup ") {
		t.Fatalf("HEAD subject = %q, want the snapshot commit", subject)
	}
}

func TestJsonlExportPacksArchiveWhenLooseSizeLimitReached(t *testing.T) {
	f := newArchiveMaintenanceFixture(t, 3, 5)
	bloat := commitArchiveBloat(t, f.archiveRepo, 256)
	seed := readArchiveObjectStats(t, f.archiveRepo)
	if seed.looseKB <= seed.looseObjects {
		t.Fatalf("seed archive must hold more loose KiB than loose objects, got %+v", seed)
	}
	// The size limit sits exactly at the loose KiB, above the object count.
	f.env["GC_JSONL_GC_LOOSE_OBJECTS"] = archiveLimitOutOfReach
	f.env["GC_JSONL_GC_LOOSE_KB"] = strconv.Itoa(seed.looseKB)

	out, err := f.run(t)
	if err != nil {
		t.Fatalf("jsonl-export.sh failed: %v\n%s", err, out)
	}
	// The pass runs after the export, so it counts the snapshot's loose
	// objects on top of the seed's.
	if want := "packing archive object database (loose_objects="; !strings.Contains(out, want) {
		t.Fatalf("expected the size limit to trigger packing (%q):\n%s", want, out)
	}
	loosePath := filepath.Join(f.archiveRepo, ".git", "objects", bloat[:2], bloat[2:])
	if _, err := os.Stat(loosePath); !os.IsNotExist(err) {
		t.Fatalf("bloat blob %s is still loose after packing (stat: %v):\n%s", bloat, err, out)
	}
	runGit(t, f.archiveRepo, "cat-file", "-e", bloat)
}

func TestJsonlExportLeavesSmallArchiveLooseUnderDefaultLimits(t *testing.T) {
	f := newArchiveMaintenanceFixture(t, 3, 5)

	out, err := f.run(t)
	if err != nil {
		t.Fatalf("jsonl-export.sh failed: %v\n%s", err, out)
	}
	if strings.Contains(out, "packing archive object database") || strings.Contains(out, "archive maintenance:") {
		t.Fatalf("default limits must not pack a small archive or report on it:\n%s", out)
	}
	if got := readArchiveObjectStats(t, f.archiveRepo).looseObjects; got == 0 {
		t.Fatalf("small archive was packed under the default limits:\n%s", out)
	}
}

// The export comes first: a failed archive repack still leaves the snapshot
// committed, clean and pushed, reports the run's summary on stderr and fails
// the run without telling the maintenance agent it finished.
func TestJsonlExportCommitsAndPushesSnapshotBeforeArchiveMaintenanceFails(t *testing.T) {
	f := newArchiveMaintenanceFixture(t, 3, 5)
	remoteRepo := f.addOrigin(t)
	seedHead := f.head(t)
	f.limitPackingToSnapshot(t)
	f.failGitRepack(t)

	stdout, stderr, err := f.runSeparate(t)
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 1 {
		t.Fatalf("expected exit 1 from the failed archive repack, got %v\nstdout:\n%s\nstderr:\n%s", err, stdout, stderr)
	}
	if !strings.Contains(stderr, "archive object database maintenance failed") {
		t.Fatalf("expected the maintenance failure diagnostic:\n%s", stderr)
	}
	if !regexp.MustCompile(`jsonl-export: jsonl — exported \d+/\d+, records: 5, push: ok; archive maintenance failed`).MatchString(stderr) {
		t.Fatalf("expected the run summary with the maintenance failure on stderr:\n%s", stderr)
	}
	if strings.Contains(stdout, "jsonl-export: jsonl — exported") {
		t.Fatalf("the summary of a failed run belongs on stderr only:\n%s", stdout)
	}
	head := f.head(t)
	if head == seedHead {
		t.Fatalf("a failed archive repack must not hold back the snapshot commit:\n%s", stderr)
	}
	if status := runGitOut(t, f.archiveRepo, "status", "--short"); status != "" {
		t.Fatalf("the snapshot must leave the archive clean:\n%s", status)
	}
	if got := f.originMain(t, remoteRepo); got != head {
		t.Fatalf("origin main = %s, want the snapshot %s pushed:\n%s", got, head, stderr)
	}
	if calls := f.gcCalls(t); strings.Contains(calls, "MAINTENANCE_DONE") {
		t.Fatalf("a failed run must not send MAINTENANCE_DONE:\n%s", calls)
	}
}

// A repack that never returns costs the run its time, not the export: the
// snapshot is committed, clean and pushed before the repack starts.
func TestJsonlExportCommitsSnapshotBeforeRepackThatNeverReturns(t *testing.T) {
	f := newArchiveMaintenanceFixture(t, 3, 5)
	remoteRepo := f.addOrigin(t)
	seedHead := f.head(t)
	f.limitPackingToSnapshot(t)

	out := f.runKilledAtRepack(t)
	head := f.head(t)
	if head == seedHead {
		t.Fatalf("expected the snapshot committed before the repack started:\n%s", out)
	}
	if status := runGitOut(t, f.archiveRepo, "status", "--short"); status != "" {
		t.Fatalf("the snapshot must leave the archive clean:\n%s", status)
	}
	if got := f.originMain(t, remoteRepo); got != head {
		t.Fatalf("origin main = %s, want the snapshot %s pushed before the repack:\n%s", got, head, out)
	}
	if calls := f.gcCalls(t); strings.Contains(calls, "MAINTENANCE_DONE") {
		t.Fatalf("a killed run must not have sent MAINTENANCE_DONE:\n%s", calls)
	}
}

func TestJsonlExportHaltSendsSpikeAlertBeforeArchiveMaintenance(t *testing.T) {
	// The [HALT] baseline commit reaches the loose-object limit and the
	// archive repack fails: the run must still fail, with the spike alert
	// already sent.
	f := newArchiveMaintenanceFixture(t, 100, 10)
	f.limitPackingToSnapshot(t)
	f.failGitRepack(t)

	out, err := f.run(t)
	if err == nil {
		t.Fatalf("expected the failed archive repack to fail the HALT run:\n%s", out)
	}
	if !strings.Contains(out, "archive object database maintenance failed") {
		t.Fatalf("expected the maintenance failure diagnostic:\n%s", out)
	}
	if subject := f.headSubject(t); !strings.HasPrefix(subject, "[HALT]") {
		t.Fatalf("HEAD subject = %q, want the [HALT] baseline commit", subject)
	}
	if mail := f.mail(t); !strings.Contains(mail, "ESCALATION: JSONL spike") {
		t.Fatalf("spike alert must be sent before archive maintenance can fail the run; mail log:\n%s\noutput:\n%s", mail, out)
	}
}

func TestJsonlExportArchiveMaintenanceRefusesWithoutFreeSpace(t *testing.T) {
	f := newArchiveMaintenanceFixture(t, 3, 5)
	seedHead := f.head(t)
	seed := readArchiveObjectStats(t, f.archiveRepo)
	f.env["GC_JSONL_GC_LOOSE_OBJECTS"] = strconv.Itoa(seed.looseObjects)
	// The largest floor the check can compare; no filesystem has this many
	// KiB free.
	f.env["GC_JSONL_GC_MIN_FREE_KB"] = "9223372036854775807"

	out, err := f.run(t)
	if err == nil {
		t.Fatalf("expected the free-space check to fail the run:\n%s", out)
	}
	if !strings.Contains(out, "set GC_JSONL_GC_MIN_FREE_KB=0") {
		t.Fatalf("expected the free-space refusal to name its setting:\n%s", out)
	}
	if strings.Contains(out, "packing archive object database") {
		t.Fatalf("archive repack must not start without free space:\n%s", out)
	}
	if got := readArchiveObjectStats(t, f.archiveRepo).packs; got != seed.packs {
		t.Fatalf("packs = %d, want no pack written (%d):\n%s", got, seed.packs, out)
	}
	if head := f.head(t); head == seedHead {
		t.Fatalf("free-space refusal comes after the export: HEAD %s must have advanced:\n%s", head, out)
	}

	// The check counts loose objects at full size, so the refusal names the
	// by-hand pack that recovers an archive it overcounts.
	_, recovery, ok := strings.Cut(out, "to pack the archive once by hand, run: ")
	if !ok {
		t.Fatalf("expected the free-space refusal to give a by-hand pack command:\n%s", out)
	}
	recovery, _, _ = strings.Cut(recovery, "\n")
	// bash parses the printed command as an operator's shell would; the
	// script runs through the shared helper rather than a new exec call site.
	recoveryScript := filepath.Join(t.TempDir(), "by-hand-pack.sh")
	writeExecutable(t, recoveryScript, "#!/usr/bin/env bash\n"+recovery+"\n")
	if packOut, err := runScriptResult(t, recoveryScript, map[string]string{}); err != nil {
		t.Fatalf("by-hand pack command %q failed: %v\n%s", recovery, err, packOut)
	}
	if got := readArchiveObjectStats(t, f.archiveRepo).looseObjects; got != 0 {
		t.Fatalf("loose objects after the by-hand pack command %q = %d, want 0", recovery, got)
	}
}

func TestJsonlExportArchiveMaintenancePacksWhenFreeSpaceUnreadable(t *testing.T) {
	f := newArchiveMaintenanceFixture(t, 3, 5)
	writeExecutable(t, filepath.Join(f.binDir, "df"), "#!/bin/sh\necho 'simulated df failure' >&2\nexit 1\n")
	f.env["GC_JSONL_GC_LOOSE_OBJECTS"] = "1"
	// The check would refuse this floor if df could answer.
	f.env["GC_JSONL_GC_MIN_FREE_KB"] = "9000000000000000000"

	out, err := f.run(t)
	if err != nil {
		t.Fatalf("an unreadable free-space figure must not block packing: %v\n%s", err, out)
	}
	if !strings.Contains(out, "could not read free disk space") {
		t.Fatalf("expected a warning about the unreadable free space:\n%s", out)
	}
	if got := readArchiveObjectStats(t, f.archiveRepo).looseObjects; got != 0 {
		t.Fatalf("loose objects after packing = %d, want 0:\n%s", got, out)
	}
}

// writeArchiveFreeSpaceDFStub replaces df with one that reports the queried
// repository's repack need plus marginKB as free, so a test can place the
// free-space floor exactly against the need the check compares. The need of a
// full repack is the loose and packed size; of a repack that keeps the largest
// pack as is, that size less the largest .pack file (in whole KiB).
func writeArchiveFreeSpaceDFStub(t *testing.T, binDir, need string, marginKB int) {
	t.Helper()
	if need != "full" && need != "non-largest" {
		t.Fatalf("unknown repack need %q", need)
	}
	writeExecutable(t, filepath.Join(binDir, "df"), fmt.Sprintf(`#!/bin/sh
for repo; do :; done
need_kb=$(git -C "$repo" count-objects -v | awk -F': ' '$1 == "size" || $1 == "size-pack" { kb += $2 } END { print kb }') || exit 1
if [ %q = non-largest ]; then
    largest=$(ls -S "$repo"/.git/objects/pack/pack-*.pack | head -n 1)
    need_kb=$((need_kb - $(wc -c < "$largest") / 1024))
fi
echo 'Filesystem 1024-blocks Used Available Capacity Mounted on'
echo "stub 0 0 $((need_kb + %d)) 0%% /"
`, need, marginKB))
}

func TestJsonlExportArchiveMaintenanceKeepsFreeSpaceFloor(t *testing.T) {
	// A full repack writes another copy of the archive and needs its size plus
	// GC_JSONL_GC_MIN_FREE_KB free. Short of that, a repack that keeps the
	// largest pack as is needs only the rest of the archive plus the floor.
	// Short of both, the run refuses.
	for _, tc := range []struct {
		name     string
		floor    string
		need     string
		marginKB int
		wantMode string // empty: the check refuses
	}{
		{"full need plus the floor repacks whole", "512", "full", 512, "geometric"},
		{"below full need keeps the largest pack", "512", "full", 511, "geometric-keep-largest"},
		{"non-largest need plus the floor keeps the largest pack", "512", "non-largest", 512, "geometric-keep-largest"},
		{"one KiB below the non-largest need refuses", "512", "non-largest", 511, ""},
		{"floor with a leading zero is decimal", "0512", "non-largest", 511, ""},
		{"zero floor disables the check", "0", "non-largest", -1, "geometric"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newArchiveMaintenanceFixture(t, 3, 5)
			largestPack := f.seedLargePackAndBacklog(t)
			largestSum := fileSHA256(t, largestPack)
			seed := readArchiveObjectStats(t, f.archiveRepo)
			if seed.packs == 0 || seed.looseKB == 0 {
				t.Fatalf("seed archive must hold both packed and loose objects, got %+v", seed)
			}
			seedHead := f.head(t)
			argvLog := filepath.Join(t.TempDir(), "git-argv.log")
			f.env["GC_JSONL_GC_LOOSE_OBJECTS"] = strconv.Itoa(seed.looseObjects)
			f.env["GC_JSONL_GC_MIN_FREE_KB"] = tc.floor
			writeArchiveFreeSpaceDFStub(t, f.binDir, tc.need, tc.marginKB)
			writeGitArgvLogStub(t, f.binDir, lookPathGit(t), argvLog)

			out, err := f.run(t)
			if head := f.head(t); head == seedHead {
				t.Fatalf("the export comes before the free-space check: HEAD %s must have advanced:\n%s", head, out)
			}
			if tc.wantMode == "" {
				if err == nil || strings.Contains(out, "packing archive object database") || !strings.Contains(out, "set GC_JSONL_GC_MIN_FREE_KB=0") {
					t.Fatalf("expected the check to refuse %d KiB beyond the %s need under a %s KiB floor (err=%v):\n%s", tc.marginKB, tc.need, tc.floor, err, out)
				}
				if got := readArchiveObjectStats(t, f.archiveRepo).packs; got != seed.packs {
					t.Fatalf("packs = %d, want no pack written (%d):\n%s", got, seed.packs, out)
				}
				return
			}
			if err != nil {
				t.Fatalf("expected packing with %d KiB beyond the %s need under a %s KiB floor: %v\n%s", tc.marginKB, tc.need, tc.floor, err, out)
			}
			if want := "mode=" + tc.wantMode + " "; !strings.Contains(out, want) {
				t.Fatalf("expected %q in the maintenance report:\n%s", want, out)
			}
			argv, readErr := os.ReadFile(argvLog)
			if readErr != nil {
				t.Fatalf("ReadFile(git argv log): %v", readErr)
			}
			keep := "--keep-pack=" + filepath.Base(largestPack)
			if tc.wantMode == "geometric-keep-largest" {
				if !strings.Contains(string(argv), keep) {
					t.Fatalf("expected the repack to pass %s:\n%s", keep, argv)
				}
				if fileSHA256(t, largestPack) != largestSum {
					t.Fatalf("the kept pack %s was rewritten:\n%s", largestPack, out)
				}
			} else if strings.Contains(string(argv), "--keep-pack") {
				t.Fatalf("a repack with room to spare must not keep a pack:\n%s", argv)
			}
		})
	}
}

func TestJsonlExportRejectsInvalidArchiveMaintenanceSettings(t *testing.T) {
	for _, tc := range []struct{ name, value string }{
		{"GC_JSONL_GC_LOOSE_OBJECTS", "2k"},
		{"GC_JSONL_GC_LOOSE_KB", "-1"},
		{"GC_JSONL_GC_MIN_FREE_KB", "500MiB"},
		// Past the 64-bit range, where [ ] cannot compare a value.
		{"GC_JSONL_GC_LOOSE_KB", "99999999999999999999"},
		{"GC_JSONL_GC_MIN_FREE_KB", "9223372036854775808"},
		{"GC_JSONL_GC_WINDOW_MEMORY", "256MB"},
		{"GC_JSONL_GC_WINDOW_MEMORY", "m"},
		// Git takes 0 as no limit and a leading 0 as octal.
		{"GC_JSONL_GC_WINDOW_MEMORY", "0"},
		{"GC_JSONL_GC_WINDOW_MEMORY", "08m"},
	} {
		t.Run(tc.name+"="+tc.value, func(t *testing.T) {
			f := newArchiveMaintenanceFixture(t, 3, 5)
			seedHead := f.head(t)
			f.env[tc.name] = tc.value

			out, err := f.run(t)
			if err == nil {
				t.Fatalf("expected %s=%q to fail the run:\n%s", tc.name, tc.value, out)
			}
			if !strings.Contains(out, "jsonl-export: "+tc.name+" must be") {
				t.Fatalf("expected a %s validation error:\n%s", tc.name, out)
			}
			if head := f.head(t); head != seedHead {
				t.Fatalf("an invalid setting must stop the run before it commits: HEAD %s, want %s", head, seedHead)
			}
		})
	}
}

// A packing failure after the snapshot commit must not hold the snapshot back
// from the remote.
func TestJsonlExportPushesSnapshotWhenArchivePackingFails(t *testing.T) {
	f := newArchiveMaintenanceFixture(t, 3, 5)
	remoteRepo := f.addOrigin(t)
	seedHead := f.head(t)
	f.limitPackingToSnapshot(t)
	f.failGitRepack(t)

	out, err := f.run(t)
	if err == nil {
		t.Fatalf("expected the failed archive repack to fail the run:\n%s", out)
	}
	if !strings.Contains(out, "archive object database maintenance failed") {
		t.Fatalf("expected the maintenance failure diagnostic:\n%s", out)
	}
	head := f.head(t)
	if head == seedHead {
		t.Fatalf("expected the run to commit a snapshot before packing:\n%s", out)
	}
	if got := f.originMain(t, remoteRepo); got != head {
		t.Fatalf("origin main = %s, want the new snapshot %s pushed despite the packing failure:\n%s", got, head, out)
	}
	if pending := f.state(t)["pending_archive_push"]; pending == true {
		t.Fatalf("pending_archive_push must clear once the snapshot is pushed:\n%s", out)
	}
}

// While the archive cannot be packed, runs stop exporting but still push the
// snapshots already committed, including when the state has lost the
// pending-push flag and only the local-only commit shows the push is due.
func TestJsonlExportPushesPendingSnapshotWhenArchiveCannotBePacked(t *testing.T) {
	for _, tc := range []struct {
		name            string
		dropPendingFlag bool
	}{
		{"pending flag in state", false},
		{"local-only commit without the flag", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// The first run halts on the spike: it commits locally and skips
			// the push.
			f := newArchiveMaintenanceFixture(t, 100, 10)
			remoteRepo := f.addOrigin(t)
			seedHead := f.head(t)
			if out, err := f.run(t); err != nil {
				t.Fatalf("HALT run failed: %v\n%s", err, out)
			}
			haltHead := f.head(t)
			if haltHead == seedHead {
				t.Fatalf("expected the HALT run to commit a local snapshot")
			}
			if got := f.originMain(t, remoteRepo); got != seedHead {
				t.Fatalf("HALT run must not push: origin main = %s, want %s", got, seedHead)
			}
			if tc.dropPendingFlag {
				f.dropStateKey(t, "pending_archive_push")
			}

			writeMultiRecordDoltStub(t, f.binDir, 11)
			f.env["GC_JSONL_GC_LOOSE_OBJECTS"] = "1"
			f.failGitRepack(t)

			out, err := f.run(t)
			if err == nil {
				t.Fatalf("expected the run to fail while the archive cannot be packed:\n%s", out)
			}
			if !strings.Contains(out, "archive object database maintenance failed") {
				t.Fatalf("expected the maintenance failure diagnostic:\n%s", out)
			}
			// The export runs before maintenance, so this run exports on top of
			// the HALT snapshot and pushes both.
			head := f.head(t)
			if head == haltHead {
				t.Fatalf("archive HEAD = %s, want a new export on top of the HALT snapshot:\n%s", head, out)
			}
			if got := f.originMain(t, remoteRepo); got != head {
				t.Fatalf("origin main = %s, want both snapshots pushed (%s):\n%s", got, head, out)
			}
			if pending := f.state(t)["pending_archive_push"]; pending == true {
				t.Fatalf("pending_archive_push must clear once the snapshot is pushed:\n%s", out)
			}
		})
	}
}

// An unsent spike alert is saved before the HALT run's packing can fail it,
// and the next run retries the alert before its own packing fails again.
func TestJsonlExportKeepsSpikeAlertWhileArchiveCannotBePacked(t *testing.T) {
	f := newArchiveMaintenanceFixture(t, 100, 10)
	writeJsonlExportGCStubWithMailExitCode(t, f.binDir, 1)
	f.limitPackingToSnapshot(t)
	f.failGitRepack(t)

	out, err := f.run(t)
	if err == nil {
		t.Fatalf("expected the failed archive repack to fail the HALT run:\n%s", out)
	}
	if subject := f.headSubject(t); !strings.HasPrefix(subject, "[HALT]") {
		t.Fatalf("HEAD subject = %q, want the [HALT] baseline commit", subject)
	}
	pending, _ := f.state(t)["pending_spike_alerts"].(map[string]any)
	if _, ok := pending["beads"]; !ok {
		t.Fatalf("the unsent spike alert must be saved before packing fails the run; pending alerts: %v\n%s", pending, out)
	}

	writeJsonlExportGCStub(t, f.binDir)
	out, err = f.run(t)
	if err == nil {
		t.Fatalf("expected the run to fail while the archive cannot be packed:\n%s", out)
	}
	if mail := f.mail(t); strings.Count(mail, "ESCALATION: JSONL spike") != 2 {
		t.Fatalf("want the failed spike alert and its retry in the mail log:\n%s\noutput:\n%s", mail, out)
	}
	if pending, ok := f.state(t)["pending_spike_alerts"]; ok {
		t.Fatalf("the retried spike alert must clear from state, got %v:\n%s", pending, out)
	}
}

// The script changes into the archive before it pushes and packs, so a
// relative GC_JSONL_ARCHIVE_REPO must still name the archive there.
func TestJsonlExportPushesAndPacksArchiveGivenRelativePath(t *testing.T) {
	f := newArchiveMaintenanceFixture(t, 3, 5)
	remoteRepo := f.addOrigin(t)
	seedHead := f.head(t)
	f.limitPackingToSnapshot(t)
	// Orders start in the city directory, which holds the archive here.
	f.env["GC_JSONL_ARCHIVE_REPO"] = filepath.Base(f.archiveRepo)

	out, err := f.runFrom(t, filepath.Dir(f.archiveRepo))
	if err != nil {
		t.Fatalf("jsonl-export.sh with a relative archive path: %v\n%s", err, out)
	}
	head := f.head(t)
	if head == seedHead {
		t.Fatalf("expected the run to commit a snapshot:\n%s", out)
	}
	if got := f.originMain(t, remoteRepo); got != head {
		t.Fatalf("origin main = %s, want the new snapshot %s:\n%s", got, head, out)
	}
	if got := readArchiveObjectStats(t, f.archiveRepo).looseObjects; got != 0 {
		t.Fatalf("loose objects after packing = %d, want 0:\n%s", got, out)
	}
}

// An ionice that cannot start a command here, such as one without -t, must
// not stop the packing.
func TestJsonlExportPacksArchiveWhenIoniceCannotRun(t *testing.T) {
	f := newArchiveMaintenanceFixture(t, 3, 5)
	writeExecutable(t, filepath.Join(f.binDir, "ionice"), "#!/bin/sh\necho 'ionice: invalid option -- t' >&2\nexit 1\n")
	f.limitPackingToSnapshot(t)

	out, err := f.run(t)
	if err != nil {
		t.Fatalf("jsonl-export.sh with an unusable ionice: %v\n%s", err, out)
	}
	if got := readArchiveObjectStats(t, f.archiveRepo).looseObjects; got != 0 {
		t.Fatalf("loose objects after packing = %d, want 0:\n%s", got, out)
	}
}

// Where ionice works, git repack runs under it in the idle I/O class, with -t so
// a class the host refuses does not stop git, and under nice.
func TestJsonlExportPacksArchiveUnderIoniceAndNice(t *testing.T) {
	f := newArchiveMaintenanceFixture(t, 3, 5)
	ioniceLog := filepath.Join(t.TempDir(), "ionice.log")
	f.env["IONICE_LOG"] = ioniceLog
	writeExecutable(t, filepath.Join(f.binDir, "ionice"), `#!/bin/sh
printf '%s\n' "$*" >> "$IONICE_LOG"
[ "$1" = -t ] && [ "$2" = -c ] && [ "$3" = 3 ] || exit 2
shift 3
exec "$@"
`)
	f.limitPackingToSnapshot(t)

	out, err := f.run(t)
	if err != nil {
		t.Fatalf("jsonl-export.sh failed: %v\n%s", err, out)
	}
	calls, err := os.ReadFile(ioniceLog)
	if err != nil {
		t.Fatalf("ReadFile(ionice log): %v\n%s", err, out)
	}
	if want := "-t -c 3 nice -n 19 git -C " + f.archiveRepo + " "; !strings.Contains(string(calls), want) {
		t.Fatalf("expected git repack to run as ionice %s...; ionice calls:\n%s", want, calls)
	}
	if got := readArchiveObjectStats(t, f.archiveRepo).looseObjects; got != 0 {
		t.Fatalf("loose objects after packing = %d, want 0:\n%s", got, out)
	}
}

// A run with nothing to export still packs a backlog above the limit, after
// pushing the snapshot an earlier run left unpushed. A failed repack fails the
// run without MAINTENANCE_DONE, with the pending snapshot already pushed.
func TestJsonlExportPacksBacklogWhenThereIsNothingToExport(t *testing.T) {
	for _, tc := range []struct {
		name        string
		pendingPush bool
		repackFails bool
	}{
		{"no changes", false, false},
		{"no changes with repack failing", false, true},
		{"pending push", true, false},
		{"pending push with repack failing", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var f archiveMaintenanceFixture
			var remoteRepo string
			if tc.pendingPush {
				// The first run halts on the spike: it commits locally and
				// skips the push. The second run's data is unchanged.
				f = newArchiveMaintenanceFixture(t, 100, 10)
				remoteRepo = f.addOrigin(t)
				f.exportOnce(t)
			} else {
				// The first run pushes, so it clears the pending push; the
				// second run then has nothing to push either and leaves by
				// the "No changes." exit. Without an origin the flag is never
				// cleared and the second run takes the pending-push exit.
				f = newArchiveMaintenanceFixture(t, 3, 5)
				remoteRepo = f.addOrigin(t)
				f.exportOnce(t)
			}
			f.seedLargePackAndBacklog(t)
			if !tc.pendingPush {
				// The backlog commits must not read as unpushed work.
				runGit(t, f.archiveRepo, "push", "-q", "origin", "main")
			}
			// Any backlog commits sit on top of the unpushed snapshot.
			headBefore := f.head(t)
			wantPushed := headBefore
			f.env["GC_JSONL_GC_LOOSE_OBJECTS"] = "1"
			f.startNewGCCallLog(t)
			if tc.repackFails {
				f.failGitRepack(t)
			}

			out, err := f.run(t)
			if f.head(t) != headBefore {
				t.Fatalf("a run with nothing to export must not commit:\n%s", out)
			}
			if remoteRepo != "" {
				if got := f.originMain(t, remoteRepo); got != wantPushed {
					t.Fatalf("origin main = %s, want the pending snapshot %s pushed first:\n%s", got, wantPushed, out)
				}
			}
			if tc.repackFails {
				if err == nil || !strings.Contains(out, "archive object database maintenance failed") {
					t.Fatalf("expected the failed repack to fail the run (err=%v):\n%s", err, out)
				}
				if strings.Contains(f.gcCalls(t), "MAINTENANCE_DONE") {
					t.Fatalf("a failed run must not send MAINTENANCE_DONE:\n%s", f.gcCalls(t))
				}
				return
			}
			if err != nil {
				t.Fatalf("jsonl-export.sh failed: %v\n%s", err, out)
			}
			if got := readArchiveObjectStats(t, f.archiveRepo).looseObjects; got != 0 {
				t.Fatalf("loose objects after packing = %d, want 0:\n%s", got, out)
			}
			if !strings.Contains(f.gcCalls(t), "MAINTENANCE_DONE") {
				t.Fatalf("expected MAINTENANCE_DONE after a successful pass:\n%s", f.gcCalls(t))
			}
		})
	}
}

// Maintenance leaves the archive's large pack alone and packs the loose
// backlog beside it.
func TestJsonlExportArchiveMaintenanceKeepsLargePackAsIs(t *testing.T) {
	f := newArchiveMaintenanceFixture(t, 3, 5)
	f.exportOnce(t)
	largestPack := f.seedLargePackAndBacklog(t)
	largestSum := fileSHA256(t, largestPack)
	headBefore := f.head(t)
	f.env["GC_JSONL_GC_LOOSE_OBJECTS"] = "1"

	out, err := f.run(t)
	if err != nil {
		t.Fatalf("jsonl-export.sh failed: %v\n%s", err, out)
	}
	if fileSHA256(t, largestPack) != largestSum {
		t.Fatalf("the large pack %s was rewritten:\n%s", largestPack, out)
	}
	if got := readArchiveObjectStats(t, f.archiveRepo).looseObjects; got != 0 {
		t.Fatalf("loose objects after packing = %d, want 0:\n%s", got, out)
	}
	runGit(t, f.archiveRepo, "fsck")
	if head := f.head(t); head != headBefore {
		t.Fatalf("maintenance moved HEAD from %s to %s", headBefore, head)
	}
}

// Packs too alike in size to form a geometric progression merge among
// themselves; the large pack still stays as it is.
func TestJsonlExportArchiveMaintenanceMergesSmallPacksBesideLargePack(t *testing.T) {
	f := newArchiveMaintenanceFixture(t, 3, 5)
	f.exportOnce(t)
	largestPack := f.seedLargePackAndBacklog(t)
	for i := 0; i < 5; i++ {
		runGit(t, f.archiveRepo, "repack", "-q", "-d")
		commitArchiveBloat(t, f.archiveRepo, 4)
	}
	largestSum := fileSHA256(t, largestPack)
	before := readArchiveObjectStats(t, f.archiveRepo)
	headBefore := f.head(t)
	f.env["GC_JSONL_GC_LOOSE_OBJECTS"] = "1"

	out, err := f.run(t)
	if err != nil {
		t.Fatalf("jsonl-export.sh failed: %v\n%s", err, out)
	}
	if fileSHA256(t, largestPack) != largestSum {
		t.Fatalf("the large pack %s was rewritten:\n%s", largestPack, out)
	}
	after := readArchiveObjectStats(t, f.archiveRepo)
	if after.looseObjects != 0 || after.packs >= before.packs {
		t.Fatalf("expected the loose backlog packed and the small packs merged: before %+v, after %+v\n%s", before, after, out)
	}
	runGit(t, f.archiveRepo, "fsck")
	if head := f.head(t); head != headBefore {
		t.Fatalf("maintenance moved HEAD from %s to %s", headBefore, head)
	}
}

// The repack is a geometric one, run under the archive's own window-memory
// and thread settings, and git's own maintenance stays out of the archive:
// the snapshot commit turns it off for that call only.
func TestJsonlExportArchiveRepackCommandAndGitAutoMaintenance(t *testing.T) {
	f := newArchiveMaintenanceFixture(t, 3, 5)
	argvLog := filepath.Join(t.TempDir(), "git-argv.log")
	writeGitArgvLogStub(t, f.binDir, lookPathGit(t), argvLog)
	f.limitPackingToSnapshot(t)
	f.env["GC_JSONL_GC_WINDOW_MEMORY"] = "64M"
	configBefore := runGitOut(t, f.archiveRepo, "config", "--local", "--list")

	out, err := f.run(t)
	if err != nil {
		t.Fatalf("jsonl-export.sh failed: %v\n%s", err, out)
	}
	data, err := os.ReadFile(argvLog)
	if err != nil {
		t.Fatalf("ReadFile(git argv log): %v", err)
	}
	log := string(data)
	if want := "ARGV -C " + f.archiveRepo + " -c pack.windowMemory=64M -c pack.threads=1 repack -d -l -q --geometric=2"; !strings.Contains(log, want+"\n") {
		t.Fatalf("expected the repack %q in the git calls:\n%s", want, log)
	}
	if !strings.Contains(log, "ARGV -c maintenance.auto=false -c gc.auto=0 commit ") {
		t.Fatalf("expected the snapshot commit to turn off git's own maintenance:\n%s", log)
	}
	if regexp.MustCompile(`(?m)^ARGV (.* )?gc( |$)`).MatchString(log) {
		t.Fatalf("the archive must not run git gc:\n%s", log)
	}
	if strings.Contains(log, "ENV ") {
		t.Fatalf("the script must not inject git configuration through GIT_CONFIG_*:\n%s", log)
	}
	if config := runGitOut(t, f.archiveRepo, "config", "--local", "--list"); config != configBefore {
		t.Fatalf("the script must not write to the archive config:\nbefore the run:\n%s\nafter:\n%s", configBefore, config)
	}
}

// The archive's fetches and rebases turn git's own maintenance off for each
// call, as the snapshot commit does. Origin advances before the run and again
// during its first push, so the run fetches and rebases both before its push
// and on the retry.
func TestJsonlExportArchiveFetchAndRebaseDisableGitAutoMaintenance(t *testing.T) {
	f := newArchiveMaintenanceFixture(t, 3, 5)
	remoteRepo := f.addOrigin(t)
	realGit := lookPathGit(t)
	advanceDir := filepath.Join(t.TempDir(), "advance")
	runGit(t, f.archiveRepo, "clone", "-q", remoteRepo, advanceDir)
	runGit(t, advanceDir, "checkout", "-q", "main")
	runGit(t, advanceDir, "config", "user.email", "test@example.invalid")
	runGit(t, advanceDir, "config", "user.name", "test")
	if err := os.WriteFile(filepath.Join(advanceDir, "remote-advance.txt"), []byte("remote advance before the run\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(remote-advance.txt): %v", err)
	}
	runGit(t, advanceDir, "add", "-A")
	runGit(t, advanceDir, "commit", "-q", "-m", "remote advance before the run")
	runGit(t, advanceDir, "push", "-q", "origin", "main")
	raceDir := t.TempDir()
	writeGitPushRemoteAdvanceRaceStub(t, raceDir, realGit, remoteRepo, filepath.Join(t.TempDir(), "git-push.log"))
	argvLog := filepath.Join(t.TempDir(), "git-argv.log")
	writeGitArgvLogStub(t, f.binDir, filepath.Join(raceDir, "git"), argvLog)
	configBefore := runGitOut(t, f.archiveRepo, "config", "--local", "--list")

	out, err := f.run(t)
	if err != nil {
		t.Fatalf("jsonl-export.sh failed: %v\n%s", err, out)
	}
	if !strings.Contains(out, "push succeeded on retry attempt 2") {
		t.Fatalf("expected the push to succeed on its retry:\n%s", out)
	}
	data, err := os.ReadFile(argvLog)
	if err != nil {
		t.Fatalf("ReadFile(git argv log): %v", err)
	}
	log := string(data)
	calls := map[string]int{}
	for _, line := range strings.Split(log, "\n") {
		match := regexp.MustCompile(`^ARGV (?:.* )?(fetch|rebase)(?: |$)`).FindStringSubmatch(line)
		if match == nil {
			continue
		}
		calls[match[1]]++
		if !strings.Contains(line, "-c maintenance.auto=false -c gc.auto=0 "+match[1]+" ") {
			t.Errorf("the %s must turn off git's own maintenance: %q", match[1], line)
		}
	}
	if calls["fetch"] < 2 || calls["rebase"] < 2 {
		t.Fatalf("expected a fetch and rebase before the push and on its retry, got %v:\n%s", calls, log)
	}
	if strings.Contains(log, "ENV ") {
		t.Fatalf("the script must not inject git configuration through GIT_CONFIG_*:\n%s", log)
	}
	if config := runGitOut(t, f.archiveRepo, "config", "--local", "--list"); config != configBefore {
		t.Fatalf("the script must not write to the archive config:\nbefore the run:\n%s\nafter:\n%s", configBefore, config)
	}
	if got := f.originMain(t, remoteRepo); got != f.head(t) {
		t.Fatalf("origin main = %s, want the rebased snapshot %s pushed:\n%s", got, f.head(t), out)
	}
}

// A git without --geometric (before 2.32) fails the pass through the normal
// failure path and names the version it needs.
func TestJsonlExportArchiveMaintenanceNamesGitVersionWhenRepackRejectsGeometric(t *testing.T) {
	f := newArchiveMaintenanceFixture(t, 3, 5)
	realGit := lookPathGit(t)
	writeExecutable(t, filepath.Join(f.binDir, "git"), fmt.Sprintf(`#!/bin/sh
for arg in "$@"; do
    case "$arg" in
    --geometric=*)
        echo "error: unknown option geometric=2" >&2
        exit 129
        ;;
    esac
done
exec '%s' "$@"
`, realGit))
	f.limitPackingToSnapshot(t)
	seedHead := f.head(t)

	out, err := f.run(t)
	if err == nil {
		t.Fatalf("expected a git without --geometric to fail the run:\n%s", out)
	}
	for _, want := range []string{"archive object database maintenance failed", "git 2.32"} {
		if !strings.Contains(out, want) {
			t.Fatalf("expected %q in the failure:\n%s", want, out)
		}
	}
	if f.head(t) == seedHead {
		t.Fatalf("the snapshot must be committed before maintenance fails:\n%s", out)
	}
}

// A pass that reaches the repack reports what it did; a failing pass reports
// what it knew before it failed.
func TestJsonlExportArchiveMaintenanceReportsTelemetry(t *testing.T) {
	t.Run("working pass", func(t *testing.T) {
		f := newArchiveMaintenanceFixture(t, 3, 5)
		f.exportOnce(t)
		f.seedLargePackAndBacklog(t)
		before := readArchiveObjectStats(t, f.archiveRepo)
		f.env["GC_JSONL_GC_LOOSE_OBJECTS"] = "1"

		out, err := f.run(t)
		if err != nil {
			t.Fatalf("jsonl-export.sh failed: %v\n%s", err, out)
		}
		match := archiveMaintenanceTelemetry.FindStringSubmatch(out)
		if match == nil {
			t.Fatalf("expected the maintenance report line:\n%s", out)
		}
		after := readArchiveObjectStats(t, f.archiveRepo)
		want := []string{"geometric", "", strconv.Itoa(before.looseObjects), "0", strconv.Itoa(before.packs), strconv.Itoa(after.packs), strconv.Itoa(after.packKB)}
		for i, field := range []string{"mode", "wall", "loose_before", "loose_after", "packs_before", "packs_after", "pack_kb_after"} {
			if field != "wall" && match[i+1] != want[i] {
				t.Errorf("%s = %s, want %s\n%s", field, match[i+1], want[i], out)
			}
		}
	})
	t.Run("failing pass", func(t *testing.T) {
		f := newArchiveMaintenanceFixture(t, 3, 5)
		f.limitPackingToSnapshot(t)
		f.failGitRepack(t)

		out, err := f.run(t)
		if err == nil {
			t.Fatalf("expected the failed repack to fail the run:\n%s", out)
		}
		if !regexp.MustCompile(`jsonl-export: archive maintenance: mode=geometric wall=\d+s loose_before=\d+ .*status=failed`).MatchString(out) {
			t.Fatalf("expected the failing pass to report its fields and status=failed:\n%s", out)
		}
	})
}

// gateSweepEnv constructs the env for a gate-sweep.sh invocation with a
// PATH-shimmed bd stub that logs every call to BD_LOG.
func gateSweepEnv(t *testing.T) (binDir, bdLog string, env map[string]string) {
	t.Helper()
	binDir = t.TempDir()
	bdLog = filepath.Join(t.TempDir(), "bd.log")
	writeMaintenanceGCStub(t, filepath.Join(binDir, "gc"), "#!/bin/sh\nexit 0\n")
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

	runScript(t, coreScriptPath("gate-sweep.sh"), env)

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

	runScript(t, coreScriptPath("gate-sweep.sh"), env)

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

	script := coreScriptPath("gate-sweep.sh")
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

	script := coreScriptPath("gate-sweep.sh")
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

	script := coreScriptPath("prune-branches.sh")
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

	script := coreScriptPath("prune-branches.sh")
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

	script := coreScriptPath("prune-branches.sh")
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

	script := coreScriptPath("prune-branches.sh")
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
	writeMaintenanceGCStub(t, filepath.Join(binDir, "gc"), "#!/bin/sh\nexit 0\n")

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
	runScript(t, coreScriptPath("wisp-compact.sh"), env)

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
	out, err := runScriptResult(t, coreScriptPath("wisp-compact.sh"), env)
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
	runScript(t, coreScriptPath("wisp-compact.sh"), env)

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
	runScript(t, coreScriptPath("wisp-compact.sh"), env)

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
	runScript(t, coreScriptPath("wisp-compact.sh"), env)

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
	runScript(t, coreScriptPath("wisp-compact.sh"), env)

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
	runScript(t, coreScriptPath("wisp-compact.sh"), env)

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
//   - `bd query 'status=closed AND closed>"..."' --json --limit 0` → returns closedJSON
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
	// pins the script's bd contract; a regression dropping --json or
	// --limit 0 (bd before v1.3.1 caps an unflagged query at 50 rows) from
	// `bd query`, or --type=blocks from `bd dep list`, would otherwise still
	// pass.
	writeExecutable(t, stubPath, fmt.Sprintf(`#!/bin/sh
case "$1" in
  query)
    case "$*" in
      *"status=closed AND closed>"*"--json"*"--limit 0"*)
        cat <<'EOF'
%s
EOF
        exit 0
        ;;
      *)
        echo "bd query called with unexpected args: $*" >&2
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
	writeMaintenanceGCStub(t, filepath.Join(binDir, "gc"), "#!/bin/sh\nexit 0\n")

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
	runScript(t, coreScriptPath("cross-rig-deps.sh"), env)

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
	out, err := runScriptResult(t, coreScriptPath("cross-rig-deps.sh"), env)
	if err != nil {
		t.Fatalf("cross-rig-deps.sh failed: %v\n%s", err, out)
	}
	// The script passes `gc bd query` stderr through so a bd refusal reaches
	// the order log, so the fake gc's stderr warning shares the combined
	// output; everything else must be exactly the summary line.
	got := strings.TrimSpace(strings.ReplaceAll(string(out), maintenanceGCStderrNoise+"\n", ""))
	if want := "cross-rig-deps: resolved 1 cross-rig dependencies"; got != want {
		t.Fatalf("summary = %q, want %q", got, want)
	}
}

func TestCrossRigDepsSkipsInternalDeps(t *testing.T) {
	closed := `[{"id":"ga-blocker"}]`
	// Internal deps lack the "external:" prefix and must be left untouched
	// — internal blocking semantics are bd's normal computeBlockedIDs path.
	deps := `[{"id":"local-rig-dep"},{"id":"another-internal"}]`

	bdLog, env := crossRigDepsEnv(t, closed, deps)
	runScript(t, coreScriptPath("cross-rig-deps.sh"), env)

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
	runScript(t, coreScriptPath("cross-rig-deps.sh"), env)

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
	runScript(t, coreScriptPath("cross-rig-deps.sh"), env)

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
	writeMaintenanceGCStub(t, filepath.Join(binDir, "gc"), "#!/bin/sh\nexit 0\n")

	env := map[string]string{
		"BD_LOG":       bdLog,
		"GC_CITY":      cityDir,
		"GC_CITY_PATH": cityDir,
		"PATH":         binDir + string(os.PathListSeparator) + os.Getenv("PATH"),
	}

	script := coreScriptPath("wisp-compact.sh")
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
	writeMaintenanceGCStub(t, filepath.Join(binDir, "gc"), "#!/bin/sh\nexit 0\n")

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

	script := coreScriptPath("wisp-compact.sh")
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
  query)
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
	writeMaintenanceGCStub(t, filepath.Join(binDir, "gc"), "#!/bin/sh\nexit 0\n")

	env := map[string]string{
		"BD_LOG":       bdLog,
		"GC_CITY":      cityDir,
		"GC_CITY_PATH": cityDir,
		"PATH":         binDir + string(os.PathListSeparator) + os.Getenv("PATH"),
	}

	script := coreScriptPath("cross-rig-deps.sh")
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

// orphanSweepGateStubArm answers `gc agent is-foreign … <identity>` (the last
// argument) the way this city's foreign-identity gate would: foreign (exit 1)
// for identities listed in ORPHAN_SWEEP_FOREIGN_IDENTITIES, otherwise local
// (exit 0), modeling a dead local claimant whose closed session record
// survives. An empty identity gets the verb's usage answer (exit 2, unknown).
// ORPHAN_SWEEP_GATE_SLEEP makes every answer take that many seconds, modeling
// a slow store. ORPHAN_SWEEP_GATE_LINGER keeps the gate running that many
// seconds after it prints a local answer, modeling a gc that answered but hung
// on its way out. It uses echo, not printf, so it is safe inside fmt.Sprintf
// stubs.
const orphanSweepGateStubArm = `if [ "$1" = "agent" ] && [ "$2" = "is-foreign" ]; then
  if [ -n "${ORPHAN_SWEEP_GATE_SLEEP:-}" ]; then sleep "$ORPHAN_SWEEP_GATE_SLEEP"; fi
  for identity; do :; done
  if [ -z "$identity" ] || [ "$identity" = "--" ]; then
    echo '{"schema_version":"1","ok":true,"verdict":"unknown","reason":"no_config","detail":"usage: exactly one identity is required","roster_source":""}'
    exit 2
  fi
  if [ -n "${ORPHAN_SWEEP_FOREIGN_IDENTITIES:-}" ] && echo "$ORPHAN_SWEEP_FOREIGN_IDENTITIES" | grep -Fxq -- "$identity"; then
    echo '{"schema_version":"1","ok":true,"verdict":"foreign","reason":"absent_from_roster","roster_source":"stub"}'
    exit 1
  fi
  echo '{"schema_version":"1","ok":true,"verdict":"local","reason":"session_record","roster_source":"stub"}'
  if [ -n "${ORPHAN_SWEEP_GATE_LINGER:-}" ]; then sleep "$ORPHAN_SWEEP_GATE_LINGER"; fi
  exit 0
fi
`

// orphanSweepSharedRigFakeGC fakes a city that shares the rig store "shared"
// with a neighboring city (gastownhall/gascity#5955). This city configures
// gastown.deacon and shared/gastown.polecat; nothing is live. The in-progress
// rows come from ORPHAN_SWEEP_HQ_BEADS_JSON and ORPHAN_SWEEP_SHARED_BEADS_JSON,
// and `bd show <id> --json` answers only for those work beads and for the
// session beads in ORPHAN_SWEEP_SESSION_BEADS_JSON, so every other
// session-bead probe the sweep derives from an assignee finds nothing, exactly
// as it does for a neighbor's claim whose session record lives in the
// neighbor's own unfederated HQ.
//
// `agent is-foreign [flags] [--] <identity>` stands in for this city's
// foreign-identity gate (orphanSweepGateStubArm). ORPHAN_SWEEP_GATE_EXIT makes
// it answer "unknown" with that exit code instead, and ORPHAN_SWEEP_GATE_ABSENT
// models a binary without the verb (exit 1, no JSON). ORPHAN_SWEEP_MAIL_EXIT
// is the exit code of `mail send`.
const orphanSweepSharedRigFakeGC = `#!/bin/sh
printf '%s\n' "$*" >> "$GC_CALL_LOG"
if [ "$1" = "agent" ] && [ -n "${ORPHAN_SWEEP_GATE_ABSENT:-}" ]; then
  echo "gc agent: unknown subcommand \"$2\"" >&2
  exit 1
fi
if [ "$1" = "agent" ] && [ -n "${ORPHAN_SWEEP_GATE_EXIT:-}" ]; then
  echo '{"schema_version":"1","ok":true,"verdict":"unknown","reason":"lookup_failed","detail":"session store unreachable","roster_source":"stub"}'
  exit "$ORPHAN_SWEEP_GATE_EXIT"
fi
` + orphanSweepGateStubArm + `if [ "$1" = "--rig" ]; then
  shift 2
fi
case "$1" in
  mail)
    exit "${ORPHAN_SWEEP_MAIL_EXIT:-0}"
    ;;
  config)
    if [ "$2" = "explain" ]; then
      cat <<'EOF'
Agent: gastown.deacon
  source: pack
Agent: shared/gastown.polecat
  source: pack
EOF
      exit 0
    fi
    ;;
  rig)
    if [ "$2" = "list" ] && [ "$3" = "--json" ]; then
      printf '{"rigs":[{"name":"hq","hq":true},{"name":"shared","hq":false}]}\n'
      exit 0
    fi
    ;;
  session)
    if [ "$2" = "list" ] && [ "$3" = "--json" ]; then
      printf '{"sessions":[{"id":"orphan-sweep-test-keepalive","session_name":"orphan-sweep-test-keepalive","closed":false}],"summary":{},"filters":{},"schema_version":"1"}\n'
      exit 0
    fi
    ;;
  bd)
    if [ "$2" = "list" ]; then
      if [ "$3" = "--rig" ] && [ "$4" = "shared" ]; then
        printf '%s\n' "${ORPHAN_SWEEP_SHARED_BEADS_JSON:-[]}"
      else
        printf '%s\n' "${ORPHAN_SWEEP_HQ_BEADS_JSON:-[]}"
      fi
      exit 0
    fi
    if [ "$2" = "show" ] && [ "$4" = "--json" ]; then
      row=$(printf '%s\n%s\n%s\n' "${ORPHAN_SWEEP_HQ_BEADS_JSON:-[]}" "${ORPHAN_SWEEP_SHARED_BEADS_JSON:-[]}" "${ORPHAN_SWEEP_SESSION_BEADS_JSON:-[]}" | jq -c --arg id "$3" '.[] | select(.id == $id)' | head -n 1)
      if [ -n "$row" ]; then
        printf '[%s]\n' "$row"
        exit 0
      fi
      exit 1
    fi
    if [ "$2" = "release-if-current" ]; then
      printf 'released\n'
      exit 0
    fi
    if [ "$2" = "update" ]; then
      exit 0
    fi
    ;;
esac
exit 1
`

// TestOrphanSweepKeepsForeignClaimsOnSharedRigAndReleasesDeadLocal is the
// gastownhall/gascity#5955 regression, with no opt-in configuration: ONE sweep
// over a shared rig carries claims held by a neighboring city's identities in
// every shape a claim can take, a dead local claimant, and a live-roster
// control.
//
// The foreign shapes are the ones measured in the field or minted by gc for an
// unaliased session: a foreign-binding pool instance (ga-8yi7ne), a foreign
// named crew member (the #5952 incident), a runtime session name
// (PoolSessionName, <base>-<bead id>), a bare session bead ID (ga-x1f77i), and
// a rig-encoded session name ("--" for "/", "__" for ".").
//
// Split across sweeps, "keeps every foreign claim" is satisfiable by a sweep
// that releases nothing and "releases the dead local claim" by one that
// releases everything. Together in one sweep they are not. The skip must also
// be LOUD: counted and named on stdout, and declared as a partial outcome so
// the controller records an order.skipped event on an exit-0 run (exec-order
// output is otherwise logged only on a non-zero exit, #4543). A decommissioned
// local agent is also absent from the roster, and a silent skip would turn a
// loud cross-city bug into a quiet local leak.
func TestOrphanSweepKeepsForeignClaimsOnSharedRigAndReleasesDeadLocal(t *testing.T) {
	foreign := []struct {
		beadID   string
		assignee string
	}{
		{"sh-foreign-pool", "shared/otherpack.polecat-3"},
		{"sh-foreign-crew", "shared/dalinar"},
		{"sh-foreign-session", "otherpack__polecat-bt-8x2kd"},
		{"sh-foreign-bead-id", "bt-q7m3v"},
		{"sh-foreign-rig-session", "shared--otherpack__polecat-4"},
	}
	const (
		localDeadID       = "sh-local-dead"
		localDeadAssignee = "gastown__polecat-gc-d34d1"
		rosterID          = "sh-local-roster"
		rosterAssignee    = "shared/gastown.polecat-2"
	)

	type row struct {
		ID       string `json:"id"`
		Status   string `json:"status"`
		Assignee string `json:"assignee"`
	}
	rows := []row{
		{ID: localDeadID, Status: "in_progress", Assignee: localDeadAssignee},
		{ID: rosterID, Status: "in_progress", Assignee: rosterAssignee},
	}
	foreignIdentities := make([]string, 0, len(foreign))
	for _, f := range foreign {
		rows = append(rows, row{ID: f.beadID, Status: "in_progress", Assignee: f.assignee})
		foreignIdentities = append(foreignIdentities, f.assignee)
	}
	sharedJSON, err := json.Marshal(rows)
	if err != nil {
		t.Fatalf("Marshal(shared rig rows): %v", err)
	}

	cityDir := t.TempDir()
	binDir := t.TempDir()
	gcLog := filepath.Join(t.TempDir(), "gc.log")
	outcomeFile := filepath.Join(t.TempDir(), "outcome.json")
	writeExecutable(t, filepath.Join(binDir, "gc"), orphanSweepSharedRigFakeGC)

	out, err := runScriptResult(t, coreScriptPath("orphan-sweep.sh"), map[string]string{
		"GC_CITY":                         cityDir,
		"GC_CITY_PATH":                    cityDir,
		"GC_CALL_LOG":                     gcLog,
		"GC_ORDER_OUTCOME_FILE":           outcomeFile,
		"ORPHAN_SWEEP_HQ_BEADS_JSON":      "[]",
		"ORPHAN_SWEEP_SHARED_BEADS_JSON":  string(sharedJSON),
		"ORPHAN_SWEEP_FOREIGN_IDENTITIES": strings.Join(foreignIdentities, "\n"),
		"PATH":                            binDir + string(os.PathListSeparator) + os.Getenv("PATH"),
	})
	if err != nil {
		t.Fatalf("orphan-sweep failed: %v\n%s", err, orphanSweepFailureContext(out, gcLog))
	}
	logData, err := os.ReadFile(gcLog)
	if err != nil {
		t.Fatalf("ReadFile(gc log): %v", err)
	}
	log := string(logData)

	var released []string
	for _, f := range foreign {
		if strings.Contains(log, "bd release-if-current "+f.beadID+" ") {
			released = append(released, f.beadID+" ("+f.assignee+")")
		}
	}
	if len(released) > 0 {
		t.Fatalf("orphan-sweep released claims held by another city's identities on the shared rig: %s\n%s",
			strings.Join(released, ", "), orphanSweepFailureContext(out, gcLog))
	}
	if !strings.Contains(log, "bd release-if-current "+localDeadID+" "+localDeadAssignee) {
		t.Fatalf("dead local claimant %s was not released\n%s", localDeadAssignee, orphanSweepFailureContext(out, gcLog))
	}
	if strings.Contains(log, "bd release-if-current "+rosterID+" ") {
		t.Fatalf("roster-known claimant %s was released\n%s", rosterAssignee, orphanSweepFailureContext(out, gcLog))
	}

	// The gate is asked exactly once about every claimant the roster and live
	// sessions did not recognize, and never about one they did.
	gateCalls := func(identity string) int {
		n := 0
		for _, line := range nonEmptyLogLines(log) {
			if strings.HasPrefix(line, "agent is-foreign ") && strings.HasSuffix(line, " "+identity) {
				n++
			}
		}
		return n
	}
	for _, f := range foreign {
		if got := gateCalls(f.assignee); got != 1 {
			t.Errorf("foreign-identity gate consulted %d times for %q, want 1\n%s", got, f.assignee, orphanSweepFailureContext(out, gcLog))
		}
	}
	if got := gateCalls(rosterAssignee); got != 0 {
		t.Errorf("foreign-identity gate consulted %d times for roster-known %q, want 0\n%s", got, rosterAssignee, orphanSweepFailureContext(out, gcLog))
	}
	// Like the Go sweep, the gate runs before revalidation and liveness
	// probes, so a protected claim costs no per-bead reads.
	for _, f := range foreign {
		if strings.Contains(log, "bd show "+f.beadID+" --json") {
			t.Errorf("protected claim %s was revalidated/probed before the gate\n%s", f.beadID, orphanSweepFailureContext(out, gcLog))
		}
	}

	text := string(out)
	for _, want := range []string{
		"orphan-sweep: reset 1 orphaned beads",
		"orphan-sweep: protected 5 foreign/unknown identities this pass (5 claims)",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("output missing %q:\n%s", want, text)
		}
	}
	for _, f := range foreign {
		if want := strconv.Quote(f.assignee) + " (1: " + f.beadID + ")"; !strings.Contains(text, want) {
			t.Errorf("summary does not name protected identity and claim %s:\n%s", want, text)
		}
	}
	if strings.Contains(text, strconv.Quote(localDeadAssignee)+" (") {
		t.Errorf("released local claimant %q must not be reported as protected:\n%s", localDeadAssignee, text)
	}

	data, err := os.ReadFile(outcomeFile)
	if err != nil || strings.TrimSpace(string(data)) == "" {
		t.Fatalf("no outcome declaration written (err=%v): a sweep that protected claims and exited 0 must declare a partial outcome\n%s", err, text)
	}
	var outcome scriptOutcome
	if err := json.Unmarshal(data, &outcome); err != nil {
		t.Fatalf("outcome file is not the typed declaration: %v\n%s", err, data)
	}
	if outcome.Outcome != "partial" {
		t.Errorf("outcome = %q, want partial (the sweep ran; a safety gate held claims back)", outcome.Outcome)
	}
	for _, f := range foreign {
		found := false
		for _, scope := range outcome.Scopes {
			if scope.Scope == f.assignee && strings.Contains(scope.Reason, f.beadID) {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("outcome declares no scope for protected identity %q naming %s: %+v", f.assignee, f.beadID, outcome.Scopes)
		}
	}
}

// orphanSweepGateRow is one in-progress work bead, or one session bead served
// by `bd show`, in an orphanSweepSharedRigFakeGC fixture.
type orphanSweepGateRow struct {
	ID        string `json:"id"`
	Status    string `json:"status"`
	Assignee  string `json:"assignee,omitempty"`
	IssueType string `json:"issue_type,omitempty"`
}

// orphanSweepGateRun is one orphanSweepSharedRigFakeGC sweep's results.
type orphanSweepGateRun struct {
	out         string
	err         error
	log         string
	outcomeFile string
	gcLog       string
}

func (r orphanSweepGateRun) context() string {
	return orphanSweepFailureContext([]byte(r.out), r.gcLog)
}

func (r orphanSweepGateRun) released(beadID string) bool {
	return strings.Contains(r.log, "bd release-if-current "+beadID+" ")
}

func (r orphanSweepGateRun) gateCalls() int {
	n := 0
	for _, line := range nonEmptyLogLines(r.log) {
		if strings.HasPrefix(line, "agent is-foreign ") {
			n++
		}
	}
	return n
}

// outcome reads the typed outcome declaration; ok is false when none was
// written.
func (r orphanSweepGateRun) outcome(t *testing.T) (scriptOutcome, bool) {
	t.Helper()
	data, err := os.ReadFile(r.outcomeFile)
	if errors.Is(err, os.ErrNotExist) || (err == nil && strings.TrimSpace(string(data)) == "") {
		return scriptOutcome{}, false
	}
	if err != nil {
		t.Fatalf("ReadFile(outcome): %v", err)
	}
	var outcome scriptOutcome
	if err := json.Unmarshal(data, &outcome); err != nil {
		t.Fatalf("outcome file is not the typed declaration: %v\n%s", err, data)
	}
	return outcome, true
}

func orphanSweepGateJSON(t *testing.T, rows []orphanSweepGateRow) string {
	t.Helper()
	if rows == nil {
		return "[]"
	}
	data, err := json.Marshal(rows)
	if err != nil {
		t.Fatalf("Marshal(rows): %v", err)
	}
	return string(data)
}

// runOrphanSweepGateFixture runs one sweep over orphanSweepSharedRigFakeGC.
func runOrphanSweepGateFixture(t *testing.T, hq, shared, sessionBeads []orphanSweepGateRow, foreign []string, extraEnv map[string]string) orphanSweepGateRun {
	t.Helper()
	cityDir := t.TempDir()
	binDir := t.TempDir()
	gcLog := filepath.Join(t.TempDir(), "gc.log")
	outcomeFile := filepath.Join(t.TempDir(), "outcome.json")
	writeExecutable(t, filepath.Join(binDir, "gc"), orphanSweepSharedRigFakeGC)
	env := map[string]string{
		"GC_CITY":                         cityDir,
		"GC_CITY_PATH":                    cityDir,
		"GC_CALL_LOG":                     gcLog,
		"GC_ORDER_OUTCOME_FILE":           outcomeFile,
		"ORPHAN_SWEEP_HQ_BEADS_JSON":      orphanSweepGateJSON(t, hq),
		"ORPHAN_SWEEP_SHARED_BEADS_JSON":  orphanSweepGateJSON(t, shared),
		"ORPHAN_SWEEP_SESSION_BEADS_JSON": orphanSweepGateJSON(t, sessionBeads),
		"ORPHAN_SWEEP_FOREIGN_IDENTITIES": strings.Join(foreign, "\n"),
		"ORPHAN_SWEEP_GATE_EXIT":          "",
		"ORPHAN_SWEEP_GATE_ABSENT":        "",
		"ORPHAN_SWEEP_MAIL_EXIT":          "0",
		"PATH":                            binDir + string(os.PathListSeparator) + os.Getenv("PATH"),
	}
	for k, v := range extraEnv {
		env[k] = v
	}
	out, err := runScriptResult(t, coreScriptPath("orphan-sweep.sh"), env)
	logData, readErr := os.ReadFile(gcLog)
	if readErr != nil && !errors.Is(readErr, os.ErrNotExist) {
		t.Fatalf("ReadFile(gc log): %v", readErr)
	}
	return orphanSweepGateRun{out: string(out), err: err, log: string(logData), outcomeFile: outcomeFile, gcLog: gcLog}
}

func inProgress(id, assignee string) orphanSweepGateRow {
	return orphanSweepGateRow{ID: id, Status: "in_progress", Assignee: assignee}
}

// TestOrphanSweepProtectsHQClaimsItCannotVouchFor applies the gate in HQ, this
// city's own store, as the Go sweep does for every store: a claim under an
// identity this city cannot vouch for (a person, a typo, a decommissioned
// agent) is kept and named, while a dead local claim whose closed session
// record answers to it is still released.
func TestOrphanSweepProtectsHQClaimsItCannotVouchFor(t *testing.T) {
	run := runOrphanSweepGateFixture(t,
		[]orphanSweepGateRow{inProgress("hq-retired", "retired-hand"), inProgress("hq-local-dead", "gc-d34d2")},
		nil,
		[]orphanSweepGateRow{{ID: "gc-d34d2", Status: "closed", IssueType: "session"}},
		[]string{"retired-hand"}, nil)
	if run.err != nil {
		t.Fatalf("orphan-sweep failed: %v\n%s", run.err, run.context())
	}
	if run.released("hq-retired") {
		t.Fatalf("HQ claim held by an identity this city cannot vouch for was released\n%s", run.context())
	}
	if !run.released("hq-local-dead") {
		t.Fatalf("dead local HQ claim was not released\n%s", run.context())
	}
	if want := `"retired-hand" (1: hq-retired)`; !strings.Contains(run.out, want) {
		t.Fatalf("output does not name the protected HQ claim %s:\n%s", want, run.out)
	}
	outcome, ok := run.outcome(t)
	if !ok || outcome.Outcome != "partial" {
		t.Fatalf("outcome = %+v (written %v), want partial\n%s", outcome, ok, run.context())
	}
	if len(outcome.Scopes) != 1 || outcome.Scopes[0].Scope != "retired-hand" || !strings.Contains(outcome.Scopes[0].Reason, "hq-retired") {
		t.Fatalf("outcome scopes = %+v, want one for retired-hand naming hq-retired", outcome.Scopes)
	}
}

// TestOrphanSweepProtectsEveryUnvouchedClaimWhenTheGateCannotAnswer is the
// fail-closed contract: when the gate cannot answer -- exit 2, a binary
// without the verb, or an exit code that disagrees with its JSON verdict --
// no claim it was asked about is probed or released, and every one is named.
// A bare exit 1 without a "foreign" verdict reads as "could not answer", not
// as foreign.
func TestOrphanSweepProtectsEveryUnvouchedClaimWhenTheGateCannotAnswer(t *testing.T) {
	for _, tc := range []struct {
		name       string
		env        map[string]string
		wantReason string
	}{
		{"gate exits 2", map[string]string{"ORPHAN_SWEEP_GATE_EXIT": "2"}, "could not answer (exit 2): lookup_failed: session store unreachable"},
		{"binary without the verb", map[string]string{"ORPHAN_SWEEP_GATE_ABSENT": "1"}, "could not answer (exit 1)"},
		{"exit 0 with an unknown verdict", map[string]string{"ORPHAN_SWEEP_GATE_EXIT": "0"}, "could not answer (exit 0)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			run := runOrphanSweepGateFixture(t, nil,
				[]orphanSweepGateRow{
					inProgress("sh-local-dead", "gastown__polecat-gc-d34d1"),
					inProgress("sh-foreign-crew", "shared/dalinar"),
					inProgress("sh-local-roster", "shared/gastown.polecat-2"),
				}, nil, nil, tc.env)
			if run.err != nil {
				t.Fatalf("orphan-sweep exited non-zero for a sweep that only protected: %v\n%s", run.err, run.context())
			}
			for _, bead := range []string{"sh-local-dead", "sh-foreign-crew", "sh-local-roster"} {
				if run.released(bead) {
					t.Fatalf("%s was released while the gate could not answer\n%s", bead, run.context())
				}
				if strings.Contains(run.log, "bd show "+bead+" --json") {
					t.Fatalf("%s was probed while the gate could not answer\n%s", bead, run.context())
				}
			}
			for _, want := range []string{
				"orphan-sweep: reset 0 orphaned beads, protected 2 foreign/unknown claims",
				`"gastown__polecat-gc-d34d1" (1: sh-local-dead)`,
				`"shared/dalinar" (1: sh-foreign-crew)`,
			} {
				if !strings.Contains(run.out, want) {
					t.Fatalf("output missing %q:\n%s", want, run.out)
				}
			}
			if strings.Contains(run.out, "shared/gastown.polecat-2") {
				t.Fatalf("roster-known claimant reported as protected:\n%s", run.out)
			}
			if strings.Contains(run.log, "mail send") {
				t.Fatalf("a sweep that only protected sent mail\n%s", run.context())
			}
			outcome, ok := run.outcome(t)
			if !ok || outcome.Outcome != "partial" || len(outcome.Scopes) != 2 {
				t.Fatalf("outcome = %+v (written %v), want partial with two scopes", outcome, ok)
			}
			for _, scope := range outcome.Scopes {
				if !strings.Contains(scope.Reason, tc.wantReason) {
					t.Fatalf("scope %q reason %q, want it to contain %q", scope.Scope, scope.Reason, tc.wantReason)
				}
			}
		})
	}
}

// TestOrphanSweepConsultsTheGateOncePerIdentityAndBoundsTheSample keeps a
// leaking identity cheap and readable: one gate call however many claims it
// holds, and a summary sample capped at five IDs with an exact count.
func TestOrphanSweepConsultsTheGateOncePerIdentityAndBoundsTheSample(t *testing.T) {
	var rows []orphanSweepGateRow
	for i := 1; i <= 7; i++ {
		rows = append(rows, inProgress(fmt.Sprintf("sh-%d", i), "shared/dalinar"))
	}
	run := runOrphanSweepGateFixture(t, nil, rows, nil, []string{"shared/dalinar"}, nil)
	if run.err != nil {
		t.Fatalf("orphan-sweep failed: %v\n%s", run.err, run.context())
	}
	if got := run.gateCalls(); got != 1 {
		t.Fatalf("gate consulted %d times for one identity, want 1\n%s", got, run.context())
	}
	if want := `orphan-sweep: protected 1 foreign/unknown identities this pass (7 claims): "shared/dalinar" (7: sh-1, sh-2, sh-3, sh-4, sh-5 +2 more)`; !strings.Contains(run.out, want) {
		t.Fatalf("output missing %q:\n%s", want, run.out)
	}
	outcome, ok := run.outcome(t)
	if !ok || len(outcome.Scopes) != 1 {
		t.Fatalf("outcome = %+v (written %v), want one scope", outcome, ok)
	}
	if reason := outcome.Scopes[0].Reason; !strings.Contains(reason, "7 claim(s) kept") || !strings.Contains(reason, "+2 more") || !strings.Contains(reason, "absent_from_roster") {
		t.Fatalf("scope reason %q, want the exact count, the bounded sample and the gate reason", reason)
	}
}

// TestOrphanSweepDeclaresNoOutcomeWhenNothingIsProtected keeps a clean sweep
// quiet: no partial outcome (so no order.skipped event) and no protected line.
func TestOrphanSweepDeclaresNoOutcomeWhenNothingIsProtected(t *testing.T) {
	run := runOrphanSweepGateFixture(t, nil,
		[]orphanSweepGateRow{
			inProgress("sh-local-dead", "gastown__polecat-gc-d34d1"),
			inProgress("sh-local-roster", "shared/gastown.polecat-2"),
		}, nil, nil, nil)
	if run.err != nil {
		t.Fatalf("orphan-sweep failed: %v\n%s", run.err, run.context())
	}
	if !run.released("sh-local-dead") {
		t.Fatalf("dead local claim was not released\n%s", run.context())
	}
	if strings.Contains(run.out, "protected") {
		t.Fatalf("a sweep that protected nothing reported protection:\n%s", run.out)
	}
	if outcome, ok := run.outcome(t); ok {
		t.Fatalf("a sweep that protected nothing declared outcome %+v", outcome)
	}
}

// TestOrphanSweepReportsProtectedIdentitiesWhenEscalationMailFails keeps the
// protected summary ahead of the loud-fail exit for an undeliverable
// escalation, so both reach the controller log.
func TestOrphanSweepReportsProtectedIdentitiesWhenEscalationMailFails(t *testing.T) {
	run := runOrphanSweepGateFixture(t,
		[]orphanSweepGateRow{inProgress("ga-foreign", "project/dalinar"), inProgress("ga-local-dead", "gastown__polecat-gc-d34d1")},
		nil, nil, []string{"project/dalinar"}, map[string]string{"ORPHAN_SWEEP_MAIL_EXIT": "1"})
	var exitErr *exec.ExitError
	if !errors.As(run.err, &exitErr) {
		t.Fatalf("orphan-sweep err = %v, want a non-zero exit for an undeliverable escalation\n%s", run.err, run.context())
	}
	for _, want := range []string{
		"orphan-sweep: reset 1 orphaned beads",
		`orphan-sweep: protected 1 foreign/unknown identities this pass (1 claims): "project/dalinar" (1: ga-foreign)`,
		"could not be delivered to 'human'",
	} {
		if !strings.Contains(run.out, want) {
			t.Fatalf("output missing %q:\n%s", want, run.out)
		}
	}
	if outcome, ok := run.outcome(t); !ok || outcome.Outcome != "partial" {
		t.Fatalf("outcome = %+v (written %v), want partial declared before the loud-fail exit", outcome, ok)
	}
}

// TestOrphanSweepSummaryQuotesUntrustedIdentities keeps an assignee written by
// another city from forging structure in the summary line: identities are
// JSON-quoted and tabs are neutralized. The outcome declaration names each
// identity verbatim, a backslash included, so the order.skipped scope is the
// identity an operator has to look up.
func TestOrphanSweepSummaryQuotesUntrustedIdentities(t *testing.T) {
	forged := `evil, "x" (9: y)`
	tabbed := "tab\tbed"
	backslashed := `DOMAIN\pat`
	run := runOrphanSweepGateFixture(t, nil,
		[]orphanSweepGateRow{inProgress("sh-forged", forged), inProgress("sh-tabbed", tabbed), inProgress("sh-backslashed", backslashed)},
		nil, []string{forged, tabbed, backslashed}, nil)
	if run.err != nil {
		t.Fatalf("orphan-sweep failed: %v\n%s", run.err, run.context())
	}
	outcome, ok := run.outcome(t)
	if !ok {
		t.Fatalf("no outcome declaration\n%s", run.context())
	}
	scopes := map[string]string{}
	for _, scope := range outcome.Scopes {
		scopes[scope.Scope] = scope.Reason
	}
	for identity, bead := range map[string]string{forged: "sh-forged", "tab bed": "sh-tabbed", backslashed: "sh-backslashed"} {
		if reason, found := scopes[identity]; !found || !strings.Contains(reason, bead) {
			t.Errorf("outcome declares no scope named %q with %s: %+v", identity, bead, outcome.Scopes)
		}
	}
	var lineB string
	for _, line := range strings.Split(run.out, "\n") {
		if strings.HasPrefix(line, "orphan-sweep: protected ") {
			if lineB != "" {
				t.Fatalf("more than one protected line:\n%s", run.out)
			}
			lineB = line
		}
	}
	if lineB == "" {
		t.Fatalf("no protected line:\n%s", run.out)
	}
	if strings.Count(lineB, "protected ") != 1 {
		t.Fatalf("protected line carries a forged prefix: %q", lineB)
	}
	if want := strconv.Quote(forged) + " (1: sh-forged)"; !strings.Contains(lineB, want) {
		t.Fatalf("protected line %q does not JSON-quote %q", lineB, want)
	}
	if strings.Contains(lineB, "\t") || !strings.Contains(lineB, `"tab bed" (1: sh-tabbed)`) {
		t.Fatalf("protected line %q does not neutralize the tab", lineB)
	}
}

// TestOrphanSweepNeverVouchesForAnEmptyIdentity keeps the gate's memo from
// answering for an identity the gate was never asked about. A newline in an
// assignee splits its row, so the tail reads as a row of its own whose bead ID
// is attacker-chosen text and whose assignee is empty. Once a dead local claim
// has been vouched for, the memo must not vouch for "" too: that would release
// an unrelated in-progress bead that has no assignee at all, a bead the sweep
// never selects on its own.
func TestOrphanSweepNeverVouchesForAnEmptyIdentity(t *testing.T) {
	run := runOrphanSweepGateFixture(t, nil,
		[]orphanSweepGateRow{
			inProgress("sh-local-dead", "gastown__polecat-gc-d34d1"),
			inProgress("sh-split", "\nsh-unassigned"),
			{ID: "sh-unassigned", Status: "in_progress"},
		}, nil, nil, nil)
	if run.err != nil {
		t.Fatalf("orphan-sweep failed: %v\n%s", run.err, run.context())
	}
	if !run.released("sh-local-dead") {
		t.Fatalf("dead local claim was not released, so the memo was never primed\n%s", run.context())
	}
	for _, bead := range []string{"sh-split", "sh-unassigned"} {
		if run.released(bead) {
			t.Fatalf("%s was released under an empty identity the gate never vouched for\n%s", bead, run.context())
		}
	}
	if !strings.Contains(run.out, `"" (2: sh-split, sh-unassigned)`) {
		t.Fatalf("output does not name the claims kept under the empty identity:\n%s", run.out)
	}
	// An empty identity cannot name a scope; it must not shift its reason into
	// the scope name either.
	outcome, ok := run.outcome(t)
	if !ok || outcome.Outcome != "partial" || len(outcome.Scopes) != 0 {
		t.Fatalf("outcome = %+v (written %v), want partial with no scope", outcome, ok)
	}
}

// TestOrphanSweepProtectsUngatedClaimsWhenTheGateBudgetIsExhausted bounds the
// gate's cost per sweep. Foreign claims are never released, so they are
// re-checked every sweep; with enough distinct foreign identities, one gate
// call each would overrun the order timeout and fail every sweep. Past the
// budget the sweep asks nothing more: each claimant it did not get to ask
// about is protected (fail-closed), named, and declared with a reason that
// says the budget ran out rather than that the claimant is foreign.
func TestOrphanSweepProtectsUngatedClaimsWhenTheGateBudgetIsExhausted(t *testing.T) {
	run := runOrphanSweepGateFixture(t, nil,
		[]orphanSweepGateRow{
			inProgress("sh-local-dead", "gastown__polecat-gc-d34d1"),
			inProgress("sh-foreign-crew", "shared/dalinar"),
			inProgress("sh-local-roster", "shared/gastown.polecat-2"),
		}, nil, []string{"shared/dalinar"},
		map[string]string{"GC_ORPHAN_SWEEP_GATE_BUDGET_SECONDS": "0"})
	if run.err != nil {
		t.Fatalf("orphan-sweep exited non-zero for a sweep that only protected: %v\n%s", run.err, run.context())
	}
	if got := run.gateCalls(); got != 0 {
		t.Fatalf("gate consulted %d times with a zero budget, want 0\n%s", got, run.context())
	}
	for _, bead := range []string{"sh-local-dead", "sh-foreign-crew", "sh-local-roster"} {
		if run.released(bead) {
			t.Fatalf("%s was released without a gate answer\n%s", bead, run.context())
		}
		if strings.Contains(run.log, "bd show "+bead+" --json") {
			t.Fatalf("%s was probed without a gate answer\n%s", bead, run.context())
		}
	}
	for _, want := range []string{
		"orphan-sweep: reset 0 orphaned beads, protected 2 foreign/unknown claims",
		`"gastown__polecat-gc-d34d1" (1: sh-local-dead)`,
		`"shared/dalinar" (1: sh-foreign-crew)`,
		"orphan-sweep: foreign-identity gate budget of 0s exhausted; 2 identities not asked this pass",
	} {
		if !strings.Contains(run.out, want) {
			t.Fatalf("output missing %q:\n%s", want, run.out)
		}
	}
	outcome, ok := run.outcome(t)
	if !ok || outcome.Outcome != "partial" || len(outcome.Scopes) != 2 {
		t.Fatalf("outcome = %+v (written %v), want partial with two scopes", outcome, ok)
	}
	if strings.Contains(outcome.Reason, "not in its roster") {
		t.Fatalf("outcome reason %q claims the gate answered when it was never asked", outcome.Reason)
	}
	for _, scope := range outcome.Scopes {
		if !strings.Contains(scope.Reason, "gate budget of 0s exhausted") {
			t.Fatalf("scope %q reason %q, want the budget-exhausted reason", scope.Scope, scope.Reason)
		}
	}
}

// TestOrphanSweepStopsAskingTheGateOnceTheBudgetRunsOut asks a slow gate (3s
// per answer) under a 2s budget: the sweep asks about one identity and no
// other once the budget is spent, protecting the rest. Where timeout(1)
// exists, that one ask is abandoned at the deadline instead of answered.
func TestOrphanSweepStopsAskingTheGateOnceTheBudgetRunsOut(t *testing.T) {
	foreign := []string{"shared/dalinar", "shared/kaladin", "shared/shallan"}
	var rows []orphanSweepGateRow
	for i, identity := range foreign {
		rows = append(rows, inProgress(fmt.Sprintf("sh-foreign-%d", i), identity))
	}
	run := runOrphanSweepGateFixture(t, nil, rows, nil, foreign, map[string]string{
		"GC_ORPHAN_SWEEP_GATE_BUDGET_SECONDS": "2",
		"ORPHAN_SWEEP_GATE_SLEEP":             "3",
	})
	if run.err != nil {
		t.Fatalf("orphan-sweep failed: %v\n%s", run.err, run.context())
	}
	if got := run.gateCalls(); got != 1 {
		t.Fatalf("gate consulted %d times past a 2s budget at 3s per answer, want 1\n%s", got, run.context())
	}
	if !strings.Contains(run.out, "orphan-sweep: protected 3 foreign/unknown identities this pass (3 claims)") {
		t.Fatalf("output does not name all three protected identities:\n%s", run.out)
	}
	outcome, ok := run.outcome(t)
	if !ok || len(outcome.Scopes) != 3 {
		t.Fatalf("outcome = %+v (written %v), want three scopes", outcome, ok)
	}
	askedReason := "absent_from_roster"
	if _, err := exec.LookPath("timeout"); err == nil {
		askedReason = "2s budget and was abandoned"
	}
	asked, budgeted := 0, 0
	for _, scope := range outcome.Scopes {
		switch {
		case strings.Contains(scope.Reason, askedReason):
			asked++
		case strings.Contains(scope.Reason, "gate budget of 2s exhausted"):
			budgeted++
		}
	}
	if asked != 1 || budgeted != 2 {
		t.Fatalf("scopes %+v: %d asked (%q) and %d budget-protected, want 1 and 2", outcome.Scopes, asked, askedReason, budgeted)
	}
}

// TestOrphanSweepAbandonsAGateAskThatOutlivesTheBudget bounds each ask, not
// only when asks start. A store-open stuck behind an unresponsive server
// would otherwise hold one ask until the order timeout killed the sweep, and
// the kill reports nothing the sweep protected. Past the budget the ask is
// abandoned and its claim kept and named, even for a dead local claimant the
// gate would vouch for given time, and even after the gate printed that
// verdict: only an exit code that agrees with it is an answer.
func TestOrphanSweepAbandonsAGateAskThatOutlivesTheBudget(t *testing.T) {
	if _, err := exec.LookPath("timeout"); err != nil {
		t.Skipf("timeout(1) not available: %v", err)
	}
	const (
		deadLocal = "gastown__polecat-gc-d34d1"
		gateHang  = 30 * time.Second
	)
	for _, tc := range []struct {
		name string
		knob string
	}{
		{name: "silent until abandoned", knob: "ORPHAN_SWEEP_GATE_SLEEP"},
		{name: "answered local then hung", knob: "ORPHAN_SWEEP_GATE_LINGER"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			start := time.Now()
			run := runOrphanSweepGateFixture(t, nil,
				[]orphanSweepGateRow{inProgress("sh-local-dead", deadLocal)},
				nil, nil, map[string]string{
					"GC_ORPHAN_SWEEP_GATE_BUDGET_SECONDS": "2",
					tc.knob:                               strconv.Itoa(int(gateHang / time.Second)),
				})
			elapsed := time.Since(start)
			if run.err != nil {
				t.Fatalf("orphan-sweep exited non-zero for a sweep that only protected: %v\n%s", run.err, run.context())
			}
			if elapsed >= gateHang/2 {
				t.Fatalf("sweep took %s under a 2s budget: the hung ask was not abandoned\n%s", elapsed, run.context())
			}
			if got := run.gateCalls(); got != 1 {
				t.Fatalf("gate consulted %d times, want 1\n%s", got, run.context())
			}
			if run.released("sh-local-dead") {
				t.Fatalf("claim released on an ask abandoned at the budget\n%s", run.context())
			}
			if strings.Contains(run.log, "bd show sh-local-dead --json") {
				t.Fatalf("claim probed without a gate answer\n%s", run.context())
			}
			if want := `"` + deadLocal + `" (1: sh-local-dead)`; !strings.Contains(run.out, want) {
				t.Fatalf("output does not name the protected claim %s:\n%s", want, run.out)
			}
			if strings.Contains(run.out, "not asked this pass") {
				t.Fatalf("an abandoned ask was reported as never asked:\n%s", run.out)
			}
			outcome, ok := run.outcome(t)
			if !ok || outcome.Outcome != "partial" || len(outcome.Scopes) != 1 {
				t.Fatalf("outcome = %+v (written %v), want partial with one scope", outcome, ok)
			}
			if scope := outcome.Scopes[0]; scope.Scope != deadLocal || !strings.Contains(scope.Reason, "2s budget and was abandoned") {
				t.Fatalf("scope = %+v, want %s kept as abandoned at the budget", scope, deadLocal)
			}
		})
	}
}

// TestOrphanSweepStartsTheGateBudgetAtTheFirstAsk keeps the claims'
// classification from spending the gate budget. Matching every in-progress
// claim against the roster and the live sessions takes real time on a large
// sweep; a budget that ran from before it could be spent before any claimant
// was asked, protecting every unrecognized claim, dead local ones included,
// on every sweep. A grep that takes 1.5s per match of the dead local claimant
// stands in for that volume.
func TestOrphanSweepStartsTheGateBudgetAtTheFirstAsk(t *testing.T) {
	const deadLocal = "gastown__polecat-gc-d34d1"
	binDir := t.TempDir()
	writeExecutable(t, filepath.Join(binDir, "gc"), orphanSweepSharedRigFakeGC)
	writeExecutable(t, filepath.Join(binDir, "grep"), `#!/bin/sh
for arg; do
  if [ "$arg" = "`+deadLocal+`" ]; then sleep 1.5; break; fi
done
PATH=${PATH#*:}
exec grep "$@"
`)
	run := runOrphanSweepGateFixture(t, nil,
		[]orphanSweepGateRow{inProgress("sh-local-dead", deadLocal)},
		nil, nil, map[string]string{
			"GC_ORPHAN_SWEEP_GATE_BUDGET_SECONDS": "2",
			"PATH":                                binDir + string(os.PathListSeparator) + os.Getenv("PATH"),
		})
	if run.err != nil {
		t.Fatalf("orphan-sweep failed: %v\n%s", run.err, run.context())
	}
	if got := run.gateCalls(); got != 1 {
		t.Fatalf("gate consulted %d times, want 1: classifying the claims spent the gate budget\n%s", got, run.context())
	}
	if !run.released("sh-local-dead") {
		t.Fatalf("dead local claim was not released\n%s", run.context())
	}
}

// TestOrphanSweepIgnoresAMalformedGateBudget falls back to the default budget
// for a value that is not a whole number of seconds, or is too long for bash
// arithmetic to hold, rather than to no gate or to a deadline that wrapped
// into the past before the first claimant was asked.
func TestOrphanSweepIgnoresAMalformedGateBudget(t *testing.T) {
	for _, budget := range []string{"-5s", "9223372036854775808"} {
		t.Run(budget, func(t *testing.T) {
			run := runOrphanSweepGateFixture(t, nil,
				[]orphanSweepGateRow{inProgress("sh-local-dead", "gastown__polecat-gc-d34d1")},
				nil, nil, map[string]string{"GC_ORPHAN_SWEEP_GATE_BUDGET_SECONDS": budget})
			if run.err != nil {
				t.Fatalf("orphan-sweep failed: %v\n%s", run.err, run.context())
			}
			if !run.released("sh-local-dead") {
				t.Fatalf("dead local claim was not released under the default budget\n%s", run.context())
			}
		})
	}
}

// TestOrphanSweepReadsAZeroPaddedGateBudgetInBaseTen keeps a zero-padded
// budget a number of seconds. Bash arithmetic reads a leading zero as octal
// and rejects "08" outright; the failed deadline then disabled the budget for
// the whole sweep, so every claimant was asked however long the gate took.
func TestOrphanSweepReadsAZeroPaddedGateBudgetInBaseTen(t *testing.T) {
	run := runOrphanSweepGateFixture(t, nil,
		[]orphanSweepGateRow{inProgress("sh-local-dead", "gastown__polecat-gc-d34d1")},
		nil, nil, map[string]string{"GC_ORPHAN_SWEEP_GATE_BUDGET_SECONDS": "08"})
	if run.err != nil {
		t.Fatalf("orphan-sweep failed: %v\n%s", run.err, run.context())
	}
	for _, symptom := range []string{"value too great for base", "integer expression expected"} {
		if strings.Contains(run.out, symptom) {
			t.Fatalf("a zero-padded budget broke the deadline arithmetic (%q):\n%s", symptom, run.out)
		}
	}
	if !run.released("sh-local-dead") {
		t.Fatalf("dead local claim was not released under an 8s budget\n%s", run.context())
	}
}

// TestJsonlExportFirstRunArchivePathCreatedBeforeTheRename covers the window
// between the first run's "archive path absent" check and its rename of the
// staging repo into place: another exporter (or anything else) can create
// the archive path in it. POSIX mv then moves the staging repo INSIDE the
// directory that appeared, and macOS mv has no -T to refuse that. A mv shim
// creates the archive path right before the rename. The run must not leave
// the staging repo nested in the archive path. It falls back to the
// in-place init, so the archive is still a repo.
func TestJsonlExportFirstRunArchivePathCreatedBeforeTheRename(t *testing.T) {
	cityDir := t.TempDir()
	binDir := t.TempDir()
	stateDir := t.TempDir()
	gcLog := filepath.Join(t.TempDir(), "gc.log")
	mailLog := filepath.Join(t.TempDir(), "gc-mail.log")
	archiveRepo := filepath.Join(cityDir, "packs", "core", "jsonl-archive")
	raceLog := filepath.Join(t.TempDir(), "mv-race.log")

	writeMultiRecordDoltStub(t, binDir, 3)
	writeJsonlExportGCStub(t, binDir)
	realMv, err := exec.LookPath("mv")
	if err != nil {
		t.Skip("mv not on PATH")
	}
	shim := "#!/bin/sh\n" +
		"for last in \"$@\"; do :; done\n" +
		"case \"$1\" in\n" +
		"  */.jsonl-archive-init.*)\n" +
		"    if [ \"$last\" = \"$ARCHIVE_PROBE\" ] && [ ! -e \"$ARCHIVE_PROBE\" ]; then mkdir -p \"$ARCHIVE_PROBE\"; echo raced >>\"$RACE_LOG\"; fi ;;\n" +
		"esac\n" +
		"exec " + realMv + " \"$@\"\n"
	if err := os.WriteFile(filepath.Join(binDir, "mv"), []byte(shim), 0o755); err != nil {
		t.Fatal(err)
	}

	env := jsonlExportEnv(t, cityDir, binDir, stateDir, archiveRepo, gcLog, mailLog)
	env["ARCHIVE_PROBE"] = archiveRepo
	env["RACE_LOG"] = raceLog
	runScript(t, coreScriptPath("jsonl-export.sh"), env)

	if data, err := os.ReadFile(raceLog); err != nil || !strings.Contains(string(data), "raced") {
		t.Fatalf("the mv shim never created the archive path before the rename (%q, %v)", data, err)
	}
	nested, err := filepath.Glob(filepath.Join(archiveRepo, ".jsonl-archive-init.*"))
	if err != nil || len(nested) != 0 {
		t.Fatalf("staging repo moved inside the archive path: %v (%v)", nested, err)
	}
	leftovers, err := filepath.Glob(filepath.Join(filepath.Dir(archiveRepo), ".jsonl-archive-init.*"))
	if err != nil || len(leftovers) != 0 {
		t.Fatalf("init staging dirs left behind: %v (%v)", leftovers, err)
	}
	if _, err := os.Stat(filepath.Join(archiveRepo, ".git")); err != nil {
		t.Fatalf("archive repo not initialized after the race: %v", err)
	}
}
