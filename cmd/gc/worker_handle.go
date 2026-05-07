package main

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/materialize"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/worker"
)

func workerSessionCatalogWithConfig(cityPath string, store beads.Store, sp runtime.Provider, cfg *config.City) (*worker.SessionCatalog, error) {
	factory, err := workerFactoryWithConfig(cityPath, store, sp, cfg)
	if err != nil {
		return nil, err
	}
	return factory.Catalog()
}

func workerFactoryWithConfig(cityPath string, store beads.Store, sp runtime.Provider, cfg *config.City) (*worker.Factory, error) {
	var (
		resolveTransport func(template, provider string) string
		searchPaths      []string
	)
	if cfg != nil {
		rigContext := currentRigContext(cfg)
		resolveTransport = func(template, provider string) string {
			agentCfg, ok := resolveAgentIdentity(cfg, template, rigContext)
			if ok {
				resolved, err := config.ResolveProvider(
					&agentCfg,
					&cfg.Workspace,
					cfg.Providers,
					func(name string) (string, error) { return name, nil },
				)
				if err != nil {
					return agentCfg.Session
				}
				return config.ResolveSessionCreateTransport(agentCfg.Session, resolved)
			}
			provider = strings.TrimSpace(provider)
			if provider == "" {
				provider = strings.TrimSpace(template)
			}
			if provider == "" {
				return ""
			}
			resolved, err := config.ResolveProvider(
				&config.Agent{Provider: provider},
				&cfg.Workspace,
				cfg.Providers,
				func(name string) (string, error) { return name, nil },
			)
			if err != nil {
				return ""
			}
			return strings.TrimSpace(resolved.ProviderSessionCreateTransport())
		}
		searchPaths = worker.MergeSearchPaths(cfg.Daemon.ObservePaths)
	}
	return worker.NewFactory(worker.FactoryConfig{
		Store:                 store,
		Provider:              sp,
		CityPath:              cityPath,
		SearchPaths:           searchPaths,
		ResolveTransport:      resolveTransport,
		ResolveSessionRuntime: workerSessionRuntimeResolverWithConfig(cityPath, cfg),
	})
}

func workerSessionRuntimeResolverWithConfig(cityPath string, cfg *config.City) worker.SessionRuntimeResolver {
	if cfg == nil {
		return nil
	}
	return func(info session.Info, sessionKind string, metadata map[string]string) (*worker.ResolvedRuntime, error) {
		runtimeCfg, err := resolvedWorkerRuntimeWithConfigAndMetadata(cityPath, cfg, info, sessionKind, metadata)
		if err != nil {
			return nil, err
		}
		if runtimeCfg == nil {
			return nil, nil
		}
		normalized, err := worker.NormalizeResolvedRuntime(*runtimeCfg)
		if err != nil {
			return nil, err
		}
		return &normalized, nil
	}
}

func workerSessionCreateHints(resolved *config.ResolvedProvider) runtime.Config {
	if resolved == nil {
		return runtime.Config{}
	}
	return runtime.Config{
		ReadyPromptPrefix:      resolved.ReadyPromptPrefix,
		ReadyDelayMs:           resolved.ReadyDelayMs,
		ProcessNames:           resolved.ProcessNames,
		EmitsPermissionWarning: resolved.EmitsPermissionWarning,
	}
}

func resolvedRuntimeMCPServersWithConfig(
	cityPath string,
	cfg *config.City,
	alias, template, provider, workDir string,
	transport string,
	metadata map[string]string,
) ([]runtime.MCPServerConfig, error) {
	if cfg == nil || strings.TrimSpace(workDir) == "" || strings.TrimSpace(transport) != "acp" {
		return nil, nil
	}
	identity := strings.TrimSpace(metadata[session.MCPIdentityMetadataKey])
	if identity == "" {
		identity = strings.TrimSpace(metadata["agent_name"])
	}
	if identity == "" {
		identity = strings.TrimSpace(alias)
	}
	if identity == "" {
		identity = strings.TrimSpace(template)
	}
	if identity == "" {
		identity = strings.TrimSpace(provider)
	}
	if agentCfg := findAgentByTemplate(cfg, template); agentCfg != nil {
		catalog, err := materialize.EffectiveMCPForSession(cfg, cityPath, agentCfg, identity, workDir)
		if err != nil {
			return nil, fmt.Errorf("loading effective MCP: %w", err)
		}
		return materialize.RuntimeMCPServers(catalog.Servers), nil
	}
	synthetic := &config.Agent{Provider: provider}
	catalog, err := materialize.EffectiveMCPForSession(cfg, cityPath, synthetic, identity, workDir)
	if err != nil {
		return nil, fmt.Errorf("loading effective MCP: %w", err)
	}
	return materialize.RuntimeMCPServers(catalog.Servers), nil
}

