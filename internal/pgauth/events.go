package pgauth

import "github.com/gastownhall/gascity/internal/events"

// PostgresCredentialResolvedPayload is emitted on
// events.PostgresCredentialResolved each time gc successfully resolves a
// Postgres password for a scope. The payload identifies the scope and
// the resolution tier that supplied the value; it never carries the
// value itself (asserted by TestPostgresEventOmitsPassword).
type PostgresCredentialResolvedPayload struct {
	ScopeKind string `json:"scope_kind"` // "city" or "rig"
	ScopeName string `json:"scope_name"` // city name, or rig name (no scheme prefix)
	Source    string `json:"source"`     // pgauth.Source.String()
	Host      string `json:"host"`       // contract.MetadataState.PostgresHost
	Port      string `json:"port"`       // contract.MetadataState.PostgresPort (string, mirrors metadata)
	User      string `json:"user"`       // contract.MetadataState.PostgresUser
}

// IsEventPayload marks PostgresCredentialResolvedPayload as an
// events.Payload variant.
func (PostgresCredentialResolvedPayload) IsEventPayload() {}

func init() {
	events.RegisterPayload(events.PostgresCredentialResolved, PostgresCredentialResolvedPayload{})
}
