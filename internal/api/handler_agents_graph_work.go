package api

import (
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
)

// graphSessionAssigneeInfix is the literal separating an agent's session name
// from the wisp session id in a graph-resident (graph.v2) work bead's assignee.
// A relocated graph store names wisp assignees
// "<sessionName>-gcg-session-<uuid>", where <sessionName> is exactly
// agent.SessionNameFor(city, qualifiedName, template) — the same string the
// runtime provider is probed with by statusProviderRunning. Splitting on this
// infix recovers the agent session-name prefix, so work executed under an
// agent-agnostic wisp session is still attributed to the agent that owns it.
//
// Confirmed against a live relocated graph store: every in-progress wisp
// assignee has the form "<sanitized-agent-name>-gcg-session-<32 hex>", e.g.
// "bd__dog-gcg-session-917c4dd26bd772e2463919029a889e0e".
const graphSessionAssigneeInfix = "-gcg-session-"

// graphActiveWorkListLimit bounds the single in-progress graph-store list the
// agent/status read path issues per request. It is a safety cap on a healthy
// fleet's concurrent wisp count, not a correctness bound: the index only keeps
// one bead per distinct agent session-name prefix.
const graphActiveWorkListLimit = 512

// graphAgentWork is a single agent's active graph-resident work: the id of an
// in-progress wisp bead assigned to the agent and the bead's last-activity
// time. The timestamp feeds computeAgentState so a graph-working agent reads
// "working" (recent) rather than "waiting" (stale), even though its named
// provider session is not the one executing the work.
type graphAgentWork struct {
	beadID       string
	lastActivity time.Time
}

// graphActiveWorkBySession lists the relocated graph store's in-progress beads
// once and indexes them by the agent session-name prefix embedded in each wisp
// assignee. It is computed a single time per request and shared across the
// per-agent status and agent-list loops, so those loops never fan a query per
// agent.
//
// On a default (single-store) city the graph class is not relocated, so
// GraphBeadStore().Store equals CityBeadStore() (or is nil) and this returns
// nil — every downstream lookup misses and the read path stays byte-identical
// to the pre-seam single-store behavior. Only a city that has RELOCATED its
// graph class to a dedicated store is scanned, matching the guard the other
// relocated-graph legs use (see isGraphConvoyID in handler_convoys.go).
func (s *Server) graphActiveWorkBySession() map[string]graphAgentWork {
	graphStore := s.state.GraphBeadStore().Store
	if graphStore == nil || graphStore == s.state.CityBeadStore() {
		return nil
	}
	inProgress, err := graphStore.List(beads.ListQuery{
		Status: "in_progress",
		Limit:  graphActiveWorkListLimit,
		Sort:   beads.SortCreatedDesc,
	})
	if err != nil || len(inProgress) == 0 {
		return nil
	}
	bySession := make(map[string]graphAgentWork, len(inProgress))
	for _, b := range inProgress {
		prefix, ok := sessionPrefixFromWispAssignee(b.Assignee)
		if !ok {
			continue
		}
		// SortCreatedDesc lists newest first, so the first bead seen for a
		// prefix is the freshest wisp; keep it and skip older duplicates.
		if _, exists := bySession[prefix]; exists {
			continue
		}
		bySession[prefix] = graphAgentWork{
			beadID:       b.ID,
			lastActivity: beadActivityTime(b),
		}
	}
	return bySession
}

// sessionPrefixFromWispAssignee splits a graph wisp assignee of the form
// "<sessionName>-gcg-session-<uuid>" into its "<sessionName>" prefix. It
// reports ok=false for an assignee that carries no wisp-session infix (a plain
// exact-assignee bead) or whose prefix would be empty.
func sessionPrefixFromWispAssignee(assignee string) (string, bool) {
	i := strings.Index(assignee, graphSessionAssigneeInfix)
	if i <= 0 {
		return "", false
	}
	return assignee[:i], true
}

// beadActivityTime returns a bead's most recent activity timestamp, preferring
// UpdatedAt and falling back to CreatedAt for legacy rows whose UpdatedAt is
// zero (mirroring the UpdatedBefore fallback in beads.ListQuery.Matches).
func beadActivityTime(b beads.Bead) time.Time {
	if !b.UpdatedAt.IsZero() {
		return b.UpdatedAt
	}
	return b.CreatedAt
}
