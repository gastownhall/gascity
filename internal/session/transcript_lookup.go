package session

import (
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/sessionlog"
	workertranscript "github.com/gastownhall/gascity/internal/worker/transcript"
)

// ResolveKeyedTranscriptPath returns a transcript only when info carries a
// stable provider session key that resolves to one exact file. It never uses a
// workdir/newest-file fallback, so callers may safely attribute the result to
// this session. The Info-based input lets list/read-model callers reuse their
// already-loaded projection without issuing a per-session store Get.
//
// Codex needs a stricter path than workertranscript.DiscoverKeyedPath: copied
// rollouts can share a UUID and workdir, so the bounded lookup refuses multiple
// physical matches instead of choosing the newest. Gemini has no exact by-key
// transcript lookup and therefore remains unsupported here.
func ResolveKeyedTranscriptPath(info Info, searchPaths []string) string {
	return ResolveKeyedTranscriptPaths([]Info{info}, searchPaths, "")[info.ID]
}

// ResolveKeyedTranscriptPaths is the page-oriented form of
// ResolveKeyedTranscriptPath. Codex targets share one batched date-directory
// scan, while providers with cheap direct keyed layouts resolve individually.
// The returned map contains exact matches only, keyed by Info.ID; keyless,
// unsupported, missing, and ambiguous rows are absent. fallbackProvider is
// consulted only when an Info row has no persisted provider identity, which
// preserves workspace-default behavior for legacy session rows.
func ResolveKeyedTranscriptPaths(infos []Info, searchPaths []string, fallbackProvider string) map[string]string {
	paths := make(map[string]string)
	if len(searchPaths) == 0 {
		searchPaths = sessionlog.DefaultSearchPaths()
	}

	var codexTargets []sessionlog.CodexSessionTarget
	codexInfoIDs := make(map[string]string)
	for i, info := range infos {
		workDir := strings.TrimSpace(info.WorkDir)
		sessionKey := strings.TrimSpace(info.SessionKey)
		if workDir == "" || sessionKey == "" {
			continue
		}
		if abs, err := filepath.Abs(workDir); err == nil {
			workDir = abs
		}
		provider := ProviderFamilyFromInfo(info, fallbackProvider)
		switch sessionlog.ProviderFamily(provider) {
		case "codex":
			anchor := info.CreatedAt
			if woke := parseTranscriptAnchorTime(info.LastWokeAt); !woke.IsZero() {
				anchor = woke
			}
			key := strconv.Itoa(i)
			codexTargets = append(codexTargets, sessionlog.CodexSessionTarget{
				Key:       key,
				WorkDir:   workDir,
				SessionID: sessionKey,
				NotBefore: info.CreatedAt,
				NotAfter:  anchor,
			})
			codexInfoIDs[key] = info.ID
		case "gemini":
			continue
		default:
			if path := workertranscript.DiscoverKeyedPath(searchPaths, provider, workDir, sessionKey); path != "" {
				paths[info.ID] = path
			}
		}
	}
	for key, path := range sessionlog.FindCodexSessionFilesByID(searchPaths, codexTargets) {
		paths[codexInfoIDs[key]] = path
	}
	return paths
}

// anchoredCodexSession is a same-workdir Codex session paired with its resolved
// start-time anchor and the tiebreak key used to order equal-start sessions.
type anchoredCodexSession struct {
	id     string
	start  time.Time
	tieKey string
}

// ResolveCodexTranscriptBySessionOrder maps an ambiguous same-workdir Codex session
// group to a transcript by using each session's wake/start timestamp. It takes the
// group as typed session.Info rows — the anchor keys (last_woke_at /
// pending_create_started_at / awake_started_at / creation_complete_at), work_dir,
// session_name, and CreatedAt are all mirrored on Info. It returns empty unless the
// target session has a unique transcript in its start window, preserving ambiguity for
// underspecified groups.
func ResolveCodexTranscriptBySessionOrder(searchPaths []string, provider, workDir, targetID string, sessions []Info) string {
	if sessionlog.ProviderFamily(provider) != "codex" || strings.TrimSpace(workDir) == "" || strings.TrimSpace(targetID) == "" {
		return ""
	}
	anchored := collectAnchoredCodexSessions(sessions, workDir)
	if len(anchored) < 2 {
		return ""
	}
	sortAnchoredCodexSessions(anchored)
	if hasDuplicateAnchorStart(anchored) {
		return ""
	}
	for i, item := range anchored {
		if item.id != targetID {
			continue
		}
		end := codexSessionWindowEnd(anchored, i)
		return workertranscript.DiscoverCodexPathInTimeWindow(searchPaths, workDir, item.start, end)
	}
	return ""
}

