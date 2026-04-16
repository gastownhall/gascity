package t3bridge

// ReuseDecision describes what to do with an existing T3 thread when a new
// startup envelope arrives.
type ReuseDecision string

const (
	ReuseDecisionReuse    ReuseDecision = "reuse"
	ReuseDecisionRecreate ReuseDecision = "recreate"
	ReuseDecisionRebind   ReuseDecision = "rebind"
)

// ReuseCheck holds the inputs for DecideThreadReuse.
type ReuseCheck struct {
	Desired       StartupEnvelope
	Stored        *StartupEnvelope
	ThreadActive  bool
	ProjectActive bool
}

// ReuseResult holds the decision and reason.
type ReuseResult struct {
	Decision ReuseDecision
	Reason   string
}

// DecideThreadReuse decides whether to reuse, rebind, or recreate a T3 thread.
func DecideThreadReuse(input ReuseCheck) ReuseResult {
	if !input.Desired.Resume.AllowThreadReuse {
		return ReuseResult{Decision: ReuseDecisionRecreate, Reason: "reuse-disabled"}
	}
	if !input.ThreadActive {
		return ReuseResult{Decision: ReuseDecisionRecreate, Reason: "thread-inactive"}
	}
	if !input.ProjectActive {
		return ReuseResult{Decision: ReuseDecisionRecreate, Reason: "project-inactive"}
	}
	if input.Stored == nil {
		return ReuseResult{Decision: ReuseDecisionRecreate, Reason: "no-stored-envelope"}
	}
	stored := input.Stored

	switch {
	case stored.Runtime.WorkDir != input.Desired.Runtime.WorkDir:
		return ReuseResult{Decision: ReuseDecisionRecreate, Reason: "workdir-mismatch"}
	case stored.GC.Agent != input.Desired.GC.Agent:
		return ReuseResult{Decision: ReuseDecisionRecreate, Reason: "agent-mismatch"}
	case stored.GC.Template != input.Desired.GC.Template:
		return ReuseResult{Decision: ReuseDecisionRecreate, Reason: "template-mismatch"}
	}

	providerChanged := stored.Runtime.Provider != input.Desired.Runtime.Provider
	modelChanged := stored.Runtime.Model != input.Desired.Runtime.Model

	if !providerChanged && !modelChanged {
		return ReuseResult{Decision: ReuseDecisionReuse, Reason: "match"}
	}

	if input.Desired.Resume.AllowRuntimeRebind {
		if providerChanged {
			return ReuseResult{Decision: ReuseDecisionRebind, Reason: "provider-rebind"}
		}
		return ReuseResult{Decision: ReuseDecisionRebind, Reason: "model-rebind"}
	}

	if providerChanged {
		return ReuseResult{Decision: ReuseDecisionRecreate, Reason: "provider-mismatch"}
	}
	return ReuseResult{Decision: ReuseDecisionRecreate, Reason: "model-mismatch"}
}
