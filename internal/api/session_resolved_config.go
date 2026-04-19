package api

import (
	"fmt"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/worker"
)

func resolvedSessionConfigForProvider(
	alias, explicitName, template, title, transport string,
	metadata map[string]string,
	resolved *config.ResolvedProvider,
	command, workDir string,
) (worker.ResolvedSessionConfig, error) {
	if resolved == nil {
		return worker.ResolvedSessionConfig{}, fmt.Errorf("%w: resolved provider is required", worker.ErrHandleConfig)
	}
	return worker.NormalizeResolvedSessionConfig(worker.ResolvedSessionConfig{
		Alias:        alias,
		ExplicitName: explicitName,
		Template:     template,
		Title:        title,
		Transport:    transport,
		Metadata:     metadata,
		Runtime: worker.ResolvedRuntime{
			Command:    firstNonEmptyString(command, resolved.CommandString(), resolved.Name),
			WorkDir:    workDir,
			Provider:   resolved.Name,
			SessionEnv: resolved.Env,
			Resume: session.ProviderResume{
				ResumeFlag:    resolved.ResumeFlag,
				ResumeStyle:   resolved.ResumeStyle,
				ResumeCommand: resolved.ResumeCommand,
				SessionIDFlag: resolved.SessionIDFlag,
			},
			Hints: sessionCreateHints(resolved),
		},
	})
}
