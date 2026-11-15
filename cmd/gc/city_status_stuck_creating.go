package main

import (
	"fmt"
	"io"
	"time"

	"github.com/gastownhall/gascity/internal/api"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/session"
)

// cityStatusNow is the clock the status text renderer reads ages against.
// Tests replace it so rendered ages are deterministic.
var cityStatusNow = time.Now

// renderStuckCreatingBlock prints the abandoned-create block of gc status.
//
// The block has three states and only one of them is silent. A nil view was
// never measured, which prints one explicit line; a complete view with no
// sessions is a measured zero and prints nothing; anything else lists each
// session. A partial read (Complete false) never claims absence, with or
// without rows. now is the caller's clock.
func renderStuckCreatingBlock(w io.Writer, v *api.StatusStuckCreatingView, now time.Time) {
	if v == nil {
		fmt.Fprintln(w)                                           //nolint:errcheck // best-effort stdout
		fmt.Fprintln(w, "Stuck creating: unknown (not measured)") //nolint:errcheck // best-effort stdout
		return
	}
	if len(v.Sessions) == 0 {
		if v.Complete {
			return
		}
		fmt.Fprintln(w)                                                                         //nolint:errcheck // best-effort stdout
		fmt.Fprintln(w, "Stuck creating: none found (partial session read; may be incomplete)") //nolint:errcheck // best-effort stdout
		return
	}
	noun := "sessions"
	if len(v.Sessions) == 1 {
		noun = "session"
	}
	fmt.Fprintln(w)                                                                                    //nolint:errcheck // best-effort stdout
	fmt.Fprintf(w, "Stuck creating: %d %s (create abandoned, no live lease)\n", len(v.Sessions), noun) //nolint:errcheck // best-effort stdout
	for _, s := range v.Sessions {
		template := s.Template
		if template == "" {
			template = "-"
		}
		fmt.Fprintf(w, "  %-12s %-24s %-14s %s\n", s.ID, template, s.State, stuckCreatingAge(s.Since, now)) //nolint:errcheck // best-effort stdout
	}
	if !v.Complete {
		fmt.Fprintln(w, "  (lower bound: the session read was partial, so more may exist)") //nolint:errcheck // best-effort stdout
	}
	fmt.Fprintln(w, "  See gc doctor for work routed to them; gc session close <id> clears one.") //nolint:errcheck // best-effort stdout
}

// stuckCreatingAge renders the age of a create attempt, or "age unknown" for a
// row with no anchor. A since ahead of now (clock skew between the server and
// this CLI) renders as 0s rather than a negative age.
func stuckCreatingAge(since, now time.Time) string {
	if since.IsZero() {
		return "age unknown"
	}
	age := now.Sub(since)
	if age < 0 {
		age = 0
	}
	return formatDuration(age)
}

// stuckCreatingFromSessionSnapshot builds the local-fallback block from the
// loaded session snapshot, through the same rule the server uses. storePresent
// says whether the city has a bead store at all: with none, nothing can be
// stuck, which is a measurement. A snapshot that failed to load, or is missing
// while a store exists, is not measured and yields nil (unknown), never an
// empty block.
func stuckCreatingFromSessionSnapshot(storePresent bool, cfg *config.City, snap *sessionBeadSnapshot, now time.Time) *api.StatusStuckCreatingView {
	if !storePresent {
		return &api.StatusStuckCreatingView{Sessions: []api.StatusStuckCreatingSessionView{}, Complete: true}
	}
	if snap == nil || snap.LoadError() != nil {
		return nil
	}
	var startupTimeout time.Duration
	if cfg != nil {
		startupTimeout = cfg.Session.StartupTimeoutDuration()
	}
	var infos []session.Info
	for _, info := range snap.OpenInfos() {
		if info.Closed || !session.IsSessionBeadOrRepairableInfo(info) {
			continue
		}
		infos = append(infos, info)
	}
	rows := session.DetectAbandonedCreates(infos, now, startupTimeout)
	out := &api.StatusStuckCreatingView{Sessions: make([]api.StatusStuckCreatingSessionView, 0, len(rows)), Complete: true}
	for _, a := range rows {
		out.Sessions = append(out.Sessions, api.StatusStuckCreatingSessionView{
			ID:       a.ID,
			Template: a.Template,
			State:    string(a.State),
			Since:    a.Since,
		})
	}
	return out
}

// stuckCreatingWire projects the view onto the wire block gc status --json
// prints, so --json carries the same names and the same three states as the
// API. Nil stays nil (absent means unknown) and Sessions is never nil.
func stuckCreatingWire(v *api.StatusStuckCreatingView) *api.StatusStuckCreating {
	if v == nil {
		return nil
	}
	out := &api.StatusStuckCreating{Sessions: make([]api.StatusStuckCreatingSession, 0, len(v.Sessions)), Complete: v.Complete}
	for _, s := range v.Sessions {
		row := api.StatusStuckCreatingSession{ID: s.ID, Template: s.Template, State: s.State}
		if !s.Since.IsZero() {
			row.Since = s.Since.UTC().Format(time.RFC3339)
		}
		out.Sessions = append(out.Sessions, row)
	}
	return out
}
