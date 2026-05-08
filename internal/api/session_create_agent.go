package api

import (
	"fmt"
	"strings"

	"github.com/gastownhall/gascity/internal/config"
	workdirutil "github.com/gastownhall/gascity/internal/workdir"
)

type agentCreateContext struct {
	Agent        config.Agent
	Alias        string
	ExplicitName string
	Identity     string
	WorkDir      string
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