func resumeRuntimeMCPServersWithConfig(
	cityPath string,
	cfg *config.City,
	info session.Info,
	resolved *config.ResolvedProvider,
	transport string,
	metadata map[string]string,
) ([]runtime.MCPServerConfig, error) {
	if cfg == nil || resolved == nil {
		return nil, nil
	}
	workDir := strings.TrimSpace(info.WorkDir)
	if workDir == "" {
		workDir = cityPath
	}
	resumeMeta := make(map[string]string)
	for key, value := range metadata {
		resumeMeta[key] = value
	}
	if agentName := strings.TrimSpace(info.AgentName); agentName != "" {
		resumeMeta["agent_name"] = agentName
	}
	mcpServers, err := resolvedRuntimeMCPServersWithConfig(
		cityPath,
		cfg,
		info.Alias,
		info.Template,
		firstNonEmptyGCString(info.Provider, resolved.Name, info.Template),
		workDir,
		transport,
		resumeMeta,
	)
	if err == nil {
		return mcpServers, nil
	}
	runtimeSnapshot, loadErr := session.LoadRuntimeMCPServersSnapshot(cityPath, info.ID)
	if loadErr != nil {
		return nil, loadErr
	}
	if len(runtimeSnapshot) > 0 {
		return runtimeSnapshot, nil
	}
	stored, decodeErr := session.DecodeMCPServersSnapshot(resumeMeta[session.MCPServersSnapshotMetadataKey])
	if decodeErr != nil {
		return nil, fmt.Errorf("decoding stored MCP snapshot: %w", decodeErr)
	}
	return session.SanitizeStoredMCPSnapshotForResume(stored), nil
}

func newWorkerSessionHandleForResolvedRuntimeWithConfig(
	cityPath string,
	store beads.Store,
	sp runtime.Provider,
	cfg *config.City,
	alias, explicitName, template, title, command, provider, workDir, transport string,
	resolved *config.ResolvedProvider,
	metadata map[string]string,
) (worker.Handle, error) {
	factory, err := workerFactoryWithConfig(cityPath, store, sp, cfg)
	if err != nil {
		return nil, err
	}
	mcpServers, err := resolvedRuntimeMCPServersWithConfig(cityPath, cfg, alias, template, provider, workDir, transport, metadata)
	if err != nil {
		return nil, err
	}
	sessionCfg, err := resolvedWorkerSessionConfigWithConfig(
		command,
		provider,
		workDir,
		alias,
		explicitName,
		template,
		title,
		transport,
		resolved,
		metadata,
		mcpServers,
	)
	if err != nil {
		return nil, err
	}
	return factory.SessionForResolvedRuntime(sessionCfg)
}

