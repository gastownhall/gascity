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
	// byStored and byName hold occupancy's indexes by stored template and
	// by every name the fresh-slot claim can count a row under
	// (indexOccupancy, A3).
	byStored, byName map[string][]int
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
	// freshPoolOccupancyInfos, whose only caller is that claim. held is
	// the slots the claim counts for its rows (A3).
	occupancy []session.Info
	held      map[int]bool
	built     bool
	// templates memoizes the slot helpers' stored-template match for the
	// agent (A3).
	templates slotTemplateMemo
	// reusable are the rows reusablePoolSessionInfo accepts for the agent's
	// own template, sorted as legacy sorts them, less the rows used so far:
	// used is its last filter, and grows only, over the view's requests
	// (realizePools). Callers share it, so it is replaced, never edited (A3).
	reusable      []session.Info
	reusableBuilt bool
}

func newPassIndex() *passIndex {
	return &passIndex{agents: make(map[string]*config.Agent)}
}

// slotTemplateMemo memoizes the slot helpers' stored-template match,
// storedTemplateMatchesPoolTemplate, for one pool template over one config,
// by the row's stored template. Each answer resolves through
// findAgentByTemplate, a scan of every agent, and the fresh-slot occupancy
// asks it for every row of every pool: O(T²·N) per pass without the memo.
// Nil, or another pool template, computes the answer, as legacy does:
// legacy never builds one (TestSlotTemplateMemoOracle).
type slotTemplateMemo struct {
	template string
	matches  map[string]bool
}

func (m *slotTemplateMemo) storedTemplateMatchesPoolTemplate(stored, template string, cfg *config.City) bool {
	if m == nil || template != m.template {
		return storedTemplateMatchesPoolTemplate(stored, template, cfg)
	}
	match, ok := m.matches[stored]
	if !ok {
		match = storedTemplateMatchesPoolTemplate(stored, template, cfg)
		m.matches[stored] = match
	}
	return match
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
	x.indexOccupancy()
}

// indexOccupancy indexes the occupancy by stored template and by each name
// a slot or canonical match reads (existingPoolSlotWithConfigInfo,
// infoIdentifiesAsCanonical): the exact agent name, alias and title, each
// "agent:" label's name, and each prefix of the slot helpers' agent name or
// of the alias that ends before a "-" (resolvePoolSlot's "<template>-<n>").
func (x *passIndex) indexOccupancy() {
	x.byStored, x.byName = make(map[string][]int), make(map[string][]int)
	for i := range x.occupancy {
		info := &x.occupancy[i]
		stored := storedTemplateRef(info)
		x.byStored[stored] = append(x.byStored[stored], i)
		keys := []string{strings.TrimSpace(info.AgentName), strings.TrimSpace(info.Alias), strings.TrimSpace(info.Title)}
		for _, label := range info.Labels {
			if name, ok := strings.CutPrefix(label, "agent:"); ok {
				keys = append(keys, name)
			}
		}
		for _, name := range []string{strings.TrimSpace(sessionBeadAgentNameInfo(*info)), keys[1]} {
			for k := range len(name) {
				if name[k] == '-' {
					keys = append(keys, name[:k])
				}
			}
		}
		slices.Sort(keys)
		for _, key := range slices.Compact(keys) {
			x.byName[key] = append(x.byName[key], i)
		}
	}
}

