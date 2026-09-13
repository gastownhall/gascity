package runproj

import "fmt"

// The detail DTO unions below mirror the summary unions in marshal.go: each
// carries every arm's fields and a custom MarshalJSON that emits exactly the
// active arm's keys, in the TS object-literal order. Key order is load-bearing
// for byte-for-byte golden parity.

// nodeStatusCounts is a per-node-status tally that preserves first-seen status
// order, matching the TS Partial<Record<RunNodeStatus, number>> insertion order
// (a Go map would sort keys and break parity).
type nodeStatusCounts struct {
	keys   []string
	counts map[string]int
}

func (c *nodeStatusCounts) inc(status string) {
	if c.counts == nil {
		c.counts = map[string]int{}
	}
	if _, ok := c.counts[status]; !ok {
		c.keys = append(c.keys, status)
	}
	c.counts[status]++
}

// MarshalJSON renders the counts in first-seen order.
func (c nodeStatusCounts) MarshalJSON() ([]byte, error) {
	pairs := make([]kv, 0, len(c.keys))
	for _, k := range c.keys {
		pairs = append(pairs, kv{k, c.counts[k]})
	}
	return marshalObject(pairs)
}

// MarshalJSON renders the active formula arm. TS: {kind:'known', name, source} |
// {kind:'unavailable', reason}.
func (f RunFormula) MarshalJSON() ([]byte, error) {
	switch f.Kind {
	case "known":
		return marshalObject([]kv{{"kind", "known"}, {"name", f.Name}, {"source", f.Source}})
	case "unavailable":
		return marshalObject([]kv{{"kind", "unavailable"}, {"reason", f.Reason}})
	default:
		return nil, fmt.Errorf("runproj: invalid RunFormula kind %q", f.Kind)
	}
}

// MarshalJSON renders the active formula-detail arm. TS: {kind:'available', name,
// target} | {kind:'unavailable', reason} (+ name / +name,target,failure variants).
func (s RunFormulaDetailState) MarshalJSON() ([]byte, error) {
	switch {
	case s.Kind == "available":
		return marshalObject([]kv{{"kind", "available"}, {"name", s.Name}, {"target", s.Target}})
	case s.Kind == "unavailable" && s.Reason == "missing_formula_metadata":
		return marshalObject([]kv{{"kind", "unavailable"}, {"reason", s.Reason}})
	case s.Kind == "unavailable" && s.Reason == "missing_run_target":
		return marshalObject([]kv{{"kind", "unavailable"}, {"reason", s.Reason}, {"name", s.Name}})
	case s.Kind == "unavailable" && s.Reason == "fetch_failed":
		return marshalObject([]kv{
			{"kind", "unavailable"},
			{"reason", s.Reason},
			{"name", s.Name},
			{"target", s.Target},
			{"failure", s.Failure},
		})
	default:
		return nil, fmt.Errorf("runproj: invalid RunFormulaDetailState kind=%q reason=%q", s.Kind, s.Reason)
	}
}

// MarshalJSON renders the active execution-path arm. TS: {kind:'known', path} |
// {kind:'unavailable', reason}.
func (p RunExecutionPath) MarshalJSON() ([]byte, error) {
	switch p.Kind {
	case "known":
		return marshalObject([]kv{{"kind", "known"}, {"path", p.Path}})
	case "unavailable":
		return marshalObject([]kv{{"kind", "unavailable"}, {"reason", p.Reason}})
	default:
		return nil, fmt.Errorf("runproj: invalid RunExecutionPath kind %q", p.Kind)
	}
}

// MarshalJSON renders the active snapshot-sequence arm. TS: {kind:'known', seq} |
// {kind:'unavailable', reason}.
func (s RunSnapshotSequence) MarshalJSON() ([]byte, error) {
	switch s.Kind {
	case "known":
		return marshalObject([]kv{{"kind", "known"}, {"seq", s.Seq}})
	case "unavailable":
		return marshalObject([]kv{{"kind", "unavailable"}, {"reason", s.Reason}})
	default:
		return nil, fmt.Errorf("runproj: invalid RunSnapshotSequence kind %q", s.Kind)
	}
}

// MarshalJSON renders the active completeness arm. TS: {kind:'complete'} |
// {kind:'partial', reasons}.
func (c FormulaRunCompleteness) MarshalJSON() ([]byte, error) {
	switch c.Kind {
	case "complete":
		return marshalObject([]kv{{"kind", "complete"}})
	case "partial":
		reasons := c.Reasons
		if reasons == nil {
			reasons = []string{}
		}
		return marshalObject([]kv{{"kind", "partial"}, {"reasons", reasons}})
	default:
		return nil, fmt.Errorf("runproj: invalid FormulaRunCompleteness kind %q", c.Kind)
	}
}

