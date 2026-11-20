package events

import "encoding/json"

// ProviderUnresolvedPayload explains why configured templates cannot start.
// A failure episode emits once per provider/command and alerts again after recovery.
type ProviderUnresolvedPayload struct {
	Provider         string   `json:"provider"`
	Command          string   `json:"command"`
	DroppedTemplates int      `json:"dropped_templates"`
	Templates        []string `json:"templates"`
	// The legacy reconciler builds a desired set. V2 reports blocked allocation
	// or wake attempts instead and omits this count.
	DesiredSessionCount *int   `json:"desired_session_count,omitempty"`
	Reconciler          string `json:"reconciler"`
}

// IsEventPayload marks the provider executable diagnostic as a typed event.
func (ProviderUnresolvedPayload) IsEventPayload() {}

// ProviderUnresolvedPayloadJSON encodes the diagnostic for the event bus.
func ProviderUnresolvedPayloadJSON(p ProviderUnresolvedPayload) json.RawMessage {
	b, _ := json.Marshal(p) //nolint:errcheck // scalars and strings only
	return b
}

func init() { RegisterPayload(ProviderUnresolved, ProviderUnresolvedPayload{}) }
