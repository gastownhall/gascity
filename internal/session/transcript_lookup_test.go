package session

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
)

// writeCodexRolloutForAnchor writes a minimal Codex rollout transcript whose
// session_meta cwd is workDir and whose payload timestamp is startedAt, laid out
// in the YYYY/MM/DD date tree the time-window resolver scans. It returns the
// rollout path.
func writeCodexRolloutForAnchor(t *testing.T, root, workDir, sessionID string, startedAt time.Time) string {
	t.Helper()
	day := startedAt.In(time.Local)
	dayDir := filepath.Join(root, day.Format("2006"), day.Format("01"), day.Format("02"))
	if err := os.MkdirAll(dayDir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dayDir, "rollout-"+startedAt.UTC().Format("2006-01-02T15-04-05")+"-"+sessionID+".jsonl")
	meta := fmt.Sprintf(`{"timestamp":%q,"type":"session_meta","payload":{"id":%q,"cwd":%q,"timestamp":%q}}`,
		startedAt.Format(time.RFC3339Nano), sessionID, workDir, startedAt.Format(time.RFC3339Nano))
	if err := os.WriteFile(path, []byte(meta+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestResolveCodexTranscriptBySessionOrderAnchorsOnAwakeStartedAt proves the
// same-workdir Codex fallback anchors each session's transcript window on the
// immutable awake_started_at rather than the later creation_complete_at. Once a
// session sleeps or drains, last_woke_at and pending_create_started_at are
// cleared, so without awake_started_at the resolver falls through to
// creation_complete_at — stamped several seconds after the rollout's
// session_meta timestamp — and filters the true transcript out of the
// [start-2s, end) window, which is exactly the historical session this fallback
// exists to recover.
func TestResolveCodexTranscriptBySessionOrderAnchorsOnAwakeStartedAt(t *testing.T) {
	root := t.TempDir()
	workDir := "/data/projects/myproject"
	const provider = "codex"

	// Target session A rolled out at startA; a later sibling B fixes A's window
	// end. Both are slept: last_woke_at and pending_create_started_at are blank,
	// awake_started_at aligns with the rollout, and creation_complete_at lands 5s
	// later — late enough that a creation_complete_at anchor (windowStart =
	// creation_complete_at - 2s) would exclude the rollout from the window.
	startA := time.Date(2026, 5, 19, 12, 0, 0, 0, time.UTC)
	startB := startA.Add(30 * time.Second)

	pathA := writeCodexRolloutForAnchor(t, root, workDir, "019e3e8e-3591-7532-a1ef-8b9e882bea2f", startA)
	writeCodexRolloutForAnchor(t, root, workDir, "019e3e8e-ffff-7000-a1ef-8b9e882bea2f", startB)

	sessions := []beads.Bead{
		sleptCodexSessionBead("sess-a", workDir, provider, startA),
		sleptCodexSessionBead("sess-b", workDir, provider, startB),
	}

	got := ResolveCodexTranscriptBySessionOrder([]string{root}, provider, workDir, "sess-a", sessions)
	if got != pathA {
		t.Fatalf("ResolveCodexTranscriptBySessionOrder() = %q, want %q (awake_started_at window must include the rollout)", got, pathA)
	}
}

// sleptCodexSessionBead builds a same-workdir Codex session bead in the
// slept/drained shape: last_woke_at and pending_create_started_at are cleared,
// awake_started_at pins the rollout start, and creation_complete_at is 5s later.
func sleptCodexSessionBead(id, workDir, provider string, awakeStart time.Time) beads.Bead {
	return beads.Bead{
		ID: id,
		Metadata: map[string]string{
			"work_dir":                  workDir,
			"provider":                  provider,
			"last_woke_at":              "",
			"pending_create_started_at": "",
			"awake_started_at":          awakeStart.Format(time.RFC3339Nano),
			"creation_complete_at":      awakeStart.Add(5 * time.Second).Format(time.RFC3339),
		},
	}
}
