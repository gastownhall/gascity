package api

import (
	"fmt"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/worker"
)

func resolvedSessionConfigForProvider(
	alias, explicitName, template, title, transport string,
	metadata map[string]string,
	resolved *config.ResolvedProvider,
	command, workDir string,
	mcpServers []runtime.MCPServerConfig,
) (worker.ResolvedSessionConfig, error) {
	if resolved == nil {
		return worker.ResolvedSessionConfig{}, fmt.Errorf("%w: resolved provider is required", worker.ErrHandleConfig)
	}
	if transport == "acp" {
		var err error
		metadata, err = session.WithStoredMCPMetadata(
			metadata,
			firstNonEmptyString(metadata[session.MCPIdentityMetadataKey], metadata["agent_name"]),
			mcpServers,
		)
		if err != nil {
			return worker.ResolvedSessionConfig{}, err
		}
	}
	// Use the ACP-specific command when the session uses ACP transport,
	// falling back to the default command for tmux sessions.
	resolvedCommand := resolved.CommandString()
	if transport == "acp" {
		resolvedCommand = resolved.ACPCommandString()
	}
	return worker.NormalizeResolvedSessionConfig(worker.ResolvedSessionConfig{
		Alias:        alias,
		ExplicitName: explicitName,
		Template:     template,
		Title:        title,
		Transport:    transport,
		Metadata:     metadata,
		Runtime: worker.ResolvedRuntime{
			Command:    firstNonEmptyString(command, resolvedCommand, resolved.Name),
			WorkDir:    workDir,
			Provider:   resolved.Name,
			SessionEnv: resolved.Env,
			Resume: session.ProviderResume{
				ResumeFlag:    resolved.ResumeFlag,
				ResumeStyle:   resolved.ResumeStyle,
				ResumeCommand: resolved.ResumeCommand,
				SessionIDFlag: resolved.SessionIDFlag,
			},
			Hints: sessionCreateHints(resolved, mcpServers),
		},
	})
}
