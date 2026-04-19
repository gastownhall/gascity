package api

type packResponse struct {
	Name   string `json:"name"`
	Source string `json:"source,omitempty"`
	Ref    string `json:"ref,omitempty"`
	Path   string `json:"path,omitempty"`
}