func resolvedWorkerSessionConfigWithConfig(
	command string,
	provider string,
	workDir string,
	alias string,
	explicitName string,
	template string,
	title string,
	transport string,
	resolved *config.ResolvedProvider,
	metadata map[string]string,
	mcpServers []runtime.MCPServerConfig,
) (worker.ResolvedSessionConfig, error) {
	if resolved == nil {
		return worker.ResolvedSessionConfig{}, fmt.Errorf("resolved provider is required")
	}
	if transport == "acp" {
		var err error
		metadata, err = session.WithStoredMCPMetadata(
			metadata,
			firstNonEmptyGCString(metadata[session.MCPIdentityMetadataKey], metadata["agent_name"]),
			mcpServers,
		)
		if err != nil {
			return worker.ResolvedSessionConfig{}, err
		}
	}
	command = strings.TrimSpace(command)
	if command == "" {
		if transport == "acp" {
			command = strings.TrimSpace(resolved.ACPCommandString())
		} else {
			command = strings.TrimSpace(resolved.CommandString())
		}
	}
	providerName := strings.TrimSpace(resolved.Name)
	if providerName == "" {
		providerName = strings.TrimSpace(provider)
	}
	if command == "" {
		command = providerName
	}
	return worker.NormalizeResolvedSessionConfig(worker.ResolvedSessionConfig{
		Alias:        alias,
		ExplicitName: explicitName,
		Template:     template,
		Title:        title,
		Transport:    transport,
		Metadata:     metadata,
		Runtime: worker.ResolvedRuntime{
			Command:    command,
			WorkDir:    workDir,
			Provider:   providerName,
			SessionEnv: resolved.Env,
			Resume: session.ProviderResume{
				ResumeFlag:    resolved.ResumeFlag,
				ResumeStyle:   resolved.ResumeStyle,
				ResumeCommand: resolved.ResumeCommand,
				SessionIDFlag: resolved.SessionIDFlag,
			},
			Hints: func() runtime.Config {
				hints := workerSessionCreateHints(resolved)
				hints.MCPServers = mcpServers
				return hints
			}(),
		},
	})
}

func workerHandleForSessionWithConfig(cityPath string, store beads.Store, sp runtime.Provider, cfg *config.City, id string) (worker.Handle, error) {
	factory, err := workerFactoryWithConfig(cityPath, store, sp, cfg)
	if err != nil {
		return nil, err
	}
	return factory.SessionByID(id)
}

func workerHandleForSessionTargetWithConfig(cityPath string, store beads.Store, sp runtime.Provider, cfg *config.City, target string) (worker.Handle, error) {
	return workerHandleForSessionTargetWithRuntimeHintsWithConfig(cityPath, store, sp, cfg, target, nil)
}

func workerHandleForSessionTargetWithRuntimeHintsWithConfig(cityPath string, store beads.Store, sp runtime.Provider, cfg *config.City, target string, processNames []string) (worker.Handle, error) {
	target = strings.TrimSpace(target)
	if target == "" {
		return nil, session.ErrSessionNotFound
	}
	factory, err := workerFactoryWithConfig(cityPath, store, sp, cfg)
	if err != nil {
		return nil, err
	}
	if store != nil {
		if bead, _, err := session.ResolveSessionBeadByExactID(store, target); err == nil {
			return factory.SessionByLoadedBead(bead)
		}
		if id, err := session.ResolveSessionID(store, target); err == nil {
			return factory.SessionByID(id)
		}
		if sp != nil {
			if sessionID, metaErr := sp.GetMeta(target, "GC_SESSION_ID"); metaErr == nil && strings.TrimSpace(sessionID) != "" {
				return factory.SessionByID(strings.TrimSpace(sessionID))
			}
		}
	}
	if sp == nil {
		return nil, session.ErrSessionNotFound
	}
	providerName := target
	if liveProvider, err := sp.GetMeta(target, "GC_PROVIDER"); err == nil && strings.TrimSpace(liveProvider) != "" {
		providerName = strings.TrimSpace(liveProvider)
	}
	return factory.RuntimeHandle(target, providerName, "", processNames)
}

func runtimeWorkerHandleWithConfig(
	cityPath string,
	store beads.Store,
	sp runtime.Provider,
	cfg *config.City,
	sessionName string,
	providerName string,
	transport string,
	processNames []string,
) (worker.Handle, error) {
	factory, err := workerFactoryWithConfig(cityPath, store, sp, cfg)
	if err != nil {
		return nil, err
	}
	return factory.RuntimeHandle(sessionName, providerName, transport, processNames)
}

func workerKillSessionTargetWithConfig(cityPath string, store beads.Store, sp runtime.Provider, cfg *config.City, target string) error {
	handle, err := workerHandleForSessionTargetWithConfig(cityPath, store, sp, cfg, target)
	if err != nil {
		return err
	}
	return handle.Kill(context.Background())
}

func workerStopSessionTargetWithConfig(cityPath string, store beads.Store, sp runtime.Provider, cfg *config.City, target string) error {
	handle, err := workerHandleForSessionTargetWithConfig(cityPath, store, sp, cfg, target)
	if err != nil {
		return err
	}
	return handle.Stop(context.Background())
}

