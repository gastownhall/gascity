package api

import (
	"errors"
	"fmt"
	"os/exec"
	"strings"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/materialize"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
	workdirutil "github.com/gastownhall/gascity/internal/workdir"
	"github.com/gastownhall/gascity/internal/worker"
)

var errAmbiguousLegacyACPTransport = errors.New("legacy session transport is ambiguous")

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

func sessionCreateHints(resolved *config.ResolvedProvider, mcpServers []runtime.MCPServerConfig) runtime.Config {
	return runtime.Config{
		ReadyPromptPrefix:      resolved.ReadyPromptPrefix,
		ReadyDelayMs:           resolved.ReadyDelayMs,
		ProcessNames:           resolved.ProcessNames,
		EmitsPermissionWarning: resolved.EmitsPermissionWarning,
		MCPServers:             mcpServers,
	}
}

func sessionResumeHints(resolved *config.ResolvedProvider, workDir string, mcpServers []runtime.MCPServerConfig) runtime.Config {
	return runtime.Config{
		WorkDir:                workDir,
		ReadyPromptPrefix:      resolved.ReadyPromptPrefix,
		ReadyDelayMs:           resolved.ReadyDelayMs,
		ProcessNames:           resolved.ProcessNames,
		EmitsPermissionWarning: resolved.EmitsPermissionWarning,
		Env:                    resolved.Env,
		MCPServers:             mcpServers,
	}
}

func resumeSessionIdentity(info session.Info, metadata map[string]string) string {
	if metadata != nil {
		if identity := strings.TrimSpace(metadata[session.MCPIdentityMetadataKey]); identity != "" {
			return identity
		}
	}
	return firstNonEmptyString(info.AgentName, info.Alias, info.Template, info.Provider)
}

func (s *Server) resumeSessionMCPServers(info session.Info, metadata map[string]string, resolved *config.ResolvedProvider, workDir, transport string) ([]runtime.MCPServerConfig, error) {
	if resolved == nil {
		return nil, nil
	}
	mcpServers, err := s.sessionMCPServers(
		info.Template,
		firstNonEmptyString(info.Provider, resolved.Name),
		resumeSessionIdentity(info, metadata),
		workDir,
		transport,
		s.sessionKind(info.ID),
	)
	if err == nil {
		return mcpServers, nil
	}
	runtimeSnapshot, loadErr := session.LoadRuntimeMCPServersSnapshot(s.state.CityPath(), info.ID)
	if loadErr != nil {
		return nil, loadErr
	}
	if len(runtimeSnapshot) > 0 {
		return runtimeSnapshot, nil
	}
	stored, decodeErr := session.DecodeMCPServersSnapshot(metadata[session.MCPServersSnapshotMetadataKey])
	if decodeErr != nil {
		return nil, fmt.Errorf("decoding stored MCP snapshot: %w", decodeErr)
	}
	return session.SanitizeStoredMCPSnapshotForResume(stored), nil
}

func (s *Server) providerSessionMCPServers(providerName, identity, workDir, transport string) ([]runtime.MCPServerConfig, error) {
	cfg := s.state.Config()
	if cfg == nil || strings.TrimSpace(workDir) == "" || strings.TrimSpace(transport) != "acp" {
		return nil, nil
	}
	synthetic := &config.Agent{Provider: providerName}
	catalog, err := materialize.EffectiveMCPForSession(cfg, s.state.CityPath(), synthetic, firstNonEmptyString(identity, providerName), workDir)
	if err != nil {
		return nil, fmt.Errorf("loading effective MCP: %w", err)
	}
	return materialize.RuntimeMCPServers(catalog.Servers), nil
}

