package main

import (
	"slices"
	"strings"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/session"
)

// The decide pass's realization index (A2). Legacy's pool realization
// rescans every open row, and for each row every assigned work bead, on
// every request, and copies the whole occupancy on every fresh-slot claim:
// quadratic at city scale (architecture §1.8). The pass therefore hands
// legacy's planner a view of its build params narrowed to one template, and
// the fresh-slot claim an occupancy limited to the agent, computed once.
// Legacy never builds one: the pass without an index runs today's path
// unchanged, and is the oracle for the indexed one
// (TestRealizationIndexOracle).

// passIndex is one pass's lookups, read only once built.
type passIndex struct {
	// agents memoizes findAgentByTemplate by template.
	agents map[string]*config.Agent
	// rowsByTemplate holds the decidable rows' indexes by resolved template
	// (the Entry's, resolvedSessionTemplateInfo's).
	rowsByTemplate map[string][]int
	// workByAssignee and workByID hold the actionable (open or in_progress)
	// pool work's indexes by trimmed assignee and by bead ID.
	workByAssignee map[string][]int
	workByID       map[string][]int
	// occupancy is freshPoolOccupancyInfos over the pass's full params,
	// taken once realization starts: nothing changes them after.
	occupancy []session.Info
	// views counts the realization views handed out, and served the legacy
	// lookups their memos answered: proof the index is in use.
	views, served int
}

// poolRealizeMemo is one pool agent's realization view's memo, carried as
// agentBuildParams.realizeMemo.
type poolRealizeMemo struct {
	index *passIndex
	cfg   *config.City
	agent *config.Agent
	// rows are the open decidable rows of the agent's template, in snapshot
	// order: the only rows reusablePoolSessionInfo accepts.
	rows []session.Info
	// all is the pass's fresh-slot occupancy (passIndex.occupancy).
	all []session.Info
	// occupancy is all limited to the rows claimFreshPoolSlotInfo can count
	// for agent, in order; built on the first claim. It answers
	// freshPoolOccupancyInfos, whose only caller is that claim.
	occupancy []session.Info
	built     bool
}

func newPassIndex() *passIndex {
	return &passIndex{agents: make(map[string]*config.Agent)}
}

// agentByTemplate is findAgentByTemplate, through the index's memo when the
// pass has one.
func (p *decidePass) agentByTemplate(template string) *config.Agent {
	if p.index == nil {
		return findAgentByTemplate(p.cfg, template)
	}
	agent, ok := p.index.agents[template]
	if !ok {
		agent = findAgentByTemplate(p.cfg, template)
		p.index.agents[template] = agent
	}
	return agent
}

// indexRealization indexes the decidable rows and the pool work, once the
// plan params are complete (named reservations included).
func (p *decidePass) indexRealization() {
	x := p.index
	if x == nil {
		return
	}
	x.rowsByTemplate = make(map[string][]int)
	for i, info := range p.decidable {
		template := p.snap.Entries[p.byID[info.ID]].Template
		x.rowsByTemplate[template] = append(x.rowsByTemplate[template], i)
	}
	x.workByAssignee = make(map[string][]int)
	x.workByID = make(map[string][]int)
	for i, wb := range p.poolWork {
		if wb.Status != "open" && wb.Status != "in_progress" {
			continue
		}
		x.workByID[wb.ID] = append(x.workByID[wb.ID], i)
		if assignee := strings.TrimSpace(wb.Assignee); assignee != "" {
			x.workByAssignee[assignee] = append(x.workByAssignee[assignee], i)
		}
	}
	x.occupancy = freshPoolOccupancyInfos(p.bp)
}

// realizeParams is the build params legacy's planner realizes cfgAgent's
// requests with. Without an index it is the pass's own. With one it is a
// copy that keeps the city's session snapshot, whose assigned work holds
// only what the template's rows' identities or the requests' work IDs can
// match (the only beads the reuse and resume checks read), and whose memo
// answers the reuse scan over the template's rows and the fresh-slot
// occupancy limited to the agent.
func (p *decidePass) realizeParams(cfgAgent *config.Agent, requests []SessionRequest) *agentBuildParams {
	x := p.index
	if x == nil {
		return p.bp
	}
	x.views++
	rows := x.rowsByTemplate[cfgAgent.QualifiedName()]
	infos := make([]session.Info, 0, len(rows))
	var work []int
	for _, i := range rows {
		info := p.decidable[i]
		if !info.Closed {
			infos = append(infos, info)
		}
		// The two identity sets legacy matches work against, exactly:
		// sessionBeadHasAssignedWorkInfo's and the one_shot reuse guard's
		// (sessionBeadHasAssignedWorkByAnyIdentityInfo).
		for _, id := range sessionAssignmentIdentifiersForConfigInfo(info, p.cfg) {
			work = append(work, x.workByAssignee[id]...)
		}
		for _, id := range sessionBeadAssigneeIdentitiesInfo(info) {
			work = append(work, x.workByAssignee[strings.TrimSpace(id)]...)
		}
	}
	for _, r := range requests {
		work = append(work, x.workByID[strings.TrimSpace(r.WorkBeadID)]...)
	}
	slices.Sort(work)
	work = slices.Compact(work)
	assigned := make([]beads.Bead, 0, len(work))
	for _, i := range work {
		assigned = append(assigned, p.poolWork[i])
	}
	view := *p.bp
	view.assignedWorkBeads = assigned
	view.realizeMemo = &poolRealizeMemo{index: x, cfg: p.cfg, agent: cfgAgent, rows: infos, all: x.occupancy}
	return &view
}

// freshOccupancy is claimFreshPoolSlotInfo's occupancy for the memo's
// agent: the rows it can count, a canonical holder or a numbered slot of
// the agent, in the full occupancy's order. The rest it skips anyway.
func (m *poolRealizeMemo) freshOccupancy() []session.Info {
	m.index.served++
	if m.built {
		return m.occupancy
	}
	m.built = true
	canonical := m.agent.QualifiedName()
	for _, info := range m.all {
		if infoIdentifiesAsCanonical(info, canonical) || existingPoolSlotWithConfigInfo(m.cfg, m.agent, info) > 0 {
			m.occupancy = append(m.occupancy, info)
		}
	}
	return m.occupancy
}

// reusablePoolSessionInfos is legacy's over the template's rows instead of
// a copy of every open row per request.
func (m *poolRealizeMemo) reusablePoolSessionInfos(bp *agentBuildParams, cfgAgent *config.Agent, template string, used map[string]bool) []session.Info {
	m.index.served++
	candidates := []session.Info{}
	for i := range m.rows {
		if reusablePoolSessionInfo(bp, cfgAgent, template, m.rows[i], used) {
			candidates = append(candidates, m.rows[i])
		}
	}
	sortSessionInfosByCreatedAtThenID(candidates)
	return candidates
}
