package api

import "github.com/gastownhall/gascity/internal/api/genclient"

// RigView is the CLI-facing shape for `gc rig list` rows. It mirrors the
// subset of fields the CLI formatter reads so cmd/gc/ never imports genclient
// directly.
type RigView struct {
	Name          string
	Path          string
	Prefix        string
	DefaultBranch string
	Suspended     bool
	RunningCount  int
}

// rigViewFromGen translates one genclient.RigResponse into a RigView.
// Optional pointer fields are dereferenced safely.
func rigViewFromGen(g genclient.RigResponse) RigView {
	out := RigView{
		Name:         g.Name,
		Path:         g.Path,
		Suspended:    g.Suspended,
		RunningCount: int(g.RunningCount),
	}
	if g.Prefix != nil {
		out.Prefix = *g.Prefix
	}
	if g.DefaultBranch != nil {
		out.DefaultBranch = *g.DefaultBranch
	}
	return out
}

// rigsFromGenList translates the genclient list body into []RigView. Returns
// an empty slice (never nil) when the body is missing or holds no items so
// callers can uniformly format the empty case.
func rigsFromGenList(body *genclient.ListBodyRigResponse) []RigView {
	if body == nil || body.Items == nil {
		return []RigView{}
	}
	items := *body.Items
	out := make([]RigView, 0, len(items))
	for _, item := range items {
		out = append(out, rigViewFromGen(item))
	}
	return out
}