func (s *Server) sessionMCPServers(template, providerName, identity, workDir, transport, sessionKind string) ([]runtime.MCPServerConfig, error) {
	cfg := s.state.Config()
	if cfg == nil || strings.TrimSpace(workDir) == "" || strings.TrimSpace(transport) != "acp" {
		return nil, nil
	}
	if sessionKind != "provider" {
		if agentCfg, ok := resolveSessionTemplateAgent(cfg, template); ok {
			catalog, err := materialize.EffectiveMCPForSession(
				cfg,
				s.state.CityPath(),
				&agentCfg,
				firstNonEmptyString(identity, template),
				workDir,
			)
			if err != nil {
				return nil, fmt.Errorf("loading effective MCP: %w", err)
			}
			return materialize.RuntimeMCPServers(catalog.Servers), nil
		}
	}
	return s.providerSessionMCPServers(firstNonEmptyString(providerName, template), identity, workDir, transport)
}

func (s *Server) sessionMetadata(sessionID string) map[string]string {
	store := s.state.CityBeadStore()
	if store == nil || strings.TrimSpace(sessionID) == "" {
		return nil
	}
	bead, err := store.Get(sessionID)
	if err != nil {
		return nil
	}
	return bead.Metadata
}

func providerSessionMCPIdentity(providerName, alias string) (string, error) {
	if alias = strings.TrimSpace(alias); alias != "" {
		return alias, nil
	}
	return session.GenerateAdhocIdentity(providerName)
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
	resolved, workDir, transport, template, err := s.resolveSessionTemplateForCreate(name)
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
	return s.resolveSessionTemplateForCreate(agentCfg.QualifiedName())
}

func (s *Server) resolveSessionTemplateForCreate(template string) (*config.ResolvedProvider, string, string, string, error) {
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
	return resolved, workDir, config.ResolveSessionCreateTransport(agentCfg.Session, resolved), agentCfg.QualifiedName(), nil
}

//nolint:unparam // kept as a focused test helper even though current call sites use one template shape.
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
	return resolved, workDir, config.ResolveSessionCreateTransport(agentCfg.Session, resolved), agentCfg.QualifiedName(), nil
}

func (s *Server) buildSessionResume(info session.Info) (string, runtime.Config, error) {
	cmd := session.BuildResumeCommand(info)
	metadata := s.sessionMetadata(info.ID)
	resolved, workDir, transport, ambiguous := s.resolveSessionRuntimeWithMetadata(info, metadata)
	if resolved == nil {
		return cmd, runtime.Config{WorkDir: info.WorkDir}, nil
	}
	if ambiguous {
		return "", runtime.Config{}, fmt.Errorf("%w: recreate the stopped session or resume it while ACP metadata can still be persisted", errAmbiguousLegacyACPTransport)
	}
	mcpServers, err := s.resumeSessionMCPServers(info, metadata, resolved, firstNonEmptyString(workDir, info.WorkDir), transport)
	if err != nil {
		return "", runtime.Config{}, err
	}
	resolvedInfo := info
	if command, err := s.resolvedSessionRuntimeCommand(resolved, transport, info.Command, metadata); err == nil {
		resolvedInfo.Command = command
	} else {
		resolvedInfo.Command = fallbackSessionRuntimeCommand(resolved, transport, info.Command, info.Provider)
	}
	resolvedInfo.Provider = resolved.Name
	resolvedInfo.Transport = transport
	resolvedInfo.ResumeFlag = resolved.ResumeFlag
	resolvedInfo.ResumeStyle = resolved.ResumeStyle
	resolvedInfo.ResumeCommand = resolved.ResumeCommand
	return session.BuildResumeCommand(resolvedInfo), sessionResumeHints(resolved, workDir, mcpServers), nil
}

