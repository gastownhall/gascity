package beadmeta

import "strings"

// AwaitsHuman reports whether asker, a claimed work bead's
// AwaitingHumanMetadataKey, says the seat holding the claim (seat, its
// session ID) asked a person and is waiting on their decision: asker names
// that seat. A key naming another seat (one whose claim was released since)
// does not.
func AwaitsHuman(asker, seat string) bool {
	asker = strings.TrimSpace(asker)
	return asker != "" && asker == strings.TrimSpace(seat)
}
