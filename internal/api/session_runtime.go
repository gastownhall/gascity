package api

import (
	"errors"
	"fmt"
	"os/exec"
	"strings"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
	workdirutil "github.com/gastownhall/gascity/internal/workdir"
	"github.com/gastownhall/gascity/internal/worker"
)

func (s *Server) sessionLogPaths() []string {
	if s.sessionLogSearchPaths != nil {
		return s.sessionLogSearchPaths
	}
	cfg := s.state.Config()
	if cfg == nil {
		return worker.DefaultSearchPaths()
	}
	return worker.MergeSearchPaths(cfg.Daemon.ObservePaths)
}

func sessionCreateHints(resolved *config.ResolvedProvider) runtime.Config {
	return runtime.Config{
		ReadyPromptPrefix:      resolved.ReadyPromptPrefix,
		ReadyDelayMs:           resolved.ReadyDelayMs,
		ProcessNames:           resolved.ProcessNames,
		EmitsPermissionWarning: resolved.EmitsPermissionWarning,
	}
}

func sessionResumeHints(resolved *config.ResolvedProvider, workDir string) runtime.Config {
	return runtime.Config{
		WorkDir:                workDir,
		ReadyPromptPrefix:      resolved.ReadyPromptPrefix,
		ReadyDelayMs:           resolved.ReadyDelayMs,
		ProcessNames:           resolved.ProcessNames,
		EmitsPermissionWarning: resolved.EmitsPermissionWarning,
		Env:                    resolved.Env,
	}
}

func sessionExplicitNameForCreate(agentCfg config.Agent, alias string) (string, error) {
	if !agentCfg.SupportsMultipleSessions() || strings.TrimSpace(alias) != "" {
		return "", nil
	}
	return session.GenerateAdhocExplicitName(agentCfg.Name)
}

func (s *Server) resolveSessionWorkDir(agentCfg config.Agent, qualifiedName string) (string, error) {
	cfg := s.state.Config()
	if cfg == nil {
		return "", errors.New("no city config loaded")
	}
	workDir, err := workdirutil.ResolveWorkDirPathStrict(
		s.state.CityPath(),
		workdirutil.CityName(s.state.CityPath(), cfg),
		qualifiedName,
		agentCfg,
		cfg.Rigs,
	)
	if err != nil {
		return "", err
	}
	if workDir == "" {
		workDir = s.state.CityPath()
	}
	return workDir, nil
}

// resolveSessionTemplateWithBareNameFallback resolves a session template
// by name, retrying with the qualified name when the input is a bare
// agent name that matches exactly one configured agent. Keeps the
// two-phase lookup out of the handler.
func (s *Server) resolveSessionTemplateWithBareNameFallback(name string) (*config.ResolvedProvider, string, string, string, error) {
	resolved, workDir, transport, template, err := s.resolveSessionTemplate(name)
	if err == nil {
		return resolved, workDir, transport, template, nil
	}
	if !errors.Is(err, errSessionTemplateNotFound) || strings.Contains(name, "/") {
		return nil, "", "", "", err
	}
	agentCfg, ok := findUniqueAgentTemplateByBareName(s.state.Config(), name)
	if !ok {
		return nil, "", "", "", err
	}
	return s.resolveSessionTemplate(agentCfg.QualifiedName())
}

func (s *Server) resolveSessionTemplate(template string) (*config.ResolvedProvider, string, string, string, error) {
	cfg := s.state.Config()
	if cfg == nil {
		return nil, "", "", "", errors.New("no city config loaded")
	}
	agentCfg, ok := resolveSessionTemplateAgent(cfg, template)
	if !ok {
		return nil, "", "", "", errSessionTemplateNotFound
	}
	resolved, err := config.ResolveProvider(&agentCfg, &cfg.Workspace, cfg.Providers, exec.LookPath)
	if err != nil {
		return nil, "", "", "", err
	}
	workDir, err := s.resolveSessionWorkDir(agentCfg, agentCfg.QualifiedName())
	if err != nil {
		return nil, "", "", "", err
	}
	return resolved, workDir, agentCfg.Session, agentCfg.QualifiedName(), nil
}

