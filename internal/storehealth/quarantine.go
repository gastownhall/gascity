package storehealth

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// QuarantineDirName builds the forensic quarantine directory name for a
// scope and sequence number: "<scope>-<seq>". The sequence is a monotonic
// counter, NEVER a wall-clock or random value, so the names are
// deterministic and tests can assert them exactly (plan item 1.5).
//
// scope is sanitized to a filesystem-safe token (path separators and other
// awkward characters collapse to '_') because scope roots are absolute
// paths.
func QuarantineDirName(scope string, seq int) string {
	return fmt.Sprintf("%s-%d", sanitizeScopeToken(scope), seq)
}

// NextQuarantineSeq returns the next monotonic sequence number for a scope
// by scanning existing "<scope>-<n>" entries under quarantineRoot and
// returning max(n)+1, or 0 when none exist. Derivation from directory
// entries (rather than a wall-clock or random source) keeps reaps
// deterministic and replayable. A read error on the root yields 0 so the
// first reap after a missing root still produces a stable name.
func NextQuarantineSeq(quarantineRoot, scope string) int {
	token := sanitizeScopeToken(scope)
	entries, err := os.ReadDir(quarantineRoot)
	if err != nil {
		return 0
	}
	prefix := token + "-"
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
	return maxSeq + 1
}

// QuarantineDirPath joins the quarantine root with the next sequenced
// directory name for a scope and returns the absolute path plus the seq.
// It does not create the directory — the forensics hook does that.
func QuarantineDirPath(quarantineRoot, scope string) (path string, seq int) {
	seq = NextQuarantineSeq(quarantineRoot, scope)
	return filepath.Join(quarantineRoot, QuarantineDirName(scope, seq)), seq
}

// sanitizeScopeToken collapses a scope root path into a filesystem-safe,
// stable token: leading separators are dropped and every remaining
// separator or awkward character becomes '_'. Two distinct scopes never
// collide because the full path is preserved (only the separators change).
func sanitizeScopeToken(scope string) string {
	scope = strings.TrimSpace(scope)
	scope = strings.Trim(scope, string(filepath.Separator))
	if scope == "" {
		return "scope"
	}
	var b strings.Builder
	for _, r := range scope {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	return b.String()
}

// sortedQuarantineSeqs returns the existing sequence numbers for a scope in
// ascending order. Helper for tests asserting deterministic accumulation.
func sortedQuarantineSeqs(quarantineRoot, scope string) []int {
	token := sanitizeScopeToken(scope)
	entries, err := os.ReadDir(quarantineRoot)
	if err != nil {
		return nil
	}
	prefix := token + "-"
	var seqs []int
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), prefix) {
			continue
		}
		if n, err := strconv.Atoi(strings.TrimPrefix(e.Name(), prefix)); err == nil && n >= 0 {
			seqs = append(seqs, n)
		}
	}
	sort.Ints(seqs)
	return seqs
}
