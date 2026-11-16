package sessionlog

import "encoding/json"

// HookPayloadTurnID is the turn id a provider's hook payload names, read
// from the field TurnSupportFor(provider).HookTurnID names (X11 D13; DES-O1
// §4.8's row 1 for an in-band delivery). It is "" for a family whose field
// X11's live arm has not verified, so until it does no hook payload links a
// delivery exactly; it is "" too for a payload that is not a JSON object or
// whose field is not a usable turn id (validTurnID).
func HookPayloadTurnID(provider string, payload []byte) string {
	return hookPayloadTurnID(TurnSupportFor(provider).HookTurnID, payload)
}

// hookPayloadTurnID reads field from payload as a turn id, "" when field is
// empty or the payload carries no usable id there.
func hookPayloadTurnID(field string, payload []byte) string {
	if field == "" || len(payload) == 0 {
		return ""
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(payload, &fields); err != nil {
		return ""
	}
	raw, ok := fields[field]
	if !ok {
		return ""
	}
	var id string
	if err := json.Unmarshal(raw, &id); err != nil {
		return ""
	}
	id, _ = validTurnID(id)
	return id
}

// HookFamily is the family whose hooks a provider runs, read from its name
// alone, as a hook's GC_PROVIDER names it: its turn family (turnFamilyKey),
// so groq and cerebras run the opencode plugin and a claude spelling no other
// family claims ("claude-eco", "claude/tmux-cli") is claude.
func HookFamily(provider string) string {
	return turnFamilyKey(provider)
}
