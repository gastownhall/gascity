package chartest

import "regexp"

// DefaultRules returns the canonicalization rules for gascity CLI output under
// the harness's file-store configuration (GC_BEADS=file → MemStore mints ids
// "gc-<n>"). It deliberately matches ONLY volatile minted tokens:
//
//   - bead ids: gc-<decimal>. Anchored with \b and \d+ (not [a-z0-9]+) so it
//     never clips the "gc" binary name, "gc-hosted"/"gc-runtime", a bare rig
//     prefix, or molecule refs like "mol-adopt-pr-v2".
//   - timestamps: RFC3339 / RFC3339Nano as emitted by stdlib time.Time JSON
//     marshaling — variable-length fractional seconds and either a trailing Z
//     or a numeric offset (local time serializes as ±hh:mm, not Z).
//
// It does NOT match stable identifiers (formula names, stable graph anchors
// like gcg-run-root, schema versions, small integers) or the real Dolt
// "ga-<base36>" ids, which are never minted under GC_DOLT=skip. Callers running
// against a real Dolt store add an anchored ga- rule themselves.
//
// Deliberately NOT canonicalized (so a golden containing one flakes LOUDLY —
// add a rule or redact at the source rather than let it silently mask a diff):
// t.TempDir() paths, httptest 127.0.0.1:<port> URLs, request/run UUIDs, and any
// non-RFC3339 time form (e.g. Go's time.Time.String(), which uses a space, not
// a T, separator). Keep such volatile tokens out of captured surfaces.
func DefaultRules() []Rule {
	return []Rule{
		{Category: "BEAD", Pattern: regexp.MustCompile(`\bgc-\d+\b`)},
		// The Go zero time is a STABLE sentinel meaning "unset", not a volatile
		// timestamp. Canonicalize it to a distinct placeholder (ahead of the
		// generic T rule, which would otherwise map a real->zero regression to
		// the same T-n and hide it). Same-span ties resolve to the earlier rule.
		{Category: "TZERO", Pattern: regexp.MustCompile(`0001-01-01T00:00:00(?:\.0+)?Z`)},
		{Category: "T", Pattern: regexp.MustCompile(`\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d+)?(?:Z|[+-]\d{2}:\d{2})`)},
	}
}
