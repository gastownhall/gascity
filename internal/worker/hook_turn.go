package worker

import "github.com/gastownhall/gascity/internal/sessionlog"

// HookPayloadTurnID is the turn id provider's hook payload names, "" unless
// X11's live arm verified that the family's hook field carries the same id as
// its transcript (sessionlog.HookPayloadTurnID). The in-band hook pass links
// a delivery exactly only through it (DES-O1 §8).
func HookPayloadTurnID(provider string, payload []byte) string {
	return sessionlog.HookPayloadTurnID(provider, payload)
}
