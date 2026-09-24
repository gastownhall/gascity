package events

// Storage-backend event payloads. Defined and registered here rather than
// beside an emitter because gc no longer owns one: a scope bound to a backend
// gc does not implement is served by the linked beads library, which runs its
// own credential ladder behind an environment namespace gc withholds whole.
//
// The type survives that move deliberately. It is a variant of the SSE
// payload union and a schema in the generated OpenAPI document, so deleting it
// would not remove a capability — it would fork the wire contract between two
// assemblies of the same product, which is the one thing the typed-wire
// invariant exists to prevent.

// BackendCredentialResolvedPayload is the typed payload for
// backend.credential_resolved: one scope's storage credential was resolved,
// and by which tier.
//
// It never carries the credential. Host, Port and User locate the endpoint an
// operator would look at; the value itself has no reason to leave the process
// that resolved it, and an event log outlives the file a password came from.
type BackendCredentialResolvedPayload struct {
	// Backend is the storage backend the credential was resolved for, spelled
	// the way the scope's metadata spells it.
	Backend string `json:"backend"`
	// ScopeKind is "city" or "rig".
	ScopeKind string `json:"scope_kind"`
	// ScopeName is the city name, or the rig name with no scheme prefix.
	ScopeName string `json:"scope_name"`
	// Source names the resolution tier that supplied the value, in whatever
	// stable vocabulary the resolving assembly publishes.
	Source string `json:"source"`
	// Host is the endpoint host the credential authenticates against.
	Host string `json:"host"`
	// Port is the endpoint port, as a string, mirroring metadata.
	Port string `json:"port"`
	// User is the endpoint user the credential belongs to.
	User string `json:"user"`
}

// IsEventPayload marks BackendCredentialResolvedPayload as a Payload variant.
func (BackendCredentialResolvedPayload) IsEventPayload() {}

func init() {
	RegisterPayload(BackendCredentialResolved, BackendCredentialResolvedPayload{})
}
