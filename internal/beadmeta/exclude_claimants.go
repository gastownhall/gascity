package beadmeta

import "strings"

// ExcludeClaimantsMetadataKey (gc.exclude_claimants, declared in keys.go)
// names, comma-separated, the fully qualified identities that must not claim,
// adopt or be continuation-pinned to a bead even when it is routed to their
// pool. A pack writes it; `gc hook --claim` enforces it.

// ExcludedClaimants parses a gc.exclude_claimants value into its member
// identities: comma-separated, each trimmed, empty members dropped. A nil
// result means the bead excludes nobody.
func ExcludedClaimants(value string) []string {
	var out []string
	for _, member := range strings.Split(value, ",") {
		if member = strings.TrimSpace(member); member != "" {
			out = append(out, member)
		}
	}
	return out
}

// ExcludesClaimant reports whether metadata's gc.exclude_claimants names any
// of identities. It is the one predicate every work-serving decision in
// `gc hook --claim` answers with: the fresh claim, the ready-assignment
// promotion, adoption of an in_progress bead, the canonical readback after a
// claim, and continuation-group pre-assignment. The default work query's jq
// clause (internal/config) mirrors it so an excluded head never hides allowed
// work from a bounded read.
//
// Matching is exact after trimming, and case-sensitive like route identities:
// config accepts case-differing agent names as distinct agents. Members are
// identities, not routes, so excluding one seat never excludes its pool. An
// empty identity never matches.
func ExcludesClaimant(metadata map[string]string, identities ...string) bool {
	excluded := ExcludedClaimants(metadata[ExcludeClaimantsMetadataKey])
	if len(excluded) == 0 {
		return false
	}
	for _, identity := range identities {
		identity = strings.TrimSpace(identity)
		if identity == "" {
			continue
		}
		for _, member := range excluded {
			if member == identity {
				return true
			}
		}
	}
	return false
}
