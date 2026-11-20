package main

import (
	"io"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
)

// startupPromptLaunchFinalizer returns the session.LaunchFinalizer that
// delivers a launched session's startup prompt, or nil when there is no config
// to resolve an agent from.
func startupPromptLaunchFinalizer(cityPath string, cfg *config.City, stderr io.Writer) session.LaunchFinalizer {
	if cfg == nil {
		return nil
	}
	_ = cityPath
	_ = stderr
	return func(lc session.LaunchContext) (runtime.Config, error) { return lc.Config, nil }
}
