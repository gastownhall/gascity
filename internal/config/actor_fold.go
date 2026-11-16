package config

import "strings"

// CanonicalActor folds an identity string the way the bead store compares
// actors, so gc and the store agree on which spellings name the same identity.
// It ports beads' canonicalActor (internal/storage/issueops/identity.go, and
// its exported twin validation.CanonicalActor) byte for byte; see
// docs/reference/specs/identity-separator-contract-v1.md for the separator
// alphabet it decodes:
//
//   - a run of '.', '_' or '-' that is exactly "--" becomes "/", the encoded
//     rig/agent boundary, so "beta--worker" and "beta/worker" fold alike;
//   - every other run of '.', '_' or '-' (including "__" and runs of three or
//     more) becomes a single "_";
//   - every other byte, including '/', passes through, and letter case is
//     kept: the store treats "Beta/worker" and "beta/worker" as different
//     actors;
//   - the empty string stays empty, so an absent actor never folds to the
//     same value as a present one.
//
// The scan is byte-wise. That is safe because the three separators are
// single-byte ASCII and never occur inside a multi-byte UTF-8 rune.
func CanonicalActor(s string) string {
	if s == "" {
		return ""
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); {
		if !isActorSeparator(s[i]) {
			b.WriteByte(s[i])
			i++
			continue
		}
		j := i
		for j < len(s) && isActorSeparator(s[j]) {
			j++
		}
		if s[i:j] == "--" {
			b.WriteByte('/')
		} else {
			b.WriteByte('_')
		}
		i = j
	}
	return b.String()
}

func isActorSeparator(c byte) bool {
	return c == '.' || c == '_' || c == '-'
}
