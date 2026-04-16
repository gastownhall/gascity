package t3bridge

import "encoding/json"

type AgentKind string

const (
	AgentKindNamed AgentKind = "named"
	AgentKindPool  AgentKind = "pool"
)

type GCSection struct {
	CityPath    string `json:"cityPath"`
	CityName    string `json:"cityName"`
	RigName     string `json:"rigName,omitempty"`
	RigPath     string `json:"rigPath,omitempty"`
	Agent       string `json:"agent"`
	Template    string `json:"template"`
	SessionID   string `json:"sessionId,omitempty"`
	SessionName string `json:"sessionName"`
}

type RuntimeSection struct {
	Provider         string `json:"provider"`
	Model            string `json:"model,omitempty"`
	SessionTransport string `json:"sessionTransport"`
	RuntimeMode      string `json:"runtimeMode"`
	InteractionMode  string `json:"interactionMode"`
	WorkDir          string `json:"workDir"`
	Branch           string `json:"branch,omitempty"`
	NewBranch        string `json:"newBranch,omitempty"`
	Command          string `json:"command,omitempty"`
}

type WorktreeSection struct {
	Cwd          string `json:"cwd"`
	WorktreePath string `json:"worktreePath"`
	Branch       string `json:"branch"`
}

type StartupSection struct {
	PromptTemplate string `json:"promptTemplate,omitempty"`
	StartupPrompt  string `json:"startupPrompt"`
	InitialNudge   string `json:"initialNudge,omitempty"`
}

type AssignmentSection struct {
	BeadID            string `json:"beadId,omitempty"`
	BeadTitle         string `json:"beadTitle,omitempty"`
	ConvoyID          string `json:"convoyId,omitempty"`
	ConvoyTitle       string `json:"convoyTitle,omitempty"`
	ConvoyStatus      string `json:"convoyStatus,omitempty"`
	ConvoyClosedCount string `json:"convoyClosedCount,omitempty"`
	ConvoyTotalCount  string `json:"convoyTotalCount,omitempty"`
	MoleculeID        string `json:"moleculeId,omitempty"`
	Formula           string `json:"formula,omitempty"`
}

type ContextSection struct {
	GCEnv map[string]string `json:"gcEnv,omitempty"`
}

type ResumeSection struct {
	Policy                 string `json:"policy"`
	AllowThreadReuse       bool   `json:"allowThreadReuse"`
	AllowRuntimeRebind     bool   `json:"allowRuntimeRebind,omitempty"`
	RequiredThreadProvider string `json:"requiredThreadProvider"`
	RequiredThreadModel    string `json:"requiredThreadModel,omitempty"`
}

type StartupEnvelope struct {
	Version    int               `json:"version"`
	GC         GCSection         `json:"gc"`
	Runtime    RuntimeSection    `json:"runtime"`
	Startup    StartupSection    `json:"startup"`
	Assignment AssignmentSection `json:"assignment,omitempty"`
	Context    ContextSection    `json:"context,omitempty"`
	Resume     ResumeSection     `json:"resume"`
	Worktree   *WorktreeSection  `json:"worktree,omitempty"`
}

type Intent struct {
	AgentKind          AgentKind
	WakeMode           string
	GC                 GCSection
	Runtime            RuntimeSection
	Startup            StartupSection
	Assignment         AssignmentSection
	Context            ContextSection
	ResumePolicy       string
	AllowRuntimeRebind bool
	RequiredProvider   string
	RequiredModel      string
}

func allowThreadReuse(kind AgentKind, wakeMode string) bool {
	if kind != AgentKindNamed {
		return false
	}
	// Named sessions should keep one durable T3 thread even if the runtime
	// prefers a fresh process on wake.
	_ = wakeMode
	return true
}

func BuildStartupEnvelope(intent Intent) (json.RawMessage, error) {
	policy := intent.ResumePolicy
	if policy == "" {
		policy = "match-or-recreate"
	}
	envelope := StartupEnvelope{
		Version:    1,
		GC:         intent.GC,
		Runtime:    intent.Runtime,
		Startup:    intent.Startup,
		Assignment: intent.Assignment,
		Context:    intent.Context,
		Resume: ResumeSection{
			Policy:                 policy,
			AllowThreadReuse:       allowThreadReuse(intent.AgentKind, intent.WakeMode),
			AllowRuntimeRebind:     intent.AllowRuntimeRebind,
			RequiredThreadProvider: intent.RequiredProvider,
			RequiredThreadModel:    intent.RequiredModel,
		},
	}
	data, err := json.Marshal(envelope)
	if err != nil {
		return nil, err
	}
	return json.RawMessage(data), nil
}
