package beadmeta

import (
	"fmt"
	"regexp"
	"slices"
	"strings"
)

// ValidKeyPattern mirrors the beads storage rule for metadata key names. Keys
// are rendered into MySQL/Dolt JSON path expressions, so the backend restricts
// them to a leading letter or underscore followed by alphanumerics,
// underscores, dots and slashes.
//
// This is a mirror, not a reference: the backend's own ValidateMetadataKey lives
// in the beads module's internal/storage package and is not importable by any
// consumer, so a caller that wants to know whether a key will be accepted has to
// restate the pattern. Keep it byte-identical to the library's
// validMetadataKeyRe; internal/beads' integration tier asserts that the real
// backend's refusal quotes this exact string.
const ValidKeyPattern = `^[a-zA-Z_][a-zA-Z0-9_./]*$`

var validKeyRe = regexp.MustCompile(ValidKeyPattern)

// ValidKey reports whether key is accepted by every bead-metadata write route.
//
// The backend does not apply this rule uniformly: a create carrying a
// non-conforming key succeeds, while a later update or metadata batch that
// names the same key is refused. Callers that mint keys should check here so a
// key can never be written by one route and then jam another.
func ValidKey(key string) bool { return validKeyRe.MatchString(key) }

// ValidateKey returns an error describing why key is not a usable bead-metadata
// key, or nil when it is.
func ValidateKey(key string) error {
	if ValidKey(key) {
		return nil
	}
	return fmt.Errorf("invalid metadata key %q: must match %s", key, ValidKeyPattern)
}

// CopyUserKeys copies the caller-owned metadata in src into dst, skipping the
// empty key, every engine-owned Namespace key, and every key the backend would
// refuse on a write. It returns the skipped non-conforming keys in sorted order
// so the caller can report what it dropped.
//
// Propagating metadata from one bead onto another is a read-then-write, and the
// read side is more permissive than the write side: a bead created with a
// non-conforming key hands that key to whoever copies its metadata, and the
// write that carries it forward is refused every time it is retried. Dropping
// the key keeps a workflow moving; carrying it forward wedges the workflow on a
// value nothing reads.
func CopyUserKeys(dst, src map[string]string) []string {
	var skipped []string
	for key, value := range src {
		if key == "" || strings.HasPrefix(key, Namespace) {
			continue
		}
		if !ValidKey(key) {
			skipped = append(skipped, key)
			continue
		}
		dst[key] = value
	}
	slices.Sort(skipped)
	return skipped
}
