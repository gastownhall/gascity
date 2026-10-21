package main

import "sync"

// Stage-1 skill materialization re-runs on every config reload and supervisor
// pass. Skipping user-owned content at a sink path is the deliberate, standing
// outcome for that path (the ownership manifest never overwrites user
// content), so the same "skipped skill" line used to print again on every
// pass, about 2.6k lines/hour across a handful of cities. The skip behavior is
// unchanged; only the report is deduplicated. A skip prints when it first
// appears for an (agent, sink path) or its message changes, and a pass that no
// longer skips that path re-arms it, so a later conflict prints again.
// GC_DEBUG restores the line on every pass.

// skillSkipKey identifies one reported skip within a city.
type skillSkipKey struct {
	agent string
	path  string
}

// skillSkipLog holds the skips reported by the last completed pass, per city.
type skillSkipLog struct {
	mu     sync.Mutex
	byCity map[string]map[skillSkipKey]string
}

var stage1SkillSkipLog = &skillSkipLog{}

// skillSkipPass tracks one materialization pass for one city.
type skillSkipPass struct {
	log        *skillSkipLog
	cityPath   string
	prev       map[skillSkipKey]string
	current    map[skillSkipKey]string
	incomplete map[string]bool
}

// beginPass snapshots the skips the previous pass reported for cityPath.
func (l *skillSkipLog) beginPass(cityPath string) *skillSkipPass {
	l.mu.Lock()
	defer l.mu.Unlock()
	prev := make(map[skillSkipKey]string, len(l.byCity[cityPath]))
	for k, v := range l.byCity[cityPath] {
		prev[k] = v
	}
	return &skillSkipPass{
		log:        l,
		cityPath:   cityPath,
		prev:       prev,
		current:    make(map[skillSkipKey]string),
		incomplete: make(map[string]bool),
	}
}

// shouldReport records a skip seen in this pass and reports whether its line
// should print: the previous pass did not report it with the same message, or
// GC_DEBUG is on.
func (p *skillSkipPass) shouldReport(agent, path, message string) bool {
	key := skillSkipKey{agent: agent, path: path}
	p.current[key] = message
	prev, seen := p.prev[key]
	return !seen || prev != message || gcDebugEnabled()
}

// markIncomplete records that agent's materialization did not finish in this
// pass. Its previously reported skips carry over, so a transient failure does
// not make them print again on the next pass.
func (p *skillSkipPass) markIncomplete(agent string) {
	p.incomplete[agent] = true
}

// finish stores this pass's skips as the city's reported set.
func (p *skillSkipPass) finish() {
	for k, v := range p.prev {
		if p.incomplete[k.agent] {
			if _, ok := p.current[k]; !ok {
				p.current[k] = v
			}
		}
	}
	p.log.mu.Lock()
	defer p.log.mu.Unlock()
	if p.log.byCity == nil {
		p.log.byCity = make(map[string]map[skillSkipKey]string)
	}
	if len(p.current) == 0 {
		delete(p.log.byCity, p.cityPath)
		return
	}
	p.log.byCity[p.cityPath] = p.current
}
