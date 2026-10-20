package beadmeta

import "strings"

// AwaitsHuman reports whether a claimed work bead's metadata says the seat
// holding the claim (its session ID) asked a person and is waiting on their
// decision: AwaitingHumanMetadataKey names that seat. A key naming another
// seat (one whose claim was released since) does not.
func AwaitsHuman(meta map[string]string, seat string) bool {
	asker := strings.TrimSpace(meta[AwaitingHumanMetadataKey])
	return asker != "" && asker == strings.TrimSpace(seat)
}
