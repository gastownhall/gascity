package api

import (
	"context"

	"github.com/gastownhall/gascity/internal/config"
)

// agentSessionResolver resolves, for one request, the runtime session each
// agent row runs under, so the row's liveness is probed on a session that
// exists.
//
// A row's canonical session name (agentSessionName) is probed first, and is
// the whole answer for an agent that runs under it. A pool instance does not:
// the controller names an unaliased pool session after its session bead
// (<template>-<beadID>), which no slot name derives, so the probe of a slot's
// canonical name misses. On a miss the row takes the join GET /status makes,
// statusRuntimeSessionName over the session snapshot. The snapshot is read at
// most once per request, only after a canonical probe has missed, and never
// on a suspended city (citySessionSnapshot).
//
// The session bead only names the session. Liveness is always the runtime
// probe of that name, so a crashed session whose bead still says active reads
// stopped.
type agentSessionResolver struct {
	cityName     string
	sessTmpl     string
	readSnapshot func() statusSessionSnapshot
	snapshot     *statusSessionSnapshot
}

// newAgentSessionResolver returns a resolver for cfg's agents that reads the
// session snapshot under ctx.
func (s *Server) newAgentSessionResolver(ctx context.Context, cfg *config.City, cityName string) *agentSessionResolver {
	return &agentSessionResolver{
		cityName: cityName,
		sessTmpl: cfg.Workspace.SessionTemplate,
		readSnapshot: func() statusSessionSnapshot {
			snapshot, _, _ := s.citySessionSnapshot(ctx, cfg)
			return snapshot
		},
	}
}

// resolve returns the runtime session name row qualifiedName of agent a runs
// under, and live's verdict on it. live is asked about the canonical name
// first and, only when that misses and the snapshot names another session,
// about that one, so its last call is always on the name returned.
func (r *agentSessionResolver) resolve(a config.Agent, qualifiedName string, live func(sessionName string) bool) (string, bool) {
	canonical := agentSessionName(r.cityName, qualifiedName, r.sessTmpl)
	if live(canonical) {
		return canonical, true
	}
	if qualifiedName == a.QualifiedName() && hasSeveralSeats(a) {
		// The pool's own name is none of its seats, so no seat's session is
		// this row's, however many of them the snapshot holds.
		return canonical, false
	}
	name := statusRuntimeSessionName(r.cityName, r.sessTmpl, qualifiedName, a.QualifiedName(), statusListsTemplateBeads(a), r.sessions())
	if name == canonical {
		return canonical, false
	}
	return name, live(name)
}

// partialErrors reports why the session snapshot a miss needed was
// incomplete. A row it could not resolve kept its canonical name, so its
// liveness is unknown, not stopped.
func (r *agentSessionResolver) partialErrors() []string {
	if r.snapshot == nil {
		return nil
	}
	return r.snapshot.partialErrors
}

func (r *agentSessionResolver) sessions() statusSessionSnapshot {
	if r.snapshot == nil {
		snapshot := r.readSnapshot()
		r.snapshot = &snapshot
	}
	return *r.snapshot
}

// hasSeveralSeats reports whether a's sessions can occupy more than one seat:
// a multi-session agent whose max_active_sessions is unset, unlimited or above
// one. A single seat, a namepool seat included, is the row named after its
// agent, so the join may resolve that row.
func hasSeveralSeats(a config.Agent) bool {
	if !isMultiSessionAgent(a) {
		return false
	}
	maxSess := a.EffectiveMaxActiveSessions()
	return maxSess == nil || *maxSess < 0 || *maxSess > 1
}