// collectAnchoredCodexSessions keeps the same-workdir Info rows carrying a non-zero
// start anchor, dropping ones without an id or a resolvable anchor. It reads
// Info.WorkDir (the legacy work_dir mirror), the anchor keys via transcriptStartAnchor,
// and Info.SessionNameMetadata as the tiebreak key.
func collectAnchoredCodexSessions(sessions []Info, workDir string) []anchoredCodexSession {
	var anchored []anchoredCodexSession
	for _, info := range sessions {
		if info.ID == "" || strings.TrimSpace(info.WorkDir) != workDir {
			continue
		}
		start := transcriptStartAnchor(info)
		if start.IsZero() {
			continue
		}
		anchored = append(anchored, anchoredCodexSession{
			id:     info.ID,
			start:  start,
			tieKey: strings.TrimSpace(info.SessionNameMetadata),
		})
	}
	return anchored
}

// sortAnchoredCodexSessions orders sessions by start time, breaking ties on the
// session name and then the id so ordering is deterministic.
func sortAnchoredCodexSessions(anchored []anchoredCodexSession) {
	sort.Slice(anchored, func(i, j int) bool {
		if anchored[i].start.Equal(anchored[j].start) {
			if anchored[i].tieKey == anchored[j].tieKey {
				return anchored[i].id < anchored[j].id
			}
			return anchored[i].tieKey < anchored[j].tieKey
		}
		return anchored[i].start.Before(anchored[j].start)
	})
}

// hasDuplicateAnchorStart reports whether any two adjacent (already sorted)
// sessions share the same start anchor, which collapses the group to ambiguous.
func hasDuplicateAnchorStart(anchored []anchoredCodexSession) bool {
	for i := 1; i < len(anchored); i++ {
		if anchored[i].start.Equal(anchored[i-1].start) {
			return true
		}
	}
	return false
}

// codexSessionWindowEnd returns the exclusive end of the start-time window for
// the session at index i: the next strictly-later session start, or zero when i
// is the last session (an open-ended window).
func codexSessionWindowEnd(anchored []anchoredCodexSession, i int) time.Time {
	for j := i + 1; j < len(anchored); j++ {
		if anchored[j].start.After(anchored[i].start) {
			return anchored[j].start
		}
	}
	return time.Time{}
}

// transcriptStartAnchor returns the best available "start of this session" time
// for windowing its Codex transcript. Preference runs most-precise first:
// last_woke_at and pending_create_started_at pin an in-flight wake/create, but
// both are cleared when a session sleeps or drains (SleepPatch,
// AcknowledgeDrainPatch). awake_started_at is the immutable
// start-of-awake-interval epoch that survives those teardowns, so it is
// preferred over creation_complete_at, which is stamped when the runtime
// finishes coming up and can land several seconds after the rollout's
// session_meta timestamp. Anchoring a slept or drained session on
// creation_complete_at would push the [start-2s, end) window past the true
// transcript and drop it; awake_started_at keeps the window aligned with the
// rollout. CreatedAt is the final fallback.
func transcriptStartAnchor(info Info) time.Time {
	for _, raw := range []string{info.LastWokeAt, info.PendingCreateStartedAt, info.AwakeStartedAt, info.CreationCompleteAt} {
		if parsed := parseTranscriptAnchorTime(raw); !parsed.IsZero() {
			return parsed
		}
	}
	return info.CreatedAt
}

func parseTranscriptAnchorTime(raw string) time.Time {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Time{}
	}
	if parsed, err := time.Parse(time.RFC3339Nano, raw); err == nil {
		return parsed
	}
	if parsed, err := time.Parse(time.RFC3339, raw); err == nil {
		return parsed
	}
	return time.Time{}
}