func (s *Server) resolvedSessionRuntimeCommand(resolved *config.ResolvedProvider, transport, storedCommand string, metadata map[string]string) (string, error) {
	configuredCommand := configuredSessionRuntimeCommand(resolved, transport)
	if configuredCommand == "" {
		if command := strings.TrimSpace(storedCommand); command != "" {
			return command, nil
		}
		return "", fmt.Errorf("resolved provider %q has no launch command", resolved.Name)
	}
	optionOverrides, err := session.ParseTemplateOverrides(metadata)
	if err != nil {
		return "", fmt.Errorf("parsing template overrides: %w", err)
	}
	launchCommand, err := config.BuildProviderLaunchCommand(s.state.CityPath(), resolved, optionOverrides, transport)
	if err != nil {
		return "", fmt.Errorf("building provider launch command: %w", err)
	}
	desiredCommand := firstNonEmptyString(launchCommand.Command, configuredCommand, resolved.Name)
	if command := strings.TrimSpace(storedCommand); shouldPreserveStoredRuntimeCommandForTransport(command, desiredCommand, transport, optionOverrides) {
		return command, nil
	}
	return desiredCommand, nil
}

func configuredSessionRuntimeCommand(resolved *config.ResolvedProvider, transport string) string {
	if resolved == nil {
		return ""
	}
	if transport == "acp" && (strings.TrimSpace(resolved.ACPCommand) != "" || resolved.ACPArgs != nil) {
		return strings.TrimSpace(resolved.ACPCommandString())
	}
	if strings.TrimSpace(resolved.Command) != "" {
		return strings.TrimSpace(resolved.CommandString())
	}
	return ""
}

func fallbackSessionRuntimeCommand(resolved *config.ResolvedProvider, transport, storedCommand, fallbackProvider string) string {
	resolvedCommand := configuredSessionRuntimeCommand(resolved, transport)
	return firstNonEmptyString(storedCommand, resolvedCommand, fallbackProvider, resolved.Name)
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
	// A bare stored command (just the provider binary) lacks schema
	// defaults like --dangerously-skip-permissions and the --settings
	// path. Rebuild from the current config instead of preserving it.
	// See #799: pool-agent sessions resumed through the control-
	// dispatcher path wedged on interactive permission prompts because
	// the bare stored command was preserved without re-injecting flags.
	if storedCommand == resolvedCommand {
		return false
	}
	return strings.HasPrefix(storedCommand, resolvedCommand+" ")
}

func shouldPreserveStoredRuntimeCommandForTransport(storedCommand, resolvedCommand, _ string, optionOverrides map[string]string) bool {
	if shouldPreserveStoredRuntimeCommand(storedCommand, resolvedCommand) {
		return true
	}
	if len(optionOverrides) == 0 && storedCommandHasSettingsArg(storedCommand) && sameRuntimeCommandExecutable(storedCommand, resolvedCommand) {
		return true
	}
	return false
}

func sameRuntimeCommandExecutable(storedCommand, resolvedCommand string) bool {
	storedFields := strings.Fields(strings.TrimSpace(storedCommand))
	resolvedFields := strings.Fields(strings.TrimSpace(resolvedCommand))
	if len(storedFields) == 0 || len(resolvedFields) == 0 {
		return false
	}
	return storedFields[0] == resolvedFields[0]
}

func storedCommandHasSettingsArg(command string) bool {
	return strings.Contains(" "+strings.TrimSpace(command)+" ", " --settings ")
}

func (s *Server) resolveWorkerSessionRuntime(info session.Info) (*worker.ResolvedRuntime, error) {
	return s.resolveWorkerSessionRuntimeWithMetadata(info, "", nil)
}

