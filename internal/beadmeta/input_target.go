package beadmeta

import "strings"

// InputTargetIDs returns the distinct bead ids a synthetic input convoy's
// metadata names as its recorded target, independent of the convoy's tracking
// edges: InputTargetIDMetadataKey ("gc.input_target_id", stamped by graph.v2's
// single-item input convoys at creation) first, then DrainMemberIDMetadataKey
// ("gc.drain_member_id", stamped by dispatch's drain unit convoys).
//
// A purge of the tracked bead deletes the bead and can cascade the tracking
// edge away with it, which is exactly when these stamps are the only record
// of what the convoy tracked; the claim-time purge probe (#8729) uses them to
// prove the recorded target is absent rather than treat the convoy as
// unresolvable. Stamps that are empty or entirely whitespace are dropped, and
// an id recorded under both keys is reported once, in stamp order.
func InputTargetIDs(metadata map[string]string) []string {
	var ids []string
	seen := make(map[string]bool, 2)
	for _, value := range []string{
		metadata[InputTargetIDMetadataKey],
		metadata[DrainMemberIDMetadataKey],
	} {
		value = strings.TrimSpace(value)
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		ids = append(ids, value)
	}
	return ids
}
