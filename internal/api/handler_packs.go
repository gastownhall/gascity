package api

// packResponse is the per-binding shape returned by GET /v0/city/{cityName}/packs.
// It mirrors the import model the add/remove handlers operate on — a binding
// name plus its durable source and optional version constraint — so what a
// client lists is exactly what it can add and remove (the forge-web UI contract
// is {name, source, version}).
type packResponse struct {
	Name    string `json:"name"`
	Source  string `json:"source,omitempty"`
	Version string `json:"version,omitempty"`
}
