package sessionlog

import (
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

// FindCodexSessionFileByIdentity resolves a rollout by filename and first-line identity.
// This is compile-only scaffolding for ga-9iikxat3's test-first handoff.
func FindCodexSessionFileByIdentity(_ []string, _, _ string, _, _ time.Time) CodexKeyedLookup {
	return CodexKeyedLookup{}
}

// findCodexSessionFileByIdentityWithReaders is the function-valued IO seam for
// fault and work-bound assertions. The builder supplies the shared batch scan
// and single first-line reader; this compile-only scaffold implements neither.
func findCodexSessionFileByIdentityWithReaders(
	_ []string, _, _ string, _, _ time.Time,
	_ func(string) ([]os.DirEntry, error),
	_ func(string) (io.ReadCloser, error),
) CodexKeyedLookup {
	return CodexKeyedLookup{}
}