func workerInterruptSessionTargetWithConfig(cityPath string, store beads.Store, sp runtime.Provider, cfg *config.City, target string) error {
	handle, err := workerHandleForSessionTargetWithConfig(cityPath, store, sp, cfg, target)
	if err != nil {
		return err
	}
	return handle.Interrupt(context.Background(), worker.InterruptRequest{})
}

func workerObserveSessionTargetWithConfig(cityPath string, store beads.Store, sp runtime.Provider, cfg *config.City, target string) (worker.LiveObservation, error) {
	return workerObserveSessionTargetWithRuntimeHintsWithConfig(cityPath, store, sp, cfg, target, nil)
}

func workerObserveSessionTargetWithRuntimeHintsWithConfig(cityPath string, store beads.Store, sp runtime.Provider, cfg *config.City, target string, processNames []string) (worker.LiveObservation, error) {
	handle, err := workerHandleForSessionTargetWithRuntimeHintsWithConfig(cityPath, store, sp, cfg, target, processNames)
	if err != nil {
		return worker.LiveObservation{}, err
	}
	return worker.ObserveHandle(context.Background(), handle)
}

func workerSessionTargetRunningWithConfig(cityPath string, store beads.Store, sp runtime.Provider, cfg *config.City, target string) (bool, error) {
	obs, err := workerObserveSessionTargetWithConfig(cityPath, store, sp, cfg, target)
	if err != nil {
		return false, err
	}
	return obs.Running, nil
}

func workerSessionTargetAliveWithConfig(store beads.Store, sp runtime.Provider, cfg *config.City, target string, processNames []string) (bool, error) {
	obs, err := workerObserveSessionTargetWithRuntimeHintsWithConfig("", store, sp, cfg, target, processNames)
	if err != nil {
		return false, err
	}
	return obs.Alive, nil
}

func workerSessionTargetAttachedWithConfig(cityPath string, store beads.Store, sp runtime.Provider, cfg *config.City, target string) (bool, error) {
	obs, err := workerObserveSessionTargetWithConfig(cityPath, store, sp, cfg, target)
	if err != nil {
		return false, err
	}
	return obs.Attached, nil
}

func workerSessionTargetLastActivityWithConfig(cityPath string, store beads.Store, sp runtime.Provider, cfg *config.City, target string) (time.Time, error) {
	obs, err := workerObserveSessionTargetWithConfig(cityPath, store, sp, cfg, target)
	if err != nil {
		return time.Time{}, err
	}
	if obs.LastActivity == nil {
		return time.Time{}, nil
	}
	return *obs.LastActivity, nil
}

func workerSessionTargetPeekWithConfig(cityPath string, store beads.Store, sp runtime.Provider, cfg *config.City, target string, lines int, processNames []string) (string, error) {
	handle, err := workerHandleForSessionTargetWithRuntimeHintsWithConfig(cityPath, store, sp, cfg, target, processNames)
	if err != nil {
		return "", err
	}
	return handle.Peek(context.Background(), lines)
}

func workerSessionTargetPendingWithConfig(cityPath string, store beads.Store, sp runtime.Provider, cfg *config.City, target string) (*worker.PendingInteraction, error) {
	handle, err := workerHandleForSessionTargetWithConfig(cityPath, store, sp, cfg, target)
	if err != nil {
		return nil, err
	}
	return handle.Pending(context.Background())
}

func workerRespondSessionTargetWithConfig(cityPath string, store beads.Store, sp runtime.Provider, cfg *config.City, target string, response worker.InteractionResponse) error {
	handle, err := workerHandleForSessionTargetWithConfig(cityPath, store, sp, cfg, target)
	if err != nil {
		return err
	}
	return handle.Respond(context.Background(), response)
}

func resolvedWorkerRuntimeWithConfig(cityPath string, cfg *config.City, info session.Info, sessionKind string) (*worker.ResolvedRuntime, error) {
	return resolvedWorkerRuntimeWithConfigAndMetadata(cityPath, cfg, info, sessionKind, nil)
}

