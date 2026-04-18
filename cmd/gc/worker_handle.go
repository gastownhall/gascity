package main

import (
	"fmt"
	"os/exec"
	"strings"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/shellquote"
	"github.com/gastownhall/gascity/internal/worker"
)

func newWorkerSessionHandleWithConfig(cityPath string, store beads.Store, sp runtime.Provider, cfg *config.City, spec worker.SessionSpec) (*worker.SessionHandle, error) {
	return worker.NewSessionHandle(worker.SessionHandleConfig{
		Manager: newSessionManagerWithConfig(cityPath, store, sp, cfg),
		Session: spec,
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
) (*worker.SessionHandle, error) {
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

func workerHandleForSessionWithConfig(cityPath string, store beads.Store, sp runtime.Provider, cfg *config.City, id string) (*worker.SessionHandle, error) {
	mgr := newSessionManagerWithConfig(cityPath, store, sp, cfg)
	info, err := mgr.Get(id)
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

	return worker.NewSessionHandle(worker.SessionHandleConfig{
		Manager: mgr,
		Session: spec,
	})
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
