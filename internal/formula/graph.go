package formula

import "encoding/json"

// ApplyGraphControls applies graph control metadata to steps in the formula.
func ApplyGraphControls(f *Formula) {
	if f == nil || f.Version < 2 {
		return
	}
	applyGraphControls(f, true)
}

// ApplyFragmentGraphControls applies graph control metadata to fragment steps in the formula.
func ApplyFragmentGraphControls(f *Formula) {
	if f == nil || f.Version < 2 {
		return
	}
	applyGraphControls(f, false)
}

func applyGraphControls(f *Formula, includeWorkflowFinalize bool) {
	scopeControlByStep := make(map[string]string)
	controls := make([]*Step, 0)
	allSteps := collectGraphSteps(f.Steps)

	for _, step := range allSteps {
		if step == nil || step.OnComplete == nil {
			continue
		}
		if step.Metadata == nil {
			step.Metadata = make(map[string]string)
		}
		step.Metadata["gc.output_json_required"] = "true"
		controlMetadata := map[string]string{
			"gc.kind":        "fanout",
			"gc.control_for": step.ID,
			"gc.for_each":    step.OnComplete.ForEach,
			"gc.bond":        step.OnComplete.Bond,
			"gc.fanout_mode": "parallel",
		}
		if step.OnComplete.Sequential {
			controlMetadata["gc.fanout_mode"] = "sequential"
		}
		if len(step.OnComplete.Vars) > 0 {
			if data, err := json.Marshal(step.OnComplete.Vars); err == nil {
				controlMetadata["gc.bond_vars"] = string(data)
			}
		}
		for _, key := range []string{"gc.scope_ref", "gc.scope_role", "gc.on_fail", "gc.step_id", "gc.ralph_step_id", "gc.attempt"} {
			if value := step.Metadata[key]; value != "" {
				controlMetadata[key] = value
			}
		}
		controls = append(controls, &Step{
			ID:       step.ID + "-fanout",
			Title:    "Expand fanout for " + step.Title,
			Type:     "task",
			Needs:    []string{step.ID},
			Metadata: controlMetadata,
		})
	}

	for _, step := range allSteps {
		if !needsScopeCheck(step) {
			continue
		}
		controlID := step.ID + "-scope-check"
		scopeControlByStep[step.ID] = controlID
		controlMetadata := map[string]string{
			"gc.kind":        "scope-check",
			"gc.scope_ref":   step.Metadata["gc.scope_ref"],
			"gc.scope_role":  "control",
			"gc.control_for": step.ID,
		}
		for _, key := range []string{"gc.step_id", "gc.ralph_step_id", "gc.attempt", "gc.on_fail"} {
			if value := step.Metadata[key]; value != "" {
				controlMetadata[key] = value
			}
		}
		controls = append(controls, &Step{
			ID:       controlID,
			Title:    "Finalize scope for " + step.Title,
			Type:     "task",
			Needs:    []string{step.ID},
			Metadata: controlMetadata,
		})
	}

	rewriteGraphStepRefs(f.Steps, scopeControlByStep)

	f.Steps = append(f.Steps, controls...)

	if !includeWorkflowFinalize {
		return
	}

	sinks := graphSinkStepIDs(f.Steps)
	if len(sinks) == 0 {
		f.Steps = sortGraphSteps(f.Steps)
		return
	}
	f.Steps = append(f.Steps, &Step{
		ID:    "workflow-finalize",
		Title: "Finalize workflow",
		Type:  "task",
		Needs: sinks,
		Metadata: map[string]string{
			"gc.kind": "workflow-finalize",
		},
	})
	f.Steps = sortGraphSteps(f.Steps)
}

func needsScopeCheck(step *Step) bool {
	if step == nil {
		return false
	}
	if step.Metadata["gc.scope_ref"] == "" {
		return false
	}
	if step.Metadata["gc.scope_role"] == "teardown" {
		return false
	}
	switch step.Metadata["gc.kind"] {
	case "scope", "scope-check", "workflow-finalize", "fanout", "check", "spec":
		return false
	default:
		return true
	}
}

func rewriteGraphRefs(in []string, replacements map[string]string) []string {
	if len(in) == 0 || len(replacements) == 0 {
		return in
	}
	out := make([]string, len(in))
	for i, id := range in {
		if replacement, ok := replacements[id]; ok {
			out[i] = replacement
			continue
		}
		out[i] = id
	}
	return out
}

