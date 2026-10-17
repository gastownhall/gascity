package session

import "strings"

// LogicalConversationID is the durable identity of the session's current
// conversation: the bead ID scoped by its continuation epoch, as
// "<bead-id>#<epoch>". A fresh reset advances the epoch and so starts a new
// logical conversation; a resumed restart keeps it. A fresh start also drops
// the session key, and a provider with a session-id flag runs the new
// conversation under a new one, even after a fresh wake that keeps the epoch,
// so for such a provider the key scopes the identity too, as
// "<bead-id>#<epoch>/<session-key>". Any other provider's key is learned after
// launch, from a hook or its transcript: it names the conversation the epoch
// already identifies, so a late report keeps the identity. A missing or
// invalid epoch reads as DefaultContinuationEpoch. An Info without a bead ID
// has no logical conversation and yields "".
func (i Info) LogicalConversationID() string {
	id := strings.TrimSpace(i.ID)
	if id == "" {
		return ""
	}
	logical := id + "#" + transcriptContinuationEpoch(i.ContinuationEpoch)
	if key := strings.TrimSpace(i.SessionKey); key != "" && strings.TrimSpace(i.SessionIDFlag) != "" {
		logical += "/" + key
	}
	return logical
}
