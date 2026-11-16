package api

import (
	"fmt"
	"time"

	"github.com/gastownhall/gascity/internal/api/genclient"
	"github.com/gastownhall/gascity/internal/session"
)

// SessionList is one whole GET /sessions read: every page's rows plus the
// server's report of a partial read. Partial is true when any page said so
// or named a failed read; PartialErrors keeps each message once, in
// first-seen order, because every page re-reads every backend.
// AgentNameMissing is true when some row came without the agent_name key: a
// current server sends it on every session, so its absence marks a server
// that predates agent_name, transport and worker_dir.
type SessionList struct {
	Sessions         []SessionListView
	Partial          bool
	PartialErrors    []string
	AgentNameMissing bool
}

// sessionListPageLacksAgentName reports whether some row of one GET
// /sessions page came without the agent_name key.
func sessionListPageLacksAgentName(body *genclient.ListBodySessionResponse) bool {
	if body == nil || body.Items == nil {
		return false
	}
	for _, item := range *body.Items {
		if item.AgentName == nil {
			return true
		}
	}
	return false
}

// sessionListDegradation accumulates the partial report over the pages of
// one session list read.
type sessionListDegradation struct {
	partial bool
	errs    []string
	seen    map[string]bool
}

// add folds one page's partial report into d.
func (d *sessionListDegradation) add(body *genclient.ListBodySessionResponse) {
	if body == nil {
		return
	}
	if body.Partial != nil && *body.Partial {
		d.partial = true
	}
	if body.PartialErrors == nil {
		return
	}
	for _, msg := range *body.PartialErrors {
		d.partial = true
		if d.seen[msg] {
			continue
		}
		if d.seen == nil {
			d.seen = make(map[string]bool)
		}
		d.seen[msg] = true
		d.errs = append(d.errs, msg)
	}
}

// SessionListView is the CLI-facing shape of one `gc session list` row read
// through the API, with its timestamps parsed. Closed is true when the API
// reports the closed lifecycle state. The API redacts command, session_key
// and the resume fields, so the view has none of them. It carries no JSON
// tags: the CLI maps it onto its own row type, which both routes share.
// WakeReasons are why the controller keeps the session awake, sent only when
// the read asked for them (ListSessionsOpts.WakeReasons).
type SessionListView struct {
	ID          string
	Template    string
	State       string
	Closed      bool
	Reason      string
	WakeReasons []string
	Title       string
	Alias       string
	AgentName   string
	Provider    string
	Transport   string
	SessionName string
	WorkDir     string
	WorkerDir   string
	// CreatedAt, LastActive and LastNudgeDeliveredAt are the zero time when
	// the API omits them.
	CreatedAt            time.Time
	LastActive           time.Time
	LastNudgeDeliveredAt time.Time
	Attached             bool
	// Running is the server's own observation of the session runtime.
	Running    bool
	LastOutput string
	// ProgramStatus is the session program's own status report; nil means
	// unknown, never idle.
	ProgramStatus *SessionProgramStatus
}

// sessionListViewsFromGen translates one page of the generated session list
// into SessionListView rows. A missing body or item list yields an empty,
// non-nil slice; a malformed timestamp is an error naming the session.
func sessionListViewsFromGen(body *genclient.ListBodySessionResponse) ([]SessionListView, error) {
	if body == nil || body.Items == nil {
		return []SessionListView{}, nil
	}
	items := *body.Items
	out := make([]SessionListView, 0, len(items))
	for _, item := range items {
		v, err := sessionListViewFromGen(item)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, nil
}

// sessionListViewFromGen translates one generated session response into a
// SessionListView, dereferencing optional fields and parsing timestamps.
func sessionListViewFromGen(g genclient.SessionResponse) (SessionListView, error) {
	v := SessionListView{
		ID:            g.Id,
		Template:      g.Template,
		State:         g.State,
		Closed:        g.State == string(session.BaseStateClosed),
		Reason:        derefStr(g.Reason),
		WakeReasons:   derefStrings(g.WakeReasons),
		Title:         g.Title,
		Alias:         derefStr(g.Alias),
		AgentName:     derefStr(g.AgentName),
		Provider:      g.Provider,
		Transport:     derefStr(g.Transport),
		SessionName:   g.SessionName,
		WorkDir:       derefStr(g.WorkDir),
		WorkerDir:     derefStr(g.WorkerDir),
		Attached:      g.Attached,
		Running:       g.Running,
		LastOutput:    derefStr(g.LastOutput),
		ProgramStatus: programStatusFromGen(g.ProgramStatus),
	}
	var err error
	if v.CreatedAt, err = parseSessionListTime(g.Id, "created_at", g.CreatedAt); err != nil {
		return SessionListView{}, err
	}
	if v.LastActive, err = parseSessionListTime(g.Id, "last_active", derefStr(g.LastActive)); err != nil {
		return SessionListView{}, err
	}
	if v.LastNudgeDeliveredAt, err = parseSessionListTime(g.Id, "last_nudge_delivered_at", derefStr(g.LastNudgeDeliveredAt)); err != nil {
		return SessionListView{}, err
	}
	return v, nil
}

// parseSessionListTime parses one RFC 3339 session timestamp. An empty value
// is the zero time.
func parseSessionListTime(id, field, raw string) (time.Time, error) {
	if raw == "" {
		return time.Time{}, nil
	}
	t, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return time.Time{}, fmt.Errorf("session %s %s %q: %w", id, field, raw, err)
	}
	return t, nil
}

// derefStrings returns the list p points to, or nil.
func derefStrings(p *[]string) []string {
	if p == nil {
		return nil
	}
	return *p
}