func resolvedWorkerRuntimeWithConfigAndMetadata(cityPath string, cfg *config.City, info session.Info, sessionKind string, metadata map[string]string) (*worker.ResolvedRuntime, error) {
	if cfg == nil {
		return nil, nil
	}
	resolved, configuredTransport := resolveWorkerRuntimeProviderWithConfig(cfg, info, sessionKind)
	if resolved == nil {
		return nil, nil
	}
	transport := resolvedWorkerRuntimeTransport(info, resolved, configuredTransport, metadata)
	if transport == "" && startedConfigHashProvesWorkerACPTransport(cityPath, cfg, info, sessionKind, resolved, metadata, configuredTransport) {
		transport = "acp"
	}
	if transport == "" && legacyWorkerACPTransportAmbiguous(resolved, configuredTransport, info.Command, metadata) {
		return nil, fmt.Errorf("legacy session transport is ambiguous: recreate the stopped session or resume it while ACP metadata can still be persisted")
	}

	command := resolvedWorkerRuntimeCommandForTransport(cityPath, resolved, transport, info.Command, info.Provider, metadata)

	workDir := strings.TrimSpace(info.WorkDir)
	if workDir == "" {
		workDir = cityPath
	}
	mcpServers, err := resumeRuntimeMCPServersWithConfig(cityPath, cfg, info, resolved, transport, metadata)
	if err != nil {
		return nil, err
	}
	return &worker.ResolvedRuntime{
		Command:    command,
		WorkDir:    workDir,
		Provider:   firstNonEmptyGCString(info.Provider, resolved.Name),
		SessionEnv: resolved.Env,
		Hints: runtime.Config{
			WorkDir:                workDir,
			ReadyPromptPrefix:      resolved.ReadyPromptPrefix,
			ReadyDelayMs:           resolved.ReadyDelayMs,
			ProcessNames:           resolved.ProcessNames,
			EmitsPermissionWarning: resolved.EmitsPermissionWarning,
			MCPServers:             mcpServers,
		},
		Resume: session.ProviderResume{
			ResumeFlag:    firstNonEmptyGCString(resolved.ResumeFlag, info.ResumeFlag),
			ResumeStyle:   firstNonEmptyGCString(resolved.ResumeStyle, info.ResumeStyle),
			ResumeCommand: firstNonEmptyGCString(resolved.ResumeCommand, info.ResumeCommand),
			SessionIDFlag: resolved.SessionIDFlag,
		},
	}, nil
}

func resolvedWorkerRuntimeCommandForTransport(cityPath string, resolved *config.ResolvedProvider, transport, storedCommand, fallbackProvider string, metadata map[string]string) string {
	command := strings.TrimSpace(storedCommand)
	configuredCommand := configuredWorkerRuntimeCommand(resolved, transport)
	if configuredCommand == "" {
		return firstNonEmptyGCString(command, fallbackProvider, resolved.Name)
	}
	desiredCommand := configuredCommand
	if optionOverrides, err := session.ParseTemplateOverrides(metadata); err == nil {
		if launchCommand, err := config.BuildProviderLaunchCommand(cityPath, resolved, optionOverrides, transport); err == nil {
			desiredCommand = firstNonEmptyGCString(launchCommand.Command, configuredCommand, resolved.Name)
			if shouldPreserveStoredRuntimeCommandForTransport(command, desiredCommand, transport, optionOverrides) {
				desiredCommand = command
			}
		}
	}
	if !shouldPreserveStoredRuntimeCommand(command, desiredCommand) {
		command = desiredCommand
	}
	return firstNonEmptyGCString(command, fallbackProvider, resolved.Name)
}

