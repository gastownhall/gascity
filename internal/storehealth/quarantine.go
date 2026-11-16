package storehealth

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// maxScopeTokenLen caps the sanitized prefix of a scope token so a deep
// scope root cannot push a quarantine directory name past NAME_MAX.
const maxScopeTokenLen = 64

// QuarantineDirName builds the forensic quarantine directory name for a
// scope and sequence number: "<scope-token>-<seq>". The sequence is a
// monotonic counter, NEVER a wall-clock or random value, so the names are
// deterministic and tests can assert them exactly (plan item 1.5).
//
// scope roots are absolute paths, so the scope is collapsed to a
// filesystem-safe token (see sanitizeScopeToken). The collapse itself is
// lossy, so the token carries a digest of the raw scope: two distinct
// scopes never share a token, even when they differ only in punctuation.
func QuarantineDirName(scope string, seq int) string {
	return fmt.Sprintf("%s-%d", sanitizeScopeToken(scope), seq)
}

// NextQuarantineSeq returns the next monotonic sequence number for a scope
// by scanning existing "<scope-token>-<n>" entries under quarantineRoot and
// returning max(n)+1, or 0 when none exist. Derivation from directory
// entries (rather than a wall-clock or random source) keeps reaps
// deterministic and replayable.
//
// A missing root yields 0 so the first reap still produces a stable name.
// Any other read error is returned: an unreadable root may already hold
// prior artifacts, and guessing 0 would aim the next reap on top of them.
func NextQuarantineSeq(quarantineRoot, scope string) (int, error) {
	entries, err := os.ReadDir(quarantineRoot)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return 0, nil
		}
		return 0, fmt.Errorf("reading quarantine root %q: %w", quarantineRoot, err)
	}
	prefix := sanitizeScopeToken(scope) + "-"
	maxSeq := -1
	for _, e := range entries {
		name := e.Name()
		if !strings.HasPrefix(name, prefix) {
			continue
		}
		n, err := strconv.Atoi(strings.TrimPrefix(name, prefix))
		if err != nil || n < 0 {
			continue
		}
		if n > maxSeq {
			maxSeq = n
		}
	}
	return maxSeq + 1, nil
}

// QuarantineDirPath joins the quarantine root with the next sequenced
// directory name for a scope and returns the absolute path plus the seq.
// It does not create the directory — the forensics hook does that.
func QuarantineDirPath(quarantineRoot, scope string) (path string, seq int, err error) {
	seq, err = NextQuarantineSeq(quarantineRoot, scope)
	if err != nil {
		return "", 0, err
	}
	return filepath.Join(quarantineRoot, QuarantineDirName(scope, seq)), seq, nil
}

// sanitizeScopeToken collapses a scope root path into a filesystem-safe,
// stable token. The collapse is lossy (every character outside
// [A-Za-z0-9.] becomes '_'), so the token carries an 8-hex-char digest of
// the raw scope: distinct scopes can never share a token even when they
// differ only in punctuation. The sanitized prefix is capped so a deep
// scope root cannot push the directory name past NAME_MAX. The digest is
// joined with '.', never '-', so the "<token>-" prefix match in
// NextQuarantineSeq stays unambiguous.
func sanitizeScopeToken(scope string) string {
	scope = strings.TrimSpace(scope)
	sum := sha256.Sum256([]byte(scope))
	digest := hex.EncodeToString(sum[:])[:8]

	trimmed := strings.Trim(scope, string(filepath.Separator))
	if trimmed == "" {
		return "scope." + digest
	}
	var b strings.Builder
	for _, r := range trimmed {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	token := b.String()
	if len(token) > maxScopeTokenLen {
		token = token[:maxScopeTokenLen]
	}
	return token + "." + digest
}
