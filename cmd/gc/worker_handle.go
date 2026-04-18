package main

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/shellquote"
	"github.com/gastownhall/gascity/internal/worker"
)

func newWorkerSessionHandleWithConfig(cityPath string, store beads.Store, sp runtime.Provider, cfg *config.City, spec worker.SessionSpec) (worker.Handle, error) {
	factory, err := workerFactoryWithConfig(cityPath, store, sp, cfg)
	if err != nil {
		return nil, err
	}
	return factory.Session(spec)
}

func workerSessionCatalogWithConfig(cityPath string, store beads.Store, sp runtime.Provider, cfg *config.City) (*worker.SessionCatalog, error) {
	factory, err := workerFactoryWithConfig(cityPath, store, sp, cfg)
	if err != nil {
		return nil, err
	}
	return factory.Catalog()
}

func workerFactoryWithConfig(cityPath string, store beads.Store, sp runtime.Provider, cfg *config.City) (*worker.Factory, error) {
	var (
		resolveTransport func(template string) string
		searchPaths      []string
	)
	if cfg != nil {
		rigContext := currentRigContext(cfg)
		resolveTransport = func(template string) string {
			agentCfg, ok := resolveAgentIdentity(cfg, template, rigContext)
			if !ok {
				return ""
			}
			return agentCfg.Session
		}
		searchPaths = worker.MergeSearchPaths(cfg.Daemon.ObservePaths)
	}
	return worker.NewFactory(worker.FactoryConfig{
		Store:            store,
		Provider:         sp,
		CityPath:         cityPath,
		SearchPaths:      searchPaths,
		ResolveTransport: resolveTransport,
	})
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

func newWorkerSessionHandleForResolvedRuntimeWithConfig(
	cityPath string,
	store beads.Store,
	sp runtime.Provider,
	cfg *config.City,
	alias, explicitName, template, title, command, provider, workDir, transport string,
	resolved *config.ResolvedProvider,
	metadata map[string]string,
) (worker.Handle, error) {
	if resolved == nil {
		return nil, fmt.Errorf("resolved provider is required")
	}
	if strings.TrimSpace(command) == "" {
		command = resolved.CommandString()
	}
	providerName := strings.TrimSpace(resolved.Name)
	if providerName == "" {
		providerName = strings.TrimSpace(provider)
	}
	if providerName == "" {
		providerName = command
		if idx := strings.IndexAny(providerName, " \t"); idx >= 0 {
			providerName = providerName[:idx]
		}
	}
	return newWorkerSessionHandleWithConfig(cityPath, store, sp, cfg, worker.SessionSpec{
		Alias:        alias,
		ExplicitName: explicitName,
		Template:     template,
		Title:        title,
		Command:      command,
		WorkDir:      workDir,
		Provider:     providerName,
		Transport:    transport,
		Env:          resolved.Env,
		Resume: session.ProviderResume{
			ResumeFlag:    resolved.ResumeFlag,
			ResumeStyle:   resolved.ResumeStyle,
			ResumeCommand: resolved.ResumeCommand,
			SessionIDFlag: resolved.SessionIDFlag,
		},
		Hints:    workerSessionCreateHints(resolved),
		Metadata: metadata,
	})
}

func workerHandleForSessionWithConfig(cityPath string, store beads.Store, sp runtime.Provider, cfg *config.City, id string) (worker.Handle, error) {
	catalog, err := workerSessionCatalogWithConfig(cityPath, store, sp, cfg)
	if err != nil {
		return nil, err
	}
	info, err := catalog.Get(id)
	if err != nil {
		return nil, err
	}

	sessionKind := ""
	spec := worker.SessionSpec{
		ID:       id,
		Command:  info.Command,
		Provider: info.Provider,
		WorkDir:  info.WorkDir,
		Resume: session.ProviderResume{
			ResumeFlag:    info.ResumeFlag,
			ResumeStyle:   info.ResumeStyle,
			ResumeCommand: info.ResumeCommand,
		},
	}
	if store != nil {
		if bead, beadErr := store.Get(id); beadErr == nil {
			sessionKind = strings.TrimSpace(bead.Metadata["mc_session_kind"])
			if profile := strings.TrimSpace(bead.Metadata["worker_profile"]); profile != "" {
				spec.Profile = worker.Profile(profile)
			}
		}
	}
	applyResolvedWorkerRuntimeWithConfig(cityPath, cfg, info, sessionKind, &spec)

	factory, err := workerFactoryWithConfig(cityPath, store, sp, cfg)
	if err != nil {
		return nil, err
	}
	return factory.Session(spec)
}

func workerHandleForSessionTargetWithConfig(cityPath string, store beads.Store, sp runtime.Provider, cfg *config.City, target string) (worker.Handle, error) {
	return workerHandleForSessionTargetWithRuntimeHintsWithConfig(cityPath, store, sp, cfg, target, nil)
}

func workerHandleForSessionTargetWithRuntimeHintsWithConfig(cityPath string, store beads.Store, sp runtime.Provider, cfg *config.City, target string, processNames []string) (worker.Handle, error) {
	target = strings.TrimSpace(target)
	if target == "" {
		return nil, session.ErrSessionNotFound
	}
	if store != nil {
		if id, err := session.ResolveSessionIDByExactID(store, target); err == nil {
			return workerHandleForSessionWithConfig(cityPath, store, sp, cfg, id)
		}
		id, err := session.ResolveSessionID(store, target)
		if err == nil {
			return workerHandleForSessionWithConfig(cityPath, store, sp, cfg, id)
		}
		if sp != nil {
			if sessionID, metaErr := sp.GetMeta(target, "GC_SESSION_ID"); metaErr == nil && strings.TrimSpace(sessionID) != "" {
				return workerHandleForSessionWithConfig(cityPath, store, sp, cfg, strings.TrimSpace(sessionID))
			}
		}
	}
	if sp == nil {
		return nil, session.ErrSessionNotFound
	}
	return worker.NewRuntimeHandle(worker.RuntimeHandleConfig{
		Provider:     sp,
		SessionName:  target,
		ProviderName: target,
		ProcessNames: append([]string(nil), processNames...),
	})
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

func applyResolvedWorkerRuntimeWithConfig(cityPath string, cfg *config.City, info session.Info, sessionKind string, spec *worker.SessionSpec) {
	if cfg == nil || spec == nil {
		return
	}
	resolved := resolveWorkerRuntimeWithConfig(cfg, info, sessionKind)
	if resolved == nil {
		return
	}

	command := resolved.CommandString()
	if defaultArgs := resolved.ResolveDefaultArgs(); len(defaultArgs) > 0 {
		command = command + " " + shellquote.Join(defaultArgs)
	}
	if sa := settingsArgs(cityPath, resolved.Name); sa != "" {
		command = command + " " + sa
	}

	spec.Command = command
	spec.Provider = resolved.Name
	if strings.TrimSpace(spec.WorkDir) == "" {
		spec.WorkDir = cityPath
	}
	spec.Hints = runtime.Config{
		WorkDir:                spec.WorkDir,
		ReadyPromptPrefix:      resolved.ReadyPromptPrefix,
		ReadyDelayMs:           resolved.ReadyDelayMs,
		ProcessNames:           resolved.ProcessNames,
		EmitsPermissionWarning: resolved.EmitsPermissionWarning,
		Env:                    resolved.Env,
	}
	spec.Resume = session.ProviderResume{
		ResumeFlag:    resolved.ResumeFlag,
		ResumeStyle:   resolved.ResumeStyle,
		ResumeCommand: resolved.ResumeCommand,
		SessionIDFlag: resolved.SessionIDFlag,
	}
}

func resolveWorkerRuntimeWithConfig(cfg *config.City, info session.Info, sessionKind string) *config.ResolvedProvider {
	if cfg == nil {
		return nil
	}
	if sessionKind != "provider" {
		if found, ok := resolveAgentIdentity(cfg, info.Template, ""); ok {
			if resolved, err := config.ResolveProvider(&found, &cfg.Workspace, cfg.Providers, exec.LookPath); err == nil {
				return resolved
			}
		}
	}
	resolved, err := config.ResolveProvider(&config.Agent{Provider: info.Template}, &cfg.Workspace, cfg.Providers, exec.LookPath)
	if err != nil {
		return nil
	}
	return resolved
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
