package session

import (
	"os"
	"strings"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/sessionlog"
	workertranscript "github.com/gastownhall/gascity/internal/worker/transcript"
)

// PinnedTranscriptMetadataKey is the session-bead metadata key holding the
// transcript file resolved for a session while its bead was still open.
//
// Transcript resolution degrades once a session bead closes: the workdir
// fallback refuses any workdir shared by more than one session, and a closed
// bead's siblings include every session that ever recycled through the same
// agent home. A pooled worker that dies before its provider session key is
// captured is therefore undiagnosable after retirement — exactly the failure
// this key exists to prevent. The pin is taken at retirement time, before the
// bead is closed, so the transcript survives the slot.
const PinnedTranscriptMetadataKey = "transcript_path"

// TranscriptPinPatch returns the session metadata patch that pins path as the
// session's transcript. An empty path clears the key, keeping the patch total
// over the empty string as the metadata⇄Info codec requires.
func TranscriptPinPatch(path string) map[string]string {
	return map[string]string{PinnedTranscriptMetadataKey: strings.TrimSpace(path)}
}

// PinnedTranscriptPath returns the transcript pinned on info, or "" when none
// is pinned or the pinned file no longer exists. A vanished pin reads as unset
// so callers fall through to live resolution instead of handing back a path
// that cannot be opened.
func PinnedTranscriptPath(info Info) string {
	path := strings.TrimSpace(info.TranscriptPath)
	if path == "" {
		return ""
	}
	if _, err := os.Stat(path); err != nil {
		return ""
	}
	return path
}

// transcriptProvider returns the provider identity used for transcript lookup:
// the raw provider_kind mirror, falling back to the raw provider mirror.
func transcriptProvider(info Info) string {
	provider := strings.TrimSpace(info.ProviderKind)
	if provider == "" {
		provider = strings.TrimSpace(info.Provider)
	}
	return provider
}

// transcriptLadder resolves the transcript file for one session and reports why
// the resolution came back empty. It is the single implementation behind both
// live reads (Manager.TranscriptPathClassified) and the retirement pin
// (Store.ResolveTranscriptPin), so the pinned path is by construction the same
// path a live read would have returned.
//
// siblings is called only when the exact lookups miss, because gathering the
// same-workdir session set is a store-wide list; it must return the sessions
// the workdir fallback would be ambiguous across, including the target itself.
//
// capture, when set, is the ACP JSON-RPC transcript the manager wrote. It
// outranks workdir discovery and a keyless start, and loses to a keyed native
// transcript. The store pin passes nil: it has no city to read a capture from.
func transcriptLadder(info Info, siblings func() ([]Info, error), searchPaths []string, capture func() string) (string, TranscriptLookup, error) {
	// The raw work_dir mirror is what same-workdir sibling matching compares
	// against, so lookups use it verbatim; only the emptiness test is trimmed.
	workDir := info.WorkDir
	if strings.TrimSpace(workDir) == "" {
		if path := callTranscriptCapture(capture); path != "" {
			return path, TranscriptFound, nil
		}
		return "", TranscriptNoWorkDir, nil
	}
	provider := transcriptProvider(info)
	if len(searchPaths) == 0 {
		searchPaths = sessionlog.DefaultSearchPaths()
	}
	if path := workertranscript.DiscoverKeyedPath(searchPaths, provider, workDir, info.SessionKey); path != "" {
		return path, TranscriptFound, nil
	}
	if path := callTranscriptCapture(capture); path != "" {
		return path, TranscriptFound, nil
	}
	// zcode carries no session_key — no session-id flag, no hook plugin — so
	// the keyed lookup above can never hit for it and the ambiguity guard below
	// would leave every pooled worker transcript-dark. Its mirror is keyed by
	// the identity the bead does hold: its name, its own id (the seat — two
	// seats can share a name and epoch), and its continuation epoch.
	if path := workertranscript.DiscoverScopedPath(
		searchPaths,
		provider,
		workDir,
		info.SessionNameMetadata,
		info.ID,
		info.ContinuationEpoch,
	); path != "" {
		return path, TranscriptFound, nil
	}

	// A provider with a session-id flag runs each new conversation under a new
	// key. A creating row of such a provider without a key is a start whose
	// conversation has no key yet, and the work-directory fallback cannot tell
	// its transcript from the one that start replaces. Other sessions in the
	// work directory leave nothing to disambiguate before that key exists, so
	// the window is absent, not ambiguous.
	if strings.TrimSpace(info.SessionKey) == "" &&
		strings.TrimSpace(info.SessionIDFlag) != "" &&
		info.State == StateCreating {
		return "", TranscriptAbsent, nil
	}

	sameWorkDir, err := siblings()
	if err != nil {
		return "", TranscriptAbsent, err
	}
	if len(sameWorkDir) > 1 {
		if path := ResolveCodexTranscriptBySessionOrder(searchPaths, provider, workDir, info.ID, sameWorkDir); path != "" {
			return path, TranscriptFound, nil
		}
		// Without a stable session key, multiple sessions sharing the same
		// workdir cannot be mapped safely to a single transcript.
		return "", TranscriptAmbiguous, nil
	}
	if path := workertranscript.DiscoverPath(searchPaths, provider, workDir, info.SessionKey); path != "" {
		return path, TranscriptFound, nil
	}
	return "", TranscriptAbsent, nil
}

// ResolveTranscriptPin resolves the transcript file to pin on session id. It is
// the front door for retirement paths that stamp PinnedTranscriptMetadataKey
// into a session bead's terminal metadata: call it while the bead is still
// open, when the same-workdir sibling set is still narrow enough to attribute a
// transcript to exactly one session.
//
// It returns "" — never an error — when the transcript cannot be attributed
// (no workdir, ambiguous live siblings, nothing written yet), so a caller can
// stamp the result unconditionally and pin nothing when nothing is knowable.
// Store lookup failures are returned as errors.
func (s *Store) ResolveTranscriptPin(id string, searchPaths []string) (string, error) {
	if !s.Backed() {
		return "", nil
	}
	info, err := s.Get(id)
	if err != nil {
		return "", err
	}
	if pinned := PinnedTranscriptPath(info); pinned != "" {
		return pinned, nil
	}
	path, _, err := transcriptLadder(info, func() ([]Info, error) {
		return s.liveSameWorkDirSessions(info)
	}, searchPaths, nil)
	if err != nil {
		return "", err
	}
	return path, nil
}

func callTranscriptCapture(capture func() string) string {
	if capture == nil {
		return ""
	}
	return capture()
}

// liveSameWorkDirSessions returns the open sessions sharing info's workdir,
// including info itself. Closed sessions are excluded: a live target is only
// ambiguous against the sessions currently able to be writing that workdir's
// transcripts, which is what makes the retirement pin resolvable at all.
func (s *Store) liveSameWorkDirSessions(info Info) ([]Info, error) {
	workDir := info.WorkDir
	if strings.TrimSpace(workDir) == "" {
		return nil, nil
	}
	candidates, err := s.ListByMetadataInfos(map[string]string{beadmeta.LegacyWorkDirMetadataKey: workDir}, 0)
	if err != nil {
		return nil, err
	}
	provider := transcriptProvider(info)
	var same []Info
	for _, other := range candidates {
		if other.Closed || !IsSessionBeadOrRepairableInfo(other) {
			continue
		}
		if other.WorkDir != workDir {
			continue
		}
		if provider != "" && transcriptProvider(other) != provider {
			continue
		}
		same = append(same, other)
	}
	return same, nil
}
