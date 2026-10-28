//go:build !windows

package main

import "github.com/gastownhall/gascity/internal/config"

// tickOverlaySkip returns the staging filter for the reconcile tick, or nil to
// skip every mergeable file, which hooks.Install owns on the tick.
func tickOverlaySkip(_ []string, _ map[string]config.ProviderSpec) func(relPath string, isDir bool) bool {
	return nil
}
