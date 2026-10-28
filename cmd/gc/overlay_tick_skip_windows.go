//go:build windows

package main

import (
	"path/filepath"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/overlay"
)

// stagingOwnedMergeableHooks maps each mergeable hook file whose only writer
// is overlay staging, unless install_agent_hooks names its provider, to that
// provider.
var stagingOwnedMergeableHooks = map[string]string{
	filepath.Join(".github", "hooks", "gascity.json"): "copilot",
}

// tickOverlaySkip returns the staging filter for the reconcile tick, or nil to
// skip every mergeable file. The tick skips a mergeable file when hooks.Install
// writes it right after staging, so that hooks.Install is its one writer then.
// A staging-owned hook file that hooks.Install will not write is staged on the
// tick as well, exactly as session start stages it. Otherwise the first
// session's config fingerprint is taken without the file, and the next tick
// drains that session for config drift once session start has written it.
func tickOverlaySkip(installHooks []string, providers map[string]config.ProviderSpec) func(relPath string, isDir bool) bool {
	installed := make(map[string]bool, 2*len(installHooks))
	for _, hook := range installHooks {
		installed[hook] = true
		installed[config.BuiltinFamily(hook, providers)] = true
	}
	return func(relPath string, isDir bool) bool {
		if isDir || !overlay.IsMergeablePath(relPath) {
			return false
		}
		provider, staged := stagingOwnedMergeableHooks[filepath.Clean(relPath)]
		return !staged || installed[provider]
	}
}
