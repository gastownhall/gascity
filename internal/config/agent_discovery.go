package config

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/BurntSushi/toml"
	"github.com/gastownhall/gascity/internal/fsys"
)

// DiscoverPackAgents scans a pack's agents/ tree and returns
// convention-discovered agents. Each immediate subdirectory is an agent.
// agent.toml provides optional per-agent config, prompt.template.md is
// canonical, prompt.md.tmpl remains temporarily supported, and prompt.md is
// the plain-markdown fallback.
func DiscoverPackAgents(fs fsys.FS, packDir, _ string, skipNames map[string]bool) ([]Agent, error) {
	agentsDir := filepath.Join(packDir, "agents")
	entries, err := fs.ReadDir(agentsDir)
	if err != nil {
		return nil, nil
	}

	var discovered []Agent
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		agentName := entry.Name()
		if strings.HasPrefix(agentName, ".") || strings.HasPrefix(agentName, "_") {
			continue
		}
		if skipNames != nil && skipNames[agentName] {
			continue
		}

		agentDir := filepath.Join(agentsDir, agentName)
		agent := Agent{Name: agentName}

		agentTomlPath := filepath.Join(agentDir, "agent.toml")
		if atData, atErr := fs.ReadFile(agentTomlPath); atErr == nil {
			if _, decErr := toml.Decode(string(atData), &agent); decErr != nil {
				return nil, fmt.Errorf("agents/%s/agent.toml: %w", agentName, decErr)
			}
			agent.Name = agentName
		}

		for _, promptName := range []string{"prompt.template.md", "prompt.md.tmpl", "prompt.md"} {
			promptPath := filepath.Join(agentDir, promptName)
			if _, pErr := fs.Stat(promptPath); pErr == nil {
				agent.PromptTemplate = promptPath
				break
			}
		}

		overlayPath := filepath.Join(agentDir, "overlay")
		if info, oErr := fs.Stat(overlayPath); oErr == nil && info.IsDir() {
			agent.OverlayDir = overlayPath
		}

		namepoolPath := filepath.Join(agentDir, "namepool.txt")
		if _, npErr := fs.Stat(namepoolPath); npErr == nil {
			agent.Namepool = namepoolPath
		}

		discovered = append(discovered, agent)
	}

	return discovered, nil
}