func (s *Server) resolveWorkerSessionRuntimeWithMetadata(info session.Info, _ string, metadata map[string]string) (*worker.ResolvedRuntime, error) {
	if metadata == nil {
		metadata = s.sessionMetadata(info.ID)
	}
	resolved, workDir, transport, ambiguous := s.resolveSessionRuntimeWithMetadata(info, metadata)
	if resolved == nil {
		return nil, nil
	}
	if ambiguous {
		return nil, fmt.Errorf("%w: recreate the stopped session or resume it while ACP metadata can still be persisted", errAmbiguousLegacyACPTransport)
	}
	mcpServers, err := s.resumeSessionMCPServers(info, metadata, resolved, firstNonEmptyString(workDir, info.WorkDir), transport)
	if err != nil {
		return nil, err
	}
	command, err := s.resolvedSessionRuntimeCommand(resolved, transport, info.Command, metadata)
	if err != nil {
		command = fallbackSessionRuntimeCommand(resolved, transport, info.Command, info.Provider)
	}
	runtimeCfg, err := worker.NormalizeResolvedRuntime(worker.ResolvedRuntime{
		Command:    command,
		WorkDir:    firstNonEmptyString(info.WorkDir, workDir),
		Provider:   firstNonEmptyString(info.Provider, resolved.Name),
		SessionEnv: resolved.Env,
		Hints:      sessionResumeHints(resolved, firstNonEmptyString(workDir, info.WorkDir), mcpServers),
		Resume: session.ProviderResume{
			ResumeFlag:    firstNonEmptyString(resolved.ResumeFlag, info.ResumeFlag),
			ResumeStyle:   firstNonEmptyString(resolved.ResumeStyle, info.ResumeStyle),
			ResumeCommand: firstNonEmptyString(resolved.ResumeCommand, info.ResumeCommand),
			SessionIDFlag: resolved.SessionIDFlag,
		},
	})
	if err != nil {
		return nil, err
	}
	return &runtimeCfg, nil
}

func storedSessionProvesACPTransport(resolved *config.ResolvedProvider, configuredTransport, storedCommand string, metadata map[string]string) bool {
	if metadata != nil {
		if strings.TrimSpace(metadata[session.MCPIdentityMetadataKey]) != "" ||
			strings.TrimSpace(metadata[session.MCPServersSnapshotMetadataKey]) != "" {
			return true
		}
		if strings.TrimSpace(configuredTransport) == "acp" && legacyResumeMetadataProvesACPTransport(metadata) {
			return true
		}
	}
	if resolved == nil {
		return false
	}
	acpCommand := strings.TrimSpace(resolved.ACPCommandString())
	defaultCommand := strings.TrimSpace(resolved.CommandString())
	if acpCommand == "" || acpCommand == defaultCommand {
		return false
	}
	return shouldPreserveStoredRuntimeCommand(storedCommand, acpCommand)
}

func legacyResumeMetadataProvesACPTransport(metadata map[string]string) bool {
	if metadata == nil {
		return false
	}
	return strings.TrimSpace(metadata["resume_command"]) != "" ||
		strings.TrimSpace(metadata["resume_flag"]) != "" ||
		strings.TrimSpace(metadata["session_key"]) != ""
}

func legacyACPTransportAmbiguous(resolved *config.ResolvedProvider, configuredTransport, storedCommand string, metadata map[string]string) bool {
	if strings.TrimSpace(configuredTransport) != "acp" || resolved == nil {
		return false
	}
	if storedSessionProvesACPTransport(resolved, configuredTransport, storedCommand, metadata) {
		return false
	}
	acpCommand := strings.TrimSpace(resolved.ACPCommandString())
	defaultCommand := strings.TrimSpace(resolved.CommandString())
	if acpCommand == "" || acpCommand != defaultCommand {
		return false
	}
	storedCommand = strings.TrimSpace(storedCommand)
	return storedCommand == "" || sameRuntimeCommandExecutable(storedCommand, defaultCommand)
}

