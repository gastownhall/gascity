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
	WorkDir     string `json:"work_dir"`
	CreatedAt   string `json:"created_at"`
	LastActive  string `json:"last_active"`
	Attached    bool   `json:"attached"`
	Running     bool   `json:"running"`
	LastOutput  string `json:"last_output"`
	// ProgramStatus is the session program's own status report, as the API
	// reports it; absent means unknown, never idle.
	ProgramStatus *SessionProgramStatus `json:"program_status,omitempty"`
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
	if g.WorkDir != nil {
		out.WorkDir = *g.WorkDir
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
	out.ProgramStatus = programStatusFromGen(g.ProgramStatus)
	return out
}

// programStatusFromGen translates the generated client's program status into
// the wire type; nil stays nil (unknown).
func programStatusFromGen(g *genclient.SessionProgramStatus) *SessionProgramStatus {
	if g == nil {
		return nil
	}
	out := &SessionProgramStatus{
		State:           string(g.State),
		Since:           g.Since,
		QuietSince:      g.QuietSince,
		WorkingChildren: int(g.WorkingChildren),
	}
	if g.Kind != nil {
		out.Kind = string(*g.Kind)
	}
	if g.App != nil {
		out.App = *g.App
	}
	if g.Progress != nil {
		progress := int(*g.Progress)
		out.Progress = &progress
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
