package api

import (
	"fmt"
	"strings"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/session"
	workdirutil "github.com/gastownhall/gascity/internal/workdir"
)

type agentCreateContext struct {
	Agent        config.Agent
	Alias        string
	ExplicitName string
	Identity     string
	WorkDir      string
}

func agentSessionCreateMetadata(metadata map[string]string, identity string) map[string]string {
	if metadata == nil {
		metadata = make(map[string]string)
	}
	metadata["agent_name"] = identity
	metadata["session_origin"] = "ephemeral"
	return metadata
}

// demandOnlySingletonCreateRefusal returns the message refusing an agent
// session create for a demand-only singleton agent, or "" when the create may
// proceed. The reconciler treats such a bead as pool capacity and never starts
// it on request, so accepting the create would return 202 for a session that
// sits start-pending forever (#6858).
func demandOnlySingletonCreateRefusal(cfg *config.City, agentCfg config.Agent) string {
	if !session.IsDemandOnlySingletonTemplate(cfg, &agentCfg) {
		return ""
	}
	return "cannot create a session: " + session.DemandOnlySingletonExplanation(agentCfg.QualifiedName())
}

// demandOnlySingletonWakeRefusal returns the message refusing an explicit wake
// of info, or "" when the wake may proceed. The wake is recorded first (holds
// and quarantine are cleared, as `gc session wake` does), but a demand-only
// singleton's pool session that is not running will not start from it: the
// reconciler starts it only from pool demand (#6858, SESSION-RECON-019). The
// classification is shared with `gc session wake`
// (session.DemandOnlySingletonWakeRefused).
func demandOnlySingletonWakeRefusal(cfg *config.City, info session.Info) string {
	agentCfg, ok := findAgentByQualifiedTemplate(cfg, info.Template)
	if !ok || !session.DemandOnlySingletonWakeRefused(cfg, &agentCfg, info) {
		return ""
	}
	return "wake recorded for session " + info.ID + ", but it will not start: " + session.DemandOnlySingletonExplanation(agentCfg.QualifiedName())
}

func (s *Server) resolveAgentCreateContext(template, alias string) (agentCreateContext, error) {
	cfg := s.state.Config()
	if cfg == nil {
		return agentCreateContext{}, fmt.Errorf("no city config loaded")
	}
	agentCfg, ok := resolveSessionTemplateAgent(cfg, template)
	if !ok {
		return agentCreateContext{}, fmt.Errorf("resolved agent template disappeared: %s", template)
	}
	if alias != "" && agentCfg.SupportsMultipleSessions() {
		alias = workdirutil.SessionQualifiedName(s.state.CityPath(), agentCfg, cfg.Rigs, alias, "")
	}
	explicitName, err := sessionExplicitNameForCreate(agentCfg, alias)
	if err != nil {
		return agentCreateContext{}, err
	}
	identity := workdirutil.SessionQualifiedName(s.state.CityPath(), agentCfg, cfg.Rigs, alias, explicitName)
	workDir, err := s.resolveSessionWorkDir(agentCfg, identity)
	if err != nil {
		return agentCreateContext{}, err
	}
	return agentCreateContext{
		Agent:        agentCfg,
		Alias:        strings.TrimSpace(alias),
		ExplicitName: explicitName,
		Identity:     identity,
		WorkDir:      workDir,
	}, nil
}