// MarshalJSON renders the active node-scope arm. TS: {kind:'run'} |
// {kind:'scoped', ref}.
func (s RunNodeScope) MarshalJSON() ([]byte, error) {
	switch s.Kind {
	case "run":
		return marshalObject([]kv{{"kind", "run"}})
	case "scoped":
		return marshalObject([]kv{{"kind", "scoped"}, {"ref", s.Ref}})
	default:
		return nil, fmt.Errorf("runproj: invalid RunNodeScope kind %q", s.Kind)
	}
}

// MarshalJSON renders the active iteration arm. TS: {kind:'base'} |
// {kind:'loop', value}.
func (i RunIteration) MarshalJSON() ([]byte, error) {
	switch i.Kind {
	case "base":
		return marshalObject([]kv{{"kind", "base"}})
	case "loop":
		return marshalObject([]kv{{"kind", "loop"}, {"value", i.Value}})
	default:
		return nil, fmt.Errorf("runproj: invalid RunIteration kind %q", i.Kind)
	}
}

// MarshalJSON renders the active attempt arm. TS: {kind:'untracked'} |
// {kind:'attempt', value}.
func (a RunAttempt) MarshalJSON() ([]byte, error) {
	switch a.Kind {
	case "untracked":
		return marshalObject([]kv{{"kind", "untracked"}})
	case "attempt":
		return marshalObject([]kv{{"kind", "attempt"}, {"value", a.Value}})
	default:
		return nil, fmt.Errorf("runproj: invalid RunAttempt kind %q", a.Kind)
	}
}

// MarshalJSON renders the active session-attachment arm. TS: {kind:'attached',
// link, streamable} | {kind:'none', reason}.
func (s RunSessionAttachment) MarshalJSON() ([]byte, error) {
	switch s.Kind {
	case "attached":
		return marshalObject([]kv{
			{"kind", "attached"},
			{"link", s.Link},
			{"streamable", s.Streamable},
		})
	case "none":
		return marshalObject([]kv{{"kind", "none"}, {"reason", s.Reason}})
	default:
		return nil, fmt.Errorf("runproj: invalid RunSessionAttachment kind %q", s.Kind)
	}
}

// MarshalJSON renders the active iteration-summary arm. TS: {kind:'single'} |
// {kind:'stacked', visibleIteration, iterationCount, control}.
func (s RunIterationSummary) MarshalJSON() ([]byte, error) {
	switch s.Kind {
	case "single":
		return marshalObject([]kv{{"kind", "single"}})
	case "stacked":
		return marshalObject([]kv{
			{"kind", "stacked"},
			{"visibleIteration", s.VisibleIteration},
			{"iterationCount", s.IterationCount},
			{"control", s.Control},
		})
	default:
		return nil, fmt.Errorf("runproj: invalid RunIterationSummary kind %q", s.Kind)
	}
}

// MarshalJSON renders the active iteration-control arm. TS: {kind:'known', id} |
// {kind:'unknown'}.
func (c RunIterationControl) MarshalJSON() ([]byte, error) {
	switch c.Kind {
	case "known":
		return marshalObject([]kv{{"kind", "known"}, {"id", c.ID}})
	case "unknown":
		return marshalObject([]kv{{"kind", "unknown"}})
	default:
		return nil, fmt.Errorf("runproj: invalid RunIterationControl kind %q", c.Kind)
	}
}

// MarshalJSON renders the active attempt-summary arm. TS: {kind:'none'} |
// {kind:'tracked', count, badge, active}.
func (s RunAttemptSummary) MarshalJSON() ([]byte, error) {
	switch s.Kind {
	case "none":
		return marshalObject([]kv{{"kind", "none"}})
	case "tracked":
		return marshalObject([]kv{
			{"kind", "tracked"},
			{"count", s.Count},
			{"badge", s.Badge},
			{"active", s.Active},
		})
	default:
		return nil, fmt.Errorf("runproj: invalid RunAttemptSummary kind %q", s.Kind)
	}
}

// MarshalJSON renders the active attempt-badge arm. TS: {kind:'bounded', label} |
// {kind:'count-only'}.
func (b RunAttemptBadge) MarshalJSON() ([]byte, error) {
	switch b.Kind {
	case "bounded":
		return marshalObject([]kv{{"kind", "bounded"}, {"label", b.Label}})
	case "count-only":
		return marshalObject([]kv{{"kind", "count-only"}})
	default:
		return nil, fmt.Errorf("runproj: invalid RunAttemptBadge kind %q", b.Kind)
	}
}

// MarshalJSON renders the active attempt-active arm. TS: {kind:'running', value}
// | {kind:'idle'}.
func (a RunAttemptActive) MarshalJSON() ([]byte, error) {
	switch a.Kind {
	case "running":
		return marshalObject([]kv{{"kind", "running"}, {"value", a.Value}})
	case "idle":
		return marshalObject([]kv{{"kind", "idle"}})
	default:
		return nil, fmt.Errorf("runproj: invalid RunAttemptActive kind %q", a.Kind)
	}
}