func configuredWorkerRuntimeCommand(resolved *config.ResolvedProvider, transport string) string {
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

func storedWorkerSessionProvesACPTransport(resolved *config.ResolvedProvider, configuredTransport, storedCommand string, metadata map[string]string) bool {
	if metadata != nil {
		if strings.TrimSpace(metadata[session.MCPIdentityMetadataKey]) != "" ||
			strings.TrimSpace(metadata[session.MCPServersSnapshotMetadataKey]) != "" {
			return true
		}
		if strings.TrimSpace(configuredTransport) == "acp" && legacyWorkerResumeMetadataProvesACPTransport(metadata) {
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

func legacyWorkerResumeMetadataProvesACPTransport(metadata map[string]string) bool {
	if metadata == nil {
		return false
	}
	return strings.TrimSpace(metadata["resume_command"]) != "" ||
		strings.TrimSpace(metadata["resume_flag"]) != "" ||
		strings.TrimSpace(metadata["session_key"]) != ""
}

func legacyWorkerACPTransportAmbiguous(resolved *config.ResolvedProvider, configuredTransport, storedCommand string, metadata map[string]string) bool {
	if strings.TrimSpace(configuredTransport) != "acp" || resolved == nil {
		return false
	}
	if storedWorkerSessionProvesACPTransport(resolved, configuredTransport, storedCommand, metadata) {
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

func startedConfigHashProvesWorkerACPTransport(
	cityPath string,
	cfg *config.City,
	info session.Info,
	_ string,
	resolved *config.ResolvedProvider,
	metadata map[string]string,
	configuredTransport string,
) bool {
	if cfg == nil || resolved == nil || metadata == nil || strings.TrimSpace(configuredTransport) != "acp" {
		return false
	}
	startedHash := strings.TrimSpace(metadata["started_config_hash"])
	if startedHash == "" {
		return false
	}
	acpCommand := resolvedWorkerRuntimeCommandForTransport(cityPath, resolved, "acp", info.Command, info.Provider, metadata)
	defaultCommand := resolvedWorkerRuntimeCommandForTransport(cityPath, resolved, "", info.Command, info.Provider, metadata)
	mcpServers, err := resolvedRuntimeMCPServersWithConfig(
		cityPath,
		cfg,
		info.Alias,
		info.Template,
		firstNonEmptyGCString(info.Provider, resolved.Name, info.Template),
		firstNonEmptyGCString(info.WorkDir, cityPath),
		"acp",
		metadata,
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

func resolvedWorkerRuntimeTransport(info session.Info, resolved *config.ResolvedProvider, configuredTransport string, metadata map[string]string) string {
	if transport := strings.TrimSpace(info.Transport); transport != "" {
		return transport
	}
	if strings.TrimSpace(info.Provider) == "acp" {
		return "acp"
	}
	if storedWorkerSessionProvesACPTransport(resolved, configuredTransport, info.Command, metadata) {
		return "acp"
	}
	if strings.TrimSpace(info.Command) == "" {
		return strings.TrimSpace(configuredTransport)
	}
	return ""
}

func resolveWorkerRuntimeProviderWithConfig(cfg *config.City, info session.Info, sessionKind string) (*config.ResolvedProvider, string) {
	if cfg == nil {
		return nil, ""
	}
	if sessionKind != "provider" {
		if found, ok := resolveAgentIdentity(cfg, info.Template, ""); ok {
			if resolved, err := config.ResolveProvider(&found, &cfg.Workspace, cfg.Providers, exec.LookPath); err == nil {
				return resolved, config.ResolveSessionCreateTransport(found.Session, resolved)
			}
		}
	}
	resolved, err := config.ResolveProvider(&config.Agent{Provider: info.Template}, &cfg.Workspace, cfg.Providers, exec.LookPath)
	if err != nil {
		return nil, ""
	}
	return resolved, strings.TrimSpace(resolved.ProviderSessionCreateTransport())
}

func workerDeliveryIntentForSubmitIntent(intent session.SubmitIntent) worker.DeliveryIntent {
	switch intent {
	case session.SubmitIntentFollowUp:
		return worker.DeliveryIntentFollowUp
	case session.SubmitIntentInterruptNow:
		return worker.DeliveryIntentInterruptNow
	default:
		return worker.DeliveryIntentDefault
	}
}

func workerNudgeDeliveryForMode(mode nudgeDeliveryMode) (worker.NudgeDelivery, bool) {
	switch mode {
	case nudgeDeliveryImmediate:
		return worker.NudgeDeliveryImmediate, true
	case nudgeDeliveryWaitIdle:
		return worker.NudgeDeliveryWaitIdle, true
	default:
		return "", false
	}
}

func firstNonEmptyGCString(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}
