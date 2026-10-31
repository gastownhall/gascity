package core

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

// wispCompactScriptPath is the on-disk copy of the wisp-compact order's
// script. The test runs it, so it needs a real path.
const wispCompactScriptPath = "assets/scripts/wisp-compact.sh"

// wispCompactEnvKeys are the settings that would point the script at a live
// city or trace file.
var wispCompactEnvKeys = map[string]struct{}{
	"GC_CITY":          {},
	"GC_BD_TRACE_JSON": {},
}

// TestWispCompactLeavesMailAlone pins that wisp-compact never deletes or
// promotes mail. Messages are ephemeral beads. Reading one labels it read and
// leaves it open; archiving closes it. Without the issue_type guard, an
// archived message past the wisp TTL takes the delete branch, and an open
// one, read or unread, takes the promote branch. The closed task wisp is the
// control: it must still be deleted, which shows the script reached its
// per-bead loop.
func TestWispCompactLeavesMailAlone(t *testing.T) {
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("jq not available; wisp-compact.sh requires it")
	}

	const old = "2026-01-01T00:00:00Z"
	list := `[` +
		`{"id":"wc-msg-archived","issue_type":"message","status":"closed","labels":["read"],"metadata":{"mail.read":"true"},"ephemeral":true,"updated_at":"` + old + `"},` +
		`{"id":"wc-msg-read","issue_type":"message","status":"open","labels":["read"],"metadata":{"mail.read":"true"},"ephemeral":true,"updated_at":"` + old + `"},` +
		`{"id":"wc-msg-unread","issue_type":"message","status":"open","ephemeral":true,"updated_at":"` + old + `"},` +
		`{"id":"wc-wisp-done","issue_type":"task","status":"closed","ephemeral":true,"updated_at":"` + old + `"}` +
		`]`
	binDir, logPath := fakeGCBin(t, `case "$1 $2" in
"bd list") printf '%s\n' '`+list+`' ;;
esac
exit 0
`)

	out, err := runPackScript(t, wispCompactScriptPath, binDir, wispCompactEnvKeys, nil)
	if err != nil {
		t.Fatalf("wisp-compact.sh failed: %v\n%s", err, out)
	}
	logged, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read gc log: %v", err)
	}
	calls := string(logged)

	if !strings.Contains(calls, "gc bd delete wc-wisp-done --force") {
		t.Fatalf("closed task wisp past TTL was not deleted; gc calls:\n%s", calls)
	}
	for _, id := range []string{"wc-msg-archived", "wc-msg-read", "wc-msg-unread"} {
		for _, line := range strings.Split(calls, "\n") {
			if strings.HasPrefix(line, "gc bd ") && !strings.HasPrefix(line, "gc bd list ") && strings.Contains(line, " "+id) {
				t.Errorf("wisp-compact acted on message %s: %q", id, line)
			}
		}
	}
}