// occupancyCandidates is, in order, every occupancy row the fresh-slot
// claim can count for the memo's agent. A row counts as the agent's
// canonical holder, which names it exactly, or by a slot: its stored
// template matches, or else its agent name or alias resolves to one, which
// takes the agent's qualified or binding-qualified name before a "-" or,
// for a namepool agent, a themed name, which any row may carry.
func (m *poolRealizeMemo) occupancyCandidates() []int {
	x := m.index
	if len(m.agent.NamepoolNames) > 0 {
		all := make([]int, len(x.occupancy))
		for i := range all {
			all[i] = i
		}
		return all
	}
	template := m.agent.QualifiedName()
	rows := slices.Clone(x.byName[template])
	if trimmed := strings.TrimSpace(template); trimmed != template {
		rows = append(rows, x.byName[trimmed]...)
	}
	if m.agent.BindingName != "" {
		rows = append(rows, x.byName[m.agent.BindingQualifiedName()]...)
	}
	for stored, matching := range x.byStored {
		if m.templates.storedTemplateMatchesPoolTemplate(stored, template, m.cfg) {
			rows = append(rows, matching...)
		}
	}
	slices.Sort(rows)
	return slices.Compact(rows)
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
	view.realizeMemo = &poolRealizeMemo{
		index: x, cfg: p.cfg, agent: cfgAgent, rows: infos, all: x.occupancy,
		templates: slotTemplateMemo{template: cfgAgent.QualifiedName(), matches: make(map[string]bool)},
	}
	return &view
}

// freshOccupancy is claimFreshPoolSlotInfo's occupancy for the memo's
// agent: the rows it can count, a canonical holder or a numbered slot of
// the agent, in the full occupancy's order. The rest it skips anyway.
func (m *poolRealizeMemo) freshOccupancy() []session.Info {
	m.index.served++
	m.buildOccupancy()
	return m.occupancy
}

func (m *poolRealizeMemo) buildOccupancy() {
	if m.built {
		return
	}
	m.built = true
	m.held = make(map[int]bool)
	canonical := m.agent.QualifiedName()
	for _, i := range m.occupancyCandidates() {
		info := &m.all[i]
		slot := existingPoolSlotWithTemplates(m.cfg, &m.templates, m.agent, info)
		if slot > 0 || infoRefIdentifiesAsCanonical(info, canonical) {
			m.occupancy = append(m.occupancy, *info)
			// A failed-create row holds its name, not its slot.
			if slot > 0 && !isFailedCreateSessionInfo(*info) {
				m.held[slot] = true
			}
		}
	}
}

// freshSlots is the set of slots claimFreshPoolSlotInfo counts for cfgAgent
// over the memo's occupancy, or nil when it must count them: without a memo
// (legacy) or for another agent.
func (m *poolRealizeMemo) freshSlots(cfgAgent *config.Agent) map[int]bool {
	if m == nil || cfgAgent != m.agent {
		return nil
	}
	m.buildOccupancy()
	return m.held
}

// reusablePoolSessionInfos is legacy's over the template's rows instead of
// a copy of every open row per request.
func (m *poolRealizeMemo) reusablePoolSessionInfos(bp *agentBuildParams, cfgAgent *config.Agent, template string, used map[string]bool) []session.Info {
	m.index.served++
	if cfgAgent != m.agent || template != m.agent.QualifiedName() {
		return m.scanReusable(bp, cfgAgent, template, used)
	}
	if !m.reusableBuilt {
		m.reusable, m.reusableBuilt = m.scanReusable(bp, cfgAgent, template, nil), true
	}
	// Requests use rows in list order, so the used rows are usually a
	// prefix: drop it without a copy.
	k := 0
	for k < len(m.reusable) && used[m.reusable[k].ID] {
		k++
	}
	m.reusable = m.reusable[k:]
	for i := range m.reusable {
		if used[m.reusable[i].ID] {
			kept := make([]session.Info, 0, len(m.reusable)-1)
			for j := range m.reusable {
				if !used[m.reusable[j].ID] {
					kept = append(kept, m.reusable[j])
				}
			}
			m.reusable = kept
			break
		}
	}
	return slices.Clip(m.reusable)
}

func (m *poolRealizeMemo) scanReusable(bp *agentBuildParams, cfgAgent *config.Agent, template string, used map[string]bool) []session.Info {
	candidates := []session.Info{}
	for i := range m.rows {
		if reusablePoolSessionInfo(bp, cfgAgent, template, m.rows[i], used) {
			candidates = append(candidates, m.rows[i])
		}
	}
	sortSessionInfosByCreatedAtThenID(candidates)
	return candidates
}