func (s *Server) buildSessionResume(info session.Info) (string, runtime.Config) {
	cmd := session.BuildResumeCommand(info)
	resolved, workDir := s.resolveSessionRuntime(info)
	if resolved == nil {
		return cmd, runtime.Config{WorkDir: info.WorkDir}
	}
	resolvedInfo := info
	if command, err := s.resolvedSessionRuntimeCommand(resolved, info.Command); err == nil {
		resolvedInfo.Command = command
	} else {
		resolvedInfo.Command = firstNonEmptyString(info.Command, resolved.CommandString(), resolved.Name)
	}
	resolvedInfo.Provider = resolved.Name
	resolvedInfo.ResumeFlag = resolved.ResumeFlag
	resolvedInfo.ResumeStyle = resolved.ResumeStyle
	resolvedInfo.ResumeCommand = resolved.ResumeCommand
	return session.BuildResumeCommand(resolvedInfo), sessionResumeHints(resolved, workDir)
}

func (s *Server) resolvedSessionRuntimeCommand(resolved *config.ResolvedProvider, storedCommand string) (string, error) {
	if command := strings.TrimSpace(storedCommand); shouldPreserveStoredRuntimeCommand(command, resolved.CommandString()) {
		return command, nil
	}
	launchCommand, err := config.BuildProviderLaunchCommand(s.state.CityPath(), resolved, nil)
	if err != nil {
		return "", fmt.Errorf("building provider launch command: %w", err)
	}
	return firstNonEmptyString(launchCommand.Command, resolved.CommandString(), resolved.Name), nil
}

func shouldPreserveStoredRuntimeCommand(storedCommand, resolvedCommand string) bool {
	storedCommand = strings.TrimSpace(storedCommand)
	if storedCommand == "" {
		return false
	}
	resolvedCommand = strings.TrimSpace(resolvedCommand)
	if resolvedCommand == "" {
		return true
	}
	return storedCommand == resolvedCommand || strings.HasPrefix(storedCommand, resolvedCommand+" ")
}

func (s *Server) resolveWorkerSessionRuntime(info session.Info, _ string) (*worker.ResolvedRuntime, error) {
	resolved, workDir := s.resolveSessionRuntime(info)
	if resolved == nil {
		return nil, nil
	}
	command, err := s.resolvedSessionRuntimeCommand(resolved, info.Command)
	if err != nil {
		return nil, err
	}
	runtimeCfg, err := worker.NormalizeResolvedRuntime(worker.ResolvedRuntime{
		Command:    command,
		WorkDir:    firstNonEmptyString(workDir, info.WorkDir),
		Provider:   firstNonEmptyString(resolved.Name, info.Provider),
		SessionEnv: resolved.Env,
		Hints:      sessionResumeHints(resolved, firstNonEmptyString(workDir, info.WorkDir)),
		Resume: session.ProviderResume{
			ResumeFlag:    resolved.ResumeFlag,
			ResumeStyle:   resolved.ResumeStyle,
			ResumeCommand: resolved.ResumeCommand,
			SessionIDFlag: resolved.SessionIDFlag,
		},
	})
	if err != nil {
		return nil, err
	}
	return &runtimeCfg, nil
}

func (s *Server) resolveSessionRuntime(info session.Info) (*config.ResolvedProvider, string) {
	kind := s.sessionKind(info.ID)
	if kind != "provider" {
		resolved, workDir, _, _, err := s.resolveSessionTemplate(info.Template)
		if err == nil {
			if info.WorkDir != "" {
				workDir = info.WorkDir
			}
			return resolved, workDir
		}
	}

	resolved, err := s.resolveBareProvider(info.Template)
	if err != nil {
		return nil, ""
	}
	workDir := info.WorkDir
	if workDir == "" {
		workDir = s.state.CityPath()
	}
	return resolved, workDir
}

// sessionKind reads the persisted mc_session_kind from bead metadata.
func (s *Server) sessionKind(sessionID string) string {
	store := s.state.CityBeadStore()
	if store == nil {
		return ""
	}
	b, err := store.Get(sessionID)
	if err != nil {
		return ""
	}
	return b.Metadata["mc_session_kind"]
}

// resolveBareProvider resolves a provider by name without an agent template.
func (s *Server) resolveBareProvider(providerName string) (*config.ResolvedProvider, error) {
	cfg := s.state.Config()
	if cfg == nil {
		return nil, errors.New("no city config loaded")
	}
	return config.ResolveProvider(
		&config.Agent{Provider: providerName},
		&cfg.Workspace,
		cfg.Providers,
		exec.LookPath,
	)
}
