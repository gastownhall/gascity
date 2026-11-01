package gastown_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestOrphanSweepReleasesClaimWhenStampedSessionNameIsNotABeadID reproduces
// gastownhall/gascity#7686: a pool claim stamps gc.session_name with the
// runtime name "<template>-<session bead id>", which is never a bead id. The
// sweep probes it with `gc bd show <name> --json`, the probe fails, and since
// the name differs from the assignee the claim is counted as unverifiable even
// though the claim's own gc.session_id reads back closed. The dead holder's
// claim must be released.
func TestOrphanSweepReleasesClaimWhenStampedSessionNameIsNotABeadID(t *testing.T) {
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
  {"id":"ga-stamped-claim","status":"in_progress","assignee":"ga-deadsess"}
]
EOF
      exit 0
    fi
    if [ "$2" = "show" ] && [ "$3" = "ga-stamped-claim" ] && [ "$4" = "--json" ]; then
      cat <<'EOF'
[
  {"id":"ga-stamped-claim","status":"in_progress","assignee":"ga-deadsess","metadata":{"gc.session_id":"ga-deadsess","gc.session_name":"project__worker-ga-deadsess"}}
]
EOF
      exit 0
    fi
    if [ "$2" = "show" ] && [ "$3" = "ga-deadsess" ] && [ "$4" = "--json" ]; then
      cat <<'EOF'
[
  {"id":"ga-deadsess","issue_type":"session","status":"closed","metadata":{"state":"closed","session_name":"project__worker-ga-deadsess"}}
]
EOF
      exit 0
    fi
    if [ "$2" = "show" ]; then
      printf 'no issue found matching %s\n' "$3" >&2
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
  mail)
    exit 0
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

	logData, err := os.ReadFile(gcLog)
	if err != nil {
		t.Fatalf("ReadFile(gc log): %v", err)
	}
	log := string(logData)
	if !strings.Contains(log, "bd release-if-current ga-stamped-claim ga-deadsess") {
		t.Fatalf("claim whose gc.session_id reads closed was not released (stamped gc.session_name probe counted as unverifiable):\noutput:\n%s\ngc log:\n%s", out, log)
	}
	if !strings.Contains(string(out), "orphan-sweep: reset 1 orphaned beads") {
		t.Fatalf("unexpected orphan-sweep output:\n%s", out)
	}
}
