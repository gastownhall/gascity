package core

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// TestNudgeOnRouteNudgesBeadRoutedAtCreation pins #4382: a bead whose
// gc.routed_to is stamped in its create payload (gc order run, formula pours)
// emits bead.created and no bead.updated before it is claimed. The fake gc
// honors `gc events --type` exactly as the real CLI does, so a script that
// reads only bead.updated never sees the routing and never nudges.
func TestNudgeOnRouteNudgesBeadRoutedAtCreation(t *testing.T) {
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("jq not available; nudge-on-route.sh requires it")
	}

	dir := t.TempDir()
	eventsPath := filepath.Join(dir, "events.jsonl")
	created := `{"seq":1,"type":"bead.created","actor":"gc","subject":"ga-born","payload":{"bead":{"id":"ga-born","status":"open","metadata":{"gc.routed_to":"gascity/builder"}}}}` + "\n"
	if err := os.WriteFile(eventsPath, []byte(created), 0o644); err != nil {
		t.Fatalf("write events: %v", err)
	}
	body := `case "$1" in
events)
  typ=''
  shift
  while [ $# -gt 0 ]; do
    if [ "$1" = "--type" ] && [ $# -ge 2 ]; then typ="$2"; fi
    shift
  done
  if [ -n "$typ" ]; then jq -c --arg t "$typ" 'select(.type == $t)' '` + eventsPath + `'; else cat '` + eventsPath + `'; fi
  ;;
session)
  if [ "$2" = "list" ]; then printf '%s\n' '{"sessions":[]}'; fi
  ;;
esac
exit 0
`
	binDir, logPath := fakeGCBin(t, body)

	out, err := runPackScript(t, nudgeOnRouteScriptPath, binDir, nudgeOnRouteEnvKeys,
		[]string{"GC_PACK_STATE_DIR=" + filepath.Join(dir, "state")})
	if err != nil {
		t.Fatalf("nudge-on-route.sh failed: %v\n%s", err, out)
	}
	got, logged := countNudges(t, logPath)
	if got != 1 || countNudgesTo(t, logPath, "gascity/builder") != 1 {
		t.Fatalf("nudges = %d, want exactly 1 to gascity/builder for a bead routed at creation (bead.created only); gc calls:\n%s", got, logged)
	}
}
