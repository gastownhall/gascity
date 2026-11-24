package worker

import (
	"context"
	"log/slog"
	"time"

	"github.com/gastownhall/gascity/internal/sessionlog"
)

// Seam names carried by the codex rollout discovery record.
const (
	codexDiscoverySeamPromptOp = "prompt_op"
	codexDiscoverySeamSweep    = "sweep"
)

// discoverCodexKeyed resolves a keyed Codex session's own rollout by identity,
// writes the one discovery record for the call, and returns the mapped result
// in the shape both discovery seams consume. seam is one of the
// codexDiscoverySeam* names and sessionID the gc session bead id.
func discoverCodexKeyed(
	seam, sessionID string,
	searchPaths []string, workDir, key string, notBefore, notAfter time.Time,
) (path string, scanClean, emptyIsPermanent bool) {
	lookup := sessionlog.FindCodexSessionFileByIdentity(searchPaths, workDir, key, notBefore, notAfter)
	path, scanClean, emptyIsPermanent = codexKeyedDiscovery(lookup)
	final := path == "" && scanClean && sweepMissIsFinal("codex", false, emptyIsPermanent)
	recordCodexKeyedDiscovery(seam, sessionID, lookup, final)
	return path, scanClean, emptyIsPermanent
}

// codexKeyedDiscovery maps a keyed lookup outcome onto the discovery triple the
// sweep classifies a miss with: the path, whether the scan was clean, and
// whether an empty path is permanent. Ambiguous and identity_mismatch are
// settled misses (a retry cannot change a contradicting or duplicated rollout);
// uncertain is a non-definitive scan; absent and invalid may resolve later. An
// outcome this function does not know is treated as uncertain.
func codexKeyedDiscovery(l sessionlog.CodexKeyedLookup) (path string, scanClean, emptyIsPermanent bool) {
	switch l.Outcome {
	case sessionlog.CodexKeyedFound:
		return l.Path, true, false
	case sessionlog.CodexKeyedAbsent, sessionlog.CodexKeyedInvalid:
		return "", true, false
	case sessionlog.CodexKeyedAmbiguous, sessionlog.CodexKeyedIdentityMismatch:
		return "", true, true
	default:
		return "", false, false
	}
}

// sweepMissIsFinal reports whether an empty discovery settles the interval: the
// miss must be permanent, and only codex (keyed or keyless) and keyless claude
// treat a permanent miss as final. A keyed claude session's ambiguous-workdir
// refusal and every opencode miss stay transient.
func sweepMissIsFinal(family string, keyless, emptyIsPermanent bool) bool {
	return emptyIsPermanent && (family == "codex" || (family == "claude" && keyless))
}

// recordCodexKeyedDiscovery writes the one structured record per keyed
// discovery call. It carries the gc session bead id, never a thread id, path
// or working directory. A settled miss at the sweep seam is a WARN because it
// ends telemetry for the interval; every other record is DEBUG.
func recordCodexKeyedDiscovery(seam, sessionID string, l sessionlog.CodexKeyedLookup, final bool) {
	level := slog.LevelDebug
	if seam == codexDiscoverySeamSweep && final {
		level = slog.LevelWarn
	}
	slog.LogAttrs(context.Background(), level, "codex rollout discovery",
		slog.String("session_id", sessionID),
		slog.String("seam", seam),
		slog.String("mechanism", "keyed"),
		slog.String("outcome", string(l.Outcome)),
		slog.Bool("final", final),
		slog.Bool("cwd_differs", l.CWDDiffers),
	)
}
