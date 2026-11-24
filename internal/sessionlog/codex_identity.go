package sessionlog

import (
	"bufio"
	"encoding/json"
	"io"
	"os"
	"time"
)

// CodexKeyedOutcome classifies a bounded keyed rollout lookup.
type CodexKeyedOutcome string

const (
	// CodexKeyedFound identifies one rollout proven to belong to the key.
	CodexKeyedFound CodexKeyedOutcome = "found"
	// CodexKeyedAbsent means no rollout filename carries the key.
	CodexKeyedAbsent CodexKeyedOutcome = "absent"
	// CodexKeyedAmbiguous means distinct physical rollouts carry the key.
	CodexKeyedAmbiguous CodexKeyedOutcome = "ambiguous"
	// CodexKeyedIdentityMismatch means a complete first line contradicts the key.
	CodexKeyedIdentityMismatch CodexKeyedOutcome = "identity_mismatch"
	// CodexKeyedUncertain means bounded IO could not establish an answer.
	CodexKeyedUncertain CodexKeyedOutcome = "uncertain"
	// CodexKeyedInvalid means the lookup arguments cannot describe a search.
	CodexKeyedInvalid CodexKeyedOutcome = "invalid"
)

// CodexKeyedLookup carries the selected path and its identity verdict.
type CodexKeyedLookup struct {
	Path       string
	Outcome    CodexKeyedOutcome
	CWDDiffers bool
}

// codexKeyedTargetKey is the single batch-correlation key of a keyed lookup.
const codexKeyedTargetKey = "identity"

// FindCodexSessionFileByIdentity resolves a keyed managed Codex session's own
// rollout by identity. The rollout is found when exactly one distinct physical
// file under the searched day directories carries key as its filename suffix
// and its first-line session_meta payload.id equals key. Its cwd is reported
// through CWDDiffers and never enforced, so a rollout whose cwd drifted from
// workDir is still found. A first line without a payload.id falls back to the
// cwd rule: the cwd must equal workDir.
//
// The arguments, the searched days and the request-wide ReadDir budget are
// those of FindCodexSessionFileByID. The first line is read at most once, and
// never when the arguments are invalid, no file or several distinct files
// carry the key, or the directory scan was incomplete. The function reads no
// clock, logs nothing and writes nothing.
func FindCodexSessionFileByIdentity(searchPaths []string, workDir, key string, notBefore, notAfter time.Time) CodexKeyedLookup {
	return findCodexSessionFileByIdentityWithReaders(searchPaths, workDir, key, notBefore, notAfter, os.ReadDir, openCodexRollout)
}

func openCodexRollout(path string) (io.ReadCloser, error) {
	return os.Open(path)
}

// findCodexSessionFileByIdentityWithReaders is the function-valued IO seam for
// fault and work-bound assertions: readDir lists directories and open opens the
// one rollout whose first line is read.
func findCodexSessionFileByIdentityWithReaders(
	searchPaths []string, workDir, key string, notBefore, notAfter time.Time,
	readDir func(string) ([]os.DirEntry, error),
	open func(string) (io.ReadCloser, error),
) CodexKeyedLookup {
	invalid := CodexKeyedLookup{Outcome: CodexKeyedInvalid}
	if readDir == nil || open == nil {
		return invalid
	}
	states := buildCodexBatchTargets([]CodexSessionTarget{{
		Key:       codexKeyedTargetKey,
		WorkDir:   workDir,
		SessionID: key,
		NotBefore: notBefore,
		NotAfter:  notAfter,
	}})
	if len(states) != 1 {
		return invalid
	}
	state := &states[0]
	if !scanCodexBatchStates(searchPaths, states, readDir, lookupCodexBatchEntrySessionID) {
		return CodexKeyedLookup{Outcome: CodexKeyedUncertain}
	}
	switch len(state.matches) {
	case 0:
		return CodexKeyedLookup{Outcome: CodexKeyedAbsent}
	case 1:
		return resolveCodexIdentity(state, open)
	default:
		return CodexKeyedLookup{Outcome: CodexKeyedAmbiguous}
	}
}

// resolveCodexIdentity reads the sole match's first line once and classifies it
// against the target's key and work directory.
func resolveCodexIdentity(state *codexBatchTarget, open func(string) (io.ReadCloser, error)) CodexKeyedLookup {
	path := state.matches[0]
	meta, complete := readCodexIdentityFirstLine(open, path)
	if !complete {
		return CodexKeyedLookup{Outcome: CodexKeyedUncertain}
	}
	if meta.Type != "session_meta" {
		return CodexKeyedLookup{Outcome: CodexKeyedIdentityMismatch}
	}
	cwd := meta.Payload.CWD
	if meta.Payload.ID == "" {
		if cwd != state.workDir {
			return CodexKeyedLookup{Outcome: CodexKeyedIdentityMismatch}
		}
		return CodexKeyedLookup{Path: path, Outcome: CodexKeyedFound}
	}
	if meta.Payload.ID != state.sessionID {
		return CodexKeyedLookup{Outcome: CodexKeyedIdentityMismatch}
	}
	return CodexKeyedLookup{Path: path, Outcome: CodexKeyedFound, CWDDiffers: cwd != "" && cwd != state.workDir}
}

// codexIdentityFirstLine is the part of a rollout's first line that decides
// identity.
type codexIdentityFirstLine struct {
	Type    string `json:"type"`
	Payload struct {
		ID  string `json:"id"`
		CWD string `json:"cwd"`
	} `json:"payload"`
}

// readCodexIdentityFirstLine decodes a rollout's first line with the same
// scanner bounds as codexSessionCandidateScan (64 KiB initial buffer, 1 MiB
// cap). complete is false for any open or read fault, an empty file, a line over
// the cap, and anything that is not exactly one valid JSON value, which includes
// a line cut short by a write still in progress.
func readCodexIdentityFirstLine(open func(string) (io.ReadCloser, error), path string) (meta codexIdentityFirstLine, complete bool) {
	f, err := open(path)
	if err != nil {
		return codexIdentityFirstLine{}, false
	}
	defer f.Close() //nolint:errcheck // read-only
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	if !scanner.Scan() || scanner.Err() != nil {
		return codexIdentityFirstLine{}, false
	}
	if err := json.Unmarshal(scanner.Bytes(), &meta); err != nil {
		return codexIdentityFirstLine{}, false
	}
	return meta, true
}