func (s *Server) startedConfigHashProvesACPTransport(
	info session.Info,
	metadata map[string]string,
	resolved *config.ResolvedProvider,
	workDir,
	configuredTransport,
	sessionKind string,
) bool {
	if strings.TrimSpace(configuredTransport) != "acp" || resolved == nil || metadata == nil {
		return false
	}
	startedHash := strings.TrimSpace(metadata["started_config_hash"])
	if startedHash == "" {
		return false
	}
	acpCommand, err := s.resolvedSessionRuntimeCommand(resolved, "acp", info.Command, metadata)
	if err != nil {
		acpCommand = fallbackSessionRuntimeCommand(resolved, "acp", info.Command, info.Provider)
	}
	defaultCommand, err := s.resolvedSessionRuntimeCommand(resolved, "", info.Command, metadata)
	if err != nil {
		defaultCommand = fallbackSessionRuntimeCommand(resolved, "", info.Command, info.Provider)
	}
	mcpServers, err := s.sessionMCPServers(
		info.Template,
		firstNonEmptyString(info.Provider, resolved.Name),
		resumeSessionIdentity(info, metadata),
		firstNonEmptyString(workDir, info.WorkDir),
		"acp",
		sessionKind,
	)
	if err != nil {
		return false
	}
	acpHash := runtime.CoreFingerprint(runtime.Config{
		Command:    acpCommand,
		Env:        resolved.Env,
		MCPServers: mcpServers,
	})
	defaultHash := runtime.CoreFingerprint(runtime.Config{
		Command: defaultCommand,
		Env:     resolved.Env,
	})
	if acpHash == defaultHash {
		return false
	}
	return startedHash == acpHash
}

func resolvedSessionTransport(info session.Info, resolved *config.ResolvedProvider, configuredTransport string, metadata map[string]string, allowConfiguredTransportFallback bool) string {
	if transport := strings.TrimSpace(info.Transport); transport != "" {
		return transport
	}
	if strings.TrimSpace(info.Provider) == "acp" {
		return "acp"
	}
	if storedSessionProvesACPTransport(resolved, configuredTransport, info.Command, metadata) {
		return "acp"
	}
	if strings.TrimSpace(info.Command) == "" {
		return strings.TrimSpace(configuredTransport)
	}
	if allowConfiguredTransportFallback {
		return strings.TrimSpace(configuredTransport)
	}
	return ""
}

func (s *Server) resolveSessionRuntimeWithMetadata(info session.Info, metadata map[string]string) (*config.ResolvedProvider, string, string, bool) {
	kind := s.sessionKind(info.ID)
	cfg := s.state.Config()
	var (
		resolved            *config.ResolvedProvider
		workDir             string
		configuredTransport string
	)
	if kind != "provider" && cfg != nil {
		if agentCfg, ok := resolveSessionTemplateAgent(cfg, info.Template); ok {
			candidate, err := config.ResolveProvider(&agentCfg, &cfg.Workspace, cfg.Providers, exec.LookPath)
			if err == nil {
				candidateWorkDir, workDirErr := s.resolveSessionWorkDir(agentCfg, agentCfg.QualifiedName())
				if workDirErr == nil {
					resolved = candidate
					workDir = candidateWorkDir
					if info.WorkDir != "" {
						workDir = info.WorkDir
					}
					configuredTransport = config.ResolveSessionCreateTransport(agentCfg.Session, resolved)
				}
			}
		}
	}
	if resolved == nil {
		candidate, err := s.resolveBareProvider(info.Template)
		if err != nil {
			return nil, "", "", false
		}
		resolved = candidate
		workDir = info.WorkDir
		if workDir == "" {
			workDir = s.state.CityPath()
		}
		configuredTransport = resolved.ProviderSessionCreateTransport()
	}
	transport := resolvedSessionTransport(info, resolved, configuredTransport, metadata, false)
	if transport == "" && s.startedConfigHashProvesACPTransport(info, metadata, resolved, workDir, configuredTransport, kind) {
		transport = "acp"
	}
	return resolved, workDir, transport, transport == "" && legacyACPTransportAmbiguous(resolved, configuredTransport, info.Command, metadata)
}

// sessionKind reads the persisted real_world_app_session_kind from bead metadata.
func (s *Server) sessionKind(sessionID string) string {
	store := s.state.CityBeadStore()
	if store == nil {
		return ""
	}
	b, err := store.Get(sessionID)
	if err != nil {
		return ""
	}
	return b.Metadata["real_world_app_session_kind"]
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
