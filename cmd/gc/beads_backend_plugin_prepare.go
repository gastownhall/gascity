package main

import (
	"fmt"
	"io"
	"strings"

	"github.com/gastownhall/gascity/internal/config"
)

func prepareBeadsBackendPluginForInit(cityPath string, cfg *config.City, stdout, stderr io.Writer) error {
	if cfg == nil || !providerUsesBdStoreContract(cfg.Beads.Provider) {
		return nil
	}
	backend := strings.ToLower(strings.TrimSpace(cfg.Beads.Backend))
	if backend == "" {
		backend = "dolt"
	}
	var plugin config.DiscoveredBackendPlugin
	found := false
	for _, candidate := range cfg.BackendPlugins {
		if strings.ToLower(strings.TrimSpace(candidate.Backend)) == backend {
			plugin = candidate
			found = true
			break
		}
	}
	if !found || len(plugin.PrepareCommand) == 0 {
		return nil
	}
	entry, args, ok := findBackendPluginPrepareCommand(plugin, cfg.PackCommands)
	if !ok {
		return fmt.Errorf("beads backend plugin %q: prepare command %q from pack %q is not installed; run \"gc import install\"",
			backend, strings.Join(plugin.PrepareCommand, " "), plugin.PackName)
	}
	if code := runDiscoveredCommand(entry, cityPath, cfg.Workspace.Name, args, strings.NewReader(""), stdout, stderr); code != 0 {
		return fmt.Errorf("beads backend plugin %q: prepare command failed with exit code %d", backend, code)
	}
	return nil
}

func findBackendPluginPrepareCommand(plugin config.DiscoveredBackendPlugin, commands []config.DiscoveredCommand) (config.DiscoveredCommand, []string, bool) {
	words := trimPrepareCommandWords(plugin.PrepareCommand)
	if len(words) == 0 {
		return config.DiscoveredCommand{}, nil, false
	}
	var best config.DiscoveredCommand
	var bestLen int
	for _, entry := range commands {
		if !sameBackendPluginPack(plugin, entry) {
			continue
		}
		if len(entry.Command) == 0 || len(entry.Command) > len(words) {
			continue
		}
		if len(entry.Command) <= bestLen {
			continue
		}
		if commandWordsEqual(entry.Command, words[:len(entry.Command)]) {
			best = entry
			bestLen = len(entry.Command)
		}
	}
	if bestLen == 0 {
		return config.DiscoveredCommand{}, nil, false
	}
	return best, append([]string(nil), words[bestLen:]...), true
}

func sameBackendPluginPack(plugin config.DiscoveredBackendPlugin, command config.DiscoveredCommand) bool {
	if strings.TrimSpace(plugin.PackDir) != "" && samePath(plugin.PackDir, command.PackDir) {
		return true
	}
	return strings.TrimSpace(plugin.PackName) != "" && strings.TrimSpace(plugin.PackName) == strings.TrimSpace(command.PackName)
}

func trimPrepareCommandWords(words []string) []string {
	out := make([]string, 0, len(words))
	for _, word := range words {
		word = strings.TrimSpace(word)
		if word != "" {
			out = append(out, word)
		}
	}
	return out
}

func commandWordsEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
