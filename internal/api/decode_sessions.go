package api

import "github.com/gastownhall/gascity/internal/api/genclient"

// SessionView is the CLI-facing shape for `gc session list` rows and
// `gc session peek` output. It mirrors the subset of server-side
// sessionResponse fields that the CLI formatter reads so cmd/gc/ never
// imports genclient directly.
type SessionView struct {
	ID          string `json:"id"`
	Template    string `json:"template"`
	State       string `json:"state"`
	Reason      string `json:"reason"`
	Title       string `json:"title"`
	Alias       string `json:"alias"`
	SessionName string `json:"session_name"`
	CreatedAt   string `json:"created_at"`
	LastActive  string `json:"last_active"`
	Attached    bool   `json:"attached"`
	Running     bool   `json:"running"`
	LastOutput  string `json:"last_output"`
}

// sessionViewFromGen translates one genclient.SessionResponse into a
// SessionView. Optional pointer fields are dereferenced safely.
func sessionViewFromGen(g genclient.SessionResponse) SessionView {
	out := SessionView{
		ID:          g.Id,
		Template:    g.Template,
		State:       g.State,
		Title:       g.Title,
		SessionName: g.SessionName,
		CreatedAt:   g.CreatedAt,
		Attached:    g.Attached,
		Running:     g.Running,
	}
	if g.Reason != nil {
		out.Reason = *g.Reason
	}
	if g.Alias != nil {
		out.Alias = *g.Alias
	}
	if g.LastActive != nil {
		out.LastActive = *g.LastActive
	}
	if g.LastOutput != nil {
		out.LastOutput = *g.LastOutput
	}
	return out
}

// sessionsFromGenList translates the genclient list body into
// []SessionView. Returns an empty slice (never nil) when the body is
// missing or holds no items so callers can uniformly format the empty
// case.
func sessionsFromGenList(body *genclient.ListBodySessionResponse) []SessionView {
	if body == nil || body.Items == nil {
		return []SessionView{}
	}
	items := *body.Items
	out := make([]SessionView, 0, len(items))
	for _, item := range items {
		out = append(out, sessionViewFromGen(item))
	}
	return out
}