func graphSinkStepIDs(steps []*Step) []string {
	allSteps := collectGraphSteps(steps)
	if len(allSteps) == 0 {
		return nil
	}
	referenced := make(map[string]struct{}, len(allSteps))
	for _, step := range allSteps {
		for _, id := range step.DependsOn {
			referenced[id] = struct{}{}
		}
		for _, id := range step.Needs {
			referenced[id] = struct{}{}
		}
	}

	sinks := make([]string, 0)
	for _, step := range allSteps {
		if step == nil {
			continue
		}
		switch step.Metadata["gc.kind"] {
		case "workflow-finalize", "spec":
			continue
		case "scope":
			// Scope bodies are terminal latches even when referenced by teardown
			// steps. Workflow finalization must see their pass/fail outcome.
			sinks = append(sinks, step.ID)
			continue
		}
		if _, ok := referenced[step.ID]; ok {
			continue
		}
		sinks = append(sinks, step.ID)
	}
	return sinks
}

func rewriteGraphStepRefs(steps []*Step, replacements map[string]string) {
	for _, step := range steps {
		if step == nil {
			continue
		}
		step.DependsOn = rewriteGraphRefs(step.DependsOn, replacements)
		step.Needs = rewriteGraphRefs(step.Needs, replacements)
		if len(step.Children) > 0 {
			rewriteGraphStepRefs(step.Children, replacements)
		}
	}
}

func collectGraphSteps(steps []*Step) []*Step {
	if len(steps) == 0 {
		return nil
	}
	var out []*Step
	var walk func([]*Step)
	walk = func(nodes []*Step) {
		for _, step := range nodes {
			if step == nil {
				continue
			}
			out = append(out, step)
			if len(step.Children) > 0 {
				walk(step.Children)
			}
		}
	}
	walk(steps)
	return out
}

func sortGraphSteps(steps []*Step) []*Step {
	if len(steps) <= 2 {
		return steps
	}

	root := steps[0]
	nonRoot := steps[1:]
	if len(nonRoot) <= 1 {
		return steps
	}

	indexByID := make(map[string]int, len(nonRoot))
	stepByID := make(map[string]*Step, len(nonRoot))
	indegree := make(map[string]int, len(nonRoot))
	adj := make(map[string][]string, len(nonRoot))

	for i, step := range nonRoot {
		if step == nil {
			continue
		}
		indexByID[step.ID] = i
		stepByID[step.ID] = step
		indegree[step.ID] = 0
	}

	for _, step := range nonRoot {
		if step == nil {
			continue
		}
		for _, depID := range append(append([]string{}, step.DependsOn...), step.Needs...) {
			if _, ok := indegree[depID]; !ok {
				continue
			}
			indegree[step.ID]++
			adj[depID] = append(adj[depID], step.ID)
		}
	}

	insertReady := func(queue []string, id string) []string {
		insertAt := len(queue)
		for i, existing := range queue {
			if indexByID[id] < indexByID[existing] {
				insertAt = i
				break
			}
		}
		queue = append(queue, "")
		copy(queue[insertAt+1:], queue[insertAt:])
		queue[insertAt] = id
		return queue
	}

	ready := make([]string, 0, len(nonRoot))
	for _, step := range nonRoot {
		if step == nil {
			continue
		}
		if indegree[step.ID] == 0 {
			ready = insertReady(ready, step.ID)
		}
	}

	ordered := make([]*Step, 0, len(steps))
	ordered = append(ordered, root)
	visited := make(map[string]bool, len(nonRoot))

	for len(ready) > 0 {
		id := ready[0]
		ready = ready[1:]
		if visited[id] {
			continue
		}
		visited[id] = true
		ordered = append(ordered, stepByID[id])
		for _, next := range adj[id] {
			indegree[next]--
			if indegree[next] == 0 {
				ready = insertReady(ready, next)
			}
		}
	}

	if len(ordered) == len(steps) {
		return ordered
	}

	// Fallback for unexpected cycles or malformed references: preserve any
	// remaining steps in their original order.
	for _, step := range nonRoot {
		if step == nil || visited[step.ID] {
			continue
		}
		ordered = append(ordered, step)
	}
	return ordered
}
