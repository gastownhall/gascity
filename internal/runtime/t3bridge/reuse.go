package t3bridge

type ReuseDecision string

const (
	ReuseDecisionReuse    ReuseDecision = "reuse"
	ReuseDecisionRecreate ReuseDecision = "recreate"
)

type ReuseCheck struct {
	Desired       StartupEnvelope
	Stored        *StartupEnvelope
	ThreadActive  bool
	ProjectActive bool
}

type ReuseResult struct {
	Decision ReuseDecision
	Reason   string
}

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
	case stored.Runtime.Provider != input.Desired.Runtime.Provider:
		return ReuseResult{Decision: ReuseDecisionRecreate, Reason: "provider-mismatch"}
	case stored.Runtime.Model != input.Desired.Runtime.Model:
		return ReuseResult{Decision: ReuseDecisionRecreate, Reason: "model-mismatch"}
	case stored.Runtime.WorkDir != input.Desired.Runtime.WorkDir:
		return ReuseResult{Decision: ReuseDecisionRecreate, Reason: "workdir-mismatch"}
	case stored.GC.Agent != input.Desired.GC.Agent:
		return ReuseResult{Decision: ReuseDecisionRecreate, Reason: "agent-mismatch"}
	case stored.GC.Template != input.Desired.GC.Template:
		return ReuseResult{Decision: ReuseDecisionRecreate, Reason: "template-mismatch"}
	default:
		return ReuseResult{Decision: ReuseDecisionReuse, Reason: "match"}
	}
}
