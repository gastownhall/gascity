package beadmeta

import (
	"strconv"
	"strings"
)

// Two counters live on attempt-shaped beads, and they are NOT interchangeable:
//
//   - gc.attempt keeps its v1.4.2 meaning. On a ralph iteration root and on
//     every member of a ralph body (including the retry controls and retry
//     attempt roots nested in it) it is the enclosing loop iteration — the value
//     pack gates join sibling beads on. Outside any loop, on a retry attempt
//     root, it is that step's retry counter, exactly as before.
//   - gc.retry_attempt is a retry attempt root's own 1-based counter, bounded by
//     its control's gc.max_attempts. Every retry path writes it and every retry
//     reader reads it, so a retry first reached in iteration N starts at 1
//     instead of N (ga-v7pu5).
//
// gc.iteration records the loop iteration explicitly wherever one exists.

// RetryAttemptValue returns the retry counter an attempt bead carries:
// gc.retry_attempt, or gc.attempt for a bead minted before the key existed.
//
// The fallback is what makes an upgrade safe for in-flight molecules. A bead
// minted by v1.4.2 carries only gc.attempt, which that binary also read as the
// retry counter, so the fallback reproduces its behavior exactly. A bead minted
// by a v1.5.0 build that predates this key carries its own counter in gc.attempt
// (the #5635 split), so the fallback reads the right value there too.
func RetryAttemptValue(meta map[string]string) string {
	if v := strings.TrimSpace(meta[RetryAttemptMetadataKey]); v != "" {
		return v
	}
	return strings.TrimSpace(meta[AttemptMetadataKey])
}

// RetryAttemptNumber parses RetryAttemptValue, returning 0 when it is absent or
// not an integer.
func RetryAttemptNumber(meta map[string]string) int {
	n, err := strconv.Atoi(RetryAttemptValue(meta))
	if err != nil {
		return 0
	}
	return n
}

// StampRetryAttempt writes the counters of retry attempt number n onto meta.
// gc.retry_attempt always takes n. gc.attempt takes the loop iteration when the
// bead sits inside one (gc.iteration is set) and n otherwise, which is the
// v1.4.2 contract for a body member and for a top-level retry respectively.
// Callers set or clear gc.iteration first.
func StampRetryAttempt(meta map[string]string, n int) {
	meta[RetryAttemptMetadataKey] = strconv.Itoa(n)
	if iteration := strings.TrimSpace(meta[IterationMetadataKey]); iteration != "" {
		meta[AttemptMetadataKey] = iteration
		return
	}
	meta[AttemptMetadataKey] = strconv.Itoa(n)
}
